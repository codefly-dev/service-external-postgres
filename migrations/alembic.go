package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/wool"
)

type Alembic struct {
	config Config
	w      *wool.Wool

	containerConnection string // For use inside Docker
	nativeConnection    string // For use on host
}

func NewAlembic(ctx context.Context, config Config) (*Alembic, error) {
	w := wool.Get(ctx)
	return &Alembic{config: config, w: w}, nil
}

func (a *Alembic) Init(ctx context.Context, configurations []*basev0.Configuration) error {
	// Get container connection string
	containerConfig, err := resources.ExtractConfiguration(configurations, resources.NewRuntimeContextContainer())
	if err != nil {
		return a.w.Wrapf(err, "cannot extract container configuration")
	}
	a.containerConnection, err = resources.GetConfigurationValue(ctx, containerConfig, "postgres", "connection")
	if err != nil {
		return a.w.Wrapf(err, "cannot get container connection string")
	}

	// Get native connection string
	nativeConfig, err := resources.ExtractConfiguration(configurations, resources.NewRuntimeContextNative())
	if err != nil {
		return a.w.Wrapf(err, "cannot extract native configuration")
	}
	a.nativeConnection, err = resources.GetConfigurationValue(ctx, nativeConfig, "postgres", "connection")
	if err != nil {
		return a.w.Wrapf(err, "cannot get native connection string")
	}

	a.w.Focus("connection strings",
		wool.Field("container", a.containerConnection),
		wool.Field("native", a.nativeConnection))
	return nil
}

func (a *Alembic) getRunner(ctx context.Context) (*runners.DockerEnvironment, error) {
	name := fmt.Sprintf("alembic-%d", time.Now().UnixMilli())

	// Debug directory contents
	a.w.Debug("checking migrations directory",
		wool.Field("dir", a.config.MigrationDir))
	entries, err := os.ReadDir(a.config.MigrationDir)
	if err != nil {
		a.w.Warn("cannot read migrations directory", wool.ErrField(err))
	} else {
		var files []string
		for _, entry := range entries {
			files = append(files, entry.Name())
		}
		a.w.Debug("migrations directory contents", wool.Field("files", files))
	}

	// Use our custom image with alembic pre-installed
	image := &resources.DockerImage{Name: "codeflydev/alembic", Tag: "latest"}
	if a.config.ImageOverride != nil {
		image, err = resources.ParseDockerImage(*a.config.ImageOverride)
		if err != nil {
			return nil, a.w.Wrapf(err, "cannot parse alembic image override")
		}
	}
	runner, err := runners.NewDockerEnvironment(ctx, image, a.config.MigrationDir, name)
	if err != nil {
		return nil, a.w.Wrapf(err, "cannot create docker environment")
	}

	// Mount migrations directory which should contain alembic.ini and versions/
	runner.WithMount(a.config.MigrationDir, "/workspace")
	if a.config.MigrationVersionDirOverride != nil {
		runner.WithMount(*a.config.MigrationVersionDirOverride, "/workspace/versions")
	}
	runner.WithWorkDir("/workspace")
	runner.WithPause()

	// Set environment variables
	runner.WithEnvironmentVariables(ctx,
		resources.Env("DATABASE_URL", a.containerConnection),
	)

	return runner, nil
}

