package main

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/shared"

	"github.com/codefly-dev/core/agents/helpers/code"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/wool"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/lib/pq"

	"github.com/codefly-dev/service-external-postgres/migrations"
)

type Runtime struct {
	*Service

	// internal
	runnerEnvironment *runners.DockerEnvironment

	postgresPort     uint16
	migrationManager migrations.Manager
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogLoadRequest(req)

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "loading base")
	}

	s.Runtime.SetEnvironment(req.Environment)

	requirements.Localize(s.Location)

	// Endpoints
	s.Endpoints, err = s.Runtime.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "cannot load endpoints")
	}

	s.Wool.Debug("endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(s.Endpoints)))

	s.TcpEndpoint, err = resources.FindTCPEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "cannot find TCP endpoint")
	}

	return s.Runtime.LoadResponse()
}

func CallingContext() *basev0.NetworkAccess {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return resources.NewContainerNetworkAccess()
	}
	return resources.NewNativeNetworkAccess()
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)

	w := s.Wool.In("runtime::init")

	s.NetworkMappings = req.ProposedNetworkMappings

	s.Configuration = req.Configuration

	net, err := resources.FindNetworkMapping(ctx, s.NetworkMappings, s.TcpEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if net == nil {
		return s.Runtime.InitError(w.NewError("network mapping is nil"))
	}

	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.TcpEndpoint, CallingContext())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if instance == nil {
		return s.Runtime.InitError(w.NewError("network instance is nil"))
	}

	w.Debug("tcp network instance", wool.Field("instance", instance))

	s.Infof("will run on %s", instance.Host)
	s.postgresPort = 5432

	// Create connection string resources for the network instance
	for _, inst := range net.Instances {
		conf, errConn := s.CreateConnectionConfiguration(ctx, s.Configuration, inst, false)
		if errConn != nil {
			return s.Runtime.InitError(errConn)
		}
		w.Debug("adding configuration", wool.Field("config", resources.MakeConfigurationSummary(conf)), wool.Field("instance", inst))
		s.Runtime.RuntimeConfigurations = append(s.Runtime.RuntimeConfigurations, conf)
	}

	s.Wool.Debug("sending runtime configuration", wool.Field("conf", resources.MakeManyConfigurationSummary(s.Runtime.RuntimeConfigurations)))

	w.Debug("setting up connection string for migrations")
	// Setup a connection string for migration
	hostInstance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.TcpEndpoint, CallingContext())
	if err != nil {
		return s.Runtime.InitError(err)

	}

	s.connection, err = s.createConnectionString(ctx, s.Configuration, hostInstance.Address, false)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	w.Debug("connection string", wool.Field("connection", s.connection))

	// Docker
	runnerImage := image
	if s.Settings.ImageOverride != nil {
		runnerImage, err = resources.ParseDockerImage(*s.Settings.ImageOverride)
		if err != nil {
			return s.Runtime.InitError(err)
		}
	}

	runner, err := runners.NewDockerHeadlessEnvironment(ctx, runnerImage, s.UniqueWithWorkspace())
	if err != nil {
		return s.Runtime.InitError(err)
	}
	s.runnerEnvironment = runner
	err = s.LoadConfiguration(ctx, s.Configuration)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	runner.WithOutput(s.Wool)
	runner.WithPortMapping(ctx, uint16(instance.Port), s.postgresPort)

	runner.WithEnvironmentVariables(
		ctx,
		resources.Env("POSTGRES_USER", s.postgresUser),
		resources.Env("POSTGRES_PASSWORD", s.postgresPassword),
		resources.Env("POSTGRES_DB", s.DatabaseName))

	s.runnerEnvironment = runner

	w.Debug("init for runner environment: will start container")
	err = s.runnerEnvironment.Init(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if !s.Settings.NoMigration {
		migrationConfig := migrations.Config{
			DatabaseName: s.Settings.DatabaseName,
			MigrationDir: s.Local("migrations"),
		}

		if s.Settings.MigrationVersionDirOverride != nil {
			versionOverride := s.Local(*s.Settings.MigrationVersionDirOverride)
			empty, err := shared.CheckEmptyDirectory(ctx, versionOverride)
			if err != nil {
				return s.Runtime.InitError(err)
			}
			if empty {
				return s.Runtime.InitError(w.NewError("migration version directory is empty"))
			}
			migrationConfig.MigrationVersionDirOverride = shared.Pointer(versionOverride)
		}

		if s.Settings.AlembicImageOverride != nil && s.Settings.MigrationFormat == "alembic" {
			migrationConfig.ImageOverride = s.Settings.AlembicImageOverride
		}

		manager, err := migrations.NewManager(ctx, s.Settings.MigrationFormat, migrationConfig)
		if err != nil {
			return s.Runtime.InitError(err)
		}
		s.migrationManager = manager
	}

	s.Wool.Debug("init successful")
	return s.Runtime.InitResponse()
}

