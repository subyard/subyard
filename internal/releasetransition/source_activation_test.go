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
				resume, err := transition.Inspect(context.Background(), goal)
				if err != nil || resume.Resume == nil || len(resume.Blockers) != 0 {
					t.Fatalf("restart inspection: %#v err=%v", resume, err)
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
