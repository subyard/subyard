package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	rollbackfixture "github.com/Subyard/Subyard/internal/testkit/rollback"
)

func TestCompletedRollbackHistoryScopesAndRepairGatesRequireExactClosure(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	home := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	targetRegistry := []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":1},"migrations":[]}`)
	sourceRegistry := []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":2},"migrations":[{"id":"future-v2","domain":"settings","fromEpoch":1,"toEpoch":2,"kind":"future-source-v1"}]}`)
	testkit.WriteFile(t, filepath.Join(root, "config", "release-transition.json"), targetRegistry, 0o600)
	runtimeRoot := filepath.Join(root, "runtime")
	environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot)
	program, err := New(Options{RepositoryRoot: root, Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	pair := releasetransition.ReleasePair{From: "1.3.0-source", Target: "1.2.3-target"}
	artifact := releasetransition.Fingerprint(strings.Repeat("a", 64))
	journal := rollbackfixture.Seed(t, rollbackfixture.Inputs{ConfigHome: home, RuntimeRoot: runtimeRoot, Releases: pair,
		SourceArtifact: releasetransition.Fingerprint(strings.Repeat("b", 64)), TargetArtifact: artifact, SourceRegistry: sourceRegistry, TargetRegistry: targetRegistry})
	store, err := releasetransition.NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	outcome := releasetransition.Outcome{Active: pair.Target, Previous: &pair.From, Target: pair.Target, Transaction: &journal.Transaction,
		Status: releasetransition.StatusOperatorActionRequired, Code: releasetransition.CodeDependencyUnavailable}
	inspection := &releasetransition.Inspection{Outcome: &outcome, Blockers: []releasetransition.Blocker{{Code: releasetransition.CodeDependencyUnavailable, Resource: "activation.materialized-config.pending-configuration", Message: "configuration is pending", Retry: "yard config apply"}}}
	goal := releasetransition.Goal{Target: pair.Target, Direction: releasetransition.DirectionActivateTarget}
	self := releasetransition.ReleasePair{From: pair.Target, Previous: &pair.From, Target: pair.Target}
	for _, changed := range []bool{false, true} {
		if changed {
			testkit.WriteFile(t, filepath.Join(home, "reconcile", "rollback", "v1", "closures", string(journal.Transaction)+".json"), []byte("{}\n"), 0o600)
		}
		if got := program.completedHistoryClean(outcome); got == changed {
			t.Fatalf("completed history admission=%t, changed=%t", got, changed)
		}
		if got := program.completedRepairJournalMatches(store, journal, outcome); got == changed {
			t.Fatalf("repair admission=%t, changed=%t", got, changed)
		}
		if got := program.pendingIntegrationConfigurationGate(&outcome, inspection); got == changed {
			t.Fatalf("pending configuration admission=%t, changed=%t", got, changed)
		}
		source := &releaseConfiguration{}
		wide, err := source.materializedScope(program, self, home, goal, artifact, "", false)
		if err != nil || wide == changed {
			t.Fatalf("native scope=%t, changed=%t, err=%v", wide, changed, err)
		}
	}
	after, err := store.ReadCurrentJournal()
	if err != nil || !bytes.Equal(before.Payload, after.Payload) {
		t.Fatalf("history admission changed S's journal: %v", err)
	}
}
