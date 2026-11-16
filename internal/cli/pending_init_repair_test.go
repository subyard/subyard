package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
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
		{`"consequences":["repair release activation"]},"outcome"`,
			`"consequences":["repair release activation"]},"blockers":[` + blockers + `],"decisions":[{"resource":"` + strings.TrimSuffix(resource, ".pending-configuration") + `","scope":"yard-attribution","decision":"preserve","result":"pending ` + yard + `"}],"outcome"`},
	} {
		if !strings.Contains(payload, replacement[0]) {
			t.Fatalf("engine fixture lacks %s", replacement[0])
		}
		payload = strings.Replace(payload, replacement[0], replacement[1], 1)
	}
	sealCurrentEngine(t, fixture, payload)
	outcome, err := fixture.cli.inspectMutationGate(context.Background(), "default")
	if err != nil || outcome == nil {
		t.Fatalf("pending gate fixture: %#v %v", outcome, err)
	}
	fixture.outcome = *outcome
	return fixture
}

// sealCurrentEngine installs payload as the current release engine and binds the
// completed journal to the resulting runtime manifest.
func sealCurrentEngine(t *testing.T, fixture configApplyRepairFixture, payload string) {
	t.Helper()
	engine := filepath.Join(fixture.cli.env["YARD_RUNTIME_ROOT"], "current", "bin", "yard-engine")
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
