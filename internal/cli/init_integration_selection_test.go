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
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestInitIntegrationSelectionWithAbsentIncusSocket(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeCLIFile(t, filepath.Join(root, "config/agents.env"), "CODING_TOOL_INTEGRATIONS=codex\nAGENT_codex_COMMAND=codex\n", 0o600)
	path := filepath.Join(root, "state/yards/demo/config.env")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, path, "SSH_PORT=2223\n", 0o600)
	directory := filepath.Join(root, "incus-state")
	t.Setenv("INCUS_DIR", directory)
	t.Setenv("INCUS_SOCKET", "")
	t.Setenv("PATH", t.TempDir())
	client := incusclient.New("", "projects")
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: client, Executor: client})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("demo")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	proposed, selection, err := program.prepareInitIntegrationSelection(ctx, loaded, nil)
	if err != nil || selection != nil || strings.Join(proposed.Integrations.Requested, " ") != "codex" {
		t.Fatalf("cold named init did not preserve inherited selection: selection=%v proposed=%#v err=%v", selection, proposed.Integrations, err)
	}
	// A stopped daemon (including after package removal) must not cause init to
	// materialize or clear an inherited selection.
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	again, next, err := program.prepareInitIntegrationSelection(ctx, loaded, nil)
	if err != nil || next != nil || strings.Join(again.Integrations.Requested, " ") != "codex" {
		t.Fatalf("unavailable Incus changed inherited selection: selection=%v proposed=%#v err=%v", next, again.Integrations, err)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "SSH_PORT=2223\n" {
		t.Fatalf("read-only assessment changed integration settings: %v", err)
	}
}

type initAdoptionExecutor struct {
	fingerprint string
	conflict    bool
	observed    bool
}

func (fixture *initAdoptionExecutor) Exec(_ context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(request.Command) == 5 && request.Command[0] == "python3" {
		var input struct {
			Adopt bool `json:"adopt"`
		}
		if err := json.Unmarshal(request.Stdin, &input); err != nil || !input.Adopt || request.Command[4] != "observe" {
			return ports.InstanceExecResult{}, errors.New("expected read-only legacy adoption assessment")
		}
		fixture.observed = true
		if fixture.conflict {
			return ports.InstanceExecResult{ExitCode: 1, Stderr: []byte(`{"Reason":"unowned selected artifact","Path":"/home/dev/.codex/AGENTS.md"}`)}, nil
		}
		return ports.InstanceExecResult{Stdout: []byte(fmt.Sprintf(`{"fingerprint":%q,"changed":true,"initial":true,"adopted":[{"id":"codex","kind":"file","path":"/home/dev/.codex/AGENTS.md"}]}`, fixture.fingerprint))}, nil
	}
	if len(request.Command) > 0 && (request.Command[0] == "sh" || request.Command[0] == "test") {
		return ports.InstanceExecResult{}, nil
	}
	return ports.InstanceExecResult{}, errors.New("unexpected guest operation during adoption assessment")
}

