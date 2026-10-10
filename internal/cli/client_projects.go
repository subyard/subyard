package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/transport"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/clientprojects"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/state"
)

type clientProjectsRequest struct {
	open, check bool
	configPath  string
}

func parseClientProjectsArguments(arguments []string) (clientProjectsRequest, error) {
	var request clientProjectsRequest
	verb := ""
	for index := 0; index < len(arguments); index++ {
		switch argument := arguments[index]; argument {
		case "--yes", "-y":
		case "--check":
			request.check = true
		case "--config":
			index++
			if index == len(arguments) || request.configPath != "" || !filepath.IsAbs(arguments[index]) {
				return request, errors.New("--config requires one absolute GUI configuration path")
			}
			request.configPath = arguments[index]
		case "export", "open":
			if verb != "" {
				return request, errors.New("choose exactly one action: export or open")
			}
			verb = argument
			request.open = argument == "open"
		default:
			return request, fmt.Errorf("unexpected argument %q; use export|open [--check] [--config PATH]", argument)
		}
	}
	if verb == "" {
		return request, errors.New("choose an action: export or open")
	}
	return request, nil
}

func (cli *CLI) clientProjectsUsage(definition command.Definition) {
	fmt.Fprintf(cli.options.Stdout, "Usage: %s [-Y <yard or HostID/yard>] %s export|open [--check] [--config PATH]\n", cli.options.Program, definition.Name)
	fmt.Fprintln(cli.options.Stdout, "Export every registered project of one selected yard using its existing L1 SSH alias.")
	fmt.Fprintln(cli.options.Stdout, "Run on the GNU/Linux desktop controller. --check previews without writing or opening the application.")
	fmt.Fprintln(cli.options.Stdout, "--config selects the absolute configuration file used by the GUI. open requests import even after an unchanged export.")
	fmt.Fprintln(cli.options.Stdout, "Prepare SSH and host trust separately; this command never registers, repairs or starts a yard.")
}

func (prepared *preparedCommand) prepareClientProjects(ctx context.Context, _ *initBootstrap) error {
	cli := prepared.CLI
	request, err := parseClientProjectsArguments(prepared.Arguments)
	if err != nil {
		return err
	}
	export, route, yard, err := cli.selectedClientProjects(ctx, prepared.Loaded, cli.env["SUBYARD_YARD"])
	if err != nil {
		return err
	}
	if err := cli.probeClientYard(ctx, route, yard); err != nil {
		return err
	}
	prepare := cli.options.ClientProjects
	if prepare == nil {
		prepare = cli.prepareDesktopClient
	}
	plan, err := prepare(ctx, prepared.Definition.Arg0, export, request.configPath)
	if err != nil {
		return err
	}
	if plan.Apply == nil || (request.open && plan.Open == nil) {
		return errors.New("desktop client returned an incomplete export plan")
	}
	preview := func() {
		change := "unchanged"
		if plan.Changed {
			change = "update"
		}
		fmt.Fprintf(cli.options.Stdout, "Yard: %s/%s\nSSH: %s\nProjects: %d\nConfig: %s\nChanges: %s (%d added projects)\n", export.HostID, export.Yard, export.SSHHost, len(export.Projects), plan.Target, change, plan.Added)
		if len(export.Projects) == 0 {
			fmt.Fprintln(cli.options.Stdout, "Selected yard has no registered projects; existing declarations are preserved.")
		}
		if request.open {
			fmt.Fprintln(cli.options.Stdout, "Open: request configuration import in the desktop application")
		}
	}
	if request.check {
		prepared.displayOnly = preview
		return nil
	}
	prepared.preview = preview
	prepared.executeNoOp = true
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return "client.projects-export", domain.ActionDelta{Changed: plan.Changed || request.open,
			Consequences: []string{"export selected yard projects to " + plan.Target}}, nil
	}
	prepared.execute = func(ctx context.Context, _ *application.Orchestrator, _ io.Writer) (domain.AdapterResult, error) {
		result := domain.AdapterResult{Schema: 1, OperationID: prepared.Plan.OperationID, Status: "ok"}
		// A prepared export cannot silently publish paths for a changed owner or route.
		current, currentRoute, currentYard, err := cli.selectedClientProjects(ctx, prepared.Loaded, cli.env["SUBYARD_YARD"])
		if err != nil {
			return result, err
		}
		if !reflect.DeepEqual(export, current) || route.SSHHost != currentRoute.SSHHost ||
			!reflect.DeepEqual(yard, currentYard) {
			return result, fmt.Errorf("%w: selected yard inventory or SSH route changed; retry the export", domain.ErrPlanStale)
		}
		if err = cli.probeClientYard(ctx, currentRoute, currentYard); err != nil {
			return result, err
		}
		if err = plan.Apply(ctx); err != nil {
			return result, err
		}
		if request.open {
			if err := plan.Open(ctx); err != nil {
				return result, err
			}
			fmt.Fprintln(cli.options.Stdout, "Desktop import requested; verify the projects in the application.")
		} else {
			fmt.Fprintln(cli.options.Stdout, "Project declaration exported.")
		}
		return result, nil
	}
	return nil
}

