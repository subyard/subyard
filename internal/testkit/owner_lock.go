package testkit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

const (
	ownerLockRootEnvironment = "SUBYARD_OWNER_LOCK_ROOT"
	ownerLockRootPrefix      = "subyard-test-owner-locks-"
	ownerLockRootHolder      = ".holder"
)

// RunWithOwnerLockRoot runs a package's tests with owner coordination under a
// private root that is removed afterwards. Each temporary data home would
// otherwise leave its lock and epoch files in the shared /tmp namespace.
// A runner-provided root (a test run's private /tmp root) is used unchanged.
func RunWithOwnerLockRoot(m *testing.M) int {
	if os.Getenv(ownerLockRootEnvironment) != "" {
		return m.Run()
	}
	return runWithOwnerLockRoot("/tmp", m.Run)
}

func runWithOwnerLockRoot(parent string, run func() int) int {
	// A test binary that was killed, such as a helper process a test stops,
	// cannot remove its root; the next one does once no process holds it.
	sweepOwnerLockRoots(parent)
	root, err := os.MkdirTemp(parent, ownerLockRootPrefix)
	if err != nil {
		fmt.Fprintln(os.Stderr, "owner lock root:", err)
		return 1
	}
	holder, err := holdOwnerLockRoot(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "owner lock root:", err)
		_ = removeOwnerLockRoot(root)
		return 1
	}
	if err := os.Setenv(ownerLockRootEnvironment, root); err != nil {
		fmt.Fprintln(os.Stderr, "owner lock root:", err)
		_ = holder.Close()
		_ = removeOwnerLockRoot(root)
		return 1
	}
	code := run()
	_ = holder.Close()
	if err := removeOwnerLockRoot(root); err != nil {
		fmt.Fprintln(os.Stderr, "owner lock root cleanup:", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// holdOwnerLockRoot keeps an exclusive flock for the life of the test binary.
// The descriptor is close-on-exec, so helper processes never inherit it.
func holdOwnerLockRoot(root string) (*os.File, error) {
	holder, err := os.OpenFile(filepath.Join(root, ownerLockRootHolder), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = holder.Close()
		return nil, err
	}
	return holder, nil
}

// sweepOwnerLockRoots removes roots whose holder is no longer locked. A root
// without a holder may still be in creation and is left alone.
func sweepOwnerLockRoots(parent string) {
	roots, _ := filepath.Glob(filepath.Join(parent, ownerLockRootPrefix+"*"))
	for _, root := range roots {
		info, err := os.Lstat(root)
		if err != nil || !info.IsDir() {
			continue
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Getuid()) {
			continue
		}
		holder, err := os.OpenFile(filepath.Join(root, ownerLockRootHolder), os.O_RDWR|syscall.O_NOFOLLOW, 0)
		if err != nil {
			continue
		}
		if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = holder.Close()
			continue
		}
		_ = removeOwnerLockRoot(root)
		_ = holder.Close()
	}
}

// Permission fixtures may leave a lock directory unreadable; restore owner
// access before removal.
func removeOwnerLockRoot(root string) error {
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if entry != nil && entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	if err := os.RemoveAll(root); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
