// Command release-transition-fixture creates host-free protected transition
// states through the public release-transition API for shell contract tests.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

type failingReconciler struct{ desired string }

func (failingReconciler) ID() string { return "shell-fixture" }

func (reconciler failingReconciler) Observe(
	context.Context,
	releasetransition.ReleasePair,
	releasetransition.ReleaseLinks,
) (releasetransition.V2ActivationObservation, error) {
	desired := "source-scope"
	if reconciler.desired != "" {
		desired = reconciler.desired
	}
	return releasetransition.V2ActivationObservation{
		Actual: fingerprint([]byte("before")), Desired: fingerprint([]byte(desired)),
	}, nil
}

func (failingReconciler) Reconcile(
	context.Context,
	releasetransition.ReleaseLinks,
) error {
	return errors.New("intentional post-activation fixture interruption")
}

type unavailableReconciler struct{}

func (unavailableReconciler) ID() string { return "unavailable" }

func (unavailableReconciler) Observe(
	context.Context,
	releasetransition.ReleasePair,
	releasetransition.ReleaseLinks,
) (releasetransition.V2ActivationObservation, error) {
	return releasetransition.V2ActivationObservation{}, errors.New("fixture unavailable")
}

func (unavailableReconciler) Reconcile(
	context.Context,
	releasetransition.ReleaseLinks,
) error {
	return errors.New("fixture unavailable")
}

type absentOwnerRegistration struct{}

func (absentOwnerRegistration) Prepare(
	context.Context,
	releasetransition.V2SettingsSnapshotView,
) (releasetransition.OwnerRegistrationObservation, error) {
	return releasetransition.OwnerRegistrationObservation{
		State: releasetransition.OwnerRegistrationAbsent,
	}, nil
}

func (absentOwnerRegistration) Observe(
	context.Context,
	releasetransition.OwnerRegistrationObservation,
) (releasetransition.OwnerRegistrationProgress, error) {
	return releasetransition.OwnerRegistrationExpected, nil
}

func (absentOwnerRegistration) Commit(
	context.Context,
	releasetransition.OwnerRegistrationObservation,
) error {
	return nil
}

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: release-transition-fixture seed|mutate|guard ...")
	}
	var err error
	switch os.Args[1] {
	case "seed":
		err = seed(os.Args[2:])
	case "mutate":
		err = mutate(os.Args[2:])
	case "guard":
		err = guard(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fatalf("%v", err)
	}
}

func seed(arguments []string) error {
	if len(arguments) != 4 {
		return errors.New("seed needs RUNTIME_ROOT CONFIG_HOME PREVIOUS SOURCE")
	}
	runtimeRoot, configHome := arguments[0], arguments[1]
	previous := releasetransition.ReleaseID(arguments[2])
	source := releasetransition.ReleaseID(arguments[3])
	sourceRoot := filepath.Join(runtimeRoot, "releases", string(source))
	registry, err := os.ReadFile(filepath.Join(sourceRoot, "config", "release-transition.json"))
	if err != nil {
		return err
	}
	manifest, err := os.ReadFile(filepath.Join(sourceRoot, "runtime-files.sha256"))
	if err != nil {
		return err
	}
	links, err := releasetransition.NewRuntimeLinkStore(runtimeRoot)
	if err != nil {
		return err
	}
	transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{
		ConfigHome: configHome,
		Releases: releasetransition.ReleasePair{
			From: previous, Target: source,
		},
		ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
			return links.Observe()
		},
		ActivateLinks: func(
			_ context.Context,
			pair releasetransition.ReleasePair,
		) (releasetransition.ReleaseLinks, error) {
			return links.Activate(pair)
		},
		Reconcilers:       []releasetransition.V2ActivationReconciler{failingReconciler{}},
		OwnerRegistration: absentOwnerRegistration{},
		RegistryPayload:   registry,
		ArtifactDigest:    fingerprint(manifest),
		CandidateVersion:  "0.11.1",
		NewTransactionID: func() releasetransition.TransactionID {
			return "tx-source-v0111"
		},
		VerifyAuthorization: func(
			releasetransition.PlanToken,
			releasetransition.Authorization,
		) bool {
			return true
		},
	})
	if err != nil {
		return err
	}
	goal := releasetransition.Goal{
		Target: source, Direction: releasetransition.DirectionActivateTarget,
	}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		return err
	}
	outcome, err := transition.Converge(context.Background(), releasetransition.Execution{
		Plan: inspection.Plan, Authorization: "shell-fixture-authorization",
	})
	if err != nil {
		return err
	}
	if outcome.Status != releasetransition.StatusRecovering || outcome.Transaction == nil ||
		*outcome.Transaction != "tx-source-v0111" {
		return fmt.Errorf("unexpected seeded outcome: %#v", outcome)
	}
	fmt.Printf("%s\n", *outcome.Transaction)
	return nil
}

