package main

import (
	"github.com/Nitro/sidecar-executor/container"
	mesos "github.com/mesos/mesos-go/api/v1/lib"
	"github.com/relistan/go-director"
	log "github.com/sirupsen/logrus"
)

// LaunchTask is a callback from Mesos driver to launch a new task in this
// executor.
func (exec *sidecarExecutor) LaunchTask(taskInfo *mesos.TaskInfo) {
	taskID := taskInfo.GetTaskID()
	log.Info("Task ID ", taskID.GetValue())

	// We need to tell the scheduler that we started the task
	exec.sendStatus(TaskRunning, &taskID)

	log.Infof("Using image '%s'", taskInfo.Container.Docker.Image)

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

	dockerLabels := container.LabelsForTask(taskInfo)

	// Look up the AWS Role in Vault if we have one defined
	if role, ok := dockerLabels["vault.AWSRole"]; ok {
		awsRoleVars, err := exec.vault.GetAWSRoleVars(role)
		if err != nil {
			log.Errorf("Failed to get AWS credentials for role '%s'", role)
			exec.failTask(taskInfo)
			return
		}

		if awsRoleVars.Vars == nil {
			log.Error("Got empty AWS vars! Expected creds. Exiting... we can't run like this")
			exec.failTask(taskInfo)
			return
		}

		log.Info("Retrieved AWS Credentials, populating env vars")
		addEnvVars = append(addEnvVars, awsRoleVars.Vars...)
		go exec.monitorAWSCredsLease(awsRoleVars.LeaseID, awsRoleVars.LeaseExpiryTime)

	}

	// Configure the container and cache the container config
	exec.containerConfig = container.ConfigForTask(
		taskInfo,
		exec.config.ForceCpuLimit,
		exec.config.ForceMemoryLimit,
		exec.config.UseCpuShares,
		addEnvVars,
	)

	// Log out what we're starting up with
	exec.logTaskEnv(taskInfo, dockerLabels, addEnvVars)

	// Try to decrypt any existing Vault encoded env.
	decryptedEnv, err := exec.vault.DecryptAllEnv(exec.containerConfig.Config.Env)
	if err != nil {
		log.Error(err.Error())
		exec.failTask(taskInfo)
		return
	}
	exec.containerConfig.Config.Env = decryptedEnv

	// create the container
	cntnr, err := exec.client.CreateContainer(*exec.containerConfig)
	if err != nil {
		log.Errorf("Failed to create Docker container: %s", err)
		exec.failTask(taskInfo)
		return
	}

	// Cache the container ID
	exec.containerID = cntnr.ID

	// Start the container
	log.Info("Starting container with ID " + cntnr.ID[:12])
	err = exec.client.StartContainer(cntnr.ID, nil)
	if err != nil {
		log.Errorf("Failed to start Docker container: %s", err)
		exec.failTask(taskInfo)
		return
	}

	// For debugging, set process title to contain container ID & image
	SetProcessName("sidecar-executor " + cntnr.ID[:12] + " (" + taskInfo.Container.Docker.Image + ")")

	exec.watchLooper = director.NewImmediateTimedLooper(
		director.FOREVER,
		exec.config.SidecarPollInterval,
		make(chan error),
	)

	// We have to do this in a different goroutine or the scheduler
	// can't send us any further updates.
	go exec.monitorTask(
		cntnr.ID, taskInfo, shouldCheckSidecar(exec.containerConfig),
	)

	// We may be responsible for log relaying. Handle, if we are.
	exec.handleContainerLogs(cntnr.ID, dockerLabels)

	log.Info("Launched Sidecar tasks... ready for Mesos instructions")
}

// KillTask is a Mesos callback that will try very hard to kill off a running
// task/container.
func (exec *sidecarExecutor) KillTask(taskID *mesos.TaskID) {
	log.Infof("Killing task: %s", taskID.Value)

	// Instruct Sidecar to set the status of the service to DRAINING
	exec.notifyDrain()

	containerName := container.GetContainerName(taskID)

	// Stop the container ourselves
	err := container.StopContainer(
		exec.client, containerName, exec.config.KillTaskTimeout,
	)
	if err != nil {
		log.Errorf("Error stopping container %s! %s", containerName, err.Error())
	}

	// Stop watching the container and report appropriate task status
	exec.watchLooper.Quit()
}
