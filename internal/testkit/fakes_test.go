package testkit

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/contracttest"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func TestManualClockAndMemoryState(t *testing.T) {
	clock := NewManualClock(time.Unix(100, 0))
	alarm := clock.After(time.Minute)
	clock.Advance(time.Minute)
	if got := <-alarm; !got.Equal(time.Unix(160, 0)) {
		t.Fatalf("unexpected manual time: %s", got)
	}
	state := NewMemoryState(domain.ProjectRecord{ProjectID: "one"})
	records, err := state.List(context.Background())
	if err != nil || len(records) != 1 {
		t.Fatalf("unexpected state: %#v, %v", records, err)
	}
}

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

func TestIncusFakeSharesReadContract(t *testing.T) {
	contracttest.IncusRead(t, &Incus{
		ServerInfo: ports.ServerInfo{Environment: "incus", Version: "6.23"},
		Instances: map[string]ports.InstanceInfo{"subyard/yard": {
			Name: "yard", Project: "subyard", Type: domain.YardContainer, Status: "Running",
			Config:  map[string]string{"security.nesting": "true"},
			Devices: map[string]map[string]string{"root": {"type": "disk"}},
		}},
	})
}
