package releaseruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestDeclaredRollbackBoundaryRefusesBeforePreparation(t *testing.T) {
	for _, test := range []struct {
		name, through                          string
		target, tamper, unsealed, format, long bool
		refused                                bool
	}{
		{name: "active release declares boundary", through: "0.30.0", refused: true},
		{name: "bounded outcome retains boundary diagnostic", through: "0.30.0", long: true, refused: true},
		{name: "older targets also refused", through: "0.30.1", refused: true},
		{name: "target beyond boundary", through: "0.27.0"},
		{name: "target declaration cannot forbid upgrade", through: "0.30.0", target: true},
		{name: "tampered active declaration", through: "0.30.0", tamper: true, refused: true},
		{name: "unsealed declaration", through: "0.30.0", unsealed: true, refused: true},
		{name: "independent format refusal", through: "0.30.0", format: true, refused: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := preparedRollbackOptions{seed: func(active, target string) {
				root := active
				if test.target {
					root = target
				}
				declaration := releasetransition.RollbackDeclaration{SchemaVersion: 1,
					PredecessorRegistryDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)),
					Migrations:                []json.RawMessage{}, ProfileFormats: []json.RawMessage{},
					Boundaries: []releasetransition.RollbackBoundary{{Component: "profile:sample-runtime", ThroughVersion: test.through,
						Reason: "sample state cannot be read by the previous release", Recovery: "keep the current release active and restore a saved sample database before manual downgrade"}}}
				if test.long {
					declaration.Boundaries[0].Reason = strings.Repeat("sample state ", 32) + "is unreadable"
				}
				data, err := json.Marshal(declaration)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(root, "config/release-rollback.json"), data, 0o600)
			}}
			if test.format {
				options.active = `,"format":2`
			}
			fixture := newPreparedRollbackFixture(t, options)
			root := filepath.Join(fixture.runtimeRoot, "releases", fixture.activeID)
			if test.tamper {
				testkit.WriteFile(t, filepath.Join(root, "config/release-rollback.json"), []byte("{}"), 0o600)
			}
			if test.unsealed {
				path := filepath.Join(root, "runtime-files.sha256")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var kept []string
				for _, line := range strings.Split(string(data), "\n") {
					if !strings.Contains(line, "./config/release-rollback.json") {
						kept = append(kept, line)
					}
				}
				testkit.WriteFile(t, path, []byte(strings.Join(kept, "\n")), 0o600)
			}
			_, err := fixture.prepare(t)
			if test.tamper || test.unsealed {
				if err == nil {
					t.Fatal("unverified declaration accepted")
				}
			} else if test.refused {
				outcome := outcomeOf(t, err)
				if outcome.Code != releasetransition.CodeRollbackIncompatible ||
					!strings.Contains(err.Error(), "declared rollback boundary") || !strings.Contains(err.Error(), "restore a saved sample database") ||
					!strings.Contains(outcome.Retry, "keep the current release active") {
					t.Fatalf("refusal lost boundary or next step: %#v, %v", outcome, err)
				}
				if test.long && !strings.Contains(outcome.Message, "declared release boundary") {
					t.Fatalf("bounded diagnostic lost boundary: %#v", outcome)
				}
				if test.format && !strings.Contains(err.Error(), "sample-runtime 1 instead of 2") {
					t.Fatalf("independent format finding lost: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			fixture.assertUnchanged(t)
			if len(fixture.applies) != 0 {
				t.Fatal("refusal mutated profile state")
			}
		})
	}
}
