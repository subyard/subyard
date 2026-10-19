package releasetransition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Extend the same opt-in owning exporter; expected bytes and policy outcomes
// come from the current Go constructors/store, not a separately maintained table.
func exportRustOwnerRecoveryCorpus(t *testing.T, destination string) {
	t.Helper()
	type vector struct {
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		Input     string `json:"input"`
		Accepted  bool   `json:"accepted"`
		Canonical string `json:"canonical"`
		Token     string `json:"token"`
	}
	vectors := []vector{}
	encode := func(kind string, p []byte) ([]byte, string, error) {
		switch kind {
		case "receipt":
			v, e := ParseRecoveryReceipt(p)
			if e != nil {
				return nil, "", e
			}
			b, e := MarshalRecoveryReceipt(v)
			return b, "", e
		case "terminal":
			v, e := ParseRecoveryLifecycleRecord(p)
			if e != nil {
				return nil, "", e
			}
			b, e := MarshalRecoveryLifecycleRecord(v)
			return b, "", e
		case "frontier":
			v, e := ParseRecoveryFrontier(p)
			if e != nil {
				return nil, "", e
			}
			b, e := MarshalRecoveryFrontier(v)
			return b, "", e
		case "request2":
			v, e := ParseRecoveryProcessRequest(p)
			if e != nil {
				return nil, "", e
			}
			b, e := MarshalRecoveryProcessRequest(v)
			return b, "", e
		case "response2":
			v, e := ParseRecoveryProcessResponse(p)
			if e != nil {
				return nil, "", e
			}
			b, e := MarshalRecoveryProcessResponse(v)
			return b, "", e
		case "migrationOwner":
			v, e := ParseMigrationOwnerReceipt(p)
			if e != nil {
				return nil, "", e
			}
			b, e := MarshalMigrationOwnerReceipt(v)
			return b, "", e
		case "assessment":
			var v struct {
				Changed      bool     `json:"changed"`
				Rollback     bool     `json:"rollback"`
				Consequences []string `json:"consequences"`
			}
			if e := json.Unmarshal(p, &v); e != nil {
				return nil, "", e
			}
			policy, e := newV2ActionPolicy()
			if e != nil {
				return nil, "", e
			}
			a, e := assessV2Action(policy, v.Changed, v.Rollback, v.Consequences)
			b, _ := json.Marshal(a)
			return b, "", e
		case "plan":
			var v PlanFacts
			if e := json.Unmarshal(p, &v); e != nil {
				return nil, "", e
			}
			token, e := BindPlan(v)
			b, _ := json.Marshal(v)
			return b, string(token), e
		case "resume":
			var v ResumePlanFacts
			if e := json.Unmarshal(p, &v); e != nil {
				return nil, "", e
			}
			token, e := BindResumePlan(v)
			b, _ := json.Marshal(v)
			return b, string(token), e
		}
		panic(kind)
	}
	add := func(name, kind string, value any) {
		p, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		b, token, e := encode(kind, p)
		vectors = append(vectors, vector{name, kind, string(p), e == nil, string(b), token})
	}
	for _, r := range []RecoveryReceiptV1{recoveryReceiptFixture(t), lifecycleReceiptFixture(t)} {
		add(r.Contract, "receipt", r)
		complete := r.Successor
		complete.Checkpoint = JournalComplete
		add(r.Contract+"-completed", "terminal", RecoveryLifecycleRecord{SchemaVersion: 1, Receipt: r, Completed: &complete})
		complete = r.Predecessor
		complete.Checkpoint = JournalComplete
		add(r.Contract+"-predecessor", "terminal", RecoveryLifecycleRecord{SchemaVersion: 1, Receipt: r, PredecessorCompleted: &complete})
		if r.SchemaVersion == 2 {
			add("frontier-initial", "frontier", RecoveryFrontier{SchemaVersion: 1, Replacement: r.Replacement, Generation: 1})
			base := PlanToken("plan-v1-" + strings.Repeat("b", 64))
			raw, _ := MarshalRecoveryReceipt(r)
			plan, _ := BindRecoveryLifecyclePlan(base, fingerprintPayload(raw))
			cancel := RecoveryLifecycleRecord{SchemaVersion: 1, Receipt: r, Cancellation: &RecoveryCancellation{BasePlan: base, Plan: plan, AuthorizationDigest: digestB}}
			add("cancelled", "terminal", cancel)
			add("frontier-cancelled", "frontier", RecoveryFrontier{SchemaVersion: 1, Replacement: r.Replacement, Generation: 2, Cancellation: &cancel})
			cancel.Cancellation.Plan = r.Successor.AuthorizationPlan
			add("cancel-wrong-plan", "terminal", cancel)
		}
		r.Replacement.Fingerprint = digestC
		add(r.Contract+"-foreign-predecessor", "receipt", r)
	}
	for _, mode := range []RecoveryProcessMode{RecoveryProcessCapabilities, RecoveryProcessLifecycleCapabilities, RecoveryProcessRuntimeCapabilities, RecoveryProcessMigrationOwnerCapabilities} {
		add(string(mode), "request2", RecoveryProcessRequest{SchemaVersion: 2, Mode: mode})
		add(string(mode)+"-input", "request2", RecoveryProcessRequest{SchemaVersion: 2, Mode: mode, ConfigHome: "/tmp/fixture"})
	}

	for _, contract := range []string{"", ActivationOnlyRecoveryContractV1, ActivationOnlyRecoveryContractV2} {
		r := recoveryReceiptFixture(t)
		q := RecoveryProcessRequest{SchemaVersion: 2, Mode: RecoveryProcessInspect, Contract: contract, RuntimeRoot: "/runtime", ConfigHome: "/config", Yard: "default", Target: r.Owner.Release, Direction: DirectionActivateTarget, ArtifactDigest: r.Owner.Artifact, RegistryDigest: r.Owner.Registry, Recovery: &r.Replacement}
		add("inspect-"+contract, "request2", q)
		q.Mode = RecoveryProcessConverge
		q.Execution = &Execution{Plan: r.Successor.AuthorizationPlan, Authorization: "bounded fixture grant"}
		add("converge-"+contract, "request2", q)
		q.Execution = nil
		add("converge-no-execution-"+contract, "request2", q)
	}
	add("ready-outcome", "response2", RecoveryProcessResponse{SchemaVersion: 2, ActivationReconciliationOwned: true, Outcome: &Outcome{Status: StatusReady, ReachedGoal: true, Active: "release-b", Target: "release-b", Code: CodeReady, Message: "release is ready"}})
	for _, c := range []RecoveryCapabilities{ActivationOnlyRecoveryCapabilities(), ActivationRecoveryLifecycleCapabilities(), MigrationOwnerReplacementCapabilities()} {
		add(c.Contract, "response2", RecoveryProcessResponse{SchemaVersion: 2, Capabilities: &c})
	}
	add("runtime-owners", "response2", RecoveryProcessResponse{SchemaVersion: 2, ActivationOwners: []string{"core-guest-runtime", "host-power"}})
	for _, changed := range []bool{false, true} {
		for _, rollback := range []bool{false, true} {
			add(fmt.Sprintf("assessment-%t-%t", changed, rollback), "assessment", struct {
				Changed      bool     `json:"changed"`
				Rollback     bool     `json:"rollback"`
				Consequences []string `json:"consequences"`
			}{changed, rollback, []string{"refresh the bounded native owner"}})
		}
	}
	facts := basePlanFacts()
	add("plan", "plan", facts)
	invalidLinks := clonePlanFacts(facts)
	invalidLinks.Links.Active = ""
	add("plan-invalid-links", "plan", invalidLinks)

	token, _ := BindPlan(facts)
	add("resume", "resume", baseResumePlanFacts(token, "tx-fixture"))
	facts.RollbackTarget = &RollbackTarget{Version: "1.0.0-18446744073709551616"}
	facts.Goal.Direction = DirectionActivatePrevious
	add("semver-overflow", "plan", facts)
	candidate, _, _ := migrationOwnerFixture(t)
	goal := Goal{Target: "release-c", Direction: DirectionActivateTarget}
	inspection, e := candidate.Inspect(context.Background(), goal)
	if e != nil {
		t.Fatal(e)
	}
	candidate.options.fault = func(point string) error {
		if point == "after-migration-owner-receipt" {
			return errors.New("oracle receipt cut")
		}
		return nil
	}
	candidate.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	receipts, e := candidate.store.migrationOwnerReceipts()
	if e != nil || len(receipts) != 1 {
		t.Fatalf("receipt oracle: %v %v", receipts, e)
	}
	for _, r := range receipts {
		if probe := os.Getenv("SUBYARD_RUST_SOURCE_PROBE"); probe != "" {
			cmd := exec.Command(probe, "owner", candidate.store.configHome)
			output, e := cmd.CombinedOutput()
			if e != nil || strings.TrimSpace(string(output)) != string(r.ProtectedInputs) {
				t.Fatalf("Rust same-inode owner source proof: %v %s", e, output)
			}
		}
		add("migration-owner", "migrationOwner", r)
		add("migration-owner-inspect", "request2", RecoveryProcessRequest{SchemaVersion: 2, Mode: RecoveryProcessInspect, Contract: MigrationOwnerReplacementContractV1, RuntimeRoot: "/runtime", ConfigHome: "/config", Yard: "default", Target: r.Successor.Goal.Target, Direction: DirectionActivateTarget, ArtifactDigest: r.Successor.ArtifactDigest, RegistryDigest: r.Successor.RegistryDigest, MigrationOwner: &r.Replacement})

		r.Registry = nil
		add("migration-owner-null-registry", "migrationOwner", r)
	}

	seenKinds := map[string]bool{}
	for _, v := range append([]vector(nil), vectors...) {
		if !v.Accepted || seenKinds[v.Kind] || v.Kind == "plan" || v.Kind == "resume" || v.Kind == "assessment" {
			continue
		}
		seenKinds[v.Kind] = true
		var object map[string]json.RawMessage
		if e := json.Unmarshal([]byte(v.Input), &object); e != nil {
			t.Fatal(e)
		}
		for _, mutant := range []string{"schemaVersion", "unknown"} {
			changed := make(map[string]json.RawMessage)
			for k, value := range object {
				changed[k] = value
			}
			changed[mutant] = json.RawMessage(`999`)
			raw, _ := json.Marshal(changed)
			canonical, token, e := encode(v.Kind, raw)
			vectors = append(vectors, vector{v.Kind + "-bad-" + mutant, v.Kind, string(raw), e == nil, string(canonical), token})
		}
	}
	type policy struct {
		Name      string            `json:"name"`
		Operation string            `json:"operation"`
		Before    map[string]string `json:"before"`
		After     map[string]string `json:"after"`
		Accepted  bool              `json:"accepted"`
	}
	policies := []policy{}
	files := func(root string) map[string]string {
		out := map[string]string{}
		e := filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			if rel == "migrations/update.lock" {
				return nil
			}
			p, e := os.ReadFile(path)
			if e == nil {
				out[filepath.ToSlash(rel)] = string(p)
			}
			return e
		})
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	for _, scenario := range []string{"live", "terminal", "referenced", "cancelled", "pending-terminal", "conflicting-pair", "unknown-terminal"} {
		store, r := lifecycleStoreFixture(t)
		if scenario != "live" && scenario != "unknown-terminal" {
			complete := r.Successor
			complete.Checkpoint = JournalComplete
			terminal := RecoveryLifecycleRecord{SchemaVersion: 1, Receipt: r, Completed: &complete}
			if scenario == "cancelled" {
				base := PlanToken("plan-v1-" + strings.Repeat("b", 64))
				p, _ := MarshalRecoveryReceipt(r)
				plan, _ := BindRecoveryLifecyclePlan(base, fingerprintPayload(p))
				terminal.Completed = nil
				terminal.Cancellation = &RecoveryCancellation{BasePlan: base, Plan: plan, AuthorizationDigest: digestB}
				if e := store.publishRecoveryFrontier(RecoveryFrontier{SchemaVersion: 1, Replacement: r.Replacement, Generation: 2, Cancellation: &terminal}); e != nil {
					t.Fatal(e)
				}
			}
			if e := store.createLifecycleRecord(terminal); e != nil {
				t.Fatal(e)
			}
			if scenario == "pending-terminal" || scenario == "conflicting-pair" {
				path := filepath.Join(store.configHome, filepath.Join(recoveryArchiveParts...), string(r.Successor.Transaction)+".json")
				p, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				if scenario == "conflicting-pair" {
					p = []byte("{}\n")
				}
				if e := os.WriteFile(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".pending"), p, 0600); e != nil {
					t.Fatal(e)
				}
			}
		}
		if scenario == "unknown-terminal" {
			path := filepath.Join(store.configHome, filepath.Join(recoveryArchiveParts...))
			if e := os.MkdirAll(path, 0700); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(path, "foreign.json"), []byte("{}\n"), 0600); e != nil {
				t.Fatal(e)
			}
		}
		// Isolate the reference policy from completion-capture during journal CAS.
		if scenario != "referenced" {
			if e := os.Remove(filepath.Join(store.configHome, "release-transition/v2/journal.json")); e != nil {
				t.Fatal(e)
			}
		}
		before := files(store.configHome)
		err := store.retireClosedRecoveryEvidence()
		policies = append(policies, policy{scenario, "retire", before, files(store.configHome), err == nil})
	}

	for _, scenario := range []string{"cancel", "cancel-prepared", "cancel-partial-pending", "cancel-foreign-current"} {
		st, r := lifecycleStoreFixture(t)
		if scenario == "cancel-prepared" || scenario == "cancel-partial-pending" {
			raw, _ := MarshalJournal(r.Successor)
			if scenario == "cancel-partial-pending" {
				raw = raw[:len(raw)/2]
			}
			if e := os.WriteFile(filepath.Join(st.configHome, "release-transition/v2/.journal.json.pending"), raw, 0600); e != nil {
				t.Fatal(e)
			}
		}
		if scenario == "cancel-foreign-current" {
			current := r.Predecessor
			current.Transaction = "tx-foreign"
			raw, _ := MarshalJournal(current)
			if e := os.WriteFile(filepath.Join(st.configHome, "release-transition/v2/journal.json"), raw, 0600); e != nil {
				t.Fatal(e)
			}
		}
		before := files(st.configHome)
		snapshot, e := st.ReadCurrentJournal()
		if e != nil {
			t.Fatal(e)
		}
		base := PlanToken("plan-v1-" + strings.Repeat("b", 64))
		raw, _ := MarshalRecoveryReceipt(r)
		plan, _ := BindRecoveryLifecyclePlan(base, fingerprintPayload(raw))
		err := st.cancelLifecycleReceipt(r, snapshot, base, plan, digestB)
		policies = append(policies, policy{scenario, "cancel", before, files(st.configHome), err == nil})
	}

	for _, scenario := range []string{"prune-unreferenced", "prune-current", "prune-live", "prune-cancelled"} {
		home := protectedConfigHome(t)
		st, _ := NewPOSIXV2Store(home)
		for index := 0; index < 3; index++ {
			r := lifecycleReceiptFixture(t)
			r.Predecessor.Transaction = TransactionID(fmt.Sprintf("tx-archive-%d", index))
			r.Replacement.Transaction = r.Predecessor.Transaction
			raw, _ := MarshalJournal(r.Predecessor)
			r.Replacement.Fingerprint = fingerprintPayload(raw)
			r.Successor.Transaction = lifecycleTransaction(t, r.Replacement, 1)
			completed := r.Successor
			completed.Checkpoint = JournalComplete
			terminal := RecoveryLifecycleRecord{SchemaVersion: 1, Receipt: r, Completed: &completed}
			if index == 0 {
				switch scenario {
				case "prune-current":
					if e := st.CompareAndSwapCurrentJournal(absentProtectedSnapshot(), mustJournalPayload(t, completed)); e != nil {
						t.Fatal(e)
					}
				case "prune-live":
					if e := st.publishRecoveryFrontier(RecoveryFrontier{SchemaVersion: 1, Replacement: r.Replacement, Generation: 1}); e != nil {
						t.Fatal(e)
					}
					p, _ := MarshalRecoveryReceipt(r)
					if e := st.createImmutable(recoveryLifecycleParts, string(r.Successor.Transaction)+".json", p); e != nil {
						t.Fatal(e)
					}
				case "prune-cancelled":
					base := PlanToken("plan-v1-" + strings.Repeat("b", 64))
					raw, _ := MarshalRecoveryReceipt(r)
					plan, _ := BindRecoveryLifecyclePlan(base, fingerprintPayload(raw))
					terminal.Completed = nil
					terminal.Cancellation = &RecoveryCancellation{BasePlan: base, Plan: plan, AuthorizationDigest: digestB}
					if e := st.publishRecoveryFrontier(RecoveryFrontier{SchemaVersion: 1, Replacement: r.Replacement, Generation: 1}); e != nil {
						t.Fatal(e)
					}
					if e := st.publishRecoveryFrontier(RecoveryFrontier{SchemaVersion: 1, Replacement: r.Replacement, Generation: 2, Cancellation: &terminal}); e != nil {
						t.Fatal(e)
					}
				}
			}
			if e := st.createLifecycleRecord(terminal); e != nil {
				t.Fatal(e)
			}
		}
		before := files(home)
		err := st.pruneLifecycleHistory(32, 0)
		policies = append(policies, policy{scenario, "prune", before, files(home), err == nil})
	}

	for _, scenario := range []string{"resume", "resume-unrelated-corrupt", "resume-rewound", "resume-foreign-frontier"} {
		st, r := lifecycleStoreFixture(t)
		selected := r.Successor
		if scenario == "resume-rewound" {
			completed := selected
			completed.Checkpoint = JournalComplete
			if e := st.createLifecycleRecord(RecoveryLifecycleRecord{SchemaVersion: 1, Receipt: r, Completed: &completed}); e != nil {
				t.Fatal(e)
			}
		}
		raw, _ := MarshalJournal(selected)
		if e := os.WriteFile(filepath.Join(st.configHome, "release-transition/v2/journal.json"), raw, 0600); e != nil {
			t.Fatal(e)
		}
		if scenario == "resume-unrelated-corrupt" {
			path := filepath.Join(st.configHome, filepath.Join(recoveryArchiveParts...))
			if e := os.MkdirAll(path, 0700); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(path, "foreign.json"), []byte("{}\n"), 0600); e != nil {
				t.Fatal(e)
			}
		}
		if scenario == "resume-foreign-frontier" {
			frontier := RecoveryFrontier{SchemaVersion: 1, Replacement: r.Replacement, Generation: 1}
			frontier.Replacement.Fingerprint = digestC
			p, e := MarshalRecoveryFrontier(frontier)
			if e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(st.configHome, filepath.Join(recoveryFrontierParts...), string(r.Replacement.Transaction)+".json"), p, 0600); e != nil {
				t.Fatal(e)
			}
		}
		before := files(st.configHome)
		_, err := st.ValidatedCurrentRecovery(selected)
		policies = append(policies, policy{scenario, "resume", before, files(st.configHome), err == nil})
	}
	type graphCase struct {
		Name        string            `json:"name"`
		Before      map[string]string `json:"before"`
		Directories []string          `json:"directories"`
		Accepted    bool              `json:"accepted"`
		Selected    []TransactionID   `json:"selected"`
	}
	graphs := []graphCase{}
	for _, scenario := range []string{"unrelated", "chain", "pending-chain", "missing-predecessor", "cycle", "fanout", "nested-chain"} {
		home := protectedConfigHome(t)
		st, _ := NewPOSIXV2Store(home)
		ids := []string{"tx-a", "tx-b", "tx-c"}
		if scenario == "missing-predecessor" {
			ids = []string{"tx-a"}
		}
		directories := []string{}
		for _, id := range ids {
			rel := "release-transition/v2/transactions/" + id
			if e := os.MkdirAll(filepath.Join(home, rel), 0700); e != nil {
				t.Fatal(e)
			}
			directories = append(directories, rel)
		}
		edges := [][2]string{}
		switch scenario {
		case "chain", "pending-chain", "missing-predecessor":
			edges = append(edges, [2]string{"tx-a", "tx-b"})
		case "cycle":
			edges = append(edges, [2]string{"tx-a", "tx-b"}, [2]string{"tx-b", "tx-a"})
		case "fanout":
			edges = append(edges, [2]string{"tx-a", "tx-c"}, [2]string{"tx-b", "tx-c"})
		case "nested-chain":
			edges = append(edges, [2]string{"tx-a", "tx-b"}, [2]string{"tx-b", "tx-c"})
		}
		for _, edge := range edges {
			prior := lifecycleReceiptFixture(t).Predecessor
			prior.Transaction = TransactionID(edge[1])
			prior.Checkpoint = JournalComplete
			raw, _ := MarshalJournal(prior)
			archive := SupersededJournalRecord{SchemaVersion: 1, AuthorizationPlan: prior.AuthorizationPlan, Replacement: JournalReplacement{Transaction: prior.Transaction, Fingerprint: fingerprintPayload(raw), Reason: JournalReplacementPostActivationScopeV0111, SourceVersion: "0.11.1"}, Journal: prior}
			raw, e := MarshalSupersededJournal(archive)
			if e != nil {
				t.Fatal(e)
			}
			name := "superseded-journal.json"
			if scenario == "pending-chain" {
				name = ".superseded-journal.json.pending"
			}
			if e := os.WriteFile(filepath.Join(home, "release-transition/v2/transactions", edge[0], name), raw, 0600); e != nil {
				t.Fatal(e)
			}
		}
		graph, err := st.inspectTransactionGraph("tx-current", nil)
		selected := []TransactionID{}
		if err == nil && graph != nil {
			selected = graph.cleanupOrder(32)
		}
		graphs = append(graphs, graphCase{scenario, files(home), directories, err == nil, selected})
	}

	type journalCase struct {
		Facts         PlanFacts     `json:"facts"`
		Transaction   TransactionID `json:"transaction"`
		Authorization Authorization `json:"authorization"`
		Journal       string        `json:"journal"`
	}
	journals := []journalCase{}
	for _, empty := range []bool{false, true} {
		tr, _, _ := v2TransitionFixture(t, nil)
		f := basePlanFacts()
		f.CatalogDigest = tr.catalog.Digest()
		if empty {
			f.Intents = []PlannerStepIntent{}
			f.Decisions = nil
			f.Observations = nil
		}
		tr.options.Releases = f.Releases
		tr.options.ArtifactDigest = f.ArtifactDigest
		tr.registryDigest = f.RegistryDigest
		tr.options.NewTransactionID = func() TransactionID { return "tx-bound-plan" }
		token, e := BindPlan(f)
		if e != nil {
			t.Fatal(e)
		}
		grant := Authorization("confirmed bounded fixture")
		observation := v2Observation{goal: f.Goal, links: f.Links, assessment: f.Assessment, decisions: f.Decisions, intents: f.Intents, blockers: f.Blockers, observationScope: f.ObservationScope}
		journal, e := tr.newJournal(observation, token, grant)
		if e != nil {
			t.Fatal(e)
		}
		raw, e := MarshalJournal(journal)
		if e != nil {
			t.Fatal(e)
		}
		journals = append(journals, journalCase{f, "tx-bound-plan", grant, string(raw)})
	}
	type sourceCase struct {
		Name        string            `json:"name"`
		Before      map[string]string `json:"before"`
		Registry    string            `json:"registry"`
		Ledger      string            `json:"ledger"`
		Transaction string            `json:"transaction"`
		Accepted    bool              `json:"accepted"`
		Steps       []JournalStep     `json:"steps"`
	}
	sources := []sourceCase{}
	for _, withOwner := range []bool{false, true} {
		var owner V2OwnerRegistration
		if withOwner {
			owner = &v2TestOwnerRegistration{state: OwnerRegistrationLegacyDirectory}
		}
		tr, st := completedSourceMetadataFixtureWithOwner(t, owner)
		if probe := os.Getenv("SUBYARD_RUST_SOURCE_PROBE"); probe != "" {
			want, e := st.sourceMetadataInventory()
			if e != nil {
				t.Fatal(e)
			}
			output, e := exec.Command(probe, "metadata", st.configHome).CombinedOutput()
			if e != nil || strings.TrimSpace(string(output)) != string(want) {
				t.Fatalf("Rust same-inode metadata inventory: %v %s", e, output)
			}
		}
		state, e := st.ReadMigrationLedger(tr.registry, tr.registryDigest)
		if e != nil {
			t.Fatal(e)
		}
		registryBytes, _ := json.Marshal(tr.registry)
		ledgerBytes, _, _ := MarshalLedgerV2(state.Ledger, tr.registry)
		for _, mutant := range []string{"valid", "missing-sidecar", "unbound-extra", "bad-recovery"} {
			root := st.configHome
			before := files(root)
			if mutant == "missing-sidecar" {
				for name := range before {
					if strings.HasSuffix(name, ".captured.json") {
						delete(before, name)
						break
					}
				}
			}
			if mutant == "unbound-extra" {
				before["release-transition/v2/transactions/tx-test-001/evidence/foreign.json"] = "{}\n"
			}
			if mutant == "bad-recovery" {
				for name := range before {
					if strings.Contains(name, "/recovery/") && strings.HasSuffix(name, ".json") {
						before[name] = "{}\n"
						break
					}
				}
			}
			// Run the actual source proof on a private clone of the same protected bytes.
			clone := protectedConfigHome(t)
			for name, value := range before {
				path := filepath.Join(clone, filepath.FromSlash(name))
				if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(path, []byte(value), 0600); e != nil {
					t.Fatal(e)
				}
			}
			copied, e := NewPOSIXV2Store(clone)
			if e != nil {
				t.Fatal(e)
			}
			bound, err := copied.sourceRetainedTerminalEvidence("tx-test-001", tr.registry)
			if err == nil {
				err = copied.sourceTerminalBindings(bound, tr.registry, "", state)
			}
			sources = append(sources, sourceCase{fmt.Sprintf("source-owner-%t-%s", withOwner, mutant), before, string(registryBytes), string(ledgerBytes), "tx-test-001", err == nil, bound.Steps})
		}
	}
	corpus := struct {
		Vectors  []vector      `json:"vectors"`
		Policies []policy      `json:"policies"`
		Sources  []sourceCase  `json:"sources"`
		Graphs   []graphCase   `json:"graphs"`
		Journals []journalCase `json:"journals"`
	}{vectors, policies, sources, graphs, journals}
	p, e := json.Marshal(corpus)
	if e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(destination, append(p, '\n'), 0600); e != nil {
		t.Fatal(e)
	}
	exportRustStoreOrchestration(t, destination)
	t.Logf("exported %d recovery vectors and %d actual Go store policy outcomes (%d bytes)", len(vectors), len(policies), len(p)+1)
}
