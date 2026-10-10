package releasetransition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestV2CheckpointConsentSurvivesPostActivationReplacement(t *testing.T) {
	fixture := newV2PostActivationRecoveryFixture(t)
	transition := fixture.transition
	transition.options.MigrationCheckpoint = true
	transition.options.ActivateLinks = func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
		*fixture.links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
		return *fixture.links, nil
	}
	inspection, err := transition.Inspect(context.Background(), fixture.goal)
	if err != nil || len(inspection.Blockers) != 0 {
		t.Fatalf("replacement inspection: %#v err=%v", inspection, err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusReady {
		t.Fatalf("replacement convergence: %#v err=%v", outcome, err)
	}
	checkpoint, err := transition.store.ReadMigrationCheckpoint()
	if err != nil || !checkpoint.Exists {
		t.Fatalf("replacement lost checkpoint consent: %#v err=%v", checkpoint, err)
	}
	final, err := transition.Inspect(context.Background(), fixture.goal)
	if err != nil || final.Outcome.Status != StatusReady || final.Assessment.Changed {
		t.Fatalf("replacement fixed point: %#v err=%v", final, err)
	}
}

func TestV2CheckpointLedgerStepsUseCompactAuthorityAndResume(t *testing.T) {
	for _, faultPoint := range []string{"", "after-settings-cas", "after-ledger-cas"} {
		name := faultPoint
		if name == "" {
			name = "complete"
		}
		t.Run(name, func(t *testing.T) {
			activeFault := faultPoint
			transition, _, _ := v2TransitionFixture(t, func(point string) error {
				if activeFault != "" && point == activeFault {
					return errors.New("interrupt checkpoint transition")
				}
				return nil
			})
			registry := transition.registry
			legacy := BaselineLedgerV2(registry)
			projectionPayload, _, err := MarshalLedgerV2(legacy, registry)
			if err != nil {
				t.Fatal(err)
			}
			missing, err := transition.store.ReadLedger()
			if err != nil {
				t.Fatal(err)
			}
			if err := transition.store.CompareAndSwapLedger(missing, projectionPayload); err != nil {
				t.Fatal(err)
			}
			digest := transition.registryDigest
			before, err := transition.store.ReadMigrationLedger(registry, digest)
			if err != nil {
				t.Fatal(err)
			}
			if err := transition.store.ConvertMigrationLedger(before, registry, digest, registry, digest); err != nil {
				t.Fatal(err)
			}
			transition.options.MigrationCheckpoint = true
			transition.options.SourceMigrationCheckpoint = true
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil || len(inspection.Blockers) != 0 {
				t.Fatalf("checkpoint inspection = %#v, err=%v", inspection, err)
			}
			outcome, err := transition.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			if faultPoint == "" {
				if err != nil || outcome.Status != StatusReady {
					t.Fatalf("checkpoint transition = %#v, err=%v", outcome, err)
				}
			} else {
				if err != nil || outcome.Status != StatusRecovering || outcome.Transaction == nil {
					t.Fatalf("interrupted checkpoint transition = %#v, err=%v", outcome, err)
				}
				journalSnapshot, readErr := transition.store.ReadCurrentJournal()
				journal, parseErr := ParseJournal(journalSnapshot.Payload)
				if readErr != nil || parseErr != nil || journal.Transaction != *outcome.Transaction {
					t.Fatalf("interrupted journal = %#v, read=%v parse=%v", journal, readErr, parseErr)
				}
				activeFault = ""
				resume, inspectErr := transition.Inspect(context.Background(), goal)
				if inspectErr != nil || resume.Resume == nil || *resume.Resume != journal.Transaction ||
					resume.Plan != journal.ResumePlan {
					t.Fatalf("resume inspection = %#v, err=%v", resume, inspectErr)
				}
				outcome, err = transition.Converge(context.Background(), Execution{Plan: resume.Plan})
				if err != nil || outcome.Status != StatusReady || outcome.Transaction == nil ||
					*outcome.Transaction != journal.Transaction {
					t.Fatalf("resumed transition = %#v, err=%v", outcome, err)
				}
			}
			actual, err := transition.store.ReadMigrationLedger(registry, digest)
			if err != nil || !actual.Checkpointed || actual.Ledger.Domains["settings"].Epoch != 2 ||
				actual.Ledger.Domains["owner-registration"].Epoch != 2 {
				t.Fatalf("checkpoint ledger = %#v, err=%v", actual, err)
			}
			if ledgerCheckpointHasSuffix(actual.Checkpoint) {
				t.Fatalf("ready checkpoint retained completed suffix: %#v", actual.Checkpoint.Domains)
			}
			legacyAfter, err := transition.store.ReadLedger()
			if err != nil || !bytes.Equal(legacyAfter.Payload, projectionPayload) {
				t.Fatalf("legacy projection changed: %q, err=%v", legacyAfter.Payload, err)
			}
			repeat, err := transition.Inspect(context.Background(), goal)
			if err != nil || repeat.Assessment.Changed || repeat.Outcome == nil || repeat.Outcome.Status != StatusReady {
				t.Fatalf("repeat checkpoint inspection = %#v, err=%v", repeat, err)
			}
		})
	}
}

func TestV2CheckpointSourceGuardSurvivesStaleIncompleteJournalReplacement(t *testing.T) {
	activeFault := "after-ledger-cas"
	transition, _, _ := v2TransitionFixture(t, func(point string) error {
		if activeFault != "" && point == activeFault {
			return errors.New("interrupt checkpoint transition")
		}
		return nil
	})
	registry, digest := transition.registry, transition.registryDigest
	projection, _, err := MarshalLedgerV2(BaselineLedgerV2(registry), registry)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := transition.store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := transition.store.CompareAndSwapLedger(missing, projection); err != nil {
		t.Fatal(err)
	}
	baseline, err := transition.store.ReadMigrationLedger(registry, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := transition.store.ConvertMigrationLedger(baseline, registry, digest, registry, digest); err != nil {
		t.Fatal(err)
	}
	transition.options.MigrationCheckpoint = true
	transition.options.SourceMigrationCheckpoint = true
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil || len(inspection.Blockers) != 0 {
		t.Fatalf("initial checkpoint inspection = %#v, err=%v", inspection, err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
	})
	if err != nil || outcome.Status != StatusRecovering || outcome.Transaction == nil {
		t.Fatalf("interrupted checkpoint transition = %#v, err=%v", outcome, err)
	}
	journalBefore, err := transition.store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ParseJournal(journalBefore.Payload)
	if err != nil || journal.Checkpoint != JournalMigrating {
		t.Fatalf("interrupted journal = %#v, err=%v", journal, err)
	}
	checkpoint, err := transition.store.ReadMigrationLedger(registry, digest)
	if err != nil || !checkpoint.DiffersFromProjection(registry) {
		t.Fatalf("expected checkpoint/projection divergence, state=%#v err=%v", checkpoint, err)
	}
	transition.options.SourceMigrationCheckpoint = false
	resume, err := transition.Inspect(context.Background(), goal)
	if err != nil || len(resume.Blockers) != 0 || resume.Resume == nil ||
		*resume.Resume != *outcome.Transaction {
		t.Fatalf("exact incomplete-journal resume was blocked: %#v, err=%v", resume, err)
	}

	// Model a source release without the sealed checkpoint-reader marker and a
	// stale preactivation plan that would otherwise be replaced with fresh work.
	activeFault = ""
	options := transition.options
	options.ArtifactDigest = digestB
	transition, err = NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := transition.Inspect(context.Background(), goal)
	if err != nil || len(blocked.Blockers) == 0 ||
		blocked.Blockers[0].Code != CodeRollbackIncompatible {
		t.Fatalf("stale replacement without checkpoint-aware source = %#v, err=%v", blocked, err)
	}
	after, err := transition.store.ReadCurrentJournal()
	if err != nil || !sameProtectedSnapshot(journalBefore, after) {
		t.Fatalf("source guard replaced the incomplete journal: before=%#v after=%#v err=%v", journalBefore, after, err)
	}
}

func TestV2CheckpointConversionRequiresConsentAndCompletesAtReadyBoundary(t *testing.T) {
	for _, faultPoint := range []string{"", "before-checkpoint-publication", "after-checkpoint-publication"} {
		name := faultPoint
		if name == "" {
			name = "normal"
		}
		t.Run(name, func(t *testing.T) {
			activeFault := ""
			transition, _, _ := v2TransitionFixture(t, func(point string) error {
				if activeFault != "" && point == activeFault {
					return errors.New("interrupt checkpoint publication")
				}
				return nil
			})
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			legacyPlan, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			if outcome, err := transition.Converge(context.Background(), Execution{
				Plan: legacyPlan.Plan, Authorization: v2TestAuthorization(legacyPlan.Plan),
			}); err != nil || outcome.Status != StatusReady {
				t.Fatalf("legacy migration = %#v, err=%v", outcome, err)
			}
			projectionBefore, err := transition.store.ReadLedger()
			if err != nil || !projectionBefore.Exists {
				t.Fatalf("legacy projection before conversion = %#v, err=%v", projectionBefore, err)
			}
			transition.options.MigrationCheckpoint = true
			conversionPlan, err := transition.Inspect(context.Background(), goal)
			if err != nil || len(conversionPlan.Blockers) != 0 || !conversionPlan.Assessment.Changed ||
				conversionPlan.Outcome == nil || conversionPlan.Outcome.Status != StatusMigrationRequired {
				t.Fatalf("conversion assessment = %#v, err=%v", conversionPlan, err)
			}
			foundDecision := false
			for _, decision := range conversionPlan.Decisions {
				foundDecision = foundDecision || (decision.Resource == "migration-history.checkpoint" &&
					decision.Result == "compact-migration-history")
			}
			if !foundDecision {
				t.Fatal("checkpoint conversion was not visible in the assessed decisions")
			}
			denied, err := transition.Converge(context.Background(), Execution{Plan: conversionPlan.Plan})
			if err != nil || denied.Code != CodeConfirmationRequired {
				t.Fatalf("conversion without authorization = %#v, err=%v", denied, err)
			}
			missing, err := transition.store.ReadMigrationCheckpoint()
			if err != nil || missing.Exists {
				t.Fatalf("checkpoint published without consent: %#v, err=%v", missing, err)
			}
			activeFault = faultPoint
			outcome, err := transition.Converge(context.Background(), Execution{
				Plan: conversionPlan.Plan, Authorization: v2TestAuthorization(conversionPlan.Plan),
			})
			if faultPoint == "before-checkpoint-publication" {
				if err != nil || outcome.Status != StatusRecovering || outcome.Transaction == nil {
					t.Fatalf("pre-publication interruption = %#v, err=%v", outcome, err)
				}
				activeFault = ""
				transition, err = NewV2Transition(transition.options)
				if err != nil {
					t.Fatal(err)
				}
				resume, inspectErr := transition.Inspect(context.Background(), goal)
				if inspectErr != nil || len(resume.Blockers) != 0 || resume.Resume == nil ||
					*resume.Resume != *outcome.Transaction || resume.Plan == "" ||
					resume.Outcome == nil || resume.Outcome.Status != StatusRecovering {
					t.Fatalf("checkpoint publication resume inspection = %#v, err=%v", resume, inspectErr)
				}
				outcome, err = transition.Converge(context.Background(), Execution{
					Plan: resume.Plan,
				})
			} else if faultPoint == "after-checkpoint-publication" && err == nil {
				// The injected process loss happened after the atomic authority
				// switch; the reducer observes ready state on the new authority.
			}
			if err != nil || outcome.Status != StatusReady {
				t.Fatalf("checkpoint conversion = %#v, err=%v", outcome, err)
			}
			checkpoint, err := transition.store.ReadMigrationCheckpoint()
			if err != nil || !checkpoint.Exists {
				t.Fatalf("checkpoint was not published after authorization: %#v, err=%v", checkpoint, err)
			}
			projectionAfter, err := transition.store.ReadLedger()
			if err != nil || !bytes.Equal(projectionAfter.Payload, projectionBefore.Payload) {
				t.Fatalf("conversion changed the retained legacy projection: %#v, err=%v", projectionAfter, err)
			}
			repeat, err := transition.Inspect(context.Background(), goal)
			if err != nil || repeat.Assessment.Changed || repeat.Outcome == nil || repeat.Outcome.Status != StatusReady {
				t.Fatalf("checkpoint fixed point = %#v, err=%v", repeat, err)
			}
		})
	}
}

func TestV2ExistingCheckpointSuffixRequiresFreshConsentToCompact(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	registry, digest := transition.registry, transition.registryDigest
	projection, _, err := MarshalLedgerV2(BaselineLedgerV2(registry), registry)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := transition.store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := transition.store.CompareAndSwapLedger(missing, projection); err != nil {
		t.Fatal(err)
	}
	baseline, err := transition.store.ReadMigrationLedger(registry, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := transition.store.ConvertMigrationLedger(baseline, registry, digest, registry, digest); err != nil {
		t.Fatal(err)
	}

	transition.options.SourceMigrationCheckpoint = true
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	legacyPlan, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := transition.Converge(context.Background(), Execution{
		Plan: legacyPlan.Plan, Authorization: v2TestAuthorization(legacyPlan.Plan),
	}); err != nil || outcome.Status != StatusReady {
		t.Fatalf("unmarked checkpoint migration = %#v, err=%v", outcome, err)
	}
	afterMigration, err := transition.store.ReadMigrationLedger(registry, digest)
	if err != nil || !ledgerCheckpointHasSuffix(afterMigration.Checkpoint) {
		t.Fatalf("migration did not leave a checkpoint suffix: %#v, err=%v", afterMigration, err)
	}

	transition.options.MigrationCheckpoint = true
	transition.options.SourceMigrationCheckpoint = true
	assessment, err := transition.Inspect(context.Background(), goal)
	if err != nil || !assessment.Assessment.Changed || assessment.Outcome == nil ||
		assessment.Outcome.Status != StatusMigrationRequired {
		t.Fatalf("suffix compaction assessment = %#v, err=%v", assessment, err)
	}
	denied, err := transition.Converge(context.Background(), Execution{Plan: assessment.Plan})
	if err != nil || denied.Code != CodeConfirmationRequired {
		t.Fatalf("suffix compaction without authorization = %#v, err=%v", denied, err)
	}
	if again, err := transition.store.ReadMigrationLedger(registry, digest); err != nil ||
		!ledgerCheckpointHasSuffix(again.Checkpoint) {
		t.Fatalf("unauthorized request changed suffix: %#v, err=%v", again, err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: assessment.Plan, Authorization: v2TestAuthorization(assessment.Plan),
	})
	if err != nil || outcome.Status != StatusReady {
		t.Fatalf("authorized suffix compaction = %#v, err=%v", outcome, err)
	}
	compacted, err := transition.store.ReadMigrationLedger(registry, digest)
	if err != nil || ledgerCheckpointHasSuffix(compacted.Checkpoint) {
		t.Fatalf("suffix remains after compaction: hasSuffix=%t, err=%v", ledgerCheckpointHasSuffix(compacted.Checkpoint), err)
	}
	repeat, err := transition.Inspect(context.Background(), goal)
	if err != nil || repeat.Assessment.Changed || repeat.Outcome == nil || repeat.Outcome.Status != StatusReady {
		t.Fatalf("compacted checkpoint fixed point = %#v, err=%v", repeat, err)
	}
}

func TestV2FutureMigrationFromCompletedCheckpointSuffixCompactsAtNewEpoch(t *testing.T) {
	for _, sourceReader := range []bool{false, true} {
		name := "source-not-checkpoint-aware"
		if sourceReader {
			name = "verified-checkpoint-aware-source"
		}
		t.Run(name, func(t *testing.T) {
			transition, _, _ := v2TransitionFixture(t, nil)
			registry, digest := transition.registry, transition.registryDigest
			projection, _, err := MarshalLedgerV2(BaselineLedgerV2(registry), registry)
			if err != nil {
				t.Fatal(err)
			}
			missing, err := transition.store.ReadLedger()
			if err != nil {
				t.Fatal(err)
			}
			if err := transition.store.CompareAndSwapLedger(missing, projection); err != nil {
				t.Fatal(err)
			}
			baseline, err := transition.store.ReadMigrationLedger(registry, digest)
			if err != nil {
				t.Fatal(err)
			}
			if err := transition.store.ConvertMigrationLedger(baseline, registry, digest, registry, digest); err != nil {
				t.Fatal(err)
			}
			transition.options.SourceMigrationCheckpoint = true
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			legacyPlan, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			if outcome, err := transition.Converge(context.Background(), Execution{
				Plan: legacyPlan.Plan, Authorization: v2TestAuthorization(legacyPlan.Plan),
			}); err != nil || outcome.Status != StatusReady {
				t.Fatalf("unmarked checkpoint migration = %#v, err=%v", outcome, err)
			}
			prior, err := transition.store.ReadMigrationLedger(registry, digest)
			if err != nil || !ledgerCheckpointHasSuffix(prior.Checkpoint) {
				t.Fatalf("unmarked checkpoint migration suffix = %#v, err=%v", prior.Checkpoint.Domains, err)
			}

			future := registry
			future.CurrentEpochs = map[string]int{}
			for domain, epoch := range registry.CurrentEpochs {
				future.CurrentEpochs[domain] = epoch
			}
			future.CurrentEpochs["settings"] = 3
			future.Migrations = append(slices.Clone(registry.Migrations), MigrationDefinitionV2{
				ID: "canonicalize-test-vms-settings-v3", Domain: "settings",
				FromEpoch: 2, ToEpoch: 3, Kind: "test-vms-settings-v1-to-v2",
			})
			futurePayload, err := json.Marshal(future)
			if err != nil {
				t.Fatal(err)
			}
			options := transition.options
			options.RegistryPayload = futurePayload
			options.MigrationCheckpoint = true
			options.SourceMigrationCheckpoint = sourceReader
			transition, err = NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			assessment, err := transition.Inspect(context.Background(), goal)
			if !sourceReader {
				if err != nil || len(assessment.Blockers) != 1 ||
					assessment.Blockers[0].Code != CodeRollbackIncompatible ||
					!strings.Contains(assessment.Blockers[0].Retry, "verified checkpoint-reader bridge") {
					t.Fatalf("unaware source checkpoint assessment = %#v, err=%v", assessment, err)
				}
				journalBefore, journalErr := transition.store.ReadCurrentJournal()
				checkpointBefore, checkpointErr := transition.store.ReadMigrationCheckpoint()
				projectionBefore, projectionErr := transition.store.ReadLedger()
				denied, convergeErr := transition.Converge(context.Background(), Execution{Plan: assessment.Plan})
				journalAfter, afterJournalErr := transition.store.ReadCurrentJournal()
				checkpointAfter, afterCheckpointErr := transition.store.ReadMigrationCheckpoint()
				projectionAfter, afterProjectionErr := transition.store.ReadLedger()
				if convergeErr != nil || denied.Code != CodeRollbackIncompatible ||
					journalErr != nil || afterJournalErr != nil || !sameProtectedSnapshot(journalBefore, journalAfter) ||
					checkpointErr != nil || afterCheckpointErr != nil || !sameProtectedSnapshot(checkpointBefore, checkpointAfter) ||
					projectionErr != nil || afterProjectionErr != nil || !sameProtectedSnapshot(projectionBefore, projectionAfter) {
					t.Fatalf("unaware source guard mutated state: outcome=%#v err=%v", denied, convergeErr)
				}
				return
			}
			if err != nil || len(assessment.Blockers) != 0 || !assessment.Assessment.Changed {
				t.Fatalf("future migration assessment = %#v, err=%v", assessment, err)
			}
			outcome, err := transition.Converge(context.Background(), Execution{
				Plan: assessment.Plan, Authorization: v2TestAuthorization(assessment.Plan),
			})
			if err != nil || outcome.Status != StatusReady {
				t.Fatalf("future migration convergence = %#v, err=%v", outcome, err)
			}
			final, err := transition.store.ReadMigrationLedger(transition.registry, transition.registryDigest)
			if err != nil || !final.Checkpointed || final.Ledger.Domains["settings"].Epoch != 3 ||
				ledgerCheckpointHasSuffix(final.Checkpoint) ||
				final.Checkpoint.Domains["settings"].CompactedThrough != 3 {
				t.Fatalf("future checkpoint did not compact epoch 3: %#v, err=%v", final.Checkpoint.Domains, err)
			}
			projectionAfter, err := transition.store.ReadLedger()
			if err != nil || !bytes.Equal(projectionAfter.Payload, projection) {
				t.Fatalf("future migration changed retained projection: %#v, err=%v", projectionAfter, err)
			}
		})
	}
}

func TestV2MigrationAndCheckpointPublicationShareOneConsent(t *testing.T) {
	for _, faultPoint := range []string{"", "before-checkpoint-publication", "after-checkpoint-publication"} {
		name := faultPoint
		if name == "" {
			name = "normal"
		}
		t.Run(name, func(t *testing.T) {
			activeFault := ""
			transition, _, _ := v2TransitionFixture(t, func(point string) error {
				if activeFault != "" && point == activeFault {
					return errors.New("interrupt marker migration")
				}
				return nil
			})
			transition.options.MigrationCheckpoint = true
			transition.options.SourceMigrationCheckpoint = true
			goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil || !inspection.Assessment.Changed || len(inspection.Blockers) != 0 {
				t.Fatalf("checkpoint-enabled migration inspection = %#v, err=%v", inspection, err)
			}
			if _, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan}); err != nil {
				t.Fatal(err)
			}
			activeFault = faultPoint
			outcome, err := transition.Converge(context.Background(), Execution{
				Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
			})
			if faultPoint == "before-checkpoint-publication" {
				if err != nil || outcome.Status != StatusRecovering || outcome.Transaction == nil {
					t.Fatalf("pre-publication interruption = %#v, err=%v", outcome, err)
				}
				activeFault = ""
				outcome, err = transition.Converge(context.Background(), Execution{
					Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan),
				})
			} else if faultPoint == "after-checkpoint-publication" {
				activeFault = ""
			}
			if err != nil || outcome.Status != StatusReady {
				t.Fatalf("checkpoint-enabled migration = %#v, err=%v", outcome, err)
			}
			checkpoint, err := transition.store.ReadMigrationCheckpoint()
			if err != nil || !checkpoint.Exists {
				t.Fatalf("checkpoint missing after the original consent: %#v, err=%v", checkpoint, err)
			}
			legacy, err := transition.store.ReadLedger()
			if err != nil || !legacy.Exists {
				t.Fatalf("compatible legacy ledger projection missing: %#v, err=%v", legacy, err)
			}
		})
	}
}

