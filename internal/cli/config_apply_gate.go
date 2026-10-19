package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// configApplyRepairPermit admits bounded ordinary operations behind a verified,
// completed release transition. It is deliberately local
// to the CLI: no environment value or public command can manufacture it.
type configApplyRepairPermit struct {
	yard                string
	allLocal            bool
	configHome          string
	journal             releasetransition.ProtectedSnapshot
	ledger              releasetransition.ProtectedSnapshot
	ledgerProjection    releasetransition.ProtectedSnapshot
	checkpointed        bool
	gate                releasetransition.Outcome
	factsFingerprint    string
	driftedTargetNames  []string
	requestedTargets    map[string]string
	selectedTargetNames []string
	selectedRuntimes    map[string]ports.RuntimeObservation
	profileInit         bool
	cleanupID           string
	sourceBinding       string
	toolingBinding      string
	cleanupPlanBinding  string
	teardownInputs      *ownerInputBaseline
}

type configApplyRepairFacts struct {
	CleanupSource    string                        `json:"cleanup_source,omitempty"`
	CleanupTooling   string                        `json:"cleanup_tooling,omitempty"`
	CleanupPlan      string                        `json:"cleanup_plan,omitempty"`
	Journal          releasetransition.Fingerprint `json:"journal"`
	Ledger           releasetransition.Fingerprint `json:"ledger"`
	Target           releasetransition.ReleaseID   `json:"target"`
	Direction        releasetransition.Direction   `json:"direction"`
	OtherActivations []configApplyActivationFact   `json:"other_activations"`
	ConfigTargets    []configApplyTargetFact       `json:"config_targets"`
	TeardownInputs   string                        `json:"teardown_inputs,omitempty"`
	TeardownPlans    map[string]string             `json:"teardown_plans,omitempty"`
}

type configApplyActivationFact struct {
	ID        string                        `json:"id"`
	Actual    releasetransition.Fingerprint `json:"actual"`
	Desired   releasetransition.Fingerprint `json:"desired"`
	Converged bool                          `json:"converged"`
}

type configApplyTargetFact struct {
	Name                 string               `json:"name"`
	DesiredFingerprint   string               `json:"desired_fingerprint"`
	IntegrationPlan      string               `json:"integration_plan,omitempty"`
	TeardownRuntimeState teardownRuntimeState `json:"teardown_runtime_state,omitempty"`
}

type teardownRuntimeState string

const (
	teardownRuntimeAbsent  teardownRuntimeState = "absent"
	teardownRuntimeStopped teardownRuntimeState = "stopped"
	teardownRuntimeRunning teardownRuntimeState = "running"
)

func (cli *CLI) prepareConfigApplyRepair(
	ctx context.Context,
	yard string,
	allLocal bool,
	outcome releasetransition.Outcome,
) (*configApplyRepairPermit, error) {
	return cli.prepareConfigApplyRepairMode(ctx, yard, allLocal, outcome, true, nil)
}

func (cli *CLI) prepareConfigApplyRepairMode(
	ctx context.Context,
	yard string,
	allLocal bool,
	outcome releasetransition.Outcome,
	requireDrift bool,
	selectedNames []string,
) (*configApplyRepairPermit, error) {
	return cli.prepareActivationRepair(ctx, yard, allLocal, outcome, requireDrift, selectedNames, false, "")
}

// Profile runtime init repair shares the completed-journal, ledger, link and lock guards of
// config repair. It permits only an ordinarily assessed init for the selected yard.
func (cli *CLI) prepareProfileInitRepair(ctx context.Context, yard string, arguments []string,
	outcome releasetransition.Outcome,
) (*configApplyRepairPermit, error) {
	request, err := parseInitArguments(arguments)
	if err != nil || request.mode != initReconcile || request.profile != "" {
		return nil, err
	}
	return cli.prepareActivationRepair(ctx, yard, false, outcome, true, nil, true, "")
}

