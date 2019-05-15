package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Nitro/sidecar-executor/container"
	"github.com/Nitro/sidecar/service"
	"github.com/fsouza/go-dockerclient"
	mesos "github.com/mesos/mesos-go/api/v0/mesosproto"
	director "github.com/relistan/go-director"
	. "github.com/smartystreets/goconvey/convey"
)

type dummyMesosDriver struct {
	isStopped      bool
	receivedUpdate *mesos.TaskStatus
}

func (d *dummyMesosDriver) Running() bool              { return !d.isStopped }
func (*dummyMesosDriver) Start() (mesos.Status, error) { return 0, nil }
func (d *dummyMesosDriver) Stop() (mesos.Status, error) {
	d.isStopped = true
	return 0, nil
}
func (*dummyMesosDriver) Abort() (mesos.Status, error) { return 0, nil }
func (*dummyMesosDriver) Join() (mesos.Status, error)  { return 0, nil }
func (*dummyMesosDriver) Run() (mesos.Status, error)   { return 0, nil }
func (d *dummyMesosDriver) SendStatusUpdate(taskStatus *mesos.TaskStatus) (mesos.Status, error) {
	d.receivedUpdate = taskStatus
	return 0, nil
}
func (*dummyMesosDriver) SendFrameworkMessage(string) (mesos.Status, error) { return 0, nil }

type mockVault struct {
	failDecrypt bool
}

func (v *mockVault) DecryptAllEnv(envs []string) ([]string, error) {
	if v.failDecrypt {
		return nil, errors.New("some error")
	}

	var decryptedEnv []string
	for _, env := range envs {
		keyValue := strings.SplitN(env, "=", 2)
		envName := keyValue[0]
		envValue := keyValue[1]

		if envValue == "encrypted" {
			decryptedEnv = append(decryptedEnv, envName+"=decrypted")
		} else {
			decryptedEnv = append(decryptedEnv, env)
		}
	}

	return decryptedEnv, nil
}

func Test_shouldCheckSidecar(t *testing.T) {
	Convey("When checking if Sidecar is enabled", t, func() {

		containerOptions := &docker.CreateContainerOptions{
			Config: &docker.Config{},
		}

		Convey("shouldCheckSidecar should be true when the label is missing", func() {
			So(shouldCheckSidecar(containerOptions), ShouldBeTrue)
		})

		Convey("shouldCheckSidecar should be true when SidecarDiscover=true", func() {
			containerOptions.Config.Labels = map[string]string{"SidecarDiscover": "true"}

			So(shouldCheckSidecar(containerOptions), ShouldBeTrue)
		})

		Convey("shouldCheckSidecar should be false when SidecarDiscover=false", func() {
			containerOptions.Config.Labels = map[string]string{"SidecarDiscover": "false"}

			So(shouldCheckSidecar(containerOptions), ShouldBeFalse)
		})
	})
}

func containerLabelsToDockerParameters(labels map[string]string) []*mesos.Parameter {
	var parameters []*mesos.Parameter
	key := "label"
	for labelKey, labelValue := range labels {
		value := labelKey + "=" + labelValue
		parameters = append(parameters,
			&mesos.Parameter{Key: &key, Value: &value},
		)
	}

	return parameters
}

