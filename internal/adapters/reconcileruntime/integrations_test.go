package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestIntegrationInventoryPreservesDriftAndRetiresOnlyOwnedFiles(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	state := filepath.Join(root, "state")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	program := strings.ReplaceAll(integrationInventoryProgram, "@STATE_ROOT@", state)
	program = strings.ReplaceAll(program, "@STATE_UID@", fmt.Sprint(os.Getuid()))
	entries := []integrationArtifact{{ID: "codex", Kind: "file", Path: home + "/rules", Content: base64.StdEncoding.EncodeToString([]byte("owned")), Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("owned")))}}
	run := func(mode string, wantError bool) integrationObservation {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"home": home, "uid": os.Getuid(), "entries": entries})
		command := exec.Command("python3", "-B", "-c", program, mode)
		command.Stdin = strings.NewReader(string(payload))
		output, err := command.CombinedOutput()
		if (err != nil) != wantError {
			t.Fatalf("%s: %v: %s", mode, err, output)
		}
		var observation integrationObservation
		if mode == "observe" && !wantError {
			if err := json.Unmarshal(output, &observation); err != nil {
				t.Fatal(err)
			}
		}
		return observation
	}
	if !run("observe", false).Changed {
		t.Fatal("missing owned file converged")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("observation wrote inventory")
	}
	run("apply", false)
	if !run("observe", false).Changed {
		t.Fatal("fresh apply without commit lost pending reconciliation")
	}
	run("commit", false)
	if run("observe", false).Changed {
		t.Fatal("committed file did not converge")
	}
	run("apply", false)
	if !run("observe", false).Changed {
		t.Fatal("converged apply without commit lost pending reconciliation")
	}
	run("commit", false)
	entries = nil
	if err := os.Chmod(home+"/rules", 0o600); err != nil {
		t.Fatal(err)
	}
	run("observe", true)
	run("apply", true)
	if _, err := os.Stat(home + "/rules"); err != nil {
		t.Fatal("metadata-drifted file was not preserved", err)
	}
	if err := os.Chmod(home+"/rules", 0o644); err != nil {
		t.Fatal(err)
	}
	// Auth is outside the inventory, even when it shares the developer home.
	auth := home + "/auth.json"
	if err := os.WriteFile(auth, []byte("fixture credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home+"/rules", []byte("operator changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("observe", true)
	run("apply", true)
	got, _ := os.ReadFile(home + "/rules")
	if string(got) != "operator changed" {
		t.Fatal("modified file lost")
	}
	if err := os.WriteFile(home+"/rules", []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home+"/rules", 0o644); err != nil {
		t.Fatal(err)
	}
	run("apply", false)
	run("commit", false)
	if _, err := os.Stat(home + "/rules"); !os.IsNotExist(err) {
		t.Fatal("owned file was not retired")
	}
	got, _ = os.ReadFile(auth)
	if string(got) != "fixture credential" {
		t.Fatal("auth was altered")
	}
}

func TestIntegrationInventoryRejectsArtifactAppearingAfterObservation(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	state := filepath.Join(root, "state")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	program := strings.ReplaceAll(integrationInventoryProgram, "@STATE_ROOT@", state)
	program = strings.ReplaceAll(program, "@STATE_UID@", fmt.Sprint(os.Getuid()))
	path := home + "/rules"
	payload := []byte("selected")
	entry := integrationArtifact{ID: "codex", Kind: "file", Path: path, Content: base64.StdEncoding.EncodeToString(payload), Digest: fmt.Sprintf("%x", sha256.Sum256(payload))}
	run := func(mode, expected string) ([]byte, error) {
		request, _ := json.Marshal(map[string]any{
			"home": home, "uid": os.Getuid(), "entries": []integrationArtifact{entry},
			"adopt": true, "expected": expected,
		})
		command := exec.Command("python3", "-B", "-c", program, mode)
		command.Stdin = strings.NewReader(string(request))
		return command.CombinedOutput()
	}
	output, err := run("observe", "")
	if err != nil {
		t.Fatal(err, string(output))
	}
	var observed integrationObservation
	if err := json.Unmarshal(output, &observed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if output, err = run("apply", observed.Fingerprint); err == nil {
		t.Fatal("stale plan adopted an artifact that appeared after observation", string(output))
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("stale apply published integration ownership")
	}
}

type retryableIntegrationExecutor struct {
	pending        bool
	serviceChanged bool
	commits        int
	hookAttempts   int
	failFirstHook  bool
	hookReady      string
}

type preparedAdoptionExecutor struct {
	fingerprint       string
	configFingerprint string
	initial           bool
	configModes       []string
}

func (fixture *preparedAdoptionExecutor) Exec(_ context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	command := strings.Join(request.Command, "\x00")
	if len(request.Command) >= 5 && request.Command[0] == "python3" && strings.Contains(request.Command[3], "Version 1 records observed ownership only") && request.Command[4] == "observe" {
		observation := integrationObservation{Fingerprint: fixture.fingerprint, Changed: fixture.initial, Initial: fixture.initial}
		if fixture.initial {
			observation.Adopted = []integrationArtifact{{ID: "codex", Kind: "file", Path: "/home/dev/.codex/AGENTS.md"}}
		}
		payload, _ := json.Marshal(observation)
		return ports.InstanceExecResult{Stdout: payload}, nil
	}
	if len(request.Command) >= 5 && request.Command[0] == "python3" && strings.Contains(request.Command[3], "config materialization:") {
		fixture.configModes = append(fixture.configModes, request.Command[4])
		payload, _ := json.Marshal(configmaterial.Observation{Adoptable: true, Fingerprint: fixture.configFingerprint})
		return ports.InstanceExecResult{Stdout: payload}, nil
	}
	if strings.Contains(command, "mode=$1") && strings.Contains(command, "changed=0") {
		return ports.InstanceExecResult{Stdout: []byte("changed=0\n")}, nil
	}
	return ports.InstanceExecResult{}, nil
}

func TestPrepareLegacyIntegrationAdoptionSkipsUnavailableIncus(t *testing.T) {
	incus := &testkit.Incus{Err: os.ErrNotExist}
	runtime := Runtime{Incus: incus, Executor: incus}
	prepared, plan, err := runtime.PrepareLegacyIntegrationAdoption(context.Background())
	if err != nil || prepared.AdoptLegacyIntegrations || plan.Changed || plan.AdoptionFingerprint != "" {
		t.Fatalf("fresh host must not require or authorize legacy adoption: plan=%#v err=%v", plan, err)
	}
	if len(incus.ExecCalls) != 0 || len(incus.ConfigUpdates) != 0 || len(incus.PowerUpdates) != 0 {
		t.Fatal("unavailable Incus triggered guest access or mutations")
	}
}

func TestPrepareLegacyIntegrationAdoptionBindsInitialSnapshotOnly(t *testing.T) {
	root := t.TempDir()
	for path, content := range map[string]string{
		"config/projects-changed.sh": "#!/bin/sh\nexit 0\n",
		"codex-AGENTS.md":            "selected instructions\n",
		"codex-config.json":          "{\"selected\":true}\n",
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	executor := &preparedAdoptionExecutor{fingerprint: strings.Repeat("a", 64), configFingerprint: strings.Repeat("c", 64), initial: true}
	instance := ports.InstanceInfo{Status: "Running", Config: map[string]string{}, Devices: map[string]map[string]string{}}
	incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: instance}}
	runtime := Runtime{RepositoryRoot: root, Incus: incus, Executor: executor, Yard: domain.Context{
		IncusProject: "test", YardInstanceName: "yard", DevUser: "dev", DevUID: os.Getuid(),
	}, Environment: []string{
		"CODING_TOOL_INTEGRATIONS=codex", "HOST_CODEX_AGENTS_MD=" + filepath.Join(root, "codex-AGENTS.md"),
		"AGENT_codex_CONFIG=" + filepath.Join(root, "codex-config.json"), "AGENT_codex_CONFIG_DEST=.codex/config.json",
	}}
	prepared, plan, err := runtime.PrepareLegacyIntegrationAdoption(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.AdoptLegacyIntegrations || prepared.LegacyIntegrationFingerprint == "" ||
		plan.AdoptionFingerprint != prepared.LegacyIntegrationFingerprint ||
		len(plan.Adoption) != 2 || !slices.Contains(plan.Adoption, "/home/dev/.codex/AGENTS.md") ||
		!slices.Contains(plan.Adoption, "/home/dev/.codex/config.json") ||
		len(executor.configModes) == 0 || executor.configModes[0] != configmaterial.ModeAssessAdopt {
		t.Fatalf("prepared adoption: runtime=%#v plan=%#v", prepared, plan)
	}
	observed, err := prepared.IntegrationPlan(context.Background())
	if err != nil || !observed.Changed || !slices.Contains(observed.Adoption, "/home/dev/.codex/config.json") || observed.AdoptionFingerprint != plan.AdoptionFingerprint {
		t.Fatalf("nonconverged eligible config was omitted from initial adoption: %#v, %v", observed, err)
	}
	executor.configFingerprint = strings.Repeat("d", 64)
	if _, err := prepared.IntegrationPlan(context.Background()); err == nil {
		t.Fatal("changed structured adoption evidence was accepted")
	}
	executor.configFingerprint = strings.Repeat("c", 64)
	executor.fingerprint = strings.Repeat("b", 64)
	if _, err := prepared.IntegrationPlan(context.Background()); err == nil {
		t.Fatal("changed initial adoption snapshot was accepted")
	}
	executor.initial = false
	if _, err := prepared.IntegrationPlan(context.Background()); err != nil {
		t.Fatal("established retry was rejected by initial adoption fingerprint", err)
	}
}

func TestIntegrationScopeBindsDesiredArtifactsWithoutRuntimeProbes(t *testing.T) {
	root := t.TempDir()
	for path, content := range map[string]string{
		"config/projects-changed.sh": "#!/bin/sh\nexit 0\n",
		"instructions.md":            "first\n",
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runtime := Runtime{RepositoryRoot: root, Yard: domain.Context{DevUser: "dev", DevUID: 1001}, Environment: []string{
		"CODING_TOOL_INTEGRATIONS=codex", "HOST_CODEX_AGENTS_MD=" + filepath.Join(root, "instructions.md"),
	}}
	first, paths, err := runtime.IntegrationScope()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/home/dev/.codex/AGENTS.md", "/etc/subyard/agent-project-hooks", "/usr/local/libexec/subyard/projects-changed"} {
		if !slices.Contains(paths, path) {
			t.Fatalf("desired scope omitted %s: %#v", path, paths)
		}
	}
	if len(first) != 64 {
		t.Fatalf("invalid desired scope fingerprint %q", first)
	}
	if err := os.WriteFile(filepath.Join(root, "instructions.md"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, secondPaths, err := runtime.IntegrationScope()
	if err != nil {
		t.Fatal(err)
	}
	if second == first || !slices.Equal(paths, secondPaths) {
		t.Fatalf("desired source change was not bound: first=%s second=%s paths=%#v secondPaths=%#v", first, second, paths, secondPaths)
	}
}

func (fixture *retryableIntegrationExecutor) Exec(_ context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	command := strings.Join(request.Command, "\x00")
	if len(request.Command) >= 5 && request.Command[0] == "python3" && strings.Contains(request.Command[3], "Version 1 records observed ownership only") {
		switch request.Command[4] {
		case "observe":
			payload, _ := json.Marshal(integrationObservation{Fingerprint: strings.Repeat("a", 64), Changed: fixture.pending})
			return ports.InstanceExecResult{Stdout: payload}, nil
		case "apply":
			fixture.pending = true
		case "commit":
			fixture.pending = false
			fixture.commits++
		}
		return ports.InstanceExecResult{}, nil
	}
	if strings.Contains(command, "mode=$1") && strings.Contains(command, "changed=0") {
		changed := fixture.serviceChanged
		if len(request.Command) > 5 && request.Command[5] == "apply" {
			fixture.serviceChanged = false
		}
		if changed {
			return ports.InstanceExecResult{Stdout: []byte("changed=1\n")}, nil
		}
		return ports.InstanceExecResult{Stdout: []byte("changed=0\n")}, nil
	}
	if strings.Contains(command, "exec timeout --kill-after=5s 120s") {
		fixture.hookAttempts++
		if fixture.hookReady != "" {
			if _, err := os.Stat(fixture.hookReady); err != nil {
				return ports.InstanceExecResult{ExitCode: 1}, errors.New("Orca handler is not ready for integration hooks")
			}
		}
		if fixture.failFirstHook && fixture.hookAttempts == 1 {
			return ports.InstanceExecResult{ExitCode: 1}, errors.New("fixture hook failure")
		}
	}
	return ports.InstanceExecResult{}, nil
}

func TestIntegrationHookFailureLeavesInventoryPendingForRetry(t *testing.T) {
	root := t.TempDir()
	for path, content := range map[string]string{
		"config/projects-changed.sh":        "#!/bin/sh\nexit 0\n",
		"scripts/reconcile-integrations.sh": "#!/bin/sh\nexit 0\n",
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	executor := &retryableIntegrationExecutor{serviceChanged: true, failFirstHook: true}
	instance := ports.InstanceInfo{Status: "Running", Config: map[string]string{}, Devices: map[string]map[string]string{}}
	incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: instance}}
	runtime := Runtime{RepositoryRoot: root, Incus: incus, Executor: executor, Yard: domain.Context{
		IncusProject: "test", YardInstanceName: "yard", DevUser: "dev", DevUID: os.Getuid(),
	}}
	plan, err := runtime.IntegrationPlan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.ApplyIntegrations(context.Background(), plan); err == nil {
		t.Fatal("fixture hook failure was accepted")
	}
	if !executor.pending || executor.commits != 0 {
		t.Fatal("failed hooks committed integration inventory")
	}
	plan, err = runtime.IntegrationPlan(context.Background())
	if err != nil || !plan.Changed {
		t.Fatalf("failed hook was not retryable: changed=%v error=%v", plan.Changed, err)
	}
	if err := runtime.ApplyIntegrations(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if executor.pending || executor.commits != 1 || executor.hookAttempts != 2 {
		t.Fatalf("retry state: pending=%v commits=%d hook attempts=%d", executor.pending, executor.commits, executor.hookAttempts)
	}
}

func TestIntegrationInventoryAdoptsOnlyExactProtectedCoreArtifacts(t *testing.T) {
	for _, asset := range []struct {
		path string
		mode os.FileMode
	}{{"/usr/local/libexec/subyard/projects-changed", 0755}, {"/etc/subyard/agent-project-hooks", 0644}} {
		for _, mutation := range []string{"none", "content", "mode", "owner", "group", "symlink", "ancestor-symlink", "ancestor-writable", "agent-file", "agent-id"} {
			t.Run(filepath.Base(asset.path)+"/"+mutation, func(t *testing.T) {
				root := t.TempDir()
				home, state := root+"/home", root+"/state"
				path := root + asset.path
				for _, directory := range []string{home, filepath.Dir(path)} {
					if err := os.MkdirAll(directory, 0700); err != nil {
						t.Fatal(err)
					}
				}
				// The private fixture root stands in for /. Match root:root using
				// the current user's UID and its same-numbered supplementary group.
				if err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					return os.Chown(path, -1, os.Getuid())
				}); err != nil {
					t.Fatal(err)
				}
				program := strings.NewReplacer(
					"@STATE_ROOT@", state, "@STATE_UID@", fmt.Sprint(os.Getuid()),
					"/usr/local/libexec/subyard/projects-changed", root+"/usr/local/libexec/subyard/projects-changed",
					"/etc/subyard/agent-project-hooks", root+"/etc/subyard/agent-project-hooks",
					"while parent != '/':", "while parent != '"+root+"':",
				).Replace(integrationInventoryProgram)
				if mutation == "agent-file" {
					path = home + "/settings"
				}
				payload := []byte("known core payload\n")
				if err := os.WriteFile(path, payload, asset.mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, asset.mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chown(path, -1, os.Getuid()); err != nil {
					t.Fatal(err)
				}
				entry := integrationArtifact{ID: "_projects", Kind: "file", Path: path, Mode: int(asset.mode), Content: base64.StdEncoding.EncodeToString(payload), Digest: fmt.Sprintf("%x", sha256.Sum256(payload))}
				run := func(mode string, wantError bool) integrationObservation {
					t.Helper()
					request, _ := json.Marshal(map[string]any{"home": home, "uid": os.Getuid(), "entries": []integrationArtifact{entry}})
					command := exec.Command("python3", "-B", "-c", program, mode)
					command.Stdin = strings.NewReader(string(request))
					output, err := command.CombinedOutput()
					if (err != nil) != wantError {
						t.Fatalf("%s: %v %s", mode, err, output)
					}
					if wantDetail := map[string]string{
						"content": "unrecognized core content", "mode": "unsafe core metadata",
						"owner": "unsafe core metadata", "group": "unsafe core metadata",
						"ancestor-writable": "unsafe core ancestor",
					}[mutation]; wantError && wantDetail != "" {
						var conflict struct{ Detail string }
						if json.Unmarshal(output, &conflict) != nil || conflict.Detail != wantDetail {
							t.Fatalf("missing ownership detail: %s", output)
						}
					}
					var result integrationObservation
					if mode == "observe" && !wantError {
						if err := json.Unmarshal(output, &result); err != nil {
							t.Fatal(err)
						}
					}
					return result
				}
				if mutation != "agent-file" && !run("observe", false).Changed {
					t.Fatal("unrecorded core artifact did not require ownership publication")
				}
				switch mutation {
				case "content":
					if err := os.WriteFile(path, []byte("foreign payload"), asset.mode); err != nil {
						t.Fatal(err)
					}
				case "mode":
					if err := os.Chmod(path, 0600); err != nil {
						t.Fatal(err)
					}
				case "owner":
					program = strings.Replace(program, "OWNER = int('"+fmt.Sprint(os.Getuid())+"')", "OWNER = int('"+fmt.Sprint(os.Getuid()+1)+"')", 1)
				case "group":
					groups, err := os.Getgroups()
					if err != nil {
						t.Fatal(err)
					}
					other := -1
					for _, group := range groups {
						if group != os.Getuid() {
							other = group
							break
						}
					}
					if other == -1 {
						t.Skip("no supplementary group for group-mismatch fixture")
					}
					if err := os.Chown(path, -1, other); err != nil {
						t.Fatal(err)
					}
				case "symlink", "ancestor-symlink":
					link := path
					if mutation == "ancestor-symlink" {
						link = filepath.Dir(path)
					}
					if err := os.Rename(link, link+"-target"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(link+"-target", link); err != nil {
						t.Fatal(err)
					}
				case "ancestor-writable":
					if err := os.Chmod(filepath.Dir(path), 0770); err != nil {
						t.Fatal(err)
					}
				case "agent-id", "agent-file":
					entry.ID = "claude"
				}
				before, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				run("observe", mutation != "none")
				if _, err := os.Stat(state); !os.IsNotExist(err) {
					t.Fatal("observation published evidence")
				}
				run("apply", mutation != "none")
				if mutation == "none" {
					run("apply", false) // Resume after ownership publication, before commit.
					run("commit", false)
					if run("observe", false).Changed {
						t.Fatal("adopted core artifact did not converge")
					}
				} else if _, err := os.Stat(state); !os.IsNotExist(err) {
					t.Fatal("rejected adoption published evidence")
				}
				after, err := os.Lstat(path)
				if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode() {
					t.Fatal("adoption changed the existing artifact")
				}
			})
		}
	}
}

func TestIntegrationInventoryAdoptsReleasedDispatcherPredecessor(t *testing.T) {
	for _, version := range []string{"v0.3", "shared-hooks"} {
		t.Run(version, func(t *testing.T) {
			root := t.TempDir()
			home, state := root+"/home", root+"/state"
			path := root + "/usr/local/libexec/subyard/projects-changed"
			for _, directory := range []string{home, filepath.Dir(path)} {
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				return os.Chown(path, -1, os.Getuid())
			}); err != nil {
				t.Fatal(err)
			}
			desired, err := os.ReadFile("../../../config/projects-changed.sh")
			if err != nil {
				t.Fatal(err)
			}
			predecessor := []byte(strings.Replace(string(desired),
				"# Shared resources own hooks in projects-changed.d; selected agent hooks live in the list.\n", "", 1))
			wantDigest := "cefded0322e335042ff9a0e74f2cba187fb1a2a6aa8a2ffbcdf249ecc8e12588"
			if version == "v0.3" {
				// Exact installer heredoc shipped in v0.3.0 through v0.5.2.
				predecessor, err = os.ReadFile("testdata/projects-changed-v0.3.sh")
				if err != nil {
					t.Fatal(err)
				}
				wantDigest = "b632dd04a13ba11abff0b7502785ae09d8a6119c0c1261f338861796de82df35"
			}
			if got := fmt.Sprintf("%x", sha256.Sum256(predecessor)); got != wantDigest {
				t.Fatalf("unexpected predecessor fixture digest %s", got)
			}
			if err := os.WriteFile(path, predecessor, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(path, -1, os.Getuid()); err != nil {
				t.Fatal(err)
			}
			program := strings.NewReplacer(
				"@STATE_ROOT@", state, "@STATE_UID@", fmt.Sprint(os.Getuid()),
				"/usr/local/libexec/subyard/projects-changed", path,
				"/etc/subyard/agent-project-hooks", root+"/etc/subyard/agent-project-hooks",
				"while parent != '/':", "while parent != '"+root+"':",
			).Replace(integrationInventoryProgram)
			entry := integrationArtifact{ID: "_projects", Kind: "file", Path: path, Mode: 0755,
				Content: base64.StdEncoding.EncodeToString(desired), Digest: fmt.Sprintf("%x", sha256.Sum256(desired))}
			run := func(program, mode string, wantError bool) integrationObservation {
				t.Helper()
				request, _ := json.Marshal(map[string]any{"home": home, "uid": os.Getuid(), "entries": []integrationArtifact{entry}})
				command := exec.Command("python3", "-B", "-c", program, mode)
				command.Stdin = strings.NewReader(string(request))
				output, err := command.CombinedOutput()
				if (err != nil) != wantError {
					t.Fatalf("%s: %v %s", mode, err, output)
				}
				var result integrationObservation
				if mode == "observe" && !wantError {
					if err := json.Unmarshal(output, &result); err != nil {
						t.Fatal(err)
					}
				}
				return result
			}
			if !run(program, "observe", false).Changed {
				t.Fatal("released predecessor did not require replacement")
			}
			if err := os.Chmod(path, 0777); err != nil {
				t.Fatal(err)
			}
			run(program, "observe", true)
			run(program, "apply", true)
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Fatal("unsafe predecessor published ownership")
			}
			if err := os.Chmod(path, 0755); err != nil {
				t.Fatal(err)
			}
			interrupted := strings.Replace(program,
				"atomic(destination, payload, entry.get('mode', 0o644), owner)",
				"raise ValueError('fixture interruption')", 1)
			run(interrupted, "apply", true)
			if !run(program, "observe", false).Changed {
				t.Fatal("interrupted predecessor replacement lost pending ownership")
			}
			run(program, "apply", false)
			run(program, "commit", false)
			if run(program, "observe", false).Changed {
				t.Fatal("released predecessor replacement did not converge")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != string(desired) {
				t.Fatalf("dispatcher replacement: %v", err)
			}
		})
	}
}

func TestIntegrationInventoryAdoptsExactSelectedArtifactsOnlyInitially(t *testing.T) {
	developer, gid := inventoryDeveloper(t)
	uid := os.Getuid()
	root := t.TempDir()
	home, state, target := root+"/home", root+"/state", root+"/sessions"
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	filePath, linkPath := home+"/rules", home+"/session-link"
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte("selected rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(filePath, -1, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(linkPath, -1, gid); err != nil {
		t.Fatal(err)
	}
	entries := []integrationArtifact{
		{ID: "codex", Kind: "file", Path: filePath, Content: base64.StdEncoding.EncodeToString([]byte("selected rules")), Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("selected rules")))},
		{ID: "codex", Kind: "link", Path: linkPath, Target: target, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(target)))},
	}
	program := strings.ReplaceAll(strings.ReplaceAll(integrationInventoryProgram, "@STATE_ROOT@", state), "@STATE_UID@", fmt.Sprint(os.Getuid()))
	run := func(mode string, adopt, wantError bool) integrationObservation {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"home": home, "uid": uid, "developer": developer, "entries": entries, "adopt": adopt})
		command := exec.Command("python3", "-B", "-c", program, mode)
		command.Stdin = strings.NewReader(string(payload))
		output, err := command.CombinedOutput()
		if (err != nil) != wantError {
			t.Fatalf("%s adopt=%v: %v %s", mode, adopt, err, output)
		}
		var result integrationObservation
		if mode == "observe" && !wantError {
			if err := json.Unmarshal(output, &result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	run("observe", false, true)
	uid++
	run("observe", true, true)
	uid--
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	foreignGID := -1
	for _, group := range groups {
		if group != gid {
			foreignGID = group
			break
		}
	}
	if foreignGID >= 0 {
		if err := os.Lchown(linkPath, -1, foreignGID); err != nil {
			t.Fatal(err)
		}
		run("observe", true, true)
		if err := os.Lchown(linkPath, -1, gid); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("declined adoption published inventory")
	}
	observed := run("observe", true, false)
	if !observed.Initial || len(observed.Adopted) != 2 || !observed.Changed {
		t.Fatalf("adoption observation: %#v", observed)
	}
	fileBefore, _ := os.Lstat(filePath)
	linkBefore, _ := os.Lstat(linkPath)
	run("apply", true, false)
	fileAfter, _ := os.Lstat(filePath)
	linkAfter, _ := os.Lstat(linkPath)
	if !os.SameFile(fileBefore, fileAfter) || fileBefore.Sys() == nil || fileAfter.Sys() == nil ||
		fileBefore.Sys().(*syscall.Stat_t).Ino != fileAfter.Sys().(*syscall.Stat_t).Ino ||
		linkBefore.Sys().(*syscall.Stat_t).Ino != linkAfter.Sys().(*syscall.Stat_t).Ino {
		t.Fatal("adoption replaced an unchanged selected artifact")
	}
	observed = run("observe", true, false)
	if observed.Initial || len(observed.Adopted) != 0 || !observed.Changed {
		t.Fatalf("interrupted adoption evidence: %#v", observed)
	}
	run("apply", true, false)
	run("commit", true, false)
	if run("observe", true, false).Changed {
		t.Fatal("committed adoption did not converge")
	}
	if foreignGID >= 0 {
		if err := os.Lchown(linkPath, -1, foreignGID); err != nil {
			t.Fatal(err)
		}
		selected := entries
		entries = entries[:1]
		run("observe", false, true)
		run("apply", false, true)
		entries = selected
		if !run("observe", false, false).Changed {
			t.Fatal("same-target link with foreign group was reported ready")
		}
		run("apply", false, false)
		run("commit", false, false)
		if run("observe", false, false).Changed {
			t.Fatal("selected link metadata was not repaired")
		}
	} else {
		t.Log("no alternate permitted group available for foreign-group filesystem checks")
	}
	extra := home + "/new-rules"
	if err := os.WriteFile(extra, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries = append(entries, integrationArtifact{ID: "claude", Kind: "file", Path: extra,
		Content: base64.StdEncoding.EncodeToString([]byte("new")), Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("new")))})
	run("observe", true, true)
	if got, err := os.ReadFile(extra); err != nil || string(got) != "new" {
		t.Fatal("post-initial foreign artifact changed")
	}
}

