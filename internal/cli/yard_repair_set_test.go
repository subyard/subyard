package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

// recordingAdapter answers every adapter request ok and records it as an effect.
type recordingAdapter struct {
	mu       sync.Mutex
	requests []domain.AdapterRequest
	apply    func(domain.AdapterRequest)
}

func (adapter *recordingAdapter) Run(_ context.Context, request domain.AdapterRequest, _ io.Reader) (domain.AdapterResult, string, error) {
	adapter.mu.Lock()
	adapter.requests = append(adapter.requests, request)
	apply := adapter.apply
	adapter.mu.Unlock()
	if apply != nil {
		apply(request)
	}
	return domain.AdapterResult{Schema: 1, OperationID: request.OperationID, Status: "ok"}, "", nil
}

func (adapter *recordingAdapter) count() int {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return len(adapter.requests)
}

// effectPlatform is the init platform fake whose applied stages are effects.
type effectPlatform struct {
	*initPlatformFixture
	mu       sync.Mutex
	applies  int
	services int
	onApply  func()
}

func (platform *effectPlatform) ApplyStage(ctx context.Context, stage ports.ReconcileStageID) error {
	platform.mu.Lock()
	platform.applies++
	onApply := platform.onApply
	platform.mu.Unlock()
	if onApply != nil {
		onApply()
	}
	return platform.initPlatformFixture.ApplyStage(ctx, stage)
}

func (platform *effectPlatform) ProfileServiceConverged(context.Context, string) (bool, error) {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	return platform.services != 0, nil
}

func (platform *effectPlatform) ApplyProfileService(context.Context, string, []ports.TeardownArtifact) error {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	platform.applies++
	platform.services++
	return nil
}

// repairHarness runs commands of a degraded scenario with host fakes that
// record every effect: adapters, init stages and Incus updates.
type repairHarness struct {
	scenario yardFailureScenario
	inspect  string
	// ready is the readiness inspection of the release with every yard repaired.
	ready    []byte
	adapter  *recordingAdapter
	platform *effectPlatform
	// configs are the yards' desired configuration files before any run.
	configs map[string]string
	// sourceDrift changes the bound installed source, as another runtime would.
	sourceDrift string
	// admittedSignal, when set, is told when a repair admission binds its source.
	admittedSignal func()
}

func newRepairHarness(t *testing.T) *repairHarness {
	t.Helper()
	return newRepairHarnessWith(t, func(scenario yardFailureScenario, _ string) {
		observeFailing(t, scenario, "named", true)
	})
}

// newRepairHarnessWith converges a declared runtime in default and named,
// lets degrade record what release history has, the ready inspection first,
// and records the readiness the commands then see.
func newRepairHarnessWith(t *testing.T, degrade func(yardFailureScenario, string)) *repairHarness {
	t.Helper()
	scenario := newYardFailureScenario(t, true, true, "default", "named")
	inspect := scenario.replayReleaseEngine(t)
	if outcome := scenario.converge(t); outcome.Status != releasetransition.StatusReady {
		t.Fatalf("baseline: %#v", outcome)
	}
	incus := scenario.fixture.cli.options.Incus.(*testkit.Incus)
	for _, yard := range []string{"default", "named"} {
		for key, instance := range incus.Instances {
			if instance.Name == scenario.fixture.instanceName(t, yard) {
				instance.Config = map[string]string{"user.subyard.managed": "true", "user.subyard.name": yard, "user.subyard.initialized": "true",
					"user.subyard.disk_tmp": "v1", "user.subyard.desired_power": "running"}
				incus.Instances[key] = instance
			}
		}
	}
	recordReadiness(t, scenario, inspect)
	ready := mustReadConfigFixture(t, inspect)
	degrade(scenario, inspect)
	recordReadiness(t, scenario, inspect)
	root := scenario.fixture.cli.options.RepositoryRoot
	registry := filepath.Join(root, "config", "commands.registry")
	extra := ""
	for _, line := range strings.Split(string(mustReadConfigFixture(t, filepath.Join(repositoryRoot(t), "config", "commands.registry"))), "\n") {
		if strings.HasPrefix(line, "integration|") {
			extra += line + "\n"
		}
	}
	testkit.WriteFile(t, registry, append(mustReadConfigFixture(t, registry), extra...), 0o600)
	// A second profile owns a scoped service, so it can be disabled per yard.
	if err := os.MkdirAll(filepath.Join(root, "config/profiles/owned"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/profiles/owned/owner.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
	testkit.WriteFile(t, filepath.Join(root, "config/profiles/owned/profile.json"), []byte(`{"schema_version":1,"owner_service":"owner.sh"}`), 0o600)
	named := filepath.Join(scenario.request.ConfigHome, "yards", "named", "config.env")
	testkit.WriteFile(t, named, []byte(strings.Replace(string(mustReadConfigFixture(t, named)), "ENVIRONMENT_PROFILES=sample\n", "ENVIRONMENT_PROFILES=sample owned\n", 1)), 0o600)
	harness := &repairHarness{scenario: scenario, inspect: inspect, ready: ready, adapter: &recordingAdapter{},
		platform: &effectPlatform{initPlatformFixture: newInitPlatformFixture()}}
	harness.adapter.apply = func(request domain.AdapterRequest) {
		// The physical lifecycle and teardown reach the state they were asked for.
		for key, instance := range incus.Instances {
			if instance.Name != request.Context["YARD_INSTANCE_NAME"] {
				continue
			}
			switch {
			case request.Adapter == "lifecycle" && request.Action == "stop":
				instance.Status = "Stopped"
				incus.Instances[key] = instance
			case request.Adapter == "lifecycle" && request.Action == "start":
				instance.Status = "Running"
				incus.Instances[key] = instance
			case request.Adapter == "teardown" && request.Action == "apply":
				delete(incus.Instances, key)
				incus.Reconcile.InstanceFound = false
			}
		}
	}
	// A repair's effect restores the yard: the release becomes ready again.
	harness.platform.onApply = func() { testkit.WriteFile(t, inspect, ready, 0o600) }
	harness.configs = map[string]string{}
	for _, yard := range []string{"default", "named"} {
		harness.configs[yard] = string(mustReadConfigFixtureOrEmpty(filepath.Join(scenario.request.ConfigHome, "yards", yard, "config.env")))
	}
	return harness
}

func (harness *repairHarness) effects() int {
	incus := harness.scenario.fixture.cli.options.Incus.(*testkit.Incus)
	harness.platform.mu.Lock()
	defer harness.platform.mu.Unlock()
	configs := 0
	for _, yard := range []string{"default", "named"} {
		if path := filepath.Join(harness.scenario.request.ConfigHome, "yards", yard, "config.env"); harness.configs[yard] != string(mustReadConfigFixtureOrEmpty(path)) {
			configs++
		}
	}
	return harness.adapter.count() + harness.platform.applies + len(incus.PowerUpdates) + len(incus.ConfigUpdates) + configs
}

func mustReadConfigFixtureOrEmpty(path string) []byte {
	payload, _ := os.ReadFile(path)
	return payload
}

func (harness *repairHarness) run(t *testing.T, during func(), arguments ...string) (int, string) {
	t.Helper()
	return harness.runWith(t, nil, during, arguments...)
}

func (harness *repairHarness) runWith(t *testing.T, environment []string, during func(), arguments ...string) (int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	options := harness.scenario.fixture.cli.options
	options.Arguments, options.Stdout, options.Stderr = arguments, &stdout, &stderr
	options.Environment = append(slices.Clone(options.Environment), environment...)
	options.Prompt = confirmingPrompt{during: during}
	options.AdapterRunner = harness.adapter
	options.InitPlatform = harness.platform
	options.NetworkPolicy = allowTestNetworkPolicy()
	integrations := &integrationRuntimeFixture{plan: reconcileruntime.IntegrationPlan{Fingerprint: strings.Repeat("c", 64), Scope: strings.Repeat("c", 64)}}
	options.IntegrationRuntime = func(config.Loaded) IntegrationRuntime { return integrations }
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	program.nestedCommand = func(context.Context, []string, io.Writer) int { return 0 }
	program.sourceBinding = harness.bindSources
	code := program.Run(context.Background())
	return code, stdout.String() + stderr.String()
}

// bindSources stands in for the installed-source proof, which a fixture CLI
// outside runtime/releases cannot give; TestRepairNeedsTheInstalledReleaseTooling
// runs the real one.
func (harness *repairHarness) bindSources(context.Context, releasetransition.JournalRecord) (string, string, error) {
	if signal := harness.admittedSignal; signal != nil {
		signal() // the admission binds the source as its last step
	}
	harness.platform.mu.Lock()
	defer harness.platform.mu.Unlock()
	return "installed-source" + harness.sourceDrift, "installed-tooling", nil
}

// flippedJournal is the journal of another transaction, as a migrate in
// another terminal would write it.
func (harness *repairHarness) flippedJournal(t *testing.T) []byte {
	t.Helper()
	journal, err := releasetransition.ParseJournal(mustReadConfigFixture(t, harness.scenario.fixture.journalPath))
	if err != nil {
		t.Fatal(err)
	}
	journal.Transaction = "tx-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	payload, err := releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// installRelay gives the sample profile a resource whose declared shutdown has
// a prepared (v2) plan; its apply leaves a marker as its effect.
func (harness *repairHarness) installRelay(t *testing.T) string {
	t.Helper()
	root := harness.scenario.fixture.cli.options.RepositoryRoot
	directory := filepath.Join(root, "config/profiles/sample/resources/relay")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/profiles/sample/resources/relay.res"), []byte(strings.Join([]string{
		"COMMAND=relay", "HANDLER=resources/relay/handler.sh", "TITLE=Sample relay",
		`ACTION="up up host-change reversible"`, `ACTION="down down host-change reversible"`,
		`ACTION="purge purge persistent-data-destruction irreversible"`,
		"BRINGUP=up", "SHUTDOWN=down",
	}, "\n")+"\n"), 0o600)
	marker := filepath.Join(root, "relay-down")
	testkit.WriteFile(t, filepath.Join(directory, "handler.sh"), []byte(`#!/usr/bin/env python3
import json, os, pathlib, sys
marker = pathlib.Path('`+marker+`')
mode = os.environ.get('SUBYARD_RESOURCE_MODE')
verb = sys.argv[1]
if mode == 'apply':
    marker.write_text(verb)
    sys.exit(0)
done = marker.exists()
step = dict(id='relay', target='fixture relay', observed='stopped' if done else 'running', desired='stopped',
            decision='skip' if done else 'apply', verify='read fixture relay', consequence='stop fixture relay')
print(json.dumps(dict(schema='yard.resource-action-assessment.v2', action=verb, changed=not done,
    consequences=[] if done else ['stop fixture relay'], steps=[step], binding='a' * 64)))
`), 0o700)
	return marker
}

