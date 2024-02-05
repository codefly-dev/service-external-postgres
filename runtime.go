package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/codefly-dev/core/agents/helpers/code"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/wool"

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
	runner               *runners.Docker
	EnvironmentVariables *configurations.EnvironmentVariableManager

	Port            int
	NetworkMappings []*basev0.NetworkMapping
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

	s.NetworkMappings = req.NetworkMappings

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

	connection, err := s.ConnectionString(ctx, net.Address, true)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	// This is the credential exposed to dependencies
	providerInfo := &basev0.ProviderInformation{Name: "postgres", Origin: s.Service.Configuration.Unique(), Data: map[string]string{"connection": connection}}

	s.Wool.Debug("init", wool.NullableField("provider", providerInfo))

	// Docker
	runner, err := runners.NewDocker(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.runner = runner
	s.runner.WithPort(runners.DockerPortMapping{Container: s.Port, Host: net.Port})
	s.runner.WithEnvironmentVariables(s.EnvironmentVariables.GetBase()...)
	s.runner.WithEnvironmentVariables(fmt.Sprintf("POSTGRES_DB=%s", s.DatabaseName))

	if s.Settings.Silent {
		s.runner.Silence()
	}

	err = s.runner.Init(ctx, image)
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

	runningContext := s.Wool.Inject(context.Background())

	err := s.runner.Start(runningContext)
	if err != nil {
		return s.Runtime.StartError(err)
	}

	instance, err := configurations.GetMappingInstance(s.NetworkMappings)
	if err != nil {
		return s.Runtime.StartError(err)
	}
	address := instance.Address

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
	return s.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()
	s.Wool.Debug("stopping service")
	err := s.runner.Stop()
	if err != nil {
		return s.Runtime.StopError(err)
	}

	// Be nice and wait for Port to be free
	err = runners.WaitForPortUnbound(ctx, s.Port)
	if err != nil {
		s.Wool.Warn("cannot wait for port to be free", wool.ErrField(err))
	}

	err = s.Base.Stop()
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
	s.Runtime.DesiredInit()
	return nil
}
