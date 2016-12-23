package container

import (
	"errors"
	"io"
	"io/ioutil"
	"testing"
	"time"

	"github.com/fsouza/go-dockerclient"
	mesos "github.com/mesos/mesos-go/mesosproto"
	. "github.com/smartystreets/goconvey/convey"
)

const (
	prelude = "LO, praise of the prowess of people-kings of spear-armed Danes, in days long sped"
	ending  = "Thus made their mourning the men of Geatland, for their hero's passing his hearth-companions"
)

type mockDockerClient struct {
	validOptions                bool
	PullImageShouldError        bool
	Images                      []docker.APIImages
	ListImagesShouldError       bool
	StopContainerShouldError    bool
	stopContainerFails          int
	StopContainerMaxFails       int
	InspectContainerShouldError bool
	logOpts                     *docker.LogsOptions
	Container                   *docker.Container
}

func (m *mockDockerClient) PullImage(opts docker.PullImageOptions, auth docker.AuthConfiguration) error {
	if m.PullImageShouldError {
		return errors.New("Something went wrong!")
	}

	if len(opts.Repository) > 5 && (docker.AuthConfiguration{}) == auth {
		m.validOptions = true
	}

	return nil
}

func (m *mockDockerClient) ListImages(opts docker.ListImagesOptions) ([]docker.APIImages, error) {
	if m.ListImagesShouldError {
		return nil, errors.New("Something went wrong!")
	}
	return m.Images, nil
}

func (m *mockDockerClient) StopContainer(id string, timeout uint) error {
	if m.StopContainerShouldError {
		m.stopContainerFails += 1

		if m.stopContainerFails > m.StopContainerMaxFails {
			return errors.New("Something went wrong!")
		}
	}
	return nil
}

func (m *mockDockerClient) InspectContainer(id string) (*docker.Container, error) {
	if m.InspectContainerShouldError {
		return nil, errors.New("Something went wrong!")
	}

	if m.Container != nil {
		return m.Container, nil
	}

	return nil, errors.New("Forgot to set the mock container!")
}

func (m *mockDockerClient) Logs(opts docker.LogsOptions) error {
	m.logOpts = &opts
	opts.OutputStream.Write([]byte(prelude))
	opts.OutputStream.(*io.PipeWriter).Close()
	opts.ErrorStream.Write([]byte(ending))
	opts.ErrorStream.(*io.PipeWriter).Close()

	return nil
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

		dockerClient := &mockDockerClient{}

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

		dockerClient := &mockDockerClient{Images: images}

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
		dockerClient := &mockDockerClient{
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
		taskId := "nginx-2392676-1479746266455-1-dev_singularity_sick_sing-DEFAULT"
		dockerClient := &mockDockerClient{}
		taskInfo := &mesos.TaskInfo{
			TaskId: &mesos.TaskID{Value: &taskId},
		}

		stdout, stderr := GetLogs(dockerClient, taskInfo, time.Now().UTC().Unix())
		output, _ := ioutil.ReadAll(stdout)
		errout, _ := ioutil.ReadAll(stderr)

		So(string(output), ShouldEqual, prelude)
		So(string(errout), ShouldEqual, ending)

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
		memory := "memoryMb"
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

		opts := ConfigForTask(taskInfo)

		Convey("gets the name from the task ID", func() {
			So(opts.Name, ShouldEqual, taskId)
		})

		Convey("properly calculates the CPU shares", func() {
			So(opts.Config.CPUShares, ShouldEqual, float64(512))
		})

		Convey("properly calculates the memory limit", func() {
			So(opts.Config.Memory, ShouldEqual, float64(128*1024*1024))
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
			opts := ConfigForTask(taskInfo)
			So(opts.HostConfig.NetworkMode, ShouldEqual, "none")
		})
	})
}
