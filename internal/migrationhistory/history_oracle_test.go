package migrationhistory_test

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	mh "github.com/Subyard/Subyard/internal/migrationhistory"
	"github.com/Subyard/Subyard/internal/testkit"
)

type historyRegistry struct {
	Minimum    map[string]int    `json:"minimum"`
	Current    map[string]int    `json:"current"`
	Migrations []mh.Migration    `json:"migrations"`
	Retired    map[string]string `json:"retired"`
}
type historyCase struct {
	Op       string                    `json:"op"`
	Registry *historyRegistry          `json:"registry,omitempty"`
	Other    *historyRegistry          `json:"other,omitempty"`
	States   map[string]mh.DomainState `json:"states,omitempty"`
	Traces   [][]string                `json:"traces,omitempty"`
	Epochs   map[string]int            `json:"epochs,omitempty"`
	Pin      string                    `json:"pin,omitempty"`
}

func historyDefinition(m []mh.Migration) *historyRegistry {
	return &historyRegistry{map[string]int{"a": 1, "b": 1}, map[string]int{"a": 3, "b": 2}, m, map[string]string{}}
}
func (d *historyRegistry) checked() (mh.Registry, error) {
	return mh.NewRegistry(d.Minimum, d.Current, d.Migrations, d.Retired)
}
func historyRejection() map[string]any { return map[string]any{"accepted": false} }
func goHistoryCase(c historyCase) map[string]any {
	if c.Op == "registry" {
		_, err := c.Registry.checked()
		return map[string]any{"accepted": err == nil}
	}
	if c.Op == "projection" {
		_, err := mh.NewCompatibilityProjection(c.Epochs, c.Pin)
		return map[string]any{"accepted": err == nil}
	}
	r, err := c.Registry.checked()
	if err != nil {
		return historyRejection()
	}
	if c.Op == "binding" {
		other, err := c.Other.checked()
		if err != nil {
			return historyRejection()
		}
		base, _ := r.Baseline()
		e, err := r.Next(base)
		if err != nil {
			return historyRejection()
		}
		pending, _ := r.Pending(base)
		ids := []string{}
		for _, m := range pending {
			ids = append(ids, m.ID)
		}
		tr, _ := r.TraceFrom(base, ids)
		complete, _ := r.Complete(tr.Terminal())
		return map[string]any{"accepted": other.CheckEdgeBinding(e) == nil, "identity": other.CheckEdge(e) == nil, "completionIdentity": other.CheckCompletion(complete) == nil}
	}
	a, err := r.History(c.States)
	if err != nil {
		return historyRejection()
	}
	p, _ := r.Pending(a)
	pending := []string{}
	for _, m := range p {
		pending = append(pending, m.ID)
	}
	var next any
	if e, err := r.Next(a); err == nil {
		next = map[string]any{"id": e.Migration().ID, "after": e.After().Domains()}
	}
	traces := []any{}
	for _, ids := range c.Traces {
		tr, err := r.TraceFrom(a, ids)
		if err != nil {
			traces = append(traces, historyRejection())
			continue
		}
		edges := []string{}
		for _, e := range tr.Edges() {
			edges = append(edges, e.Migration().ID)
		}
		traces = append(traces, map[string]any{"accepted": true, "initial": tr.Initial().Domains(), "terminal": tr.Terminal().Domains(), "edges": edges})
	}
	_, err = r.Complete(a)
	return map[string]any{"accepted": true, "pending": pending, "next": next, "complete": err == nil, "traces": traces}
}

