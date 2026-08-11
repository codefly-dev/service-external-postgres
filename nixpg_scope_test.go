package main

import "testing"

func TestNixPostgresStateKeyPreservesUnscopedIdentity(t *testing.T) {
	const location = "/workspace/modules/infra/services/postgres"
	if got := nixPostgresStateKey(location, ""); got != location {
		t.Fatalf("unscoped state key = %q, want existing location %q", got, location)
	}
}

func TestNixPostgresStateKeySeparatesNamingScopes(t *testing.T) {
	const location = "/workspace/modules/infra/services/postgres"
	first := nixPostgresStateKey(location, "qualification-a")
	repeated := nixPostgresStateKey(location, "qualification-a")
	second := nixPostgresStateKey(location, "qualification-b")

	if first != repeated {
		t.Fatalf("same naming scope produced unstable keys %q and %q", first, repeated)
	}
	if first == second {
		t.Fatalf("different naming scopes shared state key %q", first)
	}
	if serviceHash(first) == serviceHash(second) {
		t.Fatal("different naming scopes shared the same runtime-root hash")
	}
}
