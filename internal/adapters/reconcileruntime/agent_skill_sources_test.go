package reconcileruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestAgentSkillsReadinessUsesCapturedSourcesAndGuestProbe(t *testing.T) {
	for _, test := range []struct {
		output     string
		want, fail bool
	}{{"skills-current\n", true, false}, {"skills-stale\n", false, false}, {"unknown\n", false, true}} {
		incus := &testkit.Incus{ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{Stdout: []byte(test.output)}}}}
		runtime := Runtime{RepositoryRoot: "../../..", Executor: incus, Yard: domain.Context{IncusProject: "p", YardInstanceName: "i", DevUser: "fixture-user"}, Environment: []string{"ENVIRONMENT_PROFILES=synthetic"}}
		paths, err := runtime.agentSkillSources()
		if err != nil {
			t.Fatal(err)
		}
		current, err := runtime.agentSkillsConverged(context.Background(), paths)
		if current != test.want || (err != nil) != test.fail {
			t.Fatalf("%q current=%v err=%v", test.output, current, err)
		}
		request := incus.ExecCalls[0].Request
		if len(request.Command) != 4 || request.Command[0] != "python3" || !strings.Contains(request.Command[3], "skills-current") {
			t.Fatalf("wrong native probe: %v", request.Command)
		}
		var input struct {
			Files          map[string][]byte
			User, Profiles string
			Coding         bool
		}
		if err := json.Unmarshal(request.Stdin, &input); err != nil {
			t.Fatal(err)
		}
		if input.User != "fixture-user" || input.Profiles != "synthetic" || !input.Coding || len(input.Files["config/skills/deliver.py"]) == 0 || len(input.Files["config/skills/core/subyard-preview/SKILL.md"]) == 0 {
			t.Fatal("probe omitted selected source or runtime inputs")
		}
	}
}
