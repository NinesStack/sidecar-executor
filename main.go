package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/Nitro/sidecar-executor/mesosdriver"
	"github.com/Nitro/sidecar/service"
	"github.com/fsouza/go-dockerclient"
	mesosconfig "github.com/mesos/mesos-go/api/v1/lib/executor/config"
	"github.com/relistan/envconfig"
	log "github.com/sirupsen/logrus"
)

const (
	TaskRunning  = 0
	TaskFinished = iota
	TaskFailed   = iota
	TaskKilled   = iota
)

const (
	DefaultStatusSleepTime = 2 * time.Second // How long we wait for final status updates to hit Mesos worker
)

type Config struct {
	KillTaskTimeout         uint          `envconfig:"KILL_TASK_TIMEOUT" default:"5"` // Seconds
	HttpTimeout             time.Duration `envconfig:"HTTP_TIMEOUT" default:"2s"`
	SidecarRetryCount       int           `envconfig:"SIDECAR_RETRY_COUNT" default:"5"`
	SidecarRetryDelay       time.Duration `envconfig:"SIDECAR_RETRY_DELAY" default:"3s"`
	SidecarUrl              string        `envconfig:"SIDECAR_URL" default:"http://localhost:7777/state.json"`
	SidecarBackoff          time.Duration `envconfig:"SIDECAR_BACKOFF" default:"1m"`
	SidecarPollInterval     time.Duration `envconfig:"SIDECAR_POLL_INTERVAL" default:"30s"`
	SidecarMaxFails         int           `envconfig:"SIDECAR_MAX_FAILS" default:"3"`
	SidecarDrainingDuration time.Duration `envconfig:"SIDECAR_DRAINING_DURATION" default:"10s"`
	SeedSidecar             bool          `envconfig:"SEED_SIDECAR" default:"false"`
	DockerRepository        string        `envconfig:"DOCKER_REPOSITORY" default:"https://index.docker.io/v1/"`
	LogsSince               time.Duration `envconfig:"LOGS_SINCE" default:"3m"`
	ForceCpuLimit           bool          `envconfig:"FORCE_CPU_LIMIT" default:"false"`
	ForceMemoryLimit        bool          `envconfig:"FORCE_MEMORY_LIMIT" default:"false"`
	UseCpuShares            bool          `envconfig:"USE_CPU_SHARES" default:"false"`
	Debug                   bool          `envconfig:"DEBUG" default:"false"`

	// Mesos options
	MesosMasterPort string `envconfig:"MESOS_MASTER_PORT" default:"5050"`

	// Syslogging options
	RelaySyslog         bool     `envconfig:"RELAY_SYSLOG" default:"false"`
	SyslogAddr          string   `envconfig:"SYSLOG_ADDR" default:"127.0.0.1:514"`
	ContainerLogsStdout bool     `envconfig:"CONTAINER_LOGS_STDOUT" default:"false"`
	SendDockerLabels    []string `envconfig:"SEND_DOCKER_LABELS" default:""`
	LogHostname         string   `envconfig:"LOG_HOSTNAME"` // Name we log as
}

type Vault interface {
	DecryptAllEnv([]string) ([]string, error)
}

type SidecarServer struct {
	Services map[string]service.Service
}

type SidecarServices struct {
	Servers map[string]SidecarServer
}

type SidecarFetcher interface {
	Get(url string) (resp *http.Response, err error)
	Post(url string, contentType string, body io.Reader) (resp *http.Response, err error)
}

func logConfig(config Config) {
	log.Infof("Executor Config -----------------------")
	log.Infof(" * KillTaskTimeout:         %d", config.KillTaskTimeout)
	log.Infof(" * HttpTimeout:             %s", config.HttpTimeout.String())
	log.Infof(" * SidecarRetryCount:       %d", config.SidecarRetryCount)
	log.Infof(" * SidecarRetryDelay:       %s", config.SidecarRetryDelay.String())
	log.Infof(" * SidecarUrl:              %s", config.SidecarUrl)
	log.Infof(" * SidecarBackoff:          %s", config.SidecarBackoff.String())
	log.Infof(" * SidecarPollInterval:     %s", config.SidecarPollInterval.String())
	log.Infof(" * SidecarMaxFails:         %d", config.SidecarMaxFails)
	log.Infof(" * SidecarDrainingDuration: %s", config.SidecarDrainingDuration)
	log.Infof(" * SeedSidecar:             %t", config.SeedSidecar)
	log.Infof(" * DockerRepository:        %s", config.DockerRepository)
	log.Infof(" * LogsSince:               %s", config.LogsSince.String())
	log.Infof(" * ForceCpuLimit:           %t", config.ForceCpuLimit)
	log.Infof(" * ForceMemoryLimit:        %t", config.ForceMemoryLimit)
	log.Infof(" * UseCpuShares:            %t", config.UseCpuShares)
	log.Infof(" * MesosMasterPort:         %s", config.MesosMasterPort)
	log.Infof(" * RelaySyslog:             %t", config.RelaySyslog)
	log.Infof(" * SyslogAddr:              %s", config.SyslogAddr)
	log.Infof(" * ContainerLogsStdout:     %t", config.ContainerLogsStdout)
	log.Infof(" * SendDockerLabels:        %v", config.SendDockerLabels)
	log.Infof(" * LogHostname:             %s", config.LogHostname)
	log.Infof(" * Debug:                   %t", config.Debug)

	log.Infof("Environment ---------------------------")
	envVars := os.Environ()
	sort.Strings(envVars)
	for _, setting := range envVars {

		// We don't want to log the Vault token since it can be used to log in
		// as us. But it's ok to log VAULT_TOKEN_FILE, so we work around that.
		if shouldSkipLogging(setting) {
			continue
		}

		if strings.HasPrefix(setting, "MESOS") ||
			strings.HasPrefix(setting, "EXECUTOR") ||
			strings.HasPrefix(setting, "VAULT") ||
			(setting == "HOME") {

			pair := strings.Split(setting, "=")
			log.Infof(" * %-30s: %s", pair[0], pair[1])
		}
	}
	log.Infof("---------------------------------------")
}

