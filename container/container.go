package container

import (
	"fmt"
	"strconv"
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/fsouza/go-dockerclient"
	mesos "github.com/mesos/mesos-go/mesosproto"
)

// Our own narrowly-scoped interface for Docker client
type DockerClient interface {
	PullImage(docker.PullImageOptions, docker.AuthConfiguration) error
	ListImages(docker.ListImagesOptions) ([]docker.APIImages, error)
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

// Pull the Docker image refered to in the taskInfo
func PullImage(client DockerClient, taskInfo *mesos.TaskInfo) {
	log.Infof("Pulling Docker image '%s'", *taskInfo.Container.Docker.Image)
	client.PullImage(docker.PullImageOptions{
		Repository: *taskInfo.Container.Docker.Image,
	},
		docker.AuthConfiguration{},
	)
	log.Info("Pulled.")
}

// Generate a complete config with both Config and HostConfig. Does not attempt
// to be exhaustive in support for Docker options. Supports the most commonly
// used options. Others are not complex to add.
func ConfigForTask(taskInfo *mesos.TaskInfo) *docker.CreateContainerOptions {

	config := &docker.CreateContainerOptions{
		Name: *taskInfo.TaskId.Value,
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
	if cpus != nil {
		config.Config.CPUShares = int64(*cpus.Scalar.Value * float64(1024))
	}

	// Check for and calculate memory limit
	memory := getResource("memoryMb", taskInfo)
	if memory != nil {
		config.Config.Memory = int64(*memory.Scalar.Value * float64(1024*1024))
	}

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

// Map Mesos environment settings to Docker environment (-e FOO=BAR)
func EnvForTask(taskInfo *mesos.TaskInfo) []string {
	var envVars []string

	for _, param := range getParams("env", taskInfo) {
		envVars = append(envVars, *param.Value)
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
