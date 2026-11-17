package releaseruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	rollbackfixture "github.com/Subyard/Subyard/internal/testkit/rollback"
)

const futureRollbackSourceRegistry = `{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":2},"migrations":[{"id":"future-v2","domain":"settings","fromEpoch":1,"toEpoch":2,"kind":"future-source-v1"}]}`

func completedRollbackRuntimeFixture(t *testing.T, sourceRegistry []byte) (*Runtime, string, string, publishedCandidate, publishedCandidate, releasetransition.JournalRecord) {
	t.Helper()
	root := testkit.TempDir(t)
	runtimeRoot, home := filepath.Join(root, "runtime"), filepath.Join(root, "config")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	engine := fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in --version) printf 'yard-engine 1.2.3\\n';; _release-transition) %s=ready exec %q -test.run '^TestSourceAdmissionCandidateProcessHelper$';; *) exit 64;; esac\n", sourceAdmissionProcessEnvironment, executable)
	target := writeVersionedRuntimeCandidate(t, runtimeRoot, "1.2.3-target", "1.2.3", engine)
	targetArtifact := sealSourceAdmissionCandidate(t, target)
	source := writeDelegateRuntimeFixture(t, runtimeRoot, "1.3.0-source", "source", sourceRegistry, "#!/bin/sh\nprintf 'yard-engine 1.3.0\\n'\n")
	adapter := New(Config{Inputs: fixtureCommandInputs(map[string]string{"HOME": root})})
	t.Cleanup(func() { adapter.Close() })
	verified, err := adapter.verifyPublishedCandidate(context.Background(), source, runtimeRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	sourceArtifact := verified.manifestDigest
	verified.Close()
	for name, release := range map[string]releasetransition.ReleaseID{"current": target.release, "previous": source.release} {
		if err := os.Symlink("releases/"+string(release), filepath.Join(runtimeRoot, name)); err != nil {
			t.Fatal(err)
		}
	}
	journal := rollbackfixture.Seed(t, rollbackfixture.Inputs{RuntimeRoot: runtimeRoot, ConfigHome: home, Releases: releasetransition.ReleasePair{From: source.release, Target: target.release}, SourceArtifact: sourceArtifact, TargetArtifact: targetArtifact, SourceRegistry: sourceRegistry, TargetRegistry: []byte(sourceAdmissionRegistry)})
	return adapter, runtimeRoot, home, source, target, journal
}

func TestCompletedRollbackRoutesToTargetAndAdmitsNativeSourceMetadata(t *testing.T) {
	adapter, runtimeRoot, home, source, target, journal := completedRollbackRuntimeFixture(t, []byte(futureRollbackSourceRegistry))

	// S is no longer executable. Its completed, protected provenance must not
	// make its handlers a dependency of R's ordinary readiness or source repair.
	if err := os.Remove(filepath.Join(source.root, "bin", "yard-engine")); err != nil {
		t.Fatal(err)
	}
	proof, err := adapter.InspectSourceAdmission(context.Background(), runtimeRoot, home)
	if err != nil || proof.Metadata.Journal == nil || proof.Metadata.Journal.Transaction != journal.Transaction {
		t.Fatalf("native source admission = %+v, %v", proof, err)
	}
	inspection, err := adapter.inspectProtectedTransition(context.Background(), runtimeRoot, home, "default", nil)
	if err != nil || inspection == nil {
		t.Fatalf("target-owned inspection = %+v, %v", inspection, err)
	}
	if inspection.owner.candidate.release != target.release || inspection.request.RegistryDigest == journal.RegistryDigest || inspection.inspection.Outcome == nil || inspection.inspection.Outcome.Status != releasetransition.StatusReady {
		t.Fatalf("completed rollback did not route to native target readiness: %+v", inspection)
	}
}

func TestCompletedSourceOwnedRollbackRefusesMissingClosure(t *testing.T) {
	adapter, runtimeRoot, home, source, _, journal := completedRollbackRuntimeFixture(t, []byte(futureRollbackSourceRegistry))
	if err := os.Remove(filepath.Join(home, "reconcile", "rollback", "v1", "closures", string(journal.Transaction)+".json")); err != nil {
		t.Fatal(err)
	}
	// A fallback to S would try to verify its now-unavailable engine.
	if err := os.Remove(filepath.Join(source.root, "bin", "yard-engine")); err != nil {
		t.Fatal(err)
	}
	inspection, err := adapter.inspectProtectedTransition(context.Background(), runtimeRoot, home, "default", nil)
	var refusal publicReleaseInspectionError
	if inspection != nil || !errors.As(err, &refusal) || !strings.Contains(refusal.cause.Error(), "no protected closure") {
		t.Fatalf("readiness without closure = %+v, %v", inspection, err)
	}
}

func TestUnfinishedRollbackVerifiesExactSourceBeforeExecution(t *testing.T) {
	for _, sameRegistry := range []bool{false, true} {
		t.Run(fmt.Sprint(sameRegistry), func(t *testing.T) {
			payload := []byte(futureRollbackSourceRegistry)
			if sameRegistry {
				payload = []byte(sourceAdmissionRegistry)
			}
			adapter, runtimeRoot, home, source, _, journal := completedRollbackRuntimeFixture(t, payload)
			store, err := releasetransition.NewPOSIXV2Store(home)
			if err != nil {
				t.Fatal(err)
			}
			selected, err := store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			journal.Checkpoint = releasetransition.JournalMigrating
			encoded, err := releasetransition.MarshalJournal(journal)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.CompareAndSwapCurrentJournal(selected, encoded); err != nil {
				t.Fatal(err)
			}
			called := filepath.Join(home, "wrong-source-called")
			engine := fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in --version) printf 'yard-engine 1.3.0\\n';; _release-transition) printf called > %q; exit 99;; *) exit 64;; esac\n", called)
			writeDelegateRuntimeFixture(t, runtimeRoot, string(source.release), "source", payload, engine)
			_, err = adapter.inspectProtectedTransition(context.Background(), runtimeRoot, home, "default", nil)
			var public publicReleaseInspectionError
			if !errors.As(err, &public) || !strings.Contains(public.outcome.Message, "sealed rollback source differs") {
				t.Fatalf("changed source image was not refused by its exact binding: %v", err)
			}
			if _, err := os.Stat(called); !os.IsNotExist(err) {
				t.Fatalf("the wrong source image was executed: %v", err)
			}
		})
	}
}
