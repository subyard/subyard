package migration

import (
	"bytes"
	"context"
	"errors"
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
[ -z "${SUBYARD_POWER_MIGRATION_PAYLOAD_ROOT+x}" ]
/usr/bin/cp "$SUBYARD_REPOSITORY_ROOT/bin/yard-engine" "$SUBYARD_POWER_RECONCILER_PATH"
/usr/bin/sed "s|@SUBYARD_POWER_RECONCILER@|$SUBYARD_POWER_RECONCILER_PATH|g" \
  "$SUBYARD_REPOSITORY_ROOT/config/systemd/subyard-power-reconcile.service.in" \
  > "$SUBYARD_POWER_UNIT_PATH"
: > "$POWER_ENABLED"
`, 0o700)
	writePowerMigrationFile(t, reconciler, "stale\n", 0o700)
	writePowerMigrationFile(t, unit, "stale\n", 0o600)
	writePowerMigrationFile(t, filepath.Join(bin, "systemctl"), `#!/bin/sh
case "$*" in
  "show subyard-power-reconcile.service --property=LoadState --property=NeedDaemonReload")
    printf 'LoadState=loaded\nNeedDaemonReload=no\n'
    ;;
  "is-enabled --quiet subyard-power-reconcile.service") [ -f "$POWER_ENABLED" ] ;;
  *) exit 2 ;;
