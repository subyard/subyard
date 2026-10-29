package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"golang.org/x/sys/unix"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRustOwnerOperationNativeLocalSSH(t *testing.T) {
	ownerPath, helperPath, corpusPath := os.Getenv("SUBYARD_RUST_OWNER_EXECUTABLE"), os.Getenv("SUBYARD_RUST_NATIVE_HELPER"), os.Getenv("SUBYARD_RUST_RUNTIME_CORPUS")
	if ownerPath == "" || helperPath == "" || corpusPath == "" {
		t.Skip("opt-in actual Rust owner operation")
	}
	var corpus struct {
		Registry       string                                   `json:"registry"`
		Catalog        []releasetransition.CapabilityDescriptor `json:"catalog"`
		RecoveryBefore releasetransition.JournalRecord          `json:"recoveryBefore"`
	}
	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	root, environment, _ := nativeFixture(t)
	writeCLIFile(t, filepath.Join(root, "config", "projects-changed.sh"), "#!/bin/sh\nexit 0\n", 0o644)
	configHome := filepath.Join(root, "state")
	runtimeRoot := filepath.Join(root, "runtime")
	dataHome := filepath.Join(root, "data")
	for _, path := range []string{configHome, dataHome, filepath.Join(runtimeRoot, "releases", "old"), filepath.Join(runtimeRoot, "releases", "new"), filepath.Join(configHome, ".sync", "settings")} {
		if err = os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.Symlink("releases/old", filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(configHome, ".sync", "settings", "config.env"), []byte("SSH_PORT=2240\n"), 0o600)
	server, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	environment = append(environment, "SUBYARD_INCUS_SOCKET="+server.SocketPath, "SUBYARD_POWER_RECONCILER_PATH="+filepath.Join(root, "absent-power"), "SUBYARD_POWER_UNIT_PATH="+filepath.Join(root, "absent-unit"))
	// Direct component preflight gives a precise owning native failure without
	// exposing private component errors through the operation's public protocol.
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	rootFD, err := os.Open(configHome)
	if err != nil {
		t.Fatal(err)
	}
	defer rootFD.Close()
	components := &releaseOwnerComponents{cli: program, context: releaseowner.Context{Session: strings.Repeat("b", 64), DataHome: dataHome, ConfigHome: configHome, RuntimeRoot: runtimeRoot, RepositoryRoot: root, Yard: "default", Target: "new", Artifact: strings.Repeat("a", 64), Direction: releasetransition.DirectionActivateTarget, Releases: releasetransition.ReleasePair{From: "old", Target: "new"}}, root: rootFD, inputs: map[string]config.PersistentFileSnapshot{}, overlays: map[string]config.PersistentFileSnapshot{}, loaded: map[string]config.Loaded{}, prepared: map[string]releaseSettingsPrepared{}, activation: map[string]releaseActivationPrepared{}}
	names, owners, err := components.CaptureInventory(context.Background())
	if err != nil {
		t.Fatal("native inventory", err)
	}
	if _, err = components.CaptureIngress(context.Background(), nil); err != nil {
		t.Fatal("native ingress", err)
	}
	for _, yard := range names {
		if _, err = components.CaptureYard(context.Background(), yard, false); err != nil {
			t.Fatal("native raw yard", err)
		}
	}
	if _, err = components.CaptureOwner(context.Background()); err != nil {
		t.Fatal("native owner", err)
	}
	for _, yard := range names {
		if _, err = components.CaptureYard(context.Background(), yard, true); err != nil {
			t.Fatal("native resolved yard", err)
		}
	}
	value := "2240"
	if _, err = components.PrepareSettings(context.Background(), "default", []releaseowner.Edit{{Setting: "SSH_PORT", Value: &value}}); err != nil {
		t.Fatal("native settings", err)
	}
	for _, owner := range append(owners, "materialized-config", "core-guest-runtime", "key-tools-runtime", "test-vm-broker", "test-yard-route-consumers", "host-power") {
		if _, err = components.CaptureActivation(context.Background(), owner, false); err != nil {
			t.Fatalf("native activation %s: %v", owner, err)
		}
	}
	process := startRustOwner(t, ownerPath, helperPath, environment, root, false)
	defer process.close()
	exchange := func(request any) rustOwnerReply {
		value := process.exchange(request)
		if value.Error != "" {
			t.Fatalf("owner refused: %s", value.Error)
		}
		return value
	}
	const secret = "fixture-private-consent"
	options := map[string]any{"dataHome": dataHome, "configHome": configHome, "runtimeRoot": runtimeRoot, "repositoryRoot": root, "yard": "default", "releases": map[string]string{"from": "old", "target": "new"}, "artifactDigest": strings.Repeat("a", 64), "registryRaw": []byte(corpus.Registry), "catalog": corpus.Catalog, "inheritedSettingIds": []string{}, "rollbackTarget": nil, "migrationCheckpoint": false, "sourceIngress": nil}
	if packaged := os.Getenv("SUBYARD_RUST_PACKAGED_OWNER"); packaged != "" {
		rejected := startRustOwner(t, packaged, helperPath, environment, root, false)
		defer rejected.close()
		if err = rejected.encoder.Encode(map[string]any{"op": "open", "options": options}); err != nil {
			t.Fatal(err)
		}
		var reply rustOwnerReply
		if err = rejected.decoder.Decode(&reply); err == nil {
			t.Fatal("packaged owner accepted missing physical artifact tuples")
		}
		if err = rejected.cmd.Wait(); err == nil {
			t.Fatal("packaged owner missingproof exit0")
		}
		if _, err = os.Stat(filepath.Join(configHome, "migrations", "update.lock")); !os.IsNotExist(err) {
			t.Fatal("missingproof took updatelease")
		}
	}
	exchange(map[string]any{"op": "open", "options": options})
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		refused := process.exchange(map[string]any{"op": "inspect-migration-owner", "replacement": map[string]any{"transaction": "old-transaction", "fingerprint": strings.Repeat("a", 64)}})
		if refused.Error != "invalid" {
			t.Fatalf("unsupported migration contract admitted: %s", refused.Error)
		}
		if _, e := os.Stat(filepath.Join(configHome, "migrations", "update.lock")); !os.IsNotExist(e) {
			t.Fatal("unsupported migration contract acquired write lease")
		}
	}
	inspection := exchange(map[string]any{"op": "inspect", "goal": map[string]string{"target": "new", "direction": "activate-target"}}).Inspection
	if inspection == nil || len(inspection.Blockers) != 0 {
		t.Fatalf("inspection blocked: %#v", inspection)
	}
	independentPlan := startRustOwner(t, ownerPath, helperPath, environment, root, false)
	if reply := independentPlan.exchange(map[string]any{"op": "open", "options": options}); reply.Error != "" {
		t.Fatal(reply.Error)
	}
	repeated := independentPlan.exchange(map[string]any{"op": "inspect", "goal": map[string]string{"target": "new", "direction": "activate-target"}})
	left, _ := json.Marshal(inspection)
	right, _ := json.Marshal(repeated.Inspection)
	if !bytes.Equal(left, right) {
		t.Fatal("same immutable snapshot changed inspection/plan across processes")
	}
	independentPlan.socket.Close()
	if err = independentPlan.cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	independentPlan.close()
	// Preparing with a real native helper must not hold update/config write locks.
	updatePath := filepath.Join(configHome, "migrations", "update.lock")
	if _, err = os.Stat(updatePath); !os.IsNotExist(err) {
		t.Fatal("inspection created update lease")
	}
	independent, err := os.Open(configHome)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.Flock(int(independent.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		independent.Close()
		t.Fatal("pending consent held config write lock", err)
	}
	if _, err = config.ReadPersistentSnapshotAt(independent, ".sync/settings/config.env"); err != nil {
		independent.Close()
		t.Fatal("independent observation blocked", err)
	}
	unix.Flock(int(independent.Fd()), unix.LOCK_UN)
	independent.Close()
	checkContext, cancelBarrier := context.WithTimeout(context.Background(), time.Second)
	_, releaseBarrier, barrierErr := uninstall.OwnerLock(checkContext, dataHome, nil)
	cancelBarrier()
	if barrierErr != nil {
		t.Fatalf("pendingconsent held account barrier: %v", barrierErr)
	}
	releaseBarrier()
	if err = os.Remove(uninstall.OwnerLockPath(dataHome)); err != nil {
		t.Fatal(err)
	}

	outcome := exchange(map[string]any{"op": "execute", "plan": inspection.Plan, "authorization": secret}).Outcome
	if outcome == nil || outcome.Status != releasetransition.StatusReady {
		t.Fatalf("owner not ready: %#v", outcome)
	}
	contents, err := os.ReadFile(filepath.Join(configHome, "yards", "default", "config.env"))
	if err != nil || !strings.Contains(string(contents), "2240") {
		t.Fatalf("real SSH pin absent: %v", err)
	}
	original, err := os.ReadFile(filepath.Join(configHome, ".sync", "settings", "config.env"))
	if err != nil || string(original) != "SSH_PORT=2240\n" {
		t.Fatal("source changed")
	}
	target, err := os.Readlink(filepath.Join(runtimeRoot, "current"))
	if err != nil || target != "releases/new" {
		t.Fatal("runtime exchange missing")
	}
	// Actual historical Go admission must continue to understand the frozen
	// journal/history while ignoring the new sibling native-custody namespace.
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	registry, pin, err := releasetransition.ParseRegistryV2([]byte(corpus.Registry), releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.InspectSourceMetadata(registry, pin); err != nil {
		t.Fatalf("Go source admission: %v", err)
	}
	process.socket.Close()
	if err = process.cmd.Wait(); err != nil {
		t.Fatalf("owner exit: %v (%s)", err, process.stderr.String())
	}
	// A fresh process uses the original durable intent without consuming a grant.
	process = startRustOwner(t, ownerPath, helperPath, environment, root, false)
	defer process.close()
	exchange(map[string]any{"op": "open", "options": options})
	resumed := exchange(map[string]any{"op": "resume"}).Outcome
	if resumed == nil || resumed.Status != releasetransition.StatusReady || resumed.Transaction == nil || outcome.Transaction == nil || *resumed.Transaction != *outcome.Transaction {
		t.Fatalf("fresh resume changed transaction: %#v", resumed)
	}
	after, err := os.ReadFile(filepath.Join(configHome, "yards", "default", "config.env"))
	if err != nil || string(after) != string(contents) {
		t.Fatal("resume replay changed desired settings")
	}
	process.socket.Close()
	if err = process.cmd.Wait(); err != nil {
		t.Fatalf("resume exit: %v (%s)", err, process.stderr.String())
	}
	// Start from an actual Go-valid interrupted activation, with the exact
	// completed history produced above. This is fixture setup, never a runtime
	// migration of another owner's journal.
	before, err := releasetransition.MarshalJournal(corpus.RecoveryBefore)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(configHome, "release-transition", "v2", "journal.json"), before, 0o600)
	digest := sha256.Sum256(before)
	replacement := releasetransition.ActivationOnlyRecoveryRequest{Transaction: corpus.RecoveryBefore.Transaction, Fingerprint: releasetransition.Fingerprint(hex.EncodeToString(digest[:]))}
	process = startRustOwner(t, ownerPath, helperPath, environment, root, false)
	defer process.close()
	exchange(map[string]any{"op": "open", "options": options})
	reviewed := exchange(map[string]any{"op": "inspect-recovery", "replacement": replacement, "contract": releasetransition.ActivationOnlyRecoveryContractV2}).Inspection
	if reviewed == nil || reviewed.Resume != nil || len(reviewed.Blockers) != 0 {
		t.Fatalf("recovery review: %#v", reviewed)
	}
	recovered := exchange(map[string]any{"op": "execute", "plan": reviewed.Plan, "authorization": secret}).Outcome
	if recovered == nil || recovered.Status != releasetransition.StatusReady || recovered.Transaction == nil || !strings.HasPrefix(string(*recovered.Transaction), "recovery-v2-") {
		t.Fatalf("recovery outcome %#v", recovered)
	}
	process.socket.Close()
	if err = process.cmd.Wait(); err != nil {
		t.Fatalf("recovery exit %v (%s)", err, process.stderr.String())
	}
	process = startRustOwner(t, ownerPath, helperPath, environment, root, false)
	defer process.close()
	exchange(map[string]any{"op": "open", "options": options})
	resumed = exchange(map[string]any{"op": "resume"}).Outcome
	if resumed == nil || resumed.Status != releasetransition.StatusReady || resumed.Transaction == nil || *resumed.Transaction != *recovered.Transaction {
		t.Fatalf("successor resume %#v", resumed)
	}
	process.socket.Close()
	if err = process.cmd.Wait(); err != nil {
		t.Fatalf("successor exit %v (%s)", err, process.stderr.String())
	}
}

// The fixture-only owner build adds a fifth inherited control descriptor. The
// normal executable has no pause verb or control-descriptor role.
func TestRustOwnerDurableProcessCuts(t *testing.T) {
	ownerPath, helperPath, corpusPath := os.Getenv("SUBYARD_RUST_FIXTURE_OWNER"), os.Getenv("SUBYARD_RUST_NATIVE_HELPER"), os.Getenv("SUBYARD_RUST_RUNTIME_CORPUS")
	if ownerPath == "" || helperPath == "" || corpusPath == "" {
		t.Skip("opt-in real owner process cuts")
	}
	var corpus struct {
		Registry string                                   `json:"registry"`
		Catalog  []releasetransition.CapabilityDescriptor `json:"catalog"`
	}
	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	for _, cut := range []string{"after-prepared-bundle", "after-authorized-journal", "after-native-intent", "after-native-custody", "after-native-terminal", "after-staged-link", "after-link-exchange", "after-complete-journal", "after-authorized-journal/source-drift", "after-authorized-journal/target-drift", "after-authorized-journal/yard-drift", "after-complete-journal/settings-successor", "after-authorized-journal/plan-replacement", "before-terminal-observation/changed-final", "before-ready-cleanup/cleanup-failure", "after-native-terminal/error-after-effect"} {
		t.Run(cut, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			writeCLIFile(t, filepath.Join(root, "config", "projects-changed.sh"), "#!/bin/sh\nexit 0\n", 0o644)
			configHome, runtimeRoot, dataHome := filepath.Join(root, "state"), filepath.Join(root, "runtime"), filepath.Join(root, "data")
			for _, p := range []string{configHome, dataHome, filepath.Join(runtimeRoot, "releases", "old"), filepath.Join(runtimeRoot, "releases", "new"), filepath.Join(configHome, ".sync", "settings")} {
				if err = os.MkdirAll(p, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err = os.Symlink("releases/old", filepath.Join(runtimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(configHome, ".sync", "settings", "config.env"), []byte("SSH_PORT=2240\n"), 0o600)
			if strings.HasSuffix(cut, "/settings-successor") {
				profile, readErr := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "yards", "profiles", "test-vms.env"))
				if readErr != nil {
					t.Fatal(readErr)
				}
				path := filepath.Join(root, "config", "yards", "profiles", "test-vms.env")
				if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, profile, 0o600)
				path = filepath.Join(configHome, "yards", "default", "config.env")
				if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, []byte("YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\n"), 0o600)
			}
			server, e := testkit.NewIncusServer(testkit.TempDir(t))
			if e != nil {
				t.Fatal(e)
			}
			defer server.Close()
			environment = append(environment, "SUBYARD_INCUS_SOCKET="+server.SocketPath, "SUBYARD_POWER_RECONCILER_PATH="+filepath.Join(root, "absent-power"), "SUBYARD_POWER_UNIT_PATH="+filepath.Join(root, "absent-unit"))
			options := map[string]any{"dataHome": dataHome, "configHome": configHome, "runtimeRoot": runtimeRoot, "repositoryRoot": root, "yard": "default", "releases": map[string]string{"from": "old", "target": "new"}, "artifactDigest": strings.Repeat("a", 64), "registryRaw": []byte(corpus.Registry), "catalog": corpus.Catalog, "inheritedSettingIds": []string{}, "rollbackTarget": nil, "migrationCheckpoint": false, "sourceIngress": nil}
			first := startRustOwner(t, ownerPath, helperPath, environment, root, true)
			defer first.close()
			first.exchange(map[string]any{"op": "open", "options": options})
			inspection := first.exchange(map[string]any{"op": "inspect", "goal": map[string]string{"target": "new", "direction": "activate-target"}}).Inspection
			if inspection == nil || len(inspection.Blockers) != 0 {
				t.Fatalf("inspection: %#v", inspection)
			}
			if err = first.encoder.Encode(map[string]any{"op": "execute", "plan": inspection.Plan, "authorization": "fixture-private-consent"}); err != nil {
				t.Fatal(err)
			}
			first.admit(inspection.Plan, "")
			controller := bufio.NewReader(first.control)
			reached := false
			for n := 0; n < 128; n++ {
				if err = first.control.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				phase, e := controller.ReadString('\n')
				if e != nil {
					t.Fatalf("cut %s not reached: %v (%s)", cut, e, first.stderr.String())
				}
				if strings.TrimSpace(phase) == strings.SplitN(cut, "/", 2)[0] {
					reached = true
					break
				}
				if _, err = first.control.Write([]byte{'R'}); err != nil {
					t.Fatal(err)
				}
			}
			if !reached {
				t.Fatal("phase bound")
			}
			if strings.HasSuffix(cut, "/changed-final") || strings.HasSuffix(cut, "/cleanup-failure") || strings.HasSuffix(cut, "/error-after-effect") {
				response := byte('R')
				if strings.HasSuffix(cut, "/changed-final") {
					if e := os.Remove(filepath.Join(runtimeRoot, "current")); e != nil {
						t.Fatal(e)
					}
					if e := os.Symlink("releases/old", filepath.Join(runtimeRoot, "current")); e != nil {
						t.Fatal(e)
					}
				} else {
					response = 'E'
				}
				if _, e := first.control.Write([]byte{response}); e != nil {
					t.Fatal(e)
				}
				// Continue all later read-only checkpoints, preserving the
				// exact real process invocation instead of killing/restarting.
				done := make(chan struct{})
				go func() {
					defer close(done)
					for {
						if _, e := controller.ReadString('\n'); e != nil {
							return
						}
						if _, e := first.control.Write([]byte{'R'}); e != nil {
							return
						}
					}
				}()
				var reply rustOwnerReply
				if e := first.decoder.Decode(&reply); e != nil {
					t.Fatal(e, first.stderr.String())
				}
				first.socket.Close()
				first.control.Close()
				<-done
				if e := first.cmd.Wait(); e != nil {
					t.Fatal(e)
				}
				if reply.Error != "" || reply.Outcome == nil {
					t.Fatalf("terminal evaluation: %#v", reply)
				}
				journalRaw, e := os.ReadFile(filepath.Join(configHome, "release-transition/v2/journal.json"))
				if e != nil {
					t.Fatal(e)
				}
				journal, e := releasetransition.ParseJournal(journalRaw)
				if strings.HasSuffix(cut, "/error-after-effect") {
					if e != nil || journal.Checkpoint == releasetransition.JournalComplete || reply.Outcome.Status != releasetransition.StatusRecovering || reply.Outcome.Code != releasetransition.CodeVerificationFailed {
						raw, _ := json.Marshal(reply)
						t.Fatalf("post-effect recovery %s journal %s (%v)", raw, journal.Checkpoint, e)
					}
					resumed := startRustOwner(t, ownerPath, helperPath, environment, root, false)
					defer resumed.close()
					resumed.exchange(map[string]any{"op": "open", "options": options})
					result := resumed.exchange(map[string]any{"op": "resume"})
					if result.Error != "" || result.Outcome == nil || result.Outcome.Status != releasetransition.StatusReady {
						raw, _ := json.Marshal(result)
						t.Fatalf("post-effect fresh resume %s", raw)
					}
					return
				}
				if e != nil || journal.Checkpoint != releasetransition.JournalComplete {
					t.Fatal("terminal test lost Complete history", e)
				}
				if response == 'E' {
					if reply.Outcome.Status != releasetransition.StatusReady || len(reply.Outcome.Warnings) != 1 || reply.Outcome.Warnings[0] != "recovery cleanup is pending" {
						t.Fatalf("cleanup failure changed Ready: %#v", reply)
					}
				} else {
					if reply.Outcome.Status == releasetransition.StatusReady || reply.Outcome.ReachedGoal {
						t.Fatalf("Complete fabricated Ready: %#v", reply)
					}
					target, e := os.ReadFile(filepath.Join(configHome, "yards/default/config.env"))
					if e != nil || !strings.Contains(string(target), "2240") {
						t.Fatal("terminal observation changed approved settings")
					}
				}
				return
			}
			if err = first.cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err = first.cmd.Wait(); err == nil {
				t.Fatal("owner kill did not interrupt")
			}
			first.socket.Close()
			first.control.Close()
			// Wait for actual borrowed-OFD custody to close after owner EOF. This is a
			// bounded independent lock probe, never PID absence as a quiescence claim.
			fd, e := unix.Open(filepath.Join(configHome, "migrations", "update.lock"), unix.O_RDWR|unix.O_CLOEXEC, 0)
			if e != nil {
				t.Fatal(e)
			}
			defer unix.Close(fd)
			end := time.Now().Add(5 * time.Second)
			for {
				e = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
				if e == nil {
					unix.Flock(fd, unix.LOCK_UN)
					break
				}
				if e != unix.EWOULDBLOCK || time.Now().After(end) {
					t.Fatalf("custody did not drain: %v", e)
				}
				time.Sleep(10 * time.Millisecond)
			}
			drift := strings.HasSuffix(cut, "-drift")
			targetPath := filepath.Join(configHome, "yards", "default", "config.env")
			if strings.HasSuffix(cut, "/source-drift") {
				testkit.WriteFile(t, filepath.Join(configHome, ".sync", "settings", "config.env"), []byte("SSH_PORT=2241\n"), 0o600)
			}
			if strings.HasSuffix(cut, "/target-drift") {
				if err = os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, targetPath, []byte("SSH_PORT=2242\n"), 0o600)
			}
			if strings.HasSuffix(cut, "/yard-drift") {
				path := filepath.Join(configHome, "yards", "other", "config.env")
				if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, []byte("SSH_PORT=2243\n"), 0o600)
			}
			replacementCase := strings.HasSuffix(cut, "/plan-replacement")
			if replacementCase {
				raw, e := os.ReadFile(filepath.Join(configHome, "release-transition/v2/journal.json"))
				if e != nil {
					t.Fatal(e)
				}
				j, e := releasetransition.ParseJournal(raw)
				if e != nil {
					t.Fatal(e)
				}
				pin := sha256.Sum256(raw)
				options["replacement"] = releasetransition.JournalReplacement{Transaction: j.Transaction, Fingerprint: releasetransition.Fingerprint(hex.EncodeToString(pin[:])), Reason: releasetransition.JournalReplacementPreActivationPlanStale}
				testkit.WriteFile(t, filepath.Join(configHome, ".sync/settings/config.env"), []byte("SSH_PORT=2241\n"), 0600)
			}
			paths := []string{filepath.Join(configHome, "release-transition", "v2", "journal.json"), filepath.Join(configHome, "release-transition", "v2", "ledger.json"), targetPath}
			before := make([][]byte, len(paths))
			for n, path := range paths {
				before[n], _ = os.ReadFile(path)
			}
			second := startRustOwner(t, ownerPath, helperPath, environment, root, false)
			defer second.close()
			second.exchange(map[string]any{"op": "open", "options": options})
			if cut == "after-prepared-bundle" {
				reply := second.exchange(map[string]any{"op": "resume"})
				if reply.Error == "" || reply.Outcome != nil {
					t.Fatal("orphan bundle authorized resume")
				}
				if _, e = os.Stat(filepath.Join(configHome, "yards", "default", "config.env")); !os.IsNotExist(e) {
					t.Fatal("orphan bundle caused settings effect")
				}
				return
			}
			var reply rustOwnerReply
			if replacementCase {
				inspected := second.exchange(map[string]any{"op": "inspect", "goal": map[string]string{"target": "new", "direction": "activate-target"}})
				if inspected.Error != "" || inspected.Inspection == nil || len(inspected.Inspection.Blockers) != 0 {
					t.Fatalf("replacement review: %#v", inspected)
				}
				reply = second.exchange(map[string]any{"op": "execute", "plan": inspected.Inspection.Plan, "authorization": "fixture-private-consent"})
			} else {
				reply = second.exchange(map[string]any{"op": "resume"})
			}
			if drift {
				if reply.Error != "" || reply.Outcome == nil || reply.Outcome.Status != releasetransition.StatusOperatorActionRequired {
					t.Fatalf("drift was not refused: %#v", reply)
				}
				for n, path := range paths {
					after, _ := os.ReadFile(path)
					if !bytes.Equal(before[n], after) {
						t.Fatalf("drift refusal changed protected input %d", n)
					}
				}
				return
			}
			if reply.Error != "" || reply.Outcome == nil || reply.Outcome.Status != releasetransition.StatusReady {
				t.Fatalf("fresh resume: %#v (%s)", reply, second.stderr.String())
			}
			contents, e := os.ReadFile(filepath.Join(configHome, "yards", "default", "config.env"))
			expectedPort := "2240"
			if replacementCase {
				expectedPort = "2241"
			}
			if e != nil || !strings.Contains(string(contents), expectedPort) {
				t.Fatalf("resume did not preserve actual pin: %v", e)
			}
		})
	}
}

