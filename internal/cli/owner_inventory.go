package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/transport"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/state"
)

type cliOwnerSource struct {
	cli      *CLI
	loaded   config.Loaded
	readOnly bool
}

func canonicalYardIdentity(loaded config.Loaded) (string, error) {
	yard := loaded.Context.YardName
	if loaded.Context.AccessKind == domain.AccessLocal {
		hostID, _, err := configsync.ResolveHostID(
			loaded.Context.Paths.ConfigHome, loaded.Environment,
		)
		if err != nil {
			return "", err
		}
		return hostID + "/" + yard, nil
	}
	if loaded.Context.AccessKind != domain.AccessRemote {
		return "", errors.New("canonical yard identity requires a local or remote yard")
	}
	if loaded.Context.OwnerYardName != "" {
		yard = loaded.Context.OwnerYardName
	} else {
		yard = "default"
	}
	connections, err := (ownerinventory.Connections{
		Root: filepath.Join(loaded.Context.Paths.DataHome, "owner-inventory"),
	}).List()
	if err != nil {
		return "", err
	}
	for _, connection := range connections {
		if connection.Destination == loaded.Context.OwnerEndpoint {
			return connection.HostID + "/" + yard, nil
		}
	}
	return "", fmt.Errorf("canonical owner is not registered for remote yard %q", yard)
}

func (source cliOwnerSource) HostID(context.Context) (string, error) {
	hostID, pending, err := configsync.ResolveHostID(
		source.loaded.Context.Paths.ConfigHome, source.loaded.Environment,
	)
	if err != nil {
		return "", err
	}
	_ = pending // Before first setup this is the deterministic bootstrap candidate.
	return hostID, nil
}

func (source cliOwnerSource) Yards(context.Context) ([]domain.Context, error) {
	names, err := config.YardNames(config.RegistryDirectories(
		source.loaded.Context.Paths.ConfigDir, source.loaded.Context.Paths.ConfigHome,
	)...)
	if err != nil {
		return nil, err
	}
	yards := make([]domain.Context, 0, len(names))
	for _, name := range names {
		loaded, err := source.cli.loadInventoryLoaded(name, source.loaded)
		if err != nil {
			if config.IsRetiredYardTemplate(err) {
				fmt.Fprintf(source.cli.options.Stderr,
					"Warning: skipping yard %q until its registration is migrated:\n%s\n", name, err)
				continue
			}
			return nil, fmt.Errorf("load yard %q: %w", name, err)
		}
		if loaded.Context.AccessKind == domain.AccessLocal {
			yards = append(yards, loaded.Context)
		}
	}
	return yards, nil
}

func (source cliOwnerSource) Projects(ctx context.Context, yard domain.Context) ([]domain.ProjectRecord, error) {
	if source.readOnly {
		store, err := openProjectStoreReadOnly(yard.Paths.StateDir)
		if err != nil {
			return nil, err
		}
		return store.List(ctx)
	}
	store, err := openProjectStore(ctx, yard.Paths.StateDir)
	if err != nil {
		return nil, err
	}
	return store.List(ctx)
}

func (source cliOwnerSource) Runtime(
	ctx context.Context, yard domain.Context,
) (string, domain.ResolvedYardImage, error) {
	incusPort, _ := source.cli.statusPorts()
	instance, err := incusPort.Instance(ctx, yard.IncusProject, yard.YardInstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) {
		return "NOT_CREATED", "", nil
	}
	if err != nil {
		return "UNKNOWN", "", nil
	}
	// Incus records the immutable fingerprint of the image actually used to
	// create the instance. Never substitute the desired input when it is absent.
	return instance.Status, domain.ResolvedYardImage(instance.Config["volatile.base_image"]), nil
}

func (cli *CLI) ownerInventory(ctx context.Context, loaded config.Loaded) (domain.OwnerInventory, error) {
	return (application.OwnerInventoryBuilder{
		Source: cliOwnerSource{cli: cli, loaded: loaded}, Clock: cli.options.Clock,
	}).Read(ctx)
}

