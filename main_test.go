package main

import (
	"bytes"
	"errors"
	"io/ioutil"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/fsouza/go-dockerclient"
	"github.com/nitro/sidecar-executor/container"
	"github.com/relistan/go-director"
	. "github.com/smartystreets/goconvey/convey"
)

type mockFetcher struct {
	ShouldFail    bool
	ShouldError   bool
	ShouldBadJson bool
	callCount     int
}

func (m *mockFetcher) Get(string) (*http.Response, error) {
	m.callCount += 1

	if m.ShouldBadJson {
		return m.badJson()
	}

	if m.ShouldError {
		return nil, errors.New("OMG something went horribly wrong!")
	}

	if m.ShouldFail {
		return m.failedRequest()
	} else {
		return m.successRequest()
	}
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
		exec := newSidecarExecutor(client, &docker.AuthConfiguration{})
		exec.fetcher = fetcher

		config.SidecarRetryDelay = 0
		config.SidecarRetryCount = 0

		Convey("return healthy on HTTP request errors", func() {
			fetcher.ShouldError = true

			So(exec.sidecarStatus("deadbeef0010"), ShouldBeNil)
			So(exec.failCount, ShouldEqual, 0)
		})

		Convey("retries as expected", func() {
			fetcher.ShouldError = true
			config.SidecarRetryCount = 5

			exec.sidecarStatus("deadbeef0010")
			So(fetcher.callCount, ShouldEqual, 6) // 1 try + (5 retries)
		})

		Convey("healthy on JSON parse errors", func() {
			fetcher.ShouldBadJson = true
			So(exec.sidecarStatus("deadbeef0010"), ShouldBeNil)
			So(exec.failCount, ShouldEqual, 0)
		})

		Convey("errors when it can talk to Sidecar and fail count is exceeded", func() {
			fetcher.ShouldFail = true

			config.SidecarMaxFails = 3
			exec.failCount = 3

			result := exec.sidecarStatus("deadbeef0010")
			So(result, ShouldNotBeNil)
			So(result.Error(), ShouldContainSubstring, "deadbeef0010 failing task!")
			So(exec.failCount, ShouldEqual, 0) // Gets reset!
		})

		Convey("healthy when it can talk to Sidecar and fail count is below limit", func() {
			fetcher.ShouldFail = true

			config.SidecarMaxFails = 3
			exec.failCount = 1

			result := exec.sidecarStatus("deadbeef0010")
			So(result, ShouldBeNil)
			So(exec.failCount, ShouldEqual, 2)
		})

		Convey("resets failCount on first healthy response", func() {
			fetcher.ShouldFail = true

			config.SidecarMaxFails = 3
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

		log.SetOutput(output) // Don't show the output
		logConfig()

		v := reflect.ValueOf(config)
		for i := 0; i < v.NumField(); i++ {
			So(string(output.Bytes()), ShouldContainSubstring, v.Type().Field(i).Name)
		}
	})
}

func Test_watchContainer(t *testing.T) {
	Convey("When watching the container", t, func() {
		config.SidecarBackoff = time.Duration(0) // Don't wait to start health checking
		client := &container.MockDockerClient{}
		exec := newSidecarExecutor(client, &docker.AuthConfiguration{})

		resultChan := make(chan error, 5)
		exec.watchLooper = director.NewFreeLooper(1, resultChan)

		Convey("returns an error when ListContainers fails", func() {
			client.ListContainersShouldError = true
			exec.watchContainer("deadbeef0010")

			err := <-resultChan
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "[ListContainers()]")
		})

		Convey("returns an error when the container doesn't exist", func() {
			exec.watchContainer("deadbeef0010")

			err := <-resultChan
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "Container deadbeef0010 not running!")
		})
	})
}
