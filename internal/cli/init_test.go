package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestPowerYardContextsAreDiscoveredWithoutChangingSelection(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	yardDirectory := filepath.Join(root, "state", "yards")
	if err := os.MkdirAll(yardDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(yardDirectory, "demo.env"), "SSH_PORT=2233\n", 0o600)
	writeCLIFile(t, filepath.Join(yardDirectory, "remote.env"),
		"ACCESS_KIND=remote\nREMOTE_DEST=owner.example\nREMOTE_YARD=default\n", 0o600)
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	yards, err := program.powerYardContexts(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if len(yards) != 2 || yards[0].YardName != "default" || yards[1].YardName != "demo" ||
		program.env["YARD_NAME"] != "" {
		t.Fatalf("power discovery changed selection or included remote yards: %#v", yards)
	}
}

func TestRealInitPlatformCarriesOnlyPreparedSudoContext(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	hasMarker := func() bool {
		platform, ok := program.initPlatform(
			loaded, []domain.Context{loaded.Context},
		).(reconcileruntime.Runtime)
		if !ok {
			t.Fatalf("unexpected real init platform %T", platform)
		}
		return slices.Contains(platform.Environment, "SUBYARD_SUDO_PREAUTHORIZED=1")
	}
	if hasMarker() {
		t.Fatal("real init platform fabricated the preauthorized sudo marker")
	}
	if err := program.prepareSudoPrivileges(
		context.Background(), io.Discard, 0, "init",
	); err != nil {
		t.Fatal(err)
	}
	if !hasMarker() {
		t.Fatal("real init platform omitted the successfully prepared sudo marker")
	}
}

type initPlatformFixture struct {
	converged        map[ports.ReconcileStageID]bool
	applied          []ports.ReconcileStageID
	preflightFresh   []bool
	preflightErr     error
	applyErr         error
	configs          int
	configsConverged bool
	configChecks     []bool
	teardowns        int
}

func (fixture *initPlatformFixture) ConfigsConverged(context.Context) (bool, error) {
	fixture.configChecks = append(fixture.configChecks, fixture.configsConverged)
	return fixture.configsConverged, nil
}

func newInitPlatformFixture() *initPlatformFixture {
	converged := make(map[ports.ReconcileStageID]bool)
	for _, id := range []ports.ReconcileStageID{
		ports.ReconcileStageIncus, ports.ReconcileStageProject, ports.ReconcileStageNetwork,
		ports.ReconcileStagePowerImport, ports.ReconcileStageInstance, ports.ReconcileStageMounts,
		ports.ReconcileStageProvision, ports.ReconcileStageTestVMs, ports.ReconcileStageSSH,
		ports.ReconcileStageGitIdentity, ports.ReconcileStageExtras, ports.ReconcileStagePower,
		ports.ReconcileStageKeys, ports.ReconcileStageSecurity,
	} {
		converged[id] = true
	}
	converged[ports.ReconcileStageProject] = false
	return &initPlatformFixture{converged: converged}
}

func TestParseInitProfile(t *testing.T) {
	request, err := parseInitArguments([]string{"--profile", "hermes", "--yes"})
	if err != nil || request.mode != initReconcile || request.profile != "hermes" {
		t.Fatalf("unexpected init request: %#v err=%v", request, err)
	}

	for _, test := range []struct {
		name      string
		arguments []string
		want      string
	}{
		{name: "missing value", arguments: []string{"--profile"}, want: "needs a value"},
		{name: "duplicate", arguments: []string{"--profile", "hermes", "--profile", "android"}, want: "only once"},
		{name: "unsafe", arguments: []string{"--profile", "../hermes"}, want: "invalid profile"},
		{name: "configs", arguments: []string{"--configs", "--profile", "hermes"}, want: "cannot be used together"},
		{name: "reset", arguments: []string{"--profile", "hermes", "--reset"}, want: "cannot be used together"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseInitArguments(test.arguments)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseInitArguments(%v) error=%v, want %q", test.arguments, err, test.want)
			}
		})
	}
}

