package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/adapters/transport"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/state"
)

func (cli *CLI) checkUsage() error {
	_, err := fmt.Fprintf(cli.options.Stdout, `Usage: %[1]s check [--json]
       %[1]s [-Y NAME] check --host
Inspect the installed release, configuration, host and local state without changing them.
Yard substrate is not inspected; run %[1]s -Y NAME status and %[1]s -Y NAME security.
--host runs the host preflight for the selected yard.
`, cli.options.Program)
	return err
}

// installationLockWait bounds the wait for a configuration writer.
const installationLockWait = 2 * time.Second

// runInstallationCheck is the read-only installation check. It runs before
// owner recovery, audit and the release gate, with read-only SSH trust.
func (cli *CLI) runInstallationCheck(ctx context.Context, explicit bool, arguments []string) int {
	structured := false
	for _, argument := range arguments {
		switch argument {
		case "--json":
			structured = true
		case "-y", "--yes":
		case "-h", "--help":
			if err := cli.checkUsage(); err != nil {
				cli.errorf("check: %v", err)
				return 2
			}
			return 0
		default:
			cli.errorf("unknown check option %q", argument)
			return 2
		}
	}
	if explicit {
		cli.errorf("check inspects the whole installation; -Y NAME applies only to check --host")
		return 2
	}
	// Configuration is judged from its persisted sources, as config validate
	// does; host and state readers see the command environment.
	sources := cli.configSourceEnvironment()
	command := maps.Clone(cli.baseEnv)
	for _, key := range []string{"SUBYARD_YARD", "YARD_NAME", "ACCESS_KIND", "OWNER_ENDPOINT", "OWNER_YARD_NAME"} {
		delete(command, key)
	}
	if command["SUBYARD_OPERATOR_HOME"] == "" {
		command["SUBYARD_OPERATOR_HOME"] = command["HOME"]
	}
	trust := cli.sshTrust(installationDataHome(sources), false)
	trust.ReadOnly = true
	defer trust.Close()
	report, err := application.InspectConformance(cli.inspectInstallation(transport.WithSSHTrust(ctx, trust.Options), sources, command))
	if err == nil {
		if structured {
			err = json.NewEncoder(cli.options.Stdout).Encode(report)
		} else {
			err = report.WriteText(cli.options.Stdout)
		}
	}
	if err != nil {
		cli.errorf("check: %v", err)
		return 2
	}
	return report.ExitCode()
}

func installationDataHome(values map[string]string) string {
	return firstNonempty(values["SUBYARD_HOME"], filepath.Join(values["SUBYARD_OPERATOR_HOME"], ".subyard"))
}

