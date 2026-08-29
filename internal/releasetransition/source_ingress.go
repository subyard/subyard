package releasetransition

import (
	"context"
	"slices"
	"sort"

	"github.com/Subyard/Subyard/internal/config"
)

const (
	legacyV1ImportMigration = "legacy-v1-import"
	sourceInstallMigration  = "source-install-v1"
)

type V2IngressOperationKind string

const (
	V2LegacyV1Import    V2IngressOperationKind = "legacy-v1-import"
	V2SourceImport      V2IngressOperationKind = "import"
	V2SourceEntrypoints V2IngressOperationKind = "entrypoints"
)

// V2SettingsSnapshotView is the read-only settings state used during planning.
// Source ingress may overlay prospective imported files without writing them.
type V2SettingsSnapshotView interface {
	ListYards() ([]string, error)
	ReadSnapshot(path string) (config.PersistentFileSnapshot, error)
}

// V2Ingress is the closed compatibility leaf below the release transition's
// authorization and journal. It never receives an authorization.
type V2Ingress interface {
	Inspect(context.Context, *V2IngressBinding) (V2IngressInspection, error)
	Observe(context.Context, V2IngressStep) (Fingerprint, error)
	Apply(context.Context, V2IngressStep) error
	Verify(context.Context, V2IngressStep) error
}

type V2IngressBinding struct {
	Transaction TransactionID
	Plan        PlanToken
	Releases    ReleasePair
	Steps       []V2IngressStepBinding
}

type V2IngressStepBinding struct {
	Kind       V2IngressOperationKind
	Checkpoint StepCheckpoint
	Expected   Fingerprint
	Desired    Fingerprint
	Static     Fingerprint
}

type V2IngressStep struct {
	Binding  V2IngressBinding
	Kind     V2IngressOperationKind
	Expected Fingerprint
	Desired  Fingerprint
	Static   Fingerprint
}

type V2IngressOperation struct {
	Kind     V2IngressOperationKind
	Decision Decision
	Expected Fingerprint
	Desired  Fingerprint
	Static   Fingerprint
}

type V2IngressInspection struct {
	Operations  []V2IngressOperation
	Decisions   []RedactedDecision
	Blockers    []Blocker
	Prospective V2SettingsSnapshotView
}

func normalizeV2IngressInspection(value V2IngressInspection) (V2IngressInspection, error) {
	if len(value.Operations) > 3 || len(value.Decisions) > MaxPlanItems ||
		len(value.Blockers) > MaxPlanItems {
		return V2IngressInspection{}, invalid("compatibility ingress inspection exceeds its item bound")
	}
	value.Operations = slices.Clone(value.Operations)
	value.Decisions = slices.Clone(value.Decisions)
	value.Blockers = slices.Clone(value.Blockers)
	previousRank := 0
	for _, operation := range value.Operations {
		rank := ingressOperationRank(operation.Kind)
		if rank == 0 || rank <= previousRank {
			return V2IngressInspection{}, invalid("compatibility ingress operations are not canonical")
		}
		previousRank = rank
		if operation.Decision != DecisionTransform &&
			operation.Decision != DecisionCanonicalize && operation.Decision != DecisionReset &&
			operation.Decision != DecisionQuarantine {
			return V2IngressInspection{}, invalid("compatibility ingress operation has an unsupported decision")
		}
		if err := validateFingerprint(operation.Expected, "ingress expected fingerprint"); err != nil {
			return V2IngressInspection{}, err
		}
		if err := validateFingerprint(operation.Desired, "ingress desired fingerprint"); err != nil {
			return V2IngressInspection{}, err
		}
		if operation.Expected == operation.Desired &&
			(operation.Kind != V2LegacyV1Import || operation.Decision != DecisionCanonicalize) {
			return V2IngressInspection{}, invalid("compatibility ingress operation has no state change")
		}
		if operation.Kind == V2LegacyV1Import {
			if err := validateFingerprint(operation.Static, "legacy ingress static fingerprint"); err != nil {
				return V2IngressInspection{}, err
			}
		} else if operation.Static != "" {
			return V2IngressInspection{}, invalid("source ingress operation has an unexpected static binding")
		}
		if operation.Kind == V2SourceImport && value.Prospective == nil {
			return V2IngressInspection{}, invalid("source import has no prospective settings view")
		}
	}
	for _, decision := range value.Decisions {
		if err := decision.Validate(); err != nil {
			return V2IngressInspection{}, err
		}
	}
	sort.Slice(value.Decisions, func(left, right int) bool {
		leftValue, rightValue := value.Decisions[left], value.Decisions[right]
		if leftValue.Scope != rightValue.Scope {
			return leftValue.Scope < rightValue.Scope
		}
		if leftValue.Resource != rightValue.Resource {
			return leftValue.Resource < rightValue.Resource
		}
		if leftValue.Decision != rightValue.Decision {
			return leftValue.Decision < rightValue.Decision
		}
		return leftValue.Result < rightValue.Result
	})
	for _, blocker := range value.Blockers {
		if err := blocker.Validate(); err != nil {
			return V2IngressInspection{}, err
		}
	}
	sort.Slice(value.Blockers, func(left, right int) bool {
		leftValue, rightValue := value.Blockers[left], value.Blockers[right]
		if leftValue.Code != rightValue.Code {
			return leftValue.Code < rightValue.Code
		}
		if leftValue.Resource != rightValue.Resource {
			return leftValue.Resource < rightValue.Resource
		}
		if leftValue.Message != rightValue.Message {
			return leftValue.Message < rightValue.Message
		}
		return leftValue.Retry < rightValue.Retry
	})
	return value, nil
}

