package releasetransition_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	rt "github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

type crashTrace struct {
	store   *rt.POSIXV2Store
	counts  map[string]int
	cuts    map[string]bool
	barrier func(string) error
}

// Enumerate every invocation of every existing fault seam after the initial
// journal is published. Ordinals distinguish repeated store writes/fsyncs; no
// allow-list can silently omit a newly added durable stopping point.
func (trace *crashTrace) event(origin, point string) error {
	snapshot, err := trace.store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return err
	}
	key := origin + "/" + point
	trace.counts[key]++
	return trace.hit(fmt.Sprintf("%s/%03d", key, trace.counts[key]))
}

func (trace *crashTrace) hit(key string) error {
	if trace.cuts[key] {
		return nil
	}
	trace.cuts[key] = true
	if trace.barrier != nil {
		return trace.barrier(key)
	}
	return nil
}

func crashWrite(path string, body []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".physical-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0o600); err == nil {
		_, err = file.Write(body)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

type crashActivation struct {
	path  string
	trace *crashTrace
}

func (*crashActivation) ID() string { return "durable-activation-marker" }
func crashPin(value string) rt.Fingerprint {
	sum := sha256.Sum256([]byte(value))
	return rt.Fingerprint(hex.EncodeToString(sum[:]))
}
func (actor *crashActivation) Observe(context.Context, rt.ReleasePair, rt.ReleaseLinks) (rt.V2ActivationObservation, error) {
	_, err := os.Stat(actor.path)
	ready := err == nil
	if err != nil && !os.IsNotExist(err) {
		return rt.V2ActivationObservation{}, err
	}
	actual := crashPin("activation-before")
	if ready {
		actual = crashPin("activation-desired")
	}
	return rt.V2ActivationObservation{Actual: actual, Desired: crashPin("activation-desired"), Converged: ready}, nil
}
func (actor *crashActivation) Reconcile(context.Context, rt.ReleaseLinks) error {
	if _, err := os.Stat(actor.path); err == nil {
		return nil
	}
	if err := crashWrite(actor.path, []byte("activated\n")); err != nil {
		return err
	}
	return actor.trace.hit("native/activation/marker")
}

type crashFixture struct {
	links          *rt.RuntimeLinkStore
	family, target string
	home, root     string
	registry       []byte
	options        rt.V2Options
	trace          *crashTrace
	signature      func(*testing.T, string) map[string]string
}

const crashFamilyFresh = "fresh"
const crashFamilyUpdate = "update"
const crashFamilyCheckpointed = "update-checkpointed"

func crashFamily(family string) func(*testing.T) *crashFixture {
	return func(t *testing.T) *crashFixture { return classicCrashFixtureAt(t, testkit.TempDir(t), true, family) }
}

func classicCrashFixtureAt(t *testing.T, root string, initialize bool, family string) *crashFixture {
	t.Helper()
	home, runtime := filepath.Join(root, "config"), filepath.Join(root, "runtime")
	source, target := rt.ReleaseID("0.28.1-aaaaaaaaaaaa"), rt.ReleaseID("0.29.0-bbbbbbbbbbbb")
	if family == crashFamilyFresh {
		source = target
	}
	if initialize {
		for _, path := range []string{home, filepath.Join(runtime, "releases", string(source)), filepath.Join(runtime, "releases", string(target))} {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink("releases/"+string(source), filepath.Join(runtime, "current")); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, "canary"), []byte("unrelated operator data\n"), 0o600)
		if family != crashFamilyFresh {
			rt.SeedProcessCrashHistoryForTest(t, home, family == crashFamilyCheckpointed)
		}
	}
	store, err := rt.NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	trace := &crashTrace{store: store, cuts: map[string]bool{}, counts: map[string]int{}}
	payload, err := os.ReadFile("../../config/release-transition.json")
	if err != nil {
		t.Fatal(err)
	}
	links, err := rt.NewRuntimeLinkStore(runtime)
	if err != nil {
		t.Fatal(err)
	}
	options := rt.V2Options{ConfigHome: home, RegistryPayload: payload,
		Releases: rt.ReleasePair{From: source, Target: target}, Direction: rt.DirectionActivateTarget,
		ArtifactDigest: crashPin("artifact"), MigrationCheckpoint: true, SourceMigrationCheckpoint: true,
		ObserveLinks:        func(context.Context) (rt.ReleaseLinks, error) { return links.Observe() },
		ActivateLinks:       func(_ context.Context, pair rt.ReleasePair) (rt.ReleaseLinks, error) { return links.Activate(pair) },
		Reconcilers:         []rt.V2ActivationReconciler{&crashActivation{path: filepath.Join(root, "activation"), trace: trace}},
		VerifyAuthorization: func(plan rt.PlanToken, grant rt.Authorization) bool { return string(grant) == "grant:"+string(plan) },
	}
	return &crashFixture{family: family, target: string(target), home: home, root: root, registry: payload, options: options, links: links, trace: trace, signature: crashDomainSignature}
}

