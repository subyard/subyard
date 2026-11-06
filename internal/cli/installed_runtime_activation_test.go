package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
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
	// Convergence rechecks the plan its inspection approved before any effect.
	converge := request
	converge.Mode = releasetransition.ProcessConverge
	if !completedReleaseConfiguration(converge) {
		t.Fatal("convergence observed a different configuration than its approved inspection")
	}
	for _, change := range []func(*releasetransition.ProcessRequest){
		func(r *releasetransition.ProcessRequest) { r.Mode = "" },
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
	rechecked, err := (&profileRuntimeActivationReconciler{cli: fixture.cli, id: "core-guest-runtime", request: converge}).Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
	if err != nil || rechecked.Actual != observed.Actual || rechecked.Desired != observed.Desired || rechecked.Converged != observed.Converged {
		t.Fatalf("convergence recheck diverged from the approved inspection: %#v %#v %v", observed, rechecked, err)
	}
	// A completed-release repair stays inside the installed plan; ordinary
	// repair, which carries no process mode, applies the pending intent.
	if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil || platform.applied != 0 {
		t.Fatalf("completed-release repair applied pending configuration outside its plan: %v effects=%d", err, platform.applied)
	}
	ordinary := request
	ordinary.Mode = ""
	if err := (&profileRuntimeActivationReconciler{cli: fixture.cli, id: "core-guest-runtime", request: ordinary}).Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil || platform.applied != 1 {
		t.Fatalf("pending desired configuration was not applied by ordinary repair: %v effects=%d", err, platform.applied)
	}
}

// A completed-release repair whose approved plan observed the installed
// configuration repairs a stale yard to that configuration, never to intent that
// awaits init. The process keeps that decision after convergence replaces the
// completed journal; ordinary repair still applies desired intent.
func TestCompletedReleaseRepairAppliesInstalledConfiguration(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	cli := fixture.cli
	root := cli.options.RepositoryRoot
	configHome := cli.env["SUBYARD_CONFIG_HOME"]
	for _, path := range []string{filepath.Join(configHome, "yards/default"), filepath.Join(root, "config/skills/core/subyard-environment")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The pending rename and profile selection await init.
	writeCLIFile(t, filepath.Join(configHome, "yards/default/config.env"), "DEV_USER=other\nDEV_UID=1002\nENVIRONMENT_PROFILES=sample\n", 0o600)
	writeCLIFile(t, filepath.Join(root, "config/skills/core/subyard-environment/facts.py"), "# identity fixture\n", 0o600)
	mark := filepath.Join(root, "applied-runtime")
	stale, current := strings.Repeat("a", 64), strings.Repeat("b", 64)
	handler := `#!/bin/sh
[ "$DEV_USER" = dev ] && [ "$DEV_UID" = 1000 ] && [ -z "$ENVIRONMENT_PROFILES" ] || exit 19
case "$1" in
apply) : > '` + mark + `' ;;
observe) [ -f '` + mark + `' ] || { printf '%s\n' '{"state":"stale","actual":"` + stale + `","desired":"` + current + `"}'; exit 0; } ;;
*) exit 18 ;;
esac
printf '%s\n' '{"state":"current","actual":"` + current + `","desired":"` + current + `"}'
`
	writeCLIFile(t, filepath.Join(root, "config/profiles/sample/runtime.sh"), handler, 0o700)
	cli.options.Executor = appliedProfileExecutor{installed: true}
	store, err := releasetransition.NewPOSIXV2Store(configHome)
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
	request := releasetransition.ProcessRequest{Mode: releasetransition.ProcessConverge, ConfigHome: configHome, Target: journal.Goal.Target, Direction: journal.Goal.Direction, ArtifactDigest: journal.ArtifactDigest, RegistryDigest: journal.RegistryDigest}
	ordinary := request
	ordinary.Mode = ""
	if err := (&profileRuntimeActivationReconciler{cli: cli, id: "sample-runtime", request: ordinary}).Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err == nil {
		t.Fatal("ordinary repair did not observe the pending desired identity")
	}
	owner := &profileRuntimeActivationReconciler{cli: cli, id: "sample-runtime", request: request}
	approved, err := owner.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
	if err != nil || approved.Converged {
		t.Fatalf("approved repair plan: %#v %v", approved, err)
	}
	// Convergence makes the journal unfinished before any effect.
	owner.request.ArtifactDigest = "unfinished"
	if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
		t.Fatalf("installed-configuration repair: %v", err)
	}
	if _, err := os.Stat(mark); err != nil {
		t.Fatalf("stale installed runtime was not repaired: %v", err)
	}
	verified, err := owner.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
	if err != nil || !verified.Converged {
		t.Fatalf("repair did not verify against its installed plan: %#v %v", verified, err)
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
			owner = &profileRuntimeActivationReconciler{cli: cli, id: "sample-runtime", request: request}
			if rechecked, err := owner.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{}); err != nil || !reflect.DeepEqual(rechecked, result) {
				t.Fatalf("convergence recheck diverged from the approved inspection: %#v %#v %v", result, rechecked, err)
			}
			request.ArtifactDigest = "other"
			owner = &profileRuntimeActivationReconciler{cli: cli, id: "sample-runtime", request: request}
			if _, err := owner.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{}); err == nil {
				t.Fatal("unfinished release stopped using desired owner")
			}
		})
	}
}

