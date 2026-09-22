package networkruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEnsureHostLockCreatesAndPreservesValidatedInode(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "subyard-network")
	uid, gid := os.Geteuid(), os.Getegid()

	if err := ensureHostLockAt(directory, uid, gid); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		t.Fatalf("stat directory: %v", err)
	}
	if !directoryInfo.IsDir() || directoryInfo.Mode().Perm() != 0o755 {
		t.Fatalf("directory mode = %v, want a real 0755 directory", directoryInfo.Mode())
	}

	path := filepath.Join(directory, hostLockName)
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat lock: %v", err)
	}
	if !before.Mode().IsRegular() || before.Mode().Perm() != 0o660 {
		t.Fatalf("lock mode = %v, want a regular 0660 file", before.Mode())
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != uid || int(stat.Gid) != gid || stat.Nlink != 1 {
		t.Fatalf("lock identity = %#v, want uid=%d gid=%d nlink=1", stat, uid, gid)
	}

	if err := ensureHostLockAt(directory, uid, gid); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat lock after second ensure: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("idempotent ensure replaced the existing lock inode")
	}
}

func TestAcquireHostLockRequiresProvisionedLock(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "missing")
	_, err := acquireHostLockAt(context.Background(), directory, os.Geteuid(), os.Getegid())
	if err == nil || !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "run 'yard init'") {
		t.Fatalf("missing lock error = %v, want actionable initialization error", err)
	}
}

func TestCheckHostLockIsReadOnly(t *testing.T) {
	uid, gid := os.Geteuid(), os.Getegid()
	directory := filepath.Join(t.TempDir(), "subyard-network")
	if err := checkHostLockAt(directory, uid, gid); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing check error = %v, want not-exist", err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only check created its directory: %v", err)
	}

	if err := ensureHostLockAt(directory, uid, gid); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, hostLockName)
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkHostLockAt(directory, uid, gid); err != nil {
		t.Fatalf("check initialized lock: %v", err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatalf("read-only check changed lock inode or mode: before=%v after=%v", before, after)
	}
}

func TestHostLockContentionHonorsContextCancellation(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "subyard-network")
	uid, gid := os.Geteuid(), os.Getegid()
	if err := ensureHostLockAt(directory, uid, gid); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	releaseFirst, err := acquireHostLockAt(context.Background(), directory, uid, gid)
	if err != nil {
		t.Fatalf("acquire first: %v", err)
	}
	defer releaseFirst()

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = acquireHostLockAt(ctx, directory, uid, gid)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended acquire error = %v, want deadline exceeded", err)
	}
	if time.Since(started) < 50*time.Millisecond {
		t.Fatalf("contended acquire returned before the context deadline: %s", time.Since(started))
	}

	releaseFirst()
	releaseSecond, err := acquireHostLockAt(context.Background(), directory, uid, gid)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	releaseSecond()
}

func TestEnsureHostLockRejectsUnsafeExistingObjects(t *testing.T) {
	uid, gid := os.Geteuid(), os.Getegid()

	t.Run("symlink", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "subyard-network")
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(directory, hostLockName)); err != nil {
			t.Fatal(err)
		}

		err := ensureHostLockAt(directory, uid, gid)
		if err == nil || !strings.Contains(err.Error(), "lock") {
			t.Fatalf("symlink ensure error = %v, want lock validation failure", err)
		}
		contents, readErr := os.ReadFile(target)
		if readErr != nil || string(contents) != "unchanged" {
			t.Fatalf("symlink target changed: contents=%q err=%v", contents, readErr)
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "subyard-network")
		if err := ensureHostLockAt(directory, uid, gid); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(directory, hostLockName), filepath.Join(t.TempDir(), "alias")); err != nil {
			t.Fatal(err)
		}
		if err := ensureHostLockAt(directory, uid, gid); err == nil || !strings.Contains(err.Error(), "single link") {
			t.Fatalf("hard-linked lock error = %v, want single-link rejection", err)
		}
	})

	t.Run("unsafe mode", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "subyard-network")
		if err := ensureHostLockAt(directory, uid, gid); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, hostLockName)
		if err := os.Chmod(path, 0o666); err != nil {
			t.Fatal(err)
		}
		if err := ensureHostLockAt(directory, uid, gid); err == nil || !strings.Contains(err.Error(), "0660") {
			t.Fatalf("unsafe-mode lock error = %v, want 0660 rejection", err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("stat existing unsafe lock: %v", err)
		}
		if info.Mode().Perm() != 0o666 {
			t.Fatalf("existing unsafe lock was mutated: mode=%v", info.Mode())
		}
	})

	t.Run("symlink directory", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		directory := filepath.Join(root, "subyard-network")
		if err := os.Symlink(target, directory); err != nil {
			t.Fatal(err)
		}
		if err := ensureHostLockAt(directory, uid, gid); err == nil {
			t.Fatal("symlink directory was accepted")
		}
	})
}
