package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

// installedSettingsExecutor reports one installed applied-settings snapshot and
// guest identity for every running yard of the config-apply repair fixture.
type installedSettingsExecutor struct {
	settings    map[string]string
	diagnostics *reconcileruntime.IntegrationDiagnosticEvidence
	selected    []string
	profiles    []string
	// guest answers the core guest receipt projection of a completed release.
	guest map[string]string
	// byInstance answers for one instance instead, as yard init recorded it.
	byInstance map[string]installedSettingsExecutor
}

func (fixture installedSettingsExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if recorded, ok := fixture.byInstance[instance]; ok {
		return recorded.Exec(ctx, project, instance, request)
	}
	if len(request.Command) == 4 && strings.HasPrefix(request.Command[3], "__name__ = 'subyard_release_observer'\n") && fixture.guest != nil {
		payload, _ := json.Marshal(fixture.guest)
		return ports.InstanceExecResult{Stdout: payload}, nil
	}
	if len(request.Command) == 4 && strings.HasPrefix(request.Command[3], "__name__ = 'subyard_profile_selection_observer'\n") {
		selected := fixture.profiles
		if selected == nil {
			selected = []string{}
		}
		payload, _ := json.Marshal(selected)
		return ports.InstanceExecResult{Stdout: payload}, nil
	}
	if len(request.Command) == 5 {
		value := map[string]any{"installed": fixture.settings != nil, "diagnostics": fixture.diagnostics}
		if fixture.settings != nil {
			value["applied_settings"], value["selected"] = fixture.settings, append([]string{}, fixture.selected...)
		}
		payload, _ := json.Marshal(value)
		return ports.InstanceExecResult{Stdout: payload}, nil
	}
	payload, _ := json.Marshal(map[string]string{"DEV_USER": "dev", "DEV_UID": "1000"})
	return ports.InstanceExecResult{Stdout: payload}, nil
}

type pendingInitFixture struct {
	configApplyRepairFixture
	journal []byte
}

