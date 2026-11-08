package cli

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type integrationRuntimeFixture struct {
	plan       reconcileruntime.IntegrationPlan
	planErr    error
	applyErr   error
	afterApply func()
	applied    int
	requested  []string
}

type appliedSelectionPlatformFixture struct {
	*initPlatformFixture
	selected []string
	settings map[string]string
}

func (fixture *appliedSelectionPlatformFixture) AppliedIntegrationConfiguration(context.Context) (reconcileruntime.AppliedIntegrationConfiguration, error) {
	return reconcileruntime.AppliedIntegrationConfiguration{Selected: fixture.selected, Settings: fixture.settings, Installed: true}, nil
}

type appliedProfilesPlatformFixture struct {
	*appliedSelectionPlatformFixture
	profiles []string
	known    bool
}

func (fixture *appliedProfilesPlatformFixture) AppliedEnvironmentProfiles(context.Context) ([]string, bool, error) {
	return fixture.profiles, fixture.known, nil
}

func TestReleaseMaterializationUsesVerifiedAppliedProfilesIncludingEmpty(t *testing.T) {
	for _, applied := range [][]string{{}, {"retained"}} {
		cli, _, _, _, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=codex\n")
		cli.options.InitPlatform = &appliedProfilesPlatformFixture{appliedSelectionPlatformFixture: &appliedSelectionPlatformFixture{initPlatformFixture: newInitPlatformFixture(), selected: []string{"codex"}}, profiles: applied, known: true}
		loaded, err := cli.loadContext("default")
		if err != nil {
			t.Fatal(err)
		}
		loaded.Environment["ENVIRONMENT_PROFILES"] = "newly-desired"
		target, err := cli.appliedIntegrationTarget(context.Background(), configTarget{Name: "default", Loaded: loaded})
		if err != nil {
			t.Fatal(err)
		}
		if target.Loaded.Environment["ENVIRONMENT_PROFILES"] != strings.Join(applied, " ") || !slices.Equal(target.Loaded.EnvironmentProfiles.Effective, applied) || loaded.Environment["ENVIRONMENT_PROFILES"] != "newly-desired" {
			t.Fatal("installed profile selection followed pending desired configuration")
		}
	}
}

func TestReleaseMaterializationRetainsEveryAppliedIntegrationInitField(t *testing.T) {
	for _, suffix := range []string{"COMMAND", "CHECK", "HEALTH", "PROJECTS_CHANGED", "DEPENDS", "PERSIST", "PROVISION"} {
		t.Run(suffix, func(t *testing.T) {
			cli, _, _, _, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=codex\n")
			name, value := "AGENT_codex_"+suffix, "old-command"
			switch suffix {
			case "DEPENDS":
				value = ""
			case "PERSIST":
				value = ".old:/mnt/host/old"
			case "PROVISION":
				value = filepath.Join(cli.options.RepositoryRoot, "old-provision.sh")
				writeCLIFile(t, value, "#!/bin/sh\nexit 0\n", 0o700)
			}
			cli.options.InitPlatform = &appliedSelectionPlatformFixture{initPlatformFixture: newInitPlatformFixture(), selected: []string{"codex"}, settings: map[string]string{name: value}}
			loaded, err := cli.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			loaded.Environment[name] = "new-desired-value"
			target, err := cli.appliedIntegrationTarget(context.Background(), configTarget{Name: "default", Loaded: loaded})
			if err != nil || target.Loaded.Environment[name] != value || loaded.Environment[name] != "new-desired-value" {
				t.Fatalf("applied %s not retained: %v", suffix, err)
			}
		})
	}
}

func TestReleaseMaterializationUsesAppliedCodingSelection(t *testing.T) {
	cli, _, _, path, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=relay\n")
	cli.options.InitPlatform = &appliedSelectionPlatformFixture{initPlatformFixture: newInitPlatformFixture(), selected: []string{"codex"}}
	loaded, err := cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	target, err := cli.appliedIntegrationTarget(context.Background(), configTarget{Name: "default", Loaded: loaded})
	if err != nil || strings.Join(target.Loaded.Integrations.Effective, " ") != "codex" || strings.Join(loaded.Integrations.Requested, " ") != "relay" {
		t.Fatalf("release projection changed desired selection or followed pending selection: %v %#v", err, target.Loaded.Integrations)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("read-only release observation authored configuration")
	}
}

