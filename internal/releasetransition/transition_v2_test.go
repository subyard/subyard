package releasetransition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	v2PostActivationPreviousRelease  ReleaseID = "0.9.1-a1b2c3d4e5f6"
	v2PostActivationSourceRelease    ReleaseID = "0.11.1-b2c3d4e5f6a7"
	v2PostActivationCandidateRelease ReleaseID = "0.11.2-c3d4e5f6a7b8"
)

func TestV2TransitionCanonicalizesAndResetsOnce(t *testing.T) {
	transition, configHome, path := v2TransitionFixture(t, nil)
	inspection, err := transition.Inspect(context.Background(), Goal{
		Target: "release-a", Direction: DirectionActivateTarget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Resume != nil || len(inspection.Blockers) != 0 ||
		len(inspection.Decisions) != 2 || !inspection.Assessment.Changed {
		t.Fatalf("inspection = %#v", inspection)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != StatusReady || !outcome.ReachedGoal || outcome.Code != CodeReady ||
		outcome.Transaction == nil {
		t.Fatalf("outcome = %#v", outcome)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "YARD_TEMPLATE='test-vms'") ||
		strings.Contains(string(content), "NESTED_E2E_VMS") {
		t.Fatalf("migrated settings = %q", content)
	}
	store, _ := NewPOSIXV2Store(configHome)
	ledgerSnapshot, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	registry := v2TestRegistry(t)
	ledger, _, err := ParseLedgerV2(ledgerSnapshot.Payload, registry)
	if err != nil || ledger.Domains["settings"].Epoch != 2 {
		t.Fatalf("ledger = %#v, err=%v", ledger, err)
	}

	repeat, err := transition.Inspect(context.Background(), Goal{
		Target: "release-a", Direction: DirectionActivateTarget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if repeat.Resume != nil || repeat.Assessment.Changed || len(repeat.Decisions) != 0 {
		t.Fatalf("repeat inspection = %#v", repeat)
	}
	repeatedOutcome, err := transition.Converge(context.Background(), Execution{Plan: repeat.Plan})
	if err != nil || repeatedOutcome.Status != StatusReady || !repeatedOutcome.ReachedGoal {
		t.Fatalf("repeat outcome = %#v, err=%v", repeatedOutcome, err)
	}
}

func TestV2TransitionRejectsLateInheritedSettingsDriftBeforeCAS(t *testing.T) {
	for _, test := range []struct {
		name  string
		drift func(t *testing.T, transition *V2Transition, configHome string)
	}{
		{
			name: "shared file",
			drift: func(t *testing.T, _ *V2Transition, configHome string) {
				path := filepath.Join(configHome, "overrides/shared/config.env")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("NESTED_E2E_VMS=0\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "host file",
			drift: func(t *testing.T, _ *V2Transition, configHome string) {
				if err := os.WriteFile(
					filepath.Join(configHome, "config.env"), []byte("NESTED_E2E_VMS=0\n"), 0o600,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "command environment",
			drift: func(t *testing.T, transition *V2Transition, _ string) {
				transition.options.InheritedSettingIDs = []string{"NESTED_E2E_VMS"}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			drifted := false
			var transition *V2Transition
			var configHome string
			transition, configHome, path := v2TransitionFixture(t, func(point string) error {
				if point == "after-step-evidence" && !drifted {
					test.drift(t, transition, configHome)
					drifted = true
				}
				return nil
			})
			inspection, err := transition.Inspect(context.Background(), Goal{
				Target: "release-a", Direction: DirectionActivateTarget,
			})
			if err != nil || len(inspection.Blockers) != 0 {
				t.Fatalf("inspection = %#v, err=%v", inspection, err)
			}
			outcome, err := transition.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			if err != nil || outcome.Status != StatusOperatorActionRequired ||
				outcome.Code != CodeMigrationStale {
				t.Fatalf("late inherited drift outcome = %#v, err=%v", outcome, err)
			}
			content, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(content), "YARD_TEMPLATE=e2e-vms") {
				t.Fatalf("late inherited drift mutated yard settings: %q, err=%v", content, err)
			}
		})
	}
}

func TestV2TransitionIgnoresUnrelatedInheritedSettingsDrift(t *testing.T) {
	drifted := false
	var configHome string
	transition, observedConfigHome, path := v2TransitionFixture(t, func(point string) error {
		if point == "after-step-evidence" && !drifted {
			if err := os.WriteFile(
				filepath.Join(configHome, "config.env"), []byte("UNRELATED_SETTING=changed\n"), 0o600,
			); err != nil {
				return err
			}
			drifted = true
		}
		return nil
	})
	configHome = observedConfigHome
	inspection, err := transition.Inspect(context.Background(), Goal{
		Target: "release-a", Direction: DirectionActivateTarget,
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusReady {
		t.Fatalf("unrelated inherited drift outcome = %#v, err=%v", outcome, err)
	}
	content, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(content), "YARD_TEMPLATE='test-vms'") {
		t.Fatalf("yard settings were not migrated: %q, err=%v", content, err)
	}
}

func TestV2InspectionCarriesThePublicOutcomeForFreshAndRecoveringStates(t *testing.T) {
	t.Run("migration required", func(t *testing.T) {
		transition, _, _ := v2TransitionFixtureWithReleases(
			t,
			nil,
			ReleasePair{From: "release-a", Target: "release-b"},
			ReleaseLinks{Active: "release-a"},
		)
		inspection, err := transition.Inspect(context.Background(), Goal{
			Target: "release-b", Direction: DirectionActivateTarget,
		})
		if err != nil {
			t.Fatal(err)
		}
		assertV2InspectionPublicOutcome(
			t, inspection, StatusMigrationRequired, CodeTransitionRequired, nil,
		)
	})

	t.Run("recovering", func(t *testing.T) {
		activeFault := "after-journal-authorized"
		transition, configHome, _ := v2TransitionFixture(t, func(point string) error {
			if point == activeFault {
				return errors.New("stop after durable authorization")
			}
			return nil
		})
		goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
		initial, err := transition.Inspect(context.Background(), goal)
		if err != nil {
			t.Fatal(err)
		}
		interrupted, err := transition.Converge(context.Background(), Execution{
			Plan: initial.Plan, Authorization: v2TestAuthorization(initial.Plan),
		})
		if err != nil || interrupted.Status != StatusRecovering || interrupted.Transaction == nil {
			t.Fatalf("interrupted outcome = %#v, err=%v", interrupted, err)
		}
		journalPath := filepath.Join(configHome, "release-transition", "v2", "journal.json")
		before, err := os.ReadFile(journalPath)
		if err != nil {
			t.Fatal(err)
		}
		beforeInfo, err := os.Lstat(journalPath)
		if err != nil {
			t.Fatal(err)
		}

		activeFault = ""
		resume, err := transition.Inspect(context.Background(), goal)
		if err != nil {
			t.Fatal(err)
		}
		assertV2InspectionPublicOutcome(
			t, resume, StatusRecovering, CodeRecoveryPending, interrupted.Transaction,
		)
		after, err := os.ReadFile(journalPath)
		if err != nil {
			t.Fatal(err)
		}
		afterInfo, err := os.Lstat(journalPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) || !os.SameFile(beforeInfo, afterInfo) ||
			afterInfo.Mode() != beforeInfo.Mode() {
			t.Fatal("Inspect mutated the protected recovery journal")
		}
	})
}

func assertV2InspectionPublicOutcome(
	t *testing.T,
	inspection Inspection,
	status PublicStatus,
	code OutcomeCode,
	transaction *TransactionID,
) {
	t.Helper()
	payload, err := json.Marshal(inspection)
	if err != nil {
		t.Fatal(err)
	}
	var public struct {
		Outcome *Outcome `json:"outcome"`
	}
	if err := json.Unmarshal(payload, &public); err != nil {
		t.Fatal(err)
	}
	if public.Outcome == nil || public.Outcome.Status != status || public.Outcome.Code != code ||
		public.Outcome.Active == "" || public.Outcome.Target == "" ||
		public.Outcome.Retry == "" ||
		(transaction != nil && (public.Outcome.Transaction == nil ||
			*public.Outcome.Transaction != *transaction)) {
		t.Fatalf("inspection public outcome = %#v from %s", public.Outcome, payload)
	}
}

func TestV2TransitionResumesOwnerRegistrationAfterCommit(t *testing.T) {
	owner := &v2TestOwnerRegistration{state: OwnerRegistrationLegacyDirectory}
	stop := errors.New("stop after owner registration commit")
	activeFault := "after-owner-registration-commit"
	transition, configHome, _ := v2TransitionFixture(t, func(point string) error {
		if point == activeFault {
			return stop
		}
		return nil
	})
	transition.options.OwnerRegistration = owner
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil || len(inspection.Blockers) != 0 || !inspection.Assessment.Changed {
		t.Fatalf("owner inspection = %#v, err=%v", inspection, err)
	}
	interrupted, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || interrupted.Status != StatusRecovering ||
		interrupted.Code != CodeVerificationFailed {
		t.Fatalf("owner commit fault outcome = %#v, err=%v", interrupted, err)
	}
	if owner.state != OwnerRegistrationCurrent || owner.commits != 1 {
		t.Fatalf("owner after commit = %#v", owner)
	}
	activeFault = ""
	resume, err := transition.Inspect(context.Background(), goal)
	if err != nil || resume.Resume == nil {
		t.Fatalf("owner resume = %#v, err=%v", resume, err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: resume.Plan})
	if err != nil || outcome.Status != StatusReady || owner.commits != 2 {
		t.Fatalf("owner outcome = %#v owner=%#v err=%v", outcome, owner, err)
	}
	store, _ := NewPOSIXV2Store(configHome)
	ledgerSnapshot, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	registry := v2TestRegistry(t)
	ledger := BaselineLedgerV2(registry)
	if ledgerSnapshot.Exists {
		ledger, _, err = ParseLedgerV2(ledgerSnapshot.Payload, registry)
	}
	if err != nil || ledger.Domains["owner-registration"].Epoch != 2 {
		t.Fatalf("owner ledger = %#v, err=%v", ledger, err)
	}
}

func TestV2OwnerRecoveryPersistsOpaqueTokenBeforeMutation(t *testing.T) {
	owner := &v2TestOwnerRegistration{state: OwnerRegistrationLegacyDirectory}
	stop := errors.New("stop after owner recovery publication")
	recoveryBoundaries := 0
	transition, configHome, _ := v2TransitionFixture(t, func(point string) error {
		if point == "after-recovery" {
			recoveryBoundaries++
			if recoveryBoundaries == 2 {
				return stop
			}
		}
		return nil
	})
	transition.options.OwnerRegistration = owner
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusRecovering {
		t.Fatalf("recovery boundary outcome = %#v, err=%v", outcome, err)
	}
	store, err := NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	journalSnapshot, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(journalSnapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var ownerStep JournalStep
	for _, step := range journal.Steps {
		if step.Resource == ownerRegistrationResource {
			ownerStep = step
			break
		}
	}
	if ownerStep.ID == "" {
		t.Fatalf("owner recovery step missing from %#v", journal.Steps)
	}
	recovery, err := store.ReadRecovery(journal.Transaction, ownerStep.ID)
	if err != nil || !recovery.Exists {
		t.Fatalf("owner recovery = %#v, err=%v", recovery, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(recovery.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	token, ok := payload["token"].(string)
	if !ok || len(token) != 32 {
		t.Fatalf("owner recovery token = %#v", payload["token"])
	}
	for _, character := range token {
		if !strings.ContainsRune("0123456789abcdef", character) {
			t.Fatalf("owner recovery token is unsafe: %q", token)
		}
	}
	if owner.commits != 0 {
		t.Fatalf("owner mutated before recovery token publication: %#v", owner)
	}
}

func TestV2TransitionRejectsRegistrationIdentityDriftBeforeCommit(t *testing.T) {
	owner := &v2TestOwnerRegistration{
		state: OwnerRegistrationLegacyDirectory, registration: digestB,
	}
	transition, configHome, _ := v2TransitionFixture(t, nil)
	transition.options.OwnerRegistration = owner
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	owner.registration = digestC

	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusOperatorActionRequired ||
		outcome.Code != CodePlanStale || owner.commits != 0 {
		t.Fatalf("owner identity drift outcome = %#v owner=%#v err=%v", outcome, owner, err)
	}
	journal, err := transition.store.ReadCurrentJournal()
	if err != nil || journal.Exists {
		t.Fatalf("owner identity drift published journal = %#v, err=%v", journal, err)
	}
	if _, err := os.Lstat(filepath.Join(configHome, "release-transition")); !os.IsNotExist(err) {
		t.Fatalf("owner identity drift created protected state: %v", err)
	}
}

func TestV2TransitionResumesOwnerRegistrationFromIntermediateProgress(t *testing.T) {
	owner := &v2TestOwnerRegistration{
		state: OwnerRegistrationLegacyDirectory, interruptCommit: true,
	}
	transition, _, _ := v2TransitionFixture(t, nil)
	transition.options.OwnerRegistration = owner
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil || len(inspection.Blockers) != 0 || !inspection.Assessment.Changed {
		t.Fatalf("owner inspection = %#v, err=%v", inspection, err)
	}
	interrupted, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || interrupted.Status != StatusRecovering || interrupted.Transaction == nil {
		t.Fatalf("interrupted owner convergence = %#v, err=%v", interrupted, err)
	}

	resume, err := transition.Inspect(context.Background(), goal)
	if err != nil || resume.Resume == nil || resume.Outcome == nil ||
		resume.Outcome.Status != StatusRecovering {
		t.Fatalf("owner intermediate inspection = %#v, err=%v", resume, err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: resume.Plan})
	if err != nil || outcome.Status != StatusReady || owner.state != OwnerRegistrationCurrent ||
		owner.commits != 2 {
		t.Fatalf("resumed owner convergence = %#v owner=%#v err=%v", outcome, owner, err)
	}
}

func TestV2NewOwnerImpactReplacesUnmutatedPreActivationJournal(t *testing.T) {
	activeFault := true
	transition, configHome, _ := v2TransitionFixture(t, func(point string) error {
		if activeFault && point == "after-journal-authorized" {
			return errors.New("stop after the first authorization")
		}
		return nil
	})
	transactions := []TransactionID{"tx-test-001", "tx-test-002"}
	transition.options.NewTransactionID = func() TransactionID {
		transaction := transactions[0]
		transactions = transactions[1:]
		return transaction
	}
	owner := &v2TestOwnerRegistration{state: OwnerRegistrationAbsent}
	transition.options.OwnerRegistration = owner
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	first, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := transition.Converge(context.Background(), Execution{
		Plan: first.Plan, Authorization: v2TestAuthorization(first.Plan),
	})
	if err != nil || interrupted.Status != StatusRecovering || interrupted.Transaction == nil ||
		*interrupted.Transaction != "tx-test-001" {
		t.Fatalf("first transaction = %#v, err=%v", interrupted, err)
	}

	activeFault = false
	owner.state = OwnerRegistrationLegacyDirectory
	replacement, err := transition.Inspect(context.Background(), goal)
	if err != nil || replacement.Resume != nil || replacement.Outcome == nil ||
		replacement.Outcome.Status != StatusMigrationRequired ||
		replacement.Outcome.Code != CodeTransitionRequired || replacement.Plan == first.Plan {
		t.Fatalf("owner replacement inspection = %#v, err=%v", replacement, err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: replacement.Plan, Authorization: v2TestAuthorization(replacement.Plan),
	})
	if err != nil || outcome.Status != StatusReady || outcome.Transaction == nil ||
		*outcome.Transaction != "tx-test-002" || owner.state != OwnerRegistrationCurrent {
		t.Fatalf("owner replacement outcome = %#v owner=%#v err=%v", outcome, owner, err)
	}
	archive, err := transition.store.ReadSupersededJournal("tx-test-002")
	if err != nil || archive.Exists {
		t.Fatalf("pre-activation replacement archive = %#v, err=%v", archive, err)
	}
	if _, err := os.Stat(filepath.Join(
		configHome, "release-transition", "v2", "transactions", "tx-test-001",
	)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unmutated source transaction unexpectedly has artifacts: %v", err)
	}

	owner.state = OwnerRegistrationLegacyDirectory
	laterOptions := transition.options
	laterLinks := &ReleaseLinks{Active: "release-a"}
	laterOptions.Releases = ReleasePair{From: "release-a", Target: "release-b"}
	laterOptions.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return *laterLinks, nil }
	laterOptions.ActivateLinks = func(
		_ context.Context,
		pair ReleasePair,
	) (ReleaseLinks, error) {
		*laterLinks = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
		return *laterLinks, nil
	}
	laterOptions.NewTransactionID = func() TransactionID { return "tx-test-003" }
	later, err := NewV2Transition(laterOptions)
	if err != nil {
		t.Fatal(err)
	}
	ledgerSnapshot, err := later.store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	baselinePayload, _, err := MarshalLedgerV2(BaselineLedgerV2(later.registry), later.registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := later.store.CompareAndSwapLedger(ledgerSnapshot, baselinePayload); err != nil {
		t.Fatal(err)
	}
	laterGoal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
	laterInspection, err := later.Inspect(context.Background(), laterGoal)
	if err != nil || laterInspection.Outcome == nil ||
		laterInspection.Outcome.Status != StatusMigrationRequired {
		t.Fatalf("later transition inspection = %#v, outcome=%#v, err=%v", laterInspection, laterInspection.Outcome, err)
	}
	laterOutcome, err := later.Converge(context.Background(), Execution{
		Plan: laterInspection.Plan, Authorization: v2TestAuthorization(laterInspection.Plan),
	})
	if err != nil || laterOutcome.Status != StatusReady || laterOutcome.Transaction == nil ||
		*laterOutcome.Transaction != "tx-test-003" || len(laterOutcome.Warnings) != 0 {
		t.Fatalf("later transition outcome = %#v, err=%v", laterOutcome, err)
	}
	if _, err := later.Inspect(context.Background(), laterGoal); err != nil {
		t.Fatalf("historical logical predecessor blocked later inspection: %v", err)
	}
	oldArchive, err := later.store.ReadSupersededJournal("tx-test-002")
	if err != nil || oldArchive.Exists {
		t.Fatalf("historical component was not cleaned up: %#v, err=%v", oldArchive, err)
	}
	if _, err := os.Stat(filepath.Join(
		configHome, "release-transition", "v2", "transactions", "tx-test-003",
	)); err != nil {
		t.Fatalf("current transaction was not retained: %v", err)
	}
}

func TestV2NewOwnerImpactFailsClosedAfterAuthorizedMigrationMutation(t *testing.T) {
	activeFault := true
	transition, configHome, _ := v2TransitionFixture(t, func(point string) error {
		if activeFault && point == "after-settings-cas" {
			return errors.New("stop after an authorized settings mutation")
		}
		return nil
	})
	owner := &v2TestOwnerRegistration{state: OwnerRegistrationAbsent}
	transition.options.OwnerRegistration = owner
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	first, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := transition.Converge(context.Background(), Execution{
		Plan: first.Plan, Authorization: v2TestAuthorization(first.Plan),
	})
	if err != nil || interrupted.Status != StatusRecovering || interrupted.Transaction == nil {
		t.Fatalf("mutated transaction = %#v, err=%v", interrupted, err)
	}

	activeFault = false
	owner.state = OwnerRegistrationLegacyDirectory
	resume, err := transition.Inspect(context.Background(), goal)
	if err != nil || resume.Resume == nil || resume.Outcome == nil ||
		resume.Outcome.Status != StatusOperatorActionRequired ||
		resume.Outcome.Code != CodeMigrationStale {
		t.Fatalf("new owner impact after mutation = %#v, err=%v", resume, err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: resume.Plan})
	if err != nil || outcome.Status != StatusOperatorActionRequired ||
		outcome.Code != CodeMigrationStale || owner.commits != 0 {
		t.Fatalf("new owner impact convergence = %#v owner=%#v err=%v", outcome, owner, err)
	}
	store, err := NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	ledgerSnapshot, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	registry := v2TestRegistry(t)
	ledger := BaselineLedgerV2(registry)
	if ledgerSnapshot.Exists {
		ledger, _, err = ParseLedgerV2(ledgerSnapshot.Payload, registry)
	}
	if err != nil || ledger.Domains["owner-registration"].Epoch != 1 {
		t.Fatalf("owner ledger advanced past new impact: %#v, err=%v", ledger, err)
	}
}

func TestV2TransitionReplacesExactV0111PostActivationScopeFailure(t *testing.T) {
	configHome := settingsV2Fixture(t, map[string]string{
		"yards/hermes/config.env": "YARD_TEMPLATE='test-vms'\n",
	})
	sourcePair := ReleasePair{
		From: v2PostActivationPreviousRelease, Target: v2PostActivationSourceRelease,
	}
	links := ReleaseLinks{
		Active:   v2PostActivationSourceRelease,
		Previous: releaseIDPointer(v2PostActivationPreviousRelease),
	}
	sourceOptions := V2Options{
		ConfigHome: configHome, Releases: sourcePair,
		ObserveLinks:        func(context.Context) (ReleaseLinks, error) { return links, nil },
		RegistryPayload:     v2RegistryPayload(t),
		ArtifactDigest:      digestA,
		OwnerRegistration:   &v2TestOwnerRegistration{state: OwnerRegistrationAbsent},
		NewTransactionID:    func() TransactionID { return "tx-source-v0111" },
		VerifyAuthorization: func(PlanToken, Authorization) bool { return false },
	}
	source, err := NewV2Transition(sourceOptions)
	if err != nil {
		t.Fatal(err)
	}
	journal, sourceSnapshot, ledgerPayload := seedV0111PostActivationJournal(t, source)

	ordinary, err := NewV2Transition(sourceOptions)
	if err != nil {
		t.Fatal(err)
	}
	oldInspection, err := ordinary.Inspect(context.Background(), journal.Goal)
	if err != nil || oldInspection.Resume == nil || oldInspection.Outcome == nil ||
		oldInspection.Outcome.Status != StatusOperatorActionRequired ||
		oldInspection.Outcome.Code != CodePlanStale || len(oldInspection.Blockers) != 1 ||
		oldInspection.Blockers[0].Code != CodePlanStale ||
		oldInspection.Blockers[0].Resource != "transition.observation-scope" {
		t.Fatalf("ordinary v0.11.1 inspection = %#v, err=%v", oldInspection, err)
	}

	replacement := &JournalReplacement{
		Transaction: journal.Transaction, Fingerprint: sourceSnapshot.Fingerprint,
		Reason: JournalReplacementPostActivationScopeV0111, SourceVersion: "0.11.1",
	}
	candidateOptions := sourceOptions
	candidateOptions.Releases = ReleasePair{
		From: v2PostActivationSourceRelease, Previous: releaseIDPointer(v2PostActivationPreviousRelease),
		Target: v2PostActivationCandidateRelease,
	}
	candidateOptions.CandidateVersion = "0.11.2"
	candidateOptions.ArtifactDigest = digestB
	candidateOptions.Replacement = replacement
	candidateOptions.NewTransactionID = func() TransactionID { return "tx-recovery-v0112" }
	candidate, err := NewV2Transition(candidateOptions)
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{Target: v2PostActivationCandidateRelease, Direction: DirectionActivateTarget}
	inspection, err := candidate.Inspect(context.Background(), goal)
	if err != nil || inspection.Resume != nil || inspection.Outcome == nil ||
		inspection.Outcome.Status != StatusMigrationRequired ||
		inspection.Outcome.Code != CodeTransitionRequired || !inspection.Assessment.Changed {
		t.Fatalf("post-activation replacement inspection = %#v, err=%v", inspection, err)
	}
	observation, err := candidate.observe(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	if observation.replacement == nil || *observation.replacement != *replacement ||
		observation.journal != nil || len(observation.intents) != 0 || len(observation.work) != 0 {
		t.Fatalf("post-activation replacement observation = %#v", observation)
	}
	if !releasePairsEqual(journal.Releases, sourcePair) ||
		!releasePairsEqual(candidate.options.Releases, candidateOptions.Releases) ||
		observation.links.Active != v2PostActivationSourceRelease ||
		observation.links.Previous == nil ||
		*observation.links.Previous != v2PostActivationPreviousRelease {
		t.Fatalf("opaque release identities changed: source=%#v candidate=%#v links=%#v",
			journal.Releases, candidate.options.Releases, observation.links)
	}
	expectedPlan, err := BindPlan(candidate.planFacts(observation))
	if err != nil || expectedPlan != inspection.Plan {
		t.Fatalf("replacement Plan binding = %q, want %q, err=%v", inspection.Plan, expectedPlan, err)
	}
	current, err := candidate.store.ReadCurrentJournal()
	if err != nil || !sameProtectedSnapshot(current, sourceSnapshot) {
		t.Fatalf("read-only replacement inspection changed source journal: %#v, err=%v", current, err)
	}
	ledger, err := candidate.store.ReadLedger()
	if err != nil || !ledger.Exists || string(ledger.Payload) != string(ledgerPayload) {
		t.Fatalf("read-only replacement inspection changed ledger: %#v, err=%v", ledger, err)
	}
}

func TestV2TransitionPostActivationReplacementAcceptsAnyLaterSemver(t *testing.T) {
	for _, version := range []string{"0.11.2", "0.12.0", "1.0.0", "0.11.2-rc.1"} {
		t.Run(version, func(t *testing.T) {
			fixture := newV2PostActivationRecoveryFixture(t)
			fixture.transition.options.CandidateVersion = version
			inspection, err := fixture.transition.Inspect(context.Background(), fixture.goal)
			if err != nil || inspection.Resume != nil || inspection.Outcome == nil ||
				inspection.Outcome.Status != StatusMigrationRequired ||
				inspection.Outcome.Code != CodeTransitionRequired {
				t.Fatalf("candidate %q inspection = %#v, err=%v", version, inspection, err)
			}
		})
	}
}

func TestV2TransitionPostActivationReplacementEligibilityFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*v2PostActivationRecoveryFixture)
	}{
		{name: "source version", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.transition.options.Replacement.SourceVersion = "0.11.0"
		}},
		{name: "missing candidate version", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.transition.options.CandidateVersion = ""
		}},
		{name: "malformed candidate version", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.transition.options.CandidateVersion = "not-semver"
		}},
		{name: "equal candidate version", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.transition.options.CandidateVersion = "0.11.1"
		}},
		{name: "lower candidate version", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.transition.options.CandidateVersion = "0.11.0"
		}},
		{name: "journal checkpoint", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.rewriteJournal(func(journal *JournalRecord) { journal.Checkpoint = JournalTargetActive })
		}},
		{name: "candidate source ingress", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.transition.options.SourceIngress = &SourceIngressRequest{
				SchemaVersion: SourceIngressRequestSchemaV1,
				Kind:          SourceIngressPreGoV1,
				SourceRoot:    "/home/operator/source",
				DataHome:      "/home/operator/.subyard",
				BinDir:        "/home/operator/.local/bin",
				RC:            "/home/operator/.bashrc",
				LoginRC:       "/home/operator/.profile",
			}
		}},
		{name: "current link", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.links.Active = "foreign"
		}},
		{name: "previous link", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.links.Previous = nil
		}},
		{name: "direction", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.transition.options.Direction = DirectionActivatePrevious
			f.goal.Direction = DirectionActivatePrevious
		}},
		{name: "step checkpoint", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.rewriteJournal(func(journal *JournalRecord) {
				journal.Checkpoint = JournalMigrating
				journal.Steps[0].Checkpoint = StepApplied
				applied := *journal.Steps[0].Evidence
				applied.Checkpoint = EvidenceApplied
				journal.Steps[0].Evidence = &applied
			})
		}},
		{name: "step resource", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.rewriteJournal(func(journal *JournalRecord) { journal.Steps[0].Resource = "yard.fixture" })
		}},
		{name: "embedded evidence", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.rewriteJournal(func(journal *JournalRecord) { journal.Steps[0].Evidence.Recovery = digestA })
		}},
		{name: "stored captured evidence", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.removeEvidence(f.sourceJournal.Steps[0].ID, EvidenceCaptured)
		}},
		{name: "stored applied evidence", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.removeEvidence(f.sourceJournal.Steps[0].ID, EvidenceApplied)
		}},
		{name: "stored verified evidence", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.removeEvidence(f.sourceJournal.Steps[0].ID, EvidenceVerified)
		}},
		{name: "unjournaled evidence", mutate: func(f *v2PostActivationRecoveryFixture) {
			if err := f.transition.store.CreateCheckpointEvidence(
				f.sourceJournal.Transaction, "unknown-ledger", EvidenceVerified, []byte("unknown\n"),
			); err != nil {
				f.t.Fatal(err)
			}
		}},
		{name: "unjournaled transaction artifact", mutate: func(f *v2PostActivationRecoveryFixture) {
			path := filepath.Join(
				f.configHome, "release-transition", "v2", "transactions",
				string(f.sourceJournal.Transaction), "unknown.json",
			)
			if err := os.WriteFile(path, []byte("unknown\n"), 0o600); err != nil {
				f.t.Fatal(err)
			}
		}},
		{name: "ledger bytes", mutate: func(f *v2PostActivationRecoveryFixture) {
			var value any
			if err := json.Unmarshal(f.ledgerPayload, &value); err != nil {
				f.t.Fatal(err)
			}
			payload, err := json.MarshalIndent(value, "", "  ")
			if err != nil {
				f.t.Fatal(err)
			}
			payload = append(payload, '\n')
			current, err := f.transition.store.ReadLedger()
			if err != nil {
				f.t.Fatal(err)
			}
			if err := f.transition.store.CompareAndSwapLedger(current, payload); err != nil {
				f.t.Fatal(err)
			}
		}},
		{name: "recovery payload", mutate: func(f *v2PostActivationRecoveryFixture) {
			if err := f.transition.store.CreateRecovery(
				f.sourceJournal.Transaction, f.sourceJournal.Steps[0].ID, []byte("recovery\n"),
			); err != nil {
				f.t.Fatal(err)
			}
		}},
		{name: "unjournaled recovery payload", mutate: func(f *v2PostActivationRecoveryFixture) {
			if err := f.transition.store.CreateRecovery(
				f.sourceJournal.Transaction, "unknown-ledger", []byte("recovery\n"),
			); err != nil {
				f.t.Fatal(err)
			}
		}},
		{name: "registry digest", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.rewriteJournal(func(journal *JournalRecord) { journal.RegistryDigest = digestA })
		}},
		{name: "catalog digest", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.rewriteJournal(func(journal *JournalRecord) { journal.CatalogDigest = digestA })
		}},
		{name: "replacement transaction", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.transition.options.Replacement.Transaction = "tx-other"
		}},
		{name: "replacement fingerprint", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.transition.options.Replacement.Fingerprint = digestA
		}},
		{name: "noncanonical source fingerprint", mutate: func(f *v2PostActivationRecoveryFixture) {
			current, err := f.transition.store.ReadCurrentJournal()
			if err != nil {
				f.t.Fatal(err)
			}
			payload := append(append([]byte(nil), current.Payload...), '\n')
			if err := f.transition.store.CompareAndSwapCurrentJournal(current, payload); err != nil {
				f.t.Fatal(err)
			}
			updated, err := f.transition.store.ReadCurrentJournal()
			if err != nil {
				f.t.Fatal(err)
			}
			f.transition.options.Replacement.Fingerprint = updated.Fingerprint
		}},
		{name: "blocker count", mutate: func(f *v2PostActivationRecoveryFixture) {
			scope, err := f.transition.bindObservationScope(nil)
			if err != nil {
				f.t.Fatal(err)
			}
			f.rewriteJournal(func(journal *JournalRecord) { journal.ObservationScope = scope })
		}},
		{name: "blocker code", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.transition.options.Reconcilers = []V2ActivationReconciler{
				v2ObservationErrorReconciler{id: "blocked", err: errors.New("temporarily unavailable")},
			}
		}},
		{name: "candidate release identity", mutate: func(f *v2PostActivationRecoveryFixture) {
			f.transition.options.Releases.Target = v2PostActivationSourceRelease
			f.goal.Target = v2PostActivationSourceRelease
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newV2PostActivationRecoveryFixture(t)
			test.mutate(fixture)
			before := snapshotV2ProtectedTree(t, fixture.configHome)
			linksBefore := ReleaseLinks{
				Active: fixture.links.Active, Previous: cloneReleaseID(fixture.links.Previous),
			}
			inspection, err := fixture.transition.Inspect(context.Background(), fixture.goal)
			if err != nil || inspection.Outcome == nil ||
				inspection.Outcome.Status != StatusOperatorActionRequired {
				t.Fatalf("ineligible replacement inspection = %#v, err=%v", inspection, err)
			}
			after := snapshotV2ProtectedTree(t, fixture.configHome)
			if !bytes.Equal(after, before) {
				t.Fatalf("ineligible inspection changed protected tree\nbefore:\n%s\nafter:\n%s", before, after)
			}
			if !linksEqual(*fixture.links, linksBefore) {
				t.Fatalf("ineligible inspection changed links: before=%#v after=%#v", linksBefore, *fixture.links)
			}
		})
	}
}

