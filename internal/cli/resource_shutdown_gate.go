package cli

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/resource"
)

// This permit grants only a newly assessed resource shutdown. Completed release
// history is retained evidence, never authority to resume or apply activation.
type resourceShutdownPermit struct {
	history    *configApplyRepairPermit
	inputs     *ownerInputBaseline
	command    string
	verb       string
	assessment string
	blockers   string
}

func (cli *CLI) prepareResourceShutdownRepair(ctx context.Context, yard string, definition resource.Definition,
	arguments []string, outcome releasetransition.Outcome,
) (*resourceShutdownPermit, error) {
	operation := *cli
	operation.env = maps.Clone(cli.baseEnv)
	cli = &operation
	invocation, err := parseResourceInvocation(arguments)
	if err != nil || invocation.help || len(invocation.arguments) != 1 || invocation.verb != definition.Shutdown ||
		definition.Proxy == nil || !definition.Proxy.IsOwnerIPv4Ingress() {
		return nil, nil
	}
	if outcome.Status != releasetransition.StatusOperatorActionRequired || outcome.Code != releasetransition.CodeDependencyUnavailable ||
		outcome.ReachedGoal || outcome.Transaction == nil || outcome.Active == "" || outcome.Active != outcome.Target {
		return nil, nil
	}
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return nil, err
	}
	legacy, err := migration.InspectMutationGate(ctx, options)
	if err != nil || legacy != nil {
		return nil, err
	}
	store, err := releasetransition.NewPOSIXV2Store(options.ConfigHome)
	if err != nil {
		return nil, err
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return nil, err
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil || !configApplyRepairJournalMatches(journal, outcome) || *outcome.Transaction != journal.Transaction {
		return nil, nil
	}
	registryPayload, err := readConfigApplyRegistry(cli.options.RepositoryRoot)
	if err != nil {
		return nil, err
	}
	catalog := releasetransition.BuiltinCapabilityCatalog()
	registry, digest, err := releasetransition.ParseRegistryV2(registryPayload, catalog)
	if err != nil || digest != journal.RegistryDigest || catalog.Digest() != journal.CatalogDigest {
		return nil, nil
	}
	ledger, err := store.ReadMigrationLedger(registry, digest)
	if err != nil || !ledger.AuthoritySnapshot.Exists {
		return nil, err
	}
	pending, err := registry.PendingPath(ledger.Ledger)
	if err != nil || len(pending) != 0 {
		return nil, err
	}
	runtime := releaseruntime.New(releaseruntime.Config{Inputs: cli.commandInputs})
	defer runtime.Close()
	inspection, err := runtime.InspectMutationGateInspection(ctx, options.RuntimeRoot, options.ConfigHome, "default")
	if err != nil || inspection == nil || inspection.Outcome == nil || !sameConfigApplyGate(outcome, *inspection.Outcome) || len(inspection.Blockers) == 0 {
		return nil, err
	}
	// Only runtime readiness can be unavailable. Invalid source/history and
	// ambiguous ownership retain their ordinary fail-closed outcomes.
	// The installed guest helper observer also needs the unavailable agent;
	// host key-tool readiness grants no guest ingress cleanup authority.
	allowed := append(cli.profileRuntimeIDs(), "materialized-config", "core-guest-runtime")
	for _, blocker := range inspection.Blockers {
		id, ok := strings.CutPrefix(blocker.Resource, "activation.")
		if !ok || !slices.Contains(allowed, id) || blocker.Code != releasetransition.CodeDependencyUnavailable {
			return nil, nil
		}
	}
	source, tooling, err := cli.integrationCleanupSourceBindings(ctx, journal)
	if err != nil {
		return nil, nil
	}
	cli.env = maps.Clone(cli.baseEnv)
	loaded, err := cli.loadContext(yard)
	if err != nil || loaded.Context.AccessKind != domain.AccessLocal || loaded.Context.YardKind != domain.YardVM ||
		yard == "" || yard == "default" || !slices.Contains(strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"]), definition.Profile) {
		return nil, err
	}
	inputs, err := cli.captureOwnerInputs(loaded, "", nil)
	if err != nil {
		return nil, err
	}
	output, err := cli.prepareResource(ctx, loaded, definition, invocation.arguments)
	if err != nil {
		return nil, err
	}
	native, err := cli.resources.PrepareResult(cli.coreActions, definition.Command, invocation.verb, output)
	if err != nil {
		return nil, err
	}
	if err := validateResourceShutdownAssessment(definition, native.Assessment); err != nil {
		return nil, err
	}
	permit := &resourceShutdownPermit{history: &configApplyRepairPermit{yard: yard, configHome: options.ConfigHome,
		journal: snapshot, ledger: ledger.AuthoritySnapshot, ledgerProjection: ledger.ProjectionSnapshot, checkpointed: ledger.Checkpointed,
		gate: outcome, sourceBinding: source, toolingBinding: tooling}, inputs: inputs, command: definition.Command,
		verb: invocation.verb, assessment: operationStateDigest(native), blockers: operationStateDigest(inspection.Blockers)}
	if err := cli.finishTeardownRepair(ctx, permit.history); err != nil {
		return nil, err
	}
	return permit, nil
}

