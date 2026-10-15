package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/adapters/sshagentruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/uninstall"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

type teardownExecution struct {
	uninstall.RecoveryPlan
	full, holdShared, keepData, resetConfig, completedReset bool
	configSnapshot                                          *teardownConfigSnapshot
	changed, physicalChanged, revokeAgent                   bool
	liveSnapshot                                            *teardownSnapshot
	activeYard                                              *uninstall.RecoveryYard
	RecoveryBinding                                         *uninstall.Tree
	Assessed, resumed                                       bool
	ManagedRoots, PreservedRoots, MutableRoots              []string
}

type fullTeardown = teardownExecution

func (execution *teardownExecution) snapshot() *teardownSnapshot {
	if execution.activeYard == nil {
		return nil
	}
	return execution.activeYard.Snapshot
}
func (execution *teardownExecution) networkRemoval() *yardnetwork.RemovalPlan {
	if execution.activeYard == nil {
		return nil
	}
	return execution.activeYard.Network
}
func (execution *teardownExecution) selectYard(loaded config.Loaded) {
	for index := range execution.Yards {
		if execution.Yards[index].Context.IncusProject == loaded.Context.IncusProject && execution.Yards[index].Context.YardInstanceName == loaded.Context.YardInstanceName {
			execution.activeYard = &execution.Yards[index]
			return
		}
	}
	execution.Yards = append(execution.Yards, uninstall.RecoveryYard{Context: loaded.Context, Environment: structuredCommandContext(loaded)})
	execution.activeYard = &execution.Yards[len(execution.Yards)-1]
}
func (cli *CLI) teardownLoaded(yard uninstall.RecoveryYard) config.Loaded {
	loaded := config.Loaded{Catalog: cli.catalog, Context: yard.Context, Environment: yard.Environment}
	loaded.Context.Paths.RepositoryRoot = cli.options.RepositoryRoot
	loaded.Context.Paths.ConfigDir = filepath.Join(cli.options.RepositoryRoot, "config")
	return loaded
}

// resetTeardownBaseline retains the same approved physical and artifact scope
// as teardown. Init must check it before its native reset leaf and verify it
// immediately afterwards, before recreating any installation state.
type resetTeardownBaseline struct {
	loaded    config.Loaded
	execution *teardownExecution
}

func (cli *CLI) prepareResetTeardownBaseline(ctx context.Context, loaded config.Loaded) (*resetTeardownBaseline, error) {
	execution := &teardownExecution{}
	if err := cli.observeTeardownExecution(ctx, loaded, execution); err != nil {
		return nil, err
	}
	return &resetTeardownBaseline{loaded: loaded, execution: execution}, nil
}

func (baseline *resetTeardownBaseline) binding() string {
	return operationStateDigest(struct {
		Snapshot *teardownSnapshot
		Network  *yardnetwork.RemovalPlan
	}{baseline.execution.snapshot(), baseline.execution.networkRemoval()})
}

func (baseline *resetTeardownBaseline) steps() []domain.OperationStep {
	return baseline.execution.steps(baseline.loaded.Context)
}

func (baseline *resetTeardownBaseline) resources() []ports.TeardownResource {
	return append([]ports.TeardownResource{}, baseline.execution.snapshot().Resources...)
}

func (baseline *resetTeardownBaseline) artifacts() []ports.TeardownArtifact {
	return append([]ports.TeardownArtifact{}, baseline.execution.snapshot().Artifacts...)
}

func (baseline *resetTeardownBaseline) check(ctx context.Context, cli *CLI) error {
	return cli.observeTeardownExecution(ctx, baseline.loaded, baseline.execution)
}

func (baseline *resetTeardownBaseline) verify(ctx context.Context, cli *CLI) error {
	return cli.verifyTeardown(ctx, baseline.loaded, baseline.execution)
}

func prepareTeardownExecution(arguments []string) (*teardownExecution, error) {
	execution := &teardownExecution{revokeAgent: true}
	for _, argument := range arguments {
		switch argument {
		case "-y", "--yes":
		case "--keep-data":
			execution.keepData = true
		case "--reset-config":
			execution.resetConfig = true
		case "--full":
			execution.full = true
		case "-h", "--help":
			return nil, errors.New("help is not an executable teardown operation")
		default:
			return nil, fmt.Errorf("unknown teardown argument %q", argument)
		}
	}
	if execution.keepData && execution.resetConfig {
		return nil, errors.New("--reset-config cannot be combined with --keep-data")
	}
	if execution.full && execution.keepData {
		return nil, errors.New("--full cannot be combined with --keep-data")
	}
	return execution, nil
}

