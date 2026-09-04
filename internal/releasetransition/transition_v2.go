package releasetransition

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/blang/semver/v4"
)

const v2ActionID domain.ActionID = "release.transition.v2"

const v2ChangedConsequence = "apply the exact typed migration and release activation plan"

func newV2ActionPolicy() (*domain.ActionRegistry, error) {
	return domain.NewActionRegistry([]domain.ActionDefinition{{
		Action:  v2ActionID,
		Summary: "apply the inspected release transition",
		Effect:  domain.ActionMutation,
		Impacts: []domain.ActionImpact{
			domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactYardRuntime,
		},
		Recovery: domain.RecoveryReversible,
	}})
}

func assessV2Action(
	policy *domain.ActionRegistry,
	changed bool,
) (domain.ActionAssessment, error) {
	consequences := []string(nil)
	if changed {
		consequences = []string{v2ChangedConsequence}
	}
	return policy.Assess(v2ActionID, domain.ActionDelta{
		Changed: changed, Consequences: consequences,
	})
}

type V2Options struct {
	ConfigHome        string
	Releases          ReleasePair
	Direction         Direction
	ObserveLinks      func(context.Context) (ReleaseLinks, error)
	ActivateLinks     func(context.Context, ReleasePair) (ReleaseLinks, error)
	Reconcilers       []V2ActivationReconciler
	OwnerRegistration V2OwnerRegistration
	Ingress           V2Ingress
	RegistryPayload   []byte
	ArtifactDigest    Fingerprint
	// CandidateVersion is the trusted compiled runtime semver; ReleaseIDs remain opaque identities.
	CandidateVersion    string
	InheritedSettingIDs []string
	SourceIngress       *SourceIngressRequest
	Replacement         *JournalReplacement
	NewTransactionID    func() TransactionID
	VerifyAuthorization func(PlanToken, Authorization) bool
	fault               func(string) error
}

type V2ActivationReconciler interface {
	ID() string
	Observe(context.Context, ReleasePair, ReleaseLinks) (V2ActivationObservation, error)
	Reconcile(context.Context, ReleaseLinks) error
}

type V2ActivationObservation struct {
	Actual    Fingerprint `json:"actual"`
	Desired   Fingerprint `json:"desired"`
	Converged bool        `json:"converged"`
}

type V2Transition struct {
	options        V2Options
	store          *POSIXV2Store
	catalog        CapabilityCatalog
	registry       RegistryV2
	registryDigest Fingerprint
	policy         *domain.ActionRegistry

	cacheMu sync.Mutex
	cache   map[PlanToken]Goal
}

type v2WorkKind string

const (
	v2SettingsWork v2WorkKind = "settings"
	v2OwnerWork    v2WorkKind = "owner-registration"
	v2LedgerWork   v2WorkKind = "ledger"
	v2IngressWork  v2WorkKind = "ingress"
)

type v2Work struct {
	kind        v2WorkKind
	intent      PlannerStepIntent
	migration   MigrationDefinitionV2
	file        *settingsV2FilePlan
	owner       *ownerRegistrationV2Plan
	ingress     *V2IngressOperation
	ingressBind *V2IngressBinding
	ledgerFrom  ProtectedSnapshot
	ledgerTo    []byte
}

type v2Observation struct {
	goal              Goal
	links             ReleaseLinks
	ledger            LedgerV2
	ledgerSnapshot    ProtectedSnapshot
	journal           *JournalRecord
	journalSnapshot   ProtectedSnapshot
	assessment        domain.ActionAssessment
	decisions         []RedactedDecision
	blockers          []Blocker
	observations      []ResourceObservation
	intents           []PlannerStepIntent
	work              []v2Work
	activationScope   []v2ActivationScope
	observationScope  Fingerprint
	activationFixed   bool
	replacement       *JournalReplacement
	supersededJournal *JournalRecord
}

type v2ActivationScope struct {
	ID      string      `json:"id"`
	Desired Fingerprint `json:"desired"`
}

type settingsRecoveryV1 struct {
	SchemaVersion int                     `json:"schemaVersion"`
	Transaction   TransactionID           `json:"transaction"`
	Step          string                  `json:"step"`
	Migration     string                  `json:"migration"`
	Yard          string                  `json:"yard"`
	Expected      Fingerprint             `json:"expectedFingerprint"`
	Before        []settingsV2BeforeValue `json:"before"`
}

func NewV2Transition(options V2Options) (*V2Transition, error) {
	if !filepath.IsAbs(options.ConfigHome) || options.ObserveLinks == nil ||
		options.VerifyAuthorization == nil {
		return nil, errors.New("v2 transition requires config home, link observer, and authorization verifier")
	}
	if options.NewTransactionID == nil {
		options.NewTransactionID = defaultV2TransactionID
	}
	if options.Direction == "" {
		options.Direction = DirectionActivateTarget
	}
	if !validDirection(options.Direction) {
		return nil, invalid("unknown release transition direction %q", options.Direction)
	}
	if err := options.Releases.Validate(); err != nil {
		return nil, err
	}
	if options.SourceIngress != nil {
		if err := options.SourceIngress.Validate(); err != nil {
			return nil, err
		}
		if options.Ingress == nil {
			return nil, errors.New("source ingress descriptor has no compatibility adapter")
		}
	}
	if options.Replacement != nil {
		if err := options.Replacement.Validate(); err != nil {
			return nil, err
		}
	}
	store, err := NewPOSIXV2Store(options.ConfigHome)
	if err != nil {
		return nil, err
	}
	journalSnapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return nil, err
	}
	if journalSnapshot.Exists {
		journal, parseErr := ParseJournal(journalSnapshot.Payload)
		if parseErr != nil {
			return nil, parseErr
		}
		if options.Replacement == nil && (journal.Checkpoint != JournalComplete ||
			(journal.Goal.Target == options.Releases.Target &&
				journal.Goal.Direction == options.Direction &&
				journal.ArtifactDigest == options.ArtifactDigest)) {
			// The protected journal owns the immutable release pair while it is
			// unfinished and for an exact completed-goal reinspection.
			// A genuinely new target keeps the observed pair.
			// Current links may already describe staged or target-active topology.
			options.Releases = journal.Releases
		}
	}
	if err := validateFingerprint(options.ArtifactDigest, "artifact digest"); err != nil {
		return nil, err
	}
	catalog := BuiltinCapabilityCatalog()
	registry, registryDigest, err := ParseRegistryV2(options.RegistryPayload, catalog)
	if err != nil {
		return nil, err
	}
	for _, migration := range registry.Migrations {
		if migration.Kind == "test-yard-owner-v1-to-v2" && options.OwnerRegistration == nil {
			return nil, errors.New("v2 transition requires the owner-registration capability port")
		}
	}
	policy, err := newV2ActionPolicy()
	if err != nil {
		return nil, err
	}
	options.RegistryPayload = slices.Clone(options.RegistryPayload)
	options.InheritedSettingIDs = slices.Clone(options.InheritedSettingIDs)
	options.Reconcilers = slices.Clone(options.Reconcilers)
	options.Replacement = cloneJournalReplacement(options.Replacement)
	return &V2Transition{
		options: options, store: store, catalog: catalog, registry: registry,
		registryDigest: registryDigest, policy: policy, cache: make(map[PlanToken]Goal),
	}, nil
}

func (transition *V2Transition) Inspect(ctx context.Context, goal Goal) (Inspection, error) {
	if transition == nil {
		return Inspection{}, errors.New("v2 transition is required")
	}
	observation, err := transition.observe(ctx, goal)
	if err != nil {
		return Inspection{}, err
	}
	if _, _, err := transition.policy.Resolve(observation.assessment); err != nil {
		return Inspection{}, err
	}
	outcome := transition.inspectionOutcome(observation)
	if observation.journal != nil && observation.journal.Checkpoint != JournalComplete {
		transaction := observation.journal.Transaction
		return Inspection{
			Plan:       observation.journal.ResumePlan,
			Assessment: observation.assessment.Clone(),
			Decisions:  slices.Clone(observation.decisions),
			Blockers:   slices.Clone(observation.blockers), Resume: &transaction,
			Outcome: &outcome,
		}, nil
	}
	plan, err := BindPlan(transition.planFacts(observation))
	if err != nil {
		return Inspection{}, err
	}
	transition.cacheMu.Lock()
	transition.cache = map[PlanToken]Goal{plan: goal}
	transition.cacheMu.Unlock()
	return Inspection{
		Plan: plan, Assessment: observation.assessment.Clone(),
		Decisions: slices.Clone(observation.decisions),
		Blockers:  slices.Clone(observation.blockers), Outcome: &outcome,
	}, nil
}

func (transition *V2Transition) inspectionOutcome(observation v2Observation) Outcome {
	journal := observation.journal
	if journal != nil && journal.Checkpoint == JournalComplete && journal.Goal != observation.goal {
		// A completed journal for another exact goal is immutable history, not
		// recovery state for a new forward target or explicit rollback.
		journal = nil
	}
	var transaction *TransactionID
	if journal != nil {
		transaction = transactionIDPointer(journal.Transaction)
	}
	if len(observation.blockers) != 0 {
		blocker := observation.blockers[0]
		return v2OperatorOutcome(
			observation.links, observation.goal.Target, transaction,
			blocker.Code, blocker.Message, blocker.Retry,
		)
	}
	if journal != nil && journal.Checkpoint != JournalComplete {
		return v2RecoveringOutcome(
			observation.links, observation.goal.Target, transaction,
			CodeRecoveryPending,
			"the authorized release transition can resume from observed facts",
		)
	}
	facts := TransitionFacts{
		Goal: observation.goal, Releases: transition.options.Releases,
		Links: observation.links, Journal: journal,
		CurrentArtifactDigest: transition.options.ArtifactDigest,
		CurrentRegistryDigest: transition.registryDigest,
		CurrentCatalogDigest:  transition.catalog.Digest(),
		FixedPointVerified:    transition.fixedPoint(observation),
	}
	if journal != nil {
		facts.CurrentPlan = journal.ResumePlan
		facts.VerifiedAuthorizationPlan = journal.AuthorizationPlan
		facts.CurrentAuthorizationDigest = journal.AuthorizationDigest
		facts.CurrentIntents = slices.Clone(observation.intents)
		if len(facts.CurrentIntents) == 0 {
			facts.CurrentIntents = make([]PlannerStepIntent, len(journal.Steps))
			for index, step := range journal.Steps {
				facts.CurrentIntents[index] = PlannerStepIntent{
					ID: step.ID, Migration: step.Migration, Resource: step.Resource,
					Decision: step.Decision, Expected: step.Expected, Desired: step.Desired,
				}
			}
		}
	}
	return Evaluate(facts)
}

func (transition *V2Transition) preflightConverge(
	ctx context.Context,
	execution Execution,
) (PlanToken, *Outcome, error) {
	currentSnapshot, err := transition.store.ReadCurrentJournal()
	if err != nil {
		return "", nil, err
	}
	var current *JournalRecord
	if currentSnapshot.Exists {
		parsed, parseErr := ParseJournal(currentSnapshot.Payload)
		if parseErr != nil {
			return "", nil, parseErr
		}
		current = &parsed
	}
	goal, found := transition.resolveConvergeGoal(ctx, execution, current)
	if !found {
		links, linksErr := transition.options.ObserveLinks(ctx)
		if linksErr != nil {
			return "", nil, linksErr
		}
		outcome := v2OperatorOutcome(links, transition.options.Releases.Target, nil,
			CodePlanStale, "the release transition plan was not inspected by this engine",
			"run yard update --check")
		return "", &outcome, nil
	}
	observation, err := transition.observe(ctx, goal)
	if err != nil {
		return "", nil, err
	}
	if len(observation.blockers) != 0 {
		blocker := observation.blockers[0]
		var transaction *TransactionID
		if observation.journal != nil &&
			(observation.journal.Checkpoint != JournalComplete ||
				observation.journal.Goal == observation.goal) {
			transaction = transactionIDPointer(observation.journal.Transaction)
		}
		outcome := v2OperatorOutcome(
			observation.links, goal.Target, transaction,
			blocker.Code, blocker.Message, blocker.Retry,
		)
		return "", &outcome, nil
	}
	if observation.journal != nil && observation.journal.Checkpoint == JournalComplete &&
		observation.journal.Goal == observation.goal {
		return "", nil, nil
	}
	if observation.journal != nil && observation.journal.Checkpoint != JournalComplete {
		if execution.Plan != observation.journal.ResumePlan {
			outcome := v2OperatorOutcome(observation.links, goal.Target,
				transactionIDPointer(observation.journal.Transaction), CodePlanStale,
				"the authorized release transition bindings changed",
				"run yard update --check")
			return "", &outcome, nil
		}
		return "", nil, nil
	}
	plan, err := BindPlan(transition.planFacts(observation))
	if err != nil {
		return "", nil, err
	}
	if plan != execution.Plan {
		outcome := v2OperatorOutcome(observation.links, goal.Target, nil,
			CodePlanStale, "the inspected release transition changed before convergence",
			"run yard update --check")
		return "", &outcome, nil
	}
	if len(observation.work) == 0 && transition.fixedPoint(observation) {
		outcome := readyOutcome(Outcome{
			Active: observation.links.Active, Previous: cloneReleaseID(observation.links.Previous),
			Target: goal.Target,
		})
		return "", &outcome, nil
	}
	if !transition.options.VerifyAuthorization(plan, execution.Authorization) {
		outcome := v2OperatorOutcome(observation.links, goal.Target, nil,
			CodeConfirmationRequired, "the exact release transition plan is not authorized",
			"review and confirm the update plan")
		return "", &outcome, nil
	}
	return plan, nil, nil
}

