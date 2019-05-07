package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Nitro/sidecar-executor/container"
	"github.com/fsouza/go-dockerclient"
	mesos "github.com/mesos/mesos-go/api/v0/mesosproto"
	director "github.com/relistan/go-director"
	. "github.com/smartystreets/goconvey/convey"
)

type dummyMesosDriver struct {
	isStopped      bool
	receivedUpdate *mesos.TaskStatus
}

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

func Test_KillTask(t *testing.T) {
	Convey("sidecarExecutor.KillTask()", t, func(c C) {
		dummyTask := "dummy_task"
		dummyContainer := "123456654321"

		sidecarCalls := 0
		sidecarFailOnce := false
		fakeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.So(r.URL.Path, ShouldEqual, fmt.Sprintf("/api/services/%s/drain", dummyContainer))

			sidecarCalls++

			if sidecarFailOnce && sidecarCalls == 1 {
				http.Error(w, "Kaboom!", 500)
				return
			}

			w.WriteHeader(202)
		}))
		Reset(func() {
			fakeServer.Close()
		})

		watchLooper := director.NewFreeLooper(1, make(chan error))
		mesosDriver := dummyMesosDriver{}
		containerLabels := map[string]string{"SidecarDiscover": "true"}
		exec := sidecarExecutor{
			client: &container.MockDockerClient{
				Container: &docker.Container{
					State: docker.State{Status: "exited"},
				},
			},
			driver:      &mesosDriver,
			watchLooper: watchLooper,
			fetcher:     http.DefaultClient,
			config: Config{
				SidecarUrl:              fakeServer.URL + "/services.json",
				SidecarDrainingDuration: 1 * time.Millisecond,
			},
			containerID: dummyContainer,
			containerConfig: &docker.CreateContainerOptions{
				Config: &docker.Config{
					Labels: containerLabels,
				},
			},
		}

		Convey("drains the service", func() {
			exec.KillTask(exec.driver, &mesos.TaskID{Value: &dummyTask})
			So(sidecarCalls, ShouldEqual, 1)

			Convey("and sends an update to Mesos", func() {
				So(mesosDriver.receivedUpdate, ShouldNotBeNil)
				So(mesosDriver.receivedUpdate.TaskId, ShouldNotBeNil)
				So(mesosDriver.receivedUpdate.TaskId.Value, ShouldNotBeNil)
				So(*mesosDriver.receivedUpdate.TaskId.Value, ShouldEqual, dummyTask)
				So(mesosDriver.receivedUpdate.State, ShouldNotBeNil)
				So(*mesosDriver.receivedUpdate.State, ShouldEqual, *mesos.TaskState_TASK_FINISHED.Enum())
			})

			Convey("and stops the Mesos driver", func() {
				So(mesosDriver.isStopped, ShouldBeTrue)
			})
		})

		Convey("tries multiple times to drain the service", func() {
			exec.config.SidecarRetryCount = 1
			sidecarFailOnce = true
			exec.KillTask(exec.driver, &mesos.TaskID{Value: &dummyTask})
			So(sidecarCalls, ShouldEqual, 2)
		})

		Convey("doesn't drain the the service when it has the label SidecarDiscover=false", func() {
			containerLabels["SidecarDiscover"] = "false"
			exec.KillTask(exec.driver, &mesos.TaskID{Value: &dummyTask})
			So(sidecarCalls, ShouldEqual, 0)
		})

		Convey("doesn't drain the service when the SidecarDrainingDuration config parameter is 0", func() {
			exec.config.SidecarDrainingDuration = 0
			exec.KillTask(exec.driver, &mesos.TaskID{Value: &dummyTask})
			So(sidecarCalls, ShouldEqual, 0)
		})

		Convey("doesn't drain the service when the SidecarDrainingDuration label is 0", func() {
			containerLabels["SidecarDrainingDuration"] = "0"
			exec.KillTask(exec.driver, &mesos.TaskID{Value: &dummyTask})
			So(sidecarCalls, ShouldEqual, 0)
		})
	})
}