func TestIntegrationInventoryAdoptionRejectsDriftAndStaleAssessment(t *testing.T) {
	developer, gid := inventoryDeveloper(t)
	for _, scenario := range []string{"content", "mode", "link-target", "stale-mode"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			home, state, target := root+"/home", root+"/state", root+"/sessions"
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
			path := home + "/artifact"
			if err := os.Mkdir(home, 0o700); err != nil {
				t.Fatal(err)
			}
			entry := integrationArtifact{ID: "codex", Kind: "file", Path: path,
				Content: base64.StdEncoding.EncodeToString([]byte("desired")), Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("desired")))}
			if scenario == "link-target" {
				entry.Kind, entry.Target = "link", target
				entry.Digest = fmt.Sprintf("%x", sha256.Sum256([]byte(target)))
				if err := os.Symlink(root+"/other", path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte("desired"), 0o644); err != nil {
				t.Fatal(err)
			}
			if scenario == "link-target" {
				if err := os.Lchown(path, -1, gid); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Chown(path, -1, os.Getuid()); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "content":
				if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			program := strings.ReplaceAll(strings.ReplaceAll(integrationInventoryProgram, "@STATE_ROOT@", state), "@STATE_UID@", fmt.Sprint(os.Getuid()))
			run := func(mode string) error {
				payload, _ := json.Marshal(map[string]any{"home": home, "uid": os.Getuid(), "developer": developer, "entries": []integrationArtifact{entry}, "adopt": true})
				command := exec.Command("python3", "-B", "-c", program, mode)
				command.Stdin = strings.NewReader(string(payload))
				return command.Run()
			}
			if scenario == "stale-mode" {
				if err := run("observe"); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := run("apply"); err == nil {
				t.Fatal("unsafe legacy adoption succeeded")
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Fatal("rejected adoption published inventory")
			}
		})
	}
}

