package cli

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

func (cli *CLI) checkNativeOwner(ctx context.Context, loaded config.Loaded) error {
	_, err := cli.observeNativeOwner(ctx, loaded)
	return err
}

func (cli *CLI) observeNativeOwner(ctx context.Context, loaded config.Loaded) (map[string]string, error) {
	if loaded.Context.AccessKind == domain.AccessRemote {
		return nil, nil
	}
	native, _ := cli.statusPorts()
	if absent, ok := native.(interface{ LocalInstallationAbsent() bool }); ok && absent.LocalInstallationAbsent() {
		return nil, nil
	}
	instance, err := native.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) || errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]string{loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: instance.Config["user.subyard.name"]}, domain.NativeTargetOwnership(loaded.Context, instance.Config)
}

// Collision assessment only resolves addresses. Full-inventory callers report
// their load failures; an unreadable peer cannot hide an independent known pair.
func (cli *CLI) checkNativeTargets(current config.Loaded, allLocal bool, owners map[string]string) error {
	if current.Context.AccessKind == domain.AccessRemote && !allLocal {
		return nil
	}
	names, err := config.YardNames(current.Context.Paths.ConfigDir, current.Context.Paths.ConfigHome)
	if err != nil {
		return err
	}
	if current.ProspectiveFiles != nil {
		names = append(names, current.ProspectiveFiles.Yards...)
		slices.Sort(names)
		names = slices.Compact(names)
	}
	var yards []config.Loaded
	// Explicit reset retries can resolve a retired yard; it is no longer configured.
	if slices.Contains(names, current.Context.YardName) || current.ProspectiveFiles != nil {
		yards = append(yards, current)
	}
	for _, name := range names {
		if name == current.Context.YardName {
			continue
		}
		loaded, err := cli.loadNativeTargetContext(name, current.Context.Paths.ConfigHome, current.ProspectiveFiles, current.Context.Paths.ConfigDir)
		if err == nil {
			yards = append(yards, loaded)
		}
	}
	selected := current.Context.YardName
	if allLocal {
		selected = ""
	}
	return config.NativeTargetCollisions(yards, owners, selected)
}

// Registry assessment needs addresses even when a guest provisioner is unavailable.
func (cli *CLI) loadNativeTargetContext(yard, home string, files *config.ProspectiveFiles, configDir string) (config.Loaded, error) {
	environment := cli.freshMigrationEnvironment(cli.baseEnv, cli.options.RepositoryRoot)
	environment["SUBYARD_CONFIG_HOME"] = home
	if configDir != "" {
		environment["SUBYARD_CONFIG_DIR"] = configDir
	}
	return config.Load(config.LoadOptions{Catalog: &cli.catalog, RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: firstNonempty(environment["SUBYARD_OPERATOR_HOME"], environment["HOME"]), YardName: yard, Environment: environment, NativeTargetOnly: true, ProspectiveFiles: files})
}

// Address repair changes only explicit local scalar configuration. Every gate
// blocker must carry the native-target class emitted by the sealed inspector.
func nativeTargetAddressRepair(handler string, arguments []string, inspection *releasetransition.Inspection) bool {
	arguments = configArguments(arguments)
	if handler != "@config" || len(arguments) == 0 || !slices.Contains([]string{"set", "unset"}, arguments[0]) || inspection == nil || len(inspection.Blockers) == 0 {
		return false
	}
	request, err := parseConfigAuthoringRequest(arguments[0], arguments[1:], false)
	if err != nil || request.git || request.scope != config.ScopeYard && request.scope != config.ScopeHost || !slices.Contains([]string{"INCUS_PROJECT", "YARD_INSTANCE_NAME", "SRV_VOLUME"}, request.name) {
		return false
	}
	for _, blocker := range inspection.Blockers {
		if blocker.Code != releasetransition.CodeDependencyUnavailable || !strings.HasPrefix(blocker.Resource, "activation.") || !strings.HasSuffix(blocker.Resource, ".native-target") {
			return false
		}
	}
	return true
}