// The existing independent bitset oracle is the sole expected-history model.
// Exhaust all short ID sequences (including duplicates/unknowns) at every set cut.
func historyOracleTrace(m []mh.Migration, observed int, ids []string) map[string]any {
	indices := map[string]int{}
	for i, v := range m {
		indices[v.ID] = i
	}
	cut := len(ids)
	seen := map[string]bool{}
	for n, id := range ids {
		i, ok := indices[id]
		if !ok || seen[id] {
			return historyRejection()
		}
		seen[id] = true
		if observed&(1<<i) == 0 {
			if cut == len(ids) {
				cut = n
			}
		} else if cut != len(ids) {
			return historyRejection()
		}
	}
	initial := observed
	for n := cut - 1; n >= 0; n-- {
		initial &^= 1 << indices[ids[n]]
		if _, ok := oracle(m, initial); !ok {
			return historyRejection()
		}
	}
	cursor := initial
	for n, id := range ids {
		i := indices[id]
		if cursor&(1<<i) != 0 {
			return historyRejection()
		}
		if n >= cut {
			first := 0
			for first < len(m) && cursor&(1<<first) != 0 {
				first++
			}
			if first != i {
				return historyRejection()
			}
		}
		cursor |= 1 << i
		if _, ok := oracle(m, cursor); !ok {
			return historyRejection()
		}
	}
	start, _ := oracle(m, initial)
	end, _ := oracle(m, cursor)
	return map[string]any{"accepted": true, "initial": start, "terminal": end, "edges": ids}
}
func historyOracleExpected(m []mh.Migration, mask int, traces [][]string) map[string]any {
	_, valid := oracle(m, mask)
	if !valid {
		return historyRejection()
	}
	pending := []string{}
	var next any
	for i, v := range m {
		if mask&(1<<i) == 0 {
			pending = append(pending, v.ID)
			if next == nil {
				after, ok := oracle(m, mask|1<<i)
				if !ok {
					panic("invalid historyCanonical oracle edge")
				}
				next = map[string]any{"id": v.ID, "after": after}
			}
		}
	}
	results := []any{}
	for _, ids := range traces {
		results = append(results, historyOracleTrace(m, mask, ids))
	}
	return map[string]any{"accepted": true, "pending": pending, "next": next, "complete": len(pending) == 0, "traces": results}
}
func historyCorpus() ([]historyCase, []map[string]any) {
	cases := []historyCase{}
	expected := []map[string]any{}
	add := func(c historyCase, want map[string]any) { cases = append(cases, c); expected = append(expected, want) }
	for _, dependent := range []bool{false, true} {
		m := corpusMigrations(dependent)
		orders := [][]mh.Migration{m, {m[1], m[0], m[2]}}
		if !dependent {
			orders = append(orders, []mh.Migration{m[0], m[2], m[1]})
		}
		for _, m := range orders {
			traces := [][]string{{}}
			var walk func([]string)
			walk = func(path []string) {
				if len(path) == 3 {
					return
				}
				for _, id := range []string{"a2", "b2", "a3", "unknown"} {
					next := append(slices.Clone(path), id)
					traces = append(traces, next)
					walk(next)
				}
			}
			walk(nil)
			for mask := 0; mask < 1<<len(m); mask++ {
				states, _ := oracle(m, mask)
				add(historyCase{Op: "history", Registry: historyDefinition(m), States: states, Traces: traces}, historyOracleExpected(m, mask, traces))
			}
		}
	}
	valid := historyDefinition(corpusMigrations(false))
	add(historyCase{Op: "registry", Registry: valid}, map[string]any{"accepted": true})
	// Mutations each violate one registry contract; expected results are independent.
	mutations := []func(*historyRegistry){
		func(d *historyRegistry) { d.Minimum = map[string]int{} }, func(d *historyRegistry) { d.Current["foreign"] = 1 }, func(d *historyRegistry) { d.Minimum["a"] = 0 }, func(d *historyRegistry) { d.Current["a"] = 1 },
		func(d *historyRegistry) { d.Migrations[0].ID = "-unsafe" }, func(d *historyRegistry) { d.Migrations[0].Kind = ".." }, func(d *historyRegistry) { d.Migrations[1].ID = "a2" }, func(d *historyRegistry) { d.Migrations[0].Domain = "foreign" }, func(d *historyRegistry) { d.Migrations[0].ToEpoch = 3 },
		func(d *historyRegistry) { d.Migrations[0].DependsOn = []string{"a3"} }, func(d *historyRegistry) { d.Migrations[2].DependsOn = []string{"a2", "a2"} }, func(d *historyRegistry) { d.Retired["foreign"] = strings.Repeat("a", 64) }, func(d *historyRegistry) { d.Retired["a"] = strings.Repeat("A", 64) }, func(d *historyRegistry) { d.Migrations[0].ID = strings.Repeat("a", 129) },
	}
	for _, mutate := range mutations {
		d := historyDefinition(corpusMigrations(false))
		mutate(d)
		add(historyCase{Op: "registry", Registry: d}, historyRejection())
	}
	for _, mutate := range []func(map[string]mh.DomainState){func(s map[string]mh.DomainState) { delete(s, "a") }, func(s map[string]mh.DomainState) { s["a"] = mh.DomainState{Epoch: 0, Applied: []string{}} }, func(s map[string]mh.DomainState) { s["a"] = mh.DomainState{Epoch: 4, Applied: []string{"a2", "a3"}} }, func(s map[string]mh.DomainState) { s["a"] = mh.DomainState{Epoch: 2, Applied: []string{"unknown"}} }, func(s map[string]mh.DomainState) { s["a"] = mh.DomainState{Epoch: 3, Applied: []string{"a3", "a2"}} }} {
		states, _ := oracle(valid.Migrations, 0)
		mutate(states)
		add(historyCase{Op: "history", Registry: valid, States: states, Traces: [][]string{{}}}, historyRejection())
	}
	for _, pin := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		d := historyDefinition(corpusMigrations(false))
		d.Retired["a"] = pin
		other := historyDefinition(corpusMigrations(false))
		other.Retired["a"] = strings.Repeat("a", 64)
		add(historyCase{Op: "binding", Registry: d, Other: other}, map[string]any{"accepted": pin == other.Retired["a"], "identity": false, "completionIdentity": false})
	}
	for _, pin := range []string{strings.Repeat("a", 64), "", strings.Repeat("g", 64)} {
		add(historyCase{Op: "projection", Epochs: map[string]int{"a": 1}, Pin: pin}, map[string]any{"accepted": len(pin) == 64 && pin[0] == 'a'})
	}

	// Exercise epoch arithmetic at the signed 64-bit boundary on supported hosts.
	maxEpoch := int(^uint(0) >> 1)
	d := &historyRegistry{map[string]int{"a": maxEpoch}, map[string]int{"a": maxEpoch}, []mh.Migration{}, map[string]string{}}
	add(historyCase{Op: "registry", Registry: d}, map[string]any{"accepted": true})
	d = &historyRegistry{map[string]int{"a": maxEpoch}, map[string]int{"a": maxEpoch}, []mh.Migration{{ID: "overflow", Domain: "a", FromEpoch: maxEpoch, ToEpoch: -maxEpoch - 1, Kind: "kind"}}, map[string]string{}}
	add(historyCase{Op: "registry", Registry: d}, historyRejection())
	for _, mutate := range []func(*historyRegistry){func(d *historyRegistry) { d.Migrations[0].Kind = "other" }, func(d *historyRegistry) { d.Migrations[0], d.Migrations[1] = d.Migrations[1], d.Migrations[0] }, func(d *historyRegistry) { d.Migrations[2].DependsOn = []string{"b2"} }} {
		other := historyDefinition(corpusMigrations(false))
		mutate(other)
		add(historyCase{Op: "binding", Registry: valid, Other: other}, map[string]any{"accepted": false, "identity": false, "completionIdentity": false})
	}
	return cases, expected
}
func historyCanonical(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if err = decoder.Decode(&normalized); err != nil {
		t.Fatal(err)
	}
	b, err = json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRustHistoryOracleCorpus always checks Go against the independent bitset
// oracle. The required Rust gates export these exact cases before invoking the
// production crate integration test; ordinary Go tests need no Rust executable.
func TestRustHistoryOracleCorpus(t *testing.T) {
	cases, expected := historyCorpus()
	type row struct {
		Input    historyCase    `json:"input"`
		Expected map[string]any `json:"expected"`
	}
	rows := make([]row, len(cases))
	for i, c := range cases {
		if got, want := historyCanonical(t, goHistoryCase(c)), historyCanonical(t, expected[i]); !bytes.Equal(got, want) {
			t.Fatalf("Go case %d: got %s want %s", i, got, want)
		}
		rows[i] = row{c, expected[i]}
	}
	if path := os.Getenv("SUBYARD_RUST_HISTORY_CORPUS"); path != "" {
		payload, err := json.Marshal(struct {
			SchemaVersion int   `json:"schemaVersion"`
			Cases         []row `json:"cases"`
		}{1, rows})
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, append(payload, '\n'), 0600)
	}
	t.Logf("%d independent history cases checked; history/trace, invalid registry/frontier, edge/completion binding and projection", len(rows))
}
