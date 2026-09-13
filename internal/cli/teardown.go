package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

type teardownExecution struct {
	keepData        bool
	changed         bool
	physicalChanged bool
	networkRemoval  *yardnetwork.RemovalPlan
}

func prepareTeardownExecution(arguments []string) (*teardownExecution, error) {
	execution := &teardownExecution{}
	for _, argument := range arguments {
		switch argument {
		case "-y", "--yes":
		case "--keep-data":
			execution.keepData = true
		case "-h", "--help":
			return nil, errors.New("help is not an executable teardown operation")
		default:
			return nil, fmt.Errorf("unknown teardown argument %q", argument)
		}
	}
	return execution, nil
}

func (execution *teardownExecution) policy(definition command.Definition, yard domain.Context) domain.CommandPolicy {
	consequences := []string{
		"delete yard instance " + yard.YardInstanceName,
		"remove this yard's SSH config and project state",
	}
	if execution.keepData {
		consequences = append(consequences, "keep the project, bridge, storage pool and /srv data")
	} else {
		consequences = append(consequences,
			"delete the yard project and /srv volume",
			"delete the shared bridge, storage pool and disk data only when no other instance or registered local yard uses them",
			"remove the NetworkManager guard only after the bridge disappears",
		)
	}
	if execution.networkRemoval != nil {
		consequences = append(consequences, execution.networkRemoval.Consequences...)
	}
	return domain.CommandPolicy{
		Name: definition.Name, Effect: domain.CommandEffect(definition.Effect),
		RemotePolicy: domain.RemotePolicy(definition.Remote), Consequences: consequences,
	}
}

func (execution *teardownExecution) actionPlan(
	definition command.Definition,
	yard domain.Context,
) (domain.ActionID, domain.ActionDelta, error) {
	if execution == nil {
		return "", domain.ActionDelta{}, errors.New("teardown execution is required")
	}
	action := domain.ActionID("yard.teardown.purge")
	if execution.keepData {
		action = "yard.teardown.keep-data"
	}
	if !execution.physicalChanged && execution.networkRemoval != nil && execution.networkRemoval.Cleanup {
		action = "yard.network.save"
		if execution.networkRemoval.Stored.Policy.Isolation {
			action = "yard.network.apply"
		}
		return action, domain.ActionDelta{Changed: true, Consequences: execution.networkRemoval.Consequences}, nil
	}
	delta := domain.ActionDelta{Changed: execution.changed}
	if delta.Changed {
		delta.Consequences = execution.policy(definition, yard).Consequences
	}
	return action, delta, nil
}

func (cli *CLI) observeTeardownExecution(
	ctx context.Context,
	loaded config.Loaded,
	execution *teardownExecution,
) error {
	if execution == nil {
		return errors.New("teardown execution is required")
	}
	incusPort, _ := cli.statusPorts()
	incusState, err := incusPort.ReconcileState(
		ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName,
		loaded.Environment["SRV_POOL"], loaded.Environment["SRV_VOLUME"],
		loaded.Context.IncusBridge,
	)
	if err != nil {
		return fmt.Errorf("inspect teardown target: %w", err)
	}
	execution.changed = incusState.InstanceFound
	if !execution.keepData {
		execution.changed = execution.changed || incusState.ProjectFound || incusState.ProfileFound ||
			incusState.VolumeFound || incusState.HostNetworkFound || incusState.HostPoolFound
	}
	suffix := ""
	if loaded.Context.YardName != "" {
		suffix = "-" + loaded.Context.YardName
	}
	paths := []string{
		loaded.Context.Paths.StateDir,
		filepath.Join(loaded.Context.Paths.OperatorHome, ".ssh", "subyard"+suffix+".config"),
		filepath.Join(loaded.Context.Paths.DataHome, "space"+suffix+".cache"),
	}
	for _, path := range paths {
		_, statErr := os.Lstat(path)
		switch {
		case statErr == nil:
			execution.changed = true
		case errors.Is(statErr, os.ErrNotExist):
		default:
			return fmt.Errorf("inspect teardown artifact %s: %w", path, statErr)
		}
	}
	execution.physicalChanged = execution.changed
	service := cli.networkService([]domain.Context{loaded.Context})
	if service == nil {
		return errors.New("Incus network policy adapter is required for teardown")
	}
	removal, err := service.PrepareRemoval(ctx, networkYard(loaded.Context))
	if err != nil {
		return err
	}
	if execution.networkRemoval != nil && (execution.networkRemoval.Stored.Content != removal.Stored.Content || execution.networkRemoval.Stored.ETag != removal.Stored.ETag) {
		return domain.ErrPlanStale
	}
	execution.networkRemoval = &removal
	execution.changed = execution.changed || removal.Cleanup
	return nil
}

