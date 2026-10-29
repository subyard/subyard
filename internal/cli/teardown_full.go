package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/adapters/sshagentruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/state"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func (prepared *preparedCommand) bindFullInstallation() error {
	if prepared.CLI.fullTeardownRecovery == "" {
		actualRuntime := runtimeRootForRepository(prepared.CLI.options.RepositoryRoot, "")
		if actualRuntime != "" {
			self, readErr := uninstall.ReadInstallationFile(filepath.Join(actualRuntime, uninstall.ManifestName), "", uint32(os.Getuid()))
			if readErr == nil {
				for name, value := range map[string]string{"SUBYARD_HOME": self.DataHome, "SUBYARD_CONFIG_HOME": self.ConfigHome} {
					if explicit := prepared.CLI.baseEnv[name]; explicit != "" && explicit != value {
						return config.OwnerMetadataError(filepath.Join(actualRuntime, uninstall.ManifestName), "full uninstall installation "+name, "selects another installation; run full removal from its own launcher", name+": stored="+config.QuoteMetadataPath(value)+", current="+config.QuoteMetadataPath(explicit), nil)
					}
					prepared.CLI.env[name] = value
				}
				prepared.CLI.env["YARD_RUNTIME_ROOT"] = self.RuntimeRoot
				loaded, loadErr := prepared.CLI.loadContext(prepared.Loaded.Context.YardName)
				if loadErr != nil {
					return loadErr
				}
				prepared.Loaded = loaded
			} else if !uninstall.InstallationAbsent(readErr) {
				return readErr
			}
		}
	}
	return nil
}

