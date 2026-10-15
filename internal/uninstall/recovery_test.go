package uninstall

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	return RecoveryStore{Directory: root, UID: uint32(os.Getuid())}, RecoveryPlan{Schema: 1, Installation: installation, Stage: RecoveryPrepared, Trees: []Tree{tree}}
}

func TestRecoveryRetainsPackagedCoreDependencies(t *testing.T) {
	source := testkit.TempDir(t)
	paths := []string{"bin/yard", "config/commands.registry", "scripts/teardown-physical.sh", "scripts/lib/teardown-services.sh", "scripts/lib/teardown-plan.py",
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
			services, err := CaptureProducers(root, plan, map[string]string{"SUBYARD_OPERATOR_HOME": home, "SUBYARD_KEYS_SYSTEMD_DIR": directory}, []string{"keys"}, 0)
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

func TestRecoveryResumesExactInterruptedPublication(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "same plan", true: "foreign plan"}[changed], func(t *testing.T) {
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
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join("..", "..", "scripts", "lib", "teardown-plan.py")
	command := exec.Command(python, "-B", helper, "service-inventory")
	command.Env = append(os.Environ(), "SUBYARD_TEARDOWN_PLAN="+binding.Path, "SUBYARD_TEARDOWN_PLAN_SHA256="+hex.EncodeToString(digest[:]), `SUBYARD_TEARDOWN_SERVICES=[{"scope":"system","units":["subyard-foreign.service"]}]`)
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "user\tsubyard-approved.service\n" {
		t.Fatalf("service selection escaped original plan: %s %v", output, err)
	}
	command = exec.Command(python, "-B", helper, "service-inventory")
	command.Env = append(os.Environ(), "SUBYARD_TEARDOWN_PLAN="+binding.Path, "SUBYARD_TEARDOWN_PLAN_SHA256="+strings.Repeat("0", 64))
	if output, err := command.CombinedOutput(); err == nil || len(output) == 0 {
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

func TestLegacyRelayUsesSerializedApprovedYardProof(t *testing.T) {
	helper, err := filepath.Abs("../../scripts/lib/teardown-plan.py")
	if err != nil {
		t.Fatal(err)
	}
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
			body, err := json.Marshal(struct {
				Root         string
				Installation Installation
				Environment  map[string]string
				Owners       []string
				Yards        []RecoveryYard
				Port         int
			}{"/source", Installation{RuntimeRoot: "/runtime"}, map[string]string{}, []string{"relay"}, yards, 24222})
			if err != nil {
				t.Fatal(err)
			}
			// Redirect only the privileged file adapter. The real producer parser,
			// rendered identity and canonical Go request encoding remain exercised.
			command := exec.Command("python3", "-B", "-c", `
import builtins,glob,importlib.util,io,json,sys
spec=importlib.util.spec_from_file_location("guard",sys.argv[1]);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
glob.glob=lambda _: ["/etc/systemd/system/subyard-ssh-relay-24222.service"]
original=builtins.open
content=b"[Service]\nExecStart=/usr/lib/systemd/systemd-socket-proxyd 192.0.2.10:22\n"
builtins.open=lambda path,*args,**kw:io.BytesIO(content) if path=="/etc/systemd/system/subyard-ssh-relay-24222.service" else original(path,*args,**kw)
m.capture_service=lambda scope,units,files,helpers,uid:dict(scope=scope,units=units,Artifacts=[dict(Path="/approved")])
failed=False
try:result=m.core_producers(json.load(sys.stdin))
except ValueError:failed=True
assert failed==(sys.argv[2]!="approved")
if not failed:assert len(result)==1 and result[0]["units"]==["subyard-ssh-relay-24222.socket","subyard-ssh-relay-24222.service"]
`, helper, kind)
			command.Stdin = bytes.NewReader(body)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v %s", kind, err, output)
			}
		})
	}
}