func TestInitLegacyAdoptionPreservesIntentAndRejectsStaleEvidenceBeforeSettings(t *testing.T) {
	ctx := context.Background()
	root, environment, _ := nativeFixture(t)
	path := filepath.Join(root, "state/yards/default/config.env")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, path, "AGENTS=codex\n", 0o600)
	writeCLIFile(t, filepath.Join(root, "config/projects-changed.sh"), "#!/bin/sh\nexit 0\n", 0o644)
	instructions := filepath.Join(root, "instructions.md")
	writeCLIFile(t, instructions, "Selected instructions\n", 0o644)
	var output bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Stdout: &output})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := captureInitIntegrationBaseline(loaded)
	if err != nil {
		t.Fatal(err)
	}
	proposed, selection, err := program.prepareInitIntegrationSelection(ctx, loaded, nil)
	if err != nil {
		t.Fatal(err)
	}
	if selection == nil || !baseline.Selection.Present || baseline.Selection.Provenance.Scope != "yard" || proposed.Integrations.Provenance.Scope != "" {
		t.Fatal("fixture must exercise persistent legacy intent lost by candidate normalization")
	}
	executor := &initAdoptionExecutor{fingerprint: strings.Repeat("a", 64), conflict: true}
	incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "Running"}}}
	runtime := reconcileruntime.Runtime{RepositoryRoot: root, Yard: proposed.Context, Incus: incus, Executor: executor,
		Environment: []string{"CODING_TOOL_INTEGRATIONS=codex", "HOST_CODEX_AGENTS_MD=" + instructions}}
	program.options.InitPlatform = runtime
	// Stop at the ownership check so the test proves that public preparation
	// assesses legacy artifacts before proceeding to host reconciliation.
	_, err = program.prepareInitExecution(ctx, loaded, nil, nil)
	if !executor.observed || err == nil || !strings.Contains(err.Error(), "unowned selected artifact") {
		t.Fatalf("persistent legacy intent bypassed ownership assessment: %v", err)
	}
	executor.conflict = false
	platform, adoption, err := prepareLegacyIntegrationAdoption(ctx, baseline.Selection, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if adoption.AdoptionFingerprint == "" {
		t.Fatal("legacy evidence was not bound to the plan")
	}
	execution := &initExecution{loaded: proposed, platform: platform, integrationBaseline: baseline,
		integrationSelection: selection, integrationAdoption: adoption}
	program.printInitPlan(execution)
	want := "yard default: take matching legacy path under integration management: /home/dev/.codex/AGENTS.md"
	if !strings.Contains(output.String(), want) || !strings.Contains(strings.Join(execution.consequences(), "\n"), want) {
		t.Fatal("ownership adoption was omitted from the preview or consent consequences")
	}
	before, err := readConfigAuthoringTarget(path)
	if err != nil {
		t.Fatal(err)
	}
	if !sameConfigAuthoringSnapshot(before, selection.before) {
		t.Fatal("read-only adoption planning changed settings")
	}
	execution.rebuildPlatform(program)
	rebuilt := execution.platform.(reconcileruntime.Runtime)
	if !rebuilt.AdoptLegacyIntegrations || rebuilt.LegacyIntegrationFingerprint != adoption.AdoptionFingerprint {
		t.Fatal("platform reconstruction discarded approved adoption")
	}
	executor.fingerprint = strings.Repeat("b", 64)
	if err := execution.refreshAssessment(ctx); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("composed init refresh accepted changed adoption: %v", err)
	}
	if err := execution.run(ctx, program, io.Discard); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("changed ownership evidence did not reject apply: %v", err)
	}
	after, err := readConfigAuthoringTarget(path)
	if err != nil || !sameConfigAuthoringSnapshot(before, after) || before.Identity != after.Identity {
		t.Fatalf("stale adoption wrote canonical settings: %v", err)
	}
	if len(incus.ConfigUpdates) != 0 || len(incus.PowerUpdates) != 0 {
		t.Fatal("stale adoption mutated the yard")
	}
}