func (transition *V2Transition) resolveConvergeGoal(
	ctx context.Context,
	execution Execution,
	current *JournalRecord,
) (Goal, bool) {
	goal, found := transition.cachedGoal(execution.Plan)
	if current != nil && current.Checkpoint != JournalComplete &&
		execution.Plan == current.ResumePlan {
		return current.Goal, true
	}
	if found || (current != nil && current.Checkpoint != JournalComplete &&
		transition.options.Replacement == nil) {
		return goal, found
	}
	candidate := Goal{
		Target:    transition.options.Releases.Target,
		Direction: transition.options.Direction,
	}
	observation, err := transition.observe(ctx, candidate)
	if err != nil {
		return Goal{}, false
	}
	plan, err := BindPlan(transition.planFacts(observation))
	if err != nil || plan != execution.Plan {
		return Goal{}, false
	}
	return candidate, true
}

func (transition *V2Transition) Converge(
	ctx context.Context,
	execution Execution,
) (outcome Outcome, resultErr error) {
	if transition == nil {
		return Outcome{}, errors.New("v2 transition is required")
	}
	authorizedPlan, preflightOutcome, err := transition.preflightConverge(ctx, execution)
	if err != nil {
		return Outcome{}, err
	}
	if preflightOutcome != nil {
		return *preflightOutcome, nil
	}
	var recoverableJournal *JournalRecord
	unlock, err := transition.store.Lock()
	if err != nil {
		return Outcome{}, err
	}
	defer unlock()
	defer func() {
		if resultErr == nil || outcome.Code != "" || recoverableJournal == nil {
			return
		}
		outcome, resultErr = transition.reducePostMutationFailure(
			ctx, *recoverableJournal, resultErr,
		)
	}()

	currentSnapshot, err := transition.store.ReadCurrentJournal()
	if err != nil {
		return Outcome{}, err
	}
	if authorizedPlan != "" && transition.options.Replacement != nil &&
		currentSnapshot.Fingerprint != transition.options.Replacement.Fingerprint {
		links, linksErr := transition.options.ObserveLinks(ctx)
		if linksErr != nil {
			return Outcome{}, linksErr
		}
		return v2OperatorOutcome(links, transition.options.Releases.Target, nil,
			CodePlanStale, "the journal selected for replacement changed after confirmation",
			"run yard update --check"), nil
	}
	var current *JournalRecord
	if currentSnapshot.Exists {
		parsed, parseErr := ParseJournal(currentSnapshot.Payload)
		if parseErr != nil {
			return Outcome{}, parseErr
		}
		current = &parsed
		if current.Checkpoint != JournalComplete {
			recoverableJournal = current
		}
	}
	goal, found := transition.resolveConvergeGoal(ctx, execution, current)
	if !found {
		links, linksErr := transition.options.ObserveLinks(ctx)
		if linksErr != nil {
			return Outcome{}, linksErr
		}
		return v2OperatorOutcome(links, transition.options.Releases.Target, nil,
			CodePlanStale, "the release transition plan was not inspected by this engine",
			"run yard update --check"), nil
	}
	observation, err := transition.observe(ctx, goal)
	if err != nil {
		return Outcome{}, err
	}
	if len(observation.blockers) != 0 {
		blocker := observation.blockers[0]
		var transaction *TransactionID
		if observation.journal != nil &&
			(observation.journal.Checkpoint != JournalComplete ||
				observation.journal.Goal == observation.goal) {
			transaction = transactionIDPointer(observation.journal.Transaction)
		}
		return v2OperatorOutcome(
			observation.links, goal.Target, transaction,
			blocker.Code, blocker.Message, blocker.Retry,
		), nil
	}

	if observation.journal != nil && observation.journal.Checkpoint == JournalComplete &&
		observation.journal.Goal == observation.goal {
		outcome := transition.inspectionOutcome(observation)
		if outcome.Status != StatusReady {
			return outcome, nil
		}
		return transition.cleanupReady(ctx, observation.journal.Transaction, outcome), nil
	}

	journal := observation.journal
	journalSnapshot := observation.journalSnapshot
	if journal == nil || journal.Checkpoint == JournalComplete {
		plan, bindErr := BindPlan(transition.planFacts(observation))
		if bindErr != nil {
			return Outcome{}, bindErr
		}
		if plan != execution.Plan {
			return v2OperatorOutcome(observation.links, goal.Target, nil,
				CodePlanStale, "the inspected release transition changed before convergence",
				"run yard update --check"), nil
		}
		if len(observation.work) == 0 && transition.fixedPoint(observation) {
			return readyOutcome(Outcome{
				Active: observation.links.Active, Previous: cloneReleaseID(observation.links.Previous),
				Target: goal.Target,
			}), nil
		}
		if authorizedPlan != plan {
			return v2OperatorOutcome(observation.links, goal.Target, nil,
				CodeConfirmationRequired, "the exact release transition plan is not authorized",
				"review and confirm the update plan"), nil
		}
		if observation.replacement != nil {
			current, readErr := transition.store.ReadCurrentJournal()
			if readErr != nil {
				return Outcome{}, readErr
			}
			if !sameProtectedSnapshot(current, journalSnapshot) {
				return v2OperatorOutcome(observation.links, goal.Target, nil,
					CodePlanStale, "the journal selected for replacement changed after confirmation",
					"run yard update --check"), nil
			}
		}
		created, createErr := transition.newJournal(observation, plan, execution.Authorization)
		if createErr != nil {
			return Outcome{}, createErr
		}
		payload, marshalErr := MarshalJournal(created)
		if marshalErr != nil {
			return Outcome{}, marshalErr
		}
		journal = &created
		if requiresSupersededJournalArchive(observation.replacement) {
			if observation.supersededJournal == nil {
				return Outcome{}, invalid("replacement source journal is missing")
			}
			archive, archiveErr := MarshalSupersededJournal(SupersededJournalRecord{
				SchemaVersion: SupersededJournalSchemaV1, AuthorizationPlan: created.AuthorizationPlan,
				Replacement: *observation.replacement, Journal: *observation.supersededJournal,
			})
			if archiveErr != nil {
				return Outcome{}, archiveErr
			}
			if archiveErr := transition.store.CreateSupersededJournal(created.Transaction, archive); archiveErr != nil {
				return Outcome{}, archiveErr
			}
			if err := transition.inject("after-superseded-journal"); err != nil {
				return Outcome{}, err
			}
			current, readErr := transition.store.ReadCurrentJournal()
			if readErr != nil {
				return Outcome{}, readErr
			}
			if !sameProtectedSnapshot(current, journalSnapshot) {
				return v2OperatorOutcome(observation.links, goal.Target, nil,
					CodePlanStale, "the journal selected for replacement changed before publication",
					"run yard update --check"), nil
			}
			if err := transition.inject("before-replacement-journal-cas"); err != nil {
				return Outcome{}, err
			}
		}
		if err := transition.store.CompareAndSwapCurrentJournal(journalSnapshot, payload); err != nil {
			if errors.Is(err, ErrProtectedStoreStale) && observation.replacement != nil {
				return v2OperatorOutcome(observation.links, goal.Target, nil,
					CodePlanStale, "the journal selected for replacement changed before publication",
					"run yard update --check"), nil
			}
			published, readErr := transition.store.ReadCurrentJournal()
			if readErr == nil && published.Exists && bytes.Equal(published.Payload, payload) {
				recoverableJournal = journal
			}
			return Outcome{}, err
		}
		recoverableJournal = journal
		journalSnapshot = protectedSnapshotFromPayload(payload)
		if observation.replacement != nil {
			if err := transition.inject("after-replacement-journal-cas"); err != nil {
				return Outcome{}, err
			}
		}
		if err := transition.inject("after-journal-authorized"); err != nil {
			return Outcome{}, err
		}
	} else {
		if execution.Plan != journal.ResumePlan ||
			journal.ArtifactDigest != transition.options.ArtifactDigest ||
			journal.RegistryDigest != transition.registryDigest ||
			journal.CatalogDigest != transition.catalog.Digest() ||
			journal.ObservationScope != observation.observationScope ||
			!releasePairsEqual(journal.Releases, transition.options.Releases) {
			return v2OperatorOutcome(observation.links, goal.Target,
				transactionIDPointer(journal.Transaction), CodePlanStale,
				"the authorized release transition bindings changed",
				"run yard update --check"), nil
		}
	}
	links, guardOutcome, guardErr := transition.guardJournalLinks(ctx, *journal)
	if guardErr != nil || guardOutcome.Code != "" {
		return guardOutcome, guardErr
	}
	observation.links = links

	workByID := make(map[string]v2Work, len(observation.work))
	for _, work := range observation.work {
		workByID[work.intent.ID] = work
	}
	for index := range journal.Steps {
		work, exists := workByID[journal.Steps[index].ID]
		if !exists {
			return v2OperatorOutcome(observation.links, goal.Target,
				transactionIDPointer(journal.Transaction), CodeRecoveryAmbiguous,
				"a journaled migration resource cannot be reconstructed safely",
				"run yard update --check"), nil
		}
		outcome, convergeErr := transition.convergeStep(
			ctx, journal, &journalSnapshot, index, work, observation.links,
		)
		if convergeErr != nil || outcome.Code != "" {
			return outcome, convergeErr
		}
	}

	if transition.options.Releases.From == transition.options.Releases.Target {
		if journal.Checkpoint == JournalAuthorized || journal.Checkpoint == JournalMigrating {
			journal.Checkpoint = JournalTargetActive
			if err := transition.persistJournal(journal, &journalSnapshot); err != nil {
				return Outcome{}, err
			}
			if err := transition.inject("after-target-active"); err != nil {
				return Outcome{}, err
			}
		}
	} else if journal.Checkpoint == JournalAuthorized || journal.Checkpoint == JournalMigrating {
		links, linksErr := transition.options.ObserveLinks(ctx)
		if linksErr != nil {
			return Outcome{}, linksErr
		}
		if !initialReleaseLinks(links, journal.Releases) {
			return v2OperatorOutcome(links, journal.Goal.Target,
				transactionIDPointer(journal.Transaction), CodeActivationAmbiguous,
				"release links changed before the journaled activation intent",
				"run yard update --check"), nil
		}
		journal.Checkpoint = JournalActivationIntent
		if err := transition.persistJournal(journal, &journalSnapshot); err != nil {
			return Outcome{}, err
		}
		if err := transition.inject("after-activation-intent"); err != nil {
			return Outcome{}, err
		}
	}
	if transition.options.Releases.From != transition.options.Releases.Target &&
		transition.options.ActivateLinks == nil {
		return transition.evaluateTerminal(ctx, *journal)
	}
	if transition.options.Releases.From != transition.options.Releases.Target &&
		journal.Checkpoint == JournalActivationIntent {
		links, activateErr := transition.options.ActivateLinks(ctx, journal.Releases)
		if activateErr != nil {
			return Outcome{}, activateErr
		}
		if links.Active != journal.Releases.Target || links.Previous == nil ||
			*links.Previous != journal.Releases.From {
			return v2OperatorOutcome(links, journal.Goal.Target,
				transactionIDPointer(journal.Transaction), CodeActivationAmbiguous,
				"release activation did not reach the exact journaled links",
				"run yard update --check"), nil
		}
		journal.Checkpoint = JournalTargetActive
		if err := transition.persistJournal(journal, &journalSnapshot); err != nil {
			return Outcome{}, err
		}
		if err := transition.inject("after-target-active"); err != nil {
			return Outcome{}, err
		}
	}
	if journal.Checkpoint == JournalTargetActive {
		journal.Checkpoint = JournalReconciling
		if err := transition.persistJournal(journal, &journalSnapshot); err != nil {
			return Outcome{}, err
		}
		if err := transition.inject("after-reconciling"); err != nil {
			return Outcome{}, err
		}
	}
	if journal.Checkpoint == JournalReconciling {
		if outcome, reconcileErr := transition.reconcileActivation(
			ctx, *journal,
		); reconcileErr != nil || outcome.Code != "" {
			return outcome, reconcileErr
		}
		if err := transition.inject("before-journal-complete"); err != nil {
			return Outcome{}, err
		}
		journal.Checkpoint = JournalComplete
		if err := transition.persistJournal(journal, &journalSnapshot); err != nil {
			return Outcome{}, err
		}
		if err := transition.inject("after-journal-complete"); err != nil {
			return Outcome{}, err
		}
	}
	return transition.evaluateTerminal(ctx, *journal)
}

