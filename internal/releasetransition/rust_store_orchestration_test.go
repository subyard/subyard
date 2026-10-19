package releasetransition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/testkit"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Actual store mutations are exported separately so each bounded case can be
// consumed independently; the Rust test maintains no expected policy table.
func exportRustStoreOrchestration(t *testing.T, destination string) {
	t.Helper()
	type mutation struct {
		Name             string            `json:"name"`
		Operation        string            `json:"operation"`
		Arguments        any               `json:"arguments"`
		Before           map[string]string `json:"before"`
		BeforeDirs       []string          `json:"beforeDirs"`
		After            map[string]string `json:"after"`
		AfterDirs        []string          `json:"afterDirs"`
		Accepted         bool              `json:"accepted"`
		Result           string            `json:"result"`
		EquivalentGroups [][]string        `json:"equivalentGroups,omitempty"`
	}
	snapshot := func(root string) (map[string]string, []string) {
		files := map[string]string{}
		dirs := []string{}
		e := filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if rel == "." {
				return nil
			}
			if rel == "migrations" {
				return filepath.SkipDir
			}
			if d.IsDir() {
				dirs = append(dirs, rel)
				return nil
			}
			p, e := os.ReadFile(path)
			if e == nil {
				files[rel] = string(p)
			}
			return e
		})
		if e != nil {
			t.Fatal(e)
		}
		return files, dirs
	}
	cases := []string{}
	var equivalentGroups [][]string
	add := func(name, op string, args any, store *POSIXV2Store, run func() (string, error)) {
		before, bd := snapshot(store.configHome)
		result, err := run()
		after, ad := snapshot(store.configHome)
		if name == "owner-overwrite-foreign-completion" || name == "owner-overwrite-predecessor-archive" {
			if err == nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(bd, ad) {
				t.Fatalf("%s must refuse without publishing completion or overwriting journal: %v", name, err)
			}
		}
		value := mutation{name, op, args, before, bd, after, ad, err == nil, result, equivalentGroups}
		equivalentGroups = nil
		payload, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		if len(payload) > 64<<20 {
			t.Fatal("owning mutation corpus case exceeds fixed 64MiB bound")
		}
		name = fmt.Sprintf("%s.store-%02d.json", destination, len(cases))
		testkit.WriteFile(t, name, append(payload, '\n'), 0600)
		cases = append(cases, filepath.Base(name))
	}
	for _, kind := range []string{"malformed", "skipped-generation"} {
		store, r := lifecycleStoreFixture(t)
		frontier := RecoveryFrontier{SchemaVersion: 1, Replacement: r.Replacement, Generation: 1}
		pending := []byte("{}\n")
		if kind == "skipped-generation" {
			r.Generation = 2
			r.Successor.Transaction = lifecycleTransaction(t, r.Replacement, 2)
			r.BasePlan = PlanToken("plan-v1-" + strings.Repeat("b", 64))
			r.Reservation = digestA
			r.Successor.AuthorizationPlan, _ = BindRecoveryLifecyclePlan(r.BasePlan, r.Reservation)
			r.Successor.IntentDigest = bindJournalIntent(r.Successor.AuthorizationPlan, r.Successor.ResumePlan, r.Successor.ObservationScope, r.Successor.Steps)
			raw, _ := MarshalRecoveryReceipt(r)
			base := PlanToken("plan-v1-" + strings.Repeat("c", 64))
			plan, _ := BindRecoveryLifecyclePlan(base, fingerprintPayload(raw))
			terminal := RecoveryLifecycleRecord{SchemaVersion: 1, Receipt: r, Cancellation: &RecoveryCancellation{BasePlan: base, Plan: plan, AuthorizationDigest: digestC}}
			var e error
			pending, e = MarshalRecoveryFrontier(RecoveryFrontier{SchemaVersion: 1, Replacement: r.Replacement, Generation: 3, Cancellation: &terminal})
			if e != nil {
				t.Fatal(e)
			}
		}
		path := filepath.Join(store.configHome, filepath.Join(recoveryFrontierParts...), "."+string(r.Replacement.Transaction)+".json.pending")
		testkit.WriteFile(t, path, pending, 0600)
		add("frontier-refuses-"+kind, "frontier", frontier, store, func() (string, error) { return "", store.publishRecoveryFrontier(frontier) })
	}
	for _, kind := range []string{"valid", "changed-projection", "missing-predecessor-evidence", "foreign-completion", "predecessor-archive"} {
		candidate, _, _ := migrationOwnerFixture(t)
		goal := Goal{Target: "release-c", Direction: DirectionActivateTarget}
		inspection, e := candidate.Inspect(context.Background(), goal)
		if e != nil {
			t.Fatal(e)
		}
		outcome, e := candidate.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
		if e != nil || outcome.Status != StatusReady {
			t.Fatalf("complete owner fixture: %v %v", outcome, e)
		}
		store := candidate.store
		receipts, e := store.migrationOwnerReceipts()
		if e != nil {
			t.Fatal(e)
		}
		var receipt MigrationOwnerReceiptV1
		for _, r := range receipts {
			receipt = r
		}
		if kind == "changed-projection" {
			payload, _, e := MarshalLedgerV2(BaselineLedgerV2(candidate.registry), candidate.registry)
			if e != nil {
				t.Fatal(e)
			}
			testkit.WriteFile(t, filepath.Join(store.configHome, "release-transition/v2/ledger.json"), payload, 0600)
		}
		if kind == "missing-predecessor-evidence" {
			id := receipt.Predecessor.Steps[0].ID
			if e := os.Remove(filepath.Join(store.configHome, "release-transition/v2/transactions", string(receipt.Predecessor.Transaction), "evidence", id+".captured.json")); e != nil {
				t.Fatal(e)
			}
		}
		if kind == "foreign-completion" {
			if e := rustPrivateDirs(filepath.Join(store.configHome, filepath.Join(migrationOwnerCompletedParts...)), 0700); e != nil {
				t.Fatal(e)
			}
			testkit.WriteFile(t, filepath.Join(store.configHome, filepath.Join(migrationOwnerCompletedParts...), "foreign.json"), []byte("{}\n"), 0600)
		}
		if kind == "predecessor-archive" {
			testkit.WriteFile(t, filepath.Join(store.configHome, "release-transition/v2/transactions", string(receipt.Predecessor.Transaction), "superseded-journal.json"), []byte("{}\n"), 0600)
		}
		next := receipt.Predecessor
		next.Transaction = "tx-after-owner"
		next.Checkpoint = JournalComplete
		next.Steps = []JournalStep{}
		next.IntentDigest = bindJournalIntent(next.AuthorizationPlan, next.ResumePlan, next.ObservationScope, next.Steps)
		payload, e := MarshalJournal(next)
		if e != nil {
			t.Fatal(e)
		}
		expected, e := store.ReadCurrentJournal()
		if e != nil {
			t.Fatal(e)
		}
		add("owner-overwrite-"+kind, "replace", next, store, func() (string, error) { return "", store.CompareAndSwapCurrentJournal(expected, payload) })
	}

	for _, scenario := range []string{"resolve-v1-new", "resolve-v2-reuse", "resolve-v2-cancel", "cleanup-reserved-slot", "capture-at-capacity", "sticky-capture-failure"} {
		store, r := lifecycleStoreFixture(t)
		args := map[string]any{}
		if strings.HasPrefix(scenario, "resolve-") {
			base := r.BasePlan
			if scenario == "resolve-v2-cancel" {
				base = PlanToken("plan-v1-" + strings.Repeat("b", 64))
			}
			reservation, e := store.RecoveryReservation(r.Replacement, base)
			if e != nil {
				t.Fatal(e)
			}
			plan, e := BindRecoveryLifecyclePlan(base, reservation)
			if e != nil {
				t.Fatal(e)
			}
			args = map[string]any{"proposed": "recovery-v2-proposed", "request": r.Replacement, "base": base, "plan": plan, "grant": digestB}
			if scenario == "resolve-v1-new" {
				home := protectedConfigHome(t)
				store, _ = NewPOSIXV2Store(home)
				args["proposed"] = "recovery-v1-proposed"
				add(scenario, "resolve-v1", args, store, func() (string, error) {
					id, e := store.ResolveRecoveryTransaction("recovery-v1-proposed", r.Replacement, base)
					return string(id), e
				})
			} else {
				add(scenario, "resolve-v2", args, store, func() (string, error) {
					id, e := store.ResolveLifecycleRecoveryTransaction("recovery-v2-proposed", r.Replacement, base, plan, digestB)
					return string(id), e
				})
			}
			continue
		}
		completed := r.Successor
		completed.Checkpoint = JournalComplete
		selected, _ := store.ReadCurrentJournal()
		if e := store.CompareAndSwapCurrentJournal(selected, mustJournalPayload(t, completed)); e != nil {
			t.Fatal(e)
		}
		next := r.Predecessor
		next.Transaction = "tx-after-recovery"
		next.Checkpoint = JournalComplete
		args["next"] = next
		count := 32
		if scenario == "capture-at-capacity" {
			count = 512
		}
		if scenario == "sticky-capture-failure" {
			count = 0
			parent := filepath.Join(store.configHome, filepath.Join(recoveryArchiveParts...))
			if e := rustPrivateDirs(parent, 0700); e != nil {
				t.Fatal(e)
			}
			testkit.WriteFile(t, filepath.Join(parent, "foreign.json"), []byte("{}\n"), 0600)
		}
		archive := map[TransactionID]RecoveryLifecycleRecord{}
		for index := 0; index < count; index++ {
			history := lifecycleReceiptFixture(t)
			history.Predecessor.Transaction = TransactionID(fmt.Sprintf("tx-history-%03d", index))
			history.Replacement.Transaction = history.Predecessor.Transaction
			raw, _ := MarshalJournal(history.Predecessor)
			history.Replacement.Fingerprint = fingerprintPayload(raw)
			history.Successor.Transaction = lifecycleTransaction(t, history.Replacement, 1)
			terminal := history.Successor
			terminal.Checkpoint = JournalComplete
			witness := RecoveryLifecycleRecord{SchemaVersion: 1, Receipt: history, Completed: &terminal}
			payload, e := MarshalRecoveryLifecycleRecord(witness)
			if e != nil {
				t.Fatal(e)
			}
			if e := store.createImmutable(recoveryArchiveParts, string(terminal.Transaction)+".json", payload); e != nil {
				t.Fatal(e)
			}
			archive[terminal.Transaction] = witness
		}
		// Query actual Go pruning one candidate at a time. Which eligible archive
		// the bounded map iteration selects is incidental; deletion eligibility is not.
		candidates := make(map[TransactionID]RecoveryLifecycleRecord)
		originals := make(map[TransactionID][]byte)
		for id, witness := range archive {
			candidates[id] = witness
			payload, e := os.ReadFile(filepath.Join(store.configHome, filepath.Join(recoveryArchiveParts...), string(id)+".json"))
			if e != nil {
				t.Fatal(e)
			}
			originals[id] = payload
		}
		if e := store.pruneLifecycleArchive(candidates, 512, 0); e != nil {
			t.Fatal(e)
		}
		for id, payload := range originals {
			path := filepath.Join(store.configHome, filepath.Join(recoveryArchiveParts...), string(id)+".json")
			if _, e := os.Stat(path); os.IsNotExist(e) {
				relative, _ := filepath.Rel(store.configHome, path)
				equivalentGroups = append(equivalentGroups, []string{filepath.ToSlash(relative)})
				testkit.WriteFile(t, path, payload, 0600)
			}
		}
		if scenario == "cleanup-reserved-slot" {
			args["current"] = completed.Transaction
			add(scenario, "cleanup-recovery", args, store, func() (string, error) { return "", store.CleanupRecovery(completed.Transaction) })
		} else {
			operation := "replace"
			if scenario == "sticky-capture-failure" {
				operation = "replace-cleanup"
			}
			add(scenario, operation, args, store, func() (string, error) {
				before, e := store.ReadCurrentJournal()
				if e != nil {
					return "", e
				}
				e = store.CompareAndSwapCurrentJournal(before, mustJournalPayload(t, next))
				if e == nil && scenario == "sticky-capture-failure" {
					e = store.CleanupRecovery(next.Transaction)
				}
				return "", e
			})
		}
	}

	{
		home := protectedConfigHome(t)
		store, _ := NewPOSIXV2Store(home)
		request := recoveryReceiptFixture(t)
		if e := store.CompareAndSwapCurrentJournal(absentProtectedSnapshot(), mustJournalPayload(t, request.Predecessor)); e != nil {
			t.Fatal(e)
		}
		historical := map[TransactionID]RecoveryLifecycleRecord{}
		payloads := map[TransactionID][]byte{}
		for index := 0; index < 256; index++ {
			r := recoveryReceiptFixture(t)
			r.Predecessor.Transaction = TransactionID(fmt.Sprintf("tx-legacy-%03d", index))
			r.Replacement.Transaction = r.Predecessor.Transaction
			raw, _ := MarshalJournal(r.Predecessor)
			r.Replacement.Fingerprint = fingerprintPayload(raw)
			r.Successor.Transaction = TransactionID(fmt.Sprintf("recovery-v1-history-%03d", index))
			payload, e := MarshalRecoveryReceipt(r)
			if e != nil {
				t.Fatal(e)
			}
			if e := store.createImmutable(recoveryReceiptParts, string(r.Successor.Transaction)+".json", payload); e != nil {
				t.Fatal(e)
			}
			completed := r.Successor
			completed.Checkpoint = JournalComplete
			terminal := RecoveryLifecycleRecord{SchemaVersion: 1, Receipt: r, Completed: &completed}
			raw, e = MarshalRecoveryLifecycleRecord(terminal)
			if e != nil {
				t.Fatal(e)
			}
			if e := store.createImmutable(recoveryArchiveParts, string(r.Successor.Transaction)+".json", raw); e != nil {
				t.Fatal(e)
			}
			historical[r.Successor.Transaction] = terminal
			payloads[r.Successor.Transaction] = payload
		}
		for id, terminal := range historical {
			if e := store.retireClosedRecoveryEvidenceInArchive(map[TransactionID]RecoveryLifecycleRecord{id: terminal}); e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(home, filepath.Join(recoveryReceiptParts...), string(id)+".json")
			if _, e := os.Stat(path); os.IsNotExist(e) {
				rel, _ := filepath.Rel(home, path)
				equivalentGroups = append(equivalentGroups, []string{filepath.ToSlash(rel)})
				testkit.WriteFile(t, path, payloads[id], 0600)
			}
		}
		args := map[string]any{"proposed": "recovery-v1-proposed", "request": request.Replacement, "base": request.Successor.AuthorizationPlan, "plan": request.Successor.AuthorizationPlan, "grant": digestB}
		add("resolve-v1-at-capacity", "resolve-v1", args, store, func() (string, error) {
			id, e := store.ResolveRecoveryTransaction("recovery-v1-proposed", request.Replacement, request.Successor.AuthorizationPlan)
			return string(id), e
		})
	}
	for _, referenced := range []bool{false, true} {
		store, r := lifecycleStoreFixture(t)
		if e := os.Remove(filepath.Join(store.configHome, "release-transition/v2/journal.json")); e != nil {
			t.Fatal(e)
		}
		if e := os.Remove(filepath.Join(store.configHome, filepath.Join(recoveryLifecycleParts...), string(r.Successor.Transaction)+".json")); e != nil {
			t.Fatal(e)
		}
		var second RecoveryReceiptV1
		for generation := uint64(1); generation <= 4; generation++ {
			r.Generation = generation
			r.Successor.Transaction = lifecycleTransaction(t, r.Replacement, generation)
			r.Successor.AuthorizationPlan, _ = BindRecoveryLifecyclePlan(r.BasePlan, r.Reservation)
			r.Successor.IntentDigest = bindJournalIntent(r.Successor.AuthorizationPlan, r.Successor.ResumePlan, r.Successor.ObservationScope, r.Successor.Steps)
			raw, e := MarshalRecoveryReceipt(r)
			if e != nil {
				t.Fatal(e)
			}
			base := PlanToken(fmt.Sprintf("plan-v1-%064x", generation+128))
			plan, _ := BindRecoveryLifecyclePlan(base, fingerprintPayload(raw))
			terminal := RecoveryLifecycleRecord{SchemaVersion: 1, Receipt: r, Cancellation: &RecoveryCancellation{BasePlan: base, Plan: plan, AuthorizationDigest: digestB}}
			if e := store.createLifecycleRecord(terminal); e != nil {
				t.Fatal(e)
			}
			if e := store.publishRecoveryFrontier(RecoveryFrontier{SchemaVersion: 1, Replacement: r.Replacement, Generation: generation + 1, Cancellation: &terminal}); e != nil {
				t.Fatal(e)
			}
			if generation == 2 {
				second = r
			}
			r.Reservation = fingerprintPayload(raw)
		}
		if referenced {
			raw, _ := MarshalRecoveryReceipt(second)
			if e := store.createImmutable(recoveryLifecycleParts, string(second.Successor.Transaction)+".json", raw); e != nil {
				t.Fatal(e)
			}
		}
		add(fmt.Sprintf("compact-cancellations-referenced-%t", referenced), "compact", r.Replacement, store, func() (string, error) { return "", store.compactLifecycleCancellations(r.Replacement) })
	}
	for _, scenario := range []string{"ordinary-chain", "ordinary-reference", "ordinary-invalid", "ordinary-bound", "ordinary-empty-staging"} {
		home := protectedConfigHome(t)
		store, _ := NewPOSIXV2Store(home)
		count := 3
		if scenario == "ordinary-bound" {
			count = 34
		}
		for index := 0; index < count; index++ {
			id := fmt.Sprintf("tx-%03d", index)
			parent := filepath.Join(home, "release-transition/v2/transactions", id, "evidence")
			if e := rustPrivateDirs(parent, 0700); e != nil {
				t.Fatal(e)
			}
			testkit.WriteFile(t, filepath.Join(parent, "closed.json"), []byte("closed\n"), 0600)
		}
		if scenario == "ordinary-empty-staging" {
			testkit.WriteFile(t, filepath.Join(home, "release-transition/v2/transactions/tx-002/evidence/.step.captured.json.pending"), []byte{}, 0600)
		}
		prior := lifecycleReceiptFixture(t).Predecessor
		prior.Transaction = "tx-000"
		prior.Checkpoint = JournalComplete
		raw, _ := MarshalJournal(prior)
		archive := SupersededJournalRecord{SchemaVersion: 1, AuthorizationPlan: prior.AuthorizationPlan, Replacement: JournalReplacement{Transaction: prior.Transaction, Fingerprint: fingerprintPayload(raw), Reason: JournalReplacementPostActivationScopeV0111, SourceVersion: "0.11.1"}, Journal: prior}
		raw, e := MarshalSupersededJournal(archive)
		if e != nil {
			t.Fatal(e)
		}
		testkit.WriteFile(t, filepath.Join(home, "release-transition/v2/transactions/tx-001/superseded-journal.json"), raw, 0600)
		current := TransactionID("tx-current")
		if scenario == "ordinary-reference" {
			current = "tx-001"
		}
		if scenario == "ordinary-invalid" {
			if e := os.RemoveAll(filepath.Join(home, "release-transition/v2/transactions/tx-000")); e != nil {
				t.Fatal(e)
			}
		}
		add(scenario, "cleanup-transactions", current, store, func() (string, error) { return "", store.CleanupTransactions(current) })
	}

	for _, scenario := range []string{"source-complete", "source-retained", "source-no-current", "source-no-current-closed", "source-empty", "source-archive", "source-archive-no-current", "source-historical", "source-historical-retained", "source-historical-tampered-anchor"} {
		tr, store := completedSourceMetadataFixture(t)
		if strings.HasPrefix(scenario, "source-historical") {
			tr, store = rustHistoricalSourceFixture(t)
			if scenario == "source-historical-retained" {
				tr, store = interruptSourceMetadataActivation(t, tr, store)
			}
			if scenario == "source-historical-tampered-anchor" {
				raw, _, e := MarshalLedgerV2(BaselineLedgerV2(tr.registry), tr.registry)
				if e != nil {
					t.Fatal(e)
				}
				testkit.WriteFile(t, filepath.Join(store.configHome, "release-transition/v2/ledger.json"), raw, 0600)
			}
		}
		if scenario == "source-retained" {
			tr, store = interruptSourceMetadataActivation(t, tr, store)
		}
		if scenario == "source-archive" || scenario == "source-archive-no-current" {
			current, _ := store.ReadCurrentJournal()
			journal, e := ParseJournal(current.Payload)
			if e != nil {
				t.Fatal(e)
			}
			prior := journal
			prior.Transaction = "tx-archived-predecessor"
			prior.Steps = []JournalStep{}
			prior.Checkpoint = JournalReconciling
			prior.IntentDigest = bindJournalIntent(prior.AuthorizationPlan, prior.ResumePlan, prior.ObservationScope, prior.Steps)
			raw, _ := MarshalJournal(prior)
			archive := SupersededJournalRecord{SchemaVersion: 1, AuthorizationPlan: journal.AuthorizationPlan, Replacement: JournalReplacement{Transaction: prior.Transaction, Fingerprint: fingerprintPayload(raw), Reason: JournalReplacementPostActivationScopeV0111, SourceVersion: "0.11.1"}, Journal: prior}
			raw, e = MarshalSupersededJournal(archive)
			if e != nil {
				t.Fatal(e)
			}
			if e := rustPrivateDirs(filepath.Join(store.configHome, "release-transition/v2/transactions", string(prior.Transaction)), 0700); e != nil {
				t.Fatal(e)
			}
			testkit.WriteFile(t, filepath.Join(store.configHome, "release-transition/v2/transactions", string(journal.Transaction), "superseded-journal.json"), raw, 0600)
		}
		if scenario == "source-no-current-closed" {
			snapshot, _ := store.ReadCurrentJournal()
			prior, e := ParseJournal(snapshot.Payload)
			if e != nil {
				t.Fatal(e)
			}
			receipt := recoveryReceiptFixture(t)
			terminal := receipt.Successor
			terminal.Checkpoint = JournalComplete
			witness := RecoveryLifecycleRecord{SchemaVersion: 1, Receipt: receipt, Completed: &terminal}
			if e := store.createLifecycleRecord(witness); e != nil {
				t.Fatal(e)
			}
			archive := SupersededJournalRecord{SchemaVersion: 1, AuthorizationPlan: terminal.AuthorizationPlan, Replacement: JournalReplacement{Transaction: prior.Transaction, Fingerprint: snapshot.Fingerprint, Reason: JournalReplacementPostActivationScopeV0111, SourceVersion: "0.11.1"}, Journal: prior}
			raw, e := MarshalSupersededJournal(archive)
			if e != nil {
				t.Fatal(e)
			}
			parent := filepath.Join(store.configHome, "release-transition/v2/transactions", string(terminal.Transaction))
			if e := rustPrivateDirs(parent, 0700); e != nil {
				t.Fatal(e)
			}
			testkit.WriteFile(t, filepath.Join(parent, "superseded-journal.json"), raw, 0600)
		}
		if scenario == "source-no-current" || scenario == "source-no-current-closed" || scenario == "source-archive-no-current" {
			if e := os.Remove(filepath.Join(store.configHome, "release-transition/v2/journal.json")); e != nil {
				t.Fatal(e)
			}
		}
		if scenario == "source-empty" {
			home := protectedConfigHome(t)
			store, _ = NewPOSIXV2Store(home)
		}
		args := map[string]any{"registry": tr.registry, "digest": tr.registryDigest}
		add(scenario, "source", args, store, func() (string, error) {
			proof, e := store.InspectSourceMetadata(tr.registry, tr.registryDigest)
			if e != nil {
				return "", e
			}
			raw, _, e := MarshalLedgerV2(proof.Ledger.Ledger, tr.registry)
			return string(raw), e
		})
	}

	for _, scenario := range []string{"new", "reuse", "pending", "foreign-plan"} {
		home := protectedConfigHome(t)
		store, _ := NewPOSIXV2Store(home)
		prior := lifecycleReceiptFixture(t).Predecessor
		prior.Checkpoint = JournalComplete
		raw, _ := MarshalJournal(prior)
		replacement := JournalReplacement{Transaction: prior.Transaction, Fingerprint: fingerprintPayload(raw), Reason: JournalReplacementPostActivationScopeV0111, SourceVersion: "0.11.1"}
		plan := prior.AuthorizationPlan
		if e := rustPrivateDirs(filepath.Join(home, "release-transition/v2/transactions", string(prior.Transaction)), 0700); e != nil {
			t.Fatal(e)
		}
		if scenario != "new" {
			archive := SupersededJournalRecord{SchemaVersion: 1, AuthorizationPlan: plan, Replacement: replacement, Journal: prior}
			payload, e := MarshalSupersededJournal(archive)
			if e != nil {
				t.Fatal(e)
			}
			parent := filepath.Join(home, "release-transition/v2/transactions/tx-successor")
			if e := rustPrivateDirs(parent, 0700); e != nil {
				t.Fatal(e)
			}
			name := "superseded-journal.json"
			if scenario == "pending" {
				name = ".superseded-journal.json.pending"
			}
			testkit.WriteFile(t, filepath.Join(parent, name), payload, 0600)
		}
		if scenario == "foreign-plan" {
			plan = PlanToken("plan-v1-" + strings.Repeat("c", 64))
		}
		args := map[string]any{"proposed": "tx-proposed", "replacement": replacement, "plan": plan}
		add("superseded-"+scenario, "resolve-superseded", args, store, func() (string, error) {
			id, e := store.ResolveSupersededJournalTransaction("tx-proposed", replacement, plan)
			return string(id), e
		})
	}
	manifest, e := json.Marshal(cases)
	if e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, destination+".store.json", append(manifest, '\n'), 0600)
	t.Logf("exported %d actual Go store orchestration mutation cases", len(cases))
}

