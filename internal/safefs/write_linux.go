// Package safefs atomically publishes files, resolving every name in the
// publication through one pinned parent descriptor. It never follows a
// final-component symlink and sets the staging file's owner and mode through
// its descriptor before publication.
//
// The trust boundary is the host. A principal with write access to the parent
// directory can replace its entries at any time. Ancestor paths are trusted.
package safefs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type Durability uint8

const (
	Durable Durability = iota
	BestEffort
)

// Ownership sets the uid and gid before publication. A value of -1 keeps that ID.
type Ownership struct{ UID, GID int }

// Linux reserves the all-ones uid_t and gid_t values for the -1 sentinel.
func validOwnerID(id int) bool {
	return id >= -1 && int64(id) < math.MaxUint32
}

type Options struct {
	Mode        fs.FileMode // Zero defaults to 0600 unless ExactMode is set.
	ExactMode   bool
	TempPattern string      // An explicit pattern without '*' is a fixed staging name.
	ParentMode  fs.FileMode // Zero requires the parent to exist.
	Owner       *Ownership
	NoReplace   bool
	Durability  Durability
	Fault       func(Point) error // Test-only interruption hook.
}

type Point string

const (
	AfterStage     Point = "after-stage"
	AfterWrite     Point = "after-write"
	BeforePublish  Point = "before-publish"
	AfterPublish   Point = "after-publish"
	AfterDirCreate Point = "after-dir-create"
	AfterDirChmod  Point = "after-dir-chmod"
)

var (
	ErrCrash         = errors.New("safefs: simulated crash")
	ErrSymlink       = errors.New("symbolic link")
	ErrNotRegular    = errors.New("not a regular file")
	ErrNotDirectory  = errors.New("not a directory")
	ErrUnsafeOwner   = errors.New("unsafe ownership")
	ErrUnsafeMode    = errors.New("unsafe permissions")
	ErrMultipleLinks = errors.New("multiple hard links")
	ErrTooLarge      = errors.New("file exceeds size limit")
)

func pathError(op, path string, err error) error {
	return &fs.PathError{Op: op, Path: path, Err: err}
}

func refusal(path string, err error) error {
	return pathError("safefs", path, err)
}

func faultAt(fault func(Point) error, point Point) error {
	if fault == nil {
		return nil
	}
	return fault(point)
}

// Pending owns a staging file and its parent descriptor. Defer Abort immediately
// after Create, including when the caller intends to publish the file.
type Pending struct {
	parent             *directory
	file               *os.File
	identity           unix.Stat_t
	path, name, stage  string
	mode               fs.FileMode
	options            Options
	published, crashed bool
}

func WriteFile(path string, data []byte, o Options) error {
	return WriteFrom(path, o, func(file *os.File) error {
		_, err := file.Write(data)
		return err
	})
}

func WriteFrom(path string, o Options, fill func(*os.File) error) error {
	pending, err := Create(path, o)
	if err != nil {
		return err
	}
	defer pending.Abort()
	if err := fill(pending.File()); err != nil {
		return err
	}
	return pending.Commit()
}

func Create(path string, o Options) (*Pending, error) {
	if owner := o.Owner; owner != nil && (!validOwnerID(owner.UID) || !validOwnerID(owner.GID)) {
		return nil, pathError("chown", path, unix.EINVAL)
	}
	parent, name, err := openParent(path, o.ParentMode, o.Durability)
	if err != nil {
		return nil, err
	}
	p := &Pending{parent: parent, path: path, name: name, options: o}
	if err := p.create(); err != nil {
		p.Abort()
		return nil, err
	}
	return p, nil
}

