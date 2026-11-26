package domain

import (
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
	if text := RenderNext(action, chosen, Action{}); text != "run yard update for a fresh plan and confirmation" {
		t.Fatalf("owner retry not rendered verbatim: %q", text)
	}
	// A retry that repeats the attempted command is never printed as its next step.
	pending := Finding{Kind: FindingPending, Yard: "named", Retry: "run yard -Y named init"}
	attempted := Action{Verb: VerbInit, Yard: "named"}
	if action, chosen := NextStep([]Finding{pending}, attempted, nil); strings.Contains(RenderNext(action, chosen, attempted), "init") {
		t.Fatalf("attempted retry repeated: %q", RenderNext(action, chosen, attempted))
	}
}

func TestNextStepNeverRepeatsTheAttemptedCommand(t *testing.T) {
	drift := Finding{Kind: FindingDrift, Yard: "named"}
	// Drift that init left is named by config status, not by init again.
	if action, _ := NextStep([]Finding{drift}, Action{Verb: VerbInit, Yard: "named"}, nil); action != (Action{Verb: VerbConfigStatus, Yard: "named"}) {
		t.Fatalf("attempted init repeated: %#v", action)
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

func TestVerbWireNamesArePinned(t *testing.T) {
	want := map[Verb]string{
		VerbInit: "init", VerbConfigApply: "config-apply", VerbConfigStatus: "config-status",
		VerbConfigValidate: "config-validate", VerbMigrate: "migrate", VerbMigrateCheck: "migrate-check",
		VerbUpdate: "update", VerbUpdateCheck: "update-check", VerbUpdateRollback: "update-rollback",
		VerbStart: "start", VerbStopForce: "stop-force", VerbIntegrationStatus: "integration-status",
	}
	if len(want) != len(verbWords) || VerbName(VerbNone) != "" {
		t.Fatalf("verbs %d, pinned %d", len(verbWords), len(want))
	}
	for verb, name := range want {
		if VerbName(verb) != name {
			t.Fatalf("verb %d = %q, want %q", verb, VerbName(verb), name)
		}
	}
}
