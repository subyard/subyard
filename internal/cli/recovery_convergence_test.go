package cli

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/domain"
)

// recoveryWorld is a fixture host: a yard gate, an installed-source
// prerequisite and one yard's guest files. Each command and each operator
// step has a pure effect on it, so following the advice that the production
// renderers print, from any entry point, must reach readiness or a named
// terminal step.
type recoveryWorld struct {
	yards     map[string]yardGateState
	everyYard bool
	source    string // a refused source predicate
	files     string // "changed" or "unreadable" in filesYard
	filesYard string
}

func (world recoveryWorld) clone() recoveryWorld {
	world.yards = maps.Clone(world.yards)
	return world
}

func (world recoveryWorld) gate() yardGate {
	gate := yardGate{yards: map[string]*yardGateState{}, everyYard: world.everyYard}
	for _, name := range slices.Sorted(maps.Keys(world.yards)) {
		state := world.yards[name]
		if state.failed || state.drift || state.pending || state.config || state.unreachable {
			gate.yards[name] = &state
			gate.order = append(gate.order, name)
		}
	}
	// Readiness sees the files yard through the one convergence check.
	if !world.filesConverged() {
		if _, found := gate.yards[world.filesYard]; !found {
			gate.yards[world.filesYard] = &yardGateState{}
			gate.order = append(gate.order, world.filesYard)
			slices.Sort(gate.order)
		}
		gate.yards[world.filesYard].drift = true
	}
	if world.source != "" && slices.ContainsFunc(gate.order, func(name string) bool { return gate.state(name).repairOnly() }) {
		gate.source = &repairSourceRefusal{predicate: world.source}
	}
	return gate
}

// file is the guest observation of the files yard's one config file: a
// changed file differs from its desired bytes.
func (world recoveryWorld) file() configmaterial.File {
	file := configmaterial.File{Label: "agent", Destination: "/home/dev/.config/agent.md", Token: configmaterial.PayloadToken([]byte("desired\n")), Converged: true}
	switch world.files {
	case "changed":
		file.Token, file.Converged = "sha256:"+strings.Repeat("a", 64), false
	case "unreadable":
		file.Token, file.Converged = configmaterial.TokenUnreadable, false
	}
	return file
}

// filesConverged is the production state that init, config status and
// release readiness share.
func (world recoveryWorld) filesConverged() bool { return world.file().State() == "converged" }

// refresh is the real effect of init or config apply on the files yard: a
// changed file is written over its fresh observation, an unreadable one is
// refused.
func (world *recoveryWorld) refresh() {
	if world.files == "changed" {
		world.files = ""
	}
}

func (world recoveryWorld) ready() bool {
	return len(world.gate().order) == 0
}

// entry renders the one next step an entry point prints, through its
// production renderer, and the yard it advises on.
type recoveryEntry struct {
	attempted domain.Action
	render    func(world recoveryWorld, attempted domain.Action) (string, string)
}

func gateEntry(render func(gate yardGate, yard string, attempted domain.Action) string) func(recoveryWorld, domain.Action) (string, string) {
	return func(world recoveryWorld, attempted domain.Action) (string, string) {
		gate := world.gate()
		// A refused refresh of the files yard names its files and next step.
		files := []configmaterial.File{world.file()}
		if attempted.Verb == domain.VerbConfigApply && attempted.Yard == world.filesYard && !world.filesConverged() {
			return world.filesYard, configApplyNext(world.filesYard, configmaterial.Refusals(world.filesYard, files))
		}
		// config status names the files a refresh refuses, with their step.
		if attempted.Verb == domain.VerbConfigStatus && attempted.Yard == world.filesYard {
			if lines := configFileAttention(configTargetAssessment{Target: configTarget{Name: world.filesYard}, Files: files}); len(lines) != 0 {
				return world.filesYard, lastNext(lines[0])
			}
		}
		yard := gate.order[0]
		return yard, render(gate, yard, attempted)
	}
}

func lastNext(text string) string {
	index := strings.LastIndex(text, "next: ")
	if index < 0 {
		return ""
	}
	line, _, _ := strings.Cut(text[index+len("next: "):], "\n")
	return strings.TrimSuffix(line, ", then repeat this setting")
}

