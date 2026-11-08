package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
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

// Execute the production transport request against this fixture's private paths.
func (f configInventoryFixture) Exec(ctx context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	var payload map[string]any
	if err := json.Unmarshal(request.Stdin, &payload); err != nil {
		return ports.InstanceExecResult{}, err
	}
	payload["home"], payload["uid"] = f.home, os.Getuid()
	data, err := json.Marshal(payload)
	if err != nil {
		return ports.InstanceExecResult{}, err
	}
	command := exec.CommandContext(ctx, "python3", "-B", "-c", f.program, request.Command[4])
	command.Stdin = strings.NewReader(string(data))
	output, err := command.CombinedOutput()
	result := ports.InstanceExecResult{Stdout: output}
	if err != nil {
		result.ExitCode = command.ProcessState.ExitCode()
	}
	return result, err
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

func TestConfigInventoryCapturesOnlyCommittedSourceEvidence(t *testing.T) {
	f := newConfigInventoryFixture(t)
	old := configInventoryEntry(f.home, "file", "old")
	f.save([]map[string]any{old}, []string{})
	inventory := f.read()
	key := strings.Repeat("a", 64)
	inventory["diagnostics"] = map[string]any{"key": key, "digests": map[string]string{}}
	encoded, _ := json.Marshal(inventory)
	testkit.WriteFile(t, f.state+"/inventory.json", encoded, 0600)
	f.write([]byte("old"), 0644)
	desired := []map[string]any{configInventoryEntry(f.home, "file", "new")}
	f.run("configs-prepare", desired, false)
	f.write([]byte("new"), 0644)
	request := map[string]any{"home": f.home, "uid": os.Getuid(), "entries": desired, "config_diagnostics": map[string]any{"key": key, "files": map[string]any{
		"AGENT_claude_CONFIG": map[string]string{"path": "/source", "digest": strings.Repeat("b", 64), "destination": f.home + "/settings"},
		"AGENT_other_CONFIG":  map[string]string{"path": "/other", "digest": strings.Repeat("c", 64), "destination": f.home + "/unowned"},
	}}}
	payload, _ := json.Marshal(request)
	result, err := f.Exec(context.Background(), "", "", ports.InstanceExecRequest{Command: []string{"", "", "", "", "configs-commit"}, Stdin: payload})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("commit: %v", err)
	}
	files := f.read()["diagnostics"].(map[string]any)["files"].(map[string]any)
	if len(files) != 1 || files["AGENT_claude_CONFIG"].(map[string]any)["path"] != "/source" {
		t.Fatalf("wrong committed evidence: %#v", files)
	}
}

