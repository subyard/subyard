package uninstall

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func recoveryFixture(t *testing.T) (RecoveryStore, RecoveryPlan) {
	t.Helper()
	root := testkit.TempDir(t)
	installation := Installation{Schema: 1, OperatorUID: uint32(os.Getuid()), DataHome: filepath.Join(root, "custom-data"), ConfigHome: filepath.Join(root, "custom-config"), RuntimeRoot: filepath.Join(root, "custom-runtime"), CacheRoot: filepath.Join(root, "custom-cache")}
	testkit.WriteFile(t, filepath.Join(root, "state"), []byte("approved"), 0600)
	tree, err := Capture(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	return RecoveryStore{Directory: root, UID: uint32(os.Getuid())}, RecoveryPlan{Schema: RecoverySchema, Installation: installation, Stage: RecoveryPrepared, Trees: []Tree{tree}}
}

func TestRecoveryRetainsPackagedCoreDependencies(t *testing.T) {
	source := testkit.TempDir(t)
	paths := []string{"bin/yard", "config/commands.registry", "scripts/teardown-physical.sh", "scripts/lib/teardown-services.sh",
		"internal/adapters/configmaterial/tomli_w.py", "internal/adapters/configmaterial/TOMLI_W_LICENSE", "internal/unrelated.py"}
	var manifest strings.Builder
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(source, path)), 0700); err != nil {
			t.Fatal(err)
		}
		body := []byte("synthetic packaged asset: " + path)
		testkit.WriteFile(t, filepath.Join(source, path), body, 0644)
		fmt.Fprintf(&manifest, "%x  %s\n", sha256.Sum256(body), path)
	}
	testkit.WriteFile(t, filepath.Join(source, "runtime-files.sha256"), []byte(manifest.String()), 0644)
	assets, err := CaptureAssets(source)
	if err != nil {
		t.Fatal(err)
	}
	store, plan := recoveryFixture(t)
	plan.SourceRoot, plan.SourceAssets = source, assets
	if err = store.PublishRuntime(plan); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths[:len(paths)-1] {
		body, err := os.ReadFile(filepath.Join(store.Directory, "runtime", path))
		if err != nil || string(body) != "synthetic packaged asset: "+path {
			t.Fatalf("packaged dependency was not retained: %s: %v", path, err)
		}
	}
	if _, err = os.Lstat(filepath.Join(store.Directory, "runtime/internal/unrelated.py")); !os.IsNotExist(err) {
		t.Fatal("recovery copied unrelated internal source", err)
	}
	testkit.WriteFile(t, filepath.Join(source, paths[5]), []byte("changed dependency"), 0644)
	if _, err = CaptureAssets(source); err == nil {
		t.Fatal("recovery accepted a dependency differing from its installed manifest")
	}
}

func TestCoreProducerCaptureRejectsDriftAndUsesOriginalArtifacts(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"owned", "foreign-template", "unsafe-mode"} {
		t.Run(kind, func(t *testing.T) {
			home := testkit.TempDir(t)
			directory := filepath.Join(home, "units")
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			unit := filepath.Join(directory, "subyard-keys-sync.service")
			body, err := os.ReadFile(filepath.Join(root, "config/systemd/subyard-keys-sync.service.in"))
			if err != nil {
				t.Fatal(err)
			}
			body = []byte(strings.ReplaceAll(string(body), "@YARD_BIN@", filepath.Join(root, "bin/yard")))
			if kind == "foreign-template" {
				body = append(body, []byte("\nExecStart=/foreign\n")...)
			}
			mode := os.FileMode(0644)
			if kind == "unsafe-mode" {
				mode = 0666
			}
			testkit.WriteFile(t, unit, body, mode)
			plan := RecoveryPlan{Installation: Installation{OperatorUID: uint32(os.Getuid()), RuntimeRoot: filepath.Join(home, "runtime")}}
			services, err := CaptureProducers(root, plan, map[string]string{"SUBYARD_OPERATOR_HOME": home, "SUBYARD_KEYS_SYSTEMD_DIR": directory}, []string{"keys"}, 0, ProducerEvidence{})
			if kind != "owned" {
				if err == nil {
					t.Fatal("foreign producer was approved")
				}
				return
			}
			if err != nil || len(services) != 1 || len(services[0].Artifacts) != 1 || services[0].Artifacts[0].Path != unit {
				t.Fatalf("missing exact producer: %#v %v", services, err)
			}
			original := Tree(services[0].Artifacts[0])
			testkit.WriteFile(t, unit, []byte("replacement"), 0644)
			if err := original.Check(); err == nil {
				t.Fatal("changed producer escaped original artifact guard")
			}
		})
	}
}

