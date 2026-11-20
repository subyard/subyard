package state

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestListReadOnlyReportsEveryInvalidRecordWithoutWriting(t *testing.T) {
	root := testkit.TempDir(t)
	for _, name := range []string{"a.json", "b.json"} {
		testkit.WriteFile(t, filepath.Join(root, name), []byte("{"), 0600)
	}
	store, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.ListReadOnly(context.Background())
	if records != nil || err == nil || !strings.Contains(err.Error(), "a.json") || !strings.Contains(err.Error(), "b.json") {
		t.Fatalf("records=%v err=%v", records, err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 2 {
		t.Fatalf("read-only listing created a lock or repaired records: %v %v", entries, err)
	}
}