func TestIntegrationInventoryRejectsForeignSymlinksAndResumesReplacement(t *testing.T) {
	root := t.TempDir()
	home := root + "/home"
	state := root + "/state"
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	program := strings.ReplaceAll(strings.ReplaceAll(integrationInventoryProgram, "@STATE_ROOT@", state), "@STATE_UID@", fmt.Sprint(os.Getuid()))
	entry := integrationArtifact{ID: "codex", Kind: "file", Path: home + "/rules"}
	setContent := func(value string) {
		entry.Content = base64.StdEncoding.EncodeToString([]byte(value))
		entry.Digest = fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
	}
	setContent("first")
	run := func(mode string) error {
		payload, _ := json.Marshal(map[string]any{"home": home, "uid": os.Getuid(), "entries": []integrationArtifact{entry}})
		command := exec.Command("python3", "-B", "-c", program, mode)
		command.Stdin = strings.NewReader(string(payload))
		return command.Run()
	}
	if err := os.Symlink(home+"/foreign", entry.Path); err != nil {
		t.Fatal(err)
	}
	if run("observe") == nil {
		t.Fatal("foreign symlink accepted")
	}
	if err := os.Remove(entry.Path); err != nil {
		t.Fatal(err)
	}
	if err := run("apply"); err != nil {
		t.Fatal(err)
	}
	if err := run("commit"); err != nil {
		t.Fatal(err)
	}
	setContent("replacement")
	if err := run("apply"); err != nil {
		t.Fatal(err)
	}
	// Simulate failure after file rename and before inventory commit.
	if err := run("observe"); err != nil {
		t.Fatal("interrupted replacement cannot resume", err)
	}
	if err := run("apply"); err != nil {
		t.Fatal(err)
	}
	if err := run("commit"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(entry.Path)
	if string(got) != "replacement" {
		t.Fatal("replacement missing")
	}
	if err := os.Remove(entry.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(home+"/foreign", entry.Path); err != nil {
		t.Fatal(err)
	}
	if run("apply") == nil {
		t.Fatal("substituted owned symlink accepted")
	}
}

func TestIntegrationInventoryPreservesUnmanagedPathsCreatedAfterInitialization(t *testing.T) {
	root := t.TempDir()
	home, state := root+"/home", root+"/state"
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := home + "/settings.json"
	program := strings.ReplaceAll(strings.ReplaceAll(integrationInventoryProgram, "@STATE_ROOT@", state), "@STATE_UID@", fmt.Sprint(os.Getuid()))
	var entries []integrationArtifact
	run := func(mode string, wantError bool) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"home": home, "uid": os.Getuid(), "entries": entries, "legacy": []string{path}})
		command := exec.Command("python3", "-B", "-c", program, mode)
		command.Stdin = strings.NewReader(string(payload))
		output, err := command.CombinedOutput()
		if (err != nil) != wantError {
			t.Fatalf("%s: %v %s", mode, err, output)
		}
	}
	if err := os.WriteFile(path, []byte("runtime-owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("observe", true)
	run("apply", true)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	run("apply", false)
	run("commit", false)
	if err := os.WriteFile(path, []byte("runtime-owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("observe", false)
	run("apply", false)
	run("commit", false)
	entries = []integrationArtifact{{ID: "claude", Kind: "file", Path: path, Content: base64.StdEncoding.EncodeToString([]byte("desired")), Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("desired")))}}
	run("observe", true)
	run("apply", true)
	payload, err := os.ReadFile(path)
	if err != nil || string(payload) != "runtime-owned" {
		t.Fatalf("unmanaged artifact was changed: %v", err)
	}
}

func TestIntegrationMutationNeverStartsStoppedOrMissingYard(t *testing.T) {
	for _, status := range []string{"Stopped", "missing"} {
		t.Run(status, func(t *testing.T) {
			incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: status != "missing", Instance: ports.InstanceInfo{Status: status}}}
			runtime := Runtime{Incus: incus, Executor: incus, Yard: domain.Context{IncusProject: "test", YardInstanceName: "yard"}}
			if _, err := runtime.IntegrationPlan(context.Background()); err == nil {
				t.Fatal("unavailable yard accepted")
			}
			if err := runtime.ApplyIntegrations(context.Background(), IntegrationPlan{}); err == nil {
				t.Fatal("apply accepted unavailable yard")
			}
			if len(incus.ExecCalls) != 0 || len(incus.ConfigUpdates) != 0 {
				t.Fatal("stopped precondition mutated runtime")
			}
		})
	}
}