func TestReleaseMaterializationKeepsFirstAuthoredHookPending(t *testing.T) {
	cli, _, _, _, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=codex\n")
	loaded, err := cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	if _, present := loaded.Environment["AGENT_codex_PROJECTS_CHANGED"]; present {
		t.Fatal("fixture hook must be absent before its first assignment")
	}
	applied := loaded.Catalog.IntegrationInitSettings(loaded.Environment)
	cli.options.InitPlatform = &appliedSelectionPlatformFixture{initPlatformFixture: newInitPlatformFixture(), selected: []string{"codex"}, settings: applied}
	loaded.Environment["AGENT_codex_PROJECTS_CHANGED"] = "new-hook"
	target, err := cli.appliedIntegrationTarget(context.Background(), configTarget{Name: "default", Loaded: loaded})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := target.Loaded.Environment["AGENT_codex_PROJECTS_CHANGED"]; present {
		t.Fatal("first authored hook entered completed-release readiness before apply")
	}
	if loaded.Environment["AGENT_codex_PROJECTS_CHANGED"] != "new-hook" {
		t.Fatal("release projection discarded desired hook intent")
	}
}

func (fixture *integrationRuntimeFixture) IntegrationPlan(context.Context) (reconcileruntime.IntegrationPlan, error) {
	return fixture.plan, fixture.planErr
}
func (fixture *integrationRuntimeFixture) ApplyIntegrations(context.Context, reconcileruntime.IntegrationPlan) error {
	fixture.applied++
	if fixture.afterApply != nil {
		fixture.afterApply()
	}
	return fixture.applyErr
}
func integrationFixture(t *testing.T, selection string) (*CLI, *testkit.Incus, *integrationRuntimeFixture, string, *bytes.Buffer) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	manifestPath := filepath.Join(root, "config", "commands.registry")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, manifestPath, string(manifest)+"integration||@integration||forward|mutate|dynamic|public|lifecycle|simple|integration <command>|integrations|--json --yes --help|enable disable status\n", 0600)
	writeCLIFile(t, filepath.Join(root, "config", "agents.env"), "CODING_TOOL_INTEGRATIONS=\nAGENT_codex_COMMAND=codex\nAGENT_relay_COMMAND=relay\nAGENT_relay_DEPENDS=codex\n", 0600)
	path := filepath.Join(root, "state", "yards", "default", "config.env")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, path, selection, 0600)
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	runtime := &integrationRuntimeFixture{plan: reconcileruntime.IntegrationPlan{Fingerprint: "stable", Changed: true, Steps: []string{"Reconcile owned integrations"}}}
	var output bytes.Buffer
	cli, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: incus, Stdin: strings.NewReader(""), Stdout: &output, Stderr: &output, IntegrationRuntime: func(loaded config.Loaded) IntegrationRuntime {
		runtime.requested = slices.Clone(loaded.Integrations.Requested)
		return runtime
	}})
	if err != nil {
		t.Fatal(err)
	}
	return cli, incus, runtime, path, &output
}
func prepareIntegrationTest(t *testing.T, cli *CLI, arguments ...string) (*preparedCommand, error) {
	t.Helper()
	loaded, err := cli.loadContext("default")
	if err != nil {
		return nil, err
	}
	definition, _ := cli.manifest.Lookup("integration")
	return cli.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: arguments, ExplicitYard: true})
}
func TestIntegrationStoppedAndMissingRejectBeforePlanOrWrite(t *testing.T) {
	for _, state := range []string{"Stopped", "missing"} {
		t.Run(state, func(t *testing.T) {
			cli, incus, runtime, path, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=codex\n")
			if state == "missing" {
				delete(incus.Instances, "subyard/yard")
			} else {
				instance := incus.Instances["subyard/yard"]
				instance.Status = state
				incus.Instances["subyard/yard"] = instance
			}
			before, _ := os.ReadFile(path)
			prepared, err := prepareIntegrationTest(t, cli, "enable", "codex")
			if err == nil || prepared != nil || !strings.Contains(err.Error(), "running yard") {
				t.Fatalf("prepared=%v err=%v", prepared, err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) || runtime.applied != 0 {
				t.Fatal("stopped yard mutated")
			}
		})
	}
}
func TestIntegrationDependencyDisableAndTemporaryOverrideRejected(t *testing.T) {
	cli, _, _, _, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=relay\n")
	if _, err := prepareIntegrationTest(t, cli, "disable", "codex"); err == nil || !strings.Contains(err.Error(), "required by relay") {
		t.Fatalf("dependency error=%v", err)
	}
	cli, _, _, _, _ = integrationFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
	cli.baseEnv["CODING_TOOL_INTEGRATIONS"] = "codex"
	cli.env["CODING_TOOL_INTEGRATIONS"] = "codex"
	if _, err := prepareIntegrationTest(t, cli, "enable", "codex"); err == nil || !strings.Contains(err.Error(), "temporary") {
		t.Fatalf("override error=%v", err)
	}
}
func TestIntegrationRegisteredSourceAllowsLocalWrite(t *testing.T) {
	cli, _, runtime, path, output := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
	if err := configsync.RegisterSource(filepath.Join(cli.options.RepositoryRoot, "state"), filepath.Join(cli.options.RepositoryRoot, "checkout")); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareIntegrationTest(t, cli, "enable", "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if code := cli.runPreparedCommand(context.Background(), prepared, true); code != 0 || runtime.applied != 1 {
		t.Fatalf("local activation=%d %s", code, output)
	}
	after, _ := os.ReadFile(path)
	if !strings.Contains(string(after), "CODING_TOOL_INTEGRATIONS='codex'") {
		t.Fatalf("local selection missing: %q", after)
	}
}

func TestIntegrationUnmigratedOwnershipPreventsWrite(t *testing.T) {
	cli, _, runtime, path, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
	writeUnmigratedSettingsManifest(t, filepath.Join(cli.options.RepositoryRoot, "state"))
	before, _ := os.ReadFile(path)
	if _, err := prepareIntegrationTest(t, cli, "enable", "codex"); err == nil || !strings.Contains(err.Error(), "migrate Git settings") {
		t.Fatalf("guard=%v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || runtime.applied != 0 {
		t.Fatal("unmigrated settings changed")
	}
}
func TestIntegrationDesiredPersistsOnApplyFailureAndRetry(t *testing.T) {
	cli, _, runtime, path, output := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
	prepared, err := prepareIntegrationTest(t, cli, "enable", "codex")
	if err != nil {
		t.Fatal(err)
	}
	runtime.applyErr = errors.New("injected package failure")
	if code := cli.runPreparedCommand(context.Background(), prepared, true); code != 1 {
		t.Fatalf("exit=%d %s", code, output)
	}
	prepared.Close()
	content, _ := os.ReadFile(path)
	if !strings.Contains(string(content), "codex") {
		t.Fatalf("lost desired: %s %s", content, output)
	}
	runtime.applyErr = nil
	// A new invocation captures the freshly persisted desired set.
	delete(cli.env, "CODING_TOOL_INTEGRATIONS")
	delete(cli.env, "AGENTS")
	prepared, err = prepareIntegrationTest(t, cli, "enable", "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if code := cli.runPreparedCommand(context.Background(), prepared, true); code != 0 || runtime.applied != 2 {
		t.Fatalf("retry=%d calls=%d %s", code, runtime.applied, output)
	}
}

func TestIntegrationDisableLastSelectionPreparesAndPersistsEmptySet(t *testing.T) {
	cli, _, runtime, path, output := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=codex\n")
	runtime.plan.Scope = "native integration inventory"
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareIntegrationTest(t, cli, "disable", "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || runtime.applied != 0 {
		t.Fatal("preparing the empty selection changed desired or runtime state")
	}
	if code := cli.runPreparedCommand(context.Background(), prepared, true); code != 0 {
		t.Fatalf("disable failed: exit=%d %s", code, output)
	}
	assignments, err := config.ReadAssignments(path)
	value, present := assignments["CODING_TOOL_INTEGRATIONS"]
	if err != nil || !present || value != "" || len(runtime.requested) != 0 || runtime.applied != 1 {
		t.Fatalf("last integration was not removed: present=%t empty=%t calls=%d err=%v", present, value == "", runtime.applied, err)
	}
}

func TestIntegrationStoppedAfterPlanAndCASRacePreserveDesired(t *testing.T) {
	for _, failure := range []string{"stopped", "cas", "runtime"} {
		t.Run(failure, func(t *testing.T) {
			cli, incus, runtime, path, output := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
			prepared, err := prepareIntegrationTest(t, cli, "enable", "codex")
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			switch failure {
			case "stopped":
				instance := incus.Instances["subyard/yard"]
				instance.Status = "Stopped"
				incus.Instances["subyard/yard"] = instance
			case "cas":
				writeCLIFile(t, path, "CODING_TOOL_INTEGRATIONS=relay\n", 0600)
			case "runtime":
				runtime.plan.Fingerprint = "changed"
			}
			if code := cli.runPreparedCommand(context.Background(), prepared, true); code != 1 {
				t.Fatalf("exit=%d %s", code, output)
			}
			if runtime.applied != 0 {
				t.Fatal("stale operation applied")
			}
			content, _ := os.ReadFile(path)
			if strings.Contains(string(content), "codex") {
				t.Fatalf("stale intent written: %s", content)
			}
		})
	}
}
func TestIntegrationStatusStoppedReadOnly(t *testing.T) {
	cli, incus, _, path, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Stopped"
	incus.Instances["subyard/yard"] = instance
	loaded, err := cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	status, err := cli.queryIntegrationStatus(context.Background(), loaded, "")
	if err != nil || status.Observed != "stopped" || !status.Selection.Present || len(status.Selection.Requested) != 0 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(path))), "generated", "integration-locks", "default.lock")); !os.IsNotExist(err) {
		t.Fatalf("status acquired lock: %v", err)
	}
}

