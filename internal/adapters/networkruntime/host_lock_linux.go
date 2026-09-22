package networkruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	hostLockDirectory = "/run/lock/subyard-network"
	hostLockName      = "policy.lock"
	hostLockPoll      = 25 * time.Millisecond
)

// HostLock serializes physical-host network policy changes and managed yard starts.
type HostLock struct{}

// Acquire waits for the initialized host lock without creating or repairing it.
func (HostLock) Acquire(ctx context.Context) (func(), error) {
	gid, err := incusAdminGID()
	if err != nil {
		return nil, err
	}
	release, err := acquireHostLockAt(ctx, hostLockDirectory, 0, gid)
	if err != nil {
		return nil, err
	}
	return release, nil
}

// EnsureHostLock initializes the fixed host lock during root-owned provisioning.
func EnsureHostLock() error {
	if os.Geteuid() != 0 {
		return errors.New("initializing the host network policy lock requires root")
	}
	gid, err := incusAdminGID()
	if err != nil {
		return err
	}
	return ensureHostLockAt(hostLockDirectory, 0, gid)
}

// CheckHostLock validates the fixed host lock without creating or locking it.
func CheckHostLock() error {
	gid, err := incusAdminGID()
	if err != nil {
		return err
	}
	return checkHostLockAt(hostLockDirectory, 0, gid)
}

func incusAdminGID() (int, error) {
	group, err := user.LookupGroup("incus-admin")
	if err != nil {
		return 0, fmt.Errorf("resolve incus-admin group: %w", err)
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil || gid < 0 {
		return 0, fmt.Errorf("resolve incus-admin group: invalid gid %q", group.Gid)
	}
	return gid, nil
}

func ensureHostLockAt(directory string, ownerUID, groupGID int) error {
	directoryFD, _, err := openHostLockDirectory(directory, ownerUID, true)
	if err != nil {
		return err
	}
	defer unix.Close(directoryFD) //nolint:errcheck

	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_CREAT | unix.O_EXCL
	lockFD, err := unix.Openat(directoryFD, hostLockName, flags, 0o660)
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		lockFD, err = unix.Openat(
			directoryFD,
			hostLockName,
			unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
	}
	if err != nil {
		return fmt.Errorf("open host network policy lock: %w", err)
	}
	defer unix.Close(lockFD) //nolint:errcheck

	if created {
		if err := unix.Fchown(lockFD, ownerUID, groupGID); err != nil {
			_ = unix.Unlinkat(directoryFD, hostLockName, 0)
			return fmt.Errorf("set host network policy lock ownership: %w", err)
		}
		if err := unix.Fchmod(lockFD, 0o660); err != nil {
			_ = unix.Unlinkat(directoryFD, hostLockName, 0)
			return fmt.Errorf("set host network policy lock mode: %w", err)
		}
	}
	if err := validateHostLockFD(lockFD, ownerUID, groupGID); err != nil {
		if created {
			_ = unix.Unlinkat(directoryFD, hostLockName, 0)
		}
		return err
	}
	return nil
}

func checkHostLockAt(directory string, ownerUID, groupGID int) error {
	directoryFD, _, err := openHostLockDirectory(directory, ownerUID, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return hostLockNotInitialized(err)
		}
		return err
	}
	defer unix.Close(directoryFD) //nolint:errcheck

	lockFD, err := unix.Openat(
		directoryFD,
		hostLockName,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return hostLockNotInitialized(err)
		}
		return fmt.Errorf("open host network policy lock: %w", err)
	}
	defer unix.Close(lockFD) //nolint:errcheck
	return validateHostLockFD(lockFD, ownerUID, groupGID)
}