func TestInitExecutionBuildsTypedActionDelta(t *testing.T) {
	for _, test := range []struct {
		name      string
		execution initExecution
		action    domain.ActionID
		changed   bool
	}{
		{name: "converged", execution: initExecution{mode: initReconcile}, action: "yard.init.reconcile"},
		{
			name: "pending reconcile",
			execution: initExecution{mode: initReconcile, plan: application.ReconcilePlan{
				Steps: []application.ReconcileStep{{Stage: application.ReconcileStage{ID: "fixture", Label: "apply fixture"}}},
			}},
			action: "yard.init.reconcile", changed: true,
		},
		{name: "pending host identity", execution: initExecution{mode: initReconcile, hostIDPending: true}, action: "yard.init.reconcile", changed: true},
		{name: "configs converged", execution: initExecution{mode: initConfigs}, action: "yard.init.configs"},
		{name: "configs changed", execution: initExecution{mode: initConfigs, configsChanged: true}, action: "yard.init.configs", changed: true},
		{name: "reset", execution: initExecution{mode: initReset}, action: "yard.init.reset", changed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			action, delta, err := test.execution.actionPlan()
			if err != nil || action != test.action || delta.Changed != test.changed {
				t.Fatalf("action=%q delta=%#v err=%v", action, delta, err)
			}
			if test.changed && len(delta.Consequences) == 0 {
				t.Fatal("changed init action has no consequences")
			}
		})
	}
}

func TestPrepareInitConfigsUsesReadOnlyConvergence(t *testing.T) {
	for _, test := range []struct {
		name      string
		converged bool
		changed   bool
	}{
		{name: "converged", converged: true},
		{name: "drifted", changed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			platform := newInitPlatformFixture()
			platform.configsConverged = test.converged
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Environment: environment,
				WorkingDir: root, InitPlatform: platform,
			})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			execution, err := program.prepareInitExecution(
				context.Background(), loaded, []string{"--configs"}, nil,
			)
			if err != nil || execution.configsChanged != test.changed {
				t.Fatalf("execution=%#v err=%v", execution, err)
			}
		})
	}
}

func TestInitConfigsRechecksConvergenceAfterConsent(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "state", "host-id"), "5034c950-74d0-46c4-9428-b7835e602109\n", 0o600)
	platform := newInitPlatformFixture()
	platform.configsConverged = false
	prompt := &callbackPrompt{callback: func() { platform.configsConverged = true }}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"init", "--configs"},
		Environment: environment, WorkingDir: root, InitPlatform: platform,
		Prompt: prompt, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("post-consent config no-op failed: code=%d stderr=%q", code, stderr.String())
	}
	if len(prompt.requests) != 1 {
		t.Fatalf("config action did not prompt once: %#v", prompt.requests)
	}
	if platform.configs != 0 {
		t.Fatalf("converged configs were refreshed: %d checks=%v", platform.configs, platform.configChecks)
	}
}

func withoutCommandSetting(environment []string, name string) []string {
	prefix := name + "="
	return slices.DeleteFunc(slices.Clone(environment), func(value string) bool {
		return strings.HasPrefix(value, prefix)
	})
}

