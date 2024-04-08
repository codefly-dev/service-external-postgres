package main

import (
	"context"
	"embed"
	"fmt"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/templates"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"strings"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
	"github.com/codefly-dev/core/shared"
)

// Agent version
var agent = shared.Must(configurations.LoadFromFs[configurations.Agent](shared.Embed(infoFS)))

var requirements = builders.NewDependencies(agent.Name,
	builders.NewDependency("service.codefly.yaml"),
	builders.NewDependency("migrations", "migrations").WithPathSelect(shared.NewSelect("*.sql")),
)

type Settings struct {
	Debug bool `yaml:"debug"` // Developer only

	DatabaseName string `yaml:"database-name"`
	WithoutSSL   bool   `yaml:"without-ssl"` // Default to SSL

	Watch   bool `yaml:"watch"`
	Silent  bool `yaml:"silent"`
	Persist bool `yaml:"persist"`

	NoMigration bool `yaml:"no-migration"` // Developer only
}

var runnerImage = &configurations.DockerImage{Name: "postgres", Tag: "16.1"}

type Service struct {
	*services.Base

	// Settings
	*Settings

	connectionKey string
	connection    string

	tcpEndpoint *basev0.Endpoint
}

func (s *Service) GetAgentInformation(ctx context.Context, _ *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {

	readme, err := templates.ApplyTemplateFrom(ctx, shared.Embed(readmeFS), "templates/agent/README.md", s.Information)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &agentv0.AgentInformation{
		RuntimeRequirements: []*agentv0.Runtime{},
		Capabilities: []*agentv0.Capability{
			{Type: agentv0.Capability_BUILDER},
			{Type: agentv0.Capability_RUNTIME},
		},
		Protocols: []*agentv0.Protocol{},
		ConfigurationDetails: []*agentv0.ConfigurationValueDetail{
			{
				Name: "postgres", Description: "postgres credentials",
				Fields: []*agentv0.ConfigurationValueInformation{
					{
						Name: "connection", Description: "connection string",
					},
				}},
		},
		ReadMe: readme,
	}, nil
}

func NewService() *Service {
	return &Service{
		Base:     services.NewServiceBase(context.Background(), agent.Of(configurations.ServiceAgent)),
		Settings: &Settings{},
	}
}
func (s *Service) getUserPassword(ctx context.Context, conf *basev0.Configuration) (string, string, error) {
	user, err := configurations.GetConfigurationValue(ctx, conf, "postgres", "POSTGRES_USER")
	if err != nil {
		return "", "", s.Wool.Wrapf(err, "cannot get user")
	}
	password, err := configurations.GetConfigurationValue(ctx, conf, "postgres", "POSTGRES_PASSWORD")
	if err != nil {
		return "", "", s.Wool.Wrapf(err, "cannot get password")
	}
	return user, password, nil

}

func (s *Service) createConnectionString(ctx context.Context, conf *basev0.Configuration, address string, withSSL bool) (string, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	user, password, err := s.getUserPassword(ctx, conf)
	if err != nil {
		return "", s.Wool.Wrapf(err, "cannot get user and password")
	}

	conn := fmt.Sprintf("postgresql://%s:%s@%s/%s", user, password, address, s.DatabaseName)
	if !withSSL || strings.Contains(address, "localhost") || strings.Contains(address, "host.docker.internal") {
		conn += "?sslmode=disable"
	}
	return conn, nil
}

func (s *Service) CreateConnectionConfiguration(ctx context.Context, conf *basev0.Configuration, instance *basev0.NetworkInstance, withSSL bool) (*basev0.Configuration, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	connection, err := s.createConnectionString(ctx, conf, instance.Address, withSSL)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create connection string")
	}

	outputConf := &basev0.Configuration{
		Origin: s.Base.Service.Unique(),
		Scope:  instance.Scope,
		Configurations: []*basev0.ConfigurationInformation{
			{Name: "postgres",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "connection", Value: connection, Secret: true},
				},
			},
		},
	}
	return outputConf, nil
}

func main() {
	agents.Register(
		services.NewServiceAgent(agent.Of(configurations.ServiceAgent), NewService()),
		services.NewBuilderAgent(agent.Of(configurations.RuntimeServiceAgent), NewBuilder()),
		services.NewRuntimeAgent(agent.Of(configurations.BuilderServiceAgent), NewRuntime()))
}

//go:embed agent.codefly.yaml
var infoFS embed.FS

//go:embed templates/agent
var readmeFS embed.FS