func (cli *CLI) ownerInventoryReadOnly(ctx context.Context, loaded config.Loaded) (domain.OwnerInventory, error) {
	return (application.OwnerInventoryBuilder{
		Source: cliOwnerSource{cli: cli, loaded: loaded, readOnly: true}, Clock: cli.options.Clock,
	}).Read(ctx)
}

func (cli *CLI) remoteYardStatus(ctx context.Context, yard domain.Context) (domain.YardStatus, error) {
	ownerYard := yard.OwnerYardName
	if ownerYard == "" {
		ownerYard = "default"
	}
	process, err := transport.SSHYard("ssh", yard.OwnerEndpoint, ownerYard, 3*time.Second)
	if err != nil {
		return domain.YardStatus{}, err
	}
	callContext, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	return (ownerinventory.Client{Transport: process}).YardStatus(callContext)
}

func (cli *CLI) remoteOwnerYardStatus(
	ctx context.Context, loaded config.Loaded, hostID, yardName string,
) (domain.YardStatus, error) {
	connections, err := (ownerinventory.Connections{
		Root: loaded.Context.Paths.DataHome + "/owner-inventory",
	}).ListReadOnly()
	if err != nil {
		return domain.YardStatus{}, err
	}
	destination := ""
	for _, connection := range connections {
		if connection.HostID == hostID {
			destination = connection.Destination
			break
		}
	}
	if destination == "" {
		return domain.YardStatus{}, fmt.Errorf("OwnerHost %q has no registered connection", hostID)
	}
	process, err := transport.SSHYard("ssh", destination, yardName, 3*time.Second)
	if err != nil {
		return domain.YardStatus{}, err
	}
	callContext, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	return (ownerinventory.Client{Transport: process}).YardStatus(callContext)
}

func (cli *CLI) invalidateOwnerInventory(loaded config.Loaded) error {
	if loaded.Context.AccessKind != domain.AccessRemote {
		return nil
	}
	root := loaded.Context.Paths.DataHome + "/owner-inventory"
	connections, err := (ownerinventory.Connections{Root: root}).List()
	if err != nil {
		return err
	}
	for _, connection := range connections {
		if connection.Destination == loaded.Context.OwnerEndpoint {
			return (ownerinventory.Cache{Root: root}).Invalidate(connection.HostID)
		}
	}
	return nil
}

func (cli *CLI) cleanupObsoleteRemoteProjectState(
	ctx context.Context, loaded config.Loaded, projectID string,
) error {
	if loaded.Context.AccessKind != domain.AccessRemote {
		return nil
	}
	connections, err := (ownerinventory.Connections{
		Root: loaded.Context.Paths.DataHome + "/owner-inventory",
	}).List()
	if err != nil {
		return err
	}
	names := make(map[string]struct{})
	for _, connection := range connections {
		if connection.Destination == loaded.Context.OwnerEndpoint {
			for _, name := range connection.LegacyNames {
				names[name] = struct{}{}
			}
		}
	}
	for name := range names {
		legacy, err := cli.loadInventoryLoaded(name, loaded)
		if err != nil {
			continue
		}
		store, err := openProjectStore(ctx, legacy.Context.Paths.StateDir)
		if err != nil {
			return err
		}
		if err := store.Delete(ctx, projectID); err != nil && !errors.Is(err, state.ErrNotFound) {
			return err
		}
	}
	return nil
}

type ownerInventoryResult struct {
	inventory domain.OwnerInventory
	fetchedAt time.Time
	stale     bool
	err       error
}

const projectListOwnerWidth = 20

// compactProjectListOwner bounds an already validated ASCII HostID for the
// human project table. Identity-bearing outputs keep the original value.
func compactProjectListOwner(value string) string {
	if len(value) <= projectListOwnerWidth {
		return value
	}
	return value[:projectListOwnerWidth-len("...")] + "..."
}

