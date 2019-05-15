package container

import (
	"errors"

	"github.com/fsouza/go-dockerclient"
)

type MockDockerClient struct {
	ValidOptions                bool
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
	ListContainersShouldError   bool
	ListContainersContainers    []docker.APIContainers
	ContainerStarted            bool
}

func (m *MockDockerClient) PullImage(opts docker.PullImageOptions, auth docker.AuthConfiguration) error {
	if m.PullImageShouldError {
		return errors.New("Something went wrong! [PullImage()]")
	}

	if len(opts.Repository) > 5 && (docker.AuthConfiguration{}) == auth {
		m.ValidOptions = true
	}

	return nil
}

func (m *MockDockerClient) ListImages(opts docker.ListImagesOptions) ([]docker.APIImages, error) {
	if m.ListImagesShouldError {
		return nil, errors.New("Something went wrong! [ListImages()]")
	}
	return m.Images, nil
}

func (m *MockDockerClient) StopContainer(id string, timeout uint) error {
	if m.StopContainerShouldError {
		m.stopContainerFails += 1

		if m.stopContainerFails > m.StopContainerMaxFails {
			return errors.New("Something went wrong! [StopContainer()]")
		}
	}
	return nil
}

func (m *MockDockerClient) InspectContainer(id string) (*docker.Container, error) {
	if m.InspectContainerShouldError {
		return nil, errors.New("Something went wrong! [InspectContainer()]")
	}

	if m.Container != nil {
		return m.Container, nil
	}

	return nil, errors.New("Forgot to set the mock container! [InspectContainer()]")
}

func (m *MockDockerClient) Logs(opts docker.LogsOptions) error {
	m.logOpts = &opts

	_, err := opts.OutputStream.Write([]byte(m.LogOutputString))
	if err != nil {
		return err
	}

	_, err = opts.ErrorStream.Write([]byte(m.LogErrorString))
	if err != nil {
		return err
	}

	return nil
}

func (m *MockDockerClient) CreateContainer(opts docker.CreateContainerOptions) (*docker.Container, error) {
	return &docker.Container{ID: opts.Name}, nil
}

func (m *MockDockerClient) StartContainer(id string, hostConfig *docker.HostConfig) error {
	m.ContainerStarted = true
	return nil
}

func (m *MockDockerClient) ListContainers(opts docker.ListContainersOptions) ([]docker.APIContainers, error) {
	if m.ListContainersShouldError {
		return nil, errors.New("Something went wrong! [ListContainers()]")
	}

	if opts.All {
		return m.ListContainersContainers, nil
	}

	// Simulate the non-All option
	var containers []docker.APIContainers
	for _, container := range m.ListContainersContainers {
		if container.State != "exited" {
			containers = append(containers, container)
		}
	}
	return containers, nil
}
