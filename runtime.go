package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/codefly-dev/core/agents/helpers/code"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/agents/network"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
	"github.com/codefly-dev/core/runners"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/lib/pq"
)

type Runtime struct {
	*Service

	// internal
	Runner               *runners.Docker
	EnvironmentVariables *configurations.EnvironmentVariableManager

	Port int
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
}

const (
	DefaultPostgresUser     = "postgres"
	DefaultPostgresPassword = "password"
)

func (s *Runtime) ConnectionString(ctx context.Context, address string, withoutSSL bool) (string, error) {
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
	return connection, nil
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	requirements.WithDir(s.Location)

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

	var err error
	s.NetworkMappings, err = s.CustomNetwork(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	// for docker version
	s.Port = 5432

	address := s.NetworkMappings[0].Addresses[0]
	port := strings.Split(address, ":")[1]

	for _, providerInfo := range req.ProviderInfos {
		s.EnvironmentVariables.Add(configurations.ProviderInformationAsEnvironmentVariables(providerInfo)...)
	}

	connection, err := s.ConnectionString(ctx, address, true)
	if err != nil {
		return s.Runtime.InitError(err)
	}
	// This is the credential exposed to dependencies
	providerInfo := &basev0.ProviderInformation{Name: "postgres", Origin: s.Service.Configuration.Unique(), Data: map[string]string{"connection": connection}}

	// Docker
	s.Runner, err = runners.NewDocker(ctx, runners.WithWorkspace(s.Location))
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.Runner.WithPort(runners.DockerPort{Container: fmt.Sprintf("%d", s.Port), Host: port})
	s.Runner.WithEnvironmentVariables(s.EnvironmentVariables.GetBase()...)
	s.Runner.WithEnvironmentVariables(fmt.Sprintf("POSTGRES_DB=%s", s.DatabaseName))
	if s.Settings.Silent {
		s.Runner.Silence()
	}

	err = s.Runner.Init(ctx, image)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	return s.Base.Runtime.InitResponse(providerInfo)
}

func (s *Runtime) WaitForReady(ctx context.Context, connection string) error {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	maxRetry := 5
	var err error
	for retry := 0; retry < maxRetry; retry++ {
		time.Sleep(time.Second)
		db, err := sql.Open("postgres", connection)
		if err != nil {
			return s.Wool.Wrapf(err, "cannot open database")
		}

		err = db.Ping()
		if err == nil {
			return nil
		}
	}
	return s.Wool.Wrapf(err, "cannot ping database")
}

func (s *Runtime) migrationPath() string {
	absolutePath := s.Local("migrations")
	u := url.URL{
		Scheme: "file",
		Path:   absolutePath,
	}
	return u.String()
}

func (s *Runtime) applyMigration(ctx context.Context, connection string) error {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	db, err := sql.Open("postgres", connection)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot open database")
	}

	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create database instance")
	}

	m, err := migrate.NewWithDatabaseInstance(
		s.migrationPath(),
		"postgres", driver)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create migration instance")
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return s.Wool.Wrapf(err, "cannot apply migration")
	}
	return nil

}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	err := s.Runner.Start(ctx)
	if err != nil {
		return s.Runtime.StartError(err)
	}

	address := s.NetworkMappings[0].Addresses[0]

	connection, err := s.ConnectionString(ctx, address, true)
	if err != nil {
		return s.Runtime.StartError(err)
	}

	err = s.WaitForReady(ctx, connection)
	if err != nil {
		return s.Runtime.StartError(err)
	}

	err = s.applyMigration(ctx, connection)
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

	return s.Runtime.StartResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return &runtimev0.InformationResponse{}, nil
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()

	s.Wool.Debug("stopping service")

	err := s.Runner.Stop()
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot stop runner")
	}

	err = s.Base.Stop()
	if err != nil {
		return nil, err
	}
	return &runtimev0.StopResponse{}, nil
}

func (s *Runtime) Communicate(ctx context.Context, req *agentv0.Engage) (*agentv0.InformationRequest, error) {
	return s.Base.Communicate(ctx, req)
}

/* Details

 */

func (s *Runtime) EventHandler(event code.Change) error {
	s.WantRestart()
	return nil
}

func (s *Runtime) CustomNetwork(ctx context.Context) ([]*runtimev0.NetworkMapping, error) {
	endpoint := s.Endpoints[0]
	pm, err := network.NewServicePortManager(ctx)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create network manager")
	}
	err = pm.Expose(endpoint)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot add grpc endpoint to network manager")
	}
	err = pm.Reserve(ctx)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot reserve ports")
	}
	s.Port, err = pm.Port(ctx, endpoint)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot get port")
	}
	return pm.NetworkMapping(ctx)
}
