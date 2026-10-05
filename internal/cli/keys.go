package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/credentialruntime"
	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

const credentialPrepareCapability = "credential-prepare-v1"

type credentialAdapter struct{ prepared credentialruntime.Prepared }

func (adapter credentialAdapter) Run(
	ctx context.Context,
	request domain.AdapterRequest,
	_ io.Reader,
) (domain.AdapterResult, string, error) {
	if request.Adapter != "credential" || request.Action != "execute" {
		return domain.AdapterResult{}, "", errors.New("unsupported credential adapter request")
	}
	if err := adapter.prepared.Execute(ctx); err != nil {
		return domain.AdapterResult{}, "", err
	}
	return domain.AdapterResult{
		Schema: shelladapter.ProtocolSchema, OperationID: request.OperationID, Status: "ok",
	}, "", nil
}

func (cli *CLI) credentialRuntime(loaded config.Loaded) (*credentialruntime.Runtime, error) {
	return cli.credentialRuntimeWithStreams(
		loaded, cli.options.Stdin, cli.options.Stdout, cli.options.Stderr,
	)
}

func (cli *CLI) credentialRuntimeWithStreams(
	loaded config.Loaded,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) (*credentialruntime.Runtime, error) {
	root := loaded.Environment["SUBYARD_KEYS_ROOT"]
	if root == "" {
		root = filepath.Join(loaded.Context.Paths.ConfigHome, "keys")
	}
	consumerRoot := loaded.Environment["SUBYARD_KEYS_CONSUMER_ROOT"]
	if consumerRoot == "" {
		consumerRoot = filepath.Join(loaded.Context.Paths.ConfigHome, "generated")
	}
	return credentialruntime.New(credentialruntime.Config{
		RepositoryRoot:    cli.options.RepositoryRoot,
		Root:              root,
		ConsumerRoot:      consumerRoot,
		ToolsDirectory:    loaded.Environment["SUBYARD_KEYS_TOOLS_DIR"],
		HostBase:          loaded.Context.Paths.HostBase,
		Context:           loaded.Context.YardName,
		Dispatcher:        cli.options.DispatcherPath,
		Environment:       environmentList(cli.env, loaded.Environment),
		TargetEnvironment: environmentList(cli.baseEnv, nil),
		Stdin:             stdin,
		Stdout:            stdout,
		Stderr:            stderr,
		Resolve: func(ctx context.Context, name string) (credentialruntime.Target, error) {
			if !domain.SafeName(name) {
				return credentialruntime.Target{}, fmt.Errorf("invalid credential target %q", name)
			}
			targetLoaded, err := cli.loadInventoryLoaded(name, loaded)
			if err != nil {
				return credentialruntime.Target{}, err
			}
			if targetLoaded.Context.AccessKind == domain.AccessRemote {
				return credentialruntime.Target{
					Name: name, Transport: "ssh", Destination: targetLoaded.Context.OwnerEndpoint,
					OwnerYardName: targetLoaded.Context.OwnerYardName,
				}, nil
			}
			return credentialruntime.Target{Name: name, Transport: "local"}, nil
		},
	})
}

func publicKeysCommandName(arguments []string) string {
	name := "keys"
	for _, argument := range arguments {
		if argument == "-y" || argument == "--yes" {
			continue
		}
		if !strings.HasPrefix(argument, "-") {
			return name + " " + argument
		}
		break
	}
	return name
}

func keysAssumeYes(arguments []string) bool {
	for _, argument := range arguments {
		if argument == "-y" || argument == "--yes" {
			return true
		}
	}
	return false
}

func keysWithoutConsent(arguments []string) []string {
	result := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		if argument != "-y" && argument != "--yes" {
			result = append(result, argument)
		}
	}
	return result
}

func (cli *CLI) runKeys(
	ctx context.Context,
	loaded config.Loaded,
	definition command.Definition,
	arguments []string,
) int {
	if credentialExactInvocation(definition, arguments) || definition.Name == "keys" && credentialProtectedInvocation(arguments) {
		return cli.runPreparedKeys(ctx, loaded, definition, arguments)
	}
	runtime, err := cli.credentialRuntime(loaded)
	if err != nil {
		cli.errorf("keys: %v", err)
		return 1
	}
	prepared, err := runtime.Prepare(ctx, definition.Arg0, keysWithoutConsent(arguments))
	if err != nil {
		cli.errorf("keys: %v", err)
		return 1
	}
	assumeYes := definition.Visibility != command.VisibilityPublic ||
		cli.env["ASSUME_YES"] == "1" || keysAssumeYes(arguments)
	name := definition.Name
	if definition.Name == "keys" {
		name = publicKeysCommandName(arguments)
	}
	orchestrator := cli.operationOrchestrator(cli.env["SUBYARD_OPERATION_ID"], loaded, nil, &definition)
	plan, err := orchestrator.PlanAction(
		ctx, loaded.Context, name, domain.RemotePolicy(definition.Remote), prepared.Action,
		domain.ActionDelta{Changed: prepared.Changed, Consequences: prepared.Consequences},
		assumeYes,
	)
	if err != nil {
		if errors.Is(err, application.ErrDeclined) {
			cli.errorf("operation declined")
		} else {
			cli.errorf("plan %s: %v", name, err)
		}
		return 1
	}
	orchestrator.Runner = credentialAdapter{prepared: prepared}
	result, _, err := orchestrator.RunAdapter(ctx, plan, domain.AdapterRequest{
		Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID,
		Adapter: "credential", Action: "execute",
	}, nil)
	if err != nil {
		cli.errorf("%s: %v", name, err)
		return 1
	}
	if result.Status != "ok" {
		cli.errorf("%s returned %s", name, result.Status)
		return 1
	}
	return 0
}

func (cli *CLI) runRemoteKeys(ctx context.Context, loaded config.Loaded, definition command.Definition, arguments []string) int {
	if credentialProtectedInvocation(arguments) {
		cli.errorf("keys: protected credential input requires the owner host; run this command on the owner")
		return 1
	}
	if credentialExactInvocation(definition, arguments) {
		return cli.runPreparedKeys(ctx, loaded, definition, arguments)
	}
	return cli.forwardRemote(ctx, loaded.Context, definition.Name, arguments)
}
