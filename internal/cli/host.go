package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ownerinventory"
)

var errHostUsage = errors.New("invalid host invocation")

func hostArguments(arguments []string) []string {
	filtered := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		if argument != "-y" && argument != "--yes" {
			filtered = append(filtered, argument)
		}
	}
	return filtered
}

func hostReadOnlyInvocation(arguments []string) bool {
	arguments = hostArguments(arguments)
	return len(arguments) == 0 || commandHelpRequested(arguments) || (len(arguments) == 1 && arguments[0] == "list")
}

func (cli *CLI) hostUsage() {
	fmt.Fprintf(cli.options.Stdout, "Usage: %s host add <owner-endpoint> | list | rename <new-host-id> | remove <host-id> | repair <host-id> [--yes]\n", cli.options.Program)
}

func (cli *CLI) runHost(ctx context.Context, loaded config.Loaded, arguments []string) int {
	filtered := hostArguments(arguments)
	if len(filtered) == 0 || commandHelpRequested(filtered) {
		cli.hostUsage()
		return 0
	}
	if len(filtered) == 1 && filtered[0] == "list" {
		return cli.listRegisteredHosts(loaded)
	}
	definition, ok := cli.manifest.Lookup("host")
	if !ok {
		cli.errorf("host command is unavailable")
		return 1
	}
	prepared, err := cli.prepareCommand(ctx, prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: arguments})
	if err != nil {
		return cli.reportPreparationError(definition, err)
	}
	defer prepared.Close()
	return cli.runPreparedCommand(ctx, prepared, cli.env["ASSUME_YES"] == "1")
}

func (cli *CLI) listRegisteredHosts(loaded config.Loaded) int {
	connections, err := (ownerinventory.Connections{
		Root: filepath.Join(loaded.Context.Paths.DataHome, "owner-inventory"),
	}).List()
	if err != nil {
		cli.errorf("read owner-host registrations: %v", err)
		return 1
	}
	fmt.Fprintf(cli.options.Stdout, "%-24s %s\n", "HOST ID", "OWNER ENDPOINT")
	for _, connection := range connections {
		fmt.Fprintf(cli.options.Stdout, "%-24s %s\n", connection.HostID, connection.Destination)
	}
	return 0
}