func TestInitIntegrationSelectionFreshAndAdopted(t *testing.T) {
	for _, test := range []struct {
		name, yard, settings, command, want string
		existing, source, reject, inherited bool
	}{
		{name: "fresh default", yard: "default", want: "codex paseo", inherited: true},
		{name: "fresh named", yard: "demo", want: "codex paseo", inherited: true},
		{name: "existing named roots", yard: "demo", existing: true, want: "codex paseo", inherited: true},
		{name: "legacy roots", yard: "demo", settings: "AGENTS=paseo\n", existing: true, want: "paseo"},
		{name: "source inherited roots", yard: "demo", existing: true, source: true, want: "codex paseo", inherited: true},
		{name: "source legacy roots", yard: "demo", settings: "AGENTS=paseo\n", source: true, want: "paseo", inherited: true},
		{name: "temporary adoption rejected", yard: "demo", command: "CODING_TOOL_INTEGRATIONS=codex", reject: true},
		{name: "source temporary adoption rejected", yard: "demo", source: true, command: "CODING_TOOL_INTEGRATIONS=codex", reject: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			writeCLIFile(t, filepath.Join(root, "config/agents.env"), "CODING_TOOL_INTEGRATIONS='codex paseo'\nAGENT_codex_COMMAND=codex\nAGENT_paseo_DEPENDS=codex\n", 0o600)
			target := filepath.Join(root, "state/yards", test.yard, "config.env")
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			if test.yard != "default" || test.settings != "" {
				writeCLIFile(t, target, "SSH_PORT=2223\n"+test.settings, 0o600)
			}
			if test.source {
				if err := configsync.RegisterSource(filepath.Join(root, "state"), t.TempDir()); err != nil {
					t.Fatal(err)
				}
			}
			if test.command != "" {
				environment = append(environment, test.command)
			}
			platform := newInitPlatformFixture()
			platform.converged[ports.ReconcileStageInstance] = test.existing
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, InitPlatform: platform})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext(test.yard)
			if err != nil {
				t.Fatal(err)
			}
			before, err := readConfigAuthoringTarget(target)
			if err != nil {
				t.Fatal(err)
			}
			proposed, plan, err := program.prepareInitIntegrationSelection(context.Background(), loaded, nil)
			if test.reject {
				if err == nil {
					t.Fatal("unsafe adoption accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(proposed.Integrations.Requested, " ") != test.want {
				t.Fatalf("plan=%#v proposed=%#v", plan, proposed.Integrations)
			}
			if test.inherited {
				after, err := readConfigAuthoringTarget(target)
				if err != nil || plan != nil || !sameConfigAuthoringSnapshot(before, after) || before.Identity != after.Identity {
					t.Fatalf("init replaced inherited configuration: plan=%#v error=%v", plan, err)
				}
				return
			}
			if plan == nil {
				t.Fatal("explicit local legacy selection was not migrated")
			}
			if !sameConfigAuthoringSnapshot(before, plan.before) {
				t.Fatal("planning changed persistent settings")
			}
			execution := &initExecution{loaded: proposed, integrationSelection: plan}
			if err := plan.check(context.Background(), program, execution); err != nil {
				t.Fatal(err)
			}
			if err := plan.apply(execution); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(content), "AGENTS=") || !strings.Contains(string(content), "CODING_TOOL_INTEGRATIONS='"+test.want+"'") {
				t.Fatalf("persisted %q", content)
			}
		})
	}
}

