package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Inventories that releases through 0.32.0 captured record the retired host
// instruction settings in applied settings and diagnostic digests, and a newer
// release can record settings this one does not define. They are not inputs:
// the applied read drops them, and the next capture replaces them.
func TestRetiredInstructionSettingsInAppliedInventoryAreIgnored(t *testing.T) {
	root := testkit.TempDir(t)
	state := filepath.Join(root, "inventory")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	settings := map[string]string{"DEV_USER": "dev", "DEV_UID": "1001", "HOST_CLAUDE_MD": "/home/operator/.claude/CLAUDE.md", "HOST_CODEX_AGENTS_MD": "/home/operator/.codex/AGENTS.md", "FUTURE_SETTING": "private-canary"}
	key := strings.Repeat("c", 64)
	diagnostics := map[string]any{"key": key, "digests": map[string]string{"HOST_CLAUDE_MD": strings.Repeat("d", 64), "HOST_OPENCODE_AGENTS_MD": strings.Repeat("e", 64)}}
	// The binding older writers computed: sha256 of the sorted compact JSON of [settings, entries].
	bound, err := json.Marshal([]any{settings, []any{}})
	if err != nil {
		t.Fatal(err)
	}
	inventory := map[string]any{"schema": 1, "entries": []any{}, "applied_settings": settings, "diagnostics": diagnostics, "diagnostic_binding": fmt.Sprintf("%x", sha256.Sum256(bound))}
	payload, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "inventory.json")
	testkit.WriteFile(t, path, payload, 0o600)
	runtime := Runtime{RepositoryRoot: root, Executor: &appliedOwnerInventoryExecutor{state: state}, Integrations: []profile.IntegrationDefinition{}}
	check := func(want map[string]string) {
		t.Helper()
		applied, err := runtime.AppliedIntegrationConfiguration(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(applied.Settings) != len(want) || applied.Settings["DEV_USER"] != "dev" || applied.Settings["DEV_UID"] != "1001" {
			t.Fatalf("applied settings kept retired inputs: %v", applied.Settings)
		}
		if applied.Diagnostics == nil || applied.Diagnostics.Key != key || len(applied.Diagnostics.Digests) != 0 {
			t.Fatalf("applied diagnostics kept retired inputs or lost their key: %#v", applied.Diagnostics)
		}
	}
	check(map[string]string{"DEV_USER": "dev", "DEV_UID": "1001"})

	program := strings.NewReplacer("@STATE_ROOT@", state, "@STATE_UID@", fmt.Sprint(os.Getuid())).Replace(integrationInventoryProgram)
	run := func(mode string, request map[string]any) []byte {
		t.Helper()
		input, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command("python3", "-B", "-c", program, mode)
		command.Stdin = strings.NewReader(string(input))
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", mode, err, output)
		}
		return output
	}
	request := map[string]any{"home": "/home/dev", "uid": 1001, "entries": []any{}}
	var observed struct{ Fingerprint string }
	if err := json.Unmarshal(run("observe", request), &observed); err != nil || len(observed.Fingerprint) != 64 {
		t.Fatalf("observe: %#v %v", observed, err)
	}
	request["expected"] = observed.Fingerprint
	request["applied_settings"] = map[string]string{"DEV_USER": "dev", "DEV_UID": "1001"}
	request["diagnostics"] = map[string]any{"key": key, "digests": map[string]string{}}
	run("capture-settings", request)
	stored, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(stored), "HOST_") || strings.Contains(string(stored), "FUTURE_") {
		t.Fatalf("capture kept retired settings: %s %v", stored, err)
	}
	check(map[string]string{"DEV_USER": "dev", "DEV_UID": "1001"})
}

// Ignoring names this release does not read never relaxes the values of the
// names it reads, and never renders a refused value.
func TestAppliedInventoryRefusesMalformedKnownSettings(t *testing.T) {
	for _, settings := range []map[string]string{
		{"DEV_USER": "dev", "DEV_UID": "1001", "FUTURE_SETTING": "x"},
		{"DEV_USER": "dev", "DEV_UID": "private-canary", "FUTURE_SETTING": "x"},
		{"DEV_USER": "dev", "DEV_UID": "1001", "DEV_SUDO": "private-canary"},
	} {
		valid := settings["FUTURE_SETTING"] != "" && settings["DEV_UID"] == "1001"
		root := testkit.TempDir(t)
		state := filepath.Join(root, "inventory")
		if err := os.Mkdir(state, 0o700); err != nil {
			t.Fatal(err)
		}
		bound, err := json.Marshal([]any{settings, []any{}})
		if err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(map[string]any{"schema": 1, "entries": []any{}, "applied_settings": settings, "diagnostic_binding": fmt.Sprintf("%x", sha256.Sum256(bound))})
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(state, "inventory.json"), payload, 0o600)
		runtime := Runtime{RepositoryRoot: root, Executor: &appliedOwnerInventoryExecutor{state: state}, Integrations: []profile.IntegrationDefinition{}}
		applied, err := runtime.AppliedIntegrationConfiguration(context.Background())
		if valid {
			if err != nil || len(applied.Settings) != 2 {
				t.Fatalf("control snapshot %v: %#v %v", settings, applied.Settings, err)
			}
			continue
		}
		if err == nil || strings.Contains(err.Error(), "private-canary") {
			t.Fatalf("malformed applied setting %v: %v", settings, err)
		}
	}
}
