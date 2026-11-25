package releaseruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

// The shipped declaration of a 0.33.0 release that retires the 0.28-era
// migrations refuses rollback to targets through 0.31.0 and leaves 0.32.0 open.
func TestShippedRegistryBoundaryRefusesThrough031(t *testing.T) {
	shipped, err := os.ReadFile("../../../config/release-rollback.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		target  string
		refused bool
	}{
		{target: "0.28.0", refused: true},
		{target: "0.28.1", refused: true},
		{target: "0.30.0", refused: true},
		{target: "0.31.0", refused: true},
		{target: "0.32.0"},
	} {
		t.Run(test.target, func(t *testing.T) {
			fixture := newPreparedRollbackFixture(t, preparedRollbackOptions{activeVersion: "0.33.0", targetVersion: test.target,
				seed: func(active, _ string) {
					testkit.WriteFile(t, filepath.Join(active, "config/release-rollback.json"), shipped, 0o600)
				}})
			active, target := fixture.verified(t)
			message, _, cleanup, boundaryErr := declaredRollbackBoundary(active, target)
			t.Logf("target %s: refusal=%q agent notices=%d", test.target, message, len(cleanup))
			_, err := fixture.prepare(t)
			if !test.refused {
				if boundaryErr != nil || message != "" || len(cleanup) != 0 {
					t.Fatalf("0.32.0 rollback met a boundary: %q, %v", message, boundaryErr)
				}
				if err != nil {
					t.Fatalf("0.32.0 rollback preparation refused: %v", err)
				}
				return
			}
			outcome := outcomeOf(t, err)
			if outcome.Code != releasetransition.CodeRollbackIncompatible ||
				!strings.Contains(err.Error(), "declared rollback boundary for registry through 0.31.0") ||
				!strings.Contains(err.Error(), "clean installation of that release") ||
				!strings.Contains(outcome.Retry, "keep the current release active") {
				t.Fatalf("refusal = %#v, %v", outcome, err)
			}
			t.Logf("target %s: code=%s retry=%q", test.target, outcome.Code, outcome.Retry)
			fixture.assertUnchanged(t)
			if len(fixture.applies) != 0 {
				t.Fatal("refusal mutated profile state")
			}
		})
	}
}
