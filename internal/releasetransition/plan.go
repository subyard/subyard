package releasetransition

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/blang/semver/v4"
)

const MaxPlanItems = 256

type ResourceObservation struct {
	Resource    string      `json:"resource"`
	Class       string      `json:"class"`
	Fingerprint Fingerprint `json:"fingerprint"`
}

type PlannerStepIntent struct {
	ID        string      `json:"id"`
	Migration string      `json:"migration"`
	Resource  string      `json:"resource"`
	Decision  Decision    `json:"decision"`
	Expected  Fingerprint `json:"expectedFingerprint"`
	Desired   Fingerprint `json:"desiredFingerprint"`
}

type PlanFacts struct {
	Goal             Goal                    `json:"goal"`
	Releases         ReleasePair             `json:"releases"`
	Links            ReleaseLinks            `json:"links"`
	ArtifactDigest   Fingerprint             `json:"artifactDigest"`
	RegistryDigest   Fingerprint             `json:"registryDigest"`
	CatalogDigest    Fingerprint             `json:"catalogDigest"`
	ObservationScope Fingerprint             `json:"observationScope"`
	Assessment       domain.ActionAssessment `json:"assessment"`
	Decisions        []RedactedDecision      `json:"decisions"`
	Observations     []ResourceObservation   `json:"observations"`
	Intents          []PlannerStepIntent     `json:"intents"`
	Blockers         []Blocker               `json:"blockers"`
	Replacement      *JournalReplacement     `json:"replacement,omitempty"`
}

type JournalReplacementReason string

const (
	JournalReplacementPreActivationPlanStale   JournalReplacementReason = "pre-activation-plan-stale"
	JournalReplacementPostActivationScopeV0111 JournalReplacementReason = "post-activation-scope-v0.11.1"
)

// JournalReplacement binds a newly assessed plan to the exact unfinished
// journal it supersedes. The journal itself remains immutable evidence; only
// the protected current-journal pointer is replaced by CAS.
type JournalReplacement struct {
	Transaction   TransactionID            `json:"transaction"`
	Fingerprint   Fingerprint              `json:"fingerprint"`
	Reason        JournalReplacementReason `json:"reason"`
	SourceVersion string                   `json:"sourceVersion,omitempty"`
}

// ResumePlanFacts bind only immutable recovery facts. Current release links and
// resource observations deliberately remain outside this token because both can
// advance while an authorized transition is being recovered.
type ResumePlanFacts struct {
	Goal              Goal                    `json:"goal"`
	Releases          ReleasePair             `json:"releases"`
	ArtifactDigest    Fingerprint             `json:"artifactDigest"`
	RegistryDigest    Fingerprint             `json:"registryDigest"`
	CatalogDigest     Fingerprint             `json:"catalogDigest"`
	ObservationScope  Fingerprint             `json:"observationScope"`
	Assessment        domain.ActionAssessment `json:"assessment"`
	Decisions         []RedactedDecision      `json:"decisions"`
	Intents           []PlannerStepIntent     `json:"intents"`
	Blockers          []Blocker               `json:"blockers"`
	Transaction       TransactionID           `json:"transaction"`
	AuthorizationPlan PlanToken               `json:"authorizationPlan"`
}

func validateAssessmentShape(assessment domain.ActionAssessment) error {
	const maxAssessmentConsequences = 64
	if err := validateSafeID(string(assessment.Action), "action assessment ID"); err != nil {
		return err
	}
	if !slices.IsSorted(assessment.Impacts) {
		return invalid("action assessment impacts are not canonical")
	}
	for index := 1; index < len(assessment.Impacts); index++ {
		if assessment.Impacts[index] == assessment.Impacts[index-1] {
			return invalid("action assessment impacts are not canonical")
		}
	}
	if len(assessment.Consequences) > maxAssessmentConsequences {
		return invalid("action assessment has too many consequences")
	}
	for _, consequence := range assessment.Consequences {
		if err := validateText(
			consequence, "action assessment consequence", maxDiagnosticText, true,
		); err != nil {
			return err
		}
	}
	return nil
}

func BindPlan(facts PlanFacts) (PlanToken, error) {
	if err := facts.Validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(facts)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return PlanToken("plan-v1-" + hex.EncodeToString(digest[:])), nil
}

func BindResumePlan(facts ResumePlanFacts) (PlanToken, error) {
	if err := facts.Validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(facts)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return PlanToken("resume-v1-" + hex.EncodeToString(digest[:])), nil
}

