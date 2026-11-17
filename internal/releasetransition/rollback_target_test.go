package releasetransition

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Subyard/Subyard/internal/testkit"
)

type rollbackTargetFixture struct {
	transition *V2Transition
	owner      *v2TestReconciler
	request    RollbackTargetRequest
	journal    ProtectedSnapshot
	state      MigrationLedgerSnapshot
	lock       *os.File
}

func newRollbackTargetFixture(t *testing.T) rollbackTargetFixture {
	t.Helper()
	transition, home, _ := v2TransitionFixture(t, nil)
	pair := ReleasePair{From: "release-s", Previous: releaseIDPointer("release-a"), Target: "release-a"}
	links := ReleaseLinks{Active: pair.From, Previous: pair.Previous}
	transition.options.Releases = pair
	transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return links, nil }
	transition.options.ActivateLinks = func(context.Context, ReleasePair) (ReleaseLinks, error) {
		t.Fatal("R rotated links under S's journal")
		return ReleaseLinks{}, nil
	}
	transition.options.VerifyAuthorization = func(PlanToken, Authorization) bool { return false }
	owner := &v2TestReconciler{id: "target-runtime"}
	transition.options.Reconcilers = []V2ActivationReconciler{owner}
	native := completeRewindLedger(transition.registry)
	payload, _, err := MarshalLedgerV2(native, transition.registry)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := transition.store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := transition.store.CompareAndSwapLedger(missing, payload); err != nil {
		t.Fatal(err)
	}
	state, err := transition.store.ReadMigrationLedger(transition.registry, transition.registryDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := transition.store.ConvertMigrationLedger(state, transition.registry, transition.registryDigest, transition.registry, transition.registryDigest); err != nil {
		t.Fatal(err)
	}
	state, err = transition.store.ReadMigrationLedger(transition.registry, transition.registryDigest)
	if err != nil {
		t.Fatal(err)
	}
	request := RollbackTargetRequest{SchemaVersion: RollbackTargetProcessSchemaV1, Mode: "activate-under-v2", RuntimeRoot: home + "/runtime", ConfigHome: home, Yard: "default", Releases: pair, SourceArtifact: digestC, SourceRegistry: digestD, TargetArtifact: transition.options.ArtifactDigest, TargetRegistry: transition.registryDigest, Projection: state.ProjectionSnapshot.Fingerprint, NativeHistory: &native}
	inspected, err := transition.InspectRollbackTargetActivation(context.Background(), request)
	if err != nil || len(inspected.Blockers) != 0 {
		t.Fatalf("activation assessment = %+v, %v", inspected, err)
	}
	if owner.reconciles != 0 {
		t.Fatal("inspection performed activation")
	}
	journal := validJournal(pair)
	journal.Goal.Direction = DirectionActivatePrevious
	journal.ArtifactDigest = request.TargetArtifact
	journal.RegistryDigest = request.SourceRegistry
	journal.AuthorizationDigest = fingerprintPayload([]byte("S-grant"))
	journal.Checkpoint = JournalReconciling
	pin := fingerprintPayload([]byte(inspected.Plan.Token))
	journal.Steps = []JournalStep{rollbackOwnerFixtureStep(t, request), {ID: RollbackTargetActivationStep, Migration: RollbackTargetActivationStep, Resource: "activation.rollback-target-plan", Decision: DecisionPreserve, Expected: pin, Desired: pin, Checkpoint: StepVerified}}
	for index := range journal.Steps {
		step := &journal.Steps[index]
		evidence := evidenceFor(journal, *step, EvidenceVerified, step.Desired, "")
		step.Evidence = &evidence
	}
	journal.IntentDigest = bindJournalIntent(journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps)
	payload, err = MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	absent, err := transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	if err := transition.store.CompareAndSwapCurrentJournal(absent, payload); err != nil {
		t.Fatal(err)
	}
	selected, err := transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	request.Journal = selected.Fingerprint
	request.Plan = &inspected.Plan
	request.Execution = &Execution{Plan: inspected.Plan.Token}
	lock, err := transition.store.OpenTransitionLock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	// The owner S performs the link switch before invoking R's activation mode.
	links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
	return rollbackTargetFixture{transition, owner, request, selected, state, lock}
}

