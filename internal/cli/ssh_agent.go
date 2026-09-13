package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/adapters/sshagentruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

type sshAgentAdapter struct{ prepared sshagentruntime.Prepared }

func (adapter sshAgentAdapter) Run(ctx context.Context, request domain.AdapterRequest, _ io.Reader) (domain.AdapterResult, string, error) {
	if request.Adapter != "ssh-agent" || request.Action != "execute" {
		return domain.AdapterResult{}, "", errors.New("unsupported ssh-agent adapter request")
	}
	if err := adapter.prepared.Execute(ctx); err != nil {
		return domain.AdapterResult{}, "", err
	}
	return domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: request.OperationID, Status: "ok"}, "", nil
}

func (cli *CLI) sshAgentRuntime(loaded config.Loaded) (*sshagentruntime.Runtime, error) {
	dataHome := loaded.Context.Paths.DataHome
	operatorHome := loaded.Context.Paths.OperatorHome
	return sshagentruntime.New(sshagentruntime.Config{
		StateRoot: filepath.Join(dataHome, "ssh-agent"), Yard: loaded.Context.YardName,
		SSHHost: loaded.Context.SSHHost, SSHPort: loaded.Context.SSHPort,
		DevUser: loaded.Context.DevUser, DataHome: dataHome, OperatorHome: operatorHome,
		Dispatcher: cli.options.DispatcherPath, Environment: environmentList(cli.env, loaded.Environment),
		Stdin: cli.options.Stdin, Stdout: cli.options.Stdout, Stderr: cli.options.Stderr,
	})
}

func (cli *CLI) runSSHAgent(ctx context.Context, loaded config.Loaded, arguments []string) int {
	runtime, err := cli.sshAgentRuntime(loaded)
	if err != nil {
		cli.errorf("ssh-agent: %v", err)
		return 1
	}
	prepared, err := runtime.Prepare(ctx, keysWithoutConsent(arguments))
	if err != nil {
		cli.errorf("ssh-agent: %v", err)
		return 2
	}
	if prepared.Action == "ssh-agent.help" {
		if err := prepared.Execute(ctx); err != nil {
			cli.errorf("ssh-agent: %v", err)
			return 1
		}
		return 0
	}
	orchestrator := cli.operationOrchestrator(cli.ensureOperationID(), loaded, nil, nil)
	plan, err := orchestrator.PrepareAction(loaded.Context, "ssh-agent", domain.RemoteDenied,
		prepared.Action, domain.ActionDelta{Changed: prepared.Changed, Consequences: slices.Clone(prepared.Consequences)})
	if err != nil {
		cli.errorf("plan ssh-agent: %v", err)
		return 1
	}
	plan, err = orchestrator.Confirm(ctx, plan, slices.Contains(arguments, "--yes") || slices.Contains(arguments, "-y") || cli.env["ASSUME_YES"] == "1")
	if err != nil {
		if errors.Is(err, application.ErrDeclined) {
			cli.errorf("operation declined")
		} else {
			cli.errorf("plan ssh-agent: %v", err)
		}
		return 1
	}
	orchestrator.Runner = sshAgentAdapter{prepared: prepared}
	if _, _, err := orchestrator.RunAdapter(ctx, plan, domain.AdapterRequest{
		Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID,
		Adapter: "ssh-agent", Action: "execute",
	}, nil); err != nil {
		cli.errorf("ssh-agent: %v", err)
		return 1
	}
	return 0
}

func sshAgentAuditArguments(arguments []string) []string {
	result := slices.Clone(arguments)
	for index := range result {
		if result[index] == "--key" && index+1 < len(result) {
			result[index+1] = "<redacted>"
		}
		if strings.HasPrefix(result[index], "--key=") {
			result[index] = "--key=<redacted>"
		}
	}
	return result
}

func sshAgentReadOnlyInvocation(arguments []string) bool {
	for _, argument := range arguments {
		switch argument {
		case "-y", "--yes":
			continue
		case "help", "status", "-h", "--help":
			return true
		default:
			return false
		}
	}
	return true
}
