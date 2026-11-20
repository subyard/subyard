package safefs_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/safefs"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestLocksShareAndCancel(t *testing.T) {
	path := filepath.Join(testkit.TempDir(t), "lock")
	ctx := context.Background()
	first, err := safefs.Lock(ctx, path, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := safefs.Lock(ctx, path, true, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	second.Release()

	cancelled, cancel := context.WithCancelCause(ctx)
	cause := errors.New("lock wait cancelled")
	cancel(cause)
	if lock, err := safefs.Lock(cancelled, path, false, time.Millisecond); !errors.Is(err, cause) {
		if lock != nil {
			lock.Release()
		}
		t.Fatalf("exclusive lock during shared lock = %v, want %v", err, cause)
	}
	first.Release()
	exclusive, err := safefs.Lock(ctx, path, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	exclusive.Release()

	link := filepath.Join(filepath.Dir(path), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if lock, err := safefs.OpenLock(link, 0o600); !errors.Is(err, unix.ELOOP) {
		if lock != nil {
			lock.Release()
		}
		t.Fatalf("open symlink = %v", err)
	}
}