func TestV2TransitionPostActivationReplacementRequiresExactConfirmationAndSourceCAS(t *testing.T) {
	t.Run("missing and foreign grants do not write", func(t *testing.T) {
		fixture := newV2PostActivationRecoveryFixture(t)
		inspection, err := fixture.transition.Inspect(context.Background(), fixture.goal)
		if err != nil {
			t.Fatal(err)
		}
		before := snapshotV2ProtectedTree(t, fixture.configHome)
		for name, authorization := range map[string]Authorization{
			"missing": "",
			"foreign": v2TestAuthorization(PlanToken("plan-v1-" + strings.Repeat("f", 64))),
		} {
			t.Run(name, func(t *testing.T) {
				outcome, err := fixture.transition.Converge(context.Background(), Execution{
					Plan: inspection.Plan, Authorization: authorization,
				})
				if err != nil || outcome.Status != StatusOperatorActionRequired ||
					outcome.Code != CodeConfirmationRequired {
					t.Fatalf("confirmation outcome = %#v, err=%v", outcome, err)
				}
				after := snapshotV2ProtectedTree(t, fixture.configHome)
				if !bytes.Equal(after, before) {
					t.Fatalf("unauthorized convergence changed protected tree\nbefore:\n%s\nafter:\n%s", before, after)
				}
				lockPath := filepath.Join(fixture.configHome, "migrations", "update.lock")
				if _, statErr := os.Lstat(lockPath); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("unauthorized convergence created update lock: %v", statErr)
				}
			})
		}
	})

	t.Run("source change after confirmation is stale before archive", func(t *testing.T) {
		fixture := newV2PostActivationRecoveryFixture(t)
		inspection, err := fixture.transition.Inspect(context.Background(), fixture.goal)
		if err != nil {
			t.Fatal(err)
		}
		fixture.transition.options.VerifyAuthorization = func(plan PlanToken, authorization Authorization) bool {
			if authorization != v2TestAuthorization(plan) {
				return false
			}
			current, readErr := fixture.transition.store.ReadCurrentJournal()
			if readErr != nil {
				fixture.t.Fatal(readErr)
			}
			journal, parseErr := ParseJournal(current.Payload)
			if parseErr != nil {
				fixture.t.Fatal(parseErr)
			}
			journal.AuthorizationDigest = digestD
			payload, marshalErr := MarshalJournal(journal)
			if marshalErr != nil {
				fixture.t.Fatal(marshalErr)
			}
			if swapErr := fixture.transition.store.CompareAndSwapCurrentJournal(current, payload); swapErr != nil {
				fixture.t.Fatal(swapErr)
			}
			return true
		}
		outcome, err := fixture.transition.Converge(context.Background(), Execution{
			Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
		})
		if err != nil || outcome.Status != StatusOperatorActionRequired || outcome.Code != CodePlanStale {
			t.Fatalf("post-confirmation stale outcome = %#v, err=%v", outcome, err)
		}
		archive, err := fixture.transition.store.ReadSupersededJournal("tx-recovery-v0112")
		if err != nil || archive.Exists {
			t.Fatalf("stale source reached immutable archive: %#v, err=%v", archive, err)
		}
	})

	t.Run("exact grant archives before journal replacement", func(t *testing.T) {
		fixture := newV2PostActivationRecoveryFixture(t)
		fixture.transition.options.ActivateLinks = func(
			_ context.Context,
			pair ReleasePair,
		) (ReleaseLinks, error) {
			*fixture.links = ReleaseLinks{
				Active: pair.Target, Previous: releaseIDPointer(pair.From),
			}
			return *fixture.links, nil
		}
		inspection, err := fixture.transition.Inspect(context.Background(), fixture.goal)
		if err != nil {
			t.Fatal(err)
		}
		outcome, err := fixture.transition.Converge(context.Background(), Execution{
			Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
		})
		if err != nil || outcome.Status != StatusReady || outcome.Transaction == nil ||
			*outcome.Transaction != "tx-recovery-v0112" {
			t.Fatalf("replacement convergence = %#v, err=%v", outcome, err)
		}
		current, err := fixture.transition.store.ReadCurrentJournal()
		if err != nil {
			t.Fatal(err)
		}
		newJournal, err := ParseJournal(current.Payload)
		if err != nil || newJournal.Transaction != "tx-recovery-v0112" || len(newJournal.Steps) != 0 {
			t.Fatalf("replacement journal = %#v, err=%v", newJournal, err)
		}
		if !releasePairsEqual(newJournal.Releases, fixture.transition.options.Releases) ||
			newJournal.Goal.Target != v2PostActivationCandidateRelease ||
			fixture.links.Active != v2PostActivationCandidateRelease ||
			fixture.links.Previous == nil ||
			*fixture.links.Previous != v2PostActivationSourceRelease {
			t.Fatalf("replacement changed opaque release identities: journal=%#v links=%#v",
				newJournal, *fixture.links)
		}
		archiveSnapshot, err := fixture.transition.store.ReadSupersededJournal(newJournal.Transaction)
		if err != nil || !archiveSnapshot.Exists {
			t.Fatalf("superseded journal archive = %#v, err=%v", archiveSnapshot, err)
		}
		archive, err := ParseSupersededJournal(archiveSnapshot.Payload)
		if err != nil || archive.AuthorizationPlan != newJournal.AuthorizationPlan ||
			archive.Replacement != *fixture.transition.options.Replacement {
			t.Fatalf("superseded journal record = %#v, err=%v", archive, err)
		}
		if !releasePairsEqual(archive.Journal.Releases, fixture.sourceJournal.Releases) ||
			archive.Journal.Releases.From != v2PostActivationPreviousRelease ||
			archive.Journal.Releases.Target != v2PostActivationSourceRelease {
			t.Fatalf("archive changed opaque source release identities: %#v", archive.Journal.Releases)
		}
		archivedJournal, err := MarshalJournal(archive.Journal)
		if err != nil {
			t.Fatal(err)
		}
		sourceJournal, err := MarshalJournal(fixture.sourceJournal)
		if err != nil || !bytes.Equal(archivedJournal, sourceJournal) {
			t.Fatalf("archive does not preserve exact source journal: %v", err)
		}
		ledger, err := fixture.transition.store.ReadLedger()
		if err != nil || !bytes.Equal(ledger.Payload, fixture.ledgerPayload) {
			t.Fatalf("replacement changed ledger bytes: %#v, err=%v", ledger, err)
		}
	})
}