// The same authenticated historical cut constructed by the owning source test.
func rustHistoricalSourceFixture(t *testing.T) (*V2Transition, *POSIXV2Store) {
	t.Helper()
	tr, home, _ := v2TransitionFixture(t, nil)
	store, e := NewPOSIXV2Store(home)
	if e != nil {
		t.Fatal(e)
	}
	old, oldDigest := tr.registry, tr.registryDigest
	ledger := BaselineLedgerV2(old)
	path, e := old.PendingPath(ledger)
	if e != nil {
		t.Fatal(e)
	}
	for _, migration := range path {
		ledger, e = ledger.Advance(old, migration)
		if e != nil {
			t.Fatal(e)
		}
	}
	raw, _, e := MarshalLedgerV2(ledger, old)
	if e != nil {
		t.Fatal(e)
	}
	if e := store.CompareAndSwapLedger(absentProtectedSnapshot(), raw); e != nil {
		t.Fatal(e)
	}
	prior, e := store.ReadMigrationLedger(old, oldDigest)
	if e != nil {
		t.Fatal(e)
	}
	raw, e = os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if e != nil {
		t.Fatal(e)
	}
	registry, digest, e := ParseRegistryV2(raw, BuiltinCapabilityCatalog())
	if e != nil {
		t.Fatal(e)
	}
	if e := store.ConvertMigrationLedger(prior, old, oldDigest, registry, digest); e != nil {
		t.Fatal(e)
	}
	options := tr.options
	options.RegistryPayload = raw
	options.MigrationCheckpoint = true
	options.SourceMigrationCheckpoint = true
	tr, e = NewV2Transition(options)
	if e != nil {
		t.Fatal(e)
	}
	inspection, e := tr.Inspect(context.Background(), Goal{Target: "release-a", Direction: DirectionActivateTarget})
	if e != nil {
		t.Fatal(e)
	}
	outcome, e := tr.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if e != nil || outcome.Status != StatusReady {
		t.Fatalf("historical cut fixture: %v %v", outcome, e)
	}
	return tr, store
}

