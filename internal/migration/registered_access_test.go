package migration

import (
	"context"
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

type accessDeniedNative struct {
	*registeredNativeFixture
}

func (accessDeniedNative) Instance(context.Context, string, string) (ports.InstanceInfo, error) {
	return ports.InstanceInfo{}, fmt.Errorf("get server extensions: %w: %w", domain.ErrIncusAccessDenied,
		fmt.Errorf("dial unix /private/socket: %w", os.ErrPermission))
}

// Every warm local yard reports its own typed refusal with the access repair,
// not a missing release dependency, and nothing is applied.
func TestRegisteredMigrationReportsSocketAccessPerYard(t *testing.T) {
	var owners []releasetransition.V2ActivationReconciler
	var home string
	for _, yard := range []string{"default", "alpha"} {
		owner, native, _ := registeredMigrationFixture(t)
		if home == "" {
			home = owner.ConfigHome
		}
		owner.Yard = yard
		load := owner.LoadTargets
		owner.LoadTargets = func(ctx context.Context, view releasetransition.V2SettingsSnapshotView) ([]RegisteredYardTarget, error) {
			targets, err := load(ctx, view)
			for index := range targets {
				targets[index].Loaded.Context.YardName = yard
				targets[index].Runtime.Incus = accessDeniedNative{native}
			}
			return targets, err
		}
		owners = append(owners, owner)
	}
	payload := []byte(`{"schemaVersion":2,"minimumEpochs":{"owner-registration":2},"currentEpochs":{"owner-registration":3},"migrations":[{"id":"registered-v3","domain":"owner-registration","fromEpoch":2,"toEpoch":3,"kind":"registered-yards-v2-to-v3"}]}`)
	transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{ConfigHome: home, RegistryPayload: payload, ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)), Releases: releasetransition.ReleasePair{From: "release-a", Target: "release-a"}, ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
		return releasetransition.ReleaseLinks{Active: "release-a"}, nil
	}, RegisteredYards: owners, VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	goal := releasetransition.Goal{Target: "release-a", Direction: releasetransition.DirectionActivateTarget}
	plan, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatalf("access denial escaped as an inspection failure: %v", err)
	}
	if err := releasetransition.ValidateProcessInspection(goal, plan); err != nil {
		t.Fatal(err)
	}
	outcome := plan.Outcome
	if outcome == nil || outcome.Code != releasetransition.CodePreconditionBlocked {
		t.Fatalf("wrong outcome: %#v", plan)
	}
	// Target order: the default yard first, then by name.
	want := "yard default: native state cannot be inspected: this account cannot access the Incus socket; " +
		"yard alpha: native state cannot be inspected: this account cannot access the Incus socket"
	if outcome.Message != want || outcome.Retry != domain.IncusAccessNextStep {
		t.Fatalf("outcome = %q / %q", outcome.Message, outcome.Retry)
	}
	if strings.Contains(outcome.Message+outcome.Retry, "private") || strings.Contains(outcome.Retry, "compatible release") {
		t.Fatalf("unsafe or misleading outcome: %#v", outcome)
	}
	result, err := transition.Converge(context.Background(), releasetransition.Execution{Plan: plan.Plan, Authorization: "confirmed"})
	if err != nil || result.Code != releasetransition.CodePreconditionBlocked {
		t.Fatalf("refused update: %#v %v", result, err)
	}
	if !errors.Is(domain.IncusAccessRefusal("alpha", domain.ErrIncusAccessDenied), domain.ErrIncusAccessDenied) {
		t.Fatal("typed cause lost")
	}
}

// toggledNative denies the socket on demand and interrupts the first apply of
// its yard, leaving the journal migrating.
type toggledNative struct {
	*registeredNativeFixture
	deny      *bool
	interrupt context.CancelFunc
}

func (native toggledNative) Instance(ctx context.Context, project, name string) (ports.InstanceInfo, error) {
	if *native.deny {
		return accessDeniedNative{native.registeredNativeFixture}.Instance(ctx, project, name)
	}
	return native.registeredNativeFixture.Instance(ctx, project, name)
}

func (native toggledNative) AdoptRegisteredInstance(ctx context.Context, before ports.InstanceInfo, values map[string]string) error {
	if native.interrupt != nil {
		native.interrupt()
		return context.Canceled
	}
	return native.registeredNativeFixture.AdoptRegisteredInstance(ctx, before, values)
}

