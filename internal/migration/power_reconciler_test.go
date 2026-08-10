package migration

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPowerReconcilerOperationRefreshesInstalledRuntime(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "release")
	bin := filepath.Join(root, "bin")
	installed := filepath.Join(root, "installed")
	for _, directory := range []string{
		filepath.Join(repository, "bin"),
		filepath.Join(repository, "config", "systemd"),
		bin,
		installed,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	template, err := os.ReadFile(filepath.Join("..", "..", "config", "systemd",
		"subyard-power-reconcile.service.in"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(template, []byte("Restart=no")) ||
		!bytes.Contains(template, []byte("RestartForceExitStatus=75")) ||
		!bytes.Contains(template, []byte("RestartSec=10s")) ||
		!bytes.Contains(template, []byte("StartLimitIntervalSec=0")) ||
		bytes.Contains(template, []byte("StartLimitBurst=")) {
		t.Fatal("shipped power reconciler unit does not isolate temporary retries")
	}
	templatePath := filepath.Join(repository, "config", "systemd",
		"subyard-power-reconcile.service.in")
	writePowerMigrationFile(t, templatePath, string(template), 0o600)

	reconciler := filepath.Join(installed, "yard-boot-reconcile")
	unit := filepath.Join(installed, "subyard-power-reconcile.service")
	enabled := filepath.Join(installed, "enabled")
	executable := filepath.Join(repository, "bin", "yard-engine")
	writePowerMigrationFile(t, executable, `#!/bin/sh
set -eu
[ "$SUBYARD_INTERNAL_MIGRATION_CHILD" = 1 ]
[ "$*" = "_migrate reconcile-power-reconciler" ]
/usr/bin/cp "$SUBYARD_REPOSITORY_ROOT/bin/yard-engine" "$SUBYARD_POWER_RECONCILER_PATH"
/usr/bin/sed "s|@SUBYARD_POWER_RECONCILER@|$SUBYARD_POWER_RECONCILER_PATH|g" \
  "$SUBYARD_REPOSITORY_ROOT/config/systemd/subyard-power-reconcile.service.in" \
  > "$SUBYARD_POWER_UNIT_PATH"
: > "$POWER_ENABLED"
`, 0o700)
	writePowerMigrationFile(t, reconciler, "stale\n", 0o700)
	writePowerMigrationFile(t, unit, "stale\n", 0o600)
	writePowerMigrationFile(t, filepath.Join(bin, "systemctl"), `#!/bin/sh
[ "$*" = "is-enabled --quiet subyard-power-reconcile.service" ]
[ -f "$POWER_ENABLED" ]
`, 0o700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	var output bytes.Buffer
	options := ReleaseOptions{
		RepositoryRoot: repository,
		Executable:     executable,
		Environment: []string{
			"PATH=" + bin + ":" + os.Getenv("PATH"),
			"SUBYARD_POWER_RECONCILER_PATH=" + reconciler,
			"SUBYARD_POWER_UNIT_PATH=" + unit,
			"POWER_ENABLED=" + enabled,
		},
		Diagnostics: &output,
		Stderr:      &output,
	}
	before, err := preparePowerReconciler(options)
	if err != nil || before != powerReconcilerInstalled {
		t.Fatalf("prepared power reconciler = %q, %v", before, err)
	}
	if err := verifyPowerReconciler(context.Background(), options, before); err == nil {
		t.Fatal("stale power reconciler passed release verification")
	}
	if err := commitPowerReconciler(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	if err := verifyPowerReconciler(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "updated host power reconciler") {
		t.Fatalf("power reconciler refresh was not reported: %q", output.String())
	}
}

func TestPowerReconcilerOperationLeavesAbsentInstallAbsent(t *testing.T) {
	root := t.TempDir()
	options := ReleaseOptions{Environment: []string{
		"SUBYARD_POWER_RECONCILER_PATH=" + filepath.Join(root, "yard-boot-reconcile"),
		"SUBYARD_POWER_UNIT_PATH=" + filepath.Join(root, "subyard-power-reconcile.service"),
	}}
	before, err := preparePowerReconciler(options)
	if err != nil || before != powerReconcilerAbsent {
		t.Fatalf("prepared absent power reconciler = %q, %v", before, err)
	}
	if err := commitPowerReconciler(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
}

func TestPowerReconcilerRollbackRunsCandidateActionWithPreviousPayload(t *testing.T) {
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	runtimeRoot := filepath.Join(root, "runtime")
	previous := filepath.Join(runtimeRoot, "releases", "previous-release")
	installed := filepath.Join(root, "installed")
	bin := filepath.Join(root, "bin")
	for _, directory := range []string{
		filepath.Join(candidate, "bin"),
		filepath.Join(previous, "bin"),
		filepath.Join(previous, "config", "systemd"),
		installed,
		bin,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(
		filepath.Join("releases", "previous-release"),
		filepath.Join(runtimeRoot, "previous"),
	); err != nil {
		t.Fatal(err)
	}

	reconciler := filepath.Join(installed, "yard-boot-reconcile")
	unit := filepath.Join(installed, "subyard-power-reconcile.service")
	enabled := filepath.Join(installed, "enabled")
	runner := filepath.Join(candidate, "bin", "yard-engine")
	previousEngine := filepath.Join(previous, "bin", "yard-engine")
	writePowerMigrationFile(t, runner, `#!/bin/sh
set -eu
[ "$*" = "_migrate reconcile-power-reconciler" ]
/usr/bin/cp "$SUBYARD_REPOSITORY_ROOT/bin/yard-engine" "$SUBYARD_POWER_RECONCILER_PATH"
/usr/bin/sed "s|@SUBYARD_POWER_RECONCILER@|$SUBYARD_POWER_RECONCILER_PATH|g" \
  "$SUBYARD_REPOSITORY_ROOT/config/systemd/subyard-power-reconcile.service.in" \
  > "$SUBYARD_POWER_UNIT_PATH"
: > "$POWER_ENABLED"
`, 0o700)
	writePowerMigrationFile(t, previousEngine, "previous engine\n", 0o700)
	writePowerMigrationFile(t, filepath.Join(previous, "config", "systemd",
		"subyard-power-reconcile.service.in"),
		"[Service]\nExecStart=@SUBYARD_POWER_RECONCILER@ _power-reconcile\n", 0o600)
	writePowerMigrationFile(t, reconciler, "candidate engine\n", 0o700)
	writePowerMigrationFile(t, unit, "candidate unit\n", 0o600)
	writePowerMigrationFile(t, filepath.Join(bin, "systemctl"), `#!/bin/sh
[ "$*" = "is-enabled --quiet subyard-power-reconcile.service" ]
[ -f "$POWER_ENABLED" ]
`, 0o700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	options := ReleaseOptions{
		RepositoryRoot: candidate,
		RuntimeRoot:    runtimeRoot,
		Executable:     runner,
		Environment: []string{
			"PATH=" + bin + ":" + os.Getenv("PATH"),
			"SUBYARD_POWER_RECONCILER_PATH=" + reconciler,
			"SUBYARD_POWER_UNIT_PATH=" + unit,
			"POWER_ENABLED=" + enabled,
		},
	}
	if err := rollbackPowerReconciler(
		context.Background(), options, powerReconcilerInstalled,
	); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(reconciler)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != "previous engine\n" {
		t.Fatalf("rolled-back reconciler = %q", actual)
	}
}

func writePowerMigrationFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}
