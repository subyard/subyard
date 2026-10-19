package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Source-repair callers execute the selected Go or experimental Rust tooling while their installed data may
// still belong to a historical Go owner. Copy the complete normal build once
// supplied by the owning gate; never add Rust declarations to the Go data.
func sourceAdmissionToolingFixture(t *testing.T, runtimeRoot string) string {
	t.Helper()
	stage := os.Getenv("SUBYARD_RUST_SOURCE_QUERY_TOOLING_ROOT")
	if stage == "" {
		stage = filepath.Join(repositoryRoot(t), ".build/runtime/current")
	}
	stage, err := filepath.EvalSymlinks(stage)
	if err != nil {
		t.Fatal("source admission requires a normal runtime: run make build or supply SUBYARD_RUST_SOURCE_QUERY_TOOLING_ROOT", err)
	}
	for _, required := range []string{"runtime-files.sha256", "bin/yard-engine"} {
		if _, err := os.Stat(filepath.Join(stage, required)); err != nil {
			t.Fatal("normal source tooling is incomplete", err)
		}
	}
	root := filepath.Join(runtimeRoot, "releases", filepath.Base(stage))
	if err := filepath.WalkDir(stage, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(stage, path)
		if err != nil {
			return err
		}
		target := filepath.Join(root, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("normal tooling contains a nonregular asset: %s", relative)
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		testkit.WriteFile(t, target, payload, info.Mode().Perm())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return root
}

// Recompute the exact inventory after synthetic assets are finalized. This
// never changes an image or supplies history/recovery authority.
func sourceFixtureManifest(t *testing.T, root string) []byte {
	t.Helper()
	var entries []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "runtime-files.sha256" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, fmt.Sprintf("%x  ./%s", sha256.Sum256(raw), filepath.ToSlash(relative)))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(entries)
	manifest := []byte(strings.Join(entries, "\n") + "\n")
	writeProfileTestFile(t, filepath.Join(root, "runtime-files.sha256"), manifest, 0644)
	return manifest
}

// This forms selected-owner history through the actual normal private facade.
// Go stages produce Go history; explicit Rust stages must publish a retained
// Rust bundle as well. Only the seed ledger uses the canonical Go writer. The owning
// gate supplies a normal stage with an actual source-compatible version; the
// helper version and release IDs are never relabeled to make a fixture pass.
func completedRustSourceAuthoringFixture(t *testing.T, migrateSSH bool, extraAssets map[string]string) (*CLI, string, string) {
	t.Helper()
	program, ready, home, _ := completedRustSourceAuthoringRuntimeFixture(t, migrateSSH, extraAssets)
	return program, ready, home
}

func completedRustSourceAuthoringRuntimeFixture(t *testing.T, migrateSSH bool, extraAssets map[string]string, directions ...releasetransition.Direction) (*CLI, string, string, *testkit.IncusServer) {
	t.Helper()
	original, environment, _ := nativeFixture(t)
	detached, err := os.MkdirTemp("/tmp", "subyard-current-source-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(detached); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(original, detached); err != nil {
		t.Fatal(err)
	}
	for i := range environment {
		environment[i] = strings.ReplaceAll(environment[i], original, detached)
	}
	oldRoot := original
	original = detached
	t.Cleanup(func() {
		if t.Failed() {
			t.Log("retained actual source constructor:", original)
		} else {
			_ = os.RemoveAll(original)
		}
	})
	home := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	data := environmentValue(environment, "SUBYARD_HOME")
	runtimeRoot := filepath.Join(original, "runtime-source-authoring")
	for _, path := range []string{home, data, environmentValue(environment, "HOME")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	tooling := sourceAdmissionToolingFixture(t, runtimeRoot)
	versionBytes, err := exec.Command(filepath.Join(tooling, "bin/yard-engine"), "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	versionFields := strings.Fields(string(versionBytes))
	if len(versionFields) != 2 {
		t.Fatal("normal helper has invalid version")
	}
	targetID := versionFields[1] + "-aaaaaaaaaaaa"
	root := filepath.Join(runtimeRoot, "releases", targetID)
	if err := os.Rename(tooling, root); err != nil {
		t.Fatal(err)
	}
	writeProfileTestFile(t, filepath.Join(original, "repo.rules"), []byte("allow_rule()\n"), 0600)
	agents := fmt.Sprintf("CODING_TOOL_INTEGRATIONS=codex\nAGENT_codex_COMMAND=codex\nAGENT_codex_RULES=%s\nAGENT_codex_RULES_DEST=.codex/rules/repo.rules\nAGENT_claude_COMMAND=synthetic-cli\nAGENT_claude_PROVISION=${SUBYARD_CONFIG_DIR}/synthetic-integration-provision.sh\n", filepath.Join(original, "repo.rules"))
	writeProfileTestFile(t, filepath.Join(root, "config/agents.env"), []byte(agents), 0600)
	writeProfileTestFile(t, filepath.Join(root, "config/subyard.env"), []byte(strings.ReplaceAll(string(mustReadConfigFixture(t, filepath.Join(original, "config/subyard.env"))), oldRoot, original)+"ENVIRONMENT_PROFILES=''\n"), 0600)
	// Fresh release assessments deliberately discard inherited setting overrides.
	// Keep the physical host-absence inputs in the same persistent source too.
	hostPath := filepath.Join(root, "config/host.env")
	hostSettings := string(mustReadConfigFixture(t, hostPath)) + "\nSUBYARD_POWER_RECONCILER_PATH=" + filepath.Join(original, "absent-power") + "\nSUBYARD_POWER_UNIT_PATH=" + filepath.Join(original, "absent-unit") + "\n"
	writeProfileTestFile(t, hostPath, []byte(hostSettings), 0600)
	for _, relative := range []string{"config/synthetic-integration-provision.sh", "config/projects-changed.sh", "scripts/reconcile-integrations.sh"} {
		writeProfileTestFile(t, filepath.Join(root, relative), []byte("#!/bin/sh\nexit 99\n"), 0700)
	}
	assets := activationProfileRepairAssets()
	for path, value := range extraAssets {
		assets[path] = value
	}
	for relative, value := range assets {
		mode := os.FileMode(0600)
		if strings.HasSuffix(relative, ".sh") {
			mode = 0700
		}
		writeProfileTestFile(t, filepath.Join(root, relative), []byte(value), mode)
	}
	manifest := sourceFixtureManifest(t, root)
	// The source has the same normal image/assets, under its own retained root.
	sourceID := versionFields[1] + "-source"
	source := filepath.Join(runtimeRoot, "releases", sourceID)
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		target := filepath.Join(source, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		writeProfileTestFile(t, target, mustReadConfigFixture(t, path), info.Mode().Perm())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/"+sourceID, filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	direction := releasetransition.DirectionActivateTarget
	if len(directions) != 0 {
		direction = directions[0]
	}
	if direction == releasetransition.DirectionActivatePrevious {
		// Ordinary init repair requires an already identified existing host.
		writeProfileTestFile(t, filepath.Join(home, "host-id"), []byte("source-authoring-owner\n"), 0600)
		if err := os.Symlink("releases/"+targetID, filepath.Join(runtimeRoot, "previous")); err != nil {
			t.Fatal(err)
		}
	}
	rawRegistry := mustReadConfigFixture(t, filepath.Join(root, "config/release-transition.json"))
	registry, registryDigest, err := releasetransition.ParseRegistryV2(rawRegistry, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	ledger := releasetransition.BaselineLedgerV2(registry)
	for _, migration := range registry.Migrations {
		if migrateSSH && migration.Kind == "local-ssh-port-settings-v2-to-v3" {
			break
		}
		ledger, err = ledger.Advance(registry, migration)
		if err != nil {
			t.Fatal(err)
		}
	}
	payload, _, err := releasetransition.MarshalLedgerV2(ledger, registry)
	if err != nil {
		t.Fatal(err)
	}
	writeProfileTestFile(t, filepath.Join(home, "release-transition/v2/ledger.json"), payload, 0600)
	store, err := releasetransition.NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.ReadMigrationLedger(registry, registryDigest)
	if err != nil {
		t.Fatal("canonical seed ledger", err)
	}
	if err := store.ConvertMigrationLedger(state, registry, registryDigest, registry, registryDigest); err != nil {
		t.Fatal("canonical seed checkpoint", err)
	}
	if migrateSSH {
		writeProfileTestFile(t, filepath.Join(home, config.GitSettingsRelativePath, "config.env"), []byte("SSH_PORT=2240\n"), 0600)
		writeProfileTestFile(t, filepath.Join(home, config.GitSettingsRelativePath, "yards/named/config.env"), []byte("SSH_PORT=2241\n"), 0600)
	}
	server, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	server.SetExtensions("projects", "instances", "instance_all_projects", "storage", "network", "container_exec_user_group_cwd")
	// Only the physical profile read boundary is canned: no installed guest
	// clients or user services exist, and every mutation command refuses.
	fixtureBin := filepath.Join(original, "fixture-bin")
	writeProfileTestFile(t, filepath.Join(fixtureBin, "incus"), []byte(fmt.Sprintf(`#!/bin/sh
if [ -e %q ]; then case " $* " in *subyard-git-fetch*) cat >/dev/null; exit 1 ;; esac; fi
case "$1" in
 list) case " $* " in *" --all-projects "*) printf '[]\n' ;; *) printf 'RUNNING\n' ;; esac ;;
 query) case "$2" in *recursion=1*) printf '[]\n' ;; /1.0/instances/yard*) printf '%%s\n' '{"config":{"volatile.uuid":"source-authoring-default","user.subyard.managed":"true"},"devices":{}}' ;; *) exit 64 ;; esac ;;
 config) case "$2:$3" in get:*|device:list) ;; *) exit 64 ;; esac ;;
 exec) case " $* " in *" -- getent passwd "*|*" --check-removed "*|*subyard/github-client*) cat >/dev/null ;; *ai-observer-retirement*|*" docker container ls "*) cat >/dev/null ;; *" systemctl is-active "*) cat >/dev/null; exit 3 ;; *" test "*) cat >/dev/null; exit 1 ;; *" observe "*|*" bash -se -- "*) cat >/dev/null; printf '%%s\n' '{"state":"absent","actual":"","desired":""}' ;; *) exit 64 ;; esac ;;
 *) exit 64 ;;
esac
`, filepath.Join(original, "physical-profile-unavailable"))), 0700)
	writeProfileTestFile(t, filepath.Join(fixtureBin, "systemctl"), []byte(`#!/bin/sh
case " $* " in *" --user show subyard-github-"*) printf 'LoadState=not-found\nActiveState=inactive\nMainPID=0\nUnitFileState=\n' ;; *) exit 64 ;; esac
`), 0700)
	processEnvironment := []string{"HOME=" + environmentValue(environment, "HOME"), "PATH=" + fixtureBin + ":/usr/bin:/bin", "SUBYARD_OPERATOR_HOME=" + environmentValue(environment, "HOME"), "SUBYARD_CONFIG_HOME=" + home, "SUBYARD_HOME=" + data, "YARD_RUNTIME_ROOT=" + runtimeRoot, "SUBYARD_REPOSITORY_ROOT=" + root, "SUBYARD_CONFIG_DIR=" + filepath.Join(root, "config"), "SUBYARD_NO_AUDIT=1", "SUBYARD_INCUS_SOCKET=" + server.SocketPath, "SUBYARD_POWER_RECONCILER_PATH=" + filepath.Join(original, "absent-power"), "SUBYARD_POWER_UNIT_PATH=" + filepath.Join(original, "absent-unit")}
	invoke := func(request releasetransition.ProcessRequest, grant string) releasetransition.ProcessResponse {
		t.Helper()
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(context.Background(), filepath.Join(root, "bin/yard-engine"), "_release-transition")
		command.Env = append([]string{}, processEnvironment...)
		if grant != "" {
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer read.Close()
			if _, err = io.WriteString(write, grant+"\n"); err != nil {
				t.Fatal(err)
			}
			write.Close()
			command.ExtraFiles = []*os.File{read}
			command.Env = append(command.Env, "SUBYARD_RELEASE_TRANSITION_GRANT_FD=3")
		}
		var stdout, stderr bytes.Buffer
		command.Stdin = bytes.NewReader(raw)
		command.Stdout = &stdout
		command.Stderr = &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("normal Rust source %s: %v: %s", request.Mode, err, stderr.String())
		}
		var response releasetransition.ProcessResponse
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	request := releasetransition.ProcessRequest{SchemaVersion: 1, Mode: releasetransition.ProcessInspect, RuntimeRoot: runtimeRoot, ConfigHome: home, Yard: "default", Target: releasetransition.ReleaseID(targetID), Direction: direction, ArtifactDigest: releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(manifest))), RegistryDigest: releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(rawRegistry)))}
	inspected := invoke(request, "")
	if inspected.Inspection == nil || len(inspected.Inspection.Blockers) != 0 {
		t.Fatalf("normal source inspection refused: %+v", inspected.Inspection)
	}
	request.Mode = releasetransition.ProcessConverge
	request.Execution = &releasetransition.Execution{Plan: inspected.Inspection.Plan, Authorization: "source-authoring-parent-grant"}
	outcome := invoke(request, "source-authoring-parent-grant")
	if outcome.Outcome == nil || !outcome.Outcome.ReachedGoal {
		t.Fatalf("normal source formation did not complete: %+v", outcome.Outcome)
	}
	journal, err := releasetransition.ParseJournal(mustReadConfigFixture(t, filepath.Join(home, "release-transition/v2/journal.json")))
	if err != nil || journal.Checkpoint != releasetransition.JournalComplete {
		t.Fatal("real completed journal absent", err)
	}
	if _, declared := os.Stat(filepath.Join(root, "config/release-owner.json")); declared == nil {
		if _, err := os.Stat(filepath.Join(home, "migrations/native-custody/v1", string(journal.Transaction), "bundle.json")); err != nil {
			t.Fatal("real Rust bundle absent", err)
		}
	}
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
	for _, name := range []string{"default", "named"} {
		project, instance := "subyard", "yard"
		if name != "default" {
			project, instance = "subyard-"+name, "yard-"+name
		}
		fake.Instances[project+"/"+instance] = ports.InstanceInfo{Project: project, Name: instance, Status: "Running"}
	}
	environment = append(environment, processEnvironment...)
	platform := &completedSourcePlatformFixture{initPlatformFixture: newInitPlatformFixture()}
	program, err := New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root, Incus: fake, Executor: fake, InitPlatform: platform})
	if err != nil {
		t.Fatal(err)
	}
	platform.runtimeIDs = program.profileRuntimeIDs()
	ready := filepath.Join(original, "config-apply-ready")
	server.SetProject("subyard", map[string]any{"name": "subyard", "config": map[string]string{}})
	server.SetInstance("subyard", "yard", map[string]any{"name": "yard", "type": "container", "status": "Running", "config": map[string]string{"volatile.uuid": "source-authoring-default", "user.subyard.managed": "true"}, "expanded_config": map[string]string{"volatile.uuid": "source-authoring-default", "user.subyard.managed": "true"}})
	server.SetExecResponder(func(call testkit.IncusServerExecCall) testkit.IncusServerExecStep {
		return completedSourceNativeObservation(program, ready, call)
	})
	return program, ready, home, server
}

