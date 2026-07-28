package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseRuntimeImageLock(t *testing.T) {
	got, err := parseRuntimeImageLock([]byte(`{
		"name": "ghcr.io/codefly-dev/service-postgres",
		"tag": "postgres-17.10-pgvector-0.8.5-alpine3.24",
		"digest": "sha256:a5bb05518fd2f054884282f389577028c6304337bcf9d65363810ef1ad9e8c6c"
	}`))
	require.NoError(t, err)
	require.Equal(t,
		"ghcr.io/codefly-dev/service-postgres@sha256:a5bb05518fd2f054884282f389577028c6304337bcf9d65363810ef1ad9e8c6c",
		got.FullName(),
	)
	require.Equal(t, "postgres-17.10-pgvector-0.8.5-alpine3.24", got.Tag)
}

func TestParseRuntimeImageLockRejectsIncompleteReference(t *testing.T) {
	_, err := parseRuntimeImageLock([]byte(`{
		"name": "ghcr.io/codefly-dev/service-postgres",
		"tag": "postgres-17.10-pgvector-0.8.5-alpine3.24"
	}`))
	require.EqualError(t, err, "runtime image digest is required")
}

func TestDefaultImageMatchesRuntimeImageLock(t *testing.T) {
	lock, err := os.ReadFile("runtime-image.json")
	require.NoError(t, err)
	expected, err := parseRuntimeImageLock(lock)
	require.NoError(t, err)
	require.Equal(t, expected, image)
}
