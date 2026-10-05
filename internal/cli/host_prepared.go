package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
)

// hostOperation retains the native transaction plan. Controller registrations
// always stay on this controller, including when the selected yard is remote.
type hostOperation struct {
	action       domain.ActionID
	store        ownerinventory.Connections
	registration *ownerinventory.RegistrationPlan
	repair       *ownerinventory.RepairPlan
	removal      *ownerinventory.RemovalPlan
	rename       *configsync.HostIDRenamePlan
	client       ownerinventory.Client
	connection   ownerinventory.Connection
	snapshot     ownerinventory.Snapshot
	consequences []string
	step         domain.OperationStep
	result       string
}

func (operation *hostOperation) stateDigest() string {
	switch {
	case operation.registration != nil:
		return operation.registration.StateDigest()
	case operation.repair != nil:
		return operation.repair.StateDigest()
	case operation.removal != nil:
		return operation.removal.StateDigest()
	default:
		return operation.rename.StateDigest()
	}
}

func (cli *CLI) hostSnapshot(inventory domain.OwnerInventory) ownerinventory.Snapshot {
	fetchedAt := time.Now().UTC()
	if cli.options.Clock != nil {
		fetchedAt = cli.options.Clock.Now().UTC()
	}
	return ownerinventory.Snapshot{FetchedAt: fetchedAt, Inventory: inventory}
}

