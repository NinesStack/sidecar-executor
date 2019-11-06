package main

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"os"
	"sync"
	"time"

	"fmt"

	"github.com/Nitro/sidecar-executor/container"
	"github.com/Nitro/sidecar-executor/vault"
	"github.com/Nitro/sidecar/service"
	docker "github.com/fsouza/go-dockerclient"
	mesos "github.com/mesos/mesos-go/api/v1/lib"
	"github.com/relistan/go-director"
	log "github.com/sirupsen/logrus"
)

const (
	StillRunning = -1
)

// ExecDriver narrowly scopes the interface we expect from a driver. It is
// implemented by the ExecutorDriver.
type ExecDriver interface {
	NewStatus(id mesos.TaskID) mesos.TaskStatus
	SendStatusUpdate(status mesos.TaskStatus) error
	Stop()
	Run() error
}

// A sidecarExecutor is the main executor of this application. It handles all
// the application lifecycle, interacting with Sidecar, launching and killing
// tasks, etc. It is driven from the ExecutorDriver.
type sidecarExecutor struct {
	client          container.DockerClient
	fetcher         SidecarFetcher
	watchLooper     director.Looper
	watcherWg       sync.WaitGroup
	logsQuitChan    chan struct{}
	dockerAuth      *docker.AuthConfiguration
	failCount       int
	vault           Vault
	config          Config
	statusSleepTime time.Duration
	// Populated during LaunchTask
	containerConfig *docker.CreateContainerOptions
	containerID     string
	driver          ExecDriver
}

// newSidecarExecutor returns a properly configured sidecarExecutor.
func newSidecarExecutor(client container.DockerClient, auth *docker.AuthConfiguration,
	config Config) *sidecarExecutor {

	return &sidecarExecutor{
		client:          client,
		fetcher:         &http.Client{Timeout: config.HttpTimeout},
		dockerAuth:      auth,
		vault:           vault.NewDefaultVault(),
		config:          config,
		statusSleepTime: DefaultStatusSleepTime,
	}
}

func (exec *sidecarExecutor) logTaskEnv(taskInfo *mesos.TaskInfo, labels map[string]string, addEnvVars []string) {
	env := container.EnvForTask(taskInfo, labels, addEnvVars)
	if len(env) < 1 {
		log.Info("No Docker environment provided")
		return
	}

	log.Infof("Docker Environment --------------------")
	for _, setting := range env {
		log.Infof(" * %s", setting)
	}
	log.Infof("---------------------------------------")
}

// Send task status updates to Mesos via the executor driver
func (exec *sidecarExecutor) sendStatus(status int64, taskID *mesos.TaskID) {
	update := exec.driver.NewStatus(*taskID)

	switch status {
	case TaskRunning:
		update.State = mesos.TASK_RUNNING.Enum()
	case TaskFinished:
		update.State = mesos.TASK_FINISHED.Enum()
	case TaskFailed:
		update.State = mesos.TASK_FAILED.Enum()
	case TaskKilled:
		update.State = mesos.TASK_KILLED.Enum()
	}

	if err := exec.driver.SendStatusUpdate(update); err != nil {
		log.Errorf("Error sending status update %s", err.Error())
		// Panic is the only way we can really let the Agent know something
		// is drastically wrong now.
		panic(err.Error())
	}
}

// Tell Mesos and thus the framework that the task finished. Shutdown driver.
func (exec *sidecarExecutor) finishTask(taskInfo *mesos.TaskInfo) {
	taskID := taskInfo.GetTaskID()
	exec.sendStatus(TaskFinished, &taskID)

	// Unfortunately the status updates are sent async and we can't
	// get a handle on the channel used to send them. So we wait
	time.Sleep(exec.statusSleepTime)

	exec.StopDriver()
}