// inspectInstallation returns the fixed config, release, host and state
// sections. A section skips only the findings that depend on a failed read.
// Yard configuration and the release inspection are read under a shared
// configuration lock that the check never waits for beyond installationLockWait.
func (cli *CLI) inspectInstallation(ctx context.Context, sources, command map[string]string) []application.InspectionSection {
	var configuration, release, host, local []domain.Inspected
	data := installationDataHome(sources)
	home, err := config.ResolveConfigHome(sources["SUBYARD_OPERATOR_HOME"], sources)
	var unlock func()
	// Readers of loaded yards are deferred when the configuration is unavailable.
	deferred := func(cause string) {
		message := "deferred by config." + cause
		release = []domain.Inspected{{Key: "config", State: domain.InspectedDeferred, Resource: "release", Message: message}}
		host = []domain.Inspected{{Key: "credentials.config", State: domain.InspectedDeferred, Resource: "credentials", Message: message}}
		local = []domain.Inspected{{Key: "stores.projects.config", State: domain.InspectedDeferred, Resource: "projects", Message: message}}
	}
	if err != nil {
		configuration = domain.InspectionRead("root.read", "config", err)
		deferred("root.read")
	} else {
		lockCtx, cancel := context.WithTimeout(ctx, installationLockWait)
		unlock, err = config.ObserveRoot(lockCtx, home)
		cancel()
		if errors.Is(err, config.ErrConfigRootBusy) {
			configuration = append(configuration, domain.Inspected{Key: "root.lock", State: domain.InspectedBlocked, Resource: home,
				Message: "a configuration writer holds " + home, Next: "retry yard check after the configuration change finishes"})
		} else {
			configuration = append(configuration, domain.InspectionRead("root.lock", home, err)...)
		}
		if err != nil {
			unlock = nil
			deferred("root.lock")
		}
	}
	var names []string
	var loaded, effective []config.Loaded
	if home != "" {
		names, err = config.LocalBatchYardNames(home)
		configuration = append(configuration, domain.InspectionRead("registrations.read", "config", err)...)
	}
	if unlock != nil {
		for _, name := range names {
			yard, err := cli.loadInstalledYard(name, sources)
			if err != nil {
				configuration = append(configuration, domain.Inspected{Key: "schema." + domain.InspectionName(name), State: domain.InspectedBlocked, Resource: name,
					Message: (configProspectiveError{home, err}).Error(), Next: configValidateRetry(home, name, err)})
				continue
			}
			if yard.Context.AccessKind != domain.AccessLocal {
				continue
			}
			loaded = append(loaded, yard)
			if yard, err = cli.loadInstalledYard(name, command); err != nil {
				host = append(host, domain.Inspected{Key: "environment." + domain.InspectionName(name), State: domain.InspectedUnknown, Resource: name,
					Message: "yard " + name + " is unavailable with the command environment: " + err.Error()})
				continue
			}
			effective = append(effective, yard)
		}
		release = cli.inspectInstalledRelease(ctx)
		unlock()
	}
	if home != "" {
		configuration = append(configuration, inspectConfigurationSources(home, data, loaded)...)
	}
	host = append(host, cli.inspectHost(ctx, effective)...)
	local = append(local, inspectLocalState(ctx, home, data, names, effective)...)
	return []application.InspectionSection{{ID: "config", Items: configuration}, {ID: "release", Items: release}, {ID: "host", Items: host}, {ID: "state", Items: local}}
}

// loadInstalledYard loads a registered yard under the check's held lock.
func (cli *CLI) loadInstalledYard(name string, environment map[string]string) (config.Loaded, error) {
	return config.Load(config.LoadOptions{Catalog: &cli.catalog, RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: environment["SUBYARD_OPERATOR_HOME"],
		YardName: name, Environment: environment, ConfigLocked: true})
}

