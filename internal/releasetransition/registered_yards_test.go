package releasetransition

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Subyard/Subyard/internal/domain"
	"testing"
)

func TestRegisteredYardMigrationUsesDurableExistingJournal(t *testing.T) {
	for _, scenario := range []string{"complete", "cut-before-effect", "cut-after-effect", "stale-native", "late-decline", "per-yard-partial"} {
		t.Run(scenario, func(t *testing.T) {
			transition, _, _ := v2TransitionFixture(t, nil)
			owner := &nativeConsentOwner{v2TestReconciler: &v2TestReconciler{id: "yard.hermes"}, decline: scenario == "late-decline"}
			var registry RegistryV2
			if err := json.Unmarshal(transition.options.RegistryPayload, &registry); err != nil {
				t.Fatal(err)
			}
			registry.CurrentEpochs["owner-registration"] = 3
			registry.Migrations = append(registry.Migrations, MigrationDefinitionV2{ID: "registered-local-v3", Domain: "owner-registration", Kind: registeredYardsMigration, FromEpoch: 2, ToEpoch: 3, DependsOn: []string{"canonicalize-test-yard-owner-v2"}})
			payload, _ := json.Marshal(registry)
			transition.options.RegistryPayload = payload
			transition.options.RegisteredYards = []V2ActivationReconciler{owner}
			second := &nativeConsentOwner{v2TestReconciler: &v2TestReconciler{id: "yard.other"}, decline: true}
			if scenario == "per-yard-partial" {
				transition.options.RegisteredYards = append(transition.options.RegisteredYards, second)
			}
			var err error
			transition, err = NewV2Transition(transition.options)
			if err != nil {
				t.Fatal(err)
			}
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			plan, err := transition.Inspect(context.Background(), goal)
			if err != nil || len(plan.Blockers) != 0 {
				t.Fatalf("inspect: %#v %v", plan, err)
			}
			sentinel := errors.New("durable cut")
			if scenario == "stale-native" {
				owner.drift = digestC
			}
			if scenario == "cut-before-effect" {
				transition.options.fault = func(point string) error {
					if point == "after-step-evidence" {
						snapshot, _ := transition.store.ReadCurrentJournal()
						journal, _ := ParseJournal(snapshot.Payload)
						for _, step := range journal.Steps {
							if step.Resource == "yard.hermes" && step.Checkpoint == StepEvidence {
								return sentinel
							}
						}
					}
					return nil
				}
			}
			if scenario == "cut-after-effect" {
				transition.options.fault = func(point string) error {
					if point == "after-step-applied" && owner.reconciles > 0 {
						return sentinel
					}
					return nil
				}
			}
			outcome, err := transition.Converge(context.Background(), Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)})
			if scenario == "stale-native" {
				if owner.reconciles != 0 || outcome.Status == StatusReady {
					t.Fatalf("stale work ran: %#v %v", outcome, err)
				}
				return
			}
			if scenario == "complete" {
				if err != nil || outcome.Status != StatusReady || owner.reconciles != 1 {
					t.Fatalf("complete: %#v count=%d %v", outcome, owner.reconciles, err)
				}
			} else {
				if err == nil && outcome.Status == StatusReady {
					t.Fatal("cut marked ready")
				}
				snapshot, readErr := transition.store.ReadCurrentJournal()
				journal, parseErr := ParseJournal(snapshot.Payload)
				if readErr != nil || parseErr != nil || journal.Checkpoint == JournalComplete {
					t.Fatalf("cut lost journal: %#v %v %v", journal, readErr, parseErr)
				}
				transition.options.fault = nil
				owner.decline = false
				second.decline = false
				fresh, inspectErr := transition.Inspect(context.Background(), goal)
				if inspectErr != nil {
					t.Fatal(inspectErr)
				}
				outcome, err = transition.Converge(context.Background(), Execution{Plan: fresh.Plan, Authorization: v2TestAuthorization(fresh.Plan)})
				if err != nil || outcome.Status != StatusReady || owner.reconciles != 1 {
					t.Fatalf("resume: %#v count=%d %v", outcome, owner.reconciles, err)
				}
			}
			reopened, err := NewPOSIXV2Store(transition.options.ConfigHome)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reopened.InspectSourceMetadata(transition.registry, transition.registryDigest); err != nil {
				t.Fatalf("fresh-process source admission: %v", err)
			}
			repeat, err := transition.Inspect(context.Background(), goal)
			if err != nil || repeat.Assessment.Changed {
				t.Fatalf("repeat: %#v %v", repeat, err)
			}
		})
	}
}

type nativeConsentOwner struct {
	*v2TestReconciler
	decline bool
}

func (owner *nativeConsentOwner) Reconcile(ctx context.Context, links ReleaseLinks) error {
	if owner.decline {
		return domain.ErrOperationDeclined
	}
	return owner.v2TestReconciler.Reconcile(ctx, links)
}
