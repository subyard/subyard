// Package rollback provides host-free closed-rollback protocol fixtures. It
// exercises R's public activation API; it does not supply a real future S.
package rollback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	rt "github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

type Inputs struct {
	ConfigHome, RuntimeRoot        string
	Releases                       rt.ReleasePair
	SourceArtifact, TargetArtifact rt.Fingerprint
	SourceRegistry, TargetRegistry []byte
}

type artifact struct {
	Path        string         `json:"path"`
	Fingerprint rt.Fingerprint `json:"fingerprint"`
}

func fingerprint(payload []byte) rt.Fingerprint {
	digest := sha256.Sum256(payload)
	return rt.Fingerprint(hex.EncodeToString(digest[:]))
}

// Seed closes a source-owned journal only after R's actual activation operation
// returns its native completion. Callers provide verified endpoint identities.
func Seed(t testing.TB, inputs Inputs) rt.JournalRecord {
	t.Helper()
	registry, digest, err := rt.ParseRegistryV2(inputs.TargetRegistry, rt.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	native := rt.BaselineLedgerV2(registry)
	pending, err := registry.PendingPath(native)
	if err != nil || len(pending) != 0 {
		t.Fatal("rollback fixture requires a complete baseline target registry")
	}
	store, err := rt.NewPOSIXV2Store(inputs.ConfigHome)
	if err != nil {
		t.Fatal(err)
	}
	payload, _, err := rt.MarshalLedgerV2(native, registry)
	if err != nil {
		t.Fatal(err)
	}
	absent, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapLedger(absent, payload); err != nil {
		t.Fatal(err)
	}
	state, err := store.ReadMigrationLedger(registry, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConvertMigrationLedger(state, registry, digest, registry, digest); err != nil {
		t.Fatal(err)
	}
	state, err = store.ReadMigrationLedger(registry, digest)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := rt.NewV2Transition(rt.V2Options{ConfigHome: inputs.ConfigHome, RegistryPayload: inputs.TargetRegistry, ArtifactDigest: inputs.TargetArtifact,
		Releases: inputs.Releases, Direction: rt.DirectionActivatePrevious, RollbackTarget: &rt.RollbackTarget{Version: "0.30.0", RegistryDigest: digest},
		ObserveLinks: func(context.Context) (rt.ReleaseLinks, error) {
			return rt.ReleaseLinks{Active: inputs.Releases.Target, Previous: &inputs.Releases.From}, nil
		},
		VerifyAuthorization: func(_ rt.PlanToken, authorization rt.Authorization) bool { return authorization == "fixture-grant" },
	})
	if err != nil {
		t.Fatal(err)
	}
	request := rt.RollbackTargetRequest{SchemaVersion: 3, Mode: "activate-under-v2", RuntimeRoot: inputs.RuntimeRoot, ConfigHome: inputs.ConfigHome, Releases: inputs.Releases,
		SourceArtifact: inputs.SourceArtifact, SourceRegistry: fingerprint(inputs.SourceRegistry), TargetArtifact: inputs.TargetArtifact, TargetRegistry: digest, Projection: state.ProjectionSnapshot.Fingerprint, NativeHistory: &native}
	inspection, err := transition.InspectRollbackTargetActivation(context.Background(), request)
	if err != nil || len(inspection.Blockers) != 0 {
		t.Fatalf("fixture target inspection: %+v, %v", inspection, err)
	}
	pin := fingerprint([]byte(inspection.Plan.Token))
	step := rt.JournalStep{ID: rt.RollbackTargetActivationStep, Migration: rt.RollbackTargetActivationStep, Resource: "activation.rollback-target-plan", Decision: rt.DecisionPreserve, Expected: pin, Desired: pin, Checkpoint: rt.StepVerified}
	journal := rt.JournalRecord{SchemaVersion: 2, Transaction: "tx-source-rollback", Releases: inputs.Releases, Goal: rt.Goal{Target: inputs.Releases.Target, Direction: rt.DirectionActivatePrevious},
		AuthorizationPlan: rt.PlanToken("plan-v1-" + string(fingerprint([]byte("source-plan")))), ResumePlan: rt.PlanToken("resume-v1-" + string(fingerprint([]byte("source-resume")))),
		ArtifactDigest: inputs.TargetArtifact, RegistryDigest: request.SourceRegistry, CatalogDigest: fingerprint([]byte("future-source-catalog")), ObservationScope: fingerprint([]byte("source-scope")), AuthorizationDigest: fingerprint([]byte("fixture-grant")), Checkpoint: rt.JournalReconciling}
	artifacts := []artifact{{"transaction/", fingerprint(nil)}, {"transaction/evidence/", fingerprint(nil)}}
	for _, phase := range []rt.EvidenceCheckpoint{rt.EvidenceCaptured, rt.EvidenceApplied, rt.EvidenceVerified} {
		evidence := rt.EvidenceRecord{SchemaVersion: 2, Transaction: journal.Transaction, Releases: journal.Releases, Step: step.ID, Expected: pin, Desired: pin, Observed: pin, Checkpoint: phase}
		value, err := rt.MarshalEvidence(evidence)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CreateCheckpointEvidence(journal.Transaction, step.ID, phase, value); err != nil {
			t.Fatal(err)
		}
		artifacts = append(artifacts, artifact{"transaction/evidence/" + step.ID + "." + string(phase) + ".json", fingerprint(value)})
		if phase == rt.EvidenceVerified {
			step.Evidence = &evidence
		}
	}
	journal.Steps = []rt.JournalStep{step}
	// Frozen V2 intent excludes the mutable checkpoint and embedded evidence.
	steps, err := json.Marshal([]struct {
		ID        string         `json:"id"`
		Migration string         `json:"migration"`
		Resource  string         `json:"resource"`
		Decision  rt.Decision    `json:"decision"`
		Expected  rt.Fingerprint `json:"expected"`
		Desired   rt.Fingerprint `json:"desired"`
	}{{step.ID, step.Migration, step.Resource, step.Decision, step.Expected, step.Desired}})
	if err != nil {
		t.Fatal(err)
	}

	intent, err := json.Marshal(struct {
		AuthorizationPlan rt.PlanToken    `json:"authorizationPlan"`
		ResumePlan        rt.PlanToken    `json:"resumePlan"`
		ObservationScope  rt.Fingerprint  `json:"observationScope"`
		Steps             json.RawMessage `json:"steps"`
	}{journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, steps})
	if err != nil {
		t.Fatal(err)
	}
	journal.IntentDigest = fingerprint(intent)
	payload, err = rt.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCurrentJournal(selected, payload); err != nil {
		t.Fatal(err)
	}
	selected, err = store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	request.Journal, request.Plan = selected.Fingerprint, &inspection.Plan
	request.Execution = &rt.Execution{Plan: inspection.Plan.Token, Authorization: "fixture-grant"}
	response, err := transition.ConvergeRollbackTargetActivation(context.Background(), request)
	if err != nil || response.Completion == nil {
		t.Fatalf("fixture target activation: %+v, %v", response, err)
	}
	journal.Checkpoint = rt.JournalComplete
	payload, err = rt.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCurrentJournal(selected, payload); err != nil {
		t.Fatal(err)
	}
	request.Journal, request.Plan, request.Execution = "", nil, nil
	// This is the canonical closure wire shape, deliberately independent of
	// the engine's private closure type and writer.
	closure := struct {
		SchemaVersion      int                         `json:"schemaVersion"`
		Transaction        rt.TransactionID            `json:"transaction"`
		Releases           rt.ReleasePair              `json:"releases"`
		SourceArtifact     rt.Fingerprint              `json:"sourceArtifact"`
		SourceRegistry     []byte                      `json:"sourceRegistry"`
		TargetRegistry     []byte                      `json:"targetRegistry"`
		RollbackJournal    []byte                      `json:"rollbackJournal"`
		ActivationRequest  rt.RollbackTargetRequest    `json:"activationRequest"`
		ActivationPlan     rt.RollbackActivationPlan   `json:"activationPlan"`
		Completion         rt.RollbackTargetCompletion `json:"completion"`
		ProjectionProducer rt.Fingerprint              `json:"projectionProducer,omitempty"`
		Artifacts          []artifact                  `json:"artifacts"`
	}{1, journal.Transaction, journal.Releases, inputs.SourceArtifact, inputs.SourceRegistry, inputs.TargetRegistry, payload, request, inspection.Plan, *response.Completion, "", artifacts}
	slices.SortFunc(artifacts, func(left, right artifact) int { return strings.Compare(left.Path, right.Path) })
	encoded, err := json.Marshal(closure)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(inputs.ConfigHome, "reconcile", "rollback", "v1", "closures")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, string(journal.Transaction)+".json"), append(encoded, '\n'), 0o600)
	if proof, err := store.InspectCompletedRollback(registry, digest, inputs.TargetArtifact); err != nil || proof == nil {
		t.Fatalf("fixture closed history: %+v, %v", proof, err)
	}
	return journal
}
