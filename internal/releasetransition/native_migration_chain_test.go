package releasetransition

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

// A refusal of the first native migration must not hide an independent refusal
// of the next one; both are owner verdicts observed before any journal.
func TestNativeMigrationChainReportsEveryMigrationRefusal(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	var registry RegistryV2
	if err := json.Unmarshal(transition.options.RegistryPayload, &registry); err != nil {
		t.Fatal(err)
	}
	registry.CurrentEpochs["owner-registration"] = 3
	registry.CurrentEpochs["power-metadata"] = 3
	registry.Migrations = append(registry.Migrations,
		MigrationDefinitionV2{ID: "registered-local-v3", Domain: "owner-registration", Kind: registeredYardsMigration, FromEpoch: 2, ToEpoch: 3, DependsOn: []string{"canonicalize-test-yard-owner-v2"}},
		MigrationDefinitionV2{ID: "publication-v2", Domain: "power-metadata", Kind: "owner-publication-state-v1", FromEpoch: 1, ToEpoch: 2},
		MigrationDefinitionV2{ID: "guest-tmp-v3", Domain: "power-metadata", Kind: guestDiskTmpMigration, FromEpoch: 2, ToEpoch: 3})
	payload, _ := json.Marshal(registry)
	transition.options.RegistryPayload = payload
	transition.options.RegisteredYards = []V2ActivationReconciler{&refusingRegisteredOwner{v2TestReconciler: &v2TestReconciler{id: "yard.alpha"}, refusal: &registeredVerdict{"yard alpha: registration refused"}}}
	transition.options.GuestDiskTmp = []V2ActivationReconciler{&refusingRegisteredOwner{v2TestReconciler: &v2TestReconciler{id: "yard.beta"}, refusal: &registeredVerdict{"yard beta: disk tmp refused"}}}
	transition, err := NewV2Transition(transition.options)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		inspection, err := transition.Inspect(context.Background(), Goal{Target: "release-a", Direction: DirectionActivateTarget})
		if err != nil {
			t.Fatalf("verdicts became an engine failure: %v", err)
		}
		got := []string{}
		for _, blocker := range inspection.Blockers {
			got = append(got, blocker.Resource+": "+blocker.Message)
		}
		want := []string{"migration.registered-local-v3: yard alpha: registration refused", "migration.guest-tmp-v3: yard beta: disk tmp refused"}
		if !slices.Equal(got, want) || inspection.Outcome == nil || inspection.Outcome.Status != StatusOperatorActionRequired {
			t.Fatalf("blockers %q, outcome %#v", got, inspection.Outcome)
		}
	}
	if snapshot, err := transition.store.ReadCurrentJournal(); err != nil || snapshot.Exists {
		t.Fatalf("refused inspection wrote a journal: %v", err)
	}
}