func mutate(arguments []string) error {
	if len(arguments) != 3 {
		return errors.New("mutate needs RUNTIME_ROOT CONFIG_HOME MUTATION")
	}
	runtimeRoot, configHome, mutation := arguments[0], arguments[1], arguments[2]
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		return err
	}
	switch mutation {
	case "checkpoint":
		return rewriteJournal(store, func(journal *releasetransition.JournalRecord) error {
			journal.Checkpoint = releasetransition.JournalTargetActive
			return nil
		})
	case "direction":
		return rewriteJournal(store, func(journal *releasetransition.JournalRecord) error {
			journal.Goal.Direction = releasetransition.DirectionActivatePrevious
			return nil
		})
	case "source-ingress":
		return rewriteJournal(store, func(journal *releasetransition.JournalRecord) error {
			journal.SourceIngress = &releasetransition.SourceIngressRequest{
				SchemaVersion: releasetransition.SourceIngressRequestSchemaV1,
				Kind:          releasetransition.SourceIngressPreGoV1,
				SourceRoot:    "/fixture/source",
				DataHome:      "/fixture/data",
				BinDir:        "/fixture/bin",
				RC:            "/fixture/bashrc",
				LoginRC:       "/fixture/profile",
			}
			return nil
		})
	case "step-checkpoint":
		return rewriteJournal(store, func(journal *releasetransition.JournalRecord) error {
			if len(journal.Steps) == 0 || journal.Steps[0].Evidence == nil {
				return errors.New("journal has no evidence-bearing steps")
			}
			journal.Checkpoint = releasetransition.JournalMigrating
			journal.Steps[0].Checkpoint = releasetransition.StepApplied
			journal.Steps[0].Evidence.Checkpoint = releasetransition.EvidenceApplied
			return nil
		})
	case "step-resource":
		return rewriteJournal(store, func(journal *releasetransition.JournalRecord) error {
			if len(journal.Steps) == 0 {
				return errors.New("journal has no steps")
			}
			journal.Steps[0].Resource = "yard.fixture"
			return nil
		})
	case "embedded-evidence":
		return rewriteJournal(store, func(journal *releasetransition.JournalRecord) error {
			if len(journal.Steps) == 0 || journal.Steps[0].Evidence == nil {
				return errors.New("journal has no evidence-bearing steps")
			}
			journal.Steps[0].Evidence.Recovery = fingerprint([]byte("unexpected recovery"))
			return nil
		})
	case "evidence-captured":
		return removeEvidence(configHome, store, releasetransition.EvidenceCaptured)
	case "evidence-applied":
		return removeEvidence(configHome, store, releasetransition.EvidenceApplied)
	case "evidence-verified":
		return removeEvidence(configHome, store, releasetransition.EvidenceVerified)
	case "evidence-extra":
		return createExtraEvidence(store)
	case "ledger":
		return mutateLedger(store)
	case "recovery":
		return createRecovery(store)
	case "recovery-extra":
		return createExtraRecovery(store)
	case "registry":
		return rewriteJournal(store, func(journal *releasetransition.JournalRecord) error {
			journal.RegistryDigest = fingerprint([]byte("foreign registry"))
			return nil
		})
	case "catalog":
		return rewriteJournal(store, func(journal *releasetransition.JournalRecord) error {
			journal.CatalogDigest = fingerprint([]byte("foreign catalog"))
			return nil
		})
	case "replacement-transaction":
		return errors.New("replacement transaction is a process-request guard; use guard")
	case "replacement-fingerprint":
		return errors.New("replacement fingerprint is a process-request guard; use guard")
	case "transaction-artifact":
		return createTransactionArtifact(configHome, store)
	case "blocker-code":
		return rewriteJournal(store, func(journal *releasetransition.JournalRecord) error {
			journal.ObservationScope = emptyActivationScopeFingerprint()
			return nil
		})
	case "topology-current":
		return replaceRuntimeLink(runtimeRoot, "current", "releases/foreign-release")
	case "topology-previous":
		return replaceRuntimeLink(runtimeRoot, "previous", "")
	default:
		return fmt.Errorf("unknown mutation %q", mutation)
	}
}