func (transition *V2Transition) evaluateTerminal(
	ctx context.Context,
	journal JournalRecord,
) (Outcome, error) {
	outcome, err := transition.evaluateCurrent(ctx, journal)
	if err != nil || outcome.Status != StatusReady {
		return outcome, err
	}
	return transition.cleanupReady(ctx, journal.Transaction, outcome), nil
}

func (transition *V2Transition) cleanupReady(
	ctx context.Context,
	transaction TransactionID,
	outcome Outcome,
) Outcome {
	if err := transition.cleanupOwnerRegistration(ctx, transaction); err != nil {
		outcome.Warnings = append(outcome.Warnings, "recovery cleanup is pending")
		return outcome
	}
	if err := transition.inject("before-recovery-gc"); err == nil {
		err = transition.store.CleanupTransactions(transaction)
		if err == nil {
			return outcome
		}
	}
	outcome.Warnings = append(outcome.Warnings, "recovery cleanup is pending")
	return outcome
}

func (transition *V2Transition) cleanupOwnerRegistration(
	ctx context.Context,
	transaction TransactionID,
) error {
	snapshot, err := transition.store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return err
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil {
		return err
	}
	if journal.Transaction != transaction || journal.Checkpoint != JournalComplete {
		return invalid("owner cleanup journal is not the completed transaction")
	}
	found := false
	for _, step := range journal.Steps {
		if step.Resource != ownerRegistrationResource {
			continue
		}
		if found || step.Checkpoint != StepVerified {
			return invalid("owner cleanup step is invalid")
		}
		found = true
		recovery, err := transition.store.ReadRecovery(journal.Transaction, step.ID)
		if err != nil {
			return err
		}
		if !recovery.Exists {
			return nil
		}
		before, err := transition.ownerRegistrationBefore(ctx, journal, step, nil)
		if err != nil {
			return err
		}
		if !before.TerminalCleanup || transition.options.OwnerRegistration == nil {
			return invalid("owner cleanup capability is unavailable")
		}
		if err := transition.options.OwnerRegistration.Commit(ctx, before); err != nil {
			return err
		}
	}
	return nil
}

func (transition *V2Transition) reducePostMutationFailure(
	ctx context.Context,
	fallback JournalRecord,
	cause error,
) (Outcome, error) {
	links, linksErr := transition.options.ObserveLinks(ctx)
	if linksErr != nil || links.Validate() != nil {
		return v2OperatorOutcome(
			ReleaseLinks{}, fallback.Goal.Target,
			transactionIDPointer(fallback.Transaction), CodeRecoveryAmbiguous,
			"release facts cannot be observed after an interrupted transition",
			"run yard update --check",
		), nil
	}
	journalSnapshot, err := transition.store.ReadCurrentJournal()
	if err != nil {
		return v2OperatorOutcome(
			links, fallback.Goal.Target, transactionIDPointer(fallback.Transaction),
			CodeRecoveryAmbiguous,
			"durable transition intent cannot be observed after a possible mutation",
			"run yard update --check",
		), nil
	}
	if !journalSnapshot.Exists {
		// Publication did not establish durable authorization, so no Adapter
		// mutation could have followed this first journal CAS.
		return Outcome{}, cause
	}
	journal, err := ParseJournal(journalSnapshot.Payload)
	if err != nil {
		return v2OperatorOutcome(
			links, fallback.Goal.Target, transactionIDPointer(fallback.Transaction),
			CodeJournalInvalid, "the durable release transition journal is invalid",
			"run yard update --check",
		), nil
	}
	if journal.Checkpoint == JournalComplete {
		terminal, evaluateErr := transition.evaluateCurrent(ctx, journal)
		if evaluateErr == nil {
			if terminal.Status == StatusReady {
				return transition.cleanupReady(ctx, journal.Transaction, terminal), nil
			}
			return terminal, nil
		}
		return v2OperatorOutcome(
			links, journal.Goal.Target, transactionIDPointer(journal.Transaction),
			CodeRecoveryAmbiguous,
			"terminal release facts cannot be verified after an interrupted transition",
			"run yard update --check",
		), nil
	}
	observation, err := transition.observe(ctx, journal.Goal)
	if err != nil {
		return v2OperatorOutcome(
			links, journal.Goal.Target, transactionIDPointer(journal.Transaction),
			CodeRecoveryAmbiguous,
			"release facts cannot be reduced to a safe recovery checkpoint",
			"run yard update --check",
		), nil
	}
	if len(observation.blockers) != 0 {
		blocker := observation.blockers[0]
		return v2OperatorOutcome(
			observation.links, journal.Goal.Target,
			transactionIDPointer(journal.Transaction), blocker.Code,
			blocker.Message, blocker.Retry,
		), nil
	}
	if observation.journal == nil ||
		observation.journal.Transaction != journal.Transaction ||
		!journalLinksAllowed(journal, observation.links) {
		return v2OperatorOutcome(
			observation.links, journal.Goal.Target,
			transactionIDPointer(journal.Transaction), CodeRecoveryAmbiguous,
			"release facts do not match the protected recovery transaction",
			"run yard update --check",
		), nil
	}
	if errors.Is(cause, ErrInvalid) {
		return v2OperatorOutcome(
			observation.links, journal.Goal.Target,
			transactionIDPointer(journal.Transaction), CodeRecoveryAmbiguous,
			"protected recovery evidence is invalid or inconsistent",
			"run yard update --check",
		), nil
	}
	return v2RecoveringOutcome(
		observation.links, journal.Goal.Target,
		transactionIDPointer(journal.Transaction), CodeVerificationFailed,
		"the release transition was interrupted after a durable mutation checkpoint",
	), nil
}

func (transition *V2Transition) observe(ctx context.Context, goal Goal) (v2Observation, error) {
	if err := goal.Validate(); err != nil {
		return v2Observation{}, err
	}
	if goal.Target != transition.options.Releases.Target {
		return v2Observation{}, invalid("goal target does not match the verified release pair")
	}
	links, err := transition.options.ObserveLinks(ctx)
	if err != nil {
		return v2Observation{}, err
	}
	if err := links.Validate(); err != nil {
		return v2Observation{}, err
	}
	ledgerSnapshot, err := transition.store.ReadLedger()
	if err != nil {
		return v2Observation{}, err
	}
	ledger := BaselineLedgerV2(transition.registry)
	if ledgerSnapshot.Exists {
		ledger, _, err = ParseLedgerV2(ledgerSnapshot.Payload, transition.registry)
		if err != nil {
			return v2Observation{}, err
		}
	}
	journalSnapshot, err := transition.store.ReadCurrentJournal()
	if err != nil {
		return v2Observation{}, err
	}
	var journal *JournalRecord
	if journalSnapshot.Exists {
		parsed, parseErr := ParseJournal(journalSnapshot.Payload)
		if parseErr != nil {
			return v2Observation{}, parseErr
		}
		if graphErr := transition.store.validateCurrentSupersession(parsed); graphErr != nil {
			return v2Observation{}, graphErr
		}
		journal = &parsed
	}
	observation := v2Observation{
		goal: goal, links: links, ledger: ledger, ledgerSnapshot: ledgerSnapshot,
		journal: journal, journalSnapshot: journalSnapshot,
	}
	completedHistory := journal != nil && journal.Checkpoint == JournalComplete &&
		transition.completedJournalMatches(observation)
	if journal != nil && journal.Checkpoint != JournalComplete {
		replaced, replaceErr := transition.observePostActivationReplacement(ctx, &observation)
		if replaceErr != nil {
			return v2Observation{}, replaceErr
		}
		if !replaced && (journal.Goal != goal || !releasePairsEqual(journal.Releases, transition.options.Releases)) {
			observation.blockers = []Blocker{{
				Code: CodeRecoveryAmbiguous, Resource: "transition.active",
				Message: "another release transition must be recovered before this goal",
				Retry:   "run yard update --check",
			}}
		}
		if !replaced {
			if err := transition.observeResume(ctx, &observation); err != nil {
				return v2Observation{}, err
			}
		}
		if !replaced && transition.canReplacePreActivationJournal(observation) {
			replacement := &JournalReplacement{
				Transaction: journal.Transaction, Fingerprint: journalSnapshot.Fingerprint,
				Reason: JournalReplacementPreActivationPlanStale,
			}
			observation = v2Observation{
				goal: goal, links: links, ledger: ledger, ledgerSnapshot: ledgerSnapshot,
				journalSnapshot: journalSnapshot, replacement: replacement,
			}
			if err := transition.observeFresh(ctx, &observation); err != nil {
				return v2Observation{}, err
			}
		}
	} else if !completedHistory && (journal == nil || !transition.fixedPoint(observation)) {
		if !initialReleaseLinks(links, transition.options.Releases) {
			observation.blockers = append(observation.blockers, Blocker{
				Code: CodeActivationAmbiguous, Resource: "transition.links",
				Message: "release links do not match the exact unstarted transition",
				Retry:   "run yard update --check",
			})
		} else {
			if err := transition.observeFresh(ctx, &observation); err != nil {
				return v2Observation{}, err
			}
		}
	}
	if completedHistory {
		// The matching completed journal is immutable history. Later runtime
		// drift belongs to ordinary reconciliation, not migration recovery.
		observation.activationFixed = true
		observation.observationScope = journal.ObservationScope
	} else if observation.observationScope == "" {
		if err := transition.observeActivation(ctx, &observation); err != nil {
			return v2Observation{}, err
		}
		observationScope, err := transition.bindObservationScope(observation.activationScope)
		if err != nil {
			return v2Observation{}, err
		}
		observation.observationScope = observationScope
		if journal != nil && journal.Checkpoint != JournalComplete &&
			journal.ObservationScope != observationScope {
			observation.blockers = append(observation.blockers, Blocker{
				Code: CodePlanStale, Resource: "transition.observation-scope",
				Message: "the authorized transition observation scope differs from this engine",
				Retry:   "run yard update --check",
			})
		}
	}
	changed := len(observation.intents) != 0 || links.Active != goal.Target ||
		!observation.activationFixed
	if journal != nil && journal.Checkpoint != JournalComplete {
		changed = true
	}
	assessment, err := assessV2Action(transition.policy, changed)
	if err != nil {
		return v2Observation{}, err
	}
	observation.assessment = assessment
	return observation, nil
}

func (transition *V2Transition) observePostActivationReplacement(
	ctx context.Context,
	observation *v2Observation,
) (bool, error) {
	request := transition.options.Replacement
	journal := observation.journal
	if request == nil || journal == nil ||
		request.Reason != JournalReplacementPostActivationScopeV0111 ||
		request.SourceVersion != "0.11.1" ||
		request.Transaction != journal.Transaction ||
		request.Fingerprint != observation.journalSnapshot.Fingerprint ||
		transition.options.SourceIngress != nil ||
		transition.options.Direction != DirectionActivateTarget ||
		observation.goal.Direction != DirectionActivateTarget ||
		journal.Goal.Direction != DirectionActivateTarget ||
		journal.Checkpoint != JournalReconciling || journal.SourceIngress != nil ||
		journal.RegistryDigest != transition.registryDigest ||
		journal.CatalogDigest != transition.catalog.Digest() ||
		transition.options.Releases.From != journal.Releases.Target ||
		transition.options.Releases.Target == journal.Releases.Target ||
		!releaseIDsEqual(transition.options.Releases.Previous, releaseIDPointer(journal.Releases.From)) ||
		!activatedReleaseLinks(observation.links, journal.Releases) {
		return false, nil
	}
	canonicalJournal, err := MarshalJournal(*journal)
	if err != nil {
		return false, err
	}
	if fingerprintPayload(canonicalJournal) != request.Fingerprint {
		return false, nil
	}
	sourceVersion, err := semver.Parse(request.SourceVersion)
	if err != nil {
		return false, nil
	}
	candidateVersion, err := semver.Parse(transition.options.CandidateVersion)
	if err != nil || candidateVersion.String() != transition.options.CandidateVersion ||
		!candidateVersion.GT(sourceVersion) {
		return false, nil
	}
	eligible, err := transition.postActivationLedgerEvidenceMatches(*journal, observation.ledgerSnapshot)
	if err != nil || !eligible {
		return false, err
	}
	candidate := v2Observation{
		goal: observation.goal, links: observation.links,
		ledger: observation.ledger, ledgerSnapshot: observation.ledgerSnapshot,
		journalSnapshot:   observation.journalSnapshot,
		replacement:       cloneJournalReplacement(request),
		supersededJournal: journal,
	}
	if err := transition.observeActivation(ctx, &candidate); err != nil {
		return false, err
	}
	if len(candidate.blockers) != 0 {
		return false, nil
	}
	candidate.observationScope, err = transition.bindObservationScope(candidate.activationScope)
	if err != nil {
		return false, err
	}
	if candidate.observationScope == journal.ObservationScope {
		return false, nil
	}
	*observation = candidate
	return true, nil
}

