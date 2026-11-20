package releasetransition

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func seedCompletedInstallationFixture(t *testing.T, home, version string, checkpointed bool) JournalRecord {
	t.Helper()
	shape := "plain"
	if checkpointed {
		shape = "checkpointed"
	}
	files := completedFixtureFiles(t, "v"+version+"-completed-install-"+shape+".json")
	for name, content := range files {
		writeCompletedFixtureFile(t, home, name, content)
	}
	store, err := NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	registry, digest := completedFixtureRegistry(t, version)
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil || journal.Checkpoint != JournalComplete || len(journal.Steps) != len(registry.Migrations) || journal.Releases.Target != ReleaseID(version+"-aaaaaaaaaaaa") {
		t.Fatalf("incomplete tag installation: %#v %v", journal, err)
	}
	for _, step := range journal.Steps {
		if step.Checkpoint != StepVerified {
			t.Fatal("unfinished tag step")
		}
	}
	metadata, err := store.InspectSourceMetadata(registry, digest)
	if err != nil || metadata.Ledger.Checkpointed != checkpointed {
		t.Fatalf("invalid completed tag metadata: %#v %v", metadata, err)
	}
	return journal
}

// completedFixtureRegistry is the exact registry the tag engine wrote the
// fixture with: the frozen six-step history before v0.30.0, then its own.
func completedFixtureRegistry(t *testing.T, version string) (RegistryV2, Fingerprint) {
	t.Helper()
	if version != "0.30.0" {
		return historyV028, historyV028Digest
	}
	payload, err := os.ReadFile(filepath.Join("testdata", "v0.30.0-release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	registry, digest, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	return registry, digest
}

func completedFixtureFiles(t *testing.T, name string) map[string]string {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var files map[string]string
	if err := json.Unmarshal(payload, &files); err != nil {
		t.Fatal(err)
	}
	return files
}

func writeCompletedFixtureFile(t *testing.T, home, name, content string) {
	t.Helper()
	path := filepath.Join(home, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte(content), 0600)
}

// This separate, closed older prefix has no runtime or projection reference.
// Its actual typed recovery file must be collected, unlike the tag predecessor.
func seedUnreferencedClosedHistory(t *testing.T, home string) {
	t.Helper()
	for name, content := range completedFixtureFiles(t, "legacy-reader-complete.json") {
		if strings.HasPrefix(name, "release-transition/v2/transactions/tx-test-001/") {
			writeCompletedFixtureFile(t, home, name, content)
		}
	}
}

func assertCompletedHistoryCleanup(t *testing.T, home string, requirePredecessor bool) {
	t.Helper()
	orphan := filepath.Join(home, "release-transition/v2/transactions/tx-test-001")
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("unreferenced closed evidence/recovery was not collected: %v", err)
	}
	store, err := NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	if requirePredecessor {
		payload, err := os.ReadFile("../../config/release-transition.json")
		if err != nil {
			t.Fatal(err)
		}
		registry, _, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.closedTransactionEvidence("tx-completed-install", registry); err != nil {
			t.Fatal("retained tag history lost its closure", err)
		}
	}

}

// Completed v0.28.x and v0.29.0 installations take one ledger-only step to the
// NetworkManager device guard boundary; a completed v0.30.0 installation is
// already there and replays nothing.
func TestCompletedTagInstallationUpdateCleansHistory(t *testing.T) {
	for _, version := range []string{"0.28.0", "0.28.1", "0.29.0", "0.30.0"} {
		for _, checkpointed := range []bool{false, true} {
			shape := "plain"
			if checkpointed {
				shape = "checkpointed"
			}
			t.Run(version+"/"+shape, func(t *testing.T) {
				home := testkit.TempDir(t)
				journal := seedCompletedInstallationFixture(t, home, version, checkpointed)
				seedUnreferencedClosedHistory(t, home)
				links := &ReleaseLinks{Active: journal.Releases.Target}
				options := currentHistoryOptions(t, home, links, "0.30.0-bbbbbbbbbbbb")
				engine, err := NewV2Transition(options)
				if err != nil {
					t.Fatal(err)
				}
				if version == "0.30.0" {
					// The current registry reads the completed v0.30.0 history as is.
					assertCompletedV030Ledger(t, engine)
				}
				for repeat := 0; repeat < 2; repeat++ {
					goal := Goal{Target: options.Releases.Target, Direction: DirectionActivateTarget}
					plan, err := engine.Inspect(context.Background(), goal)
					if err != nil {
						t.Fatal(err)
					}
					out, err := engine.Converge(context.Background(), Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)})
					if err != nil || out.Status != StatusReady || len(out.Warnings) != 0 {
						t.Fatalf("completed-source update: %#v %v", out, err)
					}
					snapshot, err := engine.store.ReadCurrentJournal()
					if err != nil {
						t.Fatal(err)
					}
					current, err := ParseJournal(snapshot.Payload)
					if err != nil || !liveLedgerSteps(current, version != "0.30.0") || current.Checkpoint != JournalComplete {
						t.Fatalf("update replayed history: %#v %v", current, err)
					}
					assertCompletedHistoryCleanup(t, home, true)
					if _, err := engine.store.InspectSourceMetadata(engine.registry, engine.registryDigest); err != nil {
						t.Fatal(err)
					}
					assertCompletedV030Ledger(t, engine)
				}
			})
		}
	}
}

