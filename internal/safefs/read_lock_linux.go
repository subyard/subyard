package safefs

import (
	"context"
	"io"
	"io/fs"
	"math"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

type ReadOptions struct {
	MaxBytes   int64 // Zero is unbounded.
	OwnerUID   *uint32
	DenyPerm   fs.FileMode
	SingleLink bool
}

// OpenRegular opens a regular file without following its final component.
func OpenRegular(path string) (*os.File, error) {
	file, _, err := openRegular(path)
	return file, err
}

func openRegular(path string) (*os.File, unix.Stat_t, error) {
	var stat unix.Stat_t
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err == unix.ELOOP {
		return nil, stat, refusal(path, ErrSymlink)
	}
	if err != nil {
		return nil, stat, pathError("open", path, err)
	}
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, stat, pathError("fstat", path, err)
	}
	if err := regular(stat.Mode); err != nil {
		_ = unix.Close(fd)
		return nil, stat, refusal(path, err)
	}
	return os.NewFile(uintptr(fd), path), stat, nil
}

func ReadRegular(path string, o ReadOptions) ([]byte, error) {
	file, stat, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if o.OwnerUID != nil && stat.Uid != *o.OwnerUID {
		return nil, refusal(path, ErrUnsafeOwner)
	}
	if o.SingleLink && stat.Nlink != 1 {
		return nil, refusal(path, ErrMultipleLinks)
	}
	if stat.Mode&unixMode(o.DenyPerm) != 0 {
		return nil, refusal(path, ErrUnsafeMode)
	}
	if o.MaxBytes <= 0 || o.MaxBytes == math.MaxInt64 {
		return io.ReadAll(file)
	}
	body, err := io.ReadAll(io.LimitReader(file, o.MaxBytes+1))
	if int64(len(body)) > o.MaxBytes {
		return nil, refusal(path, ErrTooLarge)
	}
	return body, err
}

// LockFile holds a flock and its descriptor until Release.
type LockFile struct{ file *os.File }

func OpenLock(path string, mode fs.FileMode) (*LockFile, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return nil, pathError("open", path, err)
	}
	return &LockFile{file: os.NewFile(uintptr(fd), path)}, nil
}

func (l *LockFile) Stat() (fs.FileInfo, error) { return l.file.Stat() }

func (l *LockFile) Chmod(mode fs.FileMode) error { return l.file.Chmod(mode) }

// Identity identifies the actual lock inode, including through filesystem aliases.
func (l *LockFile) Identity() ([2]uint64, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(l.file.Fd()), &stat); err != nil {
		return [2]uint64{}, pathError("fstat", l.file.Name(), err)
	}
	return [2]uint64{stat.Dev, stat.Ino}, nil
}

// Acquire uses a blocking flock if poll is zero, or waits with cancellation otherwise.
func (l *LockFile) Acquire(ctx context.Context, shared bool, poll time.Duration) error {
	operation := unix.LOCK_EX
	if shared {
		operation = unix.LOCK_SH
	}
	fd := int(l.file.Fd())
	if poll <= 0 {
		return unix.Flock(fd, operation)
	}
	for {
		err := unix.Flock(fd, operation|unix.LOCK_NB)
		if err != unix.EWOULDBLOCK {
			return err
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(poll):
		}
	}
}

func (l *LockFile) Release() {
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	_ = l.file.Close()
}

func Lock(ctx context.Context, path string, shared bool, poll time.Duration) (*LockFile, error) {
	lock, err := OpenLock(path, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lock.Acquire(ctx, shared, poll); err != nil {
		lock.Release()
		return nil, err
	}
	return lock, nil
}
