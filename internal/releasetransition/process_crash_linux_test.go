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
	"io"
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
	tm "github.com/Subyard/Subyard/internal/testyardmigration"
)

type crashTrace struct {
	store   *rt.POSIXV2Store
	rows    []rt.JournalStep
	cuts    map[string]bool
	barrier func(string) error
}

func (trace *crashTrace) event(origin, point string) error {
	snapshot, err := trace.store.ReadCurrentJournal()
	if err != nil {
		return err
	}
	if !snapshot.Exists {
		return nil
	}
	journal, err := rt.ParseJournal(snapshot.Payload)
	if err != nil {
		return err
	}
	if len(trace.rows) == 0 {
		trace.rows = slices.Clone(journal.Steps)
	}
	row := ""
	for _, step := range journal.Steps {
		if step.Checkpoint != rt.StepVerified {
			row = step.ID
			break
		}
	}
	key := ""
	if origin == "store" && point == "after-publish-before-dir-fsync" {
		for _, step := range journal.Steps {
			if step.Checkpoint == rt.StepApplied {
				key = "row/" + step.ID + "/applied"
				break
			}
		}
	}
	if origin == "transition" {
		switch point {
		case "after-step-evidence":
			key = "row/" + row + "/evidence"
		case "after-step-applied":
			key = "row/" + row + "/applied"
		case "after-step-verified", "after-ledger-verified":
			for _, step := range journal.Steps {
				if step.Checkpoint == rt.StepVerified {
					key = "row/" + step.ID + "/verified"
				}
			}
		case "after-recovery":
			key = "recovery/" + row
		case "after-settings-cas", "after-owner-registration-commit", "after-ledger-cas":
			key = "effect/" + row + "/" + point
		case "after-journal-authorized", "after-activation-intent", "after-target-active", "after-reconciling", "before-journal-complete", "after-journal-complete", "before-recovery-gc", "before-checkpoint-publication", "after-checkpoint-publication":
			key = "global/" + point
		}
	}
	if origin == "links" {
		key = "global/links/" + point
	}
	if key != "" {
		return trace.hit(key)
	}
	return nil
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

type crashOwner struct {
	options  tm.Options
	original tm.Prepared
	trace    *crashTrace
}

func (owner *crashOwner) Prepare(ctx context.Context, view rt.V2SettingsSnapshotView) (rt.OwnerRegistrationObservation, error) {
	var before tm.Prepared
	var err error
	if view != nil {
		before, err = tm.PrepareProspective(ctx, owner.options, view.ReadSnapshot)
	} else {
		before, err = tm.Prepare(ctx, owner.options)
	}
	if err != nil {
		return rt.OwnerRegistrationObservation{}, err
	}
	state := rt.OwnerRegistrationCurrent
	if before.State == tm.StateLegacyDirectory {
		state = rt.OwnerRegistrationLegacyDirectory
		owner.original = before
	} else if before.State != tm.StateCurrent {
		return rt.OwnerRegistrationObservation{}, fmt.Errorf("unexpected owner topology %s", before.State)
	}
	return rt.OwnerRegistrationObservation{State: state, Registration: rt.Fingerprint(before.RegistrationDigest), Overrides: rt.Fingerprint(before.OverridesDigest), Controller: rt.Fingerprint(before.ControllerDigest), SharedImages: before.SharedImages}, nil
}
func (owner *crashOwner) prepared(before rt.OwnerRegistrationObservation) tm.Prepared {
	return tm.Prepared{State: tm.StateLegacyDirectory, RegistrationDigest: string(before.Registration), OverridesDigest: string(before.Overrides), ControllerDigest: string(before.Controller), SharedImages: before.SharedImages}
}
func (owner *crashOwner) Observe(ctx context.Context, before rt.OwnerRegistrationObservation) (rt.OwnerRegistrationProgress, error) {
	options := owner.options
	options.RecoveryToken = before.RecoveryToken
	options.TerminalCleanup = before.TerminalCleanup
	progress, err := tm.ObserveProgress(ctx, options, owner.prepared(before))
	if err != nil {
		return "", err
	}
	if progress == tm.ProgressDesired {
		return rt.OwnerRegistrationDesired, nil
	}
	if progress == tm.ProgressExpected {
		return rt.OwnerRegistrationExpected, nil
	}
	return rt.OwnerRegistrationInProgress, nil
}
func (owner *crashOwner) Commit(ctx context.Context, before rt.OwnerRegistrationObservation) error {
	options := owner.options
	options.RecoveryToken = before.RecoveryToken
	options.TerminalCleanup = before.TerminalCleanup
	if err := tm.Commit(ctx, options, owner.prepared(before)); err != nil {
		return err
	}
	if !before.TerminalCleanup {
		return owner.trace.hit("native/legacy/commit-return")
	}
	return nil
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

func newClassicCrashFixture(t *testing.T) *crashFixture {
	t.Helper()
	return classicCrashFixtureAt(t, testkit.TempDir(t), true)
}

func classicCrashFixtureAt(t *testing.T, root string, initialize bool) *crashFixture {
	t.Helper()
	home := filepath.Join(root, "config")
	data := filepath.Join(root, "data")
	runtime := filepath.Join(root, "runtime")
	state := filepath.Join(root, "legacy-physical")
	incus := filepath.Join(root, "incus")
	if initialize {
		for _, path := range []string{filepath.Join(home, "yards/e2e-yard"), filepath.Join(home, "yards/hermes"), data, filepath.Join(runtime, "releases/release-a"), filepath.Join(runtime, "releases/release-b")} {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		testkit.WriteFile(t, filepath.Join(home, "yards/e2e-yard/config.env"), []byte("YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n"), 0o600)
		testkit.WriteFile(t, filepath.Join(home, "yards/hermes/config.env"), []byte("YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\n"), 0o600)
		testkit.WriteFile(t, filepath.Join(home, "config.env"), []byte("# operator configuration\n"), 0o600)
		// Git-synchronized SSH ports chain a same-file successor onto the retired-template row.
		if err := os.MkdirAll(filepath.Join(home, ".sync/settings/yards/hermes"), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(home, ".sync/settings/config.env"), []byte("SSH_PORT=2240\n"), 0o600)
		testkit.WriteFile(t, filepath.Join(home, ".sync/settings/yards/hermes/config.env"), []byte("SSH_PORT=2241\n"), 0o600)
		if err := os.Symlink("releases/release-a", filepath.Join(runtime, "current")); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, state, []byte("legacy\n"), 0o600)
		testkit.WriteFile(t, incus, []byte(`#!/bin/sh
set -eu
case "$*" in
 'project list --format=json') case "$(cat "$D12_STATE")" in legacy) printf '[{"name":"subyard-e2e-yard"}]\n';; current) printf '[{"name":"subyard-test-yard"}]\n';; none) printf '[]\n';; *)exit 2;; esac;;
 'project get subyard-e2e-yard features.images'|'project get subyard-test-yard features.images') printf 'true\n';;
 'list yard-e2e-yard --project subyard-e2e-yard --format=json') printf '[{"name":"yard-e2e-yard","status":"RUNNING"}]\n';;
 *)exit 2;;
esac
`), 0o700)
		testkit.WriteFile(t, filepath.Join(root, "canary"), []byte("unrelated operator data\n"), 0o600)
	}
	store, err := rt.NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	trace := &crashTrace{store: store, cuts: map[string]bool{}}
	owner := &crashOwner{trace: trace, options: tm.Options{Executable: "/bin/false", Incus: incus, ConfigHome: home, DataHome: data, Environment: append(os.Environ(), "D12_STATE="+state), Stdout: io.Discard, Stderr: io.Discard}}
	owner.options.RunYard = func(_ context.Context, yard string, output io.Writer, args ...string) error {
		command := strings.Join(args, " ")
		if command == "check" {
			return nil
		}
		if command == "test-vms status" {
			_, err := io.WriteString(output, "test-vms: down\n")
			return err
		}
		if command == "_migrate reconcile-power-reconciler" {
			return nil
		}
		if command == "teardown --yes" && yard == tm.LegacyYard {
			if err := crashPhysicalEffect(root, state, "none\n", "legacy-teardown"); err != nil {
				return err
			}
			return trace.hit("native/legacy/teardown")
		}
		if command == "init --yes" && yard == tm.CurrentYard {
			if err := crashPhysicalEffect(root, state, "current\n", "current-init"); err != nil {
				return err
			}
			return trace.hit("native/legacy/init")
		}
		return fmt.Errorf("unexpected yard transport %s %s", yard, command)
	}
	payload, err := os.ReadFile("../../config/release-transition.json")
	if err != nil {
		t.Fatal(err)
	}
	var registry rt.RegistryV2
	if err := json.Unmarshal(payload, &registry); err != nil {
		t.Fatal(err)
	}
	registry.Migrations = slices.DeleteFunc(registry.Migrations, func(m rt.MigrationDefinitionV2) bool {
		return m.Kind == "registered-yards-v2-to-v3" || m.Kind == "registered-guest-disk-tmp-v2-to-v3"
	})
	registry.CurrentEpochs["owner-registration"] = 2
	registry.CurrentEpochs["power-metadata"] = 2
	payload, err = json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	links, err := rt.NewRuntimeLinkStore(runtime)
	if err != nil {
		t.Fatal(err)
	}
	candidateConfig, err := filepath.Abs("../../config")
	if err != nil {
		t.Fatal(err)
	}
	options := rt.V2Options{OperatorHome: root, CandidateConfigDir: candidateConfig, ConfigHome: home, RegistryPayload: payload, Releases: rt.ReleasePair{From: "release-a", Target: "release-b"}, ArtifactDigest: crashPin("artifact"), OwnerRegistration: owner, ObserveLinks: func(context.Context) (rt.ReleaseLinks, error) { return links.Observe() }, ActivateLinks: func(_ context.Context, pair rt.ReleasePair) (rt.ReleaseLinks, error) { return links.Activate(pair) }, Reconcilers: []rt.V2ActivationReconciler{&crashActivation{path: filepath.Join(root, "activation"), trace: trace}}, VerifyAuthorization: func(plan rt.PlanToken, grant rt.Authorization) bool { return string(grant) == "grant:"+string(plan) }}
	return &crashFixture{target: "release-b", home: home, root: root, registry: payload, options: options, links: links, trace: trace, signature: crashDomainSignature}
}

func crashPhysicalEffect(root, path, value, event string) error {
	current, _ := os.ReadFile(path)
	if string(current) == value {
		return nil
	}
	if err := crashWrite(path, []byte(value)); err != nil {
		return err
	}
	log := filepath.Join(root, "domain-effects")
	body, _ := os.ReadFile(log)
	return crashWrite(log, append(body, []byte(event+"\n")...))
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
		f = classicCrashFixtureAt(t, root, false)
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
	for _, relative := range []string{"config/config.env", "config/yards/hermes/config.env", "config/yards/test-yard/config.env", "legacy-physical", "domain-effects", "activation", "canary", "config/release-transition/v2/ledger.json"} {
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
		if err != nil {
			t.Fatal(err)
		}
		result["links/"+name] = value
	}
	return result
}

func TestProcessCrashClassicRowsResumeFresh(t *testing.T) {
	runCrashMatrix(t, newClassicCrashFixture, nil)
}

func runCrashMatrix(t *testing.T, build func(*testing.T) *crashFixture, verifyControl func(*testing.T, map[string]bool)) {
	t.Helper()
	control := build(t)
	outcome := crashConverge(t, control, false)
	if outcome.Status != rt.StatusReady {
		t.Fatalf("control: %+v", outcome)
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

func TestProcessCrashDiscovery(t *testing.T) {
	f := newClassicCrashFixture(t)
	transition, err := rt.NewV2Transition(f.options)
	if err != nil {
		t.Fatal(err)
	}
	rt.SetProcessCrashFaultForTest(transition, f.links, f.trace.event)
	inspection, err := transition.Inspect(context.Background(), rt.Goal{Target: "release-b", Direction: rt.DirectionActivateTarget})
	if err != nil {
		t.Fatal(err)
	}
	if len(inspection.Blockers) != 0 {
		t.Fatalf("discovery blocked: %+v", inspection.Blockers)
	}
	outcome, err := transition.Converge(context.Background(), rt.Execution{Plan: inspection.Plan, Authorization: rt.Authorization("grant:" + string(inspection.Plan))})
	if err != nil || outcome.Status != rt.StatusReady || len(outcome.Warnings) != 0 {
		t.Fatalf("control did not converge: %+v %v", outcome, err)
	}
	var registry rt.RegistryV2
	json.Unmarshal(f.registry, &registry)
	kinds := []string{}
	for _, m := range registry.Migrations {
		kinds = append(kinds, m.Kind)
	}
	counts := map[string]int{"N": len(f.trace.rows), "R": 0, "M": 0, "G": 0, "rowCuts": 0, "effectCuts": 0, "total": len(f.trace.cuts)}
	for key := range f.trace.cuts {
		switch {
		case strings.HasPrefix(key, "recovery/"):
			counts["R"]++
		case strings.HasPrefix(key, "native/"):
			counts["M"]++
		case strings.HasPrefix(key, "global/"):
			counts["G"]++
		case strings.HasPrefix(key, "row/"):
			counts["rowCuts"]++
		case strings.HasPrefix(key, "effect/"):
			counts["effectCuts"]++
		}
	}
	cuts := []string{}
	for cut := range f.trace.cuts {
		cuts = append(cuts, cut)
	}
	slices.Sort(cuts)
	data, _ := json.Marshal(map[string]any{"family": "classic", "kinds": kinds, "counts": counts, "rows": f.trace.rows, "cuts": cuts})
	t.Log(string(data))
	if counts["rowCuts"] != 3*counts["N"] {
		t.Fatalf("missing actual row checkpoint cuts: %v", counts)
	}
}