type rustCustodyContext struct {
	Session  string                `json:"session"`
	DataRoot releaseowner.Identity `json:"dataRoot"`
}
type rustOwnerReply struct {
	Admission *struct {
		Plan        releasetransition.PlanToken `json:"plan"`
		Transaction string                      `json:"transaction"`
	} `json:"admission"`
	Custody    *rustCustodyContext           `json:"custody"`
	Inspection *releasetransition.Inspection `json:"inspection"`
	Outcome    *releasetransition.Outcome    `json:"outcome"`
	Error      string                        `json:"error"`
}
type rustOwnerOutput struct {
	mu   sync.Mutex
	text strings.Builder
}

func (o *rustOwnerOutput) Write(b []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.Write(b)
}
func (o *rustOwnerOutput) String() string { o.mu.Lock(); defer o.mu.Unlock(); return o.text.String() }

type rustOwnerChild struct {
	t          *testing.T
	cmd        *exec.Cmd
	socket     *os.File
	custody    *os.File
	scope      *rustCustodyContext
	dataHome   string
	configHome string
	admitted   bool
	control    net.Conn
	encoder    *json.Encoder
	decoder    *json.Decoder
	stderr     rustOwnerOutput
	cancel     context.CancelFunc
}

func startRustOwner(t *testing.T, owner, helper string, environment []string, repositoryRoot string, control bool) *rustOwnerChild {
	return startRustOwnerWithBudget(t, owner, helper, environment, repositoryRoot, control, 30*time.Second)
}
func startRustOwnerWithBudget(t *testing.T, owner, helper string, environment []string, repositoryRoot string, control bool, budget time.Duration) *rustOwnerChild {
	t.Helper()
	pair, e := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if e != nil {
		t.Fatal(e)
	}
	p := &rustOwnerChild{t: t, socket: os.NewFile(uintptr(pair[0]), "owner client")}
	child := os.NewFile(uintptr(pair[1]), "owner socket")
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	p.cancel = cancel
	p.cmd = exec.CommandContext(ctx, owner, "_release-owner-operation-v1")
	p.cmd.Env = environment
	p.cmd.Stdin = child
	p.cmd.Stdout = child
	p.cmd.Stderr = &p.stderr
	if e = p.cmd.Start(); e != nil {
		t.Fatal(e)
	}
	child.Close()
	image, e := os.Open(helper)
	if e != nil {
		t.Fatal(e)
	}
	defer image.Close()
	r, w, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	io.WriteString(w, "fixture-private-consent")
	w.Close()
	custodyPair, e := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if e != nil {
		t.Fatal(e)
	}
	p.custody = os.NewFile(uintptr(custodyPair[0]), "late custody")
	remoteCustody := os.NewFile(uintptr(custodyPair[1]), "owner custody ingress")
	defer remoteCustody.Close()
	repositoryFD, e := unix.Open(repositoryRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer unix.Close(repositoryFD)
	rights := []int{int(image.Fd()), int(r.Fd()), int(remoteCustody.Fd()), repositoryFD}
	if control {
		pair, e := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if e != nil {
			t.Fatal(e)
		}
		local, remote := os.NewFile(uintptr(pair[0]), "controller"), os.NewFile(uintptr(pair[1]), "owner control")
		defer local.Close()
		defer remote.Close()
		p.control, e = net.FileConn(local)
		if e != nil {
			t.Fatal(e)
		}
		rights = append(rights, int(remote.Fd()))
	}
	if e = unix.Sendmsg(int(p.socket.Fd()), []byte{'C'}, unix.UnixRights(rights...), nil, unix.MSG_NOSIGNAL); e != nil {
		t.Fatal(e)
	}
	p.encoder = json.NewEncoder(p.socket)
	p.decoder = json.NewDecoder(bufio.NewReader(p.socket))
	return p
}
func (p *rustOwnerChild) exchange(request any) rustOwnerReply {
	p.t.Helper()
	if e := p.encoder.Encode(request); e != nil {
		p.t.Fatal(e)
	}
	if object, ok := request.(map[string]any); ok {
		switch object["op"] {
		case "execute":
			p.admit(object["plan"].(releasetransition.PlanToken), "")
		case "resume":
			if raw, e := os.ReadFile(filepath.Join(p.configHome, "release-transition", "v2", "journal.json")); e == nil {
				j, e := releasetransition.ParseJournal(raw)
				if e == nil {
					p.admit(j.ResumePlan, string(j.Transaction))
				}
			}
		}
	}
	var reply rustOwnerReply
	if e := p.decoder.Decode(&reply); e != nil {
		p.t.Fatalf("owner reply: %v (%s)", e, p.stderr.String())
	}
	if reply.Custody != nil {
		p.scope = reply.Custody
		object := request.(map[string]any)
		options := object["options"].(map[string]any)
		p.dataHome = options["dataHome"].(string)
		p.configHome = options["configHome"].(string)
	}
	return reply
}
func (p *rustOwnerChild) admit(plan releasetransition.PlanToken, transaction string) {
	p.t.Helper()
	if p.admitted {
		p.t.Fatal("duplicate outer custody admission")
	}
	if p.scope == nil {
		p.t.Fatal("missing pinned owner scope")
	}
	path := uninstall.OwnerLockPath(p.dataHome)
	directory := filepath.Dir(path)
	if e := os.Mkdir(directory, 0o700); e != nil && !os.IsExist(e) {
		p.t.Fatal(e)
	}
	info, e := os.Lstat(directory)
	if e != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		p.t.Fatal("unsafe owner coordination fixture")
	}
	fd, e := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CREAT|unix.O_EXCL, 0o600)
	created := e == nil
	if errors.Is(e, unix.EEXIST) {
		fd, e = unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	}
	if e != nil {
		p.t.Fatal(e)
	}
	file := os.NewFile(uintptr(fd), "fixture account barrier")
	defer file.Close()
	if created {
		if e = file.Chmod(0o600); e != nil {
			p.t.Fatal(e)
		}
	}
	identity, e := releaseowner.IdentityOf(file)
	if e != nil || identity.UID != uint32(os.Getuid()) || identity.Mode&unix.S_IFMT != unix.S_IFREG || identity.Mode&0o777 != 0o600 || identity.Links != 1 {
		p.t.Fatal("unsafe owner barrier fixture")
	}
	end := time.Now().Add(5 * time.Second)
	for {
		e = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if e == nil {
			break
		}
		if !errors.Is(e, unix.EWOULDBLOCK) || time.Now().After(end) {
			p.t.Fatal("owner barrier remains busy", e)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if created {
		p.t.Cleanup(func() {
			probe, e := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
			if e != nil {
				p.t.Error(e)
				return
			}
			defer unix.Close(probe)
			end := time.Now().Add(5 * time.Second)
			for {
				e = unix.Flock(probe, unix.LOCK_EX|unix.LOCK_NB)
				if e == nil {
					break
				}
				if !errors.Is(e, unix.EWOULDBLOCK) || time.Now().After(end) {
					p.t.Error("native owner barrier did not drain", e)
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			// Only this marker-owned fixture's unique coordination leaf is removed,
			// after all owned processes and borrowed OFDs have drained.
			if e = os.Remove(path); e != nil {
				p.t.Error(e)
			}
		})
	}
	message := struct {
		Version     int                         `json:"version"`
		Session     string                      `json:"session"`
		Plan        releasetransition.PlanToken `json:"plan"`
		Transaction string                      `json:"transaction"`
		DataRoot    releaseowner.Identity       `json:"dataRoot"`
		Lock        releaseowner.Identity       `json:"lock"`
	}{1, p.scope.Session, plan, transaction, p.scope.DataRoot, identity}
	raw, e := json.Marshal(message)
	if e != nil {
		p.t.Fatal(e)
	}
	if e = unix.Sendmsg(int(p.custody.Fd()), []byte{'A'}, unix.UnixRights(fd), nil, unix.MSG_NOSIGNAL); e != nil {
		p.t.Fatal(e)
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(raw)))
	if _, e = p.custody.Write(append(length[:], raw...)); e != nil {
		p.t.Fatal(e)
	}
	// Closing this original reference never explicitly unlocks the borrowed OFD.
	file.Close()
	connection, e := net.FileConn(p.custody)
	if e != nil {
		p.t.Fatal(e)
	}
	defer connection.Close()
	if e = connection.SetReadDeadline(time.Now().Add(5 * time.Second)); e != nil {
		p.t.Fatal(e)
	}
	var ack struct {
		Version     int                         `json:"version"`
		Session     string                      `json:"session"`
		Plan        releasetransition.PlanToken `json:"plan"`
		Transaction string                      `json:"transaction"`
		Accepted    bool                        `json:"accepted"`
	}
	if e = json.NewDecoder(connection).Decode(&ack); e != nil {
		p.t.Fatalf("outer admission: %v (%s)", e, p.stderr.String())
	}
	if ack.Version != 1 || !ack.Accepted || ack.Session != p.scope.Session || ack.Plan != plan || ack.Transaction != transaction {
		p.t.Fatal("outer custody correlation")
	}
	p.admitted = true
}
func (p *rustOwnerChild) close() {
	p.socket.Close()
	if p.custody != nil {
		p.custody.Close()
	}
	if p.control != nil {
		p.control.Close()
	}
	p.cancel()
	p.cmd.Process.Kill()
	p.cmd.Wait()
}

// One immutable component-facts set feeds the actual owning Go metadata
// constructors and Rust aggregation. No independent live Rust/Go inspection.
func TestRustOwnerActivationFactsCorpus(t *testing.T) {
	destination := os.Getenv("SUBYARD_RUST_ACTIVATION_CORPUS")
	if destination == "" {
		t.Skip("opt-in activation facts corpus")
	}
	type sample struct {
		Owner         string                                    `json:"owner"`
		Target        string                                    `json:"target"`
		Artifact      string                                    `json:"artifact"`
		Active        string                                    `json:"active"`
		Targets       []releaseowner.Target                     `json:"targets"`
		Expected      releasetransition.V2ActivationObservation `json:"expected"`
		SourcePending bool                                      `json:"sourcePending"`
	}
	cases := []sample{}
	pair := releasetransition.ReleasePair{From: "release-a", Target: "release-b"}
	links := releasetransition.ReleaseLinks{Active: "release-a"}
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, owner := range []string{"host-power", "test-vm-broker"} {
		for _, applies := range []bool{false, true} {
			for _, converged := range []bool{false, true} {
				stage := ports.ReconcileStagePower
				if owner == "test-vm-broker" {
					stage = ports.ReconcileStageTestVMs
				}
				state := "absent"
				if applies {
					state = "active"
				}
				platform := newInitPlatformFixture()
				platform.converged[stage] = converged
				r := &activationStageReconciler{id: owner, stage: stage, inspectApplicability: func(context.Context) (activationApplicability, error) {
					return activationApplicability{state: state, applies: applies}, nil
				}, platform: func(context.Context, activationApplicability) (ports.ReconcileStageRunner, error) {
					return platform, nil
				}}
				expected, err := r.Observe(context.Background(), pair, links)
				if err != nil {
					t.Fatal(err)
				}
				status := "current"
				if applies && !converged {
					status = "stale"
				}
				cases = append(cases, sample{owner, "release-b", a, "release-a", []releaseowner.Target{{Owner: owner, Class: "stage", State: status, ScopeState: state, Stage: string(stage)}}, expected, false})
			}
		}
	}
	for _, owner := range []string{"absent", "current", "legacy-directory"} {
		for _, converged := range []bool{false, true} {
			port := &routeConsumerActivationFixture{owner: owner, before: "closed-resource-scope"}
			if !converged {
				port.verifyErr = errors.New("fixture drift")
			}
			r := &routeConsumerActivationReconciler{port: port}
			expected, err := r.Observe(context.Background(), pair, links)
			if err != nil {
				t.Fatal(err)
			}
			state := "stale"
			if expected.Converged {
				state = "current"
			}
			cases = append(cases, sample{"test-yard-route-consumers", "release-b", a, "release-a", []releaseowner.Target{{Owner: "test-yard-route-consumers", Class: "route-consumers", State: state, ScopeState: owner, Before: port.before}}, expected, false})
		}
	}
	for _, changed := range []bool{false, true} {
		for _, integrations := range []bool{false, true} {
			materialized := []config.MaterializationActivationTarget{{Name: "default", Assessment: config.MaterializationAssessment{State: "drift", Changed: changed, MaterializedFingerprint: a, DesiredFingerprint: b}, IntegrationScope: b, IntegrationFingerprint: a, IntegrationChanged: integrations}}
			expected := config.AssessMaterializationActivation(materialized)
			state := "current"
			if changed {
				state = "stale"
			}
			integrationState := "current"
			if integrations {
				integrationState = "stale"
			}
			cases = append(cases, sample{"materialized-config", "release-b", a, "release-a", []releaseowner.Target{{Owner: "materialized-config", Yard: "default", Class: "materialized-files", State: state, ScopeState: "drift", ReportedActual: a, ReportedDesired: b, Scope: b}, {Owner: "materialized-config", Yard: "default", Class: "materialized-integrations", State: integrationState, ReportedActual: a}}, releasetransition.V2ActivationObservation{Actual: releasetransition.Fingerprint(expected.Actual), Desired: releasetransition.Fingerprint(expected.Desired), Converged: expected.Converged}, false})
		}
	}
	root, environment, _ := nativeFixture(t)
	runtimeRoot := filepath.Join(root, "installation-runtime")
	if err := os.Mkdir(runtimeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{RepositoryRoot: root, Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	r := &installationActivationReconciler{cli: program, request: releasetransition.ProcessRequest{RuntimeRoot: runtimeRoot, ConfigHome: environmentValue(environment, "SUBYARD_CONFIG_HOME")}}
	defer func() {
		if r.before != nil {
			r.before.Close()
		}
	}()
	for _, converged := range []bool{false, true} {
		if converged {
			if err := r.Reconcile(context.Background(), links); err != nil {
				t.Fatal(err)
			}
		}
		expected, err := r.Observe(context.Background(), pair, links)
		if err != nil {
			t.Fatal(err)
		}
		state := "stale"
		if converged {
			state = "current"
		}
		facts := []releaseowner.Target{{Owner: r.ID(), Class: "installation", State: state, ReportedActual: string(expected.Actual), ReportedDesired: string(expected.Desired), Source: string(expected.Binding)}}
		cases = append(cases, sample{r.ID(), "release-b", a, "release-a", facts, expected, false})
	}
	for _, owner := range []string{"sample-runtime", "core-guest-runtime", "key-tools-runtime"} {
		for _, state := range []ports.RuntimeState{ports.RuntimeStateStale, ports.RuntimeStateCurrent, ports.RuntimeStateDeferred, ports.RuntimeStateAbsent} {
			fixture := newConfigApplyRepairFixture(t, true)
			observation := ports.RuntimeObservation{State: state, Actual: a, Desired: b}
			if state == ports.RuntimeStateCurrent {
				observation.Actual = b
			}
			if state == ports.RuntimeStateAbsent {
				observation.Actual = ""
				observation.Desired = ""
			}
			if state == ports.RuntimeStateDeferred {
				observation.Desired = ""
			}
			if owner == "sample-runtime" {
				fixture.cli.options.InitPlatform = &sampleRepairPlatformFixture{initPlatformFixture: newInitPlatformFixture(), observation: &observation}
			} else {
				fixture.cli.options.InitPlatform = &installedActivationFixture{initPlatformFixture: newInitPlatformFixture(), state: observation}
			}
			r := &profileRuntimeActivationReconciler{cli: fixture.cli, id: owner, request: releasetransition.ProcessRequest{ConfigHome: fixture.cli.env["SUBYARD_CONFIG_HOME"], ArtifactDigest: releasetransition.Fingerprint(a)}}
			_, targets, err := r.targets()
			if err != nil {
				t.Fatal(err)
			}
			expected, err := r.Observe(context.Background(), pair, links)
			if err != nil {
				t.Fatal(err)
			}
			facts := []releaseowner.Target{}
			for _, target := range targets {
				facts = append(facts, releaseowner.Target{Owner: owner, Class: "profile-runtime", Yard: target.Name, Project: target.Loaded.Context.IncusProject, Instance: target.Loaded.Context.YardInstanceName, State: string(state), ReportedActual: observation.Actual, ReportedDesired: observation.Desired})
			}
			cases = append(cases, sample{owner, "release-b", a, "release-a", facts, expected, false})
			if state == ports.RuntimeStateStale {
				home := fixture.cli.env["SUBYARD_CONFIG_HOME"]
				for _, name := range []string{"ledger.json", "history-checkpoint.json"} {
					if e := os.Remove(filepath.Join(home, "release-transition", "v2", name)); e != nil && !os.IsNotExist(e) {
						t.Fatal(e)
					}
				}
				pending, e := r.Observe(context.Background(), pair, links)
				if e != nil || pending.Converged {
					t.Fatalf("pending source oracle: %#v %v", pending, e)
				}
				cases = append(cases, sample{owner, "release-b", a, "release-a", facts, pending, true})
			}

		}
	}
	raw, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(destination, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The affected owner image is authentic; only its interrupted protected facts
// come from the actual Go owning constructor. Candidate execution remains native.
func TestRustOwnerMigrationOwnerOperation(t *testing.T) { rustOwnerMigrationOwnerOperation(t, "") }
func TestRustOwnerMigrationOwnerProcessCuts(t *testing.T) {
	for _, cut := range []string{"after-migration-owner-receipt", "after-migration-owner-journal", "after-migration-owner-receipt/source-drift"} {
		t.Run(cut, func(t *testing.T) { rustOwnerMigrationOwnerOperation(t, cut) })
	}
}
func rustOwnerMigrationOwnerOperation(t *testing.T, cut string) {
	ownerPath, helperPath, corpusPath := os.Getenv("SUBYARD_RUST_OWNER_EXECUTABLE"), os.Getenv("SUBYARD_RUST_NATIVE_HELPER"), os.Getenv("SUBYARD_RUST_MIGRATION_ENTRY_CORPUS")
	predecessors := os.Getenv("SUBYARD_RUST_PREDECESSOR_ASSETS")
	if ownerPath == "" || helperPath == "" || corpusPath == "" || predecessors == "" {
		t.Skip("opt-in authentic defective-owner replacement")
	}
	var fixture struct {
		Files       map[string][]byte                                  `json:"files"`
		Replacement releasetransition.MigrationOwnerReplacementRequest `json:"replacement"`
	}
	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	root, environment, _ := nativeFixture(t)
	writeCLIFile(t, filepath.Join(root, "config", "projects-changed.sh"), "#!/bin/sh\nexit 0\n", 0644)
	configHome, runtimeRoot, dataHome := filepath.Join(root, "state"), filepath.Join(root, "runtime"), filepath.Join(root, "data")
	for _, p := range []string{configHome, dataHome, filepath.Join(runtimeRoot, "releases")} {
		if err = os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct{ version, id string }{{"0.18.1", "0.18.1-source"}, {"0.21.0", "0.21.0-old"}} {
		dir := filepath.Join(runtimeRoot, "releases", item.id)
		if err = os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		archive := filepath.Join(predecessors, "baseline-"+item.version, "subyard-"+item.version+"-linux-amd64.tar.gz")
		if output, e := exec.Command("tar", "-xzf", archive, "-C", dir).CombinedOutput(); e != nil {
			t.Fatalf("authentic fixture unpack: %v %s", e, output)
		}
	}
	targetID := "1.0.0-candidate"
	repository := filepath.Join(runtimeRoot, "releases", targetID)
	for _, p := range []string{filepath.Join(repository, "bin"), filepath.Join(repository, "config")} {
		if err = os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Copy only the fixture-owned component configuration, not its mutable state.
	entries, err := os.ReadDir(filepath.Join(root, "config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		b, e := os.ReadFile(filepath.Join(root, "config", entry.Name()))
		if e != nil {
			t.Fatal(e)
		}
		testkit.WriteFile(t, filepath.Join(repository, "config", entry.Name()), b, 0600)
	}
	for name, source := range map[string]string{"bin/yard-engine": helperPath, "config/release-transition.json": filepath.Join(repositoryRoot(t), "config/release-transition.json")} {
		b, e := os.ReadFile(source)
		if e != nil {
			t.Fatal(e)
		}
		testkit.WriteFile(t, filepath.Join(repository, name), b, 0700)
	}
	var manifest strings.Builder
	err = filepath.WalkDir(repository, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		relative, e := filepath.Rel(repository, path)
		if e != nil {
			return e
		}
		d := sha256.Sum256(b)
		manifest.WriteString(hex.EncodeToString(d[:]) + "  ./" + relative + "\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(repository, "runtime-files.sha256"), []byte(manifest.String()), 0600)
	for name, b := range fixture.Files {
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
			t.Fatal("unsafe owning fixture path")
		}
		p := filepath.Join(configHome, name)
		if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, p, b, 0600)
	}
	if err = os.Symlink("releases/0.21.0-old", filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("releases/0.18.1-source", filepath.Join(runtimeRoot, "previous")); err != nil {
		t.Fatal(err)
	}
	proof := func(id, version string) map[string]any {
		dir := filepath.Join(runtimeRoot, "releases", id)
		fd, e := os.Open(dir)
		if e != nil {
			t.Fatal(e)
		}
		identity, e := releaseowner.IdentityOf(fd)
		fd.Close()
		if e != nil {
			t.Fatal(e)
		}
		m, e := os.ReadFile(filepath.Join(dir, "runtime-files.sha256"))
		if e != nil {
			t.Fatal(e)
		}
		image, e := os.ReadFile(filepath.Join(dir, "bin/yard-engine"))
		if e != nil {
			t.Fatal(e)
		}
		mh, ih := sha256.Sum256(m), sha256.Sum256(image)
		return map[string]any{"release": id, "version": version, "artifactDigest": hex.EncodeToString(mh[:]), "engineDigest": hex.EncodeToString(ih[:]), "root": identity}
	}
	old, source, target := proof("0.21.0-old", "0.21.0"), proof("0.18.1-source", "0.18.1"), proof(targetID, "1.0.0")
	for _, item := range []struct{ id, version string }{{"0.21.0-old", "0.21.0"}, {"0.18.1-source", "0.18.1"}, {targetID, "1.0.0"}} {
		output, err := exec.Command(filepath.Join(runtimeRoot, "releases", item.id, "bin/yard-engine"), "--version").Output()
		if err != nil || !strings.Contains(string(output), " "+item.version) {
			t.Fatalf("sealed fixture version probe differs for %s: %v", item.id, err)
		}
	}
	if old["artifactDigest"] != string(releasetransition.AffectedMigrationOwnerManifest) {
		t.Fatal("authentic old manifest pin differs")
	}
	registry, e := os.ReadFile(filepath.Join(repository, "config/release-transition.json"))
	if e != nil {
		t.Fatal(e)
	}
	server, e := testkit.NewIncusServer(testkit.TempDir(t))
	if e != nil {
		t.Fatal(e)
	}
	defer server.Close()
	environment = append(environment, "SUBYARD_INCUS_SOCKET="+server.SocketPath, "SUBYARD_POWER_RECONCILER_PATH="+filepath.Join(root, "absent-power"), "SUBYARD_POWER_UNIT_PATH="+filepath.Join(root, "absent-unit"))
	options := map[string]any{"dataHome": dataHome, "configHome": configHome, "runtimeRoot": runtimeRoot, "repositoryRoot": repository, "yard": "default", "releases": map[string]any{"from": "0.21.0-old", "target": targetID, "previous": "0.18.1-source"}, "artifactDigest": target["artifactDigest"], "registryRaw": registry, "catalog": releasetransition.BuiltinCapabilityCatalog().Descriptors(), "inheritedSettingIds": []string{}, "rollbackTarget": nil, "migrationCheckpoint": true, "sourceIngress": nil, "sourceArtifact": old, "targetArtifact": target, "previousArtifact": source, "candidateVersion": "1.0.0", "migrationSources": map[string]any{"source": source, "previous": nil}}
	var controllers sync.WaitGroup
	defer controllers.Wait()
	process := startRustOwnerWithBudget(t, ownerPath, helperPath, environment, repository, true, 5*time.Minute)
	defer process.close()
	reached := make(chan struct{}, 1)
	interrupted := false
	advance := func(p *rustOwnerChild) {
		controllers.Add(1)
		go func() {
			defer controllers.Done()
			reader := bufio.NewReader(p.control)
			for {
				p.control.SetReadDeadline(time.Now().Add(5 * time.Minute))
				phase, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				t.Log("migration phase", strings.TrimSpace(phase))
				if !interrupted && cut != "" && strings.TrimSpace(phase) == strings.SplitN(cut, "/", 2)[0] {
					reached <- struct{}{}
					return
				}
				if _, err = p.control.Write([]byte{'R'}); err != nil {
					return
				}
			}
		}()
	}
	advance(process)
	if r := process.exchange(map[string]any{"op": "open", "options": options}); r.Error != "" {
		t.Fatal("open", r.Error)
	}
	review := process.exchange(map[string]any{"op": "inspect-migration-owner", "replacement": fixture.Replacement})
	if review.Error != "" || review.Inspection == nil || len(review.Inspection.Blockers) != 0 {
		t.Fatalf("review %s %#v", review.Error, review.Inspection)
	}
	original, _ := os.ReadFile(filepath.Join(configHome, "release-transition/v2/journal.json"))
	if string(original) != string(fixture.Files["release-transition/v2/journal.json"]) {
		t.Fatal("inspection changed predecessor")
	}
	if cut == "" {
		outcome := process.exchange(map[string]any{"op": "execute", "plan": review.Inspection.Plan, "authorization": "fixture-private-consent"})
		if outcome.Error != "" || outcome.Outcome == nil || outcome.Outcome.Status != releasetransition.StatusReady {
			t.Fatalf("migration owner outcome %s %#v", outcome.Error, outcome.Outcome)
		}
		process.socket.Close()
		if err = process.cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	} else {
		if err = process.encoder.Encode(map[string]any{"op": "execute", "plan": review.Inspection.Plan, "authorization": "fixture-private-consent"}); err != nil {
			t.Fatal(err)
		}
		process.admit(review.Inspection.Plan, "")
		early := make(chan rustOwnerReply, 1)
		decoded := make(chan error, 1)
		go func() { var reply rustOwnerReply; e := process.decoder.Decode(&reply); early <- reply; decoded <- e }()
		select {
		case <-reached:
		case reply := <-early:
			t.Fatalf("migration returned before requested cut: %#v (decode %v)", reply, <-decoded)
		case <-time.After(5 * time.Minute):
			t.Fatal("migration cut not reached")
		}
		if err = process.cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		if err = process.cmd.Wait(); err == nil {
			t.Fatal("migration owner kill did not interrupt")
		}
		<-early
		<-decoded
		process.socket.Close()
		process.control.Close()
		controllers.Wait()
		interrupted = true
		durable, e := os.ReadFile(filepath.Join(configHome, "release-transition/v2/journal.json"))
		if e != nil {
			t.Fatal(e)
		}
		if strings.HasPrefix(cut, "after-migration-owner-receipt") && !bytes.Equal(durable, original) {
			t.Fatal("receipt cut overwrote predecessor")
		}
		if cut == "after-migration-owner-journal" && bytes.Equal(durable, original) {
			t.Fatal("successor cut retained predecessor")
		}
		if strings.HasSuffix(cut, "/source-drift") {
			path := filepath.Join(runtimeRoot, "releases/0.18.1-source/config/release-transition.json")
			raw, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			testkit.WriteFile(t, path, append(raw, '\n'), 0600)
		}
	}
	process = startRustOwnerWithBudget(t, ownerPath, helperPath, environment, repository, true, 5*time.Minute)
	defer process.close()
	advance(process)
	if strings.HasSuffix(cut, "/source-drift") {
		type leaf struct {
			SHA  [32]byte
			Mode os.FileMode
		}
		protected := func() map[string]leaf {
			result := map[string]leaf{}
			e := filepath.WalkDir(configHome, func(path string, entry os.DirEntry, e error) error {
				if e != nil {
					return e
				}
				if entry.IsDir() {
					return nil
				}
				info, e := entry.Info()
				if e != nil {
					return e
				}
				raw, e := os.ReadFile(path)
				if e != nil {
					return e
				}
				rel, e := filepath.Rel(configHome, path)
				if e != nil {
					return e
				}
				result[rel] = leaf{sha256.Sum256(raw), info.Mode()}
				return nil
			})
			if e != nil {
				t.Fatal(e)
			}
			return result
		}
		protectedBefore := protected()
		if e := process.encoder.Encode(map[string]any{"op": "open", "options": options}); e != nil {
			t.Fatal(e)
		}
		var refused rustOwnerReply
		e := process.decoder.Decode(&refused)
		if e == nil && refused.Error == "" {
			t.Fatal("changed source admitted original receipt")
		}
		process.socket.Close()
		process.control.Close()
		_ = process.cmd.Wait()
		if !maps.Equal(protectedBefore, protected()) {
			t.Fatal("source admission refusal changed protected namespace")
		}
		raw, e := os.ReadFile(filepath.Join(configHome, "release-transition/v2/journal.json"))
		if e != nil || !bytes.Equal(raw, original) {
			t.Fatal("source admission failure changed predecessor")
		}
		return
	}
	if r := process.exchange(map[string]any{"op": "open", "options": options}); r.Error != "" {
		t.Fatal("resumeopen", r.Error)
	}
	var resumed rustOwnerReply
	if strings.HasPrefix(cut, "after-migration-owner-receipt") {
		reviewed := process.exchange(map[string]any{"op": "inspect-migration-owner", "replacement": fixture.Replacement})
		if strings.HasSuffix(cut, "/source-drift") {
			if reviewed.Error == "" || reviewed.Admission != nil {
				t.Fatalf("drift admitted receipt continuation: %#v", reviewed)
			}
			raw, e := os.ReadFile(filepath.Join(configHome, "release-transition/v2/journal.json"))
			if e != nil || !bytes.Equal(raw, original) {
				t.Fatal("receipt refusal overwrote predecessor")
			}
			return
		}
		if reviewed.Error != "" || reviewed.Inspection == nil || reviewed.Admission == nil {
			t.Fatalf("missing checked receipt admission: %#v", reviewed)
		}
		goal := releasetransition.Goal{Target: releasetransition.ReleaseID(targetID), Direction: releasetransition.DirectionActivateTarget}
		if e := releasetransition.ValidateProcessInspection(goal, *reviewed.Inspection); e != nil {
			t.Fatal("frozen receipt inspection", e)
		}
		raw, e := json.Marshal(releasetransition.ProcessResponse{SchemaVersion: 1, ActivationReconciliationOwned: true, Inspection: reviewed.Inspection})
		if e != nil {
			t.Fatal(e)
		}
		var roundtrip releasetransition.ProcessResponse
		if e = json.Unmarshal(raw, &roundtrip); e != nil {
			t.Fatal(e)
		}
		if e = releasetransition.ValidateProcessInspection(goal, *roundtrip.Inspection); e != nil {
			t.Fatal(e)
		}
		if reviewed.Inspection.Resume == nil || string(*reviewed.Inspection.Resume) != reviewed.Admission.Transaction || reviewed.Inspection.Plan != reviewed.Admission.Plan || !reviewed.Inspection.Assessment.Changed {
			t.Fatal("receipt public/private continuation identities differ")
		}
		if e = process.encoder.Encode(map[string]any{"op": "resume"}); e != nil {
			t.Fatal(e)
		}
		process.admit(reviewed.Admission.Plan, reviewed.Admission.Transaction)
		if e = process.decoder.Decode(&resumed); e != nil {
			t.Fatal(e, process.stderr.String())
		}
	} else {
		resumed = process.exchange(map[string]any{"op": "resume"})
	}
	if strings.HasSuffix(cut, "/source-drift") {
		if resumed.Error == "" && (resumed.Outcome == nil || resumed.Outcome.Status != releasetransition.StatusOperatorActionRequired) {
			t.Fatalf("receipt source drift accepted: %#v", resumed)
		}
		raw, e := os.ReadFile(filepath.Join(configHome, "release-transition/v2/journal.json"))
		if e != nil || !bytes.Equal(raw, original) {
			t.Fatal("receipt source refusal overwrote predecessor")
		}
		return
	}
	if resumed.Error != "" || resumed.Outcome == nil || resumed.Outcome.Status != releasetransition.StatusReady {
		rustMigrationMaterializedDiagnostic(t, environment, repository, configHome, dataHome, runtimeRoot, targetID)
		t.Fatalf("migration owner resume %s %#v", resumed.Error, resumed.Outcome)
	}
}

// Diagnose only this synthetic fixture's underlying owning reader. Production
// replies keep the safe component diagnostic and never expose the native cause.
func rustMigrationMaterializedDiagnostic(t *testing.T, environment []string, repository, configHome, dataHome, runtimeRoot, target string) {
	t.Helper()
	configFD, err := os.Open(configHome)
	if err != nil {
		t.Log("fixture diagnostic config open failed")
		return
	}
	defer configFD.Close()
	repositoryFD, err := os.Open(repository)
	if err != nil {
		t.Log("fixture diagnostic repository open failed")
		return
	}
	defer repositoryFD.Close()
	physical := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), repositoryFD.Fd())
	program, err := New(Options{RepositoryRoot: physical, WorkingDir: physical, Environment: append(environment, "SUBYARD_HOME="+dataHome, "SUBYARD_CONFIG_HOME="+configHome), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		rustFixtureNativeError(t, "constructor", err)
		return
	}
	c := &releaseOwnerComponents{cli: program, root: configFD, context: releaseowner.Context{Session: strings.Repeat("a", 64), DataHome: dataHome, ConfigHome: configHome, RuntimeRoot: runtimeRoot, RepositoryRoot: repository, PhysicalRepositoryRoot: physical, Target: target, Direction: releasetransition.DirectionActivateTarget, Releases: releasetransition.ReleasePair{From: "0.21.0-old", Target: releasetransition.ReleaseID(target)}, Artifact: strings.Repeat("a", 64), Yard: "default"}, inputs: map[string]config.PersistentFileSnapshot{}, overlays: map[string]config.PersistentFileSnapshot{}, loaded: map[string]config.Loaded{}, prepared: map[string]releaseSettingsPrepared{}, activation: map[string]releaseActivationPrepared{}}
	defer c.Close()
	_, _, err = c.CaptureInventory(context.Background())
	if err == nil {
		_, err = c.CaptureActivation(context.Background(), "materialized-config", true)
	}
	if err == nil {
		t.Log("fixture direct retained-root materialized observation succeeded")
		return
	}
	var materialized materializedConfigActivationError
	if errors.As(err, &materialized) {
		err = materialized.cause
	}
	rustFixtureNativeError(t, "materialized-reader", err)
}

// Preserve the frozen v0.11.1 bridge using an actual published owner's
// interrupted canonical journal and evidence, copied into a private fixture.
func TestRustOwnerV0111ReplacementOperation(t *testing.T) {
	ownerPath, helperPath, original := os.Getenv("SUBYARD_RUST_OWNER_EXECUTABLE"), os.Getenv("SUBYARD_RUST_NATIVE_HELPER"), os.Getenv("SUBYARD_RUST_V0111_FIXTURE")
	if ownerPath == "" || helperPath == "" || original == "" {
		t.Skip("opt-in authentic v0.11.1 post-activation replacement")
	}
	if !strings.HasPrefix(original, "/tmp/subyard-") {
		t.Fatal("historical fixture must be owned under /tmp")
	}
	root, environment, _ := nativeFixture(t)
	fixture := filepath.Join(root, "historical")
	if output, e := exec.Command("cp", "-a", original, fixture).CombinedOutput(); e != nil {
		t.Fatalf("copy historical fixture: %v %s", e, output)
	}
	configHome, runtimeRoot, dataHome := filepath.Join(fixture, "config"), filepath.Join(fixture, "runtime"), filepath.Join(root, "owner-data")
	if e := os.Mkdir(dataHome, 0700); e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadFile(filepath.Join(configHome, "release-transition/v2/journal.json"))
	if e != nil {
		t.Fatal(e)
	}
	predecessor, e := releasetransition.ParseJournal(before)
	if e != nil || predecessor.Checkpoint != releasetransition.JournalReconciling {
		t.Fatal("canonical historical cut differs", e)
	}
	sourceID := string(predecessor.Releases.Target)
	targetID := "1.0.0-bridge-candidate"
	repository := filepath.Join(runtimeRoot, "releases", targetID)
	if e = os.Mkdir(repository, 0700); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"config", "scripts"} {
		if output, e := exec.Command("cp", "-a", filepath.Join(repositoryRoot(t), name), filepath.Join(repository, name)).CombinedOutput(); e != nil {
			t.Fatalf("copy native fixture assets: %v %s", e, output)
		}
	}
	if e = os.Mkdir(filepath.Join(repository, "bin"), 0700); e != nil {
		t.Fatal(e)
	}
	image, e := os.ReadFile(helperPath)
	if e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, filepath.Join(repository, "bin/yard-engine"), image, 0700)
	registry, e := os.ReadFile(filepath.Join(runtimeRoot, "releases", sourceID, "config/release-transition.json"))
	if e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, filepath.Join(repository, "config/release-transition.json"), registry, 0600)
	// All candidate assets are fixture-owned public source. The old source
	// manifest and original frozen protected bytes remain untouched.
	var manifest strings.Builder
	e = filepath.WalkDir(repository, func(path string, entry os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if entry.IsDir() {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		relative, e := filepath.Rel(repository, path)
		if e != nil {
			return e
		}
		h := sha256.Sum256(b)
		manifest.WriteString(hex.EncodeToString(h[:]) + "  ./" + relative + "\n")
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, filepath.Join(repository, "runtime-files.sha256"), []byte(manifest.String()), 0600)
	proof := func(id, version string) map[string]any {
		p := filepath.Join(runtimeRoot, "releases", id)
		fd, e := os.Open(p)
		if e != nil {
			t.Fatal(e)
		}
		identity, e := releaseowner.IdentityOf(fd)
		fd.Close()
		if e != nil {
			t.Fatal(e)
		}
		m, e := os.ReadFile(filepath.Join(p, "runtime-files.sha256"))
		if e != nil {
			t.Fatal(e)
		}
		engine, e := os.ReadFile(filepath.Join(p, "bin/yard-engine"))
		if e != nil {
			t.Fatal(e)
		}
		mh, ih := sha256.Sum256(m), sha256.Sum256(engine)
		return map[string]any{"release": id, "version": version, "artifactDigest": hex.EncodeToString(mh[:]), "engineDigest": hex.EncodeToString(ih[:]), "root": identity}
	}
	source, target, previous := proof(sourceID, "0.11.1"), proof(targetID, "1.0.0"), proof(string(predecessor.Releases.From), "0.9.1")
	server, e := testkit.NewIncusServer(testkit.TempDir(t))
	if e != nil {
		t.Fatal(e)
	}
	defer server.Close()
	environment = append(environment, "SUBYARD_INCUS_SOCKET="+server.SocketPath, "SUBYARD_POWER_RECONCILER_PATH="+filepath.Join(root, "absent-power"), "SUBYARD_POWER_UNIT_PATH="+filepath.Join(root, "absent-unit"))
	pin := sha256.Sum256(before)
	selection := releasetransition.JournalReplacement{Transaction: predecessor.Transaction, Fingerprint: releasetransition.Fingerprint(hex.EncodeToString(pin[:])), Reason: releasetransition.JournalReplacementPostActivationScopeV0111, SourceVersion: "0.11.1"}
	options := map[string]any{"dataHome": dataHome, "configHome": configHome, "runtimeRoot": runtimeRoot, "repositoryRoot": repository, "yard": "recovery-yard", "releases": map[string]any{"from": sourceID, "target": targetID, "previous": string(predecessor.Releases.From)}, "artifactDigest": target["artifactDigest"], "registryRaw": registry, "catalog": releasetransition.BuiltinCapabilityCatalog().Descriptors(), "inheritedSettingIds": []string{}, "migrationCheckpoint": false, "sourceIngress": nil, "sourceArtifact": source, "targetArtifact": target, "previousArtifact": previous, "candidateVersion": "1.0.0", "replacement": selection}
	first := startRustOwnerWithBudget(t, ownerPath, helperPath, environment, repository, false, 5*time.Minute)
	defer first.close()
	if reply := first.exchange(map[string]any{"op": "open", "options": options}); reply.Error != "" {
		t.Fatal("open", reply.Error)
	}
	reviewed := first.exchange(map[string]any{"op": "inspect", "goal": map[string]string{"target": targetID, "direction": "activate-target"}})
	if reviewed.Error != "" || reviewed.Inspection == nil || len(reviewed.Inspection.Blockers) != 0 {
		t.Fatalf("historical bridge review %#v", reviewed)
	}
	unchanged, e := os.ReadFile(filepath.Join(configHome, "release-transition/v2/journal.json"))
	if e != nil || !bytes.Equal(before, unchanged) {
		t.Fatal("bridge inspect changed predecessor")
	}
	result := first.exchange(map[string]any{"op": "execute", "plan": reviewed.Inspection.Plan, "authorization": "fixture-private-consent"})
	if result.Error != "" || result.Outcome == nil || result.Outcome.Status != releasetransition.StatusReady {
		t.Fatalf("historical bridge result %#v", result)
	}
	first.socket.Close()
	if e = first.cmd.Wait(); e != nil {
		t.Fatal(e)
	}
	second := startRustOwnerWithBudget(t, ownerPath, helperPath, environment, repository, false, 5*time.Minute)
	defer second.close()
	if r := second.exchange(map[string]any{"op": "open", "options": options}); r.Error != "" {
		t.Fatal(r.Error)
	}
	resumed := second.exchange(map[string]any{"op": "resume"})
	if resumed.Error != "" || resumed.Outcome == nil || resumed.Outcome.Status != releasetransition.StatusReady || resumed.Outcome.Transaction == nil || *resumed.Outcome.Transaction != *result.Outcome.Transaction {
		raw, _ := json.Marshal(resumed)
		t.Fatalf("historical bridge fresh resume %s", raw)
	}
}

// The cause fingerprint correlates repetitions without printing private native
// messages, command output or configuration values.
func rustFixtureNativeError(t *testing.T, phase string, err error) {
	t.Helper()
	text := err.Error()
	hash := sha256.Sum256([]byte(text))
	exit := -1
	var child *exec.ExitError
	if errors.As(err, &child) {
		exit = child.ExitCode()
	}
	t.Logf("fixture native %s cause type=%T sha256=%x bytes=%d exit=%d canceled=%t deadline=%t missing=%t permission=%t executableMissing=%t", phase, err, hash, len(text), exit, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrNotExist), errors.Is(err, os.ErrPermission), errors.Is(err, exec.ErrNotFound))
}
