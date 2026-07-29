package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseBackfillUsesRequestedTagWhenCommitHasMultipleTags(t *testing.T) {
	repository := t.TempDir()
	runTestCommand(t, repository, nil, "git", "init", "--quiet")
	runTestCommand(t, repository, nil, "git", "config", "user.name", "Test")
	runTestCommand(t, repository, nil, "git", "config", "user.email", "test@example.com")
	runTestCommand(t, repository, nil, "git", "config", "commit.gpgsign", "false")
	runTestCommand(t, repository, nil, "git", "config", "tag.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repository, "service"), []byte("postgres"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestCommand(t, repository, nil, "git", "add", "service")
	runTestCommand(t, repository, nil, "git", "commit", "--quiet", "-m", "release")
	runTestCommand(t, repository, []string{"GIT_COMMITTER_DATE=2026-07-01T00:00:00Z"}, "git", "tag", "-a", "v1.0.0", "-m", "v1.0.0")
	runTestCommand(t, repository, []string{"GIT_COMMITTER_DATE=2026-07-02T00:00:00Z"}, "git", "tag", "-a", "v1.0.1", "-m", "v1.0.1")
	runTestCommand(t, repository, nil, "git", "checkout", "--quiet", "--detach")

	if described := strings.TrimSpace(runTestCommand(t, repository, nil, "git", "describe", "--tags", "--exact-match")); described == "v1.0.0" {
		t.Fatalf("test requires git describe to select the other tag, got %s", described)
	}

	environmentFile := filepath.Join(t.TempDir(), "github-env")
	runReleaseBackfill(t, []string{
		"RELEASE_TAG=v1.0.0",
		"GITHUB_ENV=" + environmentFile,
	}, "validate-tag", repository)

	environment, err := os.ReadFile(environmentFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(environment), "GORELEASER_CURRENT_TAG=v1.0.0\n"; got != want {
		t.Fatalf("GitHub environment = %q, want %q", got, want)
	}
}

func TestReleaseBackfillUploadsOnlyMissingAssetsToExistingRelease(t *testing.T) {
	dist := newReleaseDist(t)
	fixture := filepath.Join(t.TempDir(), "releases.json")
	if err := os.WriteFile(fixture, []byte(`[[
		{
			"tag_name": "v0.0.103",
			"draft": false,
			"assets": [{"name": "service-postgres_0.0.103_linux_amd64.tar.gz"}]
		}
	]]`), 0o644); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "gh.log")
	bin := fakeGitHubCLI(t)

	runReleaseBackfill(t, []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GH_LOG=" + log,
		"GH_RELEASES_FIXTURE=" + fixture,
		"GH_TOKEN=test-token",
		"GITHUB_REPOSITORY=codefly-dev/service-postgres",
		"RELEASE_TAG=v0.0.103",
	}, "publish", dist)

	calls := readCalls(t, log)
	if len(calls) != 2 {
		t.Fatalf("gh calls = %q, want API lookup and one upload", calls)
	}
	if !strings.HasPrefix(calls[1], "release upload v0.0.103 ") {
		t.Fatalf("second gh call = %q, want release upload", calls[1])
	}
	if strings.Contains(calls[1], "linux_amd64") {
		t.Fatalf("upload replaced an existing asset: %q", calls[1])
	}
	for _, missing := range []string{"darwin_arm64", "checksums.txt"} {
		if !strings.Contains(calls[1], missing) {
			t.Fatalf("upload omitted %s: %q", missing, calls[1])
		}
	}
}

func TestReleaseBackfillCreatesReleaseWhenMissing(t *testing.T) {
	dist := newReleaseDist(t)
	fixture := filepath.Join(t.TempDir(), "releases.json")
	if err := os.WriteFile(fixture, []byte(`[[]]`), 0o644); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "gh.log")
	bin := fakeGitHubCLI(t)

	runReleaseBackfill(t, []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GH_LOG=" + log,
		"GH_RELEASES_FIXTURE=" + fixture,
		"GH_TOKEN=test-token",
		"GITHUB_REPOSITORY=codefly-dev/service-postgres",
		"RELEASE_TAG=v0.0.103",
	}, "publish", dist)

	calls := readCalls(t, log)
	if len(calls) != 2 {
		t.Fatalf("gh calls = %q, want API lookup and release creation", calls)
	}
	if !strings.HasPrefix(calls[1], "release create v0.0.103 ") {
		t.Fatalf("second gh call = %q, want release create", calls[1])
	}
	for _, expected := range []string{"linux_amd64", "darwin_arm64", "checksums.txt", "--latest=false"} {
		if !strings.Contains(calls[1], expected) {
			t.Fatalf("release creation omitted %s: %q", expected, calls[1])
		}
	}
}

func TestReleaseBackfillCompletesExistingDraft(t *testing.T) {
	dist := newReleaseDist(t)
	fixture := filepath.Join(t.TempDir(), "releases.json")
	if err := os.WriteFile(fixture, []byte(`[[
		{
			"tag_name": "v0.0.103",
			"draft": true,
			"assets": []
		}
	]]`), 0o644); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "gh.log")
	bin := fakeGitHubCLI(t)

	runReleaseBackfill(t, []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GH_LOG=" + log,
		"GH_RELEASES_FIXTURE=" + fixture,
		"GH_TOKEN=test-token",
		"GITHUB_REPOSITORY=codefly-dev/service-postgres",
		"RELEASE_TAG=v0.0.103",
	}, "publish", dist)

	calls := readCalls(t, log)
	if len(calls) != 3 {
		t.Fatalf("gh calls = %q, want API lookup, asset upload, and draft publication", calls)
	}
	if !strings.HasPrefix(calls[1], "release upload v0.0.103 ") {
		t.Fatalf("second gh call = %q, want release upload", calls[1])
	}
	if got, want := calls[2], "release edit v0.0.103 --repo codefly-dev/service-postgres --draft=false --latest=false"; got != want {
		t.Fatalf("third gh call = %q, want %q", got, want)
	}
}

func newReleaseDist(t *testing.T) string {
	t.Helper()
	dist := t.TempDir()
	for name, contents := range map[string]string{
		"CHANGELOG.md":                                 "changes",
		"service-postgres_0.0.103_checksums.txt":       "checksums",
		"service-postgres_0.0.103_darwin_arm64.tar.gz": "darwin",
		"service-postgres_0.0.103_linux_amd64.tar.gz":  "linux",
	} {
		if err := os.WriteFile(filepath.Join(dist, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dist
}

func fakeGitHubCLI(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	source := `#!/usr/bin/env bash
set -euo pipefail
printf '%s ' "$@" >>"$GH_LOG"
printf '\n' >>"$GH_LOG"
if [[ "${1:-}" == "api" ]]; then
  cat "$GH_RELEASES_FIXTURE"
fi
`
	path := filepath.Join(bin, "gh")
	if err := os.WriteFile(path, []byte(source), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func readCalls(t *testing.T, path string) []string {
	t.Helper()
	output, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(output)), "\n")
}

func runReleaseBackfill(t *testing.T, environment []string, arguments ...string) string {
	t.Helper()
	command := exec.Command("bash", append([]string{".github/scripts/release-backfill.sh"}, arguments...)...)
	command.Env = append(os.Environ(), environment...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("release backfill failed: %v\n%s", err, output)
	}
	return string(output)
}

func runTestCommand(t *testing.T, directory string, environment []string, name string, arguments ...string) string {
	t.Helper()
	command := exec.Command(name, arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(), environment...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", name, err, output)
	}
	return string(output)
}
