package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/state"
	"github.com/Subyard/Subyard/internal/testkit"
)

type forbiddenExportArchive struct{ opens *int }

func (archive forbiddenExportArchive) Open(context.Context, string) (io.ReadCloser, error) {
	*archive.opens++
	return nil, errors.New("owner must not open a controller source")
}

func TestProjectExportRPCUsesExistingSingleUseTransferLifecycle(t *testing.T) {
	for _, outcome := range []string{"finalize", "corrupt", "disconnect", "shrink"} {
		t.Run(outcome, func(t *testing.T) {
			program, prepared, facts := ownerProjectExportFixture(t)
			now := time.Now()
			exact := bindExactOperationPlan(prepared, now)
			handler := &rpcHandler{cli: program, loaded: prepared.Loaded, plans: map[string]*preparedCommand{prepared.Plan.OperationID: prepared}, exactPlans: map[string]exactOperationPlan{prepared.Plan.OperationID: exact}, clock: func() time.Time { return now }}
			t.Cleanup(handler.closePlans)
			if outcome == "shrink" {
				facts.guest = facts.baseline
			}
			params, _ := json.Marshal(map[string]any{"confirmed": true, "digest": exact.Digest})
			call := rpc.Call{OperationID: prepared.Plan.OperationID, Method: "operation.execute", Params: params}
			if _, err := handler.Handle(context.Background(), call, func(string, any) (uint64, error) { return 1, nil }); err != nil {
				t.Fatal(err)
			}
			if len(handler.plans) != 0 || len(handler.activeProjectCopies) != 1 {
				t.Fatal("export did not retain one native transfer after authorization")
			}
			if _, err := handler.Handle(context.Background(), call, nil); err == nil {
				t.Fatal("export execution replay accepted")
			}
			if outcome == "disconnect" {
				handler.closePlans()
				if facts.cleanup != 1 {
					t.Fatal("disconnect leaked owned temporary")
				}
				return
			}
			if outcome == "corrupt" {
				facts.baseline = "corrupt streamed archive"
			}
			params, _ = json.Marshal(map[string]any{"digest": exact.Digest})
			call.Method, call.Params = "project.copy.finalize", params
			_, err := handler.Handle(context.Background(), call, nil)
			if outcome == "corrupt" {
				var protocol *rpc.Error
				if !errors.As(err, &protocol) || protocol.Code != domain.PlanStaleCode {
					t.Fatalf("corrupt export finalized: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(handler.activeProjectCopies) != 0 {
				t.Fatal("finalization retained replayable export")
			}
			if outcome == "shrink" {
				if facts.temporary != 0 || facts.cleanup != 0 {
					t.Fatal("converged export created temporary work")
				}
			} else if facts.cleanup != 1 {
				t.Fatal("finalization leaked owned export snapshot")
			}
			if _, err := handler.Handle(context.Background(), call, nil); err == nil {
				t.Fatal("export finalization replay accepted")
			}
		})
	}
}

type exportOwnerFacts struct {
	guest, baseline                  string
	temporary, cleanup, archiveOpens int
	cleanupFails                     bool
}

func ownerProjectExportFixture(t *testing.T) (*CLI, *preparedCommand, *exportOwnerFacts) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	manifestPath := filepath.Join(root, "config", "commands.registry")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, manifestPath, append(manifest, []byte("export||@project||local|mutate|dynamic|public|projects|path|export [path]|export|--yes --help|\n")...), 0o600)
	facts := &exportOwnerFacts{guest: "yard change", baseline: "controller source"}
	probe := projectActionObservationProbe{execute: func(request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		if len(request.Command) >= 3 {
			script := request.Command[2]
			if strings.Contains(script, "mkdir -m 700") {
				facts.temporary++
				return ports.InstanceExecResult{Stdout: []byte("1:2:1000\n")}, nil
			}
			if strings.Contains(script, "rm -rf") {
				facts.cleanup++
				if facts.cleanupFails {
					return ports.InstanceExecResult{ExitCode: 1}, errors.New("cleanup refused")
				}
				return ports.InstanceExecResult{}, nil
			}
			if strings.Contains(script, "-printf") {
				return ports.InstanceExecResult{Stdout: []byte("f\x00snapshot.txt\x00\x00")}, nil
			}
			if strings.Contains(script, "sha256sum --zero") {
				content := facts.guest
				if strings.HasPrefix(request.Command[4], "/tmp/subyard-export-") {
					content = facts.baseline
				}
				return ports.InstanceExecResult{Stdout: []byte(fmt.Sprintf("%x  ./snapshot.txt\x00", sha256.Sum256([]byte(content))))}, nil
			}
		}
		return ports.InstanceExecResult{}, nil
	}}
	program, err := New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root, ProjectData: probe, ProjectArchive: forbiddenExportArchive{opens: &facts.archiveOpens}})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	store, err := openProjectStore(context.Background(), loaded.Context.Paths.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	record := domain.ProjectRecord{Schema: 1, IdentityVersion: 2, ProjectID: "Demo", Name: "Demo", Mode: domain.ProjectSync, HostPath: "/controller-only/source", SourceKey: state.SourceKey("/controller-only/source"), YardPath: state.YardPath("Demo"), SSHHost: "yard", Target: "yard", ImportedAt: "2026-07-22T00:00:00Z"}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	archive, err := projectActionArchive(facts.baseline).Open(context.Background(), record.HostPath)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := application.ProjectArchiveTreeDigest(archive)
	if err := errors.Join(err, archive.Close()); err != nil {
		t.Fatal(err)
	}
	descriptor := ProjectExportDescriptor{Schema: 1, ProjectID: "Demo", SourceKey: record.SourceKey, ArchiveDigest: fmt.Sprintf("%x", sha256.Sum256([]byte("retained controller archive"))), ArchiveSize: 2048, SourceTree: tree, Destination: "/controller-only/output/Demo.patch", DestinationBinding: fmt.Sprintf("%x", sha256.Sum256([]byte("controller-only private destination facts")))}
	beforeState := nativeTreeSnapshot(t, loaded.Context.Paths.StateDir)
	prepared, err := program.prepareOwnerProjectExport(context.Background(), loaded, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(beforeState, "\n") != strings.Join(nativeTreeSnapshot(t, loaded.Context.Paths.StateDir), "\n") {
		t.Fatal("read-only export preparation changed project state")
	}
	t.Cleanup(func() {
		facts.cleanupFails = false
		if err := prepared.Close(); err != nil {
			t.Fatal(err)
		}
	})
	return program, prepared, facts
}

func TestProjectExportOwnerPlanDoesNotOpenControllerPathsOrMutateRegistry(t *testing.T) {
	_, prepared, facts := ownerProjectExportFixture(t)
	if facts.archiveOpens != 0 || facts.temporary != 0 || prepared.Project.Commit != projectCommitNone || prepared.Project.Reservation != nil {
		t.Fatal("export preparation crossed a mutation or source ownership boundary")
	}
	if prepared.Plan.Confirmation != domain.ConfirmationNever || !prepared.Plan.Confirmed || !prepared.stepsComplete || len(prepared.Plan.Steps) != 3 {
		t.Fatal("bounded export lost its native exact plan or acquired a prompt")
	}
	if prepared.Plan.Steps[2].Target != "controller:/controller-only/output/Demo.patch" {
		t.Fatal("controller destination was not retained")
	}
}

func TestProjectExportOwnerRejectsLiveDriftBeforeTemporaryWrite(t *testing.T) {
	for _, drift := range []string{"guest", "record", "input"} {
		t.Run(drift, func(t *testing.T) {
			program, prepared, facts := ownerProjectExportFixture(t)
			switch drift {
			case "guest":
				facts.guest = "new unapproved guest change"
			case "record":
				record := prepared.Project.Record
				record.HostPath = "/changed-controller-source"
				record.SourceKey = state.SourceKey(record.HostPath)
				store, err := openProjectStore(context.Background(), prepared.Loaded.Context.Paths.StateDir)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Put(context.Background(), record); err != nil {
					t.Fatal(err)
				}
			case "input":
				testkit.WriteFile(t, filepath.Join(program.options.RepositoryRoot, "config", "host.env"), []byte("# native input changed\n"), 0o600)
			}
			orchestrator := program.operationOrchestrator(prepared.Plan.OperationID, prepared.Loaded, nil, &prepared.Definition)
			if _, err := prepared.Execute(context.Background(), orchestrator, io.Discard); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("drift was not rejected: %v", err)
			}
			if facts.temporary != 0 {
				t.Fatal("guest temporary created before drift rejection")
			}
		})
	}
}