func (cli *CLI) prepareHostOperation(ctx context.Context, loaded config.Loaded, arguments []string) (*hostOperation, error) {
	if len(arguments) != 2 {
		return nil, fmt.Errorf("%w: host expects add <owner-endpoint>, list, rename, remove or repair", errHostUsage)
	}
	verb, target := arguments[0], arguments[1]
	operation := &hostOperation{action: domain.ActionID("host." + verb),
		store: ownerinventory.Connections{Root: filepath.Join(loaded.Context.Paths.DataHome, "owner-inventory")}}
	operation.step = domain.OperationStep{ID: "host." + verb, Decision: domain.StepApply}
	if verb == "rename" {
		plan, err := configsync.PrepareHostIDRename(loaded.Context.Paths.ConfigHome, target)
		if err != nil {
			return nil, fmt.Errorf("prepare owner HostID rename: %w", err)
		}
		operation.rename = &plan
		operation.consequences = []string{
			fmt.Sprintf("rename owner HostID %s -> %s", plan.OldHostID, plan.NewHostID),
			"replace the persisted owner HostID",
			"migrate machine-local configuration references atomically",
			"leave yard, Incus, storage and SSH runtime resource names unchanged",
		}
		operation.step.Target = "owner identity " + plan.OldHostID
		operation.step.Observed, operation.step.Desired = plan.OldHostID, plan.NewHostID
		operation.step.Preconditions = []string{"owner identity and configuration manifest match the captured baseline"}
		operation.step.Verify = "read the persisted owner HostID"
		operation.result = fmt.Sprintf("Owner HostID renamed: %s -> %s", plan.OldHostID, plan.NewHostID)
		return operation, nil
	}
	if verb != "add" && verb != "repair" && verb != "remove" {
		return nil, fmt.Errorf("%w: unknown host subcommand %q", errHostUsage, verb)
	}
	endpoint := target
	if verb != "add" {
		connections, err := operation.store.List()
		if err != nil {
			return nil, fmt.Errorf("read owner-host registrations: %w", err)
		}
		found := false
		for _, candidate := range connections {
			if candidate.HostID == target {
				operation.connection, found = candidate, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("OwnerHost %q is not registered", target)
		}
		endpoint = operation.connection.Destination
	}
	if verb == "remove" {
		if err := cli.guardHostLegacyRoutes(ctx, loaded, operation.connection); err != nil {
			return nil, err
		}
		client, err := operation.store.TrustedSSHClient(operation.connection, "ssh", 3*time.Second)
		if err != nil {
			return nil, fmt.Errorf("prepare OwnerHost %q removal refresh: %w", target, err)
		}
		operation.client = client
		callContext, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		fetched, err := client.FetchForConnection(callContext, operation.connection)
		if err != nil {
			return nil, fmt.Errorf("refresh OwnerHost %q before removal: %w", target, err)
		}
		if fetched.Rename != nil {
			return nil, fmt.Errorf("OwnerHost identity changed %q -> %q during removal preflight; repair the registration before removal", target, fetched.Inventory.HostID)
		}
		operation.snapshot = cli.hostSnapshot(fetched.Inventory)
		plan, err := operation.store.PrepareRemoval(operation.connection, operation.snapshot)
		if err != nil {
			return nil, fmt.Errorf("prepare OwnerHost %q removal: %w", target, err)
		}
		operation.removal = &plan
		operation.consequences = []string{
			"remove controller registration for OwnerHost " + target,
			"remove controller-owned connection, trust routes and cached inventory",
			"leave the owner host, yards, instances and projects unchanged",
			"refuse removal while authoritative owner inventory contains project references",
		}
		operation.step.Target = "controller registration " + target
		operation.step.Observed, operation.step.Desired = "registered", "absent"
		operation.step.Preconditions = []string{"registered connection matches the captured baseline", "no legacy routes or controller project references", "fresh trusted owner inventory has no project references after acquiring the host mutation lock"}
		operation.step.Verify = "read registrations and confirm this HostID is absent"
		operation.result = "OwnerHost registration removed: " + target
		return operation, nil
	}
	if !domain.SafeSSHTarget(endpoint) {
		return nil, fmt.Errorf("%w: invalid owner endpoint %q", errHostUsage, endpoint)
	}
	trust, err := ownerinventory.AssessSSHHostKey(ctx, operation.store.Root, "ssh", endpoint, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("assess OwnerHost SSH key: %w", err)
	}
	client, err := ownerinventory.CandidateSSHClient(operation.store.Root, endpoint, trust, "ssh", 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("prepare OwnerHost candidate transport: %w", err)
	}
	callContext, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	inventory, err := client.Fetch(callContext, "")
	if err != nil {
		return nil, fmt.Errorf("read authoritative owner inventory: %w", err)
	}
	operation.snapshot = cli.hostSnapshot(inventory)
	operation.step.Verify = "read the registered connection and cached authoritative inventory"
	if verb == "add" {
		plan, err := operation.store.PrepareRegistration(ownerinventory.Connection{
			HostID: inventory.HostID, Destination: endpoint, Trust: &trust,
		}, operation.snapshot)
		if err != nil {
			return nil, fmt.Errorf("prepare OwnerHost registration: %w", err)
		}
		operation.registration = &plan
		yardNames := make([]string, 0, len(inventory.Yards))
		for _, yard := range inventory.Yards {
			yardNames = append(yardNames, yard.Name)
		}
		sort.Strings(yardNames)
		operation.consequences = []string{
			fmt.Sprintf("register OwnerHost %s at %s (SSH %s)", inventory.HostID, endpoint, trust.Fingerprint),
			"store one controller connection keyed by the authoritative HostID",
			"pin SSH server key " + trust.Fingerprint + " for strict owner refresh",
			fmt.Sprintf("discover authoritative yards [%s] without controller yard aliases", strings.Join(yardNames, ", ")),
			"cache owner inventory; leave the owner host and all runtime resources unchanged",
		}
		operation.step.Target = "controller registration " + plan.HostID
		operation.step.Observed = "no managed registration"
		operation.step.Desired = fmt.Sprintf("HostID %s at %s with SSH %s and %d authoritative yards", plan.HostID, endpoint, trust.Fingerprint, len(inventory.Yards))
		operation.step.Preconditions = []string{"captured registration baseline has no conflicting HostID, endpoint, cache or routing state"}
		operation.result = fmt.Sprintf("OwnerHost registered: %s (%d yards)", inventory.HostID, len(inventory.Yards))
		return operation, nil
	}
	plan, err := operation.store.PrepareRepair(target, trust, operation.snapshot)
	if err != nil {
		return nil, fmt.Errorf("prepare OwnerHost %q repair: %w", target, err)
	}
	operation.repair = &plan
	operation.consequences = []string{
		fmt.Sprintf("repair OwnerHost %s -> %s (SSH %s -> %s)", plan.OldHostID, plan.NewHostID, plan.OldFingerprint, plan.NewFingerprint),
		"replace SSH server fingerprint " + plan.OldFingerprint + " -> " + plan.NewFingerprint,
		"migrate connection, cache, project routing and transport metadata atomically",
		"leave the owner host, yards, instances and projects unchanged",
	}
	operation.step.Target = "controller registration " + target
	operation.step.Observed = fmt.Sprintf("HostID %s with SSH %s", plan.OldHostID, plan.OldFingerprint)
	operation.step.Desired = fmt.Sprintf("HostID %s with SSH %s", plan.NewHostID, plan.NewFingerprint)
	operation.step.Preconditions = []string{"captured controller connection is unchanged", "new HostID has no conflicting registration, cache or routing state"}
	operation.result = fmt.Sprintf("OwnerHost repaired: %s -> %s (%s)", plan.OldHostID, plan.NewHostID, plan.NewFingerprint)
	return operation, nil
}

func (cli *CLI) guardHostLegacyRoutes(ctx context.Context, loaded config.Loaded, connection ownerinventory.Connection) error {
	records, err := cli.remoteControl(loaded, 0).List(ctx)
	if err != nil {
		return fmt.Errorf("list OwnerHost %q legacy routes: %w", connection.HostID, err)
	}
	for _, record := range records {
		if record.Spec.OwnerEndpoint == connection.Destination {
			alias := record.Spec.LegacyAlias
			return fmt.Errorf("OwnerHost %q still has legacy route %q; remove it with `yard remote remove %s` before host removal", connection.HostID, alias, alias)
		}
	}
	return nil
}

func (prepared *preparedCommand) prepareHost(ctx context.Context, _ *initBootstrap) error {
	arguments := hostArguments(prepared.Arguments)
	if len(arguments) == 0 || commandHelpRequested(arguments) {
		prepared.displayOnly = prepared.CLI.hostUsage
		return nil
	}
	if len(arguments) == 1 && arguments[0] == "list" {
		connections, err := (ownerinventory.Connections{Root: filepath.Join(prepared.Loaded.Context.Paths.DataHome, "owner-inventory")}).List()
		if err != nil {
			return err
		}
		prepared.displayOnly = func() {
			fmt.Fprintf(prepared.CLI.options.Stdout, "%-24s %s\n", "HOST ID", "OWNER ENDPOINT")
			for _, connection := range connections {
				fmt.Fprintf(prepared.CLI.options.Stdout, "%-24s %s\n", connection.HostID, connection.Destination)
			}
		}
		return nil
	}
	operation, err := prepared.CLI.prepareHostOperation(ctx, prepared.Loaded, arguments)
	if err != nil {
		return err
	}
	prepared.exactState = operation.stateDigest()
	prepared.policy.RemotePolicy = domain.RemoteOnController
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return operation.action, domain.ActionDelta{Changed: true, Consequences: operation.consequences}, nil
	}
	prepared.stepsComplete = true
	prepared.steps = func() []domain.OperationStep {
		step := operation.step
		step.Consequence = operation.consequences[0]
		return []domain.OperationStep{step}
	}
	prepared.execute = func(ctx context.Context, _ *application.Orchestrator, output io.Writer) (domain.AdapterResult, error) {
		if err := operation.apply(ctx, prepared.CLI, prepared.Loaded); err != nil {
			return domain.AdapterResult{}, err
		}
		fmt.Fprintln(output, operation.result)
		return domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: prepared.Plan.OperationID, Status: "ok"}, nil
	}
	return nil
}

