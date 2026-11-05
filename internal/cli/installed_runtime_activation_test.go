package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRuntimeRecoveryPlanBindsPartialDeliveryAndRejectsFurtherDrift(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	state := ports.RuntimeObservation{State: ports.RuntimeStateStale, Actual: strings.Repeat("a", 64), Desired: strings.Repeat("b", 64)}
	platform := &sampleRepairPlatformFixture{initPlatformFixture: newInitPlatformFixture(), observation: &state}
	fixture.cli.options.InitPlatform = platform
	owner := &profileRuntimeActivationReconciler{cli: fixture.cli, id: "sample-runtime", request: releasetransition.ProcessRequest{ConfigHome: fixture.cli.env["SUBYARD_CONFIG_HOME"]}}
	ctx := context.Background()
	first, err := owner.PrepareActivationRecovery(ctx, releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	state.Actual = strings.Repeat("c", 64)
	second, err := owner.PrepareActivationRecovery(ctx, releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil || first == second {
		t.Fatalf("partial delivery did not bind fresh recovery: %v", err)
	}
	state.Actual = strings.Repeat("d", 64)
	if err := owner.Reconcile(ctx, releasetransition.ReleaseLinks{}); !errors.Is(err, domain.ErrPlanStale) || len(platform.applied) != 0 {
		t.Fatalf("changed native recovery applied: %v effects=%v", err, platform.applied)
	}
}

type coldInstalledIncus struct{ *testkit.Incus }

func (coldInstalledIncus) LocalInstallationAbsent() bool { return true }

type installedActivationFixture struct {
	*initPlatformFixture
	state   ports.RuntimeObservation
	applied int
}

type installedReleaseActivationFixture struct{ *installedActivationFixture }

func (fixture *installedReleaseActivationFixture) ObserveInstalledReleaseRuntime(context.Context, string) (ports.RuntimeObservation, error) {
	return ports.RuntimeObservation{State: ports.RuntimeStateCurrent, Actual: fixture.state.Desired, Desired: fixture.state.Desired}, nil
}

func TestCompletedReleaseKeepsPendingConfigurationForOrdinaryInit(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	store, err := releasetransition.NewPOSIXV2Store(fixture.cli.env["SUBYARD_CONFIG_HOME"])
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	request := releasetransition.ProcessRequest{Mode: releasetransition.ProcessInspect, ConfigHome: fixture.cli.env["SUBYARD_CONFIG_HOME"], Target: journal.Goal.Target, Direction: journal.Goal.Direction, ArtifactDigest: journal.ArtifactDigest, RegistryDigest: journal.RegistryDigest}
	if !completedReleaseConfiguration(request) {
		t.Fatal("completed current release did not retain applied configuration")
	}
	for _, change := range []func(*releasetransition.ProcessRequest){
		func(r *releasetransition.ProcessRequest) { r.Mode = releasetransition.ProcessConverge },
		func(r *releasetransition.ProcessRequest) { r.Target = "other" },
		func(r *releasetransition.ProcessRequest) { r.Direction = releasetransition.DirectionActivatePrevious },
		func(r *releasetransition.ProcessRequest) { r.ArtifactDigest = "other" },
		func(r *releasetransition.ProcessRequest) { r.RegistryDigest = "other" },
	} {
		other := request
		change(&other)
		if completedReleaseConfiguration(other) {
			t.Fatal("configuration projection admitted a different release or mutation")
		}
	}
	platform := &installedReleaseActivationFixture{&installedActivationFixture{initPlatformFixture: newInitPlatformFixture(), state: ports.RuntimeObservation{State: ports.RuntimeStateStale, Actual: strings.Repeat("a", 64), Desired: strings.Repeat("b", 64)}}}
	fixture.cli.options.InitPlatform = platform
	owner := &profileRuntimeActivationReconciler{cli: fixture.cli, id: "core-guest-runtime", request: request}
	observed, err := owner.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
	if err != nil || !observed.Converged {
		t.Fatalf("pending desired configuration blocked ordinary commands: %#v %v", observed, err)
	}
	if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil || platform.applied != 1 {
		t.Fatalf("pending desired configuration was not applied: %v effects=%d", err, platform.applied)
	}
}

func (fixture *installedActivationFixture) ObserveInstalledRuntime(context.Context, string) (ports.RuntimeObservation, error) {
	return fixture.state, nil
}
func (fixture *installedActivationFixture) ApplyInstalledRuntime(context.Context, string, string, string, string) error {
	fixture.applied++
	fixture.state.State = ports.RuntimeStateCurrent
	fixture.state.Actual = fixture.state.Desired
	return nil
}

func TestInstalledHostActivationKeepsLocalScopesWithoutIncus(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, true)
	fixture.cli.options.Incus = coldInstalledIncus{&testkit.Incus{Err: errors.New("host activation must not query Incus")}}
	platform := &installedActivationFixture{initPlatformFixture: newInitPlatformFixture(), state: ports.RuntimeObservation{State: ports.RuntimeStateStale, Actual: strings.Repeat("a", 64), Desired: strings.Repeat("b", 64)}}
	fixture.cli.options.InitPlatform = platform
	owner := &profileRuntimeActivationReconciler{cli: fixture.cli, id: "key-tools-runtime", request: releasetransition.ProcessRequest{ConfigHome: fixture.cli.env["SUBYARD_CONFIG_HOME"]}}
	_, targets, err := owner.targets(context.Background())
	if err != nil || len(targets) != 2 {
		t.Fatalf("host scopes lost to Incus absence: %v %v", targets, err)
	}
	if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil || platform.applied != 1 {
		t.Fatalf("installed host tools did not converge once across shared local scopes: %v effects=%d", err, platform.applied)
	}
}

func TestInstalledStoppedDeferralHasNoWarningOrInitAdvice(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	fixture.cli.options.InitPlatform = &installedActivationFixture{initPlatformFixture: newInitPlatformFixture(), state: ports.RuntimeObservation{State: ports.RuntimeStateDeferred, Actual: strings.Repeat("a", 64)}}
	owner := &profileRuntimeActivationReconciler{cli: fixture.cli, id: "core-guest-runtime", request: releasetransition.ProcessRequest{ConfigHome: fixture.cli.env["SUBYARD_CONFIG_HOME"]}}
	observation, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil || !observation.Converged || len(observation.Warnings) != 0 {
		t.Fatalf("expected stopped deferral diagnosed as warning/manual init: %#v %v", observation, err)
	}
}

func TestCurrentInstalledSelectionFactsKeepReleasePlanReadyAndStable(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	platform := &installedActivationFixture{initPlatformFixture: newInitPlatformFixture(), state: ports.RuntimeObservation{
		State: ports.RuntimeStateCurrent, Actual: strings.Repeat("a", 64), Desired: strings.Repeat("a", 64),
	}}
	fixture.cli.options.InitPlatform = platform
	owner := &profileRuntimeActivationReconciler{cli: fixture.cli, id: "core-guest-runtime", request: releasetransition.ProcessRequest{ConfigHome: fixture.cli.env["SUBYARD_CONFIG_HOME"]}}
	links := releasetransition.ReleaseLinks{Active: "release-current"}
	transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{
		ConfigHome: testkit.TempDir(t), Releases: releasetransition.ReleasePair{From: "release-current", Target: "release-current"},
		RegistryPayload:     []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":1},"migrations":[]}`),
		ArtifactDigest:      releasetransition.Fingerprint(strings.Repeat("d", 64)),
		Reconcilers:         []releasetransition.V2ActivationReconciler{owner},
		VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true },
		ObserveLinks:        func(context.Context) (releasetransition.ReleaseLinks, error) { return links, nil },
		ActivateLinks: func(context.Context, releasetransition.ReleasePair) (releasetransition.ReleaseLinks, error) {
			return links, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	goal := releasetransition.Goal{Target: "release-current", Direction: releasetransition.DirectionActivateTarget}
	initial, err := transition.Inspect(ctx, goal)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := transition.Converge(ctx, releasetransition.Execution{Plan: initial.Plan, Authorization: "initial-consent"})
	if err != nil || completed.Status != releasetransition.StatusReady {
		t.Fatalf("complete: %+v %v", completed, err)
	}
	before, err := transition.Inspect(ctx, goal)
	if err != nil || before.Outcome.Status != releasetransition.StatusReady {
		t.Fatalf("before: %+v %v", before, err)
	}
	// Raw native bindings still move with the desired selection snapshot. A
	// current observation must not manufacture release drift from that movement.
	platform.state.Actual, platform.state.Desired = strings.Repeat("b", 64), strings.Repeat("b", 64)
	after, err := transition.Inspect(ctx, goal)
	if err != nil || after.Outcome.Status != releasetransition.StatusReady || after.Plan != before.Plan {
		t.Fatalf("current selection facts reopened the release: before=%+v after=%+v err=%v", before, after, err)
	}
	platform.state.State, platform.state.Actual = ports.RuntimeStateStale, strings.Repeat("c", 64)
	drift, err := transition.Inspect(ctx, goal)
	if err != nil || drift.Outcome.Status != releasetransition.StatusMigrationRequired || drift.Plan == after.Plan {
		t.Fatalf("real helper drift was suppressed: %+v %v", drift, err)
	}
}

// Generic shipped consumers receive the applied owner during completed-release
// inspection, including a yard that has never installed coding integrations.
type appliedProfileExecutor struct {
	installed bool
	profiles  []string
}

func (fixture appliedProfileExecutor) Exec(_ context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(request.Command) == 4 && strings.HasPrefix(request.Command[3], "__name__ = 'subyard_profile_selection_observer'\n") {
		selected := fixture.profiles
		if selected == nil {
			selected = []string{}
		}
		payload, _ := json.Marshal(selected)
		return ports.InstanceExecResult{Stdout: payload}, nil
	}
	if len(request.Command) == 5 {
		value := map[string]any{"installed": fixture.installed}
		if fixture.installed {
			value["applied_settings"] = map[string]string{"DEV_USER": "dev", "DEV_UID": "1000"}
			value["selected"] = []string{}
		}
		payload, _ := json.Marshal(value)
		return ports.InstanceExecResult{Stdout: payload}, nil
	}
	return ports.InstanceExecResult{Stdout: []byte(`{"DEV_USER":"dev","DEV_UID":"1000"}`)}, nil
}
func TestGenericProfileCompletedObservationUsesAppliedOwner(t *testing.T) {
	for _, installed := range []bool{true, false} {
		t.Run(fmt.Sprint(installed), func(t *testing.T) {
			fixture := newConfigApplyRepairFixture(t, false)
			cli := fixture.cli
			root := cli.options.RepositoryRoot
			for _, path := range []string{filepath.Join(cli.env["SUBYARD_CONFIG_HOME"], "yards/default"), filepath.Join(root, "config/skills/core/subyard-environment")} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			writeCLIFile(t, filepath.Join(cli.env["SUBYARD_CONFIG_HOME"], "yards/default/config.env"), "DEV_USER=other\nDEV_UID=1002\nENVIRONMENT_PROFILES=sample\n", 0o600)
			writeCLIFile(t, filepath.Join(root, "config/skills/core/subyard-environment/facts.py"), "# identity fixture\n", 0o600)
			handler := `#!/bin/sh
[ "$DEV_USER" = dev ] && [ "$DEV_UID" = 1000 ] && [ -z "$ENVIRONMENT_PROFILES" ] || exit 19
printf '%s\n' '{"state":"current","actual":"` + strings.Repeat("a", 64) + `","desired":"` + strings.Repeat("a", 64) + `"}'
`
			writeCLIFile(t, filepath.Join(root, "config/profiles/sample/runtime.sh"), handler, 0o700)
			cli.options.Executor = appliedProfileExecutor{installed: installed}
			store, err := releasetransition.NewPOSIXV2Store(cli.env["SUBYARD_CONFIG_HOME"])
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			journal, err := releasetransition.ParseJournal(snapshot.Payload)
			if err != nil {
				t.Fatal(err)
			}
			request := releasetransition.ProcessRequest{Mode: releasetransition.ProcessInspect, ConfigHome: cli.env["SUBYARD_CONFIG_HOME"], Target: journal.Goal.Target, Direction: journal.Goal.Direction, ArtifactDigest: journal.ArtifactDigest, RegistryDigest: journal.RegistryDigest}
			owner := &profileRuntimeActivationReconciler{cli: cli, id: "sample-runtime", request: request}
			result, err := owner.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
			if err != nil || !result.Converged {
				t.Fatalf("applied owner observation: %#v %v", result, err)
			}
			request.Mode = releasetransition.ProcessConverge
			owner.request = request
			if _, err := owner.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{}); err == nil {
				t.Fatal("unfinished release stopped using desired owner")
			}
		})
	}
}
