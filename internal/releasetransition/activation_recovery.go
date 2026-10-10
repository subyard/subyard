package releasetransition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
)

// V2ActivationRecoveryPlanner proves that an owner's current native plan can
// safely reconcile after an interrupted apply. Observation hashes alone do not.
type V2ActivationRecoveryPlanner interface {
	PrepareActivationRecovery(context.Context, ReleasePair, ReleaseLinks) (Fingerprint, error)
}

type activationRecoveryAssessment struct {
	transition  *V2Transition
	before      JournalRecord
	observation v2Observation
	plans       []RecoveryNativePlan
	inspection  Inspection
	basePlan    PlanToken
	reservation Fingerprint
}

func (transition *V2Transition) activationRecoveryContract() string {
	if transition.options.RecoveryContract == ActivationOnlyRecoveryContractV2 {
		return ActivationOnlyRecoveryContractV2
	}
	return ActivationOnlyRecoveryContractV1
}

func recoveryActiveLinksMatch(links ReleaseLinks, releases ReleasePair) bool {
	if releases.From == releases.Target {
		return initialReleaseLinks(links, releases)
	}
	return activatedReleaseLinks(links, releases)
}

func (transition *V2Transition) InspectActivationRecovery(ctx context.Context, request ActivationOnlyRecoveryRequest) (Inspection, error) {
	assessment, err := transition.assessActivationRecovery(ctx, request)
	return assessment.inspection, err
}

