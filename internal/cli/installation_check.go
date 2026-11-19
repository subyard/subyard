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

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/adapters/transport"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/state"
)

func (cli *CLI) checkUsage() {
	fmt.Fprintf(cli.options.Stdout, `Usage: %[1]s check [--json]
       %[1]s [-Y NAME] check --host
Inspect the installed release, configuration, host and local state without changing them.
Yard substrate is not inspected; run %[1]s -Y NAME status and %[1]s -Y NAME security.
--host runs the host preflight for the selected yard.
`, cli.options.Program)
}

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
			cli.checkUsage()
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
	// Yards load with the command environment, as other commands see them.
	values := maps.Clone(cli.baseEnv)
	for _, key := range []string{"SUBYARD_YARD", "YARD_NAME", "ACCESS_KIND", "OWNER_ENDPOINT", "OWNER_YARD_NAME"} {
		delete(values, key)
	}
	if values["SUBYARD_OPERATOR_HOME"] == "" {
		values["SUBYARD_OPERATOR_HOME"] = values["HOME"]
	}
	trust := cli.sshTrust(installationDataHome(values), false)
	trust.ReadOnly = true
	defer trust.Close()
	report, err := application.InspectConformance(cli.inspectInstallation(transport.WithSSHTrust(ctx, trust.Options), values))
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
func (cli *CLI) inspectInstallation(ctx context.Context, values map[string]string) []application.InspectionSection {
	var configuration, release, host, local []domain.Inspected
	home, err := config.ResolveConfigHome(values["SUBYARD_OPERATOR_HOME"], values)
	if err != nil {
		configuration = domain.InspectionRead("root.read", "config", err)
		release = []domain.Inspected{{Key: "config", State: domain.InspectedDeferred, Resource: "release", Message: "deferred by config.root.read"}}
	}
	var names []string
	var loaded []config.Loaded
	if home != "" {
		names, err = config.LocalBatchYardNames(home)
		configuration = append(configuration, domain.InspectionRead("registrations.read", "config", err)...)
		for _, name := range names {
			yard, err := config.Load(config.LoadOptions{Catalog: &cli.catalog, RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: values["SUBYARD_OPERATOR_HOME"], YardName: name, Environment: values})
			if err != nil {
				configuration = append(configuration, domain.Inspected{Key: "schema." + domain.InspectionName(name), State: domain.InspectedBlocked, Resource: name,
					Message: (configProspectiveError{home, err}).Error(), Next: configValidateRetry(home, name, err)})
			} else if yard.Context.AccessKind == domain.AccessLocal {
				loaded = append(loaded, yard)
			}
		}
		configuration = append(configuration, inspectConfigurationSources(home, loaded)...)
		release = cli.inspectInstalledRelease(ctx)
	}
	host = cli.inspectHost(ctx, loaded)
	local = inspectLocalState(ctx, home, installationDataHome(values), names, loaded)
	return []application.InspectionSection{{ID: "config", Items: configuration}, {ID: "release", Items: release}, {ID: "host", Items: host}, {ID: "state", Items: local}}
}

func inspectConfigurationSources(home string, loaded []config.Loaded) []domain.Inspected {
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
	}
	if len(loaded) > 0 {
		paths, err := legacyConfigInputs(loaded[0])
		result = append(result, domain.InspectionRead("sources.legacy.read", "config", err)...)
		for _, path := range paths {
			relative, _ := filepath.Rel(home, path)
			result = append(result, domain.Inspected{Key: "sources.legacy." + domain.InspectionName(relative), State: domain.InspectedNotice, Resource: path,
				Message: "legacy configuration input awaits review or import", Next: "yard config status"})
		}
	}
	permissions := map[string]int{}
	for _, refusal := range inspectManagedConfigTree(home) {
		relative, state, next := "root", domain.InspectedUnknown, ""
		var permission *configPermissionError
		var path *os.PathError
		if errors.As(refusal, &permission) {
			relative, _ = filepath.Rel(home, permission.path)
			state, next = domain.InspectedBlocked, permission.repair()
			if next != "" {
				state = domain.InspectedDrift
			}
		} else if errors.As(refusal, &path) {
			relative, _ = filepath.Rel(home, path.Path)
		}
		key := "permissions." + domain.InspectionName(relative)
		if index, found := permissions[key]; found {
			// A directory refused before and after its walk is one resource.
			result[index].Message += "; " + refusal.Error()
			continue
		}
		permissions[key] = len(result)
		result = append(result, domain.Inspected{Key: key, State: state, Resource: filepath.Join(home, relative), Message: refusal.Error(), Next: next})
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

// inspectInstalledRelease is migrate --check's inspection without its output.
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
	return runtime.InspectInstalled(ctx, options.ConfigHome)
}

// inspectHost keys the host preflight of the default yard, or of the first
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
	result := (hostruntime.HostCheck{Yard: base.Context, Yards: yards, Environment: base.Environment, Incus: incus}).Observe(ctx, base.Context.YardName != "")
	if base.Context.YardName == "" {
		return result
	}
	credentials, err := cli.credentialRuntimeWithStreams(base, nil, io.Discard, io.Discard)
	var missing []string
	if err == nil {
		missing, err = credentials.InspectInitialization()
	}
	result = append(result, domain.InspectionRead("credentials.read", "credentials", err)...)
	if len(missing) != 0 {
		result = append(result, domain.Inspected{Key: "credentials.initialization", State: domain.InspectedDrift, Resource: "credentials", Installed: "incomplete", Desired: "initialized",
			Message: "credential store initialization is incomplete: " + strings.Join(missing, ", ") + " absent or not of the expected type", Next: "yard init"})
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
	for _, path := range journals {
		relative, _ := filepath.Rel(data, path)
		pending("journals.cleanup."+domain.InspectionName(strings.TrimSuffix(relative, "/journal.json")), path, "hard cleanup recovery is pending", "resume the interrupted yard cleanup --hard command")
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
