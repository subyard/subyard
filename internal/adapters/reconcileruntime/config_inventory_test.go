package reconcileruntime

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

func (f configInventoryFixture) run(mode string, entries []map[string]any, wantError bool, options ...map[string]any) []byte {
	f.t.Helper()
	request := map[string]any{"home": f.home, "uid": os.Getuid(), "entries": entries}
	for _, extra := range options {
		for key, value := range extra {
			request[key] = value
		}
	}
	payload, err := json.Marshal(request)
	if err != nil {
		f.t.Fatal(err)
	}
	command := exec.Command("python3", "-B", "-c", f.program, mode)
	command.Stdin = strings.NewReader(string(payload))
	output, err := command.CombinedOutput()
	if (err != nil) != wantError {
		f.t.Fatalf("%s: error=%v, output=%s", mode, err, output)
	}
	return output
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

func TestConfigInventoryPreviewMigrationCustody(t *testing.T) {
	for _, scenario := range []string{"configs-lock", "selected", "retired", "retired-cut-pending", "retired-cut-first"} {
		t.Run(scenario, func(t *testing.T) {
			f := newConfigInventoryFixture(t)
			path := filepath.Join(f.home, ".claude/CLAUDE.md")
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			original := []byte("Host text.\n" + previewInstructions + "\n")
			begin, end := strings.Index(string(original), "<!-- subyard-preview -->"), strings.Index(string(original), "<!-- /subyard-preview -->")+len("<!-- /subyard-preview -->")
			stripped := append(append([]byte(nil), original[:begin]...), original[end:]...)
			final := stripped
			if scenario == "selected" {
				final = append(append([]byte(nil), stripped...), []byte("Selected source suffix.\n")...)
			}
			testkit.WriteFile(t, path, original, 0644)
			if err := os.Chown(path, -1, os.Getuid()); err != nil {
				t.Fatal(err)
			}
			record := configInventoryEntry(f.home, "file", string(original))
			record["path"] = path
			records := []map[string]any{record}
			if strings.HasPrefix(scenario, "retired-cut-") {
				second := filepath.Join(f.home, ".codex/AGENTS.md")
				if err := os.MkdirAll(filepath.Dir(second), 0755); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, second, original, 0644)
				if err := os.Chown(second, -1, os.Getuid()); err != nil {
					t.Fatal(err)
				}
				records = append(records, map[string]any{"id": "second", "kind": "file", "path": second, "digest": record["digest"]})
			}
			f.save(records, []string{})
			options := map[string]any{"preview_skill_cutover": true}
			if scenario == "configs-lock" {
				f.run("configs-prepare", []map[string]any{{"id": "sample", "kind": "package", "digest": strings.Repeat("a", 64)}}, true, options)
				if actual, err := os.ReadFile(path); err != nil || string(actual) != string(original) {
					t.Fatal("invalid config request changed preview instructions")
				}
				f.program = strings.Replace(f.program, "    # Publish retry identities before replacing any file.", `    with open(ROOT + '/inventory.lock', 'rb') as probe:
        try:
            fcntl.flock(probe.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            pass
        else:
            raise AssertionError('migration has no stable exclusive lock')
    # Publish retry identities before replacing any file.`, 1)
				f.run("configs-prepare", nil, false, options)
			} else {
				desired := []map[string]any{{"id": "sample", "kind": "package", "digest": strings.Repeat("a", 64)}}
				if scenario == "selected" {
					desired = []map[string]any{{"id": record["id"], "kind": "file", "path": path, "digest": fmt.Sprintf("%x", sha256.Sum256(final)), "content": base64.StdEncoding.EncodeToString(final)}}
				}
				capture := map[string]any{}
				if err := json.Unmarshal(f.run("observe", desired, false, map[string]any{"preview_skill_cutover": true, "after_owner": map[string]any{"capture": true}}), &capture); err != nil {
					t.Fatal(err)
				}
				witness := capture["after_owner"].(map[string]any)
				guarded := map[string]any{"preview_skill_cutover": true, "after_owner": witness}
				if strings.HasPrefix(scenario, "retired-cut-") {
					uncut := f.program
					cutPath := f.state + "/inventory.json"
					if scenario == "retired-cut-first" {
						cutPath = path
					}
					f.program = strings.Replace(f.program, "        os.fsync(parent)\n", "        os.fsync(parent)\n        if path == "+fmt.Sprintf("%q", cutPath)+": raise SystemExit(75)\n", 1)
					f.run("apply", desired, true, guarded)
					raw, err := os.ReadFile(f.state + "/inventory.json")
					if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != witness["pending"] {
						t.Fatal("cut left an unapproved inventory receipt")
					}
					for i, entry := range records {
						body, err := os.ReadFile(entry["path"].(string))
						expected := original
						if scenario == "retired-cut-first" && i == 0 {
							expected = stripped
						}
						if err != nil || string(body) != string(expected) {
							t.Fatal("cut did not reach expected preview write")
						}
					}
					f.program = uncut
				}
				f.run("apply", desired, false, guarded)
				raw, err := os.ReadFile(f.state + "/inventory.json")
				if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != witness["pending"] {
					t.Fatal("captured pending receipt differs")
				}
				f.run("apply", desired, false, guarded)
				f.run("commit", desired, false, guarded)
				raw, err = os.ReadFile(f.state + "/inventory.json")
				if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != witness["committed"] {
					t.Fatal("captured committed receipt differs")
				}
				f.run("observe", desired, false, guarded)
				released := f.read()["released"].([]any)
				if (len(released) == len(records) && released[0] == path) != strings.HasPrefix(scenario, "retired") {
					t.Fatal("preview retirement tombstone differs")
				}
			}
			body, err := os.ReadFile(path)
			if err != nil || string(body) != string(final) {
				t.Fatal("preview migration changed retained text")
			}
		})
	}
}
