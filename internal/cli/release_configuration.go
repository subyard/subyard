package cli

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// activeReleaseRepair is the only installed-versus-desired decision. The
// request identifies the operation; the journal and current link prove repair.
func activeReleaseRepair(request releasetransition.ProcessRequest) bool {
	if request.Mode != releasetransition.ProcessInspect && request.Mode != releasetransition.ProcessConverge {
		return false
	}
	links, err := releasetransition.NewRuntimeLinkStore(request.RuntimeRoot)
	if err != nil {
		return false
	}
	active, err := links.Observe()
	if err != nil || active.Active == "" || active.Active != request.Target {
		return false
	}
	store, err := releasetransition.NewPOSIXV2Store(request.ConfigHome)
	if err != nil {
		return false
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return false
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	return err == nil && journal.Goal.Target == active.Active &&
		(journal.Checkpoint == releasetransition.JournalComplete ||
			journal.Releases.From == journal.Goal.Target && journal.ArtifactDigest == request.ArtifactDigest && journal.RegistryDigest == request.RegistryDigest)
}

type releaseConfiguration struct {
	request releasetransition.ProcessRequest
	repair  bool
	mutex   sync.Mutex
	yards   map[string]releaseYardConfiguration
}

type releaseYardConfiguration struct {
	loaded config.Loaded
	err    error
}

// Scope and configuration view are separate contracts. Completed readiness is
// release-wide even without a repair request; source work retains its approved
// selected scope until its checkpoint is published.
func (source *releaseConfiguration) materializedScope(cli *CLI, releases releasetransition.ReleasePair,
	home string, goal releasetransition.Goal, artifact, registry releasetransition.Fingerprint, sourceIngress bool,
) (bool, error) {
	if source != nil && source.repair {
		return true, nil
	}
	store, err := releasetransition.NewPOSIXV2Store(home)
	if err != nil {
		return false, err
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return false, err
	}
	var journal releasetransition.JournalRecord
	if snapshot.Exists {
		journal, err = releasetransition.ParseJournal(snapshot.Payload)
		if err != nil {
			return false, err
		}
	}
	matches := snapshot.Exists && journal.Goal == goal && journal.ArtifactDigest == artifact &&
		(registry == "" || journal.RegistryDigest == registry)
	if matches && journal.Checkpoint == releasetransition.JournalComplete &&
		(journal.Releases.From != journal.Releases.Target || journal.SourceIngress != nil) {
		pending, err := releaseCheckpointPublicationPending(cli.options.RepositoryRoot, store)
		if err != nil || pending {
			return false, err
		}
	}
	if sourceIngress && !matches || matches && journal.SourceIngress != nil && journal.Checkpoint != releasetransition.JournalComplete {
		return false, nil
	}
	if releases.From != releases.Target {
		if matches && journal.Checkpoint != releasetransition.JournalComplete && len(journal.Steps) != 0 {
			return false, nil
		}
		return releaseSourceMigrationsComplete(cli.options.RepositoryRoot, store)
	}
	return !snapshot.Exists || matches && (journal.Checkpoint == releasetransition.JournalComplete ||
		journal.Releases.From == journal.Releases.Target), nil
}

func (source *releaseConfiguration) load(ctx context.Context, cli *CLI, yard, home string, files ...*config.ProspectiveFiles) (config.Loaded, error) {
	if !source.repair {
		return cli.loadReleaseTransitionContext(yard, home, nil, files...)
	}
	source.mutex.Lock()
	defer source.mutex.Unlock()
	key := home + "/" + yard
	entry, exists := source.yards[key]
	if !exists {
		entry.loaded, entry.err = cli.resolveInstalledReleaseContext(ctx, yard, home)
		if entry.err == nil && entry.loaded.Context.AccessKind != domain.AccessRemote {
			var target configTarget
			target, _, entry.err = cli.installedReleaseTarget(ctx, configTarget{Name: yard, Loaded: entry.loaded})
			entry.loaded = target.Loaded
		}
		source.yards[key] = entry
	}
	loaded := entry.loaded
	loaded.Environment = maps.Clone(loaded.Environment)
	return loaded, entry.err
}

// installedReleaseTarget is shared by activation and pending-init detection.
// Recorded integration absence stays absent. Unrecorded legacy/core inputs keep
// ordinary desired values; protected core receipts override those when known.
func (cli *CLI) installedReleaseTarget(ctx context.Context, target configTarget) (configTarget, bool, error) {
	installed, baseline, appliedErr := cli.appliedIntegrationTargetBaseline(ctx, target)
	var guest map[string]string
	var guestErr error
	observed := installed
	if appliedErr != nil {
		observed = target
	}
	if observer, ok := profileActivationPlatform(cli, observed).(interface {
		InstalledReleaseSettings(context.Context) (map[string]string, error)
	}); ok {
		guest, guestErr = observer.InstalledReleaseSettings(ctx)
	}
	if appliedErr != nil {
		appliedErr = fmt.Errorf("yard %s: inspect installed configuration: %w", target.Name, appliedErr)
	}
	if guestErr != nil {
		guestErr = fmt.Errorf("yard %s: inspect installed guest configuration: %w", target.Name, guestErr)
	}
	if err := domain.CollectRefusals(appliedErr, guestErr); err != nil {
		return target, baseline, err
	}
	installed.Loaded.Environment = maps.Clone(installed.Loaded.Environment)
	for _, name := range []string{"DEV_USER", "DEV_UID", "ENVIRONMENT_PROFILES", "ALLOWS_CODING_TOOLS", "WEB_PREVIEW_HOST_PORT", "CCUSAGE_PROVISION", "CCUSAGE_VERSION", "CCUSAGE_SHA256_AMD64", "CCUSAGE_SHA256_ARM64"} {
		if _, known := installed.Loaded.Environment[name]; !known {
			if value, present := target.Loaded.Environment[name]; present {
				installed.Loaded.Environment[name] = value
			}
		}
	}
	maps.Copy(installed.Loaded.Environment, guest)
	if installed.installed == nil {
		installed.installed = &installedConfigurationEvidence{known: map[string]bool{}}
	}
	for name := range guest {
		installed.installed.known[name] = true
	}
	if value, known := installed.Loaded.Environment["DEV_USER"]; known {
		installed.Loaded.Context.DevUser = value
	}
	if value, known := installed.Loaded.Environment["DEV_UID"]; known {
		installed.Loaded.Context.DevUID, _ = strconv.Atoi(value)
	}
	if value, known := installed.Loaded.Environment["ALLOWS_CODING_TOOLS"]; known {
		installed.Loaded.Integrations.AllowsCodingTools = value != "false"
	}
	if value, known := installed.Loaded.Environment["ENVIRONMENT_PROFILES"]; known {
		selected := strings.Fields(value)
		installed.Loaded.EnvironmentProfiles = config.EnvironmentProfileSelection{Present: true, Requested: selected, Effective: selected}
	}
	// Effective traces belong to this context too. Pending authoring history
	// remains in the separate desired context, never in an activation input.
	installed.Loaded.Settings = maps.Clone(installed.Loaded.Settings)
	if installed.Loaded.Settings == nil {
		installed.Loaded.Settings = map[string]config.SettingTrace{}
	}
	for name, trace := range installed.Loaded.Settings {
		if trace.Application == config.SettingYardInit {
			delete(installed.Loaded.Settings, name)
		}
	}
	for name, value := range installed.Loaded.Environment {
		definition, known := installed.Loaded.Catalog.LookupSetting(name)
		if known && definition.Application == config.SettingYardInit {
			installed.Loaded.Settings[name] = config.SettingTrace{Name: name, EffectiveValue: value, Kind: definition.Kind, Type: definition.Type, Application: definition.Application, Owner: definition.Owner,
				Aliases: definition.Aliases, Scopes: definition.Scopes, Syncable: definition.Syncable, Merge: definition.Merge, Sensitive: definition.Sensitive,
				Resolutions: []config.SettingResolution{{Scope: "release", Role: "activation context", Value: value, Status: "effective"}}}
		}
	}
	return installed, baseline, nil
}

func (source *releaseConfiguration) runtime(runtime reconcileruntime.Runtime) reconcileruntime.Runtime {
	if source != nil && source.repair {
		return runtime.WithInstalledConfiguration()
	}
	return runtime
}

// Every registered activation owner exposes the same constructor-supplied
// context source, including owners that do not currently need yard settings.
func (r *profileRuntimeActivationReconciler) activationConfiguration() *CLI     { return r.cli }
func (r *materializedConfigActivationReconciler) activationConfiguration() *CLI { return r.cli }
func (r *installationActivationReconciler) activationConfiguration() *CLI       { return r.cli }
func (r *activationStageReconciler) activationConfiguration() *CLI              { return r.configuration }
func (r *routeConsumerActivationReconciler) activationConfiguration() *CLI      { return r.configuration }