func guard(arguments []string) error {
	if len(arguments) != 4 {
		return errors.New("guard needs RUNTIME_ROOT CONFIG_HOME CANDIDATE_RELEASE MUTATION")
	}
	runtimeRoot, configHome := arguments[0], arguments[1]
	candidate := releasetransition.ReleaseID(arguments[2])
	mutation := arguments[3]
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		return err
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return err
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil {
		return err
	}
	links, err := releasetransition.NewRuntimeLinkStore(runtimeRoot)
	if err != nil {
		return err
	}
	registry, err := os.ReadFile(filepath.Join(
		runtimeRoot, "releases", string(journal.Releases.Target), "config", "release-transition.json",
	))
	if err != nil {
		return err
	}

	if mutation == "blocker-code" {
		transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{
			ConfigHome: configHome,
			Releases:   journal.Releases,
			ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
				return links.Observe()
			},
			Reconcilers:       []releasetransition.V2ActivationReconciler{unavailableReconciler{}},
			OwnerRegistration: absentOwnerRegistration{},
			RegistryPayload:   registry,
			ArtifactDigest:    journal.ArtifactDigest,
			CandidateVersion:  "0.11.1",
			VerifyAuthorization: func(
				releasetransition.PlanToken,
				releasetransition.Authorization,
			) bool {
				return false
			},
		})
		if err != nil {
			return err
		}
		inspection, err := transition.Inspect(context.Background(), journal.Goal)
		if err != nil {
			return err
		}
		if inspection.Outcome == nil ||
			inspection.Outcome.Status != releasetransition.StatusOperatorActionRequired ||
			len(inspection.Blockers) != 1 ||
			inspection.Blockers[0].Code != releasetransition.CodeDependencyUnavailable ||
			inspection.Blockers[0].Resource != "activation.unavailable" {
			return fmt.Errorf("alternate blocker inspection is not exact: %#v", inspection)
		}
		return nil
	}

	candidateRoot := filepath.Join(runtimeRoot, "releases", string(candidate))
	candidateManifest, err := os.ReadFile(filepath.Join(candidateRoot, "runtime-files.sha256"))
	if err != nil {
		return err
	}
	replacement := &releasetransition.JournalReplacement{
		Transaction:   journal.Transaction,
		Fingerprint:   snapshot.Fingerprint,
		Reason:        releasetransition.JournalReplacementPostActivationScopeV0111,
		SourceVersion: "0.11.1",
	}
	switch mutation {
	case "replacement-transaction":
		replacement.Transaction = "tx-other-v0111"
	case "replacement-fingerprint":
		replacement.Fingerprint = fingerprint([]byte("foreign source journal"))
	default:
		return fmt.Errorf("unknown process guard %q", mutation)
	}
	previous := journal.Releases.From
	transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{
		ConfigHome: configHome,
		Releases: releasetransition.ReleasePair{
			From: journal.Releases.Target, Previous: &previous, Target: candidate,
		},
		ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
			return links.Observe()
		},
		Reconcilers: []releasetransition.V2ActivationReconciler{
			failingReconciler{desired: "candidate-scope"},
		},
		OwnerRegistration: absentOwnerRegistration{},
		RegistryPayload:   registry,
		ArtifactDigest:    fingerprint(candidateManifest),
		CandidateVersion:  "0.11.2",
		Replacement:       replacement,
		VerifyAuthorization: func(
			releasetransition.PlanToken,
			releasetransition.Authorization,
		) bool {
			return false
		},
	})
	if err != nil {
		return err
	}
	goal := releasetransition.Goal{
		Target: candidate, Direction: releasetransition.DirectionActivateTarget,
	}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		return err
	}
	if inspection.Outcome == nil ||
		inspection.Outcome.Status != releasetransition.StatusOperatorActionRequired ||
		inspection.Resume == nil || *inspection.Resume != journal.Transaction {
		return fmt.Errorf("corrupt replacement was not rejected exactly: %#v", inspection)
	}
	return nil
}

func rewriteJournal(
	store *releasetransition.POSIXV2Store,
	change func(*releasetransition.JournalRecord) error,
) error {
	unlock, err := store.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return err
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil {
		return err
	}
	if err := change(&journal); err != nil {
		return err
	}
	journal.IntentDigest = bindJournalIntent(journal)
	payload, err := releasetransition.MarshalJournal(journal)
	if err != nil {
		return err
	}
	return store.CompareAndSwapCurrentJournal(snapshot, payload)
}

func mutateLedger(store *releasetransition.POSIXV2Store) error {
	unlock, err := store.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	snapshot, err := store.ReadLedger()
	if err != nil {
		return err
	}
	payload := append(append([]byte(nil), snapshot.Payload...), '\n')
	return store.CompareAndSwapLedger(snapshot, payload)
}

