package migration

import (
	"context"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

func TestV2CompatibilityIngressRejectsUnavailableLeaf(t *testing.T) {
	for _, kind := range []releasetransition.V2IngressOperationKind{
		releasetransition.V2LegacyV1Import,
		releasetransition.V2SourceImport,
		releasetransition.V2SourceEntrypoints,
	} {
		t.Run(string(kind), func(t *testing.T) {
			ingress := &V2CompatibilityIngress{}
			step := releasetransition.V2IngressStep{Kind: kind}
			ctx := context.Background()
			_, observeErr := ingress.Observe(ctx, step)
			for name, err := range map[string]error{
				"observe": observeErr,
				"apply":   ingress.Apply(ctx, step),
				"verify":  ingress.Verify(ctx, step),
			} {
				if err == nil || err.Error() != "compatibility ingress operation is unavailable" {
					t.Fatalf("%s unavailable leaf: %v", name, err)
				}
			}
		})
	}
}