// Scope selection is a conversation before assessment, never deletion consent.
// Read one byte at a time so a buffered reader cannot consume the later answer.
func (cli *CLI) chooseTeardownScope(yard domain.Context, arguments []string) ([]string, error) {
	execution, err := prepareTeardownExecution(arguments)
	if err != nil {
		return nil, err
	}
	if execution.full && yard.AccessKind != domain.AccessLocal {
		return nil, errors.New("--full must run locally on the owner host; run yard teardown --full there")
	}
	if execution.full || cli.env["ASSUME_YES"] == "1" || cli.promptInputTerminal == nil || !cli.promptInputTerminal() {
		return arguments, nil
	}
	for _, argument := range arguments {
		if argument == "--yes" || argument == "-y" {
			return arguments, nil
		}
	}
	owner := "this local owner host"
	if yard.AccessKind != domain.AccessLocal {
		owner = yard.OwnerEndpoint
	}
	fmt.Fprintf(cli.options.Stdout, "Teardown scope for yard %s on %s:\n  1. Only this yard (default)\n  2. Fully remove Subyard and all local yards on the owner host\nSelect [1/2], or q to cancel: ", yard.YardName, owner)
	for {
		var answer strings.Builder
		one := []byte{0}
		for {
			n, readErr := cli.options.Stdin.Read(one)
			if n != 0 {
				if one[0] == '\n' {
					break
				}
				if answer.Len() >= 32 {
					return nil, errors.New("teardown scope answer is too long")
				}
				answer.WriteByte(one[0])
			}
			if readErr != nil {
				return nil, fmt.Errorf("%w: teardown scope input ended", domain.ErrOperationDeclined)
			}
		}
		switch strings.TrimSpace(strings.ToLower(answer.String())) {
		case "", "1":
			return arguments, nil
		case "2":
			selected := append(append([]string{}, arguments...), "--full")
			if _, err := prepareTeardownExecution(selected); err != nil {
				return nil, err
			}
			if yard.AccessKind != domain.AccessLocal {
				return nil, errors.New("full removal must run locally on the owner host")
			}
			return selected, nil
		case "q", "quit", "cancel":
			return nil, domain.ErrOperationDeclined
		default:
			fmt.Fprint(cli.options.Stdout, "Select 1, 2, or q: ")
		}
	}
}

func (execution *teardownExecution) policy(definition command.Definition, yard domain.Context) domain.CommandPolicy {
	consequences := []string{
		"delete yard instance " + yard.YardInstanceName,
		"refuse deletion while a reachable yard has an active SSH session",
		"remove this yard's SSH config and project state",
	}
	if execution.keepData {
		consequences = append(consequences, "keep the project, bridge, storage pool and /srv data")
	} else {
		consequences = append(consequences,
			"delete the yard project and /srv volume",
			"delete the shared bridge, storage pool and disk data only when no other instance or registered local yard uses them",
			"remove the NetworkManager guard only after the bridge disappears",
		)
	}
	if execution.networkRemoval() != nil {
		consequences = append(consequences, execution.networkRemoval().Consequences...)
	}
	if execution.resetConfig {
		consequences = append(consequences, "remove all captured local settings and overrides for this yard after physical cleanup verifies", "keep a local ownership marker that suppresses previous Git and repository yard fallback; preserve shared and host settings")
	}
	return domain.CommandPolicy{
		Name: definition.Name, Effect: domain.CommandEffect(definition.Effect),
		RemotePolicy: domain.RemotePolicy(definition.Remote), Consequences: consequences,
	}
}

func (execution *teardownExecution) actionPlan(
	definition command.Definition,
	yard domain.Context,
) (domain.ActionID, domain.ActionDelta, error) {
	if execution == nil {
		return "", domain.ActionDelta{}, errors.New("teardown execution is required")
	}
	action := domain.ActionID("yard.teardown.purge")
	if execution.keepData {
		action = "yard.teardown.keep-data"
	}
	if !execution.resetConfig && !execution.physicalChanged && execution.snapshot() != nil && execution.snapshot().Agent != nil && execution.snapshot().Agent.State() == "locked" && execution.networkRemoval() != nil && execution.networkRemoval().Cleanup {
		action = "yard.network.save"
		if execution.networkRemoval().Stored.Policy.Isolation {
			action = "yard.network.apply"
		}
		return action, domain.ActionDelta{Changed: true, Consequences: execution.networkRemoval().Consequences}, nil
	}
	delta := domain.ActionDelta{Changed: execution.changed}
	if delta.Changed {
		delta.Consequences = execution.policy(definition, yard).Consequences
	}
	return action, delta, nil
}