func TestIntegrationSessionLinkRetirementPreservesTargetAndCredentialHome(t *testing.T) {
	developer, gid := inventoryDeveloper(t)
	root := t.TempDir()
	home := root + "/home"
	target := root + "/sessions"
	state := root + "/state"
	for _, directory := range []string{home + "/.codex", target} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{home + "/.codex/auth.json", target + "/history"} {
		if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entry := integrationArtifact{ID: "codex", Kind: "link", Path: home + "/.codex/sessions", Target: target, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(target)))}
	entries := []integrationArtifact{entry}
	program := strings.ReplaceAll(strings.ReplaceAll(integrationInventoryProgram, "@STATE_ROOT@", state), "@STATE_UID@", fmt.Sprint(os.Getuid()))
	run := func(mode string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"home": home, "uid": os.Getuid(), "developer": developer, "entries": entries})
		command := exec.Command("python3", "-B", "-c", program, mode)
		command.Stdin = strings.NewReader(string(payload))
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", mode, err, output)
		}
	}
	run("apply")
	run("commit")
	run("observe")
	if got, err := os.Readlink(entry.Path); err != nil || got != target {
		t.Fatal("session link absent", err)
	}
	info, err := os.Lstat(entry.Path)
	if err != nil || info.Sys().(*syscall.Stat_t).Gid != uint32(gid) {
		t.Fatalf("published link did not use the developer's named group: %v", err)
	}
	entries = nil
	run("observe")
	run("apply")
	run("commit")
	for _, path := range []string{home + "/.codex/auth.json", target + "/history"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "keep" {
			t.Fatalf("retirement altered persistent data %s", path)
		}
	}
	if _, err := os.Lstat(entry.Path); !os.IsNotExist(err) {
		t.Fatal("session link not retired")
	}
}

