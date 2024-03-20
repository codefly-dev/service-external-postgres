package main

import (
	"context"
	"database/sql"
	"fmt"
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
	runner *runners.Docker
	port   int32
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
}

var runnerImage = &configurations.DockerImage{Name: "postgres", Tag: "16.1"}

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

	return s.Base.Runtime.LoadResponse()
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.NetworkMappings = req.ProposedNetworkMappings

	s.Wool.Focus("init",
		wool.Field("info", configurations.MakeProviderInformationSummary(req.ProviderInfos)),
		wool.Field("network mappings", configurations.MakeNetworkMappingSummary(s.NetworkMappings)))

	net, err := configurations.FindNetworkMapping(s.tcpEndpoint, s.NetworkMappings)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.Focus("port", wool.Field("port", net.Port))
	s.LogForward("will run on address: %s", net.Address)

	// for docker version
	s.port = 5432

	for _, providerInfo := range req.ProviderInfos {
		s.EnvironmentVariables.Add(configurations.ProviderInformationAsEnvironmentVariables(providerInfo)...)
	}

	err = s.CreateConnectionString(ctx, net.Address, true)

	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.Wool.Info("connection", wool.Field("connection", s.connection))

	// This is the credential exposed to dependencies
	connectionString := &basev0.ProviderInformation{Name: "postgres", Origin: s.Service.Configuration.Unique(), Data: map[string]string{"connection": s.connection}}

	s.ServiceProviderInfos = []*basev0.ProviderInformation{connectionString}

	// Docker
	runner, err := runners.NewDocker(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.runner = runner
	if s.Settings.Persist {
		s.runner.WithPersistence()
	}
	s.runner.WithName(s.Global())
	s.runner.WithOut(s.Wool)
	s.runner.WithPort(runners.DockerPortMapping{Container: s.port, Host: net.Port})
	s.runner.WithEnvironmentVariables(s.EnvironmentVariables.GetBase()...)
	s.runner.WithEnvironmentVariables(fmt.Sprintf("POSTGRES_DB=%s", s.DatabaseName))

	if s.Settings.Silent {
		s.runner.WithSilence()
	}

	err = s.runner.Init(ctx, runnerImage)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	return s.Base.Runtime.InitResponse()
}

func (s *Runtime) WaitForReady(ctx context.Context) error {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Focus("waiting for ready", wool.Field("connection", s.connection))

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

	if !s.Settings.NoMigration {
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

	if s.runner != nil {
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