func Test_ExecutorCallbacks(t *testing.T) {
	Convey("sidecarExecutor should", t, func(c C) {
		dummyServiceName := "foobar"
		dummyTaskName := dummyServiceName + "_task"
		dummyTaskIdValue := "task_42"
		dummyTaskId := mesos.TaskID{Value: &dummyTaskIdValue}
		dummyCommand := "sudo_make_me_a_sandwich"
		dummyContainerId := "123456654321"
		dummyDockerImageTag := "666"
		dummyDockerImageId := dummyServiceName + ":" + dummyDockerImageTag
		dummyEnvironmentName := "mordor"
		dummyTaskHost := "brainiac"
		dummyMesosWorkerHost := "jotunheim"
		dummyContainerPort := uint32(80)
		dummyHostPort := uint32(8080)
		// computed via `container.GetContainerName(&dummyTaskId)`
		expectedContainerId := "mesos-5cf434e4-0723-522d-aea0-1a344f913c23"

		// Required by sidecarLookup()
		err := os.Setenv("TASK_HOST", dummyTaskHost)
		So(err, ShouldBeNil)
		defer os.Unsetenv("TASK_HOST")

		mux := http.NewServeMux()
		fakeServer := httptest.NewServer(mux)
		Reset(func() {
			fakeServer.Close()
		})

		// Sidecar services handler
		sidecarStateCalls := 0
		Reset(func() {
			sidecarStateCalls = 0
		})
		mux.HandleFunc("/state.json",
			func(w http.ResponseWriter, r *http.Request) {
				sidecarStateCalls++

				services := SidecarServices{
					Servers: map[string]SidecarServer{
						dummyTaskHost: {
							Services: map[string]service.Service{
								expectedContainerId[:12]: {},
							},
						},
					},
				}

				w.Header().Set("Content-Type", "application/json")
				err := json.NewEncoder(w).Encode(services)
				c.So(err, ShouldBeNil)
			},
		)
		// Sidecar drain handler
		sidecarDrainCalls := 0
		sidecarDrainFailOnce := false
		Reset(func() {
			sidecarDrainCalls = 0
			sidecarDrainFailOnce = false
		})
		mux.HandleFunc(
			fmt.Sprintf("/api/services/%s/drain", dummyContainerId),
			func(w http.ResponseWriter, r *http.Request) {
				c.So(r.URL.Path, ShouldEqual, fmt.Sprintf("/api/services/%s/drain", dummyContainerId))

				sidecarDrainCalls++

				if sidecarDrainFailOnce && sidecarDrainCalls == 1 {
					http.Error(w, "Kaboom!", 500)
					return
				}

				w.WriteHeader(202)
			},
		)
		// Mesos master handler
		mux.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"master_hostname":"`+fakeServer.Listener.Addr().String()+`"}`)
		})
		// Mesos slaves handler
		mux.HandleFunc("/slaves", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"slaves":[{"hostname":"`+dummyMesosWorkerHost+`"}]}`)
		})

		dummyDockerClient := container.MockDockerClient{
			Container: &docker.Container{
				State: docker.State{Status: "exited"},
			},
			Images: []docker.APIImages{
				{
					ID: dummyDockerImageId,
				},
			},
			ListContainersContainers: []docker.APIContainers{
				{ID: container.GetContainerName(&dummyTaskId)},
			},
		}

		dummyMesosDriver := dummyMesosDriver{}

		dummyVault := mockVault{}

		exec := sidecarExecutor{
			dockerAuth: &docker.AuthConfiguration{},
			client:     &dummyDockerClient,
			driver:     &dummyMesosDriver,
			fetcher:    http.DefaultClient,
			config: Config{
				SidecarUrl:              fakeServer.URL + "/state.json",
				SidecarDrainingDuration: 1 * time.Millisecond,
				SidecarPollInterval:     1 * time.Millisecond,
			},
			containerID: dummyContainerId,
			vault:       &dummyVault,
		}

		dummyContainerLabels := map[string]string{
			"ServiceName":     dummyServiceName,
			"EnvironmentName": dummyEnvironmentName,
			// We don't want to check Sidecar in most tests
			"SidecarDiscover": "false",
		}

		var dockerNetworkMode mesos.ContainerInfo_DockerInfo_Network
		taskId := "TASK_ID"
		taskHost := "TASK_HOST"
		taskInfo := mesos.TaskInfo{
			Name:   &dummyTaskName,
			TaskId: &dummyTaskId,
			Command: &mesos.CommandInfo{
				Value: &dummyCommand,
			},
			Executor: &mesos.ExecutorInfo{
				Command: &mesos.CommandInfo{
					Environment: &mesos.Environment{
						Variables: []*mesos.Environment_Variable{
							{Name: &taskId, Value: &dummyTaskIdValue},
							{Name: &taskHost, Value: &dummyTaskHost},
						},
					},
				},
			},
			Container: &mesos.ContainerInfo{
				Docker: &mesos.ContainerInfo_DockerInfo{
					Image:   &dummyDockerImageId,
					Network: &dockerNetworkMode,
					PortMappings: []*mesos.ContainerInfo_DockerInfo_PortMapping{
						{HostPort: &dummyHostPort, ContainerPort: &dummyContainerPort},
					},
					Parameters: containerLabelsToDockerParameters(dummyContainerLabels),
				},
			},
		}

		Convey("LaunchTask()", func() {
			Convey("launches a task", func() {
				Convey("and sends an update to Mesos", func() {
					exec.LaunchTask(exec.driver, &taskInfo)

					So(dummyMesosDriver.receivedUpdate, ShouldNotBeNil)
					So(dummyMesosDriver.receivedUpdate.TaskId, ShouldNotBeNil)
					So(dummyMesosDriver.receivedUpdate.TaskId.Value, ShouldNotBeNil)
					So(*dummyMesosDriver.receivedUpdate.TaskId.Value, ShouldEqual, dummyTaskIdValue)
					So(dummyMesosDriver.receivedUpdate.State, ShouldNotBeNil)
					So(*dummyMesosDriver.receivedUpdate.State, ShouldEqual, *mesos.TaskState_TASK_RUNNING.Enum())
				})

				Convey("and seeds sidecar", func() {
					exec.config.SeedSidecar = true
					err := os.Setenv("MESOS_AGENT_ENDPOINT", fakeServer.Listener.Addr().String())
					So(err, ShouldBeNil)
					defer os.Unsetenv("MESOS_AGENT_ENDPOINT")

					exec.LaunchTask(exec.driver, &taskInfo)

					So(exec.containerConfig.Config.Env, ShouldContain, "SIDECAR_SEEDS="+dummyMesosWorkerHost)
				})

				Convey("and initialises the container config", func() {
					envKey := "env"
					extraEnvVal := "TEST_VAR=dummy_value"
					taskInfo.Container.Docker.Parameters = append(
						taskInfo.Container.Docker.Parameters,
						&mesos.Parameter{Key: &envKey, Value: &extraEnvVal},
					)

					exec.LaunchTask(exec.driver, &taskInfo)

					Convey("and pulls image", func() {
						So(dummyDockerClient.ValidOptions, ShouldBeTrue)
					})

					Convey("and sets the container config on the executor", func() {
						So(exec.containerConfig, ShouldNotBeNil)
						So(exec.containerConfig.Name, ShouldEqual, expectedContainerId)
					})

					Convey("and sets the container ID on the executor", func() {
						So(exec.containerID, ShouldEqual, expectedContainerId)
					})

					Convey("and appends the task env vars", func() {
						So(exec.containerConfig.Config.Env, ShouldContain, "SERVICE_VERSION="+dummyDockerImageTag)
						So(exec.containerConfig.Config.Env, ShouldContain, taskId+"="+dummyTaskIdValue)
						So(exec.containerConfig.Config.Env, ShouldContain, taskHost+"="+dummyTaskHost)
					})

					Convey("and sets the MESOS_PORT_* env vars", func() {
						So(exec.containerConfig.Config.Env, ShouldContain, fmt.Sprintf("MESOS_PORT_%d=%d", dummyContainerPort, dummyHostPort))
					})

					Convey("and sets the MESOS_HOSTNAME env var", func() {
						So(exec.containerConfig.Config.Env, ShouldContain, "MESOS_HOSTNAME="+dummyTaskHost)
					})

					Convey("and sets any extra env vars", func() {
						So(exec.containerConfig.Config.Env, ShouldContain, extraEnvVal)
					})

					Convey("and starts the container", func() {
						So(dummyDockerClient.ContainerStarted, ShouldBeTrue)
					})

					Convey("and sets the process name", func() {
						So(os.Args[0], ShouldStartWith, "sidecar-executor")
						So(os.Args[0], ShouldContainSubstring, dummyDockerImageId)
					})

					Convey("and initialises the watch looper", func() {
						So(exec.watchLooper, ShouldNotBeNil)
					})
				})

				Convey("and reuses existing docker image", func() {
					dummyDockerClient.Images[0].RepoTags = []string{dummyDockerImageId}
					falseValue := false
					taskInfo.Container.Docker.ForcePullImage = &falseValue

					exec.LaunchTask(exec.driver, &taskInfo)

					So(dummyDockerClient.ValidOptions, ShouldBeFalse)
				})

				Convey("and force pulls docker images even if they exist, if configured to do so", func() {
					dummyDockerClient.Images[0].RepoTags = []string{dummyDockerImageId}
					trueValue := true
					taskInfo.Container.Docker.ForcePullImage = &trueValue

					exec.LaunchTask(exec.driver, &taskInfo)

					So(dummyDockerClient.ValidOptions, ShouldBeTrue)
				})

				Convey("and decrypts vault secrets", func() {
					envKey := "env"
					encryptedVal := "SUPER_SECRET_KEY=encrypted"
					decryptedVal := "SUPER_SECRET_KEY=decrypted"
					taskInfo.Container.Docker.Parameters = append(
						taskInfo.Container.Docker.Parameters,
						&mesos.Parameter{Key: &envKey, Value: &encryptedVal},
					)

					exec.LaunchTask(exec.driver, &taskInfo)

					So(exec.containerConfig.Config.Env, ShouldContain, decryptedVal)
				})

				Convey("and fails to launch a task when it can't decrypt vault secrets", func() {
					dummyVault.failDecrypt = true

					exec.LaunchTask(exec.driver, &taskInfo)

					So(dummyMesosDriver.isStopped, ShouldBeTrue)

					Convey("and sends an update to Mesos", func() {
						So(dummyMesosDriver.receivedUpdate, ShouldNotBeNil)
						So(dummyMesosDriver.receivedUpdate.TaskId, ShouldNotBeNil)
						So(dummyMesosDriver.receivedUpdate.TaskId.Value, ShouldNotBeNil)
						So(*dummyMesosDriver.receivedUpdate.TaskId.Value, ShouldEqual, dummyTaskIdValue)
						So(dummyMesosDriver.receivedUpdate.State, ShouldNotBeNil)
						So(*dummyMesosDriver.receivedUpdate.State, ShouldEqual, *mesos.TaskState_TASK_FAILED.Enum())
					})
				})

				Convey("and checks the container health via Sidecar", func() {
					dummyContainerLabels["SidecarDiscover"] = "true"
					taskInfo.Container.Docker.Parameters = containerLabelsToDockerParameters(dummyContainerLabels)
					exec.LaunchTask(exec.driver, &taskInfo)
					exec.watchLooper.Quit()
					err := exec.watchLooper.Wait()
					So(err, ShouldBeNil)
					So(sidecarStateCalls, ShouldEqual, 1)
				})

				// TODO: test exec.handleContainerLogs
			})

			Convey("fails to launch a task", func() {
				Convey("when it fails to pull an image", func() {
					dummyDockerClient.PullImageShouldError = true
					exec.LaunchTask(exec.driver, &taskInfo)

					So(dummyMesosDriver.isStopped, ShouldBeTrue)

					Convey("and sends an update to Mesos", func() {
						So(dummyMesosDriver.receivedUpdate, ShouldNotBeNil)
						So(dummyMesosDriver.receivedUpdate.TaskId, ShouldNotBeNil)
						So(dummyMesosDriver.receivedUpdate.TaskId.Value, ShouldNotBeNil)
						So(*dummyMesosDriver.receivedUpdate.TaskId.Value, ShouldEqual, dummyTaskIdValue)
						So(dummyMesosDriver.receivedUpdate.State, ShouldNotBeNil)
						So(*dummyMesosDriver.receivedUpdate.State, ShouldEqual, *mesos.TaskState_TASK_FAILED.Enum())
					})
				})
			})
		})

		Convey("KillTask()", func() {
			dummyContainerLabels["SidecarDiscover"] = "true"
			exec.containerConfig = &docker.CreateContainerOptions{
				Config: &docker.Config{
					Labels: dummyContainerLabels,
				},
			}

			exec.watchLooper = director.NewFreeLooper(1, make(chan error))

			Convey("drains the service", func() {
				exec.KillTask(exec.driver, &dummyTaskId)
				So(sidecarDrainCalls, ShouldEqual, 1)

				Convey("and sends an update to Mesos", func() {
					So(dummyMesosDriver.receivedUpdate, ShouldNotBeNil)
					So(dummyMesosDriver.receivedUpdate.TaskId, ShouldNotBeNil)
					So(dummyMesosDriver.receivedUpdate.TaskId.Value, ShouldNotBeNil)
					So(*dummyMesosDriver.receivedUpdate.TaskId.Value, ShouldEqual, dummyTaskIdValue)
					So(dummyMesosDriver.receivedUpdate.State, ShouldNotBeNil)
					So(*dummyMesosDriver.receivedUpdate.State, ShouldEqual, *mesos.TaskState_TASK_FINISHED.Enum())
				})

				Convey("and stops the Mesos driver", func() {
					So(dummyMesosDriver.isStopped, ShouldBeTrue)
				})
			})

			Convey("stops draining the service if the container exits prematurely", func() {
				exec.config.SidecarDrainingDuration = 100 * time.Millisecond
				exec.watcherDoneChan = make(chan struct{})
				go exec.watchContainer(dummyContainerId, shouldCheckSidecar(exec.containerConfig))
				go exec.monitorTask(dummyContainerId, &taskInfo)

				exec.KillTask(exec.driver, &dummyTaskId)
				So(<-exec.watcherDoneChan, ShouldResemble, struct{}{})
			})

			Convey("tries multiple times to drain the service", func() {
				exec.config.SidecarRetryCount = 1
				sidecarDrainFailOnce = true
				exec.KillTask(exec.driver, &dummyTaskId)
				So(sidecarDrainCalls, ShouldEqual, 2)
			})

			Convey("doesn't drain the the service when it has the label SidecarDiscover=false", func() {
				dummyContainerLabels["SidecarDiscover"] = "false"
				exec.KillTask(exec.driver, &dummyTaskId)
				So(sidecarDrainCalls, ShouldEqual, 0)
			})

			Convey("doesn't drain the service when the SidecarDrainingDuration config parameter is 0", func() {
				exec.config.SidecarDrainingDuration = 0
				exec.KillTask(exec.driver, &dummyTaskId)
				So(sidecarDrainCalls, ShouldEqual, 0)
			})
		})
	})
}
