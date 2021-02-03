package container

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/avast/retry-go"
	docker "github.com/fsouza/go-dockerclient"
	mesos "github.com/mesos/mesos-go/api/v1/lib"
	"github.com/pborman/uuid"
	log "github.com/sirupsen/logrus"
)

const (
	// How many times a docker image pull will be retried on error
	PullImageNumRetries = 5

	// Using a small period (50ms) to ensure a consistency latency response at the expense of burst capacity
	// See: https://www.kernel.org/doc/Documentation/scheduler/sched-bwc.txt
	defaultCpuPeriod = 50000 // 50ms
)

var portProtocolsTokenizer = regexp.MustCompile(`,\s?`)

// Our own narrowly-scoped interface for Docker client
type DockerClient interface {
	CreateContainer(opts docker.CreateContainerOptions) (*docker.Container, error)
	InspectContainer(id string) (*docker.Container, error)
	ListContainers(opts docker.ListContainersOptions) ([]docker.APIContainers, error)
	ListImages(docker.ListImagesOptions) ([]docker.APIImages, error)
	Logs(opts docker.LogsOptions) error
	PullImage(docker.PullImageOptions, docker.AuthConfiguration) error
	StartContainer(id string, hostConfig *docker.HostConfig) error
	StopContainer(id string, timeout uint) error
}

// Loop through all the images and see if we have one with a match
// on this repo image:tag combination.
func CheckImage(client DockerClient, taskInfo *mesos.TaskInfo) bool {
	images, err := client.ListImages(docker.ListImagesOptions{All: false})
	if err != nil {
		log.Errorf("Failure to list Docker images: %s", err.Error())
		return false
	}

	for _, img := range images {
		for _, tag := range img.RepoTags {
			if tag == taskInfo.Container.Docker.Image {
				// Exact match, we have this image already
				return true
			}
		}
	}

	return false
}

// Tries very hard to stop a container. Has to take a containerId instead
// of a mesos.TaskInfo because we don't have the TaskInfo in the KillTask
// callback from the executor driver.
func StopContainer(client DockerClient, containerId string, timeout uint) error {
	// Ignore error this time, we'll try again
	_ = client.StopContainer(containerId, timeout)

	cntr, err := client.InspectContainer(containerId)
	if err != nil {
		return err
	}

	if cntr.State.Status == "exited" {
		return nil // Already stopped, nothing to do
	}

	err = client.StopContainer(containerId, timeout)
	if err != nil {
		return err
	}

	if cntr.State.Status != "exited" {
		return fmt.Errorf("Unable to kill container! %s", containerId)
	}

	return nil
}

// PullImage will pull the Docker image refered to in the taskInfo. Uses the Docker
// credentials passed in.
func PullImage(client DockerClient, taskInfo *mesos.TaskInfo, authConfig *docker.AuthConfiguration) error {
	log.Infof("Pulling Docker image '%s'", taskInfo.Container.Docker.Image)

	var numRetries int

	err := retry.Do(func() error {
		numRetries = numRetries + 1

		err := client.PullImage(
			docker.PullImageOptions{
				Repository: taskInfo.Container.Docker.Image,
			},
			*authConfig,
		)
		if err != nil {
			log.Warnf("Pull failed (retry %d/%d): %s", numRetries, PullImageNumRetries, err)

			return err
		}

		log.Info("Pulled.")

		return nil
	},
		retry.Attempts(PullImageNumRetries),
	)

	return err
}

// GetLogs will fetch the Docker logs from a task and return two Readers that let
// us fetch the contents.
func GetLogs(client DockerClient, containerId string, since int64, stdout io.Writer, stderr io.Writer) {
	go func() {
		err := client.Logs(docker.LogsOptions{
			Container:    containerId,
			OutputStream: stdout,
			ErrorStream:  stderr,
			Since:        since,
			Stdout:       true,
			Stderr:       true,
		})

		if err != nil {
			log.Errorf("Failed to fetch logs for task: %s", err.Error())
		}
	}()
}

// FollowLogs will fetch the Docker logs since "since", and start pumping logs into
// the two writers that are passed in.
func FollowLogs(client DockerClient, containerId string, since int64, stdout io.Writer, stderr io.Writer) {
	go func() {
		err := client.Logs(docker.LogsOptions{
			Container:    containerId,
			OutputStream: stdout,
			ErrorStream:  stderr,
			Since:        since,
			Stdout:       true,
			Stderr:       true,
			Follow:       true,
		})

		if err != nil {
			log.Errorf("Failed to fetch logs for task: %s", err.Error())
		}
	}()
}