func TestV2TransitionPostActivationReplacementResumesEveryDurableBoundary(t *testing.T) {
	tests := []struct {
		point   string
		postCAS bool
	}{
		{point: "after-superseded-journal"},
		{point: "before-replacement-journal-cas"},
		{point: "after-replacement-journal-cas", postCAS: true},
		{point: "after-activation-intent", postCAS: true},
		{point: "after-target-active", postCAS: true},
		{point: "before-reconciler-0", postCAS: true},
		{point: "after-reconciler-0", postCAS: true},
		{point: "before-reconciler-1", postCAS: true},
		{point: "after-reconciler-1", postCAS: true},
		{point: "before-journal-complete", postCAS: true},
	}
	for _, test := range tests {
		t.Run(test.point, func(t *testing.T) {
			fixture := newV2PostActivationRecoveryFixture(t)
			fixture.transition.options.Reconcilers = []V2ActivationReconciler{
				v2NamedReconciler("first-runtime"), v2NamedReconciler("second-runtime"),
			}
			fixture.transition.options.ActivateLinks = func(
				_ context.Context,
				pair ReleasePair,
			) (ReleaseLinks, error) {
				*fixture.links = ReleaseLinks{
					Active: pair.Target, Previous: releaseIDPointer(pair.From),
				}
				return *fixture.links, nil
			}
			authorizationChecks := 0
			fixture.transition.options.VerifyAuthorization = func(
				plan PlanToken,
				authorization Authorization,
			) bool {
				authorizationChecks++
				return authorization == v2TestAuthorization(plan)
			}
			activeFault := test.point
			fixture.transition.options.fault = func(point string) error {
				if point == activeFault {
					return errors.New("injected post-activation replacement fault")
				}
				return nil
			}
			inspection, err := fixture.transition.Inspect(context.Background(), fixture.goal)
			if err != nil || inspection.Resume != nil {
				t.Fatalf("replacement inspection = %#v, err=%v", inspection, err)
			}
			_, _ = fixture.transition.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			current, err := fixture.transition.store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			journal, err := ParseJournal(current.Payload)
			if err != nil {
				t.Fatal(err)
			}
			wantTransaction := fixture.sourceJournal.Transaction
			if test.postCAS {
				wantTransaction = "tx-recovery-v0112"
			}
			if journal.Transaction != wantTransaction {
				t.Fatalf("current transaction at %s = %q, want %q", test.point, journal.Transaction, wantTransaction)
			}
			if test.postCAS && journal.SourceIngress != nil {
				t.Fatalf("post-CAS replacement journal retained source ingress: %#v", journal.SourceIngress)
			}

			activeFault = ""
			resume, err := fixture.transition.Inspect(context.Background(), fixture.goal)
			if err != nil {
				t.Fatal(err)
			}
			execution := Execution{Plan: resume.Plan}
			checksBeforeResume := authorizationChecks
			if !test.postCAS {
				if resume.Resume != nil || resume.Plan != inspection.Plan {
					t.Fatalf("pre-CAS retry inspection = %#v", resume)
				}
				execution.Authorization = v2TestAuthorization(resume.Plan)
			} else if resume.Resume == nil || *resume.Resume != "tx-recovery-v0112" ||
				resume.Plan == inspection.Plan {
				t.Fatalf("post-CAS resume inspection = %#v", resume)
			}
			outcome, err := fixture.transition.Converge(context.Background(), execution)
			if err != nil || outcome.Status != StatusReady || !outcome.ReachedGoal {
				t.Fatalf("resumed replacement outcome = %#v, err=%v", outcome, err)
			}
			if test.postCAS && authorizationChecks != checksBeforeResume {
				t.Fatalf("post-CAS resume requested a second confirmation: before=%d after=%d", checksBeforeResume, authorizationChecks)
			}
			ledger, err := fixture.transition.store.ReadLedger()
			if err != nil || !bytes.Equal(ledger.Payload, fixture.ledgerPayload) {
				t.Fatalf("fault recovery changed ledger bytes: %#v, err=%v", ledger, err)
			}
		})
	}
}

