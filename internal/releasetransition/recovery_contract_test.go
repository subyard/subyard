package releasetransition

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func recoveryReceiptFixture(t *testing.T) RecoveryReceiptV1 {
	t.Helper()
	before := validJournal(releasePair())
	before.Checkpoint, before.Steps = JournalReconciling, []JournalStep{}
	before.IntentDigest = bindJournalIntent(before.AuthorizationPlan, before.ResumePlan, before.ObservationScope, before.Steps)
	payload, err := MarshalJournal(before)
	if err != nil {
		t.Fatal(err)
	}
	links := ReleaseLinks{Active: before.Releases.Target, Previous: releaseIDPointer(before.Releases.From)}
	after := before
	after.Transaction = RecoveryTransactionPrefixV1 + "tx-002"
	after.Releases = ReleasePair{From: links.Active, Target: links.Active, Previous: links.Previous}
	after.AuthorizationPlan = PlanToken("plan-v1-" + strings.Repeat("c", 64))
	after.ResumePlan = PlanToken("resume-v1-" + strings.Repeat("d", 64))
	after.AuthorizationDigest = digestD
	after.ObservationScope = digestB
	after.Checkpoint = JournalAuthorized
	after.IntentDigest = bindJournalIntent(after.AuthorizationPlan, after.ResumePlan, after.ObservationScope, after.Steps)
	return RecoveryReceiptV1{SchemaVersion: RecoveryReceiptSchemaV1, Contract: ActivationOnlyRecoveryContractV1,
		Replacement: ActivationOnlyRecoveryRequest{before.Transaction, fingerprintPayload(payload)},
		Predecessor: before, Successor: after, Ledger: digestA, Links: links,
		Owner:       RecoveryOwner{after.Goal.Target, after.ArtifactDigest, after.RegistryDigest, after.CatalogDigest},
		NativePlans: []RecoveryNativePlan{{ID: "materialized-config", Binding: digestA, Actual: digestB, Desired: digestC}}}
}

func TestRecoveryReceiptPreservesCanonicalJournalsAndRequiresFreshBindings(t *testing.T) {
	receipt := recoveryReceiptFixture(t)
	payload, err := MarshalRecoveryReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRecoveryReceipt(payload)
	if err != nil {
		t.Fatal(err)
	}
	again, err := MarshalRecoveryReceipt(parsed)
	if err != nil || !bytes.Equal(payload, again) {
		t.Fatalf("canonical receipt changed: %v", err)
	}
	for _, checkpoint := range []JournalCheckpoint{JournalAuthorized, JournalMigrating, JournalActivationIntent, JournalTargetActive, JournalReconciling, JournalComplete} {
		current := receipt.Successor
		current.Checkpoint = checkpoint
		if err := receipt.MatchesSuccessor(current); err != nil {
			t.Fatalf("checkpoint %s: %v", checkpoint, err)
		}
	}
	for name, mutate := range map[string]func(*RecoveryReceiptV1){
		"wrong contract":       func(r *RecoveryReceiptV1) { r.Contract = "future" },
		"wrong schema":         func(r *RecoveryReceiptV1) { r.SchemaVersion++ },
		"wrong fingerprint":    func(r *RecoveryReceiptV1) { r.Replacement.Fingerprint = digestD },
		"non-reconciling":      func(r *RecoveryReceiptV1) { r.Predecessor.Checkpoint = JournalTargetActive },
		"unknown links":        func(r *RecoveryReceiptV1) { r.Links.Previous = nil },
		"foreign artifact":     func(r *RecoveryReceiptV1) { r.Owner.Artifact = digestC },
		"unchanged scope":      func(r *RecoveryReceiptV1) { r.Successor.ObservationScope = r.Predecessor.ObservationScope },
		"old grant":            func(r *RecoveryReceiptV1) { r.Successor.AuthorizationDigest = r.Predecessor.AuthorizationDigest },
		"unreserved successor": func(r *RecoveryReceiptV1) { r.Successor.Transaction = "tx-002" },
		"no native proof":      func(r *RecoveryReceiptV1) { r.NativePlans = nil },
		"false convergence":    func(r *RecoveryReceiptV1) { r.NativePlans[0].Converged = true },
	} {
		t.Run(name, func(t *testing.T) {
			r := receipt
			r.NativePlans = append([]RecoveryNativePlan(nil), receipt.NativePlans...)
			mutate(&r)
			if r.Validate() == nil {
				t.Fatal("unsafe receipt accepted")
			}
		})
	}
	for name, corrupted := range map[string][]byte{
		"outer unknown":  bytes.Replace(payload, []byte(`"contract":`), []byte(`"future":true,"contract":`), 1),
		"nested unknown": bytes.Replace(payload, []byte(`"links":{`), []byte(`"links":{"future":true,`), 1),
		"trailing":       append(append([]byte(nil), payload...), []byte(`{}`)...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRecoveryReceipt(corrupted); err == nil {
				t.Fatal("corrupt receipt accepted")
			}
		})
	}
	current := receipt.Successor
	current.AuthorizationDigest = digestB
	if receipt.MatchesSuccessor(current) == nil {
		t.Fatal("foreign current successor accepted")
	}
}

