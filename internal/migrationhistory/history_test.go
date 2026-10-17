package migrationhistory_test

import (
	"fmt"
	"reflect"
	"slices"
	"testing"

	mh "github.com/Subyard/Subyard/internal/migrationhistory"
)

func corpusMigrations(dependent bool) []mh.Migration {
	migrations := []mh.Migration{{ID: "a2", Domain: "a", FromEpoch: 1, ToEpoch: 2, Kind: "kind"}, {ID: "b2", Domain: "b", FromEpoch: 1, ToEpoch: 2, Kind: "kind"}, {ID: "a3", Domain: "a", FromEpoch: 2, ToEpoch: 3, Kind: "kind"}}
	if dependent {
		migrations[2].DependsOn = []string{"b2"}
	}
	return migrations
}

// oracle builds dependency-closed completed sets and domain continuity directly
// from bit membership. It never calls production history, advance or path code.
func oracle(migrations []mh.Migration, mask int) (map[string]mh.DomainState, bool) {
	states := map[string]mh.DomainState{"a": {Epoch: 1, Applied: []string{}}, "b": {Epoch: 1, Applied: []string{}}}
	completed := map[string]bool{}
	for i, m := range migrations {
		completed[m.ID] = mask&(1<<i) != 0
	}
	byEpoch := slices.Clone(migrations)
	slices.SortFunc(byEpoch, func(a, b mh.Migration) int { return a.FromEpoch - b.FromEpoch })
	valid := true
	for _, m := range byEpoch {
		if !completed[m.ID] {
			continue
		}
		state := states[m.Domain]
		if state.Epoch != m.FromEpoch {
			valid = false
		}
		state.Epoch = m.ToEpoch
		state.Applied = append(state.Applied, m.ID)
		states[m.Domain] = state
		for _, dependency := range m.DependsOn {
			if !completed[dependency] {
				valid = false
			}
		}
	}
	return states, valid
}
func registry(t *testing.T, migrations []mh.Migration) mh.Registry {
	t.Helper()
	r, err := mh.NewRegistry(map[string]int{"a": 1, "b": 1}, map[string]int{"a": 3, "b": 2}, migrations, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestIndependentStateModelCorpus(t *testing.T) {
	for _, dependent := range []bool{false, true} {
		original := corpusMigrations(dependent)
		orders := [][]mh.Migration{original, {original[1], original[0], original[2]}}
		if !dependent {
			orders = append(orders, []mh.Migration{original[0], original[2], original[1]})
		}
		for order, migrations := range orders {
			t.Run(fmt.Sprintf("dependency=%t/order=%d", dependent, order), func(t *testing.T) {
				r := registry(t, migrations)
				for mask := 0; mask < 1<<len(migrations); mask++ {
					states, valid := oracle(migrations, mask)
					a, err := r.History(states)
					if (err == nil) != valid {
						t.Fatalf("mask=%03b valid=%t err=%v", mask, valid, err)
					}
					if !valid {
						continue
					}
					pending, err := r.Pending(a)
					if err != nil {
						t.Fatal(err)
					}
					want := []string{}
					for i, m := range migrations {
						if mask&(1<<i) == 0 {
							want = append(want, m.ID)
						}
					}
					got := []string{}
					for _, m := range pending {
						got = append(got, m.ID)
					}
					if !slices.Equal(got, want) {
						t.Fatalf("pending=%v want %v", got, want)
					}
					edge, err := r.Next(a)
					if len(want) == 0 {
						if err == nil {
							t.Fatal("completed history has an edge")
						}
						if _, err := r.Complete(a); err != nil {
							t.Fatal(err)
						}
						continue
					}
					if err != nil || edge.Migration().ID != want[0] {
						t.Fatalf("next=%v err=%v want %v", edge.Migration(), err, want[0])
					}
					nextMask := mask
					for i, m := range migrations {
						if m.ID == want[0] {
							nextMask |= 1 << i
						}
					}
					expected, ok := oracle(migrations, nextMask)
					if !ok || !reflect.DeepEqual(edge.After().Domains(), expected) {
						t.Fatalf("edge disagrees with independent model: %#v / %#v", edge.After().Domains(), expected)
					}
				}
				// Enumerate historical legal orders using only the independent set oracle.
				var walk func(int, []string, []int)
				walk = func(mask int, path []string, cuts []int) {
					if mask == (1<<len(migrations))-1 {
						for cut, cutMask := range cuts {
							states, _ := oracle(migrations, cutMask)
							authority, err := r.History(states)
							if err != nil {
								t.Fatal(err)
							}
							trace, err := r.TraceFrom(authority, path)
							// Applied historical order may differ; future work must still
							// select the first missing migration in registry order.
							canonical := true
							futureMask := cutMask
							for _, id := range path[cut:] {
								nextIndex := -1
								for i := range migrations {
									if futureMask&(1<<i) == 0 {
										nextIndex = i
										break
									}
								}
								if nextIndex < 0 || migrations[nextIndex].ID != id {
									canonical = false
									break
								}
								futureMask |= 1 << nextIndex
							}
							if !canonical {
								if err == nil {
									t.Fatalf("noncanonical pending suffix accepted: %v cut=%d", path, cut)
								}
								continue
							}
							if err != nil {
								t.Fatalf("path=%v cut=%d: %v", path, cut, err)
							}
							if len(trace.Edges()) != len(path) {
								t.Fatal("trace edge count")
							}
							baseline, _ := oracle(migrations, 0)
							terminal, _ := oracle(migrations, mask)
							if !reflect.DeepEqual(trace.Initial().Domains(), baseline) || !reflect.DeepEqual(trace.Terminal().Domains(), terminal) {
								t.Fatal("trace frontier mismatch")
							}
						}
						return
					}
					for i, m := range migrations {
						if mask&(1<<i) != 0 {
							continue
						}
						next := mask | 1<<i
						if _, valid := oracle(migrations, next); valid {
							walk(next, append(slices.Clone(path), m.ID), append(slices.Clone(cuts), next))
						}
					}
				}
				walk(0, nil, []int{0})
			})
		}
	}
}
func TestOpaqueOwnershipAndAliasing(t *testing.T) {
	migrations := corpusMigrations(true)
	r := registry(t, migrations)
	foreign := registry(t, migrations)
	migrations[2].DependsOn[0] = "foreign"
	base, err := r.Baseline()
	if err != nil {
		t.Fatal(err)
	}
	for _, authority := range []mh.Authority{{}, func() mh.Authority { a, _ := foreign.Baseline(); return a }()} {
		if _, err := r.Next(authority); err == nil {
			t.Fatal("zero or foreign authority accepted")
		}
	}
	if err := (mh.Edge{}).Validate(); err == nil {
		t.Fatal("zero edge accepted")
	}
	edge, err := r.Next(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := foreign.CheckEdge(edge); err == nil {
		t.Fatal("foreign edge accepted")
	}
	states := edge.After().Domains()
	states["a"].Applied[0] = "corrupted"
	states["b"] = mh.DomainState{Epoch: 99}
	if edge.After().Domains()["a"].Applied[0] != "a2" || base.Domains()["a"].Epoch != 1 {
		t.Fatal("authority aliases caller output")
	}
	imported := edge.After().Domains()
	a, err := r.History(imported)
	if err != nil {
		t.Fatal(err)
	}
	imported["a"].Applied[0] = "corrupted"
	if a.Domains()["a"].Applied[0] != "a2" {
		t.Fatal("authority aliases input")
	}
	definition := edge.Migration()
	definition.ID = "foreign"
	if edge.Migration().ID != "a2" {
		t.Fatal("edge aliases definition")
	}
	for _, ids := range [][]string{{"unknown"}, {"a2", "a2"}, {"a3"}, {"b2", "a2"}} {
		if _, err := r.TraceFrom(a, ids); err == nil {
			t.Fatalf("invalid trace accepted: %v", ids)
		}
	}
	epochs := map[string]int{"a": 1}
	projection, err := mh.NewCompatibilityProjection(epochs, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	epochs["a"] = 4
	projection.Epochs()["a"] = 9
	if projection.Epochs()["a"] != 1 {
		t.Fatal("projection aliases epochs")
	}
}

func TestRegistryBindingOwnsAndChecksRetiredPins(t *testing.T) {
	migrations := corpusMigrations(false)
	minimum := map[string]int{"a": 1, "b": 1}
	current := map[string]int{"a": 3, "b": 2}
	pins := map[string]string{"a": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	r, err := mh.NewRegistry(minimum, current, migrations, pins)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := r.Baseline()
	if err != nil {
		t.Fatal(err)
	}
	edge, err := r.Next(baseline)
	if err != nil {
		t.Fatal(err)
	}
	pins["a"] = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	foreign, err := mh.NewRegistry(minimum, current, migrations, pins)
	if err != nil {
		t.Fatal(err)
	}
	if err := foreign.CheckEdgeBinding(edge); err == nil {
		t.Fatal("different retired prefix pin accepted")
	}
	if err := r.CheckEdgeBinding(edge); err != nil {
		t.Fatal("registry aliases retired pin input")
	}
	if err := r.CheckCompletion(mh.Completion{}); err == nil {
		t.Fatal("zero completion accepted")
	}
}
