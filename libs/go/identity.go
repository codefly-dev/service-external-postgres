package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AccessTokenProvider returns a short-lived credential used as the Postgres
// password for external-identity authentication. principal is the DSN login
// role of the connection being opened, so a provider federating several
// identities can pick the right one; single-identity providers ignore it. It is
// invoked before each new backend connection, so it must return a
// currently-valid token; providers wrapping a remote issuer cache and refresh
// internally.
type AccessTokenProvider func(ctx context.Context, principal string) (string, error)

// WithAccessTokenProvider authenticates every pooled connection with a
// short-lived token from provider instead of a static password. The read-only
// and read-write DSNs must carry the login principal (user) but no password:
// this is external-identity mode, where managed cloud Postgres has password
// auth off and the cloud identity provider owns the principal and issues the
// token.
func WithAccessTokenProvider(provider AccessTokenProvider) Option {
	return func(configuration *config) error {
		if provider == nil {
			return errors.New("Postgres access-token provider must not be nil")
		}
		configuration.accessTokenProvider = provider
		return nil
	}
}

// installAccessTokenProvider makes poolConfig resolve its password from provider
// on every new connection. pgx invokes BeforeConnect per physical connection, so
// token rotation is picked up as the pool grows or reconnects.
func installAccessTokenProvider(poolConfig *pgxpool.Config, provider AccessTokenProvider) {
	poolConfig.BeforeConnect = func(ctx context.Context, connConfig *pgx.ConnConfig) error {
		token, err := provider(ctx, connConfig.User)
		if err != nil {
			return err
		}
		connConfig.Password = token
		return nil
	}
}
