package migrationhistory_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	mh "github.com/Subyard/Subyard/internal/migrationhistory"
)

type pilotRegistry struct {
	Minimum    map[string]int    `json:"minimum"`
	Current    map[string]int    `json:"current"`
	Migrations []mh.Migration    `json:"migrations"`
	Retired    map[string]string `json:"retired"`
}
type pilotCase struct {
	Op       string                    `json:"op"`
	Registry *pilotRegistry            `json:"registry,omitempty"`
	Other    *pilotRegistry            `json:"other,omitempty"`
	States   map[string]mh.DomainState `json:"states,omitempty"`
	Traces   [][]string                `json:"traces,omitempty"`
	Epochs   map[string]int            `json:"epochs,omitempty"`
	Pin      string                    `json:"pin,omitempty"`
}

func pilotDefinition(m []mh.Migration) *pilotRegistry {
	return &pilotRegistry{map[string]int{"a": 1, "b": 1}, map[string]int{"a": 3, "b": 2}, m, map[string]string{}}
}
func (d *pilotRegistry) checked() (mh.Registry, error) {
	return mh.NewRegistry(d.Minimum, d.Current, d.Migrations, d.Retired)
}
func rejection() map[string]any { return map[string]any{"accepted": false} }
func goPilot(c pilotCase) map[string]any {
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
		return rejection()
	}
	if c.Op == "binding" {
		other, err := c.Other.checked()
		if err != nil {
			return rejection()
		}
		base, _ := r.Baseline()
		e, err := r.Next(base)
		if err != nil {
			return rejection()
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
		return rejection()
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
			traces = append(traces, rejection())
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
func oracleTrace(m []mh.Migration, observed int, ids []string) map[string]any {
	indices := map[string]int{}
	for i, v := range m {
		indices[v.ID] = i
	}
	cut := len(ids)
	seen := map[string]bool{}
	for n, id := range ids {
		i, ok := indices[id]
		if !ok || seen[id] {
			return rejection()
		}
		seen[id] = true
		if observed&(1<<i) == 0 {
			if cut == len(ids) {
				cut = n
			}
		} else if cut != len(ids) {
			return rejection()
		}
	}
	initial := observed
	for n := cut - 1; n >= 0; n-- {
		initial &^= 1 << indices[ids[n]]
		if _, ok := oracle(m, initial); !ok {
			return rejection()
		}
	}
	cursor := initial
	for n, id := range ids {
		i := indices[id]
		if cursor&(1<<i) != 0 {
			return rejection()
		}
		if n >= cut {
			first := 0
			for first < len(m) && cursor&(1<<first) != 0 {
				first++
			}
			if first != i {
				return rejection()
			}
		}
		cursor |= 1 << i
		if _, ok := oracle(m, cursor); !ok {
			return rejection()
		}
	}
	start, _ := oracle(m, initial)
	end, _ := oracle(m, cursor)
	return map[string]any{"accepted": true, "initial": start, "terminal": end, "edges": ids}
}
func historyExpected(m []mh.Migration, mask int, traces [][]string) map[string]any {
	_, valid := oracle(m, mask)
	if !valid {
		return rejection()
	}
	pending := []string{}
	var next any
	for i, v := range m {
		if mask&(1<<i) == 0 {
			pending = append(pending, v.ID)
			if next == nil {
				after, ok := oracle(m, mask|1<<i)
				if !ok {
					panic("invalid canonical oracle edge")
				}
				next = map[string]any{"id": v.ID, "after": after}
			}
		}
	}
	results := []any{}
	for _, ids := range traces {
		results = append(results, oracleTrace(m, mask, ids))
	}
	return map[string]any{"accepted": true, "pending": pending, "next": next, "complete": len(pending) == 0, "traces": results}
}
func pilotCorpus() ([]pilotCase, []map[string]any) {
	cases := []pilotCase{}
	expected := []map[string]any{}
	add := func(c pilotCase, want map[string]any) { cases = append(cases, c); expected = append(expected, want) }
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
				add(pilotCase{Op: "history", Registry: pilotDefinition(m), States: states, Traces: traces}, historyExpected(m, mask, traces))
			}
		}
	}
	valid := pilotDefinition(corpusMigrations(false))
	add(pilotCase{Op: "registry", Registry: valid}, map[string]any{"accepted": true})
	// Mutations each violate one registry contract; expected results are independent.
	mutations := []func(*pilotRegistry){
		func(d *pilotRegistry) { d.Minimum = map[string]int{} }, func(d *pilotRegistry) { d.Current["foreign"] = 1 }, func(d *pilotRegistry) { d.Minimum["a"] = 0 }, func(d *pilotRegistry) { d.Current["a"] = 1 },
		func(d *pilotRegistry) { d.Migrations[0].ID = "-unsafe" }, func(d *pilotRegistry) { d.Migrations[0].Kind = ".." }, func(d *pilotRegistry) { d.Migrations[1].ID = "a2" }, func(d *pilotRegistry) { d.Migrations[0].Domain = "foreign" }, func(d *pilotRegistry) { d.Migrations[0].ToEpoch = 3 },
		func(d *pilotRegistry) { d.Migrations[0].DependsOn = []string{"a3"} }, func(d *pilotRegistry) { d.Migrations[2].DependsOn = []string{"a2", "a2"} }, func(d *pilotRegistry) { d.Retired["foreign"] = strings.Repeat("a", 64) }, func(d *pilotRegistry) { d.Retired["a"] = strings.Repeat("A", 64) }, func(d *pilotRegistry) { d.Migrations[0].ID = strings.Repeat("a", 129) },
	}
	for _, mutate := range mutations {
		d := pilotDefinition(corpusMigrations(false))
		mutate(d)
		add(pilotCase{Op: "registry", Registry: d}, rejection())
	}
	for _, mutate := range []func(map[string]mh.DomainState){func(s map[string]mh.DomainState) { delete(s, "a") }, func(s map[string]mh.DomainState) { s["a"] = mh.DomainState{Epoch: 0, Applied: []string{}} }, func(s map[string]mh.DomainState) { s["a"] = mh.DomainState{Epoch: 4, Applied: []string{"a2", "a3"}} }, func(s map[string]mh.DomainState) { s["a"] = mh.DomainState{Epoch: 2, Applied: []string{"unknown"}} }, func(s map[string]mh.DomainState) { s["a"] = mh.DomainState{Epoch: 3, Applied: []string{"a3", "a2"}} }} {
		states, _ := oracle(valid.Migrations, 0)
		mutate(states)
		add(pilotCase{Op: "history", Registry: valid, States: states, Traces: [][]string{{}}}, rejection())
	}
	for _, pin := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		d := pilotDefinition(corpusMigrations(false))
		d.Retired["a"] = pin
		other := pilotDefinition(corpusMigrations(false))
		other.Retired["a"] = strings.Repeat("a", 64)
		add(pilotCase{Op: "binding", Registry: d, Other: other}, map[string]any{"accepted": pin == other.Retired["a"], "identity": false, "completionIdentity": false})
	}
	for _, pin := range []string{strings.Repeat("a", 64), "", strings.Repeat("g", 64)} {
		add(pilotCase{Op: "projection", Epochs: map[string]int{"a": 1}, Pin: pin}, map[string]any{"accepted": len(pin) == 64 && pin[0] == 'a'})
	}

	// Exercise epoch arithmetic at the signed 64-bit boundary on supported hosts.
	maxEpoch := int(^uint(0) >> 1)
	d := &pilotRegistry{map[string]int{"a": maxEpoch}, map[string]int{"a": maxEpoch}, []mh.Migration{}, map[string]string{}}
	add(pilotCase{Op: "registry", Registry: d}, map[string]any{"accepted": true})
	d = &pilotRegistry{map[string]int{"a": maxEpoch}, map[string]int{"a": maxEpoch}, []mh.Migration{{ID: "overflow", Domain: "a", FromEpoch: maxEpoch, ToEpoch: -maxEpoch - 1, Kind: "kind"}}, map[string]string{}}
	add(pilotCase{Op: "registry", Registry: d}, rejection())
	for _, mutate := range []func(*pilotRegistry){func(d *pilotRegistry) { d.Migrations[0].Kind = "other" }, func(d *pilotRegistry) { d.Migrations[0], d.Migrations[1] = d.Migrations[1], d.Migrations[0] }, func(d *pilotRegistry) { d.Migrations[2].DependsOn = []string{"b2"} }} {
		other := pilotDefinition(corpusMigrations(false))
		mutate(other)
		add(pilotCase{Op: "binding", Registry: valid, Other: other}, map[string]any{"accepted": false, "identity": false, "completionIdentity": false})
	}
	return cases, expected
}
func canonical(t *testing.T, v any) []byte {
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
func TestRustPilotCorpus(t *testing.T) {
	cases, expected := pilotCorpus()
	for i, c := range cases {
		if got, want := canonical(t, goPilot(c)), canonical(t, expected[i]); !bytes.Equal(got, want) {
			t.Fatalf("Go case %d: got %s want %s", i, got, want)
		}
	}
	binary := os.Getenv("SUBYARD_MIGRATIONHISTORY_RUST")
	if binary == "" {
		t.Logf("independent oracle checked against Go (%d cases); Rust comparison opt-in", len(cases))
		return
	}
	iterations := 1
	measure := os.Getenv("SUBYARD_MIGRATIONHISTORY_MEASURE") == "1"
	if measure {
		iterations = 100
	}
	start := time.Now()
	for range iterations {
		for _, c := range cases {
			_ = goPilot(c)
		}
	}
	goDuration := time.Since(start)
	request := map[string]any{"cases": cases, "iterations": iterations}
	input, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary)
	command.Stdin = bytes.NewReader(input)
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Results      []json.RawMessage `json:"results"`
		ElapsedNanos int64             `json:"elapsedNanos"`
		Iterations   int               `json:"iterations"`
	}
	if err = json.Unmarshal(output, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != len(expected) || response.Iterations != iterations {
		t.Fatal("incomplete Rust batch")
	}
	for i, got := range response.Results {
		if !bytes.Equal(canonical(t, got), canonical(t, expected[i])) {
			t.Fatalf("Rust case %d: got %s want %s", i, canonical(t, got), canonical(t, expected[i]))
		}
	}
	if measure {
		t.Logf("%d cases, %d iterations: Go pure=%s Rust pure=%s (registry/history/results; excludes JSON and process startup)", len(cases), iterations, goDuration, time.Duration(response.ElapsedNanos))
	} else {
		t.Logf("%d cases match independent oracle in Go and Rust", len(cases))
	}

	// These small requests exercise amplification/repetition limits without
	// constructing a large semantic result or consuming the allowed work budget.
	wideCase := func(domains, traces int) pilotCase {
		definition := &pilotRegistry{map[string]int{}, map[string]int{}, []mh.Migration{}, map[string]string{}}
		states := map[string]mh.DomainState{}
		for i := 0; i < domains; i++ {
			name := "domain" + strconv.Itoa(i)
			definition.Minimum[name] = 1
			definition.Current[name] = 1
			states[name] = mh.DomainState{Epoch: 1, Applied: []string{}}
		}
		paths := make([][]string, traces)
		for i := range paths {
			paths[i] = []string{}
		}
		return pilotCase{Op: "history", Registry: definition, States: states, Traces: paths}
	}
	for _, contract := range []struct {
		name                        string
		domains, traces, iterations int
		code                        string
	}{
		{"small_expansion_control", 2, 2, 1, ""},
		{"result_budget", 250, 100, 1, "result-budget"},
		{"work_budget", 100, 1, 10000, "work-budget"},
	} {
		t.Run(contract.name, func(t *testing.T) {
			document, err := json.Marshal(map[string]any{"cases": []pilotCase{wideCase(contract.domains, contract.traces)}, "iterations": contract.iterations})
			if err != nil {
				t.Fatal(err)
			}
			if len(document) > 32768 {
				t.Fatal("budget regression request is unexpectedly large")
			}
			cmd := exec.Command(binary)
			cmd.Stdin = bytes.NewReader(document)
			out, err := cmd.Output()
			if contract.code == "" {
				var control struct {
					Results []struct {
						Accepted bool `json:"accepted"`
					} `json:"results"`
				}
				if err != nil || json.Unmarshal(out, &control) != nil || len(control.Results) != 1 || !control.Results[0].Accepted {
					t.Fatalf("small request refused: %v %q", err, out)
				}
				return
			}
			exit, ok := err.(*exec.ExitError)
			if !ok || len(out) != 0 || strings.TrimSpace(string(exit.Stderr)) != "migrationhistory-pilot: "+contract.code {
				t.Fatalf("expected %s before evaluation, got error=%v stdout=%q stderr=%q", contract.code, err, out, func() string {
					if ok {
						return string(exit.Stderr)
					}
					return ""
				}())
			}
		})
	}
	for name, input := range map[string]string{"malformed": "{", "trailing": string(input) + "{}", "unknown_field": `{"cases":[],"iterations":1,"extra":true}`, "unknown_tag": `{"cases":[{"op":"execute"}],"iterations":1}`, "nested_unknown": `{"cases":[{"op":"registry","registry":{"minimum":{"a":1},"current":{"a":1},"migrations":[],"extra":true}}],"iterations":1}`,
		"epoch_overflow": `{"cases":[{"op":"registry","registry":{"minimum":{"a":9223372036854775808},"current":{"a":1},"migrations":[]}}],"iterations":1}`,
		"fraction":       `{"cases":[],"iterations":1.5}`, "negative": `{"cases":[],"iterations":-1}`, "overflow": `{"cases":[],"iterations":18446744073709551616}`, "oversized": strings.Repeat(" ", 1_048_577), "zero": `{"cases":[],"iterations":0}`} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(binary)
			cmd.Stdin = strings.NewReader(input)
			out, err := cmd.Output()
			if err == nil || len(out) != 0 {
				t.Fatalf("invalid codec input accepted: %v %q", err, out)
			}
		})
	}
}