func shouldSkipLogging(setting string) bool {
	return strings.HasPrefix(setting, "VAULT_TOKEN=") ||
		strings.HasPrefix(setting, "VAULT_USERNAME=") ||
		strings.HasPrefix(setting, "VAULT_PASSWORD=")
}

// Try to find the Docker registry auth information. This will look in the usual
// locations. Note that an environment variable of DOCKER_CONFIG can override the
// other locations. If it's set, we'll look in $DOCKER_CONFIG/config.json.
// https://godoc.org/github.com/fsouza/go-dockerclient#NewAuthConfigurationsFromDockerCfg
func getDockerAuthConfig(dockerRepository string) docker.AuthConfiguration {
	lookup := func() (docker.AuthConfiguration, error) {
		// Attempt to fetch and configure Docker auth
		auths, err := docker.NewAuthConfigurationsFromDockerCfg()
		if err == nil {
			if auth, ok := auths.Configs[dockerRepository]; ok {
				log.Infof("Found Docker auth configuration for '%s'", dockerRepository)
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
			dockerRepository,
		)
	}

	return auth
}

// Set the process name (must be <= current name)
func SetProcessName(name string) {
	argv0str := (*reflect.StringHeader)(unsafe.Pointer(&os.Args[0]))
	argv0 := (*[1 << 30]byte)(unsafe.Pointer(argv0str.Data))[:argv0str.Len]

	// Limit length to existing length
	if len(name) > len(argv0) {
		name = name[:len(argv0)]
	}

	// Space pad over whole pre-existing name
	if len(name) < len(argv0) {
		name = name + strings.Repeat(" ", len(argv0)-len(name))
	}

	copy(argv0, name)
}

// Set up some signal handling for kill/term/int and try to shutdown
// the container and report failure to Mesos.
func handleSignals(scExec *sidecarExecutor) {
	sigChan := make(chan os.Signal, 1) // Buffered!

	// Grab some signals we want to catch where possible
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigChan
	log.Warnf("Received signal '%s', attempting clean shutdown", sig)
	if scExec.watchLooper != nil {
		scExec.watchLooper.Done(errors.New("Got " + sig.String() + " signal!"))
	}
	if scExec.logsQuitChan != nil {
		close(scExec.logsQuitChan) // Signal loops to exit
	}
	time.Sleep(3 * time.Second) // Try to let it quit
	os.Exit(130)                // Ctrl-C received or equivalent
}

func initConfig() (Config, error) {
	var config Config
	err := envconfig.Process("executor", &config)
	if err != nil {
		return Config{}, fmt.Errorf("failed to process envconfig: %s", err)
	}

	if len(config.LogHostname) < 1 {
		// What would we do if this errored, anyway? So, just ignore it
		hostname, _ := os.Hostname()
		config.LogHostname = hostname
	}

	log.SetOutput(os.Stdout)
	if config.Debug {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}

	return config, nil
}

func main() {
	log.Info("Starting Sidecar Executor")
	config, err := initConfig()
	if err != nil {
		log.Fatalf("Failed to init config: %s", err)
	}

	logConfig(config)

	// Get a Docker client. Without one, we can't do anything.
	dockerClient, err := docker.NewClientFromEnv()
	if err != nil {
		log.Fatal(err.Error())
	}

	dockerAuth := getDockerAuthConfig(config.DockerRepository)
	scExec := newSidecarExecutor(dockerClient, &dockerAuth, config)

	// The Mesos lib has its own env configuration, so load that up as well.
	// This supports all the MESOS_* env vars passed by the agent on startup.
	cfg, err := mesosconfig.FromEnv()
	if err != nil {
		log.Fatal("failed to load configuration: " + err.Error())
	}

	// Handle UNIX signals we need to be aware of
	go handleSignals(scExec)

	log.Info("Executor process has started")

	// Configure the Mesos driver. This handles the lifecycle and events
	// that come from the Agent.
	scExec.driver = mesosdriver.NewExecutorDriver(&cfg, scExec)
	err = scExec.driver.Run()
	if err != nil {
		log.Errorf("Immediate Exit: Error from executor driver: %s", err)
		return
	}

	log.Info("Driver exited without error. Waiting 2 seconds to shut down executor.")
	time.Sleep(2 * time.Second)

	log.Info("Sidecar Executor exiting")
}
