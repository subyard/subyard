package testkit

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestOwnerLockRootRemovalRestoresUnreadableDirectories(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads through directory modes")
	}
	root := filepath.Join(TempDir(t), "locks")
	locks := filepath.Join(root, "subyard-owner-locks-1000")
	if err := os.MkdirAll(locks, 0o700); err != nil {
		t.Fatal(err)
	}
	WriteFile(t, filepath.Join(locks, "home.lock"), nil, 0o600)
	if err := os.Chmod(locks, 0o300); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnerLockRoot(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("owner lock root retained: %v", err)
	}
}

// A root whose test binary was killed is removed by the next one; a root still
// held or still in creation is kept.
func TestOwnerLockRootSweepsAbandonedRoots(t *testing.T) {
	parent := TempDir(t)
	abandoned := filepath.Join(parent, ownerLockRootPrefix+"abandoned")
	held := filepath.Join(parent, ownerLockRootPrefix+"held")
	creating := filepath.Join(parent, ownerLockRootPrefix+"creating")
	for _, root := range []string{abandoned, held, creating} {
		if err := os.MkdirAll(filepath.Join(root, "subyard-owner-locks-1000"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	WriteFile(t, filepath.Join(abandoned, ownerLockRootHolder), nil, 0o600)
	holder, err := holdOwnerLockRoot(held)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	// The run sets the variable; the test restores the original afterwards.
	t.Setenv(ownerLockRootEnvironment, "")
	var during string
	code := runWithOwnerLockRoot(parent, func() int {
		during = os.Getenv(ownerLockRootEnvironment)
		return 0
	})
	if code != 0 || filepath.Dir(during) != parent {
		t.Fatalf("run: code=%d root=%q", code, during)
	}
	for root, kept := range map[string]bool{abandoned: false, held: true, creating: true, during: false} {
		if _, err := os.Lstat(root); (err == nil) != kept {
			t.Errorf("root %s kept=%v: %v", root, kept, err)
		}
	}
}

// A sweep that runs while other binaries take their roots never removes one:
// a holder appears under its swept name only once it is locked. Several
// sweepers spin over a small directory so an unlocked holder is found quickly.
func TestOwnerLockRootSweepSparesStartingRoots(t *testing.T) {
	parent := TempDir(t)
	done := make(chan struct{})
	var sweepers sync.WaitGroup
	for range 4 {
		sweepers.Add(1)
		go func() {
			defer sweepers.Done()
			for {
				select {
				case <-done:
					return
				default:
					sweepOwnerLockRoots(parent)
				}
			}
		}()
	}
	defer func() {
		close(done)
		sweepers.Wait()
	}()
	for index := 0; index < 2000; index++ {
		root, err := os.MkdirTemp(parent, ownerLockRootPrefix)
		if err != nil {
			t.Fatal(err)
		}
		holder, err := holdOwnerLockRoot(root)
		if err != nil {
			t.Fatalf("start %d: %v", index, err)
		}
		_, err = os.Stat(filepath.Join(root, ownerLockRootHolder))
		_ = holder.Close()
		if err != nil {
			t.Fatalf("live root %d removed: %v", index, err)
		}
		_ = removeOwnerLockRoot(root)
	}
}
