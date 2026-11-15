package releasetransition

import "testing"

// SeedProcessCrashHistoryForTest supplies completed v0.28-shaped authority,
// without calling any retired effect writer or seeding an in-flight journal.
func SeedProcessCrashHistoryForTest(t *testing.T, home string, checkpointed bool) {
	t.Helper()
	seedFrozenReaderTree(t, home, "v0.28.1-first-transition-verified.json", false)
	if !checkpointed {
		return
	}
	store, err := NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := store.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	state, err := store.ReadMigrationLedger(historyV028, historyV028Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConvertMigrationLedger(state, historyV028, historyV028Digest, historyV028, historyV028Digest); err != nil {
		t.Fatal(err)
	}
}

// SetProcessCrashFaultForTest attaches only existing fault seams. This file is
// absent from production builds; it creates no execution authority.
func SetProcessCrashFaultForTest(transition *V2Transition, links *RuntimeLinkStore, fault func(string, string) error) {
	transition.options.fault = func(point string) error { return fault("transition", point) }
	transition.store.fault = func(point string) error { return fault("store", point) }
	links.fault = func(point string) error { return fault("links", point) }
}
