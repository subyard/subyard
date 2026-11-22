package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
)

func (bootstrap *initBootstrap) description() string {
	if bootstrap.profile != "" {
		return "create named yard definition from profile " + bootstrap.profile
	}
	if bootstrap.environmentProfiles != "" {
		return "create named yard definition with environment profiles [" + bootstrap.environmentProfiles + "] and no coding tools"
	}
	return "create named yard definition with empty environment profiles and coding tools"
}

// validateCreationProfileNames reports every unsafe or repeated name in an
// init --environment-profiles value before any configuration is read.
func validateCreationProfileNames(value string) error {
	var refusals []error
	seen := map[string]bool{}
	for _, name := range strings.Fields(value) {
		switch {
		case !domain.SafeName(name):
			refusals = append(refusals, fmt.Errorf("--environment-profiles: invalid profile name %q", name))
		case seen[name]:
			refusals = append(refusals, fmt.Errorf("--environment-profiles: profile %q is repeated", name))
		}
		seen[name] = true
	}
	return domain.CollectOrderedRefusals(refusals...)
}

// creationProfileEligibility reports every requested profile that is unknown or
// not eligible for the new yard.
func (cli *CLI) creationProfileEligibility(loaded config.Loaded, names []string) error {
	var refusals []error
	for _, name := range names {
		if _, err := profile.ResolvePackage(cli.options.RepositoryRoot, name); err != nil {
			refusals = append(refusals, fmt.Errorf("--environment-profiles: unknown environment profile %q", name))
			continue
		}
		eligible, reason, err := config.EnvironmentProfileEligibility(loaded.Context.Paths.ConfigDir, loaded.Context.YardName, loaded.Environment, loaded.Catalog, name)
		if err != nil {
			refusals = append(refusals, err)
		} else if !eligible {
			refusals = append(refusals, fmt.Errorf("--environment-profiles: profile %q is not eligible for this yard (%s)", name, reason))
		}
	}
	return domain.CollectOrderedRefusals(refusals...)
}

// resumedCreationSelection lets a rerun after an interrupted creation continue
// as plain init when the saved selection is the requested one.
func resumedCreationSelection(loaded config.Loaded, value string) error {
	saved := slices.Sorted(slices.Values(loaded.EnvironmentProfiles.Requested))
	requested := slices.Sorted(slices.Values(strings.Fields(value)))
	if loaded.EnvironmentProfiles.Present && slices.Equal(slices.Compact(saved), requested) {
		return nil
	}
	yard := loaded.Context.YardName
	return fmt.Errorf("yard %s already exists with a different environment profile selection; resume with plain yard -Y %s init, or change the selection with yard -Y %s profile enable|disable ID or yard -Y %s config set ENVIRONMENT_PROFILES 'LIST' --scope yard", yard, yard, yard, yard)
}

// Only init supplies this draft. Ordinary context loading still requires a
// registered name, and all freshness checks continue to inspect native inputs.
func (cli *CLI) loadEmptyNamedYard(yard, environmentProfiles string) (config.Loaded, *initBootstrap, error) {
	home := cli.env["SUBYARD_OPERATOR_HOME"]
	if home == "" {
		home = cli.env["HOME"]
	}
	configHome, err := config.ResolveConfigHome(home, cli.env)
	if err != nil {
		return config.Loaded{}, nil, err
	}
	bootstrap := &initBootstrap{targetPath: filepath.Join(configHome, "yards", yard, "config.env"), environmentProfiles: environmentProfiles}
	// Clear the independent selections, including an inherited yard preset.
	content := func(port int) []byte {
		return []byte("YARD_TEMPLATE=''\nENVIRONMENT_PROFILES='" + environmentProfiles + "'\nCODING_TOOL_INTEGRATIONS=''\nSSH_PORT=" + strconv.Itoa(port) + "\n")
	}
	bootstrap.content = content(2222)
	files := &config.ProspectiveFiles{Root: configHome, ReadSnapshot: func(path string) (config.PersistentFileSnapshot, error) {
		return config.ReadPersistentFileSnapshot(configHome, path)
	}}
	files.ReadPreparedInput = func(path string) (config.PersistentFileSnapshot, bool, error) {
		if path != bootstrap.targetPath {
			return config.PersistentFileSnapshot{}, false, nil
		}
		return config.PersistentFileSnapshot{Exists: true, Content: bootstrap.content}, true, nil
	}
	load := func() (config.Loaded, error) {
		return config.Load(config.LoadOptions{Catalog: &cli.catalog, Resources: cli.operationResourceRegistry,
			RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: home, YardName: yard,
			Environment: cli.env, ProspectiveFiles: files})
	}
	loaded, err := load()
	if err != nil {
		return config.Loaded{}, nil, err
	}
	peers, err := cli.powerYardContexts(loaded)
	if err != nil {
		return config.Loaded{}, nil, err
	}
	used := map[int]bool{}
	for _, peer := range peers {
		used[peer.SSHPort] = true
	}
	port := 2222
	for ; port <= 65535; port++ {
		if !used[port] && availableSSHPort(port) == nil {
			break
		}
	}
	if port > 65535 {
		return config.Loaded{}, nil, errors.New("no available owner-local SSH port for named yard")
	}
	bootstrap.content = content(port)
	loaded, err = load()
	if err != nil {
		return config.Loaded{}, nil, err
	}
	// The effective selection drops ineligible profiles; compare the authored one.
	if strings.Join(loaded.EnvironmentProfiles.Requested, " ") != environmentProfiles {
		return config.Loaded{}, nil, errors.New("command environment overrides new named yard at setting ENVIRONMENT_PROFILES")
	}
	for name, desired := range map[string]string{"YARD_TEMPLATE": "", "CODING_TOOL_INTEGRATIONS": "", "SSH_PORT": strconv.Itoa(port)} {
		if loaded.Environment[name] != desired {
			return config.Loaded{}, nil, fmt.Errorf("command environment overrides new named yard at setting %s", name)
		}
	}
	if err := cli.creationProfileEligibility(loaded, loaded.EnvironmentProfiles.Requested); err != nil {
		return config.Loaded{}, nil, err
	}
	if loaded.Context.AccessKind != domain.AccessLocal {
		return config.Loaded{}, nil, errors.New("new named yard init requires local access")
	}
	cli.adoptContext(loaded)
	return loaded, bootstrap, nil
}

