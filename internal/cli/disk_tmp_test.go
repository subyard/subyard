package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	protocolv1 "github.com/Subyard/Subyard/internal/releasetransition/protocol/v1"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRestartDisclosureDeclineLeavesProtectedStateUntouched(t *testing.T) {
	for _, terminal := range []bool{true, false} {
		t.Run(map[bool]string{true: "decline", false: "non-terminal"}[terminal], func(t *testing.T) {
			fixture := newConfigApplyRepairFixture(t, false)
			home := fixture.cli.env["SUBYARD_CONFIG_HOME"]
			type protected struct {
				Info os.FileInfo
				Body string
			}
			snapshot := func() map[string]protected {
				state := map[string]protected{}
				err := filepath.WalkDir(filepath.Join(home, "release-transition"), func(path string, entry os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					info, err := entry.Info()
					if err != nil {
						return err
					}
					body := ""
					if !entry.IsDir() {
						data, err := os.ReadFile(path)
						if err != nil {
							return err
						}
						body = string(data)
					}
					state[path] = protected{info, body}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				return state
			}
			before := snapshot()
			var output bytes.Buffer
			program := fixture.cli
			program.options.Stdout = &output
			program.options.Stderr = &output
			prompt := &testkit.Prompt{Answers: []bool{false}}
			if terminal {
				program.options.Prompt = prompt
			} else {
				program.options.Prompt = nil
				program.options.Stdin = strings.NewReader("")
				program.promptInputTerminal = func() bool { return false }
			}
			text := migration.DiskTmpDisclosure("synthetic", true)
			mutated := false
			prepared := &preparedCommand{CLI: program, Definition: command.Definition{Name: "update"}, Plan: domain.OperationPlan{
				OperationID: "restart-disclosure", Command: "update", Effect: domain.CommandMutate, Confirmation: domain.ConfirmationPromptDefaultYes, Target: domain.TargetLocalController,
				Steps: []domain.OperationStep{{ID: "release.resource.0", Target: "yard synthetic", Observed: "marker absent", Desired: text, Decision: domain.StepApply, Verify: "native marker"}},
			}, execute: func(context.Context, *application.Orchestrator, io.Writer) (domain.AdapterResult, error) {
				mutated = true
				return domain.AdapterResult{Status: "ok"}, nil
			}}
			prepared.seal()
			if code := program.runPreparedCommand(context.Background(), prepared, false); code != 1 || mutated {
				t.Fatalf("unconfirmed update executed: code=%d mutation=%t %s", code, mutated, output.String())
			}
			if !strings.Contains(output.String(), text) {
				t.Fatal("restart disclosure missing before confirmation", output.String())
			}
			if terminal && (len(prompt.Requests) != 1 || !strings.Contains(output.String(), "declined")) {
				t.Fatal("did not reach ordinary decline", output.String())
			}
			if !terminal && !strings.Contains(output.String(), domain.ErrConfirmationRequired.Error()) {
				t.Fatal("missing consent was not the refusal", output.String())
			}
			after := snapshot()
			if len(before) != len(after) {
				t.Fatal("decline added protected state")
			}
			for path, old := range before {
				fresh, ok := after[path]
				if !ok || old.Body != fresh.Body || !os.SameFile(old.Info, fresh.Info) || old.Info.Mode() != fresh.Info.Mode() || !old.Info.ModTime().Equal(fresh.Info.ModTime()) {
					t.Fatalf("decline changed %s", path)
				}
			}
		})
	}
}

func TestPendingDiskTmpReadyWireAndStatus(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, true)
	platform := &installedActivationFixture{initPlatformFixture: newInitPlatformFixture(), state: ports.RuntimeObservation{State: ports.RuntimeStateStale, Actual: strings.Repeat("a", 64), Desired: strings.Repeat("b", 64)}}
	fixture.cli.options.InitPlatform = platform
	owner := (fixture.cli.profileActivationReconciler(releasetransition.ProcessRequest{ConfigHome: fixture.cli.env["SUBYARD_CONFIG_HOME"]}, "core-guest-runtime").(*profileRuntimeActivationReconciler))
	ctx := context.Background()
	_, targets, err := owner.targets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	native := fixture.cli.options.Incus.(*testkit.Incus)

	for _, target := range targets {
		key := target.Loaded.Context.IncusProject + "/" + target.Loaded.Context.YardInstanceName
		info := native.Instances[key]
		if info.Config == nil {
			info.Config = map[string]string{}
		}
		info.Config["user.subyard.disk_tmp"] = "v1"
		info.LocalConfig = maps.Clone(info.Config)
		native.Instances[key] = info
	}
	before, err := owner.Observe(ctx, releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil || before.Converged {
		t.Fatalf("completed-marker stale runtime is ready: %#v %v", before, err)
	}
	for _, target := range targets {
		yard := target.Loaded.Context
		key := yard.IncusProject + "/" + yard.YardInstanceName
		info := native.Instances[key]
		info.Status = "Stopped"
		info.Config = map[string]string{"volatile.uuid": "uuid-original", "user.subyard.name": target.Name, "user.subyard.managed": "true"}
		info.LocalConfig = maps.Clone(info.Config)
		native.Instances[key] = info
	}
	after, err := owner.Observe(ctx, releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil || !after.Converged || after.Desired != before.Desired || len(after.Warnings) != len(targets) {
		t.Fatalf("deferral observation %#v %v", after, err)
	}
	if !slices.IsSorted(after.Warnings) {
		t.Fatal("warnings unsorted")
	}
	if err := owner.Reconcile(ctx, releasetransition.ReleaseLinks{}); err != nil || platform.applied != 0 {
		t.Fatalf("deferred runtime applied %d: %v", platform.applied, err)
	}
	// Host tools stay strict despite sharing the same adapter family.
	keyOwner := *owner
	keyOwner.id = "key-tools-runtime"
	keyState, err := keyOwner.Observe(ctx, releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil || keyState.Converged {
		t.Fatalf("pending marker exempted host tools: %#v %v", keyState, err)
	}
	profileOwner := *owner
	profileOwner.id = "sample-runtime"
	fixture.cli.options.InitPlatform = &sampleRepairPlatformFixture{initPlatformFixture: newInitPlatformFixture(), observation: &platform.state}
	profileState, err := profileOwner.Observe(ctx, releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil || !profileState.Converged || len(profileState.Warnings) != 0 {
		t.Fatalf("catalog runtime did not defer quietly: %#v %v", profileState, err)
	}
	if err := profileOwner.Reconcile(ctx, releasetransition.ReleaseLinks{}); err != nil {
		t.Fatal(err)
	}
	fixture.cli.options.InitPlatform = platform
	// Host boot may restore desired-running power without completing this marker.
	// Inspection must remain read-only and name the ordinary update retry.
	for _, target := range targets {
		yard := target.Loaded.Context
		key := yard.IncusProject + "/" + yard.YardInstanceName
		instance := native.Instances[key]
		instance.Status = "Running"
		native.Instances[key] = instance
	}
	after, err = owner.Observe(ctx, releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil || !after.Converged || platform.applied != 0 || len(after.Warnings) != len(targets) {
		t.Fatal("boot-restored pending yard lost read-only ready warning", after, err)
	}
	for _, warning := range after.Warnings {
		if !strings.Contains(warning, "run yard update --yes") {
			t.Fatal(warning)
		}
	}
	links := releasetransition.ReleaseLinks{Active: "release-current"}
	transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{ConfigHome: testkit.TempDir(t), Releases: releasetransition.ReleasePair{From: "release-current", Target: "release-current"}, RegistryPayload: []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":1},"migrations":[]}`), ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("d", 64)), Reconcilers: []releasetransition.V2ActivationReconciler{owner}, VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true }, ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) { return links, nil }})
	if err != nil {
		t.Fatal(err)
	}
	goal := releasetransition.Goal{Target: "release-current", Direction: releasetransition.DirectionActivateTarget}
	inspection, err := transition.InspectProcessV1(ctx, goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(ctx, releasetransition.Execution{Plan: inspection.Plan, Authorization: "consent"})
	if err != nil || outcome.Status != releasetransition.StatusReady || len(outcome.Warnings) != len(targets) {
		t.Fatalf("ready warning outcome %#v %v", outcome, err)
	}
	fresh, err := transition.InspectProcessV1(ctx, goal)
	if err != nil || fresh.Outcome.Status != releasetransition.StatusReady || len(fresh.Outcome.Warnings) != len(targets) {
		t.Fatalf("fresh inspection %#v %v", fresh, err)
	}
	payload, err := json.Marshal(releasetransition.ProcessResponse{SchemaVersion: 1, Inspection: &fresh})
	if err != nil {
		t.Fatal(err)
	}
	released, err := protocolv1.DecodeResponse(bytes.NewReader(payload))
	if err != nil || string(released.Inspection.Outcome.Status) != "ready" || len(released.Inspection.Outcome.Warnings) != len(targets) {
		t.Fatalf("released wire lost warnings: %s %v", payload, err)
	}
	var output bytes.Buffer
	fixture.cli.options.Stdout = &output
	readiness := &domain.ReleaseReadiness{Status: "ready", Message: outcome.Message, Warnings: outcome.Warnings}
	fixture.cli.printReleaseReadiness(readiness, "")
	encoded, err := json.Marshal(readiness)
	if err != nil {
		t.Fatal(err)
	}
	for _, warning := range after.Warnings {
		if !strings.Contains(output.String(), warning) || !strings.Contains(string(encoded), warning) {
			t.Fatal("status lost pending-policy warning", warning)
		}
	}
	// Completion restores strict readiness for this yard without changing Desired.
	yard := targets[0].Loaded.Context
	key := yard.IncusProject + "/" + yard.YardInstanceName
	instance := native.Instances[key]
	instance.Config["user.subyard.disk_tmp"] = "v1"
	instance.LocalConfig = maps.Clone(instance.Config)
	native.Instances[key] = instance

	strict, err := owner.Observe(ctx, releasetransition.ReleasePair{}, links)
	if err != nil || strict.Converged || strict.Desired != after.Desired {
		t.Fatalf("completed marker still exempted: %#v %v", strict, err)
	}
}

func TestDiskTmpRetryNeverExtendsUnfinishedJournal(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	home := fixture.cli.env["SUBYARD_CONFIG_HOME"]
	path := filepath.Join(home, "release-transition/v2/journal.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := releasetransition.ParseJournal(data)
	if err != nil {
		t.Fatal(err)
	}
	journal.Checkpoint = releasetransition.JournalMigrating
	data, err = json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, data, 0o600)
	prepared := &preparedCommand{CLI: fixture.cli, release: &releaseExecution{configHome: home, prepared: releaseruntime.Prepared{Action: "update.activate", SourceRelease: string(journal.Goal.Target), TargetRelease: string(journal.Goal.Target)}}}
	if err := fixture.cli.prepareDiskTmpRetry(context.Background(), prepared); err != nil || prepared.release.retryDiskTmp != nil {
		t.Fatalf("unfinished journal acquired new restart work: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, after) {
		t.Fatal("resume assessment changed its journal", err)
	}
}

func TestDiskTmpWarningsGiveExecutableRetryAndKeepSummaryLast(t *testing.T) {
	targets := []configTarget{}
	found := map[string]pendingDiskTmp{}
	for i := 0; i < 34; i++ {
		name := fmt.Sprintf("yard-%02d", i)
		targets = append(targets, configTarget{Name: name})
		found[name] = pendingDiskTmp{Power: "Stopped"}
	}
	warnings := diskTmpWarnings(found, targets)
	if len(warnings) != 33 || !strings.Contains(warnings[32], "2 additional yards") || !slices.IsSorted(warnings) || !strings.Contains(warnings[0], "yard -Y yard-00 start; then run yard migrate") {
		t.Fatal(warnings)
	}
	found["yard-00"] = pendingDiskTmp{Power: "Running"}
	if got := diskTmpWarnings(found, targets); !strings.Contains(got[0], "run yard update --yes") {
		t.Fatal(got)
	}
}
