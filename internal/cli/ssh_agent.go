package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/adapters/sshagentruntime"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/sshidentity"
)

type sshAgentInvocation struct {
	verb, key       string
	ttl             time.Duration
	json, help, yes bool
}

func parseSSHAgentArguments(arguments []string) (sshAgentInvocation, error) {
	var result sshAgentInvocation
	for i := 0; i < len(arguments); i++ {
		argument := arguments[i]
		switch argument {
		case "--yes", "-y":
			result.yes = true
		case "--help", "-h":
			result.help = true
		case "--json":
			result.json = true
		case "--key", "--ttl":
			if i+1 >= len(arguments) {
				return result, fmt.Errorf("%s requires a value", argument)
			}
			i++
			value := arguments[i]
			if argument == "--key" {
				if result.key != "" || value == "" || strings.HasPrefix(value, "-") {
					return result, errors.New("--key requires one file path")
				}
				result.key = value
			} else {
				ttl, err := time.ParseDuration(value)
				if err != nil || ttl < time.Second || ttl%time.Second != 0 || ttl > 24*time.Hour || result.ttl != 0 {
					return result, errors.New("--ttl requires one whole-second duration between 1s and 24h, for example 30m or 2h")
				}
				result.ttl = ttl
			}
		default:
			if result.verb != "" || strings.HasPrefix(argument, "-") {
				return result, errors.New("unexpected ssh-agent argument")
			}
			result.verb = argument
		}
	}
	if result.help || result.verb == "" {
		result.help = true
		return result, nil
	}
	switch result.verb {
	case "unlock":
		if result.key == "" || result.ttl == 0 || result.json {
			return result, errors.New("unlock requires --key PATH --ttl DURATION")
		}
	case "status", "lock":
		if result.key != "" || result.ttl != 0 || result.verb == "lock" && result.json {
			return result, errors.New("unexpected option for ssh-agent " + result.verb)
		}
	default:
		return result, errors.New("ssh-agent expects unlock, status or lock")
	}
	return result, nil
}

