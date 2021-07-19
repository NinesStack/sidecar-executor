package container

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	mesos "github.com/mesos/mesos-go/api/v1/lib"
	. "github.com/smartystreets/goconvey/convey"
)

const (
	prelude = "LO, praise of the prowess of people-kings of spear-armed Danes, in days long sped"
	ending  = "Thus made their mourning the men of Geatland, for their hero's passing his hearth-companions"
)

func init() {
	//log.SetOutput(ioutil.Discard)
}

func Test_PullImage(t *testing.T) {
	Convey("PullImage()", t, func() {

		image := "foo/foo:foo"
		taskInfo := &mesos.TaskInfo{
			Container: &mesos.ContainerInfo{
				Docker: &mesos.ContainerInfo_DockerInfo{
					Image: image,
				},
			},
		}

		dockerClient := &MockDockerClient{}

		Convey("passes the right params", func() {
			err := PullImage(dockerClient, taskInfo, &docker.AuthConfiguration{})

			So(dockerClient.ValidOptions, ShouldBeTrue)
			So(err, ShouldBeNil)
		})

		Convey("bubbles up errors", func() {
			dockerClient.PullImageShouldError = true
			err := PullImage(dockerClient, taskInfo, &docker.AuthConfiguration{})

			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "Something went wrong")
		})

		Convey("retries pulling image PullImageNumRetries times", func() {
			dockerClient.PullImageShouldError = true
			err := PullImage(dockerClient, taskInfo, &docker.AuthConfiguration{})

			So(err, ShouldNotBeNil)
			So(dockerClient.PullImageRetries, ShouldEqual, PullImageNumRetries)
			So(err.Error(), ShouldContainSubstring, "Something went wrong")
		})

		Convey("eventually succeeds pulling image", func() {
			dockerClient.PullImageSuccessAfterNumRetries = 3
			err := PullImage(dockerClient, taskInfo, &docker.AuthConfiguration{})

			So(err, ShouldBeNil)
			So(dockerClient.PullImageRetries, ShouldEqual, 3)
		})
	})
}