func availableSSHPort(port int) error {
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return listener.Close()
}

func (execution *initExecution) checkRegistration(ctx context.Context, cli *CLI, nativeRequired bool) error {
	return execution.checkRegistrationLocked(ctx, cli, nativeRequired, false)
}

func (execution *initExecution) checkRegistrationLocked(ctx context.Context, cli *CLI, nativeRequired, locked bool) error {
	if execution.bootstrap == nil {
		return nil
	}
	if _, native := execution.platform.(reconcileruntime.Runtime); execution.bootstrap.profile != "" && !native {
		return nil
	}
	loaded := execution.loaded
	_, err := config.FindYardRegistrationFile(loaded.Context.Paths.ConfigHome, loaded.Context.YardName)
	if !errors.Is(err, config.ErrUnknownYard) {
		return fmt.Errorf("%w: named-yard registration is no longer absent", domain.ErrPlanStale)
	}
	if _, err := os.Lstat(execution.bootstrap.targetPath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: named-yard registration target is no longer absent", domain.ErrPlanStale)
	}
	peers, err := cli.powerYardContextsLocked(loaded, locked)
	if err != nil {
		return err
	}
	for _, peer := range peers {
		if peer.SSHPort == loaded.Context.SSHPort || peer.IncusProject == loaded.Context.IncusProject || peer.Paths.HostBase == loaded.Context.Paths.HostBase {
			return fmt.Errorf("%w: named yard conflicts with registered yard %q", domain.ErrPlanStale, peer.YardName)
		}
	}
	if err := availableSSHPort(loaded.Context.SSHPort); err != nil {
		return fmt.Errorf("%w: planned owner-local SSH port is unavailable", domain.ErrPlanStale)
	}
	if cli.options.InitPlatform != nil {
		return nil // Synthetic platform owns its native observations.
	}
	incus, _ := cli.statusPorts()
	state, err := incus.ReconcileState(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName,
		loaded.Environment["SRV_POOL"], loaded.Environment["SRV_VOLUME"], loaded.Context.IncusBridge)
	if err != nil {
		if !nativeRequired {
			return nil // Incus enrollment is an approved prerequisite, before publication.
		}
		return fmt.Errorf("inspect new named-yard namespace: %w", err)
	}
	if state.ProjectFound || state.InstanceFound || state.VolumeFound {
		return fmt.Errorf("%w: new named-yard Incus namespace already exists; preserve existing resources", domain.ErrPlanStale)
	}
	return nil
}

func (execution *initExecution) publishRegistration(ctx context.Context, cli *CLI) error {
	unlock, err := lockOwnerOperation(ctx, execution.loaded.Context.Paths.ConfigHome, "yard-registration.lock")
	if err != nil {
		return err
	}
	defer unlock()
	return config.CreatePersistentFile(execution.loaded.Context.Paths.ConfigHome, execution.bootstrap.targetPath, execution.bootstrap.content, func() error {
		if err := execution.checkRegistrationLocked(ctx, cli, true, true); err != nil {
			return err
		}
		return execution.inputBaseline.checkLocked(ctx, cli, true)
	})
}
