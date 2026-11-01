//go:build linux

package releaseruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	goRuntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

// The real packaged helper is sealed exactly as a source query seals the
// active engine. No repository, owner or configuration context is supplied.
func TestRustOwnerRealPackagedRootlessCapabilities(t *testing.T) {
	installed := os.Getenv("SUBYARD_RUST_DELIVERY_RUNTIME_ROOT")
	if installed == "" {
		t.Skip("opt-in normal packaged Rust-owner engine")
	}
	published, err := filepath.EvalSymlinks(filepath.Join(installed, "current"))
	if err != nil {
		t.Fatal(err)
	}
	runtime := New(Config{})
	defer runtime.Close()
	verified, err := runtime.verifyPublishedCandidate(context.Background(), publishedCandidate{release: releasetransition.ReleaseID(filepath.Base(published)), root: published}, installed, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	if verified.releaseOwner == nil {
		t.Fatal("experimental runtime has no Rust declaration")
	}
	root := testkit.TempDir(t)
	marker := filepath.Join(root, "protected", "migrations", "current.json")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, marker, []byte("protected rootless capability sentinel\n"), 0o600)
	before, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	working := filepath.Join(root, "empty")
	if err := os.Mkdir(working, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []releasetransition.RecoveryProcessMode{releasetransition.RecoveryProcessCapabilities, releasetransition.RecoveryProcessLifecycleCapabilities} {
		payload, err := releasetransition.MarshalRecoveryProcessRequest(releasetransition.RecoveryProcessRequest{SchemaVersion: 2, Mode: kind})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		command := exec.CommandContext(ctx, "/proc/self/fd/3", "_release-transition")
		command.ExtraFiles = []*os.File{verified.engine}
		command.Env, command.Dir, command.Stdin = []string{"PATH=/nonexistent"}, working, bytes.NewReader(payload)
		output, err := command.CombinedOutput()
		cancel()
		want := releasetransition.ActivationOnlyRecoveryCapabilities()
		if kind == releasetransition.RecoveryProcessLifecycleCapabilities {
			want = releasetransition.ActivationRecoveryLifecycleCapabilities()
		}
		encoded, encodeErr := releasetransition.MarshalRecoveryProcessResponse(releasetransition.RecoveryProcessResponse{SchemaVersion: 2, Capabilities: &want})
		if err != nil || encodeErr != nil || !bytes.Equal(output, append(encoded, '\n')) {
			t.Fatalf("sealed rootless %s: output=%q err=%v encode=%v", kind, output, err, encodeErr)
		}
	}
	for _, payload := range []string{`{"schemaVersion":2,"mode":"capabilities","extra":true}`, `{"schemaVersion":2,"mode":"runtime-activation-capabilities"}`, `{"schemaVersion":2,"mode":"migration-owner-capabilities"}`} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		command := exec.CommandContext(ctx, "/proc/self/fd/3", "_release-transition")
		command.ExtraFiles = []*os.File{verified.engine}
		command.Env, command.Dir, command.Stdin = []string{"PATH=/nonexistent"}, working, strings.NewReader(payload)
		var output bytes.Buffer
		command.Stdout = &output
		err := command.Run()
		cancel()
		if err == nil || output.Len() != 0 {
			t.Fatalf("rootless request incorrectly declared: %s output=%q err=%v", payload, output.String(), err)
		}
	}
	after, err := os.Stat(marker)
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("rootless capability changed protected metadata", err)
	}
	payload, err := os.ReadFile(marker)
	if err != nil || string(payload) != "protected rootless capability sentinel\n" {
		t.Fatal("protected bytes changed", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(marker), "update.lock")); !os.IsNotExist(err) {
		t.Fatal("rootless capabilities acquired protected lease", err)
	}
}

func TestRustOwnerRealPackagedRetainedSession(t *testing.T) {
	testRustOwnerRealPackagedSession(t, "")
}

func TestRustOwnerRealPackagedArtifactTamper(t *testing.T) {
	for _, role := range []string{"source", "target"} {
		t.Run(role, func(t *testing.T) { testRustOwnerRealPackagedSession(t, role) })
	}
}

func TestRustOwnerRealPackagedOwnerCleanup(t *testing.T) {
	testRustOwnerRealPackagedSession(t, "cleanup-success")
}

