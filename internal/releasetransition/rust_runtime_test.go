package releasetransition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Subyard/Subyard/internal/config"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The actual frozen reducer evaluates one immutable facts document. Rust uses
// these exact inputs, including invalid bindings, without a second live inspect.
func TestRustOwnerReducerFactsCorpus(t *testing.T) {
	destination := os.Getenv("SUBYARD_RUST_REDUCER_CORPUS")
	if destination == "" {
		t.Skip("opt-in frozen whole-outcome reducer corpus")
	}
	type vector struct {
		Facts   TransitionFacts `json:"facts"`
		Outcome Outcome         `json:"outcome"`
	}
	values := []vector{}
	add := func(f TransitionFacts) {
		// Invalid journals cannot cross the owning frozen wire hook. Binding and
		// observation errors remain independent reducer inputs below.
		if f.Journal != nil && f.Journal.Validate() != nil {
			return
		}
		values = append(values, vector{f, Evaluate(f)})
	}
	pair := releasePair()
	for _, links := range []ReleaseLinks{initialLinks(pair), activatedLinks(pair), {Active: pair.From, Previous: releasePtr(pair.Target)}, {Active: "foreign"}} {
		for _, fixed := range []bool{false, true} {
			add(TransitionFacts{Goal: targetGoal(pair), Releases: pair, Links: links, FixedPointVerified: fixed})
		}
		for _, checkpoint := range []JournalCheckpoint{JournalAuthorized, JournalMigrating, JournalActivationIntent, JournalTargetActive, JournalReconciling, JournalComplete} {
			for _, step := range []StepCheckpoint{StepIntent, StepEvidence, StepApplied, StepVerified} {
				for _, observed := range []Fingerprint{digestA, digestC, digestD, ""} {
					f := factsWithJournal(pair, links, checkpoint, step, observed)
					for _, fixed := range []bool{false, true} {
						f.FixedPointVerified = fixed
						add(f)
					}
				}
			}
		}
	}
	base := factsWithJournal(pair, initialLinks(pair), JournalAuthorized, StepIntent, digestA)
	for _, change := range []func(*TransitionFacts){
		func(f *TransitionFacts) { f.CurrentPlan = PlanToken("resume-v1-" + strings.Repeat("d", 64)) },
		func(f *TransitionFacts) { f.CurrentArtifactDigest = digestD }, func(f *TransitionFacts) { f.CurrentRegistryDigest = digestD },
		func(f *TransitionFacts) { f.CurrentCatalogDigest = digestD }, func(f *TransitionFacts) { f.CurrentAuthorizationDigest = digestD },
		func(f *TransitionFacts) {
			f.VerifiedAuthorizationPlan = PlanToken("plan-v1-" + strings.Repeat("d", 64))
		},
		func(f *TransitionFacts) { f.CurrentIntents = nil }, func(f *TransitionFacts) { f.Steps = nil },
		func(f *TransitionFacts) { f.Steps = append(f.Steps, f.Steps...) },
		func(f *TransitionFacts) { f.Steps = []StepObservation{{Step: "foreign", Fingerprint: digestA}} },
		func(f *TransitionFacts) { f.Goal.Target = "foreign" },
	} {
		raw, _ := json.Marshal(base)
		var f TransitionFacts
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		change(&f)
		add(f)
	}
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(destination, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Runtime metadata comes from the actual owning compiled catalog and shipped
// registry. Native/process checks consume these bytes; no second policy table.
func TestRustOwnerRuntimeFactsCorpus(t *testing.T) {
	destination := os.Getenv("SUBYARD_RUST_RUNTIME_CORPUS")
	if destination == "" {
		t.Skip("opt-in Rust runtime facts")
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalog := BuiltinCapabilityCatalog()
	if _, _, err = ParseRegistryV2(raw, catalog); err != nil {
		t.Fatal(err)
	}
	before := JournalRecord{SchemaVersion: 2, Transaction: "interrupted-activation", Goal: Goal{Target: "new", Direction: DirectionActivateTarget}, Releases: ReleasePair{From: "old", Target: "new"}, AuthorizationPlan: PlanToken("plan-v1-" + strings.Repeat("b", 64)), ResumePlan: PlanToken("resume-v1-" + strings.Repeat("c", 64)), ArtifactDigest: Fingerprint(strings.Repeat("a", 64)), RegistryDigest: fingerprintPayload(raw), CatalogDigest: catalog.Digest(), ObservationScope: Fingerprint(strings.Repeat("d", 64)), AuthorizationDigest: Fingerprint(strings.Repeat("e", 64)), Checkpoint: JournalReconciling}
	before.IntentDigest = bindJournalIntent(before.AuthorizationPlan, before.ResumePlan, before.ObservationScope, before.Steps)
	if err := before.Validate(); err != nil {
		t.Fatal(err)
	}
	value := struct {
		Registry       string                 `json:"registry"`
		Catalog        []CapabilityDescriptor `json:"catalog"`
		RecoveryBefore JournalRecord          `json:"recoveryBefore"`
	}{string(raw), catalog.descriptors, before}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(destination, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Each side consumes the same parsed immutable assignment facts. Expected policy
// comes from the owning Go function, never a second expected-results table.
func TestRustOwnerSettingsFactsCorpus(t *testing.T) {
	destination := os.Getenv("SUBYARD_RUST_SETTINGS_CORPUS")
	if destination == "" {
		t.Skip("opt-in same-observation settings corpus")
	}
	type assignment struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		Line    int    `json:"line"`
		Direct  bool   `json:"direct"`
		Dynamic bool   `json:"dynamic"`
	}
	type input struct {
		Yard              string       `json:"yard"`
		CanonicalExists   bool         `json:"canonicalExists"`
		FlatExists        bool         `json:"flatExists"`
		Assignments       []assignment `json:"assignments"`
		Inherited         []string     `json:"inherited"`
		TemplateSupported bool         `json:"templateSupported"`
		Access            string       `json:"access"`
		SSHPort           int          `json:"sshPort"`
		SSHGroup          string       `json:"sshGroup"`
		Sources           []int        `json:"sources"`
	}
	type edit struct {
		Setting string  `json:"setting"`
		Value   *string `json:"value"`
	}
	type result struct {
		Input     input              `json:"input"`
		Blocked   bool               `json:"blocked"`
		Edits     []edit             `json:"edits"`
		Decisions []RedactedDecision `json:"decisions"`
		Decision  string             `json:"decision"`
	}
	cases := []result{}
	for _, template := range []string{"", "YARD_TEMPLATE=\n", "YARD_TEMPLATE=e2e-vms\n", "YARD_TEMPLATE=test-vms\n", "YARD_TEMPLATE=custom\n", "YARD_TEMPLATE=e2e-vms\nYARD_TEMPLATE=test-vms\n", "YARD_TEMPLATE=$OTHER\n"} {
		for _, nested := range []string{"", "NESTED_E2E_VMS=0\n", "NESTED_E2E_VMS=1\n", "NESTED_E2E_VMS=2\n"} {
			for inheritedMask := 0; inheritedMask < 4; inheritedMask++ {
				for _, supported := range []bool{false, true} {
					content := []byte(template + nested)
					parsed, err := config.ParsePersistentAssignments("yards/default/config.env", content)
					if err != nil {
						t.Fatal(err)
					}
					inherited := map[string]struct{}{}
					names := []string{}
					if inheritedMask&1 != 0 {
						inherited["YARD_TEMPLATE"] = struct{}{}
						names = append(names, "YARD_TEMPLATE")
					}
					if inheritedMask&2 != 0 {
						inherited["NESTED_E2E_VMS"] = struct{}{}
						names = append(names, "NESTED_E2E_VMS")
					}
					facts := input{Yard: "default", CanonicalExists: true, Assignments: []assignment{}, Inherited: names, TemplateSupported: supported, Sources: []int{}}
					for _, a := range parsed {
						facts.Assignments = append(facts.Assignments, assignment{a.Name, a.Value, a.Line, a.Direct, a.Dynamic})
					}
					file, decisions, blocker := planSettingsV2Yard(settingsV2YardSnapshot{Yard: "default", Nested: config.PersistentFileSnapshot{Exists: true, Content: content}, TemplateSupported: supported}, inherited, Fingerprint(strings.Repeat("a", 64)))
					r := result{Input: facts, Blocked: blocker != nil, Edits: []edit{}, Decisions: decisions}
					if file != nil {
						r.Decision = string(file.Decision)
						for _, setting := range []string{"YARD_TEMPLATE", "NESTED_E2E_VMS"} {
							before, _ := config.ParsePersistentAssignments("before", content)
							after, _ := config.ParsePersistentAssignments("after", file.Desired)
							var b, a *string
							for _, v := range before {
								if v.Name == setting {
									x := v.Value
									b = &x
								}
							}
							for _, v := range after {
								if v.Name == setting {
									x := v.Value
									a = &x
								}
							}
							if (b == nil) != (a == nil) || (b != nil && a != nil && *b != *a) {
								r.Edits = append(r.Edits, edit{setting, a})
							}
						}
					}
					cases = append(cases, r)
				}
			}
		}
	}
	raw, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(destination, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRustOwnerLinksFactsCorpus(t *testing.T) {
	destination := os.Getenv("SUBYARD_RUST_LINKS_CORPUS")
	if destination == "" {
		t.Skip("opt-in owning runtime links corpus")
	}
	type sample struct {
		Initial  ReleaseLinks `json:"initial"`
		Pair     ReleasePair  `json:"pair"`
		Cut      string       `json:"cut"`
		Accepted bool         `json:"accepted"`
		After    ReleaseLinks `json:"after"`
		Resumed  ReleaseLinks `json:"resumed"`
	}
	cases := []sample{}
	for _, before := range []ReleaseLinks{{Active: "release-a"}, {Active: "release-a", Previous: releaseIDPointer("release-old")}, {Active: "release-a", Previous: releaseIDPointer("release-b")}, {Active: "foreign", Previous: releaseIDPointer("release-old")}, {Active: "release-b", Previous: releaseIDPointer("release-a")}} {
		for _, cut := range []string{"", "after-staged-link", "after-link-exchange"} {
			root := runtimeLinkFixture(t, string(before.Active), func() string {
				if before.Previous != nil {
					return string(*before.Previous)
				}
				return ""
			}(), "release-a", "release-b", "release-old", "foreign")
			store, err := NewRuntimeLinkStore(root)
			if err != nil {
				t.Fatal(err)
			}
			pair := ReleasePair{From: "release-a", Target: "release-b", Previous: releaseIDPointer("release-old")}
			if before.Previous == nil {
				pair.Previous = nil
			}
			if before.Previous != nil && *before.Previous == "release-b" {
				pair.Previous = releaseIDPointer("release-b")
			}
			store.fault = func(point string) error {
				if cut != "" && point == cut {
					return errors.New("fixture cut")
				}
				return nil
			}
			_, err = store.Activate(pair)
			after, observeErr := store.Observe()
			if observeErr != nil {
				t.Fatal(observeErr)
			}
			store.fault = nil
			resumed, resumeErr := store.Activate(pair)
			cases = append(cases, sample{before, pair, cut, err == nil, after, resumed})
			if resumeErr != nil {
				cases[len(cases)-1].Resumed = after
			}
		}
	}
	raw, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(destination, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The owning reducer consumes one immutable observation. No live Go/Rust
// reinspection is used to compare release status and frozen V1 presentation.
func TestRustOwnerInspectionFactsCorpus(t *testing.T) {
	destination := os.Getenv("SUBYARD_RUST_INSPECTION_CORPUS")
	if destination == "" {
		t.Skip("opt-in immutable inspection corpus")
	}
	type input struct {
		Facts      PlanFacts      `json:"facts"`
		Journal    *JournalRecord `json:"journal"`
		Completed  bool           `json:"completed"`
		Fixed      bool           `json:"fixed"`
		Conversion bool           `json:"conversion"`
		Consent    bool           `json:"consent"`
		Warnings   []string       `json:"warnings"`
	}
	type vector struct {
		Input      input      `json:"input"`
		Inspection Inspection `json:"inspection"`
	}
	values := []vector{}
	tr, _, _ := v2TransitionFixture(t, nil)
	ledger := BaselineLedgerV2(tr.registry)
	for _, m := range tr.registry.Migrations {
		d := ledger.Domains[m.Domain]
		d.Epoch = m.ToEpoch
		d.Applied = append(d.Applied, m.ID)
		ledger.Domains[m.Domain] = d
	}
	if err := tr.registry.ValidateLedger(ledger); err != nil {
		t.Fatal(err)
	}
	for _, checkpoint := range []JournalCheckpoint{"", JournalAuthorized, JournalReconciling, JournalComplete} {
		for _, fixed := range []bool{false, true} {
			for _, conversion := range []bool{false, true} {
				for _, consent := range []bool{false, true} {
					for _, blocked := range []bool{false, true} {
						f := basePlanFacts()
						f.Releases = tr.options.Releases
						f.Goal = Goal{Target: f.Releases.Target, Direction: DirectionActivateTarget}
						f.Links = ReleaseLinks{Active: f.Releases.Target}
						f.RegistryDigest = tr.registryDigest
						f.CatalogDigest = tr.catalog.Digest()
						f.ArtifactDigest = tr.options.ArtifactDigest
						f.Intents = []PlannerStepIntent{}
						f.Decisions = []RedactedDecision{{Resource: "activation.synthetic", Scope: "activation", Decision: DecisionCanonicalize, Result: "converged"}}
						f.Assessment, _ = assessV2Action(tr.policy, true, false, []string{"refresh synthetic"})
						observation := v2Observation{goal: f.Goal, links: f.Links, ledger: ledger, activationFixed: fixed, assessment: f.Assessment, decisions: f.Decisions, intents: f.Intents, observationScope: f.ObservationScope, checkpointConversion: conversion, checkpointConsent: consent, activationWarnings: []string{"synthetic warning", "synthetic warning"}}
						if blocked {
							observation.blockers = []Blocker{{Code: CodePlanStale, Resource: "transition.observation-scope", Message: "the authorized transition observation scope differs from this engine", Retry: "run yard update --check"}}
						}
						token, err := BindPlan(f)
						if err != nil {
							t.Fatal(err)
						}
						if checkpoint != "" {
							j, err := tr.newJournal(observation, token, "immutable fixture grant")
							if err != nil {
								t.Fatal(err)
							}
							j.Checkpoint = checkpoint
							observation.journal = &j
						}
						facts := tr.planFacts(observation)
						outcome := tr.inspectionOutcome(observation)
						expected := Inspection{Assessment: observation.assessment.Clone(), Decisions: observation.decisions, Blockers: observation.blockers, Outcome: &outcome}
						if observation.journal != nil && observation.journal.Checkpoint == JournalComplete && conversion && consent && tr.fixedPoint(observation) {
							transaction := observation.journal.Transaction
							expected.Plan = observation.journal.ResumePlan
							expected.Resume = &transaction
							outcome = v2RecoveringOutcome(observation.links, observation.goal.Target, &transaction, CodeRecoveryPending, "the authorized migration checkpoint publication can resume")
						} else {
							if outcome.Status == StatusMigrationRequired && tr.completedJournalMatches(observation) {
								outcome.Status = StatusRecovering
								outcome.Code = CodeRecoveryPending
								outcome.Transaction = transactionIDPointer(observation.journal.Transaction)
								outcome.Message = "activation repair requires a new plan and authorization; the completed transaction is history"
							}
							if observation.journal != nil && observation.journal.Checkpoint != JournalComplete {
								transaction := observation.journal.Transaction
								expected.Plan = observation.journal.ResumePlan
								expected.Resume = &transaction
							} else {
								expected.Plan, err = BindPlan(facts)
								if err != nil {
									t.Fatal(err)
								}
							}
						}
						values = append(values, vector{input{facts, observation.journal, tr.completedJournalMatches(observation), tr.fixedPoint(observation), conversion, consent, observation.activationWarnings}, expected})
					}
				}
			}
		}
	}
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(destination, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The failure reducer and observation consume the same fixed reconciler facts.
// The trace records real Observe calls, not a separately maintained policy table.
func TestRustOwnerActivationFailureCorpus(t *testing.T) {
	destination := os.Getenv("SUBYARD_RUST_ACTIVATION_FAILURE_CORPUS")
	if destination == "" {
		t.Skip("opt-in actual activation failure reducer")
	}
	type afterFact struct {
		Actual    string `json:"actual"`
		Converged bool   `json:"converged"`
	}
	type vector struct {
		Journal      JournalRecord `json:"journal"`
		Before       string        `json:"before"`
		After        *afterFact    `json:"after"`
		Links        ReleaseLinks  `json:"links"`
		Outcome      Outcome       `json:"outcome"`
		ObserveCalls int           `json:"observeCalls"`
	}
	rows := []vector{}
	for _, kind := range []string{"unchanged", "changed-unconverged", "changed-converged", "invalid", "unobservable"} {
		t.Run(kind, func(t *testing.T) {
			transition, _, _ := v2TransitionFixture(t, nil)
			transition.options.Reconcilers = []V2ActivationReconciler{v2NamedReconciler("post-error-runtime")}
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if err != nil || outcome.Status != StatusReady {
				t.Fatalf("prepare %v %v", outcome, err)
			}
			snapshot, err := transition.store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			journal, err := ParseJournal(snapshot.Payload)
			if err != nil {
				t.Fatal(err)
			}
			journal.Checkpoint = JournalReconciling
			raw, err := MarshalJournal(journal)
			if err != nil {
				t.Fatal(err)
			}
			if err = transition.store.CompareAndSwapCurrentJournal(snapshot, raw); err != nil {
				t.Fatal(err)
			}
			reconciler := &v2PostErrorReconciler{failed: true, post: V2ActivationObservation{Actual: digestB, Desired: digestA}}
			if kind == "changed-unconverged" {
				reconciler.post.Actual = digestC
			}
			if kind == "changed-converged" {
				reconciler.post.Actual = digestA
				reconciler.post.Converged = true
			}
			if kind == "invalid" {
				reconciler.post.Actual = ""
			}
			if kind == "unobservable" {
				reconciler.postErr = errors.New("private observation failure")
			}
			transition.options.Reconcilers = []V2ActivationReconciler{reconciler}
			links := ReleaseLinks{Active: "release-a"}
			out := transition.reduceActivationReconcilerFailure(context.Background(), journal, reconciler, reconciler.ID(), "reconcile", links, V2ActivationObservation{Actual: digestB, Desired: digestA}, errors.New("private original failure"))
			var after *afterFact
			if kind != "invalid" && kind != "unobservable" {
				after = &afterFact{string(reconciler.post.Actual), reconciler.post.Converged}
			}
			rows = append(rows, vector{journal, string(digestB), after, links, out, reconciler.postErrorObservations})
		})
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(destination, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRustOwnerMigrationEntryFixture(t *testing.T) {
	destination := os.Getenv("SUBYARD_RUST_MIGRATION_ENTRY_CORPUS")
	if destination == "" {
		t.Skip("opt-in authentic owner replacement fixture")
	}
	candidate, _, _ := migrationOwnerFixture(t)
	snapshot, err := candidate.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(snapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	journal.Releases = ReleasePair{From: "0.18.1-source", Target: "0.21.0-old"}
	journal.Goal.Target = journal.Releases.Target
	for i := range journal.Steps {
		if journal.Steps[i].Evidence != nil {
			journal.Steps[i].Evidence.Releases = journal.Releases
		}
	}
	raw, err := MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	base := filepath.Join(candidate.options.ConfigHome, "release-transition")
	err = filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(candidate.options.ConfigHome, path)
		if err != nil {
			return err
		}
		contents, err := os.ReadFile(path)
		if err == nil {
			files[relative] = contents
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	files["release-transition/v2/journal.json"] = raw
	lock, err := os.ReadFile(filepath.Join(candidate.options.ConfigHome, "migrations/update.lock"))
	if err != nil {
		t.Fatal(err)
	}
	files["migrations/update.lock"] = lock
	for path, contents := range files {
		if strings.Contains(path, "/evidence/") {
			e, err := ParseEvidence(contents)
			if err != nil {
				t.Fatal(err)
			}
			e.Releases = journal.Releases
			files[path], err = MarshalEvidence(e)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	value := struct {
		Files       map[string][]byte                `json:"files"`
		Replacement MigrationOwnerReplacementRequest `json:"replacement"`
	}{files, MigrationOwnerReplacementRequest{Transaction: journal.Transaction, Fingerprint: fingerprintPayload(raw)}}
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(destination, payload, 0600); err != nil {
		t.Fatal(err)
	}
}

// This is the actual owning Go operation, not a hand-maintained transition
// table. The immutable initial inputs and every durable journal publication
// are retained for replay against the Rust owner operation boundary.
type rustOperationTraceReconciler struct {
	id                     string
	before, ready, pending V2ActivationObservation
	transition             *V2Transition
	calls                  *[]string
	targets                []map[string]any
	converged              bool
}

func (r *rustOperationTraceReconciler) ID() string { return r.id }
func (r *rustOperationTraceReconciler) Observe(ctx context.Context, p ReleasePair, l ReleaseLinks) (V2ActivationObservation, error) {
	*r.calls = append(*r.calls, "component.observe."+r.id)
	state, err := r.transition.store.ReadMigrationLedger(r.transition.registry, r.transition.registryDigest)
	if err != nil {
		return V2ActivationObservation{}, err
	}
	path, err := r.transition.registry.PendingPath(state.Ledger)
	if err != nil {
		return V2ActivationObservation{}, err
	}
	value := r.before
	if r.converged {
		value = r.ready
	}
	if len(path) > 0 && r.pending.Desired != "" {
		value = r.pending
	}
	if !value.Converged {
		value.Consequences = []string{"refresh " + r.id}
	}
	return value, nil
}
func (r *rustOperationTraceReconciler) Reconcile(ctx context.Context, l ReleaseLinks) error {
	for _, target := range r.targets {
		if target["state"] == "stale" && !r.converged {
			*r.calls = append(*r.calls, "component.apply."+r.id+"."+target["yard"].(string))
		}
	}
	r.converged = true
	return nil
}

// Observation values originate in actual owning Go component constructors,
// exported by TestRustOwnerActivationFactsCorpus. The scripted native peer
// receives only their immutable component inputs, never these observations.
type rustOperationComponentCapture struct {
	Owner         string                  `json:"owner"`
	Targets       []map[string]any        `json:"targets"`
	Expected      V2ActivationObservation `json:"expected"`
	SourcePending bool                    `json:"sourcePending"`
}

type rustOperationOwner struct {
	inner  *v2TestOwnerRegistration
	calls  *[]string
	reads  *[]rustOperationOwnerRead
	strict bool
}

type rustOperationOwnerRead struct {
	Method      string                       `json:"method"`
	Before      OwnerRegistrationObservation `json:"before"`
	Token       string                       `json:"token"`
	Progress    OwnerRegistrationProgress    `json:"progress"`
	AfterEffect bool                         `json:"afterEffect"`
}

func (o rustOperationOwner) Prepare(ctx context.Context, view V2SettingsSnapshotView) (OwnerRegistrationObservation, error) {
	*o.calls = append(*o.calls, "owner.prepare")
	if o.strict && o.inner.commits > 0 && o.inner.cleanups == 0 {
		return OwnerRegistrationObservation{}, errors.New("original owner source was archived; observe the retained original preparation")
	}
	return o.inner.Prepare(ctx, view)
}
func (o rustOperationOwner) Observe(ctx context.Context, before OwnerRegistrationObservation) (OwnerRegistrationProgress, error) {
	*o.calls = append(*o.calls, "owner.observe")
	progress, err := o.inner.Observe(ctx, before)
	*o.reads = append(*o.reads, rustOperationOwnerRead{"observe", before, before.RecoveryToken, progress, o.inner.commits > 0})
	return progress, err
}
func (o rustOperationOwner) Commit(ctx context.Context, before OwnerRegistrationObservation) error {
	name := "owner.apply"
	if before.TerminalCleanup {
		name = "owner.cleanup"
	}
	*o.calls = append(*o.calls, name)
	return o.inner.Commit(ctx, before)
}

func rustOperationReadFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err == nil {
			files[filepath.ToSlash(relative)] = raw
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

type rustOperationPublicFailure struct{}

func (rustOperationPublicFailure) Error() string { return "private fixture detail" }
func (rustOperationPublicFailure) ActivationDiagnostic() (string, string) {
	return "the component fixture was interrupted", "run yard update"
}
func TestRustOwnerOperationTraceCorpus(t *testing.T) {
	destination := os.Getenv("SUBYARD_RUST_OPERATION_TRACE_CORPUS")
	if destination == "" {
		t.Skip("opt-in actual Go whole-operation trajectory")
	}
	capturePath := os.Getenv("SUBYARD_RUST_ACTIVATION_CORPUS")
	if capturePath == "" {
		t.Fatal("whole-operation replay requires owning component captures")
	}
	captureRaw, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	var captures []rustOperationComponentCapture
	if err = json.Unmarshal(captureRaw, &captures); err != nil {
		t.Fatal(err)
	}
	type row struct {
		Registry                 []byte                   `json:"registry"`
		Catalog                  []CapabilityDescriptor   `json:"catalog"`
		Candidate                map[string][]byte        `json:"candidate"`
		Name                     string                   `json:"name"`
		Initial                  PlanFacts                `json:"initial"`
		Files                    map[string][]byte        `json:"files"`
		FinalFiles               map[string][]byte        `json:"finalFiles"`
		Native                   any                      `json:"native"`
		Calls                    []string                 `json:"calls"`
		Journals                 []JournalRecord          `json:"journals"`
		Outcome                  Outcome                  `json:"outcome"`
		OwnerReads               []rustOperationOwnerRead `json:"ownerReads"`
		PostEffectPrepareRefused bool                     `json:"postEffectPrepareRefused"`
		PostEffectPrepareError   string                   `json:"postEffectPrepareError"`
	}
	rows := []row{}
	for _, kind := range []string{"success", "apply-error-after-effect", "failed-reobserve", "changed-final-observation", "cleanup-failure", "owner-cleanup-success", "owner-cleanup-failure", "owner-original-progress"} {
		t.Run(kind, func(t *testing.T) {
			calls := []string{}
			links := ReleaseLinks{Active: "release-a"}
			failObserve := false
			tr, home, _ := v2TransitionFixtureWithReleases(t, nil, ReleasePair{From: "release-a", Target: "release-b"}, links)
			// The process options carry a present empty array. Preserve that exact
			// frozen input rather than comparing it with a nil Go slice.
			tr.options.InheritedSettingIDs = []string{}
			owner := &v2TestOwnerRegistration{state: OwnerRegistrationAbsent}
			if strings.HasPrefix(kind, "owner-cleanup-") || kind == "owner-original-progress" {
				owner.state = OwnerRegistrationLegacyDirectory
			}
			if kind == "owner-cleanup-failure" {
				owner.cleanupErr = errors.New("private owner archive cleanup interruption")
			}
			ownerReads := []rustOperationOwnerRead{}
			tr.options.OwnerRegistration = rustOperationOwner{owner, &calls, &ownerReads, kind == "owner-original-progress"}
			targets := map[string][]map[string]any{}
			for _, id := range []string{"sample-runtime", "materialized-config", "core-guest-runtime", "key-tools-runtime", "test-vm-broker", "test-yard-route-consumers", "host-power"} {
				var before, ready, pending *rustOperationComponentCapture
				for i := range captures {
					c := &captures[i]
					if c.Owner != id {
						continue
					}
					if c.SourcePending {
						pending = c
						continue
					}
					if id == "sample-runtime" && !c.Expected.Converged && before == nil {
						before = c
					}
					if c.Expected.Converged && ready == nil {
						ready = c
					}
				}
				if ready == nil {
					t.Fatalf("missing owning capture %s", id)
				}
				if before == nil {
					before = ready
				}
				targetRaw, e := json.Marshal(before.Targets)
				if e != nil {
					t.Fatal(e)
				}
				var nativeTargets []map[string]any
				if e = json.Unmarshal(targetRaw, &nativeTargets); e != nil {
					t.Fatal(e)
				}
				for n, target := range nativeTargets {
					// Transport guards independently bind the exact immutable captured
					// component/source tuple; semantic reported fields remain intact.
					guardRaw, e := json.Marshal(struct {
						Owner  string
						Target map[string]any
						Source map[string][]byte
					}{id, before.Targets[n], rustOperationReadFiles(t, home)})
					if e != nil {
						t.Fatal(e)
					}
					target["source"] = string(fingerprintPayload(guardRaw))
					if target["yard"] == "" {
						target["yard"] = "default"
					}
					if target["project"] == "" {
						target["project"] = "host"
					}
					if target["instance"] == "" {
						target["instance"] = "host"
					}
					if target["instanceUuid"] == "" {
						target["instanceUuid"] = "captured-" + id
					}
					actual, _ := target["reportedActual"].(string)
					desired, _ := target["reportedDesired"].(string)
					if actual == "" {
						actual = string(before.Expected.Actual)
					}
					if desired == "" {
						desired = string(before.Expected.Desired)
					}
					target["actual"] = actual
					target["desired"] = desired
				}
				targets[id] = nativeTargets
				r := &rustOperationTraceReconciler{id: id, before: before.Expected, ready: ready.Expected, transition: tr, calls: &calls, targets: nativeTargets}
				if pending != nil {
					r.pending = pending.Expected
				}
				tr.options.Reconcilers = append(tr.options.Reconcilers, r)
			}
			tr.options.ObserveLinks = func(context.Context) (ReleaseLinks, error) {
				calls = append(calls, "links.observe")
				if failObserve {
					return ReleaseLinks{}, errors.New("private unavailable observation")
				}
				return links, nil
			}
			tr.options.ActivateLinks = func(_ context.Context, p ReleasePair) (ReleaseLinks, error) {
				calls = append(calls, "links.activate")
				links = activatedLinks(p)
				return links, nil
			}
			goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
			observation, e := tr.observe(context.Background(), goal)
			if e != nil {
				t.Fatal(e)
			}
			ownerObservation := owner.observation()
			// Activation names come from the captured owning Go component target
			// inventory. They remain separate from persistent migration yard names.
			activationYards := []string{}
			for _, capture := range captures {
				for _, target := range capture.Targets {
					if yard, ok := target["yard"].(string); ok && yard != "" {
						activationYards = append(activationYards, yard)
					}
				}
			}
			slices.Sort(activationYards)
			activationYards = slices.Compact(activationYards)
			item := row{Name: kind, Initial: tr.planFacts(observation), Files: map[string][]byte{}, Candidate: map[string][]byte{}, Journals: []JournalRecord{}, Registry: tr.options.RegistryPayload, Catalog: tr.catalog.Descriptors(), Native: map[string]any{"name": kind, "yards": []string{"hermes"}, "activationYards": activationYards, "owners": []string{"sample-runtime"}, "targets": targets, "owner": map[string]any{"state": ownerObservation.State, "registration": ownerObservation.Registration, "overrides": ownerObservation.Overrides, "controller": ownerObservation.Controller, "sharedImages": ownerObservation.SharedImages}}}
			if tr.options.CandidateConfigDir != "" {
				e = filepath.WalkDir(tr.options.CandidateConfigDir, func(p string, d os.DirEntry, e error) error {
					if e != nil {
						return e
					}
					if d.IsDir() {
						return nil
					}
					rel, e := filepath.Rel(tr.options.CandidateConfigDir, p)
					if e != nil {
						return e
					}
					raw, e := os.ReadFile(p)
					if e == nil {
						item.Candidate[rel] = raw
					}
					return e
				})
				if e != nil {
					t.Fatal(e)
				}
			}
			e = filepath.WalkDir(home, func(p string, d os.DirEntry, e error) error {
				if e != nil {
					return e
				}
				if d.IsDir() {
					return nil
				}
				rel, e := filepath.Rel(home, p)
				if e != nil {
					return e
				}
				b, e := os.ReadFile(p)
				if e == nil {
					item.Files[rel] = b
				}
				return e
			})
			if e != nil {
				t.Fatal(e)
			}
			inspection, e := tr.Inspect(context.Background(), goal)
			if e != nil {
				t.Fatal(e)
			}
			calls = nil
			ownerReads = nil
			last := []byte(nil)
			tr.store.fault = func(point string) error {
				if point == "after-publish-before-dir-fsync" {
					raw, e := os.ReadFile(filepath.Join(home, "release-transition/v2/journal.json"))
					if e == nil && !bytes.Equal(raw, last) {
						j, e := ParseJournal(raw)
						if e != nil {
							t.Fatal(e)
						}
						item.Journals = append(item.Journals, j)
						last = append([]byte(nil), raw...)
						calls = append(calls, "journal."+string(j.Checkpoint))
					}
				}
				return nil
			}
			tr.options.fault = func(point string) error {
				calls = append(calls, "checkpoint."+point)
				if point == "after-owner-registration-commit" && kind == "owner-original-progress" {
					_, err := tr.options.OwnerRegistration.Prepare(context.Background(), nil)
					if err == nil {
						t.Fatal("archived owner source unexpectedly permitted fresh preparation")
					}
					item.PostEffectPrepareRefused = true
					item.PostEffectPrepareError = err.Error()
				}
				if point == "after-settings-cas" && (kind == "apply-error-after-effect" || kind == "failed-reobserve") {
					failObserve = kind == "failed-reobserve"
					return rustOperationPublicFailure{}
				}
				if point == "after-journal-complete" && kind == "changed-final-observation" {
					links = ReleaseLinks{Active: "release-a"}
				}
				if point == "before-recovery-gc" && kind == "cleanup-failure" {
					return errors.New("private cleanup interruption")
				}
				return nil
			}
			item.Outcome, e = tr.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if e != nil {
				t.Fatal(e)
			}
			item.Calls = calls
			item.OwnerReads = ownerReads
			item.FinalFiles = rustOperationReadFiles(t, home)
			if strings.HasPrefix(kind, "owner-cleanup-") && owner.cleanups != 1 {
				t.Fatal("actual terminal owner cleanup did not execute")
			}
			if kind == "owner-original-progress" {
				observedAfterEffect := false
				for _, read := range ownerReads {
					if read.AfterEffect && read.Progress == OwnerRegistrationDesired && read.Before.State == OwnerRegistrationLegacyDirectory && read.Token != "" {
						observedAfterEffect = true
					}
				}
				if owner.commits != 1 || owner.cleanups != 1 || !observedAfterEffect || !item.PostEffectPrepareRefused || item.Outcome.Status != StatusReady {
					t.Fatal("original preparation progress contract was not exercised")
				}
			}
			if kind == "owner-cleanup-failure" && (item.Outcome.Status != StatusReady || len(item.Outcome.Warnings) != 1) {
				t.Fatal("owner cleanup failure lost Ready/pending")
			}
			if len(item.Journals) == 0 {
				t.Fatal("operation did not publish durable intent")
			}
			if kind == "failed-reobserve" && (item.Outcome.Active != "" || item.Outcome.Previous != nil) {
				t.Fatal("failed observation fabricated links")
			}
			if kind == "cleanup-failure" && (item.Outcome.Status != StatusReady || len(item.Outcome.Warnings) != 1) {
				t.Fatal("cleanup failure lost Ready")
			}
			if kind == "changed-final-observation" && item.Outcome.Status == StatusReady {
				t.Fatal("Complete fabricated Ready")
			}
			if kind == "apply-error-after-effect" && item.Outcome.Message != "the component fixture was interrupted" {
				t.Fatal("original public cause lost")
			}
			rows = append(rows, item)
		})
	}
	raw, e := json.Marshal(rows)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(destination, raw, 0600); e != nil {
		t.Fatal(e)
	}
}
