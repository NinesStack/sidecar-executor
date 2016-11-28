package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"os"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/fsouza/go-dockerclient"
	"github.com/mesos/mesos-go/executor"
	mesos "github.com/mesos/mesos-go/mesosproto"
	"github.com/nitro/sidecar-executor/container"
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
)

func (exec *sidecarExecutor) sendStatus(status int64, taskInfo *mesos.TaskInfo) {
	var mesosStatus *mesos.TaskState
	switch status {
	case TaskRunning:
		mesosStatus = mesos.TaskState_TASK_RUNNING.Enum()
	case TaskFinished:
		mesosStatus = mesos.TaskState_TASK_FINISHED.Enum()
	case TaskFailed:
		mesosStatus = mesos.TaskState_TASK_FAILED.Enum()
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
	exec.client.StartContainer(container.ID, containerConfig.HostConfig)

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

func (exec *sidecarExecutor) failTask(taskInfo *mesos.TaskInfo) {
	// Tell Mesos and thus the framework that the task failed
	exec.sendStatus(TaskFailed, taskInfo)

	// Unfortunately the status updates are sent async and we can't
	// get a handle on the channel used to send them. So we wait
	time.Sleep(1 * time.Second)

	// We're done with this executor, so let's stop now.
	exec.driver.Stop()
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

/*
{
   "resources" : [
      {
         "type" : 1,
         "name" : "ports",
         "ranges" : {
            "range" : [
               {
                  "begin" : 31711,
                  "end" : 31711
               }
            ]
         }
      },
      {
         "name" : "cpus",
         "scalar" : {
            "value" : 0.1
         },
         "type" : 0
      },
      {
         "name" : "mem",
         "scalar" : {
            "value" : 128
         },
         "type" : 0
      }
   ],
   "labels" : {},
   "slave_id" : {
      "value" : "48647419-b03c-48f3-b938-2c2ad869eaab-S1"
   },
   "executor" : {
      "framework_id" : {
         "value" : "Singularity"
      },
      "source" : "nginx-2392676-1479746264572-1-NEW_DEPLOY-1479746261223",
      "command" : {
         "value" : "/home/kmatthias/sidecar-executor",
         "environment" : {
            "variables" : [
               {
                  "name" : "INSTANCE_NO",
                  "value" : "1"
               },
               {
                  "value" : "dev-singularity-sick-sing",
                  "name" : "TASK_HOST"
               },
               {
                  "name" : "TASK_REQUEST_ID",
                  "value" : "nginx"
               },
               {
                  "name" : "TASK_DEPLOY_ID",
                  "value" : "2392676"
               },
               {
                  "value" : "nginx-2392676-1479746266455-1-dev_singularity_sick_sing-DEFAULT",
                  "name" : "TASK_ID"
               },
               {
                  "name" : "ESTIMATED_INSTANCE_COUNT",
                  "value" : "3"
               },
               {
                  "name" : "PORT",
                  "value" : "31711"
               },
               {
                  "value" : "31711",
                  "name" : "PORT0"
               }
            ]
         }
      },
      "executor_id" : {
         "value" : "s1"
      }
   },
   "name" : "nginx",
   "task_id" : {
      "value" : "nginx-2392676-1479746266455-1-dev_singularity_sick_sing-DEFAULT"
   },
   "container" : {
      "docker" : {
         "network" : 2,
         "parameters" : [
            {
               "value" : "ServiceName=nginx",
               "key" : "label"
            },
            {
               "key" : "label",
               "value" : "ServicePort_80=11000"
            },
            {
               "value" : "HealthCheck=HttpGet",
               "key" : "label"
            },
            {
               "key" : "label",
               "value" : "HealthCheckArgs=http://{{ host }}:{{ tcp 11000 }}/"
            }
         ],
         "force_pull_image" : false,
         "privileged" : false,
         "image" : "nginx:latest",
         "port_mappings" : [
            {
               "protocol" : "tcp",
               "container_port" : 80,
               "host_port" : 31711
            }
         ]
      },
      "type" : 1
   }
}
*/
