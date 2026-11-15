package migration

import (
	"context"
	"errors"
	"slices"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

// V2CompatibilityIngress reads retained V1 history bound into V2 journals.
type V2CompatibilityIngress struct {
	Legacy *V2LegacyIngress
}

func (ingress *V2CompatibilityIngress) Inspect(
	ctx context.Context,
	binding *releasetransition.V2IngressBinding,
) (releasetransition.V2IngressInspection, error) {
	var result releasetransition.V2IngressInspection
	if ingress.Legacy != nil && bindingIncludesIngressKind(
		binding, releasetransition.V2LegacyV1Import,
	) {
		legacy, err := ingress.Legacy.Inspect(ctx, binding)
		if err != nil {
			return result, err
		}
		result.Operations = append(result.Operations, legacy.Operations...)
		result.Decisions = append(result.Decisions, legacy.Decisions...)
		result.Blockers = append(result.Blockers, legacy.Blockers...)
	}
	return result, nil
}

func bindingIncludesIngressKind(
	binding *releasetransition.V2IngressBinding,
	kinds ...releasetransition.V2IngressOperationKind,
) bool {
	if binding == nil {
		return true
	}
	for _, step := range binding.Steps {
		if slices.Contains(kinds, step.Kind) {
			return true
		}
	}
	return false
}

func (ingress *V2CompatibilityIngress) leaf(kind releasetransition.V2IngressOperationKind) releasetransition.V2Ingress {
	if kind == releasetransition.V2LegacyV1Import {
		if ingress.Legacy == nil {
			return nil
		}
		return ingress.Legacy
	}
	return nil
}

func (ingress *V2CompatibilityIngress) requireLeaf(
	kind releasetransition.V2IngressOperationKind,
) (releasetransition.V2Ingress, error) {
	leaf := ingress.leaf(kind)
	if leaf == nil {
		return nil, errors.New("compatibility ingress operation is unavailable")
	}
	return leaf, nil
}

func (ingress *V2CompatibilityIngress) Observe(ctx context.Context, step releasetransition.V2IngressStep) (releasetransition.Fingerprint, error) {
	leaf, err := ingress.requireLeaf(step.Kind)
	if err != nil {
		return "", err
	}
	return leaf.Observe(ctx, step)
}
func (ingress *V2CompatibilityIngress) Apply(ctx context.Context, step releasetransition.V2IngressStep) error {
	leaf, err := ingress.requireLeaf(step.Kind)
	if err != nil {
		return err
	}
	return leaf.Apply(ctx, step)
}
func (ingress *V2CompatibilityIngress) Verify(ctx context.Context, step releasetransition.V2IngressStep) error {
	leaf, err := ingress.requireLeaf(step.Kind)
	if err != nil {
		return err
	}
	return leaf.Verify(ctx, step)
}
