package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/remotecontrol"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func (cli *CLI) remoteService(loaded config.Loaded) application.RemoteService {
	return application.RemoteService{Control: cli.remoteControl(loaded, 0)}
}

func (cli *CLI) remoteControl(loaded config.Loaded, timeout time.Duration) ports.RemoteControl {
	if cli.options.RemoteControl != nil {
		return cli.options.RemoteControl
	}
	return remotecontrol.Runtime{
		Home: loaded.Context.Paths.OperatorHome, ConfigHome: loaded.Context.Paths.ConfigHome,
		ConfigDir: loaded.Context.Paths.ConfigDir, DataHome: loaded.Context.Paths.DataHome,
		PublicKey:   cli.env["SUBYARD_SSH_PUBKEY"],
		Environment: environmentList(cli.env, nil),
		Timeout:     timeout,
	}
}

func (cli *CLI) prepareRemoteExecution(ctx context.Context, loaded config.Loaded, arguments []string) (*domain.RemotePrepared, error) {
	prepared, err := cli.remoteService(loaded).Prepare(ctx, arguments)
	return &prepared, err
}

func (cli *CLI) prepareRemoteOperation(
	orchestrator *application.Orchestrator,
	loaded config.Loaded,
	prepared domain.RemotePrepared,
) (domain.OperationPlan, error) {
	action, delta, err := application.RemoteActionPlan(prepared)
	if err != nil {
		return domain.OperationPlan{}, err
	}
	policy := application.RemotePolicy(prepared)
	return orchestrator.PrepareAction(loaded.Context, policy.Name, policy.RemotePolicy, action, delta)
}

func (cli *CLI) printRemoteResult(result domain.AdapterResult) {
	if message, ok := result.Output["message"].(string); ok && message != "" {
		fmt.Fprintln(cli.options.Stdout, message)
	}
	records, ok := result.Output["records"].([]domain.RemoteRecord)
	if !ok {
		return
	}
	fmt.Fprintf(cli.options.Stdout, "%-14s %-22s %-12s %-6s %s\n", "NAME", "DEST", "REMOTE YARD", "PORT", "LAST PROBE")
	for _, record := range records {
		seen := "never"
		if !record.LastProbe.IsZero() {
			seen = ageHuman(time.Since(record.LastProbe)) + " ago"
		}
		ownerYard := record.Spec.OwnerYard
		if ownerYard == "" {
			ownerYard = "<default>"
		}
		fmt.Fprintf(cli.options.Stdout, "%-14s %-22s %-12s %-6d %s\n",
			record.Spec.Name, record.Spec.Destination, ownerYard, record.SSHPort, seen)
	}
	if len(records) == 0 {
		fmt.Fprintln(cli.options.Stdout, "  [ .. ] no remote yards registered")
	}
}

var _ ports.RemoteControl = remotecontrol.Runtime{}
