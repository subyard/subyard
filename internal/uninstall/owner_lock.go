package uninstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/domain"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var ownerFiles = struct {
	sync.Mutex
	files map[string]*os.File
}{files: map[string]*os.File{}}

// BorrowOwnerLock transfers the already-held open file description to a native
// delegated installer/candidate. It survives parent death without a new inode.
func BorrowOwnerLock(dataHome string) (*os.File, error) {
	ownerFiles.Lock()
	defer ownerFiles.Unlock()
	file := ownerFiles.files[OwnerLockPath(dataHome)]
	if file == nil {
		return nil, nil
	}
	fd, err := syscall.Dup(int(file.Fd()))
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), file.Name()), nil
}

// RegisterBorrowedOwnerLock exposes an already checked, held native owner OFD to
// existing delegated installer/guard children. The caller retains ownership of
// held; release closes only this duplicate, never unlocks any borrower's OFD.
// It performs no path reopen or lock acquisition.
func RegisterBorrowedOwnerLock(dataHome string, held *os.File) (func(), error) {
	if held == nil {
		return nil, errors.New("native owner barrier is missing")
	}
	path := OwnerLockPath(dataHome)
	current, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	info, err := held.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 ||
		stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 || !os.SameFile(current, info) {
		return nil, errors.New("native owner barrier differs from its protected installation")
	}
	fd, err := syscall.Dup(int(held.Fd()))
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(fd)
	file := os.NewFile(uintptr(fd), path)
	ownerFiles.Lock()
	previous := ownerFiles.files[path]
	ownerFiles.files[path] = file
	ownerFiles.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			ownerFiles.Lock()
			if ownerFiles.files[path] == file {
				if previous != nil {
					if _, err := previous.Stat(); err != nil {
						previous = nil
					}
				}
				if previous == nil {
					delete(ownerFiles.files, path)
				} else {
					ownerFiles.files[path] = previous
				}
			}
			ownerFiles.Unlock()
			file.Close()
		})
	}, nil
}

// Native guard children retain the already-held barriers if their caller is
// interrupted. They never acquire a new scope or unlock their parent's OFDs.
func borrowOwnerBarriers() ([]*os.File, error) {
	ownerFiles.Lock()
	defer ownerFiles.Unlock()
	files := []*os.File{}
	for _, file := range ownerFiles.files {
		fd, err := syscall.Dup(int(file.Fd()))
		if err != nil {
			for _, borrowed := range files {
				borrowed.Close()
			}
			return nil, err
		}
		syscall.CloseOnExec(fd)
		files = append(files, os.NewFile(uintptr(fd), file.Name()))
	}
	return files, nil
}

