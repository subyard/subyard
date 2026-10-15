package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// Registered-source repair is assessed before loading possibly broken effective
// settings. Only the preserved explicit subtree contributes prospective settings.
func (cli *CLI) runRegisteredHostRename(ctx context.Context, yard string, args []string, yes bool) (int, bool) {
	yes = yes || slices.Contains(args, "--yes") || slices.Contains(args, "-y")
	arguments := hostArguments(args)
	if len(arguments) == 0 || arguments[0] != "rename" || commandHelpRequested(arguments) {
		return 0, false
	}
	values := cli.configSourceEnvironment()
	home, err := config.ResolveConfigHome(values["SUBYARD_OPERATOR_HOME"], values)
	if err != nil {
		return 0, false
	}
	_, registered, err := configsync.ReadSourceRecord(home)
	if err != nil {
		cli.errorf("host rename: %v", err)
		return 1, true
	}
	if !registered {
		return 0, false
	}
	if !domain.SafeName(yard) || yard != "default" {
		cli.errorf("host rename must run on the local owner without a yard selector")
		return 2, true
	}
	sourceID := ""
	if len(arguments) == 4 && arguments[2] == "--source-host-id" {
		sourceID = arguments[3]
		arguments = arguments[:2]
	}
	if len(arguments) != 2 {
		cli.hostUsage()
		return 2, true
	}
	recovery, pendingRecovery, err := configsync.AssessHostIDRenameRecovery(home)
	if err != nil {
		cli.errorf("host rename recovery: %v", err)
		return 1, true
	}
	authority, err := cli.observeHostRenameAuthority(ctx, home)
	if err != nil {
		cli.errorf("host rename: %v", err)
		return 1, true
	}
	defer authority.close()
	if authority.installed {
		if selected := values["SUBYARD_CONFIG_DIR"]; selected != "" && filepath.Clean(selected) != authority.configDir {
			cli.errorf("host rename requires sealed configuration assets")
			return 1, true
		}
		values["SUBYARD_CONFIG_DIR"] = authority.configDir
		if authority.admission.Metadata.Journal != nil && authority.admission.Metadata.Journal.Checkpoint != releasetransition.JournalComplete && sourceID == "" && !(pendingRecovery && recovery.Source) {
			cli.errorf("finish the pending release transition before renaming; explicit preserved-source recovery requires --source-host-id")
			return 1, true
		}
	}
	store := ownerinventory.Connections{Root: filepath.Join(values["SUBYARD_HOME"], "owner-inventory")}
	if values["SUBYARD_HOME"] == "" {
		store.Root = filepath.Join(values["SUBYARD_OPERATOR_HOME"], ".subyard", "owner-inventory")
	}
	if err := guardLocalRenameOwner(store, arguments[1]); err != nil {
		cli.errorf("host rename: %v", err)
		return 1, true
	}
	if pendingRecovery {
		if recovery.Target != arguments[1] {
			cli.errorf("host rename recovery targets a different identity; resume the original rename first")
			return 1, true
		}
		if err := cli.recoverAssessedHostRename(ctx, recovery, store, authority); err != nil {
			cli.errorf("host rename recovery: %v", err)
			return 1, true
		}
		current, pending, err := configsync.ResolveHostID(home, nil)
		if err != nil || pending || current != arguments[1] {
			cli.errorf("host rename recovery did not reach the requested owner identity")
			return 1, true
		}
		fmt.Fprintln(cli.options.Stdout, "Owner HostID rename recovered: "+current)
		return 0, true
	}
	// Default-only loading discovers typed file mappings without consuming damaged
	// cache or local sources. Native preparation validates the complete prospective
	// source plus local layers, including all yard ports and file overrides.
	empty, err := os.MkdirTemp("/tmp", "subyard-rename-defaults-")
	if err != nil {
		cli.errorf("host rename: %v", err)
		return 1, true
	}
	defer os.RemoveAll(empty)
	defaults := map[string]string{}
	for key, value := range values {
		defaults[key] = value
	}
	defaults["SUBYARD_CONFIG_HOME"] = empty
	loaded, err := config.Load(config.LoadOptions{RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: values["SUBYARD_OPERATOR_HOME"], YardName: "default", Environment: defaults, DisablePrivate: true})
	if err != nil {
		cli.errorf("host rename: %v", err)
		return 1, true
	}
	loaded.Context.Paths.ConfigHome = home
	loaded.Context.Paths.OperatorHome = values["SUBYARD_OPERATOR_HOME"]
	loaded.Context.AccessKind = domain.AccessLocal
	plan, err := configsync.PrepareHostIDRenameWithSource(configsync.Options{ConfigHome: home, RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: values["SUBYARD_OPERATOR_HOME"], Environment: values, FileSettings: config.SyncableFileMappings(loaded)}, arguments[1], sourceID)
	if err != nil {
		cli.errorf("host rename: %v", err)
		return 1, true
	}
	if plan.AlreadyMigrated {
		fmt.Fprintln(cli.options.Stdout, "Owner settings are already migrated to "+plan.NewHostID)
		return 0, true
	}
	consequences := append([]string{fmt.Sprintf("rename owner HostID %s -> %s; preserve local overrides, ports and runtime resource identities", plan.OldHostID, plan.NewHostID)}, plan.SourceConsequences()...)
	cli.ensureOperationID()
	_, err = cli.operationOrchestrator(cli.env["SUBYARD_OPERATION_ID"], loaded, nil, nil).PlanAction(ctx, loaded.Context, "host rename", domain.RemoteOnController, "host.rename", domain.ActionDelta{Changed: true, Consequences: consequences}, yes || cli.env["ASSUME_YES"] == "1")
	if err != nil {
		cli.errorf("host rename: %v", err)
		return 1, true
	}
	err = authority.withLock(ctx, cli, func() error {
		return store.WithAvailableLocalHostID(ctx, plan.NewHostID, func() error { return configsync.ApplyHostIDRename(plan) })
	})
	if err != nil {
		if errors.Is(err, configsync.ErrPlanStale) {
			err = domain.ErrPlanStale
		}
		cli.errorf("host rename: %v", err)
		return 1, true
	}
	fmt.Fprintf(cli.options.Stdout, "Owner HostID renamed: %s -> %s\n", plan.OldHostID, plan.NewHostID)
	return 0, true
}

