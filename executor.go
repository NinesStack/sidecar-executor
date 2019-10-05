package main

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"os"
	"time"

	"fmt"

	"github.com/Nitro/sidecar-executor/container"
	"github.com/Nitro/sidecar-executor/vault"
	"github.com/Nitro/sidecar/service"
	"github.com/fsouza/go-dockerclient"
	mesos "github.com/mesos/mesos-go/api/v1/lib"
	"github.com/relistan/go-director"
	log "github.com/sirupsen/logrus"
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
	watcherDoneChan chan struct{}
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
		watcherDoneChan: make(chan struct{}),
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

// Loop on a timed basis and check the health of the process in Sidecar.
// Note that because of the way the retries work, the loop timing is a
// lower bound on the delay.
func (exec *sidecarExecutor) watchContainer(containerId string, checkSidecar bool) {
	log.Infof("Watching container %s [checkSidecar: %t]", containerId[:12], checkSidecar)
	if checkSidecar {
		time.Sleep(exec.config.SidecarBackoff)
	}

	exec.watchLooper.Loop(func() error {
		containers, err := exec.client.ListContainers(
			docker.ListContainersOptions{},
		)
		if err != nil {
			return err
		}

		// Loop through all the running containers, looking for a running
		// container with our Id.
		ok := false
		for _, entry := range containers {
			if entry.ID == containerId {
				ok = true
				break
			}
		}
		if !ok {
			exitCode, err := container.GetExitCode(exec.client, containerId)

			if err != nil {
				return err
			}

			msg := fmt.Sprintf("Container %s not running! - ExitCode: %d", containerId, exitCode)
			if exitCode == 0 {
				log.Infof(msg)
				exec.watchLooper.Done(nil)
				return nil
			}

			return errors.New(msg)
		}

		if !checkSidecar {
			return nil
		}

		// Validate health status with Sidecar
		if err = exec.sidecarStatus(containerId); err != nil {
			return err
		}

		return nil
	})
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

// StopDriver flags the event loop to exit on the next time around
func (exec *sidecarExecutor) StopDriver() {
	exec.driver.Stop()
}
