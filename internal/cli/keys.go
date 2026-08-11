package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/credentialruntime"
	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/adapters/transport"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/rpc"
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
			if targetLoaded.Context.YardType == domain.YardRemote {
				return credentialruntime.Target{
					Name: name, Transport: "ssh", Destination: targetLoaded.Context.RemoteDest,
					RemoteYard: targetLoaded.Context.RemoteYard,
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

func (cli *CLI) runRemoteKeys(
	ctx context.Context,
	loaded config.Loaded,
	definition command.Definition,
	arguments []string,
) int {
	plan, err := cli.prepareRemoteKeys(ctx, loaded, arguments)
	if err != nil {
		cli.errorf("plan %s on owner: %v", publicKeysCommandName(arguments), err)
		return 1
	}
	if plan.OperationID != cli.env["SUBYARD_OPERATION_ID"] ||
		plan.Command != publicKeysCommandName(arguments) || plan.Target != domain.TargetLocalOwner {
		cli.errorf("plan %s on owner: owner returned a plan for another operation", publicKeysCommandName(arguments))
		return 1
	}
	orchestrator := cli.operationOrchestrator(plan.OperationID, loaded, nil, &definition)
	if _, err := orchestrator.Confirm(
		ctx, plan, cli.env["ASSUME_YES"] == "1" || keysAssumeYes(arguments),
	); err != nil {
		if errors.Is(err, application.ErrDeclined) {
			cli.errorf("operation declined")
		} else {
			cli.errorf("plan %s: %v", plan.Command, err)
		}
		return 1
	}
	forwardArguments := append(keysWithoutConsent(arguments), "--yes")
	return cli.forwardRemote(ctx, loaded.Context, definition.Name, forwardArguments)
}

func (cli *CLI) prepareRemoteKeys(
	ctx context.Context,
	loaded config.Loaded,
	arguments []string,
) (domain.OperationPlan, error) {
	ownerYard := loaded.Context.RemoteYard
	if ownerYard == "" {
		ownerYard = "default"
	}
	process, err := transport.SSHYard("ssh", loaded.Context.RemoteDest, ownerYard, 3*time.Second)
	if err != nil {
		return domain.OperationPlan{}, err
	}
	process.Env = environmentList(cli.env, nil)
	process.Timeout = 8 * time.Second
	process.MaxBytes = rpc.MaxFrameSize
	params, err := json.Marshal(struct {
		Arguments []string `json:"arguments"`
	}{Arguments: arguments})
	if err != nil {
		return domain.OperationPlan{}, err
	}
	var request bytes.Buffer
	codec := rpc.NewCodec(bytes.NewReader(nil), &request)
	if err := codec.Write(rpc.Request{
		Version: rpc.ProtocolVersion, Type: "request", ID: "negotiate", Method: "rpc.negotiate",
	}); err != nil {
		return domain.OperationPlan{}, err
	}
	if err := codec.Write(rpc.Request{
		Version: rpc.ProtocolVersion, Type: "request", ID: "keys-prepare",
		OperationID: cli.env["SUBYARD_OPERATION_ID"], Method: "keys.prepare", Params: params,
	}); err != nil {
		return domain.OperationPlan{}, err
	}
	response, err := process.Call(ctx, loaded.Context.RemoteDest, request.Bytes())
	if err != nil {
		return domain.OperationPlan{}, err
	}
	return decodeRemoteKeysPlan(response)
}

func decodeRemoteKeysPlan(payload []byte) (domain.OperationPlan, error) {
	codec := rpc.NewCodec(bytes.NewReader(payload), io.Discard)
	negotiated := false
	for {
		response, err := codec.ReadResponse()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return domain.OperationPlan{}, fmt.Errorf("decode owner RPC: %w", err)
		}
		if response.Error != nil {
			return domain.OperationPlan{}, response.Error
		}
		switch response.ID {
		case "negotiate":
			encoded, err := json.Marshal(response.Result)
			if err != nil {
				return domain.OperationPlan{}, err
			}
			var result struct {
				Capabilities []string `json:"capabilities"`
			}
			if err := json.Unmarshal(encoded, &result); err != nil {
				return domain.OperationPlan{}, errors.New("owner returned invalid RPC negotiation")
			}
			for _, capability := range result.Capabilities {
				negotiated = negotiated || capability == credentialPrepareCapability
			}
			if !negotiated {
				return domain.OperationPlan{}, errors.New("owner does not support credential preparation; update Subyard on the owner host")
			}
		case "keys-prepare":
			if !negotiated {
				return domain.OperationPlan{}, errors.New("owner credential plan arrived before RPC negotiation")
			}
			encoded, err := json.Marshal(response.Result)
			if err != nil {
				return domain.OperationPlan{}, err
			}
			var plan domain.OperationPlan
			decoder := json.NewDecoder(bytes.NewReader(encoded))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&plan); err != nil {
				return domain.OperationPlan{}, errors.New("owner returned an invalid credential plan")
			}
			return plan, nil
		}
	}
	return domain.OperationPlan{}, errors.New("owner returned no credential plan")
}
