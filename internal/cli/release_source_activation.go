package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

type sourceActivationDraft struct {
	cli       *CLI
	home      string
	view      releasetransition.V2SettingsSnapshotView
	inputs    *config.ProspectiveFiles
	overrides map[string]string
	names     []string
}

func (cli *CLI) prepareSourceActivationDraft(home string, view releasetransition.V2SourceActivationView) (*sourceActivationDraft, error) {
	if view.Settings == nil || !filepath.IsAbs(home) {
		return nil, errors.New("prospective activation settings are unavailable")
	}
	draft := &sourceActivationDraft{cli: cli, home: home, view: view.Settings, overrides: map[string]string{}}
	var paths []string
	if inventory, ok := view.Settings.(releasetransition.V2PreparedInputPaths); ok {
		var err error
		paths, err = inventory.PreparedInputPaths()
		if err != nil {
			return nil, err
		}
	}
	names, err := view.Settings.ListYards()
	if err != nil {
		return nil, err
	}
	actual, err := config.YardNames(home)
	if err != nil {
		return nil, err
	}
	names = append(names, actual...)
	names = append(names, "default")
	slices.Sort(names)
	names = slices.Compact(names)
	draft.names = names
	for _, name := range names {
		if !domain.SafeName(name) {
			return nil, errors.New("prospective yard name is invalid")
		}
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) || !strings.HasPrefix(path, home+string(filepath.Separator)) {
			return nil, errors.New("prospective activation input path escapes configuration home")
		}
		// Preserve logical layer presence without copying or reading its bytes.
		draft.overrides[path] = path
	}
	draft.inputs = &config.ProspectiveFiles{Root: home, Yards: names, ReadSnapshot: view.Settings.ReadSnapshot, ReadPreparedInput: draft.readPreparedInput}
	return draft, nil
}
func (draft *sourceActivationDraft) readPreparedInput(path string) (config.PersistentFileSnapshot, bool, error) {
	if reader, ok := draft.view.(releasetransition.V2PreparedInputReader); ok {
		snapshot, matched, err := reader.ReadPreparedInput(path)
		if matched || err != nil {
			return snapshot, matched, err
		}
	}
	// Older prospective views cover the whole settings root. Prepared views
	// explicitly leave uncaptured inputs on the native reader.
	if _, ok := draft.view.(releasetransition.V2PreparedInputReader); !ok {
		snapshot, err := draft.view.ReadSnapshot(path)
		return snapshot, true, err
	}
	return config.PersistentFileSnapshot{}, false, nil
}
func (draft *sourceActivationDraft) readAsset(asset config.MaterializedAsset) ([]byte, error) {
	return draft.inputs.ReadAsset(asset)
}
func (draft *sourceActivationDraft) load(name string) (config.Loaded, error) {
	environment := draft.cli.freshMigrationEnvironment(draft.cli.baseEnv, draft.cli.options.RepositoryRoot)
	environment["SUBYARD_CONFIG_HOME"] = draft.home
	home := environment["SUBYARD_OPERATOR_HOME"]
	if home == "" {
		home = environment["HOME"]
	}
	return config.Load(config.LoadOptions{Catalog: &draft.cli.catalog, RepositoryRoot: draft.cli.options.RepositoryRoot, OperatorHome: home, YardName: name, Environment: environment, ProspectiveFiles: draft.inputs, FileOverrides: draft.overrides})
}

// Desired integration identity belongs to the loaded declarations, independently
// of the platform that supplies native observation and reconciliation.
func (cli *CLI) materializedIntegrationScope(loaded config.Loaded, reader func(config.MaterializedAsset) ([]byte, error)) (string, []string, error) {
	if !loaded.Integrations.AllowsCodingTools {
		return "", nil, nil
	}
	runtime := reconcileruntime.Runtime{RepositoryRoot: cli.options.RepositoryRoot, Environment: environmentList(loaded.Environment, nil), Yard: loaded.Context, Integrations: loaded.Catalog.Integrations(), SourceReader: reader}
	return runtime.IntegrationScope()
}

