package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

var exportTemporaryIdentity = regexp.MustCompile(`^[0-9]+:[0-9]+:[0-9]+$`)

func ProjectExportTemporary(operationID string) string {
	return filepath.Join("/tmp", "subyard-export-"+operationID)
}

func CreateProjectExportTemporary(ctx context.Context, data ports.YardExecutor, yard domain.Context, operationID string) (string, error) {
	if !domain.SafeID(operationID) {
		return "", errors.New("export requires a safe operation ID")
	}
	result, err := data.Execute(ctx, yard, ports.InstanceExecRequest{Command: []string{"sh", "-c", `set -eu; mkdir -m 700 -- "$1"; stat -c '%d:%i:%u' -- "$1"`, "subyard", ProjectExportTemporary(operationID)}, User: uint32(yard.DevUID), Group: uint32(yard.DevUID)})
	identity := strings.TrimSpace(string(result.Stdout))
	if err != nil || result.ExitCode != 0 || !exportTemporaryIdentity.MatchString(identity) {
		return "", executionError("create owned export snapshot", result, errors.Join(err, errors.New("export temporary identity is unavailable")))
	}
	return identity, nil
}

func checkProjectExportTemporary(ctx context.Context, data ports.YardExecutor, yard domain.Context, operationID, identity string, cleanup bool) error {
	if !domain.SafeID(operationID) || !exportTemporaryIdentity.MatchString(identity) {
		return errors.New("invalid owned export temporary")
	}
	script := `set -eu; test -d "$1"; test ! -L "$1"; test "$(stat -c '%d:%i:%u' -- "$1")" = "$2"; test "$(stat -c '%a' -- "$1")" = 700`
	if cleanup {
		script = `set -eu; if test ! -e "$1" && test ! -L "$1"; then exit 0; fi; ` + strings.TrimPrefix(script, "set -eu; ") + `; rm -rf -- "$1"`
	}
	result, err := data.Execute(ctx, yard, ports.InstanceExecRequest{Command: []string{"sh", "-c", script, "subyard", ProjectExportTemporary(operationID), identity}, User: uint32(yard.DevUID), Group: uint32(yard.DevUID)})
	if err != nil || result.ExitCode != 0 {
		return fmt.Errorf("%w: export temporary ownership changed", domain.ErrPlanStale)
	}
	return nil
}

func CleanupProjectExportTemporary(data ports.YardExecutor, yard domain.Context, operationID, identity string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return checkProjectExportTemporary(ctx, data, yard, operationID, identity, true)
}

func CheckProjectExportTemporary(ctx context.Context, data ports.YardExecutor, yard domain.Context, operationID, identity string) error {
	return checkProjectExportTemporary(ctx, data, yard, operationID, identity, false)
}

// ProjectExportDraft retains diff bytes until the owner verifies the data-plane work.
type ProjectExportDraft struct {
	Patch                          []byte
	Changed                        bool
	runner                         ProjectActionRunner
	operationID, temporaryIdentity string
	closed                         bool
	patchDigest                    [32]byte
}

func (draft *ProjectExportDraft) Close() error {
	if draft == nil || draft.closed {
		return nil
	}
	draft.closed = true
	draft.Patch = nil
	return CleanupProjectExportTemporary(draft.runner.Data, draft.runner.Yard, draft.operationID, draft.temporaryIdentity)
}