// installIncusCLI puts a fake incus command first on PATH: it reports the yard
// running and records each exec as an effect.
func (harness *repairHarness) installIncusCLI(t *testing.T) (string, []string) {
	t.Helper()
	directory := testkit.TempDir(t)
	log := filepath.Join(directory, "incus.log")
	instance := harness.scenario.fixture.instanceName(t, "named")
	testkit.WriteFile(t, filepath.Join(directory, "incus"), []byte("#!/bin/sh\ncase \"$1\" in\nlist) printf '%s,RUNNING\\n' '"+instance+"' ;;\nexec) printf '%s\\n' \"$*\" >> '"+log+"' ;;\nesac\n"), 0o700)
	// The shell resolves incus on this process's PATH.
	t.Setenv("PATH", directory+":"+os.Getenv("PATH"))
	return log, []string{"PATH=" + os.Getenv("PATH")}
}

type repairRow struct {
	arguments []string
	// underBarrier flips the journal from a writer holding the owner barrier
	// while the admitted command waits for it, for a command without a prompt.
	underBarrier bool
	// prepare adapts the harness and returns extra effects and environment.
	prepare func(*testing.T, *repairHarness) (func() int, []string)
}

// The repair set is data: every member has a row. Each row's positive run
// exits 0 and reaches its effect on a yard that admits only repair; with
// consent, a journal flipped during the confirmation refuses it as plan-stale
// with no effect at all.
func TestRepairSetTable(t *testing.T) {
	rows := map[repairMember]repairRow{
		memberInit:      {arguments: []string{"-Y", "named", "init"}},
		memberInitReset: {arguments: []string{"-Y", "named", "init", "--reset"}},
		memberTeardown: {arguments: []string{"-Y", "named", "teardown"}, prepare: func(_ *testing.T, harness *repairHarness) (func() int, []string) {
			harness.scenario.fixture.cli.options.Incus.(*testkit.Incus).Reconcile.InstanceFound = true
			return nil, nil
		}},
		memberStart: {arguments: []string{"-Y", "named", "start"}, underBarrier: true, prepare: func(t *testing.T, harness *repairHarness) (func() int, []string) {
			harness.scenario.fixture.setPower(t, "named", "Stopped")
			return nil, nil
		}},
		memberStop: {arguments: []string{"-Y", "named", "stop"}},
		memberShell: {arguments: []string{"-Y", "named", "shell", "--", "true"}, prepare: func(t *testing.T, harness *repairHarness) (func() int, []string) {
			log, environment := harness.installIncusCLI(t)
			return func() int { return len(mustReadConfigFixtureOrEmpty(log)) }, environment
		}},
		memberConfig:             {arguments: []string{"-Y", "named", "config", "set", "--scope", "yard", "--local", "SSH_PORT", "2230"}},
		memberProfileDisable:     {arguments: []string{"-Y", "named", "profile", "disable", "owned"}},
		memberIntegrationDisable: {arguments: []string{"-Y", "named", "integration", "disable", "codex"}},
		memberResourceShutdown: {arguments: []string{"-Y", "named", "relay", "down"}, prepare: func(t *testing.T, harness *repairHarness) (func() int, []string) {
			marker := harness.installRelay(t)
			return func() int { return len(mustReadConfigFixtureOrEmpty(marker)) }, nil
		}},
	}
	for _, member := range repairMembers {
		if _, ok := rows[member]; !ok {
			t.Errorf("repair set member %q has no row", member)
		}
	}
	for _, member := range repairMembers {
		row := rows[member]
		setup := func(t *testing.T) (*repairHarness, func() int, []string) {
			harness := newRepairHarness(t)
			var extra func() int
			var environment []string
			if row.prepare != nil {
				extra, environment = row.prepare(t, harness)
			}
			effects := func() int {
				count := harness.effects()
				if extra != nil {
					count += extra()
				}
				return count
			}
			return harness, effects, environment
		}
		t.Run(member.String()+"/positive", func(t *testing.T) {
			harness, effects, environment := setup(t)
			before := effects()
			code, output := harness.runWith(t, environment, func() {}, row.arguments...)
			if code != 0 || effects() == before || strings.Contains(output, "release gate:") {
				t.Fatalf("repair did not reach its effect: code=%d effects %d->%d\n%s", code, before, effects(), output[max(0, len(output)-800):])
			}
		})
		// Shell has no prepared execution: its admission is its only check.
		if member == memberShell {
			continue
		}
		t.Run(member.String()+"/flipped", func(t *testing.T) {
			harness, effects, environment := setup(t)
			flipped := harness.flippedJournal(t)
			before := effects()
			write := func() { testkit.WriteFile(t, harness.scenario.fixture.journalPath, flipped, 0o600) }
			var code int
			var output string
			if row.underBarrier {
				code, output = harness.runWhileWriterHoldsBarrier(t, environment, write, row.arguments...)
			} else {
				code, output = harness.runWith(t, environment, write, row.arguments...)
			}
			if code == 0 || !strings.Contains(output, "the release changed after confirmation") || effects() != before {
				t.Fatalf("flipped history: code=%d effects %d->%d\n%s", code, before, effects(), output)
			}
		})
	}
}

// A non-repair command and an unsafe declared verb stay refused on a yard that
// needs repair, before any effect.
func TestRepairOnlyYardRefusesOtherCommands(t *testing.T) {
	harness := newRepairHarness(t)
	harness.installRelay(t)
	for _, arguments := range [][]string{
		{"-Y", "named", "provision"},
		{"-Y", "named", "init", "--configs"},
		{"-Y", "named", "integration", "enable", "codex"},
		{"-Y", "named", "relay", "up"},
		{"-Y", "named", "relay", "purge"},
	} {
		before := harness.effects()
		code, output := harness.run(t, func() { t.Errorf("%v reached consent", arguments) }, arguments...)
		if code == 0 || !strings.Contains(output, "release gate: yard named needs repair") || harness.effects() != before {
			t.Fatalf("%v on a repair-only yard: code=%d\n%s", arguments, code, output)
		}
	}
}

// A host cause that appears while the operator confirms is a fresh decision
// that no longer admits the repair: it is refused with no effect.
func TestRepairRedecidesWhenAHostCauseAppearsDuringConsent(t *testing.T) {
	harness := newRepairHarness(t)
	var response releasetransition.ProcessResponse
	if err := json.Unmarshal(mustReadConfigFixture(t, harness.inspect), &response); err != nil {
		t.Fatal(err)
	}
	inspection := *response.Inspection
	inspection.Blockers = append(slices.Clone(inspection.Blockers), releasetransition.Blocker{Code: releasetransition.CodeDependencyUnavailable,
		Resource: "activation.host-power", Message: "host power reconciler failed", Retry: "run yard migrate"})
	response.Inspection = &inspection
	hostCause, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"-Y", "named", "stop"}, {"-Y", "named", "init"}} {
		before := harness.effects()
		code, output := harness.run(t, func() { testkit.WriteFile(t, harness.inspect, hostCause, 0o600) }, arguments...)
		if code == 0 || !strings.Contains(output, "the release changed after confirmation") || harness.effects() != before {
			t.Fatalf("%v over a new host cause: code=%d effects %d->%d\n%s", arguments, code, before, harness.effects(), output)
		}
		recordReadiness(t, harness.scenario, harness.inspect)
	}
}