func TestInitSourceManagedSelectionUsesDefaultsWithoutWriting(t *testing.T) {
	for _, test := range []struct {
		name, yard, settings, shared, profile, want string
	}{
		{name: "bare default", yard: "default", want: "codex claude"},
		{name: "profile default", yard: "default", settings: "YARD_TEMPLATE=fixture\n", profile: "CODING_TOOL_INTEGRATIONS=codex\n", want: "codex"},
		{name: "shared selection", yard: "default", shared: "CODING_TOOL_INTEGRATIONS=claude\n", want: "claude"},
		{name: "legacy yard selection", yard: "default", settings: "AGENTS=claude\n", want: "claude"},
		{name: "explicit selection", yard: "default", settings: "CODING_TOOL_INTEGRATIONS=claude\n", want: "claude"},
		{name: "explicit empty", yard: "default", settings: "CODING_TOOL_INTEGRATIONS=''\n"},
		{name: "legacy empty", yard: "default", settings: "AGENTS=none\n"},
		{name: "named profile", yard: "demo", settings: "YARD_TEMPLATE=fixture\nSSH_PORT=2223\n", profile: "CODING_TOOL_INTEGRATIONS=codex\n", want: "codex"},
		{name: "restricted profile", yard: "demo", settings: "YARD_TEMPLATE=fixture\nSSH_PORT=2223\n", profile: "ALLOWS_CODING_TOOLS=false\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			writeCLIFile(t, filepath.Join(root, "config/agents.env"), "CODING_TOOL_INTEGRATIONS='codex claude'\nAGENT_codex_COMMAND=codex\nAGENT_claude_COMMAND=claude\n", 0o600)
			configHome := filepath.Join(root, "state")
			target := filepath.Join(configHome, "yards", test.yard, "config.env")
			shared := filepath.Join(configHome, "overrides/shared/config.env")
			profile := filepath.Join(root, "config/yards/profiles/fixture.env")
			for path, content := range map[string]string{target: test.settings, shared: test.shared, profile: test.profile} {
				if content != "" {
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
					writeCLIFile(t, path, content, 0o600)
				}
			}
			if err := configsync.RegisterSource(configHome, testkit.TempDir(t)); err != nil {
				t.Fatal(err)
			}
			before := map[string]initIntegrationSource{}
			for _, path := range []string{target, shared, profile, configsync.SourceRecordPath(configHome)} {
				snapshot, err := readInitIntegrationSource(path)
				if err != nil {
					t.Fatal(err)
				}
				before[path] = snapshot
			}
			arguments := []string{"init"}
			if test.yard != "default" {
				arguments = []string{"-Y", test.yard, "init"}
			}
			platform := newInitPlatformFixture()
			for range 2 {
				var output bytes.Buffer
				program, err := New(Options{RepositoryRoot: root, Arguments: arguments,
					Environment: append(environment, "ASSUME_YES=1"), InitPlatform: platform,
					Stdout: &output, Stderr: &output})
				if err != nil {
					t.Fatal(err)
				}
				if code := program.Run(context.Background()); code != 0 {
					t.Fatalf("init failed: code=%d output=%s", code, output.String())
				}
				if got := program.env["CODING_TOOL_INTEGRATIONS"]; got != test.want {
					t.Fatalf("effective integrations=%q, want %q", got, test.want)
				}
				for path, snapshot := range before {
					after, err := readInitIntegrationSource(path)
					if err != nil || after != snapshot {
						t.Fatalf("init changed source-managed configuration %s: %v", path, err)
					}
				}
			}
			if !platform.converged[ports.ReconcileStageProject] || platform.projectHooks != 2 {
				t.Fatal("init did not reconcile and retry project hooks")
			}
		})
	}
}

