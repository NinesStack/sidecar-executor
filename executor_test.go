package main

import (
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Nitro/sidecar-executor/container"
	"github.com/fsouza/go-dockerclient"
	mesos "github.com/mesos/mesos-go/api/v1/lib"
	"github.com/relistan/go-director"
	log "github.com/sirupsen/logrus"
	. "github.com/smartystreets/goconvey/convey"
)

type mockFetcher struct {
	ShouldFail    bool
	ShouldError   bool
	ShouldBadJson bool
	callCount     int
}

func (m *mockFetcher) Get(url string) (*http.Response, error) {
	m.callCount += 1

	if m.ShouldBadJson {
		return m.badJson()
	}

	if m.ShouldError {
		return nil, errors.New("OMG something went horribly wrong!")
	}

	// Mesos master
	if strings.Contains(url, "mesos-master") {
		return httpResponse(200,
			`{"slaves":[{"hostname": "bede"},{"hostname":"chaucer"}]}`,
		), nil
	}

	// Mesos worker
	if strings.Contains(url, "mesos-worker") {
		return httpResponse(200,
			`{"master_hostname":"mesos-master"}`,
		), nil
	}

	// Sidecar
	if m.ShouldFail {
		return m.failedRequest()
	} else {
		return m.successRequest()
	}
}

func (m *mockFetcher) Post(url string, contentType string, body io.Reader) (*http.Response, error) {
	return nil, nil
}

func (m *mockFetcher) successRequest() (*http.Response, error) {
	return httpResponse(200, `
		{
			"Servers": {
				"roncevalles": {
					"Services": {
						"deadbeef0010": {
							"ID": "deadbeef0010",
							"Status": 0
						}
					}
				}
			}
		}
	`), nil
}

func (m *mockFetcher) badJson() (*http.Response, error) {
	return httpResponse(200, `OMG invalid JSON`), nil
}

func (m *mockFetcher) failedRequest() (*http.Response, error) {
	return httpResponse(500, `
		{
			"Servers": {
				"roncevalles": {
					"Services": {
						"deadbeef0010": {
							"ID": "deadbeef0010",
							"Status": 1
						},
						"running00010": {
							"ID": "running00010",
							"Status": 1
						}
					}
				}
			}
		}
	`), nil
}

func httpResponse(status int, bodyStr string) *http.Response {
	body := bytes.NewBuffer([]byte(bodyStr))

	return &http.Response{
		Status:        strconv.Itoa(status),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Body:          ioutil.NopCloser(body),
		ContentLength: int64(body.Len()),
	}
}