// Generate a complete config with both Config and HostConfig. Does not attempt
// to be exhaustive in support for Docker options. Supports the most commonly
// used options. Others are not complex to add.
func ConfigForTask(taskInfo *mesos.TaskInfo, forceCpuLimit bool, forceMemoryLimit bool, useCpuShares bool, envVars []string) *docker.CreateContainerOptions {
	labels := LabelsForTask(taskInfo)

	var command []string
	if _, ok := labels["executor.ShellCommand"]; ok {
		command = []string{labels["executor.ShellCommand"]}
		delete(labels, "executor.ShellCommand")
	}

	config := &docker.CreateContainerOptions{
		Name: GetContainerName(&taskInfo.TaskID),
		Config: &docker.Config{
			Env:          EnvForTask(taskInfo, labels, envVars),
			ExposedPorts: PortsForTask(taskInfo),
			Image:        taskInfo.Container.Docker.Image,
			Labels:       labels,
			Cmd:          command,
		},
		HostConfig: &docker.HostConfig{
			Binds:        BindsForTask(taskInfo),
			PortBindings: PortBindingsForTask(taskInfo),
			NetworkMode:  NetworkForTask(taskInfo),
			CapAdd:       CapAddForTask(taskInfo),
			CapDrop:      CapDropForTask(taskInfo),
			VolumeDriver: VolumeDriverForTask(taskInfo),
		},
	}

	// Check for and calculate CPU shares
	setCpuLimit(config, taskInfo, forceCpuLimit, useCpuShares)

	// Check for and calculate memory limit
	memory := getResource("mem", taskInfo)
	if memory != nil && forceMemoryLimit {
		memoryLimit := int64(memory.Scalar.Value * float64(1024*1024))
		log.Infof("Memory limit set to %.0fMB [HostConfig.Memory=%d] ", memory.Scalar.Value, memoryLimit)
		config.HostConfig.Memory = int64(memoryLimit)
	}

	// We waste some CPU here when debugging is off...
	jsonTaskInfo, _ := json.Marshal(*taskInfo)
	log.Debugf("Mesos TaskInfo: %s", jsonTaskInfo)

	jsonConfig, _ := json.Marshal(config)
	log.Debugf("Final config: %s", jsonConfig)

	return config
}

// setCpuLimit figures out what CPU limiting scheme we are using, if any, and
// sets the Docker HostConfig up appropriately.
func setCpuLimit(config *docker.CreateContainerOptions, taskInfo *mesos.TaskInfo, forceCpuLimit bool, useCpuShares bool) {
	cpus := getResource("cpus", taskInfo)
	if cpus == nil || !forceCpuLimit {
		return
	}

	// Use CPU Shares if we ask for old-school CPU shares limiting
	if useCpuShares {
		multiplier := cpus.Scalar.Value / float64(runtime.NumCPU())
		if multiplier > 1.0 {
			log.Errorf("CPUShares enabled, but CPUs value is more than 100%%. Scaling down to 1.0")
			multiplier = 1.0
		}
		config.HostConfig.CPUShares = int64(1024 * multiplier)
		log.Infof("CPU Shares set [HostConfig.CPUShares=%d", config.HostConfig.CPUShares)
		return
	}

	config.HostConfig.CPUPeriod = defaultCpuPeriod
	config.HostConfig.CPUQuota = int64(cpus.Scalar.Value * float64(defaultCpuPeriod))
	log.Infof("CPU limit set [HostConfig.CPUQuota=%d, HostConfig.CPUPeriod=%d]  ", config.HostConfig.CPUQuota, defaultCpuPeriod)
}

// Extract the port protocols. If no protocol is found, default to TCP
func getPortProtocols(port mesos.ContainerInfo_DockerInfo_PortMapping) []string {
	matches := portProtocolsTokenizer.Split(port.GetProtocol(), -1)

	var protocols []string
	// Filter out empty strings
	for _, protocol := range matches {
		if protocol == "" {
			return []string{"tcp"}
		} else {
			protocols = append(protocols, protocol)
		}
	}

	return protocols
}

// Translate Mesos TaskInfo port records in Docker ports map. These show up as EXPOSE
func PortsForTask(taskInfo *mesos.TaskInfo) map[docker.Port]struct{} {
	ports := make(map[docker.Port]struct{}, len(taskInfo.Container.Docker.PortMappings))

	for _, port := range taskInfo.Container.Docker.PortMappings {
		if port.ContainerPort == 0 {
			continue
		}

		for _, proto := range getPortProtocols(port) {
			portStr := docker.Port(strconv.Itoa(int(port.ContainerPort)) + "/" + proto)
			ports[portStr] = struct{}{}
		}
	}

	log.Debugf("Ports: %#v", ports)

	return ports
}

