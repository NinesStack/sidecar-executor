package container

import (
	"bytes"
	"io/ioutil"
	"testing"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/fsouza/go-dockerclient"
	mesos "github.com/mesos/mesos-go/mesosproto"
	. "github.com/smartystreets/goconvey/convey"
)

const (
	prelude = "LO, praise of the prowess of people-kings of spear-armed Danes, in days long sped"
	ending  = "Thus made their mourning the men of Geatland, for their hero's passing his hearth-companions"
)

func init() {
	log.SetOutput(ioutil.Discard)
}

func Test_PullImage(t *testing.T) {
	Convey("PullImage()", t, func() {

		image := "foo/foo:foo"
		taskInfo := &mesos.TaskInfo{
			Container: &mesos.ContainerInfo{
				Docker: &mesos.ContainerInfo_DockerInfo{
					Image: &image,
				},
			},
		}

		dockerClient := &MockDockerClient{}

		Convey("passes the right params", func() {
			err := PullImage(dockerClient, taskInfo, &docker.AuthConfiguration{})

			So(dockerClient.validOptions, ShouldBeTrue)
			So(err, ShouldBeNil)
		})

		Convey("bubbles up errors", func() {
			dockerClient.PullImageShouldError = true
			err := PullImage(dockerClient, taskInfo, &docker.AuthConfiguration{})

			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "Something went wrong")
		})
	})
}

func Test_CheckImage(t *testing.T) {
	Convey("CheckImage()", t, func() {
		image := "gonitro/sidecar:latest"
		images := []docker.APIImages{
			{
				RepoTags: []string{image, "sidecar", "sidecar:latest"},
			},
		}

		taskInfo := &mesos.TaskInfo{
			Container: &mesos.ContainerInfo{
				Docker: &mesos.ContainerInfo_DockerInfo{
					Image: &image,
				},
			},
		}

		dockerClient := &MockDockerClient{Images: images}

		Convey("handles errors", func() {
			dockerClient.ListImagesShouldError = true
			So(CheckImage(dockerClient, taskInfo), ShouldBeFalse)
		})

		Convey("matches the image", func() {
			So(CheckImage(dockerClient, taskInfo), ShouldBeTrue)
		})

		Convey("handles missing images", func() {
			wrong := "wrong"
			taskInfo.Container.Docker.Image = &wrong
			So(CheckImage(dockerClient, taskInfo), ShouldBeFalse)
		})

	})
}

func Test_StopContainer(t *testing.T) {
	Convey("When stopping containers", t, func() {
		dockerClient := &MockDockerClient{
			StopContainerShouldError: true,
			StopContainerMaxFails:    1,
			Container: &docker.Container{
				State: docker.State{
					Status: "running",
				},
			},
		}

		Convey("retries stopping the container", func() {
			err := StopContainer(dockerClient, "someid", 0)
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldNotContainSubstring, "Unable to kill")
			So(dockerClient.stopContainerFails, ShouldEqual, 2)
		})

		Convey("returns an error when it really won't stop", func() {
			dockerClient.StopContainerMaxFails = 2
			err := StopContainer(dockerClient, "someid", 0)
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "Unable to kill")
			So(dockerClient.stopContainerFails, ShouldEqual, 2)

		})
	})
}

func Test_GetLogs(t *testing.T) {
	Convey("Fetches the logs from a task", t, func() {
		containerId := "mesos-nginx-2392676-1479746266455-1-dev_singularity_sick_sing-DEFAULT"
		dockerClient := &MockDockerClient{
			LogOutputString: prelude,
			LogErrorString:  ending,
		}

		stdout := bytes.NewBuffer(make([]byte, 0, 256))
		stderr := bytes.NewBuffer(make([]byte, 0, 256))

		GetLogs(dockerClient, containerId, time.Now().UTC().Unix(), stdout, stderr)

		time.Sleep(1 * time.Millisecond) // Nasty, but lets buffer flush

		So(string(stdout.Bytes()), ShouldResemble, prelude)
		So(string(stderr.Bytes()), ShouldResemble, ending)

		So(dockerClient.logOpts.Stdout, ShouldBeTrue)
		So(dockerClient.logOpts.OutputStream, ShouldNotBeNil)
		So(dockerClient.logOpts.ErrorStream, ShouldNotBeNil)
	})
}