func (cli *CLI) observeFullTeardown(ctx context.Context, current config.Loaded, execution *fullTeardown) error {
	execution.full, execution.revokeAgent = true, true
	// A normal-reconcile grant is not evidence that deleting its config/data or
	// native workloads is authorized, even when bootstrap inventory exists.
	legacyRoot := filepath.Join(current.Context.Paths.ConfigHome, "ownership", "legacy-continuations")
	if info, err := os.Lstat(legacyRoot); err == nil {
		if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return errors.New("unsafe legacy continuation namespace; preserve its records before full removal")
		}
		entries, err := os.ReadDir(legacyRoot)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return errors.New("legacy continuation grants no full removal ownership; preserve legacy workloads and their protected continuation record")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	installation, err := uninstall.ReadInstallation(current.Context.Paths.DataHome, uint32(os.Getuid()))
	if uninstall.InstallationAbsent(err) {
		runtimeRoot := runtimeRootForRepository(cli.options.RepositoryRoot, firstNonempty(current.Environment["YARD_RUNTIME_ROOT"], filepath.Join(current.Context.Paths.DataHome, "runtime")))
		installation, err = uninstall.ReadInstallationFile(filepath.Join(runtimeRoot, uninstall.ManifestName), current.Context.Paths.DataHome, uint32(os.Getuid()))
	}
	if uninstall.InstallationAbsent(err) {
		installation, err = cli.discoverLegacyInstallation(current)
	}
	if err != nil {
		return err
	}
	// Runtime publication may add verified cache artifacts after bootstrap.
	if runtimeInventory, readErr := uninstall.ReadInstallationFile(filepath.Join(installation.RuntimeRoot, uninstall.ManifestName), installation.DataHome, installation.OperatorUID); readErr == nil {
		if err := runtimeInventory.CheckRoots(installation.ConfigHome, installation.RuntimeRoot, installation.CacheRoot); err != nil {
			return config.OwnerMetadataContext(filepath.Join(installation.RuntimeRoot, uninstall.ManifestName), "runtime installation inventory", err)
		}
		if err := runtimeInventory.CheckRuntimeScope(); err != nil {
			return config.OwnerMetadataContext(filepath.Join(installation.RuntimeRoot, uninstall.ManifestName), "runtime installation inventory", err)
		}
		byPath := map[string]uninstall.Entry{}
		for _, entry := range installation.CacheFiles {
			byPath[entry.Path] = entry
		}
		for _, entry := range runtimeInventory.CacheFiles {
			byPath[entry.Path] = entry
		}
		installation.CacheFiles = nil
		for _, entry := range byPath {
			installation.CacheFiles = append(installation.CacheFiles, entry)
		}
		sort.Slice(installation.CacheFiles, func(i, j int) bool { return installation.CacheFiles[i].Path < installation.CacheFiles[j].Path })
	} else if !uninstall.InstallationAbsent(readErr) {
		return readErr
	}
	if installation.ConfigHome != current.Context.Paths.ConfigHome {
		return config.OwnerMetadataError("", "full uninstall installation", "config root differs from the recorded installation", "configHome: stored="+config.QuoteMetadataPath(installation.ConfigHome)+", current="+config.QuoteMetadataPath(current.Context.Paths.ConfigHome), nil)
	}
	if runtimeRootForRepository(cli.options.RepositoryRoot, installation.RuntimeRoot) != installation.RuntimeRoot {
		return config.OwnerMetadataError("", "full uninstall installation", "the executing runtime belongs to a different recorded installation", "runtimeRoot: stored="+config.QuoteMetadataPath(installation.RuntimeRoot)+", current="+config.QuoteMetadataPath(runtimeRootForRepository(cli.options.RepositoryRoot, installation.RuntimeRoot)), nil)
	}
	if execution.Assessed {
		if operationStateDigest(execution.Installation) != operationStateDigest(installation) {
			return domain.ErrPlanStale
		}
		for _, yard := range execution.Yards {
			loaded := cli.teardownLoaded(yard)
			if err := cli.observeTeardownExecution(ctx, loaded, execution); err != nil {
				return err
			}
		}
		for _, tree := range append(append([]uninstall.Tree{}, execution.Trees...), execution.SourceAssets...) {
			if err := tree.Check(); err != nil {
				return err
			}
		}
		for _, tree := range execution.Launchers {
			if err := tree.Check(); err != nil {
				return err
			}
		}
		for _, edit := range execution.Startup {
			if err := edit.Before.Check(); err != nil {
				return err
			}
		}
		for _, service := range execution.Services {
			for _, artifact := range service.Artifacts {
				binding, err := teardownArtifactBinding(artifact.Path)
				if err != nil {
					return err
				}
				if binding != "" && binding != artifact.Binding {
					return domain.ErrPlanStale
				}
			}
		}
		return nil
	}
	execution.Installation = installation
	original, _ := releaseruntime.HistoricalUninstallProducerEvidence(ctx, installation.RuntimeRoot)
	execution.Services, err = cli.captureCoreServices(current, uninstall.RecoveryPlan{Installation: installation}, []string{"power", "keys"}, 0, original)
	if err != nil {
		return err
	}
	keepBinPath, err := uninstall.KeepBinPath(installation.BinDir)
	if err != nil {
		return err
	}
	for _, path := range installation.ShellFiles {
		edit, err := uninstall.PrepareStartup(path, installation.RuntimeRoot, installation.BinDir, keepBinPath)
		if err != nil {
			return err
		}
		execution.Startup = append(execution.Startup, edit)
	}
	if installation.BinDir != "" {
		for _, name := range []string{"yard", "sy"} {
			path := filepath.Join(installation.BinDir, name)
			tree, err := uninstall.Capture(path)
			if err != nil {
				return err
			}
			if len(tree.Entries) > 0 {
				target := tree.Entries[0].Link
				if target != "" && !filepath.IsAbs(target) {
					target = filepath.Clean(filepath.Join(installation.BinDir, target))
				}
				if len(tree.Entries) != 1 || tree.Entries[0].UID != installation.OperatorUID || (target != filepath.Join(installation.RuntimeRoot, "current", "bin", "yard") && target != filepath.Join(cli.options.RepositoryRoot, "bin", "yard")) {
					return fmt.Errorf("full uninstall launcher has unknown or foreign ownership: %s", path)
				}
			}
			execution.Launchers = append(execution.Launchers, tree)
		}
	}
	hostRecords, err := uninstall.ReadHostPaths(installation)
	if err != nil {
		return err
	}
	hostOwned := map[string][]string{}
	hostRecorded := map[string]bool{}
	for _, record := range hostRecords {
		hostRecorded[record.Root] = true
		for _, entry := range record.Paths {
			if entry.Created {
				hostOwned[record.Root] = append(hostOwned[record.Root], entry.Name)
			}
		}
	}
	names, err := config.YardNames(current.Context.Paths.ConfigDir, current.Context.Paths.ConfigHome)
	if err != nil {
		return err
	}
	incusPort, _ := cli.statusPorts()
	cold := false
	if client, ok := incusPort.(*incusclient.Client); ok {
		cold = client.LocalInstallationAbsent()
	}
	if cold && len(names) > 1 {
		return errors.New("registered yards remain but Incus ownership cannot be inspected; restore Incus before full removal")
	}
	if cold {
		names = nil
	}
	roots := []string{installation.DataHome, installation.ConfigHome, installation.CacheRoot}
	protected := append([]string{cli.options.RepositoryRoot}, execution.PreservedRoots...)
	// The running installed release is removed last; a user source checkout
	// remains protected even when it happens to overlap a configured root.
	if strings.HasPrefix(cli.options.RepositoryRoot, installation.RuntimeRoot+"/releases/") {
		protected = nil
	}
	source, exists, err := configsync.ReadSourceRecord(installation.ConfigHome)
	if err != nil {
		return err
	}
	if exists {
		protected = append(protected, source.Checkout)
	}
	knownProjects := map[string]bool{}
	foreignProjects := map[string]bool{}
	installationYards := []ports.InstallationYard{}
	admittedProjects := map[string]bool{}
	legacyProjects := map[string]bool{}
	if reader, ok := incusPort.(ports.InstallationYardReader); ok {
		installationYards, err = reader.InstallationYards(ctx)
		if err != nil {
			return err
		}
		for _, yard := range installationYards {
			if yard.OperatorUID != strconv.Itoa(os.Getuid()) || yard.DataHome != installation.DataHome || yard.ConfigHome != installation.ConfigHome {
				foreignProjects[yard.Project] = true
				execution.Preserved = append(execution.Preserved, "project "+yard.Project+" owned by another operator or installation")
				for _, path := range []string{yard.DataHome, yard.ConfigHome, yard.HostBase} {
					if path != "" {
						if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
							return fmt.Errorf("foreign project %s has an invalid protected root; preserve its resources and restore its owner context", yard.Project)
						}
						protected = append(protected, path)
					}
				}
			} else {
				admittedProjects[yard.Project] = true
			}
		}
	}
	ownedSources := []string{current.Context.YardName, firstNonempty(current.Environment["E2E_BROKER_SOURCE"], current.Context.YardName)}
	addYard := func(loaded config.Loaded) error {
		if loaded.Context.AccessKind == domain.AccessRemote {
			return nil
		}
		if loaded.Context.Paths.DataHome != installation.DataHome || loaded.Context.Paths.ConfigHome != installation.ConfigHome {
			return fmt.Errorf("yard %s belongs to a different installation root", loaded.Context.YardName)
		}
		if foreignProjects[loaded.Context.IncusProject] {
			execution.PreservedRoots = append(execution.PreservedRoots, loaded.Context.Paths.HostBase)
			protected = append(protected, loaded.Context.Paths.HostBase)
			return nil
		}
		legacy := false
		if !admittedProjects[loaded.Context.IncusProject] && !cold {
			native, err := incusPort.ReconcileState(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName, loaded.Environment["SRV_POOL"], loaded.Environment["SRV_VOLUME"], loaded.Context.IncusBridge)
			if err != nil {
				return err
			}
			if native.ProjectFound {
				if err := cli.inspectLegacyFullYardProvenance(ctx, loaded, installation); err != nil {
					return fmt.Errorf("project %s has no exact installation ownership: %w; preserve its resources and restore proven ownership before full removal", loaded.Context.IncusProject, err)
				}
				legacy = true
			}
		}
		ownedSources = append(ownedSources, loaded.Context.YardName, firstNonempty(loaded.Environment["E2E_BROKER_SOURCE"], loaded.Context.YardName))
		if err := cli.observeTeardownExecution(ctx, loaded, execution); err != nil {
			return err
		}
		if legacy {
			if err := validateLegacyFullYard(loaded, installation, execution.snapshot()); err != nil {
				return fmt.Errorf("project %s has no exact installation ownership: %w; preserve its resources and restore proven ownership before full removal", loaded.Context.IncusProject, err)
			}
			legacyProjects[loaded.Context.IncusProject] = true
			execution.ManagedRoots = append(execution.ManagedRoots, filepath.Join(installation.DataHome, "e2e", "routes"))
		}
		if execution.snapshot().State.ProjectFound || execution.snapshot().State.InstanceFound {
			knownProjects[loaded.Context.IncusProject] = true
			roots = append(roots, loaded.Context.Paths.HostBase)
		}
		for _, key := range []string{"SUBYARD_KEYS_ROOT", "SUBYARD_KEYS_CONSUMER_ROOT", "SUBYARD_KEYS_TOOLS_DIR"} {
			if path := loaded.Environment[key]; path != "" {
				roots = append(roots, path)
			}
		}
		credentials, err := cli.credentialRuntime(loaded)
		if err != nil {
			return err
		}
		if credentials.Initialized() {
			keyRoot := firstNonempty(loaded.Environment["SUBYARD_KEYS_ROOT"], filepath.Join(loaded.Context.Paths.ConfigHome, "keys"))
			execution.OwnedGitDirectories = append(execution.OwnedGitDirectories, filepath.Join(keyRoot, "shared", ".git"), filepath.Join(keyRoot, "local", ".git"))
		}
		store, err := state.NewFileStore(loaded.Context.Paths.StateDir)
		if err != nil {
			return err
		}
		projects, err := store.ListReadOnly(ctx)
		if err != nil {
			return err
		}
		for _, project := range projects {
			if project.HostPath != "" {
				protected = append(protected, project.HostPath)
			}
		}
		if execution.physicalChanged || execution.snapshot().Agent.State() != "locked" {
			environment := structuredCommandContext(loaded)
			for name, value := range environment {
				if err := config.ValidateNonSecretContent(name, value); err != nil {
					return errors.New("full uninstall context contains protected content")
				}
			}

		} else {
			execution.Yards = execution.Yards[:len(execution.Yards)-1]
			execution.activeYard = nil
		}
		return nil
	}
	for _, name := range names {
		loaded, err := cli.loadInventoryLoaded(name, current)
		if err != nil {
			return fmt.Errorf("inspect full uninstall yard %s: %w", name, err)
		}
		if err := addYard(loaded); err != nil {
			return err
		}
	}

	for _, yard := range installationYards {
		if foreignProjects[yard.Project] {
			continue
		}
		if !knownProjects[yard.Project] {
			if yard.Context == nil {
				return fmt.Errorf("partial yard project %s has no exact persisted context; restore its registration before full removal", yard.Project)
			}
			loaded := config.Loaded{Catalog: current.Catalog, Context: *yard.Context, Environment: maps.Clone(current.Environment)}
			loaded.Context.Paths.RepositoryRoot = cli.options.RepositoryRoot
			loaded.Context.Paths.ConfigDir = current.Context.Paths.ConfigDir
			maps.Copy(loaded.Environment, structuredAdapterContext(loaded.Context))
			loaded.Environment["SRV_POOL"], loaded.Environment["SRV_VOLUME"], loaded.Environment["STORAGE_POOL"] = yard.Pool, yard.Volume, yard.StoragePool
			if err := addYard(loaded); err != nil {
				return err
			}
		}
	}
	// Unknown managed instances must not silently become successful leftovers.
	instances := []ports.InstanceInfo{}
	if !cold {
		instances, err = incusPort.ListInstances(ctx)
	}
	if err != nil {
		return err
	}
	for _, instance := range instances {
		if instance.Config["user.subyard.managed"] == "true" && !knownProjects[instance.Project] && !foreignProjects[instance.Project] {
			return fmt.Errorf("managed project %s has no approved installation ownership; preserve it and repair its registration before full removal", instance.Project)
		}
	}
	profileContext := structuredCommandContext(current)
	profileContext["SUBYARD_DISPATCHER_PATH"] = cli.options.DispatcherPath
	profileContext["YARD_RUNTIME_ROOT"] = installation.RuntimeRoot
	profileContext["ASSUME_YES"] = "0"
	profileContext["SUBYARD_FULL_CLEANUP_READ_ONLY"] = "1"
	ownedProjects := []string{}
	for project := range knownProjects {
		ownedProjects = append(ownedProjects, project)
	}
	projectPayload, _ := json.Marshal(ownedProjects)
	sourcePayload, _ := json.Marshal(ownedSources)
	profileContext["SUBYARD_FULL_CLEANUP_PROJECTS"] = string(projectPayload)
	profileContext["SUBYARD_FULL_CLEANUP_SOURCES"] = string(sourcePayload)
	profileContext["SUBYARD_FULL_CLEANUP_OPERATOR_UID"] = strconv.Itoa(os.Getuid())
	profileContext["SUBYARD_FULL_CLEANUP_OPERATOR_GID"] = strconv.Itoa(os.Getgid())
	if cold {
		profileContext["SUBYARD_FULL_CLEANUP_INCUS_ABSENT"] = "1"
	}
	profileContext["PATH"] = firstNonempty(cli.env["PATH"], "/usr/local/bin:/usr/bin:/bin")
	for _, definition := range current.Catalog.Profiles() {
		for _, name := range definition.FullCleanupEnvironment {
			if value, exists := cli.env[name]; exists {
				if err := config.ValidateNonSecretContent(name, value); err != nil {
					return errors.New("full cleanup override contains protected content")
				}
				profileContext[name] = value
			}
		}
	}
	profilePlans, err := profile.AssessFullCleanup(ctx, cli.options.RepositoryRoot, profileContext)
	if err != nil {
		return err
	}
	for _, profilePlan := range profilePlans {
		if profilePlan.Conditional {
			for _, declaration := range current.Catalog.Profiles() {
				if declaration.Name == profilePlan.Profile {
					execution.ProfileChecks = append(execution.ProfileChecks, uninstall.RecoveryProfileCheck{Profile: declaration.Name, Hook: declaration.FullCleanup, Environment: maps.Clone(profileContext)})
				}
			}
		}
		execution.PreservedRoots = append(execution.PreservedRoots, profilePlan.PreservedRoots...)
		execution.MutableRoots = append(execution.MutableRoots, profilePlan.MutableRoots...)
		if len(profilePlan.Blockers) > 0 {
			return fmt.Errorf("profile %s full cleanup: %s", profilePlan.Profile, strings.Join(profilePlan.Blockers, "; "))
		}
		for _, managedRoot := range profilePlan.ManagedRoots {
			execution.ManagedRoots = append(execution.ManagedRoots, managedRoot)
		}
		for index, artifact := range profilePlan.Artifacts {
			captured, err := uninstall.CaptureArtifact(artifact.Path)
			if err != nil {
				return err
			}
			if captured.Binding != artifact.Binding {
				return domain.ErrPlanStale
			}
			profilePlan.Artifacts[index] = captured
		}
		if len(profilePlan.Services) == 0 && len(profilePlan.Artifacts) > 0 {
			execution.Services = append(execution.Services, fullTeardownService{Scope: "system", Artifacts: profilePlan.Artifacts})
		}
		for _, service := range profilePlan.Services {
			execution.Services = append(execution.Services, fullTeardownService{Scope: service.Scope, Units: service.Units, Artifacts: profilePlan.Artifacts})
		}
		execution.Preserved = append(execution.Preserved, profilePlan.Preserved...)
	}
	for _, yard := range execution.Yards {
		yard.Snapshot.Registered = execution.contexts()
	}
	if len(foreignProjects) > 0 {
		kept := execution.Services[:0]
		for _, service := range execution.Services {
			if slices.Contains(service.Units, "subyard-power-reconcile.service") {
				execution.Preserved = append(execution.Preserved, "shared boot power reconciler for surviving installations")
			} else {
				kept = append(kept, service)
			}
		}
		execution.Services = kept
	}
	relays, err := cli.captureCoreServices(current, execution.RecoveryPlan, []string{"relay"}, 0, uninstall.ProducerEvidence{})
	if err != nil {
		return err
	}
	execution.Services = append(execution.Services, relays...)
	for _, yard := range execution.Yards {
		directory := sshagentruntime.Directory(installation.DataHome, yard.Context.YardName)
		tree, err := uninstall.Capture(directory)
		if err != nil {
			return err
		}
		if len(tree.Entries) > 0 {
			for _, entry := range tree.Entries {
				if entry.Path != "." && !slices.Contains([]string{"manager.lock", "worker.lock", "worker.json", "control.sock", "private.sock"}, entry.Path) {
					return fmt.Errorf("SSH signing directory %s has an unknown artifact; preserve it and repair its owner state", directory)
				}
			}
			tree.Entries = tree.Entries[:1]
			tree.Opaque = true
			tree.Category = "agent"
			execution.Trees = append(execution.Trees, tree)
		}
	}
	protected = append(protected, execution.PreservedRoots...)
	for _, root := range execution.MutableRoots {
		roots = append(roots, root)
	}
	storageTrees, err := cli.captureFullShared(ctx, execution, knownProjects, legacyProjects)
	if err != nil {
		return err
	}
	for _, tree := range storageTrees {
		if tree.Category == "preserved-storage" {
			protected = append(protected, tree.Path)
		} else {
			roots = append(roots, tree.Path)
		}
	}
	for _, yard := range execution.Yards {
		root := yard.Context.Paths.HostBase
		if legacyProjects[yard.Context.IncusProject] && !hostRecorded[root] {
			// Association proves the yard, not who created old host directories.
			// Retain the entire unrecorded host namespace, including backups.
			protected = append(protected, root)
			execution.PreservedRoots = append(execution.PreservedRoots, root)
			execution.Preserved = append(execution.Preserved, "legacy host namespace "+root+" has no installation-created ownership record; its contents are preserved")
			continue
		}
		if !hostRecorded[root] {
			for name, device := range yard.Snapshot.State.Instance.Devices {
				source := device["source"]
				if (strings.HasPrefix(name, "host-") || strings.HasPrefix(name, "yx-")) && device["type"] == "disk" && filepath.Dir(source) == root {
					hostOwned[root] = append(hostOwned[root], filepath.Base(source))
				}
			}
			if _, statErr := os.Lstat(filepath.Join(root, "backups")); statErr == nil {
				return fmt.Errorf("legacy host backup namespace %s has no installation-created ownership record; preserve it and restore its ownership before full removal", filepath.Join(root, "backups"))
			} else if !errors.Is(statErr, os.ErrNotExist) && !errors.Is(statErr, os.ErrPermission) {
				return statErr
			}
		}
	}
	for _, record := range hostRecords {
		roots = append(roots, record.Root)
	}
	roots = uniqueFullRoots(append(roots, installation.CacheRoot, installation.RuntimeRoot))
	for _, root := range roots {
		if _, gitErr := os.Lstat(filepath.Join(root, ".git")); gitErr == nil && !slices.Contains(execution.OwnedGitDirectories, filepath.Join(root, ".git")) {
			execution.Preserved = append(execution.Preserved, "user Git repository "+root)
			continue
		} else if gitErr != nil && !errors.Is(gitErr, os.ErrNotExist) && !errors.Is(gitErr, os.ErrPermission) {
			return gitErr
		}

		preserved := []string{}
		preserveWhole := false
		for _, path := range protected {
			if path == root || strings.HasPrefix(root, path+"/") {
				preserveWhole = true
				break
			}
			if strings.HasPrefix(path, root+"/") {
				relative, _ := filepath.Rel(root, path)
				preserved = append(preserved, relative)
				execution.Preserved = append(execution.Preserved, "user repository or host project "+path)
			}
		}
		if preserveWhole {
			execution.Preserved = append(execution.Preserved, "user repository or host project root "+root)
			continue
		}
		if root == current.Context.Paths.OperatorHome || slices.Contains([]string{filepath.Join(current.Context.Paths.OperatorHome, ".config"), filepath.Join(current.Context.Paths.OperatorHome, ".local"), filepath.Join(current.Context.Paths.OperatorHome, ".ssh"), filepath.Join(current.Context.Paths.OperatorHome, ".cache")}, root) {
			return errors.New("full uninstall refuses the operator home as a managed root")
		}
		exclusions := append([]string{}, preserved...)
		for _, other := range roots {
			if other != root && strings.HasPrefix(other, root+"/") {
				relative, _ := filepath.Rel(root, other)
				exclusions = append(exclusions, relative)
			}
		}
		ownedNamespaces, scoped, scopeErr := fullRootNamespaces(root, installation, execution, current.Catalog.Profiles())
		if scopeErr != nil {
			return scopeErr
		}
		tree := uninstall.Tree{Path: root, Exclusions: exclusions}
		isHost := hostRecorded[root]
		for _, yard := range execution.Yards {
			isHost = isHost || yard.Context.Paths.HostBase == root
		}
		for _, storage := range storageTrees {
			isHost = isHost || storage.Category == "host" && storage.Path == root
		}
		if isHost {
			ownedNamespaces, scoped = hostOwned[root], true
			for _, record := range hostRecords {
				if record.Root == root {
					for _, entry := range record.Paths {
						if entry.Created && entry.Identity != nil {
							identity := *entry.Identity
							identity.Path = entry.Name
							tree.Namespaces = append(tree.Namespaces, identity)
						}
					}
				}
			}
		}
		var storageTree *uninstall.Tree
		for index := range storageTrees {
			if storageTrees[index].Path == root && storageTrees[index].Category == "storage" {
				storageTree = &storageTrees[index]
			}
		}
		if storageTree != nil {
			tree = *storageTree
			tree.Exclusions = exclusions
		} else if scoped {
			captured, captureErr := uninstall.CaptureScoped(root, ownedNamespaces, exclusions)
			captured.Namespaces = tree.Namespaces
			tree, err = captured, captureErr
		} else {
			tree, err = uninstall.CapturePrivateExcluded(root, exclusions)
		}
		if slices.Contains(execution.MutableRoots, root) {
			if len(tree.Entries) > 0 {
				tree.Entries = tree.Entries[:1]
				tree.Opaque = true
				tree.Mutable = true
			}
			tree.Category = "producer"
		}
		tree.Preserved = append(tree.Preserved, preserved...)
		for _, path := range tree.Preserved {
			execution.Preserved = append(execution.Preserved, "foreign content "+filepath.Join(root, path))
		}
		if root == installation.RuntimeRoot {
			tree.Category = "runtime"
		}
		if err != nil {
			return fmt.Errorf("inspect full uninstall root %s: %w", root, err)
		}
		for _, entry := range tree.Entries {
			if filepath.Base(entry.Path) == ".git" && !slices.Contains(execution.OwnedGitDirectories, filepath.Join(root, entry.Path)) {
				return fmt.Errorf("full uninstall root contains a source or config Git repository: %s; preserve and relocate its registration", root)
			}
		}
		execution.Trees = append(execution.Trees, tree)
	}
	execution.SourceAssets, err = uninstall.CaptureAssets(cli.options.RepositoryRoot)
	if err != nil {
		return err
	}
	engine, err := uninstall.Capture(cli.options.DispatcherPath)
	if err != nil {
		return err
	}
	if len(engine.Entries) != 1 {
		return errors.New("full uninstall requires an exact native recovery engine")
	}
	engine.Category = "bin/yard-engine"
	execution.SourceAssets = append(execution.SourceAssets, engine)
	execution.Schema, execution.Stage = 1, uninstall.RecoveryPrepared
	execution.SourceRoot = cli.options.RepositoryRoot
	execution.Assessed = true
	return nil
}

