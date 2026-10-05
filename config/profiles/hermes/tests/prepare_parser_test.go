package tests

import (
	"bufio"
	"bytes"
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
	registry, err := resource.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := domain.NewActionRegistry(registry.ActionDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	parse := func(observation, verb string) resource.PrepareResult {
		t.Helper()
		payload, err := os.ReadFile(filepath.Join(output, observation+".json"))
		if err != nil {
			t.Fatal(err)
		}
		plan, err := registry.PrepareResult(actions, "dashboard", verb, payload)
		if err != nil {
			t.Fatalf("CLI prepare parser rejected native %s assessment: %v", observation, err)
		}
		return plan
	}
	command := exec.Command("bash", filepath.Join(root, "config/profiles/hermes/tests/hermes-dashboard-resource.sh"), "--prepare-parser-fixture", output)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	notifications, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = input.Close()
		_ = command.Process.Kill()
	})
	scanner := bufio.NewScanner(notifications)
	expect := func(notification string) {
		t.Helper()
		if !scanner.Scan() || scanner.Text() != notification {
			t.Fatalf("native fixture expected %q, got %q: %v\n%s", notification, scanner.Text(), scanner.Err(), &stderr)
		}
	}
	for _, verb := range []string{"up", "down"} {
		expect(verb + "-plan")
		approved := parse(verb, verb)
		if !approved.Assessment.Changed || len(approved.Steps) != 1 || approved.Steps[0].Decision != domain.StepApply {
			t.Fatalf("native %s did not capture a route mutation before apply", verb)
		}
		if _, err := input.Write([]byte("apply\n")); err != nil {
			t.Fatal(err)
		}
		expect(verb + "-verified")
		verified := parse(verb+"-verified", verb)
		if verified.Assessment.Changed || verified.Steps[0].Decision != domain.StepSkip || verified.Binding != approved.Binding {
			t.Fatalf("native %s verify did not converge within the approved binding", verb)
		}
		if err := domain.CheckOperationSteps(approved.Steps, verified.Steps); err != nil {
			t.Fatalf("native %s apply-to-verify changed approved scope: %v", verb, err)
		}
	}
	_ = input.Close()
	if err := command.Wait(); err != nil {
		t.Fatalf("native prepare fixture: %v\n%s", err, &stderr)
	}
	for _, observation := range []string{"up", "up-noop", "down", "down-noop"} {
		verb := strings.TrimSuffix(observation, "-noop")
		plan := parse(observation, verb)
		projected, err := actions.Assess(plan.Assessment.Action, domain.ActionDelta{
			Changed: plan.Assessment.Changed, Consequences: domain.OperationStepConsequences(plan.Steps),
		})
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
		noOp := strings.HasSuffix(observation, "-noop")
		if projected.Changed == noOp || (noOp && policy != domain.ActionConfirmationNever) || (!noOp && policy != domain.ActionConfirmationPromptDefaultYes) {
			t.Fatalf("native %s projection changed confirmation semantics: changed=%t policy=%s", observation, projected.Changed, policy)
		}
		if len(plan.Steps) != 1 || !strings.HasPrefix(plan.Steps[0].Target, "incus:subyard-hermes-bootstrap-5b9fe2b7ea30/yard-hermes-bootstrap-5b9fe2b7ea30/") {
			t.Fatalf("native route context was lost: %#v", plan.Steps)
		}
	}
}
