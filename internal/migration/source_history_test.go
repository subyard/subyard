package migration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestSourceHistoryAbsenceSurvivesFirstSharedLock(t *testing.T) {
	home := testkit.TempDir(t)
	options := ReleaseOptions{ConfigHome: home, Version: "1.2.3"}
	before, err := InspectSourceHistory(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(home, "migrations")); !os.IsNotExist(err) {
		t.Fatal("read-only inspection created coordination state")
	}
	store, err := releasetransition.NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := store.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	after, err := InspectSourceHistory(context.Background(), options)
	if err != nil || after != before {
		t.Fatalf("ordinary shared lock changed absent history proof: %v", err)
	}
	testkit.WriteFile(t, filepath.Join(home, "migrations", "state.json"), []byte("{\"schemaVersion\":1,\"layout\":5}\n"), 0o600)
	changed, err := InspectSourceHistory(context.Background(), options)
	if err != nil || changed == before {
		t.Fatalf("new legacy history was not bound: %v", err)
	}
}

func TestSourceHistoryRejectsUnfinishedLegacyBehindOtherHistory(t *testing.T) {
	options, _, _ := publishedV1CompatibilityFixture(t, "0.4.1")
	links := releasetransition.ReleaseLinks{Active: "release-a"}
	transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{
		ConfigHome:      options.ConfigHome,
		ArtifactDigest:  releasetransition.Fingerprint(strings.Repeat("a", 64)),
		RegistryPayload: []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":1},"migrations":[]}`),
		Releases:        releasetransition.ReleasePair{From: links.Active, Target: "release-b"},
		ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
			return links, nil
		},
		ActivateLinks: func(_ context.Context, pair releasetransition.ReleasePair) (releasetransition.ReleaseLinks, error) {
			previous := pair.From
			links = releasetransition.ReleaseLinks{Active: pair.Target, Previous: &previous}
			return links, nil
		},
		VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := transition.Inspect(context.Background(), releasetransition.Goal{Target: "release-b", Direction: releasetransition.DirectionActivateTarget})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(context.Background(), releasetransition.Execution{Plan: inspection.Plan, Authorization: "new-synthetic-consent"})
	if err != nil || outcome.Status != releasetransition.StatusReady {
		t.Fatalf("completed v2 fixture: %v %v", outcome, err)
	}
	if _, err := InspectSourceHistory(context.Background(), options); err == nil {
		t.Fatal("unfinished legacy migration admitted by source repair")
	}
}
