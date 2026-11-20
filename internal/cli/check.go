package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/config"
)

func (cli *CLI) runHostCheck(
	ctx context.Context,
	loaded config.Loaded,
	arguments []string,
) int {
	for _, argument := range arguments {
		switch argument {
		case "-y", "--yes", "--host":
		case "-h", "--help":
			fmt.Fprintf(cli.options.Stdout, "Usage: %s check --host\n", cli.options.Program)
			return 0
		default:
			cli.errorf("unknown option %q", argument)
			return 2
		}
	}
	yards, err := cli.powerYardContexts(loaded)
	if err != nil {
		cli.errorf("discover local yards: %v", err)
		return 1
	}
	incusPort, _ := cli.statusPorts()
	check := hostruntime.HostCheck{
		Yard: loaded.Context, Yards: yards, Environment: loaded.Environment,
		Incus: incusPort, Output: cli.options.Stdout,
	}
	if err := check.Run(ctx, hostruntime.CheckOptions{}); err != nil {
		if !errors.Is(err, hostruntime.ErrHostNotReady) {
			cli.errorf("check host: %v", err)
		}
		return 1
	}
	return 0
}