type recordingProfilePlatform struct {
	*sampleRepairPlatformFixture
	recorded []string
	after    *ports.RuntimeObservation
}

func (platform *recordingProfilePlatform) RecordAppliedProfile(_ context.Context, name string, selected bool) error {
	platform.recorded = append(platform.recorded, fmt.Sprintf("%s=%t", name, selected))
	return nil
}

func (platform *recordingProfilePlatform) ApplyProfileRuntime(ctx context.Context, id, operation, actual, desired string) error {
	if platform.after != nil {
		platform.observation = platform.after
	}
	return platform.sampleRepairPlatformFixture.ApplyProfileRuntime(ctx, id, operation, actual, desired)
}

// Release convergence of an owner-service profile records the converged
// selection, so a completed release no longer observes a lagging snapshot left
// by an earlier profile enable or disable.
func TestProfileRuntimeReconcileRecordsOwnerServiceSelection(t *testing.T) {
	current := ports.RuntimeObservation{State: ports.RuntimeStateCurrent, Actual: strings.Repeat("b", 64), Desired: strings.Repeat("b", 64)}
	stale := ports.RuntimeObservation{State: ports.RuntimeStateStale, Actual: strings.Repeat("a", 64), Desired: strings.Repeat("b", 64)}
	deferred := ports.RuntimeObservation{State: ports.RuntimeStateDeferred}
	for _, scenario := range []struct {
		name     string
		owner    bool
		profiles string
		state    ports.RuntimeObservation
		want     string
	}{
		{"current-disabled", true, "ENVIRONMENT_PROFILES=\n", current, "sample=false"},
		{"current-selected", true, "ENVIRONMENT_PROFILES=sample\n", current, "sample=true"},
		{"applied-stale", true, "ENVIRONMENT_PROFILES=sample\n", stale, "sample=true"},
		{"deferred", true, "ENVIRONMENT_PROFILES=sample\n", deferred, ""},
		{"init-provisioned", false, "ENVIRONMENT_PROFILES=sample\n", current, ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newConfigApplyRepairFixture(t, false)
			root := fixture.cli.options.RepositoryRoot
			if scenario.owner {
				writeCLIFile(t, filepath.Join(root, "config/profiles/sample/owner.sh"), "#!/bin/sh\nexit 0\n", 0o700)
				writeCLIFile(t, filepath.Join(root, "config/profiles/sample/profile.json"), `{"schema_version":1,"owner_service":"owner.sh","runtime":{"activation_id":"sample-runtime","handler":"runtime.sh"}}`, 0o600)
				catalog, err := config.LoadCatalog(root)
				if err != nil {
					t.Fatal(err)
				}
				fixture.cli.catalog = catalog
			}
			configHome := fixture.cli.env["SUBYARD_CONFIG_HOME"]
			if err := os.MkdirAll(filepath.Join(configHome, "yards/default"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, filepath.Join(configHome, "yards/default/config.env"), scenario.profiles, 0o600)
			state := scenario.state
			platform := &recordingProfilePlatform{sampleRepairPlatformFixture: &sampleRepairPlatformFixture{initPlatformFixture: newInitPlatformFixture(), observation: &state}, after: &current}
			fixture.cli.options.InitPlatform = platform
			owner := &profileRuntimeActivationReconciler{cli: fixture.cli, id: "sample-runtime", request: releasetransition.ProcessRequest{ConfigHome: configHome}}
			if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(platform.recorded, " "); got != scenario.want {
				t.Fatalf("recorded selection=%q, want %q", got, scenario.want)
			}
		})
	}
}

