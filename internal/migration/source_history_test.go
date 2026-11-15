package migration

import (
	"context"
	"crypto/sha256"
	"fmt"
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

func TestSourceHistoryBindsCommittedV1FilesAsAbsent(t *testing.T) {
	home := testkit.TempDir(t)
	state := filepath.Join(home, "migrations", "state.json")
	transactions := filepath.Join(home, "migrations", "transactions")
	versionDigest := sha256.Sum256([]byte("0.10.0"))
	journal := filepath.Join(transactions, fmt.Sprintf("%x", versionDigest[:16]), "transaction.json")
	if err := os.MkdirAll(filepath.Dir(journal), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, state, []byte(`{"schemaVersion":1,"layout":5}`), 0600)
	testkit.WriteFile(t, journal, []byte(`{"schemaVersion":1,"fromLayout":4,"toLayout":5,"toRelease":"0.10.0","phase":"committed","migrations":[]}`), 0600)
	// Pin the absent-history wire value independently of the on-disk committed history.
	absent := fmt.Sprintf(`{%q:{"Present":false,"Digest":"","Device":0,"Inode":0,"Links":0,"Mode":0,"UID":0,"GID":0},%q:{"Present":false,"Digest":"","Device":0,"Inode":0,"Links":0,"Mode":0,"UID":0,"GID":0}}`, state, transactions)
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(absent)))
	got, err := InspectSourceHistory(context.Background(), ReleaseOptions{ConfigHome: home, Version: "0.29.0"})
	if err != nil || string(got) != want {
		t.Fatalf("committed V1 binding = %s, %v; want absent %s", got, err, want)
	}
}