func TestV2TransitionPostActivationReplacementConvergesAcrossStorePublicationFaults(t *testing.T) {
	tests := []struct {
		name       string
		point      string
		occurrence int
		postCAS    bool
	}{
		{name: "archive pending", point: "after-pending-fsync", occurrence: 1},
		{name: "archive published", point: "after-publish-before-dir-fsync", occurrence: 1},
		{name: "journal pending", point: "after-pending-fsync", occurrence: 2},
		{name: "journal published", point: "after-publish-before-dir-fsync", occurrence: 2, postCAS: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newV2PostActivationRecoveryFixture(t)
			fixture.transition.options.ActivateLinks = func(
				_ context.Context,
				pair ReleasePair,
			) (ReleaseLinks, error) {
				*fixture.links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
				return *fixture.links, nil
			}
			seen := 0
			fixture.transition.store.fault = func(point string) error {
				if point != test.point {
					return nil
				}
				seen++
				if seen == test.occurrence {
					return errors.New("injected protected publication fault")
				}
				return nil
			}
			inspection, err := fixture.transition.Inspect(context.Background(), fixture.goal)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = fixture.transition.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			current, err := fixture.transition.store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			journal, err := ParseJournal(current.Payload)
			if err != nil {
				t.Fatal(err)
			}
			want := fixture.sourceJournal.Transaction
			if test.postCAS {
				want = "tx-recovery-v0112"
			}
			if journal.Transaction != want {
				t.Fatalf("current transaction after store fault = %q, want %q", journal.Transaction, want)
			}

			fixture.transition.store.fault = nil
			resume, err := fixture.transition.Inspect(context.Background(), fixture.goal)
			if err != nil {
				t.Fatal(err)
			}
			execution := Execution{Plan: resume.Plan}
			if !test.postCAS {
				execution.Authorization = v2TestAuthorization(resume.Plan)
			} else if resume.Resume == nil || *resume.Resume != "tx-recovery-v0112" {
				t.Fatalf("published journal was not resumable: %#v", resume)
			}
			outcome, err := fixture.transition.Converge(context.Background(), execution)
			if err != nil || outcome.Status != StatusReady {
				t.Fatalf("store fault retry outcome = %#v, err=%v", outcome, err)
			}
			ledger, err := fixture.transition.store.ReadLedger()
			if err != nil || !bytes.Equal(ledger.Payload, fixture.ledgerPayload) {
				t.Fatalf("store fault retry changed ledger: %#v, err=%v", ledger, err)
			}
		})
	}
}

func TestV2TransitionPostActivationReplacementReusesPreCASArchiveAcrossRestart(t *testing.T) {
	fixture := newV2PostActivationRecoveryFixture(t)
	fixture.transition.options.NewTransactionID = func() TransactionID { return "tx-recovery-first" }
	fixture.transition.options.ActivateLinks = func(
		_ context.Context,
		pair ReleasePair,
	) (ReleaseLinks, error) {
		*fixture.links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
		return *fixture.links, nil
	}
	fixture.transition.options.fault = func(point string) error {
		if point == "after-superseded-journal" {
			return errors.New("process stopped after durable archive")
		}
		return nil
	}
	inspection, err := fixture.transition.Inspect(context.Background(), fixture.goal)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fixture.transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	archive, err := fixture.transition.store.ReadSupersededJournal("tx-recovery-first")
	if err != nil || !archive.Exists {
		t.Fatalf("first process archive = %#v, err=%v", archive, err)
	}

	options := fixture.transition.options
	options.fault = nil
	options.NewTransactionID = func() TransactionID { return "tx-recovery-second" }
	restarted, err := NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	reinspection, err := restarted.Inspect(context.Background(), fixture.goal)
	if err != nil || reinspection.Resume != nil || reinspection.Plan != inspection.Plan {
		t.Fatalf("restart inspection = %#v, err=%v", reinspection, err)
	}
	outcome, err := restarted.Converge(context.Background(), Execution{
		Plan: reinspection.Plan, Authorization: v2TestAuthorization(reinspection.Plan),
	})
	if err != nil || outcome.Status != StatusReady || outcome.Transaction == nil ||
		*outcome.Transaction != "tx-recovery-first" {
		t.Fatalf("restart outcome = %#v, err=%v", outcome, err)
	}
	second, err := restarted.store.ReadSupersededJournal("tx-recovery-second")
	if err != nil || second.Exists {
		t.Fatalf("restart created a second predecessor edge: %#v, err=%v", second, err)
	}
}