func TestIntegrationStatusSeparatesHealthFromConvergence(t *testing.T) {
	cli, incus, runtime, _, output := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=sample\n")
	writeCLIFile(t, filepath.Join(cli.options.RepositoryRoot, "config", "agents.env"), "AGENT_sample_COMMAND=sample\nAGENT_sample_HEALTH=sample-health\nAGENT_other_COMMAND=other\nAGENT_other_HEALTH=other-health\n", 0o600)
	runtime.plan.Changed = false
	cli.options.Executor = incus
	loaded, err := cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"starting", "ready", "failed"} {
		incus.ExecSteps = append(incus.ExecSteps, testkit.IncusExecStep{Result: ports.InstanceExecResult{Stdout: []byte(`{"state":"` + state + `"}`)}})
		status, err := cli.queryIntegrationStatus(context.Background(), loaded, "")
		if err != nil || status.Observed != "ready" || status.Health["sample"] != state || runtime.applied != 0 {
			t.Fatalf("health changed convergence: status=%#v err=%v", status, err)
		}
		output.Reset()
		cli.printIntegrationStatus(status, false)
		if !strings.Contains(output.String(), "sample health: "+state) {
			t.Fatalf("missing advisory health: %s", output.String())
		}
	}
	calls := len(incus.ExecCalls)
	status, err := cli.queryIntegrationStatus(context.Background(), loaded, "other")
	if err != nil || len(status.Health) != 0 || len(incus.ExecCalls) != calls {
		t.Fatalf("status probed an unselected integration: %#v, %v", status, err)
	}
	if status.ObservedScope != "yard" || len(status.Inventory) != 2 {
		t.Fatalf("status lost the full catalog or yard observation scope: %#v", status)
	}
	for _, entry := range status.Inventory {
		if entry.ID == "other" && (entry.Effective || entry.Requested || entry.Observed != "not-inspected") {
			t.Fatalf("unselected component inherited yard readiness: %#v", entry)
		}
	}
}

