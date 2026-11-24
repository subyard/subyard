package safefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type ExistingDir uint8

const (
	Finish ExistingDir = iota // Finish narrower modes on owned directories.
	Repair                    // Also repair broader modes on owned directories.
	Accept                    // Keep existing directories as they are.
)

type DirOptions struct {
	Mode       fs.FileMode // Zero defaults to 0700.
	Owner      *Ownership  // UID is an expectation. GID may be set.
	Existing   ExistingDir
	ParentMode fs.FileMode
	Durability Durability
	Fault      func(Point) error // Test-only interruption hook.
}

// DirStateError describes the metadata of a directory refused by EnsureDir.
type DirStateError struct {
	Path string
	Mode fs.FileMode
	UID  uint32
	err  error
}

func (e *DirStateError) Error() string { return e.err.Error() }
func (e *DirStateError) Unwrap() error { return e.err }

func EnsureDir(path string, o DirOptions) error {
	if o.Existing != Accept && expectedUID(o.Owner) != os.Geteuid() {
		return refusal(path, ErrUnsafeOwner)
	}
	if o.Owner != nil && !validOwnerID(o.Owner.GID) {
		return pathError("chown", path, unix.EINVAL)
	}
	if o.Mode == 0 {
		o.Mode = 0o700
	}
	parent, name, err := openParent(path, o.ParentMode, o.Durability)
	if err != nil {
		return err
	}
	defer parent.close()
	dir, err := parent.ensure(name, o)
	if err != nil {
		return err
	}
	dir.close()
	return nil
}

func EnsurePrivateDir(path string) error { return EnsureDir(path, DirOptions{}) }

// AdmitDir runs before a caller's own directory policy and never replaces it.
// It sets mode on a directory the caller owns, keeping the sticky bit, so both
// narrower and broader owned directories reach the caller's policy in the
// expected state. A set-ID directory only loses access outside mode and keeps
// its set-ID bits; it is left unchanged when the kernel would clear its
// set-group-ID bit. Every other state, including absence, a symlink, another
// owner, lookup errors and a failed chmod, is left unchanged for the caller's
// policy to handle.
func AdmitDir(path string, mode fs.FileMode) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil
	}
	dir := &directory{fd: fd, path: path}
	defer dir.close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Geteuid()) {
		return nil
	}
	want, have := uint32(mode.Perm()), stat.Mode&0o777
	// A failed finish leaves the directory for the caller's policy.
	switch {
	case have == want:
	case stat.Mode&0o6000 == 0:
		_ = dir.chmod(want | stat.Mode&0o1000)
	case have&^want != 0 && keepsSetGID(stat):
		_ = dir.chmod(have&want | stat.Mode&0o7000)
	}
	return nil
}

// keepsSetGID reports whether a chmod by the caller keeps the set-group-ID bit.
func keepsSetGID(stat unix.Stat_t) bool {
	if stat.Mode&unix.S_ISGID == 0 || os.Geteuid() == 0 || stat.Gid == uint32(os.Getegid()) {
		return true
	}
	groups, err := os.Getgroups()
	return err == nil && slices.Contains(groups, int(stat.Gid))
}

func SyncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

type directory struct {
	fd   int
	path string
}

func (d *directory) close() { _ = unix.Close(d.fd) }

func (d *directory) sync(durability Durability) error {
	if durability == BestEffort {
		return nil
	}
	fd, err := unix.Openat(d.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return pathError("open", d.path, err)
	}
	file := os.NewFile(uintptr(fd), d.path)
	defer file.Close()
	return file.Sync()
}

func openParent(path string, parentMode fs.FileMode, durability Durability) (*directory, string, error) {
	name := filepath.Base(path)
	if name == "." || name == ".." || name == string(filepath.Separator) || strings.HasSuffix(path, string(filepath.Separator)) {
		return nil, "", pathError("open", path, fs.ErrInvalid)
	}
	dir, err := openDirectory(filepath.Dir(path), parentMode, durability)
	return dir, name, err
}

func openDirectory(path string, mode fs.FileMode, durability Durability) (*directory, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err == nil {
		return &directory{fd: fd, path: path}, nil
	}
	if mode == 0 || (err != unix.ENOENT && err != unix.ENOTDIR) || filepath.Dir(path) == path {
		return nil, pathError("open", path, err)
	}
	if err == unix.ENOTDIR {
		if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
			return nil, pathError("mkdir", path, unix.ENOTDIR)
		}
	}
	parent, err := openDirectory(filepath.Dir(path), mode, durability)
	if err != nil {
		return nil, err
	}
	defer parent.close()
	return parent.ensure(filepath.Base(path), DirOptions{Mode: mode, Durability: durability})
}

