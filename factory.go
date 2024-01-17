package main

import (
	"context"
	"embed"

	"github.com/codefly-dev/core/agents/communicate"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	factoryv0 "github.com/codefly-dev/core/generated/go/services/factory/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
)

type Factory struct {
	*Service
}

func NewFactory() *Factory {
	return &Factory{
		Service: NewService(),
	}
}

func (s *Factory) Load(ctx context.Context, req *factoryv0.LoadRequest) (*factoryv0.LoadResponse, error) {
	defer s.Wool.Catch()

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return nil, err
	}

	requirements.WithDir(s.Location)

	err = s.LoadEndpoints(ctx)
	if err != nil {
		return s.Factory.LoadError(err)
	}

	gettingStarted, err := templates.ApplyTemplateFrom(shared.Embed(factory), "templates/factory/GETTING_STARTED.md", s.Information)
	if err != nil {
		return nil, err
	}

	// communication on CreateResponse
	err = s.Communication.Register(ctx, communicate.New[factoryv0.CreateRequest](s.createCommunicate()))
	if err != nil {
		return s.Factory.LoadError(err)
	}

	return s.Factory.LoadResponse(gettingStarted)
}

const Watch = "watch"
const DatabaseName = "database-name"

func (s *Factory) createCommunicate() *communicate.Sequence {
	return communicate.NewSequence(
		communicate.NewConfirm(&agentv0.Message{Name: Watch, Message: "Migration hot-reload (Recommended)?", Description: "codefly can restart your database when migration changes detected 🔎"}, true),
		communicate.NewStringInput(&agentv0.Message{Name: DatabaseName, Message: "Name of the database?", Description: "Ensure encapsulation of your data"}, s.Configuration.Application),
	)
}

type create struct {
	DatabaseName string
	TableName    string
}

func (s *Factory) Create(ctx context.Context, req *factoryv0.CreateRequest) (*factoryv0.CreateResponse, error) {
	defer s.Wool.Catch()

	session, err := s.Communication.Done(ctx, communicate.Channel[factoryv0.CreateRequest]())
	if err != nil {
		return s.Factory.CreateError(err)
	}

	s.Settings.DatabaseName, err = session.GetInputString(DatabaseName)
	if err != nil {
		return s.Factory.CreateError(err)
	}

	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create endpoints")
	}

	err = s.Templates(ctx, create{DatabaseName: s.Settings.DatabaseName, TableName: s.Configuration.Name}, services.WithFactory(factory))
	if err != nil {
		return s.Base.Factory.CreateError(err)
	}

	return s.Base.Factory.CreateResponse(ctx, s.Settings, s.Endpoints...)
}

func (s *Factory) Init(ctx context.Context, req *factoryv0.InitRequest) (*factoryv0.InitResponse, error) {
	defer s.Wool.Catch()

	s.DependencyEndpoints = req.DependenciesEndpoints

	return s.Factory.InitResponse(configurations.Unknown)
}

func (s *Factory) Update(ctx context.Context, req *factoryv0.UpdateRequest) (*factoryv0.UpdateResponse, error) {
	defer s.Wool.Catch()

	return &factoryv0.UpdateResponse{}, nil
}

func (s *Factory) Sync(ctx context.Context, req *factoryv0.SyncRequest) (*factoryv0.SyncResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	return &factoryv0.SyncResponse{}, nil
}

type Env struct {
	Key   string
	Value string
}

type DockerTemplating struct {
	Envs []Env
}

func (s *Factory) Build(ctx context.Context, req *factoryv0.BuildRequest) (*factoryv0.BuildResponse, error) {
	s.Wool.Debug("building Migration docker image")

	// We want to use DNS to create NetworkMapping
	//networkMapping, err := s.Network(req.DependenciesEndpoints)
	//if err != nil {
	//	return nil, s.Wool.Wrapf(err, "cannot create network mapping")
	//}
	//config, err := s.createConfig(ctx, networkMapping)
	//if err != nil {
	//	return nil, s.Wool.Wrapf(err, "cannot write config")
	//}
	//
	//target := s.Local("codefly/builder/settings/routing.json")
	//err = os.WriteFile(target, config, 0o644)
	//if err != nil {
	//	return nil, s.Wool.Wrapf(err, "cannot write settings to %s", target)
	//}
	//
	//err = os.Remove(s.Local("codefly/builder/Dockerfile"))
	//if err != nil {
	//	return nil, s.Wool.Wrapf(err, "cannot remove dockerfile")
	//}
	//err = s.Templates(nil, services.WithBuilder(builder))
	//if err != nil {
	//	return nil, s.Wool.Wrapf(err, "cannot copy and apply template")
	//}
	//builder, err := dockerhelpers.NewBuilder(dockerhelpers.BuilderConfiguration{
	//	Root:       s.Location,
	//	Dockerfile: "codefly/builder/Dockerfile",
	//	Image:      s.DockerImage().Name,
	//	Tag:        s.DockerImage().Tag,
	//})
	//if err != nil {
	//	return nil, s.Wool.Wrapf(err, "cannot create builder")
	//}
	//// builder.WithLogger(s.Wool)
	//_, err = builder.Build(ctx)
	//if err != nil {
	//	return nil, s.Wool.Wrapf(err, "cannot build image")
	//}
	return &factoryv0.BuildResponse{}, nil
}

type Deployment struct {
	Replicas int
}

type DeploymentParameter struct {
	Image *configurations.DockerImage
	*services.Information
	Deployment
}

func (s *Factory) Deploy(ctx context.Context, req *factoryv0.DeploymentRequest) (*factoryv0.DeploymentResponse, error) {
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
	return &factoryv0.DeploymentResponse{}, nil
}

func (s *Factory) Network(es []*basev0.Endpoint) ([]*runtimev0.NetworkMapping, error) {
	return nil, nil
	//s.DebugMe("in network: %v", configurations.Condensed(es))
	//pm, err := network.NewServiceDnsManager(ctx, s.Identity)
	//if err != nil {
	//	return nil, s.Wool.Wrapf(err, "cannot create network manager")
	//}
	//for _, endpoint := range es {
	//	err = pm.Expose(endpoint)
	//	if err != nil {
	//		return nil, s.Wool.Wrapf(err, "cannot add grpc endpoint to network manager")
	//	}
	//}
	//err = pm.Reserve()
	//if err != nil {
	//	return nil, s.Wool.Wrapf(err, "cannot reserve ports")
	//}
	//return pm.NetworkMapping()
}

//go:embed templates/factory
var factory embed.FS

//go:embed templates/builder
var builder embed.FS

//go:embed templates/deployment
var deployment embed.FS