func Test_CheckImage(t *testing.T) {
	Convey("CheckImage()", t, func() {
		image := "gonitro/sidecar:1.0.0"
		images := []docker.APIImages{
			{
				RepoTags: []string{image, "sidecar", "sidecar:latest"},
			},
		}

		taskInfo := &mesos.TaskInfo{
			Container: &mesos.ContainerInfo{
				Docker: &mesos.ContainerInfo_DockerInfo{
					Image: image,
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
			taskInfo.Container.Docker.Image = "wrong"
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

		time.Sleep(25 * time.Millisecond) // Nasty, but lets buffer flush

		So(stdout.String(), ShouldResemble, prelude)
		So(stderr.String(), ShouldResemble, ending)

		So(dockerClient.logOpts.Stdout, ShouldBeTrue)
		So(dockerClient.logOpts.OutputStream, ShouldNotBeNil)
		So(dockerClient.logOpts.ErrorStream, ShouldNotBeNil)
	})
}

func Test_ConfigGeneration(t *testing.T) {
	Convey("Generating the Docker config from a Mesos Task", t, func() {

		// The whole structure is full of pointers, so we have to define
		// a bunch of things so we can take their address.
		taskID := "nginx-2392676-1479746266455-1-dev_singularity_sick_sing-DEFAULT"
		uuidTaskID := "f317e960-8579-5258-95c8-96ccca14f317" // UUID based on ssh1(taskID)

		image := "foo/foo:1.0.0"
		command := []string{"date"}

		cpus := float64(0.5) * float64(runtime.NumCPU())
		memory := float64(128)

		envValue := "SOMETHING=123=123"
		labelValue := "ANYTHING=123=123"
		capAddValue := "NET_ADMIN"
		volumeDriverValue := "driver_test"
		capDropValue := "NET_ADMIN"

		shellCommand := `make build beowulf`
		shellCommandLabel := "executor.ShellCommand=" + shellCommand

		svcName := "dev-test-app"
		svcNameLabel := "ServiceName=" + svcName

		envName := "dev"
		envNameLabel := "Environment=" + envName

		host := mesos.ContainerInfo_DockerInfo_HOST

		port := uint32(8080)

		port2 := uint32(443)
		port2_hp := uint32(10270)

		port3 := uint32(9090)
		port3_hp := uint32(10271)
		port3Proto := "tcp,udp"

		v1_cp := "/tmp/somewhere"
		v1_hp := "/tmp/elsewhere"
		v2_cp := "/tmp/foo"
		v2_hp := "/tmp/bar"
		mode := mesos.RO

		hostname := "beowulf.example.com"
		hostKey := "TASK_HOST"

		taskInfo := &mesos.TaskInfo{
			TaskID: mesos.TaskID{Value: taskID},
			Container: &mesos.ContainerInfo{
				Docker: &mesos.ContainerInfo_DockerInfo{
					Image:   image,
					Network: &host,
					Parameters: []mesos.Parameter{
						{
							Key:   "env",
							Value: envValue,
						},
						{
							Key:   "label",
							Value: labelValue,
						},
						{
							Key:   "label",
							Value: svcNameLabel,
						},
						{
							Key:   "label",
							Value: envNameLabel,
						},
						{
							Key:   "label",
							Value: shellCommandLabel,
						},
						{
							Key:   "cap-add",
							Value: capAddValue,
						},
						{
							Key:   "cap-drop",
							Value: capDropValue,
						},
						{
							Key:   "volume-driver",
							Value: volumeDriverValue,
						},
					},
					PortMappings: []mesos.ContainerInfo_DockerInfo_PortMapping{
						{
							ContainerPort: port,
						},
						{
							ContainerPort: port2,
							HostPort:      port2_hp,
						},
						{
							ContainerPort: port3,
							HostPort:      port3_hp,
							Protocol:      &port3Proto,
						},
					},
				},
				Volumes: []mesos.Volume{
					{
						Mode:          &mode,
						ContainerPath: v1_cp,
						HostPath:      &v1_hp,
					},
					{
						ContainerPath: v2_cp,
						HostPath:      &v2_hp,
					},
				},
			},
			Resources: []mesos.Resource{
				{
					Name:   "cpus",
					Scalar: &mesos.Value_Scalar{Value: cpus},
				},
				{
					Name:   "mem",
					Scalar: &mesos.Value_Scalar{Value: memory},
				},
			},
			Executor: &mesos.ExecutorInfo{
				Command: &mesos.CommandInfo{
					Environment: &mesos.Environment{
						Variables: []mesos.Environment_Variable{
							{
								Name:  hostKey,
								Value: &hostname,
							},
						},
					},
				},
			},
			Command: &mesos.CommandInfo{
				Arguments: command,
			},
		}

		opts := ConfigForTask(taskInfo, false, false, false, []string{})
		optsForced := ConfigForTask(taskInfo, true, true, false, []string{})

		Convey("gets the name from the task ID", func() {
			So(opts.Name, ShouldEqual, "mesos-"+uuidTaskID)
		})

		Convey("properly calculates the CPU limit", func() {
			So(optsForced.HostConfig.CPUPeriod, ShouldEqual, float64(defaultCpuPeriod))
			So(optsForced.HostConfig.CPUQuota, ShouldEqual,
				float64(defaultCpuPeriod*cpus),
			)

			So(opts.HostConfig.CPUPeriod, ShouldEqual, float64(0))
			So(opts.HostConfig.CPUQuota, ShouldEqual, float64(0))
		})

		Convey("properly calculates the memory limit", func() {
			So(optsForced.HostConfig.Memory, ShouldEqual, float64(128*1024*1024))
			So(opts.HostConfig.Memory, ShouldEqual, float64(0))
		})

		Convey("populates the environment", func() {
			So(len(opts.Config.Env), ShouldBeGreaterThan, 1)
			So(opts.Config.Env[0], ShouldEqual, "TASK_HOST=beowulf.example.com")
			So(opts.Config.Env[1], ShouldEqual, "SOMETHING=123=123")
		})

		Convey("maps ports into the environment", func() {
			// We index backward to find the vars we just set
			So(opts.Config.Env, ShouldContain, "MESOS_PORT_443=10270")
		})

		Convey("maps the hostname into the environment", func() {
			So(opts.Config.Env, ShouldContain, "MESOS_HOSTNAME="+hostname)
		})

		Convey("maps the ServiceName into the environment", func() {
			So(opts.Config.Env, ShouldContain, "SERVICE_NAME="+svcName)
		})

		Convey("maps the EnvironmentName into the environment", func() {
			So(opts.Config.Env, ShouldContain, "ENVIRONMENT="+envName)
		})

		Convey("maps the version into the environment", func() {
			So(opts.Config.Env, ShouldContain, "SERVICE_VERSION=1.0.0")
		})

		Convey("fills in the exposed ports", func() {
			So(len(opts.Config.ExposedPorts), ShouldEqual, 4)
			So(opts.Config.ExposedPorts, ShouldContainKey, docker.Port("8080/tcp"))
			So(opts.Config.ExposedPorts, ShouldContainKey, docker.Port("9090/tcp"))
			So(opts.Config.ExposedPorts, ShouldContainKey, docker.Port("9090/udp"))
		})

		Convey("has the right image name", func() {
			So(opts.Config.Image, ShouldEqual, image)
		})

		Convey("gets the labels", func() {
			So(len(opts.Config.Labels), ShouldBeGreaterThanOrEqualTo, 1)
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

		Convey("gets the Volume Driver", func() {
			So(opts.HostConfig.VolumeDriver, ShouldEqual, "driver_test")
		})

		Convey("grabs and formats volume binds properly", func() {
			So(len(opts.HostConfig.Binds), ShouldEqual, 2)
			So(opts.HostConfig.Binds[0], ShouldEqual, "/tmp/elsewhere:/tmp/somewhere:ro")
			So(opts.HostConfig.Binds[1], ShouldEqual, "/tmp/bar:/tmp/foo")
		})

		Convey("handles port bindings", func() {
			So(len(opts.HostConfig.PortBindings), ShouldEqual, 3)
			So(opts.HostConfig.PortBindings["443/tcp"][0].HostPort, ShouldEqual, "10270")
			So(opts.HostConfig.PortBindings["9090/tcp"][0].HostPort, ShouldEqual, "10271")
			So(opts.HostConfig.PortBindings["9090/udp"][0].HostPort, ShouldEqual, "10271")
		})

		Convey("uses the right network mode when it's set", func() {
			So(opts.HostConfig.NetworkMode, ShouldEqual, "host")
		})

		Convey("defaults to correct network mode", func() {
			none := mesos.ContainerInfo_DockerInfo_NONE
			taskInfo.Container.Docker.Network = &none
			opts := ConfigForTask(taskInfo, false, false, false, []string{})
			So(opts.HostConfig.NetworkMode, ShouldEqual, "none")
		})

		Convey("supports CPU Shares when requested", func() {
			opts := ConfigForTask(taskInfo, true, false, true, []string{})
			So(opts.HostConfig.CPUShares, ShouldEqual, 512)
		})

		Convey("hard limits CPUs to 1.0 when CPU Shares are enabled", func() {
			taskInfo.Resources[0] = mesos.Resource{
				Name:   "cpus",
				Scalar: &mesos.Value_Scalar{Value: 35},
			}
			opts := ConfigForTask(taskInfo, true, false, true, []string{})
			So(opts.HostConfig.CPUShares, ShouldEqual, 1024)
		})

		Convey("uses the command when it's set", func() {
			cmdParts := strings.Split(shellCommand, " ")
			So(len(opts.Config.Cmd), ShouldEqual, 3)
			for i, part := range cmdParts {
				So(opts.Config.Cmd[i], ShouldResemble, part)
			}
		})
	})
}