func TestConfigInventoryCreatesEvidenceOnFirstConfigCommit(t *testing.T) {
	f := newConfigInventoryFixture(t)
	old := configInventoryEntry(f.home, "file", "old")
	f.save([]map[string]any{old}, []string{})
	f.write([]byte("old"), 0644)
	desired := []map[string]any{configInventoryEntry(f.home, "file", "new")}
	f.run("configs-prepare", desired, false)
	f.write([]byte("new"), 0644)
	runtime := Runtime{RepositoryRoot: "../../..", Executor: f, Yard: domain.Context{DevUser: "dev", DevUID: os.Getuid()}}
	prepared := &PreparedConfigs{files: []guestConfigFile{{setting: "AGENT_claude_CONFIG", source: "/source", destination: f.home + "/settings"}}, sources: map[configSourceKey]configSourceBytes{{path: "/source"}: {exists: true, payload: []byte("new")}}}
	evidence, err := runtime.prepareConfigDiagnostics(context.Background(), prepared)
	if err != nil || evidence == nil {
		t.Fatalf("prepare first evidence: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{"entries": desired, "config_diagnostics": evidence})
	result, err := f.Exec(context.Background(), "", "", ports.InstanceExecRequest{Command: []string{"", "", "", "", "configs-commit"}, Stdin: payload})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("commit: %v", err)
	}
	diagnostics := f.read()["diagnostics"].(map[string]any)
	files := diagnostics["files"].(map[string]any)
	key, err := hex.DecodeString(evidence.Key)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := config.LoadCatalog("../../..")
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := catalog.LookupSetting("AGENT_claude_CONFIG")
	if !ok {
		t.Fatal("missing config setting")
	}
	file := files["AGENT_claude_CONFIG"].(map[string]any)
	if diagnostics["key"] != evidence.Key || file["path"] != "/source" || file["digest"] != config.ConfigurationInputDigest("AGENT_claude_CONFIG", definition.Kind, key, []byte("new")) {
		t.Fatalf("first commit missing evidence: %#v", diagnostics)
	}
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

// Real apply leaves intent before the hook/commit boundary. Passive admission
// must recognize only redundant intent, without completing the pending init.
func TestAppliedInventoryObservationAfterHookFailure(t *testing.T) {
	for _, scenario := range []string{"redundant", "different-pending", "source-drift", "foreign", "missing", "mode"} {
		t.Run(scenario, func(t *testing.T) {
			f := newConfigInventoryFixture(t)
			entry := configInventoryEntry(f.home, "file", "owned")
			entry["content"] = base64.StdEncoding.EncodeToString([]byte("owned"))
			entry["mode"] = 0644
			entries := []map[string]any{entry}
			f.run("apply", entries, false)
			f.run("commit", entries, false)
			inventory := f.read()
			inventory["applied_settings"] = map[string]string{"AGENT_claude_COMMAND": "old-command"}
			payload, _ := json.Marshal(inventory)
			testkit.WriteFile(t, f.state+"/inventory.json", payload, 0600)
			f.run("apply", entries, false) // No commit: the following hook failed.
			switch scenario {
			case "different-pending":
				changed := configInventoryEntry(f.home, "file", "next")
				changed["content"] = base64.StdEncoding.EncodeToString([]byte("next"))
				changed["mode"] = 0644
				f.run("apply", []map[string]any{changed}, false)
			case "source-drift":
				entry["digest"] = strings.Repeat("a", 64)
			case "foreign":
				f.write([]byte("foreign"), 0644)
			case "missing":
				if err := os.Remove(f.home + "/settings"); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(f.home+"/settings", 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(f.state + "/inventory.json")
			if err != nil {
				t.Fatal(err)
			}
			beforeInfo, err := os.Stat(f.state + "/inventory.json")
			if err != nil {
				t.Fatal(err)
			}
			run := func(mode string) (integrationObservation, error) {
				t.Helper()
				data, _ := json.Marshal(entries)
				var artifacts []integrationArtifact
				if err := json.Unmarshal(data, &artifacts); err != nil {
					t.Fatal(err)
				}
				runtime := Runtime{Executor: f, appliedIntegrationInspection: mode == "observe-applied"}
				return runtime.integrationInventory(context.Background(), "observe", artifacts, "")
			}
			passive, err := run("observe-applied")
			if scenario == "redundant" {
				if err != nil || passive.Changed || len(passive.Fingerprint) != 64 || len(passive.Facts) == 0 {
					t.Fatalf("redundant intent prevents admission: %#v %v", passive, err)
				}
				for _, fact := range passive.Facts {
					if !fact.AtDesired {
						t.Fatalf("unexpected drift: %#v", fact)
					}
				}
				normal, err := run("observe")
				if err != nil || !normal.Changed {
					t.Fatalf("ordinary init lost pending work: %#v %v", normal, err)
				}
			} else if err == nil && !passive.Changed {
				t.Fatalf("%s admitted", scenario)
			}
			after, err := os.ReadFile(f.state + "/inventory.json")
			if err != nil {
				t.Fatal(err)
			}
			afterInfo, err := os.Stat(f.state + "/inventory.json")
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) || !os.SameFile(beforeInfo, afterInfo) || !sameInventoryMetadata(beforeInfo, afterInfo) {
				t.Fatal("passive observation changed protected pending/applied inventory")
			}
			if f.read()["applied_settings"].(map[string]any)["AGENT_claude_COMMAND"] != "old-command" {
				t.Fatal("pending command published")
			}
		})
	}
}

func sameInventoryMetadata(before, after os.FileInfo) bool {
	a, b := *before.Sys().(*syscall.Stat_t), *after.Sys().(*syscall.Stat_t)
	a.Atim, b.Atim = syscall.Timespec{}, syscall.Timespec{}
	return reflect.DeepEqual(a, b)
}

func TestInventoryDiagnosticsFollowCommittedStateAndTolerateNewerEvidence(t *testing.T) {
	f := newConfigInventoryFixture(t)
	entries := []map[string]any{{"id": "sample", "kind": "package", "digest": "installed"}}
	f.save(entries, []string{})
	request := map[string]any{"entries": entries}
	run := func(mode string) integrationObservation {
		t.Helper()
		payload, _ := json.Marshal(request)
		result, err := f.Exec(context.Background(), "", "", ports.InstanceExecRequest{Command: []string{"", "", "", "", mode}, Stdin: payload})
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("%s: %v", mode, err)
		}
		var observed integrationObservation
		if len(result.Stdout) != 0 {
			if err := json.Unmarshal(result.Stdout, &observed); err != nil {
				t.Fatal(err)
			}
		}
		return observed
	}
	request["expected"] = run("observe").Fingerprint
	request["applied_settings"] = map[string]string{"AGENT_sample_COMMAND": "before"}
	request["diagnostics"] = &IntegrationDiagnosticEvidence{Key: strings.Repeat("a", 64), Digests: map[string]string{"AGENT_sample_PROVISION": strings.Repeat("b", 64)}}
	run("capture-settings")
	delete(request, "expected")
	original := f.read()
	if run("selection").Diagnostics == nil {
		t.Fatal("fresh evidence missing")
	}
	for _, change := range []string{"settings", "entries", "newer", "malformed"} {
		t.Run(change, func(t *testing.T) {
			data, _ := json.Marshal(original)
			var inventory map[string]any
			_ = json.Unmarshal(data, &inventory)
			switch change {
			case "settings":
				inventory["applied_settings"] = map[string]string{"AGENT_sample_COMMAND": "older-writer"}
			case "entries":
				inventory["entries"].([]any)[0].(map[string]any)["digest"] = "older-writer"
			case "newer":
				inventory["diagnostics"].(map[string]any)["future_schema"] = true
			case "malformed":
				inventory["diagnostics"] = "untrusted"
			}
			data, _ = json.Marshal(inventory)
			testkit.WriteFile(t, f.state+"/inventory.json", data, 0600)
			if run("selection").Diagnostics != nil || run("settings").Diagnostics != nil {
				t.Fatal("stale or unsupported diagnostics accepted")
			}
			run("apply")
			run("commit")
			request["expected"] = run("observe").Fingerprint
			delete(request, "diagnostics") // An older writer cannot recertify retained evidence.
			run("capture-settings")
			delete(request, "expected")
			if run("selection").Diagnostics != nil {
				t.Fatal("old capture restored invalid evidence")
			}
		})
	}
}

func TestInitCaptureRecordsOriginalConfigSourceOnlyAfterConvergence(t *testing.T) {
	f := newConfigInventoryFixture(t)
	source := f.home + "/source"
	testkit.WriteFile(t, source, []byte("source bytes"), 0600)
	entry := configInventoryEntry(f.home, "file", "source bytes")
	f.save([]map[string]any{entry}, []string{})
	f.write([]byte("source bytes"), 0644)
	entries := []integrationArtifact{{ID: "claude", Kind: "file", Path: f.home + "/settings", Digest: entry["digest"].(string)}}
	sources := newDiagnosticSourceSnapshot(nil)
	runtime := Runtime{RepositoryRoot: "../../..", Integrations: []profile.IntegrationDefinition{}, Executor: f, SourceReader: sources.read,
		Environment: []string{"CODING_TOOL_INTEGRATIONS=claude", "AGENT_claude_CONFIG=" + source, "AGENT_claude_CONFIG_DEST=settings"}, Yard: domain.Context{DevUser: "dev", DevUID: os.Getuid()}}
	observed, err := runtime.integrationInventory(context.Background(), "observe", entries, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.integrationInventory(context.Background(), "capture-settings", entries, observed.Fingerprint, integrationInventoryCapture{verifySources: sources.verify})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := runtime.integrationInventory(context.Background(), "settings", nil, "")
	if err != nil || baseline.Diagnostics == nil {
		t.Fatalf("capture: %#v %v", baseline, err)
	}
	key, _ := hex.DecodeString(baseline.Diagnostics.Key)
	file := baseline.Diagnostics.Files["AGENT_claude_CONFIG"]
	if file.Path != source || file.Digest != config.ConfigurationInputDigest("AGENT_claude_CONFIG", config.SettingFile, key, []byte("source bytes")) {
		t.Fatalf("wrong original source: %#v", file)
	}
	before, err := os.ReadFile(f.state + "/inventory.json")
	if err != nil {
		t.Fatal(err)
	}
	observed, err = runtime.integrationInventory(context.Background(), "observe", entries, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.home + "/settings"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.integrationInventory(context.Background(), "capture-settings", entries, observed.Fingerprint, integrationInventoryCapture{baseline: baseline.Diagnostics, verifySources: sources.verify}); err == nil {
		t.Fatal("missing owned output accepted")
	}
	after, err := os.ReadFile(f.state + "/inventory.json")
	if err != nil || string(before) != string(after) {
		t.Fatal("failed capture changed evidence")
	}
}

func TestInitCaptureSkipsNonFileConfigEvidence(t *testing.T) {
	f := newConfigInventoryFixture(t)
	f.save([]map[string]any{}, []string{})
	root := testkit.TempDir(t)
	if err := os.MkdirAll(root+"/config/agents/fixture", 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, root+"/config/agents/fixture/integration.json", []byte(`{"schema_version":2,"settings":[{"name":"FIXTURE_INSTRUCTIONS","type":"absolute-path","optional":true,"scopes":["host","yard","command"],"application":"config-apply"}],"artifacts":[{"id":"instructions","kind":"instructions","source_setting":"FIXTURE_INSTRUCTIONS","destination":"instructions","format":"raw"}]}`), 0644)
	definitions, err := profile.LoadIntegrations(root)
	if err != nil {
		t.Fatal(err)
	}
	source := f.home + "/source"
	testkit.WriteFile(t, source, []byte("original source"), 0600)
	runtime := Runtime{RepositoryRoot: root, Integrations: definitions, Executor: f,
		Environment: []string{"CODING_TOOL_INTEGRATIONS=fixture", "FIXTURE_INSTRUCTIONS=" + source, "AGENT_fixture_PROVISION=" + source},
		Yard:        domain.Context{DevUser: "dev", DevUID: os.Getuid()}}
	observed, err := runtime.integrationInventory(context.Background(), "observe", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.integrationInventory(context.Background(), "capture-settings", nil, observed.Fingerprint); err != nil {
		t.Fatal(err)
	}
	baseline, err := runtime.integrationInventory(context.Background(), "settings", nil, "")
	if err != nil || baseline.Diagnostics == nil || len(baseline.Diagnostics.Files) != 0 || baseline.Diagnostics.Digests["AGENT_fixture_PROVISION"] == "" {
		t.Fatalf("non-file config input invalidated content evidence: %#v %v", baseline.Diagnostics, err)
	}
}
