package releasetransition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
)

func TestSourceActivationCarriesOnlyUnverifiedNativeIntentDesired(t *testing.T) {
	for _, scenario := range []string{"both", "verified", "ambiguous", "complete"} {
		t.Run(scenario, func(t *testing.T) {
			a, b := Fingerprint(strings.Repeat("a", 64)), Fingerprint(strings.Repeat("b", 64))
			native := nativeMigrationWork(PlannerStepIntent{ID: "native", Resource: "yard.default", Desired: a}, MigrationDefinitionV2{Kind: registeredYardsMigration, Domain: "owner-registration", FromEpoch: 2, ToEpoch: 3})
			guest := nativeMigrationWork(PlannerStepIntent{ID: "guest", Resource: "yard.default", Desired: b}, MigrationDefinitionV2{Kind: guestDiskTmpMigration, Domain: "power-metadata", FromEpoch: 2, ToEpoch: 3})
			observation := v2Observation{work: []v2Work{native, guest}, journal: &JournalRecord{Checkpoint: JournalMigrating}}
			if scenario == "verified" {
				observation.journal.Steps = []JournalStep{{ID: "native", Checkpoint: StepVerified}}
			}
			if scenario == "complete" {
				observation.journal.Checkpoint = JournalComplete
			}
			if scenario == "ambiguous" {
				duplicate := native
				duplicate.intent.Desired = b
				observation.work = append(observation.work, duplicate, native)
			}
			transition := &V2Transition{}
			transition.retainSourceActivationView(&observation, nil)
			if scenario == "complete" {
				if observation.sourceActivationView != nil {
					t.Fatal("completed history supplied pending repair authority")
				}
				return
			}
			view := observation.sourceActivationView
			if view == nil || view.GuestDiskTmp["yard.default"] != b {
				t.Fatal("pending guest intent missing")
			}
			want := a
			if scenario == "verified" || scenario == "ambiguous" {
				want = ""
			}
			if view.RegisteredYards["yard.default"] != want {
				t.Fatal("verified or ambiguous native work supplied repair authority")
			}
		})
	}
}

type sourceProjectionBackend struct {
	applied    map[string]Fingerprint
	applies    int
	nativeSalt string
}

type sourceProjectionOwner struct {
	id, home, settings, shared string
	owner                      *v2SettingsBoundOwnerRegistration
	backend                    *sourceProjectionBackend
	projectionSalt             string
	plans                      int
	observationErr             error
	warnings                   []string
	driftAfterReconcile        bool
}

func (owner *sourceProjectionOwner) ID() string { return owner.id }

func (owner *sourceProjectionOwner) desired(view *V2SourceActivationView) (Fingerprint, error) {
	read := func(path string) (config.PersistentFileSnapshot, error) {
		if view != nil {
			return view.Settings.ReadSnapshot(path)
		}
		return config.ReadPersistentFileSnapshot(owner.home, path)
	}
	settings, err := read(owner.settings)
	if err != nil {
		return "", err
	}
	shared, err := read(owner.shared)
	if err != nil {
		return "", err
	}
	registered := owner.owner.state == OwnerRegistrationCurrent
	salt := owner.backend.nativeSalt
	if view != nil {
		if view.OwnerRegistration == nil || view.OwnerRegistration.Registration != fingerprintPayload(settings.Content) {
			return "", errors.New("projection did not receive the exact post-settings owner before-state")
		}
		registered = true
		salt = owner.projectionSalt
	}
	if settings.Exists && registered && !strings.Contains(string(settings.Content), "YARD_TEMPLATE='test-vms'") {
		return "", errors.New("projection did not receive canonical settings")
	}
	payload, err := json.Marshal(struct {
		ID               string
		Registered       bool
		Settings, Shared []byte
		Salt             string
	}{owner.id, registered, settings.Content, shared.Content, salt})
	return fingerprintPayload(payload), err
}

