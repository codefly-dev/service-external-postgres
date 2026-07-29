package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	agenttesting "github.com/codefly-dev/core/agents/testing"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestDeploymentTemplatesWithMigration(t *testing.T) {
	dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, DeploymentTemplateParameters{
		WithBootstrap: true,
		ManagedImage:  image.FullName(),
	})
	assertMigrationResource(t, dir, true)
	assertEphemeralSecret(t, dir)
}

func TestDeploymentTemplatesWithoutBootstrap(t *testing.T) {
	dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, DeploymentTemplateParameters{
		ManagedImage: image.FullName(),
	})
	assertMigrationResource(t, dir, false)
}

func TestPromotableDeploymentUsesTypedSecretReferencesWithoutValues(t *testing.T) {
	ctx := context.Background()
	builder := NewBuilder()
	identity := &basev0.ServiceIdentity{
		Workspace: "workspace",
		Module:    "module",
		Name:      "store",
		Version:   "1.2.3",
	}
	if err := builder.HeadlessLoad(ctx, identity); err != nil {
		t.Fatal(err)
	}
	builder.DatabaseName = "users"
	builder.Information = &services.Information{
		Service: resources.ToServiceWithCase(builder.Identity),
		Module:  resources.ToModuleWithCase(builder.Identity),
	}
	builder.TcpEndpoint = &basev0.Endpoint{
		Name:    "tcp",
		Module:  identity.Module,
		Service: identity.Name,
		Api:     "tcp",
	}
	instance := resources.NewNetworkInstance("store.platform.svc.cluster.local", 5432)
	instance.Access = resources.NewPublicNetworkAccess()
	secretReferences := make(map[string]*builderv0.KubernetesSecretKeyReference)
	for _, key := range []string{
		"POSTGRES_USER",
		"POSTGRES_PASSWORD",
		"POSTGRES_READ_ONLY_PASSWORD",
		"POSTGRES_READ_WRITE_PASSWORD",
	} {
		configurationKey := resources.ServiceSecretConfigurationKeyFromUnique(builder.Unique(), "postgres", key)
		secretReferences[configurationKey] = &builderv0.KubernetesSecretKeyReference{
			Name: "store-secrets",
			Key:  configurationKey,
		}
	}
	destination := t.TempDir()

	response, err := builder.Deploy(ctx, &builderv0.DeploymentRequest{
		Environment: &basev0.Environment{Name: "local"},
		NetworkMappings: []*basev0.NetworkMapping{{
			Endpoint:  builder.TcpEndpoint,
			Instances: []*basev0.NetworkInstance{instance},
		}},
		Deployment: &builderv0.Deployment{Kind: &builderv0.Deployment_Kubernetes{
			Kubernetes: &builderv0.KubernetesDeployment{
				Namespace:        "platform",
				Destination:      destination,
				Profile:          builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
				SecretReferences: secretReferences,
				BuildContext: &builderv0.DockerBuildContext{
					DockerRepository: "registry.example.com",
					ImageDigest:      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetState().GetState() != builderv0.DeploymentStatus_SUCCESS {
		t.Fatalf("deployment failed: %s", response.GetState().GetMessage())
	}
	if !response.GetDeployment().GetKubernetes().GetValidation().GetPromotable() {
		t.Fatal("deployment is not promotable")
	}
	for _, value := range response.GetConfiguration().GetInfos()[0].GetConfigurationValues() {
		if !value.GetSecret() || value.GetValue() != "" {
			t.Fatalf("exported connection contains a value: %+v", value)
		}
	}

	tree := ""
	for _, file := range []string{"stateful-set.yaml", "job.yaml"} {
		content, readErr := os.ReadFile(filepath.Join(destination, "base", file))
		if readErr != nil {
			t.Fatal(readErr)
		}
		tree += string(content)
	}
	for _, expected := range []string{
		"name: POSTGRES_USER",
		"name: POSTGRES_PASSWORD",
		"name: POSTGRES_READ_ONLY_PASSWORD",
		"name: POSTGRES_READ_WRITE_PASSWORD",
		"name: CODEFLY_POSTGRES_MIGRATION_CONNECTION",
		"name: store-secrets",
		`value: "users"`,
	} {
		if !strings.Contains(tree, expected) {
			t.Errorf("manifest tree missing %q:\n%s", expected, tree)
		}
	}
	for configurationKey := range secretReferences {
		if strings.Contains(tree, "name: "+configurationKey) {
			t.Errorf("manifest exposed configuration key %q as a runtime variable", configurationKey)
		}
	}
}

func TestPromotableGitOpsDeploymentReturnsConfigurationAndIsolatesSecrets(t *testing.T) {
	useSuccessfulKubectl(t)
	builder, networkMappings := newDeploymentTestBuilder(t)
	destination := t.TempDir()

	response, err := builder.Deploy(context.Background(), promotableDeploymentRequest(
		destination,
		networkMappings,
		promotablePostgresSecretReferences(),
		true,
	))
	require.NoError(t, err)
	require.Equal(t, builderv0.DeploymentStatus_SUCCESS, response.GetState().GetState(), response.GetState().GetMessage())

	output := response.GetDeployment().GetKubernetes()
	require.Equal(t, builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1, output.GetProfile())
	require.Equal(t, services.KubernetesManifestContractVersion, output.GetContractVersion())
	require.Equal(t, builderv0.KubernetesManifestValidation_STATUS_PASSED, output.GetValidation().GetStaticValidation())
	require.Equal(t, builderv0.KubernetesManifestValidation_STATUS_PASSED, output.GetValidation().GetServerSideValidation())
	require.True(t, output.GetValidation().GetPromotable())

	configuration := response.GetConfiguration()
	require.Equal(t, builder.Unique(), configuration.GetOrigin())
	require.Equal(t, resources.RuntimeContextFree, configuration.GetRuntimeContext().GetKind())
	require.Len(t, configuration.GetInfos(), 1)
	require.Equal(t, "postgres", configuration.GetInfos()[0].GetName())
	values := configuration.GetInfos()[0].GetConfigurationValues()
	require.Len(t, values, 2)
	for _, key := range []string{readOnlyConnectionKey, readWriteConnectionKey} {
		var matched *basev0.ConfigurationValue
		for _, value := range values {
			if value.GetKey() == key {
				matched = value
				break
			}
		}
		require.NotNil(t, matched, "missing configuration value %q", key)
		require.True(t, matched.GetSecret(), "configuration value %q is not secret", key)
		require.Empty(t, matched.GetValue(), "configuration value %q leaked data", key)
	}
	for _, value := range values {
		require.NotEqual(t, ownerConnectionKey, value.GetKey())
	}

	baseKustomization := readDeploymentFile(t, destination, "base", "kustomization.yaml")
	require.NotContains(t, baseKustomization, "namespace.yaml")
	overlayKustomization := readDeploymentFile(t, destination, "overlays", "test", "kustomization.yaml")
	require.NotContains(t, overlayKustomization, "secret.yaml")
	require.Empty(t, strings.TrimSpace(readDeploymentFile(t, destination, "base", "namespace.yaml")))
	require.Empty(t, strings.TrimSpace(readDeploymentFile(t, destination, "overlays", "test", "secret.yaml")))

	statefulSet := readDeploymentFile(t, destination, "base", "stateful-set.yaml")
	for _, expected := range []string{
		image.FullName(),
		"name: POSTGRES_USER",
		"name: POSTGRES_PASSWORD",
		"name: POSTGRES_DB",
		`value: "test"`,
		"optional: false",
	} {
		require.Contains(t, statefulSet, expected)
	}
	for _, unexpected := range []string{
		"envFrom:",
		"name: POSTGRES_READ_ONLY_PASSWORD",
		"name: POSTGRES_READ_WRITE_PASSWORD",
		"name: " + migrationConnectionEnvironmentKey,
		"name: UNRELATED_SECRET",
	} {
		require.NotContains(t, statefulSet, unexpected)
	}

	job := readDeploymentFile(t, destination, "base", "job.yaml")
	for _, expected := range []string{
		"registry.example.com/module/postgres@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"name: POSTGRES_USER",
		"name: POSTGRES_READ_ONLY_PASSWORD",
		"name: POSTGRES_READ_WRITE_PASSWORD",
		"name: POSTGRES_DB",
		`value: "test"`,
		"name: " + migrationConnectionEnvironmentKey,
		"optional: false",
	} {
		require.Contains(t, job, expected)
	}
	for _, unexpected := range []string{
		"envFrom:",
		"name: POSTGRES_PASSWORD",
		"name: UNRELATED_SECRET",
	} {
		require.NotContains(t, job, unexpected)
	}
}

func TestPromotableGitOpsDeploymentRejectsMissingOrOptionalRequiredSecretReferences(t *testing.T) {
	required := []string{
		"POSTGRES_USER",
		"POSTGRES_PASSWORD",
		"POSTGRES_READ_ONLY_PASSWORD",
		"POSTGRES_READ_WRITE_PASSWORD",
	}
	for _, environmentVariable := range required {
		t.Run("missing/"+environmentVariable, func(t *testing.T) {
			builder, networkMappings := newDeploymentTestBuilder(t)
			references := promotablePostgresSecretReferences()
			configurationKey := resources.ServiceSecretConfigurationKeyFromUnique(
				builder.Unique(),
				"postgres",
				environmentVariable,
			)
			delete(references, configurationKey)

			response, err := builder.Deploy(context.Background(), promotableDeploymentRequest(
				t.TempDir(),
				networkMappings,
				references,
				false,
			))
			require.NoError(t, err)
			require.Equal(t, builderv0.DeploymentStatus_ERROR, response.GetState().GetState())
			require.Contains(t, response.GetState().GetMessage(), "requires a typed Kubernetes Secret reference for "+configurationKey)
			require.Nil(t, response.GetConfiguration())
		})

		t.Run("optional/"+environmentVariable, func(t *testing.T) {
			builder, networkMappings := newDeploymentTestBuilder(t)
			references := promotablePostgresSecretReferences()
			configurationKey := resources.ServiceSecretConfigurationKeyFromUnique(
				builder.Unique(),
				"postgres",
				environmentVariable,
			)
			references[configurationKey].Optional = true

			response, err := builder.Deploy(context.Background(), promotableDeploymentRequest(
				t.TempDir(),
				networkMappings,
				references,
				false,
			))
			require.NoError(t, err)
			require.Equal(t, builderv0.DeploymentStatus_ERROR, response.GetState().GetState())
			require.Contains(t, response.GetState().GetMessage(), configurationKey+" Kubernetes Secret reference must not be optional")
			require.Nil(t, response.GetConfiguration())
		})
	}
}

func newDeploymentTestBuilder(t *testing.T) (*Builder, []*basev0.NetworkMapping) {
	t.Helper()
	ctx := context.Background()
	builder := NewBuilder()
	identity := &basev0.ServiceIdentity{
		Name:      "postgres",
		Module:    "module",
		Workspace: "workspace",
		Version:   "1.2.3",
	}
	require.NoError(t, builder.Base.HeadlessLoad(ctx, identity))
	builder.Base.Information = &services.Information{
		Service: resources.ToServiceWithCase(builder.Identity),
		Module:  resources.ToModuleWithCase(builder.Identity),
	}
	builder.EnvironmentVariables.SetIdentity(identity)
	builder.DatabaseName = "test"
	builder.TcpEndpoint = &basev0.Endpoint{
		Name:    "tcp",
		Module:  identity.Module,
		Service: identity.Name,
		Api:     "tcp",
	}
	instance := resources.NewNetworkInstance("postgres.example.com", 5432)
	instance.Access = resources.NewPublicNetworkAccess()
	return builder, []*basev0.NetworkMapping{{
		Endpoint:  builder.TcpEndpoint,
		Instances: []*basev0.NetworkInstance{instance},
	}}
}

func promotableDeploymentRequest(
	destination string,
	networkMappings []*basev0.NetworkMapping,
	secretReferences map[string]*builderv0.KubernetesSecretKeyReference,
	validateServerSide bool,
) *builderv0.DeploymentRequest {
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	return &builderv0.DeploymentRequest{
		Environment:     &basev0.Environment{Name: "test"},
		NetworkMappings: networkMappings,
		Deployment: &builderv0.Deployment{
			Kind: &builderv0.Deployment_Kubernetes{
				Kubernetes: &builderv0.KubernetesDeployment{
					Namespace:            "codefly-test",
					Destination:          destination,
					BuildContext:         &builderv0.DockerBuildContext{DockerRepository: "registry.example.com", ImageDigest: digest},
					Profile:              builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
					SecretReferences:     secretReferences,
					ValidateServerSide:   validateServerSide,
					ValidationKubeconfig: "/dev/null",
					ValidationContext:    "test-context",
				},
			},
		},
	}
}

func promotablePostgresSecretReferences() map[string]*builderv0.KubernetesSecretKeyReference {
	references := map[string]*builderv0.KubernetesSecretKeyReference{
		"UNRELATED_SECRET": {Name: "unrelated-secret", Key: "token"},
	}
	for _, environmentVariable := range []string{
		"POSTGRES_USER",
		"POSTGRES_PASSWORD",
		"POSTGRES_READ_ONLY_PASSWORD",
		"POSTGRES_READ_WRITE_PASSWORD",
	} {
		configurationKey := resources.ServiceSecretConfigurationKeyFromUnique(
			"module/postgres",
			"postgres",
			environmentVariable,
		)
		references[configurationKey] = &builderv0.KubernetesSecretKeyReference{
			Name: "postgres-secrets",
			Key:  configurationKey,
		}
	}
	return references
}

func useSuccessfulKubectl(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	kubectl := filepath.Join(bin, "kubectl")
	require.NoError(t, os.WriteFile(kubectl, []byte("#!/bin/sh\ncat >/dev/null\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func assertMigrationResource(t *testing.T, dir string, expected bool) {
	t.Helper()
	content := readDeploymentFile(t, dir, "base", "kustomization.yaml")
	if got := strings.Contains(content, "- job.yaml"); got != expected {
		t.Fatalf("migration resource present = %t, want %t:\n%s", got, expected, content)
	}
}

func assertEphemeralSecret(t *testing.T, dir string) {
	t.Helper()
	require.Contains(t, readDeploymentFile(t, dir, "base", "kustomization.yaml"), "- namespace.yaml")
	require.Contains(t, readDeploymentFile(t, dir, "base", "namespace.yaml"), "kind: Namespace")
	require.Contains(t, readDeploymentFile(t, dir, "overlays", "test", "kustomization.yaml"), "- secret.yaml")
	secret := readDeploymentFile(t, dir, "overlays", "test", "secret.yaml")
	require.Contains(t, secret, "kind: Secret")
	require.Contains(t, secret, "CODEFLY_TEST_SECRET: c2VjcmV0")
}

func readDeploymentFile(t *testing.T, directory string, elements ...string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(append([]string{directory}, elements...)...))
	require.NoError(t, err)
	return string(content)
}
