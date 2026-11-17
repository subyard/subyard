package releasetransition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
)

// SourceRollbackActivation is the assessed R operation stored as recovery
// evidence. Its exact bytes and plan token are pinned by S's V2 journal.
type SourceRollbackActivation struct {
	Request        RollbackTargetRequest  `json:"request"`
	Plan           RollbackActivationPlan `json:"plan"`
	SourceRegistry []byte                 `json:"sourceRegistry"`
	TargetRegistry []byte                 `json:"targetRegistry"`
}

func (record SourceRollbackActivation) validate() (RegistryV2, error) {
	request := record.Request
	if err := request.Validate(); err != nil {
		return RegistryV2{}, err
	}
	if request.Mode != "activate-under-v2" || request.Plan != nil || request.Execution != nil || request.Journal != "" {
		return RegistryV2{}, invalid("rollback activation evidence contains execution inputs")
	}
	if err := record.Plan.validate(); err != nil {
		return RegistryV2{}, err
	}
	policy, err := newV2ActionPolicy()
	if err != nil {
		return RegistryV2{}, err
	}
	if _, _, err := policy.Resolve(record.Plan.Assessment); err != nil || !record.Plan.Assessment.Changed {
		return RegistryV2{}, invalid("rollback activation evidence has no assessed activation")
	}
	source, sourcePin, err := ParseRegistryV2(record.SourceRegistry, BuiltinCapabilityCatalog())
	if err != nil {
		return RegistryV2{}, err
	}
	target, targetPin, err := ParseRegistryV2Provenance(record.TargetRegistry)
	if err != nil {
		return RegistryV2{}, err
	}
	if sourcePin != request.SourceRegistry || targetPin != request.TargetRegistry {
		return RegistryV2{}, invalid("rollback activation evidence differs from the sealed registries")
	}
	if err := CheckRollbackTargetPrefixes(source, target); err != nil {
		return RegistryV2{}, err
	}
	pending, err := target.PendingPath(*request.NativeHistory)
	if err != nil || len(pending) != 0 {
		return RegistryV2{}, invalid("rollback activation evidence does not contain complete target history")
	}
	engine := &V2Transition{registry: target}
	token, err := engine.rollbackActivationPlan(request, record.Plan.Assessment, record.Plan.Scopes)
	if err != nil || token != record.Plan.Token {
		return RegistryV2{}, invalid("rollback activation evidence differs from its assessed plan")
	}
	return target, nil
}

