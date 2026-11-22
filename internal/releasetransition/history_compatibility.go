package releasetransition

import (
	"encoding/json"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

// HistoryCompatibilityKind is how a target release's migration registry relates
// to the registry that owns the active migration history.
type HistoryCompatibilityKind string

const (
	// HistoryIdentity: the target registry is the active registry.
	HistoryIdentity HistoryCompatibilityKind = "identity"
	// HistoryEquivalent: one registry is the other with completed prefixes
	// retired, so the target reads the history unchanged and nothing is
	// written. Admitted only within a declared release pair: rollback to the
	// active release's declared predecessor, or the return to a successor that
	// declares the active release as its predecessor.
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
	HistoryRegistryUnreadable           HistoryRefusalReason = "registry-unreadable"
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

// HistoryCompatibilityInput carries sealed facts only. A nil registry predates
// registry v2. Registries are compared by canonical digest.
type HistoryCompatibilityInput struct {
	Active, Target *RegistryV2
	// Predecessor is the canonical registry digest the active release declares
	// for rollback; empty when it declares none. TargetPredecessor is the one
	// the target release declares.
	Predecessor, TargetPredecessor Fingerprint
	// TargetRegistryFeatures are manifest-bound reader flags; absent means v0.30.
	TargetRegistryFeatures []string
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
	// Successor reports an Equivalent target that retires prefixes of the
	// active registry, rather than the reverse.
	Successor bool
	// DeclaredPredecessor reports that the target registry is the active
	// release's declared predecessor; it admits an Equivalent target that the
	// active registry retires. DeclaredSuccessor reports that the target
	// declares the active registry as its predecessor; it admits an Equivalent
	// Successor.
	DeclaredPredecessor, DeclaredSuccessor bool
	Refusals                               []HistoryRefusal
}

// HistoryCompatibility is the single decision whether a target release may own
// the active migration history: registry relation first, then, for an admitted
// relation, the authoritative ledger or checkpoint read through the target.
// History findings depend on the relation and on each preceding read, so a
// failure skips the findings after it.
func HistoryCompatibility(input HistoryCompatibilityInput) HistoryCompatibilityVerdict {
	var verdict HistoryCompatibilityVerdict
	switch {
	case input.Active == nil && input.Target == nil:
		// A registryless pair has no protected migration history to read.
		verdict.Kind, verdict.Registry = HistoryIdentity, RegistryAbsent
		return verdict.sealed()
	case input.Active == nil || input.Target == nil:
		verdict.Kind, verdict.Registry = HistoryRefuse, RegistryDiffers
		return verdict.sealed()
	}
	path, err := PlanDownPath(*input.Active, *input.Target, input.TargetRegistryFeatures...)
	if err != nil {
		verdict.Kind, verdict.Registry = HistoryRefuse, RegistryDiffers
		verdict.Refusals = []HistoryRefusal{{HistoryRegistryDiffers, "the migration registries cannot be identified", err}}
		return verdict
	}
	verdict.Kind, verdict.Registry, verdict.Successor = path.Kind, RegistryDiffers, path.Successor
	if path.Active == path.Target {
		verdict.Registry = RegistryIdentical
	}
	verdict.DeclaredPredecessor = input.Predecessor != "" && path.Target == input.Predecessor
	verdict.DeclaredSuccessor = input.TargetPredecessor != "" && path.Active == input.TargetPredecessor
	if path.Unreadable != nil {
		// An unreadable pair can also be undeclared; report both.
		verdict.Refusals = []HistoryRefusal{{
			HistoryRegistryUnreadable, "the selected release cannot parse the active release's migration registry", path.Unreadable,
		}}
		if verdict.Registry != RegistryIdentical && !verdict.DeclaredPredecessor && !verdict.DeclaredSuccessor {
			verdict.Refusals = append(verdict.Refusals, registryDiffersRefusal)
		}
		return verdict
	}
	if verdict.RegistryAdmitted() && input.History != nil {
		if refusal := verifyHistoryEvidence(*input.History); refusal != nil {
			verdict.Refusals = []HistoryRefusal{*refusal}
		}
	}
	return verdict.sealed()
}

// DownPath relates the active registry to a target registry by canonical
// digest. Active and Target are their canonical digests. Successor marks an
// Equivalent target that retires prefixes of the active registry. Unreadable
// explains a Refuse for a registry pair that would otherwise be Equivalent.
type DownPath struct {
	Kind           HistoryCompatibilityKind
	Active, Target Fingerprint
	Successor      bool
	Unreadable     error
}

// PlanDownPath decides how the active migration history reaches a target
// registry. Identity is one registry. Equivalent is one registry retired
// through the other's retired floors: the unretired registry proves every
// retired digest with its own MigrationPrefixDigest and chains the same later
// definitions, so either checkpoint reader accepts the history unchanged.
// Every other relation refuses.
//
// In either direction, the target must parse the active registry before it
// can serve a request. Use the parser itself with the target's sealed flags.
func PlanDownPath(active, target RegistryV2, targetFeatures ...string) (DownPath, error) {
	var path DownPath
	var err error
	if path.Active, err = active.CanonicalDigest(); err != nil {
		return DownPath{}, err
	}
	if path.Target, err = target.CanonicalDigest(); err != nil {
		return DownPath{}, err
	}
	path.Kind = HistoryRefuse
	if path.Target == path.Active {
		path.Kind = HistoryIdentity
	} else if equivalent, err := retiresPrefixesOf(active, target, path.Active); err != nil {
		return path, err
	} else if equivalent {
		path.Kind = HistoryEquivalent
	} else if equivalent, err := retiresPrefixesOf(target, active, path.Target); err != nil {
		return path, err
	} else if equivalent {
		path.Kind, path.Successor = HistoryEquivalent, true
	}
	if path.Kind != HistoryRefuse {
		payload, err := json.Marshal(active)
		if err != nil {
			return path, err
		}
		_, _, path.Unreadable = ParseRegistryV2WithFeatures(payload, BuiltinCapabilityCatalog(), targetFeatures)
		if path.Unreadable != nil {
			path.Kind = HistoryRefuse
		}
	}
	return path, nil
}

// retiresPrefixesOf reports whether retired, with canonical digest digest, is
// base retired through retired's floors.
func retiresPrefixesOf(retired, base RegistryV2, digest Fingerprint) (bool, error) {
	if len(retired.RetiredPrefixes) == 0 {
		return false, nil
	}
	floors := make(map[string]int, len(retired.RetiredPrefixes))
	for domain, prefix := range retired.RetiredPrefixes {
		floors[domain] = prefix.ThroughEpoch
	}
	candidate, err := base.RetirePrefixes(floors)
	if err != nil {
		// The base registry cannot reach these floors.
		return false, nil
	}
	candidateDigest, err := candidate.CanonicalDigest()
	return err == nil && candidateDigest == digest, err
}

// registryDiffersRefusal refuses a registry relation that is not admitted.
var registryDiffersRefusal = HistoryRefusal{
	Reason:  HistoryRegistryDiffers,
	Message: "the selected release's migration registry differs from the active release's registry",
}

// sealed keeps the invariant that a verdict which is not admitted carries a
// refusal. A relation that is not admitted keeps the identity refusal.
func (verdict HistoryCompatibilityVerdict) sealed() HistoryCompatibilityVerdict {
	if !verdict.RegistryAdmitted() && len(verdict.Refusals) == 0 {
		verdict.Refusals = []HistoryRefusal{registryDiffersRefusal}
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

// RegistryAdmitted reports whether the registry relation lets the target own
// the history, before any history finding: identity, or an Equivalent target
// within a declared release pair. Steps are not admitted yet.
func (verdict HistoryCompatibilityVerdict) RegistryAdmitted() bool {
	if verdict.Kind == HistoryEquivalent && verdict.Successor {
		return verdict.DeclaredSuccessor
	}
	return verdict.Kind == HistoryIdentity || verdict.Kind == HistoryEquivalent && verdict.DeclaredPredecessor
}

// Admitted reports whether the target may own the history.
func (verdict HistoryCompatibilityVerdict) Admitted() bool {
	return verdict.RegistryAdmitted() && len(verdict.Refusals) == 0
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