func (cli *CLI) observeTeardownExecution(ctx context.Context, loaded config.Loaded, execution *teardownExecution) error {
	return cli.observeTeardownYard(ctx, loaded, execution, true)
}
func (cli *CLI) observeTeardownYard(ctx context.Context, loaded config.Loaded, execution *teardownExecution, checkNetwork bool) error {
	if execution == nil {
		return errors.New("teardown execution is required")
	}
	execution.selectYard(loaded)
	incusPort, _ := cli.statusPorts()
	incusState, err := incusPort.ReconcileState(
		ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName,
		loaded.Environment["SRV_POOL"], loaded.Environment["SRV_VOLUME"],
		loaded.Context.IncusBridge,
	)
	if err != nil {
		return fmt.Errorf("inspect teardown target: %w", err)
	}
	if execution.full && !incusState.ProjectFound && !incusState.InstanceFound {
		incusState.HostPoolFound = false
		incusState.HostNetworkFound = false
	}
	execution.changed = incusState.InstanceFound
	if !execution.keepData {
		execution.changed = execution.changed || incusState.ProjectFound || incusState.ProfileFound ||
			incusState.VolumeFound || incusState.HostNetworkFound || incusState.HostPoolFound
	}
	suffix := ""
	if loaded.Context.YardName != "" && loaded.Context.YardName != "default" {
		suffix = "-" + loaded.Context.YardName
	}
	paths := []string{
		loaded.Context.Paths.StateDir,
		filepath.Join(loaded.Context.Paths.OperatorHome, ".ssh", "subyard"+suffix+".config"),
		filepath.Join(loaded.Context.Paths.DataHome, "space"+suffix+".cache"),
		filepath.Join(loaded.Context.Paths.DataHome, "space"+suffix+".cache.lock"),
		filepath.Join(loaded.Context.Paths.DataHome, "space"+suffix+".cache.tmp"),
	}
	definitions, err := profile.Load(cli.options.RepositoryRoot)
	if err != nil {
		return err
	}
	for _, definition := range definitions {
		if definition.LifecycleOnly {
			continue // These paths belong to full installation cleanup, after yard verification.
		}
		for _, managed := range definition.ManagedPaths {
			root := loaded.Context.Paths.DataHome
			if managed.Root == "operator" {
				root = loaded.Context.Paths.OperatorHome
			}
			paths = append(paths, filepath.Join(root, strings.ReplaceAll(managed.Path, "{yard}", loaded.Context.YardName)))
		}
	}
	var registered []domain.Context
	if execution.full && (execution.Assessed || execution.resumed) {
		registered = execution.contexts()
	}
	snapshot, err := cli.captureTeardownSnapshot(ctx, loaded, incusState, paths, registered)
	if err != nil {
		return err
	}
	if !execution.full {
		owners := []string{"relay"}
		others, err := incusPort.ListInstances(ctx)
		if err != nil {
			return err
		}
		managedNeighbor := hasOtherRegisteredLocalYard(loaded.Context.YardName, snapshot.Registered)
		for _, instance := range others {
			managedNeighbor = managedNeighbor || instance.Project != loaded.Context.IncusProject && instance.Config["user.subyard.managed"] == "true"
		}
		if !managedNeighbor {
			owners = append(owners, "power")
		}
		installation := uninstall.Installation{DataHome: loaded.Context.Paths.DataHome, ConfigHome: loaded.Context.Paths.ConfigHome, RuntimeRoot: firstNonempty(loaded.Environment["YARD_RUNTIME_ROOT"], filepath.Join(loaded.Context.Paths.DataHome, "runtime")), OperatorUID: uint32(os.Getuid())}
		snapshot.Services, err = cli.captureCoreServices(loaded, uninstall.RecoveryPlan{Installation: installation, Yards: []uninstall.RecoveryYard{{Context: loaded.Context, Snapshot: snapshot}}}, owners, loaded.Context.SSHPort)
		if err != nil {
			return err
		}
		for _, service := range snapshot.Services {
			snapshot.Artifacts = append(snapshot.Artifacts, service.Artifacts...)
		}
	}
	if execution.full && !incusState.ProjectFound && !incusState.InstanceFound {
		filtered := snapshot.Resources[:0]
		for _, resource := range snapshot.Resources {
			if resource.Kind != "pool" && resource.Kind != "network" {
				filtered = append(filtered, resource)
			}
		}
		snapshot.Resources = filtered
	}
	if execution.snapshot() != nil {
		if err := checkTeardownSnapshot(execution.snapshot(), snapshot); err != nil {
			return err
		}
	} else {
		execution.activeYard.Snapshot = snapshot
	}
	execution.liveSnapshot = snapshot

	for _, path := range paths {
		_, statErr := os.Lstat(path)
		switch {
		case statErr == nil:
			execution.changed = true
		case errors.Is(statErr, os.ErrNotExist):
		default:
			return fmt.Errorf("inspect teardown artifact %s: %w", path, statErr)
		}
	}
	execution.physicalChanged = execution.changed
	if execution.resetConfig {
		observed, err := captureTeardownConfig(loaded.Context)
		if err != nil {
			return err
		}
		if observed.Reset && loaded.Context.YardName != "default" {
			_, registrationErr := config.FindYardRegistrationFile(loaded.Context.Paths.ConfigDir, loaded.Context.Paths.ConfigHome, loaded.Context.YardName)
			if errors.Is(registrationErr, config.ErrUnknownYard) && (incusState.InstanceFound || incusState.ProjectFound || incusState.VolumeFound || incusState.ProfileFound) {
				return errors.New("completed configuration reset has no registered physical target; refusing derived resource deletion")
			}
			if registrationErr != nil && !errors.Is(registrationErr, config.ErrUnknownYard) {
				return registrationErr
			}
			execution.completedReset = errors.Is(registrationErr, config.ErrUnknownYard)
			if execution.completedReset {
				for _, artifact := range snapshot.Artifacts {
					if artifact.Binding != "" {
						return errors.New("unregistered reset yard acquired runtime artifacts; register the target before teardown")
					}
				}
				if snapshot.Agent.State() != "locked" {
					return errors.New("unregistered reset yard acquired an SSH signing grant")
				}
				execution.physicalChanged = false
				execution.changed = false
			}
		}
		if execution.configSnapshot != nil {
			if err := checkTeardownConfig(execution.configSnapshot, observed); err != nil {
				return err
			}
		} else {
			execution.configSnapshot = observed
		}
		execution.changed = execution.changed || !observed.Reset
		for _, artifact := range observed.Artifacts {
			execution.changed = execution.changed || artifact.Binding != ""
		}
	}
	if execution.revokeAgent && snapshot.Agent.State() != "locked" {
		execution.changed = true
	}
	service := cli.networkService([]domain.Context{loaded.Context})
	if service == nil {
		return errors.New("Incus network policy adapter is required for teardown")
	}
	removal, err := service.PrepareRemoval(ctx, networkYard(loaded.Context))
	if err != nil {
		return err
	}
	if checkNetwork && execution.networkRemoval() != nil && (execution.networkRemoval().Stored.Content != removal.Stored.Content || execution.networkRemoval().Stored.ETag != removal.Stored.ETag) {
		return domain.ErrPlanStale
	}
	if execution.activeYard.Network == nil {
		execution.activeYard.Network = &removal
	}
	if execution.completedReset && removal.Cleanup {
		return errors.New("unregistered reset yard acquired network policy; register the target before teardown")
	}
	execution.changed = execution.changed || removal.Cleanup
	return nil
}

