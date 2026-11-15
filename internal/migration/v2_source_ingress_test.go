package migration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/Subyard/Subyard/internal/testkit"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

func TestV2CompatibilityIngressRejectsUnavailableLeaf(t *testing.T) {
	for _, kind := range []releasetransition.V2IngressOperationKind{
		releasetransition.V2LegacyV1Import,
		releasetransition.V2SourceImport,
		releasetransition.V2SourceEntrypoints,
	} {
		t.Run(string(kind), func(t *testing.T) {
			ingress := &V2CompatibilityIngress{}
			step := releasetransition.V2IngressStep{Kind: kind}
			ctx := context.Background()
			_, observeErr := ingress.Observe(ctx, step)
			for name, err := range map[string]error{
				"observe": observeErr,
				"apply":   ingress.Apply(ctx, step),
				"verify":  ingress.Verify(ctx, step),
			} {
				if err == nil || err.Error() != "compatibility ingress operation is unavailable" {
					t.Fatalf("%s unavailable leaf: %v", name, err)
				}
			}
		})
	}
}

// A verified import step still belongs to an unfinished journal until activation
// completes. Retired imports cannot resume, while completed provenance stays readable.
func TestV2CompatibilityIngressRefusesUnfinishedImportWithoutWrites(t *testing.T) {
	for _, state := range []string{"applying", "verified", "complete"} {
		t.Run(state, func(t *testing.T) {
			home := testkit.TempDir(t)
			previous := releasetransition.ReleaseID("0.4.0-legacy")
			links := releasetransition.ReleaseLinks{Active: "0.4.2-legacy", Previous: &previous}
			pair := releasetransition.ReleasePair{From: links.Active, Previous: &previous, Target: "0.28.0-owner"}
			options := releasetransition.V2Options{
				ConfigHome:      home,
				Releases:        pair,
				RegistryPayload: []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":1},"migrations":[]}`),
				ArtifactDigest:  releasetransition.Fingerprint(strings.Repeat("a", 64)),
				Ingress:         retiredImportFixture{failApply: state == "applying"},
				ObserveLinks:    func(context.Context) (releasetransition.ReleaseLinks, error) { return links, nil },
				ActivateLinks: func(_ context.Context, releases releasetransition.ReleasePair) (releasetransition.ReleaseLinks, error) {
					if state != "complete" {
						return links, errors.New("fixture activation interrupted")
					}
					from := releases.From
					links = releasetransition.ReleaseLinks{Active: releases.Target, Previous: &from}
					return links, nil
				},
				VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true },
			}
			ctx := context.Background()
			transition, err := releasetransition.NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			goal := releasetransition.Goal{Target: pair.Target, Direction: releasetransition.DirectionActivateTarget}
			inspection, err := transition.Inspect(ctx, goal)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := transition.Converge(ctx, releasetransition.Execution{Plan: inspection.Plan, Authorization: "fixture-consent"})
			if err != nil {
				t.Fatal(err)
			}
			if state == "complete" && outcome.Status != releasetransition.StatusReady || state != "complete" && outcome.Status != releasetransition.StatusRecovering {
				t.Fatalf("import fixture outcome: %#v", outcome)
			}
			store, err := releasetransition.NewPOSIXV2Store(home)
			if err != nil {
				t.Fatal(err)
			}
			journalSnapshot, err := store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			journal, err := releasetransition.ParseJournal(journalSnapshot.Payload)
			if err != nil {
				t.Fatal(err)
			}
			wantCheckpoint := releasetransition.StepVerified
			if state == "applying" {
				wantCheckpoint = releasetransition.StepEvidence
			}
			if len(journal.Steps) != 1 || journal.Steps[0].Checkpoint != wantCheckpoint {
				t.Fatalf("unexpected import fixture journal: %#v", journal)
			}
			// Stray V1 metadata is irrelevant even when it cannot be parsed or followed.
			testkit.WriteFile(t, filepath.Join(home, "migrations", "state.json"), []byte("{invalid V1\n"), 0600)
			if err := os.Symlink("missing-V1-journals", filepath.Join(home, "migrations", "transactions")); err != nil {
				t.Fatal(err)
			}
			before := compatibilityTreeSnapshot(t, home)
			beforeLinks := links
			options.Ingress = &V2CompatibilityIngress{}
			if state == "complete" {
				transition, err = releasetransition.NewV2Transition(options)
				if err != nil {
					t.Fatal(err)
				}
				inspection, err = transition.Inspect(ctx, goal)
				if err != nil || inspection.Outcome == nil || inspection.Outcome.Status != releasetransition.StatusReady {
					t.Fatalf("completed legacy provenance was refused: %#v, %v", inspection, err)
				}
			} else {
				for _, replacement := range []bool{false, true} {
					if replacement {
						options.Replacement = &releasetransition.JournalReplacement{
							Transaction: journal.Transaction, Fingerprint: journalSnapshot.Fingerprint,
							Reason: releasetransition.JournalReplacementNewerRelease,
						}
					}
					if _, err := releasetransition.NewV2Transition(options); err == nil || err.Error() != "compatibility ingress operation is unavailable" {
						t.Fatalf("unfinished import refusal (replacement=%t): %v", replacement, err)
					}
				}
			}
			if !reflect.DeepEqual(before, compatibilityTreeSnapshot(t, home)) || !reflect.DeepEqual(beforeLinks, links) {
				t.Fatal("import inspection changed protected files or release links")
			}
		})
	}
}

type retiredImportFixture struct{ failApply bool }

func (retiredImportFixture) Inspect(context.Context, *releasetransition.V2IngressBinding) (releasetransition.V2IngressInspection, error) {
	facts := releasetransition.Fingerprint(strings.Repeat("b", 64))
	return releasetransition.V2IngressInspection{Operations: []releasetransition.V2IngressOperation{{
		Kind: releasetransition.V2LegacyV1Import, Decision: releasetransition.DecisionCanonicalize,
		Expected: facts, Desired: facts, Static: releasetransition.Fingerprint(strings.Repeat("c", 64)),
	}}}, nil
}
func (retiredImportFixture) Observe(_ context.Context, step releasetransition.V2IngressStep) (releasetransition.Fingerprint, error) {
	return step.Expected, nil
}
func (fixture retiredImportFixture) Apply(context.Context, releasetransition.V2IngressStep) error {
	if fixture.failApply {
		return errors.New("fixture import interrupted")
	}
	return nil
}
func (retiredImportFixture) Verify(context.Context, releasetransition.V2IngressStep) error {
	return nil
}

func compatibilityTreeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		value := fmt.Sprintf("%s %d %s", info.Mode(), info.Size(), info.ModTime())
		if info.Mode().IsRegular() {
			payload, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += " " + string(payload)
		} else if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += " " + target
		}
		result[path] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