// A journal writer holds the owner barrier. If it acquires it after admission
// and before Execute, the repair waits and refuses the unfinished journal;
// if the repair holds it first, its effect completes before the writer can
// write.
func TestRepairAndConcurrentJournalWriterInBothOrders(t *testing.T) {
	for _, order := range []string{"writer-first", "repair-first"} {
		t.Run(order, func(t *testing.T) {
			harness := newRepairHarness(t)
			home := harness.scenario.fixture.cli.env["SUBYARD_HOME"]
			flipped := harness.flippedJournal(t)
			var mu sync.Mutex
			var events []string
			record := func(event string) { mu.Lock(); events = append(events, event); mu.Unlock() }
			physical := harness.adapter.apply
			harness.adapter.apply = func(request domain.AdapterRequest) { physical(request); record("effect") }
			writer := func(held chan<- struct{}, release <-chan struct{}) {
				_, unlock, err := uninstall.OwnerLock(context.Background(), home, nil)
				if err != nil {
					t.Error(err)
					close(held)
					return
				}
				close(held)
				<-release
				testkit.WriteFile(t, harness.scenario.fixture.journalPath, flipped, 0o600)
				record("journal")
				unlock()
			}
			held, release := make(chan struct{}), make(chan struct{})
			done := make(chan struct{})
			var code int
			var output string
			if order == "writer-first" {
				confirmed := make(chan struct{})
				go func() {
					defer close(done)
					code, output = harness.run(t, func() {
						go writer(held, release)
						<-held
						close(confirmed)
					}, "-Y", "named", "stop")
				}()
				// After consent the stop waits for the barrier in Execute; the
				// writer then leaves another journal and releases it.
				select {
				case <-confirmed:
				case <-done:
					t.Fatalf("repair did not reach confirmation: code=%d output=%s", code, output)
				case <-time.After(60 * time.Second):
					t.Fatal("repair did not reach confirmation")
				}
				time.Sleep(200 * time.Millisecond)
				close(release)
			} else {
				// The writer starts while the repair holds the barrier for its
				// effect, and writes as soon as it holds the barrier itself.
				var once sync.Once
				effect := harness.adapter.apply
				harness.adapter.apply = func(request domain.AdapterRequest) {
					once.Do(func() { go writer(held, release); close(release) })
					effect(request)
				}
				go func() {
					defer close(done)
					code, output = harness.run(t, func() {}, "-Y", "named", "stop")
				}()
			}
			select {
			case <-done:
			case <-time.After(60 * time.Second):
				t.Fatal("repair and journal writer deadlocked")
			}
			<-held
			deadline := time.After(30 * time.Second)
			for {
				mu.Lock()
				written := slices.Contains(events, "journal")
				mu.Unlock()
				if written {
					break
				}
				select {
				case <-deadline:
					t.Fatal("journal writer never ran")
				case <-time.After(10 * time.Millisecond):
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if order == "writer-first" {
				if code == 0 || !strings.Contains(output, "the release changed after confirmation") || slices.Contains(events, "effect") {
					t.Fatalf("repair over a journal written while it waited: code=%d events=%v\n%s", code, events, output)
				}
				return
			}
			if code != 0 || len(events) < 2 || events[0] != "effect" || events[len(events)-1] != "journal" {
				t.Fatalf("repair holding the barrier: code=%d events=%v\n%s", code, events, output[max(0, len(output)-600):])
			}
		})
	}
}

// materializingExecutor answers materialized-file reads: drift until applied
// reports that the yard's refresh ran, then the desired content.
type materializingExecutor struct {
	ports.InstanceExecutor
	desired map[string]string // instance/destination -> desired sha256
	applied func(instance string) bool
}

func (executor materializingExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(request.Command) == 3 && request.Command[0] == "sha256sum" {
		digest, known := executor.desired[instance+"/"+request.Command[2]]
		if !known || !executor.applied(instance) {
			digest = strings.Repeat("0", 64)
		}
		return ports.InstanceExecResult{Stdout: []byte(digest + "  " + request.Command[2] + "\n")}, nil
	}
	return executor.InstanceExecutor.Exec(ctx, project, instance, request)
}

// Config apply runs the production appliers: an open yard beside one that
// needs repair applies through the ordinary child, which inherits the owner
// barrier; the yard that needs repair applies in this process, through its
// repair admission, without a child.
func TestConfigApplyWithTheProductionApplier(t *testing.T) {
	for _, yard := range []string{"default", "named"} {
		t.Run(yard, func(t *testing.T) {
			harness := newRepairHarness(t)
			fixture := harness.scenario.fixture
			root := testkit.TempDir(t)
			marker := filepath.Join(root, "child")
			testkit.WriteFile(t, filepath.Join(root, "yard"), []byte("#!/bin/sh\nprintf 'fd=%s args=%s\\n' \"${SUBYARD_OWNER_LOCK_FD:-}\" \"$*\" >> '"+marker+"'\n"), 0o700)
			fixture.cli.options.Config = nil
			fixture.cli.options.DispatcherPath = filepath.Join(root, "yard")
			loaded, err := fixture.cli.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			targets, err := fixture.cli.localConfigTargets(loaded, true)
			if err != nil {
				t.Fatal(err)
			}
			desired := map[string]string{}
			for _, target := range targets {
				materialization, _, _, err := materializationDesired(target, true, target.Loaded.ProspectiveFiles.ReadAsset)
				if err != nil {
					t.Fatal(err)
				}
				for _, asset := range materialization.Assets {
					desired[target.Loaded.Context.YardInstanceName+"/"+asset.Destination] = asset.DesiredHash
				}
			}
			instance := fixture.instanceName(t, yard)
			fixture.cli.options.Executor = materializingExecutor{InstanceExecutor: fixture.cli.options.Executor, desired: desired,
				applied: func(name string) bool {
					harness.platform.mu.Lock()
					defer harness.platform.mu.Unlock()
					return name == instance && (len(mustReadConfigFixtureOrEmpty(marker)) != 0 || harness.platform.configs != 0)
				}}
			code, output := harness.run(t, func() {}, "-Y", yard, "config", "apply")
			child := string(mustReadConfigFixtureOrEmpty(marker))
			if code != 0 {
				t.Fatalf("config apply: code=%d child=%q\n%s", code, child, output)
			}
			switch yard {
			case "default":
				if fd, args, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(child), "fd="), " args="); fd == "" || args != "init --configs --yes" || harness.platform.configs != 0 {
					t.Fatalf("open yard did not apply through the child with the barrier: child=%q in-process=%d", child, harness.platform.configs)
				}
			case "named":
				if child != "" || harness.platform.configs != 1 {
					t.Fatalf("repair-only yard did not apply in process: child=%q in-process=%d", child, harness.platform.configs)
				}
			}
		})
	}
}

// guestCallExecutor records the guest commands of the SSH-agent environment
// and controller-key paths and answers them as a working guest does.
type guestCallExecutor struct {
	ports.InstanceExecutor
	mu    *sync.Mutex
	calls *[]string
}

func (executor guestCallExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(request.Command) >= 3 && request.Command[0] == "sh" && request.Command[1] == "-eu" && (request.Command[2] == "-s" || request.Command[2] == "-c") {
		executor.mu.Lock()
		*executor.calls = append(*executor.calls, instance+" "+strings.Join(request.Command[2:3], " "))
		executor.mu.Unlock()
		return ports.InstanceExecResult{Stdout: []byte("added")}, nil
	}
	return executor.InstanceExecutor.Exec(ctx, project, instance, request)
}

// dedicatedSSHIdentity installs the canonical yard SSH identity and its
// snippet, and returns an encrypted operator key.
func dedicatedSSHIdentity(t *testing.T, fixture pendingInitFixture) string {
	t.Helper()
	loaded, err := fixture.cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(loaded.Context.Paths.DataHome, "ssh")
	if err := os.MkdirAll(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(keys, "id_ed25519")
	if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", canonical).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v %s", err, output)
	}
	if err := os.Chmod(canonical+".pub", 0o644); err != nil {
		t.Fatal(err)
	}
	snippets := filepath.Join(loaded.Context.Paths.OperatorHome, ".ssh")
	if err := os.MkdirAll(snippets, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(snippets, "subyard.config"), []byte("Host yard\n  IdentityFile "+canonical+"\n  IdentitiesOnly yes\n"), 0o600)
	testkit.WriteFile(t, filepath.Join(fixture.cli.options.RepositoryRoot, "scripts", "ssh-agent-environment.sh"),
		mustReadConfigFixture(t, filepath.Join(repositoryRoot(t), "scripts", "ssh-agent-environment.sh")), 0o644)
	operatorKey := filepath.Join(testkit.TempDir(t), "operator")
	if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "fixture passphrase", "-f", operatorKey).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v %s", err, output)
	}
	return operatorKey
}