func (a *Alembic) Apply(ctx context.Context) error {
	// Create a detached context with no timeout/deadline for migration operations
	// This will prevent context cancellation from interfering with DB operations
	migrationCtx := context.Background()

	runner, err := a.getRunner(ctx)
	if err != nil {
		return err
	}

	defer func() {
		err = runner.Shutdown(ctx)
		if err != nil {
			a.w.Warn("cannot shutdown runner", wool.ErrField(err))
		}
	}()

	err = runner.Init(ctx)
	if err != nil {
		return a.w.Wrapf(err, "cannot init runner")
	}

	// First check current state
	a.w.Focus("checking current migration state")
	currentProc, err := runner.NewProcess("alembic", "-c", "/workspace/alembic.ini", "current")
	if err != nil {
		return a.w.Wrapf(err, "cannot create current process")
	}
	currentProc.WithOutput(a.w)
	err = currentProc.Run(migrationCtx) // Use the detached context
	if err != nil {
		return a.w.Wrapf(err, "cannot check current version")
	}

	// Run alembic upgrade
	a.w.Focus("starting migrations to latest version")
	proc, err := runner.NewProcess("alembic", "-c", "/workspace/alembic.ini", "upgrade", "head")
	if err != nil {
		return a.w.Wrapf(err, "cannot create process")
	}
	proc.WithOutput(a.w)

	a.w.Focus("running upgrade process")
	err = proc.Run(migrationCtx) // Use the detached context
	if err != nil {
		return a.w.Wrapf(err, "alembic upgrade failed")
	}
	a.w.Focus("upgrade process completed")

	// Check for and attempt to commit any pending transactions
	a.w.Debug("checking for active transactions")
	txProc, err := runner.NewProcess("psql", a.containerConnection, "-c",
		"SELECT pid, state, query, xact_start, now() - xact_start AS duration FROM pg_stat_activity WHERE state LIKE '%transaction%';")
	if err != nil {
		a.w.Warn("cannot check for active transactions", wool.ErrField(err))
	} else {
		txProc.WithOutput(a.w)
		_ = txProc.Run(migrationCtx) // Use detached context
	}

	// Attempt to explicitly commit any pending transactions
	a.w.Debug("attempting to commit any pending transactions")
	commitProc, err := runner.NewProcess("psql", a.containerConnection, "-c", "COMMIT;")
	if err != nil {
		a.w.Warn("cannot run commit command", wool.ErrField(err))
	} else {
		commitProc.WithOutput(a.w)
		_ = commitProc.Run(migrationCtx) // Use detached context
	}

	// Try to terminate any idle transactions
	a.w.Debug("attempting to terminate any idle transactions")
	terminateProc, err := runner.NewProcess("psql", a.containerConnection, "-c",
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE state = 'idle in transaction' AND pid <> pg_backend_pid();")
	if err != nil {
		a.w.Warn("cannot terminate idle transactions", wool.ErrField(err))
	} else {
		terminateProc.WithOutput(a.w)
		_ = terminateProc.Run(migrationCtx) // Use detached context
	}

	// Check final state
	a.w.Focus("checking final migration state")
	finalProc, err := runner.NewProcess("alembic", "-c", "/workspace/alembic.ini", "current")
	if err != nil {
		return a.w.Wrapf(err, "cannot create final check process")
	}
	finalProc.WithOutput(a.w)
	err = finalProc.Run(migrationCtx) // Use detached context
	if err != nil {
		return a.w.Wrapf(err, "cannot check final version")
	}

	a.w.Focus("checking tables in database")

	// Check tables using native connection with retries for up to 1 minute
	maxRetries := 12 // Try 12 times with 5-second intervals = 60 seconds total
	retryDelay := time.Second * 5
	var tables []string
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			a.w.Debug("retrying table check", wool.Field("attempt", i+1), wool.Field("max_retries", maxRetries))
			time.Sleep(retryDelay)
		}

		a.w.Debug("checking database for tables", wool.Field("attempt", i+1),
			wool.Field("elapsed_time", time.Duration(i)*retryDelay),
			wool.Field("timeout", time.Duration(maxRetries)*retryDelay))
		db, err := sql.Open("postgres", a.nativeConnection)
		if err != nil {
			a.w.Debug("failed to open database connection", wool.Field("attempt", i+1), wool.ErrField(err))
			lastErr = err
			continue
		}
		defer db.Close()

		// Test the connection
		err = db.Ping()
		if err != nil {
			a.w.Debug("database ping failed", wool.Field("attempt", i+1), wool.ErrField(err))
			lastErr = err
			continue
		}

		// List all tables including version tables
		query := `
			SELECT tablename
			FROM pg_catalog.pg_tables
			WHERE schemaname = 'public'
			AND tablename NOT LIKE 'pg_%'
			AND tablename NOT LIKE 'sql_%'
			AND tablename != 'alembic_version'
		`
		rows, err := db.Query(query)
		if err != nil {
			lastErr = err
			continue
		}
		defer rows.Close()

		tables = nil
		for rows.Next() {
			var table string
			if err := rows.Scan(&table); err != nil {
				lastErr = err
				continue
			}
			tables = append(tables, table)
		}

		if len(tables) > 0 {
			break
		}
	}

	// Log tables but don't fail if empty
	a.w.Focus("tables in database", wool.Field("tables", tables))
	if len(tables) == 0 {
		// Open a fresh connection to check for alembic_version
		finalDb, finalErr := sql.Open("postgres", a.nativeConnection)
		if finalErr != nil {
			a.w.Debug("failed to open final database connection", wool.ErrField(finalErr))
		} else {
			defer finalDb.Close()

			// Check for alembic_version table to see if migrations ran but didn't create tables
			var hasAlembicVersion bool
			vErr := finalDb.QueryRow(`
				SELECT EXISTS (
					SELECT FROM pg_tables
					WHERE schemaname = 'public'
					AND tablename = 'alembic_version'
				)
			`).Scan(&hasAlembicVersion)

			if vErr == nil && hasAlembicVersion {
				// Migration ran but didn't create any tables - try to force commit again
				a.w.Debug("alembic_version table exists but no application tables - trying to clean up transactions")

				// Try to check for transaction issues
				a.w.Debug("checking for transaction issues")

				// Check for active transactions one more time
				txProc2, _ := runner.NewProcess("psql", a.containerConnection, "-c",
					"SELECT count(*) FROM pg_stat_activity WHERE state LIKE '%transaction%';")
				txProc2.WithOutput(a.w)
				_ = txProc2.Run(migrationCtx) // Use detached context

				// Try to force a transaction commit again
				commitProc2, _ := runner.NewProcess("psql", a.containerConnection, "-c", "COMMIT;")
				commitProc2.WithOutput(a.w)
				_ = commitProc2.Run(migrationCtx) // Use detached context

				// Try to explicitly terminate any idle transactions
				killProc, _ := runner.NewProcess("psql", a.containerConnection, "-c",
					"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE state = 'idle in transaction' AND pid <> pg_backend_pid();")
				killProc.WithOutput(a.w)
				_ = killProc.Run(migrationCtx) // Use detached context

				// Check version records
				var versions []string
				vRows, vRowErr := finalDb.Query("SELECT version_num FROM alembic_version")
				if vRowErr == nil {
					defer vRows.Close()
					for vRows.Next() {
						var v string
						if vRows.Scan(&v) == nil {
							versions = append(versions, v)
						}
					}
				}

				a.w.Warn("transaction issue detected: migrations completed (alembic_version exists) but no tables were created",
					wool.Field("versions", versions),
					wool.Field("max_wait_time", time.Duration(maxRetries)*retryDelay))

				return a.w.Wrap(fmt.Errorf("transaction issue detected: alembic_version table exists (versions: %v) but no application tables were created after waiting %s - this is likely due to uncommitted transactions", versions, time.Duration(maxRetries)*retryDelay))
			}
		}

		if lastErr != nil {
			return a.w.Wrapf(lastErr, "failed to check tables after %d attempts (waited %s)", maxRetries, time.Duration(maxRetries)*retryDelay)
		}
		return a.w.Wrap(fmt.Errorf("no tables found in database after waiting %s (%d attempts): migrations failed or are taking too long to commit", time.Duration(maxRetries)*retryDelay, maxRetries))
	}
	return nil
}

func (a *Alembic) Update(ctx context.Context, migrationFile string) error {
	runner, err := a.getRunner(ctx)
	if err != nil {
		return err
	}

	err = runner.Init(ctx)
	if err != nil {
		return a.w.Wrapf(err, "cannot init docker environment")
	}
	defer runner.Shutdown(ctx)

	// Force reapply by running down and up
	proc, err := runner.NewProcess("alembic", "downgrade", "-1")
	if err != nil {
		return a.w.Wrapf(err, "cannot create process")
	}
	err = proc.Run(ctx)
	if err != nil {
		return a.w.Wrapf(err, "alembic downgrade failed")
	}

	proc, err = runner.NewProcess("alembic", "upgrade", "+1")
	if err != nil {
		return a.w.Wrapf(err, "cannot create process")
	}
	err = proc.Run(ctx)
	if err != nil {
		return a.w.Wrapf(err, "alembic upgrade failed")
	}
	return nil
}
