package releasetransition

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/Subyard/Subyard/internal/domain"
)

const (
	RollbackTargetContractV1      = "native-history-rollback-target-v1"
	RollbackTargetProcessSchemaV1 = 3
	RollbackTargetActivationStep  = "rollback-target-activation-v1"
)

// RollbackTargetRequest is separate from frozen process V1 and recovery V2.
// The single operational mode assesses or performs R activation under S's V2
// journal. NativeHistory is a proposed R-native ledger, never opaque history.
type RollbackTargetRequest struct {
	SchemaVersion       int                     `json:"schemaVersion"`
	Mode                string                  `json:"mode"`
	RuntimeRoot         string                  `json:"runtimeRoot,omitempty"`
	ConfigHome          string                  `json:"configHome,omitempty"`
	Yard                string                  `json:"yard,omitempty"`
	Releases            ReleasePair             `json:"releases"`
	SourceArtifact      Fingerprint             `json:"sourceArtifact,omitempty"`
	SourceRegistry      Fingerprint             `json:"sourceRegistry,omitempty"`
	TargetArtifact      Fingerprint             `json:"targetArtifact,omitempty"`
	TargetRegistry      Fingerprint             `json:"targetRegistry,omitempty"`
	Projection          Fingerprint             `json:"projection,omitempty"`
	InheritedSettingIDs []string                `json:"inheritedSettingIDs,omitempty"`
	NativeHistory       *LedgerV2               `json:"nativeHistory,omitempty"`
	Journal             Fingerprint             `json:"journal,omitempty"`
	Plan                *RollbackActivationPlan `json:"plan,omitempty"`
	Execution           *Execution              `json:"execution,omitempty"`
}

type RollbackActivationScope struct {
	ID      string      `json:"id"`
	Desired Fingerprint `json:"desired"`
}

type RollbackActivationPlan struct {
	Token      PlanToken                 `json:"token"`
	Assessment domain.ActionAssessment   `json:"assessment"`
	Scopes     []RollbackActivationScope `json:"scopes"`
}

type RollbackActivationInspection struct {
	Plan     RollbackActivationPlan `json:"plan"`
	Blockers []Blocker              `json:"blockers"`
}

// RollbackTargetCompletion reports an observation under R's registry. S remains
// the journal owner; later R readiness must observe its activation state again.
type RollbackTargetCompletion struct {
	Contract       string        `json:"contract"`
	Transaction    TransactionID `json:"transaction"`
	Intent         Fingerprint   `json:"intent"`
	Plan           PlanToken     `json:"plan"`
	TargetRegistry Fingerprint   `json:"targetRegistry"`
	TargetArtifact Fingerprint   `json:"targetArtifact"`
	Checkpoint     Fingerprint   `json:"checkpoint"`
	Projection     Fingerprint   `json:"projection"`
	NativeHistory  Fingerprint   `json:"nativeHistory"`
}

type RollbackTargetResponse struct {
	SchemaVersion int                           `json:"schemaVersion"`
	Contract      string                        `json:"contract,omitempty"`
	Inspection    *RollbackActivationInspection `json:"inspection,omitempty"`
	Completion    *RollbackTargetCompletion     `json:"completion,omitempty"`
	Outcome       *Outcome                      `json:"outcome,omitempty"`
}

func ParseRollbackTargetRequest(payload []byte) (RollbackTargetRequest, error) {
	var request RollbackTargetRequest
	if err := decodeBoundedRecord(payload, MaxJournalBytes, &request); err != nil {
		return request, err
	}
	return request, request.Validate()
}