func (cli *CLI) fetchClientOwner(ctx context.Context, base config.Loaded, connection ownerinventory.Connection) (domain.OwnerInventory, error) {
	store := ownerinventory.Connections{Root: filepath.Join(base.Context.Paths.DataHome, "owner-inventory")}
	client, err := store.TrustedSSHClientReadOnly(connection, "ssh", 3*time.Second)
	if err != nil {
		return domain.OwnerInventory{}, err
	}
	// Fetch checks the exact registered identity; export never adopts a rename.
	return client.Fetch(ctx, connection.HostID)
}

func (cli *CLI) clientLocalInventory(ctx context.Context, loaded config.Loaded) (domain.OwnerInventory, error) {
	return (application.OwnerInventoryBuilder{Source: cliOwnerSource{
		cli: cli, loaded: loaded, readOnly: true, selected: &loaded.Context,
	}, Clock: cli.options.Clock}).Read(ctx)
}

// selectedClientProjects resolves exactly one current yard, never the listing's
// special default/all scope. Canonical and registered aliases fetch only their owner.
func (cli *CLI) selectedClientProjects(ctx context.Context, base config.Loaded, selector string) (clientprojects.Export, domain.Context, domain.OwnerYard, error) {
	var inventory domain.OwnerInventory
	var selected config.Loaded
	var err error
	localID, _, err := configsync.ResolveHostID(base.Context.Paths.ConfigHome, base.Environment)
	if err != nil {
		return clientprojects.Export{}, domain.Context{}, domain.OwnerYard{}, err
	}
	connections, err := (ownerinventory.Connections{Root: filepath.Join(base.Context.Paths.DataHome, "owner-inventory")}).ListReadOnly()
	if err != nil {
		return clientprojects.Export{}, domain.Context{}, domain.OwnerYard{}, err
	}
	hostID, yardName, canonical := strings.Cut(selector, "/")
	if canonical {
		if !domain.SafeID(hostID) || !domain.SafeName(yardName) || strings.Contains(yardName, "/") {
			return clientprojects.Export{}, domain.Context{}, domain.OwnerYard{}, fmt.Errorf("invalid yard selector %q; use <HostID>/<yard>", selector)
		}
		if hostID == localID {
			selected, err = cli.loadInventoryLoaded(yardName, base)
			if err == nil && selected.Context.AccessKind != domain.AccessLocal {
				err = errors.New("canonical local yard resolves to a remote registration")
			}
			if err == nil {
				inventory, err = cli.clientLocalInventory(ctx, selected)
			}
		} else {
			found := false
			for _, connection := range connections {
				if connection.HostID == hostID {
					found = true
					inventory, err = cli.fetchClientOwner(ctx, base, connection)
					break
				}
			}
			if !found {
				err = fmt.Errorf("OwnerHost %q has no registered connection", hostID)
			}
			selected = base
		}
	} else {
		selected, err = cli.loadInventoryLoaded(selector, base)
		if err == nil && selected.Context.AccessKind == domain.AccessRemote {
			yardName = selected.Context.OwnerYardName
			if yardName == "" {
				yardName = "default"
			}
			found := false
			for _, connection := range connections {
				if connection.Destination == selected.Context.OwnerEndpoint {
					found = true
					hostID = connection.HostID
					inventory, err = cli.fetchClientOwner(ctx, base, connection)
					break
				}
			}
			if !found {
				err = errors.New("remote yard has no registered trusted owner; prepare its connection separately")
			}
		} else if err == nil {
			hostID, yardName = localID, selected.Context.YardName
			inventory, err = cli.clientLocalInventory(ctx, selected)
			// Bare named selectors retain ordinary ambiguity checks. Failed unrelated
			// owners are ignored unless their registered routes share this yard name.
			if err == nil && selector != "default" && selector != "" {
				matches := []ownerInventoryResult{{inventory: inventory}}
				for _, connection := range connections {
					remote, fetchErr := cli.fetchClientOwner(ctx, base, connection)
					if fetchErr == nil {
						matches = append(matches, ownerInventoryResult{inventory: remote})
					} else if _, known := connection.Yards[yardName]; known {
						matches = append(matches, ownerInventoryResult{inventory: domain.OwnerInventory{HostID: connection.HostID, Yards: []domain.OwnerYard{{Name: yardName}}}, err: fetchErr})
					}
				}
				_, _, err = selectOwnerYards(matches, yardName)
			}
		}
	}
	if err != nil {
		return clientprojects.Export{}, domain.Context{}, domain.OwnerYard{}, fmt.Errorf("read selected yard %q: %w", selector, err)
	}
	var yard domain.OwnerYard
	found := false
	for _, candidate := range inventory.Yards {
		if candidate.Name == yardName {
			yard, found = candidate, true
			break
		}
	}
	if !found {
		return clientprojects.Export{}, domain.Context{}, domain.OwnerYard{}, fmt.Errorf("yard %s/%s was not found in fresh owner inventory", hostID, yardName)
	}
	if !strings.EqualFold(yard.State, "running") {
		return clientprojects.Export{}, domain.Context{}, domain.OwnerYard{}, fmt.Errorf("yard %s/%s is %s; start it separately before exporting", hostID, yardName, yard.State)
	}
	_, route, err := cli.ownerYardRouteReadOnly(ctx, selected, hostID, yardName)
	if err != nil {
		return clientprojects.Export{}, domain.Context{}, domain.OwnerYard{}, err
	}
	export := clientprojects.Export{HostID: hostID, Yard: yardName, SSHHost: route.SSHHost, Projects: []clientprojects.Project{}}
	for _, project := range yard.Projects {
		export.Projects = append(export.Projects, clientprojects.Project{Name: project.Name, Path: state.YardPath(project.ProjectID)})
	}
	slices.SortFunc(export.Projects, func(a, b clientprojects.Project) int { return strings.Compare(a.Path, b.Path) })
	return export, route, yard, nil
}

