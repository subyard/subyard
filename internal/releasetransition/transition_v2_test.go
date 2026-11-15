package releasetransition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

const (
	v2PostActivationPreviousRelease  ReleaseID = "0.9.1-a1b2c3d4e5f6"
	v2PostActivationSourceRelease    ReleaseID = "0.11.1-b2c3d4e5f6a7"
	v2PostActivationCandidateRelease ReleaseID = "0.11.2-c3d4e5f6a7b8"
)

func TestV2RollbackCompatibilityBlocksBeforeAssessmentAuthorizationOrMutation(t *testing.T) {
	incompatibleRegistry := []byte(`{
  "schemaVersion": 2,
  "minimumEpochs": {"owner-registration": 1, "power-metadata": 1, "project-state": 1, "settings": 1},
  "currentEpochs": {"owner-registration": 1, "power-metadata": 1, "project-state": 1, "settings": 1},
  "migrations": []
}
`)
	for _, test := range []struct {
		name            string
		version         string
		registryPayload []byte
		registryDigest  Fingerprint
		removePolicy    string
		ledgerState     string
		privateVersion  string
	}{
		{name: "V2 provenance cannot bypass source floor", version: "0.10.0", registryPayload: v2RegistryPayload(t), registryDigest: fingerprintPayload(v2RegistryPayload(t))},
		{name: "legacy target is older than compiled floor", version: "0.7.9", privateVersion: "0.7.9"},
		{name: "applied capability has no compiled policy", version: "0.8.0", removePolicy: "test-vms-settings-v1-to-v2"},
		{name: "absent ledger target below floor", version: "0.7.9", ledgerState: "absent"},
		{name: "baseline ledger target below floor", version: "0.7.9", ledgerState: "baseline"},
		{name: "partial ledger target below floor", version: "0.7.9", ledgerState: "partial"},
		{name: "absent ledger pending capability has no policy", version: "0.8.0", ledgerState: "absent", removePolicy: "test-yard-owner-v1-to-v2"},
		{name: "baseline ledger pending capability has no policy", version: "0.8.0", ledgerState: "baseline", removePolicy: "test-yard-owner-v1-to-v2"},
		{name: "partial ledger pending capability has no policy", version: "0.8.0", ledgerState: "partial", removePolicy: "test-yard-owner-v1-to-v2"},
		{
			name: "v2 target registry rejects current ledger prefix", version: "0.9.0",
			registryPayload: incompatibleRegistry, registryDigest: fingerprintPayload(incompatibleRegistry),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newV2RollbackBoundaryFixtureWithLedger(
				t, test.version, test.registryDigest, test.registryPayload, test.ledgerState,
			)
			if test.removePolicy != "" {
				descriptors := slices.Clone(BuiltinCapabilityCatalog().descriptors)
				for index := range descriptors {
					if descriptors[index].Kind == test.removePolicy {
						descriptors[index].RollbackCompatibility = ""
						descriptors[index].LegacyMinimumVersion = ""
					}
				}
				catalog, err := NewCapabilityCatalog(descriptors)
				if err != nil {
					t.Fatal(err)
				}
				fixture.transition.catalog = catalog
			}
			goal := Goal{Target: "release-a", Direction: DirectionActivatePrevious}
			inspection, err := fixture.transition.Inspect(context.Background(), goal)
			if err != nil || inspection.Outcome == nil ||
				inspection.Outcome.Status != StatusOperatorActionRequired ||
				inspection.Outcome.Code != CodeRollbackIncompatible ||
				len(inspection.Blockers) != 1 || inspection.Assessment.Changed ||
				inspection.Assessment.Recovery != domain.RecoveryNotNeeded ||
				inspection.Assessment.Effect != domain.ActionRead {
				t.Fatalf("blocked rollback inspection = %#v, err=%v", inspection, err)
			}
			payload, err := json.Marshal(inspection)
			if err != nil {
				t.Fatal(err)
			}
			if test.privateVersion != "" && strings.Contains(string(payload), test.privateVersion) {
				t.Fatalf("public rollback diagnostic leaked target version: %s", payload)
			}
			outcome, err := fixture.transition.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: "must-not-be-checked",
			})
			if err != nil || outcome.Status != StatusOperatorActionRequired ||
				outcome.Code != CodeRollbackIncompatible {
				t.Fatalf("blocked rollback convergence = %#v, err=%v", outcome, err)
			}
			fixture.assertUnchanged(t, true)
		})
	}
}

func TestV2RollbackCompatibilityAllowsPublishedLegacyFloorAndBindsStaleTarget(t *testing.T) {
	fixture := newV2RollbackBoundaryFixture(t, "0.11.0", "", nil)
	goal := Goal{Target: "release-a", Direction: DirectionActivatePrevious}
	inspection, err := fixture.transition.Inspect(context.Background(), goal)
	if err != nil || inspection.Outcome == nil ||
		inspection.Outcome.Status != StatusMigrationRequired ||
		inspection.Assessment.Recovery != domain.RecoveryReversible ||
		!slices.Contains(inspection.Assessment.Consequences, v2RollbackNoOpConsequence) {
		t.Fatalf("allowed legacy rollback inspection = %#v, err=%v", inspection, err)
	}
	fixture.transition.options.RollbackTarget.Version = "0.11.1"
	outcome, err := fixture.transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: "must-not-be-checked",
	})
	if err != nil || outcome.Status != StatusOperatorActionRequired ||
		outcome.Code != CodePlanStale {
		t.Fatalf("stale exact rollback target = %#v, err=%v", outcome, err)
	}
	fixture.assertUnchanged(t, false)
}

type v2RollbackBoundaryFixture struct {
	transition  *V2Transition
	configHome  string
	links       *ReleaseLinks
	ledger      ProtectedSnapshot
	reconciler  *v2TestReconciler
	owner       *v2TestOwnerRegistration
	activations *int
	authorizes  *int
}

func newV2RollbackBoundaryFixture(
	t *testing.T,
	version string,
	targetRegistryDigest Fingerprint,
	registryPayload []byte,
) v2RollbackBoundaryFixture {
	t.Helper()
	return newV2RollbackBoundaryFixtureWithLedger(t, version, targetRegistryDigest, registryPayload, "")
}