func TestLoadInitProfileUsesShippedPresetForUnknownYard(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	environment = withoutCommandSetting(environment, "SSH_PORT")
	preset := filepath.Join(root, "config", "profiles", "hermes", "yard.env")
	if err := os.MkdirAll(filepath.Dir(preset), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "ENVIRONMENT_PROFILES=hermes\nAGENTS=codex\nSSH_PORT=2234\n"
	writeCLIFile(t, preset, content, 0o600)
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}

	loaded, bootstrap, err := program.loadInitContext(
		"custom-name", true, []string{"--profile", "hermes"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Context.YardName != "custom-name" || loaded.Context.SSHPort != 2234 ||
		loaded.Environment["ENVIRONMENT_PROFILES"] != "hermes" || loaded.Environment["CODING_TOOL_INTEGRATIONS"] != "codex" {
		t.Fatalf("profile preset was not loaded for selected yard: %#v %#v", loaded.Context, loaded.Environment)
	}
	target := filepath.Join(root, "state", "yards", "custom-name", "config.env")
	if bootstrap == nil || bootstrap.profile != "hermes" || bootstrap.sourcePath != preset ||
		bootstrap.targetPath != target || string(bootstrap.content) != content {
		t.Fatalf("unexpected bootstrap: %#v", bootstrap)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("bootstrap load mutated persistent definition: %v", err)
	}
}

func TestLoadInitProfileRejectsCommandOverridesOfPresetSettings(t *testing.T) {
	for _, test := range []struct {
		name     string
		override string
		want     string
	}{
		{name: "profiles", override: "ENVIRONMENT_PROFILES=openclaw", want: "ENVIRONMENT_PROFILES"},
		{name: "agents", override: "CODING_TOOL_INTEGRATIONS=claude", want: "CODING_TOOL_INTEGRATIONS"},
		{name: "host mounts", override: "HOST_MOUNTS=/tmp:/mnt/host:ro:0755", want: "HOST_MOUNTS"},
		{name: "host links", override: "HOST_LINKS=.claude/sessions:/mnt/host/agent-sessions/claude/sessions", want: "HOST_LINKS"},
		{name: "Claude instructions", override: "HOST_CLAUDE_MD=/tmp/CLAUDE.md", want: "HOST_CLAUDE_MD"},
		{name: "Codex instructions", override: "HOST_CODEX_AGENTS_MD=/tmp/AGENTS.md", want: "HOST_CODEX_AGENTS_MD"},
		{name: "OpenCode instructions", override: "HOST_OPENCODE_AGENTS_MD=/tmp/AGENTS.md", want: "HOST_OPENCODE_AGENTS_MD"},
		{name: "capabilities", override: "YARD_CAPABILITIES=android", want: "YARD_CAPABILITIES"},
		{name: "caps", override: "YARD_CAPS=fuse", want: "YARD_CAPS"},
		{name: "devices", override: "YARD_DEVICES=gpu", want: "YARD_DEVICES"},
		{name: "yard mounts", override: "YARD_MOUNTS=cache:/srv/cache:rw:0755", want: "YARD_MOUNTS"},
		{name: "SSH forwarding", override: "FORWARD_SSH_AGENT=1", want: "FORWARD_SSH_AGENT"},
		{name: "sudo", override: "DEV_SUDO=1", want: "DEV_SUDO"},
		{name: "nested VMs", override: "NESTED_E2E_VMS=1", want: "NESTED_E2E_VMS"},
		{name: "SSH port", override: "SSH_PORT=2299", want: "SSH_PORT"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			environment = withoutCommandSetting(environment, "SSH_PORT")
			if test.name == "host mounts" {
				hostSource := filepath.Join(root, "host", "fixture")
				if err := os.MkdirAll(hostSource, 0o700); err != nil {
					t.Fatal(err)
				}
				test.override = "HOST_MOUNTS=fixture:/mnt/host:ro:0755"
			}
			setting := strings.SplitN(test.override, "=", 2)[0] + "="
			environment = slices.DeleteFunc(environment, func(value string) bool {
				return strings.HasPrefix(value, setting)
			})
			environment = append(environment, test.override)
			preset := filepath.Join(root, "config", "profiles", "hermes", "yard.env")
			if err := os.MkdirAll(filepath.Dir(preset), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, preset, `ENVIRONMENT_PROFILES=hermes
CODING_TOOL_INTEGRATIONS=codex
HOST_MOUNTS=
HOST_LINKS=
HOST_CLAUDE_MD=
HOST_CODEX_AGENTS_MD=
HOST_OPENCODE_AGENTS_MD=
YARD_CAPABILITIES=
YARD_CAPS=
YARD_DEVICES=
YARD_MOUNTS=
FORWARD_SSH_AGENT=0
DEV_SUDO=0
NESTED_E2E_VMS=0
SSH_PORT=2234
`, 0o600)
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
			})
			if err != nil {
				t.Fatal(err)
			}

			_, bootstrap, err := program.loadInitContext(
				"custom-name", true, []string{"--profile", "hermes"},
			)
			if err == nil || !strings.Contains(err.Error(),
				"command environment overrides profile \"hermes\" at setting "+test.want) ||
				bootstrap != nil {
				t.Fatalf("profile command override: bootstrap=%#v err=%v", bootstrap, err)
			}
		})
	}
}

func TestLoadInitProfileAllowsMatchingCommandValue(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	environment = withoutCommandSetting(environment, "SSH_PORT")
	environment = slices.DeleteFunc(environment, func(value string) bool {
		return strings.HasPrefix(value, "CODING_TOOL_INTEGRATIONS=")
	})
	environment = append(environment, "CODING_TOOL_INTEGRATIONS=codex")
	preset := filepath.Join(root, "config", "profiles", "hermes", "yard.env")
	if err := os.MkdirAll(filepath.Dir(preset), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, preset, "ENVIRONMENT_PROFILES=hermes\nAGENTS=codex\nSSH_PORT=2234\n", 0o600)
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}

	loaded, bootstrap, err := program.loadInitContext(
		"custom-name", true, []string{"--profile", "hermes"},
	)
	if err != nil || bootstrap == nil || loaded.Environment["CODING_TOOL_INTEGRATIONS"] != "codex" {
		t.Fatalf("matching command value was rejected: loaded=%#v bootstrap=%#v err=%v",
			loaded.Environment, bootstrap, err)
	}
}