// newPendingInitFixture installs a completed release whose repair is required and
// writes desired yard configuration over the installed dev/1000 identity.
func newPendingInitFixture(t *testing.T, named bool, yards map[string]string) pendingInitFixture {
	t.Helper()
	fixture := newConfigApplyRepairFixture(t, named)
	cli := fixture.cli
	testkit.WriteFile(t, filepath.Join(cli.options.RepositoryRoot, "config", "projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o644)
	configHome := cli.env["SUBYARD_CONFIG_HOME"]
	if err := os.MkdirAll(filepath.Join(cli.options.RepositoryRoot, "config/skills/core/subyard-environment"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(cli.options.RepositoryRoot, "config/skills/core/subyard-environment/facts.py"), "# identity fixture\n", 0o600)
	// The installed snapshot records the integration inputs init applied before
	// the yard configuration below was authored.
	loaded, err := cli.resolveReleaseTransitionContext(context.Background(), "default", configHome)
	if err != nil {
		t.Fatal(err)
	}
	cli.options.Executor = installedSettingsExecutor{settings: loaded.Catalog.IntegrationInitSettings(loaded.Environment), selected: []string{"codex"}}
	for name, content := range yards {
		path := filepath.Join(configHome, "yards", name, "config.env")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		previous, _ := os.ReadFile(path)
		writeCLIFile(t, path, string(previous)+content, 0o600)
	}
	journal, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	return pendingInitFixture{configApplyRepairFixture: fixture, journal: journal}
}

func (fixture pendingInitFixture) setPower(t *testing.T, yard, status string) {
	t.Helper()
	incus := fixture.cli.options.Incus.(*testkit.Incus)
	for key, instance := range incus.Instances {
		if instance.Name == fixture.instanceName(t, yard) {
			instance.Status = status
			incus.Instances[key] = instance
			return
		}
	}
	t.Fatalf("yard %s has no fixture instance", yard)
}

func (fixture pendingInitFixture) instanceName(t *testing.T, yard string) string {
	t.Helper()
	loaded, err := fixture.cli.resolveReleaseTransitionContext(context.Background(), yard, fixture.cli.env["SUBYARD_CONFIG_HOME"])
	if err != nil {
		t.Fatal(err)
	}
	return loaded.Context.YardInstanceName
}

func (fixture pendingInitFixture) requireJournalUnchanged(t *testing.T) {
	t.Helper()
	current, err := os.ReadFile(fixture.journalPath)
	if err != nil || !bytes.Equal(current, fixture.journal) {
		t.Fatalf("release transition journal changed before the release repair: %v", err)
	}
}

type nestedCall struct {
	arguments []string
}

// program runs one command on the fixture; nested follow-up commands are
// recorded and answered by respond.
func (fixture pendingInitFixture) program(t *testing.T, arguments []string, prompt ports.Prompter, respond func(nestedCall, io.Writer) int) (*CLI, *[]nestedCall, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	options := fixture.cli.options
	options.Arguments, options.Prompt, options.Stdout, options.Stderr = arguments, prompt, &stdout, &stderr
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	calls := &[]nestedCall{}
	program.nestedCommand = func(_ context.Context, arguments []string, diagnostics io.Writer) int {
		call := nestedCall{arguments: slices.Clone(arguments)}
		*calls = append(*calls, call)
		if diagnostics == nil {
			diagnostics = io.Discard
		}
		return respond(call, diagnostics)
	}
	return program, calls, &stdout, &stderr
}

const pendingRename = "DEV_USER=other\nDEV_UID=1002\n"

func TestPendingInitListingIsStableAndHidesSensitiveValues(t *testing.T) {
	plan := pendingInitPlan{Command: "migrate", Release: "r", Yards: []pendingInitYard{
		{Name: "alpha", Baseline: true, Pending: true, Settings: []pendingInitSetting{
			{Name: "DEV_UID", Installed: "1000", Desired: "1002", InstalledSet: true, DesiredSet: true},
			{Name: "ENVIRONMENT_PROFILES", Desired: "sample", DesiredSet: true},
			{Name: "TOKEN", Installed: "old-secret", Desired: "new-secret", Kind: domain.ConfigurationSensitive},
		}},
		{Name: "beta", Pending: true, Settings: []pendingInitSetting{{Name: "DEV_USER", Installed: "dev", Desired: "other", InstalledSet: true, DesiredSet: true}}},
		{Name: "gamma", Baseline: true},
		{Name: "delta", State: "stopped"},
	}}
	want := []string{
		"Init-owned integration settings awaiting yard init (installed -> desired); yard init runs after the release repair is ready:",
		"  yard alpha:",
		"    DEV_UID  1000 -> 1002",
		"    ENVIRONMENT_PROFILES  (unset) -> sample",
		"    TOKEN  changed",
		"  yard beta: installed integration settings unknown (no applied baseline)",
		"    DEV_USER  dev -> other",
		"  yard gamma: integration settings current",
		"  yard delta: stopped; not inspected",
	}
	if got := plan.lines(); !slices.Equal(got, want) {
		t.Fatalf("listing:\n%s", strings.Join(got, "\n"))
	}
	if listing := strings.Join(plan.lines(), "\n"); strings.Contains(listing, "secret") {
		t.Fatalf("sensitive value disclosed: %s", listing)
	}
	if got := plan.pendingYards(); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("pending yards: %v", got)
	}
	steps := plan.steps([]domain.OperationStep{{ID: "release.activate", Target: "root/current", Observed: "active release r", Desired: "verified active release r", Decision: domain.StepApply, Verify: "ready", Consequence: "activate and verify release r"}})
	if err := domain.ValidateOperationSteps(steps); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, step := range steps {
		ids = append(ids, step.ID+"="+step.Target+"="+string(step.Decision))
	}
	if !slices.Equal(ids, []string{"release.activate=root/current=apply", "release.init.alpha=yard alpha=conditional", "release.init.beta=yard beta=conditional"}) ||
		!slices.Equal(steps[2].DependsOn, []string{"release.activate"}) {
		t.Fatalf("init must follow the release repair: %v %v", ids, steps[2].DependsOn)
	}
}

func TestPendingInitSettingsCompareIntegrationValuesInOrder(t *testing.T) {
	catalog, err := config.LoadCatalog(repositoryRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	desired := config.Loaded{Catalog: catalog, Environment: map[string]string{"SSH_PORT": "2", "DEV_USER": "other", "HOST_LINKS": "a:b", "YARD_IMAGE": "same", "LIMITS_MEMORY": ""},
		Integrations: config.IntegrationSelection{Requested: []string{"codex"}, Effective: []string{"codex"}, AllowsCodingTools: true}}
	installed := config.Loaded{Catalog: catalog, Environment: map[string]string{"SSH_PORT": "1", "DEV_USER": "dev", "YARD_IMAGE": "same", "INTEGRATION_HOST_LINKS": "x"},
		Integrations: config.IntegrationSelection{AllowsCodingTools: false}}
	var got []string
	for _, setting := range pendingInitSettings(desired, installed) {
		got = append(got, setting.Name+"="+strings.ReplaceAll(setting.Value(), " -> ", "->"))
	}
	// SSH_PORT applies on the next command, so it never awaits init.
	want := []string{"ALLOWS_CODING_TOOLS=false->true", "CODING_TOOL_INTEGRATIONS=(empty)->codex", "DEV_USER=dev->other", "HOST_LINKS=(unset)->a:b", "INTEGRATION_HOST_LINKS=x->(unset)"}
	if !slices.Equal(got, want) {
		t.Fatalf("differences: %v", got)
	}
}

func TestPendingInitInspectsRunningYardsAndSkipsStoppedOnes(t *testing.T) {
	fixture := newPendingInitFixture(t, true, map[string]string{"default": pendingRename, "named": pendingRename})
	fixture.setPower(t, "named", "Stopped")
	plan, err := fixture.cli.inspectPendingInit(context.Background(), fixture.cli.env["SUBYARD_CONFIG_HOME"])
	if err != nil {
		t.Fatal(err)
	}
	listing := strings.Join(plan.lines(), "\n")
	for _, expected := range []string{"  yard default:\n    DEV_UID  1000 -> 1002\n    DEV_USER  dev -> other\n", "  yard named: stopped; not inspected"} {
		if !strings.Contains(listing, expected) {
			t.Fatalf("missing %q in listing:\n%s", expected, listing)
		}
	}
	if !slices.Equal(plan.pendingYards(), []string{"default"}) {
		t.Fatalf("pending yards: %v", plan.pendingYards())
	}
	// Without an applied snapshot the installed configuration is unknown and
	// the repair observes the same configuration as init would apply.
	fixture.cli.options.Executor = installedSettingsExecutor{}
	plan, err = fixture.cli.inspectPendingInit(context.Background(), fixture.cli.env["SUBYARD_CONFIG_HOME"])
	if err != nil || !strings.Contains(strings.Join(plan.lines(), "\n"), "yard default: installed integration settings unknown (no applied baseline)") {
		t.Fatalf("unknown baseline listing: %v %v", plan.lines(), err)
	}
}

func TestPendingInitListsInstalledGuestReceipts(t *testing.T) {
	fixture := newPendingInitFixture(t, false, map[string]string{"default": "WEB_PREVIEW_HOST_PORT=9090\nENVIRONMENT_PROFILES='sample  extra'\n"})
	incus := fixture.cli.options.Incus.(*testkit.Incus)
	for key, instance := range incus.Instances {
		// A previously installed preview helper makes the receipts authoritative.
		instance.Config = map[string]string{"user.subyard.preview_sha256": strings.Repeat("f", 64)}
		incus.Instances[key] = instance
	}
	executor := fixture.cli.options.Executor.(installedSettingsExecutor)
	executor.profiles = []string{"sample", "extra"}
	executor.guest = map[string]string{"WEB_PREVIEW_HOST_PORT": "8080", "ENVIRONMENT_PROFILES": "sample extra", "ALLOWS_CODING_TOOLS": "true", "DEV_USER": "dev"}
	fixture.cli.options.Executor = executor
	plan, err := fixture.cli.inspectPendingInit(context.Background(), fixture.cli.env["SUBYARD_CONFIG_HOME"])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		pendingInitHeading,
		"  yard default:",
		"    WEB_PREVIEW_HOST_PORT  8080 -> 9090",
	}
	if got := plan.lines(); !slices.Equal(got, want) {
		t.Fatalf("guest receipt listing:\n%s", strings.Join(got, "\n"))
	}
	// Equal receipts leave the release repair exactly as upstream plans it.
	executor.guest["WEB_PREVIEW_HOST_PORT"] = "9090"
	fixture.cli.options.Executor = executor
	if plan, err = fixture.cli.inspectPendingInit(context.Background(), fixture.cli.env["SUBYARD_CONFIG_HOME"]); err != nil || len(plan.pendingYards()) != 0 {
		t.Fatalf("current guest receipts reported pending: %v %v", plan.lines(), err)
	}
}

// The usage receipt records only the guest architecture's archive, which the
// core guest projection assigns to both pins. Distinct shipped pins are current
// when the archive matches either; a changed pin is pending.
func TestPendingInitUsagePinsCompareInstalledArchitecture(t *testing.T) {
	amd64, arm64 := strings.Repeat("4a", 32), strings.Repeat("3b", 32)
	for _, scenario := range []string{"current amd64", "current arm64", "changed pins"} {
		t.Run(scenario, func(t *testing.T) {
			desiredAMD64, desiredARM64 := amd64, arm64
			if scenario == "changed pins" {
				desiredAMD64, desiredARM64 = strings.Repeat("5c", 32), strings.Repeat("6d", 32)
			}
			fixture := newPendingInitFixture(t, false, map[string]string{"default": fmt.Sprintf(
				"CCUSAGE_VERSION=1.2.3\nCCUSAGE_SHA256_AMD64=%s\nCCUSAGE_SHA256_ARM64=%s\n", desiredAMD64, desiredARM64)})
			loaded, err := fixture.cli.resolveReleaseTransitionContext(context.Background(), "default", fixture.cli.env["SUBYARD_CONFIG_HOME"])
			if err != nil {
				t.Fatal(err)
			}
			incus := fixture.cli.options.Incus.(*testkit.Incus)
			for key, instance := range incus.Instances {
				instance.Config = map[string]string{"user.subyard.ccusage_version": "1.2.3"}
				incus.Instances[key] = instance
			}
			archive := amd64
			if scenario == "current arm64" {
				archive = arm64
			}
			// yard init applied the integration settings; only guest receipts remain.
			executor := installedSettingsExecutor{settings: loaded.Catalog.IntegrationInitSettings(loaded.Environment), selected: []string{"codex"},
				guest: map[string]string{"CCUSAGE_VERSION": "1.2.3", "CCUSAGE_SHA256_AMD64": archive, "CCUSAGE_SHA256_ARM64": archive, "ALLOWS_CODING_TOOLS": "true", "DEV_USER": "dev"}}
			fixture.cli.options.Executor = executor
			plan, err := fixture.cli.inspectPendingInit(context.Background(), fixture.cli.env["SUBYARD_CONFIG_HOME"])
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "changed pins" {
				if len(plan.pendingYards()) != 0 {
					t.Fatalf("initialized usage pins reported pending:\n%s", strings.Join(plan.lines(), "\n"))
				}
				return
			}
			want := []string{pendingInitHeading, "  yard default:",
				"    CCUSAGE_SHA256_AMD64  " + amd64 + " -> " + desiredAMD64,
				"    CCUSAGE_SHA256_ARM64  " + amd64 + " -> " + desiredARM64,
			}
			if got := plan.lines(); !slices.Equal(got, want) {
				t.Fatalf("changed usage pins listing:\n%s", strings.Join(got, "\n"))
			}
		})
	}
}

// A yard role that suppresses an inherited integration request installs and
// records the empty effective selection, so the request does not await init.
func TestPendingInitComparesEffectiveSelection(t *testing.T) {
	catalog, err := config.LoadCatalog(repositoryRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{"ALLOWS_CODING_TOOLS": "false", "CODING_TOOL_INTEGRATIONS": ""}
	desired := config.Loaded{Catalog: catalog, Environment: environment,
		Integrations: config.IntegrationSelection{Requested: []string{"codex"}, Effective: []string{}, Suppressed: true}}
	installed := config.Loaded{Catalog: catalog, Environment: maps.Clone(environment),
		Integrations: config.IntegrationSelection{Requested: []string{}, Effective: []string{}}}
	if settings := pendingInitSettings(desired, installed); len(settings) != 0 {
		t.Fatalf("suppressed request reported pending: %#v", settings)
	}
	desired.Integrations.Effective = []string{"codex"}
	if settings := pendingInitSettings(desired, installed); len(settings) != 1 || settings[0].Name != "CODING_TOOL_INTEGRATIONS" || settings[0].Desired != "codex" {
		t.Fatalf("effective selection change not listed: %#v", settings)
	}
	// An explicit empty selection is listed as empty.
	desired.Integrations = config.IntegrationSelection{Present: true, Requested: []string{}, Effective: []string{}, AllowsCodingTools: true}
	installed.Integrations = config.IntegrationSelection{Present: true, Requested: []string{"codex"}, Effective: []string{"codex"}, AllowsCodingTools: true}
	if settings := pendingInitSettings(desired, installed); len(settings) != 1 || settings[0].Value() != "codex -> (empty)" {
		t.Fatalf("explicit empty selection not listed as empty: %#v", settings)
	}
}

// The sealed fixture owner becomes ready only at its convergence boundary. Each
// nested init can prove the release repair already ran; planning remains read-only.
func enablePendingRepair(t *testing.T, fixture *pendingInitFixture) {
	t.Helper()
	root := fixture.cli.env["YARD_RUNTIME_ROOT"]
	release := filepath.Join(root, "releases", "1.2.3-aaaaaaaaaaaa")
	engine := filepath.Join(release, "bin/yard-engine")
	payload := mustReadConfigFixture(t, engine)
	payload = []byte(strings.Replace(string(payload), "*'\"mode\":\"converge\"'*)", "*'\"mode\":\"converge\"'*)\n      : > '"+fixture.readyMarker+"'", 1))
	writePendingRepairEngine(t, fixture, payload)
}

func writePendingRepairEngine(t *testing.T, fixture *pendingInitFixture, payload []byte) {
	t.Helper()
	root := fixture.cli.env["YARD_RUNTIME_ROOT"]
	release := filepath.Join(root, "releases", "1.2.3-aaaaaaaaaaaa")
	engine := filepath.Join(release, "bin/yard-engine")
	testkit.WriteFile(t, engine, payload, 0o700)
	registry := mustReadConfigFixture(t, filepath.Join(release, "config/release-transition.json"))
	manifest := []byte(fmt.Sprintf("%x  ./bin/yard-engine\n%x  ./config/release-transition.json\n", sha256.Sum256(payload), sha256.Sum256(registry)))
	testkit.WriteFile(t, filepath.Join(release, "runtime-files.sha256"), manifest, 0o600)
	journal, err := releasetransition.ParseJournal(mustReadConfigFixture(t, fixture.journalPath))
	if err != nil {
		t.Fatal(err)
	}
	journal.ArtifactDigest = releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(manifest)))
	encoded, err := releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, fixture.journalPath, encoded, 0o600)
	fixture.journal = encoded
}

