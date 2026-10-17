package releasetransition

import (
	"bytes"
	"context"
	"encoding/json"
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
	owner := &powerReaderOwnerRegistration{v2TestOwnerRegistration: &v2TestOwnerRegistration{state: OwnerRegistrationLegacyDirectory}, transition: transition}
	transition.options.OwnerRegistration = owner
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
	if owner.commits == 0 || owner.state != OwnerRegistrationCurrent {
		t.Fatal("effectful legacy owner migration did not complete")
	}
	// Only existing settings migration changes its fixture; the boundary adds no
	// Incus operation or rewrite of canonical desired-power/owning receipt data.
	after, _ := os.ReadFile(settingsPath)
	if len(before) == 0 || len(after) == 0 {
		t.Fatal("existing settings were lost")
	}
}

// Legacy owner commits install power helpers and initialize the replacement
// owner. Those publication writes require the already persisted reader epoch.
type powerReaderOwnerRegistration struct {
	*v2TestOwnerRegistration
	transition *V2Transition
}

func (owner *powerReaderOwnerRegistration) Commit(ctx context.Context, before OwnerRegistrationObservation) error {
	state, err := owner.transition.store.ReadMigrationLedger(owner.transition.registry, owner.transition.registryDigest)
	if err != nil {
		return err
	}
	if state.Ledger.Domains["power-metadata"].Epoch != 2 {
		return errors.New("legacy owner publication preceded its protected reader epoch")
	}
	return owner.v2TestOwnerRegistration.Commit(ctx, before)
}

