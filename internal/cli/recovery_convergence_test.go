package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// recoveryWorld is a fixture host: a yard gate, an installed-source
// prerequisite, an optional host owner retry and one yard's guest files.
// Each verb has a pure effect on it, so following the advice from any entry
// point must reach readiness or a named terminal finding.
type recoveryWorld struct {
	yards      map[string]yardGateState
	everyYard  bool
	source     string // a refused source predicate
	owner      string // a host-wide owner retry
	files      string // "changed", "unreadable" or "" for the config apply yard
	filesYard  string
	ownerFixed bool
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
	if world.source != "" {
		gate.source = &repairSourceRefusal{predicate: world.source}
	}
	return gate
}

// findings is every finding an entry point observes, for the first yard
// that is not ready.
func (world recoveryWorld) findings() (string, []domain.Finding) {
	var findings []domain.Finding
	if world.owner != "" {
		findings = append(findings, domain.Finding{Kind: domain.FindingOwner, Retry: world.owner})
	}
	gate := world.gate()
	if len(gate.order) != 0 {
		return gate.order[0], append(findings, gate.yardFindings(gate.order[0])...)
	}
	if world.files != "" {
		approved := configTargetAssessment{Target: configTarget{Name: world.filesYard}, Consent: &ports.ConfigConsent{Files: []ports.ConfigFileConsent{{Destination: "/home/dev/a", Token: "absent"}}}}
		fresh := approved
		fresh.Changed, fresh.Consent = true, &ports.ConfigConsent{Files: []ports.ConfigFileConsent{{Destination: "/home/dev/a", Token: "sha256:" + strings.Repeat("a", 64)}}}
		if world.files == "unreadable" {
			fresh.Unreadable, fresh.Changed = []string{"/home/dev/a"}, false
		}
		return world.filesYard, append(findings, configApplyFindings(approved, fresh, nil)...)
	}
	if world.source != "" {
		return "default", append(findings, gate.source.finding())
	}
	return "", findings
}

func (world *recoveryWorld) apply(action domain.Action) {
	state := world.yards[action.Yard]
	switch action.Verb {
	case domain.VerbUpdate:
		world.owner, world.ownerFixed = "", true
	case domain.VerbMigrate:
		if world.source == "journal-not-complete" {
			world.source = ""
		}
	case domain.VerbConfigValidate:
		state.config, world.everyYard = false, false // the operator corrected it
	case domain.VerbStopForce:
		state.unreachable = false
		state.failed = true // a stopped yard is not converged until init or start
	case domain.VerbStart:
		state.failed = false
	case domain.VerbInit:
		// Init plans for itself: on a running, stopped or uninitialized yard
		// it observes the guest files right before it writes them.
		state.failed, state.drift, state.pending = false, false, false
		if world.files != "unreadable" {
			world.files = ""
		}
	case domain.VerbConfigApply:
		state.drift = false
		if world.files == "changed" {
			world.files = ""
		}
	}
	if action.Yard != "" {
		world.yards[action.Yard] = state
	}
}