func (facts PlanFacts) Validate() error {
	if err := validateImmutablePlanFacts(
		facts.Goal, facts.Releases, facts.ArtifactDigest, facts.RegistryDigest,
		facts.CatalogDigest, facts.Assessment, facts.Decisions, facts.Intents, facts.Blockers,
	); err != nil {
		return err
	}
	if err := validateFingerprint(facts.ObservationScope, "observation scope"); err != nil {
		return err
	}
	if err := facts.Links.Validate(); err != nil {
		return err
	}
	if len(facts.Observations) > MaxPlanItems {
		return invalid("inspection contains too many plan items")
	}
	for _, observation := range facts.Observations {
		if err := observation.Validate(); err != nil {
			return err
		}
	}
	if facts.Replacement != nil {
		if err := facts.Replacement.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (replacement JournalReplacement) Validate() error {
	if err := validateTransactionID(replacement.Transaction); err != nil {
		return err
	}
	if err := validateFingerprint(
		replacement.Fingerprint, "replacement journal fingerprint",
	); err != nil {
		return err
	}
	switch replacement.Reason {
	case JournalReplacementPreActivationPlanStale:
		if replacement.SourceVersion != "" {
			return invalid("pre-activation journal replacement has a source version")
		}
	case JournalReplacementPostActivationScopeV0111:
		version, err := semver.Parse(replacement.SourceVersion)
		if err != nil || version.String() != replacement.SourceVersion {
			return invalid("post-activation journal replacement has an invalid source version")
		}
	default:
		return invalid("unknown journal replacement reason %q", replacement.Reason)
	}
	return nil
}

func cloneJournalReplacement(replacement *JournalReplacement) *JournalReplacement {
	if replacement == nil {
		return nil
	}
	clone := *replacement
	return &clone
}

func (facts ResumePlanFacts) Validate() error {
	if err := validateImmutablePlanFacts(
		facts.Goal, facts.Releases, facts.ArtifactDigest, facts.RegistryDigest,
		facts.CatalogDigest, facts.Assessment, facts.Decisions, facts.Intents, facts.Blockers,
	); err != nil {
		return err
	}
	if err := validateFingerprint(facts.ObservationScope, "observation scope"); err != nil {
		return err
	}
	if err := validateTransactionID(facts.Transaction); err != nil {
		return err
	}
	return validatePlanToken(facts.AuthorizationPlan)
}

func validateImmutablePlanFacts(
	goal Goal,
	releases ReleasePair,
	artifactDigest, registryDigest, catalogDigest Fingerprint,
	assessment domain.ActionAssessment,
	decisions []RedactedDecision,
	intents []PlannerStepIntent,
	blockers []Blocker,
) error {
	if err := goal.Validate(); err != nil {
		return err
	}
	if err := releases.Validate(); err != nil {
		return err
	}
	if goal.Target != releases.Target {
		return invalid("goal target does not match the exact release pair")
	}
	for field, value := range map[string]Fingerprint{
		"artifact digest": artifactDigest,
		"registry digest": registryDigest,
		"catalog digest":  catalogDigest,
	} {
		if err := validateFingerprint(value, field); err != nil {
			return err
		}
	}
	if len(decisions) > MaxPlanItems || len(intents) > MaxPlanItems || len(blockers) > MaxPlanItems {
		return invalid("inspection contains too many plan items")
	}
	if err := validateAssessmentShape(assessment); err != nil {
		return err
	}
	for _, decision := range decisions {
		if err := decision.Validate(); err != nil {
			return err
		}
	}
	if err := validatePlannerIntents(intents); err != nil {
		return err
	}
	for _, blocker := range blockers {
		if err := blocker.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func validatePlannerIntents(intents []PlannerStepIntent) error {
	seen := make(map[string]struct{}, len(intents))
	for _, intent := range intents {
		if err := validateSafeID(intent.ID, "planner step intent ID"); err != nil {
			return err
		}
		if _, duplicate := seen[intent.ID]; duplicate {
			return invalid("duplicate planner step intent %q", intent.ID)
		}
		seen[intent.ID] = struct{}{}
		if err := validateSafeID(intent.Migration, "planner migration ID"); err != nil {
			return err
		}
		if err := validateSafeID(intent.Resource, "planner resource ID"); err != nil {
			return err
		}
		if !validDecision(intent.Decision) || intent.Decision == DecisionBlock {
			return invalid("planner step intent %q has invalid decision %q", intent.ID, intent.Decision)
		}
		if err := validateFingerprint(intent.Expected, "planner expected fingerprint"); err != nil {
			return err
		}
		if err := validateFingerprint(intent.Desired, "planner desired fingerprint"); err != nil {
			return err
		}
	}
	return nil
}

func (decision RedactedDecision) Validate() error {
	if err := validateSafeID(decision.Resource, "decision resource"); err != nil {
		return err
	}
	if decision.Scope != "" {
		if err := validateSafeID(decision.Scope, "decision scope"); err != nil {
			return err
		}
	}
	if !validDecision(decision.Decision) {
		return invalid("unknown decision %q", decision.Decision)
	}
	return validateText(decision.Result, "redacted decision result", maxRedactedText, false)
}

func (observation ResourceObservation) Validate() error {
	if err := validateSafeID(observation.Resource, "observed resource"); err != nil {
		return err
	}
	if err := validateSafeID(observation.Class, "observation class"); err != nil {
		return err
	}
	return validateFingerprint(observation.Fingerprint, "observation fingerprint")
}

func (blocker Blocker) Validate() error {
	if !validOutcomeCode(blocker.Code) || blocker.Code == CodeReady {
		return invalid("unknown blocker code %q", blocker.Code)
	}
	if blocker.Resource != "" {
		if err := validateSafeID(blocker.Resource, "blocked resource"); err != nil {
			return err
		}
	}
	if err := validateText(blocker.Message, "blocker message", maxDiagnosticText, true); err != nil {
		return err
	}
	return validateSafeAction(blocker.Retry)
}

func cloneTransactionID(value *TransactionID) *TransactionID {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