func inspectConfigurationSources(home, data string, loaded []config.Loaded) []domain.Inspected {
	var result []domain.Inspected
	seen := map[string]bool{}
	for _, yard := range loaded {
		for _, assignment := range append(yard.IgnoredAssignments(), yard.RetiredAssignments()...) {
			// The setting in its source is the resource; removing another line keeps the key.
			relative, _ := filepath.Rel(home, assignment.Path)
			key := "sources.ignored." + domain.InspectionName(assignment.Scope+"."+relative+"."+assignment.Name)
			if !seen[key] {
				seen[key] = true
				result = append(result, domain.Inspected{Key: key, State: domain.InspectedNotice, Resource: assignment.Path, Message: assignment.String(), Next: assignment.Repair()})
			}
		}
		for _, assignment := range yard.DeprecatedAssignments() {
			if assignment.Command() {
				continue
			}
			relative, _ := filepath.Rel(home, assignment.Path)
			key := "sources.deprecated." + domain.InspectionName(assignment.Scope+"."+relative+"."+assignment.Name)
			if !seen[key] {
				seen[key] = true
				result = append(result, domain.Inspected{Key: key, State: domain.InspectedNotice, Resource: assignment.Path, Message: assignment.String(), Next: assignment.Repair()})
			}
		}
	}
	legacy, err := legacyConfigInputsAt(home, data)
	result = append(result, domain.InspectionRead("sources.legacy.read", home, err)...)
	for _, relative := range legacy {
		resource := relative
		if !strings.HasPrefix(relative, "legacy-data:") {
			resource = filepath.Join(home, relative)
		}
		result = append(result, domain.Inspected{Key: "sources.legacy." + domain.InspectionName(relative), State: domain.InspectedNotice, Resource: resource,
			Message: "legacy configuration input awaits review or import", Next: "yard config status"})
	}
	permissions := map[string]int{}
	for _, refusal := range inspectManagedConfigTree(home) {
		key, state, resource, next := "permissions.root", domain.InspectedBlocked, home, ""
		var permission *configPermissionError
		var unread *configReadError
		var path *os.PathError
		switch {
		case errors.As(refusal, &permission):
			key, resource = configPathKey(home, permission.path), permission.path
			if next = permission.repair(); next != "" {
				state = domain.InspectedDrift
			}
		case errors.As(refusal, &unread):
			// A mode repair cannot restore a failed read.
			key, state, resource = configPathKey(home, unread.path)+".read", domain.InspectedUnknown, unread.path
		case errors.As(refusal, &path):
			key, state = "permissions.root.read", domain.InspectedUnknown
		}
		if index, found := permissions[key]; found {
			result[index].Message += "; " + refusal.Error()
			continue
		}
		permissions[key] = len(result)
		result = append(result, domain.Inspected{Key: key, State: state, Resource: resource, Message: refusal.Error(), Next: next})
	}
	for _, conflict := range []struct {
		key  string
		err  error
		next string
	}{
		{"conflicts.targets", config.NativeTargetCollisions(loaded, nil, ""), ""},
		{"conflicts.listeners", config.HostListenerCollisions(loaded), "set distinct host listener ports at yard scope"},
	} {
		if conflict.err != nil {
			result = append(result, domain.Inspected{Key: conflict.key, State: domain.InspectedBlocked, Resource: "config", Message: conflict.err.Error(), Next: conflict.next})
		}
	}
	return result
}

// configPathKey keys a managed config path by its name below home.
func configPathKey(home, path string) string {
	relative, err := filepath.Rel(home, path)
	if err != nil || relative == "." {
		return "permissions.root"
	}
	return "permissions." + domain.InspectionName(relative)
}

// inspectInstalledRelease is migrate --check's inspection without its output,
// bounded like the status release readiness.
func (cli *CLI) inspectInstalledRelease(ctx context.Context) []domain.Inspected {
	options, available, err := cli.mutationGateReleaseOptions()
	if err == nil && !available {
		err = errors.New("installed release paths are unavailable")
	}
	if err != nil {
		return []domain.Inspected{{Key: "metadata.read", State: domain.InspectedUnknown, Resource: "release", Message: err.Error(), Next: "yard migrate --check"}}
	}
	configuration := cli.releaseRuntimeConfig(releaseruntime.RuntimeContext{DataHome: options.DataHome, ConfigHome: options.ConfigHome, RuntimeRoot: options.RuntimeRoot})
	configuration.Stdout, configuration.Stderr, configuration.Progress = io.Discard, io.Discard, io.Discard
	runtime := releaseruntime.New(configuration)
	defer runtime.Close()
	bounded, cancel := context.WithTimeout(ctx, profile.ReadinessTimeout)
	defer cancel()
	result := runtime.InspectInstalled(bounded, options.ConfigHome)
	if bounded.Err() != nil && ctx.Err() == nil {
		return []domain.Inspected{{Key: "read", State: domain.InspectedUnknown, Resource: "release",
			Message: fmt.Sprintf("installed release inspection did not finish within %s", profile.ReadinessTimeout), Next: "yard migrate --check"}}
	}
	return result
}

