package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/state"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProjectTransferPlanUsesOwnerConsentAndPreservesScope(t *testing.T) {
	owner := domain.OperationPlan{OperationID: "owner-copy", Command: "sync", Target: domain.TargetRemoteOwner, Confirmed: true,
		Steps:      []domain.OperationStep{{ID: "copy", Target: "/srv/projects/Demo"}},
		Assessment: &domain.ActionAssessment{}, ConfirmationRequest: &domain.ConfirmationRequest{}}
	transfer := projectTransferPlan(owner)
	if transfer.OperationID != owner.OperationID || transfer.Target != owner.Target || !transfer.Confirmed || len(transfer.Steps) != 1 || !reflect.DeepEqual(transfer.Steps[0], owner.Steps[0]) {
		t.Fatal("controller transfer lost approved owner scope")
	}
	if transfer.Assessment != nil || transfer.ConfirmationRequest != nil || owner.Assessment == nil || owner.ConfirmationRequest == nil {
		t.Fatal("controller transfer must retain owner authorization without local reassessment")
	}
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{OperationID: owner.OperationID, Status: "ok"}}}}
	orchestrator := application.Orchestrator{Runner: runner, Clock: testkit.NewManualClock(time.Unix(0, 0))}
	if _, _, err := orchestrator.RunAdapter(context.Background(), transfer, domain.AdapterRequest{OperationID: owner.OperationID}, nil); err != nil {
		t.Fatal(err)
	}
	transfer.Confirmed = false
	if _, _, err := orchestrator.RunAdapter(context.Background(), transfer, domain.AdapterRequest{OperationID: owner.OperationID}, nil); err == nil {
		t.Fatal("unconfirmed transfer accepted")
	}
}

func ownerProjectCopyFixture(t *testing.T) (*CLI, *preparedCommand, *bool) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	manifestPath := filepath.Join(root, "config", "commands.registry")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = append(manifest, []byte("sync||@project||local|mutate|dynamic|public|projects|project-target|sync [path]|sync|--name --target --yes --help|\n")...)
	testkit.WriteFile(t, manifestPath, manifest, 0o600)
	contentChanged := false
	var prepared *preparedCommand
	probe := projectActionObservationProbe{execute: func(request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		if len(request.Command) >= 3 && strings.Contains(request.Command[2], "-printf") {
			return ports.InstanceExecResult{Stdout: []byte("f\x00snapshot.txt\x00\x00")}, nil
		}
		if len(request.Command) >= 3 && strings.Contains(request.Command[2], "sha256sum --zero") {
			content := "archive"
			if contentChanged {
				content = "unexpected edit"
			}
			return ports.InstanceExecResult{Stdout: []byte(fmt.Sprintf("%x  ./snapshot.txt\x00", sha256.Sum256([]byte(content))))}, nil
		}
		if len(request.Command) > 0 && request.Command[0] == "cat" {
			payload, err := application.ProjectMetadata(prepared.Project.Record, prepared.Loaded.Context.YardName)
			return ports.InstanceExecResult{Stdout: payload}, err
		}
		return ports.InstanceExecResult{}, nil
	}}
	program, err := New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root, ProjectData: probe})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := projectActionArchive("archive").Open(context.Background(), "/controller/source")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := application.ProjectArchiveContentDigest(archive, false)
	if err := errors.Join(err, archive.Close()); err != nil {
		t.Fatal(err)
	}
	source := ProjectCopySourceDescriptor{Schema: 1, Source: "/controller/source", ArchiveDigest: fmt.Sprintf("%x", sha256.Sum256([]byte("retained archive"))), ArchiveSize: 2048, TreeDigest: tree, RequestedName: "Demo", TargetProfile: "yard"}
	prepared, err = program.prepareOwnerProjectCopy(context.Background(), loaded, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := prepared.Close(); err != nil {
			t.Fatal(err)
		}
	})
	return program, prepared, &contentChanged
}

func TestProjectOwnerCopyPrepareDoesNotReserveOrOpenControllerSource(t *testing.T) {
	program, prepared, _ := ownerProjectCopyFixture(t)
	if prepared.Project.ownerCopy.reservation != nil || prepared.Project.Commit != projectCommitNone {
		t.Fatal("copy prepare acquired an admission or enabled early commit")
	}
	if _, err := os.Stat(filepath.Join(prepared.Loaded.Context.Paths.StateDir, ".reservations")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepare published reservations: %v", err)
	}
	if _, err := program.beginOwnerProjectCopy(context.Background(), prepared); err == nil {
		t.Fatal("copy admitted without confirmation")
	}
}

func TestProjectOwnerCopyFinalizesOnlyAfterIndependentContentVerification(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			program, prepared, changed := ownerProjectCopyFixture(t)
			prepared.Plan.Confirmed = true
			if _, err := program.beginOwnerProjectCopy(context.Background(), prepared); err != nil {
				t.Fatal(err)
			}
			if _, err := prepared.Project.Store.GetReadOnly(context.Background(), prepared.Project.Record.ProjectID); !errors.Is(err, state.ErrNotFound) {
				t.Fatalf("begin prematurely registered project: %v", err)
			}
			*changed = corrupt
			_, err := program.finalizeOwnerProjectCopy(context.Background(), prepared)
			if corrupt {
				if !errors.Is(err, domain.ErrPlanStale) {
					t.Fatalf("unverified content accepted: %v", err)
				}
				if _, err := prepared.Project.Store.GetReadOnly(context.Background(), prepared.Project.Record.ProjectID); !errors.Is(err, state.ErrNotFound) {
					t.Fatalf("unverified project registered: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := prepared.Project.Store.GetReadOnly(context.Background(), prepared.Project.Record.ProjectID); err != nil {
					t.Fatal(err)
				}
				if _, err := program.finalizeOwnerProjectCopy(context.Background(), prepared); err == nil {
					t.Fatal("copy finalization replay accepted")
				}
			}
		})
	}
}

func TestProjectOwnerCopyCloseAbortsAdmission(t *testing.T) {
	program, prepared, _ := ownerProjectCopyFixture(t)
	prepared.Plan.Confirmed = true
	if _, err := program.beginOwnerProjectCopy(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if prepared.Project.ownerCopy.reservation != nil {
		t.Fatal("closed owner copy retains admission")
	}
	if _, err := prepared.Project.Store.GetReadOnly(context.Background(), prepared.Project.Record.ProjectID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("aborted copy published project: %v", err)
	}
}