func acquireHostLockAt(
	ctx context.Context,
	directory string,
	ownerUID int,
	groupGID int,
) (func(), error) {
	if ctx == nil {
		return nil, errors.New("host network policy lock context is required")
	}
	directoryFD, _, err := openHostLockDirectory(directory, ownerUID, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, hostLockNotInitialized(err)
		}
		return nil, err
	}
	defer unix.Close(directoryFD) //nolint:errcheck

	lockFD, err := unix.Openat(
		directoryFD,
		hostLockName,
		unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, hostLockNotInitialized(err)
		}
		return nil, fmt.Errorf("open host network policy lock: %w", err)
	}
	if err := validateHostLockFD(lockFD, ownerUID, groupGID); err != nil {
		_ = unix.Close(lockFD)
		return nil, err
	}

	for {
		select {
		case <-ctx.Done():
			_ = unix.Close(lockFD)
			return nil, fmt.Errorf("acquire host network policy lock: %w", ctx.Err())
		default:
		}

		err = unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			_ = unix.Close(lockFD)
			return nil, fmt.Errorf("acquire host network policy lock: %w", err)
		}
		timer := time.NewTimer(hostLockPoll)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			_ = unix.Close(lockFD)
			return nil, fmt.Errorf("acquire host network policy lock: %w", ctx.Err())
		case <-timer.C:
		}
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			_ = unix.Flock(lockFD, unix.LOCK_UN)
			_ = unix.Close(lockFD)
		})
	}, nil
}

func hostLockNotInitialized(err error) error {
	return fmt.Errorf("host network policy lock is not initialized; run 'yard init': %w", err)
}

func openHostLockDirectory(directory string, ownerUID int, create bool) (int, bool, error) {
	clean := filepath.Clean(directory)
	parent, name := filepath.Split(clean)
	if clean == "." || clean == string(filepath.Separator) || name == "" || name == "." || name == ".." {
		return -1, false, errors.New("invalid host network policy lock directory")
	}
	parent = filepath.Clean(parent)
	parentFD, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, false, fmt.Errorf("open host network policy lock parent: %w", err)
	}
	defer unix.Close(parentFD) //nolint:errcheck

	created := false
	if create {
		err = unix.Mkdirat(parentFD, name, 0o755)
		if err == nil {
			created = true
		} else if !errors.Is(err, unix.EEXIST) {
			return -1, false, fmt.Errorf("create host network policy lock directory: %w", err)
		}
	}
	directoryFD, err := unix.Openat(
		parentFD,
		name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return -1, created, fmt.Errorf("open host network policy lock directory: %w", err)
	}
	if created {
		if err := unix.Fchmod(directoryFD, 0o755); err != nil {
			_ = unix.Close(directoryFD)
			_ = unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
			return -1, true, fmt.Errorf("set host network policy lock directory mode: %w", err)
		}
	}
	if err := validateHostLockDirectoryFD(directoryFD, ownerUID); err != nil {
		_ = unix.Close(directoryFD)
		if created {
			_ = unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
		}
		return -1, created, err
	}
	return directoryFD, created, nil
}

func validateHostLockDirectoryFD(fd int, ownerUID int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("inspect host network policy lock directory: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("host network policy lock directory must be a real directory")
	}
	if stat.Mode&0o7777 != 0o755 {
		return fmt.Errorf("host network policy lock directory must have mode 0755, got %04o", stat.Mode&0o7777)
	}
	if int(stat.Uid) != ownerUID {
		return fmt.Errorf("host network policy lock directory must be owned by uid %d, got %d", ownerUID, stat.Uid)
	}
	return nil
}

func validateHostLockFD(fd int, ownerUID, groupGID int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("inspect host network policy lock: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("host network policy lock must be a regular file")
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("host network policy lock must have a single link, got %d", stat.Nlink)
	}
	if stat.Mode&0o7777 != 0o660 {
		return fmt.Errorf("host network policy lock must have mode 0660, got %04o", stat.Mode&0o7777)
	}
	if int(stat.Uid) != ownerUID || int(stat.Gid) != groupGID {
		return fmt.Errorf(
			"host network policy lock must be owned by uid %d and gid %d, got uid %d and gid %d",
			ownerUID,
			groupGID,
			stat.Uid,
			stat.Gid,
		)
	}
	return nil
}
