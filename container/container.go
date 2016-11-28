package container

import (
	"fmt"
	"strconv"

	log "github.com/Sirupsen/logrus"
	"github.com/fsouza/go-dockerclient"
	mesos "github.com/mesos/mesos-go/mesosproto"
)

func PullImage(client *docker.Client, taskInfo *mesos.TaskInfo) {
	log.Infof("Pulling Docker image '%s' because of force pull setting", *taskInfo.Container.Docker.Image)
	client.PullImage(docker.PullImageOptions{
		Repository: *taskInfo.Container.Docker.Image,
	},
		docker.AuthConfiguration{},
	)
	log.Info("Pulled.")
}

func ConfigForTask(taskInfo *mesos.TaskInfo) *docker.CreateContainerOptions {
	cpus := getResource("cpus", taskInfo)

	return &docker.CreateContainerOptions{
		Name: *taskInfo.TaskId.Value,
		Config: &docker.Config{
			CPUShares:    int64(*cpus.Scalar.Value * float64(1024)),
			ExposedPorts: PortsForTask(taskInfo),
			Image:        *taskInfo.Container.Docker.Image,
		},
		HostConfig: &docker.HostConfig{
			Binds: BindsForTask(taskInfo),
			PortBindings: PortBindingsForTask(taskInfo),
		},
	}
}

// Translate Mesos TaskInfo port records in Docker ports map. These show up as EXPOSE
func PortsForTask(taskInfo *mesos.TaskInfo) map[docker.Port]struct{} {
	ports := make(map[docker.Port]struct{}, len(taskInfo.Container.Docker.PortMappings))

	for _, port := range taskInfo.Container.Docker.PortMappings {
		portStr := docker.Port(strconv.Itoa(int(*port.ContainerPort)) + "/tcp") // TODO UDP support?
		ports[portStr] = struct{}{}
	}

	log.Debugf("Ports: %#v", ports)

	return ports
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
		portBinds[docker.Port(strconv.Itoa(int(*port.ContainerPort)) + "/tcp")] = // TODO UDP support?
			[]docker.PortBinding{
				docker.PortBinding{ HostPort: strconv.Itoa(int(*port.HostPort)) },
			}
	}

	log.Debugf("Port Bindings: %#v", portBinds)

	return portBinds
}

func getResource(name string, taskInfo *mesos.TaskInfo) *mesos.Resource {
	for _, resource := range taskInfo.Resources {
		if *resource.Name == name {
			return resource
		}
	}

	return nil
}
