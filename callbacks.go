package main

import (
	"os"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/mesos/mesos-go/executor"
	mesos "github.com/mesos/mesos-go/mesosproto"
	"github.com/Nitro/sidecar-executor/container"
	"github.com/relistan/go-director"
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
func (exec *sidecarExecutor) copyLogs(taskId string) {
	startTimeEpoch := time.Now().UTC().Add(0 - config.LogsSince).Unix()

	container.GetLogs(
		exec.client, taskId, startTimeEpoch, os.Stdout, os.Stderr,
	)
}

// monitorTask runs in a goroutine and hangs out, waiting for the watchLooper to
// complete. When it completes, it handles the Docker and Mesos interactions.
func (exec *sidecarExecutor) monitorTask(cntnrId string, taskInfo *mesos.TaskInfo) {
	log.Infof("Monitoring container %s for Mesos task %s",
		cntnrId,
		*taskInfo.TaskId.Value,
	)

	// Wait on the watchLooper to return a status
	err := exec.watchLooper.Wait()
	if err != nil {
		log.Errorf("Error! %s", err.Error())
		// Something went wrong, we better take this thing out!
		err := container.StopContainer(exec.client, *taskInfo.TaskId.Value, config.KillTaskTimeout)
		if err != nil {
			log.Errorf("Error stopping container %s! %s", *taskInfo.TaskId.Value, err.Error())
		}
		// Copy the failure logs (hopefully) to stdout/stderr so we can get them
		exec.copyLogs(*taskInfo.TaskId.Value)
		// Notify Mesos
		exec.failTask(taskInfo)
		return
	}

	exec.finishTask(taskInfo)
	log.Info("Task completed: ", taskInfo.GetName())
	return
}

// Callback from Mesos driver to launch a new task in this executor
func (exec *sidecarExecutor) LaunchTask(driver executor.ExecutorDriver, taskInfo *mesos.TaskInfo) {
	log.Infof("Launching task %s with command '%s'", taskInfo.GetName(), taskInfo.Command.GetValue())
	log.Info("Task ID ", taskInfo.GetTaskId().GetValue())

	logTaskEnv(taskInfo)

	// We need to tell the scheduler that we started the task
	exec.sendStatus(TaskRunning, taskInfo.GetTaskId())

	log.Infof("Using image '%s'", *taskInfo.Container.Docker.Image)

	// TODO implement configurable pull timeout?
	if !container.CheckImage(exec.client, taskInfo) || *taskInfo.Container.Docker.ForcePullImage {
		err := container.PullImage(exec.client, taskInfo, exec.dockerAuth)
		if err != nil {
			log.Errorf("Failed to pull image: %s", err.Error())
			exec.failTask(taskInfo)
			return
		}
	} else {
		log.Info("Re-using existing image... already present")
	}

	// Configure and create the container
	containerConfig := container.ConfigForTask(taskInfo)
	cntnr, err := exec.client.CreateContainer(*containerConfig)
	if err != nil {
		log.Errorf("Failed to create Docker container: %s", err.Error())
		exec.failTask(taskInfo)
		return
	}

	// Start the container
	log.Info("Starting container with ID " + cntnr.ID[:12])
	err = exec.client.StartContainer(cntnr.ID, containerConfig.HostConfig)
	if err != nil {
		log.Errorf("Failed to create Docker container: %s", err.Error())
		exec.failTask(taskInfo)
		return
	}

	// For debugging, set process title to contain container ID & image
	SetProcessName("sidecar-executor " + cntnr.ID[:12] + " (" + *taskInfo.Container.Docker.Image + ")")

	exec.watchLooper =
		director.NewImmediateTimedLooper(director.FOREVER, config.SidecarPollInterval, make(chan error))

	// We have to do this in a different goroutine or the scheduler
	// can't send us any further updates.
	go exec.watchContainer(cntnr.ID)
	go exec.monitorTask(cntnr.ID[:12], taskInfo)
}

func (exec *sidecarExecutor) KillTask(driver executor.ExecutorDriver, taskID *mesos.TaskID) {
	log.Infof("Killing task: %s", *taskID.Value)

	// Stop watching the container so we don't send the wrong task status
	go func() { exec.watchLooper.Quit() }()

	err := container.StopContainer(exec.client, *taskID.Value, config.KillTaskTimeout)
	if err != nil {
		log.Errorf("Error stopping container %s! %s", *taskID.Value, err.Error())
	}

	// Have to force this to be an int64
	var status int64 = TaskKilled // Default status is that we shot it in the head

	// Now we need to sort out whether the task finished nicely or not.
	// This driver callback is used both to shoot a task in the head, and when
	// a task is being replaced. The Mesos task status needs to reflect the
	// resulting container State.ExitCode.
	container, err := exec.client.InspectContainer(*taskID.Value)
	if err == nil {
		if container.State.ExitCode == 0 {
			status = TaskFinished // We exited cleanly when asked
		}
	} else {
		log.Errorf("Error inspecting container %s! %s", *taskID.Value, err.Error())
	}

	// Copy the failure logs (hopefully) to stdout/stderr so we can get them
	exec.copyLogs(*taskID.Value)
	// Notify Mesos
	exec.sendStatus(status, taskID)

	// We have to give the driver time to send the message
	time.Sleep(1 * time.Second)
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