func (operation *hostOperation) apply(ctx context.Context, cli *CLI, loaded config.Loaded) error {
	switch {
	case operation.registration != nil:
		if err := operation.store.ApplyRegistration(*operation.registration); err != nil {
			return err
		}
	case operation.repair != nil:
		if err := operation.store.ApplyRepair(*operation.repair); err != nil {
			return err
		}
	case operation.removal != nil:
		// Legacy routes are a separate store; validate their guard again after consent.
		if err := cli.guardHostLegacyRoutes(ctx, loaded, operation.connection); err != nil {
			return err
		}
		_, err := operation.store.ApplyRemoval(ctx, *operation.removal, func(ctx context.Context, current ownerinventory.Connection) (ownerinventory.Snapshot, error) {
			fetched, err := operation.client.FetchForConnection(ctx, current)
			if err != nil {
				return ownerinventory.Snapshot{}, err
			}
			if fetched.Rename != nil {
				return ownerinventory.Snapshot{}, errors.New("owner identity changed after confirmation; repair and re-run host removal")
			}
			return cli.hostSnapshot(fetched.Inventory), nil
		})
		if err != nil {
			return err
		}
	default:
		if err := configsync.ApplyHostIDRename(*operation.rename); err != nil {
			if errors.Is(err, configsync.ErrPlanStale) {
				return fmt.Errorf("%w: %w", domain.ErrPlanStale, err)
			}
			return err
		}
		current, pending, err := configsync.ResolveHostID(operation.rename.ConfigHome, nil)
		if err != nil {
			return err
		}
		if pending || current != operation.rename.NewHostID {
			return errors.New("owner HostID rename verification failed")
		}
		return nil
	}
	return operation.verify()
}

func (operation *hostOperation) verify() error {
	connections, err := operation.store.List()
	if err != nil {
		return err
	}
	hostID, fingerprint := "", ""
	switch {
	case operation.registration != nil:
		hostID, fingerprint = operation.registration.HostID, operation.registration.Fingerprint
	case operation.repair != nil:
		hostID, fingerprint = operation.repair.NewHostID, operation.repair.NewFingerprint
	default:
		for _, connection := range connections {
			if connection.HostID == operation.removal.HostID {
				return errors.New("owner registration removal verification failed")
			}
		}
		return nil
	}
	for _, connection := range connections {
		if connection.HostID == hostID && connection.Trust != nil && connection.Trust.Fingerprint == fingerprint {
			snapshot, err := (ownerinventory.Cache{Root: operation.store.Root}).Read(hostID)
			if err != nil {
				return err
			}
			if operationStateDigest(snapshot) != operationStateDigest(operation.snapshot) {
				return errors.New("owner registration inventory verification failed")
			}
			return nil
		}
	}
	return errors.New("owner registration connection verification failed")
}
