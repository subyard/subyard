package ownerinventory

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestInspectPendingRecoveryNeitherReplaysNorCreatesState(t *testing.T) {
	root := filepath.Join(testkit.TempDir(t), "owner-inventory")
	store := Connections{Root: root}
	if pending, err := store.InspectPendingRecovery(); err != nil || len(pending) != 0 {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatal("inspection created the store")
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, path := range []string{store.registrationPath(), store.connectionRepairPath(), store.hostIDAdoptionPath(), store.removalPath()} {
		testkit.WriteFile(t, path, []byte("retained journal\n"), 0600)
		want = append(want, filepath.Base(path))
	}
	if pending, err := store.InspectPendingRecovery(); err != nil || !reflect.DeepEqual(pending, want) {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	if err := store.pendingRecoveryLocked(); err == nil {
		t.Fatal("mutation admission ignored pending journals")
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != len(want) {
		t.Fatalf("inspection created lock or recovery files: %v %v", entries, err)
	}
}