func TestProjectExportOwnerFinalizationVerifiesTransferredTreeAndCleanup(t *testing.T) {
	for _, failure := range []string{"", "baseline", "guest", "cleanup"} {
		t.Run(failure, func(t *testing.T) {
			program, prepared, facts := ownerProjectExportFixture(t)
			orchestrator := program.operationOrchestrator(prepared.Plan.OperationID, prepared.Loaded, nil, &prepared.Definition)
			if _, err := prepared.Execute(context.Background(), orchestrator, io.Discard); err != nil {
				t.Fatal(err)
			}
			if facts.temporary != 1 {
				t.Fatal("export did not begin one owned temporary")
			}
			switch failure {
			case "baseline":
				facts.baseline = "corrupt transfer"
			case "guest":
				facts.guest = "post-diff unapproved source"
			case "cleanup":
				facts.cleanupFails = true
			}
			_, err := program.finalizeOwnerProjectTransfer(context.Background(), prepared)
			if failure != "" {
				if !errors.Is(err, domain.ErrPlanStale) || prepared.Project.ownerExport.finalized {
					t.Fatalf("unverified export finalized: %v", err)
				}
			} else {
				if err != nil || !prepared.Project.ownerExport.finalized || facts.cleanup != 1 {
					t.Fatalf("native finalization failed: %v", err)
				}
				if _, err := program.finalizeOwnerProjectTransfer(context.Background(), prepared); err == nil {
					t.Fatal("finalization replay accepted")
				}
			}
			if record, err := prepared.Project.Store.GetReadOnly(context.Background(), "Demo"); err != nil || record != prepared.Project.Record {
				t.Fatal("export mutated registry")
			}
		})
	}
}

func TestProjectExportHydratesRedactedSourceFromSameOwnerScope(t *testing.T) {
	_, prepared, _ := ownerProjectExportFixture(t)
	owner := prepared.Project.Record
	redacted := owner
	redacted.HostPath, redacted.ImportedAt, redacted.SSHHost, redacted.RegistrySource = "", "", "controller-yard-alias", "yard"
	project := &projectExecution{Record: redacted}
	if err := hydrateExportProject(project, owner); err != nil || project.Record != owner {
		t.Fatalf("redacted source was not hydrated: %v", err)
	}
	owner.HostPath = "/different-controller/source"
	project.Record = redacted
	if err := hydrateExportProject(project, owner); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("different source scope accepted")
	}
}
