package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io/ioutil"
	"os"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/fsouza/go-dockerclient"
	"github.com/mesos/mesos-go/executor"
	mesos "github.com/mesos/mesos-go/mesosproto"
	"github.com/nitro/sidecar-executor/container"
	"github.com/relistan/go-director"
)

type sidecarExecutor struct {
	driver *executor.MesosExecutorDriver
	client *docker.Client
}

func newSidecarExecutor(client *docker.Client) *sidecarExecutor {
	return &sidecarExecutor{
		client: client,
	}
}

const (
	TaskRunning  = 0
	TaskFinished = iota
	TaskFailed   = iota
	TaskKilled   = iota
)

const (
	KillTaskTimeout = 5 // seconds
)

func (exec *sidecarExecutor) sendStatus(status int64, taskId *mesos.TaskID) {
	var mesosStatus *mesos.TaskState
	switch status {
	case TaskRunning:
		mesosStatus = mesos.TaskState_TASK_RUNNING.Enum()
	case TaskFinished:
		mesosStatus = mesos.TaskState_TASK_FINISHED.Enum()
	case TaskFailed:
		mesosStatus = mesos.TaskState_TASK_FAILED.Enum()
	case TaskKilled:
		mesosStatus = mesos.TaskState_TASK_KILLED.Enum()
	}

	update := &mesos.TaskStatus{
		TaskId: taskId,
		State:  mesosStatus,
	}

	if _, err := exec.driver.SendStatusUpdate(update); err != nil {
		log.Errorf("Error sending status update %s", err.Error())
		panic(err.Error())
	}
}

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

	// Store the task info we were passed so we can look at it
	info, _ := json.Marshal(taskInfo)
	ioutil.WriteFile("/tmp/taskinfo.json", info, os.ModeAppend)

	exec.sendStatus(TaskRunning, taskInfo.GetTaskId())

	// TODO implement configurable pull timeout?
	if *taskInfo.Container.Docker.ForcePullImage {
		container.PullImage(exec.client, taskInfo)
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

	// TODO may need to store the handle to the looper and stop it first
	// when killing a task.
	looper := director.NewImmediateTimedLooper(director.FOREVER, 3*time.Second, make(chan error))

	// We have to do this in a different goroutine or the scheduler
	// can't send us any further updates.
	go exec.watchContainer(container, looper)
	go func() {
		log.Infof("Monitoring container %s for Mesos task %s",
			container.ID[:12],
			*taskInfo.TaskId.Value,
		)
		err = looper.Wait()
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

// Tell Mesos and thus the framework that the task finished. Shutdown driver.
func (exec *sidecarExecutor) finishTask(taskInfo *mesos.TaskInfo) {
	exec.sendStatus(TaskFinished, taskInfo.GetTaskId())
	time.Sleep(1 * time.Second)
	exec.driver.Stop()
}

// Tell Mesos and thus the framework that the task failed. Shutdown driver.
func (exec *sidecarExecutor) failTask(taskInfo *mesos.TaskInfo) {
	exec.sendStatus(TaskFailed, taskInfo.GetTaskId())

	// Unfortunately the status updates are sent async and we can't
	// get a handle on the channel used to send them. So we wait
	time.Sleep(1 * time.Second)

	// We're done with this executor, so let's stop now.
	exec.driver.Stop()
}

func (exec *sidecarExecutor) watchContainer(container *docker.Container, looper director.Looper) {
	looper.Loop(func() error {
		containers, err := exec.client.ListContainers(
			docker.ListContainersOptions{
				All: true,
			},
		)
		if err != nil {
			return err
		}

		// Loop through all the containers, looking for a running
		// container with our Id.
		ok := false
		for _, entry := range containers {
			if entry.ID == container.ID {
				ok = true
			}
		}
		if !ok {
			return errors.New("Container " + container.ID + " not running!")
		}

		return nil
	})
}

func (exec *sidecarExecutor) KillTask(driver executor.ExecutorDriver, taskID *mesos.TaskID) {
	log.Infof("Killing task: %s", *taskID.Value)
	err := exec.client.StopContainer(*taskID.Value, KillTaskTimeout)

	if err != nil {
		log.Errorf("Error! %s", err.Error())
	}

	exec.sendStatus(TaskKilled, taskID)

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

func init() {
	flag.Parse()
	log.SetOutput(os.Stdout)
	log.SetLevel(log.DebugLevel)
}

func main() {
	log.Info("Starting Sidecar Executor")

	// Get a Docker client. Without one, we can't do anything.
	dockerClient, err := docker.NewClientFromEnv()
	if err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}

	scExec := newSidecarExecutor(dockerClient)

	dconfig := executor.DriverConfig{
		Executor: scExec,
	}

	driver, err := executor.NewMesosExecutorDriver(dconfig)
	if err != nil || driver == nil {
		log.Info("Unable to create an ExecutorDriver ", err.Error())
	}

	// Give the executor a reference to the driver
	scExec.driver = driver

	_, err = driver.Start()
	if err != nil {
		log.Info("Got error:", err)
		return
	}

	log.Info("Executor process has started")

	_, err = driver.Join()
	if err != nil {
		log.Info("driver failed:", err)
	}

	log.Info("Sidecar Executor exiting")
}
