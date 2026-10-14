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
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

type sourceActivationDraft struct {
	cli        *CLI
	home       string
	view       releasetransition.V2SettingsSnapshotView
	projection testyardmigration.ActivationProjection
	inputs     *config.ProspectiveFiles
	overrides  map[string]string
	names      []string
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
	actual, err := config.YardNames(filepath.Join(cli.options.RepositoryRoot, "config"), home)
	if err != nil {
		return nil, err
	}
	names = append(names, actual...)
	names = append(names, "default")
	if view.OwnerRegistration != nil {
		options, err := cli.releaseTransitionTestYardOptions(releasetransition.ProcessRequest{ConfigHome: home})
		if err != nil {
			return nil, err
		}
		before := view.OwnerRegistration
		draft.projection, err = testyardmigration.PrepareActivationProjection(options, testyardmigration.Prepared{State: testyardmigration.State(before.State), RegistrationDigest: string(before.Registration), OverridesDigest: string(before.Overrides), ControllerDigest: string(before.Controller), SharedImages: before.SharedImages}, view.Settings.ReadSnapshot)
		if err != nil {
			return nil, err
		}
		if len(draft.projection.Sources) != 0 {
			names = slices.DeleteFunc(names, func(name string) bool { return name == testyardmigration.LegacyYard })
			names = append(names, testyardmigration.CurrentYard)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	draft.names = names
	for _, name := range names {
		if !domain.SafeName(name) {
			return nil, errors.New("prospective yard name is invalid")
		}
	}
	for path := range draft.projection.Sources {
		paths = append(paths, path)
	}
	paths = append(paths, draft.projection.Removed...)
	for _, path := range paths {
		if !filepath.IsAbs(path) || !strings.HasPrefix(path, home+string(filepath.Separator)) {
			return nil, errors.New("prospective activation input path escapes configuration home")
		}
		// Preserve logical layer presence without copying or reading its bytes.
		draft.overrides[path] = path
	}
	for _, path := range draft.projection.Removed {
		draft.overrides[path] = ""
	}
	draft.inputs = &config.ProspectiveFiles{Root: home, Yards: names, ReadSnapshot: view.Settings.ReadSnapshot, ReadPreparedInput: draft.readPreparedInput}
	return draft, nil
}
func (draft *sourceActivationDraft) readPreparedInput(path string) (config.PersistentFileSnapshot, bool, error) {
	if slices.Contains(draft.projection.Removed, path) {
		return config.PersistentFileSnapshot{}, true, nil
	}
	source := path
	alias, aliased := draft.projection.Sources[path]
	if aliased {
		source = alias
	}
	if reader, ok := draft.view.(releasetransition.V2PreparedInputReader); ok {
		snapshot, matched, err := reader.ReadPreparedInput(source)
		if matched || err != nil {
			if aliased {
				snapshot.Identity = config.PersistentFileIdentity{}
			}
			return snapshot, matched, err
		}
	}
	if aliased {
		snapshot, err := draft.view.ReadSnapshot(source)
		snapshot.Identity = config.PersistentFileIdentity{}
		return snapshot, true, err
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
		if name == testyardmigration.LegacyYard && len(draft.projection.Sources) != 0 {
			name = testyardmigration.CurrentYard
		}
		names = []string{name}
	}
	targets := []config.MaterializationActivationTarget{}
	consequences := []string{}
	plans := map[string]reconcileruntime.IntegrationPlan{}
	unavailable := map[string]projectedIntegrationUnavailable{}
	for _, name := range names {
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
		scope := ""
		if loaded.Integrations.AllowsCodingTools {
			runtime := reconcileruntime.Runtime{RepositoryRoot: reconciler.cli.options.RepositoryRoot, Environment: environmentList(loaded.Environment, nil), Yard: loaded.Context, Integrations: loaded.Catalog.Integrations()}
			scope, _, err = runtime.IntegrationScopeWithSourceReader(draft.readAsset)
			if err != nil {
				return releasetransition.V2SourceActivationPlan{}, err
			}

			incus, _ := reconciler.cli.statusPorts()
			instance, inspectErr := incus.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
			absent := errors.Is(inspectErr, ports.ErrInstanceNotFound) || errors.Is(inspectErr, os.ErrNotExist)
			if inspectErr != nil && !absent {
				return releasetransition.V2SourceActivationPlan{}, inspectErr
			}
			if absent || !strings.EqualFold(instance.Status, "running") {
				unavailable[name] = projectedIntegrationUnavailable{yard: loaded.Context, absent: absent, instance: instance}
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

func (cli *CLI) projectedOwnerApplicability(ctx context.Context, request releasetransition.ProcessRequest, view releasetransition.V2SourceActivationView) (testyardmigration.BrokerRuntimeState, testyardmigration.State, error) {
	options, err := cli.releaseTransitionTestYardOptions(request)
	if err != nil {
		return "", "", err
	}
	var before testyardmigration.Prepared
	if bound := view.OwnerRegistration; bound != nil {
		before = testyardmigration.Prepared{State: testyardmigration.State(bound.State), RegistrationDigest: string(bound.Registration), OverridesDigest: string(bound.Overrides), ControllerDigest: string(bound.Controller), SharedImages: bound.SharedImages}
	} else {
		before, err = testyardmigration.PrepareProspective(ctx, options, view.Settings.ReadSnapshot)
		if err != nil {
			return "", "", err
		}
	}
	if before.State == testyardmigration.StateAbsent {
		return testyardmigration.BrokerRuntimeAbsent, testyardmigration.StateAbsent, nil
	}
	draft, err := cli.prepareSourceActivationDraft(request.ConfigHome, view)
	if err != nil {
		return "", "", err
	}
	loaded, err := draft.load(testyardmigration.CurrentYard)
	if err != nil {
		return "", "", err
	}
	return testyardmigration.PrepareProjectedActivationApplicability(ctx, options, before, loaded)
}
func (cli *CLI) prepareBrokerSourceActivation(request releasetransition.ProcessRequest) func(context.Context, releasetransition.ReleasePair, releasetransition.ReleaseLinks, releasetransition.V2SourceActivationView) (releasetransition.V2SourceActivationPlan, error) {
	return func(ctx context.Context, releases releasetransition.ReleasePair, _ releasetransition.ReleaseLinks, view releasetransition.V2SourceActivationView) (releasetransition.V2SourceActivationPlan, error) {
		state, _, err := cli.projectedOwnerApplicability(ctx, request, view)
		if err != nil {
			return releasetransition.V2SourceActivationPlan{}, err
		}
		desired, err := activationStageFingerprint(struct {
			ID     string                      `json:"id"`
			Stage  ports.ReconcileStageID      `json:"stage"`
			State  string                      `json:"state"`
			Target releasetransition.ReleaseID `json:"target"`
		}{"test-vm-broker", ports.ReconcileStageTestVMs, string(state), releases.Target})
		return releasetransition.V2SourceActivationPlan{Desired: desired}, err
	}
}
func (cli *CLI) prepareRouteSourceActivation(request releasetransition.ProcessRequest) func(context.Context, releasetransition.ReleasePair, releasetransition.ReleaseLinks, releasetransition.V2SourceActivationView) (releasetransition.V2SourceActivationPlan, error) {
	return func(ctx context.Context, releases releasetransition.ReleasePair, _ releasetransition.ReleaseLinks, view releasetransition.V2SourceActivationView) (releasetransition.V2SourceActivationPlan, error) {
		_, state, err := cli.projectedOwnerApplicability(ctx, request, view)
		if err != nil {
			return releasetransition.V2SourceActivationPlan{}, err
		}
		desired, err := activationStageFingerprint(struct {
			ID     string                      `json:"id"`
			Target releasetransition.ReleaseID `json:"target"`
			Active bool                        `json:"active"`
		}{"test-yard-route-consumers", releases.Target, state != testyardmigration.StateAbsent})
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
func (reconciler *routeConsumerActivationReconciler) PrepareSourceActivation(ctx context.Context, releases releasetransition.ReleasePair, links releasetransition.ReleaseLinks, view releasetransition.V2SourceActivationView) (releasetransition.V2SourceActivationPlan, error) {
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
				return fmt.Errorf("%w: yard %s unavailable integration backend changed", domain.ErrPlanStale, name)
			}
		}
	}
	return nil
}