type recordingProfileExecutor struct {
	appliedProfileExecutor
	records *[]string
}

func (fixture recordingProfileExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(request.Command) == 4 && strings.HasPrefix(request.Command[3], "__name__ = 'subyard_profile_selection_recorder'\n") {
		*fixture.records = append(*fixture.records, string(request.Stdin))
		return ports.InstanceExecResult{}, nil
	}
	return fixture.appliedProfileExecutor.Exec(ctx, project, instance, request)
}

// An owner-service profile disabled before profile disable recorded its applied
// selection keeps a lagging record. A completed-release repair records the
// converged selection; it does not reinstall the disabled runtime.
func TestCompletedReleaseRepairRecordsLaggingOwnerServiceSelection(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	cli := fixture.cli
	root := cli.options.RepositoryRoot
	configHome := cli.env["SUBYARD_CONFIG_HOME"]
	for _, path := range []string{filepath.Join(configHome, "yards/default"), filepath.Join(root, "config/skills/core/subyard-environment")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeCLIFile(t, filepath.Join(configHome, "yards/default/config.env"), "ENVIRONMENT_PROFILES=\n", 0o600)
	writeCLIFile(t, filepath.Join(root, "config/skills/core/subyard-environment/facts.py"), "# identity fixture\n", 0o600)
	writeCLIFile(t, filepath.Join(root, "config/profiles/sample/owner.sh"), "#!/bin/sh\nexit 0\n", 0o700)
	writeCLIFile(t, filepath.Join(root, "config/profiles/sample/profile.json"), `{"schema_version":1,"owner_service":"owner.sh","runtime":{"activation_id":"sample-runtime","handler":"runtime.sh"}}`, 0o600)
	catalog, err := config.LoadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	cli.catalog = catalog
	digest := strings.Repeat("b", 64)
	// The runtime was removed: a selection containing sample observes it stale.
	handler := `#!/bin/sh
case "$1" in
observe) ;;
*) exit 18 ;;
esac
case " $ENVIRONMENT_PROFILES " in
*" sample "*) printf '%s\n' '{"state":"stale","actual":"` + strings.Repeat("a", 64) + `","desired":"` + digest + `"}' ;;
*) printf '%s\n' '{"state":"current","actual":"` + digest + `","desired":"` + digest + `"}' ;;
esac
`
	writeCLIFile(t, filepath.Join(root, "config/profiles/sample/runtime.sh"), handler, 0o700)
	var records []string
	cli.options.Executor = recordingProfileExecutor{appliedProfileExecutor{installed: true, profiles: []string{"sample"}}, &records}
	store, err := releasetransition.NewPOSIXV2Store(configHome)
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
	request := releasetransition.ProcessRequest{Mode: releasetransition.ProcessConverge, ConfigHome: configHome, Target: journal.Goal.Target, Direction: journal.Goal.Direction, ArtifactDigest: journal.ArtifactDigest, RegistryDigest: journal.RegistryDigest}
	owner := &profileRuntimeActivationReconciler{cli: cli, id: "sample-runtime", request: request}
	approved, err := owner.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
	if err != nil || approved.Converged {
		t.Fatalf("lagging applied selection was not observed: %#v %v", approved, err)
	}
	owner.request.ArtifactDigest = "unfinished"
	if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
		t.Fatalf("lagging selection repair: %v", err)
	}
	if len(records) != 1 || !strings.Contains(records[0], `"profile":"sample"`) || !strings.Contains(records[0], `"selected":false`) {
		t.Fatalf("converged selection was not recorded: %q", records)
	}
}

type installedReleaseRepairFixture struct {
	*installedActivationFixture
	installed      ports.RuntimeObservation
	releaseApplied int
}