func guardLocalRenameOwner(store ownerinventory.Connections, hostID string) error {
	connections, err := store.ListReadOnly()
	if err != nil {
		return err
	}
	for _, connection := range connections {
		if connection.HostID == hostID {
			return fmt.Errorf("owner HostID %q is already registered for a remote owner", hostID)
		}
	}
	return nil
}

func (cli *CLI) recoverLocalHostRename(ctx context.Context, loaded config.Loaded) error {
	recovery, pending, err := configsync.AssessHostIDRenameRecovery(loaded.Context.Paths.ConfigHome)
	if err != nil || !pending {
		return err
	}
	store := ownerinventory.Connections{Root: filepath.Join(loaded.Context.Paths.DataHome, "owner-inventory")}
	return cli.recoverAssessedHostRename(ctx, recovery, store, nil)
}

func (cli *CLI) recoverAssessedHostRename(ctx context.Context, recovery configsync.HostIDRenameRecoveryPlan, store ownerinventory.Connections, authority *hostRenameAuthority) error {
	recover := func() error {
		return store.WithAvailableLocalHostID(ctx, recovery.Target, func() error { return configsync.RecoverHostIDRenameExact(recovery) })
	}
	if authority != nil {
		return authority.withLock(ctx, cli, recover)
	}
	if !recovery.Source {
		return recover()
	}
	authority, err := cli.observeHostRenameAuthority(ctx, recovery.ConfigHome)
	if err != nil {
		return err
	}
	defer authority.close()
	return authority.withLock(ctx, cli, recover)
}

// The same protected authority encloses first publication and durable recovery.
// Observing a journal never substitutes for holding migrations/update.lock.
type hostRenameAuthority struct {
	runtime            *releaseruntime.SourceInspector
	store              *releasetransition.POSIXV2Store
	home               string
	installed          bool
	configDir, tooling string
	admission          releaseruntime.SourceAdmission
	owner              ownerWriteAdmission
}

func (authority *hostRenameAuthority) close() { _ = authority.runtime.Close() }
func (cli *CLI) observeHostRenameAuthority(ctx context.Context, home string) (authority *hostRenameAuthority, err error) {
	runtime := releaseruntime.NewSourceInspector(cli.options.RepositoryRoot, cli.options.Stderr)
	authority = &hostRenameAuthority{runtime: runtime, home: home}
	defer func() {
		if err != nil {
			authority.close()
		}
	}()
	values := cli.configSourceEnvironment()
	authority.owner, err = captureOwnerWriteAdmission(config.Loaded{Context: domain.Context{Paths: domain.RuntimePaths{
		DataHome: firstNonempty(values["SUBYARD_HOME"], filepath.Join(values["SUBYARD_OPERATOR_HOME"], ".subyard")), ConfigHome: home,
	}}}, cli.options.RepositoryRoot)
	if err != nil {
		return authority, err
	}
	authority.store, err = releasetransition.NewPOSIXV2Store(home)
	if err != nil {
		return authority, err
	}
	options, installed, err := cli.mutationGateReleaseOptions()
	if err != nil {
		return authority, err
	}
	if installed && options.ConfigHome != home {
		return authority, errors.New("owner rename release context changed")
	}
	if installed {
		if _, rootErr := os.Lstat(options.RuntimeRoot); errors.Is(rootErr, os.ErrNotExist) {
			snapshot, readErr := authority.store.ReadCurrentJournal()
			if readErr == nil && !snapshot.Exists {
				installed = false
			}
		}
	}
	authority.installed = installed
	if installed {
		authority.configDir, authority.tooling, err = runtime.InspectSourceTooling(ctx, options.RuntimeRoot, cli.options.RepositoryRoot)
		if err != nil {
			return authority, err
		}
		authority.admission, err = cli.inspectConfigSourceAdmission(ctx, runtime)
		return authority, err
	}
	outcome, err := cli.inspectMutationGate(ctx, "default")
	if err != nil {
		return authority, err
	}
	if outcome != nil {
		return authority, errors.New("protected release history does not admit owner source repair")
	}
	return authority, nil
}
func (authority *hostRenameAuthority) withLock(ctx context.Context, cli *CLI, apply func() error) error {
	release, err := cli.acquireOwnerMutation(ctx, authority.owner, false, true)
	if err != nil {
		return err
	}
	defer release()
	unlock, err := authority.store.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	current, err := cli.observeHostRenameAuthority(ctx, authority.home)
	if err != nil {
		return err
	}
	defer current.close()
	if current.installed != authority.installed || current.configDir != authority.configDir || current.tooling != authority.tooling || current.admission.Binding != authority.admission.Binding {
		return fmt.Errorf("%w: owner rename protected authority changed", domain.ErrPlanStale)
	}
	return apply()
}
