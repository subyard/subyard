package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/testkit"
)

// The completed-history gate as release activation reports a selected
// integration's persist adoption that it does not start: typed pending
// configuration whose next step is plain init of the yard.
func pendingInitGateFixture(t *testing.T, named bool, resource string, extra string) configApplyRepairFixture {
	return pendingInitGateFixtureFor(t, named, "default", resource, extra)
}

func pendingInitGateFixtureFor(t *testing.T, named bool, yard, resource string, extra string) configApplyRepairFixture {
	t.Helper()
	fixture := newConfigApplyRepairFixture(t, named)
	retry := shellquote.Command([]string{"yard", "-Y", yard, "init"})
	quoted := strings.ReplaceAll(retry, "'", `'\''`)
	message := `persist adoption of \"/home/dev/.claude/projects\" in yard %s is pending: release activation does not start data transfers; data was preserved`
	message = fmt.Sprintf(message, yard)
	blockers := fmt.Sprintf(`{"code":"dependency-unavailable","resource":%q,"message":"%s","retry":"%s"}`, resource, message, quoted) + extra
	engine := filepath.Join(fixture.cli.env["YARD_RUNTIME_ROOT"], "current", "bin", "yard-engine")
	payload := string(mustReadConfigFixture(t, engine))
	for _, replacement := range [][2]string{
		{`"status":"migration-required"`, `"status":"operator-action-required"`},
		{`"code":"transition-required"`, `"code":"dependency-unavailable"`},
		{`"message":"activation drift","retry":"run yard update"`, `"message":"` + message + `","retry":"` + quoted + `","transaction":"tx-0123456789abcdef"`},
		{`"consequences":["repair release activation"]},"outcome"`, `"consequences":["repair release activation"]},"blockers":[` + blockers + `],"outcome"`},
	} {
		if !strings.Contains(payload, replacement[0]) {
			t.Fatalf("engine fixture lacks %s", replacement[0])
		}
		payload = strings.Replace(payload, replacement[0], replacement[1], 1)
	}
	writeCLIFile(t, engine, payload, 0o700)
	manifestPath := filepath.Join(fixture.cli.env["YARD_RUNTIME_ROOT"], "current", "runtime-files.sha256")
	var manifest []string
	for _, line := range strings.Split(strings.TrimSuffix(string(mustReadConfigFixture(t, manifestPath)), "\n"), "\n") {
		if strings.HasSuffix(line, "  ./bin/yard-engine") {
			line = fmt.Sprintf("%x  ./bin/yard-engine", sha256.Sum256([]byte(payload)))
		}
		manifest = append(manifest, line)
	}
	sealed := strings.Join(manifest, "\n") + "\n"
	writeCLIFile(t, manifestPath, sealed, 0o600)
	record, err := releasetransition.ParseJournal(mustReadConfigFixture(t, fixture.journalPath))
	if err != nil {
		t.Fatal(err)
	}
	record.ArtifactDigest = releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256([]byte(sealed))))
	journal, err := releasetransition.MarshalJournal(record)
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, fixture.journalPath, string(journal), 0o600)
	outcome, err := fixture.cli.inspectMutationGate(context.Background(), "default")
	if err != nil || outcome == nil {
		t.Fatalf("pending gate fixture: %#v %v", outcome, err)
	}
	fixture.outcome = *outcome
	return fixture
}

// A pending adoption observed by the ordinary init plan; applying it adopts
// the directory, after which activation readiness is clear.
func pendingAdoptionRuntime(t *testing.T, fixture configApplyRepairFixture) *integrationRuntimeFixture {
	runtime := &integrationRuntimeFixture{plan: reconcileruntime.IntegrationPlan{
		Changed: true, Fingerprint: strings.Repeat("a", 64), Scope: strings.Repeat("b", 64)}}
	runtime.afterApply = func() {
		runtime.plan.Changed, runtime.plan.Fingerprint = false, strings.Repeat("c", 64)
		writeCLIFile(t, fixture.readyMarker, "ready\n", 0o600)
	}
	fixture.cli.options.IntegrationRuntime = func(config.Loaded) IntegrationRuntime { return runtime }
	return runtime
}

