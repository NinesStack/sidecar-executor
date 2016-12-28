package container

import (
	"errors"

	"github.com/fsouza/go-dockerclient"
)

type MockDockerClient struct {
	validOptions                bool
	PullImageShouldError        bool
	Images                      []docker.APIImages
	ListImagesShouldError       bool
	StopContainerShouldError    bool
	stopContainerFails          int
	StopContainerMaxFails       int
	InspectContainerShouldError bool
	logOpts                     *docker.LogsOptions
	Container                   *docker.Container
	LogOutputString             string
	LogErrorString              string
}

func (m *MockDockerClient) PullImage(opts docker.PullImageOptions, auth docker.AuthConfiguration) error {
	if m.PullImageShouldError {
		return errors.New("Something went wrong!")
	}

	if len(opts.Repository) > 5 && (docker.AuthConfiguration{}) == auth {
		m.validOptions = true
	}

	return nil
}

func (m *MockDockerClient) ListImages(opts docker.ListImagesOptions) ([]docker.APIImages, error) {
	if m.ListImagesShouldError {
		return nil, errors.New("Something went wrong!")
	}
	return m.Images, nil
}

func (m *MockDockerClient) StopContainer(id string, timeout uint) error {
	if m.StopContainerShouldError {
		m.stopContainerFails += 1

		if m.stopContainerFails > m.StopContainerMaxFails {
			return errors.New("Something went wrong!")
		}
	}
	return nil
}

func (m *MockDockerClient) InspectContainer(id string) (*docker.Container, error) {
	if m.InspectContainerShouldError {
		return nil, errors.New("Something went wrong!")
	}

	if m.Container != nil {
		return m.Container, nil
	}

	return nil, errors.New("Forgot to set the mock container!")
}

func (m *MockDockerClient) Logs(opts docker.LogsOptions) error {
	m.logOpts = &opts
	opts.OutputStream.Write([]byte(m.LogOutputString))
	opts.ErrorStream.Write([]byte(m.LogErrorString))

	return nil
}

func (m *MockDockerClient) CreateContainer(opts docker.CreateContainerOptions) (*docker.Container, error) {
	return nil, nil
}

func (m *MockDockerClient) StartContainer(id string, hostConfig *docker.HostConfig) error {
	return nil
}

func (m *MockDockerClient) ListContainers(opts docker.ListContainersOptions) ([]docker.APIContainers, error) {
	return nil, nil
}