func (transition *V2Transition) postActivationLedgerEvidenceMatches(
	journal JournalRecord,
	ledgerSnapshot ProtectedSnapshot,
) (bool, error) {
	if !ledgerSnapshot.Exists || len(journal.Steps) == 0 {
		return false, nil
	}
	artifactsMatch, err := transition.store.postActivationJournalArtifactsMatch(journal)
	if err != nil || !artifactsMatch {
		return false, err
	}
	ledger := BaselineLedgerV2(transition.registry)
	reconstructed := absentProtectedSnapshot()
	for _, step := range journal.Steps {
		migration, exists := transition.migration(step.Migration)
		if !exists || step.ID != migration.ID+".ledger" ||
			step.Resource != "ledger."+migration.Domain || step.Decision != DecisionTransform ||
			step.Checkpoint != StepVerified || step.Expected != reconstructed.Fingerprint ||
			step.Evidence == nil || step.Evidence.Recovery != "" {
			return false, nil
		}
		advanced, err := ledger.Advance(transition.registry, migration)
		if err != nil {
			return false, nil
		}
		payload, desired, err := MarshalLedgerV2(advanced, transition.registry)
		if err != nil || step.Desired != desired {
			return false, err
		}
		for _, checkpoint := range []EvidenceCheckpoint{
			EvidenceCaptured, EvidenceApplied, EvidenceVerified,
		} {
			snapshot, readErr := transition.store.ReadCheckpointEvidence(
				journal.Transaction, step.ID, checkpoint,
			)
			if readErr != nil || !snapshot.Exists {
				return false, nil
			}
			evidence, parseErr := ParseEvidence(snapshot.Payload)
			if parseErr != nil || !postActivationEvidenceMatches(journal, step, checkpoint, evidence) {
				return false, nil
			}
			if checkpoint == EvidenceVerified && !evidenceRecordsEqual(evidence, *step.Evidence) {
				return false, nil
			}
		}
		recovery, readErr := transition.store.ReadRecovery(journal.Transaction, step.ID)
		if readErr != nil || recovery.Exists {
			return false, nil
		}
		ledger, reconstructed = advanced, protectedSnapshotFromPayload(payload)
	}
	pending, err := transition.registry.PendingPath(ledger)
	if err != nil {
		return false, err
	}
	return len(pending) == 0 && bytes.Equal(reconstructed.Payload, ledgerSnapshot.Payload), nil
}

func postActivationEvidenceMatches(
	journal JournalRecord,
	step JournalStep,
	checkpoint EvidenceCheckpoint,
	evidence EvidenceRecord,
) bool {
	observed := step.Desired
	if checkpoint == EvidenceCaptured {
		observed = step.Expected
	}
	return evidence.SchemaVersion == JournalSchemaV2 &&
		evidence.Transaction == journal.Transaction &&
		releasePairsEqual(evidence.Releases, journal.Releases) &&
		evidence.Step == step.ID && evidence.Expected == step.Expected &&
		evidence.Desired == step.Desired && evidence.Observed == observed &&
		evidence.Recovery == "" && evidence.Checkpoint == checkpoint
}

func evidenceRecordsEqual(left, right EvidenceRecord) bool {
	return left.SchemaVersion == right.SchemaVersion && left.Transaction == right.Transaction &&
		releasePairsEqual(left.Releases, right.Releases) && left.Step == right.Step &&
		left.Expected == right.Expected && left.Desired == right.Desired &&
		left.Observed == right.Observed && left.Recovery == right.Recovery &&
		left.Checkpoint == right.Checkpoint
}

func (transition *V2Transition) canReplacePreActivationJournal(
	observation v2Observation,
) bool {
	journal := observation.journal
	if journal == nil ||
		(journal.Checkpoint != JournalAuthorized && journal.Checkpoint != JournalMigrating) ||
		!initialReleaseLinks(observation.links, journal.Releases) ||
		len(observation.blockers) == 0 {
		return false
	}
	for _, blocker := range observation.blockers {
		if blocker.Code != CodePlanStale {
			return false
		}
	}
	return true
}

func (transition *V2Transition) observeFresh(
	ctx context.Context,
	observation *v2Observation,
) error {
	var ingressAfterSettings *V2IngressOperation
	var settingsView V2SettingsSnapshotView
	if transition.options.Ingress != nil {
		ingress, inspectErr := transition.options.Ingress.Inspect(ctx, nil)
		if inspectErr != nil {
			return inspectErr
		}
		ingress, inspectErr = normalizeV2IngressInspection(ingress)
		if inspectErr != nil {
			return inspectErr
		}
		observation.decisions = append(observation.decisions, ingress.Decisions...)
		observation.blockers = append(observation.blockers, ingress.Blockers...)
		settingsView = ingress.Prospective
		for index := range ingress.Operations {
			operation := ingress.Operations[index]
			if operation.Kind == V2SourceEntrypoints {
				copy := operation
				ingressAfterSettings = &copy
			} else {
				transition.appendFreshIngressWork(observation, operation)
			}
		}
	}
	pending, err := transition.registry.PendingPath(observation.ledger)
	if err != nil {
		return err
	}
	ledger := observation.ledger
	ledgerSnapshot := observation.ledgerSnapshot
	for _, migration := range pending {
		switch migration.Kind {
		case "test-vms-settings-v1-to-v2":
			plan, inspectErr := newTestVMSettingsV2Capability(
				transition.options.ConfigHome, transition.options.InheritedSettingIDs,
				settingsView,
			).Inspect()
			if inspectErr != nil {
				return inspectErr
			}
			observation.decisions = append(observation.decisions, plan.Decisions...)
			observation.blockers = append(observation.blockers, plan.Blockers...)
			for index := range plan.Files {
				file := plan.Files[index]
				if err := file.validate(); err != nil {
					return err
				}
				intent := settingsIntent(migration, file)
				observation.intents = append(observation.intents, intent)
				observation.observations = append(observation.observations, ResourceObservation{
					Resource: "yard." + file.Yard, Class: "yard-settings-file-v1",
					Fingerprint: file.ExpectedFingerprint,
				})
				copy := file
				observation.work = append(observation.work, v2Work{
					kind: v2SettingsWork, intent: intent, migration: migration, file: &copy,
				})
			}
			settingsView = settingsViewAfterPlan(
				transition.options.ConfigHome, settingsView, plan.Files,
			)
		case "test-yard-owner-v1-to-v2":
			plan, inspectErr := inspectOwnerRegistrationV2(
				ctx, transition.options.OwnerRegistration, settingsView,
			)
			if inspectErr != nil {
				return inspectErr
			}
			if plan != nil {
				intent := PlannerStepIntent{
					ID:        migration.ID + "." + ownerRegistrationResource,
					Migration: migration.ID, Resource: ownerRegistrationResource,
					Decision: DecisionCanonicalize,
					Expected: plan.ExpectedFingerprint, Desired: plan.DesiredFingerprint,
				}
				observation.decisions = append(observation.decisions, RedactedDecision{
					Resource: ownerRegistrationResource, Scope: "owner-registration",
					Decision: DecisionCanonicalize, Result: "test-yard",
				})
				observation.intents = append(observation.intents, intent)
				observation.observations = append(observation.observations, ResourceObservation{
					Resource: "owner-registration." + ownerRegistrationResource,
					Class:    "test-yard-owner-v1", Fingerprint: plan.ExpectedFingerprint,
				})
				observation.work = append(observation.work, v2Work{
					kind: v2OwnerWork, intent: intent, migration: migration, owner: plan,
				})
			}
		default:
			observation.blockers = append(observation.blockers, Blocker{
				Code: CodeUnsupportedKind, Resource: "migration." + migration.ID,
				Message: "the pending migration kind is not compiled into this engine",
				Retry:   "install an engine that supports the migration registry",
			})
			continue
		}
		if migration.Kind == "test-vms-settings-v1-to-v2" && ingressAfterSettings != nil {
			transition.appendFreshIngressWork(observation, *ingressAfterSettings)
			ingressAfterSettings = nil
		}
		advanced, advanceErr := ledger.Advance(transition.registry, migration)
		if advanceErr != nil {
			return advanceErr
		}
		payload, desired, marshalErr := MarshalLedgerV2(advanced, transition.registry)
		if marshalErr != nil {
			return marshalErr
		}
		ledgerIntent := PlannerStepIntent{
			ID: migration.ID + ".ledger", Migration: migration.ID,
			Resource: "ledger." + migration.Domain, Decision: DecisionTransform,
			Expected: ledgerSnapshot.Fingerprint, Desired: desired,
		}
		observation.intents = append(observation.intents, ledgerIntent)
		observation.observations = append(observation.observations, ResourceObservation{
			Resource: ledgerIntent.Resource, Class: "domain-ledger-v2",
			Fingerprint: ledgerSnapshot.Fingerprint,
		})
		observation.work = append(observation.work, v2Work{
			kind: v2LedgerWork, intent: ledgerIntent, migration: migration,
			ledgerFrom: ledgerSnapshot, ledgerTo: payload,
		})
		ledger = advanced
		ledgerSnapshot = protectedSnapshotFromPayload(payload)
	}
	if ingressAfterSettings != nil {
		transition.appendFreshIngressWork(observation, *ingressAfterSettings)
	}
	return nil
}

func (transition *V2Transition) appendFreshIngressWork(
	observation *v2Observation,
	operation V2IngressOperation,
) {
	intent := ingressIntent(operation)
	_, _, _, _, class := ingressOperationIDs(operation.Kind)
	observation.intents = append(observation.intents, intent)
	observation.observations = append(observation.observations, ResourceObservation{
		Resource: intent.Resource, Class: class, Fingerprint: operation.Expected,
	})
	copy := operation
	observation.work = append(observation.work, v2Work{
		kind: v2IngressWork, intent: intent, ingress: &copy,
	})
}