// Beside a yard that needs repair, a healthy yard keeps its ordinary commands:
// SSH key access, controller key authorization and resource verbs reach their
// guest effects there, while the broken yard refuses them before any guest call.
func TestHealthyYardStaysOpenBesideABrokenOne(t *testing.T) {
	harness := newRepairHarness(t)
	fixture := harness.scenario.fixture
	marker := harness.installRelay(t)
	key := dedicatedSSHIdentity(t, fixture)
	var mu sync.Mutex
	var calls []string
	fixture.cli.options.Executor = guestCallExecutor{InstanceExecutor: fixture.cli.options.Executor, mu: &mu, calls: &calls}
	healthy, broken := fixture.instanceName(t, "default"), fixture.instanceName(t, "named")
	guestCalls := func(instance string) int {
		mu.Lock()
		defer mu.Unlock()
		count := 0
		for _, call := range calls {
			if strings.HasPrefix(call, instance+" ") {
				count++
			}
		}
		return count
	}
	run := func(yard string, stdin string, arguments ...string) (int, string) {
		var stdout, stderr bytes.Buffer
		options := fixture.cli.options
		options.Arguments, options.Stdout, options.Stderr = append([]string{"-Y", yard}, arguments...), &stdout, &stderr
		options.Prompt, options.Stdin = confirmingPrompt{during: func() {}}, strings.NewReader(stdin)
		program, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		program.operatorTerminal = func() bool { return true }
		return program.Run(context.Background()), stdout.String() + stderr.String()
	}
	controller := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA controller\n"

	// SSH key access configures the healthy yard's guest environment, its
	// effect before the grant; the grant itself needs the operator's terminal.
	code, output := run("default", "", "ssh-agent", "unlock", "--key", key, "--ttl", "1h")
	if strings.Contains(output, "release gate:") || guestCalls(healthy) == 0 {
		t.Fatalf("ssh-agent unlock on the healthy yard: code=%d calls=%v\n%s", code, calls, output)
	}
	before := guestCalls(healthy)
	if code, output := run("default", controller, "_authorize"); code != 0 || guestCalls(healthy) != before+1 {
		t.Fatalf("_authorize on the healthy yard: code=%d calls=%v\n%s", code, calls, output)
	}
	if code, output := run("default", "", "relay", "up"); code != 0 || string(mustReadConfigFixtureOrEmpty(marker)) != "up" {
		t.Fatalf("resource verb on the healthy yard: code=%d\n%s", code, output)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	for _, command := range []struct {
		stdin     string
		arguments []string
	}{
		{"", []string{"ssh-agent", "unlock", "--key", key, "--ttl", "1h"}},
		{controller, []string{"_authorize"}},
		{"", []string{"relay", "up"}},
	} {
		code, output := run("named", command.stdin, command.arguments...)
		if code == 0 || !strings.Contains(output, "release gate: yard named needs repair") || guestCalls(broken) != 0 ||
			len(mustReadConfigFixtureOrEmpty(marker)) != 0 {
			t.Fatalf("%v on the broken yard: code=%d calls=%v\n%s", command.arguments, code, calls, output)
		}
	}
}

// networkRun runs one network command on a scenario with a recording policy
// host; ready makes the host network inspectable for isolation and reconcile.
func networkRun(t *testing.T, scenario yardFailureScenario, ready bool, arguments ...string) (int, string, *networkCLIHost) {
	t.Helper()
	return networkRunWith(t, scenario, func(host *networkCLIHost) { host.ready = ready }, arguments...)
}

// networkRunWith lets configure set the host policy before the command.
func networkRunWith(t *testing.T, scenario yardFailureScenario, configure func(*networkCLIHost), arguments ...string) (int, string, *networkCLIHost) {
	t.Helper()
	root := scenario.fixture.cli.options.RepositoryRoot
	registry := filepath.Join(root, "config", "commands.registry")
	if !strings.Contains(string(mustReadConfigFixture(t, registry)), "\nnetwork|") {
		for _, line := range strings.Split(string(mustReadConfigFixture(t, filepath.Join(repositoryRoot(t), "config", "commands.registry"))), "\n") {
			if strings.HasPrefix(line, "network|") {
				testkit.WriteFile(t, registry, append(mustReadConfigFixture(t, registry), line+"\n"...), 0o600)
			}
		}
	}
	policy, _ := yardnetwork.Decode(nil)
	host := &networkCLIHost{policy: policy, allowWrites: true}
	configure(host)
	var stdout, stderr bytes.Buffer
	options := scenario.fixture.cli.options
	options.Arguments, options.Stdout, options.Stderr = arguments, &stdout, &stderr
	options.Prompt = confirmingPrompt{during: func() {}}
	options.NetworkPolicy = &yardnetwork.Service{Host: host, Lock: testNetworkPolicyLock{}}
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return program.Run(context.Background()), stdout.String() + stderr.String(), host
}

// A link acts on both endpoints, whichever is selected: an endpoint with only
// pending configuration keeps the link, one that needs repair refuses it at
// prepare, by name. Isolation and reconcile act on every yard whose policy
// they change, and a yard that needs repair or has pending findings refuses them.
func TestNetworkCommandsAreGatedByTheYardsTheyActOn(t *testing.T) {
	link := func(t *testing.T, scenario yardFailureScenario, arguments ...string) (int, string, *networkCLIHost) {
		t.Helper()
		return networkRun(t, scenario, false, arguments...)
	}
	t.Run("pending endpoint", func(t *testing.T) {
		scenario := newYardFailureScenario(t, true, true, "default", "named")
		inspect := scenario.replayReleaseEngine(t)
		scenario.runtime.failing(t, "named", true)
		requirePendingRuntimeOutcome(t, scenario.converge(t), "named")
		recordReadiness(t, scenario, inspect)
		code, output, host := link(t, scenario, "-Y", "default", "network", "link", "default", "named")
		if code != 0 || host.writes == 0 || len(host.policy.Peers("named")) != 1 {
			t.Fatalf("link to a pending yard: code=%d writes=%d\n%s", code, host.writes, output)
		}
	})
	for _, test := range []struct{ selected, a, b string }{{"default", "default", "named"}, {"default", "named", "default"}, {"named", "named", "default"}, {"named", "default", "named"}} {
		t.Run("repair endpoint "+test.a+"-"+test.b+" from "+test.selected, func(t *testing.T) {
			harness := newRepairHarness(t)
			code, output, host := link(t, harness.scenario, "-Y", test.selected, "network", "link", test.a, test.b)
			if code == 0 || host.writes != 0 || !strings.Contains(output, "yard named needs repair first") ||
				!strings.Contains(output, "next: resolve the protected release source failure with the sealed active release owner") ||
				strings.Contains(output, "yard -Y named init") || strings.Contains(output, "Operation plan") {
				t.Fatalf("link to a yard that needs repair: code=%d writes=%d\n%s", code, host.writes, output)
			}
		})
	}
	// Reconcile with nothing to change acts on no yard.
	if code, output, _ := networkRun(t, newRepairHarness(t).scenario, true, "-Y", "default", "network", "reconcile"); code != 0 {
		t.Fatalf("reconcile without work: code=%d\n%s", code, output)
	}
	for _, arguments := range [][]string{{"isolation", "on"}} {
		t.Run(strings.Join(arguments, " ")+" beside a yard that needs repair", func(t *testing.T) {
			harness := newRepairHarness(t)
			code, output, host := networkRun(t, harness.scenario, true, append([]string{"-Y", "default", "network"}, arguments...)...)
			if code == 0 || host.writes != 0 || !strings.Contains(output, "yard named needs repair first") || strings.Contains(output, "Operation plan") {
				t.Fatalf("%v beside a yard that needs repair: code=%d writes=%d\n%s", arguments, code, host.writes, output)
			}
		})
	}
	t.Run("isolation on beside a pending yard", func(t *testing.T) {
		scenario := newYardFailureScenario(t, true, true, "default", "named")
		inspect := scenario.replayReleaseEngine(t)
		scenario.runtime.failing(t, "named", true)
		requirePendingRuntimeOutcome(t, scenario.converge(t), "named")
		recordReadiness(t, scenario, inspect)
		code, output, host := networkRun(t, scenario, true, "-Y", "default", "network", "isolation", "on")
		if code == 0 || host.writes != 0 || !strings.Contains(output, "yard named has pending configuration") {
			t.Fatalf("isolation beside a pending yard: code=%d writes=%d\n%s", code, host.writes, output)
		}
	})
}

// Removal repairs need the installed release they were admitted on. The
// inspection binds the runtime links to the completed journal: a current or
// previous link changed outside the journal is no clean yard-scoped gate, so
// teardown and resource shutdown are refused host-wide; the recheck repeats
// this inspection under the owner barrier.
func TestChangedRuntimeLinkRefusesRemovalRepairs(t *testing.T) {
	for _, change := range []struct{ link, target string }{
		{"current", "releases/release-b"},
		{"previous", "releases/release-b"},
	} {
		t.Run(change.link, func(t *testing.T) {
			harness := newRepairHarness(t)
			harness.installRelay(t)
			// Another installed release, as an update outside the journal leaves it.
			releases := filepath.Join(harness.scenario.request.RuntimeRoot, "releases")
			if output, err := exec.Command("cp", "-a", filepath.Join(releases, "release-a"), filepath.Join(releases, "release-b")).CombinedOutput(); err != nil {
				t.Fatalf("copy release: %v %s", err, output)
			}
			link := filepath.Join(harness.scenario.request.RuntimeRoot, change.link)
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(change.target, link); err != nil {
				t.Fatal(err)
			}
			readiness := recordReadiness(t, harness.scenario, harness.inspect)
			program, _, _, _ := harness.scenario.fixture.program(t, nil, nil, func(nestedCall, io.Writer) int { return 0 })
			if _, ok := program.yardScopedGate(readiness.Inspection.Outcome, readiness.Inspection); ok {
				t.Fatalf("changed %s link left a yard-scoped gate: %#v", change.link, readiness.Inspection.Outcome)
			}
			harness.scenario.fixture.cli.options.Incus.(*testkit.Incus).Reconcile.InstanceFound = true
			for _, arguments := range [][]string{{"-Y", "named", "teardown"}, {"-Y", "named", "relay", "down"}} {
				before := harness.effects()
				code, output := harness.run(t, func() { t.Errorf("%v reached consent", arguments) }, arguments...)
				if code == 0 || !strings.Contains(output, mutationGateRefusalPrefix+" release ") || harness.effects() != before {
					t.Fatalf("%v over a changed %s link: code=%d\n%s", arguments, change.link, code, output)
				}
			}
		})
	}
}

// Over an unfinished activation of the active release, a lost guest agent in
// one yard leaves its journal resumable: stop and stop --force of that yard
// reach their effect and are rechecked after consent; teardown and other
// lifecycle commands stay refused.
func TestUnfinishedActivationAdmitsBoundedStop(t *testing.T) {
	harness := newRepairHarnessWith(t, func(scenario yardFailureScenario, _ string) {
		// An untyped apply failure leaves the activation unfinished.
		testkit.WriteFile(t, scenario.runtime.state("named"), []byte(strings.Repeat("d", 64)), 0o600)
		scenario.runtime.failing(t, "named", true)
		declaration := filepath.Join(scenario.fixture.cli.options.RepositoryRoot, "config/profiles/sample/profile.json")
		testkit.WriteFile(t, declaration, []byte(strings.Replace(string(mustReadConfigFixture(t, declaration)), `,"failure_scope":"yard"`, "", 1)), 0o600)
		if outcome := scenario.converge(t); outcome.ReachedGoal {
			t.Fatalf("fixture activation did not stop: %#v", outcome)
		}
		observeFailing(t, scenario, "named", true)
		scenario.fixture.cli.options.Executor = lostAgentExecutor{scenario.fixture.cli.options.Executor, scenario.fixture.instanceName(t, "named")}
	})
	var readiness releasetransition.ProcessResponse
	if err := json.Unmarshal(mustReadConfigFixture(t, harness.inspect), &readiness); err != nil {
		t.Fatal(err)
	}
	if journal := harness.scenario.journal(t); journal.Checkpoint != releasetransition.JournalReconciling ||
		readiness.Inspection.Outcome.Status != releasetransition.StatusOperatorActionRequired || readiness.Inspection.Resume == nil {
		t.Fatalf("journal %s, readiness %#v", journal.Checkpoint, readiness.Inspection.Outcome)
	}
	for _, arguments := range [][]string{{"-Y", "named", "stop"}, {"-Y", "named", "stop", "--force"}} {
		harness.scenario.fixture.setPower(t, "named", "Running")
		flipped := harness.flippedJournal(t)
		journal := mustReadConfigFixture(t, harness.scenario.fixture.journalPath)
		before := harness.effects()
		code, output := harness.run(t, func() { testkit.WriteFile(t, harness.scenario.fixture.journalPath, flipped, 0o600) }, arguments...)
		if code == 0 || !strings.Contains(output, "the release changed after confirmation") || harness.effects() != before {
			t.Fatalf("%v over a changed journal: code=%d\n%s", arguments, code, output)
		}
		testkit.WriteFile(t, harness.scenario.fixture.journalPath, journal, 0o600)
		code, output = harness.run(t, func() {}, arguments...)
		if code != 0 || harness.effects() == before {
			t.Fatalf("%v over an unfinished activation: code=%d effects %d->%d\n%s", arguments, code, before, harness.effects(), output)
		}
	}
	for _, arguments := range [][]string{{"-Y", "named", "teardown"}, {"-Y", "named", "init", "--reset"}, {"-Y", "default", "start"}} {
		before := harness.effects()
		code, output := harness.run(t, func() { t.Errorf("%v reached consent", arguments) }, arguments...)
		if code == 0 || !strings.Contains(output, mutationGateRefusalPrefix) || harness.effects() != before {
			t.Fatalf("%v over an unfinished activation: code=%d\n%s", arguments, code, output)
		}
	}
}

// Each yard-scoped owner attributes a failure in yard named, of every kind,
// to named and still reports what it observed in default, as without the
// failure: one yard's failure never hides another yard's finding. For the
// profile runtime the engine carries both into a yard-scoped gate.
func TestObservationMatrixAcrossYardScopedOwners(t *testing.T) {
	observe := func(t *testing.T, owner, kind string) []releasetransition.V2YardAttribution {
		t.Helper()
		scenario := newYardFailureScenario(t, true, true, "default", "named")
		inspect := scenario.replayReleaseEngine(t)
		scenario.converge(t)
		// The profile runtime drifts in default; the other owners see the
		// fixture's own default finding.
		testkit.WriteFile(t, scenario.runtime.state("default"), []byte(strings.Repeat("d", 64)), 0o600)
		switch kind {
		case "observe-error":
			observeFailing(t, scenario, "named", true)
		case "invalid-config":
			path := filepath.Join(scenario.request.ConfigHome, "yards", "named", "config.env")
			testkit.WriteFile(t, path, append(mustReadConfigFixture(t, path), []byte("SSH_PORT=99999\n")...), 0o600)
		case "unreachable-agent":
			observeFailing(t, scenario, "named", true)
			scenario.fixture.cli.options.Executor = lostAgentExecutor{scenario.fixture.cli.options.Executor, scenario.fixture.instanceName(t, "named")}
		}
		program, err := New(scenario.fixture.cli.options)
		if err != nil {
			t.Fatal(err)
		}
		var findings []releasetransition.V2YardAttribution
		for _, reconciler := range program.activationReconcilers(scenario.request) {
			if reconciler.ID() != owner {
				continue
			}
			observation, err := reconciler.Observe(context.Background(), scenario.journal(t).Releases, releasetransition.ReleaseLinks{})
			findings = observation.Yards
			if err != nil {
				attributed, ok := err.(releasetransition.ActivationYardError)
				if !ok || len(attributed.ActivationYardFindings()) == 0 {
					t.Fatalf("%s %s: unattributed failure %v", owner, kind, err)
				}
				findings = attributed.ActivationYardFindings()
			}
		}
		if owner == "sample-runtime" && kind != "none" {
			readiness := recordReadiness(t, scenario, inspect)
			gate, ok := program.yardScopedGate(readiness.Inspection.Outcome, readiness.Inspection)
			if !ok || !gate.state("default").drift || !gate.state("named").repairOnly() {
				t.Fatalf("%s: gate ok=%t yards=%v", kind, ok, gate.yards)
			}
		}
		return findings
	}
	want := map[string]string{"observe-error": releasetransition.YardFindingFailed, "invalid-config": releasetransition.YardFindingConfig,
		"unreachable-agent": releasetransition.YardFindingUnreachable}
	for _, owner := range (&CLI{}).yardScopedOwnersFor([]string{"sample-runtime"}) {
		t.Run(owner, func(t *testing.T) {
			var baseline []releasetransition.V2YardAttribution
			for _, finding := range observe(t, owner, "none") {
				if finding.Yard == "default" {
					baseline = append(baseline, finding)
				}
			}
			// The core guest runtime has no installed guest helpers in this
			// fixture, so it observes nothing in default; it runs the profile
			// runtime owner's Observe, whose row proves the sibling finding.
			if len(baseline) == 0 && owner != "core-guest-runtime" {
				t.Fatalf("%s observes no finding in default, so the matrix proves nothing", owner)
			}
			for _, kind := range []string{"observe-error", "invalid-config", "unreachable-agent"} {
				findings := observe(t, owner, kind)
				for _, finding := range baseline {
					if !slices.Contains(findings, finding) {
						t.Fatalf("%s: a failure in named hid default's %s: %v", kind, finding.Kind, findings)
					}
				}
				named := releasetransition.V2YardAttribution{Yard: "named", Kind: want[kind]}
				// Only the profile handler's observation fails on its own.
				if kind != "observe-error" || owner == "sample-runtime" {
					if !slices.Contains(findings, named) {
						t.Fatalf("%s: named is not %s: %v", kind, named.Kind, findings)
					}
				}
			}
		})
	}
}

// An adapter never runs for a repair admission that has not passed its
// recheck: the orchestrator's guard refuses it.
func TestEffectGuardRefusesAnUncheckedRepair(t *testing.T) {
	harness := newRepairHarness(t)
	program, _, _, _ := harness.scenario.fixture.program(t, nil, nil, func(nestedCall, io.Writer) int { return 0 })
	orchestrator := program.operationPlanner("guard")
	program.gateAdmitted = &gateAdmission{kind: admissionRepair}
	if err := orchestrator.BeforeEffects(context.Background()); err == nil {
		t.Fatal("an unchecked repair admission passed the effect guard")
	}
	program.gateAdmitted.rechecked = true
	if err := orchestrator.BeforeEffects(context.Background()); err != nil {
		t.Fatal(err)
	}
	program.gateAdmitted = &gateAdmission{kind: admissionOpen}
	if err := orchestrator.BeforeEffects(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// RPC admits a repair like the direct CLI. Execute decides again, its
// admission is the one of record and is rechecked under the owner barrier: a
// journal changed between plan and execute refuses it with no effect, and an
// unchanged plan reaches its effect.
func TestRPCRepairIsRecheckedAtExecute(t *testing.T) {
	harness := newRepairHarness(t)
	options := harness.scenario.fixture.cli.options
	options.AdapterRunner, options.InitPlatform, options.NetworkPolicy = harness.adapter, harness.platform, allowTestNetworkPolicy()
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	program.sourceBinding = harness.bindSources
	loaded, err := program.loadContext("named")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
	t.Cleanup(handler.closePlans)
	plan := func(id string) {
		t.Helper()
		if _, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: id,
			Params: json.RawMessage(`{"command":"stop","arguments":[]}`)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	execute := func(id string) error {
		_, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.execute", OperationID: id,
			Params: json.RawMessage(`{"confirmed":true}`)}, func(string, any) (uint64, error) { return 0, nil })
		return err
	}
	journal := mustReadConfigFixture(t, harness.scenario.fixture.journalPath)
	plan("stale")
	testkit.WriteFile(t, harness.scenario.fixture.journalPath, harness.flippedJournal(t), 0o600)
	if err := execute("stale"); err == nil || harness.effects() != 0 {
		t.Fatalf("RPC repair over a changed journal: %v effects=%d", err, harness.effects())
	}
	testkit.WriteFile(t, harness.scenario.fixture.journalPath, journal, 0o600)
	// Another session completes a migration of the same release between plan
	// and execute: the release is ready again with other history. Execute
	// compares the planned repair's history, as the direct CLI does after its
	// prompt, and refuses it.
	plan("migrated")
	testkit.WriteFile(t, harness.scenario.fixture.journalPath, harness.flippedJournal(t), 0o600)
	testkit.WriteFile(t, harness.inspect, harness.ready, 0o600)
	if err := execute("migrated"); err == nil || !strings.Contains(err.Error(), "the release changed after the plan") || harness.effects() != 0 {
		t.Fatalf("RPC repair over a migration completed after its plan: %v effects=%d", err, harness.effects())
	}
	testkit.WriteFile(t, harness.scenario.fixture.journalPath, journal, 0o600)
	recordReadiness(t, harness.scenario, harness.inspect)
	plan("current")
	if err := execute("current"); err != nil || harness.effects() == 0 {
		t.Fatalf("RPC repair: %v effects=%d", err, harness.effects())
	}
}

// Every Git write and every host or shared write stays out of the repair
// set and waits for another yard's pending findings, for set, unset, import
// and edit alike; the refusal on a repair-only yard names the manual repair.
func TestConfigWritesReachingEveryYardOrGit(t *testing.T) {
	for _, test := range []struct {
		arguments   []string
		everyYard   bool
		repairShape bool
	}{
		{[]string{"set", "GIT_USER_NAME", "x", "--scope", "yard", "--git"}, true, false},
		{[]string{"unset", "GIT_USER_NAME", "--scope", "yard", "--git"}, true, false},
		{[]string{"import", "AGENT_codex_RULES", "/tmp/rules", "--scope", "shared", "--local"}, true, true},
		{[]string{"edit", "AGENT_codex_RULES", "--scope", "host", "--local"}, true, true},
		{[]string{"import", "AGENT_codex_RULES", "/tmp/rules", "--scope", "yard", "--git"}, true, false},
		{[]string{"import", "AGENT_codex_RULES", "/tmp/rules", "--scope", "yard", "--local"}, false, true},
		{[]string{"set", "SSH_PORT", "2230", "--scope", "yard", "--local"}, false, true},
	} {
		requests, err := parseConfigAuthoringRequests(test.arguments[0], test.arguments[1:], false)
		if err != nil {
			t.Fatalf("%v: %v", test.arguments, err)
		}
		subject := configAuthoringSubject("default", requests)
		if got := subject.scope == scopeEvery; got != test.everyYard {
			t.Errorf("%v reaches every yard=%t, want %t", test.arguments, got, test.everyYard)
		}
		if got := subject.member == memberConfig && subject.unfit == nil; got != test.repairShape {
			t.Errorf("%v repair member=%t, want %t", test.arguments, got, test.repairShape)
		}
		pending := yardGate{yards: map[string]*yardGateState{"named": {pending: true}}, order: []string{"named"}}
		if refused := len(pending.verdict("default", subject).refusals) != 0; refused != test.everyYard {
			t.Errorf("%v beside a pending yard refused=%t", test.arguments, refused)
		}
	}
	harness := newRepairHarness(t)
	code, output := harness.run(t, func() { t.Error("Git authoring reached consent") }, "-Y", "named", "config", "set", "GIT_USER_NAME", "x", "--scope", "yard", "--git")
	if code == 0 || !strings.Contains(output, "release gate: yard named needs repair") || !strings.Contains(output, "yard -Y named config validate") {
		t.Fatalf("Git scalar authoring on a repair-only yard: code=%d\n%s", code, output)
	}
	code, output = harness.run(t, func() { t.Error("provision reached consent") }, "-Y", "named", "provision")
	if code == 0 || strings.Contains(output, "config validate") {
		t.Fatalf("the configuration hint is shown on a non-configuration refusal: code=%d\n%s", code, output)
	}
}

// A repair runs only with the installed release's own tooling: the real
// source proof refuses this fixture CLI, which is not the published runtime
// of the journal's target, before any effect. A bound source that changes
// while the operator confirms refuses the repair under the owner barrier.
func TestRepairNeedsTheInstalledReleaseTooling(t *testing.T) {
	harness := newRepairHarness(t)
	for _, arguments := range [][]string{{"-Y", "named", "init"}, {"-Y", "named", "teardown"}, {"-Y", "named", "stop"}} {
		var stdout, stderr bytes.Buffer
		options := harness.scenario.fixture.cli.options
		options.Arguments, options.Stdout, options.Stderr = arguments, &stdout, &stderr
		options.Prompt = confirmingPrompt{during: func() { t.Errorf("%v reached consent", arguments) }}
		options.AdapterRunner, options.InitPlatform, options.NetworkPolicy = harness.adapter, harness.platform, allowTestNetworkPolicy()
		program, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code == 0 || !strings.Contains(stderr.String(), "running tooling does not match the sealed completed release; next: resolve the protected release source failure with the sealed active release owner") ||
			harness.effects() != 0 {
			t.Fatalf("%v outside the installed runtime: code=%d effects=%d\n%s%s", arguments, code, harness.effects(), stdout.String(), stderr.String())
		}
	}
	code, output := harness.run(t, func() {
		harness.platform.mu.Lock()
		harness.sourceDrift = "-other-runtime"
		harness.platform.mu.Unlock()
	}, "-Y", "named", "stop")
	if code == 0 || !strings.Contains(output, "the installed release source changed after confirmation") || harness.effects() != 0 {
		t.Fatalf("stop over a changed source: code=%d effects=%d\n%s", code, harness.effects(), output)
	}
}

// runWhileWriterHoldsBarrier takes the writer's barrier after read-only
// admission so Execute waits for the journal change.
func (harness *repairHarness) runWhileWriterHoldsBarrier(t *testing.T, environment []string, write func(), arguments ...string) (int, string) {
	t.Helper()
	var once sync.Once
	harness.admittedSignal = func() {
		once.Do(func() {
			_, unlock, err := uninstall.OwnerLock(context.Background(), harness.scenario.fixture.cli.env["SUBYARD_HOME"], nil)
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				defer unlock()
				time.Sleep(200 * time.Millisecond)
				write()
			}()
		})
	}
	defer func() { harness.admittedSignal = nil }()
	return harness.runWith(t, environment, func() {}, arguments...)
}

// Local file settings are repaired through their prepared command on a yard
// that needs repair: import reaches its file and is rechecked after consent.
// Git authoring stays refused and names the manual repair.
func TestLocalConfigFileRepairOnARepairOnlyYard(t *testing.T) {
	arguments := func(source string) []string {
		return []string{"-Y", "named", "config", "import", "AGENT_codex_RULES", source, "--scope", "yard", "--local"}
	}
	source := filepath.Join(testkit.TempDir(t), "rules")
	testkit.WriteFile(t, source, []byte("repaired_rule()\n"), 0o600)
	harness := newRepairHarness(t)
	// The shipped default of the rules file lives in the package tree, which
	// gives it a persistent mapping.
	root := harness.scenario.fixture.cli.options.RepositoryRoot
	if err := os.MkdirAll(filepath.Join(root, "config/agents/codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/agents/codex/repo.rules"), []byte("allow_rule()\n"), 0o600)
	agents := filepath.Join(root, "config/agents.env")
	testkit.WriteFile(t, agents, []byte(strings.Replace(string(mustReadConfigFixture(t, agents)), "AGENT_codex_RULES="+filepath.Join(root, "repo.rules"),
		"AGENT_codex_RULES="+filepath.Join(root, "config/agents/codex/repo.rules"), 1)), 0o600)
	target := filepath.Join(harness.scenario.request.ConfigHome, "yards", "named", "overrides", "agents")
	flipped := harness.flippedJournal(t)
	journal := mustReadConfigFixture(t, harness.scenario.fixture.journalPath)
	code, output := harness.run(t, func() { testkit.WriteFile(t, harness.scenario.fixture.journalPath, flipped, 0o600) }, arguments(source)...)
	if code == 0 || !strings.Contains(output, "the release changed after confirmation") || len(filesUnder(t, target)) != 0 {
		t.Fatalf("import over a changed journal: code=%d files=%v\n%s", code, filesUnder(t, target), output)
	}
	testkit.WriteFile(t, harness.scenario.fixture.journalPath, journal, 0o600)
	code, output = harness.run(t, func() {}, arguments(source)...)
	if code != 0 || len(filesUnder(t, target)) == 0 {
		t.Fatalf("local import on a repair-only yard: code=%d\n%s", code, output)
	}
	code, output = harness.run(t, func() { t.Error("git authoring reached consent") }, "-Y", "named", "config", "import", "AGENT_codex_RULES", source, "--scope", "yard", "--git")
	if code == 0 || !strings.Contains(output, "release gate: yard named needs repair") || !strings.Contains(output, "yard -Y named config validate") {
		t.Fatalf("git import on a repair-only yard: code=%d\n%s", code, output)
	}
}

func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	return files
}