func printProjectListRow(writer io.Writer, name, mode, target, owner, yard string) {
	fmt.Fprintf(writer, "%-24s %-6s %-10s %-20s %s\n",
		name, mode, target, compactProjectListOwner(owner), yard)
}

func (cli *CLI) allOwnerInventories(
	ctx context.Context, loaded config.Loaded, force bool,
) []ownerInventoryResult {
	local, err := cli.ownerInventory(ctx, loaded)
	results := []ownerInventoryResult{{inventory: local, fetchedAt: local.ObservedAt, err: err}}
	root := loaded.Context.Paths.DataHome + "/owner-inventory"
	connectionStore := ownerinventory.Connections{Root: root}
	connections, err := connectionStore.List()
	if err != nil {
		return append(results, ownerInventoryResult{err: err})
	}
	legacy, err := cli.remoteControl(loaded, 0).List(ctx)
	if err != nil {
		return append(results, ownerInventoryResult{err: err})
	}
	for index := range connections {
		changed, mergeErr := mergeLegacyRoutes(&connections[index], legacy)
		if mergeErr != nil {
			return append(results, ownerInventoryResult{err: mergeErr})
		}
		if changed {
			if writeErr := connectionStore.Write(connections[index]); writeErr != nil {
				return append(results, ownerInventoryResult{err: writeErr})
			}
		}
	}
	knownDestinations := make(map[string]struct{}, len(connections))
	for _, connection := range connections {
		knownDestinations[connection.Destination] = struct{}{}
	}
	legacyByDestination := make(map[string][]string)
	for _, record := range legacy {
		if _, known := knownDestinations[record.Spec.OwnerEndpoint]; known {
			continue
		}
		legacyByDestination[record.Spec.OwnerEndpoint] = append(
			legacyByDestination[record.Spec.OwnerEndpoint], record.Spec.LegacyAlias,
		)
	}
	type remoteRequest struct {
		connection ownerinventory.Connection
		discover   bool
	}
	requests := make([]remoteRequest, 0, len(connections)+len(legacyByDestination))
	for _, connection := range connections {
		requests = append(requests, remoteRequest{
			connection: connection, discover: connection.Trust == nil,
		})
	}
	for destination, aliases := range legacyByDestination {
		connection := ownerinventory.Connection{
			Destination: destination, LegacyNames: aliases,
		}
		if _, mergeErr := mergeLegacyRoutes(&connection, legacy); mergeErr != nil {
			return append(results, ownerInventoryResult{err: mergeErr})
		}
		requests = append(requests, remoteRequest{
			connection: connection, discover: true,
		})
	}
	sort.Slice(requests, func(i, j int) bool {
		return requests[i].connection.Destination < requests[j].connection.Destination
	})
	remoteResults := make([]ownerInventoryResult, len(requests))
	var wait sync.WaitGroup
	common, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	for index, request := range requests {
		wait.Add(1)
		go func(index int, request remoteRequest) {
			defer wait.Done()
			if request.discover {
				// Legacy discovery remains read-only. In particular, an inventory
				// listing must never turn an unconfirmed TOFU observation into
				// controller-managed SSH trust; explicit `yard host add` owns that
				// confirmation and transaction.
				process, clientErr := transport.SSH(
					"ssh", request.connection.Destination, 3*time.Second,
				)
				if clientErr != nil {
					remoteResults[index].err = clientErr
					return
				}
				client := ownerinventory.Client{Transport: process}
				read := (ownerinventory.LegacyService{
					Store: connectionStore, Clock: cli.options.Clock,
					Fetch: func(fetchCtx context.Context, expected string) (domain.OwnerInventory, error) {
						return client.Fetch(fetchCtx, expected)
					},
				}).Read(common, request.connection, force)
				if read.Inventory.HostID == "" {
					read.Inventory.HostID = request.connection.HostID
				}
				remoteResults[index] = ownerInventoryResult{
					inventory: read.Inventory, fetchedAt: read.FetchedAt,
					stale: read.Stale, err: read.Err,
				}
				return
			}
			client, clientErr := connectionStore.TrustedSSHClient(
				request.connection, "ssh", 3*time.Second,
			)
			if clientErr != nil {
				remoteResults[index].err = clientErr
				return
			}
			service := ownerinventory.Service{
				Cache: ownerinventory.Cache{Root: root}, Store: &connectionStore, Clock: cli.options.Clock,
				Fetch: func(fetchCtx context.Context, expected string) (domain.OwnerInventory, error) {
					fetchedAt := time.Now().UTC()
					if cli.options.Clock != nil {
						fetchedAt = cli.options.Clock.Now().UTC()
					}
					return ownerinventory.RefreshConnection(
						fetchCtx, client, connectionStore, request.connection, fetchedAt,
					)
				},
			}
			read := service.Read(common, request.connection.HostID, force)
			if read.Inventory.HostID == "" {
				read.Inventory.HostID = request.connection.HostID
			}
			remoteResults[index] = ownerInventoryResult{
				inventory: read.Inventory, fetchedAt: read.FetchedAt, stale: read.Stale, err: read.Err,
			}
		}(index, request)
	}
	wait.Wait()
	for index, request := range requests {
		if !request.discover || remoteResults[index].err != nil || remoteResults[index].inventory.HostID == "" {
			continue
		}
		request.connection.HostID = remoteResults[index].inventory.HostID
		cli.discoveredOwners[request.connection.HostID] = request.connection
	}
	return append(results, remoteResults...)
}

