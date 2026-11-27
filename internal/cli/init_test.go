package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/state"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestPowerYardContextsAreDiscoveredWithoutChangingSelection(t *testing.T) {
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	yardDirectory := filepath.Join(root, "state", "yards")
	if err := os.MkdirAll(yardDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(yardDirectory, "demo.env"), "SSH_PORT=2233\n", 0o600)
	writeCLIFile(t, filepath.Join(yardDirectory, "remote.env"),
		"ACCESS_KIND=remote\nREMOTE_DEST=owner.example\nREMOTE_YARD=default\n", 0o600)
	program := cliFixture.New(Options{RepositoryRoot: root, Program: "yard", Environment: environment})
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
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	program := cliFixture.New(Options{RepositoryRoot: root, Program: "yard", Environment: environment})
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
	converged                 map[ports.ReconcileStageID]bool
	applied                   []ports.ReconcileStageID
	preflightFresh            []bool
	preflightErr              error
	applyErr                  error
	configs                   int
	consents                  []*ports.ConfigConsent
	refreshErr                error
	configsConverged          bool
	configChecks              []bool
	teardowns                 int
	projectHooks              int
	projectHooksErr           error
	hooksApplicable           bool
	hooksBeforeProfileRuntime bool
	readyOnProfileRuntime     string
	// runtimeDeferred models a stopped yard: its profile runtimes are deferred,
	// never observed converged.
	runtimeDeferred bool
	// runtimeObserveErr fails every fresh profile runtime observation;
	// observeAfterIncus clears it once the Incus stage is applied, and
	// failObserveAfterRuntime sets it once the profile runtime stage is applied.
	runtimeObserveErr       error
	observeAfterIncus       bool
	failObserveAfterRuntime bool
}

func (fixture *initPlatformFixture) ProfileRuntimeObservation(context.Context, string) (ports.RuntimeObservation, error) {
	if fixture.runtimeObserveErr != nil {
		return ports.RuntimeObservation{}, fixture.runtimeObserveErr
	}
	switch {
	case fixture.runtimeDeferred:
		return ports.RuntimeObservation{State: ports.RuntimeStateDeferred}, nil
	case fixture.converged[ports.ReconcileStageProfileRuntimes]:
		return ports.RuntimeObservation{State: ports.RuntimeStateCurrent}, nil
	default:
		return ports.RuntimeObservation{State: ports.RuntimeStateStale}, nil
	}
}

func (fixture *initPlatformFixture) InstanceExists(context.Context) (bool, error) {
	return fixture.converged[ports.ReconcileStageInstance], nil
}

func (fixture *initPlatformFixture) ProjectHooksApplicable(context.Context) (bool, error) {
	return fixture.hooksApplicable, nil
}

func (fixture *initPlatformFixture) RunProjectHooks(context.Context) error {
	fixture.hooksBeforeProfileRuntime = !fixture.converged[ports.ReconcileStageProfileRuntimes]
	fixture.projectHooks++
	return fixture.projectHooksErr
}

func (fixture *initPlatformFixture) ObserveProfileRuntimes(context.Context) (map[string]ports.RuntimeObservation, error) {
	return map[string]ports.RuntimeObservation{"sample-runtime": {State: "absent"}}, nil
}
func (fixture *initPlatformFixture) ApplyProfileRuntime(ctx context.Context, _, _, _, _ string) error {
	return fixture.ApplyStage(ctx, ports.ReconcileStageProfileRuntimes)
}

func (fixture *initPlatformFixture) ObserveInstalledRuntime(context.Context, string) (ports.RuntimeObservation, error) {
	return ports.RuntimeObservation{State: ports.RuntimeStateAbsent}, nil
}
func (fixture *initPlatformFixture) InstalledRuntimePresence(context.Context, string) (ports.RuntimePresence, error) {
	return ports.RuntimePresence{}, nil
}

func (fixture *initPlatformFixture) ApplyInstalledRuntime(context.Context, string, string, string, string) error {
	return nil
}

func (fixture *initPlatformFixture) ConfigsConverged(context.Context) (bool, error) {
	fixture.configChecks = append(fixture.configChecks, fixture.configsConverged)
	return fixture.configsConverged, nil
}

func newInitPlatformFixture() *initPlatformFixture {
	converged := make(map[ports.ReconcileStageID]bool)
	for _, id := range []ports.ReconcileStageID{
		ports.ReconcileStageIncus, ports.ReconcileStageProject, ports.ReconcileStageNetwork,
		ports.ReconcileStageNetworkPolicy,
		ports.ReconcileStagePowerImport, ports.ReconcileStageInstance, ports.ReconcileStageMounts,
		ports.ReconcileStageProvision, ports.ReconcileStageTestVMs, ports.ReconcileStageSSH,
		ports.ReconcileStageProfileServices, ports.ReconcileStageGitIdentity, ports.ReconcileStageExtras, ports.ReconcileStagePower,
		ports.ReconcileStageKeys, ports.ReconcileStageSecurity, ports.ReconcileStageProfileRuntimes,
	} {
		converged[id] = true
	}
	converged[ports.ReconcileStageProject] = false
	return &initPlatformFixture{converged: converged, hooksApplicable: true}
}

