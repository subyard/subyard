//go:build linux

package releaseruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

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
	for _, kind := range []releasetransition.RecoveryProcessMode{releasetransition.RecoveryProcessCapabilities, releasetransition.RecoveryProcessLifecycleCapabilities, releasetransition.RecoveryProcessRuntimeCapabilities} {
		response, err := session.Capabilities(context.Background(), kind)
		if err != nil {
			t.Fatalf("normal packaged %s: %v", kind, err)
		}
		if kind == releasetransition.RecoveryProcessRuntimeCapabilities {
			definitions, err := profile.Load(owner.candidate.root)
			if err != nil {
				t.Fatal(err)
			}
			for _, definition := range definitions {
				if definition.Runtime != nil && !slices.Contains(response.ActivationOwners, definition.Runtime.ActivationID) {
					t.Fatal("Rust omitted declared component owner", definition.Runtime.ActivationID)
				}
			}
		}
	}

	inspection, err := session.Inspect(context.Background(), nil)
	if err != nil {
		t.Fatalf("normal packaged Inspect: %v", err)
	}
	if _, err := os.Stat(filepath.Join(configHome, "migrations/update.lock")); !os.IsNotExist(err) {
		t.Fatal("read-only preparation acquired update lease", err)
	}

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
		if _, statErr := os.Stat(filepath.Join(configHome, "release-transition/v2/journal.json")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("artifact refusal published a journal", statErr)
		}
		current, linkErr := os.Readlink(filepath.Join(runtimeRoot, "current"))
		settings, settingsErr := os.ReadFile(filepath.Join(configHome, ".sync/settings/config.env"))
		if linkErr != nil || current != "releases/"+sourceID || settingsErr != nil || string(settings) != "SSH_PORT=2240\n" {
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

}