func TestRecoveryRetainsExactTargetsAndPrivateModes(t *testing.T) {
	store, plan := recoveryFixture(t)
	binding, err := store.Save(plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	saved, _, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Installation.RuntimeRoot != plan.Installation.RuntimeRoot || saved.Trees[0].Path != plan.Trees[0].Path {
		t.Fatal("recovery derived targets from defaults")
	}
	if _, err := store.Save(plan, nil); err == nil {
		t.Fatal("existing recovery overwritten")
	}
	plan.Stage = RecoveryProducersStopped
	if _, err := store.Save(plan, &binding); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(store.Directory, "plan.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("unsafe recovery mode: %v", err)
	}
	if _, err := store.Save(plan, &binding); err == nil {
		t.Fatal("stale previous publication accepted")
	}
}

func TestRecoveryRefusesSpecialPublicationSidecarWithoutReading(t *testing.T) {
	store, plan := recoveryFixture(t)
	binding, err := store.Save(plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(store.Directory, ".plan-next"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		plan.Stage = RecoveryProducersStopped
		_, err := store.Save(plan, &binding)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, domain.ErrPlanStale) {
			t.Fatalf("special sidecar adopted: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("publication blocked on a special sidecar")
	}
}

func TestRecoveryResumesExactInterruptedPublication(t *testing.T) {
	for _, variant := range []string{"same plan", "foreign plan", "longer sidecar"} {
		changed := variant != "same plan"
		t.Run(variant, func(t *testing.T) {
			store, plan := recoveryFixture(t)
			binding, err := store.Save(plan, nil)
			if err != nil {
				t.Fatal(err)
			}
			plan.Stage = RecoveryProducersStopped
			pending := plan
			if changed {
				pending.Installation.DataHome = filepath.Join(store.Directory, "foreign")
			}
			body, err := json.Marshal(pending)
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				body = body[:len(body)/2]
			}
			if variant == "longer sidecar" {
				// The exact plan plus one trailing byte is not a prefix of it.
				pending.Installation.DataHome = plan.Installation.DataHome
				if body, err = json.Marshal(pending); err != nil {
					t.Fatal(err)
				}
				body = append(body, '\n')
			}
			testkit.WriteFile(t, filepath.Join(store.Directory, ".plan-next"), body, 0600)
			_, err = store.Save(plan, &binding)
			if changed && err == nil {
				t.Fatal("foreign pending publication adopted")
			}
			if !changed && err != nil {
				t.Fatal(err)
			}
			saved, _, readErr := store.Read()
			if readErr != nil {
				t.Fatal(readErr)
			}
			expected := RecoveryProducersStopped
			if changed {
				expected = RecoveryPrepared
			}
			if saved.Stage != expected {
				t.Fatalf("stage=%s", saved.Stage)
			}
		})
	}
}

func TestRecoveryRefusesEscapingDescendant(t *testing.T) {
	store, plan := recoveryFixture(t)
	plan.Trees[0].Entries[0].Path = "../foreign"
	if _, err := store.Save(plan, nil); err == nil {
		t.Fatal("escape accepted")
	}
	if _, err := os.Stat(filepath.Join(store.Directory, "plan.json")); !os.IsNotExist(err) {
		t.Fatalf("invalid plan published: %v", err)
	}
}

func TestRecoveryFirstPublicationKeepsOriginalPlan(t *testing.T) {
	for _, kind := range []string{"complete", "truncated", "empty"} {
		t.Run(kind, func(t *testing.T) {
			store, plan := recoveryFixture(t)
			plan.EpochBefore = 41
			body, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "truncated" {
				body = body[:len(body)/2]
			}
			if kind != "empty" {
				testkit.WriteFile(t, filepath.Join(store.Directory, ".plan-next"), body, 0600)
			}
			original, binding, err := store.Read()
			if kind != "complete" {
				if err == nil {
					t.Fatal("incomplete original publication authorized new targets")
				}
				if _, err = os.Stat(filepath.Join(store.Directory, "plan.json")); !os.IsNotExist(err) {
					t.Fatal("assessment mutated protected store")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if binding.Path != filepath.Join(store.Directory, ".plan-next") || original.Installation.RuntimeRoot != plan.Installation.RuntimeRoot || original.EpochBefore != 41 {
				t.Fatal("original pending facts lost")
			}
			if _, err = store.Save(original, nil); err != nil {
				t.Fatal(err)
			}
			saved, _, err := store.Read()
			if err != nil || saved.Installation.RuntimeRoot != plan.Installation.RuntimeRoot || saved.EpochBefore != 41 {
				t.Fatal("first publication retry changed original targets or epoch goal", err)
			}
		})
	}
}

func TestRecoveryRefusesOverflowBeforePublishing(t *testing.T) {
	store, plan := recoveryFixture(t)
	plan.EpochBefore = ^uint64(0)
	if _, err := store.Save(plan, nil); err == nil {
		t.Fatal("overflowed cleanup goal was published")
	}
	if _, err := os.Stat(filepath.Join(store.Directory, "plan.json")); !os.IsNotExist(err) {
		t.Fatal("invalid goal changed the original store")
	}
	if err := plan.Trees[0].Check(); err != nil {
		t.Fatal("invalid goal changed approved data", err)
	}
}

func TestProtectedServicePlanIgnoresMirrorsAndRejectsReplacement(t *testing.T) {
	store, plan := recoveryFixture(t)
	artifact, err := CaptureArtifact(plan.Trees[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	plan.Services = []RecoveryService{{Scope: "user", Units: []string{"subyard-approved.service"}, Artifacts: []ports.TeardownArtifact{artifact}}}
	binding, err := store.Save(plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := store.ReadBody(binding)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	inventory := func(sha string) (string, int) {
		t.Setenv("SUBYARD_TEARDOWN_PLAN", binding.Path)
		t.Setenv("SUBYARD_TEARDOWN_PLAN_SHA256", sha)
		t.Setenv("SUBYARD_TEARDOWN_SERVICES", `[{"scope":"system","units":["subyard-foreign.service"]}]`)
		var output strings.Builder
		code := RunGuard([]string{"service-inventory"}, nil, &output, io.Discard)
		return output.String(), code
	}
	if output, code := inventory(hex.EncodeToString(digest[:])); code != 0 || output != "user\tsubyard-approved.service\n" {
		t.Fatalf("service selection escaped original plan: %s %d", output, code)
	}
	if output, code := inventory(strings.Repeat("0", 64)); code == 0 || output != "" {
		t.Fatal("changed protected plan binding accepted")
	}
	replacement := plan
	replacement.Services[0].Units = []string{"subyard-foreign.service"}
	bytes, err := json.Marshal(replacement)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, binding.Path, bytes, 0600)
	if _, err := store.ReadBody(binding); err == nil {
		t.Fatal("replacement plan became approved transport")
	}
}

func useProducerFixture(t *testing.T, relays string) {
	t.Helper()
	relayUnitDirectory, producerSystemUID = relays, uint32(os.Getuid())
	t.Cleanup(func() { relayUnitDirectory, producerSystemUID = "/etc/systemd/system", 0 })
}

func TestLegacyRelayUsesSerializedApprovedYardProof(t *testing.T) {
	for _, kind := range []string{"approved", "wrong-kind", "wrong-port", "wrong-address", "absent-proof"} {
		t.Run(kind, func(t *testing.T) {
			yard := RecoveryYard{Context: domain.Context{YardKind: domain.YardVM, SSHPort: 24222}, Snapshot: &TeardownSnapshot{State: ports.ReconcileState{Instance: ports.InstanceInfo{Devices: map[string]map[string]string{"eth0": {"ipv4.address": "192.0.2.10"}}}}}}
			switch kind {
			case "wrong-kind":
				yard.Context.YardKind = domain.YardContainer
			case "wrong-port":
				yard.Context.SSHPort++
			case "wrong-address":
				yard.Snapshot.State.Instance.Devices["eth0"]["ipv4.address"] = "192.0.2.11"
			}
			yards := []RecoveryYard{yard}
			if kind == "absent-proof" {
				yards = nil
			}
			// Redirect only the privileged unit directory and root UID; the real
			// producer parser and rendered identity remain exercised.
			directory := testkit.TempDir(t)
			useProducerFixture(t, directory)
			unit := "[Unit]\nDescription=Subyard VM SSH relay to 192.0.2.10:22\nAfter=network-online.target\n\n[Service]\nExecStart=/usr/lib/systemd/systemd-socket-proxyd 192.0.2.10:22\nNoNewPrivileges=yes\nPrivateTmp=yes\nProtectHome=yes\nProtectSystem=strict\nPrivateDevices=yes\nRestrictAddressFamilies=AF_INET AF_INET6\n"
			testkit.WriteFile(t, filepath.Join(directory, "subyard-ssh-relay-24222.service"), []byte(unit), 0644)
			services, err := CaptureProducers("/source", RecoveryPlan{Installation: Installation{RuntimeRoot: "/runtime"}, Yards: yards}, map[string]string{}, []string{"relay"}, 24222, ProducerEvidence{})
			if (err == nil) != (kind == "approved") {
				t.Fatalf("%s: %v", kind, err)
			}
			if err == nil && (len(services) != 1 || !slices.Equal(services[0].Units, []string{"subyard-ssh-relay-24222.socket", "subyard-ssh-relay-24222.service"}) || len(services[0].Artifacts) != 1) {
				t.Fatalf("legacy relay inventory: %+v", services)
			}
		})
	}
}

func TestHistoricalPowerProducerRequiresOriginalTemplateAndHelper(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(filepath.Join(root, "internal/adapters/releaseruntime/testdata/historical-project-creators/power-0.10.0.txt"))
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(filepath.Join(root, "config/systemd/subyard-power-reconcile.service.in"))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"original", "tampered-unit", "foreign-helper", "unbound-dispatcher", "mismatched-original-helper", "current-unit-retained-helper", "old-unit-current-helper", "unsafe-mode"} {
		t.Run(kind, func(t *testing.T) {
			home := testkit.TempDir(t)
			runtimeRoot := filepath.Join(home, "runtime")
			engine := filepath.Join(runtimeRoot, "releases", "old", "bin", "yard-engine")
			if err := os.MkdirAll(filepath.Dir(engine), 0700); err != nil {
				t.Fatal(err)
			}
			helperBody := []byte("sealed original helper\n")
			testkit.WriteFile(t, engine, helperBody, 0700)
			helper := filepath.Join(home, "helper")
			dispatcher := filepath.Join(home, "candidate-engine")
			testkit.WriteFile(t, dispatcher, []byte("current candidate helper\n"), 0700)
			if kind == "old-unit-current-helper" {
				helperBody = []byte("current candidate helper\n")
			}
			if kind == "foreign-helper" || kind == "unbound-dispatcher" {
				helperBody = []byte("foreign helper\n")
			}
			testkit.WriteFile(t, helper, helperBody, 0755)
			if kind == "unbound-dispatcher" {
				dispatcher = helper
			}
			unit := filepath.Join(home, "subyard-power.service")
			body := old
			if kind == "current-unit-retained-helper" {
				body = current
			}
			body = []byte(strings.ReplaceAll(string(body), "@SUBYARD_POWER_RECONCILER@", helper))
			if kind == "tampered-unit" {
				body = append(body, []byte("ExecStart=/foreign\n")...)
			}
			mode := os.FileMode(0644)
			if kind == "unsafe-mode" {
				mode = 0666
			}
			testkit.WriteFile(t, unit, body, mode)
			original := ProducerEvidence{ExecutingEngineSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("current candidate helper\n"))), EngineSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("sealed original helper\n"))), Templates: map[string]string{"subyard-power-reconcile.service.in": string(old)}}
			if kind == "mismatched-original-helper" {
				original.EngineSHA256 = strings.Repeat("0", 64)
			}
			// Only the root-UID owner is adapted to the unprivileged fixture.
			useProducerFixture(t, "/etc/systemd/system")
			services, err := CaptureProducers(root, RecoveryPlan{Installation: Installation{RuntimeRoot: runtimeRoot}}, map[string]string{"SUBYARD_POWER_UNIT_PATH": unit, "SUBYARD_POWER_RECONCILER_PATH": helper, "SUBYARD_DISPATCHER_PATH": dispatcher}, []string{"power"}, 0, original)
			if (err == nil) != (kind == "original" || kind == "current-unit-retained-helper" || kind == "old-unit-current-helper") {
				t.Fatalf("historical producer %s: %v", kind, err)
			}
			if err != nil {
				return
			}
			if len(services) != 1 || len(services[0].Artifacts) != 2 {
				t.Fatalf("missing bound original producer: %+v", services)
			}
			originalArtifact := Tree(services[0].Artifacts[0])
			testkit.WriteFile(t, originalArtifact.Path, []byte("changed after capture"), 0644)
			if originalArtifact.Check() == nil {
				t.Fatal("original producer drift escaped stale check")
			}
		})
	}
}

func completedStore(t *testing.T) RecoveryStore {
	t.Helper()
	store, plan := recoveryFixture(t)
	store.Directory = filepath.Join(store.Directory, "data.uninstall")
	if err := os.Mkdir(store.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(plan, nil); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"runtime/bin/yard", "runtime/bin/yard-engine", "runtime/scripts/lib/host.sh"} {
		path := filepath.Join(store.Directory, file)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		body := []byte(file)
		if file == "runtime/bin/yard" {
			body = []byte("#!/bin/sh\nexit 0\n")
		}
		testkit.WriteFile(t, path, body, 0700)
	}
	return store
}

// Final store removal keeps the plan until last: an interruption at any
// observation leaves the store absent, empty or still readable, and the
// printed retry runs as printed or first says to install a compatible release.
func TestStoreRemovalInterruptionNeverStrandsContentWithoutAPlan(t *testing.T) {
	for _, onPath := range []bool{false, true} {
		t.Run(fmt.Sprintf("yard-on-path=%v", onPath), func(t *testing.T) {
			tools := testkit.TempDir(t)
			if onPath {
				testkit.WriteFile(t, filepath.Join(tools, "yard"), []byte("#!/bin/sh\nexit 0\n"), 0700)
			}
			t.Setenv("PATH", tools)
			storeRemovalInterruptions(t)
		})
	}
}

func storeRemovalInterruptions(t *testing.T) {
	t.Cleanup(func() { statAt = factAt })
	for limit := 1; ; limit++ {
		store := completedStore(t)
		calls := 0
		statAt = func(fd int, name, relative string) (Entry, error) {
			if calls++; calls > limit {
				return Entry{}, errors.New("intentional interruption")
			}
			return factAt(fd, name, relative)
		}
		err := store.Remove()
		statAt = factAt
		names, statErr := os.ReadDir(store.Directory)
		if _, _, readErr := store.Read(); statErr == nil && len(names) > 0 && readErr != nil {
			t.Fatalf("observation %d stranded store content without a plan: %v", limit, readErr)
		}
		if err == nil {
			if !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("completed removal left the store")
			}
			return
		}
		hint := store.ResumeHint()
		lines := strings.Split(strings.TrimSuffix(hint, "\n"), "\n")
		command := strings.Fields(lines[len(lines)-1])
		if len(command) != 3 || command[1] != "_uninstall-resume" || command[2] != store.Directory {
			t.Fatalf("observation %d: printed retry %q", limit, hint)
		}
		install := strings.Contains(hint, "install a compatible Subyard version")
		if executable, lookErr := exec.LookPath(command[0]); lookErr != nil {
			if !install {
				t.Fatalf("observation %d: printed retry is not runnable: %v", limit, lookErr)
			}
		} else if install || exec.Command(executable, command[1:]...).Run() != nil {
			t.Fatalf("observation %d: runnable retry %q did not run as printed", limit, hint)
		}
		if err := store.Remove(); err != nil {
			t.Fatalf("observation %d: exact retry failed: %v", limit, err)
		}
		if _, err := os.Lstat(store.Directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("observation %d: exact retry left the store", limit)
		}
		if limit > 1000 {
			t.Fatal("removal never completed")
		}
	}
}

// An entry that appears after the first pass refuses before the plan is unlinked.
func TestStoreRemovalFinalPassRefusesUnexpectedEntry(t *testing.T) {
	store := completedStore(t)
	keep := filepath.Join(store.Directory, "z-keep")
	t.Cleanup(func() { statAt = factAt })
	statAt = func(fd int, name, relative string) (Entry, error) {
		if _, err := os.Lstat(filepath.Join(store.Directory, "runtime")); errors.Is(err, os.ErrNotExist) && !lexists(keep) {
			testkit.WriteFile(t, keep, []byte("foreign"), 0600)
		}
		return factAt(fd, name, relative)
	}
	if err := store.Remove(); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("final pass accepted an unexpected entry: %v", err)
	}
	statAt = factAt
	if _, _, err := store.Read(); err != nil || !lexists(keep) {
		t.Fatalf("unexpected entry or plan removed: %v", err)
	}
}