func (reconciler *materializedConfigActivationReconciler) PrepareSourceActivation(ctx context.Context, releases releasetransition.ReleasePair, _ releasetransition.ReleaseLinks, view releasetransition.V2SourceActivationView) (releasetransition.V2SourceActivationPlan, error) {
	if err := reconciler.resolveScope(releases); err != nil {
		return releasetransition.V2SourceActivationPlan{}, err
	}
	draft, err := reconciler.operation().prepareSourceActivationDraft(reconciler.configHome, view)
	if err != nil {
		return releasetransition.V2SourceActivationPlan{}, err
	}
	names := draft.names
	if !reconciler.allLocal {
		name := reconciler.yard
		if name == "" {
			name = "default"
		}
		names = []string{name}
	}
	targets := []config.MaterializationActivationTarget{}
	consequences := []string{}
	plans := map[string]reconcileruntime.IntegrationPlan{}
	unavailable := map[string]projectedIntegrationUnavailable{}
	for _, name := range names {
		if name == legacyTestYardName {
			continue
		}
		loaded, err := draft.load(name)
		if err != nil {
			return releasetransition.V2SourceActivationPlan{}, err
		}
		if loaded.Context.AccessKind == domain.AccessRemote {
			continue
		}
		desired, _, _, err := materializationDesired(configTarget{Name: name, Loaded: loaded}, true, draft.readAsset)
		if err != nil {
			return releasetransition.V2SourceActivationPlan{}, err
		}
		assessment, err := config.AssessMaterialization(desired, "absent", true, nil)
		if err != nil {
			return releasetransition.V2SourceActivationPlan{}, err
		}
		scope, _, err := reconciler.operation().materializedIntegrationScope(loaded, draft.readAsset)
		if err != nil {
			return releasetransition.V2SourceActivationPlan{}, err
		}
		if loaded.Integrations.AllowsCodingTools {
			incus, _ := reconciler.cli.statusPorts()
			instance, inspectErr := incus.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
			absent := errors.Is(inspectErr, ports.ErrInstanceNotFound) || errors.Is(inspectErr, os.ErrNotExist)
			if inspectErr != nil && !absent {
				return releasetransition.V2SourceActivationPlan{}, inspectErr
			}
			proof := projectedIntegrationUnavailable{yard: loaded.Context, absent: absent, instance: instance}
			// Only registration work can change a stopped target's native
			// ownership. Disk-/tmp work never temporarily boots or restores it.
			if !absent && instance.Config["user.subyard.desired_power"] == "stopped" {
				if expected := view.RegisteredYards["yard."+name]; expected != "" {
					owner := reconciler.operation().registeredYardMigration(releasetransition.ProcessRequest{ConfigHome: reconciler.configHome}, false, name)
					observed, err := owner.(releasetransition.V2ProspectiveActivationObserver).ObserveProspective(ctx, releases, releasetransition.ReleaseLinks{}, view.Settings)
					if err != nil {
						return releasetransition.V2SourceActivationPlan{}, err
					}
					if observed.Desired != expected {
						return releasetransition.V2SourceActivationPlan{}, domain.ErrPlanStale
					}
					if !observed.Converged {
						proof.owner, proof.desired = owner, expected
					}
				}
			}

			if absent || !strings.EqualFold(instance.Status, "running") {
				unavailable[name] = proof
			} else {
				platform := reconciler.operation().initPlatform(loaded, nil)
				if native, ok := platform.(reconcileruntime.Runtime); ok {
					plan, err := native.PrepareSourceIntegrationPlan(ctx, draft.readAsset, loaded.Integrations.Present && loaded.Integrations.Provenance.Scope != "command")
					if err != nil {
						return releasetransition.V2SourceActivationPlan{}, err
					}
					plans[name] = plan
					for _, step := range plan.Steps {
						consequences = append(consequences, "yard "+name+": "+step)
					}
					consequences = append(consequences, integrationAdoptionConsequences(name, plan)...)
				}
			}

		}
		targets = append(targets, config.MaterializationActivationTarget{Name: name, Assessment: assessment, IntegrationScope: scope})
	}
	reconciler.integrationPlans = plans
	reconciler.projectedIntegrationPlans = true
	reconciler.projectedUnavailable = unavailable
	assessed := config.AssessMaterializationActivation(targets)
	return releasetransition.V2SourceActivationPlan{Desired: releasetransition.Fingerprint(assessed.Desired), Consequences: consequences}, nil
}