func TestV2TransitionPostActivationReplacementRejectsUnsafeArchiveGraphBeforeMutation(t *testing.T) {
	tests := []struct {
		name              string
		eligibleAtInspect bool
		setup             func(*v2PostActivationRecoveryFixture)
	}{
		{name: "source already has predecessor", setup: func(f *v2PostActivationRecoveryFixture) {
			createStoreTransaction(f.t, f.transition.store, "tx-older")
			createStoreSupersession(
				f.t, f.transition.store, f.sourceJournal.Transaction, "tx-older",
				f.sourceJournal.AuthorizationPlan,
			)
		}},
		{name: "foreign successor already points to source", eligibleAtInspect: true, setup: func(f *v2PostActivationRecoveryFixture) {
			createStoreSupersession(
				f.t, f.transition.store, "tx-foreign", f.sourceJournal.Transaction,
				PlanToken("plan-v1-"+strings.Repeat("d", 64)),
			)
		}},
		{name: "allocation exceeds graph bound", eligibleAtInspect: true, setup: func(f *v2PostActivationRecoveryFixture) {
			for index := 0; index < maxTransactionGraphEntries-1; index++ {
				createStoreTransaction(
					f.t, f.transition.store, TransactionID(fmt.Sprintf("tx-bound-%03d", index)),
				)
			}
		}},
		{name: "proposed transaction collides", eligibleAtInspect: true, setup: func(f *v2PostActivationRecoveryFixture) {
			createStoreTransaction(f.t, f.transition.store, "tx-recovery-v0112")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newV2PostActivationRecoveryFixture(t)
			test.setup(fixture)
			before := snapshotV2ProtectedTree(t, fixture.configHome)
			inspection, err := fixture.transition.Inspect(context.Background(), fixture.goal)
			if err != nil || inspection.Outcome == nil {
				t.Fatalf("replacement inspection = %#v, err=%v", inspection, err)
			}
			if !test.eligibleAtInspect {
				if inspection.Outcome.Status != StatusOperatorActionRequired {
					t.Fatalf("unsafe source graph inspection = %#v", inspection)
				}
				if after := snapshotV2ProtectedTree(t, fixture.configHome); !bytes.Equal(after, before) {
					t.Fatalf("failed graph inspection changed protected state\nbefore:\n%s\nafter:\n%s", before, after)
				}
				return
			}
			if inspection.Outcome.Status != StatusMigrationRequired {
				t.Fatalf("replacement inspection = %#v", inspection)
			}
			outcome, err := fixture.transition.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			if err == nil && outcome.Status != StatusOperatorActionRequired {
				t.Fatalf("unsafe archive graph convergence = %#v, err=%v", outcome, err)
			}
			after := snapshotV2ProtectedTree(t, fixture.configHome)
			if !bytes.Equal(after, before) {
				t.Fatalf("failed archive admission changed protected state\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestV2TransitionOrdinaryInspectionIgnoresUnrelatedTransactionGraph(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusReady {
		t.Fatalf("initial convergence = %#v, err=%v", outcome, err)
	}
	for index := 0; index <= maxTransactionGraphEntries; index++ {
		createStoreTransaction(
			t, transition.store, TransactionID(fmt.Sprintf("tx-unrelated-%03d", index)),
		)
	}

	reinspection, err := transition.Inspect(context.Background(), goal)
	if err != nil || reinspection.Outcome == nil || reinspection.Outcome.Status != StatusReady {
		t.Fatalf("ordinary inspection = %#v, err=%v", reinspection, err)
	}
}

type v2PostActivationRecoveryFixture struct {
	t             *testing.T
	transition    *V2Transition
	configHome    string
	links         *ReleaseLinks
	goal          Goal
	sourceJournal JournalRecord
	ledgerPayload []byte
}

func newV2PostActivationRecoveryFixture(t *testing.T) *v2PostActivationRecoveryFixture {
	t.Helper()
	configHome := settingsV2Fixture(t, map[string]string{
		"yards/hermes/config.env": "YARD_TEMPLATE='test-vms'\n",
	})
	links := &ReleaseLinks{
		Active:   v2PostActivationSourceRelease,
		Previous: releaseIDPointer(v2PostActivationPreviousRelease),
	}
	sourceOptions := V2Options{
		ConfigHome: configHome, Releases: ReleasePair{
			From: v2PostActivationPreviousRelease, Target: v2PostActivationSourceRelease,
		},
		ObserveLinks:        func(context.Context) (ReleaseLinks, error) { return *links, nil },
		RegistryPayload:     v2RegistryPayload(t),
		ArtifactDigest:      digestA,
		OwnerRegistration:   &v2TestOwnerRegistration{state: OwnerRegistrationAbsent},
		NewTransactionID:    func() TransactionID { return "tx-source-v0111" },
		VerifyAuthorization: func(PlanToken, Authorization) bool { return false },
	}
	source, err := NewV2Transition(sourceOptions)
	if err != nil {
		t.Fatal(err)
	}
	journal, sourceSnapshot, ledgerPayload := seedV0111PostActivationJournal(t, source)
	replacement := &JournalReplacement{
		Transaction: journal.Transaction, Fingerprint: sourceSnapshot.Fingerprint,
		Reason: JournalReplacementPostActivationScopeV0111, SourceVersion: "0.11.1",
	}
	candidateOptions := sourceOptions
	candidateOptions.Releases = ReleasePair{
		From: v2PostActivationSourceRelease, Previous: releaseIDPointer(v2PostActivationPreviousRelease),
		Target: v2PostActivationCandidateRelease,
	}
	candidateOptions.CandidateVersion = "0.11.2"
	candidateOptions.ArtifactDigest = digestB
	candidateOptions.Replacement = replacement
	candidateOptions.NewTransactionID = func() TransactionID { return "tx-recovery-v0112" }
	candidateOptions.VerifyAuthorization = func(plan PlanToken, authorization Authorization) bool {
		return authorization == v2TestAuthorization(plan)
	}
	candidate, err := NewV2Transition(candidateOptions)
	if err != nil {
		t.Fatal(err)
	}
	return &v2PostActivationRecoveryFixture{
		t: t, transition: candidate, configHome: configHome, links: links,
		goal:          Goal{Target: v2PostActivationCandidateRelease, Direction: DirectionActivateTarget},
		sourceJournal: journal, ledgerPayload: ledgerPayload,
	}
}

func (fixture *v2PostActivationRecoveryFixture) rewriteJournal(change func(*JournalRecord)) {
	fixture.t.Helper()
	current, err := fixture.transition.store.ReadCurrentJournal()
	if err != nil {
		fixture.t.Fatal(err)
	}
	journal, err := ParseJournal(current.Payload)
	if err != nil {
		fixture.t.Fatal(err)
	}
	change(&journal)
	journal.IntentDigest = bindJournalIntent(
		journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps,
	)
	payload, err := MarshalJournal(journal)
	if err != nil {
		fixture.t.Fatal(err)
	}
	if err := fixture.transition.store.CompareAndSwapCurrentJournal(current, payload); err != nil {
		fixture.t.Fatal(err)
	}
	updated, err := fixture.transition.store.ReadCurrentJournal()
	if err != nil {
		fixture.t.Fatal(err)
	}
	fixture.sourceJournal = journal
	fixture.transition.options.Replacement.Transaction = journal.Transaction
	fixture.transition.options.Replacement.Fingerprint = updated.Fingerprint
}

func (fixture *v2PostActivationRecoveryFixture) removeEvidence(
	step string,
	checkpoint EvidenceCheckpoint,
) {
	fixture.t.Helper()
	name, err := checkpointEvidenceName(fixture.sourceJournal.Transaction, step, checkpoint)
	if err != nil {
		fixture.t.Fatal(err)
	}
	path := filepath.Join(
		fixture.configHome, "release-transition", "v2", "transactions",
		string(fixture.sourceJournal.Transaction), "evidence", name,
	)
	if err := os.Remove(path); err != nil {
		fixture.t.Fatal(err)
	}
}

func snapshotV2ProtectedTree(t *testing.T, configHome string) []byte {
	t.Helper()
	root := filepath.Join(configHome, "release-transition", "v2")
	var snapshot bytes.Buffer
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		snapshot.WriteString(relative)
		snapshot.WriteByte('\t')
		snapshot.WriteString(info.Mode().String())
		snapshot.WriteByte('\n')
		if info.Mode().IsRegular() {
			payload, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			snapshot.Write(payload)
			snapshot.WriteByte('\n')
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.Bytes()
}

func seedV0111PostActivationJournal(
	t *testing.T,
	transition *V2Transition,
) (JournalRecord, ProtectedSnapshot, []byte) {
	t.Helper()
	pair := transition.options.Releases
	ledger := BaselineLedgerV2(transition.registry)
	ledgerSnapshot := absentProtectedSnapshot()
	steps := make([]JournalStep, 0, len(transition.registry.Migrations))
	for _, migration := range transition.registry.Migrations {
		advanced, err := ledger.Advance(transition.registry, migration)
		if err != nil {
			t.Fatal(err)
		}
		payload, desired, err := MarshalLedgerV2(advanced, transition.registry)
		if err != nil {
			t.Fatal(err)
		}
		step := JournalStep{
			ID: migration.ID + ".ledger", Migration: migration.ID,
			Resource: "ledger." + migration.Domain, Decision: DecisionTransform,
			Expected: ledgerSnapshot.Fingerprint, Desired: desired, Checkpoint: StepVerified,
		}
		evidence := EvidenceRecord{
			SchemaVersion: JournalSchemaV2, Transaction: "tx-source-v0111",
			Releases: pair, Step: step.ID, Expected: step.Expected, Desired: step.Desired,
			Observed: step.Desired, Checkpoint: EvidenceVerified,
		}
		step.Evidence = &evidence
		steps = append(steps, step)
		ledger, ledgerSnapshot = advanced, protectedSnapshotFromPayload(payload)
	}
	journal := JournalRecord{
		SchemaVersion: JournalSchemaV2, Transaction: "tx-source-v0111",
		Goal: Goal{Target: pair.Target, Direction: DirectionActivateTarget}, Releases: pair,
		AuthorizationPlan: PlanToken("plan-v1-" + strings.Repeat("a", 64)),
		ResumePlan:        PlanToken("resume-v1-" + strings.Repeat("b", 64)),
		ArtifactDigest:    transition.options.ArtifactDigest,
		RegistryDigest:    transition.registryDigest, CatalogDigest: transition.catalog.Digest(),
		ObservationScope: digestD, AuthorizationDigest: digestC,
		Checkpoint: JournalReconciling, Steps: steps,
	}
	journal.IntentDigest = bindJournalIntent(
		journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps,
	)
	if err := transition.store.CompareAndSwapLedger(absentProtectedSnapshot(), ledgerSnapshot.Payload); err != nil {
		t.Fatal(err)
	}
	for _, step := range journal.Steps {
		for _, checkpoint := range []EvidenceCheckpoint{EvidenceCaptured, EvidenceApplied, EvidenceVerified} {
			observed := step.Desired
			if checkpoint == EvidenceCaptured {
				observed = step.Expected
			}
			evidence := EvidenceRecord{
				SchemaVersion: JournalSchemaV2, Transaction: journal.Transaction,
				Releases: pair, Step: step.ID, Expected: step.Expected, Desired: step.Desired,
				Observed: observed, Checkpoint: checkpoint,
			}
			payload, err := MarshalEvidence(evidence)
			if err != nil {
				t.Fatal(err)
			}
			if err := transition.store.CreateCheckpointEvidence(
				journal.Transaction, step.ID, checkpoint, payload,
			); err != nil {
				t.Fatal(err)
			}
		}
	}
	payload, err := MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	if err := transition.store.CompareAndSwapCurrentJournal(missing, payload); err != nil {
		t.Fatal(err)
	}
	snapshot, err := transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	return journal, snapshot, ledgerSnapshot.Payload
}

func TestV2TransitionResumesEveryDurableCheckpointWithoutNewAuthorization(t *testing.T) {
	points := []string{
		"after-journal-authorized", "after-recovery", "after-step-evidence",
		"after-settings-cas", "after-step-applied", "after-step-verified",
		"after-ledger-cas", "after-ledger-verified", "after-journal-complete",
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			injected := errors.New("injected transition fault")
			activeFault := point
			transition, _, _ := v2TransitionFixture(t, func(actual string) error {
				if actual == activeFault {
					return injected
				}
				return nil
			})
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			interrupted, err := transition.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			if err != nil {
				t.Fatalf("fault outcome = %#v, err=%v", interrupted, err)
			}
			if point == "after-journal-complete" {
				if interrupted.Status != StatusReady {
					t.Fatalf("completed fault outcome = %#v", interrupted)
				}
			} else if interrupted.Status != StatusRecovering ||
				interrupted.Code != CodeVerificationFailed {
				t.Fatalf("recoverable fault outcome = %#v", interrupted)
			}
			activeFault = ""
			resume, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			if point != "after-journal-complete" && resume.Resume == nil {
				t.Fatalf("checkpoint %s did not expose resumable transaction: %#v", point, resume)
			}
			outcome, err := transition.Converge(context.Background(), Execution{Plan: resume.Plan})
			if err != nil || outcome.Status != StatusReady || !outcome.ReachedGoal {
				t.Fatalf("resumed outcome = %#v, err=%v", outcome, err)
			}
		})
	}
}

func TestV2TransitionBlocksThirdResourceStateWithoutOverwrite(t *testing.T) {
	injected := errors.New("stop before settings CAS")
	active := true
	transition, _, path := v2TransitionFixture(t, func(point string) error {
		if active && point == "after-step-evidence" {
			return injected
		}
		return nil
	})
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || interrupted.Status != StatusRecovering ||
		interrupted.Code != CodeVerificationFailed {
		t.Fatalf("fault outcome = %#v, err=%v", interrupted, err)
	}
	third := []byte("YARD_TEMPLATE=test-vms\nNESTED_E2E_VMS=1\n")
	if err := os.WriteFile(path, third, 0o600); err != nil {
		t.Fatal(err)
	}
	active = false
	resume, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	if resume.Outcome == nil ||
		resume.Outcome.Status != StatusOperatorActionRequired ||
		resume.Outcome.Code != CodeMigrationStale || resume.Resume == nil ||
		len(resume.Blockers) == 0 || resume.Blockers[0].Code != CodeMigrationStale {
		t.Fatalf("third-state inspection = %#v", resume)
	}
	beforeConverge, err := os.ReadFile(path)
	if err != nil || string(beforeConverge) != string(third) {
		t.Fatalf("read-only inspection changed third state: %q, err=%v", beforeConverge, err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: resume.Plan})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != StatusOperatorActionRequired || outcome.Code != CodeMigrationStale {
		t.Fatalf("third-state outcome = %#v", outcome)
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != string(third) {
		t.Fatalf("third state was overwritten: %q, err=%v", actual, err)
	}
}

func TestV2TransitionBindsRecoveryToExactBeforeState(t *testing.T) {
	injected := errors.New("stop after recovery evidence")
	active := true
	transition, configHome, settingsPath := v2TransitionFixture(t, func(point string) error {
		if active && point == "after-recovery" {
			return injected
		}
		return nil
	})
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || interrupted.Status != StatusRecovering ||
		interrupted.Code != CodeVerificationFailed {
		t.Fatalf("fault outcome = %#v, err=%v", interrupted, err)
	}
	recoveryPath := filepath.Join(
		configHome, "release-transition", "v2", "transactions", "tx-test-001", "recovery",
		"canonicalize-test-vms-settings-v2.hermes.json",
	)
	payload, err := os.ReadFile(recoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	var recovery settingsRecoveryV1
	if err := json.Unmarshal(payload, &recovery); err != nil {
		t.Fatal(err)
	}
	recovery.Before[0].Value = "test-vms"
	payload, err = json.Marshal(recovery)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recoveryPath, append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	active = false
	resume, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: resume.Plan})
	if err != nil || outcome.Status != StatusOperatorActionRequired ||
		outcome.Code != CodeRecoveryAmbiguous {
		t.Fatalf("mismatched typed recovery outcome = %#v, err=%v", outcome, err)
	}
	actual, err := os.ReadFile(settingsPath)
	if err != nil || !strings.Contains(string(actual), "YARD_TEMPLATE=e2e-vms") {
		t.Fatalf("settings changed despite recovery mismatch: %q, err=%v", actual, err)
	}
}

func TestV2TransitionRejectsNonInitialLinksBeforeAnyMutation(t *testing.T) {
	for name, links := range map[string]ReleaseLinks{
		"target without journal": {Active: "release-b", Previous: releaseIDPointer("release-a")},
		"foreign pair":           {Active: "foreign", Previous: releaseIDPointer("release-old")},
	} {
		t.Run(name, func(t *testing.T) {
			transition, configHome, settingsPath := v2TransitionFixtureWithReleases(
				t, nil, ReleasePair{From: "release-a", Target: "release-b"}, links,
			)
			goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil || len(inspection.Blockers) != 1 ||
				inspection.Blockers[0].Code != CodeActivationAmbiguous {
				t.Fatalf("inspection = %#v, err=%v", inspection, err)
			}
			outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan})
			if err != nil || outcome.Code != CodeActivationAmbiguous {
				t.Fatalf("outcome = %#v, err=%v", outcome, err)
			}
			content, err := os.ReadFile(settingsPath)
			if err != nil || !strings.Contains(string(content), "YARD_TEMPLATE=e2e-vms") {
				t.Fatalf("settings mutated: %q, err=%v", content, err)
			}
			store, _ := NewPOSIXV2Store(configHome)
			ledger, ledgerErr := store.ReadLedger()
			journal, journalErr := store.ReadCurrentJournal()
			if ledgerErr != nil || journalErr != nil || ledger.Exists || journal.Exists {
				t.Fatalf("metadata mutated: ledger=%#v journal=%#v errors=%v/%v",
					ledger, journal, ledgerErr, journalErr)
			}
		})
	}
}

func TestV2TransitionCrossReleaseStopsAtActivationIntent(t *testing.T) {
	transition, _, _ := v2TransitionFixtureWithReleases(t, nil, ReleasePair{
		From: "release-a", Target: "release-b",
	}, ReleaseLinks{Active: "release-a"})
	goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != StatusRecovering || outcome.Code != CodeRecoveryPending ||
		outcome.Active != "release-a" || outcome.Target != "release-b" {
		t.Fatalf("cross-release outcome = %#v", outcome)
	}
}

func TestV2TransitionOwnsActivationAndResumesAfterLinkSwitch(t *testing.T) {
	injected := errors.New("crash after target-active journal")
	activeFault := "after-target-active"
	transition, _, _ := v2TransitionFixtureWithReleases(t, func(point string) error {
		if point == activeFault {
			return injected
		}
		return nil
	}, ReleasePair{From: "release-a", Target: "release-b"}, ReleaseLinks{Active: "release-a"})
	links := ReleaseLinks{Active: "release-a"}
	transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return links, nil }
	transition.options.ActivateLinks = func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
		links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
		return links, nil
	}
	reconciler := &v2TestReconciler{}
	transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
	goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || interrupted.Status != StatusRecovering ||
		interrupted.Code != CodeVerificationFailed {
		t.Fatalf("activation fault outcome = %#v, err=%v", interrupted, err)
	}
	if links.Active != "release-b" || reconciler.reconciles != 0 {
		t.Fatalf("post-switch facts: links=%#v reconciles=%d", links, reconciler.reconciles)
	}
	activeFault = ""
	resume, err := transition.Inspect(context.Background(), goal)
	if err != nil || resume.Resume == nil {
		t.Fatalf("activation resume = %#v, err=%v", resume, err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: resume.Plan})
	if err != nil || outcome.Status != StatusReady || !outcome.ReachedGoal || reconciler.reconciles != 1 {
		t.Fatalf("activation outcome = %#v, reconciles=%d, err=%v", outcome, reconciler.reconciles, err)
	}
}

