package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fakeTokenCredential struct {
	token string
	err   error
	scope string
}

func (f *fakeTokenCredential) GetToken(_ context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	if len(options.Scopes) == 1 {
		f.scope = options.Scopes[0]
	}
	if f.err != nil {
		return azcore.AccessToken{}, f.err
	}
	return azcore.AccessToken{Token: f.token}, nil
}

func TestAccessTokenProviderFromCredential(t *testing.T) {
	credential := &fakeTokenCredential{token: "entra-token"}
	provider := accessTokenProviderFromCredential(credential)
	token, err := provider(context.Background(), "app_reader")
	if err != nil {
		t.Fatal(err)
	}
	if token != "entra-token" {
		t.Fatalf("token = %q, want entra-token", token)
	}
	if credential.scope != azureEntraScope {
		t.Fatalf("scope = %q, want %q", credential.scope, azureEntraScope)
	}
}

func TestAccessTokenProviderFromCredentialFailsClosed(t *testing.T) {
	if _, err := accessTokenProviderFromCredential(&fakeTokenCredential{err: errors.New("boom")})(context.Background(), "app_reader"); err == nil {
		t.Fatal("credential error was swallowed")
	}
	if _, err := accessTokenProviderFromCredential(&fakeTokenCredential{token: ""})(context.Background(), "app_reader"); err == nil {
		t.Fatal("empty token was accepted")
	}
}

func TestWithAccessTokenProviderRejectsNil(t *testing.T) {
	if _, err := configured(WithAccessTokenProvider(nil)); err == nil {
		t.Fatal("nil access-token provider was accepted")
	}
	configuration, err := configured(WithAccessTokenProvider(func(context.Context, string) (string, error) { return "t", nil }))
	if err != nil {
		t.Fatal(err)
	}
	if configuration.accessTokenProvider == nil {
		t.Fatal("access-token provider was not stored")
	}
}

func TestInstallAccessTokenProviderSetsPasswordFromToken(t *testing.T) {
	poolConfig, err := pgxpool.ParseConfig("postgres://principal@example.com:5432/app?sslmode=require")
	if err != nil {
		t.Fatal(err)
	}
	var sawPrincipal string
	installAccessTokenProvider(poolConfig, func(_ context.Context, principal string) (string, error) {
		sawPrincipal = principal
		return "issued-token", nil
	})
	connConfig, err := pgx.ParseConfig("postgres://principal@example.com:5432/app?sslmode=require")
	if err != nil {
		t.Fatal(err)
	}
	if err := poolConfig.BeforeConnect(context.Background(), connConfig); err != nil {
		t.Fatal(err)
	}
	if sawPrincipal != "principal" {
		t.Fatalf("provider principal = %q, want the DSN login role", sawPrincipal)
	}
	if connConfig.Password != "issued-token" {
		t.Fatalf("password = %q, want issued-token", connConfig.Password)
	}
}

func TestInstallAccessTokenProviderPropagatesProviderError(t *testing.T) {
	poolConfig, err := pgxpool.ParseConfig("postgres://principal@example.com:5432/app?sslmode=require")
	if err != nil {
		t.Fatal(err)
	}
	installAccessTokenProvider(poolConfig, func(context.Context, string) (string, error) { return "", errors.New("no token") })
	connConfig, err := pgx.ParseConfig("postgres://principal@example.com:5432/app?sslmode=require")
	if err != nil {
		t.Fatal(err)
	}
	if err := poolConfig.BeforeConnect(context.Background(), connConfig); err == nil {
		t.Fatal("provider error did not abort the connection")
	}
}
