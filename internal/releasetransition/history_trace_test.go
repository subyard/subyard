package releasetransition

import (
	"testing"
)

func TestV2HistoricalDomainFrontierResumesAtDurableCuts(t *testing.T) {
	registry := v2TestRegistry(t)
	ledger := BaselineLedgerV2(registry)
	var steps []JournalStep
	var frontiers []LedgerV2
	for _, migration := range registry.Migrations {
		frontiers = append(frontiers, ledger)
		before, _, err := MarshalLedgerV2(ledger, registry)
		if err != nil {
			t.Fatal(err)
		}
		next, err := ledger.Advance(registry, migration)
		if err != nil {
			t.Fatal(err)
		}
		_, after, err := MarshalLedgerV2(next, registry)
		if err != nil {
			t.Fatal(err)
		}
		steps = append(steps, JournalStep{ID: migration.ID + ".ledger", Migration: migration.ID, Resource: "ledger." + migration.Domain, Decision: DecisionTransform, Expected: fingerprintPayload(before), Desired: after})
		ledger = next
	}
	frontiers = append(frontiers, ledger)
	for _, frontier := range frontiers {
		if _, err := ledgerTrace(registry, frontier, steps); err != nil {
			t.Fatal(err)
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
	_, desired, err := MarshalLedgerV2(ledgerFromAuthority(edge.After()), registry)
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
}

func TestHistoryAdaptersRejectZeroRegistryAndForeignEdge(t *testing.T) {
	if _, err := ParseMigrationLedger(absentProtectedSnapshot(), absentProtectedSnapshot(), RegistryV2{}, digestA); err == nil {
		t.Fatal("zero registry accepted")
	}
	registry := v2TestRegistry(t)
	foreign := registry.Migrations[0]
	foreign.Kind = "foreign-kind"
	if _, err := nextLedgerEdge(registry, BaselineLedgerV2(registry), foreign); err == nil {
		t.Fatal("foreign definition accepted")
	}
}
