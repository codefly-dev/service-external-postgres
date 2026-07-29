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
)

func TestDeploymentTemplatesWithMigration(t *testing.T) {
	dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, DeploymentTemplateParameters{
		WithBootstrap: true,
		ManagedImage:  image.FullName(),
	})
	assertMigrationResource(t, dir, true)
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

func assertMigrationResource(t *testing.T, dir string, expected bool) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, "base", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Contains(string(content), "- job.yaml"); got != expected {
		t.Fatalf("migration resource present = %t, want %t:\n%s", got, expected, content)
	}
}