func (p *Pending) create() error {
	mode := p.options.Mode
	if mode == 0 && !p.options.ExactMode {
		mode = 0o600
	}
	pattern := p.options.TempPattern
	if pattern == "" {
		pattern = "." + p.name + ".tmp-*"
	}
	if filepath.Base(pattern) != pattern || pattern == "." || pattern == ".." || pattern == p.name {
		return pathError("open", filepath.Join(p.parent.path, pattern), fs.ErrInvalid)
	}
	if err := p.stageFile(pattern); err != nil {
		return err
	}
	if !p.options.NoReplace {
		if err := p.checkTarget(); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if owner := p.options.Owner; owner != nil {
		if err := p.file.Chown(owner.UID, owner.GID); err != nil {
			return err
		}
	}
	if err := p.file.Chmod(mode); err != nil {
		return err
	}
	p.mode = mode
	return p.fault(AfterStage)
}

func (p *Pending) stageFile(pattern string) error {
	star := strings.LastIndexByte(pattern, '*')
	for {
		name := pattern
		if star >= 0 {
			var random [8]byte
			rand.Read(random[:])
			name = pattern[:star] + hex.EncodeToString(random[:]) + pattern[star+1:]
		}
		if name == p.name {
			continue
		}
		path := filepath.Join(p.parent.path, name)
		fd, err := unix.Openat(p.parent.fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err == nil {
			p.stage, p.file = name, os.NewFile(uintptr(fd), path)
			if err := unix.Fstat(fd, &p.identity); err != nil {
				return pathError("fstat", path, err)
			}
			return nil
		}
		if star < 0 || err != unix.EEXIST {
			return pathError("open", path, err)
		}
	}
}

func (p *Pending) File() *os.File { return p.file }
func (p *Pending) Name() string   { return p.file.Name() }

// Publish syncs and closes the staging file before making it visible. Callers
// that close File themselves are responsible for its final mode and sync.
func (p *Pending) Publish() error {
	if p.published {
		return nil
	}
	if p.parent == nil || p.crashed {
		return pathError("rename", p.path, fs.ErrClosed)
	}
	if err := p.fault(AfterWrite); err != nil {
		return err
	}
	if p.file.Fd() != ^uintptr(0) {
		// Writing can clear set-ID bits on a staging file.
		if p.mode&(fs.ModeSetuid|fs.ModeSetgid) != 0 {
			if err := p.file.Chmod(p.mode); err != nil {
				return err
			}
		}
		if p.options.Durability == Durable {
			if err := p.file.Sync(); err != nil {
				return err
			}
		}
		if err := p.file.Close(); err != nil {
			return err
		}
	}
	if err := p.fault(BeforePublish); err != nil {
		return err
	}
	if err := p.rename(); err != nil {
		return err
	}
	return p.fault(AfterPublish)
}

func (p *Pending) rename() error {
	fd := p.parent.fd
	if p.options.NoReplace {
		err := unix.Renameat2(fd, p.stage, fd, p.name, unix.RENAME_NOREPLACE)
		if err == unix.EINVAL || err == unix.ENOSYS || err == unix.EOPNOTSUPP {
			if err := unix.Linkat(fd, p.stage, fd, p.name, 0); err != nil {
				return &os.LinkError{Op: "link", Old: p.Name(), New: p.path, Err: err}
			}
			p.published = true
			if err := unix.Unlinkat(fd, p.stage, 0); err != nil {
				return pathError("remove", p.Name(), err)
			}
			p.stage = ""
			return nil
		}
		if err != nil {
			return &os.LinkError{Op: "rename", Old: p.Name(), New: p.path, Err: err}
		}
	} else {
		// Recheck after filling: a new symlink is never followed, even if it races rename.
		if err := p.checkTarget(); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := unix.Renameat(fd, p.stage, fd, p.name); err != nil {
			return &os.LinkError{Op: "rename", Old: p.Name(), New: p.path, Err: err}
		}
	}
	p.published, p.stage = true, ""
	return nil
}

func (p *Pending) matchesStage(name string) bool {
	var stat unix.Stat_t
	return unix.Fstatat(p.parent.fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW) == nil &&
		stat.Dev == p.identity.Dev && stat.Ino == p.identity.Ino
}

// Published reports whether the publication succeeded, even if Commit
// subsequently failed to sync the parent.
func (p *Pending) Published() bool { return p.published }

type directoryDestinationError struct{}

func (directoryDestinationError) Error() string        { return unix.EEXIST.Error() }
func (directoryDestinationError) Unwrap() error        { return unix.EEXIST }
func (directoryDestinationError) Is(target error) bool { return target == ErrNotRegular }

func (p *Pending) checkTarget() error {
	var target unix.Stat_t
	if err := unix.Fstatat(p.parent.fd, p.name, &target, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return pathError("lstat", p.path, err)
	}
	if target.Mode&unix.S_IFMT == unix.S_IFDIR {
		// os.Rename reports EEXIST when replacing a directory with a file.
		return &os.LinkError{Op: "rename", Old: p.Name(), New: p.path, Err: directoryDestinationError{}}
	}
	if err := regular(target.Mode); err != nil {
		return refusal(p.path, err)
	}
	return nil
}

func (p *Pending) SyncParent() error {
	if p.parent == nil || p.crashed {
		return pathError("sync", p.path, fs.ErrClosed)
	}
	return p.parent.sync(p.options.Durability)
}

func (p *Pending) Commit() error {
	if err := p.Publish(); err != nil {
		return err
	}
	return p.SyncParent()
}

func (p *Pending) Abort() {
	if p.parent == nil {
		return
	}
	if p.stage != "" && !p.crashed && p.matchesStage(p.stage) {
		_ = unix.Unlinkat(p.parent.fd, p.stage, 0)
	}
	if p.file != nil {
		_ = p.file.Close()
	}
	p.parent.close()
	p.parent = nil
}

func (p *Pending) fault(point Point) error {
	err := faultAt(p.options.Fault, point)
	p.crashed = errors.Is(err, ErrCrash)
	return err
}

func regular(mode uint32) error {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return nil
	case unix.S_IFLNK:
		return ErrSymlink
	default:
		return ErrNotRegular
	}
}
