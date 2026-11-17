package releasetransition

import (
	"errors"
	"testing"
)

func TestRollbackAbortArchiveResumesBeforePublishingClosure(t *testing.T) {
	store, selected := abortCutFixture(t, 1, "")
	cut, err := store.prepareRollbackAbortCut(selected)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(selected.Payload)
	if err != nil {
		t.Fatal(err)
	}
	authorization := PlanToken("plan-v1-" + string(digestA))
	destination := append(append([]string{}, rollbackAbortArchiveParts...), string(journal.Transaction))
	interrupted := errors.New("interrupted archive copy")
	store.fault = func(point string) error {
		if point == "after-rollback-abort-artifact-copy" {
			return interrupted
		}
		return nil
	}
	if err := store.preserveRollbackAbortArchive(cut, authorization); !errors.Is(err, interrupted) {
		t.Fatalf("archive crash = %v", err)
	}
	receipt, err := store.readRecord(destination, "archive.json")
	if err != nil || receipt.Exists {
		t.Fatalf("partial archive published closure: %v", err)
	}
	fresh, err := NewPOSIXV2Store(store.configHome)
	if err != nil {
		t.Fatal(err)
	}
	cut, err = fresh.prepareRollbackAbortCut(selected)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := fresh.preserveRollbackAbortArchive(cut, authorization); err != nil {
			t.Fatal(err)
		}
	}
	receipt, err = fresh.readRecord(destination, "archive.json")
	if err != nil || !receipt.Exists {
		t.Fatalf("verified archive has no closure: %v", err)
	}
	var archive rollbackAbortArchive
	if err := decodeBoundedRecord(receipt.Payload, MaxProtectedRecordBytes, &archive); err != nil {
		t.Fatal(err)
	}
	if archive.Authorization != authorization || archive.Checkpoint != cut.history.AuthoritySnapshot.Fingerprint || archive.Projection != cut.history.ProjectionSnapshot.Fingerprint ||
		!sameProtectedSnapshot(protectedSnapshotFromPayload(archive.Journal), selected) {
		t.Fatal("closure does not identify its assessed cut")
	}
	if err := fresh.preserveRollbackAbortArchive(cut, PlanToken("plan-v1-"+string(digestB))); !errors.Is(err, ErrProtectedStoreStale) {
		t.Fatalf("another authorization replaced the closure: %v", err)
	}
	actual, err := fresh.ReadCurrentJournal()
	if err != nil || !sameProtectedSnapshot(actual, selected) {
		t.Fatal("archive publication changed the selected journal")
	}
	if err := fresh.checkRollbackArtifactInventory(journal.Transaction, archive.Artifacts); err != nil {
		t.Fatal(err)
	}
}