func (transition *V2Transition) observeResume(
	ctx context.Context,
	observation *v2Observation,
) error {
	journal := observation.journal
	if journal.ArtifactDigest != transition.options.ArtifactDigest ||
		journal.RegistryDigest != transition.registryDigest ||
		journal.CatalogDigest != transition.catalog.Digest() {
		observation.blockers = append(observation.blockers, Blocker{
			Code: CodePlanStale, Resource: "transition.bindings",
			Message: "the authorized migration bindings differ from this engine",
			Retry:   "run yard update --check",
		})
	}
	ingressBinding := V2IngressBinding{
		Transaction: journal.Transaction, Plan: journal.AuthorizationPlan,
		Releases: journal.Releases, Steps: ingressStepBindings(journal.Steps),
	}
	var ingressPlan V2IngressInspection
	var err error
	if transition.options.Ingress != nil {
		ingressPlan, err = transition.options.Ingress.Inspect(ctx, &ingressBinding)
		if err != nil {
			return err
		}
		ingressPlan, err = normalizeV2IngressInspection(ingressPlan)
		if err != nil {
			return err
		}
		observation.blockers = append(observation.blockers, ingressPlan.Blockers...)
	}
	capabilityPlan, err := newTestVMSettingsV2Capability(
		transition.options.ConfigHome, transition.options.InheritedSettingIDs,
		ingressPlan.Prospective,
	).Inspect()
	if err != nil {
		return err
	}
	observation.blockers = append(observation.blockers, capabilityPlan.Blockers...)
	ownerSettingsView := settingsViewAfterPlan(
		transition.options.ConfigHome, ingressPlan.Prospective, capabilityPlan.Files,
	)
	planned := make(map[string]settingsV2FilePlan, len(capabilityPlan.Files))
	for _, file := range capabilityPlan.Files {
		planned[file.Yard] = file
	}
	journalSettings := make(map[string]struct{})
	plannedIngress := make(map[V2IngressOperationKind]V2IngressOperation, len(ingressPlan.Operations))
	for _, operation := range ingressPlan.Operations {
		plannedIngress[operation.Kind] = operation
	}
	journalIngress := make(map[V2IngressOperationKind]struct{})
	journalOwner := false
	journalOwnerMigration := false
	migrationStarted := false
	sourceImportPending := false
	ownerRegistrationDeferred := false
	for _, step := range journal.Steps {
		if step.Checkpoint == StepApplied || step.Checkpoint == StepVerified {
			migrationStarted = true
		}
		intent := PlannerStepIntent{
			ID: step.ID, Migration: step.Migration, Resource: step.Resource,
			Decision: step.Decision, Expected: step.Expected, Desired: step.Desired,
		}
		observation.intents = append(observation.intents, intent)
		if kind, valid := ingressOperationKindForIntent(intent); valid {
			operation, planned := plannedIngress[kind]
			if !planned || operation.Decision != step.Decision ||
				operation.Expected != step.Expected || operation.Desired != step.Desired ||
				(step.Checkpoint != StepIntent &&
					(step.Evidence == nil || operation.Static != step.Evidence.Recovery)) ||
				transition.options.Ingress == nil {
				observation.blockers = append(observation.blockers, Blocker{
					Code: CodePlanStale, Resource: step.Resource,
					Message: "the journaled ingress operation cannot be reconstructed exactly",
					Retry:   "run yard update --check",
				})
				continue
			}
			bound, bindErr := ingressStep(ingressBinding, operation)
			if bindErr != nil {
				return bindErr
			}
			actual, observeErr := transition.options.Ingress.Observe(ctx, bound)
			if observeErr != nil || (actual != step.Expected && actual != step.Desired) {
				observation.blockers = append(observation.blockers, Blocker{
					Code: CodeMigrationStale, Resource: step.Resource,
					Message: "the journaled ingress operation changed outside the authorized transition",
					Retry:   "run yard update --check",
				})
				continue
			}
			if step.Checkpoint == StepEvidence && actual == step.Desired {
				migrationStarted = true
			}
			if kind == V2SourceImport && actual == step.Expected {
				sourceImportPending = true
				ownerRegistrationDeferred = true
			}
			journalIngress[kind] = struct{}{}
			_, _, _, scope, class := ingressOperationIDs(kind)
			observation.decisions = append(observation.decisions, RedactedDecision{
				Resource: step.Resource, Scope: scope,
				Decision: step.Decision, Result: "journaled",
			})
			observation.observations = append(observation.observations, ResourceObservation{
				Resource: step.Resource, Class: class, Fingerprint: actual,
			})
			copy, bindingCopy := operation, ingressBinding
			observation.work = append(observation.work, v2Work{
				kind: v2IngressWork, intent: intent, ingress: &copy, ingressBind: &bindingCopy,
			})
			continue
		}
		migration, exists := transition.migration(step.Migration)
		if !exists {
			return invalid("journal references unknown migration %q", step.Migration)
		}
		if migration.Kind == "test-yard-owner-v1-to-v2" {
			journalOwnerMigration = true
		}
		if strings.HasPrefix(step.Resource, "ledger.") {
			ledgerFrom, reconstructErr := transition.ledgerSnapshotForFingerprint(step.Expected)
			if reconstructErr != nil {
				return reconstructErr
			}
			ledgerTo, advanceErr := observationLedgerAdvance(
				transition.registry, migration, ledgerFrom,
			)
			if advanceErr != nil || fingerprintPayload(ledgerTo) != step.Desired {
				return invalid("journaled ledger transition cannot be reconstructed")
			}
			observation.observations = append(observation.observations, ResourceObservation{
				Resource: step.Resource, Class: "domain-ledger-v2",
				Fingerprint: observation.ledgerSnapshot.Fingerprint,
			})
			if step.Checkpoint == StepEvidence &&
				observation.ledgerSnapshot.Fingerprint == step.Desired {
				migrationStarted = true
			}
			observation.work = append(observation.work, v2Work{
				kind: v2LedgerWork, intent: intent, migration: migration,
				ledgerFrom: ledgerFrom, ledgerTo: ledgerTo,
			})
			continue
		}
		if migration.Kind == "test-yard-owner-v1-to-v2" {
			if step.Resource != ownerRegistrationResource {
				return invalid("journaled owner-registration intent is invalid")
			}
			journalOwner = true
			before, bindErr := transition.ownerRegistrationBefore(
				ctx, *journal, step, ownerSettingsView,
			)
			if bindErr != nil {
				observation.blockers = append(observation.blockers, Blocker{
					Code: CodeMigrationStale, Resource: "owner-registration." + step.Resource,
					Message: "the journaled owner registration identity changed outside the authorized transition",
					Retry:   "run yard update --check",
				})
				continue
			}
			progress := OwnerRegistrationExpected
			var observeErr error
			if !ownerRegistrationDeferred {
				progress, observeErr = transition.options.OwnerRegistration.Observe(ctx, before)
			}
			if observeErr != nil {
				observation.blockers = append(observation.blockers, Blocker{
					Code: CodeMigrationStale, Resource: "owner-registration." + step.Resource,
					Message: "the journaled owner registration cannot be observed safely",
					Retry:   "repair the test-yard owner topology, then run yard update",
				})
				continue
			}
			actual, progressErr := ownerRegistrationProgressFingerprint(
				progress, step.Expected, step.Desired,
			)
			if progressErr != nil {
				return progressErr
			}
			if actual != step.Expected && actual != step.Desired {
				observation.blockers = append(observation.blockers, Blocker{
					Code: CodeMigrationStale, Resource: "owner-registration." + step.Resource,
					Message: "the journaled owner registration changed outside the authorized transition",
					Retry:   "run yard update --check",
				})
			}
			observation.decisions = append(observation.decisions, RedactedDecision{
				Resource: step.Resource, Scope: "owner-registration",
				Decision: step.Decision, Result: "journaled",
			})
			observation.observations = append(observation.observations, ResourceObservation{
				Resource: "owner-registration." + step.Resource,
				Class:    "test-yard-owner-v1", Fingerprint: actual,
			})
			observation.work = append(observation.work, v2Work{
				kind: v2OwnerWork, intent: intent, migration: migration,
				owner: &ownerRegistrationV2Plan{
					Before: before, ExpectedFingerprint: step.Expected,
					DesiredFingerprint: step.Desired,
				},
			})
			continue
		}
		if migration.Kind != "test-vms-settings-v1-to-v2" {
			return invalid("journal references unsupported migration kind %q", migration.Kind)
		}
		journalSettings[step.Resource] = struct{}{}
		var file *settingsV2FilePlan
		if plannedFile, ok := planned[step.Resource]; ok {
			copy := plannedFile
			file = &copy
		}
		if file != nil && (file.Decision != step.Decision ||
			file.ExpectedFingerprint != step.Expected || file.DesiredFingerprint != step.Desired) {
			observation.blockers = append(observation.blockers, Blocker{
				Code: CodePlanStale, Resource: "yard." + step.Resource,
				Message: "the journaled yard settings plan cannot be reconstructed exactly",
				Retry:   "run yard update --check",
			})
			continue
		}
		deferred := false
		var observeErr error
		if sourceImportPending && file != nil {
			deferred, observeErr = transition.settingsAwaitSourceImport(*file)
		}
		var actual Fingerprint
		if deferred {
			actual = step.Expected
		} else if observeErr == nil {
			_, _, actual, observeErr = transition.observeSettingsWork(step.Resource)
		}
		if observeErr != nil {
			observation.blockers = append(observation.blockers, Blocker{
				Code: CodeMigrationStale, Resource: "yard." + step.Resource,
				Message: "the journaled yard settings cannot be observed safely",
				Retry:   "repair the named yard settings, then run yard update",
			})
			continue
		}
		if step.Checkpoint == StepEvidence && actual == step.Desired {
			migrationStarted = true
		}
		if actual != step.Expected && actual != step.Desired {
			observation.blockers = append(observation.blockers, Blocker{
				Code: CodeMigrationStale, Resource: "yard." + step.Resource,
				Message: "the journaled yard settings changed outside the authorized transition",
				Retry:   "run yard update --check",
			})
		}
		if step.Resource == legacyOwnerRegistrationYard && actual == step.Expected {
			ownerRegistrationDeferred = true
		}
		observation.decisions = append(observation.decisions, RedactedDecision{
			Resource: "yard." + step.Resource, Scope: "yard",
			Decision: step.Decision, Result: "journaled",
		})
		observation.observations = append(observation.observations, ResourceObservation{
			Resource: "yard." + step.Resource, Class: "yard-settings-file-v1",
			Fingerprint: actual,
		})
		observation.work = append(observation.work, v2Work{
			kind: v2SettingsWork, intent: intent, migration: migration, file: file,
		})
	}
	for yard := range planned {
		if _, authorized := journalSettings[yard]; !authorized {
			observation.blockers = append(observation.blockers, Blocker{
				Code: CodePlanStale, Resource: "yard." + yard,
				Message: "a new migration impact appeared after authorization",
				Retry:   "run yard update --check",
			})
		}
	}
	for kind := range plannedIngress {
		if _, authorized := journalIngress[kind]; !authorized {
			_, _, resource, _, _ := ingressOperationIDs(kind)
			observation.blockers = append(observation.blockers, Blocker{
				Code: CodePlanStale, Resource: resource,
				Message: "a new ingress impact appeared after authorization",
				Retry:   "run yard update --check",
			})
		}
	}
	if journalOwnerMigration && !journalOwner {
		plan, inspectErr := inspectOwnerRegistrationV2(ctx, transition.options.OwnerRegistration, nil)
		if inspectErr != nil {
			return inspectErr
		}
		if plan != nil {
			code := CodePlanStale
			message := "a new owner-registration migration impact appeared after authorization"
			if migrationStarted {
				code = CodeMigrationStale
				message = "a new owner-registration impact appeared after authorized migration work began"
			}
			observation.blockers = append(observation.blockers, Blocker{
				Code: code, Resource: "owner-registration." + ownerRegistrationResource,
				Message: message, Retry: "run yard update --check",
			})
		}
	}
	return nil
}