func (cli *CLI) prepareTeardownPrivileges(ctx context.Context, execution *teardownExecution, diagnostics io.Writer) error {
	needsRoot := execution.physicalChanged
	if execution.full {
		needsRoot = len(execution.Services) > 0 || len(execution.Shared) > 0 || len(execution.Yards) > 0
		for _, tree := range execution.Trees {
			needsRoot = needsRoot || tree.Opaque
			for _, entry := range tree.Entries {
				needsRoot = needsRoot || entry.UID != execution.Installation.OperatorUID
			}
		}
	}
	if needsRoot && cli.options.AdapterRunner == nil {
		if err := cli.prepareSudoPrivileges(ctx, diagnostics, cli.effectiveUID(), "teardown"); err != nil {
			return err
		}
	}
	return nil
}

func (execution *teardownExecution) contexts() []domain.Context {
	contexts := make([]domain.Context, 0, len(execution.Yards))
	for _, yard := range execution.Yards {
		contexts = append(contexts, yard.Context)
	}
	sort.Slice(contexts, func(i, j int) bool { return contexts[i].YardName < contexts[j].YardName })
	return contexts
}

func (cli *CLI) executeTeardown(ctx context.Context, orchestrator *application.Orchestrator, loaded config.Loaded, plan domain.OperationPlan, execution *teardownExecution, diagnostics io.Writer) (domain.AdapterResult, error) {
	if execution == nil {
		return domain.AdapterResult{}, errors.New("teardown execution is required")
	}
	if execution.full {
		return cli.executeFullTeardown(ctx, orchestrator, loaded, plan, execution, diagnostics)
	}
	if err := cli.observeTeardownExecution(ctx, loaded, execution); err != nil {
		return domain.AdapterResult{}, err
	}
	if err := cli.prepareTeardownPrivileges(ctx, execution, diagnostics); err != nil {
		return domain.AdapterResult{}, err
	}
	result, err := cli.executeTeardownYards(ctx, orchestrator, loaded, plan, execution, diagnostics, nil, nil, nil)
	if err == nil && result.Status == "ok" && execution.resetConfig {
		err = cli.resetTeardownConfiguration(ctx, loaded, execution)
	}
	return result, err
}

