package networkruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Subyard/Subyard/internal/operatoraccess"
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

// EnsureHostLockOperator grants the captured operator access to only the fixed
// policy lock after validating the already approved administrator membership.
func EnsureHostLockOperator(ctx context.Context, uid string) error {
	gid, err := operatoraccess.ApprovedActor(uid)
	if err != nil {
		return err
	}
	if err := ensureHostLockAt(hostLockDirectory, 0, int(gid)); err != nil {
		return err
	}
	if uid == "0" {
		return nil
	}
	return grantHostLockAccessAt(ctx, hostLockDirectory, 0, int(gid), uid, operatoraccess.NativeACL)
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

	if err := flockHostLock(ctx, lockFD); err != nil {
		_ = unix.Close(lockFD)
		return nil, err
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			_ = unix.Flock(lockFD, unix.LOCK_UN)
			_ = unix.Close(lockFD)
		})
	}, nil
}

func flockHostLock(ctx context.Context, lockFD int) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("acquire host network policy lock: %w", ctx.Err())
		default:
		}

		err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("acquire host network policy lock: %w", err)
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
			return fmt.Errorf("acquire host network policy lock: %w", ctx.Err())
		case <-timer.C:
		}
	}

	return nil
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

// Pin each ancestor before native tools access the fixed lock descriptor. The
// root-owned sticky /run/lock parent is allowed; the native child stays 0755.
func grantHostLockAccessAt(ctx context.Context, directory string, ownerUID, groupGID int, uid string, tools operatoraccess.Tools) error {
	clean := filepath.Clean(directory)
	if !filepath.IsAbs(clean) || clean == "/" {
		return errors.New("invalid host network lock path")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	ancestors := map[string]unix.Stat_t{}
	path := "/"
	for _, component := range strings.Split(strings.TrimPrefix(clean, "/"), "/") {
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			return err
		}
		if stat.Uid != 0 && int(stat.Uid) != ownerUID || stat.Mode&0022 != 0 && (stat.Uid != 0 || stat.Mode&unix.S_ISVTX == 0) {
			return errors.New("host network lock ancestor has unsafe ownership or permissions")
		}
		ancestors[path] = stat
		next, err := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return errors.New("host network lock ancestor cannot be pinned")
		}
		_ = unix.Close(fd)
		fd = next
		path = filepath.Join(path, component)
	}
	if err := validateHostLockDirectoryFD(fd, ownerUID); err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	ancestors[clean] = stat
	lockFD, err := unix.Openat(fd, hostLockName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("pin host network policy lock: %w", err)
	}
	file := os.NewFile(uintptr(lockFD), "network-policy-lock")
	defer file.Close()
	if err := validateHostLockFD(lockFD, ownerUID, groupGID); err != nil {
		return err
	}
	var approved unix.Stat_t
	if err := unix.Fstat(lockFD, &approved); err != nil {
		return err
	}
	if err := flockHostLock(ctx, lockFD); err != nil {
		return err
	}
	check := func() error {
		for path, before := range ancestors {
			var current unix.Stat_t
			if err := unix.Lstat(path, &current); err != nil || !sameHostLockIdentity(before, current) {
				return errors.New("host network lock ancestor changed during operator grant")
			}
		}
		var current, pinned unix.Stat_t
		if err := unix.Fstat(lockFD, &pinned); err != nil || !sameHostLockIdentity(approved, pinned) {
			return errors.New("native network lock identity or permissions changed")
		}
		if err := unix.Lstat(filepath.Join(clean, hostLockName), &current); err != nil || !sameHostLockIdentity(approved, current) {
			return errors.New("host network lock changed during operator grant")
		}
		return nil
	}
	return operatoraccess.GrantRW(ctx, file, uid, check, tools)
}

func sameHostLockIdentity(before, after unix.Stat_t) bool {
	return before.Dev == after.Dev && before.Ino == after.Ino && before.Uid == after.Uid && before.Gid == after.Gid && before.Mode == after.Mode && (before.Mode&unix.S_IFMT != unix.S_IFREG || before.Nlink == after.Nlink)
}