func convergedRepairPlatform(fixture configApplyRepairFixture) *sampleRepairPlatformFixture {
	platform := &sampleRepairPlatformFixture{initPlatformFixture: newInitPlatformFixture()}
	for stage := range platform.converged {
		platform.converged[stage] = true
	}
	fixture.cli.options.InitPlatform = platform
	return platform
}

func TestPendingPersistAdoptionAdmitsPlainInitWithoutConfigDrift(t *testing.T) {
	fixture := pendingInitGateFixture(t, false, "activation.materialized-config.pending-configuration", "")
	convergedRepairPlatform(fixture)
	runtime := pendingAdoptionRuntime(t, fixture)
	fake := fixture.cli.options.Executor.(*testkit.Incus)
	fake.ExecSteps = nil
	loaded, err := fixture.cli.resolveReleaseTransitionContext(context.Background(), "default", fixture.cli.env["SUBYARD_CONFIG_HOME"])
	if err != nil {
		t.Fatal(err)
	}
	targets, err := fixture.cli.localConfigTargets(loaded, true)
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		for _, target := range targets {
			appendHashSteps(t, fake, target.Loaded)
		}
	}
	journalBefore := mustReadConfigFixture(t, fixture.journalPath)
	ledgerPath := filepath.Join(filepath.Dir(fixture.journalPath), "ledger.json")
	ledgerBefore := mustReadConfigFixture(t, ledgerPath)

	// The refusal's next step is admitted as the profile-init repair.
	permit, remaining, err := fixture.cli.prepareInitAdmission(context.Background(), "default", nil, fixture.outcome)
	if err != nil || permit == nil || remaining != nil {
		t.Fatalf("pending adoption refused its own next step: permit=%v remaining=%#v err=%v", permit, remaining, err)
	}
	if len(permit.driftedTargetNames) != 1 || permit.driftedTargetNames[0] != "default" {
		t.Fatal("pending item is not the selected yard's drift", permit.driftedTargetNames)
	}
	unlock, err := fixture.cli.lockConfigApplyRepair(context.Background(), permit)
	if err != nil {
		t.Fatal(err)
	}
	// The ordinary init applies the adoption under the permit.
	if err := runtime.ApplyIntegrations(context.Background(), runtime.plan); err != nil {
		t.Fatal(err)
	}
	err = fixture.cli.finishConfigApplyRepair(context.Background(), permit)
	unlock()
	if err != nil {
		t.Fatalf("adoption did not restore readiness: %v", err)
	}
	if !bytes.Equal(journalBefore, mustReadConfigFixture(t, fixture.journalPath)) ||
		!bytes.Equal(ledgerBefore, mustReadConfigFixture(t, ledgerPath)) {
		t.Fatal("pending init repair changed completed release history")
	}
	// The gate is clear and a repeat init is ordinary and has nothing to repair.
	if outcome, err := fixture.cli.inspectMutationGate(context.Background(), "default"); err != nil || outcome != nil {
		t.Fatalf("gate after adoption: %#v %v", outcome, err)
	}
	if runtime.plan.Changed {
		t.Fatal("repeat init would change persist state")
	}
}

func TestPendingInitRepairRefusesEverythingElse(t *testing.T) {
	for _, scenario := range []string{"different-yard", "untyped-blocker", "history-blocker", "profile-argument", "other-next-step"} {
		t.Run(scenario, func(t *testing.T) {
			resource, extra := "activation.materialized-config.pending-configuration", ""
			switch scenario {
			case "untyped-blocker":
				resource = "activation.materialized-config"
			case "history-blocker":
				extra = `,{"code":"dependency-unavailable","resource":"transition.source-metadata","message":"history","retry":"run yard update --check"}`
			case "other-next-step":
				extra = `,{"code":"dependency-unavailable","resource":"activation.materialized-config.pending-configuration","message":"fixture unowned selected artifact","retry":"move fixture file aside"}`
			}
			fixture := pendingInitGateFixture(t, scenario == "different-yard", resource, extra)
			convergedRepairPlatform(fixture)
			pendingAdoptionRuntime(t, fixture)
			yard, arguments := "default", []string(nil)
			switch scenario {
			case "different-yard":
				yard = "named"
			case "profile-argument":
				arguments = []string{"--profile", "sample"}
			}
			permit, err := fixture.cli.prepareProfileInitRepair(context.Background(), yard, arguments, fixture.outcome)
			if permit != nil {
				t.Fatalf("pending init repair admitted %s (err=%v)", scenario, err)
			}
			if scenario == "profile-argument" {
				return
			}
			_, remaining, _ := fixture.cli.prepareInitAdmission(context.Background(), yard, arguments, fixture.outcome)
			if remaining == nil {
				t.Fatalf("%s: init admitted without a permit", scenario)
			}
		})
	}
}