func TestInitProfileBootstrapPreservesInheritedSelectionWithoutIncus(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	environment = withoutCommandSetting(environment, "SSH_PORT")
	writeCLIFile(t, filepath.Join(root, "config/agents.env"), "CODING_TOOL_INTEGRATIONS=codex\nAGENT_codex_COMMAND=codex\n", 0o600)
	preset := filepath.Join(root, "config/profiles/minimal/yard.env")
	if err := os.MkdirAll(filepath.Dir(preset), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, preset, "SSH_PORT=2234\n", 0o600)
	incus := &testkit.Incus{Err: os.ErrNotExist}
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: incus, Executor: incus})
	if err != nil {
		t.Fatal(err)
	}
	loaded, bootstrap, err := program.loadInitContext("fresh", true, []string{"--profile", "minimal"})
	if err != nil {
		t.Fatal(err)
	}
	proposed, selection, err := program.prepareInitIntegrationSelection(context.Background(), loaded, bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if selection != nil || strings.Join(proposed.Integrations.Requested, " ") != "codex" {
		t.Fatalf("fresh bootstrap selection: plan=%#v proposed=%#v", selection, proposed.Integrations)
	}
	if _, err := os.Stat(bootstrap.targetPath); !os.IsNotExist(err) {
		t.Fatalf("bootstrap planning wrote registration: %v", err)
	}
	if string(bootstrap.content) != "SSH_PORT=2234\n" {
		t.Fatalf("init inserted a tool override into the profile: %q", bootstrap.content)
	}
	if err := configsync.RegisterSource(filepath.Join(root, "state"), testkit.TempDir(t)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := program.prepareInitIntegrationSelection(context.Background(), loaded, bootstrap); err == nil || !strings.Contains(err.Error(), "source-managed") {
		t.Fatalf("profile bootstrap accepted a source-managed registration write: %v", err)
	}
}

func TestInitPreservesProfileSelectionAcrossRuns(t *testing.T) {
	for _, source := range []bool{false, true} {
		for _, restricted := range []bool{false, true} {
			t.Run(fmt.Sprintf("source=%t/restricted=%t", source, restricted), func(t *testing.T) {
				root, environment, _ := nativeFixture(t)
				writeCLIFile(t, filepath.Join(root, "config/agents.env"), "AGENT_codex_COMMAND=codex\nAGENT_paseo_DEPENDS=codex\n", 0o600)
				profile := filepath.Join(root, "config/yards/profiles/tools.env")
				target := filepath.Join(root, "state/yards/demo/config.env")
				for _, path := range []string{profile, target} {
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				writeCLIFile(t, profile, fmt.Sprintf("CODING_TOOL_INTEGRATIONS=codex\nALLOWS_CODING_TOOLS=%t\n", !restricted), 0o600)
				writeCLIFile(t, target, "YARD_TEMPLATE=tools\nSSH_PORT=2223\n", 0o600)
				before, err := readConfigAuthoringTarget(target)
				if err != nil {
					t.Fatal(err)
				}
				if source {
					if err := configsync.RegisterSource(filepath.Join(root, "state"), t.TempDir()); err != nil {
						t.Fatal(err)
					}
				}
				platform := newInitPlatformFixture()
				platform.converged[ports.ReconcileStageInstance] = false
				program, err := New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: platform})
				if err != nil {
					t.Fatal(err)
				}
				prepare := func(want string) *initExecution {
					t.Helper()
					// Each init invocation starts from persistent inputs, not the previous
					// command's resolved runtime environment.
					program, err = New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: platform})
					if err != nil {
						t.Fatal(err)
					}
					loaded, err := program.loadContext("demo")
					if err != nil {
						t.Fatal(err)
					}
					execution, err := program.prepareInitExecution(context.Background(), loaded, nil, nil)
					if err != nil {
						t.Fatal(err)
					}
					selection := execution.loaded.Integrations
					if execution.integrationSelection != nil || strings.Join(selection.Requested, " ") != want {
						t.Fatalf("init overrode profile selection: %#v", selection)
					}
					if restricted && (len(selection.Effective) != 0 || execution.loaded.Environment["CODING_TOOL_INTEGRATIONS"] != "") {
						t.Fatal("restricted profile enabled tools")
					}
					return execution
				}
				for range 2 {
					if err := prepare("codex").run(context.Background(), program, io.Discard); err != nil {
						t.Fatal(err)
					}
				}
				stale := prepare("codex")
				applied := len(platform.applied)
				hooks := platform.projectHooks
				writeCLIFile(t, profile, fmt.Sprintf("CODING_TOOL_INTEGRATIONS=paseo\nALLOWS_CODING_TOOLS=%t\n", !restricted), 0o600)
				if err := stale.run(context.Background(), program, io.Discard); !errors.Is(err, domain.ErrPlanStale) {
					t.Fatalf("changed profile accepted by stale init: %v", err)
				}
				if len(platform.applied) != applied || platform.projectHooks != hooks {
					t.Fatal("stale init changed runtime")
				}
				if err := prepare("paseo").run(context.Background(), program, io.Discard); err != nil {
					t.Fatal(err)
				}
				after, err := readConfigAuthoringTarget(target)
				if err != nil || !sameConfigAuthoringSnapshot(before, after) || before.Identity != after.Identity {
					t.Fatalf("init froze profile selection in yard settings: %v", err)
				}
			})
		}
	}
}