func rustPrivateDirs(path string, mode os.FileMode) error {
	missing := []string{}
	for cursor := path; ; cursor = filepath.Dir(cursor) {
		if _, e := os.Stat(cursor); e == nil {
			break
		}
		missing = append(missing, cursor)
		if cursor == filepath.Dir(cursor) {
			break
		}
	}
	if e := os.MkdirAll(path, mode); e != nil {
		return e
	}
	for _, cursor := range missing {
		if e := os.Chmod(cursor, mode); e != nil {
			return e
		}
	}
	return nil
}

// The supplied receipt cannot substitute for its missing durable publication.
// Use original fixture inodes: ProtectedInputs includes their metadata.
func TestRustOwnerDirectSourceProof(t *testing.T) {
	probe := os.Getenv("SUBYARD_RUST_SOURCE_PROBE")
	if probe == "" {
		t.Skip("opt-in Rust source proof comparison")
	}
	for _, scenario := range []string{"published", "initial-empty", "unbound-completion"} {
		t.Run(scenario, func(t *testing.T) {
			candidate, _, _ := migrationOwnerFixture(t)
			inspection, err := candidate.Inspect(context.Background(), Goal{Target: "release-c", Direction: DirectionActivateTarget})
			if err != nil {
				t.Fatal(err)
			}
			candidate.options.fault = func(point string) error {
				if point == "after-migration-owner-receipt" {
					return errors.New("direct source receipt cut")
				}
				return nil
			}
			candidate.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			receipts, err := candidate.store.migrationOwnerReceipts()
			if err != nil || len(receipts) != 1 {
				t.Fatalf("receipt cut: %v", err)
			}
			var receipt MigrationOwnerReceiptV1
			for _, value := range receipts {
				receipt = value
			}
			if scenario != "published" {
				path := filepath.Join(candidate.store.configHome, filepath.Join(migrationOwnerReceiptParts...), string(receipt.Successor.Transaction)+".json")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "unbound-completion" {
				terminal := receipt.Successor
				terminal.Checkpoint = JournalComplete
				parent := filepath.Join(candidate.store.configHome, filepath.Join(migrationOwnerCompletedParts...))
				if err := rustPrivateDirs(parent, 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(parent, string(terminal.Transaction)+".json"), mustJournalPayload(t, terminal), 0600)
			}
			before, err := candidate.store.sourceMetadataInventory()
			if err != nil {
				t.Fatal(err)
			}
			expected, goErr := candidate.store.sourceMetadataInventoryWithoutMigrationOwner()
			if scenario == "unbound-completion" {
				if goErr == nil {
					t.Fatal("Go admitted unbound completion")
				}
			} else if goErr != nil || expected != receipt.ProtectedInputs {
				t.Fatalf("Go source positive: %v", goErr)
			}
			raw, err := MarshalMigrationOwnerReceipt(receipt)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(probe, "owner-direct", candidate.store.configHome)
			cmd.Stdin = bytes.NewReader(raw)
			output, rustErr := cmd.CombinedOutput()
			if (rustErr == nil) != (goErr == nil) || rustErr == nil && strings.TrimSpace(string(output)) != string(expected) {
				t.Fatalf("direct Rust/Go source proof differs: Go %v Rust %v %s", goErr, rustErr, output)
			}
			after, err := candidate.store.sourceMetadataInventory()
			if err != nil || before != after {
				t.Fatalf("read-only proof changed protected inputs: %v", err)
			}
		})
	}
}