func TestInitProfileExistingDefinitionMustMatchPreset(t *testing.T) {
	for _, test := range []struct {
		name     string
		existing string
		wantErr  string
	}{
		{
			name: "semantic match",
			existing: "# locally documented\n" +
				"ENVIRONMENT_PROFILES='hermes'\nAGENTS=\"codex\"\nSSH_PORT=2234\n",
		},
		{
			name:     "conflict",
			existing: "ENVIRONMENT_PROFILES=hermes\nAGENTS=claude\nSSH_PORT=2234\n",
			wantErr:  "conflicts with profile \"hermes\" at setting CODING_TOOL_INTEGRATIONS",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			environment = withoutCommandSetting(environment, "SSH_PORT")
			preset := filepath.Join(root, "config", "profiles", "hermes", "yard.env")
			if err := os.MkdirAll(filepath.Dir(preset), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, preset, "ENVIRONMENT_PROFILES=hermes\nAGENTS=codex\nSSH_PORT=2234\n", 0o600)
			target := filepath.Join(root, "state", "yards", "custom-name", "config.env")
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, target, test.existing, 0o600)
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
			})
			if err != nil {
				t.Fatal(err)
			}

			loaded, bootstrap, err := program.loadInitContext(
				"custom-name", true, []string{"--profile", "hermes"},
			)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("loadInitContext error=%v, want %q", err, test.wantErr)
				}
				content, readErr := os.ReadFile(target)
				if readErr != nil || string(content) != test.existing {
					t.Fatalf("conflict changed definition: content=%q err=%v", content, readErr)
				}
				return
			}
			if err != nil || bootstrap != nil || loaded.Environment["CODING_TOOL_INTEGRATIONS"] != "codex" {
				t.Fatalf("matching definition was not reused: loaded=%#v bootstrap=%#v err=%v",
					loaded.Environment, bootstrap, err)
			}
		})
	}
}

func TestInitProfileReusesSupportedLegacyDefinition(t *testing.T) {
	for _, location := range []string{"private", "flat config home"} {
		t.Run(location, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			environment = withoutCommandSetting(environment, "SSH_PORT")
			preset := filepath.Join(root, "config", "profiles", "hermes", "yard.env")
			if err := os.MkdirAll(filepath.Dir(preset), 0o700); err != nil {
				t.Fatal(err)
			}
			content := "ENVIRONMENT_PROFILES=hermes\nAGENTS=codex\nSSH_PORT=2234\n"
			writeCLIFile(t, preset, content, 0o600)
			legacy := filepath.Join(root, "private", "yards", "custom-name.env")
			if location == "flat config home" {
				legacy = filepath.Join(root, "state", "yards", "custom-name.env")
			}
			if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, legacy, content, 0o600)
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
			})
			if err != nil {
				t.Fatal(err)
			}

			loaded, bootstrap, err := program.loadInitContext(
				"custom-name", true, []string{"--profile", "hermes"},
			)
			if err != nil || bootstrap != nil || loaded.Environment["CODING_TOOL_INTEGRATIONS"] != "codex" {
				t.Fatalf("legacy definition was not reused: loaded=%#v bootstrap=%#v err=%v",
					loaded.Environment, bootstrap, err)
			}
			canonical := filepath.Join(root, "state", "yards", "custom-name", "config.env")
			if _, err := os.Lstat(canonical); !os.IsNotExist(err) {
				t.Fatalf("legacy definition was shadowed by canonical file: %v", err)
			}
		})
	}
}

func TestInitProfileRejectsRemoteContextBeforeBootstrap(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	environment = withoutCommandSetting(environment, "SSH_PORT")
	preset := filepath.Join(root, "config", "profiles", "hermes", "yard.env")
	if err := os.MkdirAll(filepath.Dir(preset), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, preset, "ENVIRONMENT_PROFILES=hermes\nAGENTS=codex\nSSH_PORT=2234\n", 0o600)
	environment = append(environment, "ACCESS_KIND=remote", "OWNER_ENDPOINT=operator@example.test")
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, bootstrap, err := program.loadInitContext(
		"custom-name", true, []string{"--profile", "hermes"},
	)
	if err == nil || !strings.Contains(err.Error(), "only supported for local yards") || bootstrap != nil {
		t.Fatalf("remote profile bootstrap: bootstrap=%#v err=%v", bootstrap, err)
	}
	target := filepath.Join(root, "state", "yards", "custom-name", "config.env")
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("remote profile bootstrap wrote a definition: %v", err)
	}
}

func (fixture *initPlatformFixture) CheckStage(_ context.Context, stage ports.ReconcileStageID) (bool, error) {
	return fixture.converged[stage], nil
}

func (fixture *initPlatformFixture) ApplyStage(_ context.Context, stage ports.ReconcileStageID) error {
	if fixture.applyErr != nil {
		return fixture.applyErr
	}
	fixture.applied = append(fixture.applied, stage)
	fixture.converged[stage] = true
	return nil
}

func (fixture *initPlatformFixture) VerifyStage(_ context.Context, stage ports.ReconcileStageID) (bool, error) {
	return fixture.converged[stage], nil
}