func (transition *V2Transition) assessActivationRecovery(ctx context.Context, request ActivationOnlyRecoveryRequest) (activationRecoveryAssessment, error) {
	var result activationRecoveryAssessment
	contract := transition.activationRecoveryContract()
	if err := request.Validate(); err != nil {
		return result, err
	}
	goal := Goal{Target: transition.options.Releases.Target, Direction: DirectionActivateTarget}
	observation, err := transition.observe(ctx, goal)
	if err != nil {
		return result, err
	}
	originalJournal := observation.journal
	blocked := func(code OutcomeCode, resource, message, retry string) (activationRecoveryAssessment, error) {
		outcome := v2OperatorOutcome(observation.links, goal.Target, transactionIDPointer(request.Transaction), code, message, retry)
		result.inspection = Inspection{Assessment: observation.assessment.Clone(), Blockers: []Blocker{{Code: code, Resource: resource, Message: message, Retry: retry}}, Outcome: &outcome}
		facts := transition.planFacts(observation)
		facts.Blockers = slices.Clone(result.inspection.Blockers)
		facts.Observations = append(facts.Observations, ResourceObservation{Resource: "recovery.predecessor", Class: contract, Fingerprint: request.Fingerprint})
		var bindErr error
		result.inspection.Plan, bindErr = BindPlan(facts)
		if bindErr != nil {
			return result, bindErr
		}
		return result, nil
	}
	before := originalJournal
	if before == nil || before.Transaction != request.Transaction || observation.journalSnapshot.Fingerprint != request.Fingerprint {
		return blocked(CodePlanStale, "transition.recovery", "the selected recovery journal changed", "run yard migrate --check")
	}
	if before.Checkpoint != JournalReconciling || before.Goal.Direction != DirectionActivateTarget ||
		before.SourceIngress != nil || len(before.Steps) != 0 || transition.options.SourceIngress != nil ||
		transition.options.Direction != DirectionActivateTarget || before.Goal != goal ||
		before.ArtifactDigest != transition.options.ArtifactDigest || before.RegistryDigest != transition.registryDigest ||
		before.CatalogDigest != transition.catalog.Digest() || !recoveryActiveLinksMatch(observation.links, before.Releases) ||
		IsRecoveryTransaction(before.Transaction) {
		return blocked(CodeRecoveryAmbiguous, "transition.recovery", "fresh recovery requires an unchanged verified owner and an activation-only reconciling journal; migration steps and replacement chains are excluded", "restore exactly known original inputs to resume, or use supported recovery tooling")
	}
	pending, err := transition.registry.PendingPath(observation.ledger)
	if err != nil {
		return result, err
	}
	if !observation.ledgerSnapshot.Exists || len(pending) != 0 {
		return blocked(CodePreconditionBlocked, "transition.ledger", "fresh activation recovery requires the completed migration ledger", "run yard migrate --check")
	}
	for _, blocker := range observation.blockers {
		if blocker.Code != CodePlanStale || blocker.Resource != "transition.observation-scope" {
			return blocked(blocker.Code, blocker.Resource, blocker.Message, blocker.Retry)
		}
	}
	if observation.observationScope == before.ObservationScope {
		return blocked(CodePlanStale, "transition.recovery", "activation inputs are unchanged; resume the existing authorized transaction", "run yard migrate")
	}
	// Plan against the already active links. The successor never rotates them.
	options := transition.options
	options.Releases = ReleasePair{From: observation.links.Active, Target: observation.links.Active, Previous: cloneReleaseID(observation.links.Previous)}
	fresh := &V2Transition{options: options, store: transition.store, catalog: transition.catalog, registry: transition.registry, registryDigest: transition.registryDigest, policy: transition.policy, cache: make(map[PlanToken]Goal)}
	observation.journal = nil
	observation.blockers = nil
	observation.observations = []ResourceObservation{
		{Resource: "recovery.predecessor", Class: contract, Fingerprint: request.Fingerprint},
		{Resource: "recovery.ledger", Class: "completed-ledger-v2", Fingerprint: observation.ledgerSnapshot.Fingerprint},
	}
	for _, reconciler := range fresh.options.Reconcilers {
		actual, observeErr := reconciler.Observe(ctx, options.Releases, observation.links)
		if observeErr != nil {
			b := activationObservationBlocker(reconciler.ID(), observeErr)
			return blocked(b.Code, b.Resource, b.Message, b.Retry)
		}
		if err := validateActivationObservation(reconciler.ID(), actual); err != nil {
			return result, err
		}
		binding := actual.Actual
		if planner, ok := reconciler.(V2ActivationRecoveryPlanner); ok {
			binding, err = planner.PrepareActivationRecovery(ctx, options.Releases, observation.links)
			if err != nil {
				b := activationObservationBlocker(reconciler.ID(), err)
				return blocked(b.Code, b.Resource, b.Message, b.Retry)
			}
		} else if !actual.Converged {
			return blocked(CodeRecoveryAmbiguous, "activation."+reconciler.ID(), fmt.Sprintf("activation owner %s cannot prove a safe native recovery plan from its current state", reconciler.ID()), "inspect the named activation owner with yard migrate --check and restore exactly known original inputs")
		}
		if err := validateFingerprint(binding, "native activation recovery plan"); err != nil {
			return result, err
		}
		plan := RecoveryNativePlan{ID: reconciler.ID(), Binding: binding, Actual: actual.Actual, Desired: actual.Desired, Converged: actual.Converged}
		result.plans = append(result.plans, plan)
		payload, err := json.Marshal(plan)
		if err != nil {
			return result, err
		}
		observation.observations = append(observation.observations, ResourceObservation{Resource: "activation." + reconciler.ID(), Class: "native-recovery-plan-v1", Fingerprint: fingerprintPayload(payload)})
	}
	sort.Slice(result.plans, func(i, j int) bool { return result.plans[i].ID < result.plans[j].ID })
	consequences := []string{"preserve the interrupted journal as immutable evidence and authorize a new activation-only transaction; keep migration ledger and runtime links"}
	if contract == ActivationOnlyRecoveryContractV2 {
		consequences = append(consequences, "retire only an exact unselected recovery reservation and preserve its authorized cancellation evidence")
	}
	observation.assessment, err = assessV2Action(fresh.policy, true, false, append(consequences, observation.activationConsequences...))
	if err != nil {
		return result, err
	}
	plan, err := BindPlan(fresh.planFacts(observation))
	if err != nil {
		return result, err
	}
	result.basePlan = plan
	if contract == ActivationOnlyRecoveryContractV2 {
		result.reservation, err = transition.store.RecoveryReservation(request, plan)
		if err != nil {
			return blocked(CodeRecoveryAmbiguous, "transition.recovery-reservation", "the existing recovery reservation cannot be safely retired; legacy reservations and referenced or invalid evidence remain protected", "restore exactly known reserved activation inputs, then run yard migrate --check")
		}
		plan, err = BindRecoveryLifecyclePlan(plan, result.reservation)
		if err != nil {
			return result, err
		}
	}
	outcome := v2RecoveringOutcome(observation.links, goal.Target, transactionIDPointer(before.Transaction), CodeRecoveryPending, "changed activation inputs require a fresh recovery plan and new authorization")
	outcome.Retry = "review and confirm the fresh plan with yard migrate or yard update"
	result.transition, result.before, result.observation = fresh, *before, observation
	result.inspection = Inspection{Plan: plan, Assessment: observation.assessment.Clone(), Outcome: &outcome}
	return result, nil
}

