package testkit

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
)

func TestSandboxFSBoundsAndAtomicFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	filesystem, err := NewSandboxFS(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "state", "record.json")
	if err := filesystem.AtomicWrite(context.Background(), path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	filesystem.WriteLimit = 2
	if err := filesystem.AtomicWrite(context.Background(), path, []byte("new"), 0o600); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected short write, got %v", err)
	}
	data, err := filesystem.ReadFile(context.Background(), path)
	if err != nil || string(data) != "old" {
		t.Fatalf("partial write replaced published data: %q %v", data, err)
	}
	if _, err := filesystem.ReadFile(context.Background(), filepath.Join(root, "..", "escape")); err == nil {
		t.Fatal("sandbox escape was accepted")
	}
}