// Integrations, persist adoption included, are applied by the provision stage.
type adoptingPlatform struct {
	*sampleRepairPlatformFixture
	runtime *integrationRuntimeFixture
}

func (platform *adoptingPlatform) CheckStage(ctx context.Context, stage ports.ReconcileStageID) (bool, error) {
	return platform.VerifyStage(ctx, stage)
}

func (platform *adoptingPlatform) VerifyStage(ctx context.Context, stage ports.ReconcileStageID) (bool, error) {
	if stage == ports.ReconcileStageProvision {
		return !platform.runtime.plan.Changed, nil
	}
	return platform.sampleRepairPlatformFixture.VerifyStage(ctx, stage)
}

func (platform *adoptingPlatform) ApplyStage(ctx context.Context, stage ports.ReconcileStageID) error {
	if err := platform.sampleRepairPlatformFixture.ApplyStage(ctx, stage); err != nil || stage != ports.ReconcileStageProvision {
		return err
	}
	return platform.runtime.ApplyIntegrations(ctx, platform.runtime.plan)
}

func TestPendingPersistAdoptionInitCommandCompletes(t *testing.T) {
	fixture := pendingInitGateFixture(t, false, "activation.materialized-config.pending-configuration", "")
	writeCLIFile(t, filepath.Join(fixture.cli.env["SUBYARD_CONFIG_HOME"], "host-id"), "owner-a\n", 0o600)
	sample := convergedRepairPlatform(fixture)
	sample.converged[ports.ReconcileStageFinalize] = true
	runtime := pendingAdoptionRuntime(t, fixture)
	platform := &adoptingPlatform{sampleRepairPlatformFixture: sample, runtime: runtime}
	fixture.cli.options.InitPlatform = platform
	fake := fixture.cli.options.Executor.(*testkit.Incus)
	fake.ExecSteps = nil
	loaded, err := fixture.cli.resolveReleaseTransitionContext(context.Background(), "default", fixture.cli.env["SUBYARD_CONFIG_HOME"])
	if err != nil {
		t.Fatal(err)
	}
	targets, err := fixture.cli.localConfigTargets(loaded, true)
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		for _, target := range targets {
			appendHashSteps(t, fake, target.Loaded)
		}
	}
	var output bytes.Buffer
	fixture.cli.options.Stdout, fixture.cli.options.Stderr = &output, &output
	fixture.cli.options.Arguments = []string{"init", "--yes"}
	if code := fixture.cli.Run(context.Background()); code != 0 || runtime.applied != 1 {
		t.Fatalf("yard init did not complete the adoption: code=%d applied=%d output=%s", code, runtime.applied, output.String())
	}
	if outcome, err := fixture.cli.inspectMutationGate(context.Background(), "default"); err != nil || outcome != nil {
		t.Fatalf("gate after init: %#v %v", outcome, err)
	}
	output.Reset()
	repeat, err := New(fixture.cli.options)
	if err != nil {
		t.Fatal(err)
	}
	if code := repeat.Run(context.Background()); code != 0 || runtime.applied != 1 || len(platform.applied) != 1 {
		t.Fatalf("repeat init was not a no-op: code=%d applied=%d stages=%v output=%s", code, runtime.applied, platform.applied, output.String())
	}
}