func TestInitRestrictedRolePreservesTransitionOwnedRegistration(t *testing.T) {
	for _, flat := range []bool{false, true} {
		for _, internal := range []bool{false, true} {
			t.Run(fmt.Sprintf("flat=%t/internal=%t", flat, internal), func(t *testing.T) {
				root, environment, _ := nativeFixture(t)
				profile := filepath.Join(root, "config/yards/profiles/restricted.env")
				if err := os.MkdirAll(filepath.Dir(profile), 0o700); err != nil {
					t.Fatal(err)
				}
				writeCLIFile(t, profile, "ALLOWS_CODING_TOOLS=false\n", 0o600)
				canonical := filepath.Join(root, "state/yards/demo/config.env")
				path := canonical
				if flat {
					path = filepath.Join(root, "state/yards/demo.env")
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				original := "# transition-owned\nYARD_TEMPLATE=restricted\nAGENTS=none\nSSH_PORT=2223\n"
				writeCLIFile(t, path, original, 0o600)
				before, err := config.ReadPersistentFileSnapshot(filepath.Join(root, "state"), path)
				if err != nil {
					t.Fatal(err)
				}
				// A caller-supplied marker alone must retain public init adoption.
				environment = append(environment, "SUBYARD_INTERNAL_MIGRATION_CHILD=1")
				program, err := New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: newInitPlatformFixture()})
				if err != nil {
					t.Fatal(err)
				}
				program.releaseTransitionChild = internal
				loaded, err := program.loadContext("demo")
				if err != nil {
					t.Fatal(err)
				}
				execution, err := program.prepareInitExecution(context.Background(), loaded, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				if execution.integrationBaseline == nil || execution.loaded.Integrations.AllowsCodingTools ||
					len(execution.loaded.Integrations.Effective) != 0 || execution.loaded.Environment["CODING_TOOL_INTEGRATIONS"] != "" {
					t.Fatal("restricted transition init lost its baseline or empty runtime selection")
				}
				if (execution.integrationSelection == nil) != internal {
					t.Fatal("registration adoption did not distinguish trusted transition from caller marker")
				}
				if err := execution.run(context.Background(), program, io.Discard); err != nil {
					t.Fatal(err)
				}
				if internal {
					after, err := config.ReadPersistentFileSnapshot(filepath.Join(root, "state"), path)
					if err != nil || !sameConfigAuthoringSnapshot(before, after) || before.Identity != after.Identity {
						t.Fatalf("transition registration changed: %v", err)
					}
					if flat {
						if _, err := os.Stat(canonical); !os.IsNotExist(err) {
							t.Fatalf("transition registration was migrated: %v", err)
						}
					}
				} else {
					content, err := os.ReadFile(canonical)
					if err != nil || !strings.Contains(string(content), "CODING_TOOL_INTEGRATIONS=''") || strings.Contains(string(content), "AGENTS=") {
						t.Fatalf("public init failed to adopt selection: %q, %v", content, err)
					}
				}
			})
		}
	}
}

func TestInitIntegrationSelectionRejectsStalePlan(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	target := filepath.Join(root, "state/yards/demo/config.env")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, target, "SSH_PORT=2223\nAGENTS=codex\n", 0o600)
	platform := newInitPlatformFixture()
	platform.converged[ports.ReconcileStageInstance] = false
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, InitPlatform: platform})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("demo")
	if err != nil {
		t.Fatal(err)
	}
	proposed, plan, err := program.prepareInitIntegrationSelection(context.Background(), loaded, nil)
	if err != nil {
		t.Fatal(err)
	}
	execution := &initExecution{loaded: proposed, integrationSelection: plan}
	value := "codex"
	if err := config.WritePersistentAssignment(filepath.Join(root, "state"), target, "CODING_TOOL_INTEGRATIONS", &value); err != nil {
		t.Fatal(err)
	}
	if err := plan.check(context.Background(), program, execution); !errors.Is(err, config.ErrPersistentTargetStale) {
		t.Fatalf("concurrent requested selection accepted: %v", err)
	}
	if err := plan.apply(execution); err == nil {
		t.Fatal("concurrent requested selection overwritten")
	}
}

