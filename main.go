package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"os"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/mesos/mesos-go/executor"
	mesos "github.com/mesos/mesos-go/mesosproto"
)

type sidecarExecutor struct {
	driver *executor.MesosExecutorDriver
}

func newSidecarExecutor() *sidecarExecutor {
	return &sidecarExecutor{}
}

const (
	TaskRunning = 0
	TaskFinished = iota
)

func (exec *sidecarExecutor) sendStatus(status int64, taskInfo *mesos.TaskInfo) {
	var mesosStatus *mesos.TaskState
	switch(status) {
		case TaskRunning:
			mesosStatus = mesos.TaskState_TASK_RUNNING.Enum()
		case TaskFinished:
			mesosStatus = mesos.TaskState_TASK_FINISHED.Enum()
	}

	update := &mesos.TaskStatus{
		TaskId: taskInfo.GetTaskId(),
		State:  mesosStatus,
	}

	if _, err := exec.driver.SendStatusUpdate(update); err != nil {
		log.Errorf("Error sending status update %s", err.Error())
		panic(err.Error())
	}
}

func (exec *sidecarExecutor) Registered(driver executor.ExecutorDriver, execInfo *mesos.ExecutorInfo, fwinfo *mesos.FrameworkInfo, slaveInfo *mesos.SlaveInfo) {
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

	exec.sendStatus(TaskRunning, taskInfo)

	// Do something!

	// Tell Mesos and thus the framework that we're done
	exec.sendStatus(TaskFinished, taskInfo)

	log.Info("Task completed: ", taskInfo.GetName())

	// Unfortunately the status updates are sent async and we can't
	// get a handle on the channel used to send them. So we wait
	// and pray that it completes in a second.
	time.Sleep(1 * time.Second)

	// We're done with this executor, so let's stop now.
	driver.Stop()
}

func (exec *sidecarExecutor) KillTask(driver executor.ExecutorDriver, taskID *mesos.TaskID) {
	log.Info("Kill task")
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
}

func main() {
	log.Info("Starting Sidecar Executor")

	scExec := newSidecarExecutor()

	dconfig := executor.DriverConfig{
		Executor: scExec,
	}

	driver, err := executor.NewMesosExecutorDriver(dconfig)
	if err != nil || driver == nil {
		log.Info("Unable to create an ExecutorDriver ", err.Error())
	}

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
