package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/stretchr/testify/require"
)

// TODO: Add tests
// - migrations: up/down

func TestCreateToRun(t *testing.T) {
	t.Run("gomigrate", func(t *testing.T) {
		runTestWithFormat(t, "gomigrate")
	})

	t.Run("alembic", func(t *testing.T) {
		runTestWithFormat(t, "alembic")
	})
}

func runTestWithFormat(t *testing.T, migrationFormat string) {
	agents.LogToConsole()
	ctx := context.Background()

	workspace := &resources.Workspace{Name: "test"}

	tmpDir, err := os.MkdirTemp("testdata", "test-*")
	require.NoError(t, err)
	defer func(path string) {
		os.RemoveAll(path)
	}(tmpDir)

	serviceName := fmt.Sprintf("svc-%v", time.Now().UnixMilli())
	service := resources.Service{Name: serviceName, Version: "test-me"}
	err = service.SaveAtDir(ctx, path.Join(tmpDir, "mod", service.Name))
	require.NoError(t, err)

	identity := &basev0.ServiceIdentity{
		Name:                service.Name,
		Module:              "mod",
		Workspace:           workspace.Name,
		WorkspacePath:       tmpDir,
		RelativeToWorkspace: fmt.Sprintf("mod/%s", service.Name),
	}
	builder := NewBuilder()
	builder.Settings.MigrationFormat = migrationFormat

	resp, err := builder.Load(ctx, &builderv0.LoadRequest{DisableCatch: true, Identity: identity, CreationMode: &builderv0.CreationMode{Communicate: false}})
	require.NoError(t, err)
	require.NotNil(t, resp)

	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

	runtime := NewRuntime()
	runtime.Settings.MigrationFormat = migrationFormat

	networkManager, err := network.NewRuntimeManager(ctx, nil)
	require.NoError(t, err)
	networkManager.WithTemporaryPorts()

	env := resources.LocalEnvironment()

	_, err = runtime.Load(ctx, &runtimev0.LoadRequest{
		Identity:     identity,
		Environment:  shared.Must(env.Proto()),
		DisableCatch: true})
	require.NoError(t, err)

	require.Equal(t, 1, len(runtime.Endpoints))

	networkMappings, err := networkManager.GenerateNetworkMappings(ctx, env, workspace, runtime.Identity, runtime.Endpoints)
	require.NoError(t, err)
	require.Equal(t, 1, len(networkMappings))

	conf := &basev0.Configuration{
		Origin:         fmt.Sprintf("mod/%s", service.Name),
		RuntimeContext: resources.NewRuntimeContextFree(),
		Infos: []*basev0.ConfigurationInformation{
			{Name: "postgres",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "POSTGRES_USER", Value: "postgres"},
					{Key: "POSTGRES_PASSWORD", Value: "password"},
				},
			},
		},
	}

	init, err := runtime.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext:          resources.NewRuntimeContextFree(),
		Configuration:           conf,
		ProposedNetworkMappings: networkMappings,
	})
	require.NoError(t, err)
	require.NotNil(t, init)

	defer func() {
		_, err = runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	}()

	_, err = runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)

	configurationOut, err := resources.ExtractConfiguration(init.RuntimeConfigurations, resources.NewRuntimeContextNative())
	require.NoError(t, err)

	connString, err := resources.GetConfigurationValue(ctx, configurationOut, "postgres", "connection")
	require.NoError(t, err)

	db, err := sql.Open("postgres", connString)
	require.NoError(t, err)

	err = db.Ping()
	require.NoError(t, err)
	_, err = db.Exec("SELECT 1")
	require.NoError(t, err)

	// Common table name for both formats - use service name
	tableName := serviceName // Will be something like "svc-1234567890"

	// Check migrations based on format
	if migrationFormat == "gomigrate" {
		// Check version table exists
		rows, err := db.Query("SELECT version FROM schema_migrations")
		require.NoError(t, err)
		defer rows.Close()

		var versions []int64
		for rows.Next() {
			var version int64
			err := rows.Scan(&version)
			require.NoError(t, err)
			versions = append(versions, version)
		}
		require.NotEmpty(t, versions)

	} else if migrationFormat == "alembic" {
		// Check version table exists
		rows, err := db.Query("SELECT version_num FROM alembic_version")
		require.NoError(t, err)
		defer rows.Close()

		var versions []string
		for rows.Next() {
			var version string
			err := rows.Scan(&version)
			require.NoError(t, err)
			versions = append(versions, version)
		}
		require.NotEmpty(t, versions)
	}

	// For both formats, just check if the table exists
	var exists bool
	err = db.QueryRow(`
		SELECT EXISTS (
			SELECT FROM information_schema.tables 
			WHERE table_schema = 'public' 
			AND table_name = $1
		)
	`, tableName).Scan(&exists)
	require.NoError(t, err)
	require.True(t, exists, "templated table not found")
}
