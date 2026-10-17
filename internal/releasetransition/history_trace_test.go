package releasetransition

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Completed settings/owner precede an independently inserted power migration.
// The first journaled frontier is valid but is not a global registry prefix.
func TestV2HistoricalDomainFrontierResumesAtDurableCuts(t *testing.T) {
	for _, checkpointed := range []bool{false, true} {
		for _, cut := range []struct {
			name, point string
			occurrence  int
		}{{"captured-power", "after-step-evidence", 1}, {"applied-power", "after-ledger-cas", 1}, {"applied-settings", "after-ledger-cas", 2}, {"reconciling", "after-reconciling", 1}} {
			name := "legacy/"
			if checkpointed {
				name = "checkpoint/"
			}
			t.Run(name+cut.name, func(t *testing.T) {
				transition, _, _ := v2TransitionFixture(t, nil)
				oldRegistry, oldDigest := transition.registry, transition.registryDigest
				ledger := BaselineLedgerV2(oldRegistry)
				path, err := oldRegistry.PendingPath(ledger)
				if err != nil {
					t.Fatal(err)
				}
				for _, migration := range path {
					ledger, err = ledger.Advance(oldRegistry, migration)
					if err != nil {
						t.Fatal(err)
					}
				}
				projection, _, err := MarshalLedgerV2(ledger, oldRegistry)
				if err != nil {
					t.Fatal(err)
				}
				missing, err := transition.store.ReadLedger()
				if err != nil {
					t.Fatal(err)
				}
				if err := transition.store.CompareAndSwapLedger(missing, projection); err != nil {
					t.Fatal(err)
				}
				prior, err := transition.store.ReadMigrationLedger(oldRegistry, oldDigest)
				if err != nil {
					t.Fatal(err)
				}
				payload, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
				if err != nil {
					t.Fatal(err)
				}
				registry, digest, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog())
				if err != nil {
					t.Fatal(err)
				}
				if checkpointed {
					if err := transition.store.ConvertMigrationLedger(prior, oldRegistry, oldDigest, registry, digest); err != nil {
						t.Fatal(err)
					}
				}
				options := transition.options
				options.RegistryPayload = payload
				options.MigrationCheckpoint = checkpointed
				options.SourceMigrationCheckpoint = true
				seen := 0
				options.fault = func(point string) error {
					if point == cut.point {
						seen++
						if seen == cut.occurrence {
							return errors.New("stop at durable cut")
						}
					}
					return nil
				}
				transition, err = NewV2Transition(options)
				if err != nil {
					t.Fatal(err)
				}
				goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
				inspection, err := transition.Inspect(context.Background(), goal)
				if err != nil || len(inspection.Blockers) != 0 {
					t.Fatalf("inspect: %+v %v", inspection, err)
				}
				outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
				if err != nil || outcome.Status != StatusRecovering {
					t.Fatalf("durable cut: %+v %v", outcome, err)
				}
				state, err := transition.store.ReadMigrationLedger(registry, digest)
				if err != nil {
					t.Fatal(err)
				}
				if state.Ledger.Domains["owner-registration"].Epoch != 2 || state.Ledger.Domains["settings"].Epoch < 2 {
					t.Fatal("completed independent domains reopened")
				}
				if checkpointed && !bytes.Equal(state.ProjectionSnapshot.Payload, projection) {
					t.Fatal("frozen projection changed")
				}
				// A newly constructed transition must recover from durable state and the
				// same approved bindings, without relying on its predecessor's memory.
				options.fault = nil
				fresh, err := NewV2Transition(options)
				if err != nil {
					t.Fatal(err)
				}
				resume, err := fresh.Inspect(context.Background(), goal)
				if err != nil || resume.Resume == nil || len(resume.Blockers) != 0 {
					t.Fatalf("resume: %+v %v", resume, err)
				}
				outcome, err = fresh.Converge(context.Background(), Execution{Plan: resume.Plan, Authorization: v2TestAuthorization(resume.Plan)})
				if err != nil || outcome.Status != StatusReady {
					t.Fatalf("resume converge: %+v %v", outcome, err)
				}
			})
		}
	}
}

