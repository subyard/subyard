package releasetransition

import (
	"bytes"

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
	ids := make([]string, len(registry.Migrations))
	for i, migration := range registry.Migrations {
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
		initialAbsent := sameLedgerV2(ledgerFromAuthority(edge.witness.Before()), BaselineLedgerV2(registry)) && step.Expected == absentProtectedSnapshot().Fingerprint
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

func (transition *V2Transition) validateLedgerWork(work v2Work, step JournalStep) error {
	if transition == nil {
		return invalid("ledger work has no transition owner")
	}
	if work.kind() != v2LedgerWork {
		return invalid("ledger work recipe is missing or typed nil")
	}
	owner, err := transition.registry.historyRegistry()
	if err != nil {
		return err
	}
	if err := owner.CheckEdgeBinding(work.ledgerRecipe().edge); err != nil {
		return invalid("%v", err)
	}
	migration := work.ledgerRecipe().edge.Migration()
	canonical, known := transition.migration(migration.ID)
	if !known || !sameMigrationDefinition(migration, canonical) {
		return invalid("ledger work edge belongs to a foreign migration registry")
	}
	if !sameMigrationDefinition(migration, work.migration) || step.Migration != migration.ID || step.ID != migration.ID+".ledger" || step.Resource != "ledger."+migration.Domain || step.Decision != DecisionTransform {
		return invalid("ledger work differs from its validated edge")
	}
	before := ledgerFromAuthority(work.ledgerRecipe().edge.Before())
	after := ledgerFromAuthority(work.ledgerRecipe().edge.After())
	beforeBytes, _, err := MarshalLedgerV2(before, transition.registry)
	if err != nil {
		return err
	}
	afterBytes, afterPin, err := MarshalLedgerV2(after, transition.registry)
	if err != nil {
		return err
	}
	if !validProtectedSnapshot(work.ledgerRecipe().from.snapshot) || work.ledgerRecipe().from.snapshot.Fingerprint != step.Expected || afterPin != step.Desired || !bytes.Equal(work.ledgerRecipe().to, afterBytes) {
		return invalid("ledger work recipe differs from its edge or journal pins")
	}
	if work.ledgerRecipe().from.snapshot.Exists {
		if !bytes.Equal(work.ledgerRecipe().from.snapshot.Payload, beforeBytes) {
			return invalid("ledger work before-image differs from its edge")
		}
	} else if !sameLedgerV2(before, BaselineLedgerV2(transition.registry)) {
		return invalid("absent ledger work is not a baseline frontier")
	}
	return nil
}