func TestRollbackTargetActivationUsesForeignJournalWithoutWritingHistory(t *testing.T) {
	f := newRollbackTargetFixture(t)
	for attempt := 0; attempt < 2; attempt++ {
		response, err := f.transition.ConvergeRollbackTargetActivation(context.Background(), f.request, f.lock)
		if err != nil || response.Completion == nil {
			t.Fatalf("target activation = %+v, %v", response, err)
		}
		if err := response.Validate(); err != nil {
			t.Fatal(err)
		}
		if response.Completion.TargetRegistry != f.transition.registryDigest || response.Completion.Transaction != "tx-001" || response.Completion.Intent == "" {
			t.Fatalf("foreign completion = %+v", response.Completion)
		}
		selected, err := f.transition.store.ReadCurrentJournal()
		if err != nil || !sameProtectedSnapshot(selected, f.journal) {
			t.Fatal("R changed S's journal")
		}
		history, err := f.transition.store.ReadMigrationLedger(f.transition.registry, f.transition.registryDigest)
		if err != nil || !sameProtectedSnapshot(history.AuthoritySnapshot, f.state.AuthoritySnapshot) || !sameProtectedSnapshot(history.ProjectionSnapshot, f.state.ProjectionSnapshot) {
			t.Fatal("R changed migration authority")
		}
	}
	if !f.owner.converged {
		t.Fatal("target resource did not converge")
	}
}

func TestRollbackTargetActivationRejectsUnboundExecutionBeforeEffects(t *testing.T) {
	for _, change := range []string{"grant", "missing durable authorization", "journal", "source artifact", "native history", "plan", "desired scope", "new consequence"} {
		t.Run(change, func(t *testing.T) {
			f := newRollbackTargetFixture(t)
			switch change {
			case "grant":
				f.request.Execution.Authorization = "foreign-grant"
			case "missing durable authorization":
				journal, err := ParseJournal(f.journal.Payload)
				if err != nil {
					t.Fatal(err)
				}
				journal.AuthorizationDigest = fingerprintPayload(nil)
				payload, err := MarshalJournal(journal)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.transition.store.CompareAndSwapCurrentJournal(f.journal, payload); err != nil {
					t.Fatal(err)
				}
				f.request.Journal = fingerprintPayload(payload)
			case "journal":
				f.request.Journal = digestB
			case "source artifact":
				f.request.SourceArtifact = digestB
			case "native history":
				f.request.NativeHistory.Domains["settings"] = DomainLedgerV2{Epoch: 1, Applied: []string{}}
			case "plan":
				f.request.Plan.Token = PlanToken("plan-v1-" + string(digestB))
			case "desired scope":
				f.owner.desired = digestD
			case "new consequence":
				f.owner.consequences = []string{"unassessed restart"}
			}
			if _, err := f.transition.ConvergeRollbackTargetActivation(context.Background(), f.request, f.lock); err == nil {
				t.Fatal("unbound target execution accepted")
			}
			if f.owner.reconciles != 0 {
				t.Fatal("unbound target activation performed effects")
			}
		})
	}
}