// inspectHost keys the host observation of the default yard, or of the first
// local yard, and that yard's credential store initialization.
func (cli *CLI) inspectHost(ctx context.Context, loaded []config.Loaded) []domain.Inspected {
	var base config.Loaded
	var yards []domain.Context
	for _, yard := range loaded {
		if base.Context.YardName == "" || yard.Context.YardName == "default" {
			base = yard
		}
		yards = append(yards, yard.Context)
	}
	incus, _ := cli.statusPorts()
	bounded, cancel := context.WithTimeout(ctx, profile.ReadinessTimeout)
	defer cancel()
	result := (hostruntime.HostCheck{Yard: base.Context, Yards: yards, Environment: base.Environment, Incus: incus}).Observe(bounded)
	if base.Context.YardName == "" {
		return result
	}
	credentials, err := cli.credentialRuntimeWithStreams(base, nil, io.Discard, io.Discard)
	var missing []string
	partial := ""
	if err == nil {
		missing, err = credentials.InspectInitialization()
	}
	if err == nil && len(missing) != 0 {
		partial, err = credentials.PartialIdentity()
	}
	result = append(result, domain.InspectionRead("credentials.read", "credentials", err)...)
	if err == nil && len(missing) != 0 {
		finding := domain.Inspected{Key: "credentials.initialization", State: domain.InspectedDrift, Resource: "credentials", Installed: "incomplete", Desired: "initialized",
			Message: "credential store initialization is incomplete: " + strings.Join(missing, ", ") + " absent or not of the expected type", Next: "yard init"}
		if partial != "" {
			// yard init refuses to replace an existing identity.
			finding.State, finding.Resource, finding.Next = domain.InspectedBlocked, partial, ""
			finding.Message += "; yard init refuses the existing partial identity " + partial + ": restore the complete identity from a backup"
		}
		result = append(result, finding)
	}
	return result
}

// inspectLocalState reads the recovery journals that block later commands,
// duplicate yard registrations and every local yard's project store.
func inspectLocalState(ctx context.Context, home, data string, names []string, loaded []config.Loaded) []domain.Inspected {
	var result []domain.Inspected
	pending := func(key, resource, message, next string) {
		result = append(result, domain.Inspected{Key: key, State: domain.InspectedBlocked, Resource: resource, Message: message, Next: next})
	}
	if home != "" {
		_, renaming, err := configsync.AssessHostIDRenameRecovery(home)
		result = append(result, domain.InspectionRead("journals.host-rename.read", "config", err)...)
		if renaming {
			pending("journals.host-rename", home, "host identity rename recovery is pending", "retry the interrupted yard host rename command")
		}
	}
	if !filepath.IsAbs(data) {
		return append(result, domain.Inspected{Key: "data.read", State: domain.InspectedUnknown, Resource: data, Message: "the data root is not an absolute path"})
	}
	journals, err := (ownerinventory.Connections{Root: filepath.Join(data, "owner-inventory")}).InspectPendingRecovery()
	result = append(result, domain.InspectionRead("journals.owner-inventory.read", "owner-inventory", err)...)
	for _, name := range journals {
		pending("journals.owner-inventory."+domain.InspectionName(name), filepath.Join(data, "owner-inventory", name), "owner inventory recovery is pending", "retry the interrupted owner operation")
	}
	journals, err = inspectHardCleanupJournals(data)
	result = append(result, domain.InspectionRead("journals.cleanup.read", "cleanup", err)...)
	for _, name := range journals {
		pending("journals.cleanup."+domain.InspectionName(name), filepath.Join(data, "cleanup", name, "journal.json"), "hard cleanup recovery is pending", "resume the interrupted yard cleanup --hard command")
	}
	for _, name := range names {
		if name == "default" || home == "" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(home, "yards", name, "config.env")); err != nil {
			continue
		}
		repair, err := config.PlanYardRegistrationRepair(home, name)
		result = append(result, domain.InspectionRead("registrations."+domain.InspectionName(name)+".read", name, err)...)
		if err == nil && repair.Changed() {
			result = append(result, domain.Inspected{Key: "registrations." + domain.InspectionName(name), State: domain.InspectedDrift, Resource: repair.FlatPath,
				Installed: "flat and nested", Desired: "nested", Message: "a flat registration shadows the nested registration", Next: "yard config repair-registration " + name})
		}
	}
	for _, yard := range loaded {
		store, err := state.NewFileStore(yard.Context.Paths.StateDir)
		if err == nil {
			_, err = store.ListReadOnly(ctx)
		}
		result = append(result, domain.InspectionRead("stores.projects."+domain.InspectionName(yard.Context.YardName), yard.Context.Paths.StateDir, err)...)
	}
	return result
}