func TestParseInitProfile(t *testing.T) {
	request, err := parseInitArguments([]string{"--profile", "sample-service", "--yes"})
	if err != nil || request.mode != initReconcile || request.profile != "sample-service" {
		t.Fatalf("unexpected init request: %#v err=%v", request, err)
	}

	for _, test := range []struct {
		name      string
		arguments []string
		want      string
	}{
		{name: "missing value", arguments: []string{"--profile"}, want: "needs a value"},
		{name: "duplicate", arguments: []string{"--profile", "sample-service", "--profile", "sample-device"}, want: "only once"},
		{name: "unsafe", arguments: []string{"--profile", "../sample-service"}, want: "invalid profile"},
		{name: "configs", arguments: []string{"--configs", "--profile", "sample-service"}, want: "cannot be used together"},
		{name: "reset", arguments: []string{"--profile", "sample-service", "--reset"}, want: "cannot be used together"},
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
		{name: "converged infrastructure retries hooks", execution: initExecution{mode: initReconcile, hooksApplicable: true}, action: "yard.init.project-hooks", changed: true},
		{name: "stopped yard skips hooks", execution: initExecution{mode: initReconcile}, action: "yard.init.project-hooks"},
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

func TestInitExecutionPlansForwardedSSHAgentBoundaryOnlyForPendingSSH(t *testing.T) {
	sshStep := application.ReconcileStep{
		Stage: application.ReconcileStage{ID: ports.ReconcileStageSSH, Label: "Set up SSH access"},
	}
	for _, test := range []struct {
		name      string
		execution initExecution
		forward   bool
		want      bool
	}{
		{
			name: "pending SSH",
			execution: initExecution{mode: initReconcile, plan: application.ReconcilePlan{
				Steps: []application.ReconcileStep{sshStep},
			}},
			forward: true,
			want:    true,
		},
		{
			name: "reset",
			execution: initExecution{mode: initReset, plan: application.ReconcilePlan{
				Steps: []application.ReconcileStep{sshStep},
			}},
			forward: true,
			want:    true,
		},
		{
			name: "pending SSH without forwarding",
			execution: initExecution{mode: initReconcile, plan: application.ReconcilePlan{
				Steps: []application.ReconcileStep{sshStep},
			}},
		},
		{
			name: "converged SSH",
			execution: initExecution{mode: initReconcile, plan: application.ReconcilePlan{
				Steps: []application.ReconcileStep{{Stage: sshStep.Stage, Converged: true}},
			}},
			forward: true,
		},
		{name: "configs only", execution: initExecution{mode: initConfigs, configsChanged: true}, forward: true},
		{name: "hooks only", execution: initExecution{mode: initReconcile, hooksApplicable: true}, forward: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.execution.loaded.Context.ForwardSSHAgent = test.forward
			_, delta, err := test.execution.actionPlan()
			if err != nil {
				t.Fatal(err)
			}
			consequences := strings.Join(delta.Consequences, "\n")
			for _, expected := range []string{
				"git push",
				"forwarded, write-enabled credential",
				"No private key is copied",
				"any process that can reach the forwarded agent can exercise it",
				"agent ask-rules are a UX safeguard, not a security boundary",
			} {
				if strings.Contains(consequences, expected) != test.want {
					t.Fatalf("forwarding consequence presence for %q = %v, want %v: %q",
						expected, strings.Contains(consequences, expected), test.want, consequences)
				}
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
			cliFixture := newFixture(t, withWorkingDir(""))
			root, environment := cliFixture.Root, cliFixture.Environment
			platform := newInitPlatformFixture()
			platform.configsConverged = test.converged
			program := cliFixture.New(Options{
				RepositoryRoot: root, Program: "yard", Environment: environment,
				WorkingDir: root, InitPlatform: platform,
			})
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

func TestPrepareInitValidatesEffectiveIncusProject(t *testing.T) {
	for _, test := range []struct {
		name, yard, saved, override, project string
		invalid                              bool
	}{
		{name: "default", yard: "default", project: "subyard"},
		{name: "named", yard: "demo", project: "subyard-demo"},
		{name: "derived underscore", yard: "demo_yard", project: "subyard-demo_yard", invalid: true},
		{name: "saved underscore", yard: "demo", saved: "custom_project", project: "custom_project", invalid: true},
		{name: "command underscore", yard: "demo", override: "custom_project", project: "custom_project", invalid: true},
		{name: "saved override", yard: "demo_yard", saved: "custom-project", project: "custom-project"},
		{name: "command repairs saved value", yard: "demo_yard", saved: "custom_project", override: "custom-project", project: "custom-project"},
		{name: "trailing hyphen", yard: "demo", override: "custom-", project: "custom-", invalid: true},
		{name: "one byte override", yard: "default", override: "a", project: "a", invalid: true},
		{name: "two byte override", yard: "default", override: "a1", project: "a1"},
		{name: "64 byte override", yard: "default", override: strings.Repeat("a", 64), project: strings.Repeat("a", 64)},
		{name: "65 byte override", yard: "default", override: strings.Repeat("a", 65), project: strings.Repeat("a", 65), invalid: true},
		{name: "64 byte derivation", yard: strings.Repeat("a", 56), project: "subyard-" + strings.Repeat("a", 56)},
		{name: "65 byte derivation", yard: strings.Repeat("a", 57), project: "subyard-" + strings.Repeat("a", 57), invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cliFixture := newFixture(t, withWorkingDir(""))
			root, environment := cliFixture.Root, cliFixture.Environment
			if test.yard != "default" {
				path := filepath.Join(root, "state", "yards", test.yard, "config.env")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				content := ""
				if test.saved != "" {
					content = "INCUS_PROJECT=" + test.saved + "\n"
				}
				writeCLIFile(t, path, content, 0o600)
			}
			if test.override != "" {
				environment = append(environment, "INCUS_PROJECT="+test.override)
			}
			platform := newInitPlatformFixture()
			program := cliFixture.New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, InitPlatform: platform})
			// Existing invalid configurations must remain readable for repair.
			loaded, err := program.loadContext(test.yard)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Context.IncusProject != test.project {
				t.Fatalf("effective project=%q, want %q", loaded.Context.IncusProject, test.project)
			}
			execution, err := program.prepareInitExecution(context.Background(), loaded, nil, nil)
			if test.invalid {
				if err == nil || !strings.Contains(err.Error(), "invalid effective INCUS_PROJECT") || execution != nil {
					t.Fatalf("invalid project reached preparation: execution=%#v err=%v", execution, err)
				}
			} else if err != nil || execution.loaded.Context.IncusProject != test.project {
				t.Fatalf("valid project was rejected or changed: err=%v", err)
			}
		})
	}
}

func TestInvalidIncusProjectBlocksCLIAndRPCInitBeforeApply(t *testing.T) {
	for _, arguments := range [][]string{nil, {"--reset"}, {"--configs"}} {
		t.Run(fmt.Sprint(arguments), func(t *testing.T) {
			cliFixture := newFixture(t, withWorkingDir(""))
			root, environment := cliFixture.Root, cliFixture.Environment
			environment = append(environment, "INCUS_PROJECT=invalid_project")
			platform := newInitPlatformFixture()
			prompt := &testkit.Prompt{}
			var stderr bytes.Buffer
			program := cliFixture.New(Options{
				RepositoryRoot: root, Program: "yard", WorkingDir: root, Environment: environment,
				Arguments: append([]string{"init", "--yes"}, arguments...), InitPlatform: platform,
				Prompt: prompt, Stderr: &stderr,
			})
			if code := program.Run(context.Background()); code == 0 || !strings.Contains(stderr.String(), "invalid effective INCUS_PROJECT") {
				t.Fatalf("CLI init: code=%d stderr=%q", code, stderr.String())
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			handler := &rpcHandler{cli: program, loaded: loaded}
			defer handler.closePlans()
			params, err := json.Marshal(map[string]any{"command": "init", "arguments": arguments})
			if err != nil {
				t.Fatal(err)
			}
			_, err = handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "invalid-project", Params: params}, nil)
			if err == nil || !strings.Contains(err.Error(), "invalid effective INCUS_PROJECT") {
				t.Fatalf("RPC init plan accepted invalid project: %v", err)
			}
			if len(prompt.Requests) != 0 || len(platform.preflightFresh) != 0 || len(platform.applied) != 0 || platform.teardowns != 0 || platform.configs != 0 {
				t.Fatalf("invalid project reached confirmation/preflight/apply: %#v", platform)
			}
			if _, err := os.Stat(filepath.Join(root, "state", "host-id")); !os.IsNotExist(err) {
				t.Fatalf("invalid project published host identity: %v", err)
			}
		})
	}
}

func TestInitConfigsRechecksConvergenceAfterConsent(t *testing.T) {
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "state", "host-id"), "5034c950-74d0-46c4-9428-b7835e602109\n", 0o600)
	platform := newInitPlatformFixture()
	platform.configsConverged = false
	prompt := &callbackPrompt{callback: func() { platform.configsConverged = true }}
	var stderr bytes.Buffer
	program := cliFixture.New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"init", "--configs"},
		Environment: environment, WorkingDir: root, InitPlatform: platform,
		Prompt: prompt, Stderr: &stderr,
	})
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

// A direct init plans its refresh itself: the runtime observes the guest
// tokens right before it writes. Only a config apply parent's init --configs
// child carries the parent's confirmed tokens, through its consent fd.
func TestInitTokenSources(t *testing.T) {
	consent := &ports.ConfigConsent{InstanceUUID: "uuid", Files: []ports.ConfigFileConsent{{Destination: "/home/dev/a", Token: "absent"}}}
	for name, arguments := range map[string][]string{
		"direct init --configs": {"init", "--configs", "--yes"},
		"consented child":       {"init", "--configs", "--yes", "--config-consent-fd"},
	} {
		t.Run(name, func(t *testing.T) {
			cliFixture := newFixture(t, withWorkingDir(""))
			root, environment := cliFixture.Root, cliFixture.Environment
			if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, filepath.Join(root, "state", "host-id"), "5034c950-74d0-46c4-9428-b7835e602109\n", 0o600)
			want := (*ports.ConfigConsent)(nil)
			if arguments[len(arguments)-1] == "--config-consent-fd" {
				payload, _ := json.Marshal(consent)
				file := filepath.Join(testkit.TempDir(t), "consent")
				testkit.WriteFile(t, file, payload, 0o600)
				handle, err := os.Open(file)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = handle.Close() })
				arguments = append(slices.Clone(arguments), fmt.Sprint(handle.Fd()))
				want = consent
			}
			platform := newInitPlatformFixture()
			platform.configsConverged = false
			var stderr bytes.Buffer
			program := cliFixture.New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: arguments,
				Environment: environment, WorkingDir: root, InitPlatform: platform, Stderr: &stderr,
			})
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("init: code=%d stderr=%q", code, stderr.String())
			}
			if len(platform.consents) != 1 || (want == nil) != (platform.consents[0] == nil) ||
				want != nil && !reflect.DeepEqual(*platform.consents[0], *want) {
				t.Fatalf("refresh consents %#v, want %#v", platform.consents, want)
			}
		})
	}
}

// An init --configs child reports a refresh that failed only with file
// refusals on the descriptor its parent named and hides the name from
// its own children; any other outcome reports nothing.
func TestInitConfigsReportsRefusedFiles(t *testing.T) {
	refused := domain.CollectRefusals(
		configmaterial.FileRefusal{Yard: "default", File: configmaterial.File{Destination: "/home/dev/a.md", Token: configmaterial.TokenUnreadable}},
		configmaterial.FileRefusal{Yard: "default", File: configmaterial.File{Destination: "/home/dev/b.json", Problem: configmaterial.ProblemChanged}})
	for name, test := range map[string]struct {
		refresh error
		want    []byte
	}{
		"file refusals": {refresh: refused, want: configmaterial.EncodeRefusals(refused)},
		"other failure": {refresh: errors.Join(refused, errors.New("apply agent: failed"))},
		"refreshed":     {},
	} {
		t.Run(name, func(t *testing.T) {
			cliFixture := newFixture(t, withWorkingDir(""))
			root, environment := cliFixture.Root, cliFixture.Environment
			if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, filepath.Join(root, "state", "host-id"), "5034c950-74d0-46c4-9428-b7835e602109\n", 0o600)
			report := filepath.Join(testkit.TempDir(t), "report")
			testkit.WriteFile(t, report, nil, 0o600)
			handle, err := os.OpenFile(report, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer handle.Close()
			// The child owns, and closes, its own descriptor.
			fd, err := unix.Dup(int(handle.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			platform := newInitPlatformFixture()
			platform.configsConverged, platform.refreshErr = false, test.refresh
			var stderr bytes.Buffer
			program := cliFixture.New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: []string{"init", "--configs", "--yes"},
				Environment: append(slices.Clone(environment), fmt.Sprintf("%s=%d", ConfigRefusalsFDVariable, fd)), WorkingDir: root, InitPlatform: platform, Stderr: &stderr,
			})
			if _, inherited := program.baseEnv[ConfigRefusalsFDVariable]; inherited || program.env[ConfigRefusalsFDVariable] != "" {
				t.Fatal("children inherit the report descriptor's name")
			}
			if code := program.Run(context.Background()); (code == 0) != (test.refresh == nil) || platform.configs != 1 {
				t.Fatalf("init: code=%d refreshes=%d stderr=%q", code, platform.configs, stderr.String())
			}
			if written, err := os.ReadFile(report); err != nil || !bytes.Equal(written, test.want) {
				t.Fatalf("report %q, want %q (%v)", written, test.want, err)
			}
		})
	}
}

