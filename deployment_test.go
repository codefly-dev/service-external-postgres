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

func TestPromotableGitOpsDeploymentReturnsRequestedProfile(t *testing.T) {
	ctx := context.Background()
	builder := NewBuilder()
	identity := &basev0.ServiceIdentity{
		Name:      "postgres",
		Module:    "module",
		Workspace: "workspace",
	}
	require.NoError(t, builder.Base.HeadlessLoad(ctx, identity))
	builder.Base.Information = &services.Information{
		Service: resources.ToServiceWithCase(builder.Identity),
		Module:  resources.ToModuleWithCase(builder.Identity),
	}
	builder.EnvironmentVariables.SetIdentity(identity)

	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	destination := t.TempDir()
	references := map[string]*builderv0.KubernetesSecretKeyReference{
		"POSTGRES_USER": {
			Name: "postgres-runtime",
			Key:  "username",
		},
		"POSTGRES_PASSWORD": {
			Name: "postgres-runtime",
			Key:  "password",
		},
		"POSTGRES_DB": {
			Name: "postgres-runtime",
			Key:  "database",
		},
		"POSTGRES_READ_ONLY_PASSWORD": {
			Name: "postgres-runtime",
			Key:  "read-only-password",
		},
		"POSTGRES_READ_WRITE_PASSWORD": {
			Name: "postgres-runtime",
			Key:  "read-write-password",
		},
		migrationConnectionEnvironmentKey: {
			Name: "postgres-runtime",
			Key:  "migration-connection",
		},
	}
	response, err := builder.Deploy(ctx, &builderv0.DeploymentRequest{
		Environment: &basev0.Environment{Name: "test"},
		Deployment: &builderv0.Deployment{
			Kind: &builderv0.Deployment_Kubernetes{
				Kubernetes: &builderv0.KubernetesDeployment{
					Namespace:        "codefly-test",
					Destination:      destination,
					BuildContext:     &builderv0.DockerBuildContext{DockerRepository: "registry.example.com", ImageDigest: digest},
					Profile:          builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
					SecretReferences: references,
				},
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, builderv0.DeploymentStatus_ERROR, response.GetState().GetState())
	require.Contains(t, response.GetState().GetMessage(), "requires successful server-side validation")
	require.Nil(t, response.GetConfiguration())

	output := response.GetDeployment().GetKubernetes()
	require.Equal(t, builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1, output.GetProfile())
	require.Equal(t, services.KubernetesManifestContractVersion, output.GetContractVersion())
	require.Equal(t, builderv0.KubernetesManifestValidation_STATUS_PASSED, output.GetValidation().GetStaticValidation())
	require.Equal(t, builderv0.KubernetesManifestValidation_STATUS_NOT_RUN, output.GetValidation().GetServerSideValidation())

	baseKustomization := readDeploymentFile(t, destination, "base", "kustomization.yaml")
	require.NotContains(t, baseKustomization, "namespace.yaml")
	overlayKustomization := readDeploymentFile(t, destination, "overlays", "test", "kustomization.yaml")
	require.NotContains(t, overlayKustomization, "secret.yaml")
	require.Empty(t, strings.TrimSpace(readDeploymentFile(t, destination, "base", "namespace.yaml")))
	require.Empty(t, strings.TrimSpace(readDeploymentFile(t, destination, "overlays", "test", "secret.yaml")))

	statefulSet := readDeploymentFile(t, destination, "base", "stateful-set.yaml")
	require.Contains(t, statefulSet, image.FullName())
	require.Contains(t, statefulSet, "name: postgres-runtime")
	require.Contains(t, statefulSet, "key: password")
	require.NotContains(t, statefulSet, "envFrom:")

	job := readDeploymentFile(t, destination, "base", "job.yaml")
	require.Contains(t, job, "registry.example.com/module/postgres@"+digest)
	require.Contains(t, job, "name: postgres-runtime")
	require.Contains(t, job, "key: migration-connection")
	require.NotContains(t, job, "envFrom:")
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
