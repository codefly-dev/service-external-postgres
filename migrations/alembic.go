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
	err = currentProc.Run(ctx)
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
	err = proc.Run(ctx)
	if err != nil {
		return a.w.Wrapf(err, "alembic upgrade failed")
	}
	a.w.Focus("upgrade process completed")

	// Check final state
	a.w.Focus("checking final migration state")
	finalProc, err := runner.NewProcess("alembic", "-c", "/workspace/alembic.ini", "current")
	if err != nil {
		return a.w.Wrapf(err, "cannot create final check process")
	}
	finalProc.WithOutput(a.w)
	err = finalProc.Run(ctx)
	if err != nil {
		return a.w.Wrapf(err, "cannot check final version")
	}

	a.w.Focus("checking tables in database")

	// Check tables using native connection
	db, err := sql.Open("postgres", a.nativeConnection)
	if err != nil {
		return a.w.Wrapf(err, "cannot open database")
	}
	defer db.Close()

	// List all tables including version tables
	rows, err := db.Query(`
		SELECT tablename
		FROM pg_catalog.pg_tables
		WHERE schemaname = 'public'
		AND tablename NOT LIKE 'pg_%'
		AND tablename NOT LIKE 'sql_%'
		AND tablename != 'alembic_version'
	`)
	if err != nil {
		return a.w.Wrapf(err, "cannot query tables")
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return a.w.Wrapf(err, "cannot scan table info")
		}
		tables = append(tables, table)
	}

	// Log tables but don't fail if empty
	a.w.Focus("tables in database", wool.Field("tables", tables))
	if len(tables) == 0 {
		return a.w.Wrap(fmt.Errorf("no tables found in database: migrations failed"))
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
