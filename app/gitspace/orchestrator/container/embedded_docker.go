// Copyright 2023 Harness, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package container

import (
	"context"
	"fmt"
	"io"

	"github.com/harness/gitness/app/gitspace/logutil"
	"github.com/harness/gitness/infraprovider"
	"github.com/harness/gitness/types"
	"github.com/harness/gitness/types/enum"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/rs/zerolog/log"
)

var _ Orchestrator = (*EmbeddedDockerOrchestrator)(nil)

const (
	loggingKey             = "gitspace.container"
	catchAllIP             = "0.0.0.0"
	catchAllPort           = "0"
	containerStateRunning  = "running"
	containerStateRemoved  = "removed"
	containerStateStopped  = "exited"
	templateCloneGit       = "clone_git.sh"
	templateSetupSSHServer = "setup_ssh_server.sh"
)

type Config struct {
	DefaultBaseImage string
}

type EmbeddedDockerOrchestrator struct {
	dockerClientFactory *infraprovider.DockerClientFactory
	vsCodeService       *VSCode
	vsCodeWebService    *VSCodeWeb
	config              *Config
	statefulLogger      *logutil.StatefulLogger
}

func NewEmbeddedDockerOrchestrator(
	dockerClientFactory *infraprovider.DockerClientFactory,
	vsCodeService *VSCode,
	vsCodeWebService *VSCodeWeb,
	config *Config,
	statefulLogger *logutil.StatefulLogger,
) Orchestrator {
	return &EmbeddedDockerOrchestrator{
		dockerClientFactory: dockerClientFactory,
		vsCodeService:       vsCodeService,
		vsCodeWebService:    vsCodeWebService,
		config:              config,
		statefulLogger:      statefulLogger,
	}
}

// CreateAndStartGitspace starts an exited container and starts a new container if the container is removed.
// If the container is newly created, it clones the code, sets up the IDE and executes the postCreateCommand.
// It returns the container ID, name and ports used.
// It returns an error if the container is not running, exited or removed.
func (e *EmbeddedDockerOrchestrator) CreateAndStartGitspace(
	ctx context.Context,
	gitspaceConfig *types.GitspaceConfig,
	devcontainerConfig *types.DevcontainerConfig,
	infra *infraprovider.Infrastructure,
	repoName string,
) (*StartResponse, error) {
	containerName := getGitspaceContainerName(gitspaceConfig)

	log := log.Ctx(ctx).With().Str(loggingKey, containerName).Logger()

	dockerClient, err := e.dockerClientFactory.NewDockerClient(ctx, infra)
	if err != nil {
		return nil, fmt.Errorf("error getting docker client from docker client factory: %w", err)
	}

	defer func() {
		closingErr := dockerClient.Close()
		if closingErr != nil {
			log.Warn().Err(closingErr).Msg("failed to close docker client")
		}
	}()

	log.Debug().Msg("checking current state of gitspace")
	state, err := e.containerState(ctx, containerName, dockerClient)
	if err != nil {
		return nil, err
	}

	ideService, err := e.getIDEService(gitspaceConfig)
	if err != nil {
		return nil, err
	}

	switch state {
	case containerStateRunning:
		log.Debug().Msg("gitspace is already running")

	case containerStateStopped:
		log.Debug().Msg("gitspace is stopped, starting it")

		logStreamInstance, loggerErr := e.statefulLogger.CreateLogStream(ctx, gitspaceConfig.ID)
		if loggerErr != nil {
			return nil, fmt.Errorf("error getting log stream for gitspace ID %d: %w", gitspaceConfig.ID, loggerErr)
		}

		defer func() {
			loggerErr = logStreamInstance.Flush()
			if loggerErr != nil {
				log.Warn().Err(loggerErr).Msgf("failed to flush log stream for gitspace ID %d", gitspaceConfig.ID)
			}
		}()

		startErr := e.startContainer(ctx, dockerClient, containerName, logStreamInstance)
		if startErr != nil {
			return nil, startErr
		}

		devcontainer := &Devcontainer{
			ContainerName: containerName,
			WorkingDir:    e.getWorkingDir(repoName),
			DockerClient:  dockerClient,
		}

		err = e.runIDE(ctx, devcontainer, ideService, logStreamInstance)
		if err != nil {
			return nil, err
		}

		// TODO: Add gitspace status reporting.
		log.Debug().Msg("started gitspace")

	case containerStateRemoved:
		log.Debug().Msg("gitspace is removed, creating it...")

		logStreamInstance, loggerErr := e.statefulLogger.CreateLogStream(ctx, gitspaceConfig.ID)
		if loggerErr != nil {
			return nil, fmt.Errorf("error getting log stream for gitspace ID %d: %w", gitspaceConfig.ID, loggerErr)
		}

		defer func() {
			loggerErr = logStreamInstance.Flush()
			if loggerErr != nil {
				log.Warn().Err(loggerErr).Msgf("failed to flush log stream for gitspace ID %d", gitspaceConfig.ID)
			}
		}()

		startErr := e.startGitspace(
			ctx,
			gitspaceConfig,
			devcontainerConfig,
			containerName,
			dockerClient,
			ideService,
			logStreamInstance,
			infra.Storage,
			e.getWorkingDir(repoName),
		)
		if startErr != nil {
			return nil, fmt.Errorf("failed to start gitspace %s: %w", containerName, startErr)
		}

		// TODO: Add gitspace status reporting.
		log.Debug().Msg("started gitspace")

	default:
		return nil, fmt.Errorf("gitspace %s is in a bad state: %s", containerName, state)
	}

	id, ports, startErr := e.getContainerInfo(ctx, containerName, dockerClient, ideService)
	if startErr != nil {
		return nil, startErr
	}

	return &StartResponse{
		ContainerID:   id,
		ContainerName: containerName,
		PortsUsed:     ports,
	}, nil
}

