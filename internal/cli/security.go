package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/securityruntime"
	"github.com/Subyard/Subyard/internal/config"
)

func (cli *CLI) runSecurity(
	ctx context.Context,
	loaded config.Loaded,
	arguments []string,
) int {
	quiet := false
	requireLive := false
	for _, argument := range arguments {
		switch argument {
		case "-y", "--yes":
		case "--quiet":
			quiet = true
		case "--require-live":
			requireLive = true
		case "-h", "--help":
			fmt.Fprintf(cli.options.Stdout, "Usage: %s security [--quiet] [--require-live]\n",
				cli.options.Program)
			return 0
		default:
			cli.errorf("unknown option %q", argument)
			return 2
		}
	}
	incusPort, _ := cli.statusPorts()
	checker := securityruntime.Runtime{
		RepositoryRoot: cli.options.RepositoryRoot,
		Environment:    loaded.Environment,
		Yard:           loaded.Context,
		Incus:          incusPort,
		Stdout:         cli.options.Stdout,
		Stderr:         cli.options.Stderr,
		ProxyContracts: cli.resources.ProxyContracts(strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"])),
	}
	if _, err := checker.CheckSecurity(ctx, requireLive, quiet); err != nil {
		if !quiet {
			cli.errorf("security: %v", err)
		}
		return 1
	}
	return 0
}
