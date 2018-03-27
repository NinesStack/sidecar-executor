package main

import (
	"testing"
	. "github.com/smartystreets/goconvey/convey"
	"github.com/fsouza/go-dockerclient"
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
			containerOptions.Config.Labels = map[string]string{"SidecarDiscover" : "true"}

			So(shouldCheckSidecar(containerOptions), ShouldBeTrue)
		})

		Convey("shouldCheckSidecar should be false when SidecarDiscover=false", func() {
			containerOptions.Config.Labels = map[string]string{"SidecarDiscover" : "false"}

			So(shouldCheckSidecar(containerOptions), ShouldBeFalse)
		})
	})
}