func TestRecoveryReceiptStoreResumesPublicationBoundariesWithoutJournalMutation(t *testing.T) {
	for _, point := range []string{"after-pending-fsync", "before-publish", "after-publish-before-dir-fsync"} {
		t.Run(point, func(t *testing.T) {
			configHome := protectedConfigHome(t)
			store, err := NewPOSIXV2Store(configHome)
			if err != nil {
				t.Fatal(err)
			}
			receipt := recoveryReceiptFixture(t)
			before, err := MarshalJournal(receipt.Predecessor)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.CompareAndSwapCurrentJournal(absentProtectedSnapshot(), before); err != nil {
				t.Fatal(err)
			}
			payload, err := MarshalRecoveryReceipt(receipt)
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("interrupted immutable receipt publication")
			store.fault = func(actual string) error {
				if actual == point {
					return injected
				}
				return nil
			}
			if err := store.CreateRecoveryReceipt(receipt.Successor.Transaction, payload); !errors.Is(err, injected) {
				t.Fatalf("publication fault: %v", err)
			}
			store.fault = nil
			transaction, err := store.ResolveRecoveryTransaction(RecoveryTransactionPrefixV1+"restart", receipt.Replacement, receipt.Successor.AuthorizationPlan)
			if err != nil || transaction != receipt.Successor.Transaction {
				t.Fatalf("restart resolver: %s %v", transaction, err)
			}
			publication, exists, err := store.ReadRecoveryReceiptForPublication(transaction)
			if err != nil || !exists || publication.Successor.AuthorizationDigest != receipt.Successor.AuthorizationDigest {
				t.Fatalf("restart lost immutable initial successor: %#v %v", publication, err)
			}
			if err := store.CreateRecoveryReceipt(transaction, payload); err != nil {
				t.Fatalf("receipt retry: %v", err)
			}
			journal, err := store.ReadCurrentJournal()
			if err != nil || !bytes.Equal(journal.Payload, before) {
				t.Fatalf("receipt mutation changed predecessor: %v", err)
			}
			if err := store.ValidateCurrentRecovery(receipt.Successor); err != nil {
				t.Fatal(err)
			}
			assertProtectedFile(t, filepath.Join(configHome, "release-transition", "recovery", "v1", "transactions", string(transaction)+".json"))
			other := receipt
			other.Successor.AuthorizationDigest = digestB
			otherPayload, err := MarshalRecoveryReceipt(other)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.CreateRecoveryReceipt(transaction, otherPayload); !errors.Is(err, ErrProtectedStoreExists) {
				t.Fatalf("conflicting immutable receipt: %v", err)
			}
		})
	}
}