func withoutCommandSetting(environment []string, name string) []string {
	prefix := name + "="
	return slices.DeleteFunc(slices.Clone(environment), func(value string) bool {
		return strings.HasPrefix(value, prefix)
	})
}

func TestLoadInitProfileUsesShippedPresetForUnknownYard(t *testing.T) {
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	environment = withoutCommandSetting(environment, "SSH_PORT")
	preset := filepath.Join(root, "config", "profiles", "sample-service", "yard.env")
	if err := os.MkdirAll(filepath.Dir(preset), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "ENVIRONMENT_PROFILES=sample-service\nAGENTS=codex\nSSH_PORT=2234\n"
	writeCLIFile(t, preset, content, 0o600)
	program := cliFixture.New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
	})

	loaded, bootstrap, err := program.loadInitContext(
		"custom-name", true, []string{"--profile", "sample-service"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Context.YardName != "custom-name" || loaded.Context.SSHPort != 2234 ||
		loaded.Environment["ENVIRONMENT_PROFILES"] != "sample-service" || loaded.Environment["CODING_TOOL_INTEGRATIONS"] != "codex" {
		t.Fatalf("profile preset was not loaded for selected yard: %#v %#v", loaded.Context, loaded.Environment)
	}
	target := filepath.Join(root, "state", "yards", "custom-name", "config.env")
	if bootstrap == nil || bootstrap.profile != "sample-service" || bootstrap.sourcePath != preset ||
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
		{name: "profiles", override: "ENVIRONMENT_PROFILES=sample-build", want: "ENVIRONMENT_PROFILES"},
		{name: "agents", override: "CODING_TOOL_INTEGRATIONS=claude", want: "CODING_TOOL_INTEGRATIONS"},
		{name: "host mounts", override: "HOST_MOUNTS=/tmp:/mnt/host:ro:0755", want: "HOST_MOUNTS"},
		{name: "host links", override: "HOST_LINKS=.claude/sessions:/mnt/host/agent-sessions/claude/sessions", want: "HOST_LINKS"},
		{name: "capabilities", override: "YARD_CAPABILITIES=sample-device", want: "YARD_CAPABILITIES"},
		{name: "SSH forwarding", override: "FORWARD_SSH_AGENT=1", want: "FORWARD_SSH_AGENT"},
		{name: "sudo", override: "DEV_SUDO=yes", want: "DEV_SUDO"},
		{name: "nested VMs", override: "NESTED_E2E_VMS=1", want: "NESTED_E2E_VMS"},
		{name: "SSH port", override: "SSH_PORT=2299", want: "SSH_PORT"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cliFixture := newFixture(t, withWorkingDir(""))
			root, environment := cliFixture.Root, cliFixture.Environment
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
			preset := filepath.Join(root, "config", "profiles", "sample-service", "yard.env")
			if err := os.MkdirAll(filepath.Dir(preset), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, preset, `ENVIRONMENT_PROFILES=sample-service
CODING_TOOL_INTEGRATIONS=codex
HOST_MOUNTS=
HOST_LINKS=
YARD_CAPABILITIES=
FORWARD_SSH_AGENT=0
DEV_SUDO=no
NESTED_E2E_VMS=0
SSH_PORT=2234
`, 0o600)
			program := cliFixture.New(Options{
				RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
			})

			_, bootstrap, err := program.loadInitContext(
				"custom-name", true, []string{"--profile", "sample-service"},
			)
			if err == nil || !strings.Contains(err.Error(),
				"command environment overrides profile \"sample-service\" at setting "+test.want) ||
				bootstrap != nil {
				t.Fatalf("profile command override: bootstrap=%#v err=%v", bootstrap, err)
			}
		})
	}
}

// Operator YARD_CAPS, YARD_DEVICES and YARD_MOUNTS are ignored at every scope:
// only environment profiles declare them. No operation refuses because of
// them; readiness views report each with its exact removal command.
func TestProfileOnlySettingsAreIgnoredAndReported(t *testing.T) {
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	if err := os.MkdirAll(filepath.Join(root, "config", "yards", "profiles"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "config", "yards", "profiles", "hostless.env"), "ALLOWS_HOST_ACCESS=false\n", 0o600)
	host := filepath.Join(configHome, "config.env")
	yard := filepath.Join(configHome, "yards", "default", "config.env")
	if err := os.MkdirAll(filepath.Dir(yard), 0o700); err != nil {
		t.Fatal(err)
	}
	settings, err := os.ReadFile(host)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	writeCLIFile(t, host, string(settings)+"YARD_CAPS=fuse\n", 0o600)
	writeCLIFile(t, yard, "YARD_TEMPLATE=hostless\nYARD_DEVICES=gpu\n", 0o600)
	environment = append(environment, "YARD_MOUNTS=cache:/srv/cache:rw:0755")
	want := []string{
		"ignored: YARD_CAPS in " + host + ":" + strconv.Itoa(strings.Count(string(settings), "\n")+1) + " is ignored: only environment profiles declare it; remove it with yard -Y default config unset YARD_CAPS --scope host --local",
		"ignored: YARD_DEVICES in " + yard + ":2 is ignored: only environment profiles declare it; remove it with yard -Y default config unset YARD_DEVICES --scope yard --local",
		"ignored: YARD_MOUNTS in the command environment is ignored: only environment profiles declare it; unset it",
	}
	reports := func(output string) bool {
		for _, line := range want {
			if strings.Count(output, line) != 1 {
				return false
			}
		}
		return true
	}
	run := func(arguments []string, options Options) (int, string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		options.RepositoryRoot, options.Program, options.Arguments, options.WorkingDir = root, "yard", arguments, root
		options.Stdout, options.Stderr = &stdout, &stderr
		if options.Environment == nil {
			options.Environment = environment
		}
		program := cliFixture.New(options)
		code := program.Run(context.Background())
		return code, stdout.String(), stderr.String()
	}

	// The loader keeps the hostless role and discards the assignments without an error.
	reader := cliFixture.New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root})
	if loaded, err := reader.resolveContextWithYardSettings("default", ""); err != nil || len(loaded.IgnoredAssignments()) != 3 || loaded.Environment["ALLOWS_HOST_ACCESS"] != "false" {
		t.Fatalf("fixture role is not hostless or lost its observations: role=%q err=%v", loaded.Environment["ALLOWS_HOST_ACCESS"], err)
	}

	// The init plan reports the observations and init proceeds.
	platform := newInitPlatformFixture()
	if code, stdout, stderr := run([]string{"init", "--yes"}, Options{InitPlatform: platform}); code != 0 || !reports(stdout) {
		t.Fatalf("init refused or hid ignored assignments: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	// Status renders them as state; so does config show below.
	if code, stdout, stderr := run([]string{"status"}, Options{Incus: lifecycleIncus()}); code != 0 || !reports(stdout) || strings.Contains(stderr, "ignored") {
		t.Fatalf("status refused or hid ignored assignments: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	// No operation refuses because of them, including operations that apply yard settings.
	shipped, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "commands.registry"))
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "config", "commands.registry"), string(shipped), 0o600)
	if code, stdout, stderr := run([]string{"config", "show"}, Options{}); code != 0 || !reports(stdout) {
		t.Fatalf("config show hid ignored assignments: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, arguments := range [][]string{
		{"provision"},
		{"migrate"},
		{"integration", "enable", "codex"},
		{"config", "apply", "--yes"},
		{"teardown"},
		{"network", "reconcile"},
		{"down"},
		{"start"},
	} {
		_, stdout, stderr := run(arguments, Options{
			InitPlatform: newInitPlatformFixture(), AdapterRunner: &testkit.ScriptedAdapter{},
			Prompt: &testkit.Prompt{Answers: []bool{false, false, false}}, Incus: lifecycleIncus(),
		})
		if strings.Contains(stdout+stderr, "only environment profiles declare") && !reports(stdout) {
			t.Errorf("%v refused because of ignored assignments: stdout=%q stderr=%q", arguments, stdout, stderr)
		}
		if strings.Contains(stderr, "only environment profiles declare") {
			t.Errorf("%v rendered ignored assignments as an error: %q", arguments, stderr)
		}
	}
	if code, _, stderr := run([]string{"config", "set", "SSH_PORT", "2299", "--scope", "yard", "--local", "--yes"}, Options{}); code != 0 {
		t.Fatalf("config set of another key refused: code=%d stderr=%q", code, stderr)
	}

	// config unset removes an ignored assignment through ordinary authoring.
	if code, _, stderr := run([]string{"config", "unset", "YARD_DEVICES", "--scope", "yard", "--local", "--yes"}, Options{}); code != 0 {
		t.Fatalf("config unset of an ignored assignment failed: code=%d stderr=%q", code, stderr)
	}
	if after, err := os.ReadFile(yard); err != nil || string(after) != "YARD_TEMPLATE=hostless\nSSH_PORT='2299'\n" {
		t.Fatalf("config unset left %q: %v", after, err)
	}
	if _, stdout, _ := run([]string{"config", "show"}, Options{}); strings.Contains(stdout, "YARD_DEVICES in") || !strings.Contains(stdout, want[0]) {
		t.Fatalf("removed assignment is still reported: %q", stdout)
	}
}