func openDirectoryAt(parent int, name, path string) (*directory, unix.Stat_t, error) {
	var stat unix.Stat_t
	fd, err := unix.Openat(parent, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, stat, pathError("open", path, err)
	}
	dir := &directory{fd: fd, path: path}
	if err := unix.Fstat(fd, &stat); err != nil {
		dir.close()
		return nil, stat, pathError("lstat", path, err)
	}
	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		return dir, stat, nil
	case unix.S_IFLNK:
		err = ErrSymlink
	default:
		err = ErrNotDirectory
	}
	dir.close()
	return nil, stat, refusal(path, err)
}

func (d *directory) ensure(name string, o DirOptions) (*directory, error) {
	path := filepath.Join(d.path, name)
	mode := unixMode(o.Mode)
	dir, stat, err := openDirectoryAt(d.fd, name, path)
	created := false
	if errors.Is(err, fs.ErrNotExist) {
		if expectedUID(o.Owner) != os.Geteuid() {
			return nil, refusal(path, ErrUnsafeOwner)
		}
		initialMode := mode
		if o.Owner != nil && o.Owner.GID != -1 {
			initialMode &= 0o700
		}
		mkdirErr := unix.Mkdirat(d.fd, name, initialMode)
		if mkdirErr != nil && mkdirErr != unix.EEXIST {
			return nil, pathError("mkdir", path, mkdirErr)
		}
		if mkdirErr == nil {
			created = true
			if err := faultAt(o.Fault, AfterDirCreate); err != nil {
				return nil, err
			}
		}
		dir, stat, err = openDirectoryAt(d.fd, name, path)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if o.Existing != Accept || created {
		if err := dir.finish(stat, mode, o); err != nil {
			dir.close()
			return nil, err
		}
	}
	// Sync the directory inode as well as its entry in the parent. Do this
	// even on an exact match so retry finishes an interrupted sync.
	if err := dir.sync(o.Durability); err != nil {
		dir.close()
		return nil, err
	}
	if err := d.sync(o.Durability); err != nil {
		dir.close()
		return nil, err
	}
	return dir, nil
}

func (d *directory) finish(stat unix.Stat_t, mode uint32, o DirOptions) error {
	if stat.Uid != uint32(expectedUID(o.Owner)) {
		return dirRefusal(d.path, stat, ErrUnsafeOwner)
	}
	actualMode := stat.Mode & 0o7777
	if o.Existing == Finish && actualMode&0o777&^mode != 0 {
		return dirRefusal(d.path, stat, ErrUnsafeMode)
	}
	changeGroup := o.Owner != nil && o.Owner.GID != -1 && int64(stat.Gid) != int64(o.Owner.GID)
	if changeGroup {
		if err := unix.Fchownat(d.fd, "", -1, o.Owner.GID, unix.AT_EMPTY_PATH); err != nil {
			return pathError("chown", d.path, err)
		}
	}
	// The descriptor pins the actual directory, including when its mode is 0000.
	if actualMode != mode || (changeGroup && mode&0o7000 != 0) {
		if err := d.chmod(mode); err != nil {
			return err
		}
		return faultAt(o.Fault, AfterDirChmod)
	}
	return nil
}

func (d *directory) chmod(mode uint32) error {
	err := unix.Fchmodat(d.fd, "", mode, unix.AT_EMPTY_PATH)
	if err == unix.EOPNOTSUPP || err == unix.ENOSYS || err == unix.EINVAL {
		// Linux before fchmodat2 can chmod an O_PATH descriptor through procfs.
		err = unix.Chmod("/proc/self/fd/"+strconv.Itoa(d.fd), mode)
	}
	if err != nil {
		return pathError("chmod", d.path, err)
	}
	return nil
}

func expectedUID(owner *Ownership) int {
	if owner != nil && owner.UID != -1 {
		return owner.UID
	}
	return os.Geteuid()
}

func dirRefusal(path string, stat unix.Stat_t, err error) error {
	mode := fs.FileMode(stat.Mode & 0o777)
	if stat.Mode&unix.S_ISUID != 0 {
		mode |= fs.ModeSetuid
	}
	if stat.Mode&unix.S_ISGID != 0 {
		mode |= fs.ModeSetgid
	}
	if stat.Mode&unix.S_ISVTX != 0 {
		mode |= fs.ModeSticky
	}
	return refusal(path, &DirStateError{Path: path, Mode: mode, UID: stat.Uid, err: err})
}

func unixMode(mode fs.FileMode) uint32 {
	bits := uint32(mode.Perm())
	if mode&fs.ModeSetuid != 0 {
		bits |= unix.S_ISUID
	}
	if mode&fs.ModeSetgid != 0 {
		bits |= unix.S_ISGID
	}
	if mode&fs.ModeSticky != 0 {
		bits |= unix.S_ISVTX
	}
	return bits
}