func TestIntegrationStatusAssessesLegacyAdoptionReadOnly(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		name := "matching legacy files"
		if conflict {
			name = "ownership conflict"
		}
		t.Run(name, func(t *testing.T) {
			cli, incus, _, settingsPath, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=codex\n")
			root := cli.options.RepositoryRoot
			writeCLIFile(t, filepath.Join(root, "config/projects-changed.sh"), "#!/bin/sh\nexit 0\n", 0o644)
			instructions := filepath.Join(root, "instructions.md")
			writeCLIFile(t, instructions, "Selected instructions\n", 0o644)
			writeCLIFile(t, filepath.Join(root, "config/host.env"), "HOST_CODEX_AGENTS_MD="+instructions+"\n", 0o600)
			incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: incus.Instances["subyard/yard"]}
			executor := &initAdoptionExecutor{fingerprint: strings.Repeat("a", 64), conflict: conflict}
			cli.options.IntegrationRuntime = nil
			cli.options.Executor = executor
			declareCLIInstructionFixture(t, root, "codex", "HOST_CODEX_AGENTS_MD", ".codex/AGENTS.md", "Codex instructions")
			catalog, err := config.LoadCatalog(root)
			if err != nil {
				t.Fatal(err)
			}
			cli.catalog = catalog
			loaded, err := cli.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			status, err := cli.queryIntegrationStatus(context.Background(), loaded, "")
			if err != nil || !executor.observed {
				t.Fatalf("status skipped initial adoption: %#v, %v", status, err)
			}
			if conflict {
				if status.Observed != "conflict" || !strings.Contains(status.Detail, "/home/dev/.codex/AGENTS.md") {
					t.Fatalf("status lost adoption conflict: %#v", status)
				}
			} else if status.Observed != "pending" || !strings.Contains(status.Detail, "adopt existing file") {
				t.Fatalf("matching legacy file was not reported as pending: %#v", status)
			}
			for _, request := range executor.requests {
				if slices.Contains(request.Command, "apply") || slices.Contains(request.Command, "commit") {
					t.Fatal("status attempted to publish integration ownership")
				}
			}
			settings, err := os.ReadFile(settingsPath)
			if err != nil || string(settings) != "CODING_TOOL_INTEGRATIONS=codex\n" || len(incus.ConfigUpdates) != 0 || len(incus.PowerUpdates) != 0 {
				t.Fatal("status changed desired settings or yard state")
			}
		})
	}
}