func TestRecoveryReceiptStoreFailsClosedAndSurvivesLegacyCleanup(t *testing.T) {
	configHome := protectedConfigHome(t)
	store, err := NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	receipt := recoveryReceiptFixture(t)
	if err := store.ValidateCurrentRecovery(receipt.Successor); err == nil {
		t.Fatal("missing mandatory successor receipt accepted")
	}
	if err := store.ValidateCurrentRecovery(receipt.Predecessor); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := store.ReadRecoveryReceipt(receipt.Successor.Transaction); err != nil || snapshot.Exists {
		t.Fatalf("read missing receipt: %#v %v", snapshot, err)
	}
	payload, err := MarshalRecoveryReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRecoveryReceipt(receipt.Successor.Transaction, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveRecoveryTransaction(RecoveryTransactionPrefixV1+"other", receipt.Replacement, receipt.Predecessor.AuthorizationPlan); err == nil {
		t.Fatal("competing plan accepted")
	}
	if _, err := store.ResolveRecoveryTransaction(RecoveryTransactionPrefixV1+"other", ActivationOnlyRecoveryRequest{receipt.Successor.Transaction, digestA}, receipt.Successor.AuthorizationPlan); err == nil {
		t.Fatal("replacement chain accepted")
	}
	current := receipt.Successor
	current.Checkpoint = JournalComplete
	journal, err := MarshalJournal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCurrentJournal(absentProtectedSnapshot(), journal); err != nil {
		t.Fatal(err)
	}
	if err := store.CleanupTransactions(current.Transaction); err != nil {
		t.Fatal(err)
	}
	stored, err := store.ReadRecoveryReceipt(current.Transaction)
	if err != nil || !bytes.Equal(stored.Payload, payload) {
		t.Fatalf("V2 cleanup damaged recovery evidence: %v", err)
	}
	path := filepath.Join(configHome, "release-transition", "recovery", "v1", "transactions", "unknown")
	testkit.WriteFile(t, path, []byte("unknown"), 0o600)
	if _, err := store.ResolveRecoveryTransaction(RecoveryTransactionPrefixV1+"other", receipt.Replacement, receipt.Successor.AuthorizationPlan); err == nil {
		t.Fatal("foreign receipt entry accepted")
	}
	if err := store.ValidateCurrentRecovery(current); err == nil {
		t.Fatal("successor resume ignored ambiguous recovery evidence")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("failed read-only admission mutated foreign entry")
	}
}

func TestRecoveryProcessV2StrictShapesLeaveFrozenV1Unchanged(t *testing.T) {
	probe := RecoveryProcessRequest{SchemaVersion: ProcessRecoverySchemaV2, Mode: RecoveryProcessCapabilities}
	payload, err := MarshalRecoveryProcessRequest(probe)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRecoveryProcessRequest(payload); err != nil {
		t.Fatal(err)
	}
	var legacy ProcessRequest
	if err := legacy.UnmarshalJSON(payload); err == nil {
		t.Fatal("frozen V1 accepted the recovery V2 schema")
	}
	capabilities := ActivationOnlyRecoveryCapabilities()
	responsePayload, err := MarshalRecoveryProcessResponse(RecoveryProcessResponse{SchemaVersion: ProcessRecoverySchemaV2, Capabilities: &capabilities})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRecoveryProcessResponse(responsePayload); err != nil {
		t.Fatal(err)
	}
	lifecycleProbe := RecoveryProcessRequest{SchemaVersion: ProcessRecoverySchemaV2, Mode: RecoveryProcessLifecycleCapabilities}
	if _, err := MarshalRecoveryProcessRequest(lifecycleProbe); err != nil {
		t.Fatal(err)
	}
	lifecycleCapabilities := ActivationRecoveryLifecycleCapabilities()
	lifecyclePayload, err := MarshalRecoveryProcessResponse(RecoveryProcessResponse{SchemaVersion: ProcessRecoverySchemaV2, Capabilities: &lifecycleCapabilities})
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := ParseRecoveryProcessResponse(lifecyclePayload); err != nil || *parsed.Capabilities != lifecycleCapabilities {
		t.Fatalf("lifecycle negotiation: %#v %v", parsed, err)
	}
	receipt := recoveryReceiptFixture(t)
	request := RecoveryProcessRequest{SchemaVersion: ProcessRecoverySchemaV2, Mode: RecoveryProcessInspect,
		RuntimeRoot: "/runtime", ConfigHome: "/config", Yard: "default", Target: receipt.Owner.Release,
		Direction: DirectionActivateTarget, ArtifactDigest: receipt.Owner.Artifact, RegistryDigest: receipt.Owner.Registry,
		Recovery: &receipt.Replacement}
	payload, err = MarshalRecoveryProcessRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRecoveryProcessRequest(payload); err != nil {
		t.Fatal(err)
	}
	request.Contract = ActivationOnlyRecoveryContractV2
	if encoded, err := MarshalRecoveryProcessRequest(request); err != nil {
		t.Fatal(err)
	} else if parsed, err := ParseRecoveryProcessRequest(encoded); err != nil || parsed.TransitionRequest().RecoveryContract != request.Contract {
		t.Fatalf("explicit lifecycle contract: %#v %v", parsed, err)
	}
	request.Contract = "future-contract"
	if _, err := MarshalRecoveryProcessRequest(request); err == nil {
		t.Fatal("unknown lifecycle contract accepted")
	}
	for name, corrupted := range map[string][]byte{
		"V1 reason":                  bytes.Replace(payload, []byte(`"recovery":{`), []byte(`"recovery":{"reason":"post-activation-scope-v0.11.1",`), 1),
		"source ingress":             bytes.Replace(payload, []byte(`"mode":`), []byte(`"sourceIngress":{},"mode":`), 1),
		"rollback":                   bytes.Replace(payload, []byte(`"activate-target"`), []byte(`"activate-previous"`), 1),
		"inspect execution":          bytes.Replace(payload, []byte(`"mode":`), []byte(`"execution":{},"mode":`), 1),
		"missing converge execution": bytes.Replace(payload, []byte(`"inspect"`), []byte(`"converge"`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRecoveryProcessRequest(corrupted); err == nil {
				t.Fatal("unsafe recovery request accepted")
			}
		})
	}
	probe.ConfigHome = "/config"
	if _, err := MarshalRecoveryProcessRequest(probe); err == nil {
		t.Fatal("capability probe with transition inputs accepted")
	}
	for _, corrupted := range [][]byte{
		bytes.Replace(responsePayload, []byte(`"receiptSchema":1`), []byte(`"receiptSchema":2`), 1),
		bytes.Replace(responsePayload, []byte(`"contract":`), []byte(`"future":true,"contract":`), 1),
		append(append([]byte(nil), responsePayload...), []byte(`{}`)...),
	} {
		if _, err := ParseRecoveryProcessResponse(corrupted); err == nil {
			t.Fatal("unsupported capability response accepted")
		}
	}
}