// Retrying an interrupted transition without socket access reports each
// yard's typed refusal: a yard with a verified row, and an implicit default
// yard that has no instance and therefore no journal row.
func TestInterruptedRegisteredMigrationResumeReportsAccessPerYard(t *testing.T) {
	for _, defaultAbsent := range []bool{false, true} {
		t.Run(fmt.Sprintf("default-absent=%t", defaultAbsent), func(t *testing.T) {
			deny := false
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var owners []releasetransition.V2ActivationReconciler
			var home string
			for _, yard := range []string{"default", "alpha"} {
				owner, native, _ := registeredMigrationFixture(t)
				if home == "" {
					home = owner.ConfigHome
				}
				owner.Yard, owner.ConfigHome = yard, home
				instance := native.Instances["fixture/yard"]
				instance.Config["user.subyard.name"], instance.LocalConfig["user.subyard.name"] = yard, yard
				native.Instances["fixture/yard"] = instance
				native.Reconcile.Instance = instance
				if yard == "default" && defaultAbsent {
					// The implicit default yard has no instance: converged, no journal row.
					delete(native.Instances, "fixture/yard")
				}
				wrapped := toggledNative{registeredNativeFixture: native, deny: &deny}
				if yard == "alpha" {
					wrapped.interrupt = cancel
				}
				load := owner.LoadTargets
				owner.LoadTargets = func(ctx context.Context, view releasetransition.V2SettingsSnapshotView) ([]RegisteredYardTarget, error) {
					targets, err := load(ctx, view)
					for index := range targets {
						targets[index].Loaded.Context.YardName = yard
						targets[index].Loaded.Context.Paths.ConfigHome = home
						targets[index].Runtime.Incus = wrapped
					}
					return targets, err
				}
				owners = append(owners, owner)
			}
			payload := []byte(`{"schemaVersion":2,"minimumEpochs":{"owner-registration":2},"currentEpochs":{"owner-registration":3},"migrations":[{"id":"registered-v3","domain":"owner-registration","fromEpoch":2,"toEpoch":3,"kind":"registered-yards-v2-to-v3"}]}`)
			transition := func() *releasetransition.V2Transition {
				transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{ConfigHome: home, RegistryPayload: payload, ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)), Releases: releasetransition.ReleasePair{From: "release-a", Target: "release-a"}, ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
					return releasetransition.ReleaseLinks{Active: "release-a"}, nil
				}, RegisteredYards: owners, RegisteredEndpointSettings: []string{"SAMPLE_HOST", "SAMPLE_PORT"},
					VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true }})
				if err != nil {
					t.Fatal(err)
				}
				return transition
			}
			alphaConfig := filepath.Join(home, "yards", "alpha", "config.env")
			if err := os.MkdirAll(filepath.Dir(alphaConfig), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, alphaConfig, []byte("ENVIRONMENT_PROFILES=sample\n"), 0o600)
			goal := releasetransition.Goal{Target: "release-a", Direction: releasetransition.DirectionActivateTarget}
			plan, err := transition().Inspect(context.Background(), goal)
			if err != nil || plan.Outcome != nil && plan.Outcome.Status == releasetransition.StatusOperatorActionRequired {
				t.Fatalf("initial plan: %#v %v", plan.Outcome, err)
			}
			interrupted, err := transition().Converge(ctx, releasetransition.Execution{Plan: plan.Plan, Authorization: "confirmed"})
			if err == nil && interrupted.Status == releasetransition.StatusReady {
				t.Fatalf("interrupted convergence reported success: %#v", interrupted)
			}
			t.Logf("interrupted: %#v %v", interrupted, err)
			deny = true
			retry, err := transition().Inspect(context.Background(), goal)
			if err != nil {
				t.Fatalf("access denial escaped the resumed inspection: %v", err)
			}
			if retry.Outcome == nil || retry.Outcome.Retry != domain.IncusAccessNextStep {
				t.Fatalf("resumed outcome lost the access repair: %#v", retry.Outcome)
			}
			for _, yard := range []string{"default", "alpha"} {
				if !strings.Contains(retry.Outcome.Message, "yard "+yard+": native state cannot be inspected") {
					t.Fatalf("resumed outcome omitted yard %s: %q", yard, retry.Outcome.Message)
				}
			}

		})
	}
}