func TestFreshV2TransitionResumesOriginalPairAtStableLinkCheckpoints(t *testing.T) {
	checkpoints := []struct {
		name            string
		activate        func(*ReleaseLinks, ReleasePair) (ReleaseLinks, error)
		transitionFault string
	}{
		{
			name: "staged links",
			activate: func(links *ReleaseLinks, pair ReleasePair) (ReleaseLinks, error) {
				*links = ReleaseLinks{Active: pair.From, Previous: releaseIDPointer(pair.Target)}
				return *links, errors.New("interrupted after staging links")
			},
		},
		{
			name: "target active",
			activate: func(links *ReleaseLinks, pair ReleasePair) (ReleaseLinks, error) {
				*links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
				return *links, nil
			},
			transitionFault: "after-target-active",
		},
	}
	for _, checkpoint := range checkpoints {
		t.Run(checkpoint.name, func(t *testing.T) {
			links := ReleaseLinks{Active: "release-a"}
			transition, configHome, _ := v2TransitionFixtureWithReleases(
				t, func(point string) error {
					if point == checkpoint.transitionFault {
						return errors.New("interrupted at durable checkpoint")
					}
					return nil
				},
				ReleasePair{From: "release-a", Target: "release-b"}, links,
			)
			transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) {
				return links, nil
			}
			transition.options.ActivateLinks = func(
				_ context.Context,
				pair ReleasePair,
			) (ReleaseLinks, error) {
				return checkpoint.activate(&links, pair)
			}
			goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			interrupted, err := transition.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			if err != nil || interrupted.Status != StatusRecovering ||
				interrupted.Code != CodeVerificationFailed ||
				interrupted.Transaction == nil || *interrupted.Transaction != "tx-test-001" {
				t.Fatalf("checkpoint outcome = %#v, err=%v", interrupted, err)
			}

			observedPair := ReleasePair{
				From: links.Active, Previous: cloneReleaseID(links.Previous), Target: "release-b",
			}
			fresh, err := NewV2Transition(V2Options{
				ConfigHome: configHome, Releases: observedPair,
				ObserveLinks: func(context.Context) (ReleaseLinks, error) { return links, nil },
				ActivateLinks: func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
					links = ReleaseLinks{
						Active: pair.Target, Previous: releaseIDPointer(pair.From),
					}
					return links, nil
				},
				RegistryPayload: v2RegistryPayload(t), ArtifactDigest: digestA,
				OwnerRegistration:   &v2TestOwnerRegistration{state: OwnerRegistrationCurrent},
				VerifyAuthorization: func(PlanToken, Authorization) bool { return false },
			})
			if err != nil {
				t.Fatal(err)
			}
			resume, err := fresh.Inspect(context.Background(), goal)
			if err != nil || resume.Resume == nil || *resume.Resume != "tx-test-001" {
				t.Fatalf("fresh inspection = %#v, err=%v", resume, err)
			}
			outcome, err := fresh.Converge(context.Background(), Execution{Plan: resume.Plan})
			if err != nil || outcome.Status != StatusReady || !outcome.ReachedGoal ||
				outcome.Transaction == nil || *outcome.Transaction != "tx-test-001" {
				t.Fatalf("fresh outcome = %#v, err=%v", outcome, err)
			}
		})
	}
}

func TestV2TransitionReturnsStructuredOutcomeAfterMutationFault(t *testing.T) {
	for _, point := range []string{"after-settings-cas", "after-step-applied", "after-target-active"} {
		t.Run(point, func(t *testing.T) {
			links := ReleaseLinks{Active: "release-a"}
			releases := ReleasePair{From: "release-a", Target: "release-a"}
			if point == "after-target-active" {
				releases.Target = "release-b"
			}
			transition, _, _ := v2TransitionFixtureWithReleases(t, func(actual string) error {
				if actual == point {
					return errors.New("private injected failure")
				}
				return nil
			}, releases, links)
			transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) {
				return links, nil
			}
			if releases.From != releases.Target {
				transition.options.ActivateLinks = func(
					_ context.Context,
					pair ReleasePair,
				) (ReleaseLinks, error) {
					links = ReleaseLinks{
						Active: pair.Target, Previous: releaseIDPointer(pair.From),
					}
					return links, nil
				}
			}
			goal := Goal{Target: releases.Target, Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := transition.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			if err != nil || outcome.Status != StatusRecovering ||
				outcome.Code != CodeVerificationFailed || outcome.Active != links.Active ||
				outcome.Target != releases.Target || outcome.Transaction == nil ||
				*outcome.Transaction != "tx-test-001" || outcome.Retry != "run yard update" ||
				strings.Contains(outcome.Message, "private injected failure") {
				t.Fatalf("fault outcome = %#v, err=%v", outcome, err)
			}
		})
	}
}

func TestV2TransitionGuardsCheckpointLinksBeforeMutation(t *testing.T) {
	checkpoints := []struct {
		name       string
		fault      string
		unexpected ReleaseLinks
	}{
		{name: "authorized", fault: "after-journal-authorized", unexpected: ReleaseLinks{Active: "release-b", Previous: releaseIDPointer("release-a")}},
		{name: "migrating", fault: "after-step-evidence", unexpected: ReleaseLinks{Active: "foreign", Previous: releaseIDPointer("release-old")}},
		{name: "activation intent", fault: "after-activation-intent", unexpected: ReleaseLinks{Active: "foreign", Previous: releaseIDPointer("release-old")}},
		{name: "target active", fault: "after-target-active", unexpected: ReleaseLinks{Active: "release-a"}},
		{name: "reconciling", fault: "after-reconciling", unexpected: ReleaseLinks{Active: "release-a"}},
	}
	for _, test := range checkpoints {
		t.Run(test.name, func(t *testing.T) {
			stop := errors.New("checkpoint stop")
			activeFault := test.fault
			links := ReleaseLinks{Active: "release-a"}
			transition, _, settingsPath := v2TransitionFixtureWithReleases(t, func(point string) error {
				if point == activeFault {
					return stop
				}
				return nil
			}, ReleasePair{From: "release-a", Target: "release-b"}, links)
			transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return links, nil }
			transition.options.ActivateLinks = func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
				links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
				return links, nil
			}
			transition.options.Reconcilers = []V2ActivationReconciler{&v2TestReconciler{}}
			goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			interrupted, err := transition.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			if err != nil || interrupted.Status != StatusRecovering ||
				interrupted.Code != CodeVerificationFailed {
				t.Fatalf("checkpoint fault outcome = %#v, err=%v", interrupted, err)
			}
			before, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			links = test.unexpected
			activeFault = ""
			resume, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := transition.Converge(context.Background(), Execution{Plan: resume.Plan})
			if err != nil || outcome.Code != CodeActivationAmbiguous || outcome.Status != StatusOperatorActionRequired {
				t.Fatalf("guard outcome = %#v, err=%v", outcome, err)
			}
			after, err := os.ReadFile(settingsPath)
			if err != nil || string(after) != string(before) {
				t.Fatalf("settings changed after invalid links: before=%q after=%q err=%v", before, after, err)
			}
		})
	}
}

func TestV2TransitionDoesNotReopenCompletedMigrationForReconcilerDrift(t *testing.T) {
	links := ReleaseLinks{Active: "release-a"}
	reconciler := &v2TestReconciler{}
	transition, configHome, _ := v2TransitionFixtureWithReleases(
		t, nil, ReleasePair{From: "release-a", Target: "release-a"}, links,
	)
	transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	}); err != nil {
		t.Fatal(err)
	}
	store, _ := NewPOSIXV2Store(configHome)
	before, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	observes := reconciler.observes
	reconciles := reconciler.reconciles
	reconciler.converged = false
	repeat, err := transition.Inspect(context.Background(), goal)
	if err != nil || repeat.Outcome == nil || repeat.Outcome.Status != StatusReady ||
		repeat.Assessment.Changed || repeat.Outcome.Transaction == nil ||
		*repeat.Outcome.Transaction != "tx-test-001" {
		t.Fatalf("drift inspection = %#v, err=%v", repeat, err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: repeat.Plan})
	if err != nil || outcome.Status != StatusReady || reconciler.converged ||
		reconciler.observes != observes || reconciler.reconciles != reconciles {
		t.Fatalf("drift outcome = %#v reconciler=%#v err=%v", outcome, reconciler, err)
	}
	after, err := store.ReadLedger()
	if err != nil || before.Fingerprint != after.Fingerprint {
		t.Fatalf("ledger changed during reconcile: before=%#v after=%#v err=%v", before, after, err)
	}
}

func TestV2TransitionReconcilesDriftForNextReleaseAfterCompletedMigration(t *testing.T) {
	links := ReleaseLinks{Active: "release-a"}
	reconciler := &v2TestReconciler{}
	completed, _, _ := v2TransitionFixtureWithReleases(
		t, nil, ReleasePair{From: "release-a", Target: "release-a"}, links,
	)
	completed.options.Reconcilers = []V2ActivationReconciler{reconciler}
	goalA := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := completed.Inspect(context.Background(), goalA)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, convergeErr := completed.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	}); convergeErr != nil || outcome.Status != StatusReady {
		t.Fatalf("initial convergence = %#v, err=%v", outcome, convergeErr)
	}

	reconciler.converged = false
	reconciles := reconciler.reconciles
	options := completed.options
	options.Releases = ReleasePair{From: "release-a", Target: "release-b"}
	options.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return links, nil }
	options.ActivateLinks = func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
		links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
		return links, nil
	}
	options.NewTransactionID = func() TransactionID { return "tx-test-002" }
	next, err := NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	goalB := Goal{Target: "release-b", Direction: DirectionActivateTarget}
	nextInspection, err := next.Inspect(context.Background(), goalB)
	if err != nil || nextInspection.Outcome == nil ||
		nextInspection.Outcome.Status != StatusMigrationRequired ||
		!nextInspection.Assessment.Changed {
		t.Fatalf("next release inspection = %#v, err=%v", nextInspection, err)
	}
	outcome, err := next.Converge(context.Background(), Execution{
		Plan: nextInspection.Plan, Authorization: v2TestAuthorization(nextInspection.Plan),
	})
	if err != nil || outcome.Status != StatusReady || outcome.Active != "release-b" ||
		!reconciler.converged || reconciler.reconciles != reconciles+1 {
		t.Fatalf("next release convergence = %#v reconciler=%#v err=%v", outcome, reconciler, err)
	}
}