func TestIntegrationConcurrentHostSettingsChangeCannotReportReady(t *testing.T) {
	cli, _, runtime, path, output := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
	prepared, err := prepareIntegrationTest(t, cli, "enable", "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	runtime.afterApply = func() {
		writeCLIFile(t, filepath.Join(cli.options.RepositoryRoot, "state", "config.env"), "AGENT_codex_CHECK=changed-check\n", 0600)
	}
	if code := cli.runPreparedCommand(context.Background(), prepared, true); code != 1 || !strings.Contains(output.String(), "configuration changed during") {
		t.Fatalf("concurrent settings incorrectly reported ready: %d %s", code, output)
	}
	content, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(content), "codex") {
		t.Fatalf("lost saved intent: %q %v", content, err)
	}
}

func TestRetiredIntegrationCommands(t *testing.T) {
	fixture := func(t *testing.T, selection string) (*CLI, string) {
		t.Helper()
		cli, _, _, path, _ := integrationFixture(t, selection)
		packageRoot := filepath.Join(cli.options.RepositoryRoot, "config", "retired-integrations", "widget")
		if err := os.MkdirAll(packageRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		writeCLIFile(t, filepath.Join(packageRoot, "integration.json"), `{"schema_version":1,"retired":true,"retired_settings":["AGENT_widget_COMMAND"],"lifecycle":{"handler":"retire.py","instance_config_keys":[],"instance_devices":[]}}`, 0o600)
		writeCLIFile(t, filepath.Join(packageRoot, "retire.py"), "#!/bin/sh\n", 0o700)
		reloaded, err := New(cli.options)
		if err != nil {
			t.Fatal(err)
		}
		return reloaded, path
	}
	t.Run("enable refuses", func(t *testing.T) {
		cli, path := fixture(t, "CODING_TOOL_INTEGRATIONS=\n")
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := prepareIntegrationTest(t, cli, "enable", "widget"); err == nil || !strings.Contains(err.Error(), "integration widget is retired") {
			t.Fatalf("enable error=%v", err)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatalf("refused enable changed selection: %q, %v", after, err)
		}
	})
	t.Run("disable clears an authored selection", func(t *testing.T) {
		cli, path := fixture(t, "CODING_TOOL_INTEGRATIONS='codex widget'\n")
		prepared, err := prepareIntegrationTest(t, cli, "disable", "widget")
		if err != nil {
			t.Fatal(err)
		}
		defer prepared.Close()
		if code := cli.runPreparedCommand(context.Background(), prepared, true); code != 0 {
			t.Fatalf("retired disable=%d", code)
		}
		after, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(after), "CODING_TOOL_INTEGRATIONS='codex'") {
			t.Fatalf("retired selection remains: %q, %v", after, err)
		}
	})
	t.Run("status reports retired selection", func(t *testing.T) {
		cli, _ := fixture(t, "CODING_TOOL_INTEGRATIONS='codex widget'\n")
		loaded, err := cli.loadContext("default")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(loaded.Integrations.Retired, " "); got != "widget" {
			t.Fatalf("retired selection=%q", got)
		}
		if got := strings.Join(loaded.Integrations.Effective, " "); got != "codex" {
			t.Fatalf("effective selection=%q", got)
		}
		if _, err := prepareIntegrationTest(t, cli, "status", "widget"); err != nil {
			t.Fatalf("retired status refused: %v", err)
		}
	})
	t.Run("init plan names the retirement beside its operation steps", func(t *testing.T) {
		cli, _ := fixture(t, "CODING_TOOL_INTEGRATIONS='codex widget'\n")
		loaded, err := cli.loadContext("default")
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		cli.options.Stdout = &output
		execution := &initExecution{loaded: loaded, integrationRetirements: []string{"temporary start"}, integrationWarnings: []string{"kept state"}}
		cli.printInitRetirements(execution)
		for _, want := range []string{"[do  ] yard default: temporary start", "Warning: yard default: kept state"} {
			if !strings.Contains(output.String(), want) {
				t.Fatalf("init plan output=%q", output.String())
			}
		}
	})
	t.Run("host warnings stop at the proof", func(t *testing.T) {
		cli, _ := fixture(t, "CODING_TOOL_INTEGRATIONS='codex widget'\n")
		loaded, err := cli.loadContext("default")
		if err != nil {
			t.Fatal(err)
		}
		incus := cli.options.Incus.(*testkit.Incus)
		instance := incus.Instances["subyard/yard"]
		instance.Status = "Stopped"
		incus.Instances["subyard/yard"] = instance
		want := "retired integration widget may still be installed in yard default; run: yard -Y default start; then run yard migrate"
		check := func(wanted string) {
			t.Helper()
			status, err := cli.queryIntegrationStatus(context.Background(), loaded, "")
			if err != nil || status.Observed != "stopped" || status.Detail != wanted {
				t.Fatalf("status=%+v err=%v", status, err)
			}
			var started bytes.Buffer
			execution := &lifecycleExecution{action: "start", observed: &instance}
			cli.warnRetiredIntegrationsAfterStart(loaded, execution, domain.AdapterResult{Status: "ok"}, nil, &started)
			line := ""
			if wanted != "" {
				line = "Warning: retired integration widget may still be installed in yard default; run: yard migrate\n"
			}
			if started.String() != line {
				t.Fatalf("start=%q, want %q", started.String(), line)
			}
		}
		check(want)
		var failed bytes.Buffer
		cli.warnRetiredIntegrationsAfterStart(loaded, &lifecycleExecution{action: "start", observed: &instance}, domain.AdapterResult{Status: "ok"}, errors.New("start failed"), &failed)
		cli.warnRetiredIntegrationsAfterStart(loaded, &lifecycleExecution{action: "stop", observed: &instance}, domain.AdapterResult{Status: "ok"}, nil, &failed)
		if failed.Len() != 0 {
			t.Fatalf("warned outside a successful start: %q", failed.String())
		}
		instance.Config = maps.Clone(instance.Config)
		instance.Config["user.subyard.retired_integration.widget"] = "true"
		incus.Instances["subyard/yard"] = instance
		check("")
	})
}
