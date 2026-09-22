package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/state"
)

// Detects source deduplication and stale-preview rejection losing a concurrent copy.
func TestProjectCopiesAllocateAfterConcurrentPreviews(t *testing.T) {
	for _, command := range []string{"sync", "clone"} {
		t.Run(command, func(t *testing.T) {
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
				if command == "clone" {
					executions[i], err = program.prepareProjectClone(ctx, loaded, []string{source})
				} else {
					executions[i], err = program.prepareProjectImport(ctx, loaded, command, []string{source})
				}
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
		})
	}
}

// Detects reuse of an unregistered retained workspace or interrupted transfer.
func TestProjectPreviewSkipsPhysicalWorkspaceNames(t *testing.T) {
	for _, command := range []string{"sync", "clone"} {
		t.Run(command, func(t *testing.T) {
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
			var execution *projectExecution
			if command == "clone" {
				execution, err = program.prepareProjectClone(ctx, loaded, []string{source})
			} else {
				execution, err = program.prepareProjectImport(ctx, loaded, command, []string{source})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := program.observeProjectAction(ctx, command, execution); err != nil {
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
		})
	}
}

func TestRemoteCopyReservationHandlesConcurrentNamesAndLegacyOwners(t *testing.T) {
	for _, mode := range []domain.ProjectMode{domain.ProjectSync, domain.ProjectGit} {
		for _, legacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/legacy=%t", mode, legacy), func(t *testing.T) {
				record := domain.ProjectRecord{
					Schema: 1, IdentityVersion: 2, ProjectID: "Demo", Name: "Demo",
					HostPath: "https://example.invalid/Demo.git", YardPath: state.YardPath("Demo"),
					Mode: mode, SSHHost: "yard", Target: "yard",
				}
				response := map[string]any{"projectId": "Demo-2", "name": "Demo-2", "reserved": true}
				if legacy {
					response = map[string]any{"projectId": "Demo", "name": "Demo", "existing": record}
				}
				payload, err := json.Marshal(response)
				if err != nil {
					t.Fatal(err)
				}
				fakeBin := t.TempDir()
				writeCLIFile(t, filepath.Join(fakeBin, "ssh"), "#!/bin/sh\ncat <<'JSON'\n"+string(payload)+"\nJSON\n", 0o700)
				program := &CLI{
					options: Options{WorkingDir: t.TempDir(), Stderr: io.Discard},
					env:     map[string]string{"PATH": os.Getenv("PATH"), "SUBYARD_OPERATION_ID": "copy-operation"},
				}
				t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
				execution := &projectExecution{
					Loaded: config.Loaded{Context: domain.Context{AccessKind: domain.AccessRemote, OwnerEndpoint: "dev@owner.example"}},
					Record: record, Commit: projectCommitPut, OperationID: "copy-operation", RequestedName: "Demo",
				}
				err = program.reserveProjectExecution(context.Background(), execution)
				if legacy {
					if err == nil || !strings.Contains(err.Error(), "update the owner runtime") || execution.RemoteReserved {
						t.Fatalf("legacy owner reservation accepted: %#v, %v", execution, err)
					}
					_, err = program.previewProjectAdmission(context.Background(), execution.Loaded, nil, record.HostPath, mode, "Demo", false)
					if err == nil || !strings.Contains(err.Error(), "update the owner runtime") {
						t.Fatalf("legacy owner preview accepted: %v", err)
					}
					return
				}
				if err != nil || !execution.RemoteReserved || execution.Record.ProjectID != "Demo-2" ||
					execution.Record.YardPath != state.YardPath("Demo-2") || execution.Environment["SUBYARD_PROJECT_ID"] != "Demo-2" {
					t.Fatalf("concurrent copy reservation = %#v, %v", execution, err)
				}
			})
		}
	}
}