// A sync is gated by the yards its plan writes and the yards whose consumers
// it refreshes. As a configuration repair it may write a yard that needs
// repair and refresh one limited only by drift; a failed consumer and a
// written pending yard refuse it before publication, and a change it cannot
// place in one yard reaches every yard.
func TestConfigSyncIsGatedByTheYardsItWrites(t *testing.T) {
	gate := yardGate{yards: map[string]*yardGateState{"named": {failed: true}, "drifted": {drift: true}, "pending": {pending: true}}, order: []string{"drifted", "named", "pending"}}
	for _, test := range []struct {
		name      string
		paths     []string
		consumers []string
		refused   []string
	}{
		{"healthy target", []string{"yards/default/config.env"}, []string{"default"}, nil},
		{"written pending yard", []string{"yards/pending/config.env"}, nil, []string{"pending"}},
		{"written yard that needs repair", []string{"yards/named/config.env"}, nil, nil},
		{"consumer limited by drift", []string{"yards/default/config.env"}, []string{"default", "drifted"}, nil},
		{"consumer that failed", []string{"yards/default/config.env"}, []string{"default", "named"}, []string{"named"}},
		{"shared change", []string{"shared/config.env"}, nil, []string{"pending"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			verdict := gate.verdict("default", configSyncSubject(test.paths, test.consumers))
			var refused []string
			for _, refusal := range verdict.refusals {
				for _, yard := range []string{"drifted", "named", "pending"} {
					if strings.Contains(refusal.Error(), "yard "+yard) {
						refused = append(refused, yard)
					}
				}
			}
			if !slices.Equal(refused, test.refused) {
				t.Fatalf("refused %v, want %v: %v", refused, test.refused, verdict.err())
			}
		})
	}
}

