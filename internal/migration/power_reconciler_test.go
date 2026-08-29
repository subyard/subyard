package migration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestPowerReconcilerVerificationRequiresFreshLoadedManagerState(t *testing.T) {
	for _, test := range []struct {
		name       string
		loadState  string
		needReload string
		wantError  bool
	}{
		{name: "bad setting", loadState: "bad-setting", needReload: "no", wantError: true},
		{name: "daemon reload pending", loadState: "loaded", needReload: "yes", wantError: true},
		{name: "fresh loaded unit", loadState: "loaded", needReload: "no"},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := powerReconcilerVerificationFixture(t, test.loadState, test.needReload)
			err := verifyPowerReconciler(context.Background(), options, powerReconcilerInstalled)
			if (err != nil) != test.wantError {
				t.Fatalf("verify error = %v, want error %v", err, test.wantError)
			}
		})
	}
}

func powerReconcilerVerificationFixture(t *testing.T, loadState, needReload string) ReleaseOptions {
	t.Helper()
	root := t.TempDir()
	repository := filepath.Join(root, "release")
	installed := filepath.Join(root, "installed")
	bin := filepath.Join(root, "bin")
	for _, directory := range []string{
		filepath.Join(repository, "bin"),
		filepath.Join(repository, "config", "systemd"),
		installed,
		bin,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	reconciler := filepath.Join(installed, "yard-boot-reconcile")
	unit := filepath.Join(installed, "subyard-power-reconcile.service")
	executable := filepath.Join(repository, "bin", "yard-engine")
	writePowerMigrationFile(t, executable, "engine\n", 0o700)
	writePowerMigrationFile(t, reconciler, "engine\n", 0o700)
	templatePath := filepath.Join(repository, "config", "systemd", "subyard-power-reconcile.service.in")
	writePowerMigrationFile(t, templatePath,
		"[Service]\nExecStart=@SUBYARD_POWER_RECONCILER@ _power-reconcile\n", 0o600)
	writePowerMigrationFile(t, unit, materializedPowerUnit(t, templatePath, reconciler), 0o600)
	writePowerMigrationFile(t, filepath.Join(bin, "systemctl"), `#!/bin/sh
set -eu
case "$*" in
  "show subyard-power-reconcile.service --property=LoadState --property=NeedDaemonReload")
    printf 'LoadState=%s\nNeedDaemonReload=%s\n' "$POWER_LOAD_STATE" "$POWER_NEED_RELOAD"
    ;;
  "is-enabled --quiet subyard-power-reconcile.service") ;;
  *) exit 2 ;;
esac
`, 0o700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return ReleaseOptions{
		RepositoryRoot: repository,
		Executable:     executable,
		Environment: []string{
			"PATH=" + bin + ":" + os.Getenv("PATH"),
			"SUBYARD_POWER_RECONCILER_PATH=" + reconciler,
			"SUBYARD_POWER_UNIT_PATH=" + unit,
			"POWER_LOAD_STATE=" + loadState,
			"POWER_NEED_RELOAD=" + needReload,
		},
	}
}

func materializedPowerUnit(t *testing.T, templatePath, reconciler string) string {
	t.Helper()
	template, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(template), "@SUBYARD_POWER_RECONCILER@", reconciler)
}

func writePowerMigrationFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}
