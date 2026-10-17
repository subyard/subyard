// Package migrationhistory owns migration history semantics. It has no storage,
// authorization or runtime-readiness effects. Wire codecs and capability policy
// remain with the adapters that construct its immutable values.
package migrationhistory

import (
	"fmt"
	"maps"
	"slices"

	"github.com/Subyard/Subyard/internal/domain"
)

// Migration retains the frozen registry definition shape.
type Migration struct {
	ID        string   `json:"id"`
	Domain    string   `json:"domain"`
	FromEpoch int      `json:"fromEpoch"`
	ToEpoch   int      `json:"toEpoch"`
	Kind      string   `json:"kind"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

type DomainState struct {
	Epoch   int      `json:"epoch"`
	Applied []string `json:"applied"`
}

type registryData struct {
	minimum, current map[string]int
	retired          map[string]string
	migrations       []Migration
	ids              map[string]int
}
type Registry struct{ data *registryData }

// Authority is a validated per-domain frontier, bound to one registry instance.
// It cannot be constructed from a compatibility projection or a digest.
type Authority struct {
	registry *registryData
	domains  map[string]DomainState
}

// CompatibilityProjection is a pinned reader view, never planning authority.
// The adapter validates its bytes and pin before creating it.
type CompatibilityProjection struct {
	epochs map[string]int
	pin    string
}

// Edge proves one exact enabled domain transition. Next selects the canonical
// next edge; Trace can also prove historical orders of independent domains.
type Edge struct {
	before, after Authority
	migration     Migration
}
type Trace struct {
	initial, terminal Authority
	edges             []Edge
}

// Completion proves only that no one-time migrations remain. It makes no claim
// about activation, resource freshness, authorization or runtime readiness.
type Completion struct{ terminal Authority }

func NewRegistry(minimum, current map[string]int, migrations []Migration, retired map[string]string) (Registry, error) {
	if len(minimum) == 0 || len(minimum) != len(current) {
		return Registry{}, fmt.Errorf("registry domain epochs are required and must match")
	}
	data := &registryData{minimum: maps.Clone(minimum), current: maps.Clone(current), retired: maps.Clone(retired), ids: map[string]int{}}
	expected := maps.Clone(minimum)
	for name, pin := range retired {
		if _, ok := minimum[name]; !ok || !validPin(pin) {
			return Registry{}, fmt.Errorf("invalid retired migration prefix binding")
		}
	}
	for name, floor := range minimum {
		if !safeID(name) || floor < 1 || current[name] < floor {
			return Registry{}, fmt.Errorf("registry domain %q has invalid epoch range", name)
		}
	}
	for name := range current {
		if _, ok := minimum[name]; !ok {
			return Registry{}, fmt.Errorf("registry has unknown domain %q", name)
		}
	}
	for i, m := range migrations {
		if !safeID(m.ID) || !safeID(m.Kind) {
			return Registry{}, fmt.Errorf("migration ID or kind is not a safe ID")
		}
		if _, ok := data.ids[m.ID]; ok {
			return Registry{}, fmt.Errorf("duplicate migration ID %q", m.ID)
		}
		if _, ok := minimum[m.Domain]; !ok {
			return Registry{}, fmt.Errorf("migration %q has unknown domain %q", m.ID, m.Domain)
		}
		if m.ToEpoch != m.FromEpoch+1 || m.FromEpoch != expected[m.Domain] {
			return Registry{}, fmt.Errorf("migration %q has a gap or does not advance exactly one epoch", m.ID)
		}
		seen := map[string]bool{}
		for _, dep := range m.DependsOn {
			if !safeID(dep) || seen[dep] {
				return Registry{}, fmt.Errorf("migration %q has invalid or duplicate dependency %q", m.ID, dep)
			}
			if _, ok := data.ids[dep]; !ok {
				return Registry{}, fmt.Errorf("migration %q dependency %q is unknown, cyclic or not ordered before it", m.ID, dep)
			}
			seen[dep] = true
		}
		expected[m.Domain] = m.ToEpoch
		data.ids[m.ID] = i
		data.migrations = append(data.migrations, cloneMigration(m))
	}
	if !maps.Equal(expected, current) {
		return Registry{}, fmt.Errorf("registry path does not end at current epochs")
	}
	return Registry{data}, nil
}
func safeID(id string) bool                { return len(id) <= 128 && domain.SafeID(id) }
func cloneMigration(m Migration) Migration { m.DependsOn = slices.Clone(m.DependsOn); return m }
func cloneDomains(source map[string]DomainState) map[string]DomainState {
	out := make(map[string]DomainState, len(source))
	for name, state := range source {
		state.Applied = slices.Clone(state.Applied)
		out[name] = state
	}
	return out
}
func (r Registry) History(domains map[string]DomainState) (Authority, error) {
	if r.data == nil {
		return Authority{}, fmt.Errorf("uninitialized migration registry")
	}
	if domains == nil || len(domains) != len(r.data.minimum) {
		return Authority{}, fmt.Errorf("ledger domains do not exactly match registry domains")
	}
	completed := map[string]bool{}
	for name, state := range domains {
		floor, ok := r.data.minimum[name]
		if !ok || state.Epoch < floor || state.Epoch > r.data.current[name] {
			return Authority{}, fmt.Errorf("ledger domain %q epoch %d is outside supported range", name, state.Epoch)
		}
		expected, _ := r.Applied(name, floor, state.Epoch)
		if !slices.Equal(state.Applied, expected) {
			return Authority{}, fmt.Errorf("ledger domain %q applied history is not the exact registry prefix", name)
		}
		for _, id := range state.Applied {
			completed[id] = true
		}
	}
	for _, m := range r.data.migrations {
		if completed[m.ID] {
			for _, dep := range m.DependsOn {
				if !completed[dep] {
					return Authority{}, fmt.Errorf("ledger migration %q is applied before dependency %q", m.ID, dep)
				}
			}
		}
	}
	return Authority{r.data, cloneDomains(domains)}, nil
}
func (r Registry) owns(a Authority) bool {
	return r.data != nil && a.registry == r.data && a.domains != nil
}
func (r Registry) Baseline() (Authority, error) {
	if r.data == nil {
		return Authority{}, fmt.Errorf("uninitialized migration registry")
	}
	states := map[string]DomainState{}
	for name, floor := range r.data.minimum {
		states[name] = DomainState{Epoch: floor, Applied: []string{}}
	}
	return r.History(states)
}
func (a Authority) Domains() map[string]DomainState { return cloneDomains(a.domains) }
func (r Registry) Pending(a Authority) ([]Migration, error) {
	if !r.owns(a) {
		return nil, fmt.Errorf("zero or foreign migration authority")
	}
	pending := []Migration{}
	for _, m := range r.data.migrations {
		if a.domains[m.Domain].Epoch < m.ToEpoch {
			pending = append(pending, cloneMigration(m))
		}
	}
	return pending, nil
}
func (r Registry) Applied(name string, after, through int) ([]string, error) {
	if r.data == nil {
		return nil, fmt.Errorf("uninitialized migration registry")
	}
	floor, ok := r.data.minimum[name]
	if !ok || after < floor || through < after || through > r.data.current[name] {
		return nil, fmt.Errorf("migration prefix is outside the registry path")
	}
	var out []string
	for _, m := range r.data.migrations {
		if m.Domain == name && m.FromEpoch >= after && m.ToEpoch <= through {
			out = append(out, m.ID)
		}
	}
	return out, nil
}
func (r Registry) Path(name string, from int) ([]Migration, error) {
	if r.data == nil {
		return nil, fmt.Errorf("uninitialized migration registry")
	}
	if _, err := r.Applied(name, from, r.data.current[name]); err != nil {
		return nil, err
	}
	out := []Migration{}
	for _, m := range r.data.migrations {
		if m.Domain == name && m.FromEpoch >= from {
			out = append(out, cloneMigration(m))
		}
	}
	return out, nil
}
func (r Registry) Next(a Authority) (Edge, error) {
	pending, err := r.Pending(a)
	if err != nil {
		return Edge{}, err
	}
	if len(pending) == 0 {
		return Edge{}, fmt.Errorf("migration history is complete")
	}
	return r.advance(a, pending[0])
}
func (r Registry) advance(a Authority, m Migration) (Edge, error) {
	if !r.owns(a) || a.domains[m.Domain].Epoch != m.FromEpoch {
		return Edge{}, fmt.Errorf("migration is not the next domain transition")
	}
	states := a.Domains()
	state := states[m.Domain]
	state.Epoch = m.ToEpoch
	state.Applied = append(state.Applied, m.ID)
	states[m.Domain] = state
	after, err := r.History(states)
	if err != nil {
		return Edge{}, err
	}
	return Edge{a, after, cloneMigration(m)}, nil
}
func (e Edge) Before() Authority    { return e.before }
func (e Edge) After() Authority     { return e.after }
func (e Edge) Migration() Migration { return cloneMigration(e.migration) }

// CheckEdge rejects zero and foreign handles before an adapter performs effects.
func (r Registry) CheckEdge(e Edge) error {
	if !r.owns(e.before) || !r.owns(e.after) || e.migration.ID == "" {
		return fmt.Errorf("zero or foreign migration edge")
	}
	return nil
}

// TraceFrom reconstructs the exact journal path from an authenticated observed
// frontier, which can be at any cut of that path. No hashes are enumerated and
// registry-wide prefixes are never substituted for historical domain frontiers.
func (r Registry) TraceFrom(observed Authority, ids []string) (Trace, error) {
	if !r.owns(observed) {
		return Trace{}, fmt.Errorf("zero or foreign migration authority")
	}
	seen := map[string]bool{}
	cut := len(ids)
	for i, id := range ids {
		index, ok := r.data.ids[id]
		if !ok || seen[id] {
			return Trace{}, fmt.Errorf("trace has unknown or duplicate migration %q", id)
		}
		seen[id] = true
		m := r.data.migrations[index]
		if observed.domains[m.Domain].Epoch < m.ToEpoch {
			if cut == len(ids) {
				cut = i
			}
		} else if cut != len(ids) {
			return Trace{}, fmt.Errorf("observed frontier is not a trace cut")
		}
	}
	initial := observed
	for i := cut - 1; i >= 0; i-- {
		m := r.data.migrations[r.data.ids[ids[i]]]
		states := initial.Domains()
		state := states[m.Domain]
		if state.Epoch != m.ToEpoch || len(state.Applied) == 0 || state.Applied[len(state.Applied)-1] != m.ID {
			return Trace{}, fmt.Errorf("trace is not the exact historical domain prefix")
		}
		state.Epoch = m.FromEpoch
		state.Applied = slices.Clone(state.Applied[:len(state.Applied)-1])
		if state.Applied == nil {
			state.Applied = []string{}
		}
		states[m.Domain] = state
		var err error
		initial, err = r.History(states)
		if err != nil {
			return Trace{}, err
		}
	}
	trace := Trace{initial: initial}
	cursor := initial
	for index, id := range ids {
		var edge Edge
		var err error
		if index < cut {
			edge, err = r.advance(cursor, r.data.migrations[r.data.ids[id]])
		} else {
			edge, err = r.Next(cursor)
			if err == nil && edge.migration.ID != id {
				return Trace{}, fmt.Errorf("pending trace migration %q is not the canonical next transition", id)
			}
		}
		if err != nil {
			return Trace{}, err
		}
		trace.edges = append(trace.edges, edge)
		cursor = edge.after
	}
	trace.terminal = cursor
	return trace, nil
}
func (t Trace) Edges() []Edge       { return slices.Clone(t.edges) }
func (t Trace) Initial() Authority  { return t.initial }
func (t Trace) Terminal() Authority { return t.terminal }
func (r Registry) Complete(a Authority) (Completion, error) {
	pending, err := r.Pending(a)
	if err != nil {
		return Completion{}, err
	}
	if len(pending) != 0 {
		return Completion{}, fmt.Errorf("migration history is incomplete")
	}
	return Completion{a}, nil
}
func NewCompatibilityProjection(epochs map[string]int, pin string) (CompatibilityProjection, error) {
	if epochs == nil || !validPin(pin) {
		return CompatibilityProjection{}, fmt.Errorf("invalid compatibility projection")
	}
	for name, epoch := range epochs {
		if !safeID(name) || epoch < 1 {
			return CompatibilityProjection{}, fmt.Errorf("invalid compatibility projection epoch")
		}
	}
	return CompatibilityProjection{maps.Clone(epochs), pin}, nil
}
func (p CompatibilityProjection) Epochs() map[string]int { return maps.Clone(p.epochs) }
func (p CompatibilityProjection) Pin() string            { return p.pin }

func (e Edge) Validate() error { return (Registry{e.before.registry}).CheckEdge(e) }

// CheckEdgeBinding accepts an immutable edge only when its complete semantic
// registry matches this reader, including order, floors and retired prefix pins.
// Adapter reconstruction creates fresh handles, so pointer identity alone is
// insufficient here. This does not constrain completed historical edge order.
func (r Registry) CheckEdgeBinding(e Edge) error {
	if err := e.Validate(); err != nil {
		return err
	}
	other := e.before.registry
	if r.data == nil || !maps.Equal(r.data.minimum, other.minimum) || !maps.Equal(r.data.current, other.current) || !maps.Equal(r.data.retired, other.retired) || len(r.data.migrations) != len(other.migrations) {
		return fmt.Errorf("foreign migration registry binding")
	}
	for i, m := range r.data.migrations {
		n := other.migrations[i]
		if m.ID != n.ID || m.Domain != n.Domain || m.Kind != n.Kind || m.FromEpoch != n.FromEpoch || m.ToEpoch != n.ToEpoch || !slices.Equal(m.DependsOn, n.DependsOn) {
			return fmt.Errorf("foreign migration registry binding")
		}
	}
	return nil
}

func (r Registry) CheckCompletion(completion Completion) error {
	pending, err := r.Pending(completion.terminal)
	if err != nil {
		return err
	}
	if len(pending) != 0 {
		return fmt.Errorf("migration completion is incomplete")
	}
	return nil
}

func validPin(pin string) bool {
	if len(pin) != 64 {
		return false
	}
	for _, c := range pin {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