// getParams fetches items by key from the Docker.Parameters slice
func getParams(key string, taskInfo *mesos.TaskInfo) (params []mesos.Parameter) {
	for _, param := range taskInfo.Container.Docker.Parameters {
		if param.Key == "" || param.Key != key {
			continue
		}

		params = append(params, param)
	}

	return params
}

// getHostname finds the hostname for the host from the TaskInfo
func getHostname(taskInfo *mesos.TaskInfo) string {
	if taskInfo.Executor == nil || taskInfo.Executor.Command == nil ||
		taskInfo.Executor.Command.Environment == nil ||
		taskInfo.Executor.Command.Environment.Variables == nil {

		return ""
	}

	for _, envVar := range taskInfo.Executor.Command.Environment.Variables {
		if envVar.Name == "TASK_HOST" {
			return *envVar.Value
		}
	}

	return ""
}

// Map Task Env to Docker Env
func AppendTaskEnv(envVars []string, taskInfo *mesos.TaskInfo) []string {
	if taskInfo.Executor == nil || taskInfo.Executor.Command == nil ||
		taskInfo.Executor.Command.Environment == nil ||
		taskInfo.Executor.Command.Environment.Variables == nil {
		return envVars
	}

	for _, vars := range taskInfo.Executor.Command.Environment.Variables {
		envVars = append(
			envVars,
			fmt.Sprintf("%s=%s", vars.Name, *vars.Value),
		)
	}

	return envVars
}

// Map Mesos environment settings to Docker environment (-e FOO=BAR). Adds a few
// environment variables derived from the labels we were passed, as well. Useful
// for services in containers to know more about their environment.
func EnvForTask(taskInfo *mesos.TaskInfo, labels map[string]string,
	addEnvVars []string) []string {

	var envVars []string

	// Add Task env as Docker Envs (TASK_ID, TASK_DEPLOY_ID, TASK_RACK_ID,
	// TASK_REQUEST_ID, ...)
	envVars = AppendTaskEnv(envVars, taskInfo)

	for _, param := range getParams("env", taskInfo) {
		envVars = append(envVars, param.Value)
	}

	// Add the environment variables generated by the executor config
	envVars = append(envVars, addEnvVars...)

	// Expose port mappings to the container via env vars. This
	// lets the container know its externally-facing ports for
	// purposes of reporting to other services, etc.
	for _, port := range taskInfo.Container.Docker.PortMappings {
		if port.ContainerPort < 1 || port.HostPort < 1 {
			continue
		}

		envVars = append(
			envVars,
			fmt.Sprintf("MESOS_PORT_%d=%d", port.ContainerPort, port.HostPort),
		)
	}

	// We must also expose the external hostname into the container
	// so that tasks can know their public hostname. Otherwise they
	// only know about their container ID as the hostname per Docker.
	hostname := getHostname(taskInfo)
	if len(hostname) > 0 {
		envVars = append(envVars, "MESOS_HOSTNAME="+getHostname(taskInfo))
	}

	// We expose the value in the ServiceName and EnvironmentName
	// labels as env vars to the container as well.
	svcName, svcOk := labels["ServiceName"]
	envName, envOk := labels["EnvironmentName"]
	if svcOk {
		envVars = append(envVars, "SERVICE_NAME="+svcName)
	} else {
		log.Warnf("No ServiceName set for %s", taskInfo.TaskID.Value)
	}

	if envOk {
		envVars = append(envVars, "ENVIRONMENT_NAME="+envName)
	} else {
		log.Warnf("No EnvironmentName set for %s", taskInfo.TaskID.Value)
	}

	// We parse out and expose the version from the Docker tag as well
	if taskInfo.Container == nil || taskInfo.Container.Docker == nil || taskInfo.Container.Docker.Image == "" {
		log.Warnf("Unable to extract version from Docker image for %s", taskInfo.TaskID)
		return envVars
	}

	versionParts := strings.Split(taskInfo.Container.Docker.Image, ":")
	if len(versionParts) < 2 {
		log.Warnf("Unable to extract version from Docker image for %s", taskInfo.TaskID)
		return envVars
	}

	envVars = append(envVars, "SERVICE_VERSION="+versionParts[1])

	return envVars
}

