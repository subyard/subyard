package migration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func replaceTestEnvironment(environment []string, name, value string) []string {
	result := make([]string, 0, len(environment)+1)
	prefix := name + "="
	for _, assignment := range environment {
		if !strings.HasPrefix(assignment, prefix) {
			result = append(result, assignment)
		}
	}
	return append(result, prefix+value)
}

func typedReleaseMigrationFixture(
	t *testing.T,
	ttl string,
) (ReleaseOptions, string, string, string) {
	t.Helper()
	root := t.TempDir()
	configHome := filepath.Join(root, "config-home")
	dataHome := filepath.Join(root, "data-home")
	runtimeRoot := filepath.Join(root, "runtime")
	repositoryRoot := filepath.Join(runtimeRoot, "releases", "2.0.0-test-release")
	for _, directory := range []string{
		configHome,
		dataHome,
		filepath.Join(repositoryRoot, "config"),
		filepath.Join(runtimeRoot, "releases", "1.0.0-test-release"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	registry := Registry{
		SchemaVersion: 1,
		MinimumLayout: 1,
		CurrentLayout: 2,
		Migrations: []Definition{{
			ID:             "migrate-test-yard-owner",
			FromLayout:     1,
			ToLayout:       2,
			Resources:      []string{"test-yard-owner"},
			FinalizePolicy: orderedFinalizePolicy,
			RollbackPolicy: orderedRollbackPolicy,
			Operations: []Operation{{
				ID: "test-yard-owner", Kind: OperationKindTestYardOwnerV1,
			}},
		}},
	}
	registryPath := filepath.Join(repositoryRoot, "config", "migrations.json")
	writeRegistryFixture(t, registryPath, registry)
	if err := os.Symlink(
		filepath.Join("releases", "1.0.0-test-release"),
		filepath.Join(runtimeRoot, "current"),
	); err != nil {
		t.Fatal(err)
	}
	legacyRegistration := filepath.Join(configHome, "yards", "e2e-yard", "config.env")
	currentRegistration := filepath.Join(configHome, "yards", "test-yard", "config.env")
	writeMigrationFixture(t, legacyRegistration, "YARD_TEMPLATE=test-vms\n")
	writeMigrationFixture(t,
		filepath.Join(dataHome, "e2e", "controllers", "e2e-yard", ".operator-enrollment-v1"),
		"managed\n",
	)
	calls := filepath.Join(root, "calls")
	projectState := filepath.Join(root, "incus-state")
	writeMigrationFixture(t, projectState, "legacy\n")
	executable := filepath.Join(root, "yard")
	writeMigrationFixture(t, executable, `#!/bin/sh
set -eu
[ "$SUBYARD_INTERNAL_MIGRATION_CHILD" = 1 ]
printf '%s\n' "$*" >> "$MIGRATION_CALLS"
update_projects() {
  current="$(cat "$MIGRATION_INCUS_STATE")"
  case "$1:$current" in
    remove-legacy:legacy) printf 'none\n' > "$MIGRATION_INCUS_STATE" ;;
    remove-legacy:both) printf 'current\n' > "$MIGRATION_INCUS_STATE" ;;
    remove-current:current) printf 'none\n' > "$MIGRATION_INCUS_STATE" ;;
    remove-current:both) printf 'legacy\n' > "$MIGRATION_INCUS_STATE" ;;
    add-legacy:none) printf 'legacy\n' > "$MIGRATION_INCUS_STATE" ;;
    add-legacy:current) printf 'both\n' > "$MIGRATION_INCUS_STATE" ;;
    add-current:none) printf 'current\n' > "$MIGRATION_INCUS_STATE" ;;
    add-current:legacy) printf 'both\n' > "$MIGRATION_INCUS_STATE" ;;
  esac
}
case "$*" in
  "-Y e2e-yard teardown --yes")
    update_projects remove-legacy
    find "$MIGRATION_CONFIG_HOME/yards/e2e-yard/projects" -depth -delete 2>/dev/null || :
    ;;
  "-Y test-yard teardown --yes")
    [ "$SUBYARD_REPOSITORY_ROOT" = "$MIGRATION_CANDIDATE_REPOSITORY" ]
    update_projects remove-current
    ;;
  "-Y e2e-yard init --yes")
    update_projects add-legacy
    install -d -m 0700 "$MIGRATION_CONFIG_HOME/yards/e2e-yard/projects"
    : > "$MIGRATION_CONFIG_HOME/yards/e2e-yard/projects/.lock"
    chmod 0600 "$MIGRATION_CONFIG_HOME/yards/e2e-yard/projects/.lock"
    ;;
  "-Y test-yard init --yes")
    update_projects add-current
    install -d -m 0700 "$MIGRATION_CONFIG_HOME/yards/test-yard/projects"
    : > "$MIGRATION_CONFIG_HOME/yards/test-yard/projects/.lock"
    chmod 0600 "$MIGRATION_CONFIG_HOME/yards/test-yard/projects/.lock"
    ;;
esac
if [ "${3:-}" = test-vms ] && [ "${4:-}" = status ]; then
  printf 'ttl_remaining_seconds\t%s\n' "$MIGRATION_TTL"
fi
`)
	if err := os.Chmod(executable, 0o700); err != nil {
		t.Fatal(err)
	}
	previousExecutable := filepath.Join(
		runtimeRoot, "releases", "1.0.0-test-release", "bin", "yard-engine",
	)
	executablePayload, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	writeMigrationFixture(t, previousExecutable, string(executablePayload))
	if err := os.Chmod(previousExecutable, 0o700); err != nil {
		t.Fatal(err)
	}
	incus := filepath.Join(root, "incus")
	writeMigrationFixture(t, incus, `#!/bin/sh
set -eu
state="$(cat "$MIGRATION_INCUS_STATE")"
case "$*" in
  "list yard-e2e-yard --project subyard-e2e-yard --format=json")
    printf '[{"name":"yard-e2e-yard","status":"RUNNING"}]\n'
    ;;
  "project list --format=json")
    case "$state" in
      legacy) printf '[{"name":"subyard-e2e-yard"}]\n' ;;
      current) printf '[{"name":"subyard-test-yard"}]\n' ;;
      both) printf '[{"name":"subyard-e2e-yard"},{"name":"subyard-test-yard"}]\n' ;;
      none) printf '[]\n' ;;
    esac
    ;;
  "project get subyard-e2e-yard features.images")
    [ "$state" = legacy ] || [ "$state" = both ]
    printf 'false\n'
    ;;
  "project get subyard-test-yard features.images")
    [ "$state" = current ] || [ "$state" = both ]
    printf 'false\n'
    ;;
  "project create subyard-e2e-yard -c features.images=false")
    case "$state" in
      none) printf 'legacy\n' > "$MIGRATION_INCUS_STATE" ;;
      current) printf 'both\n' > "$MIGRATION_INCUS_STATE" ;;
    esac
    ;;
  "project create subyard-test-yard -c features.images=false")
    case "$state" in
      none) printf 'current\n' > "$MIGRATION_INCUS_STATE" ;;
      legacy) printf 'both\n' > "$MIGRATION_INCUS_STATE" ;;
    esac
    ;;
  "config set yard-e2e-yard user.subyard.desired_power running --project subyard-e2e-yard")
    printf 'desired-power|running\n' >> "$MIGRATION_CALLS"
    ;;
  *) exit 2 ;;
esac
`)
	if err := os.Chmod(incus, 0o700); err != nil {
		t.Fatal(err)
	}
	return ReleaseOptions{
		RegistryPath: registryPath, RepositoryRoot: repositoryRoot,
		RuntimeRoot: runtimeRoot, ConfigHome: configHome, DataHome: dataHome,
		Version: "2.0.0-test", Executable: executable, Incus: incus,
		Environment: append(os.Environ(),
			"MIGRATION_CALLS="+calls,
			"MIGRATION_CONFIG_HOME="+configHome,
			"MIGRATION_TTL="+ttl,
			"MIGRATION_INCUS_STATE="+projectState,
			"MIGRATION_CANDIDATE_REPOSITORY="+repositoryRoot,
			"SUBYARD_REPOSITORY_ROOT="+repositoryRoot,
			"SUBYARD_CONFIG_DIR="+filepath.Join(repositoryRoot, "config"),
		),
	}, legacyRegistration, currentRegistration, calls
}

func writeRegistryFixture(t *testing.T, path string, registry Registry) {
	t.Helper()
	payload, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func activateFixtureRelease(t *testing.T, options ReleaseOptions) {
	t.Helper()
	oldTarget, err := os.Readlink(filepath.Join(options.RuntimeRoot, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(options.RuntimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(options.RuntimeRoot, "previous"))
	if err := os.Symlink(
		filepath.Join("releases", filepath.Base(options.RepositoryRoot)),
		filepath.Join(options.RuntimeRoot, "current"),
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldTarget, filepath.Join(options.RuntimeRoot, "previous")); err != nil {
		t.Fatal(err)
	}
}

func writeMigrationFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