func TestRollbackTargetInheritedLockSurvivesSourceDeath(t *testing.T) {
	home := testkit.TempDir(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	control, release, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	defer release.Close()
	observation, report, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer observation.Close()
	defer report.Close()
	if err := observation.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	source := exec.CommandContext(ctx, executable, "-test.run", "^TestRollbackLockProcessHelper$")
	source.Env = append(os.Environ(), "SUBYARD_TEST_ROLLBACK_LOCK_ROLE=source", "SUBYARD_TEST_ROLLBACK_LOCK_HOME="+home)
	source.Stdin, source.Stdout, source.Stderr = control, report, os.Stderr
	if err := source.Start(); err != nil {
		t.Fatal(err)
	}
	defer source.Wait()
	if err := report.Close(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(observation)
	if marker, err := reader.ReadString('\n'); err != nil || marker != "R holds lock\n" {
		t.Fatalf("R did not inherit the held lock: %q, %v", marker, err)
	}
	if err := source.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := source.Wait(); err == nil {
		t.Fatal("S exited successfully")
	}
	contender, err := os.Open(filepath.Join(home, "migrations", "update.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer contender.Close()
	if err := unix.Flock(int(contender.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("a fresh updater could take the lock while R continued: %v", err)
	}
	if err := release.Close(); err != nil {
		t.Fatal(err)
	}
	if marker, err := io.ReadAll(reader); err != nil || string(marker) != "R released lock\n" {
		t.Fatalf("R did not finish with its inherited lock: %q, %v", marker, err)
	}
	if err := unix.Flock(int(contender.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("R left the transition lock held: %v", err)
	}
}

func TestRollbackLockProcessHelper(t *testing.T) {
	role := os.Getenv("SUBYARD_TEST_ROLLBACK_LOCK_ROLE")
	if role == "" {
		return
	}
	store, err := NewPOSIXV2Store(os.Getenv("SUBYARD_TEST_ROLLBACK_LOCK_HOME"))
	if err != nil {
		t.Fatal(err)
	}
	if role == "source" {
		lock, err := store.OpenTransitionLock()
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		target := exec.Command(executable, "-test.run", "^TestRollbackLockProcessHelper$")
		target.Env = append(os.Environ(), "SUBYARD_TEST_ROLLBACK_LOCK_ROLE=target")
		target.ExtraFiles = []*os.File{lock}
		target.Stdin, target.Stdout, target.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := target.Run(); err != nil {
			t.Fatal(err)
		}
		return
	}
	lock := os.NewFile(3, "inherited release transition lock")
	if err := store.checkInheritedTransitionLock(lock); err != nil {
		t.Fatal(err)
	}
	fmt.Println("R holds lock")
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
	if err := store.checkInheritedTransitionLock(lock); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	fmt.Println("R released lock")
	os.Exit(0)
}

func TestRollbackTargetRequiresTheHeldTransitionLockDescription(t *testing.T) {
	f := newRollbackTargetFixture(t)
	otherStore, err := NewPOSIXV2Store(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	wrongInode, err := otherStore.OpenTransitionLock()
	if err != nil {
		t.Fatal(err)
	}
	defer wrongInode.Close()
	wrongDescription, err := os.Open(filepath.Join(f.transition.store.configHome, "migrations", "update.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer wrongDescription.Close()
	for _, tc := range []struct {
		lock    *os.File
		message string
	}{
		{nil, "requires S's inherited transition lock"},
		{wrongInode, "belongs to another inode"},
		{wrongDescription, "is not held exclusively"},
	} {
		if _, err := f.transition.ConvergeRollbackTargetActivation(context.Background(), f.request, tc.lock); err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Fatalf("invalid lock acceptance: %v", err)
		}
	}
	if f.owner.reconciles != 0 {
		t.Fatal("invalid lock allowed activation effects")
	}
}

func TestRollbackTargetProtocolIsBoundedAndStrict(t *testing.T) {
	probe := []byte(`{"schemaVersion":3,"mode":"capabilities"}`)
	request, err := ParseRollbackTargetRequest(probe)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRollbackTargetRequest(encoded); err != nil {
		t.Fatal(err)
	}
	for _, payload := range [][]byte{
		[]byte(`{"schemaVersion":3,"mode":"capabilities","configHome":"/private"}`),
		[]byte(`{"schemaVersion":3,"mode":"capabilities","unknown":true}`),
		append(append([]byte{}, probe...), []byte(" {}")...), bytes.Repeat([]byte("x"), MaxJournalBytes+1),
	} {
		if _, err := ParseRollbackTargetRequest(payload); err == nil {
			t.Fatal("invalid target request accepted")
		}
	}
	response := RollbackTargetResponse{SchemaVersion: 3, Contract: RollbackTargetContractV1}
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRollbackTargetResponse(payload); err != nil {
		t.Fatal(err)
	}
	response.Inspection = &RollbackActivationInspection{}
	if err := response.Validate(); err == nil {
		t.Fatal("ambiguous target response accepted")
	}
}

func TestRegistryProvenanceDoesNotGrantExecutionSupport(t *testing.T) {
	source, _ := rewindRegistries()
	payload, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog()); err == nil {
		t.Fatal("strict executor accepted unknown kind")
	}
	if registry, digest, err := ParseRegistryV2Provenance(payload); err != nil || digest != fingerprintPayload(payload) || len(registry.Migrations) != 2 {
		t.Fatalf("structural provenance = %+v, %s, %v", registry, digest, err)
	}
	source.Migrations[1].FromEpoch = 5
	payload, err = json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseRegistryV2Provenance(payload); err == nil {
		t.Fatal("structural reader accepted broken domain history")
	}
}

func TestRollbackTargetActivationResumesAfterUncertainResult(t *testing.T) {
	f := newRollbackTargetFixture(t)
	interrupted := false
	f.transition.options.fault = func(point string) error {
		if point == "after-reconciler-0" && !interrupted {
			interrupted = true
			return context.Canceled
		}
		return nil
	}
	response, err := f.transition.ConvergeRollbackTargetActivation(context.Background(), f.request, f.lock)
	if err == nil || response.Completion != nil || !f.owner.converged {
		t.Fatalf("uncertain activation = %+v, %v", response, err)
	}
	options := f.transition.options
	options.fault = nil
	f.transition, err = NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.lock.Close(); err != nil {
		t.Fatal(err)
	}
	lock, err := f.transition.store.OpenTransitionLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	response, err = f.transition.ConvergeRollbackTargetActivation(context.Background(), f.request, lock)
	if err != nil || response.Completion == nil {
		t.Fatalf("fresh target process could not resume = %+v, %v", response, err)
	}
	selected, err := f.transition.store.ReadCurrentJournal()
	if err != nil || !sameProtectedSnapshot(selected, f.journal) {
		t.Fatal("target resume changed S's transaction")
	}
}