func TestCompletedSourceAuthoringFixture(t *testing.T) {
	program, _, home := completedRustSourceAuthoringFixture(t, false, nil)
	reader := releaseruntime.NewSourceInspector(program.options.RepositoryRoot, io.Discard)
	defer reader.Close()
	proof, err := reader.InspectSourceAdmission(context.Background(), program.env["YARD_RUNTIME_ROOT"], home)
	if err != nil || proof.Metadata.Journal == nil || proof.Metadata.Journal.Checkpoint != releasetransition.JournalComplete {
		t.Fatal("real completed source admission", err)
	}
}

// The parent CLI component fixture follows the full normal catalog, while its
// native guest fixture observes every unselected profile installation absent.
type completedSourcePlatformFixture struct {
	*initPlatformFixture
	runtimeIDs []string
}

func (fixture *completedSourcePlatformFixture) ObserveProfileRuntimes(context.Context) (map[string]ports.RuntimeObservation, error) {
	values := make(map[string]ports.RuntimeObservation, len(fixture.runtimeIDs))
	for _, id := range fixture.runtimeIDs {
		values[id] = ports.RuntimeObservation{State: ports.RuntimeStateAbsent}
	}
	return values, nil
}
func sourceAuthoringPlatformFixture(t *testing.T, value ports.InitPlatform) *initPlatformFixture {
	t.Helper()
	switch fixture := value.(type) {
	case *completedSourcePlatformFixture:
		return fixture.initPlatformFixture
	case *initPlatformFixture:
		return fixture
	case *sampleRepairPlatformFixture:
		return fixture.initPlatformFixture
	default:
		t.Fatalf("unexpected source authoring platform %T", value)
		return nil
	}
}

