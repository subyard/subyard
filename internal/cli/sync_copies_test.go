package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/state"
)

// Detects source deduplication and stale-preview rejection losing a concurrent copy.
func TestSyncCopiesAllocateAfterConcurrentPreviews(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	source := filepath.Join(root, "Demo")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	executions := make([]*projectExecution, 3)
	for i := range executions {
		program.env["SUBYARD_OPERATION_ID"] = fmt.Sprintf("copy-%d", i)
		executions[i], err = program.prepareProjectImport(ctx, loaded, "sync", []string{source})
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, execution := range executions {
		if err := program.reserveProjectExecution(ctx, execution); err != nil {
			t.Fatal(err)
		}
		want := []string{"Demo", "Demo-2", "Demo-3"}[i]
		if execution.Record.ProjectID != want || execution.Record.Name != want || execution.Record.YardPath != "/srv/workspaces/"+want+"/src" || execution.Environment["SUBYARD_PROJECT_ID"] != want {
			t.Fatalf("copy %d: %#v", i, execution)
		}
		if err := program.commitProjectExecution(ctx, execution); err != nil {
			t.Fatal(err)
		}
	}
	records, err := executions[0].Store.List(ctx)
	if err != nil || len(records) != 3 {
		t.Fatalf("records=%#v err=%v", records, err)
	}
	for _, record := range records {
		if record.HostPath != source {
			t.Fatalf("lost provenance: %#v", record)
		}
	}
	store := executions[0].Store
	if _, err := program.resolveLocalProject(ctx, loaded.Context, store, source); !errors.Is(err, state.ErrAmbiguous) {
		t.Fatalf("shared source selected an arbitrary copy: %v", err)
	}
	for _, name := range []string{"Demo", "Demo-2", "Demo-3"} {
		match, err := program.resolveLocalProject(ctx, loaded.Context, store, name)
		if err != nil || match.Record.ProjectID != name {
			t.Fatalf("selector %q: %#v %v", name, match, err)
		}
	}
	if err := (state.Service{Store: store}).RemoveProject(ctx, "Demo-2", state.SourceKey(source)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Demo", "Demo-3"} {
		if _, err := store.Get(ctx, name); err != nil {
			t.Fatalf("removal lost sibling %s: %v", name, err)
		}
	}
}

// Detects reuse of an unregistered retained workspace or interrupted transfer.
func TestSyncPreviewSkipsPhysicalWorkspaceNames(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	source := filepath.Join(root, "Demo")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
		ProjectData: projectActionObservationProbe{execute: func(ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
			return ports.InstanceExecResult{Stdout: []byte("Demo\x00Demo-2\x00")}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	execution, err := program.prepareProjectImport(ctx, loaded, "sync", []string{source})
	if err != nil {
		t.Fatal(err)
	}
	if err := program.observeProjectAction(ctx, "sync", execution); err != nil {
		t.Fatal(err)
	}
	if execution.Record.ProjectID != "Demo-3" {
		t.Fatalf("retained workspaces reused: %#v", execution.Record)
	}
	if err := program.reserveProjectExecution(ctx, execution); err != nil {
		t.Fatal(err)
	}
	defer program.abortProjectExecution(ctx, execution)
	if execution.Record.YardPath != "/srv/workspaces/Demo-3/src" {
		t.Fatal(execution.Record.YardPath)
	}
}
