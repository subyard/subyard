package cli

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

// credentialExactInvocation selects public metadata workflows. Protected input
// and internal exchanges keep their dedicated owner-local transport.
func credentialExactInvocation(definition command.Definition, arguments []string) bool {
	if definition.Name != "keys" || definition.Arg0 != "" {
		return false
	}
	arguments = keysWithoutConsent(arguments)
	if len(arguments) == 0 {
		return false
	}
	switch arguments[0] {
	case "rollback", "revoke", "delete", "materialize", "sync", "trust", "untrust", "move":
		return true
	case "resolve":
		return slices.Contains(arguments[1:], "--choose") && !slices.Contains(arguments[1:], "--rotate")
	case "auto-sync":
		return len(arguments) > 1 && (arguments[1] == "pause" || arguments[1] == "resume")
	default:
		return false
	}
}

func credentialProtectedInvocation(arguments []string) bool {
	arguments = keysWithoutConsent(arguments)
	if len(arguments) == 0 {
		return false
	}
	switch arguments[0] {
	case "add", "rotate":
		return true
	case "import":
		return !slices.Contains(arguments[1:], "--dry-run")
	case "resolve":
		return slices.Contains(arguments[1:], "--rotate")
	default:
		return false
	}
}

func (prepared *preparedCommand) prepareKeys(ctx context.Context, _ *initBootstrap) error {
	protected := prepared.Definition.Name == "keys" && credentialProtectedInvocation(prepared.Arguments)
	if !credentialExactInvocation(prepared.Definition, prepared.Arguments) && !protected {
		return errors.New("credential operation requires its dedicated owner-local transport")
	}
	var input io.Reader = strings.NewReader("")
	if protected {
		input = prepared.CLI.options.Stdin
	}
	runtime, err := prepared.CLI.credentialRuntimeWithStreams(prepared.Loaded, input, io.Discard, prepared.CLI.options.Stderr)
	if err != nil {
		return err
	}
	native, err := runtime.Prepare(ctx, prepared.Definition.Arg0, keysWithoutConsent(prepared.Arguments))
	if err != nil {
		return err
	}
	prepared.exactState = native.Binding
	prepared.stepsComplete = len(native.Steps) != 0 && native.Binding != ""
	prepared.steps = func() []domain.OperationStep { return domain.CloneOperationSteps(native.Steps) }
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return native.Action, domain.ActionDelta{Changed: native.Changed, Consequences: slices.Clone(native.Consequences)}, nil
	}
	prepared.executeNoOp = true // Native closures recheck captured metadata even for skip.
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, _ io.Writer) (domain.AdapterResult, error) {
		orchestrator.Runner = credentialAdapter{prepared: native}
		result, _, err := orchestrator.RunAdapter(ctx, prepared.Plan, domain.AdapterRequest{Schema: shelladapter.ProtocolSchema, OperationID: prepared.Plan.OperationID, Adapter: "credential", Action: "execute"}, nil)
		return result, err
	}
	return nil
}

func (cli *CLI) runPreparedKeys(ctx context.Context, loaded config.Loaded, definition command.Definition, arguments []string) int {
	prepared, err := cli.prepareCommand(ctx, prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: arguments, ExplicitYard: true})
	if err != nil {
		cli.errorf("keys: %v", err)
		return 1
	}
	defer prepared.Close()
	return cli.runPreparedCommand(ctx, prepared, cli.env["ASSUME_YES"] == "1" || keysAssumeYes(arguments))
}