func TestV2TransitionDoesNotAttributeForeignCompletedJournalToBlockedGoal(t *testing.T) {
	transition, configHome, _ := v2TransitionFixture(t, nil)
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || completed.Status != StatusReady || completed.Transaction == nil {
		t.Fatalf("initial convergence = %#v, err=%v", completed, err)
	}

	foreign, err := NewV2Transition(V2Options{
		ConfigHome: configHome,
		Releases:   ReleasePair{From: "release-a", Target: "release-b"},
		ObserveLinks: func(context.Context) (ReleaseLinks, error) {
			return ReleaseLinks{Active: "release-b", Previous: releaseIDPointer("release-a")}, nil
		},
		RegistryPayload:   v2RegistryPayload(t),
		ArtifactDigest:    digestA,
		OwnerRegistration: &v2TestOwnerRegistration{state: OwnerRegistrationAbsent},
		VerifyAuthorization: func(plan PlanToken, authorization Authorization) bool {
			return authorization == v2TestAuthorization(plan)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	foreignGoal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
	blocked, err := foreign.Inspect(context.Background(), foreignGoal)
	if err != nil || blocked.Outcome == nil ||
		blocked.Outcome.Status != StatusOperatorActionRequired ||
		blocked.Outcome.Code != CodeActivationAmbiguous || blocked.Outcome.Transaction != nil {
		t.Fatalf("foreign journal inspection = %#v, err=%v", blocked, err)
	}
	outcome, err := foreign.Converge(context.Background(), Execution{
		Plan: blocked.Plan, Authorization: v2TestAuthorization(blocked.Plan),
	})
	if err != nil || outcome.Status != StatusOperatorActionRequired ||
		outcome.Code != CodeActivationAmbiguous || outcome.Transaction != nil {
		t.Fatalf("foreign journal convergence = %#v, err=%v", outcome, err)
	}
}

func TestV2TransitionCannotReplaceUnsafeCompletedJournalForSameGoal(t *testing.T) {
	links := ReleaseLinks{Active: "release-a"}
	transition, _, _ := v2TransitionFixtureWithReleases(
		t, nil, ReleasePair{From: "release-a", Target: "release-b"}, links,
	)
	transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return links, nil }
	transition.options.ActivateLinks = func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
		links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
		return links, nil
	}
	goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, convergeErr := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	}); convergeErr != nil || outcome.Status != StatusReady {
		t.Fatalf("initial convergence = %#v, err=%v", outcome, convergeErr)
	}

	links = ReleaseLinks{Active: "release-a"}
	unsafe, err := transition.Inspect(context.Background(), goal)
	if err != nil || unsafe.Outcome == nil ||
		unsafe.Outcome.Status != StatusOperatorActionRequired ||
		unsafe.Outcome.Code != CodeActivationAmbiguous {
		t.Fatalf("unsafe completed inspection = %#v, err=%v", unsafe, err)
	}
	before, err := transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: unsafe.Plan, Authorization: v2TestAuthorization(unsafe.Plan),
	})
	if err != nil || outcome.Status != StatusOperatorActionRequired ||
		outcome.Code != CodeActivationAmbiguous || links.Active != "release-a" {
		t.Fatalf("unsafe completed convergence = %#v links=%#v err=%v", outcome, links, err)
	}
	after, err := transition.store.ReadCurrentJournal()
	if err != nil || before.Fingerprint != after.Fingerprint {
		t.Fatalf("unsafe completed journal changed: before=%#v after=%#v err=%v", before, after, err)
	}
}

func TestV2TransitionNamesFailedActivationReconcilerAndPhase(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	transition.options.Reconcilers = []V2ActivationReconciler{
		&v2FailingReconciler{id: "host-power", reconcileErr: v2PhasedError{
			phase: "apply", err: errors.New("private platform detail"),
		}},
	}
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusRecovering ||
		outcome.Code != CodeDependencyUnavailable || outcome.Active != "release-a" ||
		outcome.Target != "release-a" || outcome.Transaction == nil ||
		*outcome.Transaction != "tx-test-001" || outcome.Retry != "run yard update" ||
		!strings.Contains(outcome.Message, `activation reconciler "host-power"`) ||
		!strings.Contains(outcome.Message, "apply") ||
		strings.Contains(outcome.Message, "private platform detail") {
		t.Fatalf("diagnostic outcome = %#v, err=%v", outcome, err)
	}
}

func TestV2TransitionClassifiesActivationTopologyConflict(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	transition.options.Reconcilers = []V2ActivationReconciler{
		v2ObservationErrorReconciler{
			id: "test-vm-broker", err: v2ActivationConflictError{},
		},
	}
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil || inspection.Outcome == nil ||
		inspection.Outcome.Status != StatusOperatorActionRequired ||
		inspection.Outcome.Code != CodeActivationAmbiguous {
		t.Fatalf("topology conflict inspection = %#v, err=%v", inspection, err)
	}
}

func TestV2TransitionReportsActivationObservationFailureAsBlocker(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	transition.options.Reconcilers = []V2ActivationReconciler{
		v2ObservationErrorReconciler{id: "test-vm-broker", err: errors.New("private route detail")},
	}
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}

	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil || inspection.Outcome == nil ||
		inspection.Outcome.Status != StatusOperatorActionRequired ||
		inspection.Outcome.Code != CodeDependencyUnavailable ||
		inspection.Outcome.Retry != "run yard update --check" ||
		len(inspection.Blockers) != 1 ||
		inspection.Blockers[0].Resource != "activation.test-vm-broker" ||
		strings.Contains(inspection.Outcome.Message, "private route detail") {
		t.Fatalf("activation observation inspection = %#v, err=%v", inspection, err)
	}
}

func TestV2TransitionDoesNotReopenCompletedMigrationForActivationObservationFailure(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	transition.options.Reconcilers = []V2ActivationReconciler{&v2TestReconciler{}}
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, convergeErr := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	}); convergeErr != nil || outcome.Status != StatusReady {
		t.Fatalf("initial outcome = %#v, err=%v", outcome, convergeErr)
	}

	transition.options.Reconcilers = []V2ActivationReconciler{
		v2ObservationErrorReconciler{id: "test-runtime", err: errors.New("private completed detail")},
	}
	reinspection, err := transition.Inspect(context.Background(), goal)
	if err != nil || reinspection.Outcome == nil ||
		reinspection.Outcome.Status != StatusReady ||
		reinspection.Outcome.Code != CodeReady || reinspection.Assessment.Changed ||
		reinspection.Outcome.Transaction == nil ||
		*reinspection.Outcome.Transaction != "tx-test-001" ||
		strings.Contains(reinspection.Outcome.Message, "private completed detail") {
		t.Fatalf("completed activation inspection = %#v, err=%v", reinspection, err)
	}
}

func TestV2TransitionReportsCleanupFailureAsReadyWarning(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, func(point string) error {
		if point == "before-recovery-gc" {
			return errors.New("injected cleanup failure")
		}
		return nil
	})
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusReady || !outcome.ReachedGoal ||
		len(outcome.Warnings) != 1 || outcome.Warnings[0] != "recovery cleanup is pending" {
		t.Fatalf("cleanup outcome = %#v, err=%v", outcome, err)
	}
}

func TestV2TransitionRevalidatesLinksImmediatelyBeforeReconcilerMutation(t *testing.T) {
	stop := errors.New("stop while reconciling")
	activeFault := "after-reconciling"
	activated := ReleaseLinks{Active: "release-b", Previous: releaseIDPointer("release-a")}
	links := ReleaseLinks{Active: "release-a"}
	transition, _, _ := v2TransitionFixtureWithReleases(t, func(point string) error {
		if point == activeFault {
			return stop
		}
		return nil
	}, ReleasePair{From: "release-a", Target: "release-b"}, links)
	transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return links, nil }
	transition.options.ActivateLinks = func(context.Context, ReleasePair) (ReleaseLinks, error) {
		links = activated
		return links, nil
	}
	reconciler := &v2TestReconciler{}
	transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
	goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || interrupted.Status != StatusRecovering ||
		interrupted.Code != CodeVerificationFailed {
		t.Fatalf("reconciling fault outcome = %#v, err=%v", interrupted, err)
	}
	activeFault = ""
	resume, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) {
		reads++
		if reads >= 3 {
			return ReleaseLinks{Active: "foreign", Previous: releaseIDPointer("release-old")}, nil
		}
		return activated, nil
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: resume.Plan})
	if err != nil || outcome.Code != CodeActivationAmbiguous || reconciler.reconciles != 0 {
		t.Fatalf("reconciler guard outcome=%#v reconciler=%#v reads=%d err=%v", outcome, reconciler, reads, err)
	}
}

func TestV2TransitionReducesPostMutationFailureWhileHoldingUpdateLock(t *testing.T) {
	reducing := false
	lockHeld := false
	var probeErr error
	transition, configHome, _ := v2TransitionFixture(t, func(point string) error {
		if point == "after-settings-cas" {
			reducing = true
			return errors.New("interrupted after settings mutation")
		}
		return nil
	})
	transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) {
		if !reducing {
			return ReleaseLinks{Active: "release-a"}, nil
		}
		fd, err := unix.Open(
			filepath.Join(configHome, "migrations", "update.lock"),
			unix.O_RDWR|unix.O_CLOEXEC,
			0,
		)
		if err != nil {
			probeErr = err
			return ReleaseLinks{Active: "release-a"}, nil
		}
		defer unix.Close(fd)
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			_ = unix.Flock(fd, unix.LOCK_UN)
		} else if errors.Is(err, unix.EWOULDBLOCK) {
			lockHeld = true
		} else {
			probeErr = err
		}
		return ReleaseLinks{Active: "release-a"}, nil
	}
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusRecovering || probeErr != nil || !lockHeld {
		t.Fatalf(
			"outcome=%#v err=%v lockHeld=%t probeErr=%v",
			outcome, err, lockHeld, probeErr,
		)
	}
}

func TestV2TransitionReducesFirstJournalPostPublicationFailure(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	injected := errors.New("journal published before directory fsync failed")
	transition.store.fault = func(point string) error {
		if point == "after-publish-before-dir-fsync" {
			return injected
		}
		return nil
	}
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusRecovering ||
		outcome.Code != CodeVerificationFailed || outcome.Transaction == nil ||
		*outcome.Transaction != "tx-test-001" {
		t.Fatalf("post-publication outcome = %#v, err=%v", outcome, err)
	}
	current, err := transition.store.ReadCurrentJournal()
	if err != nil || !current.Exists {
		t.Fatalf("published journal = %#v, err=%v", current, err)
	}
}

func TestV2TransitionReplacesOrphanedFirstJournalPendingWriteAcrossProcesses(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	injected := errors.New("process crashed after first journal pending fsync")
	transition.store.fault = func(point string) error {
		if point == "after-pending-fsync" {
			return injected
		}
		return nil
	}
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	}); !errors.Is(err, injected) {
		t.Fatalf("first process error = %v", err)
	}

	options := transition.options
	options.NewTransactionID = func() TransactionID { return "tx-test-002" }
	restarted, err := NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	reinspection, err := restarted.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := restarted.Converge(context.Background(), Execution{
		Plan: reinspection.Plan, Authorization: v2TestAuthorization(reinspection.Plan),
	})
	if err != nil || outcome.Status != StatusReady || !outcome.ReachedGoal {
		t.Fatalf("restarted outcome = %#v, err=%v", outcome, err)
	}
	current, err := restarted.store.ReadCurrentJournal()
	if err != nil || !current.Exists {
		t.Fatalf("current journal = %#v, err=%v", current, err)
	}
	journal, err := ParseJournal(current.Payload)
	if err != nil || journal.Transaction != "tx-test-002" {
		t.Fatalf("current transaction = %q, err=%v", journal.Transaction, err)
	}
}

func TestV2TransitionReobservesFailedActivationReconcilerResult(t *testing.T) {
	tests := []struct {
		name       string
		post       V2ActivationObservation
		wantStatus PublicStatus
		wantCode   OutcomeCode
	}{
		{
			name: "completed desired state remains retryable",
			post: V2ActivationObservation{
				Actual: digestA, Desired: digestA, Converged: true,
			},
			wantStatus: StatusRecovering,
			wantCode:   CodeDependencyUnavailable,
		},
		{
			name: "third state requires operator",
			post: V2ActivationObservation{
				Actual: digestC, Desired: digestA, Converged: false,
			},
			wantStatus: StatusOperatorActionRequired,
			wantCode:   CodeRecoveryAmbiguous,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reconciler := &v2PostErrorReconciler{post: test.post}
			transition, _, _ := v2TransitionFixture(t, nil)
			transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := transition.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			if err != nil || outcome.Status != test.wantStatus || outcome.Code != test.wantCode ||
				reconciler.postErrorObservations == 0 {
				t.Fatalf(
					"outcome=%#v err=%v post-error observations=%d",
					outcome, err, reconciler.postErrorObservations,
				)
			}
		})
	}
}