func TestFreshNamedSelectionClearsDerivedRuntimeLinks(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			_, environment, _ := nativeFixture(t)
			if explicit {
				environment = append(environment, "HOST_LINKS=custom:/mnt/host/custom")
			}
			program, err := New(Options{RepositoryRoot: repositoryRoot(t), Environment: environment})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			if !explicit && loaded.Environment["INTEGRATION_HOST_LINKS"] == "" {
				t.Fatal("fixture needs inherited links")
			}
			candidate, err := config.WithIntegrationSelection(loaded, []string{})
			if err != nil {
				t.Fatal(err)
			}
			wanted := ""
			if explicit {
				wanted = "custom:/mnt/host/custom"
			}
			if candidate.Environment["HOST_LINKS"] != wanted || candidate.Environment["INTEGRATION_HOST_LINKS"] != "" {
				t.Fatalf("candidate links: %q / %q", candidate.Environment["HOST_LINKS"], candidate.Environment["INTEGRATION_HOST_LINKS"])
			}
			runtime := program.initPlatform(candidate, nil).(reconcileruntime.Runtime)
			values := map[string]string{}
			for _, pair := range runtime.Environment {
				name, value, _ := strings.Cut(pair, "=")
				values[name] = value
			}
			if values["HOST_LINKS"] != wanted || values["INTEGRATION_HOST_LINKS"] != "" {
				t.Fatalf("runtime retained inherited links: %q / %q", values["HOST_LINKS"], values["INTEGRATION_HOST_LINKS"])
			}
		})
	}
}

func TestInitRejectsIntegrationInputsChangedAfterPlan(t *testing.T) {
	for _, change := range []string{"selection", "role", "same-bytes replacement", "new explicit selection", "new role", "source selection", "source profile"} {
		t.Run(change, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			path := filepath.Join(root, "state/yards/default/config.env")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(change, "new ") {
				writeCLIFile(t, path, "CODING_TOOL_INTEGRATIONS=''\n", 0o600)
			}
			profile := filepath.Join(root, "config/yards/profiles/test-vms.env")
			if err := os.MkdirAll(filepath.Dir(profile), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, profile, "ALLOWS_CODING_TOOLS=false\n", 0o600)
			if strings.HasPrefix(change, "source ") {
				writeCLIFile(t, path, "YARD_TEMPLATE=test-vms\n", 0o600)
				if err := configsync.RegisterSource(filepath.Join(root, "state"), testkit.TempDir(t)); err != nil {
					t.Fatal(err)
				}
			}
			platform := newInitPlatformFixture()
			program, err := New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: platform})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			execution, err := program.prepareInitExecution(context.Background(), loaded, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if execution.integrationSelection != nil {
				t.Fatal("fixture must not require a selection write")
			}
			switch change {
			case "new explicit selection":
				writeCLIFile(t, path, "CODING_TOOL_INTEGRATIONS=''\n", 0o600)
			case "new role":
				writeCLIFile(t, path, "YARD_TEMPLATE=test-vms\n", 0o600)
			case "selection", "source selection":
				writeCLIFile(t, path, "CODING_TOOL_INTEGRATIONS=codex\n", 0o600)
			case "role":
				writeCLIFile(t, path, "CODING_TOOL_INTEGRATIONS=''\nYARD_TEMPLATE=test-vms\n", 0o600)
			case "source profile":
				writeCLIFile(t, profile, "ALLOWS_CODING_TOOLS=true\n", 0o600)
			default:
				writeCLIFile(t, path+".new", "CODING_TOOL_INTEGRATIONS=''\n", 0o600)
				if err := os.Rename(path+".new", path); err != nil {
					t.Fatal(err)
				}
			}
			if err := execution.checkIntegrationBaseline(program); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("changed integration inputs accepted: %v", err)
			}
			if err := execution.run(context.Background(), program, io.Discard); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("changed inputs reached runtime: %v", err)
			}
			if len(platform.applied) != 0 || platform.projectHooks != 0 {
				t.Fatalf("stale init mutated runtime: %#v", platform)
			}
		})
	}
}