func (request RollbackTargetRequest) Validate() error {
	if request.SchemaVersion != RollbackTargetProcessSchemaV1 {
		return invalid("unsupported rollback target process schema")
	}
	if request.Mode == "capabilities" {
		expected := RollbackTargetRequest{SchemaVersion: RollbackTargetProcessSchemaV1, Mode: "capabilities"}
		actual, _ := json.Marshal(request)
		empty, _ := json.Marshal(expected)
		if string(actual) != string(empty) {
			return invalid("rollback capability probe carries transition inputs")
		}
		return nil
	}
	if request.Mode != "activate-under-v2" {
		return invalid("unsupported rollback target mode")
	}
	for _, root := range []string{request.RuntimeRoot, request.ConfigHome} {
		if err := validateSourceIngressRolePath(root); err != nil {
			return err
		}
	}
	if err := request.Releases.Validate(); err != nil {
		return err
	}
	if request.Releases.From == request.Releases.Target {
		return invalid("foreign rollback requires distinct endpoints")
	}
	if request.Yard != "" {
		if err := validateSafeID(request.Yard, "rollback yard"); err != nil {
			return err
		}
	}
	for _, pin := range []Fingerprint{request.SourceArtifact, request.SourceRegistry, request.TargetArtifact, request.TargetRegistry, request.Projection} {
		if err := validateFingerprint(pin, "rollback target binding"); err != nil {
			return err
		}
	}
	if len(request.InheritedSettingIDs) > MaxPlanItems {
		return invalid("too many inherited rollback settings")
	}
	seenIDs := map[string]bool{}
	for _, id := range request.InheritedSettingIDs {
		if err := validateSafeID(id, "inherited rollback setting"); err != nil {
			return err
		}
		if seenIDs[id] {
			return invalid("duplicate inherited rollback setting")
		}
		seenIDs[id] = true
	}
	if request.NativeHistory == nil {
		return invalid("rollback target native history is required")
	}
	if request.Journal != "" {
		if err := validateFingerprint(request.Journal, "rollback journal"); err != nil {
			return err
		}
	}
	if request.Execution != nil {
		if request.Journal == "" || request.Plan == nil {
			return invalid("rollback target execution has no journal or assessed plan")
		}
		if err := validatePlanToken(request.Execution.Plan); err != nil {
			return err
		}
		if err := validateText(string(request.Execution.Authorization), "rollback authorization", maxDiagnosticText, true); err != nil {
			return err
		}
	} else if request.Plan != nil {
		return invalid("rollback target inspection carries an execution plan")
	}
	if request.Plan != nil {
		return request.Plan.validate()
	}
	return nil
}

func (plan RollbackActivationPlan) validate() error {
	if err := validatePlanToken(plan.Token); err != nil {
		return err
	}
	if err := validateAssessmentShape(plan.Assessment); err != nil {
		return err
	}
	if len(plan.Scopes) > MaxPlanItems {
		return invalid("rollback target activation scope exceeds its bound")
	}
	seen := map[string]bool{}
	for _, scope := range plan.Scopes {
		if err := validateSafeID(scope.ID, "rollback activation owner"); err != nil {
			return err
		}
		if seen[scope.ID] {
			return invalid("duplicate rollback activation owner")
		}
		seen[scope.ID] = true
		if err := validateFingerprint(scope.Desired, "rollback activation desired state"); err != nil {
			return err
		}
	}
	return nil
}

func (transition *V2Transition) rollbackActivationPlan(request RollbackTargetRequest, assessment domain.ActionAssessment, scopes []RollbackActivationScope) (PlanToken, error) {
	_, history, err := MarshalLedgerV2(*request.NativeHistory, transition.registry)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		Contract                                                                            string
		RuntimeRoot, ConfigHome, Yard                                                       string
		Releases                                                                            ReleasePair
		SourceArtifact, SourceRegistry, TargetArtifact, TargetRegistry, Projection, History Fingerprint
		InheritedSettingIDs                                                                 []string
		Assessment                                                                          domain.ActionAssessment
		Scopes                                                                              []RollbackActivationScope
	}{RollbackTargetContractV1, request.RuntimeRoot, request.ConfigHome, request.Yard, request.Releases, request.SourceArtifact, request.SourceRegistry, request.TargetArtifact, request.TargetRegistry, request.Projection, history, request.InheritedSettingIDs, assessment, scopes})
	if err != nil {
		return "", err
	}
	return PlanToken("plan-v1-" + string(fingerprintPayload(payload))), nil
}

func (transition *V2Transition) InspectRollbackTargetActivation(ctx context.Context, request RollbackTargetRequest) (RollbackActivationInspection, error) {
	ctx, flushDiagnostics := captureMigrationDiagnostics(ctx)
	defer flushDiagnostics()
	if err := request.Validate(); err != nil {
		return RollbackActivationInspection{}, err
	}
	if transition == nil || request.Mode != "activate-under-v2" || request.TargetArtifact != transition.options.ArtifactDigest || request.TargetRegistry != transition.registryDigest || !releasePairsEqual(request.Releases, transition.options.Releases) {
		return RollbackActivationInspection{}, invalid("rollback target activation belongs to another release")
	}
	owner, authority, err := transition.registry.historyAuthority(*request.NativeHistory)
	if err != nil {
		return RollbackActivationInspection{}, err
	}
	if _, err := owner.Complete(authority); err != nil {
		return RollbackActivationInspection{}, invalid("rollback preview is not complete native target history")
	}
	links, err := transition.options.ObserveLinks(ctx)
	if err != nil {
		return RollbackActivationInspection{}, err
	}
	if !initialReleaseLinks(links, request.Releases) && !activatedReleaseLinks(links, request.Releases) {
		return RollbackActivationInspection{}, invalid("rollback activation links are outside the selected release pair")
	}
	observation := v2Observation{goal: Goal{Target: request.Releases.Target, Direction: DirectionActivatePrevious}, links: links, activationFixed: true}
	if err := transition.observeActivation(ctx, &observation); err != nil {
		return RollbackActivationInspection{}, err
	}
	assessment, err := assessV2Action(transition.policy, true, false, observation.activationConsequences)
	if err != nil {
		return RollbackActivationInspection{}, err
	}
	plan := RollbackActivationPlan{Assessment: assessment, Scopes: []RollbackActivationScope{}}
	for _, scope := range observation.activationScope {
		plan.Scopes = append(plan.Scopes, RollbackActivationScope(scope))
	}
	plan.Token, err = transition.rollbackActivationPlan(request, plan.Assessment, plan.Scopes)
	if err != nil {
		return RollbackActivationInspection{}, err
	}
	return RollbackActivationInspection{Plan: plan, Blockers: slices.Clone(observation.blockers)}, nil
}