func (cli *CLI) executeTeardown(
	ctx context.Context,
	orchestrator *application.Orchestrator,
	loaded config.Loaded,
	plan domain.OperationPlan,
	execution *teardownExecution,
	diagnostics io.Writer,
) (domain.AdapterResult, error) {
	if execution == nil {
		return domain.AdapterResult{}, errors.New("teardown execution is required")
	}
	contextValues := structuredCommandContext(loaded)
	if execution.physicalChanged && cli.options.AdapterRunner == nil {
		if err := cli.prepareSudoPrivileges(
			ctx, diagnostics, cli.effectiveUID(), "teardown",
		); err != nil {
			return domain.AdapterResult{}, err
		}
	}
	if execution.physicalChanged && cli.env["SUBYARD_SUDO_PREAUTHORIZED"] == "1" {
		contextValues["SUBYARD_SUDO_PREAUTHORIZED"] = "1"
	}
	if execution.keepData {
		contextValues["SUBYARD_TEARDOWN_KEEP_DATA"] = "1"
	} else {
		contextValues["SUBYARD_TEARDOWN_KEEP_DATA"] = "0"
	}
	yards, err := cli.powerYardContexts(loaded)
	if err != nil {
		return domain.AdapterResult{}, fmt.Errorf("discover local yards before teardown: %w", err)
	}
	contextValues["SUBYARD_TEARDOWN_KEEP_SHARED"] = "0"
	if hasOtherRegisteredLocalYard(loaded.Context.YardName, yards) {
		contextValues["SUBYARD_TEARDOWN_KEEP_SHARED"] = "1"
	}
	// Teardown must not leave a credential service reconnecting to a future yard.
	agentRuntime, agentErr := cli.sshAgentRuntime(loaded)
	if agentErr != nil {
		return domain.AdapterResult{}, agentErr
	}
	if agentErr = agentRuntime.Revoke(ctx); agentErr != nil {
		return domain.AdapterResult{}, agentErr
	}
	request := domain.AdapterRequest{
		Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID,
		Adapter: "teardown", Action: "apply", Arguments: []string{"--yes"}, Context: contextValues,
	}
	service := cli.networkService(yards)
	if service == nil || execution.networkRemoval == nil {
		return domain.AdapterResult{}, errors.New("prepared network cleanup is required for teardown")
	}
	var result domain.AdapterResult
	var stderr string
	err = service.WithRemoval(ctx, *execution.networkRemoval, func() error {
		if !execution.physicalChanged {
			result = domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID, Status: "ok"}
			return nil
		}
		var runErr error
		result, stderr, runErr = orchestrator.RunAdapter(ctx, plan, request, nil)
		if runErr == nil && result.Status != "ok" {
			return errors.New("physical teardown did not succeed")
		}
		return runErr
	})
	writeAdapterDiagnostics(diagnostics, stderr)
	return result, err
}

func hasOtherRegisteredLocalYard(current string, yards []domain.Context) bool {
	for _, yard := range yards {
		if yard.AccessKind == domain.AccessLocal &&
			yard.YardName != "default" && yard.YardName != current {
			return true
		}
	}
	return false
}