func TestRecoveryAdviceConvergesFromEveryEntryPoint(t *testing.T) {
	worlds := map[string]recoveryWorld{
		"completed successor with drift":  {yards: map[string]yardGateState{"default": {drift: true}, "named": {drift: true}}},
		"failed peer":                     {yards: map[string]yardGateState{"named": {failed: true}}},
		"default configuration invalid":   {yards: map[string]yardGateState{"default": {config: true}, "named": {drift: true}}, everyYard: true},
		"unreachable guest agent":         {yards: map[string]yardGateState{"named": {unreachable: true}}},
		"pending owner retry":             {yards: map[string]yardGateState{"named": {pending: true, retry: "run yard -Y named integration status"}}},
		"pending publication":             {yards: map[string]yardGateState{"named": {drift: true}}, source: "source-proof"},
		"wrong tooling":                   {yards: map[string]yardGateState{"named": {drift: true}}, source: "tooling-dir"},
		"unfinished journal":              {yards: map[string]yardGateState{"named": {drift: true}}, source: "journal-not-complete"},
		"stopped uninitialized yard":      {yards: map[string]yardGateState{"named": {failed: true, drift: true}}},
		"older owner":                     {owner: "run yard update", yards: map[string]yardGateState{}},
		"owner retry outside the grammar": {owner: "run yard update for a fresh plan and confirmation", yards: map[string]yardGateState{"named": {drift: true}}},
		"file changed after plan":         {files: "changed", filesYard: "named", yards: map[string]yardGateState{}},
		"unreadable peer file":            {files: "unreadable", filesYard: "named", yards: map[string]yardGateState{}},
		"stopped mid-batch then drift":    {yards: map[string]yardGateState{"a": {drift: true}, "b": {failed: true}}},
		"ready":                           {yards: map[string]yardGateState{}},
	}
	entries := map[string]domain.Action{
		"status": {}, "rpc": {}, "config apply": {Verb: domain.VerbConfigApply, Yard: "named"},
		"init": {Verb: domain.VerbInit, Yard: "named"}, "sync": {Verb: domain.VerbConfigApply, Yard: "default"},
		"migrate --check": {Verb: domain.VerbMigrateCheck}, "update --check": {Verb: domain.VerbUpdateCheck},
	}
	for name, initial := range worlds {
		for entry, attempted := range entries {
			t.Run(name+"/"+entry, func(t *testing.T) {
				world, seen := initial.clone(), map[string]bool{}
				for hop := 0; ; hop++ {
					yard, findings := world.findings()
					if len(findings) == 0 {
						return
					}
					if hop == 8 {
						t.Fatalf("no convergence in 8 hops: %#v", world)
					}
					gate := world.gate()
					action, chosen := domain.NextStep(findings, attempted, gate.admits)
					rendered := domain.RenderRecovery(findings, action, chosen, 0)
					if action.Verb == domain.VerbNone {
						// A named terminal finding: a source predicate, the owner's own
						// words, or the finding the attempted command itself left.
						terminal := chosen.Kind == domain.FindingSource || chosen.Kind == domain.FindingOwner || chosen.Kind == domain.FindingPending ||
							chosen.Kind == domain.FindingFileUnreadable || slices.Equal(chosen.Remedies(attempted), []domain.Action{attempted})
						if !terminal {
							t.Fatalf("none without a terminal finding at %s: %#v", yard, chosen)
						}
						if chosen.Kind == domain.FindingOwner && rendered != "next: "+chosen.Retry {
							t.Fatalf("owner retry contradicted: %q", rendered)
						}
						return
					}
					if parsed, err := domain.ParseAction(domain.RenderAction(action)); err != nil || parsed != action {
						t.Fatalf("action %#v does not round-trip", action)
					}
					if strings.Count(rendered, "next: ") != 1 || !gate.admits(action) {
						t.Fatalf("advice %q is not one admitted next step", rendered)
					}
					if action.Yard != "" && gate.state(action.Yard).config && action.Verb != domain.VerbConfigValidate {
						t.Fatalf("invalid configuration advised %#v", action)
					}
					key := fmt.Sprintf("%#v|%#v", world, action)
					if seen[key] {
						t.Fatalf("repeated state and action %s", key)
					}
					seen[key] = true
					world.apply(action)
					attempted = action
				}
			})
		}
	}
}

// The step a refusal prints is the same typed choice for every entry point,
// so status and the repair it names never disagree.
func TestGateStepIsTheSameChoiceForStatusAndRefusal(t *testing.T) {
	gate := recoveryWorld{yards: map[string]yardGateState{"named": {drift: true}}, source: "tooling-dir"}.gate()
	refusal := yardGateRefusal("named", gate)
	step := gate.step("named", domain.Action{})
	if !strings.Contains(refusal, "next: "+step) || !strings.Contains(step, "failed: tooling-dir") || strings.Contains(step, "yard -Y named init") {
		t.Fatalf("refusal %q, step %q", refusal, step)
	}
	if gate.admits(domain.Action{Verb: domain.VerbInit, Yard: "named"}) || gate.admits(domain.Action{Verb: domain.VerbConfigApply, Yard: "named"}) {
		t.Fatal("a repair was admitted with a refused installed source")
	}
}