func TestFreshV2TransitionRejectsChangedObservationScope(t *testing.T) {
	tests := []struct {
		name               string
		initialReconcilers []V2ActivationReconciler
		freshReconcilers   []V2ActivationReconciler
		initialInputs      []string
		freshInputs        []string
	}{
		{
			name: "reduced reconciler set",
			initialReconcilers: []V2ActivationReconciler{
				v2NamedReconciler("runtime-a"), v2NamedReconciler("runtime-b"),
			},
			freshReconcilers: []V2ActivationReconciler{v2NamedReconciler("runtime-a")},
		},
		{
			name:          "changed scoped inputs",
			initialInputs: []string{"UNRELATED_SETTING"},
			freshInputs:   nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transition, configHome, _ := v2TransitionFixture(t, func(point string) error {
				if point == "after-journal-authorized" {
					return errors.New("stop after authorization")
				}
				return nil
			})
			transition.options.Reconcilers = test.initialReconcilers
			transition.options.InheritedSettingIDs = test.initialInputs
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := transition.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			}); err != nil {
				t.Fatal(err)
			}

			fresh, err := NewV2Transition(V2Options{
				ConfigHome: configHome,
				Releases:   ReleasePair{From: "release-a", Target: "release-a"},
				ObserveLinks: func(context.Context) (ReleaseLinks, error) {
					return ReleaseLinks{Active: "release-a"}, nil
				},
				Reconcilers:     test.freshReconcilers,
				RegistryPayload: v2RegistryPayload(t), ArtifactDigest: digestA,
				InheritedSettingIDs: test.freshInputs,
				OwnerRegistration:   &v2TestOwnerRegistration{state: OwnerRegistrationAbsent},
				VerifyAuthorization: func(PlanToken, Authorization) bool { return false },
			})
			if err != nil {
				t.Fatal(err)
			}
			resume, err := fresh.Inspect(context.Background(), goal)
			if err != nil || resume.Resume == nil {
				t.Fatalf("resume = %#v, err=%v", resume, err)
			}
			outcome, err := fresh.Converge(context.Background(), Execution{Plan: resume.Plan})
			if err != nil || outcome.Status != StatusOperatorActionRequired ||
				outcome.Code != CodePlanStale {
				t.Fatalf("changed-scope outcome = %#v, err=%v", outcome, err)
			}
		})
	}
}

func TestV2TransitionRejectsObservationScopeChangedAfterInspection(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	transition.options.InheritedSettingIDs = []string{"UNRELATED_SETTING"}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusOperatorActionRequired ||
		outcome.Code != CodePlanStale || outcome.Transaction != nil {
		t.Fatalf("changed pre-authorization scope outcome = %#v, err=%v", outcome, err)
	}
	journal, err := transition.store.ReadCurrentJournal()
	if err != nil || journal.Exists {
		t.Fatalf("changed scope published journal = %#v, err=%v", journal, err)
	}
}

func TestV2TransitionDoesNotFabricateLinksWhenPostMutationObservationFails(t *testing.T) {
	failObservation := false
	links := ReleaseLinks{Active: "release-a"}
	transition, _, _ := v2TransitionFixtureWithReleases(t, func(point string) error {
		if point == "after-target-active" {
			failObservation = true
			return errors.New("interrupted after target activation")
		}
		return nil
	}, ReleasePair{From: "release-a", Target: "release-b"}, links)
	transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) {
		if failObservation {
			return ReleaseLinks{}, errors.New("links unavailable")
		}
		return links, nil
	}
	transition.options.ActivateLinks = func(
		_ context.Context,
		pair ReleasePair,
	) (ReleaseLinks, error) {
		links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
		return links, nil
	}
	goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusOperatorActionRequired ||
		outcome.Code != CodeRecoveryAmbiguous || outcome.Active != "" ||
		outcome.Previous != nil || outcome.Target != "release-b" ||
		outcome.Transaction == nil || *outcome.Transaction != "tx-test-001" {
		t.Fatalf("unobserved-link outcome = %#v, err=%v", outcome, err)
	}
}

type v2TestReconciler struct {
	converged  bool
	observes   int
	reconciles int
}

type v2PostErrorReconciler struct {
	post                  V2ActivationObservation
	failed                bool
	postErrorObservations int
}

type v2NamedReconciler string

type v2ObservationErrorReconciler struct {
	id  string
	err error
}

type v2ActivationConflictError struct{}

func (v2ActivationConflictError) Error() string            { return "private topology detail" }
func (v2ActivationConflictError) ActivationConflict() bool { return true }

type v2TestOwnerRegistration struct {
	state           OwnerRegistrationState
	registration    Fingerprint
	commits         int
	cleanups        int
	cleanupErr      error
	intermediate    bool
	interruptCommit bool
}

func (owner *v2TestOwnerRegistration) observation() OwnerRegistrationObservation {
	return owner.observationFor(owner.state)
}

func (owner *v2TestOwnerRegistration) observationFor(
	state OwnerRegistrationState,
) OwnerRegistrationObservation {
	observation := OwnerRegistrationObservation{State: state}
	if state != OwnerRegistrationAbsent {
		observation.Registration = owner.registration
		if observation.Registration == "" {
			observation.Registration = digestB
		}
		observation.Overrides = digestB
		observation.Controller = digestB
		observation.SharedImages = true
	}
	return observation
}

func (owner *v2TestOwnerRegistration) Prepare(
	_ context.Context,
	_ V2SettingsSnapshotView,
) (OwnerRegistrationObservation, error) {
	if owner.intermediate {
		return OwnerRegistrationObservation{}, errors.New("owner registration has authorized intermediate state")
	}
	return owner.observation(), nil
}

func (owner *v2TestOwnerRegistration) Observe(
	_ context.Context,
	before OwnerRegistrationObservation,
) (OwnerRegistrationProgress, error) {
	actual := owner.observation()
	if actual.Registration != before.Registration || actual.Overrides != before.Overrides ||
		actual.Controller != before.Controller || actual.SharedImages != before.SharedImages {
		return "", errors.New("owner registration identity changed outside the transition")
	}
	if owner.intermediate && owner.state == before.State {
		return OwnerRegistrationInProgress, nil
	}
	if owner.state == before.State {
		return OwnerRegistrationExpected, nil
	}
	if owner.state == OwnerRegistrationCurrent {
		return OwnerRegistrationDesired, nil
	}
	return "", errors.New("owner registration changed outside the transition")
}

func (owner *v2TestOwnerRegistration) Commit(
	_ context.Context,
	before OwnerRegistrationObservation,
) error {
	actual := owner.observation()
	if before.State != OwnerRegistrationLegacyDirectory ||
		actual.Registration != before.Registration || actual.SharedImages != before.SharedImages ||
		(owner.state != before.State && owner.state != OwnerRegistrationCurrent) {
		return errors.New("owner registration changed before commit")
	}
	if before.TerminalCleanup {
		owner.cleanups++
		return owner.cleanupErr
	}
	owner.commits++
	if owner.state == OwnerRegistrationCurrent {
		return nil
	}
	if owner.interruptCommit {
		owner.interruptCommit = false
		owner.intermediate = true
		return errors.New("interrupted after an internal owner mutation")
	}
	owner.intermediate = false
	owner.state = OwnerRegistrationCurrent
	return nil
}

func TestV2TransitionRunsOwnerCleanupBeforeRecoveryGC(t *testing.T) {
	owner := &v2TestOwnerRegistration{state: OwnerRegistrationLegacyDirectory}
	transition, _, _ := v2TransitionFixture(t, nil)
	transition.options.OwnerRegistration = owner
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusReady || len(outcome.Warnings) != 0 ||
		owner.cleanups != 1 {
		t.Fatalf("owner cleanup outcome = %#v owner=%#v err=%v", outcome, owner, err)
	}
}

func TestV2TransitionRetainsRecoveryUntilOwnerCleanupSucceeds(t *testing.T) {
	cleanupErr := errors.New("owner archive cleanup unavailable")
	owner := &v2TestOwnerRegistration{
		state: OwnerRegistrationLegacyDirectory, cleanupErr: cleanupErr,
	}
	transition, _, _ := v2TransitionFixture(t, nil)
	transition.options.OwnerRegistration = owner
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusReady || len(outcome.Warnings) != 1 ||
		outcome.Warnings[0] != "recovery cleanup is pending" || owner.cleanups != 1 {
		t.Fatalf("failed owner cleanup outcome = %#v owner=%#v err=%v", outcome, owner, err)
	}
	owner.cleanupErr = nil
	resume, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err = transition.Converge(context.Background(), Execution{Plan: resume.Plan})
	if err != nil || outcome.Status != StatusReady || len(outcome.Warnings) != 0 ||
		owner.cleanups != 2 {
		t.Fatalf("resumed owner cleanup outcome = %#v owner=%#v err=%v", outcome, owner, err)
	}
}

type v2FailingReconciler struct {
	id           string
	reconcileErr error
}

type v2PhasedError struct {
	phase string
	err   error
}

func (failure v2PhasedError) Error() string { return failure.err.Error() }

func (failure v2PhasedError) ActivationPhase() string { return failure.phase }

func (reconciler *v2FailingReconciler) ID() string { return reconciler.id }

func (*v2FailingReconciler) Observe(context.Context, ReleasePair, ReleaseLinks) (V2ActivationObservation, error) {
	return V2ActivationObservation{Actual: digestB, Desired: digestA}, nil
}

func (reconciler *v2FailingReconciler) Reconcile(context.Context, ReleaseLinks) error {
	return reconciler.reconcileErr
}

func (*v2TestReconciler) ID() string { return "test-runtime" }

func (reconciler *v2PostErrorReconciler) ID() string { return "post-error-runtime" }

func (reconciler *v2PostErrorReconciler) Observe(
	context.Context,
	ReleasePair,
	ReleaseLinks,
) (V2ActivationObservation, error) {
	if reconciler.failed {
		reconciler.postErrorObservations++
		return reconciler.post, nil
	}
	return V2ActivationObservation{Actual: digestB, Desired: digestA, Converged: false}, nil
}

func (reconciler *v2PostErrorReconciler) Reconcile(context.Context, ReleaseLinks) error {
	reconciler.failed = true
	return errors.New("private reconciler failure")
}

func (reconciler v2NamedReconciler) ID() string { return string(reconciler) }

func (reconciler v2ObservationErrorReconciler) ID() string { return reconciler.id }

func (reconciler v2ObservationErrorReconciler) Observe(
	context.Context,
	ReleasePair,
	ReleaseLinks,
) (V2ActivationObservation, error) {
	return V2ActivationObservation{}, reconciler.err
}

func (v2ObservationErrorReconciler) Reconcile(context.Context, ReleaseLinks) error { return nil }

func (v2NamedReconciler) Observe(
	context.Context,
	ReleasePair,
	ReleaseLinks,
) (V2ActivationObservation, error) {
	return V2ActivationObservation{Actual: digestA, Desired: digestA, Converged: true}, nil
}

func (v2NamedReconciler) Reconcile(context.Context, ReleaseLinks) error { return nil }

func (reconciler *v2TestReconciler) Observe(context.Context, ReleasePair, ReleaseLinks) (V2ActivationObservation, error) {
	reconciler.observes++
	return V2ActivationObservation{Actual: map[bool]Fingerprint{true: digestA, false: digestB}[reconciler.converged], Desired: digestA, Converged: reconciler.converged}, nil
}

func (reconciler *v2TestReconciler) Reconcile(context.Context, ReleaseLinks) error {
	reconciler.reconciles++
	reconciler.converged = true
	return nil
}

func v2TransitionFixture(
	t *testing.T,
	fault func(string) error,
) (*V2Transition, string, string) {
	return v2TransitionFixtureWithReleases(t, fault, ReleasePair{
		From: "release-a", Target: "release-a",
	}, ReleaseLinks{Active: "release-a"})
}

func v2TransitionFixtureWithReleases(
	t *testing.T,
	fault func(string) error,
	releases ReleasePair,
	links ReleaseLinks,
) (*V2Transition, string, string) {
	t.Helper()
	configHome := settingsV2Fixture(t, map[string]string{
		"yards/hermes/config.env": "YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\nSSH_PORT=2224\n",
	})
	transition, err := NewV2Transition(V2Options{
		ConfigHome: configHome, Releases: releases,
		ObserveLinks:    func(context.Context) (ReleaseLinks, error) { return links, nil },
		RegistryPayload: v2RegistryPayload(t), ArtifactDigest: digestA,
		OwnerRegistration: &v2TestOwnerRegistration{state: OwnerRegistrationAbsent},
		NewTransactionID:  func() TransactionID { return "tx-test-001" },
		VerifyAuthorization: func(plan PlanToken, authorization Authorization) bool {
			return authorization == v2TestAuthorization(plan)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	transition.options.fault = fault
	return transition, configHome, filepath.Join(configHome, "yards", "hermes", "config.env")
}

func v2TestAuthorization(plan PlanToken) Authorization {
	return Authorization("test-grant." + string(plan))
}

func v2RegistryPayload(t *testing.T) []byte {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func v2TestRegistry(t *testing.T) RegistryV2 {
	t.Helper()
	registry, _, err := ParseRegistryV2(v2RegistryPayload(t), BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	return registry
}
