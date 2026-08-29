package releasetransition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
)

func TestV2SourceImportRunsBeforeProspectiveSettingsCAS(t *testing.T) {
	transition, _, settingsPath := v2TransitionFixture(t, nil)
	if err := os.Remove(settingsPath); err != nil {
		t.Fatal(err)
	}
	imported := []byte("YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\nSSH_PORT=2224\n")
	source := &v2IngressFixture{
		inspection: V2IngressInspection{
			Operations: []V2IngressOperation{{
				Kind: V2SourceImport, Decision: DecisionCanonicalize,
				Expected: digestB, Desired: digestC,
			}},
			Decisions: []RedactedDecision{{
				Resource: "source-install.config", Scope: "source-install",
				Decision: DecisionCanonicalize, Result: "imported",
			}},
			Prospective: v2MemorySettingsView{
				yards: []string{"hermes"},
				snapshots: map[string]config.PersistentFileSnapshot{
					settingsPath: {Exists: true, Content: imported},
				},
			},
		},
		actual: map[V2IngressOperationKind]Fingerprint{V2SourceImport: digestB},
		applyHook: func(step V2IngressStep) error {
			if step.Kind != V2SourceImport {
				return nil
			}
			return os.WriteFile(settingsPath, imported, 0o600)
		},
	}
	transition.options.Ingress = source
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusReady {
		t.Fatalf("source/settings outcome = %#v, err=%v", outcome, err)
	}
	content, err := os.ReadFile(settingsPath)
	if err != nil || !strings.Contains(string(content), "YARD_TEMPLATE='test-vms'") ||
		strings.Contains(string(content), "NESTED_E2E_VMS") {
		t.Fatalf("source/settings content = %q, err=%v", content, err)
	}
}

func TestV2SourceProspectiveOwnerImpactIsAuthorizedBeforeImport(t *testing.T) {
	transition, configHome, _ := v2TransitionFixture(t, nil)
	settingsPath := v2SourceSettingsPath(configHome, "e2e-yard")
	imported := []byte("YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\nSSH_PORT=2224\n")
	owner := &v2SettingsBoundOwnerRegistration{configHome: configHome, path: settingsPath}
	transition.options.OwnerRegistration = owner
	source := &v2IngressFixture{
		inspection: V2IngressInspection{
			Operations: []V2IngressOperation{{
				Kind: V2SourceImport, Decision: DecisionCanonicalize,
				Expected: digestB, Desired: digestC,
			}},
			Prospective: v2MemorySettingsView{
				yards: []string{"e2e-yard"},
				snapshots: map[string]config.PersistentFileSnapshot{
					settingsPath: {Exists: true, Content: imported},
				},
			},
		},
		actual:   map[V2IngressOperationKind]Fingerprint{V2SourceImport: digestB},
		applyErr: errors.New("source import process handoff"),
		applyHook: func(V2IngressStep) error {
			if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
				return err
			}
			return os.WriteFile(settingsPath, imported, 0o600)
		},
	}
	transition.options.Ingress = source
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}

	interrupted, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || interrupted.Status != StatusRecovering {
		t.Fatalf("source handoff outcome = %#v err=%v", interrupted, err)
	}
	resume, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: resume.Plan})
	if err != nil || outcome.Status != StatusReady || owner.commits != 1 ||
		owner.state != OwnerRegistrationCurrent {
		t.Fatalf("prospective owner outcome = %#v owner=%#v err=%v", outcome, owner, err)
	}
}