func (transition *V2Transition) convergeStep(
	ctx context.Context,
	journal *JournalRecord,
	journalSnapshot *ProtectedSnapshot,
	index int,
	work v2Work,
	links ReleaseLinks,
) (Outcome, error) {
	step := &journal.Steps[index]
	if work.kind == v2IngressWork {
		binding := V2IngressBinding{
			Transaction: journal.Transaction, Plan: journal.AuthorizationPlan,
			Releases: journal.Releases, Steps: ingressStepBindings(journal.Steps),
		}
		work.ingressBind = &binding
	}
	var recovery Fingerprint
	needsRecovery := work.kind == v2SettingsWork || work.kind == v2OwnerWork ||
		(work.kind == v2IngressWork && work.ingress != nil &&
			work.ingress.Kind == V2LegacyV1Import)
	if needsRecovery {
		var err error
		switch work.kind {
		case v2SettingsWork:
			recovery, err = transition.ensureRecovery(*journal, *step, work)
		case v2OwnerWork:
			var token string
			recovery, token, err = transition.ensureOwnerRecovery(*journal, *step, work)
			if err == nil && work.owner != nil {
				work.owner.Before.RecoveryToken = token
			}
		case v2IngressWork:
			recovery = work.ingress.Static
			err = validateFingerprint(recovery, "legacy ingress static fingerprint")
		}
		if err != nil {
			return Outcome{}, err
		}
		if step.Checkpoint != StepIntent &&
			(step.Evidence == nil || step.Evidence.Recovery != recovery) {
			return Outcome{}, invalid("journaled migration recovery binding is missing or stale")
		}
	}
	if step.Checkpoint == StepIntent {
		if work.kind == v2SettingsWork || work.kind == v2OwnerWork {
			if err := transition.inject("after-recovery"); err != nil {
				return Outcome{}, err
			}
		}
		evidence := evidenceFor(*journal, *step, EvidenceCaptured, step.Expected, recovery)
		if err := transition.persistEvidence(evidence); err != nil {
			return Outcome{}, err
		}
		step.Checkpoint, step.Evidence = StepEvidence, &evidence
		if journal.Checkpoint == JournalAuthorized {
			journal.Checkpoint = JournalMigrating
		}
		if err := transition.persistJournal(journal, journalSnapshot); err != nil {
			return Outcome{}, err
		}
		if err := transition.inject("after-step-evidence"); err != nil {
			return Outcome{}, err
		}
	}
	if step.Checkpoint == StepEvidence {
		actual, err := transition.workFingerprint(ctx, work)
		if err != nil {
			return Outcome{}, err
		}
		if actual == step.Expected || work.kind == v2OwnerWork && actual == step.Desired {
			switch work.kind {
			case v2SettingsWork:
				if work.file == nil {
					return v2OperatorOutcome(links, journal.Goal.Target,
						transactionIDPointer(journal.Transaction), CodeRecoveryAmbiguous,
						"the expected settings recipe cannot be reconstructed",
						"run yard update --check"), nil
				}
				expected, path, observed, observeErr := transition.observeSettingsWork(
					work.intent.Resource,
				)
				if observeErr != nil || path != work.file.Path ||
					observed != step.Expected {
					return v2OperatorOutcome(links, journal.Goal.Target,
						transactionIDPointer(journal.Transaction), CodeMigrationStale,
						"the settings resource changed before its protected update",
						"run yard update --check"), nil
				}
				if err := config.CompareAndSwapPersistentFile(
					transition.options.ConfigHome, work.file.Path,
					expected, work.file.Desired,
				); err != nil && !errors.Is(err, config.ErrPersistentTargetStale) {
					return Outcome{}, err
				}
				if err := transition.inject("after-settings-cas"); err != nil {
					return Outcome{}, err
				}
			case v2OwnerWork:
				if work.owner == nil || transition.options.OwnerRegistration == nil {
					return v2OperatorOutcome(links, journal.Goal.Target,
						transactionIDPointer(journal.Transaction), CodeRecoveryAmbiguous,
						"the expected owner-registration recipe cannot be reconstructed",
						"run yard update --check"), nil
				}
				if err := transition.options.OwnerRegistration.Commit(ctx, work.owner.Before); err != nil {
					return Outcome{}, err
				}
				if err := transition.inject("after-owner-registration-commit"); err != nil {
					return Outcome{}, err
				}
			case v2IngressWork:
				stepRequest, requestErr := transition.ingressStepForWork(work)
				if requestErr != nil {
					return Outcome{}, requestErr
				}
				if err := transition.options.Ingress.Apply(ctx, stepRequest); err != nil {
					return Outcome{}, err
				}
				if err := transition.inject("after-source-" + string(stepRequest.Kind) + "-apply"); err != nil {
					return Outcome{}, err
				}
			case v2LedgerWork:
				if len(work.ledgerTo) == 0 {
					advanced, advanceErr := observationLedgerAdvance(
						transition.registry, work.migration, work.ledgerFrom,
					)
					if advanceErr != nil {
						return Outcome{}, advanceErr
					}
					work.ledgerTo = advanced
				}
				if err := transition.store.CompareAndSwapLedger(work.ledgerFrom, work.ledgerTo); err != nil && !errors.Is(err, ErrProtectedStoreStale) {
					return Outcome{}, err
				}
				if err := transition.inject("after-ledger-cas"); err != nil {
					return Outcome{}, err
				}
			}
		}
		actual, err = transition.workFingerprint(ctx, work)
		if err != nil {
			return Outcome{}, err
		}
		if actual != step.Desired {
			return v2OperatorOutcome(links, journal.Goal.Target,
				transactionIDPointer(journal.Transaction), CodeMigrationStale,
				"a migration resource differs from both expected and desired state",
				"run yard update --check"), nil
		}
		evidence := evidenceFor(*journal, *step, EvidenceApplied, step.Desired, recovery)
		if err := transition.persistEvidence(evidence); err != nil {
			return Outcome{}, err
		}
		step.Checkpoint, step.Evidence = StepApplied, &evidence
		if err := transition.persistJournal(journal, journalSnapshot); err != nil {
			return Outcome{}, err
		}
		if work.kind == v2SettingsWork || work.kind == v2OwnerWork {
			if err := transition.inject("after-step-applied"); err != nil {
				return Outcome{}, err
			}
		}
	}
	if step.Checkpoint == StepApplied {
		actual, err := transition.workFingerprint(ctx, work)
		if err != nil {
			return Outcome{}, err
		}
		if actual != step.Desired {
			return v2OperatorOutcome(links, journal.Goal.Target,
				transactionIDPointer(journal.Transaction), CodeVerificationFailed,
				"a migration resource failed typed verification",
				"run yard update --check"), nil
		}
		if work.kind == v2IngressWork {
			stepRequest, requestErr := transition.ingressStepForWork(work)
			if requestErr != nil {
				return Outcome{}, requestErr
			}
			if err := transition.options.Ingress.Verify(ctx, stepRequest); err != nil {
				return Outcome{}, err
			}
			actual, err = transition.workFingerprint(ctx, work)
			if err != nil {
				return Outcome{}, err
			}
			if actual != step.Desired {
				return v2OperatorOutcome(links, journal.Goal.Target,
					transactionIDPointer(journal.Transaction), CodeVerificationFailed,
					"a source operation failed typed verification",
					"run yard update --check"), nil
			}
		}
		evidence := evidenceFor(*journal, *step, EvidenceVerified, step.Desired, recovery)
		if err := transition.persistEvidence(evidence); err != nil {
			return Outcome{}, err
		}
		if work.kind == v2IngressWork && work.ingress != nil &&
			work.ingress.Kind == V2LegacyV1Import {
			if journal.Releases.Previous == nil {
				return Outcome{}, invalid("legacy import compatibility evidence has no previous release")
			}
			payload, marshalErr := MarshalCompatibilityEvidence(CompatibilityEvidence{
				SchemaVersion: CompatibilityEvidenceSchemaV1,
				Kind:          V2LegacyV1Import,
				Identity:      recovery,
				From:          journal.Releases.From,
				Previous:      *journal.Releases.Previous,
			})
			if marshalErr != nil {
				return Outcome{}, marshalErr
			}
			if err := transition.store.CreateCompatibilityEvidence(recovery, payload); err != nil {
				return Outcome{}, err
			}
		}
		step.Checkpoint, step.Evidence = StepVerified, &evidence
		if err := transition.persistJournal(journal, journalSnapshot); err != nil {
			return Outcome{}, err
		}
		point := "after-step-verified"
		if work.kind == v2LedgerWork {
			point = "after-ledger-verified"
		}
		if err := transition.inject(point); err != nil {
			return Outcome{}, err
		}
	}
	return Outcome{}, nil
}

func (transition *V2Transition) newJournal(
	observation v2Observation,
	authorizationPlan PlanToken,
	authorization Authorization,
) (JournalRecord, error) {
	transaction := transition.options.NewTransactionID()
	if requiresSupersededJournalArchive(observation.replacement) {
		resolved, err := transition.store.ResolveSupersededJournalTransaction(
			transaction, *observation.replacement, authorizationPlan,
		)
		if err != nil {
			return JournalRecord{}, err
		}
		transaction = resolved
	}
	if err := validateTransactionID(transaction); err != nil {
		return JournalRecord{}, err
	}
	resumePlan, err := BindResumePlan(ResumePlanFacts{
		Goal: observation.goal, Releases: transition.options.Releases,
		ArtifactDigest: transition.options.ArtifactDigest,
		RegistryDigest: transition.registryDigest, CatalogDigest: transition.catalog.Digest(),
		ObservationScope: observation.observationScope,
		Assessment:       observation.assessment.Clone(), Decisions: slices.Clone(observation.decisions),
		Intents: slices.Clone(observation.intents), Blockers: []Blocker{},
		Transaction: transaction, AuthorizationPlan: authorizationPlan,
	})
	if err != nil {
		return JournalRecord{}, err
	}
	steps := make([]JournalStep, len(observation.intents))
	for index, intent := range observation.intents {
		steps[index] = JournalStep{
			ID: intent.ID, Migration: intent.Migration, Resource: intent.Resource,
			Decision: intent.Decision, Expected: intent.Expected, Desired: intent.Desired,
			Checkpoint: StepIntent,
		}
	}
	record := JournalRecord{
		SchemaVersion: JournalSchemaV2, Transaction: transaction,
		Goal: observation.goal, Releases: transition.options.Releases,
		AuthorizationPlan: authorizationPlan, ResumePlan: resumePlan,
		ArtifactDigest: transition.options.ArtifactDigest,
		RegistryDigest: transition.registryDigest, CatalogDigest: transition.catalog.Digest(),
		ObservationScope:    observation.observationScope,
		AuthorizationDigest: fingerprintPayload([]byte(authorization)),
		SourceIngress:       cloneSourceIngressRequest(transition.options.SourceIngress),
		Checkpoint:          JournalAuthorized, Steps: steps,
	}
	record.IntentDigest = bindJournalIntent(
		record.AuthorizationPlan, record.ResumePlan, record.ObservationScope, record.Steps,
	)
	return record, record.Validate()
}

func requiresSupersededJournalArchive(replacement *JournalReplacement) bool {
	return replacement != nil &&
		replacement.Reason == JournalReplacementPostActivationScopeV0111
}

func (transition *V2Transition) planFacts(observation v2Observation) PlanFacts {
	return PlanFacts{
		Goal: observation.goal, Releases: transition.options.Releases, Links: observation.links,
		ArtifactDigest: transition.options.ArtifactDigest,
		RegistryDigest: transition.registryDigest, CatalogDigest: transition.catalog.Digest(),
		ObservationScope: observation.observationScope,
		Assessment:       observation.assessment.Clone(), Decisions: slices.Clone(observation.decisions),
		Observations: slices.Clone(observation.observations), Intents: slices.Clone(observation.intents),
		Blockers: slices.Clone(observation.blockers), Replacement: observation.replacement,
	}
}

func (transition *V2Transition) persistJournal(
	journal *JournalRecord,
	snapshot *ProtectedSnapshot,
) error {
	payload, err := MarshalJournal(*journal)
	if err != nil {
		return err
	}
	if err := transition.store.CompareAndSwapCurrentJournal(*snapshot, payload); err != nil {
		return err
	}
	*snapshot = protectedSnapshotFromPayload(payload)
	return nil
}

func (transition *V2Transition) persistEvidence(record EvidenceRecord) error {
	payload, err := MarshalEvidence(record)
	if err != nil {
		return err
	}
	return transition.store.CreateCheckpointEvidence(
		record.Transaction, record.Step, record.Checkpoint, payload,
	)
}

func (transition *V2Transition) ensureRecovery(
	journal JournalRecord,
	step JournalStep,
	work v2Work,
) (Fingerprint, error) {
	existing, err := transition.store.ReadRecovery(journal.Transaction, step.ID)
	if err != nil {
		return "", err
	}
	if existing.Exists {
		var recovery settingsRecoveryV1
		if err := decodeBoundedRecord(existing.Payload, 4096, &recovery); err != nil {
			return "", err
		}
		if err := validateSettingsRecovery(recovery, journal, step); err != nil {
			return "", err
		}
		if work.file != nil && !slices.Equal(recovery.Before, work.file.Before) {
			return "", invalid("typed settings recovery does not match the inspected before state")
		}
		return existing.Fingerprint, nil
	}
	if work.file == nil {
		return "", invalid("settings recovery evidence is missing before mutation")
	}
	recovery := settingsRecoveryV1{
		SchemaVersion: 1, Transaction: journal.Transaction,
		Step: step.ID, Migration: step.Migration, Yard: step.Resource,
		Expected: step.Expected,
		Before:   slices.Clone(work.file.Before),
	}
	if err := validateSettingsRecovery(recovery, journal, step); err != nil {
		return "", err
	}
	payload, err := json.Marshal(recovery)
	if err != nil {
		return "", err
	}
	payload = append(payload, '\n')
	if err := transition.store.CreateRecovery(journal.Transaction, step.ID, payload); err != nil {
		return "", err
	}
	return fingerprintPayload(payload), nil
}

func (transition *V2Transition) ensureOwnerRecovery(
	journal JournalRecord,
	step JournalStep,
	work v2Work,
) (Fingerprint, string, error) {
	existing, err := transition.store.ReadRecovery(journal.Transaction, step.ID)
	if err != nil {
		return "", "", err
	}
	if existing.Exists {
		var recovery ownerRegistrationRecoveryV1
		if err := decodeBoundedRecord(existing.Payload, 4096, &recovery); err != nil {
			return "", "", err
		}
		if err := validateOwnerRegistrationRecovery(recovery, journal, step); err != nil {
			return "", "", err
		}
		if work.owner != nil && ownerRegistrationFingerprint(recovery.Before) !=
			ownerRegistrationFingerprint(work.owner.Before) {
			return "", "", invalid("typed owner-registration recovery does not match the inspected before state")
		}
		return existing.Fingerprint, recovery.Token, nil
	}
	if work.owner == nil {
		return "", "", invalid("owner-registration recovery evidence is missing before mutation")
	}
	token, err := newOwnerRecoveryToken()
	if err != nil {
		return "", "", err
	}
	before := work.owner.Before
	before.RecoveryToken = ""
	before.TerminalCleanup = false
	recovery := ownerRegistrationRecoveryV1{
		SchemaVersion: 1, Transaction: journal.Transaction,
		Step: step.ID, Migration: step.Migration, Resource: step.Resource,
		Expected: step.Expected, Token: token, Before: before,
	}
	if err := validateOwnerRegistrationRecovery(recovery, journal, step); err != nil {
		return "", "", err
	}
	payload, err := json.Marshal(recovery)
	if err != nil {
		return "", "", err
	}
	payload = append(payload, '\n')
	if err := transition.store.CreateRecovery(journal.Transaction, step.ID, payload); err != nil {
		return "", "", err
	}
	return fingerprintPayload(payload), token, nil
}

func (transition *V2Transition) ownerRegistrationBefore(
	ctx context.Context,
	journal JournalRecord,
	step JournalStep,
	prospective V2SettingsSnapshotView,
) (OwnerRegistrationObservation, error) {
	existing, err := transition.store.ReadRecovery(journal.Transaction, step.ID)
	if err != nil {
		return OwnerRegistrationObservation{}, err
	}
	if existing.Exists {
		var recovery ownerRegistrationRecoveryV1
		if err := decodeBoundedRecord(existing.Payload, 4096, &recovery); err != nil {
			return OwnerRegistrationObservation{}, err
		}
		if err := validateOwnerRegistrationRecovery(recovery, journal, step); err != nil {
			return OwnerRegistrationObservation{}, err
		}
		before := recovery.Before
		before.RecoveryToken = recovery.Token
		before.TerminalCleanup = step.Checkpoint == StepVerified
		return before, nil
	}
	if step.Checkpoint != StepIntent || transition.options.OwnerRegistration == nil {
		return OwnerRegistrationObservation{}, invalid(
			"journaled owner-registration recovery binding is unavailable",
		)
	}
	before, err := transition.options.OwnerRegistration.Prepare(ctx, prospective)
	if err != nil {
		return OwnerRegistrationObservation{}, err
	}
	if err := validateOwnerRegistrationObservation(before); err != nil {
		return OwnerRegistrationObservation{}, err
	}
	if before.State == OwnerRegistrationAbsent || before.State == OwnerRegistrationCurrent ||
		ownerRegistrationFingerprint(before) != step.Expected ||
		ownerRegistrationFingerprint(desiredOwnerRegistrationObservation(before)) != step.Desired {
		return OwnerRegistrationObservation{}, invalid(
			"journaled owner-registration before identity is stale",
		)
	}
	return before, nil
}