func (e *EmbeddedDockerOrchestrator) getWorkingDir(repoName string) string {
	return "/" + repoName
}

func (e *EmbeddedDockerOrchestrator) startGitspace(
	ctx context.Context,
	gitspaceConfig *types.GitspaceConfig,
	devcontainerConfig *types.DevcontainerConfig,
	containerName string,
	dockerClient *client.Client,
	ideService IDE,
	logStreamInstance *logutil.LogStreamInstance,
	volumeName string,
	workingDirectory string,
) error {
	var imageName = devcontainerConfig.Image
	if imageName == "" {
		imageName = e.config.DefaultBaseImage
	}

	err := e.pullImage(ctx, imageName, dockerClient, logStreamInstance)
	if err != nil {
		return err
	}

	err = e.createContainer(
		ctx,
		dockerClient,
		imageName,
		containerName,
		ideService,
		logStreamInstance,
		volumeName,
		workingDirectory,
	)
	if err != nil {
		return err
	}

	err = e.startContainer(ctx, dockerClient, containerName, logStreamInstance)
	if err != nil {
		return err
	}

	var devcontainer = &Devcontainer{
		ContainerName: containerName,
		DockerClient:  dockerClient,
		WorkingDir:    workingDirectory,
	}

	err = e.setupIDE(ctx, gitspaceConfig.GitspaceInstance, devcontainer, ideService, logStreamInstance)
	if err != nil {
		return err
	}

	err = e.runIDE(ctx, devcontainer, ideService, logStreamInstance)
	if err != nil {
		return err
	}

	err = e.cloneCode(ctx, gitspaceConfig, devcontainer, logStreamInstance)
	if err != nil {
		return err
	}

	err = e.executePostCreateCommand(ctx, devcontainerConfig, devcontainer, logStreamInstance)
	if err != nil {
		return err
	}

	return nil
}

// TODO: Instead of explicitly running IDE related processes, we can explore service to run the service on boot.