func (owner *sourceProjectionOwner) Observe(context.Context, ReleasePair, ReleaseLinks) (V2ActivationObservation, error) {
	if owner.observationErr != nil {
		return V2ActivationObservation{}, owner.observationErr
	}
	desired, err := owner.desired(nil)
	actual := owner.backend.applied[owner.id]
	if actual == "" {
		actual = digestD
	}
	return V2ActivationObservation{Actual: actual, Desired: desired, Converged: actual == desired, Warnings: owner.warnings}, err
}

func (owner *sourceProjectionOwner) PrepareSourceActivation(_ context.Context, _ ReleasePair, _ ReleaseLinks, view V2SourceActivationView) (V2SourceActivationPlan, error) {
	owner.plans++
	desired, err := owner.desired(&view)
	return V2SourceActivationPlan{Desired: desired, Consequences: []string{"reconcile prepared " + owner.id}}, err
}

func (owner *sourceProjectionOwner) Reconcile(context.Context, ReleaseLinks) error {
	desired, err := owner.desired(nil)
	if err != nil {
		return err
	}
	owner.backend.applies++
	owner.backend.applied[owner.id] = desired
	if owner.driftAfterReconcile {
		owner.backend.nativeSalt = "unapproved post-reconcile desired"
	}
	return nil
}

type sourceActivationDiagnosticError struct{ conflict bool }

func (sourceActivationDiagnosticError) Error() string { return "private-native-observation-detail" }
func (sourceActivationDiagnosticError) ActivationDiagnostic() (string, string) {
	return "yard named: source activation observation failed", "run yard -Y named status"
}
func (err sourceActivationDiagnosticError) ActivationConflict() bool { return err.conflict }

func TestPreparedSourceActivationPreservesNativeObservationFailure(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("conflict=%t", conflict), func(t *testing.T) {
			transition, _, owners, backend := sourceProjectionFixture(t)
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			transition.options.fault = func(point string) error {
				if point == "after-owner-registration-commit" {
					owners[0].observationErr = fmt.Errorf("private-native-wrapper: %w", sourceActivationDiagnosticError{conflict: conflict})
					owners[1].warnings = []string{"z current warning", "a current warning", "z current warning"}
				}
				return nil
			}
			outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			code := CodeDependencyUnavailable
			if conflict {
				code = CodeActivationAmbiguous
			}
			if err != nil || outcome.Status != StatusOperatorActionRequired || outcome.Code != code ||
				outcome.Message != "yard named: source activation observation failed" || outcome.Retry != "run yard -Y named status" ||
				!slices.Equal(outcome.Warnings, []string{"a current warning", "z current warning"}) || backend.applies != 0 {
				t.Fatalf("native observation failure: %#v applies=%d err=%v", outcome, backend.applies, err)
			}
			payload, err := json.Marshal(outcome)
			if err != nil || strings.Contains(string(payload), "private-native") {
				t.Fatalf("private observation detail escaped: %s err=%v", payload, err)
			}
			checkpoint, err := transition.store.ReadMigrationCheckpoint()
			if err != nil || checkpoint.Exists {
				t.Fatalf("failed observation published checkpoint: %t err=%v", checkpoint.Exists, err)
			}
		})
	}
}

func TestPreparedSourceActivationRejectsPostReconcileDesiredDrift(t *testing.T) {
	transition, _, owners, backend := sourceProjectionFixture(t)
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	owners[0].driftAfterReconcile = true
	outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || outcome.Code != CodePlanStale || backend.applies != 1 {
		t.Fatalf("post-reconcile drift: %#v applies=%d err=%v", outcome, backend.applies, err)
	}
	checkpoint, err := transition.store.ReadMigrationCheckpoint()
	if err != nil || checkpoint.Exists {
		t.Fatalf("post-reconcile drift published checkpoint: %t err=%v", checkpoint.Exists, err)
	}
}