// StartSourceRollbackActivation selects an already authorized rollback ready
// for link activation. This path is for a cut that is already target-native;
// the caller supplies the accepted source plan and grant.
func (store *POSIXV2Store) StartSourceRollbackActivation(record SourceRollbackActivation, authorization Execution) (JournalRecord, error) {
	target, err := record.validate()
	if err != nil {
		return JournalRecord{}, err
	}
	if authorization.Authorization == "" || validatePlanToken(authorization.Plan) != nil {
		return JournalRecord{}, invalid("rollback activation requires a source authorization")
	}
	request := record.Request
	if request.ConfigHome != store.configHome || request.Releases.Previous == nil || *request.Releases.Previous != request.Releases.Target {
		return JournalRecord{}, invalid("rollback activation requires the selected retained release")
	}
	version, err := ReleaseIDVersion(request.Releases.Target)
	if err != nil {
		return JournalRecord{}, err
	}
	lock, err := store.OpenTransitionLock()
	if err != nil {
		return JournalRecord{}, err
	}
	defer lock.Close()
	selected, err := store.ReadCurrentJournal()
	if err != nil {
		return JournalRecord{}, err
	}
	if selected.Exists {
		return JournalRecord{}, ErrProtectedStoreStale
	}
	links, err := NewRuntimeLinkStore(request.RuntimeRoot)
	if err != nil {
		return JournalRecord{}, err
	}
	observed, err := links.Observe()
	if err != nil {
		return JournalRecord{}, err
	}
	if !initialReleaseLinks(observed, request.Releases) {
		return JournalRecord{}, ErrProtectedStoreStale
	}
	state, err := store.checkSourceActivationHistory(record, target)
	if err != nil {
		return JournalRecord{}, err
	}
	owner := rollbackRequestSourceOwner(request)
	ownerPin, err := owner.Fingerprint()
	if err != nil {
		return JournalRecord{}, err
	}
	history := rollbackHistoryOrigin{SchemaVersion: 1, Owner: owner, SourceRegistry: record.SourceRegistry, TargetRegistry: record.TargetRegistry, Projection: state.ProjectionSnapshot, Checkpoint: state.AuthoritySnapshot}
	origin, err := history.marshal()
	if err != nil {
		return JournalRecord{}, err
	}
	activation, err := json.Marshal(record)
	if err != nil {
		return JournalRecord{}, err
	}
	originPin := fingerprintPayload(origin)
	scope := fingerprintPayload(activation)
	transaction := defaultV2TransactionID()
	journal := JournalRecord{
		SchemaVersion: JournalSchemaV2, Transaction: transaction, Releases: request.Releases,
		Goal:              Goal{Target: request.Releases.Target, Direction: DirectionActivatePrevious},
		AuthorizationPlan: authorization.Plan, AuthorizationDigest: fingerprintPayload([]byte(authorization.Authorization)),
		RegistryDigest: request.SourceRegistry, ArtifactDigest: request.TargetArtifact,
		CatalogDigest: BuiltinCapabilityCatalog().Digest(), ObservationScope: scope, Checkpoint: JournalActivationIntent,
	}
	type binding struct {
		id, resource string
		pin          Fingerprint
		recovery     []byte
	}
	bindings := []binding{
		{RollbackSourceOwnerStep, "rollback.source-owner", ownerPin, nil},
		{rollbackSourcePlanStep, "rollback.source-plan", originPin, origin},
		{RollbackTargetActivationStep, "activation.rollback-target-plan", fingerprintPayload([]byte(record.Plan.Token)), activation},
	}
	var intents []PlannerStepIntent
	for _, value := range bindings {
		var recovery Fingerprint
		if value.recovery != nil {
			if err := store.CreateRecovery(transaction, value.id, value.recovery); err != nil {
				return JournalRecord{}, err
			}
			recovery = fingerprintPayload(value.recovery)
		}
		intent := PlannerStepIntent{ID: value.id, Migration: value.id, Resource: value.resource, Decision: DecisionPreserve, Expected: value.pin, Desired: value.pin}
		intents = append(intents, intent)
		step := JournalStep{ID: intent.ID, Migration: intent.Migration, Resource: intent.Resource, Decision: intent.Decision, Expected: intent.Expected, Desired: intent.Desired, Checkpoint: StepVerified}
		for _, phase := range []EvidenceCheckpoint{EvidenceCaptured, EvidenceApplied, EvidenceVerified} {
			evidence := evidenceFor(journal, step, phase, value.pin, recovery)
			payload, err := MarshalEvidence(evidence)
			if err != nil {
				return JournalRecord{}, err
			}
			if err := store.CreateCheckpointEvidence(transaction, step.ID, phase, payload); err != nil {
				return JournalRecord{}, err
			}
			step.Evidence = &evidence
		}
		journal.Steps = append(journal.Steps, step)
	}
	journal.ResumePlan, err = BindResumePlan(ResumePlanFacts{
		Goal: journal.Goal, Releases: journal.Releases, ArtifactDigest: journal.ArtifactDigest, RegistryDigest: journal.RegistryDigest,
		CatalogDigest: journal.CatalogDigest, RollbackTarget: &RollbackTarget{Version: version.String(), RegistryDigest: request.TargetRegistry},
		ObservationScope: scope, Assessment: record.Plan.Assessment, Intents: intents,
		Transaction: transaction, AuthorizationPlan: authorization.Plan,
	})
	if err != nil {
		return JournalRecord{}, err
	}
	journal.IntentDigest = bindJournalIntent(journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps)
	payload, err := MarshalJournal(journal)
	if err != nil {
		return JournalRecord{}, err
	}
	return journal, store.CompareAndSwapCurrentJournal(selected, payload)
}

func (store *POSIXV2Store) checkSourceActivationHistory(record SourceRollbackActivation, target RegistryV2) (MigrationLedgerSnapshot, error) {
	state, err := store.ReadMigrationLedger(target, record.Request.TargetRegistry)
	if err != nil {
		return state, err
	}
	if !state.Checkpointed || state.ProjectionSnapshot.Fingerprint != record.Request.Projection || !sameLedgerV2(state.Ledger, *record.Request.NativeHistory) {
		return state, invalid("rollback activation history changed")
	}
	return state, nil
}

