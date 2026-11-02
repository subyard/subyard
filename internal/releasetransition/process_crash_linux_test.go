package releasetransition_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func crashPin(value string) rt.Fingerprint {
	sum := sha256.Sum256([]byte(value))
	return rt.Fingerprint(hex.EncodeToString(sum[:]))
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
	f := registeredCrashFixtureAt(t, root, false, os.Getenv("SUBYARD_TEST_CRASH_FAMILY"))
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