func (s *Runtime) WaitForReady(ctx context.Context) error {
	defer s.Wool.Catch()
	_ = s.Wool.Inject(ctx)

	s.Wool.Debug("waiting for ready", wool.Field("connection", s.connection))

	// Add connection timeout to the connection string
	connString := s.connection
	if !strings.Contains(connString, "connect_timeout=") {
		if strings.Contains(connString, "?") {
			connString += "&connect_timeout=10"
		} else {
			connString += "?connect_timeout=10"
		}
	}

	maxRetry := 10 // Increased from 5
	retryDelay := 3 * time.Second
	for retry := 0; retry < maxRetry; retry++ {
		db, err := sql.Open("postgres", connString)
		if err != nil {
			s.Wool.Debug("failed to open database connection", wool.ErrField(err))
			time.Sleep(retryDelay)
			continue
		}

		// Set connection timeout
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = db.PingContext(ctx)
		cancel()

		if err == nil {
			s.Wool.Debug("ping successful")
			// Try to execute a simple query with timeout
			ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
			_, err = db.ExecContext(ctx, "SELECT 1")
			cancel()

			if err == nil {
				s.Wool.Debug("database ready!")
				return nil
			}
		}

		s.Wool.Debug("waiting for database to be ready",
			wool.ErrField(err),
			wool.Field("retry", retry+1),
			wool.Field("max_retries", maxRetry))

		time.Sleep(retryDelay)
	}

	return s.Wool.NewError("database is not ready after maximum retries")
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

	if !s.Settings.NoMigration && s.migrationManager != nil {
		err = s.migrationManager.Init(ctx, s.Runtime.RuntimeConfigurations)
		if err != nil {
			return s.Runtime.StartError(err)
		}
		s.Wool.Focus("applying migrations")
		err = s.migrationManager.Apply(ctx)
		if err != nil {
			return s.Runtime.StartError(err)
		}
		s.Wool.Focus("migrations applied")
	}

	if s.Settings.HotReload {
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

	s.Wool.Debug("nothing to stop: keep environment alive")

	err := s.Base.Stop()
	if err != nil {
		return s.Runtime.StopError(err)
	}
	return s.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("Destroying")

	// Get the runner environment
	runner, err := runners.NewDockerHeadlessEnvironment(ctx, image, s.UniqueWithWorkspace())
	if err != nil {
		return s.Runtime.DestroyError(err)
	}

	err = runner.Shutdown(ctx)
	if err != nil {
		return s.Runtime.DestroyError(err)
	}
	return s.Runtime.DestroyResponse()
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	return s.Runtime.TestResponse()
}

func (s *Runtime) Communicate(ctx context.Context, req *agentv0.Engage) (*agentv0.InformationRequest, error) {
	return s.Base.Communicate(ctx, req)
}

/* Details

 */

func (s *Runtime) EventHandler(event code.Change) error {
	if strings.Contains(event.Path, "migrations") && s.migrationManager != nil {
		err := s.migrationManager.Update(context.Background(), event.Path)
		if err != nil {
			s.Wool.Warn("cannot apply migration", wool.ErrField(err))
		}
	}
	return nil
}
