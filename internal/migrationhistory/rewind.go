package migrationhistory

import (
	"fmt"
	"maps"
	"slices"
)

// ReverseEdge proves removal of one terminal domain edge while preserving
// dependency closure. It proves history semantics, not that effects were undone.
type ReverseEdge struct {
	before, after Authority
	migration     Migration
}

func (e ReverseEdge) Before() Authority    { return e.before }
func (e ReverseEdge) After() Authority     { return e.after }
func (e ReverseEdge) Migration() Migration { return cloneMigration(e.migration) }

func (r Registry) Rewind(a Authority, id string) (ReverseEdge, error) {
	if !r.owns(a) {
		return ReverseEdge{}, fmt.Errorf("zero or foreign migration authority")
	}
	index, exists := r.data.ids[id]
	if !exists {
		return ReverseEdge{}, fmt.Errorf("unknown rewind migration %q", id)
	}
	migration := r.data.migrations[index]
	states := a.Domains()
	state := states[migration.Domain]
	if state.Epoch != migration.ToEpoch || len(state.Applied) == 0 || state.Applied[len(state.Applied)-1] != id {
		return ReverseEdge{}, fmt.Errorf("rewind migration %q is not the terminal domain edge", id)
	}
	state.Epoch = migration.FromEpoch
	state.Applied = slices.Clone(state.Applied[:len(state.Applied)-1])
	if state.Applied == nil {
		state.Applied = []string{}
	}
	states[migration.Domain] = state
	after, err := r.History(states)
	if err != nil {
		return ReverseEdge{}, err
	}
	return ReverseEdge{a, after, cloneMigration(migration)}, nil
}

func (r Registry) CheckReverseEdgeBinding(edge ReverseEdge) error {
	// The inverse handle carries the same immutable registry binding as its
	// corresponding forward edge. Neither direction grants storage authority.
	return r.CheckEdgeBinding(Edge{before: edge.after, after: edge.before, migration: edge.migration})
}

// RewindPath selects terminal edges in reverse registry/dependency order. All
// target epochs must be supplied; new-domain omission belongs to the adapter.
func (r Registry) RewindPath(a Authority, epochs map[string]int) ([]ReverseEdge, error) {
	if !r.owns(a) || len(epochs) != len(r.data.minimum) {
		return nil, fmt.Errorf("rewind target does not match migration domains")
	}
	states := a.Domains()
	for name, state := range states {
		epoch, exists := epochs[name]
		if !exists || epoch > state.Epoch {
			return nil, fmt.Errorf("rewind target advances or omits domain %q", name)
		}
		applied, err := r.Applied(name, r.data.minimum[name], epoch)
		if err != nil {
			return nil, err
		}
		states[name] = DomainState{Epoch: epoch, Applied: applied}
	}
	if _, err := r.History(states); err != nil {
		return nil, err
	}
	cursor := a
	var edges []ReverseEdge
	for index := len(r.data.migrations) - 1; index >= 0; index-- {
		migration := r.data.migrations[index]
		if cursor.domains[migration.Domain].Epoch < migration.ToEpoch || migration.ToEpoch <= epochs[migration.Domain] {
			continue
		}
		edge, err := r.Rewind(cursor, migration.ID)
		if err != nil {
			return nil, err
		}
		edges = append(edges, edge)
		cursor = edge.after
	}
	actual := map[string]int{}
	for name, state := range cursor.domains {
		actual[name] = state.Epoch
	}
	if !maps.Equal(actual, epochs) {
		return nil, fmt.Errorf("rewind did not reach the exact target")
	}
	return edges, nil
}
