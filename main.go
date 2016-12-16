package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/fsouza/go-dockerclient"
	"github.com/mesos/mesos-go/executor"
	mesos "github.com/mesos/mesos-go/mesosproto"
	"github.com/newrelic/sidecar/service"
	"github.com/nitro/sidecar-executor/container"
	"github.com/relistan/envconfig"
	"github.com/relistan/go-director"
)

const (
	TaskRunning  = 0
	TaskFinished = iota
	TaskFailed   = iota
	TaskKilled   = iota
)

const (
	StatusSleepTime = 1 * time.Second // How long we wait for final status updates to hit Mesos worker
)

var (
	config Config
)

type Config struct {
	KillTaskTimeout     uint          `split_words:"true" default:"5"` // Seconds
	HttpTimeout         time.Duration `split_words:"true" default:"2s"`
	SidecarRetryCount   int           `split_words:"true" default:"5"`
	SidecarRetryDelay   time.Duration `split_words:"true" default:"3s"`
	SidecarUrl          string        `split_words:"true" default:"http://localhost:7777/state.json"`
	SidecarBackoff      time.Duration `split_words:"true" default:"1m"`
	SidecarPollInterval time.Duration `split_words:"true" default:"30s"`
	SidecarMaxFails     int           `split_words:"true" default:"3"`
	DockerRepository    string        `split_words:"true" default:"https://index.docker.io/v1/"`
}

type sidecarExecutor struct {
	driver      *executor.MesosExecutorDriver
	client      *docker.Client
	httpClient  *http.Client
	watchLooper director.Looper
	dockerAuth  *docker.AuthConfiguration
	failCount   int
}

type SidecarServices struct {
	Servers map[string]struct {
		Services map[string]service.Service
	}
}

func newSidecarExecutor(client *docker.Client, auth *docker.AuthConfiguration) *sidecarExecutor {
	return &sidecarExecutor{
		client:     client,
		httpClient: &http.Client{Timeout: config.HttpTimeout},
		dockerAuth: auth,
	}
}

func logConfig() {
	log.Infof("Executor Config -----------------------")
	log.Infof(" * KillTaskTimeout:     %d", config.KillTaskTimeout)
	log.Infof(" * HttpTimeout:         %s", config.HttpTimeout.String())
	log.Infof(" * SidecarRetryCount:   %d", config.SidecarRetryCount)
	log.Infof(" * SidecarRetryDelay:   %s", config.SidecarRetryDelay.String())
	log.Infof(" * SidecarUrl:          %s", config.SidecarUrl)
	log.Infof(" * SidecarBackoff:      %s", config.SidecarBackoff.String())
	log.Infof(" * SidecarPollInterval: %s", config.SidecarPollInterval.String())
	log.Infof(" * SidecarMaxFails:     %d", config.SidecarMaxFails)
	log.Infof(" * DockerRepository:    %s", config.DockerRepository)

	log.Infof("Environment ---------------------------")
	for _, setting := range os.Environ() {
		if (len(setting) >= 5 && setting[:5] == "MESOS") ||
			((len(setting) >= 8) && setting[:8] == "EXECUTOR") ||
			(setting == "HOME") {
			pair := strings.Split(setting, "=")
			log.Infof(" * %-25s: %s", pair[0], pair[1])
		}
	}
	log.Infof("---------------------------------------")
}

func logTaskEnv(taskInfo *mesos.TaskInfo) {
	env := container.EnvForTask(taskInfo)
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

// Tell Mesos and thus the framework that the task finished. Shutdown driver.
func (exec *sidecarExecutor) finishTask(taskInfo *mesos.TaskInfo) {
	exec.sendStatus(TaskFinished, taskInfo.GetTaskId())
	time.Sleep(StatusSleepTime)
	exec.driver.Stop()
}

// Tell Mesos and thus the framework that the task failed. Shutdown driver.
func (exec *sidecarExecutor) failTask(taskInfo *mesos.TaskInfo) {
	exec.sendStatus(TaskFailed, taskInfo.GetTaskId())

	// Unfortunately the status updates are sent async and we can't
	// get a handle on the channel used to send them. So we wait
	time.Sleep(StatusSleepTime)

	// We're done with this executor, so let's stop now.
	exec.driver.Stop()
}

// Loop on a timed basis and check the health of the process in Sidecar.
// Note that because of the way the retries work, the loop timing is a
// lower bound on the delay.
func (exec *sidecarExecutor) watchContainer(container *docker.Container) {
	time.Sleep(config.SidecarBackoff)

	exec.watchLooper.Loop(func() error {
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

		// Validate health status with Sidecar
		if err = exec.sidecarStatus(container); err != nil {
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
	return exec.failCount < config.SidecarMaxFails
}

// Validate the status of this task with Sidecar
func (exec *sidecarExecutor) sidecarStatus(container *docker.Container) error {
	fetch := func() ([]byte, error) {
		resp, err := exec.httpClient.Get(config.SidecarUrl)
		defer resp.Body.Close()
		if err != nil {
			return nil, err
		}

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}

		return body, nil
	}

	// Try to connect to Sidecar, with some retries
	var err error
	var data []byte
	for i := 0; i < config.SidecarRetryCount; i++ {
		data, err = fetch()
		if err != nil {
			log.Warnf("Failed %d health checks!", i)
			time.Sleep(config.SidecarRetryDelay)
			continue
		}
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

	svc, ok := sidecarLookup(container.ID, services)
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

		log.Errorf("Health failure count exceeded %d", config.SidecarMaxFails)

		exec.failCount = 0
		return errors.New("Unhealthy container: " + container.ID + " failing task!")
	}

	return nil
}

// Try to find the Docker registry auth information. This will look in the usual
// locations. Note that an environment variable of DOCKER_CONFIG can override the
// other locations. If it's set, we'll look in $DOCKER_CONFIG/config.json.
// https://godoc.org/github.com/fsouza/go-dockerclient#NewAuthConfigurationsFromDockerCfg
func getDockerAuthConfig() docker.AuthConfiguration {
	lookup := func() (docker.AuthConfiguration, error) {
		// Attempt to fetch and configure Docker auth
		auths, err := docker.NewAuthConfigurationsFromDockerCfg()
		if err == nil {
			if auth, ok := auths.Configs[config.DockerRepository]; ok {
				log.Infof("Found Docker auth configuration for '%s'", config.DockerRepository)
				return auth, nil
			}
		}

		return docker.AuthConfiguration{}, errors.New("No auth match for repository")
	}

	// Try the first time
	auth, err := lookup()
	if err == nil {
		return auth
	}

	// Set the home dir to the likely path, then try one more time
	os.Setenv("HOME", "/root")
	auth, err = lookup()
	if err != nil {
		log.Warnf(
			"No docker auth match for repository '%s'... proceeding anyway",
			config.DockerRepository,
		)
	}

	return auth
}

func init() {
	flag.Parse()
	err := envconfig.Process("executor", &config)
	if err != nil {
		log.Fatal(err.Error())
	}

	log.SetOutput(os.Stdout)
	log.SetLevel(log.DebugLevel)
}

func main() {
	log.Info("Starting Sidecar Executor")
	logConfig()

	// Get a Docker client. Without one, we can't do anything.
	dockerClient, err := docker.NewClientFromEnv()
	if err != nil {
		log.Fatal(err.Error())
	}

	dockerAuth := getDockerAuthConfig()
	scExec := newSidecarExecutor(dockerClient, &dockerAuth)
	dconfig := executor.DriverConfig{Executor: scExec}

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