func TestFrozenHistoryStillRequiresRollbackAndCatalogCompatibility(t *testing.T) {
	payload, err := os.ReadFile("../../config/release-transition.json")
	if err != nil {
		t.Fatal(err)
	}
	catalog := BuiltinCapabilityCatalog()
	registry, _, err := ParseRegistryV2(payload, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Migrations) != 1 || registry.Migrations[0].Kind != nmDeviceGuardMigration {
		t.Fatal("fixture must have only the NetworkManager device guard live migration")
	}
	for _, source := range []CapabilityCatalog{PreviousBuiltinCapabilityCatalog(), preRegisteredYardsCapabilityCatalog()} {
		if catalog.SupportsSourceMetadata(source.Digest(), registry) {
			t.Fatal("retired definitions escaped original catalog validation")
		}
	}
	if !catalog.SupportsSourceMetadata(catalog.Digest(), registry) {
		t.Fatal("current catalog rejected frozen history")
	}
	engine := &V2Transition{registry: registry, catalog: catalog, options: V2Options{Direction: DirectionActivatePrevious, RollbackTarget: &RollbackTarget{Version: "0.29.0"}}}
	if engine.rollbackCompatibilityBlocker(true) == nil {
		t.Fatal("retired applied capabilities escaped the legacy rollback policy")
	}
	// A verified registry target is decided by its own ledger reader instead
	// (TestNetworkManagerDeviceGuardLedgerIsUnreadableBelowTheBoundary).
	engine.options.RollbackTarget.RegistryDigest = historyV028Digest
	if engine.rollbackCompatibilityBlocker(true) != nil {
		t.Fatal("verified registry target lost its existing rollback path")
	}
}

// assertCompletedV030Ledger requires the seven v0.30.0 applied IDs at
// power-metadata epoch 4.
func assertCompletedV030Ledger(t *testing.T, engine *V2Transition) {
	t.Helper()
	state, err := engine.store.ReadMigrationLedger(engine.registry, engine.registryDigest)
	if err != nil {
		t.Fatal(err)
	}
	applied := 0
	for _, domain := range state.Ledger.Domains {
		applied += len(domain.Applied)
	}
	power := state.Ledger.Domains["power-metadata"]
	if applied != 7 || power.Epoch != 4 || !slices.Contains(power.Applied, "introduce-nm-device-guard-v4") {
		t.Fatalf("completed v0.30.0 ledger: %#v", state.Ledger)
	}
	if pending, err := engine.registry.PendingPath(state.Ledger); err != nil || len(pending) != 0 {
		t.Fatalf("completed v0.30.0 ledger left pending work: %v %v", pending, err)
	}
}

// liveLedgerSteps reports whether a journal holds exactly the verified live
// ledger-only steps (or none), never a frozen historical one.
func liveLedgerSteps(journal JournalRecord, want bool) bool {
	if !want {
		return len(journal.Steps) == 0
	}
	return len(journal.Steps) == 1 && journal.Steps[0].ID == "introduce-nm-device-guard-v4.ledger" && journal.Steps[0].Checkpoint == StepVerified
}