// installPool gives the sample profile a resource whose declared shutdown
// destroys persistent data: never a repair of a yard that needs one.
func (harness *repairHarness) installPool(t *testing.T) string {
	t.Helper()
	root := harness.scenario.fixture.cli.options.RepositoryRoot
	directory := filepath.Join(root, "config/profiles/sample/resources/pool")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/profiles/sample/resources/pool.res"), []byte(strings.Join([]string{
		"COMMAND=pool", "HANDLER=resources/pool/handler.sh", "TITLE=Sample pool",
		`ACTION="up up host-change reversible"`, `ACTION="purge purge persistent-data-destruction irreversible"`,
		"BRINGUP=up", "SHUTDOWN=purge",
	}, "\n")+"\n"), 0o600)
	marker := filepath.Join(root, "pool-purged")
	testkit.WriteFile(t, filepath.Join(directory, "handler.sh"), []byte(`#!/usr/bin/env python3
import json, os, pathlib, sys
marker = pathlib.Path('`+marker+`')
mode = os.environ.get('SUBYARD_RESOURCE_MODE')
verb = sys.argv[1]
if mode == 'apply':
    marker.write_text(verb)
    sys.exit(0)
done = marker.exists()
step = dict(id='pool', target='fixture pool', observed='absent' if done else 'present', desired='absent',
            decision='skip' if done else 'apply', verify='read fixture pool', consequence='purge fixture pool')
print(json.dumps(dict(schema='yard.resource-action-assessment.v2', action=verb, changed=not done,
    consequences=[] if done else ['purge fixture pool'], steps=[step], binding='b' * 64)))
`), 0o700)
	return marker
}

