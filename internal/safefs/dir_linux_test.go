package safefs_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/safefs"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestEnsureDirInterruption(t *testing.T) {
	failure := errors.New("injected fault")
	for _, point := range []safefs.Point{safefs.AfterDirCreate, safefs.AfterDirChmod} {
		for _, crash := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/crash=%t", point, crash), func(t *testing.T) {
				path := filepath.Join(testkit.TempDir(t), "private")
				previous := unix.Umask(0o277)
				t.Cleanup(func() { unix.Umask(previous) })
				wantErr := failure
				if crash {
					wantErr = safefs.ErrCrash
				}
				o := safefs.DirOptions{Fault: func(observed safefs.Point) error {
					if observed == point {
						return fmt.Errorf("failure at %s: %w", point, wantErr)
					}
					return nil
				}}
				if err := safefs.EnsureDir(path, o); !errors.Is(err, wantErr) {
					t.Fatalf("ensure = %v, want %v", err, wantErr)
				}
				if point == safefs.AfterDirCreate {
					checkMode(t, path, 0o500)
				} else {
					checkMode(t, path, 0o700)
				}
				if err := safefs.EnsurePrivateDir(path); err != nil {
					t.Fatal(err)
				}
				checkMode(t, path, 0o700)
			})
		}
	}
}

func TestEnsureDirPolicies(t *testing.T) {
	root := testkit.TempDir(t)
	path := filepath.Join(root, "private")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o750); err != nil {
		t.Fatal(err)
	}
	err := safefs.EnsurePrivateDir(path)
	var state *safefs.DirStateError
	if !errors.Is(err, safefs.ErrUnsafeMode) || !errors.As(err, &state) ||
		state.Mode.Perm() != 0o750 || state.UID != uint32(os.Geteuid()) || state.Path != path {
		t.Fatalf("broad directory refusal = %v, state = %+v", err, state)
	}
	checkMode(t, path, 0o750)
	if err := safefs.EnsureDir(path, safefs.DirOptions{Existing: safefs.Repair}); err != nil {
		t.Fatal(err)
	}
	checkMode(t, path, 0o700)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if err := safefs.EnsureDir(path, safefs.DirOptions{Existing: safefs.Exact}); !errors.Is(err, safefs.ErrUnsafeMode) {
		t.Fatalf("exact directory with narrow mode = %v", err)
	}
	checkMode(t, path, 0o000)
	if err := safefs.EnsurePrivateDir(path); err != nil {
		t.Fatal(err)
	}
	checkMode(t, path, 0o700)
	link := filepath.Join(root, "symlink")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := safefs.EnsurePrivateDir(link); !errors.Is(err, safefs.ErrSymlink) {
		t.Fatalf("ensure symlink = %v", err)
	}
	file := filepath.Join(root, "file")
	testkit.WriteFile(t, file, nil, 0o600)
	if err := safefs.EnsurePrivateDir(file); !errors.Is(err, safefs.ErrNotDirectory) {
		t.Fatalf("ensure non-directory = %v", err)
	}
}

func TestOwnershipBeforePublishAndAfterDirectoryRetry(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown to another uid needs root")
	}
	root := testkit.TempDir(t)
	owner := safefs.Ownership{UID: 65534, GID: 65534}
	path := filepath.Join(root, "file")
	o := safefs.Options{Owner: &owner, Mode: 0o640 | os.ModeSetgid, Fault: func(point safefs.Point) error {
		if point == safefs.AfterStage {
			files, err := filepath.Glob(filepath.Join(root, ".file.tmp-*"))
			if err != nil || len(files) != 1 {
				t.Fatalf("staging path = %v, %v", files, err)
			}
			checkOwner(t, files[0], owner)
			checkMode(t, files[0], 0o640|os.ModeSetgid)
		}
		return nil
	}}
	if err := safefs.WriteFile(path, []byte("payload"), o); err != nil {
		t.Fatal(err)
	}
	checkOwner(t, path, owner)
	checkMode(t, path, 0o640|os.ModeSetgid)

	directory := filepath.Join(root, "directory")
	dirOptions := safefs.DirOptions{Owner: &owner, Fault: func(point safefs.Point) error {
		if point == safefs.AfterDirChmod {
			return safefs.ErrCrash
		}
		return nil
	}}
	if err := safefs.EnsureDir(directory, dirOptions); !errors.Is(err, safefs.ErrCrash) {
		t.Fatalf("interrupt before chown = %v", err)
	}
	checkMode(t, directory, 0o700)
	checkOwner(t, directory, safefs.Ownership{UID: 0, GID: os.Getegid()})
	dirOptions.Fault = nil
	if err := safefs.EnsureDir(directory, dirOptions); err != nil {
		t.Fatal(err)
	}
	checkOwner(t, directory, owner)
	if err := safefs.EnsurePrivateDir(directory); !errors.Is(err, safefs.ErrUnsafeOwner) {
		t.Fatalf("refusal for foreign directory = %v", err)
	}
	checkOwner(t, directory, owner)
}

func checkOwner(t *testing.T, path string, owner safefs.Ownership) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != owner.UID || int(stat.Gid) != owner.GID {
		t.Fatalf("ownership of %s = %d:%d, want %d:%d", path, stat.Uid, stat.Gid, owner.UID, owner.GID)
	}
}