var recoveryEntries = map[string]recoveryEntry{
	"status": {render: gateEntry(func(gate yardGate, yard string, _ domain.Action) string {
		var output bytes.Buffer
		(&CLI{options: Options{Stdout: &output}}).printYardAttention(gate, yard)
		return lastNext(output.String())
	})},
	"refusal": {render: gateEntry(func(gate yardGate, yard string, _ domain.Action) string {
		return lastNext(yardGateRefusal(yard, gate))
	})},
	"rpc": {render: gateEntry(func(gate yardGate, yard string, attempted domain.Action) string { return gate.step(yard, attempted) })},
	"init": {attempted: domain.Action{Verb: domain.VerbInit, Yard: "named"},
		render: gateEntry(func(gate yardGate, yard string, attempted domain.Action) string { return gate.step(yard, attempted) })},
	"config apply": {attempted: domain.Action{Verb: domain.VerbConfigApply, Yard: "named"},
		render: gateEntry(func(gate yardGate, yard string, attempted domain.Action) string { return gate.step(yard, attempted) })},
	"sync": {attempted: domain.Action{Verb: domain.VerbConfigApply, Yard: "default"},
		render: gateEntry(func(gate yardGate, yard string, _ domain.Action) string {
			return gate.step(yard, domain.Action{Verb: domain.VerbConfigApply, Yard: yard})
		})},
	"migrate": {attempted: domain.Action{Verb: domain.VerbMigrate},
		render: gateEntry(func(gate yardGate, yard string, attempted domain.Action) string { return gate.step(yard, attempted) })},
	"migrate --check": {attempted: domain.Action{Verb: domain.VerbMigrateCheck},
		render: gateEntry(func(gate yardGate, yard string, attempted domain.Action) string { return gate.step(yard, attempted) })},
}

// parseStep reads the command of a printed step: a bare command, a command
// with its named cause, an owner's "run" form, or an operator step followed
// by a command. operator reports a step the operator does before it.
func parseStep(text string) (action domain.Action, operator, ok bool) {
	if parsed, err := domain.ParseAction(text); err == nil {
		return parsed, false, true
	}
	if before, _, found := strings.Cut(text, " ("); found {
		if parsed, err := domain.ParseAction(before); err == nil {
			return parsed, false, true
		}
	}
	for _, separator := range []string{"; then run ", ", then run ", ", then "} {
		if _, after, found := strings.Cut(text, separator); found {
			if parsed, _, retry := domain.ParseRetry("run " + strings.TrimPrefix(after, "run ")); retry {
				return parsed, true, true
			}
		}
	}
	if parsed, _, retry := domain.ParseRetry(text); retry {
		return parsed, false, true
	}
	return domain.Action{}, false, false
}

// operatorStep is what the operator does for a printed prerequisite.
func (world *recoveryWorld) operatorStep(text string) {
	switch {
	case strings.Contains(text, "correct the yard's configuration"):
		for name, state := range world.yards {
			state.config = false
			world.yards[name] = state
		}
		world.everyYard = false
	case strings.Contains(text, "replace it with a regular file"):
		world.files = "changed" // replaced, so the next apply plans again
	}
}

func (world *recoveryWorld) apply(action domain.Action) {
	state := world.yards[action.Yard]
	switch action.Verb {
	case domain.VerbUpdate:
		switch world.source {
		case "release-options", "journal-absent", "recovery-history", "source-proof", "proof-transaction", "proof-artifact":
			world.source = ""
		}
	case domain.VerbMigrate:
		switch world.source {
		case "journal-not-complete", "pending-publication", "pending-archive":
			world.source = ""
		}
	case domain.VerbStopForce:
		state.unreachable, state.failed = false, true // stopped, not converged until started
	case domain.VerbStart:
		state.failed = false
	case domain.VerbInit:
		// Init plans for itself on a running, stopped or uninitialized yard.
		state.failed, state.drift, state.pending = false, false, false
		if action.Yard == world.filesYard {
			world.refresh()
		}
	case domain.VerbConfigApply:
		state.drift = false
		if action.Yard == world.filesYard {
			world.refresh()
		}
	case domain.VerbIntegrationStatus:
		state.pending, state.retry = false, "" // the owner's resume finished
	}
	if action.Yard != "" {
		world.yards[action.Yard] = state
	}
}