func TestReleaseRepairRunsPendingInitAfterReady(t *testing.T) {
	for _, command := range []string{"migrate", "update"} {
		for _, yes := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/yes=%v", command, yes), func(t *testing.T) {
				fixture := newPendingInitFixture(t, true, map[string]string{"default": pendingRename, "named": "DEV_USER=named\n"})
				enablePendingRepair(t, &fixture)
				arguments := []string{command}
				prompt := &testkit.Prompt{Answers: []bool{true}}
				if yes {
					arguments = append(arguments, "--yes")
				}
				program, calls, stdout, stderr := fixture.program(t, arguments, prompt, func(call nestedCall, _ io.Writer) int {
					if _, err := os.Stat(fixture.readyMarker); err != nil {
						t.Fatal("init ran before the release was ready", err)
					}
					return 0
				})
				if code := program.Run(context.Background()); code != 0 {
					t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
				}
				want := [][]string{{"-Y", "default", "init", "--yes"}, {"-Y", "named", "init", "--yes"}}
				if len(*calls) != len(want) {
					t.Fatalf("calls=%#v", *calls)
				}
				for i, call := range *calls {
					if !slices.Equal(call.arguments, want[i]) {
						t.Fatalf("call=%v want=%v", call.arguments, want[i])
					}
				}
				prompts := 1
				if yes {
					prompts = 0
				}
				if len(prompt.Requests) != prompts {
					t.Fatalf("prompts=%#v", prompt.Requests)
				}
				text := stdout.String()
				if strings.Index(text, "release.init.default") < strings.Index(text, "release.activate") ||
					!strings.Contains(text, "DEV_USER  dev -> other") || !strings.Contains(text, "DEV_USER  dev -> named") {
					t.Fatalf("plan/listing=%s", text)
				}
			})
		}
	}
}

