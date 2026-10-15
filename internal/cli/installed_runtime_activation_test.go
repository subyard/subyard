package cli

import (
	"context"
	"errors"
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
	_, targets, err := owner.targets()
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