func (fixture *initPlatformFixture) Preflight(_ context.Context, fresh bool) error {
	fixture.preflightFresh = append(fixture.preflightFresh, fresh)
	return fixture.preflightErr
}

func (fixture *initPlatformFixture) RefreshConfigs(context.Context) error {
	fixture.configs++
	return nil
}

func TestMigrationBrokerReconcileIsBoundedToTestVMStage(t *testing.T) {
	root := repositoryRoot(t)
	home := t.TempDir()
	configHome := filepath.Join(home, ".config", "subyard")
	yardDirectory := filepath.Join(configHome, "yards", "test-yard")
	if err := os.MkdirAll(yardDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(yardDirectory, "config.env"),
		"YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n", 0o600)
	environment := []string{
		"HOME=" + home,
		"SUBYARD_OPERATOR_HOME=" + home,
		"SUBYARD_CONFIG_HOME=" + configHome,
		"SUBYARD_HOME=" + filepath.Join(home, ".subyard"),
		"SUBYARD_NO_AUDIT=1",
	}

	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"-Y", "test-yard", "_migrate", "reconcile-test-vm-broker"},
		Environment: environment, WorkingDir: home, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code == 0 ||
		!strings.Contains(stderr.String(), "migration child is required") {
		t.Fatalf("broker reconcile accepted a public invocation: code=%d stderr=%q",
			code, stderr.String())
	}

	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	sudoLog := filepath.Join(home, "sudo.log")
	writeCLIFile(t, filepath.Join(bin, "sudo"), `#!/bin/sh
printf '%s\n' "$*" >> "$SUDO_LOG"
case "$*" in
  "-n true")
    [ -f "$SUDO_LOG.auth" ]
    ;;
  "-v")
    IFS= read -r input
    printf 'input=%s\n' "$input" >> "$SUDO_LOG"
    : > "$SUDO_LOG.auth"
    ;;
  *) exit 90 ;;
esac
`, 0o700)
	t.Setenv("PATH", bin)
	terminalPath := filepath.Join(home, "terminal")
	writeCLIFile(t, terminalPath, "migration-password\n", 0o600)
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{"-Y", "test-yard", "_migrate", "reconcile-test-vm-broker"},
		Environment: append(
			environment,
			"PATH="+bin,
			"SUDO_LOG="+sudoLog,
			"SUBYARD_INTERNAL_MIGRATION_CHILD=1",
		),
		WorkingDir: home, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	program.operatorTerminal = func() bool { return false }
	program.openTerminal = func() (*os.File, error) {
		return os.OpenFile(terminalPath, os.O_RDWR, 0)
	}
	program.effectiveUID = func() int { return 1000 }
	_ = program.Run(context.Background())
	sudoCalls, err := os.ReadFile(sudoLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(sudoCalls) != "-n true\n-v\ninput=migration-password\n-n true\n" ||
		strings.Contains(stderr.String(), "sudo authorization expired") {
		t.Fatalf("migration did not authorize sudo before its real platform: calls=%q stderr=%q",
			sudoCalls, stderr.String())
	}

	platform := newInitPlatformFixture()
	platform.converged[ports.ReconcileStageTestVMs] = false
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"-Y", "test-yard", "_migrate", "reconcile-test-vm-broker"},
		Environment: append(environment, "SUBYARD_INTERNAL_MIGRATION_CHILD=1"),
		WorkingDir:  home, Stderr: &stderr, InitPlatform: platform,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("broker reconcile failed: code=%d stderr=%q", code, stderr.String())
	}
	if !slices.Equal(platform.applied, []ports.ReconcileStageID{
		ports.ReconcileStageTestVMs,
	}) {
		t.Fatalf("migration broker reconcile applied stages %v", platform.applied)
	}
	if len(platform.preflightFresh) != 0 {
		t.Fatalf("migration broker reconcile ran host preflight: %v",
			platform.preflightFresh)
	}

	platform.applied = nil
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"-Y", "test-yard", "_migrate", "reconcile-test-vm-broker"},
		Environment: append(environment, "SUBYARD_INTERNAL_MIGRATION_CHILD=1"),
		WorkingDir:  home, Stderr: &stderr, InitPlatform: platform,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("converged broker reconcile failed: code=%d stderr=%q",
			code, stderr.String())
	}
	if len(platform.applied) != 0 {
		t.Fatalf("converged migration broker reapplied stages %v", platform.applied)
	}
}