// OwnerLock is stable account coordination state outside installation removal
// roots. Never unlink it: an already-waiting setup must keep the same inode.
func OwnerLock(ctx context.Context, dataHome string, expected *Tree) (Tree, func(), error) {
	uid := uint32(os.Getuid())
	directory := filepath.Join("/tmp", "subyard-owner-locks-"+strconv.Itoa(os.Getuid()))
	if err := privateDirectory(directory); err != nil {
		return Tree{}, nil, err
	}
	digest := sha256.Sum256([]byte(filepath.Clean(dataHome)))
	path := filepath.Join(directory, hex.EncodeToString(digest[:])+".lock")
	if expected != nil {
		if expected.Path != path {
			return Tree{}, nil, errors.New("saved owner lock belongs to another installation")
		}
		if err := CheckOwnerLockIdentity(*expected); err != nil {
			return Tree{}, nil, err
		}
	}
	parent, err := openParent(directory)
	if err != nil {
		return Tree{}, nil, err
	}
	defer parent.Close()
	name := filepath.Base(path)
	var file *os.File
	if inherited := os.Getenv("SUBYARD_OWNER_LOCK_FD"); inherited != "" {
		fd, parseErr := strconv.Atoi(inherited)
		if parseErr != nil || fd < 3 {
			return Tree{}, nil, errors.New("invalid inherited owner barrier descriptor")
		}
		duplicate, dupErr := syscall.Dup(fd)
		if dupErr != nil {
			return Tree{}, nil, dupErr
		}
		syscall.CloseOnExec(duplicate)
		file = os.NewFile(uintptr(duplicate), path)
		current, statErr := parent.Lstat(name)
		inheritedInfo, infoErr := file.Stat()
		if statErr != nil || infoErr != nil || !os.SameFile(current, inheritedInfo) {
			file.Close()
			return Tree{}, nil, errors.New("inherited owner barrier belongs to another installation")
		}
	} else {
		file, err = parent.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if err == nil {
			err = file.Chmod(0600)
		} else if errors.Is(err, os.ErrExist) {
			file, err = parent.OpenFile(name, os.O_RDWR|syscall.O_NOFOLLOW, 0)
		}
	}
	if err != nil {
		if file != nil {
			file.Close()
		}
		return Tree{}, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return Tree{}, nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Uid != uid || stat.Nlink != 1 {
		file.Close()
		return Tree{}, nil, errors.New("owner mutation lock must be an operator-owned real mode 0600 file")
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			file.Close()
			return Tree{}, nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return Tree{}, nil, fmt.Errorf("waiting for owner mutation lock: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	binding, err := Capture(path)
	if err == nil {
		current, statErr := parent.Lstat(name)
		if statErr != nil || !os.SameFile(info, current) {
			err = errors.New("owner coordination lock changed while acquiring its barrier")
		}
	}
	if err == nil && expected != nil {
		err = CheckOwnerLockIdentity(*expected)
	}
	if err != nil {
		file.Close()
		return Tree{}, nil, err
	}
	ownerFiles.Lock()
	previous := ownerFiles.files[path]
	ownerFiles.files[path] = file
	ownerFiles.Unlock()
	return binding, func() {
		ownerFiles.Lock()
		if ownerFiles.files[path] == file {
			if previous != nil {
				ownerFiles.files[path] = previous
			} else {
				delete(ownerFiles.files, path)
			}
		}
		ownerFiles.Unlock()
		// SCM_RIGHTS/Dup borrowers share this open file description. Explicit
		// LOCK_UN would release their barrier while accepted work still owns it.
		file.Close()
	}, nil
}

// OwnerLockPath names shared coordination without creating it.
func OwnerLockPath(dataHome string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(dataHome)))
	return filepath.Join("/tmp", "subyard-owner-locks-"+strconv.Itoa(os.Getuid()), hex.EncodeToString(digest[:])+".lock")
}

func ReadOwnerEpoch(dataHome string) (uint64, error) {
	path := OwnerEpochPath(dataHome)
	if err := safePath(path); err != nil {
		return 0, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Uid != uint32(os.Getuid()) {
		return 0, errors.New("unsafe owner coordination epoch")
	}
	parent, err := openParent(filepath.Dir(path))
	if err != nil {
		return 0, err
	}
	defer parent.Close()
	file, err := parent.OpenFile(filepath.Base(path), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, 32))
	if err != nil {
		return 0, err
	}
	epoch, err := strconv.ParseUint(strings.TrimSuffix(string(body), "\n"), 10, 64)
	if err != nil || string(body) != strconv.FormatUint(epoch, 10)+"\n" {
		return 0, errors.New("invalid owner coordination epoch")
	}
	return epoch, nil
}

// OwnerEpochPath is nonsecret coordination alongside the stable lock. Atomic
// epoch publication never renames the live lock inode used by queued processes.
func OwnerEpochPath(dataHome string) string { return OwnerLockPath(dataHome) + ".epoch" }

// HasOwnerInstallation admits read/session writers only to a still-established
// install after a completed uninstall epoch. Explicit setup can establish fresh
// ownership; observing a retained foreign directory cannot do so.
func HasOwnerInstallation(dataHome string) bool {
	if _, err := ReadInstallation(dataHome, uint32(os.Getuid())); err == nil {
		return true
	}
	records, err := ReadHostPaths(Installation{DataHome: dataHome, OperatorUID: uint32(os.Getuid())})
	return err == nil && len(records) > 0
}

// AdvanceOwnerEpoch is called with the stable lock held and the before/after
// intent durably saved in recovery. A partial private publication can resume,
// while readers observe only the old or complete new epoch.
func AdvanceOwnerEpoch(dataHome string, before, after uint64) error {
	if after != before+1 || after == 0 {
		return errors.New("invalid owner epoch advancement")
	}
	current, err := ReadOwnerEpoch(dataHome)
	if err != nil {
		return err
	}
	if current == after {
		return nil
	}
	if current != before {
		return errors.New("owner installation epoch changed outside the approved cleanup")
	}
	return nativeArtifact("advance-epoch", struct {
		Path          string
		Before, After uint64
		UID           uint32
	}{OwnerEpochPath(dataHome), before, after, uint32(os.Getuid())}, nil)
}

func CheckOwnerLockIdentity(expected Tree) error {
	current, err := Capture(expected.Path)
	if err != nil {
		return err
	}
	if len(current.Entries) != 1 || len(expected.Entries) != 1 {
		return errors.New("owner coordination lock identity is missing")
	}
	old, fresh := expected.Entries[0], current.Entries[0]
	old.Size, fresh.Size = 0, 0
	old.Modified, fresh.Modified = 0, 0
	old.Changed, fresh.Changed = 0, 0
	if old != fresh {
		return errors.New("owner coordination lock inode or permissions changed")
	}
	return nil
}

// Admit checks the originally prepared generation after acquiring the stable
// owner barrier. A protected pending store blocks new writers even before its
// first plan is fully published; only that original recovery may continue.
func Admit(home string, generation []Tree, epoch uint64, recovery, established bool) error {
	current, err := ReadOwnerEpoch(home)
	if err != nil {
		return err
	}
	if current != epoch {
		return fmt.Errorf("%w: owner installation epoch changed while waiting for mutation lock", domain.ErrPlanStale)
	}
	if !recovery {
		if _, err := os.Lstat(home + ".uninstall"); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("full uninstall recovery is pending at %s; retain and resume its original saved plan before setup or writes", home+".uninstall")
		}
	}
	// Legacy installations predate ownership records; a completed uninstall is
	// identified by its epoch, after which reads need newly established ownership.
	if established && epoch > 0 && !HasOwnerInstallation(home) {
		return errors.New("installation has been fully removed; no state was written")
	}
	return CheckGeneration(generation)
}
