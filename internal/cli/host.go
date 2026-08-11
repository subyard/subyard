package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
)

func (cli *CLI) runHost(ctx context.Context, loaded config.Loaded, arguments []string) int {
	assumeYes := cli.env["ASSUME_YES"] == "1"
	filtered := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		if argument == "-y" || argument == "--yes" {
			assumeYes = true
			continue
		}
		filtered = append(filtered, argument)
	}
	if len(filtered) == 0 || commandHelpRequested(filtered) {
		fmt.Fprintf(cli.options.Stdout, "Usage: %s host add <owner-endpoint> | list | rename <new-host-id> | remove <host-id> | repair <host-id> [--yes]\n", cli.options.Program)
		return 0
	}
	if len(filtered) == 1 && filtered[0] == "list" {
		return cli.listRegisteredHosts(loaded)
	}
	if len(filtered) != 2 {
		cli.errorf("host expects add <owner-endpoint>, list, rename, remove or repair")
		return 2
	}
	if filtered[0] == "add" {
		return cli.addRegisteredHost(ctx, loaded, filtered[1], assumeYes)
	}
	if filtered[0] == "remove" || filtered[0] == "repair" {
		return cli.runRegisteredHost(ctx, loaded, filtered[0], filtered[1], assumeYes)
	}
	if filtered[0] != "rename" {
		cli.errorf("unknown host subcommand %q", filtered[0])
		return 2
	}
	if err := configsync.RecoverHostIDRename(loaded.Context.Paths.ConfigHome); err != nil {
		cli.errorf("recover owner HostID rename: %v", err)
		return 1
	}
	plan, err := configsync.PrepareHostIDRename(loaded.Context.Paths.ConfigHome, filtered[1])
	if err != nil {
		cli.errorf("prepare owner HostID rename: %v", err)
		return 1
	}
	summary := fmt.Sprintf("Rename owner HostID %s -> %s", plan.OldHostID, plan.NewHostID)
	consequences := []string{
		"replace the persisted owner HostID",
		"migrate machine-local configuration references atomically",
		"leave yard, Incus, storage and SSH runtime resource names unchanged",
	}
	confirmed := assumeYes
	if !confirmed {
		confirmed, err = cli.confirmHostRename(ctx, summary, consequences)
		if err != nil {
			cli.errorf("confirm owner HostID rename: %v", err)
			return 1
		}
	}
	if !confirmed {
		fmt.Fprintln(cli.options.Stdout, "Owner HostID rename cancelled.")
		return 1
	}
	if err := configsync.ApplyHostIDRename(plan); err != nil {
		cli.errorf("rename owner HostID: %v", err)
		return 1
	}
	fmt.Fprintf(cli.options.Stdout, "Owner HostID renamed: %s -> %s\n", plan.OldHostID, plan.NewHostID)
	return 0
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

func (cli *CLI) addRegisteredHost(
	ctx context.Context, loaded config.Loaded, endpoint string, assumeYes bool,
) int {
	if !domain.SafeSSHTarget(endpoint) {
		cli.errorf("invalid owner endpoint %q", endpoint)
		return 2
	}
	root := filepath.Join(loaded.Context.Paths.DataHome, "owner-inventory")
	trust, err := ownerinventory.AssessSSHHostKey(ctx, root, "ssh", endpoint, 3*time.Second)
	if err != nil {
		cli.errorf("assess OwnerHost SSH key: %v", err)
		return 1
	}
	client, err := ownerinventory.CandidateSSHClient(root, endpoint, trust, "ssh", 3*time.Second)
	if err != nil {
		cli.errorf("prepare OwnerHost transport: %v", err)
		return 1
	}
	callContext, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	inventory, err := client.Fetch(callContext, "")
	if err != nil {
		cli.errorf("read authoritative owner inventory: %v", err)
		return 1
	}
	yardNames := make([]string, 0, len(inventory.Yards))
	for _, yard := range inventory.Yards {
		yardNames = append(yardNames, yard.Name)
	}
	sort.Strings(yardNames)
	store := ownerinventory.Connections{Root: root}
	fetchedAt := time.Now().UTC()
	if cli.options.Clock != nil {
		fetchedAt = cli.options.Clock.Now().UTC()
	}
	plan, err := store.PrepareRegistration(ownerinventory.Connection{
		HostID: inventory.HostID, Destination: endpoint, Trust: &trust,
	}, ownerinventory.Snapshot{FetchedAt: fetchedAt, Inventory: inventory})
	if err != nil {
		cli.errorf("prepare OwnerHost registration: %v", err)
		return 1
	}
	summary := fmt.Sprintf(
		"Register OwnerHost %s at %s (SSH %s)", inventory.HostID, endpoint, trust.Fingerprint,
	)
	consequences := []string{
		"store one controller connection keyed by the authoritative HostID",
		"pin SSH server key " + trust.Fingerprint + " for strict owner refresh",
		fmt.Sprintf("discover authoritative yards [%s] without controller yard aliases", strings.Join(yardNames, ", ")),
		"cache owner inventory; leave the owner host and all runtime resources unchanged",
	}
	if !assumeYes {
		confirmed, confirmErr := cli.confirmHostRename(ctx, summary, consequences)
		if confirmErr != nil {
			cli.errorf("confirm OwnerHost registration: %v", confirmErr)
			return 1
		}
		if !confirmed {
			fmt.Fprintln(cli.options.Stdout, "OwnerHost registration cancelled.")
			return 1
		}
	}
	if err := store.ApplyRegistration(plan); err != nil {
		cli.errorf("register OwnerHost: %v", err)
		return 1
	}
	fmt.Fprintf(cli.options.Stdout, "OwnerHost registered: %s (%d yards)\n", inventory.HostID, len(inventory.Yards))
	return 0
}

