package main

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/codefly-dev/core/shared"
	"strings"
	"time"

	"github.com/codefly-dev/core/agents/helpers/code"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
	"github.com/codefly-dev/core/runners"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/lib/pq"
)

type Runtime struct {
	*Service

	// internal
	runner               *runners.Docker
	EnvironmentVariables *configurations.EnvironmentVariableManager

	Port            int
	NetworkMappings []*basev0.NetworkMapping
	createDataFirst bool
	connection      string
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
}

var runnerImage = &configurations.DockerImage{Name: "postgres", Tag: "16.1"}

const (
	DefaultPostgresUser     = "postgres"
	DefaultPostgresPassword = "password"
)

func (s *Runtime) CreateConnectionString(ctx context.Context, address string, withoutSSL bool) error {
	user, err := s.EnvironmentVariables.GetServiceProvider(ctx, s.Unique(), "postgres", "POSTGRES_USER")
	if err != nil {
		s.Wool.Warn("using default user")
		user = DefaultPostgresUser
	}
	password, err := s.EnvironmentVariables.GetServiceProvider(ctx, s.Unique(), "postgres", "POSTGRES_PASSWORD")
	if err != nil {
		s.Wool.Warn("using default")
		password = DefaultPostgresPassword
	}
	connection := fmt.Sprintf("postgresql://%s:%s@%s/%s", user, password, address, s.DatabaseName)
	if withoutSSL {
		connection += "?sslmode=disable"
	}
	s.connection = connection
	return nil
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	requirements.Localize(s.Location)

	err = s.LoadEndpoints(ctx)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	s.EnvironmentVariables = configurations.NewEnvironmentVariableManager()

	return s.Base.Runtime.LoadResponse()
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.NetworkMappings = req.ProposedNetworkMappings

	s.Wool.Focus("info", wool.Field("info", configurations.MakeProviderInformationSummary(req.ProviderInfos)))

	net, err := configurations.GetMappingInstance(s.NetworkMappings)
	if err != nil {
		return s.Runtime.InitError(err)
	}
	s.LogForward("will run on: %s", net.Address)

	// for docker version
	s.Port = 5432

	for _, providerInfo := range req.ProviderInfos {
		s.EnvironmentVariables.Add(configurations.ProviderInformationAsEnvironmentVariables(providerInfo)...)
	}

	err = s.CreateConnectionString(ctx, net.Address, true)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	// This is the credential exposed to dependencies
	providerInfo := &basev0.ProviderInformation{Name: "postgres", Origin: s.Service.Configuration.Unique(), Data: map[string]string{"connection": s.connection}}

	s.Wool.Debug("init", wool.NullableField("provider", providerInfo))

	// Docker
	runner, err := runners.NewDocker(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.runner = runner
	s.runner.WithOut(s.Wool)
	s.runner.WithPort(runners.DockerPortMapping{Container: s.Port, Host: net.Port})
	s.runner.WithEnvironmentVariables(s.EnvironmentVariables.GetBase()...)
	s.runner.WithEnvironmentVariables(fmt.Sprintf("POSTGRES_DB=%s", s.DatabaseName))

	// Persist data
	if s.Settings.Persist {
		exists, err := shared.CheckDirectoryOrCreate(ctx, s.Local("data"))
		if err != nil {
			return s.Runtime.InitError(err)
		}
		s.createDataFirst = !exists
		s.runner.WithMount(s.Local("data"), "/var/lib/postgresql/data")
	}

	if s.Settings.Silent {
		s.runner.Silence()
	}

	err = s.runner.Init(ctx, runnerImage)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	return s.Base.Runtime.InitResponse(s.NetworkMappings, providerInfo)
}

func (s *Runtime) WaitForReady(ctx context.Context) error {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	maxRetry := 20
	var err error
	for retry := 0; retry < maxRetry; retry++ {
		db, err := sql.Open("postgres", s.connection)
		if err != nil {
			return s.Wool.Wrapf(err, "cannot open database")
		}

		err = db.Ping()
		if err == nil {
			// Try to execute a simple query
			_, err = db.Exec("SELECT 1")
			if err == nil {
				return nil
			}
		}
		s.Wool.Debug("waiting for database", wool.ErrField(err))
		time.Sleep(3 * time.Second)
	}
	return s.Wool.Wrapf(err, "cannot ping database")
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("starting")

	runningContext := s.Wool.Inject(context.Background())

	err := s.runner.Start(runningContext)
	if err != nil {
		return s.Runtime.StartError(err)
	}

	s.Wool.Debug("waiting for ready")
	err = s.WaitForReady(ctx)
	if err != nil {
		return s.Runtime.StartError(err)
	}

	s.Wool.Debug("applying migrations")
	err = s.applyMigration(ctx)
	if err != nil {
		return s.Runtime.StartError(err)
	}

	if s.Settings.Watch {
		conf := services.NewWatchConfiguration(requirements)
		err := s.SetupWatcher(ctx, conf, s.EventHandler)
		if err != nil {
			s.Wool.Warn("error in watcher", wool.ErrField(err))
		}
	}
	s.Wool.Debug("start done")
	return s.Runtime.StartResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()
	s.Wool.Debug("stopping service")
	if s.runner == nil {
		err := s.runner.Stop()
		if err != nil {
			return s.Runtime.StopError(err)
		}
	}
	err := s.Base.Stop()
	if err != nil {
		return s.Runtime.StopError(err)
	}
	return s.Runtime.StopResponse()
}

func (s *Runtime) Communicate(ctx context.Context, req *agentv0.Engage) (*agentv0.InformationRequest, error) {
	return s.Base.Communicate(ctx, req)
}

/* Details

 */

func (s *Runtime) EventHandler(event code.Change) error {
	if strings.Contains(event.Path, "migrations") {
		err := s.updateMigration(context.Background(), event.Path)
		if err != nil {
			s.Wool.Warn("cannot apply migration", wool.ErrField(err))
		}
	}
	return nil
}
