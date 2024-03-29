package main

import (
	"context"
	"embed"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/templates"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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

func (s *Service) CreateConnectionString(ctx context.Context, address string, withoutSSL bool) error {
	//user, err := s.EnvironmentVariables.GetServiceProvider(ctx, s.Unique(), "postgres", "POSTGRES_USER")
	//if err != nil {
	//	return s.Wool.Wrapf(err, "cannot get user")
	//}
	//password, err := s.EnvironmentVariables.GetServiceProvider(ctx, s.Unique(), "postgres", "POSTGRES_PASSWORD")
	//if err != nil {
	//	return s.Wool.Wrapf(err, "cannot get password")
	//}
	//connection := fmt.Sprintf("postgresql://%s:%s@%s/%s", user, password, address, s.DatabaseName)
	//if withoutSSL {
	//	connection += "?sslmode=disable"
	//}
	//s.connection = connection
	return nil
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
