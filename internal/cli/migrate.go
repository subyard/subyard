package cli

import (
	"context"
	"errors"
	"io"
	"maps"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

// Migration belongs to the host installation. Its preparation must remain
// reachable without loading or recovering unrelated yard configuration.
func (cli *CLI) currentMigrationContext() (config.Loaded, error) {
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil {
		return config.Loaded{}, err
	}
	if !available {
		return config.Loaded{}, errors.New("installed release paths are unavailable")
	}
	environment := maps.Clone(cli.baseEnv)
	environment["SUBYARD_HOME"] = options.DataHome
	environment["SUBYARD_CONFIG_HOME"] = options.ConfigHome
	environment["YARD_RUNTIME_ROOT"] = options.RuntimeRoot
	environment["SUBYARD_OPERATION_ID"] = cli.ensureOperationID()
	return config.Loaded{
		Environment: environment,
		Context: domain.Context{
			YardName: "default", AccessKind: domain.AccessLocal,
			Paths: domain.RuntimePaths{DataHome: options.DataHome, ConfigHome: options.ConfigHome},
		},
	}, nil
}

func (cli *CLI) runCurrentMigration(ctx context.Context, definition command.Definition, arguments []string, explicit bool, yes bool) int {
	if explicit && !commandHelpRequested(arguments) {
		cli.errorf("migrate operates on this host's installed release; run it without a yard selector")
		return 2
	}
	loaded, err := cli.currentMigrationContext()
	if err != nil {
		cli.errorf("migrate: %v", err)
		return 1
	}
	if cli.env["SUBYARD_NO_AUDIT"] == "" {
		cli.audit(definition.Name, arguments, "", "")
	}
	prepared, err := cli.prepareCommand(ctx, prepareCommandRequest{
		Loaded: loaded, Definition: definition, Arguments: arguments,
	})
	if err != nil {
		return cli.reportPreparationError(definition, err)
	}
	defer prepared.Close()
	return cli.runPreparedCommand(ctx, prepared, yes || cli.env["ASSUME_YES"] == "1")
}

func (prepared *preparedCommand) prepareCurrentMigration(ctx context.Context, _ *initBootstrap) error {
	if prepared.Loaded.Context.AccessKind == domain.AccessRemote {
		return errors.New("migrate operates on the local installation; run it on the owner host")
	}
	cli := prepared.CLI
	loaded, err := cli.currentMigrationContext()
	if err != nil {
		return err
	}
	prepared.Loaded = loaded
	runtime := releaseruntime.New(cli.releaseRuntimeConfig(loaded.Environment))
	operation, err := runtime.PrepareCurrentTransition(ctx, prepared.Arguments,
		loaded.Context.Paths.ConfigHome, "default", cli.releaseTransitionInheritedSettingIDs())
	if err != nil {
		_ = runtime.Close()
		return err
	}
	execution := &releaseExecution{prepared: operation, yard: "default", runtime: runtime, phase: "execute"}
	prepared.closeResource = execution.Close
	prepared.executeNoOp = true
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return operation.Action, domain.ActionDelta{Changed: operation.Changed, Consequences: operation.Consequences}, nil
	}
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, _ io.Writer) (domain.AdapterResult, error) {
		return cli.executeRelease(ctx, orchestrator, prepared.Plan, execution)
	}
	return nil
}

func releaseRecoveryCommand(definition command.Definition) bool {
	return definition.Handler == "@update" || definition.Handler == "@current-migration"
}
