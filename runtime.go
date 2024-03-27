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
	"github.com/codefly-dev/core/runners"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/lib/pq"
)

type Runtime struct {
	*Service

	// internal
	runner runners.Runner

	postgresPort uint16
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

	w := s.Wool.In("load")

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	s.Runtime.Scope = req.Scope

	if s.Runtime.Scope != basev0.RuntimeScope_Container {
		return s.Base.Runtime.LoadError(fmt.Errorf("not implemented: cannot load service in scope %s", req.Scope))
	}

	requirements.Localize(s.Location)

	// Endpoints
	s.Endpoints, err = s.Runtime.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	w.Focus("endpoints", wool.Field("endpoints", configurations.MakeManyEndpointSummary(s.Endpoints)))

	s.tcpEndpoint, err = configurations.FindTCPEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}
	return s.Base.Runtime.LoadResponse()
}

func (s *Runtime) getUserPassword(ctx context.Context) (string, string, error) {

	user, err := configurations.GetConfigurationValue(ctx, s.Configuration, "postgres", "POSTGRES_USER")
	if err != nil {
		return "", "", s.Wool.Wrapf(err, "cannot get user")
	}
	password, err := configurations.GetConfigurationValue(ctx, s.Configuration, "postgres", "POSTGRES_PASSWORD")
	if err != nil {
		return "", "", s.Wool.Wrapf(err, "cannot get password")
	}
	return user, password, nil

}

func (s *Runtime) createConnectionString(ctx context.Context, address string, withSSL bool) (string, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	user, password, err := s.getUserPassword(ctx)
	if err != nil {
		return "", s.Wool.Wrapf(err, "cannot get user and password")
	}

	conn := fmt.Sprintf("postgresql://%s:%s@%s/%s", user, password, address, s.DatabaseName)
	if !withSSL {
		conn += "?sslmode=disable"
	}
	return conn, nil
}

func (s *Runtime) CreateConnectionConfiguration(ctx context.Context, instance *basev0.NetworkInstance, withSSL bool) (*basev0.Configuration, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	connection, err := s.createConnectionString(ctx, instance.Address, withSSL)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create connection string")
	}

	conf := &basev0.Configuration{
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
	return conf, nil
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	w := s.Wool.In("init")

	s.NetworkMappings = req.ProposedNetworkMappings

	w.Focus("network", wool.Field("endpoint", configurations.MakeEndpointSummary(s.tcpEndpoint)))

	w.Focus("proposed network mapping", wool.Field("network", configurations.MakeManyNetworkMappingSummary(req.ProposedNetworkMappings)))

	s.NetworkMappings = req.ProposedNetworkMappings

	s.Configuration = req.Configuration

	// Extract the port
	net, err := configurations.FindNetworkMapping(s.NetworkMappings, s.tcpEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	instance, err := s.Runtime.NetworkInstance(s.NetworkMappings, s.tcpEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	w.Focus("network instance", wool.Field("instance", instance))

	s.LogForward("will run on localhost:%d", instance.Port)
	s.postgresPort = 5432

	// Configurations
	w.Focus("configurations",
		wool.Field("service configuration", configurations.MakeConfigurationSummary(req.Configuration)),
		wool.Field("dependency configurations", configurations.MakeManyConfigurationSummary(req.DependenciesConfigurations)))

	// Create connection string configurations for the network instance
	for _, inst := range net.Instances {
		conf, err := s.CreateConnectionConfiguration(ctx, inst, false)
		if err != nil {
			return s.Runtime.InitError(err)
		}
		s.ExportedConfigurations = append(s.ExportedConfigurations, conf)
	}

	// Setup a connection string for migration
	// We are inside the agent so we need to use the Native one!
	hostInstance, err := configurations.FindNetworkInstance(s.NetworkMappings, s.tcpEndpoint, basev0.RuntimeScope_Native)
	if err != nil {
		return s.Runtime.InitError(err)

	}
	s.connection, err = s.createConnectionString(ctx, hostInstance.Address, false)
	if err != nil {
		return s.Runtime.InitError(err)
	}
	s.Wool.Focus("connection string", wool.Field("connection", s.connection))

	// Docker
	runner, err := runners.NewDocker(ctx, runnerImage)
	if err != nil {
		return s.Runtime.InitError(err)
	}
	user, password, err := s.getUserPassword(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.runner = runner
	if s.Settings.Persist {
		runner.WithPersistence()
	}
	runner.WithName(s.Global())
	runner.WithOut(s.Wool)
	runner.WithPort(runners.DockerPortMapping{Container: s.postgresPort, Host: uint16(instance.Port)})

	runner.WithEnvironmentVariables(
		fmt.Sprintf("POSTGRES_USER=%s", user),
		fmt.Sprintf("POSTGRES_PASSWORD=%s", password),
		fmt.Sprintf("POSTGRES_DB=%s", s.DatabaseName))

	if s.Settings.Silent {
		runner.WithSilence()
	}

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