func sourceProjectionFixture(t *testing.T) (*V2Transition, *v2IngressFixture, []*sourceProjectionOwner, *sourceProjectionBackend) {
	t.Helper()
	transition, home, _ := v2TransitionFixture(t, nil)
	settings := v2SourceSettingsPath(home, "e2e-yard")
	shared := filepath.Join(home, "config.env")
	imported := []byte("YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\nSSH_PORT=2224\n")
	owner := &v2SettingsBoundOwnerRegistration{configHome: home, path: settings}
	transition.options.OwnerRegistration = owner
	transition.options.MigrationCheckpoint = true
	transition.options.SourceIngress = &SourceIngressRequest{SchemaVersion: SourceIngressRequestSchemaV1, Kind: SourceIngressPreGoV1,
		SourceRoot: filepath.Join(home, "source"), DataHome: filepath.Join(home, "data"), BinDir: filepath.Join(home, "bin"), RC: filepath.Join(home, "rc"), LoginRC: filepath.Join(home, "login-rc")}
	source := &v2IngressFixture{inspection: V2IngressInspection{
		Operations: []V2IngressOperation{{Kind: V2SourceImport, Decision: DecisionCanonicalize, Expected: digestB, Desired: digestC}},
		Prospective: v2MemorySettingsView{yards: []string{"e2e-yard", "hermes"}, snapshots: map[string]config.PersistentFileSnapshot{
			settings: {Exists: true, Content: imported}, shared: {Exists: true, Content: []byte("SYNTHETIC_IMPORTED=1\n")},
		}},
	}, actual: map[V2IngressOperationKind]Fingerprint{V2SourceImport: digestB}, applyHook: func(V2IngressStep) error {
		if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(settings, imported, 0o600); err != nil {
			return err
		}
		return os.WriteFile(shared, []byte("SYNTHETIC_IMPORTED=1\n"), 0o600)
	}}
	transition.options.Ingress = source
	backend := &sourceProjectionBackend{applied: map[string]Fingerprint{}}
	var owners []*sourceProjectionOwner
	for _, id := range []string{"synthetic-assets", "synthetic-broker", "synthetic-routes"} {
		reconciler := &sourceProjectionOwner{id: id, home: home, settings: settings, shared: shared, owner: owner, backend: backend}
		desired, err := reconciler.desired(nil)
		if err != nil {
			t.Fatal(err)
		}
		backend.applied[id] = desired
		owners = append(owners, reconciler)
		transition.options.Reconcilers = append(transition.options.Reconcilers, reconciler)
	}
	return transition, source, owners, backend
}

func restartSourceProjection(t *testing.T, original *V2Transition, owners []*sourceProjectionOwner) (*V2Transition, []*sourceProjectionOwner) {
	t.Helper()
	options := original.options
	options.fault = nil
	options.Reconcilers = nil
	var freshOwners []*sourceProjectionOwner
	for _, owner := range owners {
		fresh := *owner
		fresh.plans = 0
		freshOwners = append(freshOwners, &fresh)
		options.Reconcilers = append(options.Reconcilers, &fresh)
	}
	fresh, err := NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	return fresh, freshOwners
}