func TestLoadInitProfileAllowsMatchingCommandValue(t *testing.T) {
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	environment = withoutCommandSetting(environment, "SSH_PORT")
	environment = slices.DeleteFunc(environment, func(value string) bool {
		return strings.HasPrefix(value, "CODING_TOOL_INTEGRATIONS=")
	})
	environment = append(environment, "CODING_TOOL_INTEGRATIONS=codex")
	preset := filepath.Join(root, "config", "profiles", "sample-service", "yard.env")
	if err := os.MkdirAll(filepath.Dir(preset), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, preset, "ENVIRONMENT_PROFILES=sample-service\nAGENTS=codex\nSSH_PORT=2234\n", 0o600)
	program := cliFixture.New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
	})

	loaded, bootstrap, err := program.loadInitContext(
		"custom-name", true, []string{"--profile", "sample-service"},
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
				"ENVIRONMENT_PROFILES='sample-service'\nAGENTS=\"codex\"\nSSH_PORT=2234\n",
		},
		{
			name:     "conflict",
			existing: "ENVIRONMENT_PROFILES=sample-service\nAGENTS=claude\nSSH_PORT=2234\n",
			wantErr:  "conflicts with profile \"sample-service\" at setting CODING_TOOL_INTEGRATIONS",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cliFixture := newFixture(t, withWorkingDir(""))
			root, environment := cliFixture.Root, cliFixture.Environment
			environment = withoutCommandSetting(environment, "SSH_PORT")
			preset := filepath.Join(root, "config", "profiles", "sample-service", "yard.env")
			if err := os.MkdirAll(filepath.Dir(preset), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, preset, "ENVIRONMENT_PROFILES=sample-service\nAGENTS=codex\nSSH_PORT=2234\n", 0o600)
			target := filepath.Join(root, "state", "yards", "custom-name", "config.env")
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, target, test.existing, 0o600)
			program := cliFixture.New(Options{
				RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
			})

			loaded, bootstrap, err := program.loadInitContext(
				"custom-name", true, []string{"--profile", "sample-service"},
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
	for _, location := range []string{"ignored overlay", "flat config home"} {
		t.Run(location, func(t *testing.T) {
			cliFixture := newFixture(t, withWorkingDir(""))
			root, environment := cliFixture.Root, cliFixture.Environment
			environment = withoutCommandSetting(environment, "SSH_PORT")
			preset := filepath.Join(root, "config", "profiles", "sample-service", "yard.env")
			if err := os.MkdirAll(filepath.Dir(preset), 0o700); err != nil {
				t.Fatal(err)
			}
			content := "ENVIRONMENT_PROFILES=sample-service\nAGENTS=codex\nSSH_PORT=2234\n"
			writeCLIFile(t, preset, content, 0o600)
			legacy := filepath.Join(root, "private", "yards", "custom-name.env")
			if location == "flat config home" {
				legacy = filepath.Join(root, "state", "yards", "custom-name.env")
			}
			if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, legacy, content, 0o600)
			program := cliFixture.New(Options{
				RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
			})

			loaded, bootstrap, err := program.loadInitContext(
				"custom-name", true, []string{"--profile", "sample-service"},
			)
			if err != nil || (bootstrap != nil) != (location == "ignored overlay") || loaded.Environment["CODING_TOOL_INTEGRATIONS"] != "codex" {
				t.Fatalf("unexpected init registration plan: loaded=%#v bootstrap=%#v err=%v",
					loaded.Environment, bootstrap, err)
			}
			canonical := filepath.Join(root, "state", "yards", "custom-name", "config.env")
			if _, err := os.Lstat(canonical); !os.IsNotExist(err) {
				t.Fatalf("legacy definition was shadowed by canonical file: %v", err)
			}
		})
	}
}

func TestInitProfileIgnoresPrivateDefinitionFromEffectiveConfigDir(t *testing.T) {
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	environment = withoutCommandSetting(environment, "SSH_PORT")
	preset := filepath.Join(root, "config", "profiles", "sample-service", "yard.env")
	if err := os.MkdirAll(filepath.Dir(preset), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "ENVIRONMENT_PROFILES=sample-service\nAGENTS=codex\nSSH_PORT=2234\n"
	writeCLIFile(t, preset, content, 0o600)
	alternateConfig := filepath.Join(root, "installed", "config")
	if err := os.MkdirAll(alternateConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(alternateConfig, "agents.env"), "AGENT_codex_COMMAND=codex\n", 0o600)
	writeCLIFile(t, filepath.Join(alternateConfig, "ports.env"), "SSH_PORT=2222\n", 0o600)
	environment = append(environment, "SUBYARD_CONFIG_DIR="+alternateConfig)
	legacy := filepath.Join(root, "installed", "private", "yards", "custom-name.env")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, legacy, content, 0o600)
	program := cliFixture.New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
	})

	loaded, bootstrap, err := program.loadInitContext(
		"custom-name", true, []string{"--profile", "sample-service"},
	)
	if err != nil || bootstrap == nil || loaded.Environment["CODING_TOOL_INTEGRATIONS"] != "codex" {
		t.Fatalf("init did not plan a fresh local definition: loaded=%#v bootstrap=%#v err=%v",
			loaded.Environment, bootstrap, err)
	}
	plain := cliFixture.New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root})
	loaded, bootstrap, err = plain.loadInitContext("custom-name", true, nil)
	if err != nil || bootstrap == nil || len(loaded.Integrations.Requested) != 0 {
		t.Fatalf("plain init did not plan a fresh local yard: bootstrap=%#v err=%v", bootstrap, err)
	}
	if got := string(mustReadConfigFixture(t, legacy)); got != content {
		t.Fatalf("init changed checkout overlay: %q", got)
	}
}

func TestInitProfileRejectsRemoteContextBeforeBootstrap(t *testing.T) {
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	environment = withoutCommandSetting(environment, "SSH_PORT")
	preset := filepath.Join(root, "config", "profiles", "sample-service", "yard.env")
	if err := os.MkdirAll(filepath.Dir(preset), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, preset, "ENVIRONMENT_PROFILES=sample-service\nAGENTS=codex\nSSH_PORT=2234\n", 0o600)
	environment = append(environment, "ACCESS_KIND=remote", "OWNER_ENDPOINT=operator@example.test")
	program := cliFixture.New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
	})

	_, bootstrap, err := program.loadInitContext(
		"custom-name", true, []string{"--profile", "sample-service"},
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
	// A repaired Incus makes profile runtimes observable again.
	if stage == ports.ReconcileStageIncus && fixture.observeAfterIncus {
		fixture.runtimeObserveErr = nil
	}
	if stage == ports.ReconcileStageProfileRuntimes && fixture.failObserveAfterRuntime {
		fixture.runtimeObserveErr = errors.New("injected profile runtime observation failure")
	}
	if stage == ports.ReconcileStageProfileRuntimes && fixture.readyOnProfileRuntime != "" {
		if err := os.WriteFile(fixture.readyOnProfileRuntime, []byte("ready\n"), 0o600); err != nil { //nolint:forbidigo // fake without testing.TB
			return err
		}
	}
	return nil
}

func (fixture *initPlatformFixture) VerifyStage(_ context.Context, stage ports.ReconcileStageID) (bool, error) {
	return fixture.converged[stage], nil
}

func (fixture *initPlatformFixture) Preflight(_ context.Context, fresh bool) error {
	fixture.preflightFresh = append(fixture.preflightFresh, fresh)
	return fixture.preflightErr
}

func (fixture *initPlatformFixture) RefreshConfigs(_ context.Context, consent *ports.ConfigConsent) error {
	fixture.configs++
	fixture.consents = append(fixture.consents, consent)
	return fixture.refreshErr
}

func (fixture *initPlatformFixture) Teardown(context.Context) error {
	fixture.teardowns++
	for stage := range fixture.converged {
		fixture.converged[stage] = false
	}
	return nil
}