func Test_sidecarStatus(t *testing.T) {
	Convey("When handling Sidecar status", t, func() {
		log.SetOutput(ioutil.Discard) // Don't show logged errors/warnings/etc
		os.Setenv("TASK_HOST", "roncevalles")
		fetcher := &mockFetcher{}

		client := &container.MockDockerClient{}
		exec := newSidecarExecutor(client, &docker.AuthConfiguration{}, Config{})
		exec.fetcher = fetcher

		Convey("return healthy on HTTP request errors", func() {
			fetcher.ShouldError = true

			So(exec.sidecarStatus("deadbeef0010"), ShouldBeNil)
			So(exec.failCount, ShouldEqual, 0)
		})

		Convey("retries as expected", func() {
			fetcher.ShouldError = true
			exec.config.SidecarRetryCount = 5

			So(exec.sidecarStatus("deadbeef0010"), ShouldBeNil)
			So(fetcher.callCount, ShouldEqual, 6) // 1 try + (5 retries)
		})

		Convey("healthy on JSON parse errors", func() {
			fetcher.ShouldBadJson = true
			So(exec.sidecarStatus("deadbeef0010"), ShouldBeNil)
			So(exec.failCount, ShouldEqual, 0)
		})

		Convey("errors when it can talk to Sidecar and fail count is exceeded", func() {
			fetcher.ShouldFail = true

			exec.config.SidecarMaxFails = 3
			exec.failCount = 3

			result := exec.sidecarStatus("deadbeef0010")
			So(result, ShouldNotBeNil)
			So(result.Error(), ShouldContainSubstring, "deadbeef0010 failing task!")
			So(exec.failCount, ShouldEqual, 0) // Gets reset!
		})

		Convey("healthy when it can talk to Sidecar and fail count is below limit", func() {
			fetcher.ShouldFail = true

			exec.config.SidecarMaxFails = 3
			exec.failCount = 1

			result := exec.sidecarStatus("deadbeef0010")
			So(result, ShouldBeNil)
			So(exec.failCount, ShouldEqual, 2)
		})

		Convey("resets failCount on first healthy response", func() {
			fetcher.ShouldFail = true

			exec.config.SidecarMaxFails = 3
			exec.failCount = 1

			result := exec.sidecarStatus("deadbeef0010")
			So(result, ShouldBeNil)
			So(exec.failCount, ShouldEqual, 2)

			// Get a healthy response, reset the counter
			fetcher.ShouldFail = false
			result = exec.sidecarStatus("deadbeef0010")
			So(result, ShouldBeNil)
			So(exec.failCount, ShouldEqual, 0)
		})

		Convey("healthy when the host doesn't exist in Sidecar", func() {
			os.Setenv("TASK_HOST", "zaragoza")
			fetcher.ShouldError = false

			So(exec.sidecarStatus("deadbeef0010"), ShouldBeNil)
			So(exec.failCount, ShouldEqual, 0)
		})
	})
}

func Test_logConfig(t *testing.T) {
	// We want to make sure we don't forget to print settings when they get added
	Convey("Logs all the config settings", t, func() {
		output := bytes.NewBuffer([]byte{})

		os.Setenv("MESOS_LEGEND", "roncevalles")

		config, err := initConfig()
		So(err, ShouldBeNil)

		log.SetOutput(output) // Capture the output
		logConfig(config)

		v := reflect.ValueOf(config)
		for i := 0; i < v.NumField(); i++ {
			So(output.String(), ShouldContainSubstring, v.Type().Field(i).Name)
		}

		So(output.String(), ShouldContainSubstring, "roncevalles")
	})
}

func Test_logTaskEnv(t *testing.T) {
	Convey("Logging Docker task env vars", t, func() {
		output := bytes.NewBuffer([]byte{})
		log.SetOutput(output) // Capture the output
		fetcher := &mockFetcher{}
		client := &container.MockDockerClient{}
		exec := newSidecarExecutor(client, &docker.AuthConfiguration{}, Config{})
		exec.fetcher = fetcher

		taskInfo := &mesos.TaskInfo{
			TaskID: mesos.TaskID{Value: "my-task-id"},
			Container: &mesos.ContainerInfo{
				Docker: &mesos.ContainerInfo_DockerInfo{
					Parameters: []mesos.Parameter{
						{
							Key:   "env",
							Value: "BOCACCIO=author",
						},
					},
				},
			},
		}

		Convey("dumps the vars it finds", func() {
			exec.logTaskEnv(taskInfo, container.LabelsForTask(taskInfo), []string{})

			So(output.String(), ShouldContainSubstring, "--------")
			So(output.String(), ShouldContainSubstring, "BOCACCIO=author")
		})

		Convey("has environment and service name if defined", func() {
			taskInfo.Container.Docker.Parameters = []mesos.Parameter{
				{
					Key:   "label",
					Value: "ServiceName=test-service",
				},
				{
					Key:   "label",
					Value: "EnvironmentName=dev",
				},
			}

			exec.logTaskEnv(taskInfo, container.LabelsForTask(taskInfo), []string{})

			So(output.String(), ShouldContainSubstring, "SERVICE_NAME=test-service")
			So(output.String(), ShouldContainSubstring, "ENVIRONMENT_NAME=dev")
		})

		Convey("leaves environment and service undefined if no labels are set", func() {
			taskInfo.Container.Docker.Parameters = []mesos.Parameter{}

			exec.logTaskEnv(taskInfo, container.LabelsForTask(taskInfo), []string{})

			So(output.String(), ShouldNotContainSubstring, "SERVICE_NAME=")
			So(output.String(), ShouldNotContainSubstring, "ENVIRONMENT_NAME=")
		})

		Convey("leaves version unset if it can't be parsed", func() {
			taskInfo.Container.Docker.Image = "test-service"
			exec.logTaskEnv(taskInfo, container.LabelsForTask(taskInfo), []string{})

			So(output.String(), ShouldNotContainSubstring, "SERVICE_VERSION=")
		})

		Convey("shows added env vars", func() {
			exec.logTaskEnv(taskInfo, container.LabelsForTask(taskInfo), []string{"ADDED_VAR=true"})
			So(output.String(), ShouldContainSubstring, "ADDED_VAR=")
		})

		Convey("adds Sidecar seeds", func() {
			os.Setenv("MESOS_AGENT_ENDPOINT", "mesos-worker:5050")
			exec.config.SeedSidecar = true
			addEnvVars := exec.addSidecarSeeds([]string{})
			exec.logTaskEnv(taskInfo, container.LabelsForTask(taskInfo), addEnvVars)
			So(output.String(), ShouldContainSubstring, "SIDECAR_SEEDS=bede,chaucer")
		})
	})
}