func TestRecoveryAdviceConvergesFromEveryEntryPoint(t *testing.T) {
	worlds := map[string]recoveryWorld{
		"completed successor with drift":          {yards: map[string]yardGateState{"default": {drift: true}, "named": {drift: true}}},
		"failed peer":                             {yards: map[string]yardGateState{"named": {failed: true}}},
		"default configuration invalid":           {yards: map[string]yardGateState{"default": {config: true}, "named": {drift: true}}, everyYard: true},
		"unreachable guest agent":                 {yards: map[string]yardGateState{"named": {unreachable: true}}},
		"pending owner retry":                     {yards: map[string]yardGateState{"named": {pending: true, retry: "run yard -Y named integration status"}}},
		"pending owner retry in prose":            {yards: map[string]yardGateState{"named": {pending: true, retry: "restore the settings, then run yard -Y named integration status"}}},
		"stopped uninitialized yard":              {yards: map[string]yardGateState{"named": {failed: true, drift: true}}},
		"stopped mid-batch then drift":            {yards: map[string]yardGateState{"a": {drift: true}, "b": {failed: true}}},
		"file changed after plan":                 {files: "changed", filesYard: "named", yards: map[string]yardGateState{}},
		"unreadable peer file":                    {files: "unreadable", filesYard: "named", yards: map[string]yardGateState{}},
		"ready":                                   {yards: map[string]yardGateState{}},
		"wrong tooling":                           {yards: map[string]yardGateState{"named": {drift: true}}, source: "tooling-dir"},
		"unreadable store":                        {yards: map[string]yardGateState{"named": {drift: true}}, source: "store"},
		"pending owner retry outside the grammar": {yards: map[string]yardGateState{"named": {pending: true, retry: "run yard update for a fresh plan and confirmation"}}},
	}
	for _, predicate := range []string{"release-options", "journal-absent", "journal-not-complete", "pending-publication", "pending-archive", "recovery-history", "source-proof", "proof-transaction", "proof-artifact"} {
		worlds["source "+predicate] = recoveryWorld{yards: map[string]yardGateState{"named": {drift: true}}, source: predicate}
	}
	// Only these steps may leave the world unready: each names what the
	// operator must do, and none is a command the gate would refuse.
	terminal := map[string]string{
		"wrong tooling":                           "run it with the installed yard command; failed: tooling-dir",
		"unreadable store":                        "failed: store",
		"pending owner retry outside the grammar": "run yard update for a fresh plan and confirmation",
	}
	for name, initial := range worlds {
		for entryName, entry := range recoveryEntries {
			t.Run(name+"/"+entryName, func(t *testing.T) {
				world, attempted, seen := initial.clone(), entry.attempted, map[string]bool{}
				for hop := 0; !world.ready(); hop++ {
					if hop == 8 {
						t.Fatalf("no convergence in 8 hops: %#v", world)
					}
					// The entry point prints the first step; each command run
					// after it prints the next one, knowing what it attempted.
					render := entry.render
					if hop != 0 {
						render = recoveryEntries["rpc"].render
						if attempted.Verb == domain.VerbConfigApply {
							render = recoveryEntries["config apply"].render
						}
					}
					yard, text := render(world, attempted)
					if strings.Contains(text, "\n") || text == "" {
						t.Fatalf("not one next step for %s: %q", yard, text)
					}
					action, operator, ok := parseStep(text)
					if !ok || action == attempted && action.Verb != domain.VerbConfigApply {
						// After the read-only check, a source finding names its
						// failed predicate as the operator's to resolve.
						inspected := attempted.Verb == domain.VerbMigrateCheck && strings.Contains(text, "; failed: ")
						if want := terminal[name]; !inspected && (want == "" || !strings.Contains(text, want)) {
							t.Fatalf("%s: no admitted step at %s: %q after %#v", name, yard, text, attempted)
						}
						return
					}
					if operator {
						world.operatorStep(text)
					}
					if !world.gate().admits(action) {
						t.Fatalf("advised %q is not admitted", text)
					}
					key := fmt.Sprintf("%#v|%#v", world, action)
					if seen[key] {
						t.Fatalf("repeated state and action: %q in %#v", text, world)
					}
					seen[key] = true
					world.apply(action)
					attempted = action
				}
			})
		}
	}
}

// A refused installed source names its predicate and the command that makes
// it hold, at every entry point; no repair is advised behind it.
func TestSourceRefusalNamesItsPredicateAndRemedy(t *testing.T) {
	for predicate, want := range map[string]string{
		"proof-transaction":   "yard update (the installed release source belongs to another transaction; failed: proof-transaction",
		"pending-publication": "yard migrate (finish the pending protected publication with its sealed owner before source repair; failed: pending-publication",
		"journal-absent":      "yard update (",
		"tooling-dir":         "run it with the installed yard command; failed: tooling-dir",
	} {
		gate := recoveryWorld{yards: map[string]yardGateState{"named": {drift: true}}, source: predicate}.gate()
		refusal, step := yardGateRefusal("named", gate), gate.step("named", domain.Action{})
		if !strings.Contains(step, want) || !strings.Contains(refusal, "next: "+step) || strings.Contains(step, "init") {
			t.Fatalf("%s: refusal %q, step %q", predicate, refusal, step)
		}
		if gate.admits(domain.Action{Verb: domain.VerbInit, Yard: "named"}) || gate.admits(domain.Action{Verb: domain.VerbConfigApply, Yard: "named"}) {
			t.Fatalf("%s: a repair was admitted with a refused installed source", predicate)
		}
	}
	if text := (repairSourceRefusal{predicate: "proof-artifact"}).Error(); !strings.Contains(text, "failed: proof-artifact; next: yard update") {
		t.Fatalf("refusal text %q", text)
	}
}