func TestNativeInitRefusesBubblewrapOptOutWithCodexBeforeChanges(t *testing.T) {
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	dir := filepath.Join(root, "state", "yards", "default")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(dir, "config.env"), "CODING_TOOL_INTEGRATIONS=codex\nBWRAP_APPARMOR=off\n", 0600)
	platform := newInitPlatformFixture()
	var stdout, stderr bytes.Buffer
	program := cliFixture.New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"init", "--yes"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, InitPlatform: platform})
	if code := program.Run(context.Background()); code == 0 || !strings.Contains(stderr.String(), "BWRAP_APPARMOR=off requires deselecting Codex") {
		t.Fatalf("unexpected opt-out result: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(platform.preflightFresh) != 0 || len(platform.applied) != 0 || platform.projectHooks != 0 {
		t.Fatalf("incompatible setup reached reconciliation: %+v", platform)
	}
}

func TestNativeInitReportsPolicyConflictWithBubblewrapOptOut(t *testing.T) {
	const conflict = "Bubblewrap AppArmor: the yard's Bubblewrap AppArmor policy cannot be verified or reconciled. Inspect the yard policy and restore the original Subyard files from backup or resolve the administrator policy before rerunning yard setup."
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	dir := filepath.Join(root, "state", "yards", "default")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(dir, "config.env")
	const content = "CODING_TOOL_INTEGRATIONS=codex\nBWRAP_APPARMOR=off\n"
	writeCLIFile(t, settings, content, 0600)
	incus := &testkit.Incus{
		Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "Running"}},
		ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{ExitCode: 1, Stderr: []byte(conflict + "\n")}}},
	}
	var stdout, stderr bytes.Buffer
	program := cliFixture.New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"init", "--yes"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Incus: incus, Executor: incus})
	if code := program.Run(context.Background()); code == 0 ||
		strings.Count(stderr.String(), "BWRAP_APPARMOR=off requires deselecting Codex") != 1 ||
		!strings.Contains(stderr.String(), conflict) {
		t.Fatalf("expected both refusals once: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(incus.ExecCalls) != 1 || incus.ExecCalls[0].Request.Command[5] != "observe" ||
		len(incus.ConfigUpdates) != 0 || len(incus.PowerUpdates) != 0 {
		t.Fatal("expected only a read-only guest service observation")
	}
	if got, err := os.ReadFile(settings); err != nil || string(got) != content {
		t.Fatalf("init changed the refused settings: %v", err)
	}
}

func TestNativeInitOwnsPlanResumeAndFinalization(t *testing.T) {
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	platform := newInitPlatformFixture()
	var stdout, stderr bytes.Buffer
	program := cliFixture.New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"init", "--yes"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
		InitPlatform: platform,
	})
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
	if !strings.Contains(stdout.String(), "[apply] init.stage.project:") ||
		!strings.Contains(stdout.String(), "[conditional] init.stage.provision:") ||
		!strings.Contains(stdout.String(), fmt.Sprintf("native policy lock access for operator UID %d", os.Getuid())) {
		t.Fatalf("init plan omitted live stage state:\n%s", stdout.String())
	}
	if platform.projectHooks != 1 {
		t.Fatalf("successful init did not retry project hooks once: %d", platform.projectHooks)
	}
	if strings.Contains(stdout.String(), "Everything is already set up") {
		t.Fatal("pending init reported prior convergence")
	}

	stdout.Reset()
	stderr.Reset()
	platform.projectHooksErr = errors.New("optional hook failed")
	program = cliFixture.New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"init", "--yes"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
		InitPlatform: platform,
	})
	if code := program.Run(context.Background()); code == 0 || !strings.Contains(stderr.String(), "project hook verification failed") {
		t.Fatalf("failed hook verification did not fail init: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(platform.applied) != 2 || platform.projectHooks != 2 {
		t.Fatalf("hook failure changed native stage execution: applied=%v hooks=%d", platform.applied, platform.projectHooks)
	}
	if strings.Contains(stdout.String(), "Everything is already set up") {
		t.Fatal("failed hook verification reported successful convergence")
	}
	stdout.Reset()
	stderr.Reset()
	platform.projectHooksErr = nil
	program = cliFixture.New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"init", "--yes"}, Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, InitPlatform: platform})
	if code := program.Run(context.Background()); code != 0 || platform.projectHooks != 3 || len(platform.applied) != 2 {
		t.Fatalf("converged init retry failed: code=%d hooks=%d applied=%v stderr=%s", code, platform.projectHooks, platform.applied, stderr.String())
	}
	if strings.Count(stdout.String(), "Everything is already set up") != 1 {
		t.Fatal("successful converged retry omitted or duplicated its result summary")
	}
	stdout.Reset()
	stderr.Reset()
	platform.hooksApplicable = false
	program = cliFixture.New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"init"}, Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, InitPlatform: platform})
	if code := program.Run(context.Background()); code != 0 || platform.projectHooks != 3 || len(platform.applied) != 2 || strings.Count(stdout.String(), "Everything is already set up") != 1 {
		t.Fatalf("converged no-op did not report one result without native work: code=%d hooks=%d applied=%v stderr=%s", code, platform.projectHooks, platform.applied, stderr.String())
	}
}

func TestInitProjectHookFailureRendersApprovedLabel(t *testing.T) {
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	platform := newInitPlatformFixture()
	const hook = "/usr/local/libexec/subyard/projects-changed.d/fixture"
	platform.projectHooksErr = &application.ProjectHookFailure{
		Yard: "default", Reason: "exited unsuccessfully", Hooks: []string{hook},
	}
	var stderr bytes.Buffer
	program := cliFixture.New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"init", "--yes"},
		Environment: environment, WorkingDir: root, Stdout: io.Discard, Stderr: &stderr,
		InitPlatform: platform,
	})
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), "yard default: project hook "+hook+" failed") ||
		!strings.Contains(stderr.String(), "retry init") {
		t.Fatalf("init hook failure: code=%d stderr=%q", code, stderr.String())
	}
}