// One driver owns all operation-wide preparation. The physical leaf never
// re-enters public execution or acquires a second network/owner lock.
func (cli *CLI) executeTeardownYards(ctx context.Context, orchestrator *application.Orchestrator, current config.Loaded, plan domain.OperationPlan, execution *teardownExecution, diagnostics io.Writer, before, after func() error, leaf func(string, map[string]string) error) (domain.AdapterResult, error) {
	result := domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID, Status: "ok"}
	execution.holdShared = execution.full
	if execution.full && (len(execution.Yards) > 0 || len(execution.Shared) > 0) && cli.options.AdapterRunner == nil {
		values := structuredCommandContext(current)
		values["SUBYARD_DISPATCHER_PATH"] = cli.options.DispatcherPath
		if err := leaf("prepare-lock", values); err != nil {
			return result, err
		}
	}
	apply := func() error {
		for _, yard := range execution.Yards {
			if execution.revokeAgent {
				manager := sshagentruntime.Manager{Config: sshagentruntime.Config{Directory: sshagentruntime.Directory(yard.Context.Paths.DataHome, yard.Context.YardName)}}
				if err := manager.LockPrepared(ctx, yard.Snapshot.Agent); err != nil {
					return err
				}
			}
			if execution.full {
				loaded := cli.teardownLoaded(yard)
				if err := cli.observeTeardownYard(ctx, loaded, execution, false); err != nil {
					return err
				}
				values := structuredCommandContext(loaded)
				values["SUBYARD_DISPATCHER_PATH"] = cli.options.DispatcherPath
				payload, _ := json.Marshal(execution.liveSnapshot.Resources)
				values["SUBYARD_TEARDOWN_INVENTORY"] = string(payload)
				payload, _ = json.Marshal(ports.ArtifactBindings(yard.Snapshot.Artifacts))
				values["SUBYARD_TEARDOWN_ARTIFACTS"] = string(payload)
				if err := leaf("check-sessions", values); err != nil {
					return err
				}
			}
		}
		if before != nil {
			if err := before(); err != nil {
				return err
			}
		}
		for _, yard := range execution.Yards {
			loaded := cli.teardownLoaded(yard)
			if err := cli.observeTeardownYard(ctx, loaded, execution, false); err != nil {
				return err
			}
			applied, err := cli.applyTeardownYard(ctx, orchestrator, loaded, plan, execution, diagnostics)
			if err != nil {
				return err
			}
			result = applied
		}
		return nil
	}
	var err error
	if len(execution.Yards) == 0 && len(execution.Shared) == 0 {
		err = apply()
		if err == nil && after != nil {
			err = after()
		}
	} else {
		contexts := execution.contexts()
		if !execution.full {
			contexts = execution.snapshot().Registered
		}
		service := cli.networkService(contexts)
		if service == nil {
			return result, errors.New("prepared network cleanup is required for teardown")
		}
		removals := make([]yardnetwork.RemovalPlan, 0, len(execution.Yards))
		for _, yard := range execution.Yards {
			if yard.Network == nil {
				return result, errors.New("approved network removal is missing")
			}
			removals = append(removals, *yard.Network)
		}
		err = service.WithRemovals(ctx, removals, apply, after)
	}
	if err == nil {
		for _, yard := range execution.Yards {
			loaded := cli.teardownLoaded(yard)
			execution.selectYard(loaded)
			if err = cli.verifyTeardown(ctx, loaded, execution); err != nil {
				break
			}
		}
	}
	return result, err
}