// allOwnerInventoriesReadOnly resolves existing owner identities without
// persisting cache refreshes or legacy-route normalization. A normal read uses
// a fresh cache; force performs a live fetch while still leaving the cache
// untouched.
func (cli *CLI) allOwnerInventoriesReadOnly(
	ctx context.Context, loaded config.Loaded, force bool,
) []ownerInventoryResult {
	local, err := cli.ownerInventoryReadOnly(ctx, loaded)
	results := []ownerInventoryResult{{inventory: local, fetchedAt: local.ObservedAt, err: err}}
	root := loaded.Context.Paths.DataHome + "/owner-inventory"
	connections, err := (ownerinventory.Connections{Root: root}).ListReadOnly()
	if err != nil {
		return append(results, ownerInventoryResult{err: err})
	}
	legacy, err := cli.remoteControl(loaded, 0).List(ctx)
	if err != nil {
		return append(results, ownerInventoryResult{err: err})
	}
	knownDestinations := make(map[string]struct{}, len(connections))
	for index := range connections {
		if _, mergeErr := mergeLegacyRoutes(&connections[index], legacy); mergeErr != nil {
			return append(results, ownerInventoryResult{err: mergeErr})
		}
		knownDestinations[connections[index].Destination] = struct{}{}
	}
	for _, record := range legacy {
		if _, known := knownDestinations[record.Spec.OwnerEndpoint]; known {
			continue
		}
		connection := ownerinventory.Connection{
			Destination: record.Spec.OwnerEndpoint,
			LegacyNames: []string{record.Spec.LegacyAlias},
		}
		if _, mergeErr := mergeLegacyRoutes(&connection, legacy); mergeErr != nil {
			return append(results, ownerInventoryResult{err: mergeErr})
		}
		connections = append(connections, connection)
		knownDestinations[record.Spec.OwnerEndpoint] = struct{}{}
	}
	sort.Slice(connections, func(i, j int) bool {
		return connections[i].Destination < connections[j].Destination
	})
	cache := ownerinventory.Cache{Root: root}
	remote := make([]ownerInventoryResult, len(connections))
	common, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var wait sync.WaitGroup
	for index, connection := range connections {
		wait.Add(1)
		go func(index int, connection ownerinventory.Connection) {
			defer wait.Done()
			if connection.HostID == "" {
				process, transportErr := transport.SSH(
					"ssh", connection.Destination, 3*time.Second,
				)
				if transportErr != nil {
					remote[index].err = transportErr
					return
				}
				inventory, fetchErr := (ownerinventory.Client{Transport: process}).Fetch(common, "")
				if fetchErr != nil {
					remote[index].err = fetchErr
					return
				}
				remote[index] = ownerInventoryResult{inventory: inventory, fetchedAt: nowUTC(cli)}
				return
			}
			snapshot, cacheErr := cache.Read(connection.HostID)
			now := nowUTC(cli)
			if cacheErr == nil && !force && now.Sub(snapshot.FetchedAt) <= ownerinventory.Freshness {
				remote[index] = ownerInventoryResult{
					inventory: snapshot.Inventory, fetchedAt: snapshot.FetchedAt,
				}
				return
			}
			process, transportErr := transport.SSH("ssh", connection.Destination, 3*time.Second)
			if transportErr == nil {
				inventory, fetchErr := (ownerinventory.Client{Transport: process}).Fetch(common, connection.HostID)
				if fetchErr == nil {
					remote[index] = ownerInventoryResult{inventory: inventory, fetchedAt: now}
					return
				}
				transportErr = fetchErr
			}
			if cacheErr == nil {
				remote[index] = ownerInventoryResult{
					inventory: snapshot.Inventory, fetchedAt: snapshot.FetchedAt, stale: true, err: transportErr,
				}
				return
			}
			if errors.Is(cacheErr, os.ErrNotExist) {
				cacheErr = nil
			}
			remote[index].err = errors.Join(transportErr, cacheErr)
		}(index, connection)
	}
	wait.Wait()
	for index, connection := range connections {
		if connection.HostID != "" || remote[index].err != nil ||
			remote[index].inventory.HostID == "" {
			continue
		}
		connection.HostID = remote[index].inventory.HostID
		cli.discoveredOwners[connection.HostID] = connection
	}
	return append(results, remote...)
}

