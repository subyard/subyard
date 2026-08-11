package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/statusruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func (cli *CLI) runSpace(
	ctx context.Context,
	loaded config.Loaded,
	explicit bool,
	arguments []string,
) int {
	refresh := false
	for _, argument := range arguments {
		switch argument {
		case "--refresh":
			refresh = true
		case "-h", "--help":
			fmt.Fprintf(cli.options.Stdout, "Usage: %s space [--refresh]\n", cli.options.Program)
			return 0
		case "--yes":
		default:
			cli.errorf("unknown option %q", argument)
			return 2
		}
	}

	var yards []domain.Context
	if cli.yardSelectionExplicit(loaded, explicit) {
		if loaded.Context.AccessKind != domain.AccessLocal {
			cli.errorf("space: remote yards are not supported")
			return 1
		}
		yards = []domain.Context{loaded.Context}
	} else {
		var err error
		yards, err = (cliOwnerSource{cli: cli, loaded: loaded}).Yards(ctx)
		if err != nil {
			cli.errorf("space: %v", err)
			return 1
		}
	}
	incusPort, executor := cli.statusPorts()
	currentTime := time.Now
	if cli.options.Clock != nil {
		currentTime = cli.options.Clock.Now
	}
	runtime := statusruntime.Runtime{
		Executor: executor,
		Now:      currentTime,
	}
	failed := false
	fmt.Fprintf(cli.options.Stdout, "%-16s %-12s %8s %s\n", "YARD", "STATE", "USED", "MEASURED")
	for _, yard := range yards {
		state, stateErr := localYardState(ctx, incusPort, yard)
		if refresh && stateErr != nil {
			cli.errorf("space: %s: state: %v", yard.YardName, stateErr)
			failed = true
		}
		if refresh && stateErr == nil && state == "RUNNING" {
			if _, err := runtime.RefreshSpace(ctx, yard); err != nil {
				cli.errorf("space: %s: refresh: %v", yard.YardName, err)
				failed = true
			}
		}
		used, measured := "—", "never"
		if measurement, ok := runtime.ReadSpace(yard); ok {
			used = measurement.Figure
			measured = ageHuman(currentTime().Sub(measurement.MeasuredAt)) + " ago"
		}
		fmt.Fprintf(cli.options.Stdout, "%-16s %-12s %8s %s\n",
			yard.YardName, state, used, measured)
	}
	if failed {
		return 1
	}
	return 0
}

func localYardState(
	ctx context.Context,
	incusPort ports.Incus,
	yard domain.Context,
) (string, error) {
	instance, err := incusPort.Instance(ctx, yard.IncusProject, yard.YardInstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) {
		return "NOT_CREATED", nil
	}
	if err != nil {
		return "UNKNOWN", err
	}
	if instance.Status == "" {
		return "UNKNOWN", errors.New("instance state is empty")
	}
	return strings.ToUpper(instance.Status), nil
}