func TestPostRepairInitCollectsEveryRefusalAndKeepsReady(t *testing.T) {
	fixture := newPendingInitFixture(t, true, map[string]string{"default": pendingRename, "named": pendingRename})
	enablePendingRepair(t, &fixture)
	program, calls, stdout, stderr := fixture.program(t, []string{"migrate", "--yes"}, nil, func(call nestedCall, diagnostics io.Writer) int {
		if _, err := os.Stat(fixture.readyMarker); err != nil {
			t.Fatal("release not ready", err)
		}
		fmt.Fprintf(diagnostics, "yard: init: %s has connected sessions\n", call.arguments[1])
		return 1
	})
	if code := program.Run(context.Background()); code != 1 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if len(*calls) != 2 {
		t.Fatalf("did not attempt every pending init: %#v", *calls)
	}
	text := stderr.String()
	first, second := strings.Index(text, "yard default:"), strings.Index(text, "yard named:")
	if first < 0 || second < first || !strings.Contains(text, "next: run yard -Y default init") || !strings.Contains(text, "next: run yard -Y named init") || !strings.Contains(text, "is ready") {
		t.Fatal(text)
	}
	if remaining, err := program.inspectMutationGate(context.Background(), "default"); err != nil || remaining != nil {
		t.Fatalf("init refusal undid release readiness: %v %v", remaining, err)
	}
}

func TestRepairWithoutPendingInitPreservesPlan(t *testing.T) {
	fixture := newPendingInitFixture(t, false, nil)
	program, calls, stdout, stderr := fixture.program(t, []string{"migrate"}, &testkit.Prompt{Answers: []bool{false}}, func(nestedCall, io.Writer) int { t.Fatal("no init pending"); return 1 })
	if code := program.Run(context.Background()); code != 1 || !strings.Contains(stderr.String(), "operation declined") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if len(*calls) != 0 || strings.Contains(stdout.String(), "awaiting yard init") || strings.Contains(stdout.String(), "release.init.") {
		t.Fatal(stdout.String())
	}
	fixture.requireJournalUnchanged(t)
}

func TestRepairPendingInitWithoutTerminalRequiresConsent(t *testing.T) {
	fixture := newPendingInitFixture(t, false, map[string]string{"default": pendingRename})
	enablePendingRepair(t, &fixture)
	program, calls, stdout, stderr := fixture.program(t, []string{"migrate"}, nil, func(nestedCall, io.Writer) int { t.Fatal("no confirmation"); return 1 })
	if code := program.Run(context.Background()); code != 1 || len(*calls) != 0 {
		t.Fatalf("code=%d calls=%v stderr=%s", code, *calls, stderr)
	}
	if !strings.Contains(stdout.String(), "DEV_USER  dev -> other") {
		t.Fatal(stdout.String())
	}
	if _, err := os.Stat(fixture.readyMarker); !os.IsNotExist(err) {
		t.Fatal("unconfirmed repair ran", err)
	}
}