func (runner ProjectActionRunner) PrepareExport(ctx context.Context, operationID, temporaryIdentity string) (_ *ProjectExportDraft, err error) {
	if runner.Project.Mode != domain.ProjectSync {
		return nil, fmt.Errorf("%s projects cannot be exported", runner.Project.Mode)
	}
	archivePort := runner.Archive
	if runner.PreparedArchive != nil {
		archivePort = runner.PreparedArchive
	}
	if archivePort == nil || runner.Data == nil {
		return nil, errors.New("project archive and data plane are required")
	}
	if runner.VerifyExport != nil {
		if err := runner.VerifyExport(ctx); err != nil {
			return nil, err
		}
	}
	archive, err := archivePort.Open(ctx, runner.Project.HostPath)
	if err != nil {
		return nil, fmt.Errorf("read host copy: %w", err)
	}
	defer archive.Close()
	if runner.ExportObservedTree != "" {
		live, err := ObserveProjectTree(ctx, runner.Data, runner.Yard, runner.Project.YardPath)
		if err != nil {
			return nil, err
		}
		if live != runner.ExportObservedTree {
			return nil, fmt.Errorf("%w: project export source changed", domain.ErrPlanStale)
		}
	}
	if temporaryIdentity == "" {
		temporaryIdentity, err = CreateProjectExportTemporary(ctx, runner.Data, runner.Yard, operationID)
		if err != nil {
			return nil, err
		}
	} else if err := checkProjectExportTemporary(ctx, runner.Data, runner.Yard, operationID, temporaryIdentity, false); err != nil {
		return nil, err
	}
	draft := &ProjectExportDraft{runner: runner, operationID: operationID, temporaryIdentity: temporaryIdentity}
	defer func() {
		if err != nil {
			err = errors.Join(err, draft.Close())
		}
	}()
	temporary := ProjectExportTemporary(operationID)
	if err := runner.execute(ctx, "prepare export snapshot", ports.InstanceExecRequest{Command: []string{"mkdir", "-m", "700", "--", filepath.Join(temporary, "a")}, User: uint32(runner.Yard.DevUID), Group: uint32(runner.Yard.DevUID)}); err != nil {
		return nil, err
	}
	result, streamErr := runner.Data.Stream(ctx, runner.Yard, ports.InstanceExecRequest{Command: []string{"tar", "-C", filepath.Join(temporary, "a"), "-xf", "-"}, User: uint32(runner.Yard.DevUID), Group: uint32(runner.Yard.DevUID)}, archive)
	if err := errors.Join(streamErr, archive.Close()); err != nil || result.ExitCode != 0 {
		return nil, executionError("copy host snapshot", result, err)
	}
	if err := checkProjectExportTemporary(ctx, runner.Data, runner.Yard, operationID, temporaryIdentity, false); err != nil {
		return nil, err
	}
	if runner.ExportSourceTree != "" {
		baseline, err := ObserveProjectTree(ctx, runner.Data, runner.Yard, filepath.Join(temporary, "a"))
		if err != nil {
			return nil, err
		}
		if baseline != runner.ExportSourceTree {
			return nil, fmt.Errorf("%w: extracted export snapshot differs from retained source", domain.ErrPlanStale)
		}
	}
	result, err = runner.Data.Execute(ctx, runner.Yard, ports.InstanceExecRequest{Command: []string{"diff", "-ruN", "--exclude=.git", filepath.Join(temporary, "a"), runner.Project.YardPath}, User: uint32(runner.Yard.DevUID), Group: uint32(runner.Yard.DevUID)})
	if result.ExitCode != 0 && result.ExitCode != 1 || err != nil && result.ExitCode != 1 {
		return nil, executionError("diff project copies", result, err)
	}
	draft.Changed = result.ExitCode == 1
	if draft.Changed {
		draft.Patch = portablePatch(result.Stdout, filepath.Join(temporary, "a"), runner.Project.YardPath)
		draft.patchDigest = sha256.Sum256(draft.Patch)
	}
	if runner.ExportObservedTree != "" {
		live, err := ObserveProjectTree(ctx, runner.Data, runner.Yard, runner.Project.YardPath)
		if err != nil {
			return nil, err
		}
		if live != runner.ExportObservedTree {
			return nil, fmt.Errorf("%w: project export source changed during diff", domain.ErrPlanStale)
		}
	}
	return draft, nil
}

func (runner ProjectActionRunner) PublishExport(ctx context.Context, draft *ProjectExportDraft) (string, error) {
	if draft == nil || draft.closed || !draft.Changed || runner.Exports == nil {
		return "", errors.New("export has no retained patch to publish")
	}
	if sha256.Sum256(draft.Patch) != draft.patchDigest {
		return "", fmt.Errorf("%w: retained export diff changed", domain.ErrPlanStale)
	}
	if runner.VerifyExport != nil {
		if err := runner.VerifyExport(ctx); err != nil {
			return "", err
		}
	}
	path, err := runner.Exports.Publish(ctx, runner.Project.ProjectID, draft.Patch)
	if err != nil {
		return "", fmt.Errorf("publish export: %w", err)
	}
	if runner.ExportObservedTree != "" {
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, draft.Patch) {
			return "", fmt.Errorf("verify exported patch: %w", errors.Join(err, errors.New("published patch differs from prepared diff")))
		}
	}
	return path, nil
}