func TestPreparedSourceActivationSharesOneConsentAndReconstructsAfterRestart(t *testing.T) {
	for _, fault := range []string{"", "after-journal-authorized", "ingress-apply", "after-source-import-apply", "after-settings-cas", "after-owner-registration-commit", "before-checkpoint-publication"} {
		t.Run(fault, func(t *testing.T) {
			transition, source, owners, backend := sourceProjectionFixture(t)
			if fault == "ingress-apply" {
				source.applyErr = errors.New("source import process handoff")
			}
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			before, err := transition.observe(context.Background(), goal)
			if err != nil || !before.activationFixed || !before.sourceActivationPrepared || !before.checkpointConsent {
				t.Fatalf("prospective observation: %#v err=%v", before, err)
			}
			for index, owner := range owners {
				native, err := owner.Observe(context.Background(), transition.options.Releases, before.links)
				if err != nil || !native.Converged || native.Desired == before.activationScope[index].Desired {
					t.Fatalf("native pre-state was replaced: %#v err=%v", native, err)
				}
			}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil || len(inspection.Blockers) != 0 {
				t.Fatalf("inspect: %#v err=%v", inspection, err)
			}
			grants := 0
			transition.options.VerifyAuthorization = func(plan PlanToken, grant Authorization) bool { grants++; return grant == v2TestAuthorization(plan) }
			transition.options.fault = func(point string) error {
				if fault != "" && point == fault {
					return errors.New("interrupt source transition")
				}
				return nil
			}
			outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if fault != "" {
				if err != nil || outcome.Status != StatusRecovering {
					t.Fatalf("interrupt: %#v err=%v", outcome, err)
				}
				transition, owners = restartSourceProjection(t, transition, owners)
				resume, inspectErr := transition.Inspect(context.Background(), goal)
				if inspectErr != nil || resume.Resume == nil || len(resume.Blockers) != 0 {
					t.Fatalf("restart inspection: %#v err=%v", resume, inspectErr)
				}
				outcome, err = transition.Converge(context.Background(), Execution{Plan: resume.Plan})
			}
			if err != nil || outcome.Status != StatusReady || grants != 1 || backend.applies != 3 {
				t.Fatalf("completion: %#v grants=%d applies=%d err=%v", outcome, grants, backend.applies, err)
			}
			owner := owners[0].owner
			wantCommits := 1
			if fault == "after-owner-registration-commit" {
				// A crash before recording evidence replays the guarded commit.
				wantCommits = 2
			}
			canonical, err := os.ReadFile(owner.path)
			if err != nil || !strings.Contains(string(canonical), "YARD_TEMPLATE='test-vms'") || strings.Contains(string(canonical), "NESTED_E2E_VMS") || owner.commits != wantCommits || owner.state != OwnerRegistrationCurrent {
				t.Fatalf("canonical settings/owner commit: bytes=%q commits=%d state=%s err=%v", canonical, owner.commits, owner.state, err)
			}
			checkpoint, err := transition.store.ReadMigrationCheckpoint()
			if err != nil || !checkpoint.Exists {
				t.Fatalf("missing checkpoint: exists=%t err=%v", checkpoint.Exists, err)
			}
			final, err := transition.Inspect(context.Background(), goal)
			if err != nil || final.Outcome.Status != StatusReady || final.Assessment.Changed {
				t.Fatalf("final: %#v err=%v", final, err)
			}
		})
	}
}

func TestPreparedSourceActivationRejectsChangedProjectionBeforeSourceWrites(t *testing.T) {
	transition, source, owners, backend := sourceProjectionFixture(t)
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	owners[0].projectionSalt = "unexpected input"
	outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || outcome.Code != CodePlanStale || source.applies != 0 || backend.applies != 0 {
		t.Fatalf("changed projection: %#v source=%d applies=%d err=%v", outcome, source.applies, backend.applies, err)
	}
	journal, err := transition.store.ReadCurrentJournal()
	if err != nil || journal.Exists {
		t.Fatalf("stale projection published journal: %t err=%v", journal.Exists, err)
	}
}

func TestPreparedSourceActivationRejectsUnexpectedNativeDesiredBeforeActivation(t *testing.T) {
	for _, fault := range []string{"after-owner-registration-commit", "before-reconciler-0"} {
		t.Run(fault, func(t *testing.T) {
			transition, _, _, backend := sourceProjectionFixture(t)
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			transition.options.fault = func(point string) error {
				if point == fault {
					backend.nativeSalt = "outside approved source work"
				}
				return nil
			}
			outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if err != nil || outcome.Code != CodePlanStale || backend.applies != 0 {
				t.Fatalf("native drift: %#v applies=%d err=%v", outcome, backend.applies, err)
			}
			checkpoint, err := transition.store.ReadMigrationCheckpoint()
			if err != nil || checkpoint.Exists {
				t.Fatalf("native drift published checkpoint: %t err=%v", checkpoint.Exists, err)
			}
		})
	}
}