// LabelsForTask maps Mesos parameter lables to Docker labels
func LabelsForTask(taskInfo *mesos.TaskInfo) map[string]string {
	labels := make(map[string]string, len(taskInfo.Container.Docker.Parameters))

	for _, param := range getParams("label", taskInfo) {
		values := strings.SplitN(param.Value, "=", 2)
		if len(values) < 2 {
			log.Debugf("Got label with empty value: %s", param.Key)
			continue // No empty labels
		}
		labels[values[0]] = values[1]
	}

	return labels
}

// BindsForTask turns Mesos volume information to Docker volume binds at runtime
// (equivalent to -v)
func BindsForTask(taskInfo *mesos.TaskInfo) []string {
	var binds []string
	for _, binding := range taskInfo.Container.Volumes {
		if binding.Mode != nil && *binding.Mode != mesos.RW {
			binds = append(binds, fmt.Sprintf("%s:%s:ro", *binding.HostPath, binding.ContainerPath))
		} else {
			binds = append(binds, fmt.Sprintf("%s:%s", *binding.HostPath, binding.ContainerPath))
		}
	}

	log.Debugf("Volumes Binds: %#v", binds)

	return binds
}

// PortBindingsForTask returns the actual ports bound to this container, not
// just EXPOSEd (equivalent to -P)
func PortBindingsForTask(taskInfo *mesos.TaskInfo) map[docker.Port][]docker.PortBinding {
	portBinds := make(map[docker.Port][]docker.PortBinding, len(taskInfo.Container.Docker.PortMappings))

	for _, port := range taskInfo.Container.Docker.PortMappings {
		if port.HostPort < 1 {
			continue
		}

		for _, proto := range getPortProtocols(port) {
			portBinds[docker.Port(strconv.Itoa(int(port.ContainerPort))+"/"+proto)] =
				[]docker.PortBinding{
					{HostPort: strconv.Itoa(int(port.HostPort))},
				}
		}
	}

	log.Debugf("Port Bindings: %#v", portBinds)

	return portBinds
}

// CapAddForTask scans for cap-adds and generate string slice
func CapAddForTask(taskInfo *mesos.TaskInfo) []string {
	var params []string
	for _, param := range getParams("cap-add", taskInfo) {
		params = append(params, param.Value)
	}
	return params
}

// CapDropForTask scans for cap-drops and generate string slice
func CapDropForTask(taskInfo *mesos.TaskInfo) []string {
	var params []string
	for _, param := range getParams("cap-drop", taskInfo) {
		params = append(params, param.Value)
	}
	return params
}

// VolumeDriverForTask scans for volume-driver
func VolumeDriverForTask(taskInfo *mesos.TaskInfo) string {
	var volumeDriver string

	// This should only occur once, but can be set more than once
	// so we just take the last occurrence
	for _, param := range getParams("volume-driver", taskInfo) {
		volumeDriver = param.Value
	}

	return volumeDriver
}

// NetworkForTask maps Mesos enum to strings for Docker
func NetworkForTask(taskInfo *mesos.TaskInfo) string {
	var networkMode string

	switch *taskInfo.Container.Docker.Network {
	case mesos.ContainerInfo_DockerInfo_HOST:
		networkMode = "host"
	case mesos.ContainerInfo_DockerInfo_BRIDGE:
		networkMode = "default"
	case mesos.ContainerInfo_DockerInfo_NONE:
		networkMode = "none"
	default:
		networkMode = "default"
	}

	return networkMode
}

// getResource loops through the resource slice and return the named resource
func getResource(name string, taskInfo *mesos.TaskInfo) *mesos.Resource {
	for _, resource := range taskInfo.Resources {
		if resource.Name == name {
			return &resource
		}
	}

	return nil
}

// Prefix used to name Docker containers in order to distinguish those
// created by Mesos from those created manually.
const DockerNamePrefix = "mesos-"

// GetContainerName constructs a Mesos-friendly container name. This lets Mesos
// properly handle agent/master resolution without us.
func GetContainerName(taskId *mesos.TaskID) string {
	// unique uuid based on the TaskID
	containerUUID := uuid.NewSHA1(uuid.NIL, []byte(taskId.Value))
	return DockerNamePrefix + containerUUID.String()
}

// GetExitCode returns the exit code for a container so that we can try to see
// how it exited and map that to a Mesos status.
func GetExitCode(client DockerClient, containerId string) (int, error) {
	inspect, err := client.InspectContainer(containerId)
	if err != nil {
		return 0, fmt.Errorf("Container %s not found! - %s", containerId, err.Error())
	}
	return inspect.State.ExitCode, nil
}