func Test_ConfigGeneration(t *testing.T) {
	Convey("Generating the Docker config from a Mesos Task", t, func() {

		// The whole structure is full of pointers, so we have to define
		// a bunch of things so we can take their address.
		taskId := "nginx-2392676-1479746266455-1-dev_singularity_sick_sing-DEFAULT"
		image := "foo/foo:foo"
		cpus := "cpus"
		cpusValue := float64(0.5)
		memory := "mem"
		memoryValue := float64(128)

		env := "env"
		envValue := "SOMETHING=123=123"
		label := "label"
		labelValue := "ANYTHING=123=123"
		capAdd := "cap-add"
		capAddValue := "NET_ADMIN"
		capDrop := "cap-drop"
		capDropValue := "NET_ADMIN"

		host := mesos.ContainerInfo_DockerInfo_HOST

		port := uint32(8080)
		port2 := uint32(443)
		port2_hp := uint32(10270)

		v1_cp := "/tmp/somewhere"
		v1_hp := "/tmp/elsewhere"
		v2_cp := "/tmp/foo"
		v2_hp := "/tmp/bar"
		mode := mesos.Volume_RO

		taskInfo := &mesos.TaskInfo{
			TaskId: &mesos.TaskID{Value: &taskId},
			Container: &mesos.ContainerInfo{
				Docker: &mesos.ContainerInfo_DockerInfo{
					Image:   &image,
					Network: &host,
					Parameters: []*mesos.Parameter{
						{
							Key:   &env,
							Value: &envValue,
						},
						{
							Key:   &label,
							Value: &labelValue,
						},
						{
							Key:   &capAdd,
							Value: &capAddValue,
						},
						{
							Key:   &capDrop,
							Value: &capDropValue,
						},
					},
					PortMappings: []*mesos.ContainerInfo_DockerInfo_PortMapping{
						{
							ContainerPort: &port,
						},
						{
							ContainerPort: &port2,
							HostPort:      &port2_hp,
						},
					},
				},
				Volumes: []*mesos.Volume{
					{
						Mode:          &mode,
						ContainerPath: &v1_cp,
						HostPath:      &v1_hp,
					},
					{
						ContainerPath: &v2_cp,
						HostPath:      &v2_hp,
					},
				},
			},
			Resources: []*mesos.Resource{
				{
					Name:   &cpus,
					Scalar: &mesos.Value_Scalar{Value: &cpusValue},
				},
				{
					Name:   &memory,
					Scalar: &mesos.Value_Scalar{Value: &memoryValue},
				},
			},
		}

		opts := ConfigForTask(taskInfo, false, false)
		optsForced := ConfigForTask(taskInfo, true, true)

		Convey("gets the name from the task ID", func() {
			So(opts.Name, ShouldEqual, "mesos-" + taskId)
		})

		Convey("properly calculates the CPU limit", func() {
			So(optsForced.HostConfig.CPUPeriod, ShouldEqual, float64(50000))
			So(optsForced.HostConfig.CPUQuota, ShouldEqual, float64(25000))

			So(opts.HostConfig.CPUPeriod, ShouldEqual, float64(0))
			So(opts.HostConfig.CPUQuota, ShouldEqual, float64(0))
		})

		Convey("properly calculates the memory limit", func() {
			So(optsForced.HostConfig.Memory, ShouldEqual, float64(128*1024*1024))
			So(opts.HostConfig.Memory, ShouldEqual, float64(0))
		})

		Convey("populates the environment", func() {
			So(len(opts.Config.Env), ShouldEqual, 1)
			So(opts.Config.Env[0], ShouldEqual, "SOMETHING=123=123")
		})

		Convey("fills in the exposed ports", func() {
			So(len(opts.Config.ExposedPorts), ShouldEqual, 2)
			So(opts.Config.ExposedPorts["8080/tcp"], ShouldNotBeNil)
		})

		Convey("has the right image name", func() {
			So(opts.Config.Image, ShouldEqual, image)
		})

		Convey("gets the labels", func() {
			So(len(opts.Config.Labels), ShouldEqual, 1)
			So(opts.Config.Labels["ANYTHING"], ShouldEqual, "123=123")
		})

		Convey("gets the cap-adds", func() {
			So(len(opts.HostConfig.CapAdd), ShouldEqual, 1)
			So(opts.HostConfig.CapAdd[0], ShouldEqual, "NET_ADMIN")
		})

		Convey("gets the cap-drops", func() {
			So(len(opts.HostConfig.CapDrop), ShouldEqual, 1)
			So(opts.HostConfig.CapDrop[0], ShouldEqual, "NET_ADMIN")
		})

		Convey("grabs and formats volume binds properly", func() {
			So(len(opts.HostConfig.Binds), ShouldEqual, 2)
			So(opts.HostConfig.Binds[0], ShouldEqual, "/tmp/elsewhere:/tmp/somewhere:ro")
			So(opts.HostConfig.Binds[1], ShouldEqual, "/tmp/bar:/tmp/foo")
		})

		Convey("handles port bindings", func() {
			So(len(opts.HostConfig.PortBindings), ShouldEqual, 1)
			So(opts.HostConfig.PortBindings["443/tcp"][0].HostPort, ShouldEqual, "10270")
		})

		Convey("uses the right network mode when it's set", func() {
			So(opts.HostConfig.NetworkMode, ShouldEqual, "host")
		})

		Convey("defaults to correct network mode", func() {
			none := mesos.ContainerInfo_DockerInfo_NONE
			taskInfo.Container.Docker.Network = &none
			opts := ConfigForTask(taskInfo, false, false)
			So(opts.HostConfig.NetworkMode, ShouldEqual, "none")
		})
	})
}