func TestInitRepairsProfileRuntimeBeforeHooksInProvisionedYard(t *testing.T) {
	// "converged": the fault was fixed and the runtime converged elsewhere (for
	// example by the profile's own command), so init takes its hooks-only path.
	// "converged-no-hooks": the same with no project hook work left (for
	// example after the profile removed itself), which is otherwise a no-op init.
	// "stopped" and "stopped-full": a stopped yard defers its runtimes, so
	// neither the hooks-only nor the full path may retire the record.
	// "incus-repair": Incus is unreachable until this init repairs it; planning
	// observes no runtime and the record is retired only after the repair.
	// "observe-after-repair-fails": the refreshed runtime cannot be observed;
	// the record stays, hooks and finalize still run, and init reports it.
	for _, scenario := range []string{"repair", "converged", "converged-no-hooks", "stopped", "stopped-full", "incus-repair", "observe-after-repair-fails", "failure", "declined"} {
		t.Run(scenario, func(t *testing.T) {
			cliFixture := newFixture(t, withWorkingDir(""))
			root, environment := cliFixture.Root, cliFixture.Environment
			// A recorded yard-scoped runtime failure of this yard.
			writeRuntimeProfileFixture(t, root)
			writeCLIFile(t, filepath.Join(root, "config", "profiles", "sample", "profile.json"),
				`{"schema_version":1,"runtime":{"activation_id":"sample-runtime","handler":"runtime.sh","failure_scope":"yard"}}`, 0o600)
			configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
			if err := os.MkdirAll(configHome, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := writeActivationPendingRecord(configHome, activationPendingRecord{Yard: "default", Project: "p", Instance: "i", Owner: "sample-runtime",
				Actual: strings.Repeat("d", 64), Desired: strings.Repeat("b", 64), Inputs: "x"}); err != nil {
				t.Fatal(err)
			}
			record, _ := activationPendingRecordPath(configHome, "default", "sample-runtime")
			if scenario == "converged" || scenario == "converged-no-hooks" || scenario == "stopped" || scenario == "stopped-full" {
				// A published owner HostID leaves no host step pending.
				writeCLIFile(t, filepath.Join(configHome, "host-id"), "owner-a\n", 0o600)
			}
			platform := newInitPlatformFixture()
			for stage := range platform.converged {
				platform.converged[stage] = true
			}
			platform.converged[ports.ReconcileStageFinalize] = true
			platform.converged[ports.ReconcileStageProfileRuntimes] = scenario != "repair" && scenario != "failure" && scenario != "declined" &&
				scenario != "observe-after-repair-fails"
			if scenario == "incus-repair" {
				platform.converged[ports.ReconcileStageIncus] = false
				platform.runtimeObserveErr = errors.New("injected unreachable Incus")
				platform.observeAfterIncus = true
			}
			platform.failObserveAfterRuntime = scenario == "observe-after-repair-fails"
			if scenario == "observe-after-repair-fails" {
				// Finalize has work, so it shows it ran after the failed retirement.
				platform.converged[ports.ReconcileStageFinalize] = false
			}
			platform.hooksApplicable = scenario != "converged-no-hooks" && scenario != "stopped"
			platform.runtimeDeferred = scenario == "stopped" || scenario == "stopped-full"
			if scenario == "stopped-full" {
				// Another stage keeps the full path.
				platform.converged[ports.ReconcileStageExtras] = false
			}
			if scenario == "failure" {
				platform.applyErr = errors.New("injected ProfileRuntime installation failure")
			}
			arguments := []string{"init"}
			if scenario != "declined" {
				arguments = append(arguments, "--yes")
			}
			var output bytes.Buffer
			program := cliFixture.New(Options{RepositoryRoot: root, Program: "yard", Arguments: arguments,
				Environment: environment, WorkingDir: root, Stdout: &output, Stderr: &output,
				InitPlatform: platform, Stdin: strings.NewReader("")})
			code := program.Run(context.Background())
			// Only a converged init resolves the recorded failure.
			resolved := scenario == "repair" || scenario == "converged" || scenario == "converged-no-hooks" || scenario == "incus-repair"
			if scenario == "repair" && !strings.Contains(output.String(), "init.pending-records.refreshed") {
				t.Fatalf("the plan did not show the conditional record retirement of a refreshed runtime: %s", output.String())
			}
			if scenario == "incus-repair" {
				if !strings.Contains(output.String(), "init.pending-records.refreshed") {
					t.Fatalf("the plan did not show the conditional record retirement while Incus needs repair: %s", output.String())
				}
				if code != 0 || !slices.Contains(platform.applied, ports.ReconcileStageIncus) {
					t.Fatalf("an unreachable Incus failed the init plan of a yard with a pending record: code=%d applied=%v output=%s", code, platform.applied, output.String())
				}
				if _, err := os.Stat(record); !os.IsNotExist(err) {
					t.Fatalf("the record survived the init that repaired Incus: %v", err)
				}
				return
			}
			if scenario == "observe-after-repair-fails" {
				if _, err := os.Stat(record); err != nil {
					t.Fatalf("an unobservable refreshed runtime lost its record: %v", err)
				}
				if code == 0 || platform.projectHooks != 1 || !slices.Contains(platform.applied, ports.ReconcileStageFinalize) ||
					!strings.Contains(output.String(), "pending activation records were kept") {
					t.Fatalf("a failed retirement skipped hooks or finalize or went unreported: code=%d hooks=%d applied=%v output=%s",
						code, platform.projectHooks, platform.applied, output.String())
				}
				return
			}
			if scenario == "stopped" || scenario == "stopped-full" {
				if _, err := os.Stat(record); err != nil || code != 0 {
					t.Fatalf("a stopped yard's init retired the record of an unobserved runtime: code=%d err=%v output=%s", code, err, output.String())
				}
				if scenario == "stopped" && !strings.Contains(output.String(), "Everything is already set up") {
					t.Fatalf("a stopped yard's record counted as init work: %s", output.String())
				}
				return
			}
			if _, err := os.Stat(record); (err == nil) != !resolved {
				t.Fatalf("pending record kept=%v after %s: %v", err == nil, scenario, err)
			}
			if scenario == "converged" || scenario == "converged-no-hooks" {
				// The fixture counts every hook retry; the native runner skips
				// one without hook work.
				if code != 0 || platform.projectHooks != 1 || len(platform.applied) != 0 {
					t.Fatalf("hooks-only init: code=%d hooks=%d applied=%v output=%s", code, platform.projectHooks, platform.applied, output.String())
				}
				return
			}
			if !resolved {
				if code == 0 || platform.projectHooks != 0 || len(platform.applied) != 0 || strings.Contains(output.String(), "Subyard initialized") {
					t.Fatalf("failed/declined repair ran hooks or reported success: code=%d hooks=%d applied=%v output=%s", code, platform.projectHooks, platform.applied, output.String())
				}
				return
			}
			if code != 0 || platform.hooksBeforeProfileRuntime || platform.projectHooks != 1 ||
				!slices.Equal(platform.applied, []ports.ReconcileStageID{ports.ReconcileStageProfileRuntimes}) {
				t.Fatalf("repair order: code=%d hooks=%d before=%v applied=%v output=%s", code, platform.projectHooks, platform.hooksBeforeProfileRuntime, platform.applied, output.String())
			}
		})
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
			cliFixture := newFixture(t, withWorkingDir(""))
			root, environment := cliFixture.Root, cliFixture.Environment
			environment = withoutCommandSetting(environment, "SSH_PORT")
			preset := filepath.Join(root, "config", "profiles", "sample-service", "yard.env")
			if err := os.MkdirAll(filepath.Dir(preset), 0o700); err != nil {
				t.Fatal(err)
			}
			content := "ENVIRONMENT_PROFILES=sample-service\nAGENTS=codex\nSSH_PORT=2234\n"
			writeCLIFile(t, preset, content, 0o600)
			platform := newInitPlatformFixture()
			platform.preflightErr = test.preflightErr
			platform.applyErr = test.applyErr
			if test.allConverged {
				platform.converged[ports.ReconcileStageProject] = true
			}
			prompt := &testkit.Prompt{Answers: []bool{test.confirm}}
			arguments := []string{"-Y", "sample-service", "init", "--profile", "sample-service"}
			var stdout, stderr bytes.Buffer
			program := cliFixture.New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: arguments,
				Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
				InitPlatform: platform, Prompt: prompt,
			})
			target := filepath.Join(root, "state", "yards", "sample-service", "config.env")
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
			want := strings.Replace(content, "AGENTS=codex", "CODING_TOOL_INTEGRATIONS='codex'", 1)
			if err != nil || string(stored) != want {
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
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	program := cliFixture.New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"-Y", "sample-service", "init", "--profile", "missing", "--yes"},
		Environment: environment, WorkingDir: root, InitPlatform: newInitPlatformFixture(),
	})
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
	target := filepath.Join(root, "state", "yards", "sample-service", "config.env")
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("unknown profile wrote a definition: %v", err)
	}

	repositoryFixture := newFixture(t, withCheckoutConfig(), withWorkingDir(""))
	repositoryProgram := repositoryFixture.New(Options{
		RepositoryRoot: repositoryFixture.Root, Program: "yard", Environment: environment,
	})
	definition, ok = repositoryProgram.manifest.Lookup("init")
	if !ok || !slices.Contains(definition.Options, "--profile") {
		t.Fatalf("repository init manifest does not advertise --profile: %#v", definition.Options)
	}
}

func TestNativeInitModesStayInOneConfirmedWorkflow(t *testing.T) {
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	platform := newInitPlatformFixture()
	for _, arguments := range [][]string{{"init", "--configs", "--yes"}, {"init", "--reset", "--yes"}} {
		program := cliFixture.New(Options{
			RepositoryRoot: root, Program: "yard", Arguments: arguments, Environment: environment,
			WorkingDir: root, InitPlatform: platform,
		})
		if code := program.Run(context.Background()); code != 0 {
			t.Fatalf("%v failed with code %d", arguments, code)
		}
	}
	if platform.configs != 1 || platform.teardowns != 1 ||
		!slices.Contains(platform.preflightFresh, true) {
		t.Fatalf("init modes bypassed native workflow: %#v", platform)
	}
}

type initRecheckPlatformFixture struct {
	*initPlatformFixture
	lateExpansion bool
	lockPath      string
	observedLock  bool
}

func TestInitExecutionLockScopeBelongsToOneLiveExecution(t *testing.T) {
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	program := cliFixture.New(Options{RepositoryRoot: root, Environment: environment})
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	execution := &initExecution{loaded: loaded}
	ctx, unlock, err := execution.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first := ctx.Value(initExecutionLockKey{}).(*initExecutionBoundary)
	first.assessed = true
	other := &initExecution{loaded: loaded}
	other.loaded.Context.YardName = "other"
	otherCtx, otherUnlock, err := other.lock(ctx)
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	if otherCtx.Value(initExecutionLockKey{}) == first {
		t.Fatal("another execution reused the lock or assessment")
	}
	otherUnlock()
	unlock()
	if first.active || first.assessed {
		t.Fatal("released lock retained its assessment")
	}
	ctx, unlock, err = execution.lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if ctx.Value(initExecutionLockKey{}) == first {
		t.Fatal("expired context reused its lock scope")
	}
}

func (fixture *initRecheckPlatformFixture) CheckStage(ctx context.Context, stage ports.ReconcileStageID) (bool, error) {
	if fixture.lateExpansion && stage == ports.ReconcileStageProject {
		fd, err := unix.Open(fixture.lockPath, unix.O_RDWR|unix.O_CLOEXEC, 0)
		if errors.Is(err, os.ErrNotExist) {
			return fixture.initPlatformFixture.CheckStage(ctx, stage)
		}
		if err != nil {
			return false, err
		}
		defer unix.Close(fd)
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); errors.Is(err, unix.EWOULDBLOCK) {
			fixture.observedLock = true
			fixture.converged[stage] = false
		} else if err != nil {
			return false, err
		} else if err := unix.Flock(fd, unix.LOCK_UN); err != nil {
			return false, err
		}
	}
	return fixture.initPlatformFixture.CheckStage(ctx, stage)
}

