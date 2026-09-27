package reconcileruntime

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

type configInventoryFixture struct {
	t                    *testing.T
	home, state, program string
}

func newConfigInventoryFixture(t *testing.T) configInventoryFixture {
	t.Helper()
	root := testkit.TempDir(t)
	home := root + "/home"
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	return configInventoryFixture{t, home, root + "/state", strings.NewReplacer(
		"@STATE_ROOT@", root+"/state", "@STATE_UID@", fmt.Sprint(os.Getuid()),
	).Replace(integrationInventoryProgram)}
}

func (f configInventoryFixture) run(mode string, entries []map[string]any, wantError bool) {
	f.t.Helper()
	payload, err := json.Marshal(map[string]any{"home": f.home, "uid": os.Getuid(), "entries": entries})
	if err != nil {
		f.t.Fatal(err)
	}
	command := exec.Command("python3", "-B", "-c", f.program, mode)
	command.Stdin = strings.NewReader(string(payload))
	output, err := command.CombinedOutput()
	if (err != nil) != wantError {
		f.t.Fatalf("%s: error=%v, output=%s", mode, err, output)
	}
}

func (f configInventoryFixture) save(entries []map[string]any, released []string) {
	f.t.Helper()
	if err := os.MkdirAll(f.state, 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(f.state, 0700); err != nil {
		f.t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"schema": 1, "entries": entries, "released": released})
	if err != nil {
		f.t.Fatal(err)
	}
	testkit.WriteFile(f.t, f.state+"/inventory.json", payload, 0600)
}

func (f configInventoryFixture) write(payload []byte, mode os.FileMode) {
	f.t.Helper()
	testkit.WriteFile(f.t, f.home+"/settings", payload, mode)
	if err := os.Chown(f.home+"/settings", -1, os.Getuid()); err != nil {
		f.t.Fatal(err)
	}
}

func (f configInventoryFixture) read() map[string]any {
	f.t.Helper()
	payload, err := os.ReadFile(f.state + "/inventory.json")
	if err != nil {
		f.t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(payload, &value); err != nil {
		f.t.Fatal(err)
	}
	return value
}

func configInventoryEntry(home, kind, content string) map[string]any {
	entry := map[string]any{"id": "claude", "kind": kind, "path": home + "/settings", "digest": fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}
	if kind == "structured" {
		entry["format"] = "json"
	}
	return entry
}

func TestConfigInventoryAcknowledgement(t *testing.T) {
	t.Run("structured preserves unrelated intent and tombstones", func(t *testing.T) {
		f := newConfigInventoryFixture(t)
		old := configInventoryEntry(f.home, "structured", "old")
		old["pending_digest"] = "integration-intent"
		unrelated := map[string]any{"id": "codex", "kind": "package", "digest": "old-package", "pending_digest": "pending-package"}
		f.save([]map[string]any{old, unrelated}, []string{f.home + "/retired"})
		before := f.read()
		desired := []map[string]any{configInventoryEntry(f.home, "structured", "new")}
		f.run("configs-prepare", desired, false)
		f.run("configs-commit", desired, false)
		after := f.read()
		records := after["entries"].([]any)
		got := records[0].(map[string]any)
		if got["digest"] != desired[0]["digest"] || got["pending_digest"] != "integration-intent" || got["config_pending_digest"] != nil {
			t.Fatalf("config acknowledgement lost or retained wrong intent: %v", got)
		}
		if !reflect.DeepEqual(records[1], before["entries"].([]any)[1]) || !reflect.DeepEqual(after["released"], before["released"]) {
			t.Fatal("config acknowledgement changed unrelated ownership")
		}
		f.run("configs-prepare", desired, false)
		if !reflect.DeepEqual(after, f.read()) {
			t.Fatal("unchanged config preparation introduced pending intent")
		}
	})
	t.Run("missing ownership is not adopted", func(t *testing.T) {
		f := newConfigInventoryFixture(t)
		desired := []map[string]any{configInventoryEntry(f.home, "structured", "new")}
		for _, mode := range []string{"configs-prepare", "configs-commit"} {
			f.run(mode, desired, false)
		}
		if _, err := os.Stat(f.state); !os.IsNotExist(err) {
			t.Fatalf("missing inventory was created: %v", err)
		}
		f.save([]map[string]any{}, []string{f.home + "/retired"})
		before := f.read()
		for _, mode := range []string{"configs-prepare", "configs-commit"} {
			f.run(mode, desired, false)
		}
		if !reflect.DeepEqual(before, f.read()) {
			t.Fatal("missing record was adopted")
		}
	})
	t.Run("plain interrupted write can retry or retire", func(t *testing.T) {
		f := newConfigInventoryFixture(t)
		old := configInventoryEntry(f.home, "file", "old")
		f.save([]map[string]any{old}, []string{})
		f.write([]byte("old"), 0644)
		desired := []map[string]any{configInventoryEntry(f.home, "file", "new")}
		f.run("configs-prepare", desired, false)
		f.write([]byte("new"), 0644)
		f.run("observe", desired, false)
		f.run("configs-prepare", desired, false)
		f.run("configs-commit", desired, false)
		if got := f.read()["entries"].([]any)[0].(map[string]any); got["digest"] != desired[0]["digest"] || got["config_pending_digest"] != nil {
			t.Fatalf("retry did not acknowledge config: %v", got)
		}
		next := []map[string]any{configInventoryEntry(f.home, "file", "next")}
		f.run("configs-prepare", next, false)
		f.write([]byte("next"), 0644)
		f.run("apply", []map[string]any{}, false)
		f.run("commit", []map[string]any{}, false)
		if _, err := os.Stat(f.home + "/settings"); !os.IsNotExist(err) {
			t.Fatalf("interrupted config could not retire: %v", err)
		}
	})
	t.Run("reject owned drift identity mismatch and stale commit", func(t *testing.T) {
		f := newConfigInventoryFixture(t)
		old := configInventoryEntry(f.home, "file", "old")
		f.save([]map[string]any{old}, []string{})
		desired := []map[string]any{configInventoryEntry(f.home, "file", "new")}
		f.write([]byte("operator edit"), 0644)
		before := f.read()
		f.run("configs-prepare", desired, true)
		if !reflect.DeepEqual(before, f.read()) {
			t.Fatal("rejected drift changed inventory")
		}
		content, err := os.ReadFile(f.home + "/settings")
		if err != nil || string(content) != "operator edit" {
			t.Fatal("rejected drift changed user file")
		}
		f.write([]byte("old"), 0644)
		wrong := configInventoryEntry(f.home, "file", "new")
		wrong["id"] = "codex"
		f.run("configs-prepare", []map[string]any{wrong}, true)
		f.run("configs-prepare", desired, false)
		prepared := f.read()
		f.run("configs-commit", []map[string]any{old}, true)
		if !reflect.DeepEqual(prepared, f.read()) {
			t.Fatal("stale commit discarded prepared intent")
		}
		// A correct intent is insufficient until both bytes and permissions match.
		f.run("configs-commit", desired, true)
		f.write([]byte("new"), 0600)
		f.run("configs-commit", desired, true)
	})
}
