package reconcileruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

// An update captures shipped sources under the candidate's inherited root
// handle, which is gone once that process exits; retained releases are later
// pruned. Passive inspection must still read the reading release's shipped file.
func TestAppliedShippedSourcesFollowReadingRelease(t *testing.T) {
	const releases = "/home/operator/.subyard/runtime/releases"
	// @root is the reading release root; only its own shipped copy may replace a capture.
	const current = "@root/config/agents/claude/provision.sh"
	for _, scenario := range []struct {
		name, applied, current, want string
	}{
		{"inherited-root", "/proc/69188/fd/7/config/agents/claude/provision.sh", current, current},
		{"pruned-release", releases + "/0.21.3-0e4eaa271b72/config/agents/claude/provision.sh", current, current},
		{"operator-file", "/home/operator/provision.sh", current, "/home/operator/provision.sh"},
		{"other-shipped-file", "/proc/69188/fd/7/config/agents/claude/legacy.sh", current, "/proc/69188/fd/7/config/agents/claude/legacy.sh"},
		{"outside-config", "/proc/69188/fd/7/bin/provision.sh", "@root/bin/provision.sh", "/proc/69188/fd/7/bin/provision.sh"},
		{"operator-current", "/proc/69188/fd/7/config/agents/claude/provision.sh", "/home/operator/provision.sh", "/proc/69188/fd/7/config/agents/claude/provision.sh"},
		{"operator-release-shaped", "/proc/69188/fd/7/config/agents/claude/provision.sh", "/home/operator/releases/v3/config/agents/claude/provision.sh", "/proc/69188/fd/7/config/agents/claude/provision.sh"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			scenario.current = strings.ReplaceAll(scenario.current, "@root", root)
			scenario.want = strings.ReplaceAll(scenario.want, "@root", root)
			state := filepath.Join(root, "inventory")
			if err := os.Mkdir(state, 0o700); err != nil {
				t.Fatal(err)
			}
			settings := map[string]string{"DEV_USER": "dev", "DEV_UID": "1001", "AGENT_claude_PROVISION": scenario.applied, "AGENT_claude_COMMAND": "claude"}
			inventory := map[string]any{"schema": 1, "entries": []integrationArtifact{{ID: "claude", Kind: "package", Digest: strings.Repeat("b", 64)}}, "applied_settings": settings}
			payload, _ := json.Marshal(inventory)
			testkit.WriteFile(t, filepath.Join(state, "inventory.json"), payload, 0o600)
			runtime := Runtime{RepositoryRoot: root, Executor: &appliedOwnerInventoryExecutor{state: state}, Integrations: []profile.IntegrationDefinition{},
				Environment: []string{"AGENT_claude_PROVISION=" + scenario.current, "AGENT_claude_COMMAND=other"}, Yard: domain.Context{DevUser: "dev", DevUID: 1001}}
			applied, err := runtime.AppliedIntegrationConfiguration(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := applied.Settings["AGENT_claude_PROVISION"]; got != scenario.want {
				t.Fatalf("applied provision = %q, want %q", got, scenario.want)
			}
			if applied.Settings["AGENT_claude_COMMAND"] != "claude" || applied.Settings["DEV_USER"] != "dev" {
				t.Fatalf("non-source applied settings changed: %#v", applied.Settings)
			}
			if settings["AGENT_claude_PROVISION"] != scenario.applied {
				t.Fatal("relocation mutated the observed snapshot")
			}
		})
	}
}
