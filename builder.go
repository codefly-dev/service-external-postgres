package main

import (
	"context"
	"embed"
	"fmt"
	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	"github.com/codefly-dev/core/configurations"
	v0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/agents/communicate"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"

	"github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
)

type Builder struct {
	*Service
}

func NewBuilder() *Builder {
	return &Builder{
		Service: NewService(),
	}
}

func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()

	ctx = s.Wool.Inject(ctx)

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return nil, err
	}

	s.EnvironmentVariables.SetEnvironment(s.Environment)

	s.Wool.Debug("base loaded", wool.Field("identity", s.Identity))

	requirements.Localize(s.Location)

	s.Endpoints, err = s.Builder.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Builder.LoadError(err)
	}

	s.tcpEndpoint, err = configurations.FindTCPEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Builder.LoadError(err)
	}

	s.Wool.Debug("endpoint", wool.Field("tcp", s.tcpEndpoint))

	gettingStarted, err := templates.ApplyTemplateFrom(ctx, shared.Embed(factoryFS), "templates/factory/GETTING_STARTED.md", s.Information)
	if err != nil {
		return nil, err
	}

	// communication on CreateResponse
	err = s.Communication.Register(ctx, communicate.New[builderv0.CreateRequest](s.createCommunicate()))
	if err != nil {
		return s.Builder.LoadError(err)
	}

	return s.Builder.LoadResponse(gettingStarted)
}

func (s *Builder) Init(ctx context.Context, req *builderv0.InitRequest) (*builderv0.InitResponse, error) {
	defer s.Wool.Catch()

	return s.Builder.InitResponse()
}

func (s *Builder) Update(ctx context.Context, req *builderv0.UpdateRequest) (*builderv0.UpdateResponse, error) {
	defer s.Wool.Catch()

	return &builderv0.UpdateResponse{}, nil
}

func (s *Builder) Sync(ctx context.Context, req *builderv0.SyncRequest) (*builderv0.SyncResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	return s.Builder.SyncResponse()
}

type DockerTemplating struct {
	ConnectionStringKeyHolder string
}

func (s *Builder) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	defer s.Wool.Catch()

	s.Wool.Debug("building migration docker runnerImage")

	ctx = s.Wool.Inject(ctx)

	image := s.DockerImage(req.BuildContext)

	if !dockerhelpers.IsValidDockerImageName(image.Name) {
		return s.Builder.BuildError(fmt.Errorf("invalid docker runnerImage name: %s", image.Name))
	}

	connectionKey := configurations.ServiceSecretConfigurationKey(s.Base.Service, "postgres", "connection")
	docker := DockerTemplating{ConnectionStringKeyHolder: fmt.Sprintf("${%s}", connectionKey)}

	err := shared.DeleteFile(ctx, s.Local("builder/Dockerfile"))
	if err != nil {
		return s.Builder.BuildError(err)
	}

	err = s.Templates(ctx, docker, services.WithBuilder(builderFS))
	if err != nil {
		return s.Builder.BuildError(err)
	}

	builder, err := dockerhelpers.NewBuilder(dockerhelpers.BuilderConfiguration{
		Root:        s.Location,
		Dockerfile:  "builder/Dockerfile",
		Destination: image,
		Output:      s.Wool,
	})
	if err != nil {
		return s.Builder.BuildError(err)
	}
	_, err = builder.Build(ctx)
	if err != nil {
		return s.Builder.BuildError(err)
	}

	s.Builder.WithDockerImages(image)

	return s.Builder.BuildResponse()
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()

	s.Builder.LogDeployRequest(req, s.Wool.Debug)

	s.EnvironmentVariables.SetRunning(true)

	instance, err := configurations.FindNetworkInstance(ctx, req.NetworkMappings, s.tcpEndpoint, v0.NetworkScope_External)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	s.Wool.Debug("network instance", wool.Field("instance", instance))

	s.LogForward("will run on localhost:%d", instance.Port)

	s.Configuration = req.Configuration

	conf, err := s.CreateConnectionConfiguration(ctx, instance, !s.Settings.WithoutSSL)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	err = s.EnvironmentVariables.AddConfigurations(conf)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	s.Configuration = conf

	cm, err := services.EnvsAsConfigMapData(s.EnvironmentVariables.Configurations()...)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	secrets, err := services.EnvsAsSecretData(s.EnvironmentVariables.Secrets()...)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	params := services.DeploymentParameters{
		ConfigMap: cm,
		SecretMap: secrets,
	}

	err = s.Builder.GenericServiceDeploy(ctx, req, deploymentFS, params)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	return s.Builder.DeployResponse()
}

const Watch = "watch"
const DatabaseName = "database-name"

func (s *Builder) createCommunicate() *communicate.Sequence {
	return communicate.NewSequence(
		communicate.NewConfirm(&agentv0.Message{Name: Watch, Message: "Migration hot-reload (Recommended)?", Description: "codefly can restart your database when migration changes detected 🔎"}, true),
		communicate.NewStringInput(&agentv0.Message{Name: DatabaseName, Message: "Name of the database?", Description: "Ensure encapsulation of your data"}, s.Builder.Service.Application),
	)
}

type create struct {
	DatabaseName string
	TableName    string
}

func (s *Builder) Create(ctx context.Context, req *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()

	session, err := s.Communication.Done(ctx, communicate.Channel[builderv0.CreateRequest]())
	if err != nil {
		return s.Builder.CreateError(err)
	}

	s.Settings.DatabaseName, err = session.GetInputString(DatabaseName)
	if err != nil {
		return s.Builder.CreateError(err)
	}

	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create endpoints")
	}

	c := create{DatabaseName: s.Settings.DatabaseName, TableName: s.Builder.Service.Name}
	err = s.Templates(ctx, c, services.WithFactory(factoryFS))
	if err != nil {
		return s.Base.Builder.CreateError(err)
	}

	s.Endpoints = []*v0.Endpoint{
		{
			Name:       "tcp",
			Visibility: configurations.VisibilityExternal,
		},
	}

	return s.Base.Builder.CreateResponse(ctx, s.Settings)
}

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/builder
var builderFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
