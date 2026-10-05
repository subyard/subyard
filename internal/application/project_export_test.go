package application

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type exportStreamFailure struct{ projectProcessExecutor }

func (data exportStreamFailure) Stream(context.Context, domain.Context, ports.InstanceExecRequest, io.Reader) (ports.InstanceExecResult, error) {
	return ports.InstanceExecResult{ExitCode: 7}, nil
}

func exportNativeRunner(t *testing.T) (ProjectActionRunner, string) {
	t.Helper()
	root := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(root, "content.txt"), []byte("yard content"), 0o600)
	payload := validProjectArchive(t, "controller content")
	tree, err := ProjectArchiveTreeDigest(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	runner := ProjectActionRunner{Data: projectProcessExecutor{}, Archive: &countingProjectArchive{payload: payload}, Exports: &projectExportStub{path: filepath.Join(root, "result.patch")},
		Project: domain.ProjectRecord{Schema: 1, ProjectID: "Demo", Name: "Demo", HostPath: "/controller/source", Mode: domain.ProjectSync, YardPath: root}, ExportSourceTree: tree}
	operationID := fmt.Sprintf("export-native-%d", time.Now().UnixNano())
	return runner, operationID
}

func TestProjectExportNativeDraftRetainsPortableBytesUntilVerification(t *testing.T) {
	runner, operationID := exportNativeRunner(t)
	draft, err := runner.PrepareExport(context.Background(), operationID, "")
	if err != nil {
		t.Fatal(err)
	}
	defer draft.Close()
	if !draft.Changed || !bytes.Contains(draft.Patch, []byte("--- a/content.txt")) || !bytes.Contains(draft.Patch, []byte("+++ b/content.txt")) {
		t.Fatal("native portable diff was not retained")
	}
	store := runner.Exports.(*projectExportStub)
	runner.VerifyExport = func(context.Context) error { return domain.ErrPlanStale }
	if _, err := runner.PublishExport(context.Background(), draft); !errors.Is(err, domain.ErrPlanStale) || len(store.patch) != 0 {
		t.Fatal("verification failure published a patch")
	}
	runner.VerifyExport = nil
	draft.Patch = append(draft.Patch, []byte("unapproved mutation")...)
	if _, err := runner.PublishExport(context.Background(), draft); !errors.Is(err, domain.ErrPlanStale) || len(store.patch) != 0 {
		t.Fatal("changed retained diff published")
	}
	if err := draft.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ProjectExportTemporary(operationID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned temporary remained")
	}
}

func TestProjectExportNativeRefusesCorruptBaselineOrFailedStream(t *testing.T) {
	for _, failure := range []string{"baseline", "stream"} {
		t.Run(failure, func(t *testing.T) {
			runner, operationID := exportNativeRunner(t)
			if failure == "baseline" {
				runner.ExportSourceTree = strings.Repeat("f", 64)
			} else {
				runner.Data = exportStreamFailure{}
			}
			if _, err := runner.PrepareExport(context.Background(), operationID, ""); err == nil {
				t.Fatal("unverified input accepted")
			}
			if len(runner.Exports.(*projectExportStub).patch) != 0 {
				t.Fatal("unverified input published a patch")
			}
			if _, err := os.Stat(ProjectExportTemporary(operationID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed transfer left owned temporary")
			}
		})
	}
}

func TestProjectExportNativeForeignTemporaryIsNeverDeleted(t *testing.T) {
	runner, operationID := exportNativeRunner(t)
	foreign := ProjectExportTemporary(operationID)
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(foreign) })
	marker := filepath.Join(foreign, "foreign.txt")
	testkit.WriteFile(t, marker, []byte("foreign"), 0o600)
	if _, err := runner.PrepareExport(context.Background(), operationID, ""); err == nil {
		t.Fatal("foreign temporary collision adopted")
	}
	if actual, err := os.ReadFile(marker); err != nil || string(actual) != "foreign" {
		t.Fatal("foreign temporary changed")
	}
}

func TestProjectExportNativeCleanupRefusesReplacedTemporary(t *testing.T) {
	runner, operationID := exportNativeRunner(t)
	draft, err := runner.PrepareExport(context.Background(), operationID, "")
	if err != nil {
		t.Fatal(err)
	}
	path := ProjectExportTemporary(operationID)
	owned := path + "-original"
	if err := os.Rename(path, owned); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path); _ = os.RemoveAll(owned) })
	testkit.WriteFile(t, filepath.Join(path, "foreign.txt"), []byte("foreign"), 0o600)
	if err := draft.Close(); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("cleanup did not report replaced temporary")
	}
	if _, err := os.Stat(filepath.Join(path, "foreign.txt")); err != nil {
		t.Fatal("cleanup removed foreign replacement")
	}
}

func TestProjectExportNativeChecksRetainedArchiveBeforeGuestWrites(t *testing.T) {
	data := &projectDataStub{}
	runner := ProjectActionRunner{Data: data, PreparedArchive: failedPreparedProjectArchive{}, Project: domain.ProjectRecord{Mode: domain.ProjectSync}}
	if _, err := runner.PrepareExport(context.Background(), "export-input-failure", ""); !errors.Is(err, domain.ErrPlanStale) || len(data.requests) != 0 {
		t.Fatalf("input failure reached guest writes: %v", err)
	}
}
