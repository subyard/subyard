package releasetransition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

// RollbackPrepTransactionPrefixV1 names the active release's down transaction:
// before a rollback switches links, the active release converts profile runtime
// state to the formats the rollback target reads.
const RollbackPrepTransactionPrefixV1 = "rollback-prep-v1-"

// Fault points of the down transaction, in order.
const (
	FaultAfterRollbackHook    = "after-rollback-hook"
	FaultBeforeTargetDispatch = "before-target-dispatch"
)

// Rollback preparation states. A deferred (stopped) yard is never planned.
const (
	RollbackPrepAbsent  = "absent"
	RollbackPrepCurrent = "current"
	RollbackPrepStale   = "stale"
)

// RollbackPrepItem is one profile runtime owner in one yard, as its
// rollback-observe reports it against the format it prepares.
type RollbackPrepItem struct {
	Owner   string      `json:"owner"`
	Yard    string      `json:"yard"`
	State   string      `json:"state"`
	Actual  Fingerprint `json:"actual,omitempty"`
	Desired Fingerprint `json:"desired,omitempty"`
}

func (item RollbackPrepItem) Validate() error {
	if err := validateSafeID(item.Owner, "rollback preparation owner"); err != nil {
		return err
	}
	if !domain.SafeName(item.Yard) {
		return invalid("rollback preparation yard is not a safe name")
	}
	switch item.State {
	case RollbackPrepAbsent:
		if item.Actual != "" || item.Desired != "" {
			return invalid("absent rollback preparation carries fingerprints")
		}
	case RollbackPrepCurrent, RollbackPrepStale:
		if item.Actual != "" && !validFingerprint(item.Actual) || !validFingerprint(item.Desired) ||
			item.State == RollbackPrepCurrent && item.Actual != item.Desired {
			return invalid("rollback preparation fingerprints are invalid")
		}
	default:
		return invalid("unknown rollback preparation state %q", item.State)
	}
	return nil
}

// RollbackPrepHooks runs the owners' rollback actions in one yard.
type RollbackPrepHooks interface {
	Observe(ctx context.Context, owner, yard string) (RollbackPrepItem, error)
	Apply(ctx context.Context, item RollbackPrepItem, operation TransactionID) error
}

// RollbackPrep is the down transaction the active release runs after the
// operator confirmed a rollback and before the target owns it. Items are the
// confirmed observations. Gate proves the target accepts the prepared state;
// Dispatch hands the rollback to the target. A failure at any step leaves the
// active release active; its runtime owners then report drift for yard migrate.
type RollbackPrep struct {
	ConfigHome                     string
	Active, Target                 ReleaseID
	ArtifactDigest, RegistryDigest Fingerprint
	Items                          []RollbackPrepItem
	Hooks                          RollbackPrepHooks
	Gate, Dispatch                 func(context.Context) error
	NewTransactionID               func() TransactionID
	fault                          func(string) error
}

// RollbackPrepBinding fingerprints confirmed observations for a plan binding.
func RollbackPrepBinding(items []RollbackPrepItem) (Fingerprint, error) {
	for _, item := range items {
		if err := item.Validate(); err != nil {
			return "", err
		}
	}
	payload, err := json.Marshal(struct {
		Contract string             `json:"contract"`
		Items    []RollbackPrepItem `json:"items"`
	}{RollbackPrepTransactionPrefixV1, items})
	if err != nil {
		return "", err
	}
	return fingerprintPayload(payload), nil
}

// Run rechecks every confirmed observation, records the authorized
// transaction, converts the stale owners, passes the gate and dispatches.
// Resume is a new run: owners already converted observe as current.
func (prep RollbackPrep) Run(ctx context.Context, authorization Authorization) error {
	if prep.Hooks == nil || prep.Gate == nil || prep.Dispatch == nil || authorization == "" {
		return errors.New("rollback preparation requires hooks, a gate, a dispatcher and an authorization")
	}
	if prep.Active == prep.Target {
		return invalid("rollback preparation requires a distinct target")
	}
	for _, item := range prep.Items {
		observed, err := prep.Hooks.Observe(ctx, item.Owner, item.Yard)
		if err != nil {
			return fmt.Errorf("observe rollback preparation %s in yard %s: %w", item.Owner, item.Yard, err)
		}
		if observed != item {
			return fmt.Errorf("%w: rollback preparation of %s in yard %s changed after confirmation", domain.ErrPlanStale, item.Owner, item.Yard)
		}
	}
	transaction, err := prep.publish(authorization)
	if err != nil {
		return err
	}
	for _, item := range prep.Items {
		if item.State != RollbackPrepStale {
			continue
		}
		if err := prep.Hooks.Apply(ctx, item, transaction); err != nil {
			return fmt.Errorf("prepare %s in yard %s for rollback: %w", item.Owner, item.Yard, err)
		}
		if err := prep.inject(FaultAfterRollbackHook); err != nil {
			return err
		}
	}
	if err := prep.Gate(ctx); err != nil {
		return err
	}
	if err := prep.inject(FaultBeforeTargetDispatch); err != nil {
		return err
	}
	return prep.Dispatch(ctx)
}

