package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Nitro/sidecar-executor/container"
	"github.com/fsouza/go-dockerclient"
	"github.com/mesos/mesos-go/api/v0/executor"
	mesos "github.com/mesos/mesos-go/api/v0/mesosproto"
	"github.com/relistan/go-director"
	log "github.com/sirupsen/logrus"
)

// Callbacks from the Mesos driver. These are required to implement
// the Executor interface.

func (exec *sidecarExecutor) Registered(driver executor.ExecutorDriver,
	execInfo *mesos.ExecutorInfo, fwinfo *mesos.FrameworkInfo, slaveInfo *mesos.SlaveInfo) {
	log.Info("Registered Executor on slave ", slaveInfo.GetHostname())
}

func (exec *sidecarExecutor) Reregistered(driver executor.ExecutorDriver, slaveInfo *mesos.SlaveInfo) {
	log.Info("Re-registered Executor on slave ", slaveInfo.GetHostname())
}

func (exec *sidecarExecutor) Disconnected(driver executor.ExecutorDriver) {
	log.Info("Executor disconnected.")
}

// Copy the Docker container logs to stdout and stderr so we can capture some
// failure information in the Mesos logs. Then tooling can fetch crash info
// from the Mesos API.
func (exec *sidecarExecutor) copyLogs(containerId string) {
	startTimeEpoch := time.Now().UTC().Add(0 - config.LogsSince).Unix()

	container.GetLogs(
		exec.client, containerId, startTimeEpoch, os.Stdout, os.Stderr,
	)
}

// monitorTask runs in a goroutine and hangs out, waiting for the watchLooper to
// complete. When it completes, it handles the Docker and Mesos interactions.
func (exec *sidecarExecutor) monitorTask(cntnrId string, taskInfo *mesos.TaskInfo) {
	log.Infof("Monitoring Mesos task %s for container %s",
		*taskInfo.TaskId.Value,
		cntnrId,
	)

	containerName := container.GetContainerName(taskInfo.TaskId)
	// Wait on the watchLooper to return a status
	err := exec.watchLooper.Wait()
	if err != nil {
		log.Errorf("Error! %s", err)
		// Something went wrong, we better take this thing out!
		err := container.StopContainer(exec.client, containerName, config.KillTaskTimeout)
		if err != nil {
			log.Errorf("Error stopping container %s! %s", containerName, err)
		}
		// Copy the failure logs (hopefully) to stdout/stderr so we can get them
		exec.copyLogs(containerName)
		// Notify Mesos
		exec.failTask(taskInfo)
		return
	}

	log.Info("Task completed: ", taskInfo.GetName())
	exec.finishTask(taskInfo)
	return
}

// Callback from Mesos driver to launch a new task in this executor
func (exec *sidecarExecutor) LaunchTask(driver executor.ExecutorDriver, taskInfo *mesos.TaskInfo) {
	log.Infof("Launching task %s with command '%s'", taskInfo.GetName(), taskInfo.Command.GetValue())
	log.Info("Task ID ", taskInfo.GetTaskId().GetValue())

	// We need to tell the scheduler that we started the task
	exec.sendStatus(TaskRunning, taskInfo.GetTaskId())

	log.Infof("Using image '%s'", *taskInfo.Container.Docker.Image)

	// TODO implement configurable pull timeout?
	if !container.CheckImage(exec.client, taskInfo) || *taskInfo.Container.Docker.ForcePullImage {
		err := container.PullImage(exec.client, taskInfo, exec.dockerAuth)
		if err != nil {
			log.Errorf("Failed to pull image: %s", err)
			exec.failTask(taskInfo)
			return
		}
	} else {
		log.Info("Re-using existing image... already present")
	}

	// Additional environment variables we'll pass to the container
	var addEnvVars []string
	if exec.config.SeedSidecar {
		// Fetch the Mesos slave list and append the SIDECAR_SEEDS env var
		addEnvVars = exec.addSidecarSeeds(addEnvVars)
	}

	// Configure the container
	containerConfig := container.ConfigForTask(
		taskInfo,
		exec.config.ForceCpuLimit,
		exec.config.ForceMemoryLimit,
		addEnvVars,
	)

	// Log out what we're starting up with
	exec.logTaskEnv(taskInfo, container.LabelsForTask(taskInfo), addEnvVars)

	// Try to decrypt any existing Vault encoded env.
	decryptedEnv, err := exec.vault.DecryptAllEnv(containerConfig.Config.Env)
	if err != nil {
		log.Error(err.Error())
		exec.failTask(taskInfo)
		return
	}
	containerConfig.Config.Env = decryptedEnv

	// create the container
	cntnr, err := exec.client.CreateContainer(*containerConfig)
	if err != nil {
		log.Errorf("Failed to create Docker container: %s", err)
		exec.failTask(taskInfo)
		return
	}

	// Start the container
	log.Info("Starting container with ID " + cntnr.ID[:12])
	err = exec.client.StartContainer(cntnr.ID, nil)
	if err != nil {
		log.Errorf("Failed to start Docker container: %s", err)
		exec.failTask(taskInfo)
		return
	}

	// For debugging, set process title to contain container ID & image
	SetProcessName("sidecar-executor " + cntnr.ID[:12] + " (" + *taskInfo.Container.Docker.Image + ")")

	exec.watchLooper =
		director.NewImmediateTimedLooper(director.FOREVER, config.SidecarPollInterval, make(chan error))

	exec.logsQuitChan = make(chan struct{})

	// We have to do this in a different goroutine or the scheduler
	// can't send us any further updates.
	go exec.watchContainer(cntnr.ID, shouldCheckSidecar(containerConfig))
	go exec.monitorTask(cntnr.ID[:12], taskInfo)

	// If we're configured to do it, let's watch and relay logs to syslog.
	if exec.config.RelaySyslog {
		go exec.relayLogs(exec.logsQuitChan, cntnr.ID, os.Stdout)
	}

	log.Info("Launched Sidecar tasks... ready for Mesos instructions")
}