func TestV2LegacyInFlightJournalFinishesBeforeFreshCheckpointConsent(t *testing.T) {
	activeFault := "after-settings-cas"
	transition, _, _ := v2TransitionFixture(t, func(point string) error {
		if point == activeFault {
			return errors.New("interrupt legacy journal")
		}
		return nil
	})
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	initial, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), Execution{
		Plan: initial.Plan, Authorization: v2TestAuthorization(initial.Plan),
	})
	if err != nil || outcome.Status != StatusRecovering {
		t.Fatalf("legacy journal interruption = %#v, err=%v", outcome, err)
	}
	activeFault = ""
	transition.options.MigrationCheckpoint = true
	resume, err := transition.Inspect(context.Background(), goal)
	if err != nil || resume.Resume == nil || len(resume.Decisions) == 0 {
		t.Fatalf("legacy resume inspection = %#v, err=%v", resume, err)
	}
	for _, decision := range resume.Decisions {
		if decision.Resource == "migration-history.checkpoint" {
			t.Fatal("legacy journal resume incorrectly added checkpoint conversion to its grant")
		}
	}
	outcome, err = transition.Converge(context.Background(), Execution{Plan: resume.Plan})
	if err != nil || outcome.Status != StatusReady {
		t.Fatalf("legacy journal resume = %#v, err=%v", outcome, err)
	}
	checkpoint, err := transition.store.ReadMigrationCheckpoint()
	if err != nil || checkpoint.Exists {
		t.Fatalf("checkpoint was published under legacy authorization: %#v, err=%v", checkpoint, err)
	}
	conversion, err := transition.Inspect(context.Background(), goal)
	if err != nil || conversion.Outcome == nil || conversion.Outcome.Status != StatusMigrationRequired {
		t.Fatalf("fresh conversion inspection = %#v, err=%v", conversion, err)
	}
	outcome, err = transition.Converge(context.Background(), Execution{
		Plan: conversion.Plan, Authorization: v2TestAuthorization(conversion.Plan),
	})
	if err != nil || outcome.Status != StatusReady {
		t.Fatalf("freshly authorized conversion = %#v, err=%v", outcome, err)
	}
	checkpoint, err = transition.store.ReadMigrationCheckpoint()
	if err != nil || !checkpoint.Exists {
		t.Fatalf("checkpoint missing after fresh consent: %#v, err=%v", checkpoint, err)
	}
}