// ConvergeRollbackTargetActivation holds no second lock and writes no journal,
// ledger, checkpoint, or links. S supplies the grant and owns durable progress.
func (transition *V2Transition) ConvergeRollbackTargetActivation(ctx context.Context, request RollbackTargetRequest) (RollbackTargetResponse, error) {
	inspected, err := transition.InspectRollbackTargetActivation(ctx, request)
	if err != nil {
		return RollbackTargetResponse{}, err
	}
	if request.Execution == nil || request.Plan == nil {
		return RollbackTargetResponse{}, invalid("rollback target activation requires execution")
	}
	if _, _, err := transition.policy.Resolve(request.Plan.Assessment); err != nil {
		return RollbackTargetResponse{}, err
	}
	token, err := transition.rollbackActivationPlan(request, request.Plan.Assessment, request.Plan.Scopes)
	if err != nil || token != request.Plan.Token || request.Execution.Plan != token {
		return RollbackTargetResponse{}, invalid("rollback target assessment binding changed")
	}
	if len(inspected.Blockers) != 0 {
		return RollbackTargetResponse{SchemaVersion: RollbackTargetProcessSchemaV1, Inspection: &inspected}, nil
	}
	if len(inspected.Plan.Scopes) != len(request.Plan.Scopes) {
		return RollbackTargetResponse{}, invalid("rollback activation owner set changed")
	}
	for index, scope := range inspected.Plan.Scopes {
		if scope != request.Plan.Scopes[index] {
			return RollbackTargetResponse{}, invalid("rollback activation desired state changed")
		}
	}
	for _, consequence := range inspected.Plan.Assessment.Consequences {
		if !slices.Contains(request.Plan.Assessment.Consequences, consequence) {
			return RollbackTargetResponse{}, invalid("rollback activation has an unassessed consequence")
		}
	}
	snapshot, err := transition.store.ReadCurrentJournal()
	if err != nil {
		return RollbackTargetResponse{}, err
	}
	if !snapshot.Exists || snapshot.Fingerprint != request.Journal {
		return RollbackTargetResponse{}, ErrProtectedStoreStale
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil {
		return RollbackTargetResponse{}, err
	}
	if journal.SourceIngress != nil || IsRecoveryTransaction(journal.Transaction) || journal.RegistryDigest != request.SourceRegistry || journal.ArtifactDigest != request.TargetArtifact || journal.Goal.Direction != DirectionActivatePrevious || !releasePairsEqual(journal.Releases, request.Releases) || journal.Checkpoint != JournalReconciling {
		return RollbackTargetResponse{}, invalid("rollback activation is not owned by S's reconciling journal")
	}
	if err := rollbackRequestSourceOwner(request).CheckJournal(journal); err != nil {
		return RollbackTargetResponse{}, err
	}
	bound := false
	for _, step := range journal.Steps {
		if step.ID == RollbackTargetActivationStep && step.Migration == RollbackTargetActivationStep && step.Resource == "activation.rollback-target-plan" && step.Checkpoint == StepVerified && step.Decision == DecisionPreserve && step.Expected == step.Desired && step.Desired == fingerprintPayload([]byte(token)) {
			bound = true
		}
	}
	if !bound || fingerprintPayload([]byte(request.Execution.Authorization)) != journal.AuthorizationDigest || !transition.options.VerifyAuthorization(request.Execution.Plan, request.Execution.Authorization) {
		return RollbackTargetResponse{}, invalid("rollback activation grant is not bound to S's journal")
	}
	state, err := transition.store.ReadMigrationLedger(transition.registry, transition.registryDigest)
	if err != nil {
		return RollbackTargetResponse{}, err
	}
	if !state.Checkpointed || state.ProjectionSnapshot.Fingerprint != request.Projection || !sameLedgerV2(state.Ledger, *request.NativeHistory) {
		return RollbackTargetResponse{}, invalid("rollback target does not have its assessed native history")
	}
	scopes := make([]v2ActivationScope, len(request.Plan.Scopes))
	for index, scope := range request.Plan.Scopes {
		scopes[index] = v2ActivationScope(scope)
	}
	outcome, pending, err := transition.reconcileActivation(ctx, journal, scopes)
	if err != nil {
		return RollbackTargetResponse{}, err
	}
	if outcome.Code != "" || pending {
		if outcome.Code == "" {
			outcome = v2RecoveringOutcome(ReleaseLinks{Active: journal.Releases.Target, Previous: releaseIDPointer(journal.Releases.From)}, journal.Goal.Target, transactionIDPointer(journal.Transaction), CodeRecoveryPending, "rollback activation has pending configuration")
		}
		return RollbackTargetResponse{SchemaVersion: RollbackTargetProcessSchemaV1, Outcome: &outcome}, nil
	}
	after, err := transition.store.ReadCurrentJournal()
	if err != nil {
		return RollbackTargetResponse{}, err
	}
	if !sameProtectedSnapshot(snapshot, after) {
		return RollbackTargetResponse{}, ErrProtectedStoreStale
	}
	final, err := transition.store.ReadMigrationLedger(transition.registry, transition.registryDigest)
	if err != nil {
		return RollbackTargetResponse{}, err
	}
	if !sameProtectedSnapshot(state.AuthoritySnapshot, final.AuthoritySnapshot) || !sameProtectedSnapshot(state.ProjectionSnapshot, final.ProjectionSnapshot) {
		return RollbackTargetResponse{}, fmt.Errorf("%w: target activation changed migration history", ErrProtectedStoreStale)
	}
	_, history, err := MarshalLedgerV2(final.Ledger, transition.registry)
	if err != nil {
		return RollbackTargetResponse{}, err
	}
	links, err := transition.options.ObserveLinks(ctx)
	if err != nil {
		return RollbackTargetResponse{}, err
	}
	proof, err := transition.observedReadyProof(v2Observation{goal: journal.Goal, links: links, ledger: final.Ledger, activationFixed: true})
	if err != nil {
		return RollbackTargetResponse{}, err
	}
	if _, err := transition.readyResult(proof); err != nil {
		return RollbackTargetResponse{}, err
	}
	completion := RollbackTargetCompletion{RollbackTargetContractV1, journal.Transaction, journal.IntentDigest, token, transition.registryDigest, request.TargetArtifact, final.AuthoritySnapshot.Fingerprint, request.Projection, history}
	return RollbackTargetResponse{SchemaVersion: RollbackTargetProcessSchemaV1, Completion: &completion}, nil
}

func ParseRollbackTargetResponse(payload []byte) (RollbackTargetResponse, error) {
	var response RollbackTargetResponse
	if err := decodeBoundedRecord(payload, MaxJournalBytes, &response); err != nil {
		return response, err
	}
	return response, response.Validate()
}

func (response RollbackTargetResponse) Validate() error {
	if response.SchemaVersion != RollbackTargetProcessSchemaV1 {
		return invalid("unsupported rollback target response schema")
	}
	variants := 0
	if response.Contract != "" {
		variants++
		if response.Contract != RollbackTargetContractV1 {
			return invalid("unknown rollback target contract")
		}
	}
	if response.Inspection != nil {
		variants++
		if err := response.Inspection.Plan.validate(); err != nil {
			return err
		}
		if len(response.Inspection.Blockers) > MaxPlanItems {
			return invalid("too many rollback target refusals")
		}
		for _, blocker := range response.Inspection.Blockers {
			if err := blocker.Validate(); err != nil {
				return err
			}
		}
	}
	if response.Completion != nil {
		variants++
		completion := response.Completion
		if completion.Contract != RollbackTargetContractV1 {
			return invalid("unknown rollback completion contract")
		}
		if err := validateTransactionID(completion.Transaction); err != nil {
			return err
		}
		if err := validatePlanToken(completion.Plan); err != nil {
			return err
		}
		for _, pin := range []Fingerprint{completion.Intent, completion.TargetRegistry, completion.TargetArtifact, completion.Checkpoint, completion.Projection, completion.NativeHistory} {
			if err := validateFingerprint(pin, "rollback target completion"); err != nil {
				return err
			}
		}
	}
	if response.Outcome != nil {
		variants++
		if err := ValidateProcessOutcome(Goal{Target: response.Outcome.Target, Direction: DirectionActivatePrevious}, *response.Outcome); err != nil {
			return err
		}
	}
	if variants != 1 {
		return invalid("rollback target response must contain exactly one result")
	}
	return nil
}