func (e *EmbeddedDockerOrchestrator) runIDE(
	ctx context.Context,
	devcontainer *Devcontainer,
	ideService IDE,
	logStreamInstance *logutil.LogStreamInstance,
) error {
	loggingErr := logStreamInstance.Write("Running the IDE inside container: " + string(ideService.Type()))
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	output, err := ideService.Run(ctx, devcontainer)
	if err != nil {
		loggingErr = logStreamInstance.Write("Error while running IDE inside container: " + err.Error())

		err = fmt.Errorf("failed to run the IDE for gitspace %s: %w", devcontainer.ContainerName, err)

		if loggingErr != nil {
			err = fmt.Errorf("original error: %w; logging error: %w", err, loggingErr)
		}

		return err
	}

	loggingErr = logStreamInstance.Write("IDE run output...\n" + string(output))
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	loggingErr = logStreamInstance.Write("Successfully run the IDE inside container")
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	return nil
}

func (e *EmbeddedDockerOrchestrator) setupIDE(
	ctx context.Context,
	gitspaceInstance *types.GitspaceInstance,
	devcontainer *Devcontainer,
	ideService IDE,
	logStreamInstance *logutil.LogStreamInstance,
) error {
	loggingErr := logStreamInstance.Write("Setting up IDE inside container: " + string(ideService.Type()))
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	output, err := ideService.Setup(ctx, devcontainer, gitspaceInstance)
	if err != nil {
		loggingErr = logStreamInstance.Write("Error while setting up IDE inside container: " + err.Error())

		err = fmt.Errorf("failed to setup IDE for gitspace %s: %w", devcontainer.ContainerName, err)

		if loggingErr != nil {
			err = fmt.Errorf("original error: %w; logging error: %w", err, loggingErr)
		}

		return err
	}

	loggingErr = logStreamInstance.Write("IDE setup output...\n" + string(output))
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	loggingErr = logStreamInstance.Write("Successfully set up IDE inside container")
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	return nil
}

func (e *EmbeddedDockerOrchestrator) getContainerInfo(
	ctx context.Context,
	containerName string,
	dockerClient *client.Client,
	ideService IDE,
) (string, map[enum.IDEType]string, error) {
	inspectResp, err := dockerClient.ContainerInspect(ctx, containerName)
	if err != nil {
		return "", nil, fmt.Errorf("could not inspect container %s: %w", containerName, err)
	}

	usedPorts := map[enum.IDEType]string{}
	for port, bindings := range inspectResp.NetworkSettings.Ports {
		if port == nat.Port(ideService.PortAndProtocol()) {
			usedPorts[ideService.Type()] = bindings[0].HostPort
		}
	}

	return inspectResp.ID, usedPorts, nil
}

func (e *EmbeddedDockerOrchestrator) getIDEService(gitspaceConfig *types.GitspaceConfig) (IDE, error) {
	var ideService IDE

	switch gitspaceConfig.IDE {
	case enum.IDETypeVSCode:
		ideService = e.vsCodeService
	case enum.IDETypeVSCodeWeb:
		ideService = e.vsCodeWebService
	default:
		return nil, fmt.Errorf("unsupported IDE: %s", gitspaceConfig.IDE)
	}

	return ideService, nil
}

func (e *EmbeddedDockerOrchestrator) cloneCode(
	ctx context.Context,
	gitspaceConfig *types.GitspaceConfig,
	devcontainer *Devcontainer,
	logStreamInstance *logutil.LogStreamInstance,
) error {
	gitCloneScript, err := GenerateScriptFromTemplate(
		templateCloneGit, &CloneGitPayload{
			RepoURL: gitspaceConfig.CodeRepoURL,
			Image:   e.config.DefaultBaseImage,
			Branch:  gitspaceConfig.Branch,
		})
	if err != nil {
		return fmt.Errorf("failed to generate scipt to clone git from template %s: %w", templateCloneGit, err)
	}

	loggingErr := logStreamInstance.Write(
		"Cloning git repo inside container: " + gitspaceConfig.CodeRepoURL + " branch: " + gitspaceConfig.Branch)
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	output, err := devcontainer.ExecuteCommand(ctx, gitCloneScript, false)
	if err != nil {
		loggingErr = logStreamInstance.Write("Error while cloning git repo inside container: " + err.Error())

		err = fmt.Errorf("failed to clone code: %w", err)

		if loggingErr != nil {
			err = fmt.Errorf("original error: %w; logging error: %w", err, loggingErr)
		}

		return err
	}

	loggingErr = logStreamInstance.Write("Cloning git repo output...\n" + string(output))
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	loggingErr = logStreamInstance.Write("Successfully cloned git repo inside container")
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	return nil
}

