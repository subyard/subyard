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
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
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
	}
	rawRegistry := mustReadConfigFixture(t, filepath.Join(root, "config/release-transition.json"))
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
	ownerRoot := source
	invoke := func(request releasetransition.ProcessRequest, grant string) releasetransition.ProcessResponse {
		t.Helper()
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(context.Background(), filepath.Join(ownerRoot, "bin/yard-engine"), "_release-transition")
		command.Env = append([]string{}, processEnvironment...)
		command.Env = append(command.Env, "SUBYARD_REPOSITORY_ROOT="+ownerRoot, "SUBYARD_CONFIG_DIR="+filepath.Join(ownerRoot, "config"))
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
	request := releasetransition.ProcessRequest{SchemaVersion: 1, Mode: releasetransition.ProcessInspect, RuntimeRoot: runtimeRoot, ConfigHome: home, Yard: "default", Target: releasetransition.ReleaseID(sourceID), Direction: releasetransition.DirectionActivateTarget, ArtifactDigest: releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(manifest))), RegistryDigest: releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(rawRegistry)))}
	converge := func(request releasetransition.ProcessRequest) releasetransition.JournalRecord {
		t.Helper()
		inspected := invoke(request, "")
		if inspected.Inspection == nil || len(inspected.Inspection.Blockers) != 0 {
			t.Fatalf("normal source inspection refused: %+v", inspected.Inspection)
		}
		again := invoke(request, "")
		if again.Inspection == nil || again.Inspection.Plan != inspected.Inspection.Plan {
			t.Fatalf("normal source inspection changed: first=%+v second=%+v", inspected.Inspection, again.Inspection)
		}
		request.Mode = releasetransition.ProcessConverge
		request.Execution = &releasetransition.Execution{Plan: inspected.Inspection.Plan, Authorization: "source-authoring-parent-grant"}
		outcome := invoke(request, "source-authoring-parent-grant")
		if outcome.Outcome == nil || !outcome.Outcome.ReachedGoal {
			t.Fatalf("normal source formation did not complete: %+v", outcome.Outcome)
		}
		request.Mode, request.Execution = releasetransition.ProcessInspect, nil
		ready := invoke(request, "")
		if ready.Inspection == nil || ready.Inspection.Outcome == nil || ready.Inspection.Outcome.Status != releasetransition.StatusReady || ready.Inspection.Assessment.Changed {
			t.Fatalf("normal source was not ready: %+v", ready.Inspection)
		}
		journal, err := releasetransition.ParseJournal(mustReadConfigFixture(t, filepath.Join(home, "release-transition/v2/journal.json")))
		if err != nil || journal.Checkpoint != releasetransition.JournalComplete || journal.Goal.Target != request.Target || journal.Goal.Direction != request.Direction {
			t.Fatal("real completed journal absent", err)
		}
		if _, declared := os.Stat(filepath.Join(ownerRoot, "config/release-owner.json")); declared == nil {
			if _, err := os.Stat(filepath.Join(home, "migrations/native-custody/v1", string(journal.Transaction), "bundle.json")); err != nil {
				t.Fatal("real Rust bundle absent", err)
			}
		}
		return journal
	}
	// Start with an actual empty-history self-activation; the initial source
	// owns its migration effects and its completed checkpoint.
	bootstrap := converge(request)
	sshEvidence := map[string][]byte{}
	if migrateSSH {
		for _, step := range bootstrap.Steps {
			if step.Migration == "localize-ssh-port-settings-v3" && step.Checkpoint == releasetransition.StepVerified {
				path := filepath.Join(home, "release-transition/v2/transactions", string(bootstrap.Transaction), "evidence", string(step.ID)+".verified.json")
				sshEvidence[path] = mustReadConfigFixture(t, path)
			}
		}
		for _, id := range []string{"localize-ssh-port-settings-v3.default", "localize-ssh-port-settings-v3.named", "localize-ssh-port-settings-v3.ledger"} {
			path := filepath.Join(home, "release-transition/v2/transactions", string(bootstrap.Transaction), "evidence", id+".verified.json")
			if _, ok := sshEvidence[path]; !ok {
				t.Fatalf("bootstrap did not verify SSH migration step %s", id)
			}
		}
	}
	// Other callers require both current and previous; establish them with a
	// real forward activation, then perform a real rollback when requested.
	ownerRoot = root
	request.Target = releasetransition.ReleaseID(targetID)
	converge(request)
	for path, before := range sshEvidence {
		if !bytes.Equal(before, mustReadConfigFixture(t, path)) {
			t.Fatal("forward activation changed retained SSH migration evidence")
		}
	}
	if direction == releasetransition.DirectionActivatePrevious {
		ownerRoot = source
		request.Target, request.Direction = releasetransition.ReleaseID(sourceID), direction
		converge(request)
		root = source
		processEnvironment = append(processEnvironment, "SUBYARD_REPOSITORY_ROOT="+root, "SUBYARD_CONFIG_DIR="+filepath.Join(root, "config"))
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
		return completedSourceNativeObservation(t, program, ready, call)
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

func TestCompletedSSHSourceAuthoringFixture(t *testing.T) {
	program, _, home := completedRustSourceAuthoringFixture(t, true, nil)
	for name, port := range map[string]string{"default": "2240", "named": "2241"} {
		raw := mustReadConfigFixture(t, filepath.Join(home, "yards", name, "config.env"))
		if !strings.Contains(string(raw), "SSH_PORT='"+port+"'\n") {
			t.Fatalf("bootstrap did not localize the %s SSH setting", name)
		}
	}
	reader := releaseruntime.NewSourceInspector(program.options.RepositoryRoot, io.Discard)
	defer reader.Close()
	proof, err := reader.InspectSourceAdmission(context.Background(), program.env["YARD_RUNTIME_ROOT"], home)
	if err != nil || proof.Metadata.Journal == nil || proof.Metadata.Journal.Checkpoint != releasetransition.JournalComplete {
		t.Fatal("completed SSH source admission", err)
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

// This fixture selects the current runtime policy: instruction files use actual
// host bytes, while preview guidance is supplied by the separate agent skill.
func completedSourceMaterializedPayloads(t *testing.T, program *CLI, yard string) (config.Loaded, []config.MaterializationSource, map[string][]byte, error) {
	loaded, err := program.loadSelectionRepair(yard)
	if err != nil {
		t.Log("completed source observation failed: stage=selection-load")
		return loaded, nil, nil, err
	}
	sources, err := config.SelectMaterializationSources(loaded.Environment, loaded.Context.DevUser, config.MaterializationRuntime, loaded.Catalog.Integrations()...)
	if err != nil {
		t.Log("completed source observation failed: stage=select-materialization")
		return loaded, nil, nil, err
	}
	payloads := map[string][]byte{}
	for _, source := range sources {
		raw, readErr := source.Asset.ReadSource()
		if readErr != nil && !os.IsNotExist(readErr) {
			t.Log("completed source observation failed: stage=asset-read")
			return loaded, nil, nil, readErr
		}
		payload, present, err := config.PrepareMaterializationContent(config.MaterializationRuntime, false, config.MaterializationContent{Present: readErr == nil, Payload: raw}, nil)
		if err != nil {
			t.Log("completed source observation failed: stage=prepare-content")
			return loaded, nil, nil, err
		}
		if present {
			payloads[source.Asset.Destination] = payload
		}
	}
	return loaded, sources, payloads, nil
}

func completedSourceNativeObservation(t *testing.T, program *CLI, ready string, call testkit.IncusServerExecCall) testkit.IncusServerExecStep {
	if slices.Equal(call.Command, []string{"bash", "-s", "--", "--observe"}) {
		payload, _ := json.Marshal(map[string]any{"projects": fmt.Sprintf("%x", sha256.Sum256(nil)), "wiring": fmt.Sprintf("%x", sha256.Sum256(nil)), "hooks": []string{}, "roots": []string{}, "facts": map[string]string{}, "resolved": map[string]string{}})
		return testkit.IncusServerExecStep{Stdout: payload}
	}
	_, err := os.Stat(ready)
	current := err == nil
	if len(call.Command) == 5 && call.Command[0] == "python3" && call.Command[1] == "-B" && call.Command[2] == "-c" && strings.Contains(call.Command[3], "Version 1 records observed ownership only") && call.Command[4] == "configs-observe" {
		yard := strings.TrimPrefix(call.Project, "subyard-")
		if call.Project == "subyard" {
			yard = "default"
		}
		loaded, sources, payloads, sourceErr := completedSourceMaterializedPayloads(t, program, yard)
		if sourceErr != nil {
			return testkit.IncusServerExecStep{ExitCode: 1}
		}
		type entry struct {
			ID     string `json:"id"`
			Kind   string `json:"kind"`
			Path   string `json:"path"`
			Digest string `json:"digest"`
			Format string `json:"format,omitempty"`
		}
		type observationRequest struct {
			Home    string  `json:"home"`
			UID     int     `json:"uid"`
			Entries []entry `json:"entries"`
		}
		var expected observationRequest
		expected.Home, expected.UID = "/home/"+loaded.Context.DevUser, loaded.Context.DevUID
		if expected.UID <= 0 {
			expected.UID = 1000
		}
		expected.Entries = []entry{}
		files := map[string][]byte{}
		for _, source := range sources {
			payload, present := payloads[source.Asset.Destination]
			if !present {
				continue
			}
			kind := "file"
			if source.Asset.OwnedFormat != "" {
				kind = "structured"
			}
			expected.Entries = append(expected.Entries, entry{source.Integration, kind, source.Asset.Destination, fmt.Sprintf("%x", sha256.Sum256(payload)), source.Asset.OwnedFormat})
			if current {
				guestPayload := payload
				if source.Asset.OwnedFormat != "" {
					request, prepareErr := configmaterial.Request(source.Asset.OwnedFormat, configmaterial.ModeObserve, loaded.Context.DevUser, source.Asset.Destination, loaded.Context.DevUID, payload)
					if prepareErr != nil {
						t.Log("completed source configs-observe failed: stage=structured-request")
						return testkit.IncusServerExecStep{ExitCode: 1}
					}
					_, guestPayload, prepareErr = completedSourceStructuredObservation(request, true)
					if prepareErr != nil {
						t.Log("completed source configs-observe failed: stage=structured-observation")
						return testkit.IncusServerExecStep{ExitCode: 1}
					}
				}
				files[source.Asset.Destination] = guestPayload
			}
		}
		// The SDK fixture selects stdout before websocket stdin arrives. Bind
		// that canned physical observation to the actual request in AfterInput.
		request, _ := json.Marshal(expected)
		physical := afterOwnerConfigTestState{&nativeMaterializedState{files: files}}
		result, observeErr := physical.Exec(context.Background(), call.Project, call.Name, ports.InstanceExecRequest{Command: call.Command, Stdin: request})
		if observeErr != nil {
			t.Log("completed source configs-observe failed: stage=physical-observation")
			return testkit.IncusServerExecStep{ExitCode: 1}
		}
		if result.ExitCode != 0 {
			t.Logf("completed source configs-observe failed: stage=physical-exit current=%t sources=%d entries=%d files=%d", current, len(sources), len(expected.Entries), len(files))
		}
		return testkit.IncusServerExecStep{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode, AfterInput: func(actual testkit.IncusServerExecCall) int {
			var value observationRequest
			decoder := json.NewDecoder(bytes.NewReader(actual.Stdin))
			decoder.DisallowUnknownFields()
			decoded := decoder.Decode(&value) == nil
			if !decoded || value.Entries == nil || value.Home != expected.Home || value.UID != expected.UID || !slices.Equal(value.Entries, expected.Entries) {
				t.Logf("completed source configs-observe failed: stage=input decoded=%t entries_present=%t home_match=%t uid_match=%t entries_match=%t entries=%d expected=%d", decoded, value.Entries != nil, value.Home == expected.Home, value.UID == expected.UID, slices.Equal(value.Entries, expected.Entries), len(value.Entries), len(expected.Entries))
				return 1
			}
			var extra any
			if decoder.Decode(&extra) != io.EOF {
				t.Log("completed source configs-observe failed: stage=input-trailing")
				return 1
			}
			return 0
		}}
	}

	if (len(call.Command) == 11 && call.Command[4] == configmaterial.ModeObserve || len(call.Command) == 10 && (call.Command[4] == configmaterial.ModeObserve || call.Command[4] == configmaterial.ModeAssessAdopt)) && call.Command[0] == "python3" {
		reject := testkit.IncusServerExecStep{ExitCode: 1}
		yard := strings.TrimPrefix(call.Project, "subyard-")
		if call.Project == "subyard" {
			yard = "default"
		}
		loaded, sources, payloads, loadErr := completedSourceMaterializedPayloads(t, program, yard)
		if loadErr != nil || call.Project != loaded.Context.IncusProject || call.Name != loaded.Context.YardInstanceName || call.User != 0 || call.Group != 0 || len(call.Environment) != 0 {
			return reject
		}
		for _, source := range sources {
			payload, present := payloads[source.Asset.Destination]
			if !present || source.Asset.OwnedFormat == "" {
				continue
			}
			var templates [][]byte
			if call.Command[4] == configmaterial.ModeAssessAdopt && source.Asset.OwnedFormat == "toml" {
				root := filepath.Join(loaded.Context.Paths.RepositoryRoot, "config", "agents", source.Integration, "legacy-config")
				entries, readErr := os.ReadDir(root)
				if readErr != nil && !os.IsNotExist(readErr) {
					return reject
				}
				for _, entry := range entries {
					if filepath.Ext(entry.Name()) != ".toml" {
						continue
					}
					raw, readErr := (config.MaterializedAsset{Source: filepath.Join(root, entry.Name())}).ReadSource()
					if readErr != nil {
						return reject
					}
					templates = append(templates, raw)
				}
			}
			request, requestErr := configmaterial.Request(source.Asset.OwnedFormat, call.Command[4], loaded.Context.DevUser, source.Asset.Destination, loaded.Context.DevUID, payload, templates...)
			if requestErr != nil || !slices.Equal(request.Command, call.Command[:10]) {
				continue
			}
			capture := request
			if len(call.Command) == 11 {
				capture.Command = append(append([]string(nil), request.Command...), `{"capture":true}`)
			}
			result, _, observeErr := completedSourceStructuredObservation(capture, current)
			if observeErr != nil {
				return reject
			}
			observation, observeErr := configmaterial.ParseObservation(result)
			if observeErr != nil {
				return reject
			}
			if len(call.Command) == 11 && call.Command[10] != `{"capture":true}` {
				d := observation.Destination
				expected := configmaterial.ApplyExpectedState{State: observation.State, Fingerprint: observation.Fingerprint, Exists: d.Exists, UID: d.UID, GID: d.GID, Mode: d.Mode}
				encoded, _ := json.Marshal(expected)
				if call.Command[10] != string(encoded) {
					return reject
				}
			}

			return testkit.IncusServerExecStep{Stdout: result, AfterInput: func(actual testkit.IncusServerExecCall) int {
				if actual.Project != call.Project || actual.Name != call.Name || actual.User != 0 || actual.Group != 0 || len(actual.Environment) != 0 || !slices.Equal(actual.Command, call.Command) || !bytes.Equal(actual.Stdin, request.Stdin) {
					return 1
				}
				return 0
			}}
		}
		return reject
	}

	if len(call.Command) == 3 && call.Command[0] == "sha256sum" && call.Command[1] == "--" {
		digest := strings.Repeat("0", 64)
		if current {
			yard := strings.TrimPrefix(call.Project, "subyard-")
			if call.Project == "subyard" {
				yard = "default"
			}
			_, _, payloads, sourceErr := completedSourceMaterializedPayloads(t, program, yard)
			if sourceErr != nil {
				return testkit.IncusServerExecStep{ExitCode: 1}
			}
			payload, found := payloads[call.Command[2]]
			if !found {
				return testkit.IncusServerExecStep{ExitCode: 1}
			}
			digest = fmt.Sprintf("%x", sha256.Sum256(payload))
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

// Reuse the shipped observer's pure projection and evidence constructors. The
// fixture's applied phase has actual canonical baseline/receipt bytes; absence
// stays explicit. Read-only probes never advance that phase or write guest state.
func completedSourceStructuredObservation(request ports.InstanceExecRequest, applied bool) ([]byte, []byte, error) {
	index := strings.LastIndex(request.Command[3], "\ntry:\n    main()\n")
	if index < 0 {
		return nil, nil, fmt.Errorf("unknown materialization observer entrypoint")
	}
	input, err := json.Marshal(map[string]any{"command": request.Command, "payload": request.Stdin, "applied": applied})
	if err != nil {
		return nil, nil, err
	}
	const observe = `
import base64
value=json.load(sys.stdin)
command=value["command"]
payload=base64.b64decode(value["payload"],validate=True)
desired=parse_document(payload)
current=copy.deepcopy(desired) if value["applied"] else {}
original=document_payload(current,None) if value["applied"] else None
baseline=evidence=None
state={"baseline":"","receipt":""}
if value["applied"]:
    evidence=applied_evidence(current,desired,command[5],command[6],command[8])
    baseline=compatible_baseline(evidence)
    raw_baseline=semantic_payload(baseline)+b"\n"
    raw_receipt=semantic_payload(evidence)+b"\n"
    baseline=validate_baseline(raw_baseline,command[5],command[6])
    evidence=validate_baseline(raw_receipt,command[5],command[6])
    state={"baseline":hashlib.sha256(raw_baseline).hexdigest(),"receipt":hashlib.sha256(raw_receipt).hexdigest()}
report=materialized_observation(current,desired,baseline,evidence,evidence is not None,original,command[8])
if command[4]=="assess-adopt":
    report["adoptable"]=evidence is not None and projection_digest(current,evidence["owned"])==evidence["projection_digest"]
if len(command)==11:
    report["state"]=state
    report["destination"]={"exists":original is not None,"digest":"" if original is None else hashlib.sha256(original).hexdigest(),"uid":int(command[7]) if original is not None else 0,"gid":int(command[7]) if original is not None else 0,"mode":0o644 if original is not None else 0}
print(json.dumps({"observation":report,"payload":None if original is None else base64.b64encode(original).decode()},separators=(",",":"),sort_keys=True))
`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-B", "-c", request.Command[3][:index]+observe)
	cmd.Stdin = bytes.NewReader(input)
	result, err := cmd.Output()
	if err != nil || len(result) > 4096 {
		return nil, nil, fmt.Errorf("fixture protected observation failed: %w", err)
	}
	var state struct {
		Observation json.RawMessage `json:"observation"`
		Payload     []byte          `json:"payload"`
	}
	if err := json.Unmarshal(result, &state); err != nil {
		return nil, nil, err
	}
	return state.Observation, state.Payload, nil
}

func TestCompletedSourceStructuredObservationMatchesProtectedEvidence(t *testing.T) {
	for _, format := range []string{"json", "toml"} {
		t.Run(format, func(t *testing.T) {
			payload := []byte(`{"managed":"desired"}`)
			if format == "toml" {
				payload = []byte("managed = 'desired'\n")
			}
			root := testkit.TempDir(t)
			home := filepath.Join(root, "home")
			destination := filepath.Join(home, ".agent", "config")
			if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
				t.Fatal(err)
			}
			request, err := configmaterial.Request(format, configmaterial.ModeObserve, "synthetic", "/home/synthetic/.agent/config", os.Getuid(), payload)
			if err != nil {
				t.Fatal(err)
			}
			request.Command[6], request.Command[9] = destination, home
			// Explicit private guest namespace, using the unchanged observer and writer.
			request.Command[3] = strings.Replace(request.Command[3], `STATE_ROOT = "/var/lib/subyard/config-materialization"`, "STATE_ROOT = "+strconv.Quote(filepath.Join(root, "state")), 1)
			request.Command[3] = strings.Replace(request.Command[3], "STATE_UID = 0", "STATE_UID = "+strconv.Itoa(os.Getuid()), 1)
			request, err = configmaterial.WithAfterOwnerObservation(request, nil)
			if err != nil {
				t.Fatal(err)
			}
			run := func(r ports.InstanceExecRequest) []byte {
				t.Helper()
				cmd := exec.Command(r.Command[0], r.Command[1:]...)
				cmd.Stdin = bytes.NewReader(r.Stdin)
				raw, err := cmd.Output()
				if err != nil {
					t.Fatal("private guest observer", err)
				}
				return raw
			}
			for _, applied := range []bool{false, true} {
				if applied {
					write := request
					write.Command = append([]string(nil), request.Command[:10]...)
					write.Command[4] = configmaterial.ModeApply
					run(write)
				}
				real, err := configmaterial.ParseObservation(run(request))
				if err != nil {
					t.Fatal(err)
				}
				raw, guestPayload, err := completedSourceStructuredObservation(request, applied)
				if err != nil {
					t.Fatal(err)
				}
				modeled, err := configmaterial.ParseObservation(raw)
				if err != nil || !reflect.DeepEqual(real, modeled) {
					t.Fatal("fixture differs from real protected observer", format, applied, err)
				}
				if applied {
					actual, readErr := os.ReadFile(destination)
					if readErr != nil || !bytes.Equal(actual, guestPayload) {
						t.Fatal("fixture guest bytes differ from real writer", readErr)
					}
				}
				if real.Converged != applied || real.State == nil || real.Destination == nil || real.Destination.Exists != applied || (real.State.Baseline != "") != applied || (real.State.Receipt != "") != applied {
					t.Fatal("incoherent protected phase", applied)
				}
			}
		})
	}
}