func TestPreparedInitAllowsShrinkToHooksAndRejectsExpansion(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		shrink        bool
		lateExpansion bool
	}{{name: "shrink", shrink: true}, {name: "expansion"}, {name: "late expansion", lateExpansion: true}, {name: "shrink late drift", shrink: true, lateExpansion: true}} {
		t.Run(scenario.name, func(t *testing.T) {
			cliFixture, root, environment, stateDirectory := newPreparationParityFixture(t)
			environment, incus, data, fixture := preparationConvergedInit(t, root, environment, stateDirectory)
			const canary = "PRIVATE_INIT_DIAGNOSTIC_CANARY"
			environment = append(environment, "FIXTURE_PRIVATE_CANARY="+canary)
			platform := &initRecheckPlatformFixture{initPlatformFixture: fixture.(*initPlatformFixture)}
			platform.converged[ports.ReconcileStageProject] = !scenario.shrink
			program := cliFixture.New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root,
				Incus: incus, ProjectData: data, InitPlatform: platform})
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			definition, _ := program.manifest.Lookup("init")
			prepared, err := admitPreparedFixture(program).prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition})
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			orchestrator := program.operationOrchestrator(prepared.Plan.OperationID, prepared.Loaded, nil, &prepared.Definition)
			_, err = prepared.Confirm(context.Background(), orchestrator, true)
			if err != nil {
				t.Fatal(err)
			}
			platform.converged[ports.ReconcileStageProject] = scenario.shrink || scenario.lateExpansion
			platform.lateExpansion = scenario.lateExpansion
			platform.lockPath = filepath.Join(loaded.Context.Paths.ConfigHome, "generated", "integration-locks", "default.lock")
			_, err = prepared.Execute(context.Background(), orchestrator, io.Discard)
			if scenario.shrink && !scenario.lateExpansion {
				if err != nil || platform.projectHooks != 1 || len(platform.applied) != 0 {
					t.Fatalf("safe shrink did not run only hooks: error=%v hooks=%d applied=%v", err, platform.projectHooks, platform.applied)
				}
			} else if !errors.Is(err, domain.ErrPlanStale) || platform.projectHooks != 0 || len(platform.applied) != 0 {
				t.Fatalf("unconfirmed expansion was accepted: error=%v hooks=%d applied=%v", err, platform.projectHooks, platform.applied)
			}
			if scenario.lateExpansion {
				message := err.Error()
				if !platform.observedLock || !strings.Contains(message, `init stage "project" now requires work`) || strings.Contains(message, canary) {
					t.Fatalf("refusal omitted safe planning facts or leaked private input: %s", message)
				}
			}
		})
	}
}

func TestPreparedEmptyInitSkipsDispatchAfterLockedRefresh(t *testing.T) {
	for _, scenario := range []string{"ready", "stage drift", "new owner"} {
		t.Run(scenario, func(t *testing.T) {
			cliFixture, root, environment, stateDirectory := newPreparationParityFixture(t)
			environment, incus, data, fixture := preparationConvergedInit(t, root, environment, stateDirectory)
			platform := &initRecheckPlatformFixture{initPlatformFixture: fixture.(*initPlatformFixture)}
			program := cliFixture.New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root, Incus: incus, ProjectData: data, InitPlatform: platform})
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			definition, _ := program.manifest.Lookup("init")
			prepared, err := admitPreparedFixture(program).prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition})
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			// Supply the native captured hook/metadata inputs alongside the injected
			// stage fixture. No extra execution guard is installed by this test.
			digest := strings.Repeat("b", 64)
			native := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "running"}}, ExecSteps: []testkit.IncusExecStep{
				{Result: ports.InstanceExecResult{Stdout: []byte(`{"projects":"` + digest + `","wiring":"` + digest + `","hooks":[],"roots":[]}`)}},
			}}
			runtime := reconcileruntime.Runtime{RepositoryRoot: "../..", Profiles: []profile.Definition{}, Integrations: []profile.IntegrationDefinition{}, Incus: native, Executor: native, Yard: domain.Context{YardName: "default"}}
			runtime.HookPlan, err = runtime.PrepareProjectHooks(context.Background(), false)
			if err != nil {
				t.Fatal(err)
			}
			execution := prepared.initExecution
			execution.hookPlan = runtime.HookPlan
			store, err := openProjectStoreReadOnly(loaded.Context.Paths.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			execution.hookProjects, err = store.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(execution.hookProjects) != 0 {
				t.Fatal("fixture unexpectedly registered projects")
			}
			runtime.HookPlan.CaptureOwnerProjects(execution.hookProjects)
			if err := runtime.PrepareProjectMetadata(context.Background()); err != nil || len(native.ExecCalls) != 1 {
				t.Fatalf("empty owner set acquired metadata probes: calls=%d err=%v", len(native.ExecCalls), err)
			}
			execution.hooksApplicable, err = execution.projectHooksApplicable(context.Background(), program, true)
			if err != nil || execution.hooksApplicable {
				t.Fatalf("empty native plan requires dispatch: %v", err)
			}
			// The effectful closure also owns the post-effect release gate. An
			// unchanged action must reach neither it nor the hook adapter.
			dispatched := false
			prepared.execute = func(context.Context, *application.Orchestrator, io.Writer) (domain.AdapterResult, error) {
				dispatched = true
				return domain.AdapterResult{}, errors.New("unexpected init effects or final readiness gate")
			}
			if err := prepared.preparePlan(context.Background()); err != nil {
				t.Fatal(err)
			}
			if slices.ContainsFunc(prepared.Plan.Steps, func(step domain.OperationStep) bool { return step.ID == "init.project-metadata" }) {
				t.Fatal("empty owner inventory acquired an artificial metadata step")
			}
			orchestrator := program.operationOrchestrator(prepared.Plan.OperationID, prepared.Loaded, nil, &prepared.Definition)
			if _, err := prepared.Confirm(context.Background(), orchestrator, true); err != nil {
				t.Fatal(err)
			}
			platform.lateExpansion = scenario == "stage drift"
			platform.lockPath = filepath.Join(loaded.Context.Paths.ConfigHome, "generated", "integration-locks", "default.lock")
			if scenario == "new owner" {
				writer, err := state.NewFileStore(loaded.Context.Paths.StateDir)
				if err != nil {
					t.Fatal(err)
				}
				if err := writer.Put(context.Background(), projectRemovalRecord(domain.ProjectSync)); err != nil {
					t.Fatal(err)
				}
			}
			_, err = prepared.Execute(context.Background(), orchestrator, io.Discard)
			if scenario != "ready" && !errors.Is(err, domain.ErrPlanStale) || scenario == "ready" && err != nil || dispatched || platform.projectHooks != 0 || len(platform.applied) != 0 || scenario == "stage drift" && !platform.observedLock {
				t.Fatalf("empty init boundary: err=%v dispatched=%t hooks=%d applied=%v locked=%t", err, dispatched, platform.projectHooks, platform.applied, platform.observedLock)
			}
			// Direct callers, repair completion, and injected platforms without
			// native plans retain their existing applicability behavior.
			platform.hooksApplicable = true
			if applicable, err := execution.projectHooksApplicable(context.Background(), program, false); err != nil || !applicable {
				t.Fatalf("unlocked fallback omitted hooks: applicable=%t err=%v", applicable, err)
			}
		})
	}
}

