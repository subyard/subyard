package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// A converged stage's readiness diagnostic is rendered with its observed state;
// it neither adds work nor appears on a stage that still has work.
func TestInitPlanRendersConvergedStageDiagnostics(t *testing.T) {
	yard := domain.Context{YardName: "vm", YardKind: domain.YardVM, IncusProject: "subyard", YardInstanceName: "yard"}
	var instance, power application.ReconcileStage
	for _, stage := range application.InitStages(yard) {
		switch stage.ID {
		case ports.ReconcileStageInstance:
			instance = stage
		case ports.ReconcileStagePower:
			power = stage
		}
	}
	const diagnostic = "headless VM device policy applies at the next yard start"
	execution := &initExecution{mode: initReconcile, loaded: config.Loaded{Context: yard}, hostID: "host"}
	execution.plan = application.ReconcilePlan{Steps: []application.ReconcileStep{
		{Stage: instance, Converged: true, Diagnostic: diagnostic},
		{Stage: power, Diagnostic: diagnostic},
	}}
	steps := execution.operationSteps()
	if err := domain.ValidateOperationSteps(steps); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	(&CLI{options: Options{Stdout: &output}}).printOperationSteps(domain.OperationPlan{OperationID: "operation", Steps: steps})
	rendered := output.String()
	if !strings.Contains(rendered, "[skip] init.stage.instance: subyard/yard\n    observed: converged; "+diagnostic+"\n") {
		t.Fatalf("converged instance stage lost its diagnostic:\n%s", rendered)
	}
	if strings.Count(rendered, diagnostic) != 1 {
		t.Fatalf("diagnostic was rendered on a stage with work:\n%s", rendered)
	}
	if consequences := domain.OperationStepConsequences(steps); strings.Contains(strings.Join(consequences, "\n"), diagnostic) {
		t.Fatalf("diagnostic became a consequence: %v", consequences)
	}
}
