package main

import (
	"time"

	"github.com/mesos/mesos-go/executor"
	mesos "github.com/mesos/mesos-go/mesosproto"
	"github.com/nitro/sidecar-executor/container"
	log "github.com/Sirupsen/logrus"
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

func (exec *sidecarExecutor) LaunchTask(driver executor.ExecutorDriver, taskInfo *mesos.TaskInfo) {
	log.Infof("Launching task %s with command '%s'", taskInfo.GetName(), taskInfo.Command.GetValue())
	log.Info("Task ID ", taskInfo.GetTaskId().GetValue())

	logTaskEnv(taskInfo)

	exec.sendStatus(TaskRunning, taskInfo.GetTaskId())

	log.Info("Using image '%s'", *taskInfo.Container.Docker.Image)

	// TODO implement configurable pull timeout?
	if !container.CheckImage(exec.client, taskInfo) || *taskInfo.Container.Docker.ForcePullImage {
		container.PullImage(exec.client, taskInfo)
	} else {
		log.Info("Re-using existing image... already present")
	}

	// Configure and create the container
	containerConfig := container.ConfigForTask(taskInfo)
	container, err := exec.client.CreateContainer(*containerConfig)
	if err != nil {
		log.Error("Failed to create Docker container: %s", err.Error())
		exec.failTask(taskInfo)
		return
	}

	// Start the container
	log.Info("Starting container with ID " + container.ID[:12])
	err = exec.client.StartContainer(container.ID, containerConfig.HostConfig)
	if err != nil {
		log.Error("Failed to create Docker container: %s", err.Error())
		exec.failTask(taskInfo)
		return
	}

	exec.watchLooper =
		director.NewImmediateTimedLooper(director.FOREVER, config.SidecarPollInterval, make(chan error))

	// We have to do this in a different goroutine or the scheduler
	// can't send us any further updates.
	go exec.watchContainer(container)
	go func() {
		log.Infof("Monitoring container %s for Mesos task %s",
			container.ID[:12],
			*taskInfo.TaskId.Value,
		)
		err = exec.watchLooper.Wait()
		if err != nil {
			log.Errorf("Error! %s", err.Error())
			exec.failTask(taskInfo)
			return
		}

		exec.finishTask(taskInfo)
		log.Info("Task completed: ", taskInfo.GetName())
		return
	}()
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