func newV2RollbackBoundaryFixtureWithLedger(
	t *testing.T,
	version string,
	targetRegistryDigest Fingerprint,
	registryPayload []byte,
	ledgerState string,
) v2RollbackBoundaryFixture {
	t.Helper()
	configHome := settingsV2Fixture(t, map[string]string{
		"yards/hermes/config.env": "YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\n",
	})
	switch ledgerState {
	case "absent":
	case "baseline":
		seedV2LedgerPrefix(t, configHome, 0)
	case "partial":
		seedV2LedgerPrefix(t, configHome, 1)
	case "":
		seedAppliedV2Ledger(t, configHome)
	default:
		t.Fatalf("unknown ledger fixture state %q", ledgerState)
	}
	if registryPayload == nil {
		registryPayload = v2RegistryPayload(t)
	}
	links := &ReleaseLinks{Active: "release-b", Previous: releaseIDPointer("release-a")}
	reconciler := &v2TestReconciler{}
	owner := &v2TestOwnerRegistration{state: OwnerRegistrationAbsent}
	activations, authorizes := 0, 0
	transition, err := NewV2Transition(V2Options{
		ConfigHome: configHome,
		Releases: ReleasePair{
			From: "release-b", Previous: releaseIDPointer("release-a"), Target: "release-a",
		},
		Direction: DirectionActivatePrevious,
		RollbackTarget: &RollbackTarget{
			Version: version, RegistryDigest: targetRegistryDigest,
		},
		ObserveLinks: func(context.Context) (ReleaseLinks, error) { return *links, nil },
		ActivateLinks: func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
			activations++
			*links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
			return *links, nil
		},
		Reconcilers: []V2ActivationReconciler{reconciler}, OwnerRegistration: owner,
		RegistryPayload: registryPayload, ArtifactDigest: digestA,
		NewTransactionID: func() TransactionID { return "tx-rollback-001" },
		VerifyAuthorization: func(PlanToken, Authorization) bool {
			authorizes++
			return true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := transition.store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	return v2RollbackBoundaryFixture{
		transition: transition, configHome: configHome, links: links, ledger: ledger,
		reconciler: reconciler, owner: owner, activations: &activations, authorizes: &authorizes,
	}
}

func seedAppliedV2Ledger(t *testing.T, configHome string) {
	t.Helper()
	seedV2LedgerPrefix(t, configHome, len(v2TestRegistry(t).Migrations))
}

func seedV2LedgerPrefix(t *testing.T, configHome string, count int) {
	t.Helper()
	registry := v2TestRegistry(t)
	ledger := BaselineLedgerV2(registry)
	var err error
	for _, migration := range registry.Migrations[:count] {
		ledger, err = ledger.Advance(registry, migration)
		if err != nil {
			t.Fatal(err)
		}
	}
	payload, _, err := MarshalLedgerV2(ledger, registry)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapLedger(before, payload); err != nil {
		t.Fatal(err)
	}
}

func (fixture v2RollbackBoundaryFixture) assertUnchanged(t *testing.T, requireNoObservation bool) {
	t.Helper()
	if *fixture.activations != 0 || *fixture.authorizes != 0 ||
		(requireNoObservation && fixture.reconciler.observes != 0) || fixture.reconciler.reconciles != 0 ||
		fixture.owner.prepares != 0 || fixture.owner.observes != 0 || fixture.owner.commits != 0 ||
		!linksEqual(*fixture.links, ReleaseLinks{Active: "release-b", Previous: releaseIDPointer("release-a")}) {
		t.Fatalf("rollback crossed proof boundary: activations=%d authorizes=%d links=%#v reconciler=%#v owner=%#v",
			*fixture.activations, *fixture.authorizes, *fixture.links, fixture.reconciler, fixture.owner)
	}
	ledger, err := fixture.transition.store.ReadLedger()
	if err != nil || ledger.Exists != fixture.ledger.Exists || !bytes.Equal(ledger.Payload, fixture.ledger.Payload) {
		t.Fatalf("rollback changed ledger: before=%q after=%q err=%v", fixture.ledger.Payload, ledger.Payload, err)
	}
	journal, err := fixture.transition.store.ReadCurrentJournal()
	if err != nil || journal.Exists {
		t.Fatalf("rollback published journal: %#v, err=%v", journal, err)
	}
	settings, err := os.ReadFile(filepath.Join(fixture.configHome, "yards", "hermes", "config.env"))
	if err != nil || string(settings) != "YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\n" {
		t.Fatalf("rollback changed settings: %q, err=%v", settings, err)
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
	source.catalog = publishedV0111CatalogFixture(t)
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

func publishedV0111CatalogFixture(t *testing.T) CapabilityCatalog {
	t.Helper()
	// Frozen from the published v0.11.1 descriptors, before rollback metadata.
	catalog, err := NewCapabilityCatalog([]CapabilityDescriptor{
		{Kind: "test-vms-settings-v1-to-v2", Domain: "settings", Version: 1},
		{Kind: "test-yard-owner-v1-to-v2", Domain: "owner-registration", Version: 1},
	})
	if err != nil || catalog.Digest() != "49e4c86efea0f1be557a43569728d9eb7f9df519379b0bdf45edd91ac5c93cf2" {
		t.Fatalf("published v0.11.1 catalog = %q, err=%v", catalog.Digest(), err)
	}
	return catalog
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

func TestV2TransitionResumeAfterAnotherCallerCompletes(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(fmt.Sprintf("drift=%t", drift), func(t *testing.T) {
			ctx := context.Background()
			links := ReleaseLinks{Active: "release-a"}
			transition, _, _ := v2TransitionFixtureWithReleases(t, func(point string) error {
				if point == "after-target-active" {
					return errors.New("interrupted before activation reconciliation")
				}
				return nil
			}, ReleasePair{From: "release-a", Target: "release-b"}, links)
			transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return links, nil }
			transition.options.ActivateLinks = func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
				links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
				return links, nil
			}
			reconciler := &v2TestReconciler{warnings: []string{"runtime warning"}}
			transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
			goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
			initial, err := transition.Inspect(ctx, goal)
			if err != nil {
				t.Fatal(err)
			}
			interrupted, err := transition.Converge(ctx, Execution{
				Plan: initial.Plan, Authorization: v2TestAuthorization(initial.Plan),
			})
			if err != nil || interrupted.Status != StatusRecovering {
				t.Fatalf("interrupted outcome = %#v, err=%v", interrupted, err)
			}
			resume, err := transition.InspectProcessV1(ctx, goal)
			if err != nil || resume.Resume == nil {
				t.Fatalf("resume inspection = %#v, err=%v", resume, err)
			}
			transition.options.fault = nil
			completed, err := transition.Converge(ctx, Execution{Plan: resume.Plan})
			if err != nil || completed.Status != StatusReady {
				t.Fatalf("other caller outcome = %#v, err=%v", completed, err)
			}
			before, err := transition.store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			reconciler.converged = !drift
			fresh, err := NewV2Transition(transition.options)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := fresh.Converge(ctx, Execution{Plan: resume.Plan})
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateProcessConvergence(goal, resume, outcome); err != nil {
				t.Fatalf("invalid resumed outcome = %#v: %v", outcome, err)
			}
			want := CodeReady
			if drift {
				want = CodePlanStale
			}
			if outcome.Code != want || reconciler.reconciles != 1 {
				t.Fatalf("outcome = %#v, reconciles=%d", outcome, reconciler.reconciles)
			}
			if !slices.Equal(outcome.Warnings, []string{"runtime warning"}) {
				t.Fatalf("completed resume lost current warnings: %v", outcome.Warnings)
			}
			after, err := fresh.store.ReadCurrentJournal()
			if err != nil || !sameProtectedSnapshot(before, after) {
				t.Fatalf("completed journal changed: %v", err)
			}
		})
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
			originalSettings, _ := os.ReadFile(settingsPath)
			originalLedger, _ := transition.store.ReadLedger()
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
			if err != nil || !bytes.Equal(content, originalSettings) {
				t.Fatalf("settings mutated: %q, err=%v", content, err)
			}
			store, _ := NewPOSIXV2Store(configHome)
			ledger, ledgerErr := store.ReadLedger()
			journal, journalErr := store.ReadCurrentJournal()
			if ledgerErr != nil || journalErr != nil || !sameProtectedSnapshot(ledger, originalLedger) || journal.Exists {
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
	for _, point := range []string{"after-journal-authorized", "after-target-active"} {
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
		{name: "authorized foreign links", fault: "after-journal-authorized", unexpected: ReleaseLinks{Active: "foreign", Previous: releaseIDPointer("release-old")}},
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

func TestV2TransitionCompletedHistoryNormalizesStaleCallerPairBeforeDriftRepair(t *testing.T) {
	links := ReleaseLinks{Active: "release-a"}
	transition, _, _ := v2TransitionFixtureWithReleases(
		t, nil, ReleasePair{From: "release-a", Target: "release-b"}, links,
	)
	reconciler := &v2TestReconciler{}
	transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
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
	completed, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || completed.Status != StatusReady {
		t.Fatalf("initial convergence = %#v, err=%v", completed, err)
	}
	ledgerBefore, err := transition.store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	options := transition.options
	options.Releases = ReleasePair{From: "historical-release", Target: goal.Target}
	options.NewTransactionID = func() TransactionID { return "tx-test-002" }
	options.ActivateLinks = func(context.Context, ReleasePair) (ReleaseLinks, error) {
		t.Fatal("activation-only repair must not switch release links")
		return ReleaseLinks{}, nil
	}
	fresh, err := NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := fresh.Inspect(context.Background(), goal)
	if err != nil || ready.Outcome == nil || ready.Outcome.Status != StatusReady ||
		!ready.Outcome.ReachedGoal || ready.Assessment.Changed || ready.Resume != nil {
		t.Fatalf("completed history inspection = %#v, err=%v", ready, err)
	}
	links.Previous = releaseIDPointer("foreign-release")
	blocked, err := fresh.Inspect(context.Background(), goal)
	if err != nil || blocked.Outcome == nil ||
		blocked.Outcome.Status != StatusOperatorActionRequired ||
		blocked.Outcome.Code != CodeActivationAmbiguous {
		t.Fatalf("foreign previous link inspection = %#v, err=%v", blocked, err)
	}
	links.Previous = releaseIDPointer("release-a")
	reconciler.converged = false
	repair, err := fresh.Inspect(context.Background(), goal)
	if err != nil || repair.Outcome == nil || repair.Outcome.Status != StatusMigrationRequired ||
		!repair.Assessment.Changed || repair.Resume != nil || repair.Outcome.Transaction != nil {
		t.Fatalf("drift inspection = %#v, err=%v", repair, err)
	}
	outcome, err := fresh.Converge(context.Background(), Execution{
		Plan: repair.Plan, Authorization: v2TestAuthorization(repair.Plan),
	})
	if err != nil || outcome.Status != StatusReady || outcome.Transaction == nil ||
		*outcome.Transaction != "tx-test-002" {
		t.Fatalf("drift repair = %#v, err=%v", outcome, err)
	}
	ledgerAfter, err := fresh.store.ReadLedger()
	if err != nil || ledgerAfter.Fingerprint != ledgerBefore.Fingerprint {
		t.Fatalf("activation-only repair changed migration ledger: %v", err)
	}
	snapshot, err := fresh.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil || len(journal.Steps) != 0 || journal.Releases.From != goal.Target ||
		journal.Releases.Target != goal.Target || journal.Releases.Previous == nil ||
		*journal.Releases.Previous != "release-a" {
		t.Fatalf("activation-only journal = %#v, err=%v", journal, err)
	}
}

func TestV2TransitionRepairsActivationDriftAfterCompletedMigration(t *testing.T) {
	links := ReleaseLinks{Active: "release-a"}
	reconciler := &v2TestReconciler{}
	transition, configHome, settingsPath := v2TransitionFixtureWithReleases(
		t, nil, ReleasePair{From: "release-a", Target: "release-a"}, links,
	)
	transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
	activateCalls := 0
	transition.options.ActivateLinks = func(context.Context, ReleasePair) (ReleaseLinks, error) {
		activateCalls++
		return ReleaseLinks{}, errors.New("same-release activation must not switch links")
	}
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
	settingsBefore, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	owner := transition.options.OwnerRegistration.(*v2TestOwnerRegistration)
	ownerCalls := [3]int{owner.prepares, owner.observes, owner.commits}
	transition.options.NewTransactionID = func() TransactionID { return "tx-test-002" }
	reconciler.converged = false
	repeat, err := transition.Inspect(context.Background(), goal)
	if err != nil || repeat.Resume != nil || repeat.Outcome == nil ||
		repeat.Outcome.Status != StatusMigrationRequired || !repeat.Assessment.Changed ||
		repeat.Outcome.Transaction != nil {
		t.Fatalf("drift inspection = %#v, err=%v", repeat, err)
	}
	firstDriftPlan := repeat.Plan
	if !strings.Contains(repeat.Outcome.Message, "completed migrations remain applied") ||
		!strings.Contains(repeat.Outcome.Message, "activation.test-runtime") {
		t.Fatalf("repair diagnosis lost completed history or affected resource: %q", repeat.Outcome.Message)
	}
	if !slices.Contains(repeat.Decisions, RedactedDecision{
		Resource: "activation.test-runtime", Scope: "activation",
		Decision: DecisionCanonicalize, Result: "converged",
	}) {
		t.Fatalf("drift inspection does not identify pending runtime work: %#v", repeat.Decisions)
	}
	reconciler.drift = digestC
	repeat, err = transition.Inspect(context.Background(), goal)
	if err != nil || repeat.Plan == firstDriftPlan {
		t.Fatalf("activation observation is not bound into plan: first=%q repeat=%#v err=%v",
			firstDriftPlan, repeat, err)
	}
	reconciles := reconciler.reconciles
	outcome, err := transition.Converge(context.Background(), Execution{Plan: repeat.Plan})
	if err != nil || outcome.Status != StatusOperatorActionRequired ||
		outcome.Code != CodeConfirmationRequired || reconciler.reconciles != reconciles {
		t.Fatalf("unauthorized repair = %#v reconciler=%#v err=%v", outcome, reconciler, err)
	}
	outcome, err = transition.Converge(context.Background(), Execution{
		Plan: repeat.Plan, Authorization: v2TestAuthorization(repeat.Plan),
	})
	if err != nil || outcome.Status != StatusReady || !reconciler.converged ||
		reconciler.reconciles != reconciles+1 || outcome.Transaction == nil ||
		*outcome.Transaction != "tx-test-002" || activateCalls != 0 {
		t.Fatalf("drift outcome = %#v reconciler=%#v err=%v", outcome, reconciler, err)
	}
	after, err := store.ReadLedger()
	if err != nil || !bytes.Equal(before.Payload, after.Payload) {
		t.Fatalf("ledger changed during reconcile: before=%#v after=%#v err=%v", before, after, err)
	}
	settingsAfter, err := os.ReadFile(settingsPath)
	if err != nil || !bytes.Equal(settingsBefore, settingsAfter) ||
		ownerCalls != [3]int{owner.prepares, owner.observes, owner.commits} {
		t.Fatalf("migration capability ran during repair: settings before=%q after=%q owner=%#v err=%v",
			settingsBefore, settingsAfter, owner, err)
	}
	journalSnapshot, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(journalSnapshot.Payload)
	if err != nil || journal.Transaction != "tx-test-002" ||
		journal.Checkpoint != JournalComplete || len(journal.Steps) != 0 {
		t.Fatalf("activation-only journal = %#v, err=%v", journal, err)
	}
}

func TestV2TransitionResumesDurableActivationOnlyRepairAtEveryCheckpoint(t *testing.T) {
	for _, faultPoint := range []string{
		"after-journal-authorized",
		"before-reconciler-0", "after-reconciler-0",
		"before-reconciler-1", "after-reconciler-1",
		"before-journal-complete",
	} {
		t.Run(faultPoint, func(t *testing.T) {
			injected := errors.New("injected activation-only repair fault")
			activeFault := ""
			transition, configHome, settingsPath := v2TransitionFixture(t, func(point string) error {
				if point == activeFault {
					return injected
				}
				return nil
			})
			reconcilers := []*v2TestReconciler{{id: "test-runtime-0"}, {id: "test-runtime-1"}}
			transition.options.Reconcilers = []V2ActivationReconciler{reconcilers[0], reconcilers[1]}
			activateCalls := 0
			transition.options.ActivateLinks = func(context.Context, ReleasePair) (ReleaseLinks, error) {
				activateCalls++
				return ReleaseLinks{}, errors.New("same-release activation must not switch links")
			}
			authorizations := 0
			transition.options.VerifyAuthorization = func(plan PlanToken, authorization Authorization) bool {
				authorizations++
				return authorization == v2TestAuthorization(plan)
			}
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			initial, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			completed, err := transition.Converge(context.Background(), Execution{
				Plan: initial.Plan, Authorization: v2TestAuthorization(initial.Plan),
			})
			if err != nil || completed.Status != StatusReady {
				t.Fatalf("initial convergence = %#v, err=%v", completed, err)
			}
			// Completed migration history must not reopen for later settings drift.
			testkit.WriteFile(t, settingsPath, []byte("YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\n"), 0o600)
			store, err := NewPOSIXV2Store(configHome)
			if err != nil {
				t.Fatal(err)
			}
			ledgerBefore, err := store.ReadLedger()
			if err != nil {
				t.Fatal(err)
			}
			settingsBefore, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			owner := transition.options.OwnerRegistration.(*v2TestOwnerRegistration)
			ownerCalls := [3]int{owner.prepares, owner.observes, owner.commits}
			unrelatedSettings := map[string][]byte{
				filepath.Join(configHome, "yards", "unrelated.env"): []byte("YARD_TEMPLATE=synthetic-other\n"),
				filepath.Join(configHome, "yards", "empty.env"):     []byte("YARD_TEMPLATE=\n"),
			}
			for path, payload := range unrelatedSettings {
				testkit.WriteFile(t, path, payload, 0o600)
			}
			transition.options.NewTransactionID = func() TransactionID { return "tx-test-002" }
			for _, reconciler := range reconcilers {
				reconciler.converged = false
			}
			repair, err := transition.Inspect(context.Background(), goal)
			if err != nil || repair.Resume != nil || !repair.Assessment.Changed {
				t.Fatalf("repair inspection = %#v, err=%v", repair, err)
			}
			activeFault = faultPoint
			interrupted, err := transition.Converge(context.Background(), Execution{
				Plan: repair.Plan, Authorization: v2TestAuthorization(repair.Plan),
			})
			if err != nil || interrupted.Status != StatusRecovering ||
				interrupted.Transaction == nil || *interrupted.Transaction != "tx-test-002" {
				t.Fatalf("interrupted repair = %#v, err=%v", interrupted, err)
			}
			journalSnapshot, err := store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			journal, err := ParseJournal(journalSnapshot.Payload)
			if err != nil || journal.Transaction != "tx-test-002" || len(journal.Steps) != 0 {
				t.Fatalf("durable repair journal = %#v, err=%v", journal, err)
			}
			resumePlan := journal.ResumePlan
			activeFault = ""
			resume, err := transition.Inspect(context.Background(), goal)
			if err != nil || resume.Resume == nil || *resume.Resume != "tx-test-002" ||
				resume.Plan != resumePlan || resume.Outcome == nil ||
				resume.Outcome.Status != StatusRecovering {
				t.Fatalf("repair resume inspection = %#v, err=%v", resume, err)
			}
			settled, err := transition.Converge(context.Background(), Execution{Plan: resume.Plan})
			if err != nil || settled.Status != StatusReady || settled.Transaction == nil ||
				*settled.Transaction != "tx-test-002" || authorizations != 2 || activateCalls != 0 {
				t.Fatalf("resumed repair = %#v authorizations=%d activate=%d err=%v",
					settled, authorizations, activateCalls, err)
			}
			ledgerAfter, err := store.ReadLedger()
			settingsAfter, settingsErr := os.ReadFile(settingsPath)
			if err != nil || settingsErr != nil || !bytes.Equal(ledgerBefore.Payload, ledgerAfter.Payload) ||
				!bytes.Equal(settingsBefore, settingsAfter) ||
				ownerCalls != [3]int{owner.prepares, owner.observes, owner.commits} {
				t.Fatalf("repair changed migration state: ledger before=%q after=%q settings before=%q after=%q owner=%#v err=%v settingsErr=%v",
					ledgerBefore.Payload, ledgerAfter.Payload, settingsBefore, settingsAfter, owner, err, settingsErr)
			}
			for path, payload := range unrelatedSettings {
				preserved, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(payload, preserved) {
					t.Errorf("activation-only repair changed unrelated settings %s: %v", path, err)
				}
			}
		})
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

func TestV2TransitionForwardAfterPublishedRollbackHistoryRequiresFreshAuthorization(t *testing.T) {
	links := ReleaseLinks{Active: "release-a"}
	completed, _, _ := v2TransitionFixtureWithReleases(t, nil,
		ReleasePair{From: "release-a", Target: "release-a"}, links)
	initial, err := completed.Inspect(context.Background(), Goal{Target: "release-a", Direction: DirectionActivateTarget})
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := completed.Converge(context.Background(), Execution{
		Plan: initial.Plan, Authorization: v2TestAuthorization(initial.Plan),
	}); err != nil || outcome.Status != StatusReady {
		t.Fatalf("initial migration = %#v, %v", outcome, err)
	}
	snapshot, err := completed.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	history, err := ParseJournal(snapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	history.Goal.Direction = DirectionActivatePrevious
	history.Releases.From = "release-old"
	history.Releases.Previous = releaseIDPointer("release-a")
	history.CatalogDigest = "49e4c86efea0f1be557a43569728d9eb7f9df519379b0bdf45edd91ac5c93cf2"
	history.Steps = []JournalStep{}
	history.IntentDigest = bindJournalIntent(history.AuthorizationPlan, history.ResumePlan, history.ObservationScope, history.Steps)
	// The published writer omitted rollbackTarget; do not synthesize old proof.
	payload, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	if err := completed.store.CompareAndSwapCurrentJournal(snapshot, payload); err != nil {
		t.Fatal(err)
	}
	ledgerBefore, err := completed.store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	links.Previous = releaseIDPointer("release-old")
	options := completed.options
	options.Releases = ReleasePair{From: "release-a", Previous: links.Previous, Target: "release-b"}
	options.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return links, nil }
	options.ActivateLinks = func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
		links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
		return links, nil
	}
	options.NewTransactionID = func() TransactionID { return "tx-new-forward" }
	next, err := NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := next.Inspect(context.Background(), Goal{Target: "release-b", Direction: DirectionActivateTarget})
	if err != nil || inspection.Outcome == nil || inspection.Outcome.Status != StatusMigrationRequired || inspection.Resume != nil {
		t.Fatalf("forward inspection = %#v, %v", inspection, err)
	}
	for _, execution := range []Execution{
		{Plan: history.ResumePlan},
		{Plan: history.AuthorizationPlan, Authorization: v2TestAuthorization(history.AuthorizationPlan)},
		{Plan: inspection.Plan, Authorization: v2TestAuthorization(history.AuthorizationPlan)},
	} {
		if outcome, err := next.Converge(context.Background(), execution); err == nil && outcome.Status == StatusReady {
			t.Fatal("historical authorization executed the new forward action")
		}
	}
	unchanged, err := next.store.ReadCurrentJournal()
	if err != nil || !bytes.Equal(unchanged.Payload, payload) || links.Active != "release-a" {
		t.Fatalf("inspection/rejected grants changed historical state: %v", err)
	}
	outcome, err := next.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusReady || outcome.Active != "release-b" ||
		outcome.Transaction == nil || *outcome.Transaction != "tx-new-forward" {
		t.Fatalf("freshly authorized forward action = %#v, %v", outcome, err)
	}
	ledgerAfter, err := next.store.ReadLedger()
	if err != nil || !bytes.Equal(ledgerAfter.Payload, ledgerBefore.Payload) {
		t.Fatalf("forward action replayed the completed migration ledger: %v", err)
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

func TestV2ActivationConsequencesReachProcessInspectionAndBindConsent(t *testing.T) {
	for _, change := range []string{"none", "consequence", "snapshot", "native binding"} {
		t.Run(change, func(t *testing.T) {
			transition, _, configPath := v2TransitionFixture(t, nil)
			reconciler := &v2TestReconciler{consequences: []string{
				"Adopt existing Codex rules under integration management: /home/dev/.codex/rules/repo.rules",
			}}
			if change == "native binding" {
				reconciler.binding = digestA
			}
			transition.options.MigrationCheckpoint = false
			transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			inspection, err := transition.InspectProcessV1(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			want := append([]string{v2ChangedConsequence}, reconciler.consequences...)
			if !slices.Equal(inspection.Assessment.Consequences, want) {
				t.Fatalf("missing activation consequences: %#v", inspection.Assessment.Consequences)
			}
			encoded, err := json.Marshal(ProcessResponse{SchemaVersion: 1, ActivationReconciliationOwned: true, Inspection: &inspection})
			if err != nil {
				t.Fatal(err)
			}
			var decoded ProcessResponse
			if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Inspection == nil || !slices.Equal(decoded.Inspection.Assessment.Consequences, want) {
				t.Fatalf("existing process protocol lost consequences: %v", err)
			}
			before, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "consequence":
				reconciler.consequences[0] = "Adopt a different existing artifact"
			case "snapshot":
				reconciler.drift = digestC
			case "native binding":
				reconciler.binding = digestC
			}
			outcome, err := transition.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			if change == "none" {
				if err != nil || outcome.Status != StatusReady || reconciler.reconciles != 1 {
					t.Fatalf("confirmed adoption failed: %#v err=%v", outcome, err)
				}
				repeated, err := transition.InspectProcessV1(context.Background(), goal)
				if err != nil || repeated.Assessment.Changed || len(repeated.Assessment.Consequences) != 0 {
					t.Fatalf("completed activation retained adoption consequences: %#v err=%v", repeated, err)
				}
				return
			}
			if err != nil || outcome.Code != CodePlanStale || reconciler.reconciles != 0 {
				t.Fatalf("changed adoption plan executed: %#v err=%v", outcome, err)
			}
			after, err := os.ReadFile(configPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("stale adoption plan changed configuration")
			}
			journal, err := transition.store.ReadCurrentJournal()
			if err != nil || journal.Exists {
				t.Fatalf("stale adoption plan published a journal: %v", err)
			}
		})
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

func TestV2TransitionPreservesExplicitPublicActivationDiagnostic(t *testing.T) {
	for _, test := range []struct {
		message, retry string
		valid          bool
	}{
		{"Known legacy file has unexpected contents; file preserved.", "incus exec yard --project subyard -- sha256sum /usr/local/libexec/subyard/projects-changed", true},
		{"Owned integration conflict; artifacts preserved.", "'/tmp/candidate with quotes/bin/yard-engine' '-Y' 'fixture' 'integration' 'cleanup' 'synthetic' '--check'", true},
		{"unsafe\nmessage", "yard update --check", false},
		{strings.Repeat("x", maxDiagnosticText+1), "yard update --check", false},
		{"known message", "inspect; mutate", false},
		{"invalid utf8\xff", "yard update --check", false},
		{"", "", false},
	} {
		transition, _, _ := v2TransitionFixture(t, nil)
		transition.options.Reconcilers = []V2ActivationReconciler{
			v2ObservationErrorReconciler{id: "materialized-config", err: fmt.Errorf("private wrapper: %w", v2PublicActivationError{test.message, test.retry})},
		}
		inspection, err := transition.Inspect(context.Background(), Goal{Target: "release-a", Direction: DirectionActivateTarget})
		if err != nil || inspection.Outcome == nil || len(inspection.Blockers) != 1 {
			t.Fatalf("inspection: %#v, %v", inspection, err)
		}
		outcome := inspection.Outcome
		if test.valid {
			if outcome.Message != test.message || outcome.Retry != test.retry ||
				inspection.Blockers[0].Message != test.message || inspection.Blockers[0].Retry != test.retry {
				t.Fatalf("public diagnostic lost: %#v", inspection)
			}
		} else if outcome.Message != `activation reconciler "materialized-config" cannot be observed safely` || outcome.Retry != "run yard update --check" {
			t.Fatalf("invalid diagnostic escaped: %#v", outcome)
		}
	}
}

type v2PublicActivationError struct{ message, retry string }

func (err v2PublicActivationError) Error() string { return "private observation detail" }
func (err v2PublicActivationError) ActivationDiagnostic() (string, string) {
	return err.message, err.retry
}

func TestPublicActivationDiagnosticCollectsIndependentFailures(t *testing.T) {
	cause := domain.CollectRefusals(
		fmt.Errorf("first: %w", v2PublicActivationError{"yard work: profile unavailable", "run yard -Y work config status"}),
		fmt.Errorf("second: %w", v2PublicActivationError{"yard default: profile unavailable", "run yard -Y default config status"}),
	)
	message, retry := publicActivationDiagnostic(cause, "fallback", "run yard update --check")
	if message != "yard default: profile unavailable; yard work: profile unavailable" || retry != "run yard -Y default config status, then run yard -Y work config status" || strings.Contains(message, "private observation") {
		t.Fatalf("diagnostic %q / %q", message, retry)
	}
}

func TestV2TransitionPreservesPublicReconcileDiagnostic(t *testing.T) {
	for _, test := range []struct {
		name           string
		cause          error
		message, retry string
	}{
		{"public", fmt.Errorf("private wrapper: %w", v2PublicActivationError{
			"yard demo: integration reconciliation failed", "run yard -Y demo integration status",
		}), "yard demo: integration reconciliation failed", "run yard -Y demo integration status"},
		{"raw", errors.New("private failure"), "", ""},
		{"empty", v2PublicActivationError{}, "", ""},
		{"multiline", v2PublicActivationError{"unsafe\nmessage", "run yard update"}, "", ""},
		{"oversized", v2PublicActivationError{strings.Repeat("x", maxDiagnosticText+1), "run yard update"}, "", ""},
		{"unsafe action", v2PublicActivationError{"known message", "inspect; mutate"}, "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			transition, _, _ := v2TransitionFixture(t, nil)
			transition.options.Reconcilers = []V2ActivationReconciler{
				&v2FailingReconciler{id: "materialized-config", reconcileErr: test.cause},
			}
			inspection, err := transition.Inspect(context.Background(), Goal{Target: "release-a", Direction: DirectionActivateTarget})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := transition.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			message, retry := test.message, test.retry
			if message == "" {
				message, retry = `activation reconciler "materialized-config" failed during reconcile`, "run yard update"
			}
			if err != nil || outcome.Status != StatusRecovering || outcome.Code != CodeDependencyUnavailable ||
				outcome.Message != message || outcome.Retry != retry || strings.Contains(outcome.Message, "private") {
				t.Fatalf("reconcile diagnostic = %#v, err=%v", outcome, err)
			}
		})
	}
}

func TestV2TransitionPreservesPublicCauseAcrossSecondaryActivationFailures(t *testing.T) {
	for _, branch := range []string{"reobserve", "release observation", "unknown state", "bounded diagnostic"} {
		for _, public := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/public=%t", branch, public), func(t *testing.T) {
				transition, configHome, _ := v2TransitionFixture(t, nil)
				diagnostic := v2PublicActivationError{"yard hermes: runtime reconciliation failed", "run yard -Y hermes status"}
				if branch == "bounded diagnostic" {
					diagnostic.message = "yard hermes: " + strings.Repeat("é", 249) + "x"
				}
				cause := errors.New("private primary sentinel")
				if public {
					cause = fmt.Errorf("private wrapper sentinel: %w", diagnostic)
				}
				reconciler := &v2PostErrorReconciler{
					post: V2ActivationObservation{Actual: digestB, Desired: digestA}, reconcileErr: cause,
				}
				wantCode := CodeRecoveryAmbiguous
				wantRecovery := "unknown state"
				switch branch {
				case "reobserve", "bounded diagnostic":
					reconciler.postErr = errors.New("private secondary sentinel")
					wantRecovery = "result cannot be observed safely"
				case "release observation":
					transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) {
						if reconciler.failed {
							return ReleaseLinks{}, errors.New("private secondary sentinel")
						}
						return ReleaseLinks{Active: "release-a"}, nil
					}
					wantRecovery = "release facts cannot be observed"
				case "settings blocker":
					reconciler.onFailure = func() {
						testkit.WriteFile(t, filepath.Join(configHome, "yards", "ordinary.env"),
							[]byte("YARD_TEMPLATE=private-assignment-sentinel\n"), 0o600)
					}
					wantCode = CodePreconditionBlocked
					wantRecovery = "yards/ordinary.env:1: YARD_TEMPLATE"
				case "unknown state":
					reconciler.post.Actual = digestC
				}
				transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
				goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
				inspection, err := transition.Inspect(context.Background(), goal)
				if err != nil || len(inspection.Blockers) != 0 {
					t.Fatalf("inspection = %#v, err=%v", inspection, err)
				}
				outcome, err := transition.Converge(context.Background(), Execution{
					Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
				})
				wantRetry := "run yard update --check"
				if branch == "settings blocker" {
					wantRetry = "run yard -Y ordinary config status"
				}
				if err != nil || outcome.Status != StatusOperatorActionRequired || outcome.Code != wantCode ||
					outcome.Retry != wantRetry || outcome.Transaction == nil || *outcome.Transaction != "tx-test-001" ||
					outcome.Target != goal.Target || !strings.Contains(outcome.Message, wantRecovery) {
					t.Fatalf("secondary failure = %#v, err=%v", outcome, err)
				}
				wantActive := ReleaseID("release-a")
				if branch == "release observation" {
					wantActive = ""
				}
				if outcome.Active != wantActive || outcome.Previous != nil ||
					strings.Contains(outcome.Message, "sentinel") || strings.Contains(outcome.Message, "private observation detail") ||
					strings.Contains(outcome.Message, "yard hermes:") != public ||
					len(outcome.Message) > maxDiagnosticText || !utf8.ValidString(outcome.Message) {
					t.Fatalf("unsafe or incomplete diagnostic = %#v", outcome)
				}
				if !public && !strings.Contains(outcome.Message, `activation reconciler "post-error-runtime" failed during reconcile`) {
					t.Fatalf("secondary failure lost reconciler and phase: %#v", outcome)
				}
				if err := ValidateProcessConvergence(goal, inspection, outcome); err != nil {
					t.Fatalf("outcome exceeds released protocol contract: %v", err)
				}
			})
		}
	}
}

func TestV2TransitionBoundsPublicCauseAndRecoveryExplanation(t *testing.T) {
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	for _, character := range []string{"x", "é", "🦄"} {
		t.Run(character, func(t *testing.T) {
			primary := "yard hermes: " + strings.Repeat(character, (maxDiagnosticText-13)/len(character))
			recovery := "protected recovery: " + strings.Repeat(character, (maxDiagnosticText-20)/len(character))
			guard := v2OperatorOutcome(ReleaseLinks{Active: "release-a", Previous: releaseIDPointer("release-old")},
				goal.Target, transactionIDPointer("tx-test-001"), CodeRecoveryAmbiguous, recovery, "run yard update --check")
			outcome := withPublicFailureCause(guard, v2PublicActivationError{primary, "run yard -Y hermes status"}, "")
			if len(outcome.Message) > maxDiagnosticText || !utf8.ValidString(outcome.Message) ||
				!strings.HasPrefix(outcome.Message, "yard hermes: ") ||
				!strings.Contains(outcome.Message, "; protected recovery: ") {
				t.Fatalf("bounded composition = %#v", outcome)
			}
			if err := ValidateProcessOutcome(goal, outcome); err != nil {
				t.Fatalf("bounded outcome violates released protocol contract: %v", err)
			}
			outcome.Message = guard.Message
			before, beforeErr := json.Marshal(guard)
			after, afterErr := json.Marshal(outcome)
			if beforeErr != nil || afterErr != nil || !bytes.Equal(before, after) {
				t.Fatalf("public cause changed protected outcome fields: before=%s after=%s", before, after)
			}
		})
	}
}

func TestV2TransitionPreservesPublicCauseAfterMutation(t *testing.T) {
	for _, branch := range []string{"recovering", "terminal recovery", "release observation"} {
		t.Run(branch, func(t *testing.T) {
			diagnostic := v2PublicActivationError{"yard hermes: settings reconciliation failed", "run yard -Y hermes status"}
			failed := false
			reconciler := &v2TestReconciler{}
			var configHome, settingsPath string
			transition, home, path := v2TransitionFixture(t, func(point string) error {
				faultPoint := "after-journal-authorized"
				if branch == "terminal recovery" {
					faultPoint = "after-journal-complete"
				}
				if point != faultPoint {
					return nil
				}
				failed = true
				switch branch {
				case "terminal recovery":
					reconciler.converged = false
				case "settings blocker":
					testkit.WriteFile(t, filepath.Join(configHome, "yards", "ordinary.env"),
						[]byte("YARD_TEMPLATE=private-assignment-sentinel\n"), 0o600)
				case "stale settings":
					testkit.WriteFile(t, settingsPath, []byte("YARD_TEMPLATE=test-vms\nSSH_PORT=2299\n"), 0o600)
				}
				return fmt.Errorf("private primary sentinel: %w", diagnostic)
			})
			configHome, settingsPath = home, path
			transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
			transition.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) {
				if failed && branch == "release observation" {
					return ReleaseLinks{}, errors.New("private secondary sentinel")
				}
				return ReleaseLinks{Active: "release-a"}, nil
			}
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil || len(inspection.Blockers) != 0 {
				t.Fatalf("inspection = %#v, err=%v", inspection, err)
			}
			outcome, err := transition.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			wantStatus, wantCode := StatusOperatorActionRequired, CodeRecoveryAmbiguous
			wantRetry, wantRecovery := "run yard update --check", "release facts cannot be observed"
			wantActive := ReleaseID("release-a")
			switch branch {
			case "recovering":
				wantStatus, wantCode = StatusRecovering, CodeVerificationFailed
				wantRetry, wantRecovery = diagnostic.retry, diagnostic.message
			case "terminal recovery":
				wantStatus, wantCode = StatusRecovering, CodeRecoveryPending
				wantRetry, wantRecovery = "run yard update", "can resume from observed facts"
			case "release observation":
				wantActive = ""
			case "settings blocker":
				wantCode, wantRecovery = CodePreconditionBlocked, "yards/ordinary.env:1: YARD_TEMPLATE"
				wantRetry = "run yard -Y ordinary config status"
			case "stale settings":
				wantCode, wantRecovery = CodeMigrationStale, "changed outside the authorized transition"
			}
			if err != nil || outcome.Status != wantStatus || outcome.Code != wantCode || outcome.Retry != wantRetry ||
				outcome.Active != wantActive || outcome.Previous != nil || outcome.Target != goal.Target ||
				outcome.Transaction == nil || *outcome.Transaction != "tx-test-001" ||
				!strings.Contains(outcome.Message, diagnostic.message) || !strings.Contains(outcome.Message, wantRecovery) ||
				strings.Contains(outcome.Message, "sentinel") {
				t.Fatalf("post-mutation cause = %#v, err=%v", outcome, err)
			}
			if err := ValidateProcessConvergence(goal, inspection, outcome); err != nil {
				t.Fatalf("outcome violates released protocol contract: %v", err)
			}
		})
	}
}

func TestV2TransitionActivationOnlyRepairPreservesFailureAndResumes(t *testing.T) {
	transition, configHome, settingsPath := v2TransitionFixture(t, nil)
	seedAppliedV2Ledger(t, configHome)
	testkit.WriteFile(t, settingsPath, []byte("YARD_TEMPLATE=test-vms\n"), 0o600)
	unrelatedPath := filepath.Join(configHome, "yards", "ordinary.env")
	unrelatedSettings := []byte("YARD_TEMPLATE=other-template\n")
	testkit.WriteFile(t, unrelatedPath, unrelatedSettings, 0o600)
	diagnostic := v2PublicActivationError{
		"yard demo: runtime reconciliation failed", "run yard -Y demo status",
	}
	transition.options.MigrationCheckpoint = false
	transition.options.Reconcilers = []V2ActivationReconciler{
		&v2FailingReconciler{id: "test-runtime", reconcileErr: diagnostic},
	}
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil || len(inspection.Blockers) != 0 || len(inspection.Decisions) != 1 {
		t.Fatalf("activation-only inspection = %#v, err=%v", inspection, err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusRecovering || outcome.Code != CodeDependencyUnavailable ||
		outcome.Message != diagnostic.message || outcome.Retry != diagnostic.retry {
		t.Fatalf("activation failure diagnostic = %#v, err=%v", outcome, err)
	}
	transition.options.Reconcilers = []V2ActivationReconciler{&v2TestReconciler{}}
	resume, err := transition.Inspect(context.Background(), goal)
	if err != nil || resume.Resume == nil || len(resume.Blockers) != 0 {
		t.Fatalf("activation-only resume = %#v, err=%v", resume, err)
	}
	settled, err := transition.Converge(context.Background(), Execution{Plan: resume.Plan})
	if err != nil || settled.Status != StatusReady {
		t.Fatalf("resumed repair = %#v, err=%v", settled, err)
	}
	if actual, err := os.ReadFile(unrelatedPath); err != nil || !bytes.Equal(actual, unrelatedSettings) {
		t.Fatalf("repair changed unrelated settings: %q, err=%v", actual, err)
	}
}

func TestV2TransitionReportsCompletedActivationObservationFailureAsBlocker(t *testing.T) {
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
		reinspection.Outcome.Status != StatusOperatorActionRequired ||
		reinspection.Outcome.Code != CodeDependencyUnavailable || !reinspection.Assessment.Changed ||
		reinspection.Outcome.Transaction == nil ||
		*reinspection.Outcome.Transaction != "tx-test-001" ||
		len(reinspection.Blockers) != 1 ||
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
		if point == "after-journal-authorized" {
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
		failure    error
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
			name:       "public diagnostic does not override third state guard",
			post:       V2ActivationObservation{Actual: digestC, Desired: digestA},
			failure:    v2PublicActivationError{"public reconcile detail", "run yard -Y demo integration status"},
			wantStatus: StatusOperatorActionRequired,
			wantCode:   CodeRecoveryAmbiguous,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reconciler := &v2PostErrorReconciler{post: test.post, reconcileErr: test.failure}
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
			if test.failure != nil && (!strings.Contains(outcome.Message, "public reconcile detail") ||
				!strings.Contains(outcome.Message, "unknown state") ||
				outcome.Retry != "run yard update --check") {
				t.Fatalf("reconcile diagnostic replaced recovery guard: %#v", outcome)
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

func TestFreshV2CompletedConvergeObservesOnlyUnderLock(t *testing.T) {
	for _, test := range []struct {
		name                                                    string
		driftAfterLock, cleanupFails, checkpointed, invalidPlan bool
	}{{name: "ready"}, {name: "checkpointed", checkpointed: true}, {name: "invalid-plan", invalidPlan: true}, {name: "drift", driftAfterLock: true}, {name: "cleanup-warning", cleanupFails: true}} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			transition, configHome, _ := v2TransitionFixture(t, nil)
			transition.options.MigrationCheckpoint = test.checkpointed
			reconciler := &v2TestReconciler{converged: false, warnings: []string{"runtime probe warning"}}
			transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			initial, err := transition.Inspect(ctx, goal)
			if err != nil {
				t.Fatal(err)
			}
			if outcome, err := transition.Converge(ctx, Execution{Plan: initial.Plan, Authorization: v2TestAuthorization(initial.Plan)}); err != nil || outcome.Status != StatusReady {
				t.Fatalf("initial convergence: %+v %v", outcome, err)
			}
			ready, err := transition.Inspect(ctx, goal)
			if err != nil || ready.Outcome.Status != StatusReady || ready.Assessment.Changed || ready.Resume != nil {
				t.Fatalf("ready inspection: %+v %v", ready, err)
			}
			options := transition.options
			if test.cleanupFails {
				options.fault = func(point string) error {
					if point == "before-recovery-gc" {
						return errors.New("cleanup interrupted")
					}
					return nil
				}
			}
			observedUnderLock := false
			observeLinks := options.ObserveLinks
			options.ObserveLinks = func(ctx context.Context) (ReleaseLinks, error) {
				fd, err := unix.Open(filepath.Join(configHome, "migrations", "update.lock"), unix.O_RDWR|unix.O_CLOEXEC, 0)
				if err != nil {
					return ReleaseLinks{}, err
				}
				defer unix.Close(fd)
				if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err == nil {
					if err := unix.Flock(fd, unix.LOCK_UN); err != nil {
						return ReleaseLinks{}, err
					}
				} else if errors.Is(err, unix.EWOULDBLOCK) {
					observedUnderLock = true
					if test.driftAfterLock {
						reconciler.converged = false
					}
				} else {
					return ReleaseLinks{}, err
				}
				return observeLinks(ctx)
			}
			fresh, err := NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			before := snapshotStoreV2Tree(t, configHome)
			reconciler.observes, reconciler.reconciles = 0, 0
			plan := ready.Plan
			if test.invalidPlan {
				plan = "plan-v1-" + PlanToken(strings.Repeat("f", 64))
			}
			outcome, err := fresh.Converge(ctx, Execution{Plan: plan})
			wantStatus, wantCode := StatusReady, CodeReady
			if test.driftAfterLock || test.invalidPlan {
				wantStatus, wantCode = StatusOperatorActionRequired, CodePlanStale
			}
			if err != nil || outcome.Status != wantStatus || outcome.Code != wantCode || reconciler.observes != 1 || reconciler.reconciles != 0 || !observedUnderLock {
				t.Fatalf("fresh convergence: outcome=%+v err=%v observes=%d reconciles=%d observedUnderLock=%t", outcome, err, reconciler.observes, reconciler.reconciles, observedUnderLock)
			}
			warnings := []string{"runtime probe warning"}
			if test.cleanupFails {
				warnings = append([]string{"recovery cleanup is pending"}, warnings...)
			}
			if !slices.Equal(outcome.Warnings, warnings) {
				t.Fatalf("ready warnings = %v, want %v", outcome.Warnings, warnings)
			}
			if !bytes.Equal(before, snapshotStoreV2Tree(t, configHome)) {
				t.Fatal("ready repeat or stale refusal changed protected state")
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
	binding       Fingerprint
	id            string
	converged     bool
	desired       Fingerprint
	drift         Fingerprint
	observes      int
	reconciles    int
	consequences  []string
	warnings      []string
	driftWarnings []string
}

type v2PostErrorReconciler struct {
	post                  V2ActivationObservation
	reconcileErr          error
	postErr               error
	onFailure             func()
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
	prepares        int
	observes        int
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
	owner.prepares++
	if owner.intermediate {
		return OwnerRegistrationObservation{}, errors.New("owner registration has authorized intermediate state")
	}
	return owner.observation(), nil
}

func (owner *v2TestOwnerRegistration) Observe(
	_ context.Context,
	before OwnerRegistrationObservation,
) (OwnerRegistrationProgress, error) {
	owner.observes++
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

func TestV2ActivationWarningsReachOutcomesAndClearAfterRepair(t *testing.T) {
	t.Run("invalid public warnings are rejected", func(t *testing.T) {
		for _, warning := range []string{"", "unsafe\nmessage", strings.Repeat("x", maxDiagnosticText+1)} {
			transition, _, _ := v2TransitionFixture(t, nil)
			transition.options.Reconcilers = []V2ActivationReconciler{&v2TestReconciler{warnings: []string{warning}}}
			if _, err := transition.Inspect(context.Background(), Goal{Target: "release-a", Direction: DirectionActivateTarget}); err == nil {
				t.Fatal("invalid warning was accepted")
			}
		}
	})

	t.Run("persistent warning is deduplicated", func(t *testing.T) {
		reconciler := &v2TestReconciler{
			warnings: []string{"yard default: handler refresh deferred", "yard default: handler refresh deferred"},
		}
		transition, _, _ := v2TransitionFixture(t, nil)
		transition.options.Reconcilers = []V2ActivationReconciler{reconciler, &v2TestReconciler{
			id:        "second-runtime",
			converged: true,
			warnings:  []string{"another persistent warning", "yard default: handler refresh deferred"},
		}}
		goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
		inspection, err := transition.Inspect(context.Background(), goal)
		if err != nil || inspection.Outcome == nil || len(inspection.Outcome.Warnings) != 2 ||
			inspection.Outcome.Warnings[0] != "another persistent warning" ||
			inspection.Outcome.Warnings[1] != "yard default: handler refresh deferred" {
			t.Fatalf("inspection warnings = %#v, err=%v", inspection.Outcome, err)
		}
		outcome, err := transition.Converge(context.Background(), Execution{
			Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
		})
		if err != nil || outcome.Status != StatusReady || len(outcome.Warnings) != 2 ||
			outcome.Warnings[0] != "another persistent warning" ||
			outcome.Warnings[1] != "yard default: handler refresh deferred" {
			t.Fatalf("execution warnings = %#v, err=%v", outcome, err)
		}
	})

	t.Run("transient warning clears after repair", func(t *testing.T) {
		reconciler := &v2TestReconciler{driftWarnings: []string{"repairable runtime drift"}}
		transition, _, _ := v2TransitionFixture(t, nil)
		transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
		goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
		inspection, err := transition.Inspect(context.Background(), goal)
		if err != nil || inspection.Outcome == nil || len(inspection.Outcome.Warnings) != 1 ||
			inspection.Outcome.Warnings[0] != "repairable runtime drift" {
			t.Fatalf("inspection warnings = %#v, err=%v", inspection.Outcome, err)
		}
		outcome, err := transition.Converge(context.Background(), Execution{
			Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
		})
		if err != nil || outcome.Status != StatusReady || len(outcome.Warnings) != 0 {
			t.Fatalf("execution retained cleared warning: %#v, err=%v", outcome, err)
		}
	})
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

func (reconciler *v2TestReconciler) ID() string {
	if reconciler.id != "" {
		return reconciler.id
	}
	return "test-runtime"
}

func (reconciler *v2PostErrorReconciler) ID() string { return "post-error-runtime" }

func (reconciler *v2PostErrorReconciler) Observe(
	context.Context,
	ReleasePair,
	ReleaseLinks,
) (V2ActivationObservation, error) {
	if reconciler.failed {
		reconciler.postErrorObservations++
		return reconciler.post, reconciler.postErr
	}
	return V2ActivationObservation{Actual: digestB, Desired: digestA, Converged: false}, nil
}

func (reconciler *v2PostErrorReconciler) Reconcile(context.Context, ReleaseLinks) error {
	reconciler.failed = true
	if reconciler.onFailure != nil {
		reconciler.onFailure()
	}
	if reconciler.reconcileErr != nil {
		return reconciler.reconcileErr
	}
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
	desired := reconciler.desired
	if desired == "" {
		desired = digestA
	}
	drift := reconciler.drift
	if drift == "" {
		drift = digestB
	}
	warnings := slices.Clone(reconciler.warnings)
	if !reconciler.converged {
		warnings = append(warnings, reconciler.driftWarnings...)
	}
	return V2ActivationObservation{
		Binding: reconciler.binding,
		Actual:  map[bool]Fingerprint{true: desired, false: drift}[reconciler.converged],
		Desired: desired, Converged: reconciler.converged,
		Consequences: slices.Clone(reconciler.consequences), Warnings: warnings,
	}, nil
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
	seedLegacyCompletedLedger(t, configHome)
	transition, err := NewV2Transition(V2Options{
		ConfigHome: configHome, Releases: releases, MigrationCheckpoint: true, SourceMigrationCheckpoint: true,
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
	payload := append([]byte(nil), historyV028Payload...)
	var err error
	if err != nil {
		t.Fatal(err)
	}
	// Retained-caller fixtures keep the original power and settings contracts.
	// Publication and SSH localization have their own current-registry tests.
	var registry RegistryV2
	if err := json.Unmarshal(payload, &registry); err != nil {
		t.Fatal(err)
	}
	registry.CurrentEpochs["owner-registration"] = 2
	registry.Migrations = slices.DeleteFunc(registry.Migrations, func(m MigrationDefinitionV2) bool { return nativeMigration(m) })
	registry.CurrentEpochs["power-metadata"] = 1
	registry.Migrations = slices.DeleteFunc(registry.Migrations, func(m MigrationDefinitionV2) bool { return m.Kind == "owner-publication-state-v1" })
	registry.CurrentEpochs["settings"] = 2
	registry.Migrations = slices.DeleteFunc(registry.Migrations, func(m MigrationDefinitionV2) bool { return m.Kind == localSSHPortMigration })
	payload, err = json.Marshal(registry)
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

func TestSupportedBuiltinCatalogRetainsIndependentIntroductions(t *testing.T) {
	previous := PreviousBuiltinCapabilityCatalog()
	if len(previous.descriptors) != 2 {
		t.Fatal("sealed historical catalog acquired a later implementation")
	}
	for _, kinds := range [][]string{
		nil,
		{localSSHPortMigration},
		{"owner-publication-state-v1"},
		{localSSHPortMigration, "owner-publication-state-v1"},
	} {
		descriptors := slices.Clone(previous.descriptors)
		for _, kind := range kinds {
			descriptor, ok := BuiltinCapabilityCatalog().descriptor(kind)
			if !ok {
				t.Fatalf("current catalog lost %s", kind)
			}
			descriptors = append(descriptors, descriptor)
		}
		catalog, err := NewCapabilityCatalog(descriptors)
		if err != nil {
			t.Fatal(err)
		}
		retained, supported := SupportedBuiltinCatalog(catalog.Digest())
		if !supported || retained.Digest() != catalog.Digest() {
			t.Fatalf("exact historical catalog not retained: %v", kinds)
		}
		descriptors[0].Version++
		changed, err := NewCapabilityCatalog(descriptors)
		if err != nil {
			t.Fatal(err)
		}
		if _, supported := SupportedBuiltinCatalog(changed.Digest()); supported {
			t.Fatal("changed implementation acquired historical recovery authority")
		}
	}
}

type v2RefusingFreshOwner struct{ *v2TestOwnerRegistration }

func (*v2RefusingFreshOwner) FreshWarning(context.Context, V2SettingsSnapshotView) (string, error) {
	return "test-yard requires recreation; run yard -Y e2e-yard teardown --reset-config, then yard service install test-vms", nil
}

func TestPublicActivationDiagnosticPreservesWrappedFallbackAndSafeActions(t *testing.T) {
	cause := fmt.Errorf("private wrapper: %w", domain.CollectRefusals(errors.New("private transport"), v2PublicActivationError{"route unavailable", "run yard update --check"}))
	message, retry := publicActivationDiagnostic(cause, "installation cannot be inspected", "inspect installation ownership")
	if message != "installation cannot be inspected; route unavailable" || retry != "inspect installation ownership, then run yard update --check" || validateSafeAction(retry) != nil {
		t.Fatalf("%q / %q", message, retry)
	}
	message, retry = publicActivationDiagnostic(domain.CollectRefusals(errors.New("private one"), errors.New("private two")), "installation unavailable", "inspect installation ownership")
	if message != "installation unavailable" || retry != "inspect installation ownership" {
		t.Fatalf("%q / %q", message, retry)
	}
}
