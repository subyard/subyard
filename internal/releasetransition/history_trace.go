package releasetransition

import (
	"github.com/Subyard/Subyard/internal/migrationhistory"
)

// ledgerTrace binds a semantic trace to the frozen ledger codec and exact
// journal pins. The observed ledger is validated authority, never a bare hash.
func ledgerTrace(registry RegistryV2, observed LedgerV2, steps []JournalStep) (map[string]sourceLedgerEdge, error) {
	owner, authority, err := registry.historyAuthority(observed)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, step := range steps {
		if step.ID == step.Migration+".ledger" {
			ids = append(ids, step.Migration)
		}
	}
	trace, err := owner.TraceFrom(authority, ids)
	if err != nil {
		return nil, invalid("journaled ledger trace cannot be reconstructed: %v", err)
	}
	return encodeLedgerTrace(registry, trace, steps)
}

// canonicalLedgerTrace preserves closed retained records whose endpoint predates
// today's authority. It uses the same semantic trace owner and exact byte pins.
func canonicalLedgerTrace(registry RegistryV2, steps []JournalStep) (map[string]sourceLedgerEdge, error) {
	owner, err := registry.historyRegistry()
	if err != nil {
		return nil, err
	}
	baseline, err := owner.Baseline()
	if err != nil {
		return nil, err
	}
	history := registry.historyData()
	ids := make([]string, len(history.Migrations))
	for i, migration := range history.Migrations {
		ids[i] = migration.ID
	}
	trace, err := owner.TraceFrom(baseline, ids)
	if err != nil {
		return nil, invalid("%v", err)
	}
	return encodeLedgerTrace(registry, trace, steps)
}
func encodeLedgerTrace(registry RegistryV2, trace migrationhistory.Trace, steps []JournalStep) (map[string]sourceLedgerEdge, error) {
	edges := map[string]sourceLedgerEdge{}
	for _, edge := range trace.Edges() {
		before := ledgerFromAuthority(edge.Before())
		after := ledgerFromAuthority(edge.After())
		beforeBytes, beforePin, err := MarshalLedgerV2(before, registry)
		if err != nil {
			return nil, err
		}
		afterBytes, afterPin, err := MarshalLedgerV2(after, registry)
		if err != nil {
			return nil, err
		}
		edges[edge.Migration().ID] = sourceLedgerEdge{migration: edge.Migration(), before: beforePin, after: afterPin, witness: edge, from: protectedSnapshotFromPayload(beforeBytes), to: afterBytes}
	}
	for _, step := range steps {
		if step.ID != step.Migration+".ledger" {
			continue
		}
		edge, ok := edges[step.Migration]
		if !ok {
			return nil, invalid("journaled ledger edge is missing")
		}
		initialAbsent := sameLedgerV2(ledgerFromAuthority(edge.witness.Before()), BaselineLedgerV2(registry.historyData())) && step.Expected == absentProtectedSnapshot().Fingerprint
		if step.Desired != edge.after || step.Expected != edge.before && !initialAbsent {
			return nil, invalid("journaled ledger transition does not bind its historical frontier")
		}
		if initialAbsent {
			edge.from = absentProtectedSnapshot()
			edges[step.Migration] = edge
		}
	}
	return edges, nil
}

func nextLedgerEdge(registry RegistryV2, ledger LedgerV2, migration MigrationDefinitionV2) (migrationhistory.Edge, error) {
	owner, authority, err := registry.historyAuthority(ledger)
	if err != nil {
		return migrationhistory.Edge{}, err
	}
	edge, err := owner.Next(authority)
	if err != nil || !sameMigrationDefinition(edge.Migration(), migration) {
		return migrationhistory.Edge{}, invalid("migration %q is not the exact next ledger transition", migration.ID)
	}
	return edge, nil
}