func TestMigrationPowerReconcileIsBoundedToPowerStage(t *testing.T) {
	root := repositoryRoot(t)
	home := t.TempDir()
	environment := []string{
		"HOME=" + home,
		"SUBYARD_OPERATOR_HOME=" + home,
		"SUBYARD_CONFIG_HOME=" + filepath.Join(home, ".config", "subyard"),
		"SUBYARD_HOME=" + filepath.Join(home, ".subyard"),
		"SUBYARD_NO_AUDIT=1",
	}
	platform := newInitPlatformFixture()
	var stderr bytes.Buffer

	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"_migrate", "reconcile-power-reconciler"},
		Environment: environment, Stderr: &stderr, InitPlatform: platform,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code == 0 ||
		!strings.Contains(stderr.String(), "migration child is required") {
		t.Fatalf("power reconcile accepted a public invocation: code=%d stderr=%q",
			code, stderr.String())
	}

	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{"_migrate", "reconcile-power-reconciler"},
		Environment: append(
			environment,
			"SUBYARD_INTERNAL_MIGRATION_CHILD=1",
		),
		Stderr: &stderr, InitPlatform: platform,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("power reconcile failed: code=%d stderr=%q", code, stderr.String())
	}
	if !slices.Equal(platform.applied, []ports.ReconcileStageID{ports.ReconcileStagePower}) {
		t.Fatalf("power reconciler migration applied stages %v", platform.applied)
	}
	if len(platform.preflightFresh) != 0 {
		t.Fatalf("power reconciler migration ran host preflight: %v", platform.preflightFresh)
	}
}

