package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nitro/sidecar-executor/container"
	"github.com/Nitro/sidecar-executor/vault"
	"github.com/Nitro/sidecar/service"
	docker "github.com/fsouza/go-dockerclient"
	mesos "github.com/mesos/mesos-go/api/v1/lib"
	"github.com/pborman/uuid"
	"github.com/relistan/go-director"
	log "github.com/sirupsen/logrus"
	. "github.com/smartystreets/goconvey/convey"
)

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

type mockMesosDriver struct {
	sync.Mutex
	receivedUpdate *mesos.TaskStatus
	isStopped      bool
}

func (d *mockMesosDriver) NewStatus(id mesos.TaskID) mesos.TaskStatus {
	return mesos.TaskStatus{
		TaskID:     id,
		Source:     mesos.SOURCE_EXECUTOR.Enum(),
		ExecutorID: &mesos.ExecutorID{Value: "fooExecutor"},
		UUID:       []byte(uuid.NewRandom()),
	}
}

func (d *mockMesosDriver) SendStatusUpdate(status mesos.TaskStatus) error {
	d.Lock()
	d.receivedUpdate = &status
	d.Unlock()
	return nil
}

func (d *mockMesosDriver) Run() error {
	return nil
}

func (d *mockMesosDriver) Stop() {
	d.Lock()
	d.isStopped = true
	d.Unlock()
}