// Tell Mesos and thus the framework that the task failed. Shutdown driver.
func (exec *sidecarExecutor) failTask(taskInfo *mesos.TaskInfo) {
	taskID := taskInfo.GetTaskID()
	exec.sendStatus(TaskFailed, &taskID)

	// Unfortunately the status updates are sent async and we can't
	// get a handle on the channel used to send them. So we wait
	time.Sleep(exec.statusSleepTime)

	exec.StopDriver()
}

// taskKilled tells Mesos and the framework that the task was killed. Shutdown driver.
func (exec *sidecarExecutor) taskKilled(taskInfo *mesos.TaskInfo) {
	taskID := taskInfo.GetTaskID()
	exec.sendStatus(TaskKilled, &taskID)

	// Unfortunately the status updates are sent async and we can't
	// get a handle on the channel used to send them. So we wait
	time.Sleep(exec.statusSleepTime)

	exec.StopDriver()
}


// Lookup a container in a service list
func sidecarLookup(containerId string, services SidecarServices) (*service.Service, bool) {
	hostname := os.Getenv("TASK_HOST") // Mesos supplies this
	if _, ok := services.Servers[hostname]; !ok {
		// Don't even have this host!
		log.Warnf("Host not found in Sidecar, can't manage this container! (%s)", hostname)
		return nil, ok
	}

	svc, ok := services.Servers[hostname].Services[containerId[:12]]

	return &svc, ok
}

// We only want to kill things that are definitely unhealthy
func shouldBeKilled(svc *service.Service) bool {
	return svc.Status == service.UNHEALTHY || svc.Status == service.TOMBSTONE
}

func (exec *sidecarExecutor) exceededFailCount() bool {
	return exec.failCount >= exec.config.SidecarMaxFails
}

// Validate the status of this task with Sidecar
func (exec *sidecarExecutor) sidecarStatus(containerId string) error {
	fetch := func() ([]byte, error) {
		resp, err := exec.fetcher.Get(exec.config.SidecarUrl)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}

		return body, nil
	}

	// Try to connect to Sidecar, with some retries
	var err error
	var data []byte
	for i := 0; i <= exec.config.SidecarRetryCount; i++ {
		data, err = fetch()
		if err == nil {
			break
		}

		log.Warnf("Failed %d attempts to fetch state from Sidecar!", i+1)
		time.Sleep(exec.config.SidecarRetryDelay)
	}

	// We really really don't want to shut off all the jobs if Sidecar
	// is down. That would make it impossible to deploy Sidecar, and
	// would make the entire system dependent on it for services to
	// even start.
	if err != nil {
		log.Error("Can't contact Sidecar! Assuming healthy...")
		return nil
	}

	// We got a successful result from Sidecar, so let's parse it!
	var services SidecarServices
	err = json.Unmarshal(data, &services)
	if err != nil {
		log.Error("Can't parse Sidecar results! Assuming healthy...")
		return nil
	}

	svc, ok := sidecarLookup(containerId, services)
	if !ok {
		log.Errorf("Can't find this service in Sidecar yet! Assuming healthy...")
		return nil
	}

	// This is the one and only place where we're going to raise our hand
	// and say something is wrong with this service and it needs to be
	// shot by Mesos.
	if shouldBeKilled(svc) {
		// Only bail out if we've exceed the setting for number of failures
		if !exec.exceededFailCount() {
			exec.failCount += 1
			log.Warnf("Failed Sidecar health check, but below fail limit")
			return nil
		}

		log.Errorf("Health failure count exceeded %d", exec.config.SidecarMaxFails)

		exec.failCount = 0
		return errors.New("Unhealthy container: " + containerId + " failing task!")
	}

	exec.failCount = 0 // Reset because we were healthy!

	return nil
}