esac
`, 0o700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	var output bytes.Buffer
	options := ReleaseOptions{
		RepositoryRoot: repository,
		Executable:     executable,
		Environment: []string{
			"PATH=" + bin + ":" + os.Getenv("PATH"),
			"SUBYARD_POWER_MIGRATION_PAYLOAD_ROOT=/stale/inherited/payload",
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
	if err := commitPowerReconciler(context.Background(), options, before); err != nil {
		t.Fatalf("repeated power reconciler refresh: %v", err)
	}
	if err := verifyPowerReconciler(context.Background(), options, before); err != nil {
		t.Fatalf("verify repeated power reconciler refresh: %v", err)
	}
	if strings.Count(output.String(), "updated host power reconciler") != 2 {
		t.Fatalf("power reconciler refresh was not reported: %q", output.String())
	}
}

func TestPowerReconcilerOperationLeavesAbsentInstallAbsent(t *testing.T) {
	root := t.TempDir()
	reconciler := filepath.Join(root, "yard-boot-reconcile")
	unit := filepath.Join(root, "subyard-power-reconcile.service")
	options := ReleaseOptions{Environment: []string{
		"SUBYARD_POWER_RECONCILER_PATH=" + reconciler,
		"SUBYARD_POWER_UNIT_PATH=" + unit,
	}}
	before, err := preparePowerReconciler(options)
	if err != nil || before != powerReconcilerAbsent {
		t.Fatalf("prepared absent power reconciler = %q, %v", before, err)
	}
	if err := commitPowerReconciler(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{reconciler, unit} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("absent power reconciler path %q after commit: %v", path, err)
		}
	}
}

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
				writePowerMigrationFile(
					t,
					filepath.Join(previous, "bin", "yard-engine"),
					"engine\n",
					0o700,
				)
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
				if err := os.Symlink(outside,
					filepath.Join(previous, "bin", "yard-engine")); err != nil {
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
				writePowerMigrationFile(
					t,
					filepath.Join(previous, "bin", "yard-engine"),
					"engine\n",
					0o600,
				)
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
				RuntimeRoot: runtimeRoot,
				Environment: environment,
			})
			if strings.Join(environment, "\x00") !=
				strings.Join(originalEnvironment, "\x00") {
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
				!strings.Contains(joinedEnvironment, "SUBYARD_CONFIG_DIR="+
					filepath.Join(previous, "config")) {
				t.Fatalf("previous runtime environment = %q", joinedEnvironment)
			}
		})
	}
}

func TestPowerReconcilerVerificationRequiresFreshLoadedManagerState(t *testing.T) {
	tests := []struct {
		name       string
		loadState  string
		needReload string
		wantError  bool
	}{
		{name: "bad setting", loadState: "bad-setting", needReload: "no", wantError: true},
		{name: "daemon reload pending", loadState: "loaded", needReload: "yes", wantError: true},
		{name: "fresh loaded unit", loadState: "loaded", needReload: "no"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := powerReconcilerVerificationFixture(t, test.loadState, test.needReload)
			err := verifyPowerReconciler(
				context.Background(), options, powerReconcilerInstalled,
			)
			if (err != nil) != test.wantError {
				t.Fatalf("verify error = %v, want error %v", err, test.wantError)
			}
		})
	}
}

func powerReconcilerVerificationFixture(
	t *testing.T,
	loadState string,
	needReload string,
) ReleaseOptions {
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
	templatePath := filepath.Join(repository, "config", "systemd",
		"subyard-power-reconcile.service.in")
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

func TestPowerReconcilerRollbackSelectsRunnerFromOperationIdentityAndLayout(t *testing.T) {
	tests := []struct {
		name       string
		kind       string
		fromLayout int
		wantRunner string
	}{
		{
			name: "legacy kind from layout 1 uses candidate",
			kind: OperationKindPowerReconcilerRuntimeV1, fromLayout: 1,
			wantRunner: "candidate",
		},
		{
			name: "compatibility kind from layout 4 uses previous",
			kind: OperationKindPowerReconcilerSystemdCompatV1, fromLayout: 4,
			wantRunner: "previous",
		},
	}
	for fromLayout := 1; fromLayout <= 3; fromLayout++ {
		tests = append(tests, struct {
			name       string
			kind       string
			fromLayout int
			wantRunner string
		}{
			name:       "compatibility catch-up uses candidate from layout " + string(rune('0'+fromLayout)),
			kind:       OperationKindPowerReconcilerSystemdCompatV1,
			fromLayout: fromLayout,
			wantRunner: "candidate",
		})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := powerReconcilerRollbackFixture(t, test.wantRunner == "previous")
			if err := rollbackTypedOperation(
				context.Background(),
				fixture.options,
				Operation{ID: "power-reconciler", Kind: test.kind},
				powerReconcilerInstalled,
				test.fromLayout,
			); err != nil {
				t.Fatal(err)
			}
			assertFileContents(t, fixture.reconciler,
				string(mustReadPowerMigrationFile(t, fixture.previousEngine)))
			assertFileContents(t, fixture.unit,
				materializedPowerUnit(t, fixture.previousTemplate, fixture.reconciler))
			activeRepository := fixture.candidateRepository
			payloadOverride := "set"
			if test.wantRunner == "previous" {
				activeRepository = fixture.previousRepository
				payloadOverride = "unset"
			}
			assertFileContents(t, fixture.runnerCalls,
				test.wantRunner+"|"+activeRepository+"|"+fixture.previousRepository+"|"+
					payloadOverride+"\n")
		})
	}
}

func TestPowerReconcilerRollbackRequiresSettledPreviousManagerState(t *testing.T) {
	tests := []struct {
		name       string
		loadState  string
		needReload string
		wantError  bool
	}{
		{name: "published bad setting is settled", loadState: "bad-setting", needReload: "no"},
		{name: "reload pending", loadState: "loaded", needReload: "yes", wantError: true},
		{name: "unit not found", loadState: "not-found", needReload: "no", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := powerReconcilerRollbackFixture(t, true)
			setPowerReconcilerEnvironment(
				fixture.options.Environment, "POWER_LOAD_STATE", test.loadState,
			)
			setPowerReconcilerEnvironment(
				fixture.options.Environment, "POWER_NEED_RELOAD", test.needReload,
			)

			err := rollbackTypedOperation(
				context.Background(),
				fixture.options,
				Operation{
					ID:   "power-reconciler",
					Kind: OperationKindPowerReconcilerSystemdCompatV1,
				},
				powerReconcilerInstalled,
				4,
			)
			if (err != nil) != test.wantError {
				t.Fatalf("rollback error = %v, want error %v", err, test.wantError)
			}
			if test.wantError {
				return
			}
			assertFileContents(t, fixture.reconciler,
				string(mustReadPowerMigrationFile(t, fixture.previousEngine)))
			assertFileContents(t, fixture.unit,
				materializedPowerUnit(t, fixture.previousTemplate, fixture.reconciler))
		})
	}
}

func TestPowerReconcilerRollbackRejectsUnsupportedCompatibilityLayout(t *testing.T) {
	fixture := powerReconcilerRollbackFixture(t, false)
	err := rollbackTypedOperation(
		context.Background(),
		fixture.options,
		Operation{
			ID:   "power-reconciler",
			Kind: OperationKindPowerReconcilerSystemdCompatV1,
		},
		powerReconcilerInstalled,
		5,
	)
	if err == nil || !strings.Contains(err.Error(), "unsupported compatibility layout") {
		t.Fatalf("unsupported compatibility rollback error = %v", err)
	}
}

func TestPowerReconcilerCatchUpRollbackRejectsBadSettingManagerState(t *testing.T) {
	tests := []struct {
		name       string
		kind       string
		fromLayout int
	}{
		{
			name: "legacy operation",
			kind: OperationKindPowerReconcilerRuntimeV1, fromLayout: 1,
		},
	}
	for fromLayout := 1; fromLayout <= 3; fromLayout++ {
		tests = append(tests, struct {
			name       string
			kind       string
			fromLayout int
		}{
			name:       "compatibility catch-up from layout " + string(rune('0'+fromLayout)),
			kind:       OperationKindPowerReconcilerSystemdCompatV1,
			fromLayout: fromLayout,
		})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := powerReconcilerRollbackFixture(t, false)
			setPowerReconcilerEnvironment(
				fixture.options.Environment, "POWER_LOAD_STATE", "bad-setting",
			)
			setPowerReconcilerEnvironment(
				fixture.options.Environment, "POWER_NEED_RELOAD", "no",
			)

			err := rollbackTypedOperation(
				context.Background(),
				fixture.options,
				Operation{ID: "power-reconciler", Kind: test.kind},
				powerReconcilerInstalled,
				test.fromLayout,
			)
			if err == nil || !strings.Contains(err.Error(), "manager state is stale") {
				t.Fatalf("bad-setting catch-up rollback error = %v", err)
			}
		})
	}
}

func setPowerReconcilerEnvironment(environment []string, name, value string) {
	prefix := name + "="
	for index, assignment := range environment {
		if strings.HasPrefix(assignment, prefix) {
			environment[index] = prefix + value
			return
		}
	}
}

type powerReconcilerRollbackTestFixture struct {
	options             ReleaseOptions
	reconciler          string
	unit                string
	candidateRepository string
	previousRepository  string
	previousEngine      string
	previousTemplate    string
	runnerCalls         string
}

func powerReconcilerRollbackFixture(
	t *testing.T,
	previousSupportsAction bool,
) powerReconcilerRollbackTestFixture {
	t.Helper()
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
	runnerCalls := filepath.Join(root, "runner-calls")
	candidateEngine := filepath.Join(candidate, "bin", "yard-engine")
	previousEngine := filepath.Join(previous, "bin", "yard-engine")
	writePowerMigrationFile(t, candidateEngine, powerReconcilerRunnerScript("candidate", true), 0o700)
	writePowerMigrationFile(t, previousEngine,
		powerReconcilerRunnerScript("previous", previousSupportsAction), 0o700)
	previousTemplate := filepath.Join(previous, "config", "systemd",
		"subyard-power-reconcile.service.in")
	copyPowerMigrationFile(t,
		filepath.Join("..", "..", "tests", "fixtures", "systemd",
			"subyard-power-reconcile-v0.7.2.service.in"),
		previousTemplate,
		0o600,
	)
	writePowerMigrationFile(t, reconciler, "candidate engine\n", 0o700)
	writePowerMigrationFile(t, unit, "candidate unit\n", 0o600)
	writePowerMigrationFile(t, filepath.Join(bin, "systemctl"), `#!/bin/sh
