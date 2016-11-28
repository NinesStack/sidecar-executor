package container

import (
	"testing"

	"github.com/fsouza/go-dockerclient"
	mesos "github.com/mesos/mesos-go/mesosproto"
	. "github.com/smartystreets/goconvey/convey"
)

type mockDockerClient struct {
	ValidOptions bool
}

func (m *mockDockerClient) PullImage(opts docker.PullImageOptions, auth docker.AuthConfiguration) error {
	if len(opts.Repository) > 5 && (docker.AuthConfiguration{}) == auth {
		m.ValidOptions = true
	}

	return nil
}

func Test_PullImage(t *testing.T) {
	Convey("PullImage() passes the right params", t, func() {
		image := "foo/foo:foo"
		taskInfo := &mesos.TaskInfo{
			Container: &mesos.ContainerInfo{
				Docker: &mesos.ContainerInfo_DockerInfo{
					Image: &image,
				},
			},
		}

		dockerClient := &mockDockerClient{}

		PullImage(dockerClient, taskInfo)

		So(dockerClient.ValidOptions, ShouldBeTrue)
	})
}
