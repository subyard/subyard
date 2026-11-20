package safefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type ExistingDir uint8

const (
	Finish ExistingDir = iota // Finish narrower modes on owned directories.
	Repair                    // Also repair broader modes on owned directories.
	Exact                     // Require exact metadata on existing directories.
)

type DirOptions struct {
	Mode       fs.FileMode // Zero defaults to 0700.
	Owner      *Ownership
	Existing   ExistingDir
	ParentMode fs.FileMode
	Root       string
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
	if o.Mode == 0 {
		o.Mode = 0o700
	}
	parent, name, err := openParent(path, o.Root, o.ParentMode, o.Durability, o.Owner)
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

func openParent(path, root string, parentMode fs.FileMode, durability Durability, owner *Ownership) (*directory, string, error) {
	name := filepath.Base(path)
	if name == "." || name == ".." || name == string(filepath.Separator) || strings.HasSuffix(path, string(filepath.Separator)) {
		return nil, "", pathError("open", path, fs.ErrInvalid)
	}
	parentPath := filepath.Dir(path)
	if root == "" {
		dir, err := openDirectory(parentPath, parentMode, durability)
		return dir, name, err
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, "", err
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	relative, err := filepath.Rel(absoluteRoot, absolutePath)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, "", refusal(path, ErrEscapesRoot)
	}
	// The caller supplies a trusted root. Validate only its descendants.
	dir, _, err := openDirectoryAt(unix.AT_FDCWD, absoluteRoot, absoluteRoot)
	if err != nil {
		return nil, "", err
	}
	parts := strings.Split(relative, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		childPath := filepath.Join(dir.path, part)
		child, stat, err := openDirectoryAt(dir.fd, part, childPath)
		if errors.Is(err, fs.ErrNotExist) && parentMode != 0 {
			if parentMode&0o022 != 0 {
				err = refusal(childPath, ErrUnsafeMode)
			} else {
				child, err = dir.ensure(part, DirOptions{Mode: parentMode, Owner: owner, Durability: durability})
			}
		} else if err == nil {
			switch {
			case stat.Uid != uint32(expectedUID(owner)):
				err = dirRefusal(childPath, stat, ErrUnsafeOwner)
			case stat.Mode&0o022 != 0:
				err = dirRefusal(childPath, stat, ErrUnsafeMode)
			}
			if err != nil {
				child.close()
			}
		}
		dir.close()
		if err != nil {
			return nil, "", err
		}
		dir = child
	}
	return dir, name, nil
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
		initialMode := mode
		if o.Owner != nil && (o.Owner.UID != -1 || o.Owner.GID != -1) {
			initialMode &= 0o700
		}
		mkdirErr := unix.Mkdirat(d.fd, name, initialMode)
		created = mkdirErr == nil
		if mkdirErr != nil && mkdirErr != unix.EEXIST {
			return nil, pathError("mkdir", path, mkdirErr)
		}
		if created {
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
	if err := dir.finish(stat, mode, created, o); err != nil {
		dir.close()
		return nil, err
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

func (d *directory) finish(stat unix.Stat_t, mode uint32, created bool, o DirOptions) error {
	uid, gid := expectedUID(o.Owner), -1
	if o.Owner != nil {
		gid = o.Owner.GID
	}
	actualMode := stat.Mode & 0o7777
	changeUID := stat.Uid != uint32(uid)
	changeGID := gid != -1 && stat.Gid != uint32(gid)
	if changeUID && (!created || os.Geteuid() != 0 || stat.Uid != 0) {
		return dirRefusal(d.path, stat, ErrUnsafeOwner)
	}
	if !created && (o.Existing == Exact && actualMode != mode || o.Existing == Finish && actualMode&^mode != 0) {
		return dirRefusal(d.path, stat, ErrUnsafeMode)
	}
	if !created && o.Existing == Exact && changeGID {
		return dirRefusal(d.path, stat, ErrUnsafeOwner)
	}
	if changeUID || changeGID {
		if !changeUID {
			uid = -1
		}
		if err := unix.Fchownat(d.fd, "", uid, gid, unix.AT_EMPTY_PATH); err != nil {
			return pathError("chown", d.path, err)
		}
	}
	// The descriptor pins the actual directory, including when its mode is 0000.
	if created || actualMode != mode || ((changeUID || changeGID) && mode&0o7000 != 0) {
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
