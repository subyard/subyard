package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/adapters/networkruntime"
	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

var errNetworkUsage = errors.New("invalid network arguments")

func networkArgumentError(message string) error {
	return fmt.Errorf("%w: %s", errNetworkUsage, message)
}

func (cli *CLI) networkUsage() {
	fmt.Fprintf(cli.options.Stdout, `Usage: %s network status [--json]
       %s network link <yard> <yard> [--yes]
       %s network unlink <yard> <yard> [--yes]
       %s network isolation <on|off> [--yes]
       %s network reconcile [--yes]

Links are bidirectional and apply only to yards on this host.
Isolation requires all yards to share one managed Incus bridge.
Isolation is disabled until explicitly enabled; saved links survive either mode.
Applying isolation or links while isolation is enabled briefly restarts affected running yards
and closes their active connections.
`, cli.options.Program, cli.options.Program, cli.options.Program, cli.options.Program, cli.options.Program)
}

func networkYard(y domain.Context) yardnetwork.Yard {
	return yardnetwork.Yard{Name: y.YardName, Project: y.IncusProject, Instance: y.YardInstanceName, Network: y.IncusBridge}
}

func (cli *CLI) networkService(yards []domain.Context) *yardnetwork.Service {
	if cli.options.NetworkPolicy != nil {
		return cli.options.NetworkPolicy
	}
	incus, _ := cli.statusPorts()
	host, ok := incus.(yardnetwork.Host)
	if !ok {
		return nil
	}
	bridges := []string{}
	for _, y := range yards {
		if y.IncusBridge != "" && !slices.Contains(bridges, y.IncusBridge) {
			bridges = append(bridges, y.IncusBridge)
		}
	}
	return &yardnetwork.Service{Host: host, Lock: networkruntime.HostLock{}, Guard: func(ctx context.Context) error { return (hostruntime.NetworkGuard{}).Check(ctx, bridges) }}
}