func TestMigrationPowerReconcileUsesRetainedPayloadWithActiveCLI(t *testing.T) {
	active := repositoryRoot(t)
	home := t.TempDir()
	runtimeRoot := filepath.Join(home, "runtime")
	previous := filepath.Join(runtimeRoot, "releases", "legacy-release")
	if err := os.MkdirAll(filepath.Dir(previous), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(previous, "config"), os.DirFS(filepath.Join(active, "config"))); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(previous, "config", "commands.registry"),
		"legacy|manifest|with|thirteen|fields|that|the|active|parser|must|not|read|here\n",
		0o600)
	marker := filepath.Join(home, "retained-power-adapter")
	if err := os.MkdirAll(filepath.Join(previous, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	retainedAdapter := strings.NewReplacer(
		"@PREVIOUS@", previous,
		"@MARKER@", marker,
	).Replace(`#!/bin/sh
set -eu
[ "$*" = "--yes" ]
[ "$PWD" = "@PREVIOUS@" ]
[ "$SUBYARD_DISPATCHER_PATH" = "@PREVIOUS@/bin/yard-engine" ]
[ "$SUBYARD_POWER_ENGINE_SOURCE" = "@PREVIOUS@/bin/yard-engine" ]
[ "$ACCESS_KIND" = local ]
[ "$YARD_TYPE" = "$ACCESS_KIND" ]
[ "$INSTANCE_TYPE" = "$YARD_KIND" ]
[ "$INSTANCE_NAME" = "$YARD_INSTANCE_NAME" ]
[ "$REMOTE_DEST" = "$OWNER_ENDPOINT" ]
[ "$REMOTE_YARD" = "$OWNER_YARD_NAME" ]
[ "$BASE_IMAGE" = "$YARD_IMAGE" ]
[ "$BASE_IMAGE_FALLBACK" = "$YARD_IMAGE_FALLBACK" ]
[ "$YARD_PROFILES" = "${ENVIRONMENT_PROFILES:-}" ]
[ "$AGENTS" = "${CODING_TOOL_INTEGRATIONS:-}" ]
printf 'retained-adapter-ok\n' > "@MARKER@"
`)
	writeCLIFile(t, filepath.Join(previous, "scripts", "install-power-reconciler.sh"),
		retainedAdapter, 0o700)
	if err := os.Symlink(
		filepath.Join("releases", "legacy-release"),
		filepath.Join(runtimeRoot, "previous"),
	); err != nil {
		t.Fatal(err)
	}

	activeDispatcher := filepath.Join(active, "bin", "yard-engine")
	var stderr bytes.Buffer
	environment := []string{
		"HOME=" + home,
		"SUBYARD_OPERATOR_HOME=" + home,
		"SUBYARD_CONFIG_HOME=" + filepath.Join(home, ".config", "subyard"),
		"SUBYARD_HOME=" + filepath.Join(home, ".subyard"),
		"SUBYARD_NO_AUDIT=1",
		"SUBYARD_INTERNAL_MIGRATION_CHILD=1",
		"YARD_RUNTIME_ROOT=" + runtimeRoot,
	}
	program, err := New(Options{
		RepositoryRoot: active,
		DispatcherPath: activeDispatcher,
		Program:        "yard",
		Arguments:      []string{"_migrate", "reconcile-power-reconciler"},
		Environment:    append(environment, "SUBYARD_POWER_MIGRATION_PAYLOAD_ROOT="+previous),
		Stderr:         &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	program.effectiveUID = func() int { return 0 }

	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("power reconcile failed: code=%d stderr=%q", code, stderr.String())
	}
	if program.options.RepositoryRoot != active || program.options.DispatcherPath != activeDispatcher {
		t.Fatalf("active CLI options were not restored: root %q dispatcher %q",
			program.options.RepositoryRoot, program.options.DispatcherPath)
	}
	if program.retainedAdapterCompatibility {
		t.Fatal("retained adapter compatibility leaked into the active CLI")
	}
	ordinaryLoaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	ordinaryPlatform, ok := program.initPlatform(
		ordinaryLoaded, []domain.Context{ordinaryLoaded.Context},
	).(reconcileruntime.Runtime)
	if !ok {
		t.Fatalf("ordinary init platform = %T", ordinaryPlatform)
	}
	for _, alias := range strings.Fields(
		"YARD_TYPE INSTANCE_TYPE INSTANCE_NAME REMOTE_DEST REMOTE_YARD " +
			"BASE_IMAGE BASE_IMAGE_FALLBACK YARD_PROFILES AGENTS",
	) {
		prefix := alias + "="
		if slices.ContainsFunc(ordinaryPlatform.Environment, func(assignment string) bool {
			return strings.HasPrefix(assignment, prefix)
		}) {
			t.Fatalf("ordinary current platform retained legacy alias %s", alias)
		}
	}
	contents, err := os.ReadFile(marker)
	if err != nil || string(contents) != "retained-adapter-ok\n" {
		t.Fatalf("retained power adapter marker = %q, %v", contents, err)
	}

	foreign := filepath.Join(home, "foreign-release")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: active,
		DispatcherPath: activeDispatcher,
		Program:        "yard",
		Arguments:      []string{"_migrate", "reconcile-power-reconciler"},
		Environment:    append(environment, "SUBYARD_POWER_MIGRATION_PAYLOAD_ROOT="+foreign),
		Stderr:         &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code == 0 ||
		!strings.Contains(stderr.String(), "not the retained previous release") {
		t.Fatalf("foreign power payload accepted: code=%d stderr=%q", code, stderr.String())
	}
}

func TestPowerMigrationRepositoryRootAcceptsOnlyRetainedPreviousRelease(t *testing.T) {
	root := t.TempDir()
	active := filepath.Join(root, "active")
	runtimeRoot := filepath.Join(root, "runtime")
	previous := filepath.Join(runtimeRoot, "releases", "previous-release")
	foreign := filepath.Join(root, "foreign")
	for _, directory := range []string{active, previous, foreign} {
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

	tests := []struct {
		name        string
		payloadRoot string
		want        string
		wantError   bool
	}{
		{name: "active release by default", want: active},
		{name: "exact retained previous release", payloadRoot: previous, want: previous},
		{name: "foreign payload", payloadRoot: foreign, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := map[string]string{"YARD_RUNTIME_ROOT": runtimeRoot}
			if test.payloadRoot != "" {
				environment["SUBYARD_POWER_MIGRATION_PAYLOAD_ROOT"] = test.payloadRoot
			}
			got, err := powerMigrationRepositoryRoot(active, environment)
			if (err != nil) != test.wantError {
				t.Fatalf("repository root error = %v, want error %v", err, test.wantError)
			}
			if err == nil && got != test.want {
				t.Fatalf("repository root = %q, want %q", got, test.want)
			}
		})
	}
}

func (fixture *initPlatformFixture) Teardown(context.Context) error {
	fixture.teardowns++
	for stage := range fixture.converged {
		fixture.converged[stage] = false
	}
	return nil
}

func TestNativeInitOwnsPlanResumeAndFinalization(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	platform := newInitPlatformFixture()
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"init", "--yes"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
		InitPlatform: platform,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("init failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !slices.Equal(platform.preflightFresh, []bool{false}) ||
		!slices.Equal(platform.applied, []ports.ReconcileStageID{
			ports.ReconcileStageProject, ports.ReconcileStageFinalize,
		}) {
		t.Fatalf("native init bypassed live plan/apply/finalize: preflight=%v applied=%v",
			platform.preflightFresh, platform.applied)
	}
	if !strings.Contains(stdout.String(), "[do  ] Create the Incus project") ||
		!strings.Contains(stdout.String(), "[do  ] Provision the yard") {
		t.Fatalf("init plan omitted live stage state:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"init", "--yes"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
		InitPlatform: platform,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 ||
		!strings.Contains(stdout.String(), "Everything is already set up") {
		t.Fatalf("no-op init failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(platform.applied) != 2 {
		t.Fatalf("no-op init reapplied stages: %v", platform.applied)
	}
}

func TestInitProfileCreatesDefinitionOnlyAfterConfirmationAndPreflight(t *testing.T) {
	for _, test := range []struct {
		name          string
		confirm       bool
		preflightErr  error
		applyErr      error
		allConverged  bool
		wantCode      int
		wantPersisted bool
	}{
		{name: "success", confirm: true, wantPersisted: true},
		{name: "converged infrastructure still preflights", confirm: true, allConverged: true, wantPersisted: true},
		{name: "declined", wantCode: 1},
		{name: "preflight failure", confirm: true, preflightErr: errors.New("unsupported host"), wantCode: 1},
		{name: "reconcile failure is resumable", confirm: true, applyErr: errors.New("incus failed"), wantCode: 1, wantPersisted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			environment = withoutCommandSetting(environment, "SSH_PORT")
			preset := filepath.Join(root, "config", "profiles", "hermes", "yard.env")
			if err := os.MkdirAll(filepath.Dir(preset), 0o700); err != nil {
				t.Fatal(err)
			}
			content := "ENVIRONMENT_PROFILES=hermes\nAGENTS=codex\nSSH_PORT=2234\n"
			writeCLIFile(t, preset, content, 0o600)
			platform := newInitPlatformFixture()
			platform.preflightErr = test.preflightErr
			platform.applyErr = test.applyErr
			if test.allConverged {
				platform.converged[ports.ReconcileStageProject] = true
			}
			prompt := &testkit.Prompt{Answers: []bool{test.confirm}}
			arguments := []string{"-Y", "hermes", "init", "--profile", "hermes"}
			var stdout, stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: arguments,
				Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
				InitPlatform: platform, Prompt: prompt,
			})
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "state", "yards", "hermes", "config.env")
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("definition existed before init: %v", err)
			}
			if code := program.Run(context.Background()); code != test.wantCode {
				t.Fatalf("init code=%d want=%d stdout=%q stderr=%q",
					code, test.wantCode, stdout.String(), stderr.String())
			}
			if test.allConverged && !slices.Equal(platform.preflightFresh, []bool{false}) {
				t.Fatalf("bootstrap skipped preflight: %v", platform.preflightFresh)
			}
			stored, err := os.ReadFile(target)
			if !test.wantPersisted {
				if !os.IsNotExist(err) {
					t.Fatalf("unconfirmed definition was written: content=%q err=%v", stored, err)
				}
				return
			}
			if err != nil || string(stored) != content {
				t.Fatalf("definition content=%q err=%v", stored, err)
			}
			info, err := os.Lstat(target)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("definition mode=%v err=%v", info.Mode(), err)
			}
		})
	}
}