func (execution *fullTeardown) installationBinding() string {
	return operationStateDigest(execution.RecoveryPlan)
}

func uniqueFullRoots(paths []string) []string { sort.Strings(paths); return slices.Compact(paths) }

func (execution *fullTeardown) installationSteps(cli *CLI) []domain.OperationStep {
	steps := execution.sharedSteps()
	for index, check := range execution.ProfileChecks {
		steps = append(steps, domain.OperationStep{ID: fmt.Sprintf("conditional-owner-%d", index), Target: "profile/" + check.Profile, Observed: "private owner evidence requires privileged inspection", Desired: "approved producer ownership verified", Decision: domain.StepApply, Preconditions: []string{"read-only privileged proof after consent and before resource or data removal", "foreign or unknown producer evidence refuses dependent cleanup"}, Verify: "owning hook proves only the approved installation sources", Consequence: "conditionally inspect private ownership metadata for " + check.Profile + " without exposing its contents"})
	}
	steps = append(steps, domain.OperationStep{ID: "retain-owner-coordination", Target: uninstall.OwnerLockPath(execution.Installation.DataHome), Observed: "stable shared account lock", Desired: "preserved", Decision: domain.StepSkip, Preconditions: []string{"operator-owned mode 0600 lock in private shared account namespace"}, Verify: "queued operations retain the same lock inode and recheck their installation generation", Consequence: "preserve the secret-free owner coordination lock so waiting commands cannot bypass serialization"})
	steps = append(steps, domain.OperationStep{ID: "retain-owner-epoch", Target: uninstall.OwnerEpochPath(execution.Installation.DataHome), Observed: "nonsecret installation coordination generation", Desired: "advanced and preserved", Decision: domain.StepApply, Preconditions: []string{"verified cleanup and durable original before/after intent", "atomic epoch publication preserves the live lock inode"}, Verify: "old queued operations and writers refuse the completed installation generation", Consequence: "retain only the nonsecret coordination epoch beside the shared lock; completed uninstall recovery is removed"})
	if len(execution.Yards) > 0 || len(execution.Shared) > 0 {
		steps = append(steps, domain.OperationStep{ID: "retain-host-coordination", Target: "/run/lock/subyard-network", Observed: "shared host network coordination", Desired: "preserved", Decision: domain.StepSkip, Preconditions: []string{"shared OS runtime namespace"}, Verify: "hold the host network barrier through cleanup and verification", Consequence: "preserve the shared host network lock used by other installations"})
	}

	for index, edit := range execution.Startup {
		if len(edit.Blocks) == 0 {
			continue
		}
		steps = append(steps, domain.OperationStep{ID: fmt.Sprintf("startup-%d", index), Target: edit.Before.Path, Observed: "managed blocks present", Desired: "managed blocks absent", Decision: domain.StepApply, Preconditions: []string{"captured operator file identity unchanged"}, Verify: "exact owned blocks absent and other shell content preserved", Consequence: "remove only this installation's shell startup and completion blocks"})
	}
	for index, tree := range execution.Launchers {
		decision := domain.StepApply
		if len(tree.Entries) == 0 {
			decision = domain.StepSkip
		}
		steps = append(steps, domain.OperationStep{ID: fmt.Sprintf("launcher-%d", index), Target: tree.Path, Observed: "captured launcher", Desired: "absent", Decision: decision, Preconditions: []string{"exact owned launcher target and identity unchanged"}, Verify: "approved launcher absent", Consequence: "remove this installation's CLI launcher " + tree.Path})
	}
	for index, service := range execution.Services {
		steps = append(steps, domain.OperationStep{ID: fmt.Sprintf("host-producers-%d", index), Target: service.Scope + "/" + strings.Join(service.Units, ","), Observed: "installed", Desired: "stopped and removed", Decision: domain.StepApply,
			Preconditions: []string{"captured matching unit, enablement and helper identities unchanged"}, Verify: "units inactive and processes absent before removing artifacts", Consequence: "synchronously stop and remove " + strings.Join(service.Units, ", ") + " before their data"})
	}
	for _, yard := range execution.Yards {
		loaded := cli.teardownLoaded(yard)
		execution.selectYard(loaded)
		for _, step := range execution.steps(loaded.Context) {
			step.ID = "yard-" + loaded.Context.YardName + "-" + step.ID
			steps = append(steps, step)
		}
	}
	for index, tree := range execution.Trees {
		decision := domain.StepApply
		if len(tree.Entries) == 0 {
			decision = domain.StepSkip
		}
		desired := "absent"
		scope := []string{"captured installation ownership and filesystem identities remain unchanged", "matching producers synchronously stopped", "dependent yard cleanup verified"}
		if tree.Scoped {
			desired = "approved owner namespaces absent; unrecorded contents preserved"
			for namespaceIndex, namespace := range tree.OwnedPaths {
				if namespace == "" {
					namespace = "approved root"
				}
				if len(tree.OwnedPaths) < 60 {
					scope = append(scope, "owned namespace: "+namespace)
				} else {
					steps = append(steps, domain.OperationStep{ID: fmt.Sprintf("installation-namespace-%d-%d", index, namespaceIndex), Target: filepath.Join(tree.Path, namespace), Observed: "installer-owned namespace", Desired: "absent", Decision: domain.StepApply, Preconditions: []string{"native installer ownership and captured identity unchanged"}, Verify: "approved namespace absent", Consequence: "remove only this exact installation-owned namespace"})
				}
			}
		}
		if tree.Opaque {
			scope = append(scope, "conditionally refine only the approved private owner namespaces after consent")
		}
		steps = append(steps, domain.OperationStep{ID: fmt.Sprintf("installation-root-%d", index), Target: tree.Path, Observed: map[bool]string{true: "present", false: "absent"}[len(tree.Entries) != 0], Desired: desired, Decision: decision,
			Preconditions: scope,
			Verify:        "approved installation root absent", Consequence: "permanently remove " + tree.Path + " including owned credentials, host secrets, agent sessions, logs and backups"})
	}
	steps = append(steps, domain.OperationStep{ID: "preserve-foreign", Target: "host/shared-resources", Observed: "shared", Desired: "preserved", Decision: domain.StepSkip,
		Preconditions: []string{"shared or foreign ownership"}, Verify: "foreign resources remain", Consequence: "preserve remote hosts, host projects, user Git repositories, shared packages and account lingering"})
	return steps
}