func (store *POSIXV2Store) readSourceRollbackActivation(journal JournalRecord) (SourceRollbackActivation, MigrationLedgerSnapshot, error) {
	var record SourceRollbackActivation
	for _, step := range journal.Steps {
		if step.ID != RollbackTargetActivationStep {
			continue
		}
		if step.Migration != step.ID || step.Resource != "activation.rollback-target-plan" || step.Decision != DecisionPreserve ||
			step.Checkpoint != StepVerified || step.Expected != step.Desired || step.Evidence.Recovery == "" {
			return record, MigrationLedgerSnapshot{}, invalid("rollback recovery has no verified activation plan")
		}
		if err := store.checkRewindStepEvidence(journal, step); err != nil {
			return record, MigrationLedgerSnapshot{}, err
		}
		snapshot, err := store.ReadRecovery(journal.Transaction, step.ID)
		if err != nil {
			return record, MigrationLedgerSnapshot{}, err
		}
		if !snapshot.Exists || snapshot.Fingerprint != step.Evidence.Recovery {
			return record, MigrationLedgerSnapshot{}, invalid("rollback activation recovery bytes changed")
		}
		if err := decodeBoundedRecord(snapshot.Payload, MaxProtectedRecordBytes, &record); err != nil {
			return record, MigrationLedgerSnapshot{}, err
		}
		canonical, err := json.Marshal(record)
		if err != nil || !bytes.Equal(canonical, snapshot.Payload) || step.Desired != fingerprintPayload([]byte(record.Plan.Token)) {
			return record, MigrationLedgerSnapshot{}, invalid("rollback activation recovery bytes differ from the journaled plan")
		}
		target, err := record.validate()
		if err != nil {
			return record, MigrationLedgerSnapshot{}, err
		}
		if err := rollbackRequestSourceOwner(record.Request).CheckJournal(journal); err != nil {
			return record, MigrationLedgerSnapshot{}, err
		}
		state, err := store.checkSourceActivationHistory(record, target)
		return record, state, err
	}
	return record, MigrationLedgerSnapshot{}, invalid("rollback recovery has no assessed target activation")
}