func crashConverge(t *testing.T, f *crashFixture, resume bool) rt.Outcome {
	t.Helper()
	transition, err := rt.NewV2Transition(f.options)
	if err != nil {
		t.Fatal(err)
	}
	rt.SetProcessCrashFaultForTest(transition, f.links, f.trace.event)
	inspection, err := transition.Inspect(context.Background(), rt.Goal{Target: rt.ReleaseID(f.target), Direction: rt.DirectionActivateTarget})
	if err != nil || len(inspection.Blockers) != 0 {
		t.Fatalf("inspection: %+v %v", inspection, err)
	}
	execution := rt.Execution{Plan: inspection.Plan}
	if !resume {
		execution.Authorization = rt.Authorization("grant:" + string(inspection.Plan))
	}
	outcome, err := transition.Converge(context.Background(), execution)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func TestProcessCrashWriter(t *testing.T) {
	root := os.Getenv("SUBYARD_TEST_CRASH_ROOT")
	if root == "" {
		t.Skip("child helper")
	}
	var f *crashFixture
	if strings.HasPrefix(os.Getenv("SUBYARD_TEST_CRASH_FAMILY"), crashFamilyRegistered) {
		f = registeredCrashFixtureAt(t, root, false, os.Getenv("SUBYARD_TEST_CRASH_FAMILY"))
	} else {
		f = classicCrashFixtureAt(t, root, false, os.Getenv("SUBYARD_TEST_CRASH_FAMILY"))
	}
	cut := os.Getenv("SUBYARD_TEST_CRASH_CUT")
	f.trace.barrier = func(key string) error {
		if key == cut {
			fmt.Println("D12_READY " + key)
			for {
				time.Sleep(time.Hour)
			}
		}
		return nil
	}
	outcome := crashConverge(t, f, os.Getenv("SUBYARD_TEST_CRASH_RESUME") == "1")
	if outcome.Status != rt.StatusReady || len(outcome.Warnings) != 0 {
		t.Fatalf("fresh process did not reach Ready: %+v", outcome)
	}
}

func crashChild(t *testing.T, f *crashFixture, cut string, resume, kill bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessCrashWriter$", "-test.v")
	command.Env = append(os.Environ(), "SUBYARD_TEST_CRASH_ROOT="+f.root, "SUBYARD_TEST_CRASH_CUT="+cut, "SUBYARD_TEST_CRASH_FAMILY="+f.family)
	if resume {
		command.Env = append(command.Env, "SUBYARD_TEST_CRASH_RESUME=1")
	}
	if !kill {
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("fresh process: %v %s", err, out)
		}
		return
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if command.ProcessState == nil {
			command.Process.Kill()
			command.Wait()
		}
	}()
	scanner := bufio.NewScanner(stdout)
	ready := false
	var output strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		output.WriteString(line + "\n")
		if line == "D12_READY "+cut {
			ready = true
			break
		}
	}
	if !ready {
		command.Wait()
		t.Fatalf("cut not reached: %s %s", output.String(), stderr.String())
	}
	if err := command.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	err = command.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("writer did not die by SIGKILL: %v", err)
	}
}