func ingressOperationRank(kind V2IngressOperationKind) int {
	switch kind {
	case V2LegacyV1Import:
		return 1
	case V2SourceImport:
		return 2
	case V2SourceEntrypoints:
		return 3
	default:
		return 0
	}
}

func ingressOperationIDs(kind V2IngressOperationKind) (migration, step, resource, scope, class string) {
	switch kind {
	case V2LegacyV1Import:
		return legacyV1ImportMigration, "legacy-v1.import", "legacy-v1.journal", "legacy-v1", "legacy-v1-journal"
	case V2SourceImport:
		return sourceInstallMigration, "source-install.import", "source-install.config", "source-install", "source-install-v1"
	case V2SourceEntrypoints:
		return sourceInstallMigration, "source-install.entrypoints", "source-install.entrypoints", "source-install", "source-install-v1"
	default:
		return "", "", "", "", ""
	}
}

func ingressIntent(operation V2IngressOperation) PlannerStepIntent {
	migration, step, resource, _, _ := ingressOperationIDs(operation.Kind)
	return PlannerStepIntent{
		ID: step, Migration: migration, Resource: resource,
		Decision: operation.Decision, Expected: operation.Expected, Desired: operation.Desired,
	}
}

func ingressOperationKindForIntent(intent PlannerStepIntent) (V2IngressOperationKind, bool) {
	for _, kind := range []V2IngressOperationKind{
		V2LegacyV1Import, V2SourceImport, V2SourceEntrypoints,
	} {
		migration, step, resource, _, _ := ingressOperationIDs(kind)
		if intent.Migration == migration && intent.ID == step && intent.Resource == resource {
			return kind, true
		}
	}
	return "", false
}

func ingressStep(
	binding V2IngressBinding,
	operation V2IngressOperation,
) (V2IngressStep, error) {
	if err := validateTransactionID(binding.Transaction); err != nil {
		return V2IngressStep{}, err
	}
	if err := validatePlanToken(binding.Plan); err != nil {
		return V2IngressStep{}, err
	}
	if err := binding.Releases.Validate(); err != nil {
		return V2IngressStep{}, err
	}
	previousRank := 0
	for _, step := range binding.Steps {
		rank := ingressOperationRank(step.Kind)
		if rank == 0 || rank <= previousRank || !validStepCheckpoint(step.Checkpoint) ||
			validateFingerprint(step.Expected, "bound ingress expected fingerprint") != nil ||
			validateFingerprint(step.Desired, "bound ingress desired fingerprint") != nil {
			return V2IngressStep{}, invalid("ingress step bindings are not canonical")
		}
		if step.Static != "" {
			if err := validateFingerprint(step.Static, "bound ingress static fingerprint"); err != nil {
				return V2IngressStep{}, err
			}
		}
		previousRank = rank
	}
	if ingressOperationRank(operation.Kind) == 0 {
		return V2IngressStep{}, invalid("ingress step has an unknown operation kind")
	}
	if err := validateFingerprint(operation.Expected, "ingress expected fingerprint"); err != nil {
		return V2IngressStep{}, err
	}
	if err := validateFingerprint(operation.Desired, "ingress desired fingerprint"); err != nil {
		return V2IngressStep{}, err
	}
	binding.Steps = slices.Clone(binding.Steps)
	return V2IngressStep{
		Binding: binding, Kind: operation.Kind,
		Expected: operation.Expected, Desired: operation.Desired, Static: operation.Static,
	}, nil
}

func ingressStepBindings(steps []JournalStep) []V2IngressStepBinding {
	result := make([]V2IngressStepBinding, 0, 3)
	for _, step := range steps {
		kind, ok := ingressOperationKindForIntent(PlannerStepIntent{
			ID: step.ID, Migration: step.Migration, Resource: step.Resource,
		})
		if ok {
			static := Fingerprint("")
			if step.Evidence != nil {
				static = step.Evidence.Recovery
			}
			result = append(result, V2IngressStepBinding{
				Kind: kind, Checkpoint: step.Checkpoint,
				Expected: step.Expected, Desired: step.Desired, Static: static,
			})
		}
	}
	return result
}
