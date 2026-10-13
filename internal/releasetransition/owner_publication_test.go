package releasetransition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnerPublicationBoundaryCommitsBeforeActivationAndResumes(t *testing.T) {
	transition, _, settingsPath := v2TransitionFixtureWithReleases(t, nil,
		ReleasePair{From: "release-a", Target: "release-b"}, ReleaseLinks{Active: "release-a"})
	payload, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	transition.registry, transition.registryDigest, err = ParseRegistryV2(payload, transition.catalog)
	if err != nil {
		t.Fatal(err)
	}
	transition.options.RegistryPayload = payload
	before, _ := os.ReadFile(settingsPath)
	interrupted := false
	transition.options.fault = func(point string) error {
		if point == "after-ledger-cas" && !interrupted {
			snapshot, readErr := transition.store.ReadLedger()
			if readErr != nil {
				return readErr
			}
			ledger, _, parseErr := ParseLedgerV2(snapshot.Payload, transition.registry)
			if parseErr != nil {
				return parseErr
			}
			if ledger.Domains["power-metadata"].Epoch == 2 {
				interrupted = true
				return errors.New("interrupt committed power boundary")
			}
		}
		return nil
	}
	activated := false
	links := ReleaseLinks{Active: "release-a"}
	transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return links, nil }
	transition.options.ActivateLinks = func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
		state, readErr := transition.store.ReadMigrationLedger(transition.registry, transition.registryDigest)
		if readErr != nil || state.Ledger.Domains["power-metadata"].Epoch != 2 {
			t.Fatalf("activation before reader boundary: %+v %v", state, readErr)
		}
		activated = true
		links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
		return links, nil
	}
	goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil || len(inspection.Blockers) != 0 {
		t.Fatalf("epoch-1 upgrade: %+v %v", inspection, err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || !interrupted || activated || outcome.Status != StatusRecovering {
		t.Fatalf("boundary interruption: %+v %v activated=%v", outcome, err, activated)
	}
	resume, err := transition.Inspect(context.Background(), goal)
	if err != nil || resume.Resume == nil {
		t.Fatalf("resume: %+v %v", resume, err)
	}
	outcome, err = transition.Converge(context.Background(), Execution{Plan: resume.Plan})
	if err != nil || !activated || outcome.Status != StatusReady {
		t.Fatalf("resume completion: %+v %v", outcome, err)
	}
	state, err := transition.store.ReadMigrationLedger(transition.registry, transition.registryDigest)
	if err != nil || state.Ledger.Domains["power-metadata"].Epoch != 2 || len(state.Ledger.Domains["power-metadata"].Applied) != 1 {
		t.Fatalf("power boundary committed more than once: %+v %v", state, err)
	}
	// Only existing settings migration changes its fixture; the boundary adds no
	// Incus operation or rewrite of canonical desired-power/owning receipt data.
	after, _ := os.ReadFile(settingsPath)
	if len(before) == 0 || len(after) == 0 {
		t.Fatal("existing settings were lost")
	}
}

func TestOwnerPublicationBoundaryRefusesEpochOneRollbackBeforeMutation(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"absent", "baseline", "partial", ""} {
		t.Run(state, func(t *testing.T) {
			fixture := newV2RollbackBoundaryFixtureWithLedger(t, "0.18.1", "", payload, state)
			inspection, err := fixture.transition.Inspect(context.Background(), Goal{Target: "release-a", Direction: DirectionActivatePrevious})
			if err != nil || inspection.Outcome == nil || inspection.Outcome.Code != CodeRollbackIncompatible || inspection.Assessment.Changed {
				t.Fatalf("unsafe rollback: %+v %v", inspection, err)
			}
			fixture.assertUnchanged(t, true)
		})
	}
}
