package releaseruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Export actual component leaf observations beside the historical aggregate
// verdict. The Rust policy oracle consumes facts, never a runtime eligibility RPC.
func TestRollbackSupportLeafOracle(t *testing.T) {
	type entry struct {
		Name         string                `json:"name"`
		Support      ports.RollbackSupport `json:"support"`
		TargetOwners []string              `json:"targetOwners"`
		Accepted     bool                  `json:"accepted"`
	}
	corpus := []entry{}
	for _, name := range []string{"same-owned", "same-unowned-absent", "guest-selected-loss", "guest-deselected-absent", "guest-deselected-stale", "managed-present-unowned", "managed-present-loss", "native-current-no-ownership", "native-current-owned", "core-same", "core-different-absent", "core-different-installed", "core-different-supported", "core-settings-installed", "core-settings-absent"} {
		t.Run(name, func(t *testing.T) {
			runtime, source, target, home, installed := retainedRuntimeFixture(t)
			defer runtime.Close()
			owners := []string{"sample-runtime"}
			observation := ports.RuntimeObservation{State: ports.RuntimeStateAbsent}
			presence := ports.RuntimePresence{Actual: "absent"}
			switch name {
			case "same-unowned-absent":
				owners = []string{}
			case "guest-selected-loss", "guest-deselected-absent", "guest-deselected-stale":
				testkit.WriteFile(t, filepath.Join(source, "config/profiles/sample/profile.json"), []byte(`{"schema_version":1,"runtime":{"activation_id":"sample-runtime","handler":"runtime.sh"}}`), 0o600)
				testkit.WriteFile(t, filepath.Join(target, "config/profiles/sample/profile.json"), []byte(`{"schema_version":1}`), 0o600)
				selected := ""
				if name == "guest-selected-loss" {
					selected = "sample"
				}
				testkit.WriteFile(t, filepath.Join(home, "config.env"), []byte("ENVIRONMENT_PROFILES="+selected+"\n"), 0o600)
				if name == "guest-deselected-stale" {
					observation.State = ports.RuntimeStateStale
				}
			case "managed-present-unowned", "managed-present-loss":
				testkit.WriteFile(t, installed, []byte("installed"), 0o600)
				if name == "managed-present-unowned" {
					owners = []string{}
				} else {
					testkit.WriteFile(t, filepath.Join(target, "config/profiles/sample/profile.json"), []byte(`{"schema_version":1}`), 0o600)
				}
			case "native-current-no-ownership", "native-current-owned":
				testkit.WriteFile(t, filepath.Join(target, "config/profiles/sample/profile.json"), []byte(`{"schema_version":1}`), 0o600)
				observation.State = ports.RuntimeStateCurrent
				observation.OwnershipAbsent = name == "native-current-no-ownership"
			}
			if len(name) >= 5 && name[:5] == "core-" {
				for _, root := range []string{source, target} {
					if err := os.MkdirAll(filepath.Join(root, "config/preview"), 0o700); err != nil {
						t.Fatal(err)
					}
					testkit.WriteFile(t, filepath.Join(root, "config/preview/subyard-preview"), []byte("same"), 0o755)
				}
				if name != "core-same" && name != "core-settings-installed" && name != "core-settings-absent" {
					testkit.WriteFile(t, filepath.Join(target, "config/preview/subyard-preview"), []byte("different"), 0o755)
				}
				if name == "core-different-installed" || name == "core-different-supported" {
					presence.Preview = true
				}
				if name == "core-different-supported" {
					owners = append(owners, "core-guest-runtime")
				}
				if name == "core-settings-installed" || name == "core-settings-absent" {
					testkit.WriteFile(t, filepath.Join(source, "config/agents.env"), []byte("CCUSAGE_VERSION=20.0.30\n"), 0o600)
					testkit.WriteFile(t, filepath.Join(target, "config/agents.env"), []byte("CCUSAGE_VERSION=20.0.20\n"), 0o600)
					presence.CCUsage = name == "core-settings-installed"
				}
			}
			runtime.config.ProfileRuntimeObservation = func(context.Context, string, config.Loaded, string) (ports.RuntimeObservation, error) {
				return observation, nil
			}
			runtime.config.InstalledRuntimePresence = func(_ context.Context, _ string, _ config.Loaded, owner string) (ports.RuntimePresence, error) {
				if owner == "core-guest-runtime" {
					return presence, nil
				}
				return ports.RuntimePresence{Actual: "absent"}, nil
			}
			support, err := CaptureRollbackSupport(context.Background(), runtime.config, source, target, home, owners, nil)
			if err != nil {
				t.Fatal(err)
			}
			ownerMap := map[string]bool{}
			for _, owner := range owners {
				ownerMap[owner] = true
			}
			_, legacyErr := runtime.retainedRuntimeBinding(context.Background(), source, target, home, ownerMap)
			corpus = append(corpus, entry{name, support, owners, legacyErr == nil})
		})
	}
	if destination := os.Getenv("SUBYARD_RUST_ROLLBACK_CORPUS"); destination != "" {
		raw, err := json.Marshal(corpus)
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, destination, raw, 0o600)
	}
}