func (e *EmbeddedDockerOrchestrator) executePostCreateCommand(
	ctx context.Context,
	devcontainerConfig *types.DevcontainerConfig,
	devcontainer *Devcontainer,
	logStreamInstance *logutil.LogStreamInstance,
) error {
	if devcontainerConfig.PostCreateCommand == "" {
		loggingErr := logStreamInstance.Write("No post-create command provided, skipping execution")
		if loggingErr != nil {
			return fmt.Errorf("logging error: %w", loggingErr)
		}

		return nil
	}

	loggingErr := logStreamInstance.Write("Executing postCreate command: " + devcontainerConfig.PostCreateCommand)
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	output, err := devcontainer.ExecuteCommand(ctx, devcontainerConfig.PostCreateCommand, false)
	if err != nil {
		loggingErr = logStreamInstance.Write("Error while executing postCreate command")

		err = fmt.Errorf("failed to execute postCreate command %q: %w", devcontainerConfig.PostCreateCommand, err)

		if loggingErr != nil {
			err = fmt.Errorf("original error: %w; logging error: %w", err, loggingErr)
		}

		return err
	}

	loggingErr = logStreamInstance.Write("Post create command execution output...\n" + string(output))
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	loggingErr = logStreamInstance.Write("Successfully executed postCreate command")
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	return nil
}

func (e *EmbeddedDockerOrchestrator) startContainer(
	ctx context.Context,
	dockerClient *client.Client,
	containerName string,
	logStreamInstance *logutil.LogStreamInstance,
) error {
	loggingErr := logStreamInstance.Write("Starting container: " + containerName)
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	err := dockerClient.ContainerStart(ctx, containerName, dockerTypes.ContainerStartOptions{})
	if err != nil {
		loggingErr = logStreamInstance.Write("Error while creating container: " + err.Error())

		err = fmt.Errorf("could not start container %s: %w", containerName, err)

		if loggingErr != nil {
			err = fmt.Errorf("original error: %w; logging error: %w", err, loggingErr)
		}

		return err
	}

	loggingErr = logStreamInstance.Write("Successfully started container")
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	return nil
}

func (e *EmbeddedDockerOrchestrator) createContainer(
	ctx context.Context,
	dockerClient *client.Client,
	imageName string,
	containerName string,
	ideService IDE,
	logStreamInstance *logutil.LogStreamInstance,
	volumeName string,
	workingDirectory string,
) error {
	portUsedByIDE := ideService.PortAndProtocol()

	hostPortBindings := []nat.PortBinding{
		{
			HostIP:   catchAllIP,
			HostPort: catchAllPort,
		},
	}

	exposedPorts := nat.PortSet{}
	portBindings := nat.PortMap{}

	if portUsedByIDE != "" {
		natPort := nat.Port(portUsedByIDE)
		exposedPorts[natPort] = struct{}{}
		portBindings[natPort] = hostPortBindings
	}

	loggingErr := logStreamInstance.Write("Creating container: " + containerName)
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	_, err := dockerClient.ContainerCreate(ctx, &container.Config{
		Image:        imageName,
		Entrypoint:   []string{"/bin/sh"},
		Cmd:          []string{"-c", "trap 'exit 0' 15; sleep infinity & wait $!"},
		ExposedPorts: exposedPorts,
	}, &container.HostConfig{
		PortBindings: portBindings,
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Source: volumeName,
				Target: workingDirectory,
			},
		},
	}, nil, containerName)
	if err != nil {
		loggingErr = logStreamInstance.Write("Error while creating container: " + err.Error())

		err = fmt.Errorf("could not create container %s: %w", containerName, err)

		if loggingErr != nil {
			err = fmt.Errorf("original error: %w; logging error: %w", err, loggingErr)
		}

		return err
	}

	return nil
}