func completedSourceNativeObservation(program *CLI, ready string, call testkit.IncusServerExecCall) testkit.IncusServerExecStep {
	if slices.Equal(call.Command, []string{"bash", "-s", "--", "--observe"}) {
		payload, _ := json.Marshal(map[string]any{"projects": fmt.Sprintf("%x", sha256.Sum256(nil)), "wiring": fmt.Sprintf("%x", sha256.Sum256(nil)), "hooks": []string{}, "roots": []string{}, "facts": map[string]string{}, "resolved": map[string]string{}})
		return testkit.IncusServerExecStep{Stdout: payload}
	}
	_, err := os.Stat(ready)
	current := err == nil
	if len(call.Command) == 10 && call.Command[0] == "python3" && call.Command[4] == "observe" {
		digest := call.Command[8]
		if !current {
			digest = strings.Repeat("0", 64)
		}
		payload, _ := json.Marshal(map[string]any{"converged": current, "fingerprint": digest})
		return testkit.IncusServerExecStep{Stdout: payload}
	}
	if len(call.Command) == 3 && call.Command[0] == "sha256sum" && call.Command[1] == "--" {
		digest := strings.Repeat("0", 64)
		if current {
			yard := strings.TrimPrefix(call.Project, "subyard-")
			if call.Project == "subyard" {
				yard = "default"
			}
			loaded, loadErr := program.loadSelectionRepair(yard)
			if loadErr != nil {
				return testkit.IncusServerExecStep{ExitCode: 1}
			}
			assets, assetErr := effectiveConfigAssets(loaded)
			if assetErr != nil {
				return testkit.IncusServerExecStep{ExitCode: 1}
			}
			found := false
			for _, asset := range assets {
				if asset.Destination == call.Command[2] {
					content, readErr := os.ReadFile(asset.Source)
					if readErr != nil {
						return testkit.IncusServerExecStep{ExitCode: 1}
					}
					digest = fmt.Sprintf("%x", sha256.Sum256(content))
					found = true
					break
				}
			}
			if !found {
				return testkit.IncusServerExecStep{ExitCode: 1}
			}
		}
		return testkit.IncusServerExecStep{Stdout: []byte(digest + "  " + call.Command[2] + "\n")}
	}
	physical := &selectionOnlyIntegrationExecutor{staleFiles: os.IsNotExist(err), serviceDrift: os.IsNotExist(err)}
	result, callErr := physical.Exec(context.Background(), call.Project, call.Name, ports.InstanceExecRequest{Command: call.Command, Stdin: call.Stdin})
	if callErr != nil && result.ExitCode == 0 {
		result.ExitCode = 1
	}
	return testkit.IncusServerExecStep{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode}
}
