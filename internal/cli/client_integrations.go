package cli

import (
	"context"
	"fmt"

	"github.com/Subyard/Subyard/internal/adapters/codexdesktop"
	"github.com/Subyard/Subyard/internal/clientprojects"
)

// This composition boundary binds shipped client commands to their adapters.
// Inventory preparation does not depend on a server profile or client schema.
func (cli *CLI) prepareDesktopClient(_ context.Context, tool string, export clientprojects.Export, override string) (clientprojects.Plan, error) {
	if tool != "codex" {
		return clientprojects.Plan{}, fmt.Errorf("unsupported desktop integration %q", tool)
	}
	home := cli.env["SUBYARD_OPERATOR_HOME"]
	if home == "" {
		home = cli.env["HOME"]
	}
	path, err := codexdesktop.ConfigPath(home, cli.env["CODEX_HOME"], override)
	if err != nil {
		return clientprojects.Plan{}, err
	}
	return codexdesktop.Prepare(path, export, codexdesktop.OpenURL)
}