func (e *EmbeddedDockerOrchestrator) pullImage(
	ctx context.Context,
	imageName string,
	dockerClient *client.Client,
	logStreamInstance *logutil.LogStreamInstance,
) error {
	loggingErr := logStreamInstance.Write("Pulling image: " + imageName)
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	pullResponse, err := dockerClient.ImagePull(ctx, imageName, dockerTypes.ImagePullOptions{})

	defer func() {
		closingErr := pullResponse.Close()
		if closingErr != nil {
			log.Warn().Err(closingErr).Msg("failed to close image pull response")
		}
	}()

	if err != nil {
		loggingErr = logStreamInstance.Write("Error while pulling image: " + err.Error())

		err = fmt.Errorf("could not pull image %s: %w", imageName, err)

		if loggingErr != nil {
			err = fmt.Errorf("original error: %w; logging error: %w", err, loggingErr)
		}

		return err
	}

	// NOTE: It is necessary to read all the data in pullResponse to ensure the image has been completely downloaded.
	// If the execution proceeds before the response is completed, the container will not find the required image.
	output, err := io.ReadAll(pullResponse)
	if err != nil {
		loggingErr = logStreamInstance.Write("Error while parsing image pull response: " + err.Error())

		err = fmt.Errorf("error while parsing pull image output %s: %w", imageName, err)

		if loggingErr != nil {
			err = fmt.Errorf("original error: %w; logging error: %w", err, loggingErr)
		}

		return err
	}

	loggingErr = logStreamInstance.Write(string(output))
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	loggingErr = logStreamInstance.Write("Successfully pulled image")
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	return nil
}

// StopGitspace stops a container. If it is removed, it returns an error.
func (e EmbeddedDockerOrchestrator) StopGitspace(
	ctx context.Context,
	gitspaceConfig *types.GitspaceConfig,
	infra *infraprovider.Infrastructure,
) error {
	containerName := getGitspaceContainerName(gitspaceConfig)

	log := log.Ctx(ctx).With().Str(loggingKey, containerName).Logger()

	dockerClient, err := e.dockerClientFactory.NewDockerClient(ctx, infra)
	if err != nil {
		return fmt.Errorf("error getting docker client from docker client factory: %w", err)
	}

	defer func() {
		closingErr := dockerClient.Close()
		if closingErr != nil {
			log.Warn().Err(closingErr).Msg("failed to close docker client")
		}
	}()

	log.Debug().Msg("checking current state of gitspace")
	state, err := e.containerState(ctx, containerName, dockerClient)
	if err != nil {
		return err
	}

	if state == containerStateRemoved {
		return fmt.Errorf("gitspace %s is removed", containerName)
	}

	if state == containerStateStopped {
		log.Debug().Msg("gitspace is already stopped")
		return nil
	}

	log.Debug().Msg("stopping gitspace")

	logStreamInstance, loggerErr := e.statefulLogger.CreateLogStream(ctx, gitspaceConfig.ID)
	if loggerErr != nil {
		return fmt.Errorf("error getting log stream for gitspace ID %d: %w", gitspaceConfig.ID, loggerErr)
	}

	defer func() {
		loggerErr = logStreamInstance.Flush()
		if loggerErr != nil {
			log.Warn().Err(loggerErr).Msgf("failed to flush log stream for gitspace ID %d", gitspaceConfig.ID)
		}
	}()

	err = e.stopContainer(ctx, containerName, dockerClient, logStreamInstance)
	if err != nil {
		return fmt.Errorf("failed to stop gitspace %s: %w", containerName, err)
	}

	log.Debug().Msg("stopped gitspace")

	return nil
}

func (e EmbeddedDockerOrchestrator) stopContainer(
	ctx context.Context,
	containerName string,
	dockerClient *client.Client,
	logStreamInstance *logutil.LogStreamInstance,
) error {
	loggingErr := logStreamInstance.Write("Stopping container: " + containerName)
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	err := dockerClient.ContainerStop(ctx, containerName, nil)
	if err != nil {
		loggingErr = logStreamInstance.Write("Error while stopping container: " + err.Error())

		err = fmt.Errorf("could not stop container %s: %w", containerName, err)

		if loggingErr != nil {
			err = fmt.Errorf("original error: %w; logging error: %w", err, loggingErr)
		}

		return err
	}

	loggingErr = logStreamInstance.Write("Successfully stopped container")
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	return nil
}