func TestRepairRPCListsInitWithoutRunningIt(t *testing.T) {
	fixture := newPendingInitFixture(t, false, map[string]string{"default": pendingRename})
	enablePendingRepair(t, &fixture)
	program, calls, stdout, stderr := fixture.program(t, nil, nil, func(nestedCall, io.Writer) int { t.Fatal("RPC ran a nested init"); return 1 })
	loaded, err := program.currentMigrationContext()
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded}
	defer handler.closePlans()
	_, err = handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "repair-rpc", Params: json.RawMessage(`{"command":"migrate","arguments":[]}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = handler.Handle(context.Background(), rpc.Call{Method: "operation.execute", OperationID: "repair-rpc", Params: json.RawMessage(`{"confirmed":true}`)}, func(method string, event any) (uint64, error) {
		encoded, _ := json.Marshal(event)
		fmt.Fprintln(stdout, string(encoded))
		return 1, nil
	})
	if err != nil {
		t.Fatalf("RPC execute=%v stdout=%s stderr=%s", err, stdout, stderr)
	}
	// RPC executes prepared commands with stderr as their diagnostics writer;
	// stdout carries the session's framed responses and events.
	if len(*calls) != 0 || !strings.Contains(stderr.String(), "yard -Y default init") || !strings.Contains(stderr.String(), "DEV_USER  dev -> other") {
		t.Fatalf("RPC omitted pending diagnostics: stdout=%s stderr=%s", stdout, stderr)
	}
}

// The real prepared init path must run after repair with ordinary admission.
// A converged init records no new snapshot; irrelevant settings must still cease
// to be pending rather than requiring an endless series of no-op inits.
func TestPendingInitEmptyAfterRealNoOpInit(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	testkit.WriteFile(t, filepath.Join(root, "config/agents.env"), []byte("CODING_TOOL_INTEGRATIONS=codex\nAGENT_codex_COMMAND=codex\nAGENT_claude_COMMAND=claude\n"), 0o600)
	testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o644)
	platform := newInitPlatformFixture()
	platform.converged[ports.ReconcileStageProject] = true
	platform.converged[ports.ReconcileStageFinalize] = true
	if _, err := configsync.EnsureHostID(configHome, nil); err != nil {
		t.Fatal(err)
	}
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
	var output bytes.Buffer
	options := Options{RepositoryRoot: root, Environment: environment, Incus: incus, InitPlatform: platform, Stdout: &output, Stderr: &output, Arguments: []string{"init", "--yes"}}
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	incus.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName] = ports.InstanceInfo{Name: loaded.Context.YardInstanceName, Status: "Running"}
	baseline := installedSettingsExecutor{settings: loaded.Catalog.IntegrationInitSettings(loaded.Environment), selected: []string{"codex"}}
	options.Executor = baseline
	program, err = New(options)
	if err != nil {
		t.Fatal(err)
	}
	writeConfigCommandFile(t, filepath.Join(configHome, "config.env"), "HOST_CLAUDE_MD=/tmp/unused-instructions\n")
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("real init code=%d output=%s", code, &output)
	}
	if len(platform.applied) != 0 || platform.projectHooks != 1 {
		t.Fatalf("not a no-op init: applied=%v hooks=%d", platform.applied, platform.projectHooks)
	}
	program.options.InitPlatform = nil
	plan, err := program.inspectPendingInit(context.Background(), configHome)
	if err != nil || len(plan.pendingYards()) != 0 {
		t.Fatalf("successful no-op init left pending: %v %v", plan.lines(), err)
	}
	if !maps.Equal(baseline.settings, program.options.Executor.(installedSettingsExecutor).settings) {
		t.Fatal("no-op init fabricated a new applied snapshot")
	}
}

// Exercise the direct boundary without a sealed-runtime stub: the release leaf
// publishes readiness, closes its locks, then the confirmed ordinary inits run.
func TestPreparedRepairPostInitBoundary(t *testing.T) {
	for _, scenario := range []string{"yes", "confirmed", "refusals", "declined", "no-pending", "unavailable", "stopped", "resumed", "interactive-edited", "nonTTY-edited", "assume-yes-edited"} {
		t.Run(scenario, func(t *testing.T) {
			edits := map[string]string{"default": pendingRename, "named": pendingRename}
			if scenario == "no-pending" || scenario == "unavailable" || scenario == "stopped" {
				edits = nil
			}
			fixture := newPendingInitFixture(t, true, edits)
			if scenario == "unavailable" {
				writeConfigCommandFile(t, filepath.Join(fixture.cli.env["SUBYARD_CONFIG_HOME"], "yards/named/config.env"), "SSH_PORT=2223\nAGENT_codex_PROVISION="+filepath.Join(fixture.cli.options.RepositoryRoot, "missing-provision")+"\n")
			}
			if scenario == "stopped" {
				fixture.setPower(t, "named", "Stopped")
			}
			releaseRan, closed := false, false
			prompt := &testkit.Prompt{Answers: []bool{scenario != "declined"}}
			program, calls, stdout, stderr := fixture.program(t, nil, prompt, func(call nestedCall, diagnostics io.Writer) int {
				if !releaseRan || !closed {
					t.Fatal("init ran before release readiness and lock closure")
				}
				want := []string{"-Y", call.arguments[1], "init"}
				if scenario != "interactive-edited" || call.arguments[1] != "named" {
					want = append(want, "--yes")
				}
				if !slices.Equal(call.arguments, want) {
					t.Fatalf("init consent: got %v want %v", call.arguments, want)
				}
				if scenario == "refusals" {
					fmt.Fprintf(diagnostics, "%s has connected sessions\n", call.arguments[1])
					return 1
				}
				return 0
			})
			program.promptInputTerminal = func() bool { return scenario == "interactive-edited" }
			if scenario == "assume-yes-edited" {
				program.env["ASSUME_YES"] = "1"
			}
			if scenario == "resumed" {
				_, journal := repairRequest(t, fixture)
				publishRepairResume(t, fixture, journal, false)
			}
			loaded, err := program.currentMigrationContext()
			if err != nil {
				t.Fatal(err)
			}
			definition, _ := program.manifest.Lookup("migrate")
			prepared := &preparedCommand{CLI: program, Loaded: loaded, Definition: definition, lifecycle: &preparedLifecycle{}, exactState: "release-state", stepsComplete: true, executeNoOp: true}
			prepared.steps = func() []domain.OperationStep {
				return []domain.OperationStep{{ID: "release.activate", Target: "release", Observed: "drift", Desired: "ready", Decision: domain.StepApply, Verify: "readiness", Consequence: "repair active release"}}
			}
			prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
				return "migrate.apply", domain.ActionDelta{Changed: scenario != "resumed"}, nil
			}
			prepared.execute = func(context.Context, *application.Orchestrator, io.Writer) (domain.AdapterResult, error) {
				releaseRan = true
				if strings.Contains(scenario, "edited") {
					writeConfigCommandFile(t, filepath.Join(program.env["SUBYARD_CONFIG_HOME"], "yards/named/config.env"), "SSH_PORT=2223\nDEV_USER=edited\nDEV_UID=1003\n")
				}
				return domain.AdapterResult{Status: "ok"}, nil
			}
			prepared.closeResource = func() error { closed = true; return nil }
			repair, _ := repairRequest(t, fixture)
			execution := &releaseExecution{configHome: loaded.Context.Paths.ConfigHome, prepared: releaseruntime.Prepared{Request: repair, Action: "migrate.apply", Changed: scenario != "resumed", SourceRelease: "1.2.3-aaaaaaaaaaaa", TargetRelease: "1.2.3-aaaaaaaaaaaa"}}
			if err := prepared.preparePostRepairInit(context.Background(), execution); err != nil {
				t.Fatal(err)
			}
			prepared.policy = commandPolicy(definition, loaded.Context, nil, nil)
			if err := prepared.preparePlan(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			if scenario == "resumed" && (prepared.pendingInit == nil || !prepared.Plan.Assessment.Changed) {
				t.Fatal("resumed repair omitted the pending init plan and its consent")
			}
			code := program.runPreparedCommand(context.Background(), prepared, scenario == "yes" || scenario == "refusals" || scenario == "no-pending")
			expectedCode, expectedCalls := 0, 2
			if scenario == "declined" {
				expectedCode, expectedCalls = 1, 0
			}
			if scenario == "refusals" {
				expectedCode = 1
			}
			if scenario == "nonTTY-edited" {
				expectedCode, expectedCalls = 1, 1
			}
			if scenario == "no-pending" || scenario == "unavailable" || scenario == "stopped" {
				expectedCalls = 0
			}
			if code != expectedCode || len(*calls) != expectedCalls {
				t.Fatalf("code=%d calls=%v stdout=%s stderr=%s", code, *calls, stdout, stderr)
			}
			if scenario == "refusals" {
				text := stderr.String()
				first, second := strings.Index(text, "yard default:"), strings.Index(text, "yard named:")
				if first < 0 || second < first || !strings.Contains(text, "yard -Y default init") || !strings.Contains(text, "yard -Y named init") || !strings.Contains(text, "is ready") {
					t.Fatal(text)
				}
			}
			if scenario == "declined" && releaseRan {
				t.Fatal("declined repair ran")
			}
			if scenario == "no-pending" && (prepared.pendingInit != nil || strings.Contains(stdout.String(), "release.init")) {
				t.Fatal("pending-free plan changed")
			}
			if scenario == "unavailable" && !strings.Contains(stdout.String(), "yard named: not inspected (unavailable: desired integration sources cannot be read)") {
				t.Fatalf("unavailable-only listing missing from command output: %s", stdout)
			}
			if scenario == "stopped" && !strings.Contains(stdout.String(), "yard named: stopped; not inspected") {
				t.Fatalf("stopped-only listing missing from command output: %s", stdout)
			}
			if (scenario == "unavailable" || scenario == "stopped") && strings.Contains(stdout.String(), "release.init") {
				t.Fatal("uninspected yard scheduled init")
			}
		})
	}
}

func TestUpdateRetainsStrictDisabledRoleSourceValidation(t *testing.T) {
	fixture := newPendingInitFixture(t, false, map[string]string{"default": "YARD_TEMPLATE=disabled\nAGENT_codex_PROVISION=/missing-disabled-role-provision\n"})
	writeConfigCommandFile(t, filepath.Join(fixture.cli.options.RepositoryRoot, "config/yards/profiles/disabled.env"), "ALLOWS_CODING_TOOLS=false\n")
	program, calls, stdout, stderr := fixture.program(t, []string{"update", "--version", "1.2.4", "--yes"}, nil, func(nestedCall, io.Writer) int {
		t.Fatal("invalid configuration ran a follow-up")
		return 1
	})
	if code := program.Run(context.Background()); code != 2 || !strings.Contains(stderr.String(), "file setting source is not openable") || len(*calls) != 0 {
		t.Fatalf("ordinary update changed source validation: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
}

type pendingReadFailurePlatform struct{ *initPlatformFixture }

func (pendingReadFailurePlatform) AppliedIntegrationConfiguration(context.Context) (reconcileruntime.AppliedIntegrationConfiguration, error) {
	return reconcileruntime.AppliedIntegrationConfiguration{}, fmt.Errorf("applied settings unavailable")
}
func (pendingReadFailurePlatform) InstalledReleaseSettings(context.Context) (map[string]string, error) {
	return nil, fmt.Errorf("guest receipts unavailable")
}

func TestPendingInitCollectsIndependentReadRefusals(t *testing.T) {
	fixture := newPendingInitFixture(t, true, nil)
	fixture.cli.options.InitPlatform = pendingReadFailurePlatform{newInitPlatformFixture()}
	_, err := fixture.cli.inspectPendingInit(context.Background(), fixture.cli.env["SUBYARD_CONFIG_HOME"])
	var refusals *domain.Refusals
	if !errors.As(err, &refusals) || len(refusals.Unwrap()) != 4 {
		t.Fatalf("independent reads were skipped: %v", err)
	}
	for _, yard := range []string{"default", "named"} {
		for _, phase := range []string{"installed configuration", "installed guest configuration"} {
			if !strings.Contains(err.Error(), "yard "+yard+": inspect "+phase+":") {
				t.Fatal(err)
			}
		}
	}
}

type pendingUnavailableIncus struct {
	*testkit.Incus
	unavailable string // Empty means every instance read fails.
	inspected   []string
}

func (fixture *pendingUnavailableIncus) Instance(ctx context.Context, project, name string) (ports.InstanceInfo, error) {
	fixture.inspected = append(fixture.inspected, name)
	if fixture.unavailable == "" || name == fixture.unavailable {
		return ports.InstanceInfo{}, errors.New("synthetic instance read unavailable")
	}
	return fixture.Incus.Instance(ctx, project, name)
}

func TestPendingInitLeavesUnavailableInstancesToReleaseAssessment(t *testing.T) {
	for _, allUnavailable := range []bool{false, true} {
		t.Run(fmt.Sprintf("all-unavailable=%t", allUnavailable), func(t *testing.T) {
			fixture := newPendingInitFixture(t, true, map[string]string{"default": pendingRename, "named": pendingRename})
			defaultInstance, namedInstance := fixture.instanceName(t, "default"), fixture.instanceName(t, "named")
			incus := &pendingUnavailableIncus{Incus: fixture.cli.options.Incus.(*testkit.Incus), unavailable: defaultInstance}
			fixture.cli.options.Incus = incus
			if allUnavailable {
				incus.unavailable = ""
				// statusPorts may supply a native executor, but must retain the
				// injected reader. Unavailable yards require no guest reads.
				fixture.cli.options.Executor = nil
			}
			plan, err := fixture.cli.inspectPendingInit(context.Background(), fixture.cli.env["SUBYARD_CONFIG_HOME"])
			if err != nil {
				t.Fatalf("instance unavailability added a release refusal: %v", err)
			}
			if !slices.Contains(incus.inspected, defaultInstance) || !slices.Contains(incus.inspected, namedInstance) {
				t.Fatalf("pending detection bypassed the injected reader: %v", incus.inspected)
			}
			if len(plan.Yards) != 2 || plan.Yards[0].State != "unavailable" || plan.Yards[0].Pending {
				t.Fatalf("unavailable yard was inspected or scheduled for init: %#v", plan.Yards)
			}
			wantPending := []string{"named"}
			if allUnavailable {
				wantPending = nil
				if !slices.Equal(incus.inspected, []string{defaultInstance, namedInstance}) {
					t.Fatalf("unavailable instances received additional reads: %v", incus.inspected)
				}
			}
			if !slices.Equal(plan.pendingYards(), wantPending) {
				t.Fatalf("pending=%v want=%v", plan.pendingYards(), wantPending)
			}
			if !strings.Contains(strings.Join(plan.lines(), "\n"), "yard default: not inspected (unavailable)") {
				t.Fatalf("unavailability was not disclosed: %v", plan.lines())
			}
		})
	}
}

func TestPostRepairInitRechecksConsentAfterEdit(t *testing.T) {
	for _, scenario := range []struct {
		name                               string
		explicit, terminal, editDuringInit bool
		wantCode                           int
		wantYes                            bool
	}{
		{"unchanged", false, false, false, 0, true},
		{"explicit yes", true, false, false, 0, true},
		{"interactive changed", false, true, false, 0, false},
		{"terminal unavailable", false, false, false, 1, false},
		{"edited during earlier init", false, false, true, 1, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newPendingInitFixture(t, true, map[string]string{"default": pendingRename, "named": pendingRename})
			plan, err := fixture.cli.inspectPendingInit(context.Background(), fixture.cli.env["SUBYARD_CONFIG_HOME"])
			if err != nil {
				t.Fatal(err)
			}
			plan.Command, plan.Release = "migrate", "fixture-release"
			program, calls, stdout, stderr := fixture.program(t, nil, nil, func(call nestedCall, _ io.Writer) int { return 0 })
			program.promptInputTerminal = func() bool { return scenario.terminal }
			edit := func() {
				writeConfigCommandFile(t, filepath.Join(program.env["SUBYARD_CONFIG_HOME"], "yards/named/config.env"), "SSH_PORT=2223\nDEV_USER=edited\nDEV_UID=1003\n")
			}
			if scenario.name != "unchanged" && !scenario.editDuringInit {
				edit()
			}
			original := program.nestedCommand
			program.nestedCommand = func(ctx context.Context, args []string, diagnostics io.Writer) int {
				if scenario.editDuringInit && args[1] == "default" {
					edit()
				}
				return original(ctx, args, diagnostics)
			}
			code := program.runPostRepairInit(context.Background(), plan, scenario.explicit)
			if code != scenario.wantCode {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			wantCalls := 2
			if scenario.wantCode != 0 {
				wantCalls = 1
			}
			if len(*calls) != wantCalls {
				t.Fatalf("calls=%v", *calls)
			}
			if !slices.Contains((*calls)[0].arguments, "--yes") {
				t.Fatal("unchanged yard lost its original consent")
			}
			if wantCalls == 2 && slices.Contains((*calls)[1].arguments, "--yes") != scenario.wantYes {
				t.Fatalf("changed yard consent=%v", (*calls)[1].arguments)
			}
			if scenario.name != "unchanged" && !strings.Contains(stdout.String(), "DEV_USER  dev -> edited") {
				t.Fatalf("new listing not printed: %s", stdout)
			}
			if scenario.wantCode != 0 && (!strings.Contains(stderr.String(), "yard -Y named init") || !strings.Contains(stderr.String(), "is ready")) {
				t.Fatal(stderr.String())
			}
		})
	}
}

func TestPendingInitDoesNotDetectResourceSettings(t *testing.T) {
	fixture := newPendingInitFixture(t, false, map[string]string{"default": "LIMITS_MEMORY=4GiB\n"})
	plan, err := fixture.cli.inspectPendingInit(context.Background(), fixture.cli.env["SUBYARD_CONFIG_HOME"])
	if err != nil || len(plan.pendingYards()) != 0 || len(plan.steps(nil)) != 0 {
		t.Fatalf("resource-only change scheduled init: %#v %v", plan, err)
	}
	listing := strings.Join(plan.lines(), "\n")
	if !strings.Contains(listing, "integration settings current") || strings.Contains(listing, "installed configuration current") {
		t.Fatalf("listing overstates its scope: %s", listing)
	}
}

type interruptedPendingInitYard struct{ *updateFollowupYard }

func (*interruptedPendingInitYard) Reconcile(context.Context, releasetransition.ReleaseLinks) error {
	return errors.New("fixture activation interruption")
}

// Reuse the native owner process from the update follow-up fixture. The actual
// transition engine publishes an interrupted repair journal; a fresh CLI must
// resume it and run the disclosed inits after completion.
func TestResumedMigrateRunsPendingInitAfterRepair(t *testing.T) {
	fixture := newPendingInitFixture(t, false, map[string]string{"default": pendingRename})
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fixture.cli.options.Environment = append(fixture.cli.options.Environment,
		"UPDATE_FOLLOWUP_BINARY="+binary, "UPDATE_FOLLOWUP_REPOSITORY="+fixture.cli.options.RepositoryRoot,
		"UPDATE_FOLLOWUP_SCENARIO=automation")
	engine := []byte(`#!/bin/sh
set -eu
if [ "$1" = --version ]; then printf 'yard-engine 1.2.3\n'; exit; fi
[ "$1" = _release-transition ] || exit 64
UPDATE_FOLLOWUP_HELPER=1 exec "$UPDATE_FOLLOWUP_BINARY" -test.run='^TestUpdateCompletesFreshActivationRepairInOneCommand$' --
`)
	writePendingRepairEngine(t, &fixture, engine)
	request, _ := repairRequest(t, fixture)
	request.SchemaVersion, request.Mode = releasetransition.ProcessProtocolSchemaV1, releasetransition.ProcessInspect
	request.RuntimeRoot, request.Yard = fixture.cli.env["YARD_RUNTIME_ROOT"], "default"
	request.InheritedSettingIDs = fixture.cli.releaseTransitionInheritedSettingIDs()
	actors := []releasetransition.V2ActivationReconciler{
		&interruptedPendingInitYard{&updateFollowupYard{home: request.ConfigHome, name: "default"}},
		&updateFollowupYard{home: request.ConfigHome, name: "secondary"},
	}
	invoke := func() releasetransition.ProcessResponse {
		t.Helper()
		response, err := executeReleaseTransitionRequest(context.Background(), fixture.cli.options.RepositoryRoot, request,
			func(_ releasetransition.PlanToken, grant releasetransition.Authorization) bool { return grant != "" },
			actors, releaseTransitionOwnerFixture{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	inspection := invoke()
	if inspection.Inspection == nil || !inspection.Inspection.Assessment.Changed || inspection.Inspection.Resume != nil {
		t.Fatalf("fresh repair inspection: %#v", inspection)
	}
	goal := releasetransition.Goal{Target: request.Target, Direction: request.Direction}
	if err := releasetransition.ValidateProcessInspection(goal, *inspection.Inspection); err != nil {
		t.Fatal(err)
	}
	request.Mode = releasetransition.ProcessConverge
	request.Execution = &releasetransition.Execution{Plan: inspection.Inspection.Plan, Authorization: "fixture-confirmed-repair"}
	interrupted := invoke()
	if interrupted.Outcome == nil || interrupted.Outcome.Status != releasetransition.StatusRecovering {
		t.Fatalf("repair did not interrupt: %#v", interrupted)
	}
	_, journal := repairRequest(t, fixture)
	if journal.Checkpoint == releasetransition.JournalComplete || journal.Releases.From != journal.Goal.Target {
		t.Fatalf("not an interrupted in-place repair: %#v", journal)
	}

	t.Run("owner-protocol", func(t *testing.T) {
		request.Mode, request.Execution = releasetransition.ProcessInspect, nil
		payload, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		owner := exec.CommandContext(context.Background(), filepath.Join(request.RuntimeRoot, "releases", string(request.Target), "bin/yard-engine"), "_release-transition")
		owner.Env, owner.Stdin = fixture.cli.options.Environment, bytes.NewReader(payload)
		output, err := owner.CombinedOutput()
		if err != nil {
			t.Fatalf("native owner: %v: %s", err, output)
		}
		var resumed releasetransition.ProcessResponse
		if err := json.Unmarshal(output, &resumed); err != nil {
			t.Fatal(err)
		}
		if resumed.Inspection == nil || resumed.Inspection.Resume == nil || *resumed.Inspection.Resume != journal.Transaction || resumed.Inspection.Plan != journal.ResumePlan {
			t.Fatalf("owner did not inspect its actual interrupted journal: %#v", resumed)
		}
		if err := releasetransition.ValidateProcessInspection(goal, *resumed.Inspection); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("migrate", func(t *testing.T) {
		second, calls, stdout, stderr := fixture.program(t, []string{"migrate", "--yes"}, nil, func(call nestedCall, _ io.Writer) int {
			_, current := repairRequest(t, fixture)
			if current.Checkpoint != releasetransition.JournalComplete {
				t.Fatal("init ran before resumed repair completed")
			}
			return 0
		})
		if code := second.Run(context.Background()); code != 0 {
			t.Fatalf("resumed migrate code=%d stdout=%s stderr=%s", code, stdout, stderr)
		}
		if len(*calls) != 1 || !slices.Equal((*calls)[0].arguments, []string{"-Y", "default", "init", "--yes"}) {
			t.Fatalf("resumed migrate skipped pending init: %v", *calls)
		}
		if !strings.Contains(stdout.String(), "DEV_USER  dev -> other") || !strings.Contains(stdout.String(), "release.init.default") {
			t.Fatalf("resumed migrate omitted the listing/steps: %s", stdout)
		}
	})
}

func TestPendingInitAndConfigStatusShareContentEvidence(t *testing.T) {
	fixture := newPendingInitFixture(t, false, nil)
	path := filepath.Join(testkit.TempDir(t), "provision.sh")
	testkit.WriteFile(t, path, []byte("old source"), 0600)
	loaded, err := fixture.cli.resolveReleaseTransitionContext(context.Background(), "default", fixture.cli.env["SUBYARD_CONFIG_HOME"])
	if err != nil {
		t.Fatal(err)
	}
	loaded.Environment["AGENT_codex_PROVISION"] = path
	key := bytes.Repeat([]byte{9}, 32)
	executor := fixture.cli.options.Executor.(installedSettingsExecutor)
	executor.settings = loaded.Catalog.IntegrationInitSettings(loaded.Environment)
	executor.diagnostics = &reconcileruntime.IntegrationDiagnosticEvidence{Key: hex.EncodeToString(key), Digests: loaded.Catalog.IntegrationDiagnosticDigests(loaded.Environment, key, nil)}
	evidence := executor.diagnostics
	testkit.WriteFile(t, path, []byte("new source"), 0600)
	for _, selected := range []bool{true, false} {
		current, err := config.WithIntegrationSelection(loaded, nil)
		if selected {
			current, err = config.WithIntegrationSelection(loaded, []string{"codex"})
		}
		if err != nil {
			t.Fatal(err)
		}
		executor.selected = current.Integrations.Effective
		executor.settings = current.Catalog.IntegrationInitSettings(current.Environment)
		fixture.cli.options.Executor = executor
		target := configTarget{Name: "default", Loaded: current}
		pending, err := fixture.cli.inspectPendingInitTarget(context.Background(), target)
		if err != nil {
			t.Fatal(err)
		}
		status, err := fixture.cli.configInstalledDiagnostic(context.Background(), target, "running")
		if err != nil {
			t.Fatal(err)
		}
		expected := "AGENT_codex_PROVISION  content changed"
		if pending.Pending != selected || strings.Contains(strings.Join((pendingInitPlan{Yards: []pendingInitYard{pending}}).lines(), "\n"), expected) != selected || strings.Contains(domain.FormatConfigurationDiagnostics([]domain.YardConfigurationDiagnostic{status}), expected) != selected {
			t.Fatalf("selected=%v pending=%#v status=%#v", selected, pending, status)
		}
		executor.diagnostics = nil
		fixture.cli.options.Executor = executor
		unknown, err := fixture.cli.inspectPendingInitTarget(context.Background(), target)
		if err != nil || unknown.Pending {
			t.Fatalf("unknown evidence scheduled init: %#v %v", unknown, err)
		}
		executor.diagnostics = evidence
	}
}

func TestPendingInitAndConfigStatusFindRemovedDigestOnlyConsumer(t *testing.T) {
	fixture := newPendingInitFixture(t, false, nil)
	loaded, err := fixture.cli.resolveReleaseTransitionContext(context.Background(), "default", fixture.cli.env["SUBYARD_CONFIG_HOME"])
	if err != nil {
		t.Fatal(err)
	}
	const name = "AGENT_codex_PROVISION"
	// Sensitive inputs likewise have an installed digest but no applied value.
	// This catalog-owned file tests that consumer selection does not depend on
	// the desired environment still containing the name.
	delete(loaded.Environment, name)
	for _, selected := range []bool{true, false} {
		current, err := config.WithIntegrationSelection(loaded, nil)
		if selected {
			current, err = config.WithIntegrationSelection(loaded, []string{"codex"})
		}
		if err != nil {
			t.Fatal(err)
		}
		executor := fixture.cli.options.Executor.(installedSettingsExecutor)
		executor.selected = current.Integrations.Effective
		executor.settings = current.Catalog.IntegrationInitSettings(current.Environment)
		executor.diagnostics = &reconcileruntime.IntegrationDiagnosticEvidence{
			Key: strings.Repeat("a", 64), Digests: map[string]string{name: strings.Repeat("b", 64)},
		}
		fixture.cli.options.Executor = executor
		target := configTarget{Name: "default", Loaded: current}
		pending, err := fixture.cli.inspectPendingInitTarget(context.Background(), target)
		if err != nil {
			t.Fatal(err)
		}
		status, err := fixture.cli.configInstalledDiagnostic(context.Background(), target, "running")
		if err != nil {
			t.Fatal(err)
		}
		expected := name + "  content changed"
		if pending.Pending != selected || strings.Contains(strings.Join((pendingInitPlan{Yards: []pendingInitYard{pending}}).lines(), "\n"), expected) != selected || strings.Contains(domain.FormatConfigurationDiagnostics([]domain.YardConfigurationDiagnostic{status}), expected) != selected {
			t.Fatalf("selected=%v pending=%#v status=%#v", selected, pending, status)
		}
	}
}