func validateResourceShutdownAssessment(definition resource.Definition, assessment domain.ActionAssessment) error {
	local, ok := localResourceAction(definition, assessment.Action)
	if !ok || local != definition.Shutdown || validateOrphanShutdown(definition, assessment) != nil ||
		assessment.Effect == domain.ActionMutation && (!slices.Contains(assessment.Impacts, domain.ImpactHostIncus) ||
			!slices.Contains(assessment.Impacts, domain.ImpactHostNetwork)) {
		return errors.New("release recovery admits only the assessed reversible public-ingress shutdown")
	}
	allowed := []domain.ActionImpact{domain.ImpactAccess, domain.ImpactHostIncus, domain.ImpactHostNetwork, domain.ImpactSecurity, domain.ImpactSharedWorkload, domain.ImpactTrust}
	for _, impact := range assessment.Impacts {
		if !slices.Contains(allowed, impact) {
			return errors.New("release recovery shutdown has effects outside public ingress")
		}
	}
	return nil
}

func (cli *CLI) validateResourceShutdownRepair(definition resource.Definition, verb string, loaded config.Loaded, native resource.PrepareResult) error {
	permit := cli.resourceShutdownRepair
	if permit == nil {
		return nil
	}
	if definition.Command != permit.command || verb != permit.verb || resourceSettingsDigest(loaded) != resourceSettingsDigest(permit.inputs.initial) ||
		operationStateDigest(native) != permit.assessment || validateResourceShutdownAssessment(definition, native.Assessment) != nil {
		return domain.ErrPlanStale
	}
	return nil
}

func (cli *CLI) lockResourceShutdownRepair(ctx context.Context) (func(), error) {
	permit := cli.resourceShutdownRepair
	if permit == nil {
		return func() {}, nil
	}
	store, err := releasetransition.NewPOSIXV2Store(permit.history.configHome)
	if err != nil {
		return nil, err
	}
	unlock, err := store.Lock()
	if err != nil {
		return nil, err
	}
	if err = permit.inputs.check(ctx, cli); err == nil {
		operation := *cli
		operation.env = maps.Clone(cli.baseEnv)
		err = operation.finishTeardownRepair(ctx, permit.history)
	}
	if err == nil {
		err = cli.checkResourceShutdownGate(ctx, permit)
	}
	if err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

func (cli *CLI) finishResourceShutdownRepair(ctx context.Context) error {
	if cli.resourceShutdownRepair == nil {
		return nil
	}
	operation := *cli
	operation.env = maps.Clone(cli.baseEnv)
	return operation.finishTeardownRepair(ctx, cli.resourceShutdownRepair.history)
}

func (cli *CLI) checkResourceShutdownGate(ctx context.Context, permit *resourceShutdownPermit) error {
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return domain.ErrPlanStale
	}
	runtime := releaseruntime.New(releaseruntime.Config{Inputs: cli.commandInputs})
	defer runtime.Close()
	inspection, err := runtime.InspectMutationGateInspection(ctx, options.RuntimeRoot, options.ConfigHome, "default")
	if err != nil {
		return err
	}
	if inspection == nil || inspection.Outcome != nil && inspection.Outcome.Status == releasetransition.StatusReady && inspection.Outcome.ReachedGoal {
		return nil
	}
	if inspection.Outcome == nil || !sameConfigApplyGate(permit.history.gate, *inspection.Outcome) || operationStateDigest(inspection.Blockers) != permit.blockers {
		return domain.ErrPlanStale
	}
	return nil
}
