package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Nitro/sidecar-executor/container"
	docker "github.com/fsouza/go-dockerclient"
	log "github.com/sirupsen/logrus"
	. "github.com/smartystreets/goconvey/convey"
)

func Test_relayLogs(t *testing.T) {
	Convey("relayLogs()", t, func() {
		// Stub the docker client
		dockerClient := &container.MockDockerClient{
			LogOutputString: "this is some stdout text\n",
			LogErrorString:  "this is some stderr text\n",
		}

		config, err := initConfig()
		So(err, ShouldBeNil)
		exec := newSidecarExecutor(dockerClient, &docker.AuthConfiguration{}, config)
		exec.config.SendDockerLabels = []string{"Environment", "ServiceName"}

		// Stub out the fetcher
		fetcher := &mockFetcher{}
		exec.fetcher = fetcher

		// Capture logging output
		var result bytes.Buffer

		quitChan := make(chan struct{})

		Convey("handles both stderr and stdout", func() {
			// Janky that we have to sleep here, but not a good way to
			// sync on this.
			go func() { time.Sleep(1 * time.Millisecond); close(quitChan) }()

			exec.relayLogs(quitChan, "deadbeef123123123", map[string]string{}, &result)

			So(result.String(), ShouldContainSubstring, "some stdout text")
			So(result.String(), ShouldContainSubstring, "some stderr text")
		})

		Convey("includes the requested Docker labels", func() {
			// Janky that we have to sleep here, but not a good way to
			// sync on this.
			go func() { time.Sleep(1 * time.Millisecond); close(quitChan) }()

			labels := map[string]string{
				"Environment": "prod",
				"ServiceName": "beowulf",
			}

			exec.relayLogs(quitChan, "deadbeef123123123", labels, &result)
			exec.config.ContainerLogsStdout = true

			So(result.String(), ShouldContainSubstring, `"Environment":"prod"`)
			So(result.String(), ShouldContainSubstring, `"ServiceName":"beowulf"`)
		})
	})
}

func Test_handleOneStream(t *testing.T) {
	Convey("handleOneStream()", t, func() {
		fetcher := &mockFetcher{}
		client := &container.MockDockerClient{}
		config, err := initConfig()
		So(err, ShouldBeNil)
		exec := newSidecarExecutor(client, &docker.AuthConfiguration{}, config)
		exec.fetcher = fetcher

		quitChan := make(chan struct{})
		data := []byte("testing testing testing\n123\n456")

		reader := bytes.NewReader(data)
		var result bytes.Buffer

		logger := log.New()
		logger.SetOutput(&result)
		relay := logger.WithFields(log.Fields{"SomeTag": "test"})

		Reset(func() {
			result.Reset()
		})

		Convey("splits line appropriately", func() {
			// This test exist on EOF from the buffer
			var captured bytes.Buffer // System log, NOT logger
			log.SetOutput(&captured)
			exec.handleOneStream(quitChan, "stdout", relay, reader)

			So(result.String(), ShouldContainSubstring,
				`level=info msg="testing testing testing" SomeTag=test`)

			So(len(strings.Split(result.String(), "\n")), ShouldEqual, 4)
			So(captured.String(), ShouldNotContainSubstring, "error reading Docker")
		})

		Convey("errors out when the name is not stderr or stdout", func() {
			var captured bytes.Buffer // System log, NOT logger
			log.SetOutput(&captured)

			exec.handleOneStream(quitChan, "junk", relay, reader)

			So(captured.String(), ShouldContainSubstring, "Unknown stream type")
		})
	})
}