func nowUTC(cli *CLI) time.Time {
	if cli != nil && cli.options.Clock != nil {
		return cli.options.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func mergeLegacyRoutes(
	connection *ownerinventory.Connection, records []domain.RemoteRecord,
) (bool, error) {
	changed := false
	if connection.Yards == nil {
		connection.Yards = make(map[string]ownerinventory.YardRoute)
	}
	aliases := make(map[string]struct{}, len(connection.LegacyNames))
	for _, name := range connection.LegacyNames {
		aliases[name] = struct{}{}
	}
	for _, record := range records {
		if record.Spec.OwnerEndpoint != connection.Destination {
			continue
		}
		yard := record.Spec.OwnerYardName
		if yard == "" {
			yard = "default"
		}
		route := ownerinventory.YardRoute{SSHHost: "yard-" + record.Spec.LegacyAlias}
		if existing, exists := connection.Yards[yard]; exists && existing != route {
			return false, fmt.Errorf(
				"OwnerHost %q has conflicting transport routes for yard %q",
				connection.HostID, yard,
			)
		}
		if _, exists := connection.Yards[yard]; !exists {
			connection.Yards[yard] = route
			changed = true
		}
		if _, exists := aliases[record.Spec.LegacyAlias]; !exists {
			connection.LegacyNames = append(connection.LegacyNames, record.Spec.LegacyAlias)
			aliases[record.Spec.LegacyAlias] = struct{}{}
			changed = true
		}
	}
	return changed, nil
}

func selectOwnerYards(
	results []ownerInventoryResult, selector string,
) ([]ownerInventoryResult, string, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" || selector == "default" {
		return results, "", nil
	}
	hostID, yardName, canonical := strings.Cut(selector, "/")
	if canonical && (hostID == "" || yardName == "" || strings.Contains(yardName, "/")) {
		return nil, "", fmt.Errorf("invalid yard selector %q; use <HostID>/<yard>", selector)
	}
	type match struct {
		result ownerInventoryResult
		yard   domain.OwnerYard
	}
	var matches []match
	for _, result := range results {
		for _, yard := range result.inventory.Yards {
			if (canonical && result.inventory.HostID == hostID && yard.Name == yardName) ||
				(!canonical && yard.Name == selector) {
				copyResult := result
				copyResult.inventory.Yards = []domain.OwnerYard{yard}
				matches = append(matches, match{result: copyResult, yard: yard})
			}
		}
	}
	if len(matches) == 1 {
		return []ownerInventoryResult{matches[0].result}, "", nil
	}
	if len(matches) == 0 {
		return nil, "", fmt.Errorf("yard %q was not found in owner inventory", selector)
	}
	candidates := make([]string, 0, len(matches))
	for _, match := range matches {
		candidates = append(candidates, match.result.inventory.HostID+"/"+match.yard.Name)
	}
	sort.Strings(candidates)
	return nil, "", fmt.Errorf(
		"yard %q is ambiguous; use one of: %s", selector, strings.Join(candidates, ", "),
	)
}

func printOwnerCompletions(output io.Writer, results []ownerInventoryResult, kind string) {
	yardCounts := make(map[string]int)
	projectCounts := make(map[string]int)
	for _, result := range results {
		for _, yard := range result.inventory.Yards {
			yardCounts[yard.Name]++
			for _, project := range yard.Projects {
				projectCounts[domain.ProjectNameKey(project.Name)]++
			}
		}
	}
	values := make(map[string]struct{})
	for _, result := range results {
		for _, yard := range result.inventory.Yards {
			if kind == "yards" {
				values[result.inventory.HostID+"/"+yard.Name] = struct{}{}
				if yardCounts[yard.Name] == 1 {
					values[yard.Name] = struct{}{}
				}
				continue
			}
			for _, project := range yard.Projects {
				values[canonicalProjectSelector(
					project.Name, yard.Name, result.inventory.HostID,
				)] = struct{}{}
				if projectCounts[domain.ProjectNameKey(project.Name)] == 1 {
					values[project.Name] = struct{}{}
				}
			}
		}
	}
	sorted := make([]string, 0, len(values))
	for value := range values {
		sorted = append(sorted, value)
	}
	sort.Strings(sorted)
	for _, value := range sorted {
		fmt.Fprintln(output, value)
	}
}

func canonicalProjectSelector(project, yard, hostID string) string {
	if yard == "default" {
		return project + "/" + hostID
	}
	return project + "/" + yard + "/" + hostID
}

func projectSelectorMatches(selector, project, yard, hostID string, fold bool) bool {
	equal := func(left, right string) bool {
		if fold {
			return domain.ProjectNamesEqual(left, right)
		}
		return left == right
	}
	return equal(selector, project) ||
		equal(selector, canonicalProjectSelector(project, yard, hostID)) ||
		equal(selector, project+"/"+yard+"/"+hostID) ||
		equal(selector, yard+"/"+project) ||
		equal(selector, hostID+"/"+yard+"/"+project)
}

func (cli *CLI) resolveOwnerProject(
	ctx context.Context, loaded config.Loaded, selector string, explicit bool, force bool, readOnly bool,
) (state.Match, error) {
	if readOnly {
		results := cli.allOwnerInventoriesReadOnly(ctx, loaded, force)
		return cli.resolveOwnerProjectFromInventories(ctx, loaded, selector, explicit, readOnly, results)
	}
	results := cli.allOwnerInventories(ctx, loaded, force)
	return cli.resolveOwnerProjectFromInventories(ctx, loaded, selector, explicit, readOnly, results)
}

func (cli *CLI) resolveOwnerProjectFromInventories(
	ctx context.Context,
	loaded config.Loaded,
	selector string,
	explicit bool,
	readOnly bool,
	results []ownerInventoryResult,
) (state.Match, error) {
	for _, result := range results {
		if result.err != nil && len(result.inventory.Yards) == 0 {
			return state.Match{}, fmt.Errorf("refresh owner inventory: %w", result.err)
		}
	}
	scopeHost, scopeYard := "", ""
	if explicit {
		scopeYard = loaded.Context.YardName
		if loaded.Context.AccessKind == domain.AccessLocal {
			scopeHost = results[0].inventory.HostID
		} else {
			if loaded.Context.OwnerYardName != "" {
				scopeYard = loaded.Context.OwnerYardName
			} else {
				scopeYard = "default"
			}
			connectionStore := ownerinventory.Connections{
				Root: loaded.Context.Paths.DataHome + "/owner-inventory",
			}
			var connections []ownerinventory.Connection
			var err error
			if readOnly {
				connections, err = connectionStore.ListReadOnly()
			} else {
				connections, err = connectionStore.List()
			}
			if err != nil {
				return state.Match{}, err
			}
			for _, connection := range connections {
				if connection.Destination == loaded.Context.OwnerEndpoint {
					scopeHost = connection.HostID
					break
				}
			}
		}
	}
	if strings.Count(selector, "/") > 2 {
		return state.Match{}, fmt.Errorf(
			"invalid project selector %q; use <project>/<yard>/<HostID>", selector,
		)
	}
	type match struct {
		hostID  string
		yard    domain.OwnerYard
		project domain.OwnerProject
	}
	var exact, named []match
	for _, result := range results {
		for _, yard := range result.inventory.Yards {
			if scopeHost != "" && (result.inventory.HostID != scopeHost || yard.Name != scopeYard) {
				continue
			}
			for _, project := range yard.Projects {
				candidate := match{hostID: result.inventory.HostID, yard: yard, project: project}
				if projectSelectorMatches(
					selector, project.ProjectID, yard.Name, result.inventory.HostID, false,
				) {
					exact = append(exact, candidate)
				}
				if projectSelectorMatches(
					selector, project.Name, yard.Name, result.inventory.HostID, true,
				) {
					named = append(named, candidate)
				}
			}
		}
	}
	matches := named
	if len(exact) != 0 {
		matches = exact
	}
	if len(matches) == 0 {
		return state.Match{}, fmt.Errorf("project %q was not found", selector)
	}
	if len(matches) > 1 {
		candidates := make([]string, 0, len(matches))
		for _, match := range matches {
			candidates = append(candidates, canonicalProjectSelector(
				match.project.Name, match.yard.Name, match.hostID,
			))
		}
		sort.Strings(candidates)
		return state.Match{}, fmt.Errorf("%w: use one of: %s",
			state.ErrAmbiguous, strings.Join(candidates, ", "))
	}
	selected := matches[0]
	var contextName string
	var contextValue domain.Context
	var err error
	if readOnly {
		contextName, contextValue, err = cli.ownerYardRouteReadOnly(
			ctx, loaded, selected.hostID, selected.yard.Name,
		)
	} else {
		contextName, contextValue, err = cli.ownerYardRoute(
			ctx, loaded, selected.hostID, selected.yard.Name,
		)
	}
	if err != nil {
		return state.Match{}, err
	}
	var record domain.ProjectRecord
	if readOnly {
		store, storeErr := openProjectStoreReadOnly(contextValue.Paths.StateDir)
		if storeErr != nil {
			return state.Match{}, storeErr
		}
		record, err = store.Get(ctx, selected.project.ProjectID)
	} else {
		store, storeErr := openProjectStore(ctx, contextValue.Paths.StateDir)
		if storeErr != nil {
			return state.Match{}, storeErr
		}
		record, err = store.Get(ctx, selected.project.ProjectID)
	}
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		return state.Match{}, err
	}
	if errors.Is(err, state.ErrNotFound) {
		record = domain.ProjectRecord{
			Schema: 1, IdentityVersion: selected.project.IdentityVersion,
			ProjectID: selected.project.ProjectID, Name: selected.project.Name,
			SourceKey: selected.project.SourceKey,
			YardPath:  state.YardPath(selected.project.ProjectID),
			Mode:      domain.ProjectMode(selected.project.Mode), SSHHost: contextValue.SSHHost,
			Target: selected.project.Target, RegistrySource: "yard",
		}
	}
	return state.Match{Yard: contextName, Record: record}, nil
}

