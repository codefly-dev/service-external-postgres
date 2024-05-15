package main

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/codefly-dev/core/agents"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
	"time"
)

// TODO: Add tests
// - migrations: up/down

func TestCreateToRun(t *testing.T) {
	wool.SetGlobalLogLevel(wool.DEBUG)
	agents.LogToConsole()
	ctx := context.Background()

	workspace := &resources.Workspace{Name: "test"}

	tmpDir := t.TempDir()
	defer func(path string) {
		err := os.RemoveAll(path)
		require.NoError(t, err)
	}(tmpDir)

	serviceName := fmt.Sprintf("svc-%v", time.Now().UnixMilli())
	service := resources.Service{Name: serviceName, Module: "mod", Version: "test-me"}
	err := service.SaveAtDir(ctx, tmpDir)
	require.NoError(t, err)

	identity := &basev0.ServiceIdentity{
		Name:      service.Name,
		Module:    service.Module,
		Workspace: workspace.Name,
		Location:  tmpDir,
	}
	builder := NewBuilder()

	resp, err := builder.Load(ctx, &builderv0.LoadRequest{DisableCatch: true, Identity: identity, CreationMode: &builderv0.CreationMode{Communicate: false}})
	require.NoError(t, err)
	require.NotNil(t, resp)

	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

	// Now run it
	runtime := NewRuntime()

	// Create temporary network mappings
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

	networkMappings, err := networkManager.GenerateNetworkMappings(ctx, env, workspace, runtime.Base.Service, runtime.Endpoints)
	require.NoError(t, err)
	require.Equal(t, 1, len(networkMappings))

	// Configurations are passed in
	conf := &basev0.Configuration{
		Origin:         service.Unique(),
		RuntimeContext: resources.NewRuntimeContextFree(),
		Configurations: []*basev0.ConfigurationInformation{
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

	// Extract logs

	_, err = runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)

	// Get the configuration and connect to postgres
	configurationOut, err := resources.ExtractConfiguration(init.RuntimeConfigurations, resources.NewRuntimeContextNative())
	require.NoError(t, err)

	// extract the connection string
	connString, err := resources.FindConfigurationValue(configurationOut, "postgres", "connection")
	require.NoError(t, err)

	// Do a SQL query
	db, err := sql.Open("postgres", connString)
	require.NoError(t, err)

	err = db.Ping()
	require.NoError(t, err)
	_, err = db.Exec("SELECT 1")
	require.NoError(t, err)
}