func (cli *CLI) runSSHAgent(ctx context.Context, loaded config.Loaded, definition command.Definition, arguments []string) int {
	invocation, err := parseSSHAgentArguments(arguments)
	if err != nil {
		cli.errorf("ssh-agent: %v", err)
		return 2
	}
	if invocation.help {
		fmt.Fprintf(cli.options.Stdout, "Usage: %s ssh-agent unlock --key PATH --ttl DURATION [--yes]\n       %s ssh-agent status [--json]\n       %s ssh-agent lock\n\nRun on the yard owner host. Unlock asks for the key passphrase in the terminal.\nExpiry and lock prevent new authentication; existing SSH connections are not disconnected.\n", cli.options.Program, cli.options.Program, cli.options.Program)
		return 0
	}
	yard := loaded.Context
	manager := sshagentruntime.Manager{Config: sshagentruntime.Config{
		Directory:  sshagentruntime.Directory(yard.Paths.DataHome, yard.YardName),
		Executable: cli.options.DispatcherPath,
		SSHPort:    yard.SSHPort, Developer: yard.DevUser,
		IdentityFile:   filepath.Join(yard.Paths.DataHome, "ssh", "id_ed25519"),
		KnownHostsFile: filepath.Join(yard.Paths.DataHome, "ssh", "known_hosts"),
	}, Stdin: cli.options.Stdin, Stdout: cli.options.Stdout, Stderr: cli.options.Stderr,
		Environment: environmentList(cli.env, map[string]string{"SSH_ASKPASS_REQUIRE": "never"})}
	if invocation.verb == "status" {
		status, err := manager.Status(ctx)
		if err != nil {
			cli.errorf("ssh-agent status: %v", err)
			return 1
		}
		return cli.writeSSHAgentStatus(status, invocation.json)
	}
	var setup []byte
	if invocation.verb == "unlock" {
		if cli.operatorTerminal == nil || !cli.operatorTerminal() {
			cli.errorf("ssh-agent unlock requires a terminal on the owner host for the key passphrase")
			return 1
		}
		if !filepath.IsAbs(invocation.key) {
			invocation.key = filepath.Join(cli.options.WorkingDir, invocation.key)
		}
		if keyErr := sshagentruntime.ValidateKey(invocation.key); keyErr != nil {
			cli.errorf("ssh-agent unlock: %v", keyErr)
			return 1
		}
		if sshidentity.Classify(yard.Paths.OperatorHome, yard.Paths.DataHome, yard.YardName) != sshidentity.Dedicated {
			cli.errorf("yard SSH transport requires reconciliation; run yard init first")
			return 1
		}
		incus, _ := cli.statusPorts()
		instance, stateErr := incus.Instance(ctx, yard.IncusProject, yard.YardInstanceName)
		if stateErr != nil || !strings.EqualFold(instance.Status, "running") {
			cli.errorf("SSH access requires a running yard; run yard start first")
			return 1
		}
		setup, err = os.ReadFile(filepath.Join(cli.options.RepositoryRoot, "scripts", "ssh-agent-environment.sh"))
		if err != nil {
			cli.errorf("SSH agent environment adapter is unavailable")
			return 1
		}
	}
	consequences := []string(nil)
	if invocation.verb == "unlock" {
		consequences = []string{
			fmt.Sprintf("allow processes in yard %s to authenticate with the selected SSH key for %s", yard.YardName, invocation.ttl),
			"replace any existing temporary grant for this yard; the host's ordinary agent is unchanged",
			"configure the yard environment and, if needed for first setup, restart Orca once",
			"expiry and lock stop new authentication, not already authenticated SSH connections",
		}
	}
	orchestrator := cli.operationOrchestrator(cli.ensureOperationID(), loaded, nil, &definition)
	plan, err := orchestrator.PlanAction(ctx, yard, "ssh-agent "+invocation.verb, domain.RemotePolicy(definition.Remote),
		domain.ActionID("ssh-agent."+invocation.verb), domain.ActionDelta{Changed: true, Consequences: consequences}, invocation.yes || cli.env["ASSUME_YES"] == "1")
	if err != nil {
		cli.errorf("ssh-agent: %v", err)
		return 1
	}
	var status sshagentruntime.Status
	orchestrator.Runner = sshAgentAdapter{execute: func(ctx context.Context) error {
		if invocation.verb == "lock" {
			return manager.Lock(ctx)
		}
		_, executor := cli.statusPorts()
		setupCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		result, err := executor.Exec(setupCtx, yard.IncusProject, yard.YardInstanceName, ports.InstanceExecRequest{
			Command: []string{"sh", "-eu", "-s", "--", "ensure", yard.DevUser}, Stdin: setup,
		})
		if err != nil || result.ExitCode != 0 {
			return errors.New("could not configure the yard SSH-agent environment")
		}
		status, err = manager.Unlock(ctx, invocation.key, invocation.ttl)
		return err
	}}
	_, _, err = orchestrator.RunAdapter(ctx, plan, domain.AdapterRequest{Schema: shelladapter.ProtocolSchema,
		OperationID: plan.OperationID, Adapter: "ssh-agent", Action: invocation.verb}, nil)
	if err != nil {
		cli.errorf("ssh-agent %s: %v", invocation.verb, err)
		return 1
	}
	if invocation.verb == "lock" {
		fmt.Fprintln(cli.options.Stdout, "SSH key access: locked. Existing authenticated SSH connections are unchanged.")
		return 0
	}
	return cli.writeSSHAgentStatus(status, false)
}

func (cli *CLI) writeSSHAgentStatus(status sshagentruntime.Status, machine bool) int {
	if machine {
		if err := json.NewEncoder(cli.options.Stdout).Encode(status); err != nil {
			cli.errorf("write SSH-agent status: %v", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(cli.options.Stdout, "SSH key access: %s", status.State)
	if status.RemainingSeconds > 0 {
		fmt.Fprintf(cli.options.Stdout, " (%s remaining; expires %s)", time.Duration(status.RemainingSeconds)*time.Second, status.ExpiresAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintln(cli.options.Stdout)
	return 0
}

type sshAgentAdapter struct{ execute func(context.Context) error }

func (adapter sshAgentAdapter) Run(ctx context.Context, request domain.AdapterRequest, _ io.Reader) (domain.AdapterResult, string, error) {
	if request.Adapter != "ssh-agent" || request.Action != "unlock" && request.Action != "lock" {
		return domain.AdapterResult{}, "", errors.New("unsupported SSH-agent adapter action")
	}
	if err := adapter.execute(ctx); err != nil {
		return domain.AdapterResult{}, "", err
	}
	return domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: request.OperationID, Status: "ok"}, "", nil
}
