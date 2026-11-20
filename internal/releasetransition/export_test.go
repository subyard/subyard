package releasetransition

import "testing"

// SeedProcessCrashHistoryForTest seeds a complete tag-produced installation,
// including its journal/evidence, plus an unreferenced closed recovery to collect.
func SeedProcessCrashHistoryForTest(t *testing.T, home, version string, checkpointed bool) {
	t.Helper()
	seedCompletedInstallationFixture(t, home, version, checkpointed)
	seedUnreferencedClosedHistory(t, home)
}

func AssertProcessCrashCleanupForTest(t *testing.T, home string, requirePredecessor bool) {
	t.Helper()
	assertCompletedHistoryCleanup(t, home, requirePredecessor)
}

// SetProcessCrashFaultForTest attaches only existing fault seams. This file is
// absent from production builds; it creates no execution authority.
func SetProcessCrashFaultForTest(transition *V2Transition, links *RuntimeLinkStore, fault func(string, string) error) {
	transition.options.fault = func(point string) error { return fault("transition", point) }
	transition.store.fault = func(point string) error { return fault("store", point) }
	links.fault = func(point string) error { return fault("links", point) }
}
