package migration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestPreviousRuntimeOptionsRequireRetainedRegularExecutable(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, runtimeRoot, previous, outside string)
		wantError  string
		wantResult bool
	}{
		{
			name: "retained regular executable",
			setup: func(t *testing.T, _, previous, _ string) {
				if err := os.MkdirAll(filepath.Join(previous, "bin"), 0o700); err != nil {
					t.Fatal(err)
				}
				writePowerMigrationFile(t, filepath.Join(previous, "bin", "yard-engine"), "engine\n", 0o700)
			},
			wantResult: true,
		},
		{
			name: "foreign previous target",
			setup: func(t *testing.T, runtimeRoot, _, outside string) {
				if err := os.MkdirAll(outside, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(runtimeRoot, "previous")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(runtimeRoot, "previous")); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "escapes the release store",
		},
		{
			name: "symbolic executable",
			setup: func(t *testing.T, _, previous, outside string) {
				writePowerMigrationFile(t, outside, "engine\n", 0o700)
				if err := os.MkdirAll(filepath.Join(previous, "bin"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(previous, "bin", "yard-engine")); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "not a regular file",
		},
		{
			name: "non-executable regular file",
			setup: func(t *testing.T, _, previous, _ string) {
				if err := os.MkdirAll(filepath.Join(previous, "bin"), 0o700); err != nil {
					t.Fatal(err)
				}
				writePowerMigrationFile(t, filepath.Join(previous, "bin", "yard-engine"), "engine\n", 0o600)
			},
			wantError: "not executable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			runtimeRoot := filepath.Join(root, "runtime")
			previous := filepath.Join(runtimeRoot, "releases", "previous-release")
			outside := filepath.Join(root, "outside-engine")
			if err := os.MkdirAll(previous, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("releases", "previous-release"),
				filepath.Join(runtimeRoot, "previous")); err != nil {
				t.Fatal(err)
			}
			test.setup(t, runtimeRoot, previous, outside)
			environment := []string{
				"SUBYARD_REPOSITORY_ROOT=/stale",
				"SUBYARD_CONFIG_DIR=/stale/config",
				"SUBYARD_POWER_MIGRATION_PAYLOAD_ROOT=/stale/payload",
				"MIGRATION_KEEP=unchanged",
			}
			originalEnvironment := append([]string(nil), environment...)
			options, err := previousRuntimeOptions(ReleaseOptions{
				RuntimeRoot: runtimeRoot, Environment: environment,
			})
			if strings.Join(environment, "\x00") != strings.Join(originalEnvironment, "\x00") {
				t.Fatalf("previous runtime mutated caller environment: %q", environment)
			}
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("previous runtime error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !test.wantResult || options.RepositoryRoot != previous ||
				options.Executable != filepath.Join(previous, "bin", "yard-engine") {
				t.Fatalf("previous runtime options = %#v", options)
			}
			joinedEnvironment := strings.Join(options.Environment, "\n")
			if strings.Contains(joinedEnvironment, "SUBYARD_POWER_MIGRATION_PAYLOAD_ROOT=") ||
				!strings.Contains(joinedEnvironment, "SUBYARD_REPOSITORY_ROOT="+previous) ||
				!strings.Contains(joinedEnvironment, "SUBYARD_CONFIG_DIR="+filepath.Join(previous, "config")) {
				t.Fatalf("previous runtime environment = %q", joinedEnvironment)
			}
		})
	}
}

func writePowerMigrationFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	testkit.WriteFile(t, path, []byte(contents), mode)
}
