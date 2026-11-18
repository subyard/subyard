package application

import (
	"errors"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func TestProjectRemovalAssessmentRejectsNewWorkspace(t *testing.T) {
	presence := "missing"
	data := &projectDataStub{run: func(ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		return ports.InstanceExecResult{Stdout: []byte(presence)}, nil
	}}
	runner := ProjectActionRunner{
		Data:    data,
		Yard:    domain.Context{AccessKind: domain.AccessRemote},
		Project: domain.ProjectRecord{YardPath: "/srv/workspaces/demo/src"},
	}

	var observed ProjectRemovalObservation
	var approved *ProjectRemovalObservation
	action := domain.ActionID("project.remove-workspace")
	if err := runner.ObserveRemoval(t.Context(), action, &observed, &approved); err != nil {
		t.Fatal(err)
	}
	if approved == nil || !approved.WorkspaceChecked || approved.WorkspacePresent {
		t.Fatalf("approval = %#v, want checked and absent workspace", approved)
	}

	presence = "present"
	err := runner.ObserveRemoval(t.Context(), action, &observed, &approved)
	if !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("reassessment error = %v, want stale plan", err)
	}
	if !observed.WorkspacePresent || approved.WorkspacePresent {
		t.Fatalf("observed = %#v, approval = %#v, want original approval preserved", observed, approved)
	}
}