// Teardown grants new destructive authority only for the ordinarily assessed
// local target. Completed release history remains immutable; reset stays gated.
func (cli *CLI) prepareTeardownRepair(ctx context.Context, loaded config.Loaded, arguments []string, outcome releasetransition.Outcome) (*configApplyRepairPermit, error) {
	execution, err := prepareTeardownExecution(arguments)
	if err != nil || execution.resetConfig || !configApplyRepairGateShape(outcome) {
		return nil, err
	}
	if loaded.Context.AccessKind != domain.AccessLocal {
		return nil, errors.New("completed-history teardown requires the local source owner")
	}
	inputs, err := cli.captureOwnerInputs(loaded, "", nil)
	if err != nil {
		return nil, err
	}
	yard := loaded.Context.YardName
	return cli.prepareActivationRepair(ctx, yard, false, outcome, true, []string{yard}, false, "", inputs)
}

func (cli *CLI) prepareActivationRepair(
	ctx context.Context, yard string, allLocal bool, outcome releasetransition.Outcome,
	requireDrift bool, selectedNames []string, profileInit bool, cleanupID string,
	teardownInputs ...*ownerInputBaseline,
) (*configApplyRepairPermit, error) {
	var teardown *ownerInputBaseline
	if len(teardownInputs) != 0 {
		teardown = teardownInputs[0]
	}
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil {
		return nil, err
	}
	if !available || cleanupID == "" && !configApplyRepairGateShape(outcome) {
		return nil, nil
	}
	// A V2 drift outcome can mask unfinished legacy migration metadata in the
	// ordinary gate. Both journal families must independently admit this repair.
	legacy, err := migration.InspectMutationGate(ctx, options)
	if err != nil || legacy != nil {
		return nil, nil
	}
	store, err := releasetransition.NewPOSIXV2Store(options.ConfigHome)
	if err != nil {
		return nil, err
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return nil, nil
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil || !configApplyRepairJournalMatches(journal, outcome) &&
		!(profileInit && profileInitRepairRollbackJournalMatches(journal, outcome)) {
		return nil, nil
	}
	var sourceBinding, toolingBinding string
	if configApplyRepairRustGateShape(outcome) {
		// The completed Rust drift status retains its original transaction.
		// Authenticate the original bytes, then distinguish a fresh drift plan
		// from the original terminal-cleanup continuation before admitting edits.
		reader := releaseruntime.NewSourceInspector(cli.options.RepositoryRoot, cli.options.Stderr)
		defer reader.Close()
		proof, proofErr := cli.inspectConfigSourceAdmission(ctx, reader)
		if proofErr != nil || proof.Metadata.Journal == nil {
			return nil, nil
		}
		raw, marshalErr := releasetransition.MarshalJournal(*proof.Metadata.Journal)
		if marshalErr != nil || !bytes.Equal(raw, snapshot.Payload) {
			return nil, nil
		}
		owner := releaseruntime.New(releaseruntime.Config{Inputs: cli.commandInputs,
			Context: releaseruntime.RuntimeContext{OperatorHome: firstNonempty(cli.env["SUBYARD_OPERATOR_HOME"], cli.env["HOME"]), DataHome: options.DataHome, ConfigHome: options.ConfigHome, RuntimeRoot: options.RuntimeRoot, OperationID: cli.env["SUBYARD_OPERATION_ID"]}, Stderr: cli.options.Stderr})
		defer owner.Close()
		inspection, inspectErr := owner.InspectMutationGateInspection(ctx, options.RuntimeRoot, options.ConfigHome, "default")
		if inspectErr != nil || !configApplyRepairRustInspectionMatches(journal, outcome, inspection, profileInit) {
			return nil, nil
		}
		sourceBinding = proof.Binding
	}
	if cleanupID != "" {
		sourceBinding, toolingBinding, err = cli.integrationCleanupRecoveryAuthority(ctx, cleanupID, outcome, journal)
		if err != nil {
			return nil, nil
		}
	}
	if teardown != nil {
		sourceBinding, toolingBinding, err = cli.integrationCleanupSourceBindings(ctx, journal)
		if err != nil {
			return nil, nil
		}
	}
	observedGate, err := cli.inspectMutationGate(ctx, yard)
	if err != nil || observedGate != nil && !sameConfigApplyGate(outcome, *observedGate) ||
		observedGate == nil && requireDrift {
		return nil, nil
	}
	gateReady := observedGate == nil
	registryPayload, err := readConfigApplyRegistry(cli.options.RepositoryRoot)
	if err != nil {
		return nil, nil
	}
	catalog := releasetransition.BuiltinCapabilityCatalog()
	registry, registryDigest, err := releasetransition.ParseRegistryV2(registryPayload, catalog)
	if err != nil || registryDigest != journal.RegistryDigest || catalog.Digest() != journal.CatalogDigest {
		return nil, nil
	}
	ledgerState, err := store.ReadMigrationLedger(registry, registryDigest)
	if err != nil || !ledgerState.AuthoritySnapshot.Exists {
		return nil, nil
	}
	ledgerSnapshot := ledgerState.AuthoritySnapshot
	pending, err := registry.PendingPath(ledgerState.Ledger)
	if err != nil || len(pending) != 0 {
		return nil, nil
	}

	// Match the trusted repair child, including inventory reloads: command
	// overrides must never change what is assessed relative to what is written.
	operation := *cli
	operation.baseEnv = cli.freshMigrationEnvironment(cli.baseEnv, cli.options.RepositoryRoot)
	operation.env = maps.Clone(operation.baseEnv)
	loaded, err := operation.resolveReleaseTransitionContext(yard, options.ConfigHome)
	if err != nil {
		return nil, nil
	}
	allTargets, err := operation.localConfigTargets(loaded, true)
	if err != nil {
		return nil, nil
	}
	requested := make(map[string]struct{})
	if selectedNames == nil {
		requestedTargets, err := operation.localConfigTargets(loaded, allLocal)
		if err != nil {
			return nil, nil
		}
		for _, target := range requestedTargets {
			requested[target.Name] = struct{}{}
		}
	} else {
		for _, name := range selectedNames {
			if _, duplicate := requested[name]; duplicate {
				return nil, nil
			}
			requested[name] = struct{}{}
		}
	}
	facts := configApplyRepairFacts{
		Journal: snapshot.Fingerprint, Ledger: ledgerSnapshot.Fingerprint, Target: journal.Goal.Target,
		Direction:     journal.Goal.Direction,
		CleanupSource: sourceBinding, CleanupTooling: toolingBinding,
	}
	if teardown != nil {
		facts.TeardownInputs, facts.TeardownPlans = teardown.binding(), map[string]string{}
	}
	var driftedNames []string
	requestedDesired := make(map[string]string, len(requested))
	selectedRuntimes := make(map[string]ports.RuntimeObservation, len(requested))
	for _, target := range allTargets {
		assessment, assessErr := operation.assessConfigTarget(ctx, target, true)
		if assessErr != nil {
			return nil, nil
		}
		targetFact := configApplyTargetFact{
			Name: target.Name, DesiredFingerprint: assessment.DesiredFingerprint,
		}
		if teardown != nil {
			incus, _ := operation.statusPorts()
			instance, err := incus.Instance(ctx, target.Loaded.Context.IncusProject, target.Loaded.Context.YardInstanceName)
			switch {
			case errors.Is(err, ports.ErrInstanceNotFound):
				targetFact.TeardownRuntimeState = teardownRuntimeAbsent
			case err != nil:
				return nil, nil
			case strings.EqualFold(instance.Status, "stopped"):
				targetFact.TeardownRuntimeState = teardownRuntimeStopped
			case strings.EqualFold(instance.Status, "running"):
				targetFact.TeardownRuntimeState = teardownRuntimeRunning
			default:
				return nil, nil
			}
		}
		facts.ConfigTargets = append(facts.ConfigTargets, targetFact)
		if _, selected := requested[target.Name]; selected {
			requestedDesired[target.Name] = assessment.DesiredFingerprint
		}
		if cleanupID != "" {
			runtime := cli.integrationRuntime(target.Loaded)
			plan, planErr := runtime.IntegrationPlan(ctx)
			if target.Name != yard {
				if planErr != nil || plan.Changed {
					return nil, nil
				}
			} else {
				var conflict interface{ IntegrationCleanupTarget() string }
				if planErr == nil || !errors.As(planErr, &conflict) || conflict.IntegrationCleanupTarget() != cleanupID ||
					!target.Loaded.Integrations.Present || slices.Contains(target.Loaded.Integrations.Effective, cleanupID) ||
					target.Loaded.Environment["AGENT_"+cleanupID+"_CLEANUP"] == "" {
					return nil, nil
				}
				cleanup, ok := runtime.(IntegrationCleanupRuntime)
				if !ok {
					return nil, nil
				}
				cleanupPlan, err := cleanup.IntegrationCleanupPlan(ctx, cleanupID)
				if err != nil {
					return nil, nil
				}
				facts.CleanupPlan = cleanupPlan.StateBinding()
			}
		}
		changed := assessment.Changed
		if profileInit && target.Loaded.Integrations.AllowsCodingTools &&
			(assessment.State == "converged" || assessment.State == "drift") {
			runtime, available := operation.initPlatform(target.Loaded, nil).(IntegrationRuntime)
			if operation.options.IntegrationRuntime != nil {
				runtime, available = operation.integrationRuntime(target.Loaded), true
			}
			if available {
				plan, err := runtime.IntegrationPlan(ctx)
				if err != nil {
					return nil, nil
				}
				facts.ConfigTargets[len(facts.ConfigTargets)-1].IntegrationPlan = plan.StateBinding()
				changed = changed || plan.Changed
			}
		}
		if teardown != nil {
			if target.Name == yard && (operationStateDigest(target.Loaded.Context) != operationStateDigest(teardown.initial.Context) ||
				integrationConfigurationFingerprint(target.Loaded) != integrationConfigurationFingerprint(teardown.initial)) {
				return nil, nil
			}
			// Match materialized release readiness: native integrations need a running target.
			// Known unavailable targets retain source validation and an exact state binding.
			if targetFact.TeardownRuntimeState == teardownRuntimeRunning {
				plan, err := operation.integrationRuntime(target.Loaded).IntegrationPlan(ctx)
				if err != nil || !validConfigApplyActivationObservation(releasetransition.V2ActivationObservation{
					Actual: releasetransition.Fingerprint(plan.Fingerprint), Desired: releasetransition.Fingerprint(plan.Scope),
				}) {
					return nil, nil
				}
				facts.TeardownPlans[target.Name] = plan.StateBinding()
				changed = changed || plan.Changed
			}
		}
		if profileInit || teardown != nil {
			contracts, err := profileActivationPlatform(&operation, target).ObserveProfileRuntimes(ctx)
			if err != nil {
				return nil, nil
			}
			for _, id := range cli.profileRuntimeIDs() {
				contract, exists := contracts[id]
				if !exists {
					return nil, nil
				}
				if teardown != nil {
					switch contract.State {
					case ports.RuntimeStateAbsent, ports.RuntimeStateDeferred:
						if contract.Actual != "" || contract.Desired != "" || contract.HookBinding != "" {
							return nil, nil
						}
					case ports.RuntimeStateCurrent, ports.RuntimeStateStale:
						if !validConfigApplyActivationObservation(releasetransition.V2ActivationObservation{
							Actual: releasetransition.Fingerprint(firstNonempty(contract.Actual, contract.Desired)), Desired: releasetransition.Fingerprint(contract.Desired),
						}) || contract.State == ports.RuntimeStateCurrent && contract.Actual != contract.Desired ||
							contract.State == ports.RuntimeStateStale && contract.Actual == contract.Desired ||
							contract.HookBinding != "" && !validConfigApplyActivationObservation(releasetransition.V2ActivationObservation{
								Actual: releasetransition.Fingerprint(contract.HookBinding), Desired: releasetransition.Fingerprint(contract.Desired),
							}) {
							return nil, nil
						}
					default:
						return nil, nil
					}
				}
				if _, selected := requested[target.Name]; selected {
					selectedRuntimes[target.Name+"/"+id] = contract
				}
				changed = changed || contract.State == ports.RuntimeStateStale
			}
		}
		if changed {
			if _, selected := requested[target.Name]; !selected {
				return nil, nil
			}
			driftedNames = append(driftedNames, target.Name)
		}
	}
	if len(requestedDesired) != len(requested) {
		return nil, nil
	}
	request := releasetransition.ProcessRequest{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
		RuntimeRoot:   options.RuntimeRoot, ConfigHome: options.ConfigHome, Yard: yard,
		Target: journal.Goal.Target, Direction: journal.Goal.Direction,
		ArtifactDigest: journal.ArtifactDigest, RegistryDigest: journal.RegistryDigest,
		InheritedSettingIDs: cli.releaseTransitionInheritedSettingIDs(),
	}
	pair := normalizedCompletedConfigApplyPair(journal.Releases)
	links, err := releasetransition.NewRuntimeLinkStore(options.RuntimeRoot)
	if err != nil {
		return nil, nil
	}
	observedLinks, err := links.Observe()
	if err != nil || observedLinks.Active != outcome.Active ||
		!sameConfigApplyReleaseID(observedLinks.Previous, pair.Previous) {
		return nil, nil
	}
	for _, reconciler := range cli.nonConfigActivationReconcilers(request) {
		observation, observeErr := reconciler.Observe(ctx, pair, observedLinks)
		if observeErr != nil || !validConfigApplyActivationObservation(observation) {
			return nil, nil
		}
		if runtime, ok := reconciler.(*profileRuntimeActivationReconciler); (profileInit || teardown != nil) && ok && !coreInstalledRuntimeID(runtime.id) {
			// Per-target contracts bind profile drift; teardown requires an exact locked recheck.
			observation.Actual, observation.Converged = observation.Desired, true
		}
		if stage, ok := reconciler.(*activationStageReconciler); profileInit && ok && stage.stage == ports.ReconcileStageTestVMs {
			applicability, err := stage.inspectApplicability(ctx)
			if err != nil {
				return nil, nil
			}
			if _, selected := requested[applicability.target]; selected && applicability.applies {
				platform, err := stage.platform(ctx, applicability)
				if err != nil {
					return nil, nil
				}
				if owner, ok := platform.(interface {
					ObserveTestVMSlotCountRepair(context.Context) (ports.RuntimeObservation, error)
				}); ok {
					contract, err := owner.ObserveTestVMSlotCountRepair(ctx)
					if err != nil || !validSlotCountRepairObservation(contract) {
						return nil, nil
					}
					selectedRuntimes[applicability.target+"/@test-vm-slot-count"] = contract
					facts.OtherActivations = append(facts.OtherActivations, configApplyActivationFact{
						ID: stage.ID() + ".slot-count", Actual: releasetransition.Fingerprint(contract.Desired),
						Desired: releasetransition.Fingerprint(contract.Desired), Converged: true,
					})
					if contract.State == ports.RuntimeStateStale {
						driftedNames = append(driftedNames, applicability.target)
					}
					observation.Actual, observation.Converged = observation.Desired, true
				}
			}
		}
		if !observation.Converged || observation.Actual != observation.Desired {
			return nil, nil
		}
		facts.OtherActivations = append(facts.OtherActivations, configApplyActivationFact{
			ID: reconciler.ID(), Actual: observation.Actual,
			Desired: observation.Desired, Converged: observation.Converged,
		})
	}
	if requireDrift && len(driftedNames) == 0 || gateReady && len(driftedNames) != 0 {
		return nil, nil
	}
	sort.Slice(facts.ConfigTargets, func(i, j int) bool {
		return facts.ConfigTargets[i].Name < facts.ConfigTargets[j].Name
	})
	sort.Slice(facts.OtherActivations, func(i, j int) bool {
		return facts.OtherActivations[i].ID < facts.OtherActivations[j].ID
	})
	fingerprint, err := configApplyFactsFingerprint(facts)
	if err != nil {
		return nil, err
	}
	sort.Strings(driftedNames)
	driftedNames = slices.Compact(driftedNames)
	return &configApplyRepairPermit{
		yard: yard, allLocal: allLocal, configHome: options.ConfigHome,
		journal: snapshot, ledger: ledgerSnapshot, ledgerProjection: ledgerState.ProjectionSnapshot,
		checkpointed: ledgerState.Checkpointed, gate: outcome, factsFingerprint: fingerprint,
		driftedTargetNames:  driftedNames,
		requestedTargets:    requestedDesired,
		selectedTargetNames: slices.Clone(selectedNames),
		selectedRuntimes:    selectedRuntimes,
		profileInit:         profileInit,
		cleanupID:           cleanupID, sourceBinding: sourceBinding, toolingBinding: toolingBinding,
		cleanupPlanBinding: facts.CleanupPlan,
		teardownInputs:     teardown,
	}, nil
}

func validSlotCountRepairObservation(observation ports.RuntimeObservation) bool {
	if observation.HookBinding != "" ||
		!validConfigApplyActivationObservation(releasetransition.V2ActivationObservation{
			Actual: releasetransition.Fingerprint(observation.Actual), Desired: releasetransition.Fingerprint(observation.Desired),
		}) {
		return false
	}
	return observation.State == ports.RuntimeStateCurrent && observation.Actual == observation.Desired ||
		observation.State == ports.RuntimeStateStale && observation.Actual != observation.Desired
}

func readConfigApplyRegistry(root string) ([]byte, error) {
	file, err := os.OpenFile(filepath.Join(root, "config", "release-transition.json"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("release transition registry is not a regular file")
	}
	return io.ReadAll(io.LimitReader(file, releasetransition.MaxRegistryV2Bytes+1))
}

func (permit *configApplyRepairPermit) matchesRequestedConfigs(assessments []configTargetAssessment) bool {
	if len(permit.requestedTargets) != len(assessments) {
		return false
	}
	for _, assessment := range assessments {
		expected, exists := permit.requestedTargets[assessment.Target.Name]
		if !exists || expected != assessment.DesiredFingerprint {
			return false
		}
	}
	return true
}

// lockConfigApplyRepair holds the protected update lock from the final
// observation through apply verification. The caller must always invoke the
// returned unlock function.
func (cli *CLI) lockConfigApplyRepair(
	ctx context.Context,
	permit *configApplyRepairPermit,
) (func(), error) {
	if permit == nil {
		return nil, errors.New("config apply release repair permit is required")
	}
	store, err := releasetransition.NewPOSIXV2Store(permit.configHome)
	if err != nil {
		return nil, err
	}
	unlock, err := store.Lock()
	if err != nil {
		return nil, err
	}
	if permit.teardownInputs != nil {
		if err := permit.teardownInputs.check(ctx, cli); err != nil {
			unlock()
			return nil, err
		}
	}
	refreshed, err := cli.prepareActivationRepair(
		ctx, permit.yard, permit.allLocal, permit.gate, false, permit.selectedTargetNames, permit.profileInit, permit.cleanupID,
		permit.teardownInputs,
	)
	if err != nil || refreshed == nil ||
		refreshed.factsFingerprint != permit.factsFingerprint ||
		refreshed.journal.Fingerprint != permit.journal.Fingerprint ||
		!bytes.Equal(refreshed.journal.Payload, permit.journal.Payload) ||
		permit.teardownInputs != nil && !maps.Equal(permit.selectedRuntimes, refreshed.selectedRuntimes) ||
		!configApplyDriftSubset(refreshed.driftedTargetNames, permit.driftedTargetNames) ||
		!validRuntimeRepairTransition(permit.selectedRuntimes, refreshed.selectedRuntimes) {
		unlock()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("config apply release repair changed after confirmation")
	}
	return unlock, nil
}

func validRuntimeRepairTransition(
	confirmed, current map[string]ports.RuntimeObservation,
) bool {
	if len(confirmed) != len(current) {
		return false
	}
	for name, before := range confirmed {
		after, exists := current[name]
		if !exists {
			return false
		}
		if before == after {
			continue
		}
		if before.State != "stale" || after.State != "current" ||
			before.Desired != after.Desired || after.Actual != after.Desired {
			return false
		}
	}
	return true
}

func configApplyDriftSubset(current, admitted []string) bool {
	allowed := make(map[string]struct{}, len(admitted))
	for _, name := range admitted {
		allowed[name] = struct{}{}
	}
	for _, name := range current {
		if _, ok := allowed[name]; !ok {
			return false
		}
	}
	return true
}

func (cli *CLI) finishConfigApplyRepair(
	ctx context.Context,
	permit *configApplyRepairPermit,
) error {
	if err := cli.checkConfigApplyRepairHistory(permit); err != nil {
		return err
	}
	outcome, err := cli.inspectMutationGate(ctx, permit.yard)
	if err != nil {
		return err
	}
	if outcome != nil {
		return fmt.Errorf("materialized configuration did not restore release readiness: %s", outcome.Code)
	}
	return nil
}

func (cli *CLI) finishTeardownRepair(ctx context.Context, permit *configApplyRepairPermit) error {
	if err := cli.checkConfigApplyRepairHistory(permit); err != nil {
		return err
	}
	journal, err := releasetransition.ParseJournal(permit.journal.Payload)
	if err != nil {
		return domain.ErrPlanStale
	}
	source, tooling, err := cli.integrationCleanupSourceBindings(ctx, journal)
	if err != nil || source != permit.sourceBinding || tooling != permit.toolingBinding {
		return domain.ErrPlanStale
	}
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return domain.ErrPlanStale
	}
	links, err := releasetransition.NewRuntimeLinkStore(options.RuntimeRoot)
	if err != nil {
		return err
	}
	observed, err := links.Observe()
	if err != nil || observed.Active != permit.gate.Active || !sameConfigApplyReleaseID(observed.Previous, normalizedCompletedConfigApplyPair(journal.Releases).Previous) {
		return domain.ErrPlanStale
	}
	return nil
}

func (cli *CLI) checkConfigApplyRepairHistory(permit *configApplyRepairPermit) error {
	if permit == nil {
		return errors.New("config apply release repair permit is required")
	}
	store, err := releasetransition.NewPOSIXV2Store(permit.configHome)
	if err != nil {
		return err
	}
	journal, err := store.ReadCurrentJournal()
	if err != nil {
		return err
	}
	if journal.Fingerprint != permit.journal.Fingerprint ||
		!bytes.Equal(journal.Payload, permit.journal.Payload) {
		return errors.New("config apply changed the completed release journal")
	}
	ledger, err := store.ReadLedger()
	if err != nil {
		return err
	}
	if ledger.Fingerprint != permit.ledgerProjection.Fingerprint ||
		!bytes.Equal(ledger.Payload, permit.ledgerProjection.Payload) {
		return errors.New("config apply changed the release ledger compatibility projection")
	}
	checkpoint, err := store.ReadMigrationCheckpoint()
	if err != nil {
		return err
	}
	if checkpoint.Exists != permit.checkpointed {
		return errors.New("config apply changed the release ledger authority")
	}
	if permit.checkpointed {
		ledger = checkpoint
	}
	if ledger.Fingerprint != permit.ledger.Fingerprint ||
		!bytes.Equal(ledger.Payload, permit.ledger.Payload) {
		return errors.New("config apply changed the completed release ledger")
	}
	return nil
}

func configApplyRepairGateShape(outcome releasetransition.Outcome) bool {
	return !outcome.ReachedGoal && outcome.Retry != "" && outcome.Active != "" && outcome.Active == outcome.Target &&
		(outcome.Status == releasetransition.StatusMigrationRequired && outcome.Code == releasetransition.CodeTransitionRequired && outcome.Transaction == nil ||
			configApplyRepairRustGateShape(outcome))
}

func configApplyRepairRustGateShape(outcome releasetransition.Outcome) bool {
	return outcome.Status == releasetransition.StatusRecovering && outcome.Code == releasetransition.CodeRecoveryPending &&
		outcome.Transaction != nil && *outcome.Transaction != ""
}

// The caller obtains this inspection from the verified owner's frozen reader.
// A cleanup admission is a durable continuation, never source-edit authority.
func configApplyRepairRustInspectionMatches(journal releasetransition.JournalRecord, outcome releasetransition.Outcome, inspection *releasetransition.Inspection, profileInit bool) bool {
	journalMatches := configApplyRepairJournalMatches(journal, outcome) || profileInit && profileInitRepairRollbackJournalMatches(journal, outcome)
	return configApplyRepairGateShape(outcome) && configApplyRepairRustGateShape(outcome) &&
		journalMatches && inspection != nil && inspection.Resume == nil &&
		inspection.Outcome != nil && sameConfigApplyGate(outcome, *inspection.Outcome)
}

func configApplyRepairJournalMatches(
	journal releasetransition.JournalRecord,
	outcome releasetransition.Outcome,
) bool {
	return journal.Checkpoint == releasetransition.JournalComplete &&
		journal.Goal.Direction == releasetransition.DirectionActivateTarget &&
		journal.Goal.Target == outcome.Target && journal.Goal.Target == outcome.Active &&
		(!configApplyRepairRustGateShape(outcome) || *outcome.Transaction == journal.Transaction)
}

func profileInitRepairRollbackJournalMatches(
	journal releasetransition.JournalRecord,
	outcome releasetransition.Outcome,
) bool {
	return journal.Checkpoint == releasetransition.JournalComplete &&
		journal.Goal.Direction == releasetransition.DirectionActivatePrevious &&
		journal.Goal.Target == outcome.Target && journal.Goal.Target == outcome.Active &&
		(!configApplyRepairRustGateShape(outcome) || *outcome.Transaction == journal.Transaction)
}

func sameConfigApplyReleaseID(
	left, right *releasetransition.ReleaseID,
) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func sameConfigApplyGate(left, right releasetransition.Outcome) bool {
	leftPayload, leftErr := json.Marshal(left)
	rightPayload, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftPayload, rightPayload)
}

func validConfigApplyActivationObservation(
	observation releasetransition.V2ActivationObservation,
) bool {
	for _, fingerprint := range []releasetransition.Fingerprint{
		observation.Actual, observation.Desired,
	} {
		if len(fingerprint) != sha256.Size*2 {
			return false
		}
		if _, err := hex.DecodeString(string(fingerprint)); err != nil {
			return false
		}
	}
	return true
}

func normalizedCompletedConfigApplyPair(
	pair releasetransition.ReleasePair,
) releasetransition.ReleasePair {
	if pair.From != pair.Target {
		previous := pair.From
		pair.Previous = &previous
		pair.From = pair.Target
	}
	return pair
}

func configApplyFactsFingerprint(facts configApplyRepairFacts) (string, error) {
	payload, err := json.Marshal(facts)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}
