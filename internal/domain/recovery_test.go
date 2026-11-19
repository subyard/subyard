package domain

import (
	"fmt"
	"strings"
	"testing"
)

func TestRecoveryActionsRoundTrip(t *testing.T) {
	for verb := range verbWords {
		for _, yard := range []string{"", "named"} {
			action := Action{Verb: verb, Yard: yard}
			if verb.Host() {
				action.Yard = ""
			}
			parsed, err := ParseAction(RenderAction(action))
			if err != nil || parsed != action {
				t.Fatalf("%#v rendered %q parsed %#v %v", action, RenderAction(action), parsed, err)
			}
		}
	}
	update := Action{Verb: VerbUpdate, Version: "0.30.0"}
	if parsed, err := ParseAction(RenderAction(update)); err != nil || parsed != update {
		t.Fatalf("update version: %#v %v", parsed, err)
	}
	for _, text := range []string{"yard -Y named migrate", "yard -Y ../x init", "yard  init", "yard init; rm -rf /", "yard update --version x", "sudo yard init"} {
		if _, err := ParseAction(text); err == nil {
			t.Fatalf("parsed %q", text)
		}
	}
}

func TestOwnerRetryKeepsUnparsedTextVerbatim(t *testing.T) {
	for retry, want := range map[string]Action{
		"yard -Y named integration status":                  {Verb: VerbIntegrationStatus, Yard: "named"},
		"run yard migrate":                                  {Verb: VerbMigrate},
		"restore the settings, then run yard -Y named init": {Verb: VerbInit, Yard: "named"},
	} {
		if action, _, ok := ParseRetry(retry); !ok || action != want {
			t.Fatalf("%q: %#v", retry, action)
		}
	}
	owner := Finding{Kind: FindingOwner, Retry: "run yard update for a fresh plan and confirmation"}
	drift := Finding{Kind: FindingDrift, Yard: "named"}
	action, chosen := NextStep([]Finding{drift, owner}, Action{}, nil)
	if action.Verb != VerbNone || chosen != owner {
		t.Fatalf("a lower remedy contradicted the owner: %#v %#v", action, chosen)
	}
	if text := RenderRecovery([]Finding{drift, owner}, action, chosen, 0); text != "next: run yard update for a fresh plan and confirmation" {
		t.Fatalf("owner retry not rendered verbatim: %q", text)
	}
}

func TestNextStepNeverRepeatsTheAttemptedCommand(t *testing.T) {
	drift := Finding{Kind: FindingDrift, Yard: "named"}
	if action, _ := NextStep([]Finding{drift}, Action{Verb: VerbInit, Yard: "named"}, nil); action.Verb != VerbNone {
		t.Fatalf("attempted init repeated: %#v", action)
	}
	// A file that changed after the plan is a new observation: rerun the apply.
	changed := Finding{Kind: FindingFileChanged, Yard: "named"}
	if action, _ := NextStep([]Finding{changed}, Action{Verb: VerbConfigApply, Yard: "named"}, nil); action.Verb != VerbConfigApply {
		t.Fatalf("changed file did not re-plan: %#v", action)
	}
	unreachable := Finding{Kind: FindingUnreachable, Yard: "named"}
	var sequence []string
	for attempted, hops := (Action{}), 0; hops < 3; hops++ {
		attempted, _ = NextStep([]Finding{unreachable}, attempted, nil)
		sequence = append(sequence, RenderAction(attempted))
	}
	if got := strings.Join(sequence, ", "); got != "yard migrate, yard -Y named stop --force, yard -Y named start" {
		t.Fatalf("unreachable sequence: %s", got)
	}
}

// The readiness RPC bounds message and next to 8192 characters.
func TestRecoveryRenderingFitsTheReadinessBudget(t *testing.T) {
	var findings []Finding
	for index := range 162 {
		findings = append(findings, Finding{Kind: FindingDrift, Yard: fmt.Sprintf("yard%03d", index),
			Public: fmt.Sprintf("yard yard%03d: materialized-config drift in %s", index, strings.Repeat("x", 40))})
	}
	action, chosen := NextStep(findings, Action{}, nil)
	text := RenderRecovery(findings, action, chosen, 8192)
	if len(text) > 8192 || !strings.HasSuffix(text, "next: yard -Y yard000 init") || !strings.Contains(text, "more; yard migrate --check)") {
		t.Fatalf("budget: %d bytes, tail %q", len(text), text[max(0, len(text)-120):])
	}
}