func TestPreparedSourceActivationRestartRejectsChangedDesired(t *testing.T) {
	for _, fault := range []string{"after-journal-authorized", "after-owner-registration-commit", "after-reconciling"} {
		t.Run(fault, func(t *testing.T) {
			transition, source, owners, backend := sourceProjectionFixture(t)
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			transition.options.fault = func(point string) error {
				if point == fault {
					return errors.New("restart before activation")
				}
				return nil
			}
			outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if err != nil || outcome.Status != StatusRecovering {
				t.Fatalf("interrupt: %#v err=%v", outcome, err)
			}
			transition, owners = restartSourceProjection(t, transition, owners)
			if fault == "after-journal-authorized" {
				owners[0].projectionSalt = "changed prepared input"
			}
			resume, err := transition.Inspect(context.Background(), goal)
			if err != nil || resume.Resume == nil {
				t.Fatalf("resume: %#v err=%v", resume, err)
			}
			if fault == "after-owner-registration-commit" {
				observed, err := transition.observe(context.Background(), goal)
				if err != nil || !observed.sourceActivationPrepared {
					t.Fatalf("native-exact owner handoff lost prepared guards: prepared=%t err=%v", observed.sourceActivationPrepared, err)
				}
				transition.options.fault = func(point string) error {
					if point == "after-ledger-verified" {
						backend.nativeSalt = "unexpected drift after owner handoff"
					}
					return nil
				}
			}
			if fault == "after-reconciling" {
				transition.options.fault = func(point string) error {
					if point == "before-reconciler-0" {
						backend.nativeSalt = "changed verified source input"
					}
					return nil
				}
			}
			beforeImports := source.applies
			outcome, err = transition.Converge(context.Background(), Execution{Plan: resume.Plan})
			if err != nil || outcome.Code != CodePlanStale || backend.applies != 0 || source.applies != beforeImports {
				t.Fatalf("restart changed desired: %#v applies=%d source=%d/%d err=%v", outcome, backend.applies, source.applies, beforeImports, err)
			}
			checkpoint, err := transition.store.ReadMigrationCheckpoint()
			if err != nil || checkpoint.Exists {
				t.Fatalf("restart drift published checkpoint: %t err=%v", checkpoint.Exists, err)
			}
		})
	}
}