func crashDomainSignature(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, relative := range []string{"activation", "canary", "config/release-transition/v2/ledger.json", "config/release-transition/v2/history-checkpoint.json"} {
		body, err := os.ReadFile(filepath.Join(root, relative))
		if os.IsNotExist(err) {
			result[relative] = "absent"
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		result[relative] = strings.ReplaceAll(string(body), root, "$ROOT")
	}
	for _, name := range []string{"current", "previous"} {
		value, err := os.Readlink(filepath.Join(root, "runtime", name))
		if os.IsNotExist(err) {
			result["links/"+name] = "absent"
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		result["links/"+name] = value
	}
	return result
}

func TestProcessCrashUpdatesResumeFresh(t *testing.T) {
	for _, family := range []string{crashFamilyUpdate, crashFamilyCheckpointed, crashFamilyFresh} {
		t.Run(family, func(t *testing.T) { runCrashMatrix(t, crashFamily(family), nil) })
	}
}

func runCrashMatrix(t *testing.T, build func(*testing.T) *crashFixture, verifyControl func(*testing.T, map[string]bool)) {
	t.Helper()
	control := build(t)
	outcome := crashConverge(t, control, false)
	if outcome.Status != rt.StatusReady {
		t.Fatalf("control: %+v", outcome)
	}
	snapshot, err := control.trace.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := rt.ParseJournal(snapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if control.family == crashFamilyFresh {
		if len(journal.Steps) != 1 || journal.Steps[0].ID != "initialize-ledger-v2.initialize" || journal.Steps[0].Checkpoint != rt.StepVerified {
			t.Fatalf("fresh bootstrap did not close its sole baseline step: %#v", journal.Steps)
		}
	} else if len(journal.Steps) != 0 {
		t.Fatalf("completed history replayed migration steps: %#v", journal.Steps)
	}
	// These assertions prove the fixture reaches its intended boundaries. They
	// do not select cuts: every discovered hook invocation below is still killed.
	required := []string{"transition/after-journal-authorized/", "transition/after-target-active/", "transition/after-reconciling/", "transition/before-journal-complete/", "transition/after-journal-complete/", "transition/before-recovery-gc/", "store/after-publish-before-dir-fsync/"}
	if control.family == crashFamilyUpdate || control.family == crashFamilyCheckpointed {
		required = append(required, "transition/after-activation-intent/", "links/after-staged-link/", "links/after-link-exchange/")
	}
	if control.family == crashFamilyFresh {
		required = append(required, "transition/before-baseline-ledger/", "transition/after-baseline-ledger/", "transition/after-step-evidence/", "transition/after-step-verified/")
	}
	if control.family == crashFamilyUpdate || control.family == crashFamilyFresh {
		required = append(required, "transition/before-checkpoint-publication/", "transition/after-checkpoint-publication/")
	}
	for _, prefix := range required {
		found := false
		for cut := range control.trace.cuts {
			found = found || strings.HasPrefix(cut, prefix)
		}
		if !found {
			t.Fatalf("control did not reach %s", prefix)
		}
	}
	if verifyControl != nil {
		verifyControl(t, control.trace.cuts)
	}
	want := control.signature(t, control.root)
	cuts := []string{}
	for cut := range control.trace.cuts {
		cuts = append(cuts, cut)
	}
	slices.Sort(cuts)
	inventory, _ := json.Marshal(map[string]any{"family": control.family, "count": len(cuts), "cuts": cuts})
	t.Log(string(inventory))
	if len(cuts) == 0 {
		t.Fatal("no durable stopping points discovered")
	}
	for _, cut := range cuts {
		t.Run(cut, func(t *testing.T) {
			f := build(t)
			crashChild(t, f, cut, false, true)
			before, err := f.trace.store.ReadCurrentJournal()
			if err != nil || !before.Exists {
				t.Fatal("killed writer lost authorized journal", err)
			}
			journal, err := rt.ParseJournal(before.Payload)
			if err != nil {
				t.Fatal(err)
			}
			crashChild(t, f, "", true, false)
			after, err := f.trace.store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			complete, err := rt.ParseJournal(after.Payload)
			if err != nil || complete.Checkpoint != rt.JournalComplete || complete.Transaction != journal.Transaction || complete.AuthorizationPlan != journal.AuthorizationPlan || complete.AuthorizationDigest != journal.AuthorizationDigest || complete.ResumePlan != journal.ResumePlan || complete.ObservationScope != journal.ObservationScope {
				t.Fatal("resume replaced original transaction/grant", err)
			}
			got := f.signature(t, f.root)
			if !mapsEqual(got, want) {
				t.Fatalf("resumed domain effects differ from uninterrupted control: got=%v want=%v", got, want)
			}
			crashChild(t, f, "", true, false)
			if again := f.signature(t, f.root); !mapsEqual(again, got) {
				t.Fatal("fresh Ready repeat changed domain effects")
			}
			repeated, err := f.trace.store.ReadCurrentJournal()
			if err != nil || !bytes.Equal(after.Payload, repeated.Payload) {
				t.Fatal("fresh Ready repeat rewrote the completed transaction/grant", err)
			}
		})
	}
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}
