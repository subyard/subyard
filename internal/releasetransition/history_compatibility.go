package releasetransition

import (
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

// HistoryCompatibilityKind is how a target release's migration registry relates
// to the registry that owns the active migration history.
type HistoryCompatibilityKind string

const (
	// HistoryIdentity: the target registry is the active registry.
	HistoryIdentity HistoryCompatibilityKind = "identity"
	// HistoryEquivalent: the registries differ only in history the target reads
	// unchanged. Not produced or admitted yet.
	HistoryEquivalent HistoryCompatibilityKind = "equivalent"
	// HistorySteps: the active release declares down steps to the target
	// registry. Not produced or admitted yet.
	HistorySteps HistoryCompatibilityKind = "steps"
	// HistoryRefuse: no supported relation exists.
	HistoryRefuse HistoryCompatibilityKind = "refuse"
)

// HistoryRegistryRelation compares registry identities only, independently of
// whether the target is admitted.
type HistoryRegistryRelation string

const (
	RegistryIdentical HistoryRegistryRelation = "identical"
	RegistryDiffers   HistoryRegistryRelation = "differs"
	// RegistryAbsent: neither release has a v2 registry.
	RegistryAbsent HistoryRegistryRelation = "absent"
)

// HistoryRefusalReason is a stable machine-readable refusal class.
type HistoryRefusalReason string

const (
	HistoryRegistryDiffers              HistoryRefusalReason = "registry-differs"
	HistoryUnverifiable                 HistoryRefusalReason = "history-unverifiable"
	HistoryMissing                      HistoryRefusalReason = "history-missing"
	HistoryCheckpointReaderUnverifiable HistoryRefusalReason = "checkpoint-reader-unverifiable"
	HistoryProjectionUnreadable         HistoryRefusalReason = "projection-unreadable"
	HistoryProjectionDiffers            HistoryRefusalReason = "projection-differs"
)

type HistoryRefusal struct {
	Reason  HistoryRefusalReason
	Message string
	Cause   error
}

// HistoryCompatibilityInput carries sealed facts only. Both registry identities
// must be of one kind: canonical digests at the runtime boundary, sealed file
// digests inside the engine. An empty identity predates registry v2.
type HistoryCompatibilityInput struct {
	ActiveRegistry Fingerprint
	TargetRegistry Fingerprint
	// Predecessor is the canonical registry digest the active release declares
	// for rollback; empty when it declares none.
	Predecessor Fingerprint
	// History, when present, must be readable by the target registry.
	History *HistoryEvidence
}

// HistoryEvidence is the authoritative active history and the target's sealed
// reader facts. A reader marker error matters only for checkpointed history.
type HistoryEvidence struct {
	Registry            RegistryV2
	RegistryDigest      Fingerprint
	Ledger, Checkpoint  ProtectedSnapshot
	CheckpointReader    bool
	CheckpointReaderErr error
}

// A verdict that is not admitted always carries at least one refusal.
type HistoryCompatibilityVerdict struct {
	Kind     HistoryCompatibilityKind
	Registry HistoryRegistryRelation
	// DeclaredPredecessor reports that the target registry is the active
	// release's declared predecessor. It does not admit the target yet.
	DeclaredPredecessor bool
	Refusals            []HistoryRefusal
}

// HistoryCompatibility is the single decision whether a target release may own
// the active migration history: registry relation first, then, for an admitted
// relation, the authoritative ledger or checkpoint read through the target.
// History findings depend on the relation and on each preceding read, so a
// failure skips the findings after it.
func HistoryCompatibility(input HistoryCompatibilityInput) HistoryCompatibilityVerdict {
	verdict := HistoryCompatibilityVerdict{
		DeclaredPredecessor: input.Predecessor != "" && input.TargetRegistry == input.Predecessor,
	}
	switch {
	case input.ActiveRegistry != input.TargetRegistry:
		verdict.Kind, verdict.Registry = HistoryRefuse, RegistryDiffers
		return verdict.sealed()
	case input.TargetRegistry == "":
		// A registryless pair has no protected migration history to read.
		verdict.Kind, verdict.Registry = HistoryIdentity, RegistryAbsent
		return verdict.sealed()
	}
	verdict.Kind, verdict.Registry = HistoryIdentity, RegistryIdentical
	if input.History != nil {
		if refusal := verifyHistoryEvidence(*input.History); refusal != nil {
			verdict.Refusals = []HistoryRefusal{*refusal}
		}
	}
	return verdict.sealed()
}

// sealed keeps the invariant that a verdict which is not admitted carries a
// refusal. A relation this slice does not admit keeps the identity refusal.
func (verdict HistoryCompatibilityVerdict) sealed() HistoryCompatibilityVerdict {
	if verdict.Kind != HistoryIdentity && len(verdict.Refusals) == 0 {
		verdict.Refusals = []HistoryRefusal{{
			Reason:  HistoryRegistryDiffers,
			Message: "the selected release's migration registry differs from the active release's registry",
		}}
	}
	return verdict
}

func verifyHistoryEvidence(history HistoryEvidence) *HistoryRefusal {
	state, err := ParseMigrationLedger(history.Ledger, history.Checkpoint, history.Registry, history.RegistryDigest)
	if err != nil {
		return &HistoryRefusal{HistoryUnverifiable, "the selected release cannot verify the authoritative active migration history", err}
	}
	if !state.AuthoritySnapshot.Exists {
		return &HistoryRefusal{HistoryMissing, "the authoritative active migration history is missing", nil}
	}
	if !state.Checkpointed {
		return nil
	}
	if history.CheckpointReaderErr != nil {
		return &HistoryRefusal{HistoryCheckpointReaderUnverifiable, "the selected release's migration checkpoint capability cannot be verified", history.CheckpointReaderErr}
	}
	if history.CheckpointReader {
		return nil
	}
	if _, _, err := ParseLedgerV2(history.Ledger.Payload, history.Registry); err != nil {
		return &HistoryRefusal{HistoryProjectionUnreadable, "the retained release cannot read the legacy migration ledger projection", err}
	}
	if state.DiffersFromProjection(history.Registry) {
		return &HistoryRefusal{HistoryProjectionDiffers, "the retained release requires a legacy ledger that matches the authoritative migration checkpoint", nil}
	}
	return nil
}

// Admitted reports whether this slice lets the target own the history. Only
// identity is admitted; Equivalent and Steps keep the identity refusal.
func (verdict HistoryCompatibilityVerdict) Admitted() bool {
	return verdict.Kind == HistoryIdentity && len(verdict.Refusals) == 0
}

// Refusal joins the refusal messages in their stable order and collects causes.
func (verdict HistoryCompatibilityVerdict) Refusal() (string, error) {
	messages := make([]string, 0, len(verdict.Refusals))
	causes := make([]error, 0, len(verdict.Refusals))
	for _, refusal := range verdict.Refusals {
		messages = append(messages, refusal.Message)
		causes = append(causes, refusal.Cause)
	}
	return strings.Join(messages, "; "), domain.CollectRefusals(causes...)
}