func (cli *CLI) ownerYardRoute(
	ctx context.Context, loaded config.Loaded, hostID, yardName string,
) (string, domain.Context, error) {
	return cli.ownerYardRouteWithMode(ctx, loaded, hostID, yardName, false)
}

func (cli *CLI) ownerYardRouteReadOnly(
	ctx context.Context, loaded config.Loaded, hostID, yardName string,
) (string, domain.Context, error) {
	return cli.ownerYardRouteWithMode(ctx, loaded, hostID, yardName, true)
}

func (cli *CLI) ownerYardRouteWithMode(
	ctx context.Context, loaded config.Loaded, hostID, yardName string, readOnly bool,
) (string, domain.Context, error) {
	localHostID, _, err := configsync.ResolveHostID(
		loaded.Context.Paths.ConfigHome, loaded.Environment,
	)
	if err != nil {
		return "", domain.Context{}, err
	}
	if hostID == localHostID {
		contextValue, err := cli.loadInventoryContext(yardName, loaded)
		return yardName, contextValue, err
	}
	connectionStore := ownerinventory.Connections{
		Root: loaded.Context.Paths.DataHome + "/owner-inventory",
	}
	var connections []ownerinventory.Connection
	if readOnly {
		connections, err = connectionStore.ListReadOnly()
	} else {
		connections, err = connectionStore.List()
	}
	if err != nil {
		return "", domain.Context{}, err
	}
	discovered, legacyDiscovery := cli.discoveredOwners[hostID]
	if legacyDiscovery {
		connections = append(connections, discovered)
	}
	destination := ""
	for _, connection := range connections {
		if connection.HostID == hostID {
			destination = connection.Destination
			break
		}
	}
	var route ownerinventory.YardRoute
	for _, connection := range connections {
		if connection.HostID == hostID {
			route = connection.Yards[yardName]
			break
		}
	}
	if route.SSHHost != "" {
		contextValue := loaded.Context
		contextValue.YardName = yardName
		contextValue.AccessKind = domain.AccessRemote
		contextValue.OwnerEndpoint = destination
		contextValue.OwnerYardName = yardName
		contextValue.SSHHost = route.SSHHost
		contextValue.Paths.StateDir = filepath.Join(contextValue.Paths.DataHome,
			"owner-inventory", "routing", hostID, yardName, "projects")
		if legacyDiscovery && len(discovered.LegacyNames) == 1 {
			contextValue.Paths.StateDir = filepath.Join(
				loaded.Context.Paths.ConfigHome, "yards", discovered.LegacyNames[0], "projects",
			)
		}
		environment := make(map[string]string, len(loaded.Environment))
		for key, value := range loaded.Environment {
			environment[key] = value
		}
		environment["YARD_NAME"] = yardName
		environment["ACCESS_KIND"] = string(domain.AccessRemote)
		environment["OWNER_ENDPOINT"] = destination
		environment["OWNER_YARD_NAME"] = yardName
		environment["SSH_HOST"] = route.SSHHost
		environment["SUBYARD_STATE_DIR"] = contextValue.Paths.StateDir
		routeKey := hostID + "/" + yardName
		cli.inventoryRoutes[routeKey] = config.Loaded{
			Context: contextValue, Environment: environment,
		}
		return routeKey, contextValue, nil
	}
	return "", domain.Context{}, fmt.Errorf(
		"yard %s/%s is known through owner inventory but has no data-plane route; "+
			"register it with yard remote add before a project mutation",
		hostID, yardName,
	)
}