func TestRustOwnerRealPackagedOwnerCleanupRetry(t *testing.T) {
	for _, scenario := range []string{"cleanup-failure", "cleanup-helper-loss", "cleanup-source-drift", "cleanup-missing-previous"} {
		t.Run(scenario, func(t *testing.T) { testRustOwnerRealPackagedSession(t, scenario) })
	}
}

func TestRustOwnerRealPreV2RollbackAdmission(t *testing.T) {
	if os.Getenv("SUBYARD_RUST_DELIVERY_ROLLBACK_ROOT") == "" {
		t.Skip("opt-in authenticated original pre-v2 runtime assets")
	}
	for _, scenario := range []string{"legacy-rollback", "legacy-wrong-direction", "legacy-registry-present", "legacy-wrong-helper", "legacy-source-tamper"} {
		t.Run(scenario, func(t *testing.T) { testRustOwnerRealPackagedSession(t, scenario) })
	}
}

func testRustOwnerRealPackagedSession(t *testing.T, tamper string) {
	t.Helper()
	installed := os.Getenv("SUBYARD_RUST_DELIVERY_RUNTIME_ROOT")
	if installed == "" {
		t.Skip("opt-in normal packaged Rust owner and exact native helper")
	}
	published, err := filepath.EvalSymlinks(filepath.Join(installed, "current"))
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "subyard-packaged-owner-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Log("retained controlled packaged fixture:", root)
			return
		}
		_ = os.RemoveAll(root)
	})
	runtimeRoot, configHome, dataHome := filepath.Join(root, "runtime"), filepath.Join(root, "config"), filepath.Join(root, "data")
	for _, path := range []string{runtimeRoot, configHome, dataHome, filepath.Join(root, "home"), filepath.Join(configHome, ".sync/settings")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	diagnostics, err := os.Create(filepath.Join(root, "diagnostics.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostics.Close()
	sourceID, targetID := filepath.Base(published)+"-source", filepath.Base(published)+"-target"
	rollbackRoot := ""
	if strings.HasPrefix(tamper, "legacy-") {
		rollbackRoot = os.Getenv("SUBYARD_RUST_DELIVERY_ROLLBACK_ROOT")
		targetID = "0.9.1-authentic-rollback"
	}
	tools := filepath.Join(root, "tools")
	if err := os.Mkdir(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range strings.Fields("bash dirname uname jq sha256sum tar gzip cp chmod mv mktemp cut install readlink basename ln sed awk sort find env date id getent python3 cat head stat tr grep wc realpath flock rm cmp comm mkdir rmdir sync") {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(tools, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		directory := os.Getenv("SUBYARD_RUST_DELIVERY_EVIDENCE_DIR")
		if directory == "" {
			return
		}
		for _, tree := range []struct{ source, name string }{{configHome, "failed-config"}, {dataHome, "failed-data"}} {
			_ = filepath.WalkDir(tree.source, func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				relative, _ := filepath.Rel(tree.source, path)
				destination := filepath.Join(directory, filepath.Base(root), tree.name, relative)
				if entry.IsDir() {
					return os.MkdirAll(destination, 0o700)
				}
				if !entry.Type().IsRegular() {
					return nil
				}
				body, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				testkit.WriteFile(t, destination, body, 0o600)
				return nil
			})
		}

	})
	server, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if strings.HasPrefix(tamper, "cleanup-") {
		server.SetProject("subyard-test-yard", map[string]any{"name": "subyard-test-yard", "config": map[string]string{"features.images": "false"}})
		testkit.WriteFile(t, filepath.Join(tools, "incus"), []byte(fmt.Sprintf(`#!/usr/bin/python3
import sys,os,json,time,glob
root=%q
a=sys.argv[1:]
if a==['--version']:print('6.23.0')
elif a==['project','list','--format=json']:
 try:
  journal=json.load(open(root+'/config/release-transition/v2/journal.json'))
  complete=journal['checkpoint']=='complete'
 except FileNotFoundError:complete=False
 accepted=False
 if complete:
  for path in glob.glob(root+'/config/migrations/native-custody/v1/'+journal['transaction']+'/*.intent.json'):
   value=json.load(open(path))
   if value['owner']=='owner-registration-cleanup' and os.path.exists(path.replace('.intent.json','.accepted.json')):accepted=True
 if accepted and os.path.exists(root+'/block-cleanup') and not os.path.exists(root+'/cleanup-query-ready'):
  fd=os.open(root+'/cleanup-query-ready',os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600);os.fchmod(fd,0o600);os.close(fd)
  deadline=time.monotonic()+30
  while not os.path.exists(root+'/release-cleanup-query'):
   if time.monotonic()>deadline:raise SystemExit('readonly cleanup query fixture timed out')
   time.sleep(.01)
 print('[{"name":"subyard-test-yard"}]')
elif a==['project','get','subyard-test-yard','features.images']:print('false')
elif a and a[0]=='list':print('[]')
else:raise SystemExit('unexpected readonly fixture Incus command')
`, root)), 0o755)
		if tamper != "cleanup-success" {
			testkit.WriteFile(t, filepath.Join(root, "block-cleanup"), []byte("block\n"), 0o600)
		}
		if err := os.MkdirAll(filepath.Join(configHome, "yards"), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(configHome, "yards/e2e-yard.env"), []byte("YARD_TEMPLATE=test-vms\nSSH_PORT=2223\nNESTED_E2E_VMS=0\n"), 0o600)
		if err := os.MkdirAll(filepath.Join(dataHome, "e2e/controllers/e2e-yard"), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(dataHome, "e2e/controllers/e2e-yard/.operator-enrollment-v1"), []byte("managed\n"), 0o600)
	}
	runtime := New(Config{Inputs: CaptureCommandInputs([]string{"HOME=" + filepath.Join(root, "home"), "PATH=" + tools, "SUBYARD_NO_AUDIT=1", "SUBYARD_INCUS_SOCKET=" + server.SocketPath, "SUBYARD_POWER_RECONCILER_PATH=" + filepath.Join(root, "absent-power"), "SUBYARD_POWER_UNIT_PATH=" + filepath.Join(root, "absent-unit")}, nil),
		Context: RuntimeContext{DataHome: dataHome, ConfigHome: configHome, RuntimeRoot: runtimeRoot}, Stderr: diagnostics})
	defer runtime.Close()
	releases := []string{sourceID, targetID}
	if strings.HasPrefix(tamper, "cleanup-") {
		releases = append(releases, sourceID+"-previous")
	}
	for _, release := range releases {
		destination := filepath.Join(runtimeRoot, "releases", release)
		input := published
		if release == targetID && rollbackRoot != "" {
			input = rollbackRoot
		}
		err := filepath.WalkDir(input, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(input, path)
			if err != nil {
				return err
			}
			target := filepath.Join(destination, relative)
			if entry.IsDir() {
				return os.MkdirAll(target, 0o700)
			}
			if !entry.Type().IsRegular() {
				return errors.New("package fixture requires regular assets")
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			testkit.WriteFile(t, target, body, info.Mode().Perm())
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, asset := range []string{"runtime-files.sha256", "config/release-checkpoint.json"} {
			if err := os.Chmod(filepath.Join(destination, asset), 0o644); err != nil && !(asset == "config/release-checkpoint.json" && errors.Is(err, os.ErrNotExist)) {
				t.Fatal(err)
			}
		}
	}
	if err := os.Symlink("releases/"+sourceID, filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	if rollbackRoot != "" {
		if err := os.Symlink("releases/"+targetID, filepath.Join(runtimeRoot, "previous")); err != nil {
			t.Fatal(err)
		}
	}
	if strings.HasPrefix(tamper, "cleanup-") {
		if err := os.Symlink("releases/"+sourceID+"-previous", filepath.Join(runtimeRoot, "previous")); err != nil {
			t.Fatal(err)
		}
	}
	if tamper == "legacy-registry-present" {
		registry, err := os.ReadFile(filepath.Join(runtimeRoot, "releases", sourceID, "config/release-transition.json"))
		if err != nil {
			t.Fatal(err)
		}
		candidate := publishedCandidate{release: releasetransition.ReleaseID(targetID), root: filepath.Join(runtimeRoot, "releases", targetID)}
		testkit.WriteFile(t, filepath.Join(candidate.root, "config/release-transition.json"), registry, 0o644)
		rewriteReleaseOwnerManifest(t, candidate)
	}
	if !strings.HasPrefix(tamper, "cleanup-") {
		testkit.WriteFile(t, filepath.Join(configHome, ".sync/settings/config.env"), []byte("SSH_PORT=2240\n"), 0600)
	}
	if tamper == "" {
		if err := os.MkdirAll(filepath.Join(configHome, "yards/default"), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(configHome, "yards/default/config.env"), []byte("DEV_USER='dev'\n"), 0o600)
	}

	if strings.HasPrefix(tamper, "cleanup-") {
		// Start from a valid partial settings frontier. Reapplying the historical
		// template conversion deliberately removes NESTED_E2E_VMS=0 and requires
		// real nested-VM host devices, unrelated to archive cleanup acceptance.
		if err := os.MkdirAll(filepath.Join(configHome, "release-transition/v2"), 0700); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(runtimeRoot, "releases", targetID, "config/release-transition.json"))
		if err != nil {
			t.Fatal(err)
		}
		registry, _, err := releasetransition.ParseRegistryV2(raw, releasetransition.BuiltinCapabilityCatalog())
		if err != nil {
			t.Fatal(err)
		}
		ledger := releasetransition.BaselineLedgerV2(registry)
		ledger, err = ledger.Advance(registry, registry.Migrations[0])
		if err != nil {
			t.Fatal(err)
		}
		payload, _, err := releasetransition.MarshalLedgerV2(ledger, registry)
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(configHome, "release-transition/v2/ledger.json"), payload, 0600)
	}

	verified, err := runtime.verifyPublishedCandidate(context.Background(), publishedCandidate{release: releasetransition.ReleaseID(targetID), root: filepath.Join(runtimeRoot, "releases/"+targetID)}, runtimeRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	if tamper == "" || tamper == "source" || tamper == "target" {
		// Form a real completed source using the packaged owner before testing a
		// distinct target. Cold bootstrap is valid only for the active image itself.
		source, err := runtime.verifyPublishedCandidate(context.Background(), publishedCandidate{release: releasetransition.ReleaseID(sourceID), root: filepath.Join(runtimeRoot, "releases", sourceID)}, runtimeRoot, nil)
		if err != nil {
			t.Fatal("verify bootstrap source:", err)
		}
		defer source.Close()
		bootstrap := releasetransition.ProcessRequest{SchemaVersion: 1, Mode: releasetransition.ProcessInspect, RuntimeRoot: runtimeRoot, ConfigHome: configHome, Yard: "default", Target: releasetransition.ReleaseID(sourceID), Direction: releasetransition.DirectionActivateTarget, ArtifactDigest: source.manifestDigest, RegistryDigest: source.registryDigest}
		initial, err := runtime.startRustOwner(context.Background(), source, source, source, bootstrap, "")
		if err != nil {
			t.Fatal("bootstrap Open:", err)
		}
		initialInspection, err := initial.Inspect(context.Background(), nil)
		if err != nil || len(initialInspection.Blockers) != 0 {
			t.Fatalf("bootstrap Inspect: %+v err=%v", initialInspection, err)
		}
		_, unlock, err := uninstall.OwnerLock(context.Background(), dataHome, nil)
		if err != nil {
			t.Fatal("bootstrap owner lock:", err)
		}
		initialOutcome, err := initial.Execute(context.Background(), initialInspection.Plan, "")
		unlock()
		if err != nil || !initialOutcome.ReachedGoal {
			t.Fatalf("bootstrap Execute: %+v err=%v", initialOutcome, err)
		}
		if err := initial.Close(); err != nil {
			t.Fatal("bootstrap Close:", err)
		}
		ready, err := runtime.InspectCurrentReadiness(context.Background(), configHome)
		if err != nil || ready.Status != releasetransition.StatusReady || !ready.ReachedGoal {
			t.Fatalf("bootstrap readiness: %+v err=%v", ready, err)
		}
	}
	lockPath := filepath.Join(configHome, "migrations/update.lock")
	lockBefore, lockErr := os.Stat(lockPath)
	if lockErr != nil && !errors.Is(lockErr, os.ErrNotExist) {
		t.Fatal(lockErr)
	}
	releaseReadOnlyLock := func() {}
	if lockBefore != nil {
		lock, err := os.OpenFile(lockPath, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal("hold update lease during read-only preparation:", err)
		}
		// Released after inspection below; execution must acquire the real lease.
		defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
		releaseReadOnlyLock = func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }
	}
	checkReadOnlyLock := func() {
		t.Helper()
		after, err := os.Stat(lockPath)
		if lockBefore == nil {
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatal("read-only operation created update lease", err)
			}
		} else if err != nil || !os.SameFile(lockBefore, after) || lockBefore.Mode() != after.Mode() || lockBefore.Size() != after.Size() || !lockBefore.ModTime().Equal(after.ModTime()) {
			t.Fatal("read-only operation changed update lease", err)
		}
	}
	request := releasetransition.ProcessRequest{SchemaVersion: 1, Mode: releasetransition.ProcessInspect, RuntimeRoot: runtimeRoot, ConfigHome: configHome, Yard: "default", Target: releasetransition.ReleaseID(targetID), Direction: releasetransition.DirectionActivateTarget, ArtifactDigest: verified.manifestDigest, RegistryDigest: verified.registryDigest}
	owner := verified
	if rollbackRoot != "" {
		owner, err = runtime.verifyPublishedCandidate(context.Background(), publishedCandidate{release: releasetransition.ReleaseID(sourceID), root: filepath.Join(runtimeRoot, "releases", sourceID)}, runtimeRoot, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Close()
		request.Direction, request.RegistryDigest = releasetransition.DirectionActivatePrevious, owner.registryDigest
	}
	engine := owner
	switch tamper {
	case "legacy-wrong-direction":
		request.Direction = releasetransition.DirectionActivateTarget
	case "legacy-wrong-helper":
		engine = verified
	case "legacy-source-tamper":
		path := filepath.Join(owner.candidate.root, "config/yards/README.md")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, append(body, '\n'), 0o644)
	}
	if tamper == "" {
		// Exercise the compiled normal engine ingress, not a synthetic TestMain
		// or a direct adapter call. materialized-config distinguishes Rust's
		// inventory from the ordinary Go capability response.
		definitions, err := profile.Load(owner.candidate.root)
		if err != nil {
			t.Fatal(err)
		}
		wantOwners := []string{"materialized-config", "core-guest-runtime", "key-tools-runtime", "test-vm-broker", "test-yard-route-consumers", "host-power"}
		for _, definition := range definitions {
			if definition.Runtime != nil {
				wantOwners = append(wantOwners, definition.Runtime.ActivationID)
			}
		}
		slices.Sort(wantOwners)
		settingsPath := filepath.Join(configHome, ".sync/settings/config.env")
		settingsBefore, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatal(err)
		}
		settingsInfo, err := os.Stat(settingsPath)
		if err != nil {
			t.Fatal(err)
		}
		invoke := func(kind releasetransition.RecoveryProcessMode) (releasetransition.RecoveryProcessResponse, []byte, error) {
			t.Helper()
			payload, err := releasetransition.MarshalRecoveryProcessRequest(releasetransition.RecoveryProcessRequest{SchemaVersion: 2, Mode: kind})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, filepath.Join(owner.candidate.root, "bin/yard-engine"), "_release-transition")
			command.Env = []string{"HOME=" + filepath.Join(root, "home"), "PATH=" + tools,
				"SUBYARD_REPOSITORY_ROOT=" + owner.candidate.root, "SUBYARD_HOME=" + dataHome, "SUBYARD_CONFIG_HOME=" + configHome,
				"SUBYARD_NO_AUDIT=1", "SUBYARD_INCUS_SOCKET=" + server.SocketPath,
				"SUBYARD_POWER_RECONCILER_PATH=" + filepath.Join(root, "absent-power"), "SUBYARD_POWER_UNIT_PATH=" + filepath.Join(root, "absent-unit")}
			var stdout, stderr bytes.Buffer
			command.Stdin, command.Stdout, command.Stderr = bytes.NewReader(payload), &stdout, &stderr
			if err := command.Run(); err != nil {
				return releasetransition.RecoveryProcessResponse{}, stdout.Bytes(), fmt.Errorf("%w: %s", err, stderr.String())
			}
			response, err := releasetransition.ParseRecoveryProcessResponse(stdout.Bytes())
			return response, stdout.Bytes(), err
		}
		response, _, err := invoke(releasetransition.RecoveryProcessRuntimeCapabilities)
		if err != nil || !slices.Equal(response.ActivationOwners, wantOwners) {
			t.Fatalf("compiled Rust inventory ingress: %+v err=%v want=%v", response, err, wantOwners)
		}
		if goRuntime.GOARCH == "amd64" {
			response, _, err := invoke(releasetransition.RecoveryProcessMigrationOwnerCapabilities)
			want := releasetransition.MigrationOwnerReplacementCapabilities()
			if err != nil || response.Capabilities == nil || *response.Capabilities != want {
				t.Fatalf("compiled Rust migration capability ingress: %+v err=%v", response, err)
			}
		} else {
			before := rustCleanupQueryState(t, configHome, dataHome)
			_, output, err := invoke(releasetransition.RecoveryProcessMigrationOwnerCapabilities)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || len(output) != 0 {
				t.Fatalf("unsupported compiled migration capability exit: output=%q err=%v", output, err)
			}
			if !reflect.DeepEqual(before, rustCleanupQueryState(t, configHome, dataHome)) {
				t.Fatal("unsupported compiled migration capability changed protected state")
			}
		}
		asset := filepath.Join(owner.candidate.root, releaseOwnerImagePath)
		original, err := os.ReadFile(asset)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(asset)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(asset, asset+".retained"); err != nil {
			t.Fatal(err)
		}
		for _, negative := range []string{"missing", "tampered"} {
			if negative == "tampered" {
				testkit.WriteFile(t, asset, []byte("tampered owner image\n"), info.Mode().Perm())
			}
			_, output, refused := invoke(releasetransition.RecoveryProcessRuntimeCapabilities)
			if negative == "tampered" {
				if err := os.Remove(asset); err != nil {
					t.Fatal(err)
				}
			}
			if refused == nil || len(output) != 0 {
				t.Fatalf("compiled Rust %s owner fell back: output=%q err=%v", negative, output, refused)
			}
		}
		if err := os.Rename(asset+".retained", asset); err != nil {
			t.Fatal(err)
		}
		restored, err := os.ReadFile(asset)
		if err != nil || !bytes.Equal(restored, original) {
			t.Fatal("owner fixture restoration failed", err)
		}
		settingsAfter, err := os.ReadFile(settingsPath)
		if err != nil || !bytes.Equal(settingsBefore, settingsAfter) {
			t.Fatal("capability ingress changed protected settings", err)
		}
		after, err := os.Stat(settingsPath)
		if err != nil || !os.SameFile(settingsInfo, after) || !settingsInfo.ModTime().Equal(after.ModTime()) {
			t.Fatal("capability ingress replaced protected settings", err)
		}
		checkReadOnlyLock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	session, err := runtime.startRustOwner(ctx, owner, engine, verified, request, "")
	if strings.HasPrefix(tamper, "legacy-") && tamper != "legacy-rollback" {
		cancel()
		if err == nil {
			session.Close()
			t.Fatal("invalid pre-v2 owner/assets pairing was admitted", tamper)
		}
		if _, statErr := os.Stat(filepath.Join(configHome, "migrations/update.lock")); !os.IsNotExist(statErr) {
			t.Fatal("invalid pre-v2 pairing acquired update lease", statErr)
		}
		return
	}
	if err != nil {
		t.Fatalf("normal packaged Open: %v", err)
	}
	defer session.Close()
	cancel()
	kinds := []releasetransition.RecoveryProcessMode{releasetransition.RecoveryProcessCapabilities, releasetransition.RecoveryProcessLifecycleCapabilities, releasetransition.RecoveryProcessRuntimeCapabilities}
	if goRuntime.GOARCH == "amd64" {
		kinds = append(kinds, releasetransition.RecoveryProcessMigrationOwnerCapabilities)
	} else {
		refusalContext, cancelRefusal := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelRefusal()
		rejected, err := runtime.startRustOwner(refusalContext, owner, engine, verified, request, "")
		if err != nil {
			t.Fatal("unsupported capability control Open", err)
		}
		defer rejected.Close()
		before := rustCleanupQueryState(t, configHome, dataHome)
		if _, err := rejected.Capabilities(refusalContext, releasetransition.RecoveryProcessMigrationOwnerCapabilities); err == nil {
			t.Fatal("unsupported migration capability admitted through Go SDK")
		}
		select {
		case <-rejected.done:
		case <-time.After(5 * time.Second):
			t.Fatal("unsupported migration capability coordinator did not exit")
		}
		var exit *exec.ExitError
		if !errors.As(rejected.exitErr, &exit) || exit.ExitCode() != 1 {
			t.Fatal("unsupported migration capability coordinator exit", rejected.exitErr)
		}
		if err := rejected.Close(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, rustCleanupQueryState(t, configHome, dataHome)) {
			t.Fatal("unsupported migration capability through Go SDK changed protected state")
		}
	}
	for _, kind := range kinds {
		response, err := session.Capabilities(context.Background(), kind)
		if err != nil {
			t.Fatalf("normal packaged %s: %v", kind, err)
		}
		if kind == releasetransition.RecoveryProcessRuntimeCapabilities {
			definitions, err := profile.Load(owner.candidate.root)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"materialized-config", "core-guest-runtime", "key-tools-runtime", "test-vm-broker", "test-yard-route-consumers", "host-power"}
			for _, definition := range definitions {
				if definition.Runtime != nil {
					want = append(want, definition.Runtime.ActivationID)
				}
			}
			slices.Sort(want)
			if !slices.Equal(response.ActivationOwners, want) {
				t.Fatalf("Rust activation inventory: got %v want %v", response.ActivationOwners, want)
			}
		}
		if kind != releasetransition.RecoveryProcessRuntimeCapabilities {
			want := releasetransition.ActivationOnlyRecoveryCapabilities()
			if kind == releasetransition.RecoveryProcessLifecycleCapabilities {
				want = releasetransition.ActivationRecoveryLifecycleCapabilities()
			}
			if kind == releasetransition.RecoveryProcessMigrationOwnerCapabilities {
				want = releasetransition.MigrationOwnerReplacementCapabilities()
			}
			if response.Capabilities == nil || *response.Capabilities != want {
				t.Fatalf("%s exact capability tuple differs: %+v", kind, response)
			}
		}
	}

	inspection, err := session.Inspect(context.Background(), nil)
	if err != nil {
		t.Fatalf("normal packaged Inspect: %v", err)
	}
	checkReadOnlyLock()
	releaseReadOnlyLock()

	t.Logf("normal inspection status=%s changed=%v blockers=%+v", inspection.Outcome.Status, inspection.Assessment.Changed, inspection.Blockers)
	if rollbackRoot != "" {
		if inspection.Outcome.Code != releasetransition.CodeRollbackIncompatible || inspection.Outcome.ReachedGoal || inspection.Assessment.Changed {
			t.Fatal("pre-v2 source-owner admission bypassed frozen minimum rollback policy", inspection)
		}
		return
	}
	if len(inspection.Blockers) != 0 {
		t.Fatal("normal packaged preflight refused; execution was not admitted")
	}
	if tamper == "source" || tamper == "target" {
		release := sourceID
		if tamper == "target" {
			release = targetID
		}
		// This inert asset is manifest-listed but has no component behavior.
		// Admission has finished; only the owner's under-lease byte verification
		// can reject its replacement before the first migration effect.
		path := filepath.Join(runtimeRoot, "releases", release, "config/yards/README.md")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, append(body, '\n'), 0o644)
	}
	_, release, err := uninstall.OwnerLock(context.Background(), dataHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var beforeTamper map[string]string
	var settingsBeforeTamper []byte
	if tamper == "source" || tamper == "target" {
		beforeTamper = rustCleanupQueryState(t, configHome, dataHome)
		settingsBeforeTamper, err = os.ReadFile(filepath.Join(configHome, ".sync/settings/config.env"))
		if err != nil {
			t.Fatal(err)
		}
	}
	if strings.HasPrefix(tamper, "cleanup-") && tamper != "cleanup-success" {
		testRustOwnerRealCleanupRetry(t, runtime, owner, verified, request, session, inspection, root, tamper, release)
		return
	}
	outcome, err := session.Execute(context.Background(), inspection.Plan, "")
	if tamper == "source" || tamper == "target" {
		t.Logf("changed %s artifact: status=%s code=%s refusal=%v", tamper, outcome.Status, outcome.Code, err)
		if err == nil && outcome.ReachedGoal {
			t.Fatal("owner accepted changed", tamper, "artifact")
		}
		if !reflect.DeepEqual(beforeTamper, rustCleanupQueryState(t, configHome, dataHome)) {
			t.Fatal("artifact refusal changed source history or protected state")
		}
		current, linkErr := os.Readlink(filepath.Join(runtimeRoot, "current"))
		settings, settingsErr := os.ReadFile(filepath.Join(configHome, ".sync/settings/config.env"))
		if linkErr != nil || current != "releases/"+sourceID || settingsErr != nil || !bytes.Equal(settings, settingsBeforeTamper) {
			t.Fatal("artifact refusal changed release links or migration settings")
		}
		return
	}
	if err != nil {
		session.Close()
		details, _ := os.ReadFile(filepath.Join(root, "diagnostics.log"))
		t.Fatalf("normal packaged Execute: %v; owner exit %v; %s", err, session.exitErr, details)
	}
	if !outcome.ReachedGoal {
		raw, _ := os.ReadFile(filepath.Join(configHome, "release-transition/v2/journal.json"))
		var journal struct {
			Checkpoint string                                   `json:"checkpoint"`
			Steps      []struct{ Migration, Checkpoint string } `json:"steps"`
		}
		_ = json.Unmarshal(raw, &journal)
		t.Fatalf("normal packaged outcome: %+v; checkpoint=%s steps=%+v", outcome, journal.Checkpoint, journal.Steps)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if tamper == "cleanup-success" {
		for _, pattern := range []string{filepath.Join(configHome, "yards/.owner-migration-registration-archive.*"), filepath.Join(dataHome, "e2e/controllers/.owner-migration-controller-archive.*")} {
			archives, err := filepath.Glob(pattern)
			if err != nil || len(archives) != 0 {
				t.Fatal("actual owning cleanup retained a completed archive", err)
			}
		}
		// The third original endpoint is no longer a current runtime link.
		// Clean/no-op history must remain readable after its normal expiry.
		if err := os.Rename(filepath.Join(runtimeRoot, "releases", sourceID+"-previous"), filepath.Join(root, "expired-original-previous")); err != nil {
			t.Fatal(err)
		}
		query := NewSourceInspector(verified.candidate.root, diagnostics)
		defer query.Close()
		before := rustCleanupQueryState(t, configHome, dataHome)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		proof, err := query.InspectSourceAdmission(ctx, runtimeRoot, configHome)
		if err != nil || proof.Metadata.Journal == nil || proof.Metadata.Journal.Checkpoint != releasetransition.JournalComplete || !lowerSHA256(proof.Binding) {
			t.Fatal("closed cleanup source admission required expired original assets", err)
		}
		if !reflect.DeepEqual(before, rustCleanupQueryState(t, configHome, dataHome)) {
			t.Fatal("closed cleanup source query changed original history")
		}
	}
	release()
	noop, err := runtime.startRustOwner(context.Background(), verified, verified, verified, request, "")
	if err != nil {
		t.Fatal("normal no-op Open", err)
	}
	defer noop.Close()
	settled, err := noop.Inspect(context.Background(), nil)
	if err != nil {
		t.Fatal("normal no-op Inspect", err)
	}
	if settled.Assessment.Changed || settled.Resume != nil {
		t.Fatalf("completed operation did not become a no-op: %+v", settled)
	}
	if _, err := noop.Execute(context.Background(), settled.Plan, ""); err != nil {
		t.Fatal("normal no-grant/no-account-barrier no-op", err)
	}
	if tamper == "" {
		content, err := os.ReadFile(filepath.Join(configHome, "yards/default/config.env"))
		if err != nil || string(content) != "DEV_USER='dev'\nSSH_PORT='2240'\n" {
			t.Fatal("normal selected-owner settings bytes", err)
		}

		if err := CheckSourcePolicy(context.Background(), request); err != nil {
			t.Fatal("normal selected-owner source admission", err)
		}
		ready, err := runtime.InspectCurrentReadiness(context.Background(), configHome)
		if err != nil || ready.Status != releasetransition.StatusReady || !ready.ReachedGoal {
			t.Fatalf("normal selected-owner current readiness: err=%v status=%s code=%s message=%s", err, ready.Status, ready.Code, ready.Message)
		}
		if tooling := os.Getenv("SUBYARD_RUST_SOURCE_QUERY_TOOLING_ROOT"); tooling != "" {
			query := NewSourceInspector(tooling, diagnostics)
			defer query.Close()
			before := rustCleanupQueryState(t, configHome, dataHome)
			proof, err := query.InspectSourceAdmission(context.Background(), runtimeRoot, configHome)
			if err != nil || proof.Metadata.Journal == nil || proof.Metadata.Journal.Checkpoint != releasetransition.JournalComplete || !lowerSHA256(proof.Binding) {
				t.Fatal("later tooling source admission", err)
			}
			if !reflect.DeepEqual(before, rustCleanupQueryState(t, configHome, dataHome)) {
				t.Fatal("later tooling source query changed original history")
			}
		}
	}

}
