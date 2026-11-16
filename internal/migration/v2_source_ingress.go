package migration

import (
	"context"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

// V2CompatibilityIngress refuses retired ingress in unfinished V2 journals.
// Completed V2 provenance is read through the release transition store.
type V2CompatibilityIngress struct{}

func (*V2CompatibilityIngress) Inspect(
	_ context.Context,
	binding *releasetransition.V2IngressBinding,
) (releasetransition.V2IngressInspection, error) {
	if binding != nil {
		for _, step := range binding.Steps {
			if step.Kind == releasetransition.V2LegacyV1Import {
				return releasetransition.V2IngressInspection{}, releasetransition.ErrCompatibilityIngressUnavailable
			}
		}
	}
	return releasetransition.V2IngressInspection{}, nil
}

func (*V2CompatibilityIngress) Observe(context.Context, releasetransition.V2IngressStep) (releasetransition.Fingerprint, error) {
	return "", releasetransition.ErrCompatibilityIngressUnavailable
}
func (*V2CompatibilityIngress) Apply(context.Context, releasetransition.V2IngressStep) error {
	return releasetransition.ErrCompatibilityIngressUnavailable
}
func (*V2CompatibilityIngress) Verify(context.Context, releasetransition.V2IngressStep) error {
	return releasetransition.ErrCompatibilityIngressUnavailable
}