case "$*" in
  "show subyard-power-reconcile.service --property=LoadState --property=NeedDaemonReload")
    printf 'LoadState=%s\nNeedDaemonReload=%s\n' "$POWER_LOAD_STATE" "$POWER_NEED_RELOAD"
    ;;
  "is-enabled --quiet subyard-power-reconcile.service") [ -f "$POWER_ENABLED" ] ;;
  *) exit 2 ;;
esac
`, 0o700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	return powerReconcilerRollbackTestFixture{
		options: ReleaseOptions{
			RepositoryRoot: candidate,
			RuntimeRoot:    runtimeRoot,
			Executable:     candidateEngine,
			Environment: []string{
				"PATH=" + bin + ":" + os.Getenv("PATH"),
				"SUBYARD_POWER_RECONCILER_PATH=" + reconciler,
				"SUBYARD_POWER_UNIT_PATH=" + unit,
				"POWER_ENABLED=" + enabled,
				"POWER_LOAD_STATE=loaded",
				"POWER_NEED_RELOAD=no",
				"RUNNER_CALLS=" + runnerCalls,
			},
		},
		reconciler:          reconciler,
		unit:                unit,
		candidateRepository: candidate,
		previousRepository:  previous,
		previousEngine:      previousEngine,
		previousTemplate:    previousTemplate,
		runnerCalls:         runnerCalls,
	}
}

func powerReconcilerRunnerScript(name string, supportsAction bool) string {
	action := "exit 64"
	if supportsAction {
		action = `payload="${SUBYARD_POWER_MIGRATION_PAYLOAD_ROOT:-$SUBYARD_REPOSITORY_ROOT}"
/usr/bin/cp "$payload/bin/yard-engine" "$SUBYARD_POWER_RECONCILER_PATH"
/usr/bin/sed "s|@SUBYARD_POWER_RECONCILER@|$SUBYARD_POWER_RECONCILER_PATH|g" \
  "$payload/config/systemd/subyard-power-reconcile.service.in" \
  > "$SUBYARD_POWER_UNIT_PATH"
: > "$POWER_ENABLED"`
	}
	return `#!/bin/sh
set -eu
[ "$*" = "_migrate reconcile-power-reconciler" ]
payload="${SUBYARD_POWER_MIGRATION_PAYLOAD_ROOT:-$SUBYARD_REPOSITORY_ROOT}"
override=unset
[ -z "${SUBYARD_POWER_MIGRATION_PAYLOAD_ROOT+x}" ] || override=set
printf '` + name + `|%s|%s|%s\n' "$SUBYARD_REPOSITORY_ROOT" "$payload" "$override" >> "$RUNNER_CALLS"
` + action + `
`
}

func mustReadPowerMigrationFile(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func copyPowerMigrationFile(t *testing.T, source, destination string, mode os.FileMode) {
	t.Helper()
	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	writePowerMigrationFile(t, destination, string(contents), mode)
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