func TestV2SourceProspectiveOwnerResumesBeforeImport(t *testing.T) {
	activeFault := true
	transition, configHome, _ := v2TransitionFixture(t, func(point string) error {
		if activeFault && point == "after-journal-authorized" {
			return errors.New("source import has not started")
		}
		return nil
	})
	settingsPath := v2SourceSettingsPath(configHome, "e2e-yard")
	imported := []byte("YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\nSSH_PORT=2224\n")
	owner := &v2SettingsBoundOwnerRegistration{configHome: configHome, path: settingsPath}
	transition.options.OwnerRegistration = owner
	source := &v2IngressFixture{
		inspection: V2IngressInspection{
			Operations: []V2IngressOperation{{
				Kind: V2SourceImport, Decision: DecisionCanonicalize,
				Expected: digestB, Desired: digestC,
			}},
			Prospective: v2MemorySettingsView{
				yards: []string{"e2e-yard"},
				snapshots: map[string]config.PersistentFileSnapshot{
					settingsPath: {Exists: true, Content: imported},
				},
			},
		},
		actual: map[V2IngressOperationKind]Fingerprint{V2SourceImport: digestB},
		applyHook: func(V2IngressStep) error {
			if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
				return err
			}
			return os.WriteFile(settingsPath, imported, 0o600)
		},
	}
	transition.options.Ingress = source
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || interrupted.Status != StatusRecovering {
		t.Fatalf("pre-import interruption = %#v err=%v", interrupted, err)
	}

	activeFault = false
	resume, err := transition.Inspect(context.Background(), goal)
	if err != nil || resume.Resume == nil || len(resume.Blockers) != 0 {
		t.Fatalf("pre-import resume = %#v err=%v", resume, err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: resume.Plan})
	if err != nil || outcome.Status != StatusReady || owner.commits != 1 ||
		owner.state != OwnerRegistrationCurrent {
		t.Fatalf("resumed prospective owner = %#v owner=%#v err=%v", outcome, owner, err)
	}
}

func TestV2SourceApplyFaultResumesExactOuterTransactionWithoutNewAuthorization(t *testing.T) {
	transition, configHome, _ := v2TransitionFixture(t, nil)
	applyFault := errors.New("source apply interrupted after mutation")
	source := &v2IngressFixture{
		inspection: V2IngressInspection{
			Operations: []V2IngressOperation{
				{Kind: V2SourceImport, Decision: DecisionCanonicalize, Expected: digestB, Desired: digestC},
				{Kind: V2SourceEntrypoints, Decision: DecisionCanonicalize, Expected: digestC, Desired: digestA},
			},
			Decisions: []RedactedDecision{
				{Resource: "source-install.config", Scope: "source-install", Decision: DecisionCanonicalize, Result: "imported"},
				{Resource: "source-install.entrypoints", Scope: "source-install", Decision: DecisionCanonicalize, Result: "stable-runtime"},
			},
			Prospective: v2DiskSettingsView{configHome: configHome, yards: []string{"hermes"}},
		},
		actual: map[V2IngressOperationKind]Fingerprint{
			V2SourceImport: digestB, V2SourceEntrypoints: digestC,
		},
		applyErr: applyFault,
	}
	transition.options.Ingress = source
	authorizations := 0
	transition.options.VerifyAuthorization = func(plan PlanToken, authorization Authorization) bool {
		authorizations++
		return authorization == v2TestAuthorization(plan)
	}
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}

	interrupted, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || interrupted.Status != StatusRecovering ||
		interrupted.Code != CodeVerificationFailed || interrupted.Transaction == nil {
		t.Fatalf("interrupted source outcome = %#v, err=%v", interrupted, err)
	}
	if source.applies != 1 || source.actual[V2SourceImport] != digestC || authorizations != 1 {
		t.Fatalf("interrupted source state = %#v authorizations=%d", source, authorizations)
	}
	transaction := *interrupted.Transaction

	resume, err := transition.Inspect(context.Background(), goal)
	if err != nil || resume.Resume == nil || *resume.Resume != transaction {
		t.Fatalf("source resume inspection = %#v, err=%v", resume, err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: resume.Plan})
	if err != nil || outcome.Status != StatusReady || outcome.Transaction == nil ||
		*outcome.Transaction != transaction {
		t.Fatalf("source resume outcome = %#v, err=%v", outcome, err)
	}
	if source.applies != 2 || source.verifies != 2 ||
		source.actual[V2SourceEntrypoints] != digestA || authorizations != 1 {
		t.Fatalf("resumed source state = %#v authorizations=%d", source, authorizations)
	}
	for _, step := range source.steps {
		if step.Binding.Transaction != transaction || step.Binding.Plan != inspection.Plan {
			t.Fatalf("source step escaped outer binding: %#v", step)
		}
	}
}