func (prepared *preparedCommand) prepareNetwork(ctx context.Context, _ *initBootstrap) error {
	if prepared.Loaded.Context.AccessKind != domain.AccessLocal {
		return errors.New("network policy requires the local owner host")
	}
	args := []string{}
	jsonOutput := false
	for _, arg := range prepared.Arguments {
		switch arg {
		case "--yes", "-y":
		case "--json":
			jsonOutput = true
		default:
			args = append(args, arg)
		}
	}
	if len(args) == 0 {
		return networkArgumentError("network requires status, link, unlink, isolation, or reconcile")
	}
	change := yardnetwork.Change{}
	switch args[0] {
	case "status", "reconcile":
		if len(args) != 1 {
			return networkArgumentError("unexpected network arguments")
		}
	case "isolation":
		if len(args) != 2 || (args[1] != "on" && args[1] != "off") {
			return networkArgumentError("usage: network isolation <on|off>")
		}
		enabled := args[1] == "on"
		change.Isolation = &enabled
	case "link", "unlink":
		if len(args) != 3 {
			return networkArgumentError("usage: network link|unlink <yard> <yard>")
		}
		hostID, _, err := configsync.ResolveHostID(prepared.Loaded.Context.Paths.ConfigHome, prepared.Loaded.Environment)
		if err != nil {
			return err
		}
		names := []string{}
		for _, arg := range args[1:] {
			selector, err := domain.ParseYardSelector(arg)
			if err != nil {
				return networkArgumentError(err.Error())
			}
			if selector.HostID != "" && selector.HostID != hostID {
				return networkArgumentError("network links require two yards on this host")
			}
			names = append(names, selector.YardName)
		}
		link := &yardnetwork.Link{A: names[0], B: names[1]}
		if link.A == link.B {
			return networkArgumentError("a link needs two distinct local yards")
		}
		if args[0] == "link" {
			change.Link = link
		} else {
			change.Unlink = link
		}
	default:
		return networkArgumentError(fmt.Sprintf("unknown network command %q", args[0]))
	}
	if jsonOutput && args[0] != "status" {
		return networkArgumentError("--json is supported only by network status")
	}
	contexts, err := prepared.CLI.powerYardContexts(prepared.Loaded)
	if err != nil {
		return err
	}
	service := prepared.CLI.networkService(contexts)
	if service == nil {
		return errors.New("Incus network policy adapter is unavailable")
	}
	yards := []yardnetwork.Yard{}
	for _, y := range contexts {
		yards = append(yards, networkYard(y))
	}
	if args[0] == "status" {
		status, err := service.Status(ctx, yards)
		if err != nil {
			return err
		}
		prepared.displayOnly = func() {
			if jsonOutput {
				_ = json.NewEncoder(prepared.CLI.options.Stdout).Encode(status)
				return
			}
			p := status.Policy
			fmt.Fprintf(prepared.CLI.options.Stdout, "Isolation: desired=%t applied=%t; converged=%t (revision %d/%d)\n", p.Isolation, p.AppliedIsolation, status.Converged, p.AppliedRevision, p.Revision)
			if status.Diagnostic != "" {
				fmt.Fprintln(prepared.CLI.options.Stdout, "Diagnostic: "+status.Diagnostic)
			}
			for _, link := range p.Links {
				fmt.Fprintf(prepared.CLI.options.Stdout, "  %s <-> %s\n", link.A, link.B)
			}
			if len(p.Links) == 0 {
				fmt.Fprintln(prepared.CLI.options.Stdout, "No explicit links.")
			}
		}
		return nil
	}
	plan, err := service.Prepare(ctx, yards, change)
	if err != nil {
		return err
	}
	actionDelta := func(p yardnetwork.Plan) (domain.ActionID, domain.ActionDelta, error) {
		action := domain.ActionID("yard.network.save")
		consequences := []string{"save explicit network links on this host; isolation remains disabled"}
		if p.Physical || p.Before.Policy.Isolation || p.Policy.Isolation {
			action = "yard.network.apply"
			mode := "disable host yard isolation and restore original network settings; retain saved links"
			if p.Policy.Isolation {
				mode = "enable host yard isolation; allow only explicitly linked peers"
			}
			consequences = []string{mode, "briefly stop and restart affected running yards; active connections will close", "on failure, keep incomplete policy visible and affected yards stopped; retry with yard network reconcile"}
			names := []string{}
			for _, u := range p.Updates {
				names = append(names, u.Yard.Name)
			}
			consequences = append(consequences, "affected yards: "+strings.Join(names, ", "))
		}
		if !p.Changed {
			consequences = nil
		}
		return action, domain.ActionDelta{Changed: p.Changed, Consequences: consequences}, nil
	}
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) { return actionDelta(plan) }
	prepared.executeNoOp = true
	prepared.refresh = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
		fresh, err := service.Prepare(ctx, yards, change)
		if err != nil {
			return "", domain.ActionDelta{}, err
		}
		if fresh.Fingerprint != plan.Fingerprint {
			return "", domain.ActionDelta{}, domain.ErrPlanStale
		}
		return actionDelta(fresh)
	}
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, _ io.Writer) (domain.AdapterResult, error) {
		orchestrator.Runner = networkPolicyAdapter{service: service, plan: plan}
		result, _, err := orchestrator.RunAdapter(ctx, prepared.Plan, domain.AdapterRequest{Schema: shelladapter.ProtocolSchema, OperationID: prepared.Plan.OperationID, Adapter: "network", Action: "apply"}, nil)
		return result, err
	}
	prepared.printResult = func(domain.AdapterResult) { fmt.Fprintln(prepared.CLI.options.Stdout, "Network policy converged.") }
	return nil
}

type networkPolicyAdapter struct {
	service *yardnetwork.Service
	plan    yardnetwork.Plan
}

func (adapter networkPolicyAdapter) Run(ctx context.Context, request domain.AdapterRequest, _ io.Reader) (domain.AdapterResult, string, error) {
	if request.Adapter != "network" || request.Action != "apply" || adapter.service == nil {
		return domain.AdapterResult{}, "", errors.New("invalid network policy adapter request")
	}
	if err := adapter.service.Apply(ctx, adapter.plan); err != nil {
		return domain.AdapterResult{}, "", err
	}
	return domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: request.OperationID, Status: "ok"}, "", nil
}