func TestLedgerTraceRejectsChangedPinsAndUnprovenHistory(t *testing.T) {
	registry := v2TestRegistry(t)
	ledger := BaselineLedgerV2(registry)
	migration := registry.Migrations[0]
	edge, err := nextLedgerEdge(registry, ledger, migration)
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := MarshalLedgerV2(ledger, registry)
	if err != nil {
		t.Fatal(err)
	}
	after, desired, err := MarshalLedgerV2(ledgerFromAuthority(edge.After()), registry)
	if err != nil {
		t.Fatal(err)
	}
	step := JournalStep{ID: migration.ID + ".ledger", Migration: migration.ID, Resource: "ledger." + migration.Domain, Decision: DecisionTransform, Expected: fingerprintPayload(before), Desired: desired}
	for _, mutate := range []func(*JournalStep){func(s *JournalStep) { s.Expected = digestB }, func(s *JournalStep) { s.Desired = digestB }, func(s *JournalStep) { s.Migration = "foreign"; s.ID = "foreign.ledger" }} {
		altered := step
		mutate(&altered)
		if _, err := ledgerTrace(registry, ledger, []JournalStep{altered}); err == nil {
			t.Fatal("altered trace accepted")
		}
	}
	transition := &V2Transition{registry: registry}
	work := ledgerMigrationWork(PlannerStepIntent{}, migration, edge, authorityLedgerRevision{protectedSnapshotFromPayload(before)}, after)
	if err := transition.validateLedgerWork(work, step); err != nil {
		t.Fatal(err)
	}
	work.ledgerRecipe().to = append([]byte(nil), after...)
	work.ledgerRecipe().to[0] = 'x'
	if err := transition.validateLedgerWork(work, step); err == nil {
		t.Fatal("changed edge recipe accepted")
	}
}

func TestHistoryAdaptersRejectZeroRegistryAndForeignEdge(t *testing.T) {
	if _, err := ParseMigrationLedger(absentProtectedSnapshot(), absentProtectedSnapshot(), RegistryV2{}, digestA); err == nil {
		t.Fatal("absent snapshots admitted a zero registry")
	}
	registry := v2TestRegistry(t)
	foreign := registry
	foreign.Migrations = append([]MigrationDefinitionV2(nil), registry.Migrations...)
	foreign.Migrations[0].Kind = "foreign-kind"
	before := BaselineLedgerV2(registry)
	edge, err := nextLedgerEdge(foreign, before, foreign.Migrations[0])
	if err != nil {
		t.Fatal(err)
	}
	from, _, err := MarshalLedgerV2(before, registry)
	if err != nil {
		t.Fatal(err)
	}
	to, desired, err := MarshalLedgerV2(ledgerFromAuthority(edge.After()), registry)
	if err != nil {
		t.Fatal(err)
	}
	migration := edge.Migration()
	step := JournalStep{ID: migration.ID + ".ledger", Migration: migration.ID, Resource: "ledger." + migration.Domain, Decision: DecisionTransform, Expected: fingerprintPayload(from), Desired: desired}
	work := ledgerMigrationWork(PlannerStepIntent{}, migration, edge, authorityLedgerRevision{protectedSnapshotFromPayload(from)}, to)
	transition := &V2Transition{registry: registry}
	if err := transition.validateLedgerWork(work, step); err == nil {
		t.Fatal("foreign edge admitted")
	}
}

func TestLedgerWorkRejectsReorderedForeignRegistry(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	registry, _, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	foreign := registry
	foreign.Migrations = append([]MigrationDefinitionV2(nil), registry.Migrations...)
	foreign.Migrations[0], foreign.Migrations[1] = foreign.Migrations[1], foreign.Migrations[0]
	before := BaselineLedgerV2(registry)
	migration := foreign.Migrations[0]
	edge, err := nextLedgerEdge(foreign, before, migration)
	if err != nil {
		t.Fatal(err)
	}
	from, _, err := MarshalLedgerV2(before, registry)
	if err != nil {
		t.Fatal(err)
	}
	to, desired, err := MarshalLedgerV2(ledgerFromAuthority(edge.After()), registry)
	if err != nil {
		t.Fatal(err)
	}
	step := JournalStep{ID: migration.ID + ".ledger", Migration: migration.ID, Resource: "ledger." + migration.Domain, Decision: DecisionTransform, Expected: fingerprintPayload(from), Desired: desired}
	work := ledgerMigrationWork(PlannerStepIntent{}, migration, edge, authorityLedgerRevision{protectedSnapshotFromPayload(from)}, to)
	transition := &V2Transition{registry: registry}
	if err := transition.validateLedgerWork(work, step); err == nil {
		t.Fatal("foreign registry with reordered independent definitions admitted its different canonical next edge")
	}
}