func TestPreparedSourceActivationCompletedJournalUsesNativeDesired(t *testing.T) {
	transition, _, owners, backend := sourceProjectionFixture(t)
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	transition.options.fault = func(point string) error {
		if point == "before-checkpoint-publication" {
			return errors.New("stop before publication")
		}
		return nil
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || outcome.Status != StatusRecovering {
		t.Fatalf("interrupt: %#v err=%v", outcome, err)
	}
	backend.nativeSalt = "changed after completed migration"
	transition, owners = restartSourceProjection(t, transition, owners)
	observed, err := transition.observe(context.Background(), goal)
	if err != nil || observed.checkpointConsent || observed.sourceActivationPrepared {
		t.Fatalf("completed consent reused: %#v err=%v", observed, err)
	}
	for _, owner := range owners {
		if owner.plans != 0 {
			t.Fatal("completed history used frozen projection")
		}
	}
	outcome, err = transition.Converge(context.Background(), Execution{Plan: inspection.Plan})
	if err != nil || outcome.Status == StatusReady {
		t.Fatalf("completed stale grant: %#v err=%v", outcome, err)
	}
	checkpoint, err := transition.store.ReadMigrationCheckpoint()
	if err != nil || checkpoint.Exists {
		t.Fatalf("completed drift published checkpoint: %t err=%v", checkpoint.Exists, err)
	}
}

func TestPreparedSourceActivationFreshGoalAfterSyntheticCompletedRegistryPrefix(t *testing.T) {
	for _, direction := range []Direction{DirectionActivateTarget, DirectionActivatePrevious} {
		t.Run(string(direction), func(t *testing.T) {
			original, _, owners, backend := sourceProjectionFixture(t)
			options := original.options
			options.SourceIngress, options.Ingress = nil, nil
			options.MigrationCheckpoint = false
			previous := ReleaseID("release-b")
			links := ReleaseLinks{Active: "release-a", Previous: &previous}
			options.ObserveLinks = func(context.Context) (ReleaseLinks, error) { return links, nil }
			options.Releases.Previous = &previous
			var prefix RegistryV2
			if err := json.Unmarshal(options.RegistryPayload, &prefix); err != nil {
				t.Fatal(err)
			}
			prefix.Migrations = []MigrationDefinitionV2{}
			prefix.CurrentEpochs = map[string]int{}
			for domain, epoch := range prefix.MinimumEpochs {
				prefix.CurrentEpochs[domain] = epoch
			}
			priorOptions := options
			var err error
			priorOptions.RegistryPayload, err = json.Marshal(prefix)
			if err != nil {
				t.Fatal(err)
			}
			priorOptions.NewTransactionID = func() TransactionID { return "tx-history-" + TransactionID(direction) }
			backend.applied[owners[0].id] = digestD
			prior, err := NewV2Transition(priorOptions)
			if err != nil {
				t.Fatal(err)
			}
			priorGoal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			inspection, err := prior.Inspect(context.Background(), priorGoal)
			if err != nil || len(inspection.Blockers) != 0 {
				t.Fatal(inspection, err)
			}
			outcome, err := prior.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if err != nil || outcome.Status != StatusReady {
				t.Fatal(outcome, err)
			}
			historySnapshot, err := prior.store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			history, err := ParseJournal(historySnapshot.Payload)
			if err != nil || history.Checkpoint != JournalComplete || len(history.Steps) != 0 {
				t.Fatal(history, err)
			}
			if err := os.MkdirAll(filepath.Dir(owners[0].settings), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(owners[0].settings, []byte("YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\nSSH_PORT=2225\n"), 0600); err != nil {
				t.Fatal(err)
			}
			options.Releases.Target, options.Direction = previous, direction
			if direction == DirectionActivatePrevious {
				options.RollbackTarget = &RollbackTarget{Version: "0.25.2", RegistryDigest: fingerprintPayload(options.RegistryPayload)}
			}
			activations, grants := 0, 0
			options.ActivateLinks = func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
				activations++
				old := pair.From
				links = ReleaseLinks{Active: pair.Target, Previous: &old}
				return links, nil
			}
			options.VerifyAuthorization = func(plan PlanToken, grant Authorization) bool { grants++; return grant == v2TestAuthorization(plan) }
			options.NewTransactionID = func() TransactionID { return "tx-fresh-" + TransactionID(direction) }
			options.fault = func(point string) error {
				if point == "after-owner-registration-commit" {
					return errors.New("pause after authorized source commit")
				}
				return nil
			}
			fresh, err := NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			goal := Goal{Target: previous, Direction: direction}
			prepared, err := fresh.observe(context.Background(), goal)
			if err != nil || len(prepared.blockers) != 0 || !prepared.sourceActivationPrepared || prepared.observationScope == history.ObservationScope {
				t.Fatalf("fresh source goal borrowed completed history: prepared=%t blockers=%v scope=%s history=%s err=%v", prepared.sourceActivationPrepared, prepared.blockers, prepared.observationScope, history.ObservationScope, err)
			}
			unchanged, err := fresh.store.ReadCurrentJournal()
			if err != nil || !slices.Equal(unchanged.Payload, historySnapshot.Payload) {
				t.Fatal("fresh planning rewrote historical grant", err)
			}
			inspection, err = fresh.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err = fresh.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if err != nil || outcome.Status != StatusRecovering || grants != 1 || activations != 0 {
				t.Fatal(outcome, grants, activations, err)
			}
			options.fault = nil
			resumed, err := NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			inspection, err = resumed.Inspect(context.Background(), goal)
			if err != nil || inspection.Resume == nil || *inspection.Resume == history.Transaction || len(inspection.Blockers) != 0 {
				t.Fatal(inspection, err)
			}
			outcome, err = resumed.Converge(context.Background(), Execution{Plan: inspection.Plan})
			if err != nil || outcome.Status != StatusReady || grants != 1 || activations != 1 {
				t.Fatal(outcome, grants, activations, err)
			}
		})
	}
}
