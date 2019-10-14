package mesosdriver

import (
	"testing"

	mesos "github.com/mesos/mesos-go/api/v1/lib"
	"github.com/mesos/mesos-go/api/v1/lib/executor/config"
	. "github.com/smartystreets/goconvey/convey"
)


type MockDelegate struct{}

func (m *MockDelegate) LaunchTask(taskInfo *mesos.TaskInfo) {}
func (m *MockDelegate) KillTask(taskID *mesos.TaskID)       {}

func Test_NewExecutorDriver(t *testing.T) {
	Convey("NewExecutorDriver()", t, func() {
		mesosConfig := &config.Config{}
		mockDelegate := &MockDelegate{}

		Convey("Builds a properly configured driver", func() {
			driver := NewExecutorDriver(mesosConfig, mockDelegate)

			So(driver, ShouldNotBeNil)
			So(driver.cli, ShouldNotBeNil)
			So(driver.unackedTasks, ShouldNotBeNil)
			So(driver.unackedUpdates, ShouldNotBeNil)
			So(driver.failedTasks, ShouldNotBeNil)
			So(driver.delegate, ShouldEqual, mockDelegate)
			So(driver.cfg, ShouldEqual, mesosConfig)
			So(driver.subscriber, ShouldNotBeNil)
		})
	})
}

func Test_NewStatus(t *testing.T) {
	Convey("NewStatus()", t, func() {
		mesosConfig := &config.Config{}
		mockDelegate := &MockDelegate{}
		driver := NewExecutorDriver(mesosConfig, mockDelegate)

		taskID := mesos.TaskID{Value: "kingarthur"}

		Convey("Builds a properly configured TaskStatus", func() {
			status := driver.NewStatus(taskID)

			So(status, ShouldNotBeNil)
			So(status.TaskID, ShouldResemble, taskID)
			So(status.Source, ShouldNotBeNil)
			So(status.ExecutorID, ShouldNotBeNil)
			So(status.UUID, ShouldNotBeEmpty)
		})
	})
}
