package container

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/fsouza/go-dockerclient"
	mesos "github.com/mesos/mesos-go/mesosproto"
)

// Using a small period (50ms) to ensure a consistency latency response at the expense of burst capacity
// See: https://www.kernel.org/doc/Documentation/scheduler/sched-bwc.txt
const defaultCpuPeriod = 50000 // 50ms microseconds

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
			if tag == *taskInfo.Container.Docker.Image {
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
	client.StopContainer(containerId, timeout)

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

// Pull the Docker image refered to in the taskInfo. Uses the Docker
// credentials passed in.
func PullImage(client DockerClient, taskInfo *mesos.TaskInfo, authConfig *docker.AuthConfiguration) error {
	log.Infof("Pulling Docker image '%s'", *taskInfo.Container.Docker.Image)
	err := client.PullImage(
		docker.PullImageOptions{
			Repository: *taskInfo.Container.Docker.Image,
		},
		*authConfig,
	)
	if err != nil {
		return err
	}

	log.Info("Pulled.")

	return nil
}

// Fetch the Docker logs from a task and return two Readers that let
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

// Generate a complete config with both Config and HostConfig. Does not attempt
// to be exhaustive in support for Docker options. Supports the most commonly
// used options. Others are not complex to add.
func ConfigForTask(taskInfo *mesos.TaskInfo, forceCpuLimit bool, forceMemoryLimit bool) *docker.CreateContainerOptions {
	config := &docker.CreateContainerOptions{
		Name: GetContainerName(taskInfo.TaskId),
		Config: &docker.Config{
			Env:          EnvForTask(taskInfo),
			ExposedPorts: PortsForTask(taskInfo),
			Image:        *taskInfo.Container.Docker.Image,
			Labels:       LabelsForTask(taskInfo),
		},
		HostConfig: &docker.HostConfig{
			Binds:        BindsForTask(taskInfo),
			PortBindings: PortBindingsForTask(taskInfo),
			NetworkMode:  NetworkForTask(taskInfo),
			CapAdd:       CapAddForTask(taskInfo),
			CapDrop:      CapDropForTask(taskInfo),
		},
	}

	// Check for and calculate CPU shares
	cpus := getResource("cpus", taskInfo)
	if cpus != nil && forceCpuLimit {
		config.HostConfig.CPUPeriod = defaultCpuPeriod
		config.HostConfig.CPUQuota = int64(*cpus.Scalar.Value * float64(defaultCpuPeriod))
		log.Infof("CPU limit set [HostConfig.CPUQuota=%d, HostConfig.CPUPeriod=%d]  ", config.HostConfig.CPUQuota, defaultCpuPeriod)
	}

	// Check for and calculate memory limit
	memory := getResource("mem", taskInfo)
	if memory != nil && forceMemoryLimit {
		memoryLimit := int64(*memory.Scalar.Value * float64(1024*1024))
		log.Infof("Memory limit set to %.0fMB [HostConfig.Memory=%d] ", *memory.Scalar.Value, memoryLimit)
		config.HostConfig.Memory = int64(memoryLimit)
	}

	// We waste some CPU here when debugging is off...
	jsonTaskInfo, _ := json.Marshal(*taskInfo)
	log.Debugf("Mesos TaskInfo: %s", jsonTaskInfo)

	jsonConfig, _ := json.Marshal(config)
	log.Debugf("Final config: %s", jsonConfig)

	return config
}

// Translate Mesos TaskInfo port records in Docker ports map. These show up as EXPOSE
func PortsForTask(taskInfo *mesos.TaskInfo) map[docker.Port]struct{} {
	ports := make(map[docker.Port]struct{}, len(taskInfo.Container.Docker.PortMappings))

	for _, port := range taskInfo.Container.Docker.PortMappings {
		if port.ContainerPort == nil {
			continue
		}
		portStr := docker.Port(strconv.Itoa(int(*port.ContainerPort)) + "/tcp") // TODO UDP support?
		ports[portStr] = struct{}{}
	}

	log.Debugf("Ports: %#v", ports)

	return ports
}

func getParams(key string, taskInfo *mesos.TaskInfo) (params []*mesos.Parameter) {
	for _, param := range taskInfo.Container.Docker.Parameters {
		if param.Key == nil || *param.Key != key {
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
		if *envVar.Name == "TASK_HOST" {
			return *envVar.Value
		}
	}

	return ""
}

// Map Mesos environment settings to Docker environment (-e FOO=BAR)
func EnvForTask(taskInfo *mesos.TaskInfo) []string {
	var envVars []string

	for _, param := range getParams("env", taskInfo) {
		envVars = append(envVars, *param.Value)
	}

	// Expose port mappings to the container via env vars. This
	// lets the container know its externally-facing ports for
	// purposes of reporting to other services, etc.
	for _, port := range taskInfo.Container.Docker.PortMappings {
		if port.ContainerPort == nil || port.HostPort == nil {
			continue
		}

		envVars = append(
			envVars,
			fmt.Sprintf("MESOS_PORT_%d=%d", *port.ContainerPort, *port.HostPort),
		)
	}

	// We must also expose the external hostname into the container
	// so that tasks can know their public hostname. Otherwise they
	// only know about their container ID as the hostname per Docker.
	hostname := getHostname(taskInfo)
	if len(hostname) > 0 {
		envVars = append(envVars, "MESOS_HOSTNAME="+getHostname(taskInfo))
	}

	return envVars
}

// Map Mesos parameter lables to Docker labels
func LabelsForTask(taskInfo *mesos.TaskInfo) map[string]string {
	labels := make(map[string]string, len(taskInfo.Container.Docker.Parameters))

	for _, param := range getParams("label", taskInfo) {
		values := strings.SplitN(*param.Value, "=", 2)
		if len(values) < 2 {
			log.Debugf("Got label with empty value: %s", *param.Key)
			continue // No empty labels
		}
		labels[values[0]] = values[1]
	}

	return labels
}

// Mesos volume information to Docker volume binds at runtime (equivalent to -v)
func BindsForTask(taskInfo *mesos.TaskInfo) []string {
	var binds []string
	for _, binding := range taskInfo.Container.Volumes {
		if binding.Mode != nil && *binding.Mode != mesos.Volume_RW {
			binds = append(binds, fmt.Sprintf("%s:%s:ro", *binding.HostPath, *binding.ContainerPath))
		} else {
			binds = append(binds, fmt.Sprintf("%s:%s", *binding.HostPath, *binding.ContainerPath))
		}
	}

	log.Debugf("Volumes Binds: %#v", binds)

	return binds
}

// The actual ports bound to this container, nost just EXPOSEd (equivalent to -P)
func PortBindingsForTask(taskInfo *mesos.TaskInfo) map[docker.Port][]docker.PortBinding {
	portBinds := make(map[docker.Port][]docker.PortBinding, len(taskInfo.Container.Docker.PortMappings))

	for _, port := range taskInfo.Container.Docker.PortMappings {
		if port.HostPort == nil {
			continue
		}
		portBinds[docker.Port(strconv.Itoa(int(*port.ContainerPort))+"/tcp")] = // TODO UDP support?
			[]docker.PortBinding{
				docker.PortBinding{HostPort: strconv.Itoa(int(*port.HostPort))},
			}
	}

	log.Debugf("Port Bindings: %#v", portBinds)

	return portBinds
}

// Scan for cap-adds and generate string slice
func CapAddForTask(taskInfo *mesos.TaskInfo) []string {
	var params []string
	for _, param := range getParams("cap-add", taskInfo) {
		params = append(params, *param.Value)
	}
	return params
}

// Scan for cap-drops and generate string slice
func CapDropForTask(taskInfo *mesos.TaskInfo) []string {
	var params []string
	for _, param := range getParams("cap-drop", taskInfo) {
		params = append(params, *param.Value)
	}
	return params
}

// Map Mesos enum to strings for Docker
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

// Loop through the resource slice and return the named resource
func getResource(name string, taskInfo *mesos.TaskInfo) *mesos.Resource {
	for _, resource := range taskInfo.Resources {
		if *resource.Name == name {
			return resource
		}
	}

	return nil
}

// Prefix used to name Docker containers in order to distinguish those
// created by Mesos from those created manually.
const DockerNamePrefix = "mesos-"

func GetContainerName(taskId *mesos.TaskID) string {
	return DockerNamePrefix + *taskId.Value
}

func GetExitCode(client DockerClient, containerId string) (int, error) {
	inspect, err := client.InspectContainer(containerId)
	if err != nil {
		return 0, fmt.Errorf("Container %s not found! - %s", containerId, err.Error())
	}
	return inspect.State.ExitCode, nil
}
