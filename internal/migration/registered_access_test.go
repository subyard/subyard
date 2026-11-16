package migration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
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
	want := "yard alpha: native state cannot be inspected: this account cannot access the Incus socket; " +
		"yard default: native state cannot be inspected: this account cannot access the Incus socket"
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