func validateSettingsRecovery(
	recovery settingsRecoveryV1,
	journal JournalRecord,
	step JournalStep,
) error {
	if recovery.SchemaVersion != 1 || recovery.Transaction != journal.Transaction ||
		recovery.Step != step.ID || recovery.Migration != step.Migration ||
		recovery.Yard != step.Resource || recovery.Expected != step.Expected ||
		!domain.SafeName(recovery.Yard) || len(recovery.Before) > 2 {
		return invalid("typed settings recovery evidence is invalid")
	}
	seen := make(map[string]struct{}, len(recovery.Before))
	for _, value := range recovery.Before {
		if !affectedSettingsV2ID(value.Setting) {
			return invalid("typed settings recovery references an unknown setting")
		}
		if _, duplicate := seen[value.Setting]; duplicate {
			return invalid("typed settings recovery contains a duplicate setting")
		}
		seen[value.Setting] = struct{}{}
		if value.Present {
			switch value.Setting {
			case "YARD_TEMPLATE":
				if value.Value != "e2e-vms" && value.Value != "test-vms" {
					return invalid("typed settings recovery contains an unsupported template")
				}
			case "NESTED_E2E_VMS":
				if value.Value != "0" && value.Value != "1" {
					return invalid("typed settings recovery contains an unsupported boolean")
				}
			}
		} else if value.Value != "" {
			return invalid("absent typed settings recovery has a value")
		}
	}
	return nil
}

func (transition *V2Transition) workFingerprint(
	ctx context.Context,
	work v2Work,
) (Fingerprint, error) {
	switch work.kind {
	case v2IngressWork:
		step, err := transition.ingressStepForWork(work)
		if err != nil {
			return "", err
		}
		actual, err := transition.options.Ingress.Observe(ctx, step)
		if err != nil {
			return "", err
		}
		if err := validateFingerprint(actual, "ingress actual fingerprint"); err != nil {
			return "", err
		}
		return actual, nil
	case v2SettingsWork:
		_, _, actual, err := transition.observeSettingsWork(work.intent.Resource)
		if err != nil {
			return "", err
		}
		return actual, nil
	case v2OwnerWork:
		if work.owner == nil || transition.options.OwnerRegistration == nil {
			return "", invalid("owner-registration work is unavailable")
		}
		progress, err := transition.options.OwnerRegistration.Observe(ctx, work.owner.Before)
		if err != nil {
			return "", err
		}
		return ownerRegistrationProgressFingerprint(
			progress, work.owner.ExpectedFingerprint, work.owner.DesiredFingerprint,
		)
	case v2LedgerWork:
		snapshot, err := transition.store.ReadLedger()
		if err != nil {
			return "", err
		}
		return snapshot.Fingerprint, nil
	default:
		return "", invalid("unknown v2 migration work kind")
	}
}

func (transition *V2Transition) ingressStepForWork(work v2Work) (V2IngressStep, error) {
	if transition.options.Ingress == nil || work.ingress == nil || work.ingressBind == nil {
		return V2IngressStep{}, invalid("ingress work is unavailable")
	}
	return ingressStep(*work.ingressBind, *work.ingress)
}

func (transition *V2Transition) observeYard(
	yard string,
) (config.PersistentFileSnapshot, string, error) {
	if !domain.SafeName(yard) {
		return config.PersistentFileSnapshot{}, "", invalid("journal yard is unsafe")
	}
	nestedPath := filepath.Join(transition.options.ConfigHome, "yards", yard, "config.env")
	legacyPath := filepath.Join(transition.options.ConfigHome, "yards", yard+".env")
	nested, nestedErr := config.ReadPersistentFileSnapshot(transition.options.ConfigHome, nestedPath)
	legacy, legacyErr := config.ReadPersistentFileSnapshot(transition.options.ConfigHome, legacyPath)
	if nestedErr != nil || legacyErr != nil || nested.Exists == legacy.Exists {
		return config.PersistentFileSnapshot{}, "", errors.New("journaled yard settings ownership is ambiguous")
	}
	if nested.Exists {
		return nested, nestedPath, nil
	}
	return legacy, legacyPath, nil
}

func (transition *V2Transition) observeSettingsWork(
	yard string,
) (config.PersistentFileSnapshot, string, Fingerprint, error) {
	snapshot, path, err := transition.observeYard(yard)
	if err != nil {
		return config.PersistentFileSnapshot{}, "", "", err
	}
	_, inheritance, blocker, err := newTestVMSettingsV2Capability(
		transition.options.ConfigHome, transition.options.InheritedSettingIDs,
	).observeInheritedSettings()
	if err != nil {
		return config.PersistentFileSnapshot{}, "", "", err
	}
	if blocker != nil {
		return config.PersistentFileSnapshot{}, "", "", errors.New(blocker.Message)
	}
	return snapshot, path, settingsV2ResourceFingerprint(snapshot, inheritance), nil
}

func (transition *V2Transition) settingsAwaitSourceImport(file settingsV2FilePlan) (bool, error) {
	nestedPath := filepath.Join(transition.options.ConfigHome, "yards", file.Yard, "config.env")
	if !file.Expected.Exists || file.Path != nestedPath {
		return false, nil
	}
	legacyPath := filepath.Join(transition.options.ConfigHome, "yards", file.Yard+".env")
	nested, nestedErr := config.ReadPersistentFileSnapshot(transition.options.ConfigHome, nestedPath)
	legacy, legacyErr := config.ReadPersistentFileSnapshot(transition.options.ConfigHome, legacyPath)
	if nestedErr != nil || legacyErr != nil {
		return false, errors.Join(nestedErr, legacyErr)
	}
	return !nested.Exists && !legacy.Exists, nil
}

func (transition *V2Transition) migration(id string) (MigrationDefinitionV2, bool) {
	for _, migration := range transition.registry.Migrations {
		if migration.ID == id {
			return migration, true
		}
	}
	return MigrationDefinitionV2{}, false
}

func (transition *V2Transition) fixedPoint(observation v2Observation) bool {
	pending, err := transition.registry.PendingPath(observation.ledger)
	if err != nil || len(pending) != 0 || !observation.activationFixed {
		return false
	}
	if transition.options.Releases.From == transition.options.Releases.Target {
		return initialReleaseLinks(observation.links, transition.options.Releases)
	}
	return activatedReleaseLinks(observation.links, transition.options.Releases)
}

func (transition *V2Transition) completedJournalMatches(observation v2Observation) bool {
	journal := observation.journal
	if journal == nil || journal.Checkpoint != JournalComplete ||
		journal.Goal != observation.goal ||
		!releasePairsEqual(journal.Releases, transition.options.Releases) ||
		journal.ArtifactDigest != transition.options.ArtifactDigest ||
		journal.RegistryDigest != transition.registryDigest ||
		journal.CatalogDigest != transition.catalog.Digest() {
		return false
	}
	if journal.Releases.From == journal.Releases.Target {
		return initialReleaseLinks(observation.links, journal.Releases)
	}
	return activatedReleaseLinks(observation.links, journal.Releases)
}

func (transition *V2Transition) observeActivation(
	ctx context.Context,
	observation *v2Observation,
) error {
	observation.activationFixed = true
	seen := make(map[string]struct{}, len(transition.options.Reconcilers))
	for _, reconciler := range transition.options.Reconcilers {
		if reconciler == nil {
			return errors.New("nil activation reconciler")
		}
		id := reconciler.ID()
		if err := validateSafeID(id, "activation reconciler ID"); err != nil {
			return err
		}
		if _, duplicate := seen[id]; duplicate {
			return invalid("duplicate activation reconciler %q", id)
		}
		seen[id] = struct{}{}
		actual, err := reconciler.Observe(ctx, transition.options.Releases, observation.links)
		if err != nil {
			observation.activationFixed = false
			observation.blockers = append(
				observation.blockers,
				activationObservationBlocker(id, err),
			)
			continue
		}
		if err := validateActivationObservation(id, actual); err != nil {
			return err
		}
		payload, err := json.Marshal(struct {
			ID      string      `json:"id"`
			Actual  Fingerprint `json:"actual"`
			Desired Fingerprint `json:"desired"`
		}{ID: id, Actual: actual.Actual, Desired: actual.Desired})
		if err != nil {
			return err
		}
		observation.observations = append(observation.observations, ResourceObservation{
			Resource: "activation." + id, Class: "activation-reconciler-v1",
			Fingerprint: fingerprintPayload(payload),
		})
		observation.activationScope = append(observation.activationScope, v2ActivationScope{
			ID: id, Desired: actual.Desired,
		})
		observation.activationFixed = observation.activationFixed && actual.Converged
	}
	return nil
}

func (transition *V2Transition) bindObservationScope(
	activation []v2ActivationScope,
) (Fingerprint, error) {
	if len(activation) > MaxPlanItems || len(transition.options.InheritedSettingIDs) > MaxPlanItems {
		return "", invalid("release transition observation scope is too large")
	}
	activation = slices.Clone(activation)
	for _, scoped := range activation {
		if err := validateSafeID(scoped.ID, "activation reconciler ID"); err != nil {
			return "", err
		}
		if err := validateFingerprint(scoped.Desired, "activation desired fingerprint"); err != nil {
			return "", err
		}
	}
	inherited := slices.Clone(transition.options.InheritedSettingIDs)
	slices.Sort(inherited)
	inherited = slices.Compact(inherited)
	for _, setting := range inherited {
		if err := validateSafeID(setting, "inherited setting ID"); err != nil {
			return "", err
		}
	}
	payload, err := json.Marshal(struct {
		SchemaVersion         int                 `json:"schemaVersion"`
		ActivationReconcilers []v2ActivationScope `json:"activationReconcilers"`
		InheritedSettingIDs   []string            `json:"inheritedSettingIds"`
	}{
		SchemaVersion: 1, ActivationReconcilers: activation,
		InheritedSettingIDs: inherited,
	})
	if err != nil {
		return "", err
	}
	return fingerprintPayload(payload), nil
}

func (transition *V2Transition) reconcileActivation(
	ctx context.Context,
	journal JournalRecord,
) (Outcome, error) {
	for index, reconciler := range transition.options.Reconcilers {
		if reconciler == nil {
			return Outcome{}, errors.New("nil activation reconciler")
		}
		id := reconciler.ID()
		if err := validateSafeID(id, "activation reconciler ID"); err != nil {
			return Outcome{}, err
		}
		if err := transition.inject(fmt.Sprintf("before-reconciler-%d", index)); err != nil {
			return Outcome{}, err
		}
		links, guardOutcome, guardErr := transition.guardJournalLinks(ctx, journal)
		if guardErr != nil || guardOutcome.Code != "" {
			return guardOutcome, guardErr
		}
		before, observeErr := reconciler.Observe(ctx, journal.Releases, links)
		if observeErr != nil {
			blocker := activationObservationBlocker(id, observeErr)
			if blocker.Code == CodeActivationAmbiguous {
				return v2OperatorOutcome(
					links, journal.Goal.Target, transactionIDPointer(journal.Transaction),
					blocker.Code, blocker.Message, blocker.Retry,
				), nil
			}
			return v2RecoveringOutcome(
				links, journal.Goal.Target, transactionIDPointer(journal.Transaction),
				blocker.Code, blocker.Message,
			), nil
		}
		if err := validateActivationObservation(id, before); err != nil {
			return v2OperatorOutcome(
				links, journal.Goal.Target, transactionIDPointer(journal.Transaction),
				CodeRecoveryAmbiguous,
				fmt.Sprintf("activation reconciler %q has an invalid observed state", id),
				"run yard update --check",
			), nil
		}
		if err := reconciler.Reconcile(ctx, links); err != nil {
			phase := "reconcile"
			var phased interface{ ActivationPhase() string }
			if errors.As(err, &phased) && phased.ActivationPhase() != "" {
				phase = phased.ActivationPhase()
			}
			return transition.reduceActivationReconcilerFailure(
				ctx, journal, reconciler, id, phase, links, before,
			), nil
		}
		actual, err := reconciler.Observe(ctx, journal.Releases, links)
		if err != nil || validateActivationObservation(id, actual) != nil ||
			!actual.Converged {
			return v2RecoveringOutcome(links, journal.Goal.Target,
				transactionIDPointer(journal.Transaction), CodeDependencyUnavailable,
				fmt.Sprintf("activation reconciler %q did not reach its fixed point during verification", id),
			), nil
		}
		if err := transition.inject(fmt.Sprintf("after-reconciler-%d", index)); err != nil {
			return Outcome{}, err
		}
	}
	links, guardOutcome, guardErr := transition.guardJournalLinks(ctx, journal)
	if guardErr != nil || guardOutcome.Code != "" {
		return guardOutcome, guardErr
	}
	id, fixed := transition.activationFixedPointStatus(ctx, journal.Releases, links)
	if !fixed {
		message := "activation reconcilers did not retain their aggregate fixed point"
		if id != "" {
			message = fmt.Sprintf(
				"activation reconciler %q did not retain its fixed point during aggregate verification",
				id,
			)
		}
		return v2RecoveringOutcome(links, journal.Goal.Target,
			transactionIDPointer(journal.Transaction), CodeDependencyUnavailable,
			message,
		), nil
	}
	return Outcome{}, nil
}