// publish records the authorized down transaction as the active release's
// completed same-release journal before the first hook. It has no steps:
// every hook is an idempotent observe/apply pair, so an interrupted run
// resumes by observing again. Older releases and yard migrate read it as
// ordinary completed history of the active release.
func (prep RollbackPrep) publish(authorization Authorization) (TransactionID, error) {
	newID := prep.NewTransactionID
	if newID == nil {
		newID = func() TransactionID {
			return TransactionID(RollbackPrepTransactionPrefixV1 + strings.TrimPrefix(string(defaultV2TransactionID()), "tx-"))
		}
	}
	transaction := newID()
	if !strings.HasPrefix(string(transaction), RollbackPrepTransactionPrefixV1) {
		return "", invalid("rollback preparation transaction has no %s prefix", RollbackPrepTransactionPrefixV1)
	}
	record, err := prep.journal(transaction, authorization)
	if err != nil {
		return "", err
	}
	payload, err := MarshalJournal(record)
	if err != nil {
		return "", err
	}
	store, err := NewPOSIXV2Store(prep.ConfigHome)
	if err != nil {
		return "", err
	}
	unlock, err := store.Lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	current, err := store.ReadCurrentJournal()
	if err != nil {
		return "", err
	}
	if current.Exists {
		journal, err := ParseJournal(current.Payload)
		if err != nil {
			return "", err
		}
		if journal.Checkpoint != JournalComplete {
			return "", fmt.Errorf("%w: an unfinished release transition must be recovered before rollback preparation", domain.ErrPlanStale)
		}
	}
	if err := store.CompareAndSwapCurrentJournal(current, payload); err != nil {
		return "", err
	}
	return transaction, nil
}

func (prep RollbackPrep) journal(transaction TransactionID, authorization Authorization) (JournalRecord, error) {
	scope, err := RollbackPrepBinding(prep.Items)
	if err != nil {
		return JournalRecord{}, err
	}
	target := prep.Target
	goal := Goal{Target: prep.Active, Direction: DirectionActivateTarget}
	releases := ReleasePair{From: prep.Active, Previous: &target, Target: prep.Active}
	policy, err := newV2ActionPolicy()
	if err != nil {
		return JournalRecord{}, err
	}
	consequences := []string{}
	decisions := []RedactedDecision{}
	for _, item := range prep.Items {
		decision := RedactedDecision{Resource: "rollback-prep." + item.Owner, Scope: "yard." + item.Yard, Decision: DecisionPreserve, Result: item.State}
		if item.State == RollbackPrepStale {
			decision.Decision = DecisionTransform
			consequences = append(consequences, fmt.Sprintf("prepare %s in yard %s for release %s", item.Owner, item.Yard, prep.Target))
		}
		decisions = append(decisions, decision)
	}
	assessment, err := assessV2Action(policy, true, false, consequences)
	if err != nil {
		return JournalRecord{}, err
	}
	planPayload, err := json.Marshal(struct {
		Contract string      `json:"contract"`
		Releases ReleasePair `json:"releases"`
		Artifact Fingerprint `json:"artifact"`
		Registry Fingerprint `json:"registry"`
		Scope    Fingerprint `json:"scope"`
	}{RollbackPrepTransactionPrefixV1, releases, prep.ArtifactDigest, prep.RegistryDigest, scope})
	if err != nil {
		return JournalRecord{}, err
	}
	catalog := BuiltinCapabilityCatalog().Digest()
	plan := PlanToken("plan-v1-" + string(fingerprintPayload(planPayload)))
	resume, err := BindResumePlan(ResumePlanFacts{
		Goal: goal, Releases: releases, ArtifactDigest: prep.ArtifactDigest,
		RegistryDigest: prep.RegistryDigest, CatalogDigest: catalog, ObservationScope: scope,
		Assessment: assessment, Decisions: slices.Clone(decisions), Intents: []PlannerStepIntent{},
		Blockers: []Blocker{}, Transaction: transaction, AuthorizationPlan: plan,
	})
	if err != nil {
		return JournalRecord{}, err
	}
	record := JournalRecord{
		SchemaVersion: JournalSchemaV2, Transaction: transaction, Goal: goal, Releases: releases,
		AuthorizationPlan: plan, ResumePlan: resume,
		ArtifactDigest: prep.ArtifactDigest, RegistryDigest: prep.RegistryDigest, CatalogDigest: catalog,
		ObservationScope: scope, AuthorizationDigest: fingerprintPayload([]byte(authorization)),
		Checkpoint: JournalComplete, Steps: []JournalStep{},
	}
	record.IntentDigest = bindJournalIntent(record.AuthorizationPlan, record.ResumePlan, record.ObservationScope, record.Steps)
	return record, record.Validate()
}

func (prep RollbackPrep) inject(point string) error {
	if prep.fault == nil {
		return nil
	}
	return prep.fault(point)
}