func TestIntegrationSessionLinkPublicationPreservesConcurrentDestination(t *testing.T) {
	developer, _ := inventoryDeveloper(t)
	root := t.TempDir()
	home := root + "/home"
	target := root + "/sessions"
	for _, directory := range []string{home, target} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	entry := integrationArtifact{ID: "codex", Kind: "link", Path: home + "/sessions", Target: target, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(target)))}
	foreign := root + "/foreign"
	program := strings.ReplaceAll(strings.ReplaceAll(integrationInventoryProgram, "@STATE_ROOT@", root+"/state"), "@STATE_UID@", fmt.Sprint(os.Getuid()))
	// Model another writer creating the destination after our temporary link exists.
	program = fmt.Sprintf(`import os
original_symlink = os.symlink
def concurrent_symlink(src, dst, **kwargs):
    original_symlink(src, dst, **kwargs)
    if dst.startswith('.integration-'):
        original_symlink(%q, %q)
os.symlink = concurrent_symlink
`, foreign, entry.Path) + program
	payload, err := json.Marshal(map[string]any{"home": home, "uid": os.Getuid(), "developer": developer, "entries": []integrationArtifact{entry}})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("python3", "-B", "-c", program, "apply")
	command.Stdin = strings.NewReader(string(payload))
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("publication overwrote a concurrent destination: %s", output)
	}
	if got, err := os.Readlink(entry.Path); err != nil || got != foreign {
		t.Fatalf("concurrent destination was not preserved: target=%q error=%v", got, err)
	}
	if temporary, err := filepath.Glob(home + "/.integration-*"); err != nil || len(temporary) != 0 {
		t.Fatalf("temporary publication link remains: %v %v", temporary, err)
	}
}

