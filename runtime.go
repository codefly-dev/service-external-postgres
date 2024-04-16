package main

import (
	"context"
	"database/sql"
	"fmt"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"strings"
	"time"

	"github.com/codefly-dev/core/agents/helpers/code"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/configurations"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
	runners "github.com/codefly-dev/core/runners/base"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/lib/pq"
)

type Runtime struct {
	*Service

	// internal
	runner runners.RunnerEnvironment

	postgresPort uint16
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.SetScope(req)

	if !s.Runtime.Container() {
		return s.Base.Runtime.LoadError(fmt.Errorf("not implemented: cannot load service in scope %s", req.Scope))
	}

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	requirements.Localize(s.Location)

	// Endpoints
	s.Endpoints, err = s.Runtime.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	s.Wool.Debug("endpoints", wool.Field("endpoints", configurations.MakeManyEndpointSummary(s.Endpoints)))

	s.tcpEndpoint, err = configurations.FindTCPEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}
	return s.Base.Runtime.LoadResponse()
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)

	w := s.Wool.In("runtime::init")

	s.NetworkMappings = req.ProposedNetworkMappings

	s.Configuration = req.Configuration

	net, err := configurations.FindNetworkMapping(ctx, s.NetworkMappings, s.tcpEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	instance, err := s.Runtime.NetworkInstance(ctx, s.NetworkMappings, s.tcpEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	w.Debug("network instance", wool.Field("instance", instance))

	s.LogForward("will run on localhost:%d", instance.Port)
	s.postgresPort = 5432

	// Create connection string configurations for the network instance
	for _, inst := range net.Instances {
		conf, err := s.CreateConnectionConfiguration(ctx, req.Configuration, inst, false)
		if err != nil {
			return s.Runtime.InitError(err)
		}
		s.Runtime.ExportedConfigurations = append(s.Runtime.ExportedConfigurations, conf)
	}

	// Setup a connection string for migration
	// We are inside the agent so we need to use the Native one!
	hostInstance, err := configurations.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.tcpEndpoint, basev0.NetworkScope_Native)
	if err != nil {
		return s.Runtime.InitError(err)

	}
	s.connection, err = s.createConnectionString(ctx, req.Configuration, hostInstance.Address, false)
	if err != nil {
		return s.Runtime.InitError(err)
	}
	s.Wool.Debug("connection string", wool.Field("connection", s.connection))

	// Docker
	runner, err := runners.NewDockerHeadlessEnvironment(ctx, runnerImage, s.UniqueWithProject())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	user, password, err := s.getUserPassword(ctx, req.Configuration)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	runner.WithOutput(s.Logger)
	runner.WithPortMapping(ctx, uint16(instance.Port), s.postgresPort)

	runner.WithEnvironmentVariables(
		configurations.Env("POSTGRES_USER", user),
		configurations.Env("POSTGRES_PASSWORD", password),
		configurations.Env("POSTGRES_DB", s.DatabaseName))

	s.runner = runner

	err = s.runner.Init(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	return s.Base.Runtime.InitResponse()
}

func (s *Runtime) WaitForReady(ctx context.Context) error {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("waiting for ready", wool.Field("connection", s.connection))

	maxRetry := 10
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
				s.Wool.Debug("database ready!")
				return nil
			}
		}
		s.Wool.Debug("waiting for database", wool.ErrField(err))
		time.Sleep(time.Second)
	}
	return s.Wool.Wrapf(err, "cannot ping database")
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("starting")

	s.Wool.Debug("waiting for ready")

	err := s.WaitForReady(ctx)
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
	if s.Settings.Persist {
		s.Wool.Debug("persisting service")
		return s.Runtime.StopResponse()
	}

	s.Wool.Debug("stopping service")

	if s.runner != nil {
		err := s.runner.Stop(ctx)
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

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	//TODO implement me
	panic("implement me")
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
