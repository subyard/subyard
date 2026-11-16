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
	"syscall"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
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
	pendingInputs       bool
}

type configApplyRepairFacts struct {
	CleanupTooling   string                        `json:"cleanup_tooling,omitempty"`
	Journal          releasetransition.Fingerprint `json:"journal"`
	Ledger           releasetransition.Fingerprint `json:"ledger"`
	Target           releasetransition.ReleaseID   `json:"target"`
	Direction        releasetransition.Direction   `json:"direction"`
	OtherActivations []configApplyActivationFact   `json:"other_activations"`
	ConfigTargets    []configApplyTargetFact       `json:"config_targets"`
}

type configApplyActivationFact struct {
	ID        string                        `json:"id"`
	Actual    releasetransition.Fingerprint `json:"actual"`
	Desired   releasetransition.Fingerprint `json:"desired"`
	Converged bool                          `json:"converged"`
}

type configApplyTargetFact struct {
	Name               string `json:"name"`
	DesiredFingerprint string `json:"desired_fingerprint"`
	IntegrationPlan    string `json:"integration_plan,omitempty"`
}

// Each permit names its bounded operation; refresh keeps that mode and only relaxes drift.
type activationRepairMode struct {
	requireDrift  bool
	selectedNames []string
}

func (cli *CLI) prepareActivationRepair(
	ctx context.Context, yard string, allLocal bool, outcome releasetransition.Outcome,
	mode activationRepairMode,
) (*configApplyRepairPermit, error) {
	requireDrift, selectedNames := mode.requireDrift, mode.selectedNames
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil {
		return nil, err
	}
	if !available {
		return nil, nil
	}
	if !configApplyRepairGateShape(outcome) {
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
	if err != nil || !configApplyRepairJournalMatches(journal, outcome) {
		return nil, nil
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
	loaded, err := operation.resolveReleaseTransitionContext(ctx, yard, options.ConfigHome)
	if err != nil {
		return nil, nil
	}
	allTargets, err := operation.releaseConfigTargets(ctx, loaded, true)
	if err != nil {
		return nil, nil
	}
	requested := make(map[string]struct{})
	if selectedNames == nil {
		requestedTargets, err := operation.releaseConfigTargets(ctx, loaded, allLocal)
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
		Direction: journal.Goal.Direction,
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
		facts.ConfigTargets = append(facts.ConfigTargets, targetFact)
		if _, selected := requested[target.Name]; selected {
			requestedDesired[target.Name] = assessment.DesiredFingerprint
		}
		changed := assessment.Changed
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
	pendingInputs := false
	for _, reconciler := range cli.nonConfigActivationReconcilers(request) {
		observation, observeErr := reconciler.Observe(ctx, pair, observedLinks)
		if observeErr != nil || !validConfigApplyActivationObservation(observation) {
			return nil, nil
		}
		if owner, ok := reconciler.(*profileRuntimeActivationReconciler); ok && !observation.Converged {
			pending := false
			for _, target := range allTargets {
				state, err := owner.observeTarget(ctx, &operation, target)
				if err != nil {
					return nil, nil
				}
				switch state.State {
				case ports.RuntimeStatePending:
					if _, selected := requested[target.Name]; !selected {
						return nil, nil
					}
					pending = true
				case ports.RuntimeStateCurrent, ports.RuntimeStateAbsent, ports.RuntimeStateDeferred:
				default:
					return nil, nil
				}
				binding, err := activationStageFingerprint(state)
				if err != nil {
					return nil, err
				}
				facts.OtherActivations = append(facts.OtherActivations, configApplyActivationFact{ID: owner.ID() + "/" + target.Name, Actual: binding, Desired: binding, Converged: state.State != ports.RuntimeStatePending})
			}
			if pending {
				pendingInputs = true
				continue
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
		pendingInputs:       pendingInputs,
	}, nil
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
	refreshed, err := cli.prepareActivationRepair(
		ctx, permit.yard, permit.allLocal, permit.gate, activationRepairMode{selectedNames: permit.selectedTargetNames},
	)
	if err != nil || refreshed == nil ||
		refreshed.factsFingerprint != permit.factsFingerprint ||
		refreshed.journal.Fingerprint != permit.journal.Fingerprint ||
		!bytes.Equal(refreshed.journal.Payload, permit.journal.Payload) ||
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

// finishConfigApplyRepair verifies the readiness a repair permit restored. The
// returned pending configuration remains for yards' own inits; the operation
// boundary reports it. An init must have resolved its own yard's items.
func (cli *CLI) finishConfigApplyRepair(
	ctx context.Context,
	permit *configApplyRepairPermit,
) ([]releasetransition.Blocker, error) {
	if err := cli.checkConfigApplyRepairHistory(permit); err != nil {
		return nil, err
	}
	outcome, inspection, err := cli.inspectMutationGateInspection(ctx, permit.yard)
	if err != nil {
		return nil, err
	}
	if outcome != nil {
		if gate, ok := cli.yardScopedGate(outcome, inspection); ok && !gate.state(permit.yard).repairOnly() {
			return gate.otherYardFindings(permit.yard), nil
		}
	}
	if outcome != nil {
		if permit.pendingInputs && outcome.Status == releasetransition.StatusMigrationRequired &&
			outcome.Code == releasetransition.CodeTransitionRequired && outcome.Active == permit.gate.Active && outcome.Target == permit.gate.Target {
			fmt.Fprintln(cli.options.Stdout, "Materialized configuration repaired; next: run yard migrate")
			return nil, nil
		}
		return nil, fmt.Errorf("materialized configuration did not restore release readiness: %s (%s); next: %s", outcome.Code, outcome.Message, releaseruntime.CurrentReleaseRetry(*outcome))
	}
	return nil, nil
}

// reportStillPending renders pending configuration that waits for yards' inits.
func reportStillPending(output io.Writer, pending []releasetransition.Blocker) {
	for _, blocker := range pending {
		fmt.Fprintf(output, "Still pending: %s; next: %s\n", blocker.Message, blocker.Retry)
	}
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

// A typed pending-configuration gate whose every blocker names plain init of a
// yard as its next step is repaired, one yard at a time, by the init of a yard
// it names. All other repair guards still apply; the pending item counts as the
// selected yard's drift. It returns the other yards that wait for their inits.
func configApplyRepairGateShape(outcome releasetransition.Outcome) bool {
	return !outcome.ReachedGoal && outcome.Retry != "" && outcome.Active != "" && outcome.Active == outcome.Target &&
		outcome.Status == releasetransition.StatusMigrationRequired && outcome.Code == releasetransition.CodeTransitionRequired && outcome.Transaction == nil
}

func configApplyRepairJournalMatches(
	journal releasetransition.JournalRecord,
	outcome releasetransition.Outcome,
) bool {
	return journal.Checkpoint == releasetransition.JournalComplete &&
		journal.Goal.Direction == releasetransition.DirectionActivateTarget &&
		journal.Goal.Target == outcome.Target && journal.Goal.Target == outcome.Active
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