// monitorTask runs in a goroutine and hangs out, waiting for the watchLooper to
// complete. When it completes, it handles the Docker and Mesos interactions.
func (exec *sidecarExecutor) monitorTask(cntnrId string, taskInfo *mesos.TaskInfo, checkSidecar bool) {
	log.Infof("Monitoring Mesos task %s for container %s [checkSidecar: %t]",
		taskInfo.TaskID.Value, cntnrId[:12], checkSidecar,
	)

	// Wait for Sidecar backoff interval
	if checkSidecar {
		time.Sleep(exec.config.SidecarBackoff)
	}

	// watcherWg is used to let the Sidecar draining exit early if
	// the process exits
	exec.watcherWg.Add(1)

	containerName := container.GetContainerName(&taskInfo.TaskID)

	// Note that because of the way the retries work, the loop timing is a
	// lower bound on the delay.
	var exitCode int = StillRunning
	go exec.watchLooper.Loop(func() error {
		var err error
		exitCode, err = exec.checkContainerStatus(cntnrId, checkSidecar)
		return err
	})

	err := exec.watchLooper.Wait()

	if err != nil {
		log.Errorf("Error! %s", err)

		if exitCode == StillRunning {
			// Something went wrong, we better take this thing out!
			err := container.StopContainer(
				exec.client, containerName, exec.config.KillTaskTimeout,
			)
			if err != nil {
				log.Errorf("Error stopping container %s! %s", containerName, err)
			}
		}
	}
	// Release any goroutines waiting for the watcher to complete
	exec.watcherWg.Done()

	exec.handleContainerExit(taskInfo, exitCode)
}

func (exec *sidecarExecutor) handleContainerExit(taskInfo *mesos.TaskInfo, exitCode int) {
	if exitCode != 0 {
		containerName := container.GetContainerName(&taskInfo.TaskID)
		// Copy the failure logs (hopefully) to stdout/stderr so we can get them
		exec.copyLogs(containerName)
	}

	switch code := exitCode; {
	case code == 137:
		log.Error("Task was killed, notifying Mesos")

		// Notify Mesos
		exec.taskKilled(taskInfo)
	case code > 0:
		log.Error("Task failed, notifying Mesos")

		// Notify Mesos
		exec.failTask(taskInfo)
	case code < 0:
		log.Error("Task may still be running despite attempts to kill!")

		// Notify Mesos
		exec.failTask(taskInfo)
	default:
		log.Info("Task completed: ", taskInfo.GetName())
		exec.finishTask(taskInfo)
	}
}

// checkContainerStatus is called on a timed basis and checks the health of the
// process in Sidecar.
func (exec *sidecarExecutor) checkContainerStatus(containerId string, checkSidecar bool) (int, error) {
	containers, err := exec.client.ListContainers(
		docker.ListContainersOptions{},
	)
	if err != nil {
		return StillRunning, err
	}

	// Loop through all the running containers, looking for a running container
	// with our Id.
	if !containerIsPresent(containers, containerId) {
		exec.watchLooper.Quit() // Will cause looper to exit on next iter

		exitCode, err := container.GetExitCode(exec.client, containerId)
		if err != nil {
			return StillRunning, err
		}

		msg := fmt.Sprintf("Container %s not running! - ExitCode: %d", containerId, exitCode)
		if exitCode == 0 {
			log.Info(msg)
			return 0, nil
		}

		return exitCode, errors.New(msg)
	}

	// It was present, so we're either good, or we report the status from Sidecar
	return StillRunning, exec.maybeCheckSidecar(containerId, checkSidecar)
}

// maybeCheckSidecar will get the container status from Sidecar if we're
// configured to monitor it.
func (exec *sidecarExecutor) maybeCheckSidecar(containerId string, checkSidecar bool) error {
	if !checkSidecar {
		return nil
	}

	// Validate health status with Sidecar
	return exec.sidecarStatus(containerId)
}

// containerIsPresent checks a list of container for the current container
func containerIsPresent(containers []docker.APIContainers, containerId string) bool {
	for _, entry := range containers {
		if entry.ID == containerId {
			return true
		}
	}

	return false
}

// StopDriver flags the event loop to exit on the next time around
func (exec *sidecarExecutor) StopDriver() {
	exec.driver.Stop()
}