func inventoryDeveloper(t *testing.T) (string, int) {
	t.Helper()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroup(account.Username)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		t.Fatal(err)
	}
	return account.Username, gid
}

func TestIntegrationRetirementRemembersReleasedStructuredDocuments(t *testing.T) {
	root := t.TempDir()
	home := root + "/home"
	state := root + "/state"
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := home + "/settings.json"
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries := []integrationArtifact{{ID: "claude", Kind: "structured", Format: "json", Path: path, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("{}")))}}
	program := strings.ReplaceAll(strings.ReplaceAll(integrationInventoryProgram, "@STATE_ROOT@", state), "@STATE_UID@", fmt.Sprint(os.Getuid()))
	run := func(mode string) integrationObservation {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"home": home, "uid": os.Getuid(), "entries": entries, "legacy": []string{path}})
		command := exec.Command("python3", "-B", "-c", program, mode)
		command.Stdin = strings.NewReader(string(payload))
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", mode, err, output)
		}
		var observed integrationObservation
		if mode == "observe" {
			if err := json.Unmarshal(output, &observed); err != nil {
				t.Fatal(err)
			}
		}
		return observed
	}
	run("apply")
	run("commit")
	entries = nil
	observed := run("observe")
	if len(observed.Retired) != 1 || observed.Retired[0].Kind != "structured" || observed.Retired[0].Path != path {
		t.Fatalf("structured retirement missing from observation: %#v", observed.Retired)
	}
	run("apply")
	run("commit")
	if run("observe").Changed {
		t.Fatal("preserved retired document prevents convergence")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("structured document deleted", err)
	}
}