// getMasterHostname talks to the local worker endpoint and discovers the
// Mesos master hostname.
func (exec *sidecarExecutor) getMasterHostname() (string, error) {
	envEndpoint := os.Getenv("MESOS_AGENT_ENDPOINT")

	if len(envEndpoint) < 1 { // Did we get anything in the env var?
		return "", fmt.Errorf("Can't get MESOS_AGENT_ENDPOINT from env! Won't provide Sidecar seeds.")
	}
	localEndpoint := "http://" + envEndpoint + "/state"

	localStruct := struct {
		MasterHostname string `json:"master_hostname"`
	}{}

	// Let's find out the Mesos master's hostname
	resp, err := exec.fetcher.Get(localEndpoint)
	if err != nil {
		return "", fmt.Errorf("Unable to fetch Mesos master info from worker endpoint: %s", err)
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("Error reading response body from Mesos worker! '%s'", err)
	}

	err = json.Unmarshal(body, &localStruct)
	if err != nil {
		return "", fmt.Errorf("Error parsing response body from Mesos worker! '%s'", err)
	}

	return localStruct.MasterHostname, nil
}

// getWorkerHostnames returns a slice of all the current worker hostnames
func (exec *sidecarExecutor) getWorkerHostnames(masterHostname string) ([]string, error) {
	masterEndpoint := "http://" + masterHostname + ":5050/slaves"

	type workersStruct struct {
		Hostname string `json:"hostname"`
	}

	masterStruct := struct {
		Slaves []workersStruct `json:"slaves"`
	}{}

	// Let's find out the Mesos master's hostname
	resp, err := exec.fetcher.Get(masterEndpoint)
	if err != nil {
		return nil, fmt.Errorf("Unable to fetch info from master endpoint: %s", err)
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Error reading response body from Mesos master! '%s'", err)
	}

	err = json.Unmarshal(body, &masterStruct)
	if err != nil {
		return nil, fmt.Errorf("Error parsing response body from Mesos master! '%s'", err)
	}

	var workers []string
	for _, worker := range masterStruct.Slaves {
		workers = append(workers, worker.Hostname)
	}

	return workers, nil
}

// addSidecarSeeds mutates the passed slice and inserts an env var formatted
// string (FOO=BAR_1) containing the list of Sidecar seeds that should be
// used to bootstrap a Sidecar instance.
func (exec *sidecarExecutor) addSidecarSeeds(envVars []string) []string {
	masterHostname, err := exec.getMasterHostname()
	if err != nil {
		log.Error(err.Error())
		return envVars
	}

	workerNames, err := exec.getWorkerHostnames(masterHostname)
	if err != nil {
		log.Error(err.Error())
		return envVars
	}

	return append(envVars, "SIDECAR_SEEDS="+strings.Join(workerNames, ","))
}

func (exec *sidecarExecutor) KillTask(driver executor.ExecutorDriver, taskID *mesos.TaskID) {
	log.Infof("Killing task: %s", *taskID.Value)

	// Stop watching the container so we don't send the wrong task status
	go func() { exec.watchLooper.Quit() }()

	containerName := container.GetContainerName(taskID)

	err := container.StopContainer(exec.client, containerName, config.KillTaskTimeout)
	if err != nil {
		log.Errorf("Error stopping container %s! %s", containerName, err.Error())
	}

	// Have to force this to be an int64
	var status int64 = TaskKilled // Default status is that we shot it in the head

	// Now we need to sort out whether the task finished nicely or not.
	// This driver callback is used both to shoot a task in the head, and when
	// a task is being replaced. The Mesos task status needs to reflect the
	// resulting container State.ExitCode.
	container, err := exec.client.InspectContainer(containerName)
	if err == nil {
		if container.State.ExitCode == 0 {
			status = TaskFinished // We exited cleanly when asked
		}
	} else {
		log.Errorf("Error inspecting container %s! %s", containerName, err.Error())
	}

	// Copy the failure logs (hopefully) to stdout/stderr so we can get them
	exec.copyLogs(containerName)
	// Notify Mesos
	exec.sendStatus(status, taskID)

	// We have to give the driver time to send the message
	time.Sleep(StatusSleepTime)

	log.Info("Executor believes container has exited, stopping Mesos driver")
	exec.driver.Stop()
}

func (exec *sidecarExecutor) FrameworkMessage(driver executor.ExecutorDriver, msg string) {
	log.Info("Got framework message: ", msg)
}

func (exec *sidecarExecutor) Shutdown(driver executor.ExecutorDriver) {
	log.Info("Shutting down the executor")
}

func (exec *sidecarExecutor) Error(driver executor.ExecutorDriver, err string) {
	log.Info("Got error message:", err)
}

// Check if it should check Sidecar status, assuming enabled by default
func shouldCheckSidecar(containerConfig *docker.CreateContainerOptions) bool {
	value, ok := containerConfig.Config.Labels["SidecarDiscover"]
	if !ok {
		return true
	}

	if enabled, err := strconv.ParseBool(value); err == nil {
		return enabled
	}

	return true
}