func (transition *V2Transition) ConvergeActivationRecovery(ctx context.Context, request ActivationOnlyRecoveryRequest, execution Execution) (Outcome, error) {
	assessment, err := transition.assessActivationRecovery(ctx, request)
	if err != nil {
		return Outcome{}, err
	}
	if len(assessment.inspection.Blockers) != 0 {
		return *assessment.inspection.Outcome, nil
	}
	if execution.Plan != assessment.inspection.Plan {
		return v2OperatorOutcome(assessment.observation.links, assessment.before.Goal.Target, transactionIDPointer(request.Transaction), CodePlanStale, "the fresh recovery plan changed before authorization", "run yard migrate --check"), nil
	}
	if !transition.options.VerifyAuthorization(execution.Plan, execution.Authorization) {
		return v2OperatorOutcome(assessment.observation.links, assessment.before.Goal.Target, transactionIDPointer(request.Transaction), CodeConfirmationRequired, "the fresh activation recovery plan requires new authorization", "review and confirm the fresh plan with yard migrate or yard update"), nil
	}
	unlock, err := transition.store.Lock()
	if err != nil {
		return Outcome{}, err
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	fresh, err := transition.assessActivationRecovery(ctx, request)
	if err != nil {
		return Outcome{}, err
	}
	if len(fresh.inspection.Blockers) != 0 {
		return *fresh.inspection.Outcome, nil
	}
	if fresh.inspection.Plan != execution.Plan || !slices.Equal(fresh.plans, assessment.plans) {
		return v2OperatorOutcome(fresh.observation.links, fresh.before.Goal.Target, transactionIDPointer(request.Transaction), CodePlanStale, "the fresh recovery assessment changed after confirmation", "run yard migrate --check"), nil
	}
	contract := transition.activationRecoveryContract()
	prefix := RecoveryTransactionPrefixV1
	if contract == ActivationOnlyRecoveryContractV2 {
		prefix = RecoveryTransactionPrefixV2
	}
	proposed := TransactionID(prefix + string(transition.options.NewTransactionID()))
	var transaction TransactionID
	if contract == ActivationOnlyRecoveryContractV2 {
		transaction, err = transition.store.ResolveLifecycleRecoveryTransaction(proposed, request, fresh.basePlan, execution.Plan, fingerprintPayload([]byte(execution.Authorization)))
	} else {
		transaction, err = transition.store.ResolveRecoveryTransaction(proposed, request, execution.Plan)
	}
	if err != nil {
		return Outcome{}, err
	}
	fresh.transition.options.NewTransactionID = func() TransactionID { return transaction }
	journalObservation := fresh.observation
	journalObservation.assessment, err = assessV2Action(fresh.transition.policy, true, false, fresh.observation.activationConsequences)
	if err != nil {
		return Outcome{}, err
	}
	created, err := fresh.transition.newJournal(journalObservation, execution.Plan, execution.Authorization)
	if err != nil {
		return Outcome{}, err
	}
	prior, exists, err := transition.store.ReadRecoveryReceiptForPublication(transaction)
	if err != nil {
		return Outcome{}, err
	}
	if exists {
		// A crash after evidence fsync must reuse its exact authorized intent.
		// This invocation still supplied a newly verified grant for the same plan.
		if prior.Replacement != request || prior.Successor.AuthorizationPlan != execution.Plan || prior.Ledger != fresh.observation.ledgerSnapshot.Fingerprint || !slices.Equal(prior.NativePlans, fresh.plans) {
			return v2OperatorOutcome(fresh.observation.links, created.Goal.Target, transactionIDPointer(request.Transaction), CodePlanStale, "published recovery evidence differs from the current assessment", "run yard migrate --check"), nil
		}
		created = prior.Successor
	}
	receipt := RecoveryReceiptV1{SchemaVersion: RecoveryReceiptSchemaV1, Contract: ActivationOnlyRecoveryContractV1, Replacement: request, Predecessor: fresh.before, Successor: created, Ledger: fresh.observation.ledgerSnapshot.Fingerprint, Links: fresh.observation.links, Owner: RecoveryOwner{Release: created.Goal.Target, Artifact: created.ArtifactDigest, Registry: created.RegistryDigest, Catalog: created.CatalogDigest}, NativePlans: fresh.plans}
	if contract == ActivationOnlyRecoveryContractV2 {
		receipt.SchemaVersion, receipt.Contract = RecoveryReceiptSchemaV2, contract
		receipt.BasePlan, receipt.Reservation = fresh.basePlan, fresh.reservation
		frontier, present, err := transition.store.recoveryFrontier(request)
		if err != nil || !present {
			return Outcome{}, invalid("recovery publication has no durable generation frontier")
		}
		receipt.Generation = frontier.Generation
	}
	if err := transition.inject("before-recovery-receipt"); err != nil {
		return Outcome{}, err
	}
	receiptPayload, err := MarshalRecoveryReceipt(receipt)
	if err != nil {
		return Outcome{}, err
	}
	if err := transition.store.CreateRecoveryReceipt(transaction, receiptPayload); err != nil {
		return Outcome{}, err
	}
	if err := transition.inject("after-recovery-receipt"); err != nil {
		return Outcome{}, err
	}
	// The receipt is durable before CAS. Recheck every authorized fact at the
	// final boundary so failed publication preserves the original current journal.
	rechecked, err := transition.assessActivationRecovery(ctx, request)
	if err != nil {
		return Outcome{}, err
	}
	if len(rechecked.inspection.Blockers) != 0 {
		return *rechecked.inspection.Outcome, nil
	}
	if rechecked.inspection.Plan != execution.Plan {
		return v2OperatorOutcome(rechecked.observation.links, created.Goal.Target, transactionIDPointer(request.Transaction), CodePlanStale, "the fresh recovery assessment changed before publication", "run yard migrate --check"), nil
	}
	payload, err := MarshalJournal(created)
	if err != nil {
		return Outcome{}, err
	}
	if err := transition.inject("before-recovery-journal-cas"); err != nil {
		return Outcome{}, err
	}
	rechecked, err = transition.assessActivationRecovery(ctx, request)
	if err != nil {
		return Outcome{}, err
	}
	if len(rechecked.inspection.Blockers) != 0 {
		return *rechecked.inspection.Outcome, nil
	}
	if rechecked.inspection.Plan != execution.Plan {
		return v2OperatorOutcome(rechecked.observation.links, created.Goal.Target, transactionIDPointer(request.Transaction), CodePlanStale, "the fresh recovery assessment changed before publication", "run yard migrate --check"), nil
	}
	if err := transition.store.CompareAndSwapCurrentJournal(fresh.observation.journalSnapshot, payload); err != nil {
		if errors.Is(err, ErrProtectedStoreStale) {
			return v2OperatorOutcome(fresh.observation.links, created.Goal.Target, transactionIDPointer(request.Transaction), CodePlanStale, "the recovery journal changed before publication", "run yard migrate --check"), nil
		}
		current, readErr := transition.store.ReadCurrentJournal()
		if readErr != nil || !bytes.Equal(current.Payload, payload) {
			return Outcome{}, err
		}
	}
	if err := transition.inject("after-recovery-journal-cas"); err != nil {
		return v2RecoveringOutcome(fresh.observation.links, created.Goal.Target, transactionIDPointer(created.Transaction), CodeRecoveryPending, "the fresh recovery transaction is published and can resume"), nil
	}
	unlock()
	locked = false
	// Resume goes through the ordinary V2 engine and the exact newly published
	// grant. No original token is accepted for this successor.
	return fresh.transition.Converge(ctx, Execution{Plan: created.ResumePlan})
}
