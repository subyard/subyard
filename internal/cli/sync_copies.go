package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/state"
)

func (execution *projectExecution) automaticCopy() bool {
	return (execution.Record.Mode == domain.ProjectSync || execution.Record.Mode == domain.ProjectGit) && !execution.ExplicitName && execution.PreviewExisting == nil
}

func (execution *projectExecution) setCopyIdentity(id, name string) {
	execution.Record.ProjectID, execution.Record.Name = id, name
	execution.Record.YardPath = state.YardPath(id)
	execution.Environment = projectSnapshot(execution.Record, false)
}

// Physical names remain occupied after soft removal or an interrupted transfer.
// Observe without writing before consent; the physical action also checks its
// destination before writing the copy.
func (cli *CLI) observeProjectCopy(ctx context.Context, execution *projectExecution) error {
	result, err := cli.projectDataPlane().Execute(ctx, execution.Loaded.Context, ports.InstanceExecRequest{
		Command: []string{"sh", "-c", `set -eu; if [ -e /srv/workspaces ] || [ -L /srv/workspaces ]; then find /srv/workspaces -mindepth 1 -maxdepth 1 -printf '%f\0'; fi`},
	})
	if err != nil {
		return fmt.Errorf("inspect project workspace names: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("inspect project workspace names: exit %d", result.ExitCode)
	}
	execution.WorkspaceNames = nil
	for _, name := range strings.Split(string(result.Stdout), "\x00") {
		if domain.SafeProjectName(name) {
			execution.WorkspaceNames = append(execution.WorkspaceNames, name)
		}
	}
	if !execution.CopyObserved {
		admission, err := cli.previewProjectAdmission(ctx, execution.Loaded, execution.Store,
			execution.Record.HostPath, execution.Record.Mode, execution.RequestedName, execution.ExplicitName, execution.WorkspaceNames...)
		if err != nil {
			return err
		}
		execution.setCopyIdentity(admission.ProjectID, admission.Name)
		execution.CopyObserved = true
	}
	execution.ActionChanged = true
	return nil
}
