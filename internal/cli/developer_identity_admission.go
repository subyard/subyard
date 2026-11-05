package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

func identityReleaseBinding(root string) (string, error) {
	snapshot, err := config.ReadPersistentFileSnapshot(root, filepath.Join(root, "release-transition", "v2", "journal.json"))
	if err != nil {
		return "", err
	}
	return operationStateDigest(config.BindPersistentSnapshot(snapshot)), nil
}

// admitDeveloperIdentity precedes every mutating dispatch, including commands
// which ordinarily own their release gate. Only exact plain init may recover; independently assessed full removal remains available.
func (cli *CLI) admitDeveloperIdentity(ctx context.Context, yard, handler string, arguments []string) (bool, error) {
	// Full removal owns a separately assessed destructive scope and producer
	// fencing. It does not resume identity work or replace its live source.
	if handler == "@teardown" && !strings.Contains(yard, "/") {
		if execution, err := prepareTeardownExecution(arguments); err == nil && execution.full {
			return false, nil
		}
	}
	root, err := config.ResolveConfigHome(firstNonempty(cli.env["SUBYARD_OPERATOR_HOME"], cli.env["HOME"]), cli.env)
	if err != nil {
		return false, err
	}
	markers, err := config.PendingDeveloperIdentities(root)
	if err != nil {
		return false, err
	}
	if len(markers) == 0 {
		return false, nil
	}
	request, parseErr := parseInitArguments(arguments)
	if len(markers) != 1 || handler != "@init" || parseErr != nil || request.mode != initReconcile || request.profile != "" || request.adopt || request.continueLegacy || markers[0].Yard != yard {
		return false, fmt.Errorf("developer identity transition is pending; finish plain init for yard %s before another mutation", markers[0].Yard)
	}
	loaded, err := config.Load(config.LoadOptions{Catalog: &cli.catalog, RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: firstNonempty(cli.baseEnv["SUBYARD_OPERATOR_HOME"], cli.baseEnv["HOME"]), YardName: yard, Environment: cli.baseEnv})
	if err != nil {
		return false, err
	}
	peers, err := cli.powerYardContexts(loaded)
	if err != nil {
		return false, err
	}
	inputs, err := cli.captureOwnerInputs(loaded, "", peers)
	if err != nil {
		return false, err
	}
	release, err := identityReleaseBinding(root)
	if err != nil {
		return false, err
	}
	if inputs.binding() != markers[0].Inputs || release != markers[0].Release {
		return false, fmt.Errorf("%w: pending developer identity configuration, source or completed release changed", domain.ErrPlanStale)
	}
	return true, nil
}