// The init plan names each owned extras device the pending extras stage
// detaches, and renders an extras diagnostic of a converged VM yard stage as
// observed state.
func TestInitPlanDisclosesExtrasDetachAndDiagnostics(t *testing.T) {
	extras := application.ReconcileStage{ID: ports.ReconcileStageExtras, Label: "yard extras"}
	execution := &initExecution{mode: initReconcile, loaded: config.Loaded{Context: domain.Context{YardName: "box", IncusProject: "subyard-box", YardInstanceName: "yard-box"}}}
	execution.plan = application.ReconcilePlan{Steps: []application.ReconcileStep{{Stage: extras, Diagnostic: `YARD_DEVICES entry "kvm" in /p/profile.conf is not applicable to VM yards`}}}
	execution.extrasDetach = reconcileruntime.ExtrasDetach{Devices: []string{"yx-dev-dri-renderD128", "yx-dev-fuse"}}
	steps := execution.operationSteps()
	index := slices.IndexFunc(steps, func(step domain.OperationStep) bool { return step.ID == "init.stage.extras" })
	if index < 0 {
		t.Fatalf("plan lacks the extras stage: %#v", steps)
	}
	step := steps[index]
	if step.Consequence != "yard box: detaches devices no selected profile requests: yx-dev-dri-renderD128 yx-dev-fuse" ||
		step.Observed != "not converged" || step.Decision != domain.StepApply {
		t.Fatalf("extras step = %#v", step)
	}
	// An unreadable instance discloses the generic loss.
	execution.extrasDetach = reconcileruntime.ExtrasDetach{Unverified: true}
	if step := execution.operationSteps()[index]; step.Consequence != "yard box: detaches any attached yx-* device that no selected profile requests (the instance is not readable now)" {
		t.Fatalf("unverified extras step = %#v", step)
	}
	// Many long device names stay within the step text bound.
	var many []string
	for index := range 25 {
		many = append(many, fmt.Sprintf("yx-%s-%02d", strings.Repeat("long-mount-name", 3), index))
	}
	execution.extrasDetach = reconcileruntime.ExtrasDetach{Devices: many}
	step = execution.operationSteps()[index]
	step.DependsOn = nil
	if err := domain.ValidateOperationSteps([]domain.OperationStep{step}); err != nil {
		t.Fatalf("bounded step refused: %v", err)
	}
	if consequence := step.Consequence; len(consequence) > 512 || !strings.Contains(consequence, many[0]) || !regexp.MustCompile(` and \d+ more$`).MatchString(consequence) {
		t.Fatalf("unbounded consequence %q", consequence)
	}
	execution.extrasDetach = reconcileruntime.ExtrasDetach{}
	// Many VM not-applicable diagnostics stay within the step text bound too.
	var diagnostics []string
	for index := range 12 {
		diagnostics = append(diagnostics, fmt.Sprintf(`YARD_DEVICES entry "kvm" in /srv/%s-%02d/profile.conf is not applicable to VM yards`, strings.Repeat("profile-path", 3), index))
	}
	execution.plan.Steps[0].Converged, execution.plan.Steps[0].Diagnostic = true, strings.Join(diagnostics, "; ")
	step = execution.operationSteps()[index]
	step.DependsOn = nil
	if err := domain.ValidateOperationSteps([]domain.OperationStep{step}); err != nil || !strings.HasPrefix(step.Observed, "converged; "+diagnostics[0]) ||
		!regexp.MustCompile(` and \d+ more$`).MatchString(step.Observed) {
		t.Fatalf("unbounded diagnostics %q: %v", step.Observed, err)
	}
	execution.plan.Steps[0].Diagnostic = `YARD_DEVICES entry "kvm" in /p/profile.conf is not applicable to VM yards`
	step = execution.operationSteps()[index]
	if step.Consequence != "yard extras" || step.Observed != `converged; YARD_DEVICES entry "kvm" in /p/profile.conf is not applicable to VM yards` {
		t.Fatalf("converged extras step = %#v", step)
	}
}

// A cold host plans its conditional extras stage without Incus. An unreadable
// instance discloses the generic device loss; a declaration refusal still
// refuses.
func TestInitExtrasDetachPreviewReadsNoIncusOnColdHost(t *testing.T) {
	stages := application.InitStages(domain.Context{IncusProject: "subyard", YardInstanceName: "yard"})
	incusStage, extrasStage := stages[0], stages[slices.IndexFunc(stages, func(stage application.ReconcileStage) bool { return stage.ID == ports.ReconcileStageExtras })]
	root, devices := testkit.TempDir(t), testkit.TempDir(t)
	incus := &testkit.Incus{Err: errors.New("dial unix /var/lib/incus/unix.socket: connect: no such file or directory")}
	runtime := reconcileruntime.Runtime{RepositoryRoot: root, Incus: incus, HostDeviceRoot: devices,
		Yard: domain.Context{IncusProject: "subyard", YardInstanceName: "yard"}}
	execution := &initExecution{mode: initReconcile}
	execution.plan = application.ReconcilePlan{Steps: []application.ReconcileStep{{Stage: incusStage}, {Stage: extrasStage, Conditional: true}}}
	// The unreadable instance discloses the generic loss, since a deselected
	// profile's device can remain attached.
	if err := execution.assessExtrasDetach(context.Background(), runtime); err != nil || len(execution.extrasDetach.Devices) != 0 || !execution.extrasDetach.Unverified {
		t.Fatalf("cold init detach preview = %#v, %v", execution.extrasDetach, err)
	}
	// A leftover instance of a yard being registered is inspected too.
	incus.Err, execution.bootstrap = nil, &initBootstrap{}
	incus.Instances = map[string]ports.InstanceInfo{"subyard/yard": {Name: "yard", Project: "subyard", LocalDevices: map[string]map[string]string{"yx-dev-kvm": {}}}}
	if err := execution.assessExtrasDetach(context.Background(), runtime); err != nil || execution.extrasDetach.Unverified || strings.Join(execution.extrasDetach.Devices, " ") != "yx-dev-kvm" {
		t.Fatalf("leftover instance detach = %#v, %v", execution.extrasDetach, err)
	}
	// A declaration refusal refuses planning even when Incus is unreadable.
	incus.Err = errors.New("Incus is stopped")
	if err := os.MkdirAll(filepath.Join(root, "config", "profiles", "bad"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "config", "profiles", "bad", "profile.conf"), "YARD_DEVICES='tape'\n", 0o600)
	runtime.Environment = []string{"ENVIRONMENT_PROFILES=bad"}
	if err := execution.assessExtrasDetach(context.Background(), runtime); err == nil || !strings.Contains(err.Error(), `invalid YARD_DEVICES entry "tape"`) {
		t.Fatalf("declaration refusal hidden: %v", err)
	}
}

// convergingPrompt approves the plan after another init finished a stage.
type convergingPrompt struct {
	platform *initPlatformFixture
	stage    ports.ReconcileStageID
}

func (prompt convergingPrompt) Confirm(context.Context, domain.ConfirmationRequest) (bool, error) {
	prompt.platform.converged[prompt.stage] = true
	return true, nil
}

// An approved full init that shrinks to its hooks-only remainder before it runs
// still retires the yard's resolved pending record instead of becoming a no-op.
func TestInitShrinkingToHooksOnlyStillRetiresResolvedRecord(t *testing.T) {
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	writeRuntimeProfileFixture(t, root)
	writeCLIFile(t, filepath.Join(root, "config", "profiles", "sample", "profile.json"),
		`{"schema_version":1,"runtime":{"activation_id":"sample-runtime","handler":"runtime.sh","failure_scope":"yard"}}`, 0o600)
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeActivationPendingRecord(configHome, activationPendingRecord{Yard: "default", Project: "p", Instance: "i", Owner: "sample-runtime",
		Actual: strings.Repeat("d", 64), Desired: strings.Repeat("b", 64), Inputs: "x"}); err != nil {
		t.Fatal(err)
	}
	record, _ := activationPendingRecordPath(configHome, "default", "sample-runtime")
	writeCLIFile(t, filepath.Join(configHome, "host-id"), "owner-a\n", 0o600)
	platform := newInitPlatformFixture()
	for stage := range platform.converged {
		platform.converged[stage] = true
	}
	platform.converged[ports.ReconcileStageFinalize] = true
	platform.converged[ports.ReconcileStageExtras] = false
	platform.hooksApplicable = false
	var output bytes.Buffer
	program := cliFixture.New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"init"},
		Environment: environment, WorkingDir: root, Stdout: &output, Stderr: &output,
		InitPlatform: platform, Stdin: strings.NewReader(""), Prompt: convergingPrompt{platform: platform, stage: ports.ReconcileStageExtras}})
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("init failed: %d %s", code, output.String())
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("a shrunk init left the resolved pending record: %v %s", err, output.String())
	}
	if strings.Contains(output.String(), "Everything is already set up") {
		t.Fatalf("a shrunk init that retired a record reported nothing to do: %s", output.String())
	}
}

// Every owner whose pending record cannot be checked is reported, not only the
// first, and init does not plan past the failed reads.
func TestInitReportsEveryUnobservablePendingRecordOwner(t *testing.T) {
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	writeRuntimeProfileFixture(t, root)
	for _, name := range []string{"sample", "second"} {
		directory := filepath.Join(root, "config", "profiles", name)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		writeCLIFile(t, filepath.Join(directory, "runtime.sh"), "#!/bin/sh\nprintf '%s\\n' '{\"state\":\"absent\",\"actual\":\"\",\"desired\":\"\"}'\n", 0o700)
		writeCLIFile(t, filepath.Join(directory, "profile.json"),
			`{"schema_version":1,"runtime":{"activation_id":"`+name+`-runtime","handler":"runtime.sh","failure_scope":"yard"}}`, 0o600)
	}
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"sample-runtime", "second-runtime"} {
		if err := writeActivationPendingRecord(configHome, activationPendingRecord{Yard: "default", Project: "p", Instance: "i", Owner: owner,
			Actual: strings.Repeat("d", 64), Desired: strings.Repeat("b", 64), Inputs: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	writeCLIFile(t, filepath.Join(configHome, "host-id"), "owner-a\n", 0o600)
	platform := newInitPlatformFixture()
	for stage := range platform.converged {
		platform.converged[stage] = true
	}
	platform.runtimeObserveErr = errors.New("injected observation failure")
	var output bytes.Buffer
	program := cliFixture.New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"init", "--yes"},
		Environment: environment, WorkingDir: root, Stdout: &output, Stderr: &output, InitPlatform: platform, Stdin: strings.NewReader("")})
	if code := program.Run(context.Background()); code == 0 {
		t.Fatalf("init planned past unobservable pending record owners: %s", output.String())
	}
	for _, owner := range []string{"sample-runtime", "second-runtime"} {
		if !strings.Contains(output.String(), "observe profile runtime "+owner+" for its pending activation record") {
			t.Fatalf("the refusal omitted %s: %s", owner, output.String())
		}
	}
}