func TestV2NewIngressImpactReplacesPreActivationJournalWithNewAuthorization(t *testing.T) {
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
	source := &v2IngressFixture{
		inspection: V2IngressInspection{
			Operations: []V2IngressOperation{{
				Kind: V2SourceImport, Decision: DecisionCanonicalize,
				Expected: digestB, Desired: digestC,
			}},
			Prospective: v2DiskSettingsView{configHome: configHome, yards: []string{"hermes"}},
		},
		actual: map[V2IngressOperationKind]Fingerprint{V2SourceImport: digestB},
	}
	transition.options.Ingress = source
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
	source.inspection.Operations = append(source.inspection.Operations, V2IngressOperation{
		Kind: V2SourceEntrypoints, Decision: DecisionCanonicalize,
		Expected: digestC, Desired: digestA,
	})
	source.actual[V2SourceEntrypoints] = digestC
	replacement, err := transition.Inspect(context.Background(), goal)
	if err != nil || replacement.Resume != nil || replacement.Outcome == nil ||
		replacement.Outcome.Status != StatusMigrationRequired ||
		replacement.Outcome.Code != CodeTransitionRequired || replacement.Plan == first.Plan {
		t.Fatalf("replacement inspection = %#v, err=%v", replacement, err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: replacement.Plan, Authorization: v2TestAuthorization(replacement.Plan),
	})
	if err != nil || outcome.Status != StatusReady || outcome.Transaction == nil ||
		*outcome.Transaction != "tx-test-002" ||
		source.actual[V2SourceEntrypoints] != digestA {
		t.Fatalf("replacement outcome = %#v source=%#v err=%v", outcome, source, err)
	}
}

func TestV2SourceWorkUsesTheJournaledImportSettingsEntrypointsOrder(t *testing.T) {
	transition, configHome, _ := v2TransitionFixture(t, func(point string) error {
		if point == "after-journal-authorized" {
			return errors.New("inspect journal order")
		}
		return nil
	})
	transition.options.Ingress = &v2IngressFixture{
		inspection: V2IngressInspection{
			Operations: []V2IngressOperation{
				{Kind: V2LegacyV1Import, Decision: DecisionCanonicalize, Expected: digestB, Desired: digestB, Static: digestC},
				{Kind: V2SourceImport, Decision: DecisionCanonicalize, Expected: digestB, Desired: digestC},
				{Kind: V2SourceEntrypoints, Decision: DecisionCanonicalize, Expected: digestC, Desired: digestA},
			},
			Prospective: v2DiskSettingsView{configHome: configHome, yards: []string{"hermes"}},
		},
		actual: map[V2IngressOperationKind]Fingerprint{
			V2LegacyV1Import: digestB, V2SourceImport: digestB, V2SourceEntrypoints: digestC,
		},
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
	store, err := NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(journal.Steps))
	for index, step := range journal.Steps {
		ids[index] = step.ID
	}
	want := []string{
		"legacy-v1.import",
		"source-install.import",
		"canonicalize-test-vms-settings-v2.hermes",
		"source-install.entrypoints",
		"canonicalize-test-vms-settings-v2.ledger",
	}
	if len(ids) < len(want) || !slices.Equal(ids[:len(want)], want) {
		t.Fatalf("journaled source/settings order = %v, want prefix %v", ids, want)
	}
}

func TestV2SourceDriftBeforeConvergeIsRejectedBeforeMutation(t *testing.T) {
	transition, configHome, _ := v2TransitionFixture(t, nil)
	source := &v2IngressFixture{
		inspection: V2IngressInspection{
			Operations: []V2IngressOperation{{
				Kind: V2SourceImport, Decision: DecisionCanonicalize,
				Expected: digestB, Desired: digestC,
			}},
			Decisions: []RedactedDecision{{
				Resource: "source-install.config", Scope: "source-install",
				Decision: DecisionCanonicalize, Result: "imported",
			}},
			Prospective: v2DiskSettingsView{configHome: configHome, yards: []string{"hermes"}},
		},
		actual: map[V2IngressOperationKind]Fingerprint{V2SourceImport: digestB},
	}
	transition.options.Ingress = source
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}

	source.inspection.Operations[0].Expected = digestD
	source.actual[V2SourceImport] = digestD
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusOperatorActionRequired || outcome.Code != CodePlanStale {
		t.Fatalf("source drift outcome = %#v, err=%v", outcome, err)
	}
	if source.applies != 0 {
		t.Fatalf("source drift was overwritten: %#v", source)
	}
	if _, err := os.Lstat(filepath.Join(configHome, "release-transition")); !os.IsNotExist(err) {
		t.Fatalf("source drift created outer journal: %v", err)
	}
}

