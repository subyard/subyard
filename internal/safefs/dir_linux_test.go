package safefs_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	if err := safefs.EnsureDir(path, safefs.DirOptions{Existing: safefs.Accept}); err != nil {
		t.Fatal(err)
	}
	checkMode(t, path, 0o750)
	if err := safefs.EnsureDir(path, safefs.DirOptions{Existing: safefs.Repair}); err != nil {
		t.Fatal(err)
	}
	checkMode(t, path, 0o700)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
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

func TestOwnershipBeforePublishAndForeignDirectoryRefusal(t *testing.T) {
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
		t.Fatalf("foreign UID reached %s", point)
		return nil
	}}
	for range 2 {
		if err := safefs.EnsureDir(directory, dirOptions); !errors.Is(err, safefs.ErrUnsafeOwner) {
			t.Fatalf("create with foreign UID = %v", err)
		}
		if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refused UID left a directory behind: %v", err)
		}
	}
	absentParent := filepath.Join(root, "absent")
	dirOptions.ParentMode = 0o700
	if err := safefs.EnsureDir(filepath.Join(absentParent, "child"), dirOptions); !errors.Is(err, safefs.ErrUnsafeOwner) {
		t.Fatalf("create with foreign UID and missing parent = %v", err)
	}
	if _, err := os.Lstat(absentParent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused UID left a parent directory behind: %v", err)
	}
	if err := safefs.EnsurePrivateDir(directory); err != nil {
		t.Fatal(err)
	}
	rootOwner := safefs.Ownership{UID: 0, GID: os.Getegid()}
	if err := safefs.EnsureDir(directory, dirOptions); !errors.Is(err, safefs.ErrUnsafeOwner) {
		t.Fatalf("existing directory accepted a different UID: %v", err)
	}
	checkOwner(t, directory, rootOwner)
	if err := os.Chown(directory, owner.UID, owner.GID); err != nil {
		t.Fatal(err)
	}
	if err := safefs.EnsurePrivateDir(directory); !errors.Is(err, safefs.ErrUnsafeOwner) {
		t.Fatalf("foreign directory was accepted: %v", err)
	}
	checkOwner(t, directory, owner)
}

func TestEnsureDirChecksModeAfterMkdir(t *testing.T) {
	path := filepath.Join(testkit.TempDir(t), "directory")
	options := safefs.DirOptions{Fault: func(point safefs.Point) error {
		if point != safefs.AfterDirCreate {
			return nil
		}
		if err := os.Rename(path, path+".original"); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			return err
		}
		return os.Chmod(path, 0o750)
	}}
	if err := safefs.EnsureDir(path, options); !errors.Is(err, safefs.ErrUnsafeMode) {
		t.Fatalf("ensure accepted broader directory after mkdir: %v", err)
	}
	checkMode(t, path, 0o750)
}

func TestEnsureDirFinishesUnderSetgidParent(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.Chmod(root, os.ModeSetgid|0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "directory")
	if err := safefs.EnsurePrivateDir(path); err != nil {
		t.Fatal(err)
	}
	checkMode(t, path, 0o700)
}

func TestOwnershipRejectsOutOfRangeIDs(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("Linux ownership IDs exceed the range of int on this architecture")
	}
	overflow, err := strconv.Atoi("4294967296")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{-2, overflow - 1, overflow} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			path := filepath.Join(testkit.TempDir(t), "missing", "entry")
			for _, operation := range []func() error{
				func() error {
					return safefs.EnsureDir(path, safefs.DirOptions{
						Owner: &safefs.Ownership{UID: -1, GID: id}, ParentMode: 0o700,
					})
				},
				func() error {
					return safefs.WriteFile(path, nil, safefs.Options{
						Owner: &safefs.Ownership{UID: -1, GID: id}, ParentMode: 0o700,
					})
				},
				func() error {
					return safefs.WriteFile(path, nil, safefs.Options{
						Owner: &safefs.Ownership{UID: id, GID: -1}, ParentMode: 0o700,
					})
				},
			} {
				if err := operation(); !errors.Is(err, unix.EINVAL) {
					t.Errorf("invalid ownership operation = %v, want EINVAL", err)
				}
				if _, err := os.Lstat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid ownership created a parent: %v", err)
				}
			}
		})
	}
}

func TestEnsureDirChangesGroupBeforeWidening(t *testing.T) {
	gid := -1
	if os.Geteuid() == 0 {
		gid = os.Getegid() + 1
	} else {
		groups, err := os.Getgroups()
		if err != nil {
			t.Fatal(err)
		}
		for _, group := range groups {
			if group != os.Getegid() {
				gid = group
				break
			}
		}
	}
	if gid == -1 {
		t.Skip("a second permitted gid is required")
	}
	path := filepath.Join(testkit.TempDir(t), "logs")
	if err := safefs.EnsurePrivateDir(path); err != nil {
		t.Fatal(err)
	}
	owner := safefs.Ownership{UID: os.Geteuid(), GID: gid}
	o := safefs.DirOptions{Mode: 0o770, Owner: &owner, Fault: func(point safefs.Point) error {
		if point == safefs.AfterDirChmod {
			checkMode(t, path, 0o770)
			checkOwner(t, path, owner)
			return safefs.ErrCrash
		}
		return nil
	}}
	if err := safefs.EnsureDir(path, o); !errors.Is(err, safefs.ErrCrash) {
		t.Fatalf("interrupt after widening = %v", err)
	}
	o.Fault = nil
	if err := safefs.EnsureDir(path, o); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureDirSyncsChangedDirectoryInode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory read permission used to induce a sync failure")
	}
	path := filepath.Join(testkit.TempDir(t), "new")
	if err := safefs.EnsureDir(path, safefs.DirOptions{Mode: 0o300}); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("changing metadata on a directory that cannot be opened for sync: %v", err)
	}
	checkMode(t, path, 0o300)
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