func (cli *CLI) runRegisteredHost(
	ctx context.Context, loaded config.Loaded, action, hostID string, assumeYes bool,
) int {
	store := ownerinventory.Connections{
		Root: filepath.Join(loaded.Context.Paths.DataHome, "owner-inventory"),
	}
	connections, err := store.List()
	if err != nil {
		cli.errorf("read owner-host registrations: %v", err)
		return 1
	}
	var connection ownerinventory.Connection
	found := false
	for _, candidate := range connections {
		if candidate.HostID == hostID {
			connection, found = candidate, true
			break
		}
	}
	if !found {
		cli.errorf("OwnerHost %q is not registered", hostID)
		return 1
	}
	if action == "repair" {
		root := filepath.Join(loaded.Context.Paths.DataHome, "owner-inventory")
		candidateTrust, assessErr := ownerinventory.AssessSSHHostKey(
			ctx, root, "ssh", connection.Destination, 3*time.Second,
		)
		if assessErr != nil {
			cli.errorf("assess OwnerHost %q SSH key: %v", hostID, assessErr)
			return 1
		}
		candidateClient, clientErr := ownerinventory.CandidateSSHClient(
			root, connection.Destination, candidateTrust, "ssh", 3*time.Second,
		)
		if clientErr != nil {
			cli.errorf("prepare OwnerHost %q candidate transport: %v", hostID, clientErr)
			return 1
		}
		callContext, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		inventory, fetchErr := candidateClient.Fetch(callContext, "")
		if fetchErr != nil {
			cli.errorf("read OwnerHost %q authoritative inventory through candidate key: %v", hostID, fetchErr)
			return 1
		}
		fetchedAt := time.Now().UTC()
		if cli.options.Clock != nil {
			fetchedAt = cli.options.Clock.Now().UTC()
		}
		plan, prepareErr := store.PrepareRepair(hostID, candidateTrust, ownerinventory.Snapshot{
			FetchedAt: fetchedAt, Inventory: inventory,
		})
		if prepareErr != nil {
			cli.errorf("prepare OwnerHost %q repair: %v", hostID, prepareErr)
			return 1
		}
		summary := fmt.Sprintf(
			"Repair OwnerHost %s -> %s (SSH %s -> %s)",
			plan.OldHostID, plan.NewHostID, plan.OldFingerprint, plan.NewFingerprint,
		)
		consequences := []string{
			"replace SSH server fingerprint " + plan.OldFingerprint + " -> " + plan.NewFingerprint,
			"migrate connection, cache, project routing and transport metadata atomically",
			"leave the owner host, yards, instances and projects unchanged",
		}
		if !assumeYes {
			confirmed, confirmErr := cli.confirmHostRename(ctx, summary, consequences)
			if confirmErr != nil {
				cli.errorf("confirm host repair: %v", confirmErr)
				return 1
			}
			if !confirmed {
				fmt.Fprintln(cli.options.Stdout, "OwnerHost repair cancelled.")
				return 1
			}
		}
		if applyErr := store.ApplyRepair(plan); applyErr != nil {
			cli.errorf("repair OwnerHost %q: %v", hostID, applyErr)
			return 1
		}
		fmt.Fprintf(cli.options.Stdout, "OwnerHost repaired: %s -> %s (%s)\n",
			plan.OldHostID, plan.NewHostID, plan.NewFingerprint)
		return 0
	}
	// Legacy remote aliases live in the separate remote-control store. Keep
	// host removal single-store and crash-recoverable: operators remove those
	// compatibility records explicitly before removing the canonical host.
	control := cli.remoteControl(loaded, 0)
	legacyRecords, listErr := control.List(ctx)
	if listErr != nil {
		cli.errorf("list OwnerHost %q legacy routes: %v", hostID, listErr)
		return 1
	}
	for _, record := range legacyRecords {
		if record.Spec.OwnerEndpoint != connection.Destination {
			continue
		}
		alias := record.Spec.LegacyAlias
		cli.errorf("OwnerHost %q still has legacy route %q; remove it with `yard remote remove %s` before host removal", hostID, alias, alias)
		return 1
	}
	trustedClient, clientErr := store.TrustedSSHClient(connection, "ssh", 3*time.Second)
	if clientErr != nil {
		cli.errorf("prepare OwnerHost %q removal refresh: %v", hostID, clientErr)
		return 1
	}
	fetchedAt := time.Now().UTC()
	if cli.options.Clock != nil {
		fetchedAt = cli.options.Clock.Now().UTC()
	}
	callContext, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	fresh, refreshErr := ownerinventory.RefreshConnection(
		callContext, trustedClient, store, connection, fetchedAt,
	)
	if refreshErr != nil {
		cli.errorf("refresh OwnerHost %q before removal: %v", hostID, refreshErr)
		return 1
	}
	if fresh.HostID != hostID {
		cli.errorf("OwnerHost identity changed %q -> %q during removal preflight; re-run removal with the new HostID", hostID, fresh.HostID)
		return 1
	}
	removalPlan, prepareErr := store.PrepareRemoval(connection, ownerinventory.Snapshot{
		FetchedAt: fetchedAt, Inventory: fresh,
	})
	if prepareErr != nil {
		cli.errorf("prepare OwnerHost %q removal: %v", hostID, prepareErr)
		return 1
	}
	verb := "Remove controller registration for OwnerHost " + hostID
	consequences := []string{
		"remove controller-owned connection, trust routes and cached inventory",
		"leave the owner host, yards, instances and projects unchanged",
		"refuse removal while cached owner inventory contains project references",
	}
	if !assumeYes {
		confirmed, confirmErr := cli.confirmHostRename(ctx, verb, consequences)
		if confirmErr != nil {
			cli.errorf("confirm host %s: %v", action, confirmErr)
			return 1
		}
		if !confirmed {
			fmt.Fprintf(cli.options.Stdout, "OwnerHost %s cancelled.\n", action)
			return 1
		}
	}
	if _, err := store.ApplyRemoval(removalPlan); err != nil {
		cli.errorf("remove OwnerHost %q: %v", hostID, err)
		return 1
	}
	fmt.Fprintf(cli.options.Stdout, "OwnerHost registration removed: %s\n", hostID)
	return 0
}

func (cli *CLI) confirmHostRename(
	ctx context.Context, summary string, consequences []string,
) (bool, error) {
	if cli.options.Prompt != nil {
		return cli.options.Prompt.Confirm(ctx, domain.ConfirmationRequest{
			Summary: summary, Consequences: consequences, Default: domain.ConfirmationDefaultYes,
		})
	}
	for _, line := range append([]string{"", summary, "This will:"}, consequences...) {
		if line == "" {
			fmt.Fprintln(cli.options.Stderr)
		} else if line == summary || line == "This will:" {
			fmt.Fprintln(cli.options.Stderr, line)
		} else {
			fmt.Fprintf(cli.options.Stderr, "  - %s\n", line)
		}
	}
	fmt.Fprint(cli.options.Stderr, "\nProceed? [Y/n] ")
	answer, err := bufio.NewReader(cli.options.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer == "y" || answer == "yes" {
		return true, nil
	}
	if answer == "" && err == nil && cli.operatorTerminal() {
		return true, nil
	}
	return false, nil
}