func (cli *CLI) probeClientYard(ctx context.Context, route domain.Context, yard domain.OwnerYard) error {
	if cli.options.ClientYardProbe != nil {
		return cli.options.ClientYardProbe(ctx, route, yard)
	}
	if !domain.SafeSSHTarget(route.SSHHost) {
		return errors.New("selected yard has no safe existing SSH alias")
	}
	environment := environmentList(cli.env, nil)
	configuration, err := (transport.Process{Program: "ssh", Arguments: []string{"-G", route.SSHHost},
		Env: environment, Timeout: 5 * time.Second, MaxBytes: 1 << 20}).Call(ctx, "", nil)
	if err != nil {
		return fmt.Errorf("resolve existing yard SSH alias %s: %w", route.SSHHost, err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(configuration), "\n") {
		key, value, ok := strings.Cut(line, " ")
		if ok {
			values[key] = strings.TrimSpace(value)
		}
	}
	if values["hostname"] == "" || values["hostname"] == route.SSHHost ||
		values["port"] != strconv.Itoa(yard.SSHPort) || values["user"] != yard.DevUser {
		return fmt.Errorf("SSH alias %s does not resolve to the selected yard's existing L1 connection; prepare it separately", route.SSHHost)
	}
	// The trust gate verifies existing pins for the entire proxy chain and refuses
	// unknown keys. No project directory discovery or per-project RPC is needed.
	output, err := (transport.Process{Program: "ssh", SSHTarget: route.SSHHost,
		Arguments: []string{"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=3", "-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes", route.SSHHost, "--", "hostname; id -un; test -d /srv/workspaces && printf 'subyard-workspaces\\n'"},
		Env:       environment, Timeout: 10 * time.Second, MaxBytes: 4096}).Call(ctx, "", nil)
	if err != nil {
		return fmt.Errorf("existing L1 SSH connection %s is unavailable: %w", route.SSHHost, err)
	}
	want := yard.Instance + "\n" + yard.DevUser + "\nsubyard-workspaces"
	if strings.TrimSpace(string(output)) != want {
		return fmt.Errorf("SSH alias %s does not reach the selected L1 yard %s as %s", route.SSHHost, yard.Instance, yard.DevUser)
	}
	return nil
}
