package tests

import (
	"os"
	"os/exec"
	"path/filepath"
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
	registry, err := resource.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := domain.NewActionRegistry(registry.ActionDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		script, command string
		verbs           []string
	}{
		{"qa-lifecycle-exact.sh", "qa-pool", []string{"expose", "down", "destroy", "destroy-purge"}},
		{"staging-exact.sh", "staging", []string{"stop", "down", "destroy", "destroy-purge"}},
	} {
		t.Run(fixture.command, func(t *testing.T) {
			output := testkit.TempDir(t)
			command := exec.Command("bash", filepath.Join(root, "config/profiles/openclaw/tests", fixture.script), "--prepare-parser-fixture", output)
			if log, err := command.CombinedOutput(); err != nil {
				t.Fatalf("native prepare fixture: %v\n%s", err, log)
			}
			for _, action := range fixture.verbs {
				payload, err := os.ReadFile(filepath.Join(output, action+".json"))
				if err != nil {
					t.Fatal(err)
				}
				verb := strings.TrimSuffix(action, "-purge")
				plan, err := registry.PrepareResult(actions, fixture.command, verb, payload)
				if err != nil {
					t.Fatalf("CLI prepare parser rejected native %s assessment: %v", action, err)
				}
				if _, err := actions.Assess(plan.Assessment.Action, domain.ActionDelta{
					Changed: plan.Assessment.Changed, Consequences: domain.OperationStepConsequences(plan.Steps),
				}); err != nil {
					t.Fatalf("native %s step projection cannot form an action plan: %v", action, err)
				}
				for _, step := range plan.Steps {
					if step.Decision != domain.StepSkip && step.Consequence == "" {
						t.Fatalf("native changed target lacks a consequence: %s", step.ID)
					}
					if !strings.HasPrefix(step.Target, "incus:subyard-openclaw-bootstrap-5b9fe2b7ea30/yard-openclaw-bootstrap-5b9fe2b7ea30") {
						t.Fatalf("native target context was lost: %q", step.Target)
					}
				}
			}
		})
	}
}