func Test_watchContainer(t *testing.T) {
	Convey("When watching the container", t, func() {
		client := &container.MockDockerClient{}
		config, err := initConfig()
		So(err, ShouldBeNil)
		config.SidecarBackoff = time.Duration(0)    // Don't wait to start health checking
		config.SidecarRetryDelay = time.Duration(0) // Sidecar status should fail if ever checked
		log.SetOutput(ioutil.Discard)               // Has to be here to take effect. Can't be higher up
		exec := newSidecarExecutor(client, &docker.AuthConfiguration{}, config)

		resultChan := make(chan error, 5)
		exec.watchLooper = director.NewFreeLooper(1, resultChan)

		exec.failCount = exec.config.SidecarMaxFails
		os.Setenv("TASK_HOST", "roncevalles")
		exec.fetcher = &mockFetcher{
			ShouldFail: true,
		}

		client.ListContainersContainers = []docker.APIContainers{
			{
				ID:    "deadbeef0010",
				State: "exited",
			},
			{
				ID:    "running00010",
				State: "running",
			},
		}

		client.Container = &docker.Container{
			State: docker.State{
				Status: "exited",
			},
		}

		Convey("returns an error when ListContainers fails", func() {
			client.ListContainersShouldError = true
			exec.watchContainer("deadbeef0010", true)

			err := <-resultChan
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "[ListContainers()]")
		})

		Convey("returns an error when the container doesn't exist", func() {
			client.Container = nil

			exec.watchContainer("missingbeef0010", true)

			err := <-resultChan
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "Container missingbeef0010 not found!")
		})

		Convey("returns an error when the container exists but has exited with errors", func() {
			client.Container.State.ExitCode = 1

			exec.watchContainer("deadbeef0010", true)

			err := <-resultChan
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "Container deadbeef0010 not running!")
		})

		Convey("returns without errors when the container exists and has exited without errors", func() {
			client.Container.State.ExitCode = 0

			exec.watchContainer("deadbeef0010", true)

			err := <-resultChan
			So(err, ShouldBeNil) // Container stopped without errors
		})

		Convey("check Sidecar status for a running container with SidecarDiscover: true", func() {
			exec.watchContainer("running00010", true)

			err := <-resultChan
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "Unhealthy container: running00010 failing task!")
		})

		Convey("don't check Sidecar for a running container with SidecarDiscover: false", func() {
			exec.watchContainer("running00010", false)

			err := <-resultChan
			So(err, ShouldBeNil) // Container running, Sidecar no checked.
		})
	})
}