// ProcessSourceRollbackActivation implements S's protected recovery path for
// the activation phases. The V1 updater retains its original resume protocol.
func (store *POSIXV2Store) ProcessSourceRollbackActivation(
	ctx context.Context, request ProcessRequest, sourceRoot string,
	activate func(ReleasePair) (ReleaseLinks, error),
	invoke func(context.Context, RollbackTargetRequest, *os.File) (RollbackTargetResponse, error),
) (ProcessResponse, bool, error) {
	selected, err := store.ReadCurrentJournal()
	if err != nil || !selected.Exists {
		return ProcessResponse{}, false, err
	}
	journal, err := ParseJournal(selected.Payload)
	if err != nil {
		return ProcessResponse{}, false, err
	}
	if !HasRollbackSourceOwner(journal) || journal.Checkpoint == JournalComplete ||
		journal.Goal != (Goal{Target: request.Target, Direction: request.Direction}) {
		return ProcessResponse{}, false, nil
	}
	response := ProcessResponse{SchemaVersion: ProcessProtocolSchemaV1, ActivationReconciliationOwned: true}
	if journal.Checkpoint != JournalActivationIntent && journal.Checkpoint != JournalTargetActive && journal.Checkpoint != JournalReconciling {
		return response, true, invalid("rollback activation recovery requires a verified history cut")
	}
	var lock *os.File
	if request.Mode == ProcessConverge {
		lock, err = store.OpenTransitionLock()
		if err != nil {
			return response, true, err
		}
		defer lock.Close()
		current, err := store.ReadCurrentJournal()
		if err != nil {
			return response, true, err
		}
		if !sameProtectedSnapshot(current, selected) || request.Execution == nil || request.Execution.Authorization != "" || request.Execution.Plan != journal.ResumePlan {
			return response, true, ErrProtectedStoreStale
		}
	}
	record, state, err := store.readSourceRollbackActivation(journal)
	if err != nil {
		return response, true, err
	}
	input := record.Request
	if input.ConfigHome != store.configHome || input.RuntimeRoot != request.RuntimeRoot || request.ArtifactDigest != input.TargetArtifact ||
		request.RegistryDigest != input.SourceRegistry || request.Yard != input.Yard || !slices.Equal(request.InheritedSettingIDs, input.InheritedSettingIDs) ||
		journal.AuthorizationDigest == fingerprintPayload(nil) || request.SourceIngress != nil || request.Replacement != nil {
		return response, true, invalid("rollback recovery differs from its source authorization")
	}
	source, err := os.Stat(sourceRoot)
	if err != nil {
		return response, true, err
	}
	verified, err := os.Stat(filepath.Join(input.RuntimeRoot, "releases", string(journal.Releases.From)))
	if err != nil || !os.SameFile(source, verified) {
		return response, true, invalid("rollback recovery must run from the selected source image")
	}
	links, err := NewRuntimeLinkStore(request.RuntimeRoot)
	if err != nil {
		return response, true, err
	}
	observed, err := links.Observe()
	if err != nil {
		return response, true, err
	}
	if !activatedReleaseLinks(observed, journal.Releases) && (journal.Checkpoint != JournalActivationIntent || !initialReleaseLinks(observed, journal.Releases)) {
		return response, true, ErrProtectedStoreStale
	}
	if request.Mode == ProcessInspect {
		outcome := v2RecoveringOutcome(observed, journal.Goal.Target, &journal.Transaction, CodeRecoveryPending, "resume the authorized rollback activation")
		response.Inspection = &Inspection{Plan: journal.ResumePlan, Assessment: record.Plan.Assessment, Resume: &journal.Transaction, Outcome: &outcome}
		return response, true, ValidateProcessInspection(journal.Goal, *response.Inspection)
	}
	if lock == nil || activate == nil || invoke == nil {
		return response, true, invalid("rollback recovery requires an authorized activation runner")
	}
	if err := store.sourceTransactionArtifacts(journal); err != nil {
		return response, true, err
	}
	if journal.Checkpoint == JournalActivationIntent {
		observed, err = activate(journal.Releases)
		if err != nil {
			return response, true, err
		}
		if !activatedReleaseLinks(observed, journal.Releases) {
			return response, true, ErrProtectedStoreStale
		}
	}
	if journal.Checkpoint != JournalReconciling {
		journal.Checkpoint = JournalReconciling
		payload, err := MarshalJournal(journal)
		if err != nil {
			return response, true, err
		}
		if err := store.CompareAndSwapCurrentJournal(selected, payload); err != nil {
			return response, true, err
		}
		selected = protectedSnapshotFromPayload(payload)
	}
	input.Journal, input.Plan = selected.Fingerprint, &record.Plan
	input.Execution = &Execution{Plan: record.Plan.Token}
	converged, err := invoke(ctx, input, lock)
	if err != nil {
		return response, true, err
	}
	if converged.Outcome != nil {
		response.Outcome = converged.Outcome
		return response, true, nil
	}
	if converged.Completion == nil {
		return response, true, invalid("retained rollback target did not finish activation")
	}
	finalCheckpoint, checkpointErr := store.ReadMigrationCheckpoint()
	finalProjection, projectionErr := store.ReadLedger()
	observed, err = links.Observe()
	if err := errors.Join(checkpointErr, projectionErr, err); err != nil {
		return response, true, err
	}
	if !sameProtectedSnapshot(state.AuthoritySnapshot, finalCheckpoint) || converged.Completion.Checkpoint != finalCheckpoint.Fingerprint ||
		!sameProtectedSnapshot(state.ProjectionSnapshot, finalProjection) || !activatedReleaseLinks(observed, journal.Releases) {
		return response, true, ErrProtectedStoreStale
	}
	journal.Checkpoint = JournalComplete
	payload, err := MarshalJournal(journal)
	if err != nil {
		return response, true, err
	}
	inventory, err := store.rollbackArtifactInventory(journal.Transaction)
	if err != nil {
		return response, true, err
	}
	closure := rollbackClosureV1{
		SchemaVersion: 1, Transaction: journal.Transaction, Releases: journal.Releases,
		SourceArtifact: input.SourceArtifact, SourceRegistry: record.SourceRegistry, TargetRegistry: record.TargetRegistry,
		RollbackJournal: payload, ActivationRequest: record.Request, ActivationPlan: record.Plan, Completion: *converged.Completion, Artifacts: inventory,
	}
	closed, err := marshalRollbackClosure(closure)
	if err != nil {
		return response, true, err
	}
	if err := store.createImmutable(rollbackClosureParts, string(journal.Transaction)+".json", closed); err != nil {
		return response, true, err
	}
	if verified, err := store.readRollbackClosure(journal.Transaction); err != nil {
		return response, true, err
	} else if verified == nil || verified.Retirement != nil || !bytes.Equal(verified.RollbackJournal, payload) {
		return response, true, ErrProtectedStoreStale
	}
	if err := store.CompareAndSwapCurrentJournal(selected, payload); err != nil {
		return response, true, err
	}
	outcome := v2PublicOutcome(StatusReady, observed, journal.Goal.Target, &journal.Transaction, CodeReady, "retained release activation verified", "")
	outcome.ReachedGoal = true
	response.Outcome = &outcome
	return response, true, nil
}