func TestPendingInitRepairAdmitsANamedYard(t *testing.T) {
	fixture := pendingInitGateFixtureFor(t, true, "named", "activation.materialized-config.pending-configuration", "")
	convergedRepairPlatform(fixture)
	settled := &integrationRuntimeFixture{plan: reconcileruntime.IntegrationPlan{Fingerprint: strings.Repeat("d", 64), Scope: strings.Repeat("e", 64)}}
	pending := &integrationRuntimeFixture{plan: reconcileruntime.IntegrationPlan{Changed: true, Fingerprint: strings.Repeat("a", 64), Scope: strings.Repeat("b", 64)}}
	fixture.cli.options.IntegrationRuntime = func(loaded config.Loaded) IntegrationRuntime {
		if loaded.Context.YardName == "named" {
			return pending
		}
		return settled
	}
	matchingConfigHashes(t, fixture, 4)
	permit, remaining, err := fixture.cli.prepareInitAdmission(context.Background(), "named", nil, fixture.outcome)
	if err != nil || permit == nil || remaining != nil || len(permit.driftedTargetNames) != 1 || permit.driftedTargetNames[0] != "named" {
		t.Fatalf("named yard init was not admitted: permit=%v remaining=%#v err=%v", permit, remaining, err)
	}
	// The gate names the named yard; init of the default yard is not its next step.
	if other, err := fixture.cli.prepareProfileInitRepair(context.Background(), "default", nil, fixture.outcome); other != nil || err != nil {
		t.Fatalf("default yard init admitted for another yard's gate: %v %v", other, err)
	}
}

func TestPendingInitRepairShowsTheAdoptionRefusal(t *testing.T) {
	for _, mode := range []string{"permit", "command"} {
		t.Run(mode, func(t *testing.T) {
			fixture := pendingInitGateFixture(t, false, "activation.materialized-config.pending-configuration", "")
			writeCLIFile(t, filepath.Join(fixture.cli.env["SUBYARD_CONFIG_HOME"], "host-id"), "owner-a\n", 0o600)
			convergedRepairPlatform(fixture)
			// What the ordinary init plan reports for an unsafe recreated source: every
			// refusal, each with its own next step.
			refusal := &initIntegrationRepairFixture{integrationRuntimeFixture: &integrationRuntimeFixture{}, err: domain.CollectRefusals(
				errors.New(`persist transfer in yard default: unsafe persist data at "/home/dev/.claude/projects" (must be owned by the developer and not writable by group or others); data was preserved. Next: chmod`),
				errors.New(`persist transfer in yard default: "/home/dev/.claude/projects/mnt" is a mount point inside persist data. Next: findmnt`),
			)}
			fixture.cli.options.IntegrationRuntime = func(config.Loaded) IntegrationRuntime { return refusal }
			var text string
			if mode == "permit" {
				permit, remaining, err := fixture.cli.prepareInitAdmission(context.Background(), "default", nil, fixture.outcome)
				if permit != nil || remaining != nil || err == nil {
					t.Fatalf("refused adoption was hidden behind the gate: permit=%v remaining=%#v err=%v", permit, remaining, err)
				}
				text = err.Error()
			} else {
				var output bytes.Buffer
				fixture.cli.options.Stdout, fixture.cli.options.Stderr = &output, &output
				fixture.cli.options.Arguments = []string{"init", "--yes"}
				if code := fixture.cli.Run(context.Background()); code == 0 {
					t.Fatal("init with a refused adoption succeeded")
				}
				text = output.String()
			}
			for _, want := range []string{"unsafe persist data", "Next: chmod", "is a mount point", "Next: findmnt"} {
				if !strings.Contains(text, want) {
					t.Fatalf("refusal omitted %q: %s", want, text)
				}
			}
			if strings.Contains(text, "release activation does not start data transfers") {
				t.Fatalf("init reprinted the gate instead of the cause: %s", text)
			}
		})
	}
}

// No configuration drift on any yard: every assessment reads matching hashes.
func matchingConfigHashes(t *testing.T, fixture configApplyRepairFixture, rounds int) {
	t.Helper()
	fake := fixture.cli.options.Executor.(*testkit.Incus)
	fake.ExecSteps = nil
	loaded, err := fixture.cli.resolveReleaseTransitionContext(context.Background(), "default", fixture.cli.env["SUBYARD_CONFIG_HOME"])
	if err != nil {
		t.Fatal(err)
	}
	targets, err := fixture.cli.localConfigTargets(loaded, true)
	if err != nil {
		t.Fatal(err)
	}
	for range rounds {
		for _, target := range targets {
			appendHashSteps(t, fake, target.Loaded)
		}
	}
}
