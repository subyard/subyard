package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

func TestRollbackAgentCleanupIsVisibleWithThePlanAndResult(t *testing.T) {
	for _, action := range []domain.ActionID{"update.rollback", releaseruntime.PrepareRollbackAction} {
		t.Run(string(action), func(t *testing.T) {
			var output bytes.Buffer
			cli := &CLI{options: Options{Stdout: &output}}
			execution := &releaseExecution{prepared: releaseruntime.Prepared{
				Action:       action,
				AgentCleanup: []releasetransition.RollbackBoundary{{Reason: "approval policy survives rollback", Recovery: "stop agents and restore the previous managed policy", Files: []string{"/etc/sample/48-policy.json", "/etc/sample/49-policy.json", "/etc/sample/51-policy.json"}}},
			}}
			// The same rendering accompanies a displayed operation plan, including --yes.
			cli.printUpdateAgentCleanup(execution)
			plan := output.String()
			output.Reset()
			cli.printUpdateResult(execution, true)
			for _, rendered := range []string{plan, output.String()} {
				for _, want := range []string{"manual cleanup; rollback proceeds", "/etc/sample/48-policy.json", "/etc/sample/49-policy.json", "/etc/sample/51-policy.json", "stop agents and restore the previous managed policy"} {
					if !strings.Contains(rendered, want) {
						t.Fatalf("missing %q from %s", want, rendered)
					}
				}
			}
		})
	}
}