func (fixture *installedReleaseRepairFixture) ObserveInstalledReleaseRuntime(context.Context, string) (ports.RuntimeObservation, error) {
	return fixture.installed, nil
}

func (fixture *installedReleaseRepairFixture) ApplyInstalledReleaseRuntime(_ context.Context, _, _, actual, desired string) error {
	if actual != fixture.installed.Actual || desired != fixture.installed.Desired {
		return errors.New("installed repair received another observation")
	}
	fixture.releaseApplied++
	fixture.installed = ports.RuntimeObservation{State: ports.RuntimeStateCurrent, Actual: desired, Desired: desired}
	return nil
}

// A stale core runtime of a completed release is repaired on the installed
// projection its plan observed, never on desired settings awaiting init.
func TestCompletedReleaseCoreRepairUsesInstalledProjection(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	configHome := fixture.cli.env["SUBYARD_CONFIG_HOME"]
	store, err := releasetransition.NewPOSIXV2Store(configHome)
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
	request := releasetransition.ProcessRequest{Mode: releasetransition.ProcessConverge, ConfigHome: configHome, Target: journal.Goal.Target, Direction: journal.Goal.Direction, ArtifactDigest: journal.ArtifactDigest, RegistryDigest: journal.RegistryDigest}
	// Desired settings would produce another digest than the installed projection.
	platform := &installedReleaseRepairFixture{
		installedActivationFixture: &installedActivationFixture{initPlatformFixture: newInitPlatformFixture(), state: ports.RuntimeObservation{State: ports.RuntimeStateStale, Actual: strings.Repeat("a", 64), Desired: strings.Repeat("c", 64)}},
		installed:                  ports.RuntimeObservation{State: ports.RuntimeStateStale, Actual: strings.Repeat("a", 64), Desired: strings.Repeat("b", 64)},
	}
	fixture.cli.options.InitPlatform = platform
	owner := &profileRuntimeActivationReconciler{cli: fixture.cli, id: "core-guest-runtime", request: request}
	if approved, err := owner.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{}); err != nil || approved.Converged {
		t.Fatalf("approved repair plan: %#v %v", approved, err)
	}
	owner.request.ArtifactDigest = "unfinished"
	if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
		t.Fatalf("installed core repair: %v", err)
	}
	if platform.releaseApplied != 1 || platform.applied != 0 {
		t.Fatalf("core repair effects: installed=%d desired=%d", platform.releaseApplied, platform.applied)
	}
}

type unfinishedFactsPlatform struct{ *recordingProfilePlatform }

func (unfinishedFactsPlatform) RecordAppliedProfile(context.Context, string, bool) error {
	return reconcileruntime.ErrGuestFactsUnfinished
}

// Unfinished guest facts belong to the core guest refresh that runs later in the
// same transition; a profile runtime must not stop a release retry before it.
func TestProfileRuntimeReconcileLeavesUnfinishedFactsToCoreRefresh(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	root := fixture.cli.options.RepositoryRoot
	writeCLIFile(t, filepath.Join(root, "config/profiles/sample/owner.sh"), "#!/bin/sh\nexit 0\n", 0o700)
	writeCLIFile(t, filepath.Join(root, "config/profiles/sample/profile.json"), `{"schema_version":1,"owner_service":"owner.sh","runtime":{"activation_id":"sample-runtime","handler":"runtime.sh"}}`, 0o600)
	catalog, err := config.LoadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	fixture.cli.catalog = catalog
	current := ports.RuntimeObservation{State: ports.RuntimeStateCurrent, Actual: strings.Repeat("b", 64), Desired: strings.Repeat("b", 64)}
	fixture.cli.options.InitPlatform = unfinishedFactsPlatform{&recordingProfilePlatform{sampleRepairPlatformFixture: &sampleRepairPlatformFixture{initPlatformFixture: newInitPlatformFixture(), observation: &current}}}
	owner := &profileRuntimeActivationReconciler{cli: fixture.cli, id: "sample-runtime", request: releasetransition.ProcessRequest{ConfigHome: fixture.cli.env["SUBYARD_CONFIG_HOME"]}}
	if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
		t.Fatalf("unfinished facts stopped the release retry: %v", err)
	}
}