type mockVault struct {
	failDecrypt                   bool
	renewAWSCredsLeaseShouldError bool
	maybeRevokeTokenShouldError   bool

	maybeRevokeTokenWasCalled bool
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

func (v *mockVault) MaybeRevokeToken() error {
	v.maybeRevokeTokenWasCalled = true

	if v.maybeRevokeTokenShouldError {
		return errors.New("Intentional test error")
	}
	return nil
}

func (v *mockVault) GetAWSCredsLease(role string) (*vault.VaultAWSCredsLease, error) {
	if role == "valid-aws-role" {
		return &vault.VaultAWSCredsLease{
			Vars:            []string{"AWS_SECRET_ACCESS_KEY=1234awesome", "AWS_ACCESS_KEY_ID=AZ123123COOL"},
			LeaseExpiryTime: time.Now().UTC().Add(60 * time.Minute),
			LeaseID:         "aws/creds/valid-aws-role/9ElbN9g177AAAAjftE1uLtSW",
			Role:            role,
		}, nil
	}

	return nil, errors.New("Intentional test error")
}

func (v *mockVault) RevokeAWSCredsLease(leaseID, role string) error {
	log.Infof("Revoking lease ID '%s' for role '%s'", leaseID, role)

	if role == "valid-aws-role" && leaseID == "aws/creds/valid-aws-role/9ElbN9g177AAAAjftE1uLtSW" {
		log.Info("TEST success revoking AWS role")
		return nil
	}

	return errors.New("Intentional test error")
}

func (v *mockVault) RenewAWSCredsLease(awsCredsLease *vault.VaultAWSCredsLease, ttl int) (*vault.VaultAWSCredsLease, error) {
	if v.renewAWSCredsLeaseShouldError {
		return nil, errors.New("intentional test error from renew")
	}
	return &vault.VaultAWSCredsLease{
		LeaseID:         awsCredsLease.LeaseID,
		LeaseExpiryTime: awsCredsLease.LeaseExpiryTime.Add(time.Duration(ttl) * time.Second),
	}, nil
}

func labelsToDockerParams(labels map[string]string) []mesos.Parameter {
	var parameters []mesos.Parameter
	key := "label"
	for labelKey, labelValue := range labels {
		value := labelKey + "=" + labelValue
		parameters = append(parameters,
			mesos.Parameter{Key: key, Value: value},
		)
	}

	return parameters
}

func Test_ExecutorCallbacks(t *testing.T) {
	Convey("When end-to-end testing sidecarExecutor", t, func(c C) {
		log.SetOutput(ioutil.Discard)
		dummyServiceName := "foobar"
		dummyTaskName := dummyServiceName + "_task"
		dummyTaskIDValue := "task_42"
		dummyTaskID := mesos.TaskID{Value: dummyTaskIDValue}
		dummyCommand := "sudo_make_me_a_sandwich"
		dummyContainerId := "123456654321"
		dummyDockerImageTag := "666"
		dummyDockerImageId := dummyServiceName + ":" + dummyDockerImageTag
		dummyEnvironmentName := "mordor"
		dummyTaskHost := "brainiac"
		dummyMesosWorkerHost := "jotunheim"
		dummyContainerPort := uint32(80)
		dummyHostPort := uint32(8080)
		// computed via `container.GetContainerName(&dummyTaskID)`
		expectedContainerId := "mesos-5cf434e4-0723-522d-aea0-1a344f913c23"

		// Required by sidecarLookup()
		err := os.Setenv("TASK_HOST", dummyTaskHost)
		So(err, ShouldBeNil)
		defer os.Unsetenv("TASK_HOST")

		mux := http.NewServeMux()
		fakeServer := httptest.NewServer(mux)
		Reset(func() {
			log.SetLevel(log.FatalLevel)
			fakeServer.Close()
		})

		// Sidecar services handler
		sidecarStateCalls := 0
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
		// Overwrite with a new chan if a test needs to block and assert state
		// while Sidecar is draining the service
		var sidecarDrainChan chan struct{}
		mux.HandleFunc(
			fmt.Sprintf("/api/services/%s/drain", dummyContainerId),
			func(w http.ResponseWriter, r *http.Request) {
				c.So(r.URL.Path, ShouldEqual, fmt.Sprintf("/api/services/%s/drain", dummyContainerId))

				sidecarDrainCalls++

				if sidecarDrainChan != nil {
					// Signal that we're here and then block until we are told to continue
					sidecarDrainChan <- struct{}{}
					<-sidecarDrainChan
				}

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
				{ID: container.GetContainerName(&dummyTaskID)},
			},
		}

		mockDriver := mockMesosDriver{}

		dummyVault := mockVault{}

		exec := sidecarExecutor{
			dockerAuth: &docker.AuthConfiguration{},
			client:     &dummyDockerClient,
			fetcher:    http.DefaultClient,
			config: Config{
				SidecarUrl:              fakeServer.URL + "/state.json",
				SidecarDrainingDuration: 1 * time.Millisecond,
				SidecarPollInterval:     1 * time.Millisecond,
			},
			containerID: dummyContainerId,
			vault:       &dummyVault,
			driver:      &mockDriver,
		}

		dummyContainerLabels := map[string]string{
			"ServiceName":     dummyServiceName,
			"EnvironmentName": dummyEnvironmentName,
			// We don't want to check Sidecar in most tests
			"SidecarDiscover": "false",
		}

		var dockerNetworkMode mesos.ContainerInfo_DockerInfo_Network
		taskID := "TASK_ID"
		taskHost := "TASK_HOST"
		taskInfo := mesos.TaskInfo{
			Name:   dummyTaskName,
			TaskID: dummyTaskID,
			Command: &mesos.CommandInfo{
				Value: &dummyCommand,
			},
			Executor: &mesos.ExecutorInfo{
				Command: &mesos.CommandInfo{
					Environment: &mesos.Environment{
						Variables: []mesos.Environment_Variable{
							{Name: taskID, Value: &dummyTaskIDValue},
							{Name: taskHost, Value: &dummyTaskHost},
						},
					},
				},
			},
			Container: &mesos.ContainerInfo{
				Docker: &mesos.ContainerInfo_DockerInfo{
					Image:   dummyDockerImageId,
					Network: &dockerNetworkMode,
					PortMappings: []mesos.ContainerInfo_DockerInfo_PortMapping{
						{HostPort: dummyHostPort, ContainerPort: dummyContainerPort},
					},
					Parameters: labelsToDockerParams(dummyContainerLabels),
				},
			},
		}

		Convey("LaunchTask()", func() {
			Convey("Sends and update to Mesos when launching a task", func() {
				exec.LaunchTask(&taskInfo)

				So(mockDriver.receivedUpdate, ShouldNotBeNil)
				So(mockDriver.receivedUpdate.TaskID, ShouldNotBeNil)
				So(mockDriver.receivedUpdate.TaskID.Value, ShouldNotBeNil)
				So(mockDriver.receivedUpdate.TaskID.Value, ShouldEqual, dummyTaskIDValue)
				So(mockDriver.receivedUpdate.State, ShouldNotBeNil)
				So(*mockDriver.receivedUpdate.State, ShouldEqual, *mesos.TASK_RUNNING.Enum())
			})

			Convey("Seeds sidecar", func() {
				exec.config.SeedSidecar = true
				err := os.Setenv("MESOS_AGENT_ENDPOINT", fakeServer.Listener.Addr().String())
				So(err, ShouldBeNil)
				defer os.Unsetenv("MESOS_AGENT_ENDPOINT")

				exec.LaunchTask(&taskInfo)

				So(exec.containerConfig.Config.Env, ShouldContain, "SIDECAR_SEEDS="+dummyMesosWorkerHost)
			})

			Convey("When initializing the container config", func() {
				envKey := "env"
				extraEnvVal := "TEST_VAR=dummy_value"
				taskInfo.Container.Docker.Parameters = append(
					taskInfo.Container.Docker.Parameters,
					mesos.Parameter{Key: envKey, Value: extraEnvVal},
				)

				exec.LaunchTask(&taskInfo)

				Convey("pulls image", func() {
					So(dummyDockerClient.ValidOptions, ShouldBeTrue)
				})

				Convey("sets the container config on the executor", func() {
					So(exec.containerConfig, ShouldNotBeNil)
					So(exec.containerConfig.Name, ShouldEqual, expectedContainerId)
				})

				Convey("sets the container ID on the executor", func() {
					So(exec.containerID, ShouldEqual, expectedContainerId)
				})

				Convey("appends the task env vars", func() {
					So(exec.containerConfig.Config.Env, ShouldContain, "SERVICE_VERSION="+dummyDockerImageTag)
					So(exec.containerConfig.Config.Env, ShouldContain, taskID+"="+dummyTaskIDValue)
					So(exec.containerConfig.Config.Env, ShouldContain, taskHost+"="+dummyTaskHost)
				})

				Convey("sets the MESOS_PORT_* env vars", func() {
					So(exec.containerConfig.Config.Env, ShouldContain, fmt.Sprintf("MESOS_PORT_%d=%d", dummyContainerPort, dummyHostPort))
				})

				Convey("sets the MESOS_HOSTNAME env var", func() {
					So(exec.containerConfig.Config.Env, ShouldContain, "MESOS_HOSTNAME="+dummyTaskHost)
				})

				Convey("sets any extra env vars", func() {
					So(exec.containerConfig.Config.Env, ShouldContain, extraEnvVal)
				})

				Convey("starts the container", func() {
					So(dummyDockerClient.ContainerStarted, ShouldBeTrue)
				})

				Convey("sets the process name", func() {
					So(os.Args[0], ShouldStartWith, "sidecar-executor")
					So(os.Args[0], ShouldContainSubstring, dummyDockerImageId)
				})

				Convey("initialises the watch looper", func() {
					So(exec.watchLooper, ShouldNotBeNil)
				})
			})

			Convey("Reuses the existing docker image", func() {
				dummyDockerClient.Images[0].RepoTags = []string{dummyDockerImageId}
				falseValue := false
				taskInfo.Container.Docker.ForcePullImage = &falseValue

				exec.LaunchTask(&taskInfo)

				So(dummyDockerClient.ValidOptions, ShouldBeFalse)
			})

			Convey("Force pulls docker images even if they exist, if configured to do so", func() {
				dummyDockerClient.Images[0].RepoTags = []string{dummyDockerImageId}
				trueValue := true
				taskInfo.Container.Docker.ForcePullImage = &trueValue

				exec.LaunchTask(&taskInfo)

				So(dummyDockerClient.ValidOptions, ShouldBeTrue)
			})

			Convey("Decrypts vault secrets", func() {
				envKey := "env"
				encryptedVal := "SUPER_SECRET_KEY=encrypted"
				decryptedVal := "SUPER_SECRET_KEY=decrypted"
				taskInfo.Container.Docker.Parameters = append(
					taskInfo.Container.Docker.Parameters,
					mesos.Parameter{Key: envKey, Value: encryptedVal},
				)

				exec.LaunchTask(&taskInfo)

				So(exec.containerConfig.Config.Env, ShouldContain, decryptedVal)
			})

			Convey("Gets AWS creds from Vault when a role is specified", func() {
				exec.config.AWSRole = "valid-aws-role"
				taskInfo.Container.Docker.Parameters = labelsToDockerParams(dummyContainerLabels)

				// We'll use logging output to validate that the goroutine ran
				var capture bytes.Buffer
				log.SetLevel(log.DebugLevel)
				log.SetOutput(&capture)

				exec.LaunchTask(&taskInfo)

				log.SetOutput(ioutil.Discard)

				// Make sure some things happened
				So(capture.String(), ShouldContainSubstring, "Monitoring AWS Credentials")
				So(capture.String(), ShouldContainSubstring, "Retrieved AWS Credentials")

				// Make sure the vars are present in the container's config
				So(exec.containerConfig.Config.Env, ShouldContain, "AWS_SECRET_ACCESS_KEY=1234awesome")
				So(exec.containerConfig.Config.Env, ShouldContain, "AWS_ACCESS_KEY_ID=AZ123123COOL")
			})

			Convey("Ups the TTL on creds from Vault when specified", func() {
				exec.config.AWSRole = "valid-aws-role"
				exec.config.AWSRoleTTL = time.Duration(1*time.Minute+40*time.Second)
				taskInfo.Container.Docker.Parameters = labelsToDockerParams(dummyContainerLabels)

				// We'll use logging output to validate that the goroutine ran
				var capture bytes.Buffer
				log.SetLevel(log.DebugLevel)
				log.SetOutput(&capture)

				exec.LaunchTask(&taskInfo)

				log.SetOutput(ioutil.Discard)

				// Make sure some things happened
				So(capture.String(), ShouldContainSubstring, "Monitoring AWS Credentials")
				So(capture.String(), ShouldContainSubstring, "Renewing AWS Lease")
				So(capture.String(), ShouldNotContainSubstring, "Unable to renew")
			})

			Convey("Fails to launch a task when the AWS Rols is wrong", func() {
				exec.config.AWSRole = "invalid-aws-role"
				taskInfo.Container.Docker.Parameters = labelsToDockerParams(dummyContainerLabels)

				var capture bytes.Buffer
				log.SetLevel(log.DebugLevel)
				log.SetOutput(&capture)

				exec.LaunchTask(&taskInfo)

				log.SetOutput(ioutil.Discard)

				So(capture.String(), ShouldContainSubstring, "Failed to get AWS credentials for role")
				So(mockDriver.isStopped, ShouldBeTrue)

				Convey("sends an update to Mesos", func() {
					So(mockDriver.receivedUpdate, ShouldNotBeNil)
					So(mockDriver.receivedUpdate.TaskID, ShouldNotBeNil)
					So(mockDriver.receivedUpdate.TaskID.Value, ShouldNotBeNil)
					So(mockDriver.receivedUpdate.TaskID.Value, ShouldEqual, dummyTaskIDValue)
					So(mockDriver.receivedUpdate.State, ShouldNotBeNil)
					So(*mockDriver.receivedUpdate.State, ShouldEqual, *mesos.TASK_FAILED.Enum())
				})
			})

			Convey("Fails to launch a task when it can't decrypt vault secrets", func() {
				dummyVault.failDecrypt = true
				exec.LaunchTask(&taskInfo)

				So(mockDriver.isStopped, ShouldBeTrue)

				Convey("sends an update to Mesos", func() {
					So(mockDriver.receivedUpdate, ShouldNotBeNil)
					So(mockDriver.receivedUpdate.TaskID, ShouldNotBeNil)
					So(mockDriver.receivedUpdate.TaskID.Value, ShouldNotBeNil)
					So(mockDriver.receivedUpdate.TaskID.Value, ShouldEqual, dummyTaskIDValue)
					So(mockDriver.receivedUpdate.State, ShouldNotBeNil)
					So(*mockDriver.receivedUpdate.State, ShouldEqual, *mesos.TASK_FAILED.Enum())
				})
			})

			// TODO: test exec.handleContainerLogs

			Convey("checks the container health via Sidecar", func() {
				dummyContainerLabels["SidecarDiscover"] = "true"
				taskInfo.Container.Docker.Parameters =
					labelsToDockerParams(dummyContainerLabels)
				exec.LaunchTask(&taskInfo)
				exec.watchLooper.Quit()
				err := exec.watchLooper.Wait()
				So(err, ShouldBeNil)
				So(sidecarStateCalls, ShouldEqual, 1)
			})

			Convey("fails to launch a task", func() {
				Convey("when it fails to pull an image", func() {
					dummyDockerClient.PullImageShouldError = true
					exec.LaunchTask(&taskInfo)

					So(mockDriver.isStopped, ShouldBeTrue)

					Convey("sends an update to Mesos", func() {
						So(mockDriver.receivedUpdate, ShouldNotBeNil)
						So(mockDriver.receivedUpdate.TaskID, ShouldNotBeNil)
						So(mockDriver.receivedUpdate.TaskID.Value, ShouldNotBeNil)
						So(mockDriver.receivedUpdate.TaskID.Value, ShouldEqual, dummyTaskIDValue)
						So(mockDriver.receivedUpdate.State, ShouldNotBeNil)
						So(*mockDriver.receivedUpdate.State, ShouldEqual, *mesos.TASK_FAILED.Enum())
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

			taskInfo.Container.Docker.Parameters = labelsToDockerParams(dummyContainerLabels)

			exec.watchLooper = director.NewFreeLooper(1, make(chan error))

			Convey("when draining the service", func() {
				go exec.monitorTask(dummyContainerId, &taskInfo, true)
				exec.KillTask(&dummyTaskID)
				So(sidecarDrainCalls, ShouldEqual, 1)

				Convey("sends an update to Mesos", func() {
					So(mockDriver.receivedUpdate, ShouldNotBeNil)
					So(mockDriver.receivedUpdate.TaskID, ShouldNotBeNil)
					So(mockDriver.receivedUpdate.TaskID.Value, ShouldNotBeNil)
					So(mockDriver.receivedUpdate.TaskID.Value, ShouldEqual, dummyTaskIDValue)
					So(mockDriver.receivedUpdate.State, ShouldNotBeNil)
					So(mockDriver.receivedUpdate.State, ShouldResemble, mesos.TASK_FINISHED.Enum())
				})

				Convey("stops the Mesos driver", func() {
					So(mockDriver.isStopped, ShouldBeTrue)
				})
			})

			Convey("notifies Sidecar to drain the service before stopping the watch looper", func() {
				doneChan := make(chan error)
				exec.watchLooper = director.NewFreeLooper(director.FOREVER, doneChan)
				// The looper needs to be looping something in order to unblock Wait() after a call to Quit()
				go exec.watchLooper.Loop(func() error { return nil })

				sidecarDrainChan = make(chan struct{})
				quitChan := make(chan struct{})
				go func() {
					exec.KillTask(&dummyTaskID)
					// Unblock this test
					close(quitChan)
				}()

				// Wait for exec.KillTask() to call exec.notifyDrain()
				// which will block waiting for us to close sidecarDrainChan
				<-sidecarDrainChan

				// Make sure exec.KillTask() didn't call exec.watchLooper.Quit() yet
				select {
				case <-doneChan:
					// Fail test, we seem to have stopped the watchLooper before draining the service
					// Also make sure we unblock service draininig to prevent Convey from waiting
					// forever while the httptest mux handler is blocked
					close(sidecarDrainChan)
					So(false, ShouldBeTrue)
				default:
				}

				// Unblock service draininig
				close(sidecarDrainChan)

				// Wait for exec.KillTask() to stop the watch looper
				So(exec.watchLooper.Wait(), ShouldBeNil)

				// Make sure we drained the service
				So(sidecarDrainCalls, ShouldEqual, 1)

				select {
				case <-quitChan:
					// All good, exec.KillTask() has finished successfully
				default:
					// Fail test, something is preventing exec.KillTask() from terminating
					So(false, ShouldBeTrue)
				}
			})

			Convey("stops draining the service if the container exits prematurely", func() {
				exec.config.SidecarDrainingDuration = 100 * time.Millisecond
				go exec.monitorTask(dummyContainerId, &taskInfo,
					shouldCheckSidecar(exec.containerConfig))

				exec.KillTask(&dummyTaskID)
				exec.watcherWg.Wait()
				// Just make sure we don't block
			})

			Convey("tries multiple times to drain the service", func() {
				exec.watcherWg.Add(1)
				exec.config.SidecarRetryCount = 1
				sidecarDrainFailOnce = true
				exec.KillTask(&dummyTaskID)
				exec.watcherWg.Done()
				So(sidecarDrainCalls, ShouldEqual, 2)
			})

			Convey("doesn't drain the the service when it has the label SidecarDiscover=false", func() {
				dummyContainerLabels["SidecarDiscover"] = "false"
				exec.KillTask(&dummyTaskID)
				So(sidecarDrainCalls, ShouldEqual, 0)
			})

			Convey("doesn't drain the service when the SidecarDrainingDuration config parameter is 0", func() {
				exec.config.SidecarDrainingDuration = 0
				exec.KillTask(&dummyTaskID)
				So(sidecarDrainCalls, ShouldEqual, 0)
			})

			Convey("Revokes AWS creds from Vault when have a lease", func() {
				exec.config.AWSRole = "valid-aws-role"
				taskInfo.Container.Docker.Parameters = labelsToDockerParams(dummyContainerLabels)

				// We'll use logging output to validate that the goroutine ran
				var capture bytes.Buffer
				log.SetLevel(log.DebugLevel)
				log.SetOutput(&capture)

				exec.LaunchTask(&taskInfo)
				exec.KillTask(&taskInfo.TaskID)
				exec.watcherWg.Wait()

				log.SetOutput(ioutil.Discard)

				// Make sure some things happened
				So(capture.String(), ShouldContainSubstring, "TEST success revoking AWS role")
			})

			Convey("Does NOT revoke AWS creds when we don't have a lease", func() {
				// We'll use logging output to validate that the goroutine ran
				var capture bytes.Buffer
				log.SetLevel(log.DebugLevel)
				log.SetOutput(&capture)

				exec.LaunchTask(&taskInfo)
				exec.KillTask(&taskInfo.TaskID)
				exec.watcherWg.Wait()

				log.SetOutput(ioutil.Discard)

				// Make sure some things happened
				So(capture.String(), ShouldContainSubstring, "No AWS lease to clean up, skipping")
			})

			Convey("Revoke a service-specific Vault token when AWS role is specified", func() {
				exec.config.AWSRole = "valid-aws-role"
				taskInfo.Container.Docker.Parameters = labelsToDockerParams(dummyContainerLabels)

				// We'll use logging output to validate that the goroutine ran
				var capture bytes.Buffer
				log.SetLevel(log.DebugLevel)
				log.SetOutput(&capture)

				exec.LaunchTask(&taskInfo)
				exec.KillTask(&taskInfo.TaskID)
				exec.watcherWg.Wait()

				log.SetOutput(ioutil.Discard)
				So(dummyVault.maybeRevokeTokenWasCalled, ShouldBeTrue)
			})

			Convey("Logs and error when AWS role is specified and we can't revoke our token", func() {
				exec.config.AWSRole = "valid-aws-role"
				taskInfo.Container.Docker.Parameters = labelsToDockerParams(dummyContainerLabels)

				// We'll use logging output to validate that the goroutine ran
				var capture bytes.Buffer
				log.SetLevel(log.DebugLevel)
				log.SetOutput(&capture)

				dummyVault.maybeRevokeTokenShouldError = true

				exec.LaunchTask(&taskInfo)
				exec.KillTask(&taskInfo.TaskID)
				exec.watcherWg.Wait()

				log.SetOutput(ioutil.Discard)
				So(dummyVault.maybeRevokeTokenWasCalled, ShouldBeTrue)
				So(capture.String(), ShouldContainSubstring, "Intentional test error")
			})
		})
	})
}