func (transition *V2Transition) reduceActivationReconcilerFailure(
	ctx context.Context,
	journal JournalRecord,
	reconciler V2ActivationReconciler,
	id string,
	phase string,
	links ReleaseLinks,
	before V2ActivationObservation,
) Outcome {
	after, err := reconciler.Observe(ctx, journal.Releases, links)
	if err != nil || validateActivationObservation(id, after) != nil {
		links, _, _ = transition.guardJournalLinks(ctx, journal)
		return v2OperatorOutcome(
			links, journal.Goal.Target, transactionIDPointer(journal.Transaction),
			CodeRecoveryAmbiguous,
			fmt.Sprintf("activation reconciler %q result cannot be observed safely", id),
			"run yard update --check",
		)
	}
	observation, err := transition.observe(ctx, journal.Goal)
	if err != nil {
		return v2OperatorOutcome(
			ReleaseLinks{}, journal.Goal.Target, transactionIDPointer(journal.Transaction),
			CodeRecoveryAmbiguous,
			"release facts cannot be observed after activation reconciliation",
			"run yard update --check",
		)
	}
	if len(observation.blockers) != 0 {
		blocker := observation.blockers[0]
		return v2OperatorOutcome(
			observation.links, journal.Goal.Target,
			transactionIDPointer(journal.Transaction), blocker.Code,
			blocker.Message, blocker.Retry,
		)
	}
	if observation.journal == nil || observation.journal.Transaction != journal.Transaction {
		return v2OperatorOutcome(
			observation.links, journal.Goal.Target,
			transactionIDPointer(journal.Transaction), CodeRecoveryAmbiguous,
			"protected transition facts changed during activation reconciliation",
			"run yard update --check",
		)
	}
	if after.Actual != before.Actual && !after.Converged {
		return v2OperatorOutcome(
			observation.links, journal.Goal.Target,
			transactionIDPointer(journal.Transaction), CodeRecoveryAmbiguous,
			fmt.Sprintf("activation reconciler %q reached an unknown state during %s", id, phase),
			"run yard update --check",
		)
	}
	return v2RecoveringOutcome(
		observation.links, journal.Goal.Target,
		transactionIDPointer(journal.Transaction), CodeDependencyUnavailable,
		fmt.Sprintf("activation reconciler %q failed during %s", id, phase),
	)
}

func activationObservationBlocker(id string, err error) Blocker {
	code := CodeDependencyUnavailable
	message := fmt.Sprintf("activation reconciler %q cannot be observed safely", id)
	var conflict interface{ ActivationConflict() bool }
	if errors.As(err, &conflict) && conflict.ActivationConflict() {
		code = CodeActivationAmbiguous
		message = fmt.Sprintf("activation reconciler %q reports ambiguous activation topology", id)
	}
	return Blocker{
		Code: code, Resource: "activation." + id, Message: message,
		Retry: "run yard update --check",
	}
}

func validateActivationObservation(id string, observation V2ActivationObservation) error {
	if err := validateSafeID(id, "activation reconciler ID"); err != nil {
		return err
	}
	if err := validateFingerprint(observation.Actual, "activation actual fingerprint"); err != nil {
		return err
	}
	if err := validateFingerprint(observation.Desired, "activation desired fingerprint"); err != nil {
		return err
	}
	if observation.Converged && observation.Actual != observation.Desired {
		return invalid("activation reconciler %q reported a false fixed point", id)
	}
	return nil
}

func (transition *V2Transition) guardJournalLinks(
	ctx context.Context,
	journal JournalRecord,
) (ReleaseLinks, Outcome, error) {
	links, err := transition.options.ObserveLinks(ctx)
	if err != nil {
		return ReleaseLinks{}, Outcome{}, err
	}
	if !journalLinksAllowed(journal, links) {
		return links, v2OperatorOutcome(
			links, journal.Goal.Target, transactionIDPointer(journal.Transaction),
			CodeActivationAmbiguous,
			"release links do not match the journaled activation checkpoint",
			"run yard update --check",
		), nil
	}
	return links, Outcome{}, nil
}

func journalLinksAllowed(journal JournalRecord, links ReleaseLinks) bool {
	initial := initialReleaseLinks(links, journal.Releases)
	activated := activatedReleaseLinks(links, journal.Releases)
	staged := journal.Releases.From != journal.Releases.Target &&
		linksEqual(links, ReleaseLinks{
			Active: journal.Releases.From, Previous: releaseIDPointer(journal.Releases.Target),
		})
	return linksAllowed(
		journal.Checkpoint, initial, staged, activated,
		journal.Releases.From == journal.Releases.Target,
	)
}

func activatedReleaseLinks(links ReleaseLinks, releases ReleasePair) bool {
	return linksEqual(links, ReleaseLinks{
		Active: releases.Target, Previous: releaseIDPointer(releases.From),
	})
}

func (transition *V2Transition) evaluateCurrent(
	ctx context.Context,
	journal JournalRecord,
) (Outcome, error) {
	links, err := transition.options.ObserveLinks(ctx)
	if err != nil {
		return Outcome{}, err
	}
	ledgerSnapshot, err := transition.store.ReadLedger()
	if err != nil {
		return Outcome{}, err
	}
	ledger, _, err := ParseLedgerV2(ledgerSnapshot.Payload, transition.registry)
	if err != nil {
		return Outcome{}, err
	}
	pending, err := transition.registry.PendingPath(ledger)
	if err != nil {
		return Outcome{}, err
	}
	intents := make([]PlannerStepIntent, len(journal.Steps))
	for index, step := range journal.Steps {
		intents[index] = PlannerStepIntent{
			ID: step.ID, Migration: step.Migration, Resource: step.Resource,
			Decision: step.Decision, Expected: step.Expected, Desired: step.Desired,
		}
	}
	return Evaluate(TransitionFacts{
		Goal: journal.Goal, Releases: journal.Releases, Links: links, Journal: &journal,
		CurrentPlan: journal.ResumePlan, CurrentIntents: intents,
		VerifiedAuthorizationPlan:  journal.AuthorizationPlan,
		CurrentArtifactDigest:      transition.options.ArtifactDigest,
		CurrentRegistryDigest:      transition.registryDigest,
		CurrentCatalogDigest:       transition.catalog.Digest(),
		CurrentAuthorizationDigest: journal.AuthorizationDigest,
		FixedPointVerified: len(pending) == 0 &&
			((journal.Releases.From == journal.Releases.Target &&
				initialReleaseLinks(links, journal.Releases)) ||
				activatedReleaseLinks(links, journal.Releases)) &&
			transition.activationFixedPoint(ctx, journal.Releases, links),
	}), nil
}

func (transition *V2Transition) activationFixedPoint(
	ctx context.Context,
	releases ReleasePair,
	links ReleaseLinks,
) bool {
	_, fixed := transition.activationFixedPointStatus(ctx, releases, links)
	return fixed
}

func (transition *V2Transition) activationFixedPointStatus(
	ctx context.Context,
	releases ReleasePair,
	links ReleaseLinks,
) (string, bool) {
	for _, reconciler := range transition.options.Reconcilers {
		if reconciler == nil {
			return "", false
		}
		id := reconciler.ID()
		if validateSafeID(id, "activation reconciler ID") != nil {
			return "", false
		}
		actual, err := reconciler.Observe(ctx, releases, links)
		if err != nil || validateActivationObservation(id, actual) != nil ||
			!actual.Converged {
			return id, false
		}
	}
	return "", true
}

func (transition *V2Transition) cachedGoal(plan PlanToken) (Goal, bool) {
	transition.cacheMu.Lock()
	defer transition.cacheMu.Unlock()
	goal, exists := transition.cache[plan]
	return goal, exists
}

func (transition *V2Transition) inject(point string) error {
	if transition.options.fault != nil {
		return transition.options.fault(point)
	}
	return nil
}

func settingsIntent(
	migration MigrationDefinitionV2,
	file settingsV2FilePlan,
) PlannerStepIntent {
	return PlannerStepIntent{
		ID: migration.ID + "." + file.Yard, Migration: migration.ID, Resource: file.Yard,
		Decision: file.Decision, Expected: file.ExpectedFingerprint, Desired: file.DesiredFingerprint,
	}
}

func evidenceFor(
	journal JournalRecord,
	step JournalStep,
	checkpoint EvidenceCheckpoint,
	observed Fingerprint,
	recovery Fingerprint,
) EvidenceRecord {
	return EvidenceRecord{
		SchemaVersion: JournalSchemaV2, Transaction: journal.Transaction,
		Releases: journal.Releases, Step: step.ID,
		Expected: step.Expected, Desired: step.Desired, Observed: observed,
		Recovery: recovery, Checkpoint: checkpoint,
	}
}

func initialReleaseLinks(links ReleaseLinks, pair ReleasePair) bool {
	return links.Active == pair.From && releaseIDsEqual(links.Previous, pair.Previous)
}

func observationLedgerAdvance(
	registry RegistryV2,
	migration MigrationDefinitionV2,
	snapshot ProtectedSnapshot,
) ([]byte, error) {
	ledger := BaselineLedgerV2(registry)
	if snapshot.Exists {
		parsed, _, err := ParseLedgerV2(snapshot.Payload, registry)
		if err != nil {
			return nil, err
		}
		ledger = parsed
	}
	advanced, err := ledger.Advance(registry, migration)
	if err != nil {
		return nil, err
	}
	payload, _, err := MarshalLedgerV2(advanced, registry)
	return payload, err
}

func (transition *V2Transition) ledgerSnapshotForFingerprint(
	fingerprint Fingerprint,
) (ProtectedSnapshot, error) {
	absent := absentProtectedSnapshot()
	if absent.Fingerprint == fingerprint {
		return absent, nil
	}
	ledger := BaselineLedgerV2(transition.registry)
	payload, _, err := MarshalLedgerV2(ledger, transition.registry)
	if err != nil {
		return ProtectedSnapshot{}, err
	}
	snapshot := protectedSnapshotFromPayload(payload)
	if snapshot.Fingerprint == fingerprint {
		return snapshot, nil
	}
	for _, migration := range transition.registry.Migrations {
		ledger, err = ledger.Advance(transition.registry, migration)
		if err != nil {
			return ProtectedSnapshot{}, err
		}
		payload, _, err = MarshalLedgerV2(ledger, transition.registry)
		if err != nil {
			return ProtectedSnapshot{}, err
		}
		snapshot = protectedSnapshotFromPayload(payload)
		if snapshot.Fingerprint == fingerprint {
			return snapshot, nil
		}
	}
	return ProtectedSnapshot{}, invalid("journaled ledger before state is not a registry prefix")
}

func protectedSnapshotFromPayload(payload []byte) ProtectedSnapshot {
	return ProtectedSnapshot{
		Exists: true, Payload: slices.Clone(payload), Fingerprint: fingerprintPayload(payload),
	}
}

func v2OperatorOutcome(
	links ReleaseLinks,
	target ReleaseID,
	transaction *TransactionID,
	code OutcomeCode,
	message string,
	retry string,
) Outcome {
	return v2PublicOutcome(
		StatusOperatorActionRequired, links, target, transaction, code, message, retry,
	)
}

func v2RecoveringOutcome(
	links ReleaseLinks,
	target ReleaseID,
	transaction *TransactionID,
	code OutcomeCode,
	message string,
) Outcome {
	return v2PublicOutcome(
		StatusRecovering, links, target, transaction, code, message, "run yard update",
	)
}

func v2PublicOutcome(
	status PublicStatus,
	links ReleaseLinks,
	target ReleaseID,
	transaction *TransactionID,
	code OutcomeCode,
	message string,
	retry string,
) Outcome {
	return Outcome{
		Status: status,
		Active: links.Active, Previous: cloneReleaseID(links.Previous), Target: target,
		Code: code, Message: message, Retry: retry, Transaction: transaction,
	}
}

func defaultV2TransactionID() TransactionID {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(fmt.Sprintf("generate release transition transaction: %v", err))
	}
	return TransactionID("tx-" + hex.EncodeToString(value[:]))
}
