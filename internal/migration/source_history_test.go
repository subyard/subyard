package migration

import (
	"context"
	"os"
	"path/filepath"
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
	if err != nil || changed != before {
		t.Fatalf("ignored legacy history changed the binding: %v", err)
	}
}

func TestSourceHistoryIgnoresUnsafeV1Files(t *testing.T) {
	home := testkit.TempDir(t)
	options := ReleaseOptions{ConfigHome: home}
	before, err := InspectSourceHistory(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-legacy-history", filepath.Join(home, "migrations")); err != nil {
		t.Fatal(err)
	}
	after, err := InspectSourceHistory(context.Background(), options)
	if err != nil || after != before {
		t.Fatalf("ignored legacy symlink changed source binding: %s, %v", after, err)
	}
}