func getGitspaceContainerName(config *types.GitspaceConfig) string {
	return "gitspace-" + config.UserID + "-" + config.Identifier
}

// Status is NOOP for EmbeddedDockerOrchestrator as the docker host is verified by the infra provisioner.
func (e *EmbeddedDockerOrchestrator) Status(_ context.Context, _ *infraprovider.Infrastructure) error {
	return nil
}

func (e *EmbeddedDockerOrchestrator) containerState(
	ctx context.Context,
	containerName string,
	dockerClient *client.Client,
) (string, error) {
	var args = filters.NewArgs()
	args.Add("name", containerName)

	containers, err := dockerClient.ContainerList(ctx, dockerTypes.ContainerListOptions{All: true, Filters: args})
	if err != nil {
		return "", fmt.Errorf("could not list container %s: %w", containerName, err)
	}

	if len(containers) == 0 {
		return containerStateRemoved, nil
	}

	return containers[0].State, nil
}

// StopAndRemoveGitspace stops the container if not stopped and removes it.
// If the container is already removed, it returns.
func (e *EmbeddedDockerOrchestrator) StopAndRemoveGitspace(
	ctx context.Context,
	gitspaceConfig *types.GitspaceConfig,
	infra *infraprovider.Infrastructure,
) error {
	containerName := getGitspaceContainerName(gitspaceConfig)

	log := log.Ctx(ctx).With().Str(loggingKey, containerName).Logger()

	dockerClient, err := e.dockerClientFactory.NewDockerClient(ctx, infra)
	if err != nil {
		return fmt.Errorf("error getting docker client from docker client factory: %w", err)
	}

	defer func() {
		closingErr := dockerClient.Close()
		if closingErr != nil {
			log.Warn().Err(closingErr).Msg("failed to close docker client")
		}
	}()

	log.Debug().Msg("checking current state of gitspace")
	state, err := e.containerState(ctx, containerName, dockerClient)
	if err != nil {
		return err
	}

	if state == containerStateRemoved {
		log.Debug().Msg("gitspace is already removed")
		return nil
	}

	logStreamInstance, loggerErr := e.statefulLogger.CreateLogStream(ctx, gitspaceConfig.ID)
	if loggerErr != nil {
		return fmt.Errorf("error getting log stream for gitspace ID %d: %w", gitspaceConfig.ID, loggerErr)
	}

	defer func() {
		loggerErr = logStreamInstance.Flush()
		if loggerErr != nil {
			log.Warn().Err(loggerErr).Msgf("failed to flush log stream for gitspace ID %d", gitspaceConfig.ID)
		}
	}()

	if state != containerStateStopped {
		log.Debug().Msg("stopping gitspace")

		err = e.stopContainer(ctx, containerName, dockerClient, logStreamInstance)
		if err != nil {
			return fmt.Errorf("failed to stop gitspace %s: %w", containerName, err)
		}

		log.Debug().Msg("stopped gitspace")
	}

	log.Debug().Msg("removing gitspace")

	err = e.removeContainer(ctx, containerName, dockerClient, logStreamInstance)
	if err != nil {
		return fmt.Errorf("failed to remove gitspace %s: %w", containerName, err)
	}

	log.Debug().Msg("removed gitspace")

	return nil
}

func (e EmbeddedDockerOrchestrator) removeContainer(
	ctx context.Context,
	containerName string,
	dockerClient *client.Client,
	logStreamInstance *logutil.LogStreamInstance,
) error {
	loggingErr := logStreamInstance.Write("Removing container: " + containerName)
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	err := dockerClient.ContainerRemove(ctx, containerName, dockerTypes.ContainerRemoveOptions{Force: true})
	if err != nil {
		loggingErr = logStreamInstance.Write("Error while removing container: " + err.Error())

		err = fmt.Errorf("could not remove container %s: %w", containerName, err)

		if loggingErr != nil {
			err = fmt.Errorf("original error: %w; logging error: %w", err, loggingErr)
		}

		return err
	}

	loggingErr = logStreamInstance.Write("Successfully removed container")
	if loggingErr != nil {
		return fmt.Errorf("logging error: %w", loggingErr)
	}

	return nil
}