func (cli *CLI) applyTeardownYard(
	ctx context.Context,
	orchestrator *application.Orchestrator,
	loaded config.Loaded,
	plan domain.OperationPlan,
	execution *teardownExecution,
	diagnostics io.Writer,
) (domain.AdapterResult, error) {
	if execution == nil {
		return domain.AdapterResult{}, errors.New("teardown execution is required")
	}
	contextValues := structuredCommandContext(loaded)
	contextValues["SUBYARD_DISPATCHER_PATH"] = cli.options.DispatcherPath
	inventory, err := json.Marshal(execution.liveSnapshot.Resources)
	if err != nil {
		return domain.AdapterResult{}, err
	}
	if len(inventory) > 64<<10 {
		return domain.AdapterResult{}, errors.New("teardown inventory exceeds physical guard limit")
	}
	contextValues["SUBYARD_TEARDOWN_INVENTORY"] = string(inventory)
	artifacts, err := json.Marshal(ports.ArtifactBindings(execution.snapshot().Artifacts))
	if err != nil {
		return domain.AdapterResult{}, err
	}
	if len(artifacts) > 64<<10 {
		return domain.AdapterResult{}, errors.New("teardown artifacts exceed physical guard limit")
	}
	contextValues["SUBYARD_TEARDOWN_ARTIFACTS"] = string(artifacts)
	if !execution.full {
		services, err := json.Marshal(execution.snapshot().Services)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		if len(services) > 64<<10 {
			return domain.AdapterResult{}, errors.New("teardown producers exceed physical guard limit")
		}
		contextValues["SUBYARD_TEARDOWN_SERVICES"] = string(services)
	}
	if execution.full {
		if err := execution.bindFullPlanContext(cli, contextValues); err != nil {
			return domain.AdapterResult{}, err
		}
	}
	if execution.physicalChanged && cli.env["SUBYARD_SUDO_PREAUTHORIZED"] == "1" {
		contextValues["SUBYARD_SUDO_PREAUTHORIZED"] = "1"
	}
	if execution.keepData {
		contextValues["SUBYARD_TEARDOWN_KEEP_DATA"] = "1"
	} else {
		contextValues["SUBYARD_TEARDOWN_KEEP_DATA"] = "0"
	}
	contextValues["SUBYARD_TEARDOWN_RESET_CONFIG"] = "0"
	if execution.resetConfig {
		contextValues["SUBYARD_TEARDOWN_RESET_CONFIG"] = "1"
	}
	yards := execution.snapshot().Registered
	contextValues["SUBYARD_TEARDOWN_KEEP_SHARED"] = "0"
	if !execution.full && hasOtherRegisteredLocalYard(loaded.Context.YardName, yards) {
		contextValues["SUBYARD_TEARDOWN_KEEP_SHARED"] = "1"
	}
	if execution.holdShared {
		contextValues["SUBYARD_TEARDOWN_KEEP_SHARED"] = "1"
	}
	contextValues["SUBYARD_TEARDOWN_FULL"] = "0"
	if execution.full {
		contextValues["SUBYARD_TEARDOWN_FULL"] = "1"
	}
	request := domain.AdapterRequest{
		Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID,
		Adapter: "teardown", Action: "apply", Arguments: []string{"--yes"}, Context: contextValues,
	}
	var result domain.AdapterResult
	var stderr string
	if !execution.physicalChanged {
		return domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID, Status: "ok"}, nil
	}
	result, stderr, err = orchestrator.RunAdapter(ctx, plan, request, nil)
	if err == nil && result.Status != "ok" {
		err = errors.New("physical teardown did not succeed")
	}
	writeAdapterDiagnostics(diagnostics, stderr)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == ports.TeardownPlanStaleExitCode {
		err = fmt.Errorf("%w: teardown artifact changed before deletion", domain.ErrPlanStale)
	}
	return result, err
}

func hasOtherRegisteredLocalYard(current string, yards []domain.Context) bool {
	for _, yard := range yards {
		if yard.AccessKind == domain.AccessLocal &&
			yard.YardName != "default" && yard.YardName != current {
			return true
		}
	}
	return false
}