// The gate is flipped between plan and execute in each direction, for each
// prepared repair member, an unsafe declared shutdown and a writer outside the
// repair set, on the direct CLI (consent) and RPC (plan, then execute). The
// effects follow the verdict at execute: the CLI's open admission is decided
// once on its snapshot; a repair is rechecked on fresh state; for RPC the
// execute-time admission is of record, so an open plan executed on a yard
// that now needs repair runs only as that repair.
func TestGateFlipBetweenPlanAndExecute(t *testing.T) {
	type command struct {
		name      string
		arguments []string // after -Y named
		rpc       string   // RPC command; arguments follow
	}
	commands := []command{
		{"stop", []string{"stop"}, "stop"},
		{"init", []string{"init"}, "init"},
		{"config set", []string{"config", "set", "SSH_PORT", "2231", "--scope", "yard", "--local"}, "config"},
		{"safe shutdown", []string{"relay", "down"}, "relay"},
		{"unsafe shutdown", []string{"pool", "purge"}, "pool"},
		{"writer outside the set", []string{"integration", "enable", "codex"}, "integration"},
	}
	// Outcome per command: open→repair and repair→open, for CLI then RPC.
	// effect: reaches its effect; none: refused with no effect.
	want := map[string][4]string{
		"stop":                   {"effect", "effect", "effect", "effect"},
		"init":                   {"effect", "effect", "effect", "effect"},
		"config set":             {"effect", "effect", "effect", "effect"},
		"safe shutdown":          {"effect", "effect", "effect", "effect"},
		"unsafe shutdown":        {"effect", "none", "none", "none"},
		"writer outside the set": {"effect", "none", "none", "none"},
	}
	for _, test := range commands {
		for index, mode := range []string{"cli open→repair", "cli repair→open", "rpc open→repair", "rpc repair→open"} {
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				harness := newRepairHarness(t)
				relay, pool := harness.installRelay(t), harness.installPool(t)
				failed := mustReadConfigFixture(t, harness.inspect)
				before, after := harness.ready, failed
				if strings.Contains(mode, "repair→open") {
					before, after = failed, harness.ready
				}
				testkit.WriteFile(t, harness.inspect, before, 0o600)
				effects := func() int {
					return harness.effects() + len(mustReadConfigFixtureOrEmpty(relay)) + len(mustReadConfigFixtureOrEmpty(pool))
				}
				start := effects()
				flip := func() { testkit.WriteFile(t, harness.inspect, after, 0o600) }
				var output string
				if strings.HasPrefix(mode, "cli") {
					_, output = harness.run(t, flip, append([]string{"-Y", "named"}, test.arguments...)...)
				} else {
					output = harness.rpcPlanExecute(t, test.rpc, test.arguments[1:], flip)
				}
				got := "none"
				if effects() != start {
					got = "effect"
				}
				if got != want[test.name][index] {
					t.Fatalf("%s: %s, want %s\n%s", mode, got, want[test.name][index], output)
				}
			})
		}
	}
}