// The broker's projected Desired observes its yard through the prepared
// settings, exactly as its native observation does after they are imported,
// and derives Desired through the same owner.
func (cli *CLI) prepareBrokerSourceActivation(request releasetransition.ProcessRequest, owner *activationStageReconciler) func(context.Context, releasetransition.ReleasePair, releasetransition.ReleaseLinks, releasetransition.V2SourceActivationView) (releasetransition.V2SourceActivationPlan, error) {
	return func(ctx context.Context, releases releasetransition.ReleasePair, _ releasetransition.ReleaseLinks, view releasetransition.V2SourceActivationView) (releasetransition.V2SourceActivationPlan, error) {
		draft, err := cli.prepareSourceActivationDraft(request.ConfigHome, view)
		if err != nil {
			return releasetransition.V2SourceActivationPlan{}, err
		}
		applicability, err := cli.serviceStageApplicability(ctx, owner.id, draft.names, draft.load)
		if err != nil {
			return releasetransition.V2SourceActivationPlan{}, err
		}
		desired, err := owner.desired(applicability, releases.Target)
		return releasetransition.V2SourceActivationPlan{Desired: desired}, err
	}
}
func (reconciler *activationStageReconciler) PrepareSourceActivation(ctx context.Context, releases releasetransition.ReleasePair, links releasetransition.ReleaseLinks, view releasetransition.V2SourceActivationView) (releasetransition.V2SourceActivationPlan, error) {
	if reconciler.sourceActivation != nil {
		return reconciler.sourceActivation(ctx, releases, links, view)
	}
	observation, err := reconciler.Observe(ctx, releases, links)
	return releasetransition.V2SourceActivationPlan{Desired: observation.Desired, Consequences: observation.Consequences}, err
}

type projectedIntegrationUnavailable struct {
	yard     domain.Context
	absent   bool
	instance ports.InstanceInfo
	owner    releasetransition.V2ActivationReconciler
	desired  releasetransition.Fingerprint
}

func (reconciler *materializedConfigActivationReconciler) verifyProjectedUnavailable(ctx context.Context) error {
	if len(reconciler.projectedUnavailable) == 0 {
		return nil
	}
	incus, _ := reconciler.cli.statusPorts()
	for name, before := range reconciler.projectedUnavailable {
		fresh, err := incus.Instance(ctx, before.yard.IncusProject, before.yard.YardInstanceName)
		absent := errors.Is(err, ports.ErrInstanceNotFound) || errors.Is(err, os.ErrNotExist)
		if err != nil && !absent {
			return err
		}
		if absent != before.absent {
			return fmt.Errorf("%w: yard %s integration backend availability changed", domain.ErrPlanStale, name)
		}
		if !absent {
			original, _ := activationStageFingerprint(before.instance)
			current, _ := activationStageFingerprint(fresh)
			if original != current {
				if before.owner != nil && fresh.Config["volatile.uuid"] == before.instance.Config["volatile.uuid"] && fresh.Config["user.subyard.desired_power"] == "stopped" && strings.EqualFold(fresh.Status, "stopped") {
					observed, err := before.owner.Observe(ctx, releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
					if err != nil {
						return err
					}
					if observed.Converged && observed.Desired == before.desired {
						continue
					}
				}
				return fmt.Errorf("%w: yard %s unavailable integration backend changed", domain.ErrPlanStale, name)
			}
		}
	}
	return nil
}