func TestOwnerPublicationOrderPreservesCompletedHistoricalOwnerPrefix(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	registry, digest, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	// The former cross-domain order completed owner epoch 2 while power stayed
	// at epoch 1. Its exact domain histories and compacted prefixes remain valid.
	historical := registry
	historical.CurrentEpochs = cloneEpochs(registry.CurrentEpochs)
	historical.CurrentEpochs["settings"] = 2
	historical.Migrations = nil
	for _, domain := range []string{"settings", "owner-registration", "power-metadata"} {
		for _, migration := range registry.Migrations {
			if migration.Domain == domain && migration.Kind != localSSHPortMigration {
				historical.Migrations = append(historical.Migrations, migration)
			}
		}
	}
	historicalPayload, err := json.Marshal(historical)
	if err != nil {
		t.Fatal(err)
	}
	historical, historicalDigest, err := ParseRegistryV2(historicalPayload, BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	ledger := BaselineLedgerV2(historical)
	for _, domain := range []string{"settings", "owner-registration"} {
		for _, migration := range historical.Migrations {
			if migration.Domain == domain {
				ledger, err = ledger.Advance(historical, migration)
				if err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	projectionPayload, _, err := MarshalLedgerV2(ledger, historical)
	if err != nil {
		t.Fatalf("historical owner epoch 2 / power epoch 1: %v", err)
	}
	projection := protectedSnapshotFromPayload(projectionPayload)
	epochs := map[string]int{}
	for domain, state := range ledger.Domains {
		epochs[domain] = state.Epoch
	}
	checkpoint, err := newLedgerCheckpointV1(ledger, historical, historicalDigest, projection, epochs)
	if err != nil {
		t.Fatal(err)
	}
	checkpointPayload, _, err := MarshalLedgerCheckpointV1(checkpoint, historical, historicalDigest, projection)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := ParseMigrationLedger(projection, protectedSnapshotFromPayload(checkpointPayload), registry, digest)
	if err != nil {
		t.Fatalf("compacted historical owner prefix: %v", err)
	}
	pending, err := registry.PendingPath(retained.Ledger)
	if err != nil || len(pending) != 2 || pending[0].Domain != "power-metadata" || pending[1].Kind != localSSHPortMigration {
		t.Fatalf("historical remaining work: %+v %v", pending, err)
	}
}

func TestOwnerPublicationRecoveryFromCompletedIndependentDomains(t *testing.T) {
	for _, history := range []string{"settings-v2", "settings-v3"} {
		for _, interruption := range []string{"after-journal-authorized", "after-ledger-cas", "reconcile"} {
			for _, mode := range []string{"legacy", "conversion", "checkpoint"} {
				t.Run(history+"/"+interruption+"/"+mode, func(t *testing.T) {
					ctx := context.Background()
					links := ReleaseLinks{Active: "release-a"}
					transition, _, _ := v2TransitionFixtureWithReleases(t, nil,
						ReleasePair{From: "release-a", Target: "release-b"}, links)
					payload, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
					if err != nil {
						t.Fatal(err)
					}
					transition.registry, transition.registryDigest, err = ParseRegistryV2(payload, transition.catalog)
					if err != nil {
						t.Fatal(err)
					}
					transition.options.RegistryPayload = payload
					transition.options.MigrationCheckpoint = mode != "legacy"
					transition.options.SourceMigrationCheckpoint = mode == "checkpoint"
					transition.options.fault = func(point string) error {
						if point == interruption {
							return errors.New("interrupt independent-domain update")
						}
						return nil
					}
					transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return links, nil }
					transition.options.ActivateLinks = func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
						links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
						return links, nil
					}
					// A predecessor completed settings and owner migrations before the independent
					// power migration was introduced into the middle of the registry order.
					ledger := BaselineLedgerV2(transition.registry)
					for _, migration := range transition.registry.Migrations {
						if migration.Domain == "power-metadata" || history == "settings-v2" && migration.Kind == localSSHPortMigration {
							continue
						}
						domain := ledger.Domains[migration.Domain]
						domain.Epoch = migration.ToEpoch
						domain.Applied = append(domain.Applied, migration.ID)
						ledger.Domains[migration.Domain] = domain
					}
					before, _, err := MarshalLedgerV2(ledger, transition.registry)
					if err != nil {
						t.Fatal(err)
					}
					missing, err := transition.store.ReadLedger()
					if err != nil {
						t.Fatal(err)
					}
					if err := transition.store.CompareAndSwapLedger(missing, before); err != nil {
						t.Fatal(err)
					}
					if mode == "checkpoint" {
						state, err := transition.store.ReadMigrationLedger(transition.registry, transition.registryDigest)
						if err != nil {
							t.Fatal(err)
						}
						if err := transition.store.ConvertMigrationLedger(state, transition.registry, transition.registryDigest, transition.registry, transition.registryDigest); err != nil {
							t.Fatal(err)
						}
					}
					transition.options.Reconcilers = []V2ActivationReconciler{&v2PostErrorReconciler{
						post:         V2ActivationObservation{Actual: digestB, Desired: digestA},
						reconcileErr: v2PublicActivationError{"approved observation changed before effects", "run yard update --check"},
					}}
					goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
					inspection, err := transition.Inspect(ctx, goal)
					if err != nil || len(inspection.Blockers) != 0 {
						t.Fatalf("independent domain inspection: %+v err=%v", inspection, err)
					}
					outcome, err := transition.Converge(ctx, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
					if err != nil {
						t.Fatal(err)
					}
					resume, err := transition.Inspect(ctx, goal)
					if err != nil {
						t.Fatalf("post-failure inspection: %v; execution outcome: %+v", err, outcome)
					}
					if outcome.Status != StatusRecovering || outcome.Active != links.Active ||
						interruption == "reconcile" && outcome.Code != CodeDependencyUnavailable ||
						resume.Resume == nil || len(resume.Blockers) != 0 {
						t.Fatalf("activation failure lost recoverable history: outcome=%+v inspection=%+v", outcome, resume)
					}
					options := transition.options
					options.fault = nil
					options.Reconcilers = []V2ActivationReconciler{&v2TestReconciler{id: "post-error-runtime", converged: true}}
					options.VerifyAuthorization = func(PlanToken, Authorization) bool {
						t.Fatal("unchanged resume requested a fresh grant")
						return false
					}
					fresh, err := NewV2Transition(options)
					if err != nil {
						t.Fatal(err)
					}
					resumed, err := fresh.Inspect(ctx, goal)
					if err != nil || resumed.Resume == nil || resumed.Plan != resume.Plan {
						t.Fatalf("restart lost durable plan: %+v err=%v", resumed, err)
					}
					settled, err := fresh.Converge(ctx, Execution{Plan: resumed.Plan})
					if err != nil || settled.Status != StatusReady || settled.Transaction == nil || *settled.Transaction != *resume.Resume {
						t.Fatalf("unchanged resume: %+v err=%v", settled, err)
					}
					state, err := fresh.store.ReadMigrationLedger(fresh.registry, fresh.registryDigest)
					if err != nil || state.Checkpointed != (mode != "legacy") || state.Ledger.Domains["power-metadata"].Epoch != 2 {
						t.Fatalf("resumed history: %+v err=%v", state, err)
					}
					if mode == "checkpoint" && !bytes.Equal(before, state.ProjectionSnapshot.Payload) {
						t.Fatal("resumed checkpoint update rewrote the frozen legacy projection")
					}
					final, err := fresh.Inspect(ctx, goal)
					if err != nil || final.Outcome == nil || final.Outcome.Status != StatusReady || final.Assessment.Changed {
						t.Fatalf("resumed fixed point: %+v err=%v", final, err)
					}
				})
			}
		}
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

func TestOwnerPublicationOrderRefusesChangedAuthorizedRegistryWithoutMutation(t *testing.T) {
	transition, _, settingsPath := v2TransitionFixture(t, func(point string) error {
		if point == "after-settings-cas" {
			return errors.New("interrupt exact authorized registry")
		}
		return nil
	})
	payload, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	transition.registry, transition.registryDigest, err = ParseRegistryV2(payload, transition.catalog)
	if err != nil {
		t.Fatal(err)
	}
	transition.options.RegistryPayload = payload
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil || len(inspection.Blockers) != 0 {
		t.Fatalf("original inspection: %+v %v", inspection, err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || outcome.Status != StatusRecovering {
		t.Fatalf("authorized interruption: %+v %v", outcome, err)
	}
	journal, err := transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	if !journal.Exists {
		t.Fatal("fixture did not retain an authorized migration journal")
	}
	ledger, err := transition.store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	settings, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	options := transition.options
	// Even semantically identical registry bytes cannot rebind a sealed journal.
	options.RegistryPayload = append(append([]byte(nil), payload...), '\n')
	options.fault = nil
	changed, err := NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	resume, err := changed.Inspect(context.Background(), goal)
	if err != nil || resume.Resume != nil || resume.Plan == inspection.Plan {
		t.Fatalf("changed registry inspection: %+v %v", resume, err)
	}
	// Fresh inspection may offer separately authorized repair. The original
	// pinned plan and its consent must never transfer to changed registry bytes.
	outcome, err = changed.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || outcome.Code != CodePlanStale {
		t.Fatalf("changed registry convergence: %+v %v", outcome, err)
	}
	afterJournal, err := transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	afterLedger, err := transition.store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	afterSettings, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(journal.Payload, afterJournal.Payload) || !bytes.Equal(ledger.Payload, afterLedger.Payload) || !bytes.Equal(settings, afterSettings) {
		t.Fatal("changed registry mutated authorized state")
	}
}