func TestNativeInitProfileIsAdvertisedAndUnknownPresetDoesNotWrite(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"-Y", "hermes", "init", "--profile", "missing", "--yes"},
		Environment: environment, WorkingDir: root, InitPlatform: newInitPlatformFixture(),
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := program.manifest.Lookup("init")
	if !ok || !slices.Contains(definition.Options, "--profile") {
		t.Fatalf("init manifest does not advertise --profile: %#v", definition.Options)
	}
	var stderr bytes.Buffer
	program.options.Stderr = &stderr
	if code := program.Run(context.Background()); code != 2 ||
		!strings.Contains(stderr.String(), "has no named-yard preset") {
		t.Fatalf("unknown profile: code=%d stderr=%q", code, stderr.String())
	}
	target := filepath.Join(root, "state", "yards", "hermes", "config.env")
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("unknown profile wrote a definition: %v", err)
	}

	repositoryProgram, err := New(Options{
		RepositoryRoot: repositoryRoot(t), Program: "yard", Environment: environment,
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, ok = repositoryProgram.manifest.Lookup("init")
	if !ok || !slices.Contains(definition.Options, "--profile") {
		t.Fatalf("repository init manifest does not advertise --profile: %#v", definition.Options)
	}
}

func TestNativeInitModesStayInOneConfirmedWorkflow(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	platform := newInitPlatformFixture()
	for _, arguments := range [][]string{{"init", "--configs", "--yes"}, {"init", "--reset", "--yes"}} {
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard", Arguments: arguments, Environment: environment,
			WorkingDir: root, InitPlatform: platform,
		})
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code != 0 {
			t.Fatalf("%v failed with code %d", arguments, code)
		}
	}
	if platform.configs != 1 || platform.teardowns != 1 ||
		!slices.Contains(platform.preflightFresh, true) {
		t.Fatalf("init modes bypassed native workflow: %#v", platform)
	}
}