func TestV2SourceThirdStateRequiresOperatorActionWithoutOverwrite(t *testing.T) {
	activeFault := true
	transition, configHome, _ := v2TransitionFixture(t, func(point string) error {
		if activeFault && point == "after-step-evidence" {
			return errors.New("stop before source apply")
		}
		return nil
	})
	source := &v2IngressFixture{
		inspection: V2IngressInspection{
			Operations: []V2IngressOperation{{
				Kind: V2SourceImport, Decision: DecisionCanonicalize,
				Expected: digestB, Desired: digestC,
			}},
			Decisions: []RedactedDecision{{
				Resource: "source-install.config", Scope: "source-install",
				Decision: DecisionCanonicalize, Result: "imported",
			}},
			Prospective: v2DiskSettingsView{configHome: configHome, yards: []string{"hermes"}},
		},
		actual: map[V2IngressOperationKind]Fingerprint{V2SourceImport: digestB},
	}
	transition.options.Ingress = source
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || interrupted.Status != StatusRecovering {
		t.Fatalf("source checkpoint outcome = %#v, err=%v", interrupted, err)
	}

	activeFault = false
	source.actual[V2SourceImport] = digestD
	resume, err := transition.Inspect(context.Background(), goal)
	if err != nil || resume.Resume == nil || resume.Outcome == nil ||
		resume.Outcome.Status != StatusOperatorActionRequired ||
		resume.Outcome.Code != CodeMigrationStale {
		t.Fatalf("source third-state inspection = %#v, err=%v", resume, err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{Plan: resume.Plan})
	if err != nil || outcome.Status != StatusOperatorActionRequired ||
		outcome.Code != CodeMigrationStale || source.applies != 0 ||
		source.actual[V2SourceImport] != digestD {
		t.Fatalf("source third-state outcome = %#v source=%#v err=%v", outcome, source, err)
	}
}

func TestV2SourceProspectiveSettingsAreClassifiedBeforeAuthorization(t *testing.T) {
	transition, configHome, settingsPath := v2TransitionFixture(t, nil)
	if err := os.Remove(settingsPath); err != nil {
		t.Fatal(err)
	}
	prospective := config.PersistentFileSnapshot{
		Exists:  true,
		Content: []byte("YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\nSSH_PORT=2224\n"),
	}
	source := &v2IngressFixture{inspection: V2IngressInspection{
		Operations: []V2IngressOperation{{
			Kind: V2SourceImport, Decision: DecisionCanonicalize,
			Expected: digestB, Desired: digestC,
		}},
		Decisions: []RedactedDecision{{
			Resource: "source-install.config", Scope: "source-install",
			Decision: DecisionCanonicalize, Result: "imported",
		}},
		Prospective: v2MemorySettingsView{
			yards: []string{"hermes"},
			snapshots: map[string]config.PersistentFileSnapshot{
				settingsPath: prospective,
			},
		},
	}}
	transition.options.Ingress = source

	inspection, err := transition.Inspect(context.Background(), Goal{
		Target: "release-a", Direction: DirectionActivateTarget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasV2Decision(inspection.Decisions, "setting.YARD_TEMPLATE", "yard.hermes", DecisionCanonicalize) ||
		!hasV2Decision(inspection.Decisions, "setting.NESTED_E2E_VMS", "yard.hermes", DecisionReset) {
		t.Fatalf("prospective settings decisions = %#v", inspection.Decisions)
	}
	if source.applies != 0 || source.verifies != 0 || source.observes != 0 {
		t.Fatalf("prospective inspection mutated source state: %#v", source)
	}
	if _, err := os.Lstat(settingsPath); !os.IsNotExist(err) {
		t.Fatalf("prospective inspection created destination: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(configHome, "release-transition")); !os.IsNotExist(err) {
		t.Fatalf("prospective inspection created transition metadata: %v", err)
	}
}

func TestV2SourceProspectiveSettingsRespectTheExistingYardBound(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	yards := make([]string, maximumSettingsV2Yards+1)
	for index := range yards {
		yards[index] = fmt.Sprintf("yard-%03d", index)
	}
	transition.options.Ingress = &v2IngressFixture{inspection: V2IngressInspection{
		Operations: []V2IngressOperation{{
			Kind: V2SourceImport, Decision: DecisionCanonicalize,
			Expected: digestB, Desired: digestC,
		}},
		Prospective: v2MemorySettingsView{yards: yards},
	}}

	_, err := transition.Inspect(context.Background(), Goal{
		Target: "release-a", Direction: DirectionActivateTarget,
	})
	if err == nil || !strings.Contains(err.Error(), "too many persistent yards") {
		t.Fatalf("unbounded prospective view error = %v", err)
	}
}

func hasV2Decision(
	decisions []RedactedDecision,
	resource string,
	scope string,
	decision Decision,
) bool {
	return slices.ContainsFunc(decisions, func(actual RedactedDecision) bool {
		return actual.Resource == resource && actual.Scope == scope && actual.Decision == decision
	})
}

type v2MemorySettingsView struct {
	yards     []string
	snapshots map[string]config.PersistentFileSnapshot
}

func (view v2MemorySettingsView) ListYards() ([]string, error) {
	return slices.Clone(view.yards), nil
}

func (view v2MemorySettingsView) ReadSnapshot(path string) (config.PersistentFileSnapshot, error) {
	return view.snapshots[path], nil
}

func TestV2IngressInspectionIsReadOnlyAndBindsSourceFactsIntoPlan(t *testing.T) {
	transition, configHome, _ := v2TransitionFixture(t, nil)
	source := &v2IngressFixture{inspection: V2IngressInspection{
		Operations: []V2IngressOperation{
			{Kind: V2SourceImport, Decision: DecisionCanonicalize, Expected: digestB, Desired: digestC},
			{Kind: V2SourceEntrypoints, Decision: DecisionCanonicalize, Expected: digestC, Desired: digestA},
		},
		Decisions: []RedactedDecision{
			{Resource: "source-install.config", Scope: "source-install", Decision: DecisionCanonicalize, Result: "imported"},
			{Resource: "source-install.entrypoints", Scope: "source-install", Decision: DecisionCanonicalize, Result: "stable-runtime"},
		},
		Prospective: v2DiskSettingsView{configHome: configHome, yards: []string{"hermes"}},
	}}
	transition.options.Ingress = source
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}

	first, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	if source.applies != 0 || source.verifies != 0 || source.observes != 0 {
		t.Fatalf("source inspection invoked mutation methods: %#v", source)
	}
	if !slices.Contains(first.Decisions, source.inspection.Decisions[0]) ||
		!slices.Contains(first.Decisions, source.inspection.Decisions[1]) {
		t.Fatalf("source decisions are absent from the combined inspection: %#v", first.Decisions)
	}

	source.inspection.Operations[0].Expected = digestA
	second, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	if second.Plan == first.Plan {
		t.Fatalf("source observation did not change the combined plan: %q", first.Plan)
	}
	if source.applies != 0 || source.verifies != 0 || source.observes != 0 {
		t.Fatalf("reinspection invoked source mutation methods: %#v", source)
	}
}

func TestV2IngressInspectionNormalizationHasOneCanonicalOrder(t *testing.T) {
	decisions := []RedactedDecision{
		{Resource: "source-install.config", Scope: "source-install", Decision: DecisionReset, Result: "reset"},
		{Resource: "source-install.config", Scope: "source-install", Decision: DecisionCanonicalize, Result: "imported"},
	}
	blockers := []Blocker{
		{Code: CodePlanStale, Resource: "source-install.config", Message: "state changed", Retry: "run yard update again"},
		{Code: CodeMigrationStale, Resource: "source-install.config", Message: "migration changed", Retry: "inspect source state"},
	}
	first, err := normalizeV2IngressInspection(V2IngressInspection{
		Decisions: slices.Clone(decisions),
		Blockers:  slices.Clone(blockers),
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(decisions)
	slices.Reverse(blockers)
	second, err := normalizeV2IngressInspection(V2IngressInspection{
		Decisions: decisions,
		Blockers:  blockers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first.Decisions, second.Decisions) ||
		!slices.Equal(first.Blockers, second.Blockers) {
		t.Fatalf("normalization depends on adapter order:\nfirst=%#v\nsecond=%#v", first, second)
	}
}

type v2DiskSettingsView struct {
	configHome string
	yards      []string
}

func (view v2DiskSettingsView) ListYards() ([]string, error) {
	return slices.Clone(view.yards), nil
}

func (view v2DiskSettingsView) ReadSnapshot(path string) (config.PersistentFileSnapshot, error) {
	return config.ReadPersistentFileSnapshot(view.configHome, path)
}

type v2SettingsBoundOwnerRegistration struct {
	configHome string
	path       string
	state      OwnerRegistrationState
	commits    int
}

func (owner *v2SettingsBoundOwnerRegistration) observation(
	view V2SettingsSnapshotView,
) (OwnerRegistrationObservation, error) {
	var snapshot config.PersistentFileSnapshot
	var err error
	if view != nil {
		snapshot, err = view.ReadSnapshot(owner.path)
	} else {
		snapshot, err = config.ReadPersistentFileSnapshot(owner.configHome, owner.path)
	}
	if err != nil {
		return OwnerRegistrationObservation{}, err
	}
	if !snapshot.Exists {
		return OwnerRegistrationObservation{State: OwnerRegistrationAbsent}, nil
	}
	state := OwnerRegistrationLegacyDirectory
	if owner.state == OwnerRegistrationCurrent {
		state = OwnerRegistrationCurrent
	}
	return OwnerRegistrationObservation{
		State: state, Registration: fingerprintPayload(snapshot.Content),
		Overrides: digestB, Controller: digestB, SharedImages: true,
	}, nil
}

func (owner *v2SettingsBoundOwnerRegistration) Prepare(
	_ context.Context,
	view V2SettingsSnapshotView,
) (OwnerRegistrationObservation, error) {
	return owner.observation(view)
}

func (owner *v2SettingsBoundOwnerRegistration) Observe(
	_ context.Context,
	before OwnerRegistrationObservation,
) (OwnerRegistrationProgress, error) {
	actual, err := owner.observation(nil)
	if err != nil {
		return "", err
	}
	if actual.Registration != before.Registration {
		return "", errors.New("settings-bound owner registration changed")
	}
	if owner.state == OwnerRegistrationCurrent {
		return OwnerRegistrationDesired, nil
	}
	return OwnerRegistrationExpected, nil
}

func (owner *v2SettingsBoundOwnerRegistration) Commit(
	ctx context.Context,
	before OwnerRegistrationObservation,
) error {
	if before.TerminalCleanup {
		return nil
	}
	if _, err := owner.Observe(ctx, before); err != nil {
		return err
	}
	owner.commits++
	owner.state = OwnerRegistrationCurrent
	return nil
}

type v2IngressFixture struct {
	inspection V2IngressInspection
	actual     map[V2IngressOperationKind]Fingerprint
	applyErr   error
	applyHook  func(V2IngressStep) error
	steps      []V2IngressStep
	applies    int
	verifies   int
	observes   int
}

func (fixture *v2IngressFixture) Inspect(
	context.Context,
	*V2IngressBinding,
) (V2IngressInspection, error) {
	return fixture.inspection, nil
}

func (fixture *v2IngressFixture) Observe(
	_ context.Context,
	step V2IngressStep,
) (Fingerprint, error) {
	fixture.observes++
	if actual := fixture.actual[step.Kind]; actual != "" {
		return actual, nil
	}
	return digestA, nil
}

func (fixture *v2IngressFixture) Apply(_ context.Context, step V2IngressStep) error {
	fixture.applies++
	fixture.steps = append(fixture.steps, step)
	actual := fixture.actual[step.Kind]
	if actual != step.Expected && actual != step.Desired {
		return fmt.Errorf("source apply saw third state %q", actual)
	}
	if fixture.applyHook != nil {
		if err := fixture.applyHook(step); err != nil {
			return err
		}
	}
	if actual == step.Expected {
		fixture.actual[step.Kind] = step.Desired
	}
	err := fixture.applyErr
	fixture.applyErr = nil
	return err
}

func (fixture *v2IngressFixture) Verify(_ context.Context, step V2IngressStep) error {
	fixture.verifies++
	fixture.steps = append(fixture.steps, step)
	if fixture.actual[step.Kind] != step.Desired {
		return fmt.Errorf("source verification did not reach desired state")
	}
	return nil
}

func v2SourceSettingsPath(configHome, yard string) string {
	return filepath.Join(configHome, "yards", yard, "config.env")
}
