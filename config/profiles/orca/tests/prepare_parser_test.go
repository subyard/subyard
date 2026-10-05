package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestNativePrepareAcceptsHashedIncusContext(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	output := testkit.TempDir(t)
	command := exec.Command("bash", filepath.Join(root, "config/profiles/orca/tests/orca-profile-resource.sh"), "--prepare-parser-fixture", output)
	if log, err := command.CombinedOutput(); err != nil {
		t.Fatalf("native prepare fixture: %v\n%s", err, log)
	}
	registry, err := resource.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := domain.NewActionRegistry(registry.ActionDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	for _, observation := range []string{"up", "up-noop", "restart", "down", "down-noop", "restart-verified", "sync", "sync-verified"} {
		verb := strings.TrimSuffix(strings.TrimSuffix(observation, "-verified"), "-noop")
		payload, err := os.ReadFile(filepath.Join(output, observation+".json"))
		if err != nil {
			t.Fatal(err)
		}
		plan, err := registry.PrepareResult(actions, "orca", verb, payload)
		if err != nil {
			t.Fatalf("CLI prepare parser rejected native %s assessment: %v", verb, err)
		}
		consequences := domain.OperationStepConsequences(plan.Steps)
		projected, err := actions.Assess(plan.Assessment.Action, domain.ActionDelta{Changed: plan.Assessment.Changed, Consequences: consequences})
		if err != nil {
			t.Fatalf("native %s step projection cannot form an action plan: %v", observation, err)
		}
		if !slices.Equal(projected.Consequences, plan.Assessment.Consequences) {
			t.Fatalf("native %s global and per-target consequences disagree", observation)
		}
		policy, _, err := actions.Resolve(projected)
		if err != nil {
			t.Fatal(err)
		}
		noOp := strings.HasSuffix(observation, "-noop") || strings.HasSuffix(observation, "-verified")
		never := noOp || verb == "sync"
		if projected.Changed == noOp || (never && policy != domain.ActionConfirmationNever) || (!never && policy != domain.ActionConfirmationPromptDefaultYes) {
			t.Fatalf("native %s projection changed confirmation semantics: changed=%t policy=%s", observation, projected.Changed, policy)
		}
		definition, _ := registry.Lookup("orca")
		alias, err := registry.PrepareResult(actions, definition.Name, verb, payload)
		if err != nil || alias.Assessment.Action != projected.Action {
			t.Fatalf("native resource alias changed action metadata: %v", err)
		}
		for _, step := range plan.Steps {
			if step.Decision != domain.StepSkip && step.Consequence == "" {
				t.Fatalf("native changed target lacks a consequence: %s", step.ID)
			}
			if !strings.HasPrefix(step.Target, "incus:subyard-orca-bootstrap-5b9fe2b7ea30/yard-orca-bootstrap-5b9fe2b7ea30/") {
				t.Fatalf("native target context was lost: %q", step.Target)
			}
			if observation == "restart" && step.ID == "service" && step.Observed != "invocation:000000000000000000000000deadbeef" {
				t.Fatalf("native invocation identity was lost: %q", step.Observed)
			}
			if observation == "restart-verified" && (step.Decision != domain.StepSkip || step.Observed != step.Desired) {
				t.Fatalf("native restart guard/verify did not converge: %#v", step)
			}
			if observation == "down" && step.ID == "route" && !strings.HasPrefix(step.Observed, "sha256:") {
				t.Fatalf("native route fingerprint was lost: %q", step.Observed)
			}
		}
		if verb == "up" {
			unlabeled := strings.ReplaceAll(string(payload), "incus:", "")
			if _, err := registry.PrepareResult(actions, "orca", verb, []byte(unlabeled)); err == nil {
				t.Fatal("protected opaque-payload filter accepted unlabeled native bootstrap context")
			}
		}
	}
}

func TestNativePrepareStartupRetainsBindingWithDrainingIncusAcrossSourceRelocation(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fixture := testkit.TempDir(t)
	var handlers []string
	for _, name := range []string{"relocated", "changed"} {
		copyRoot := filepath.Join(fixture, name)
		if err := os.MkdirAll(filepath.Join(copyRoot, "config", "profiles"), 0700); err != nil {
			t.Fatal(err)
		}
		for _, relative := range []string{"scripts", "config/profiles/orca"} {
			command := exec.Command("cp", "-a", filepath.Join(root, relative), filepath.Join(copyRoot, relative))
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("copy native source fixture: %v\n%s", err, output)
			}
		}
		dispatcher, err := os.ReadFile(filepath.Join(root, "config", "projects-changed.sh"))
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(copyRoot, "config", "projects-changed.sh"), dispatcher, 0755)
		handlers = append(handlers, filepath.Join(copyRoot, "config/profiles/orca/resources/orca/handler.sh"))
	}
	changed, err := os.ReadFile(handlers[1])
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, handlers[1], append(changed, []byte("\n# changed native source fixture\n")...), 0755)
	output := testkit.TempDir(t)
	command := exec.Command("bash", filepath.Join(root, "config/profiles/orca/tests/orca-profile-resource.sh"),
		"--startup-binding-fixture", output, handlers[0], handlers[1])
	if log, err := command.CombinedOutput(); err != nil {
		t.Fatalf("native startup/relocation/tamper fixture: %v\n%s", err, log)
	}
	registry, err := resource.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := domain.NewActionRegistry(registry.ActionDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	var approved resource.PrepareResult
	for _, state := range []string{"cold", "running", "relocated", "verified"} {
		payload, err := os.ReadFile(filepath.Join(output, state+".json"))
		if err != nil {
			t.Fatal(err)
		}
		plan, err := registry.PrepareResult(actions, "orca", "up", payload)
		if err != nil {
			t.Fatalf("CLI prepare parser rejected native %s assessment: %v", state, err)
		}
		if state == "cold" {
			approved = plan
			if len(approved.Steps) != 12 {
				t.Fatalf("cold plan omitted native target scope: %d steps", len(approved.Steps))
			}
			for _, step := range plan.Steps {
				if step.Decision != domain.StepConditional {
					t.Fatalf("cold guest observation was not conditional: %#v", step)
				}
			}
			continue
		}
		if approved.Binding != plan.Binding {
			t.Fatalf("native source relocation/startup changed retained binding at %s", state)
		}
		if err := domain.CheckOperationSteps(approved.Steps, plan.Steps); err != nil {
			t.Fatalf("native startup expanded approved scope at %s: %v", state, err)
		}
		if state == "verified" && plan.Assessment.Changed {
			t.Fatal("native post-start resource effects did not converge")
		}
	}
}
