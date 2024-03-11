package main

import (
	"context"
	"embed"
	"fmt"
	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"

	"github.com/codefly-dev/core/agents/communicate"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
)

type Builder struct {
	*Service
	NetworkMappings []*basev0.NetworkMapping
}

func NewBuilder() *Builder {
	return &Builder{
		Service: NewService(),
	}
}

func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return nil, err
	}

	requirements.Localize(s.Location)

	err = s.LoadEndpoints(ctx)
	if err != nil {
		return s.Builder.LoadError(err)
	}

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

	s.NetworkMappings = req.ProposedNetworkMappings

	s.DependencyEndpoints = req.DependenciesEndpoints

	return s.Builder.InitResponse(s.NetworkMappings, configurations.Unknown)
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

type Env struct {
	Key   string
	Value string
}

type DockerTemplating struct {
	ConnectionStringHolder string
}

func (s *Builder) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	s.Wool.Debug("building migration docker runnerImage")

	ctx = s.WoolAgent.Inject(ctx)

	image := s.DockerImage(req.BuildContext)

	if !dockerhelpers.IsValidDockerImageName(image.Name) {
		return s.Builder.BuildError(fmt.Errorf("invalid docker runnerImage name: %s", image.Name))
	}

	// We want the placeholder to match the environment variable for provider info for connection string
	info := &basev0.ProviderInformation{
		Name:   "postgres",
		Origin: s.Configuration.Unique(),
	}
	key := configurations.ProviderInformationEnvKey(info, "connection")

	docker := DockerTemplating{ConnectionStringHolder: key}

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
	return s.Builder.BuildResponse()
}

type Deployment struct {
	Replicas int
}

type DeploymentParameter struct {
	Image *configurations.DockerImage
	*services.Information
	Deployment
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()

	//deploy := DeploymentParameter{Image: s.DockerImage(), Information: s.Information, Deployment: Deployment{Replicas: 1}}
	//err := s.Templates(deploy,
	//	services.WithDeploymentFor(deployment, "kustomize/base", templates.WithOverrideAll()),
	//	services.WithDeploymentFor(deployment, "kustomize/overlays/environment",
	//		services.WithDestination("kustomize/overlays/%s", req.Environment.Name), templates.WithOverrideAll()),
	//)
	//if err != nil {
	//	return nil, err
	//}
	return &builderv0.DeploymentResponse{}, nil
}

const Watch = "watch"
const DatabaseName = "database-name"

func (s *Builder) createCommunicate() *communicate.Sequence {
	return communicate.NewSequence(
		communicate.NewConfirm(&agentv0.Message{Name: Watch, Message: "Migration hot-reload (Recommended)?", Description: "codefly can restart your database when migration changes detected 🔎"}, true),
		communicate.NewStringInput(&agentv0.Message{Name: DatabaseName, Message: "Name of the database?", Description: "Ensure encapsulation of your data"}, s.Configuration.Application),
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

	c := create{DatabaseName: s.Settings.DatabaseName, TableName: s.Configuration.Name}
	err = s.Templates(ctx, c, services.WithFactory(factoryFS))
	if err != nil {
		return s.Base.Builder.CreateError(err)
	}

	return s.Base.Builder.CreateResponse(ctx, s.Settings)
}

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/builder
var builderFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