// rpcPlanExecute plans one operation on yard named, runs between, then
// executes it, and returns what both returned.
func (harness *repairHarness) rpcPlanExecute(t *testing.T, name string, arguments []string, between func()) string {
	t.Helper()
	options := harness.scenario.fixture.cli.options
	options.AdapterRunner, options.InitPlatform, options.NetworkPolicy = harness.adapter, harness.platform, allowTestNetworkPolicy()
	integrations := &integrationRuntimeFixture{plan: reconcileruntime.IntegrationPlan{Fingerprint: strings.Repeat("c", 64), Scope: strings.Repeat("c", 64)}}
	options.IntegrationRuntime = func(config.Loaded) IntegrationRuntime { return integrations }
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	program.sourceBinding = harness.bindSources
	loaded, err := program.loadContext("named")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
	t.Cleanup(handler.closePlans)
	params, err := json.Marshal(map[string]any{"command": name, "arguments": arguments, "exact": name == "integration"})
	if err != nil {
		t.Fatal(err)
	}
	var plan any
	if plan, err = handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "flip", Params: params}, nil); err != nil {
		return "plan: " + err.Error()
	}
	between()
	executeParams := map[string]any{"confirmed": true}
	if exact, ok := plan.(exactOperationPlan); ok {
		executeParams["digest"] = exact.Digest
	}
	payload, err := json.Marshal(executeParams)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.execute", OperationID: "flip", Params: payload},
		func(string, any) (uint64, error) { return 0, nil }); err != nil {
		return "execute: " + err.Error()
	}
	return "executed"
}

// A prepared command admitted as read-only cannot run an effect in Execute,
// whatever argv made admission take it for a reader.
func TestReadAdmissionRunsNoEffect(t *testing.T) {
	harness := newRepairHarnessWith(t, func(yardFailureScenario, string) {})
	program, _, _, _ := harness.scenario.fixture.program(t, nil, nil, func(nestedCall, io.Writer) int { return 0 })
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := program.manifest.Lookup("config")
	program.gateAdmitted = &gateAdmission{kind: admissionRead}
	prepared, err := admitPreparedFixture(program).prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition,
		Arguments: []string{"set", "SSH_PORT", "2232", "--scope", "yard", "--local"}})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	orchestrator := program.operationOrchestrator(prepared.Plan.OperationID, loaded, nil, &definition)
	if _, err := prepared.Confirm(context.Background(), orchestrator, true); err != nil {
		t.Fatal(err)
	}
	before := harness.effects()
	if _, err := prepared.Execute(context.Background(), orchestrator, io.Discard); err == nil || !strings.Contains(err.Error(), "admitted as read-only") ||
		harness.effects() != before {
		t.Fatalf("read admission ran an effect: %v effects %d->%d", err, before, harness.effects())
	}
}

// A plan made under a repair keeps that repair's binding through execute: when
// execute is re-admitted on a ready release and a writer changes release
// history while it waits for the owner barrier, the binding compared under the
// barrier refuses it with no effect.
func TestRPCPlannedRepairBindingIsComparedUnderTheBarrier(t *testing.T) {
	harness := newRepairHarness(t)
	harness.scenario.fixture.cli.options.AdapterRunner = harness.adapter
	flipped := harness.flippedJournal(t)
	var once sync.Once
	var output string
	done := make(chan struct{})
	go func() {
		defer close(done)
		output = harness.rpcPlanExecute(t, "stop", nil, func() {
			// The yard was repaired outside history before execute.
			testkit.WriteFile(t, harness.inspect, harness.ready, 0o600)
			// Execute checks the planned binding early, then waits for the
			// barrier; meanwhile another migration writes history.
			harness.admittedSignal = func() {
				once.Do(func() {
					_, unlock, err := uninstall.OwnerLock(context.Background(), harness.scenario.fixture.cli.env["SUBYARD_HOME"], nil)
					if err != nil {
						t.Error(err)
						return
					}
					go func() {
						defer unlock()
						time.Sleep(200 * time.Millisecond)
						testkit.WriteFile(t, harness.scenario.fixture.journalPath, flipped, 0o600)
					}()
				})
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("RPC execute deadlocked")
	}
	if !strings.Contains(output, "the release changed after the plan") || harness.effects() != 0 {
		t.Fatalf("RPC execute over history changed under the barrier: effects=%d\n%s", harness.effects(), output)
	}
}

// A published configuration change refreshes its consumers in process only as
// their repair, decided on the state after publication: a ready release runs
// the ordinary child; a consumer limited to repair only by drift is refreshed
// with the installed source bound; a consumer with another finding, or tooling
// that is not the installed release's, is refused with its next step.
func TestConfigSyncConsumerRefreshIsAConsumerRepair(t *testing.T) {
	harness := newRepairHarnessWith(t, func(scenario yardFailureScenario, _ string) {
		observeFailing(t, scenario, "named", true)
		testkit.WriteFile(t, scenario.runtime.state("default"), []byte(strings.Repeat("d", 64)), 0o600)
	})
	program, _, _, _ := harness.scenario.fixture.program(t, nil, nil, func(nestedCall, io.Writer) int { return 0 })
	owner, unlock, err := uninstall.OwnerLock(context.Background(), program.env["SUBYARD_HOME"], nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	program.ownerMutationLock = &owner
	program.sourceBinding = harness.bindSources
	inProcess, finish, err := program.consumerRefreshAdmission(context.Background(), "default", []string{"default"})
	if err != nil || !inProcess || finish == nil || finish() != nil {
		t.Fatalf("drifted consumer: inProcess=%t err=%v", inProcess, err)
	}
	if _, _, err := program.consumerRefreshAdmission(context.Background(), "default", []string{"default", "named"}); err == nil ||
		!strings.Contains(err.Error(), "yard named has failed activation; next: yard -Y named init") {
		t.Fatalf("failed consumer: %v", err)
	}
	program.sourceBinding = nil // the real proof refuses this fixture CLI
	if _, _, err := program.consumerRefreshAdmission(context.Background(), "default", []string{"default"}); err == nil ||
		!strings.Contains(err.Error(), "running tooling does not match the sealed completed release; next: resolve the protected release source failure with the sealed active release owner") {
		t.Fatalf("consumer refresh with other tooling: %v", err)
	}
	testkit.WriteFile(t, harness.inspect, harness.ready, 0o600)
	if inProcess, _, err := program.consumerRefreshAdmission(context.Background(), "default", []string{"default", "named"}); err != nil || inProcess {
		t.Fatalf("ready release: inProcess=%t err=%v", inProcess, err)
	}
}

// A shell into a yard that needs repair diagnoses it with any tooling: it is
// not bound to the installed release's source.
func TestShellIntoARepairOnlyYardNeedsNoInstalledTooling(t *testing.T) {
	harness := newRepairHarness(t)
	log, environment := harness.installIncusCLI(t)
	var stdout, stderr bytes.Buffer
	options := harness.scenario.fixture.cli.options
	options.Arguments, options.Stdout, options.Stderr = []string{"-Y", "named", "shell", "--", "true"}, &stdout, &stderr
	options.Environment = append(slices.Clone(options.Environment), environment...)
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 || len(mustReadConfigFixtureOrEmpty(log)) == 0 {
		t.Fatalf("shell without installed tooling: code=%d\n%s%s", code, stdout.String(), stderr.String())
	}
}

// A network policy plan acts on every yard it stops or starts, not only the
// yards named: a reconcile that starts a pending yard that needs repair, and a
// link under isolation that restarts a drifted third yard or a pending
// endpoint, are refused before consent, naming that yard.
func TestNetworkPlansAreGatedByEveryYardTheyRestart(t *testing.T) {
	t.Run("reconcile starting a yard that needs repair", func(t *testing.T) {
		harness := newRepairHarness(t)
		code, output, host := networkRunWith(t, harness.scenario, func(host *networkCLIHost) {
			host.ready = true
			host.policy.PendingStart = []string{"named"}
		}, "-Y", "default", "network", "reconcile")
		if code == 0 || host.writes != 0 || !strings.Contains(output, "yard named needs repair first") {
			t.Fatalf("reconcile starting a yard that needs repair: code=%d writes=%d\n%s", code, host.writes, output)
		}
	})
	t.Run("link restarting a drifted third yard", func(t *testing.T) {
		harness := newRepairHarness(t)
		third := filepath.Join(harness.scenario.request.ConfigHome, "yards", "third", "config.env")
		if err := os.MkdirAll(filepath.Dir(third), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, third, []byte("SSH_PORT=2224\n"), 0o600)
		code, output, host := networkRunWith(t, harness.scenario, func(host *networkCLIHost) {
			host.ready = true
			host.policy.Isolation = true
		}, "-Y", "default", "network", "link", "default", "third")
		if code == 0 || host.writes != 0 || !strings.Contains(output, "yard named needs repair first") {
			t.Fatalf("link restarting a drifted yard that needs repair: code=%d writes=%d\n%s", code, host.writes, output)
		}
	})
	t.Run("link restarting a pending endpoint", func(t *testing.T) {
		scenario := newYardFailureScenario(t, true, true, "default", "named")
		inspect := scenario.replayReleaseEngine(t)
		scenario.runtime.failing(t, "named", true)
		requirePendingRuntimeOutcome(t, scenario.converge(t), "named")
		recordReadiness(t, scenario, inspect)
		code, output, host := networkRunWith(t, scenario, func(host *networkCLIHost) {
			host.ready = true
			host.policy.Isolation = true
		}, "-Y", "default", "network", "link", "default", "named")
		if code == 0 || host.writes != 0 || !strings.Contains(output, "yard named has pending configuration") {
			t.Fatalf("link restarting a pending endpoint: code=%d writes=%d\n%s", code, host.writes, output)
		}
	})
}