func removeEvidence(
	configHome string,
	store *releasetransition.POSIXV2Store,
	checkpoint releasetransition.EvidenceCheckpoint,
) error {
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return err
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil {
		return err
	}
	if len(journal.Steps) == 0 {
		return errors.New("journal has no evidence-bearing steps")
	}
	path := filepath.Join(
		configHome, "release-transition", "v2", "transactions", string(journal.Transaction),
		"evidence", journal.Steps[0].ID+"."+string(checkpoint)+".json",
	)
	return os.Remove(path)
}

func createExtraEvidence(store *releasetransition.POSIXV2Store) error {
	journal, err := readJournal(store)
	if err != nil {
		return err
	}
	return store.CreateCheckpointEvidence(
		journal.Transaction, "unknown-ledger", releasetransition.EvidenceVerified,
		[]byte("unexpected evidence\n"),
	)
}

func createRecovery(store *releasetransition.POSIXV2Store) error {
	journal, err := readJournal(store)
	if err != nil {
		return err
	}
	if len(journal.Steps) == 0 {
		return errors.New("journal has no recovery-bearing steps")
	}
	return store.CreateRecovery(journal.Transaction, journal.Steps[0].ID, []byte("recovery\n"))
}

func createExtraRecovery(store *releasetransition.POSIXV2Store) error {
	journal, err := readJournal(store)
	if err != nil {
		return err
	}
	return store.CreateRecovery(journal.Transaction, "unknown-ledger", []byte("recovery\n"))
}

func createTransactionArtifact(
	configHome string,
	store *releasetransition.POSIXV2Store,
) error {
	journal, err := readJournal(store)
	if err != nil {
		return err
	}
	path := filepath.Join(
		configHome, "release-transition", "v2", "transactions", string(journal.Transaction),
		"unknown.json",
	)
	return os.WriteFile(path, []byte("unexpected artifact\n"), 0o600)
}

func replaceRuntimeLink(runtimeRoot, name, target string) error {
	path := filepath.Join(runtimeRoot, name)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if target == "" {
		return nil
	}
	return os.Symlink(target, path)
}

func emptyActivationScopeFingerprint() releasetransition.Fingerprint {
	payload, err := json.Marshal(struct {
		SchemaVersion         int      `json:"schemaVersion"`
		ActivationReconcilers []string `json:"activationReconcilers"`
		InheritedSettingIDs   []string `json:"inheritedSettingIds"`
	}{SchemaVersion: 1})
	if err != nil {
		panic(err)
	}
	return fingerprint(payload)
}

func readJournal(store *releasetransition.POSIXV2Store) (releasetransition.JournalRecord, error) {
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return releasetransition.JournalRecord{}, err
	}
	return releasetransition.ParseJournal(snapshot.Payload)
}

func bindJournalIntent(journal releasetransition.JournalRecord) releasetransition.Fingerprint {
	type intentStep struct {
		ID        string                        `json:"id"`
		Migration string                        `json:"migration"`
		Resource  string                        `json:"resource"`
		Decision  releasetransition.Decision    `json:"decision"`
		Expected  releasetransition.Fingerprint `json:"expected"`
		Desired   releasetransition.Fingerprint `json:"desired"`
	}
	bound := struct {
		AuthorizationPlan releasetransition.PlanToken   `json:"authorizationPlan"`
		ResumePlan        releasetransition.PlanToken   `json:"resumePlan"`
		ObservationScope  releasetransition.Fingerprint `json:"observationScope"`
		Steps             []intentStep                  `json:"steps"`
	}{
		AuthorizationPlan: journal.AuthorizationPlan,
		ResumePlan:        journal.ResumePlan,
		ObservationScope:  journal.ObservationScope,
		Steps:             make([]intentStep, len(journal.Steps)),
	}
	for index, step := range journal.Steps {
		bound.Steps[index] = intentStep{
			ID: step.ID, Migration: step.Migration, Resource: step.Resource,
			Decision: step.Decision, Expected: step.Expected, Desired: step.Desired,
		}
	}
	payload, err := json.Marshal(bound)
	if err != nil {
		panic(err)
	}
	return fingerprint(payload)
}

func fingerprint(payload []byte) releasetransition.Fingerprint {
	digest := sha256.Sum256(payload)
	return releasetransition.Fingerprint(fmt.Sprintf("%x", digest))
}

func fatalf(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, "release-transition-fixture: "+format+"\n", arguments...)
	os.Exit(1)
}
