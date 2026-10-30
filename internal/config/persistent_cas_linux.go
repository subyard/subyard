package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// CompareAndSwapPersistentFile publishes a complete protected settings file
// only when bytes, inode identity and security metadata still match Inspect.
// Every traversal and the final rename are relative to pinned directory FDs.
func CompareAndSwapPersistentFile(
	configHome string,
	path string,
	expected PersistentFileSnapshot,
	desired []byte,
) error {
	return compareAndSwapPersistentFileGuarded(configHome, path, expected, desired, func() error {
		return checkLocalSettingsTargetWritable(configHome, path)
	}, nil, false)
}

// CompareAndSwapLocalSettingsFile preserves an existing local file's mode and
// can create an exactly absent target without replacing a concurrent file.
func CompareAndSwapLocalSettingsFile(configHome, path string, expected PersistentFileSnapshot, desired []byte) error {
	return compareAndSwapPersistentFileGuarded(configHome, path, expected, desired, func() error { return checkLocalSettingsTargetWritable(configHome, path) }, nil, true)
}

// RemovePersistentFileSnapshot retires only the exact assessed protected file.
// It uses the same root flock and pinned no-follow traversal as publication.
func RemovePersistentFileSnapshot(configHome, path string, expected PersistentFileSnapshot) error {
	if !expected.Exists || expected.Identity == (PersistentFileIdentity{}) {
		return ErrPersistentTargetStale
	}
	parts, err := persistentRelativeParts(configHome, path)
	if err != nil {
		return err
	}
	root, err := unix.Open(filepath.Clean(configHome), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	if err := validatePersistentDirectoryFD(root); err != nil {
		return err
	}
	if err := unix.Flock(root, unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(root, unix.LOCK_UN)
	parent := "."
	if len(parts) > 1 {
		parent = strings.Join(parts[:len(parts)-1], "/")
	}
	fd, err := openPersistentDirectoryAt(root, parent)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	current, err := readPersistentSnapshotAt(fd, parts[len(parts)-1])
	if err != nil {
		return err
	}
	if !samePersistentFileSnapshotExact(current, expected) {
		return ErrPersistentTargetStale
	}
	if err := unix.Unlinkat(fd, parts[len(parts)-1], 0); err != nil {
		return err
	}
	return unix.Fsync(fd)
}

func compareAndSwapPersistentFile(
	configHome string,
	path string,
	expected PersistentFileSnapshot,
	desired []byte,
	fault func(string) error,
) error {
	return compareAndSwapPersistentFileGuarded(configHome, path, expected, desired, nil, fault, false)
}

func compareAndSwapPersistentFileGuarded(
	configHome, path string,
	expected PersistentFileSnapshot,
	desired []byte,
	guard func() error,
	fault func(string) error,
	local bool,
) (result error) {
	defer func() {
		var source *SourceError
		if errors.As(result, &source) && source.Path == "" {
			result = sourceContext(path, 0, "", source.Role, result)
		}
	}()
	if expected.Exists && expected.Identity == (PersistentFileIdentity{}) || !expected.Exists && (!local || expected.Identity != (PersistentFileIdentity{}) || len(expected.Content) != 0) {
		return errors.New("protected persistent CAS requires an exact existing snapshot")
	}
	if len(desired) > 8<<20 {
		return errors.New("persistent setting content exceeds its size bound")
	}
	parts, err := persistentRelativeParts(configHome, path)
	if err != nil {
		return err
	}
	root, err := unix.Open(
		filepath.Clean(configHome),
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return sourceAccessError(configHome, "persistent settings directory", err)
	}
	defer unix.Close(root)
	if err := validatePersistentDirectoryFD(root); err != nil {
		return sourceContext(filepath.Clean(configHome), 0, "", "persistent settings directory", err)
	}
	if err := unix.Flock(root, unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(root, unix.LOCK_UN)
	return compareAndSwapPersistentFileAt(root, filepath.Clean(configHome), parts, expected, desired, guard, fault, local)
}

func compareAndSwapPersistentFileAt(root int, displayRoot string, parts []string, expected PersistentFileSnapshot, desired []byte, guard func() error, fault func(string) error, local bool) error {
	current := root
	defer func() {
		if current != root {
			_ = unix.Close(current)
		}
	}()
	currentPath := displayRoot
	if guard != nil {
		if err := guard(); err != nil {
			return err
		}
	}
	for _, part := range parts[:len(parts)-1] {
		createdParent := false
		next, openErr := unix.Openat(
			current, part,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		if local && !expected.Exists && errors.Is(openErr, unix.ENOENT) {
			if err := unix.Mkdirat(current, part, 0o700); err == nil {
				createdParent = true
			} else if !errors.Is(err, unix.EEXIST) {
				return err
			}
			if err := unix.Fsync(current); err != nil {
				return err
			}
			next, openErr = unix.Openat(current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			return sourceAccessError(filepath.Join(currentPath, part), "persistent settings directory", openErr)
		}
		if createdParent {
			if err := unix.Fchmod(next, 0o700); err != nil {
				unix.Close(next)
				return err
			}
			if err := unix.Fsync(next); err != nil {
				unix.Close(next)
				return err
			}
		}
		nextPath := filepath.Join(currentPath, part)
		if err := validatePersistentDirectoryFD(next); err != nil {
			_ = unix.Close(next)
			return sourceContext(nextPath, 0, "", "persistent settings directory", err)
		}
		if current != root {
			_ = unix.Close(current)
		}
		current = next
		currentPath = nextPath
		if createdParent && fault != nil {
			if err := fault("after-parent-create"); err != nil {
				return err
			}
		}
	}
	name := parts[len(parts)-1]
	observed, err := readPersistentSnapshotAt(current, name)
	if err != nil {
		return err
	}
	if !samePersistentFileSnapshotExact(observed, expected) {
		return ErrPersistentTargetStale
	}
	pending := "." + name + ".release-transition.pending"
	mode := uint32(0o600)
	if local && expected.Exists {
		mode = expected.Identity.Mode & 0o777
	}
	if err := preparePersistentPendingAt(current, pending, desired, mode); err != nil {
		return err
	}
	if fault != nil {
		if err := fault("after-pending-fsync"); err != nil {
			return err
		}
	}
	observed, err = readPersistentSnapshotAt(current, name)
	if err != nil {
		return err
	}
	if !samePersistentFileSnapshotExact(observed, expected) {
		return ErrPersistentTargetStale
	}
	if fault != nil {
		if err := fault("before-publish"); err != nil {
			return err
		}
	}
	if local && !expected.Exists {
		if err := unix.Renameat2(current, pending, current, name, unix.RENAME_NOREPLACE); err != nil {
			if errors.Is(err, unix.EEXIST) {
				return ErrPersistentTargetStale
			}
			return err
		}
	} else if err := unix.Renameat(current, pending, current, name); err != nil {
		return err
	}
	if fault != nil {
		if err := fault("after-publish-before-dir-fsync"); err != nil {
			return err
		}
	}
	return unix.Fsync(current)
}

func persistentRelativeParts(configHome, path string) ([]string, error) {
	if !filepath.IsAbs(configHome) || !filepath.IsAbs(path) {
		return nil, errors.New("persistent setting paths must be absolute")
	}
	root := filepath.Clean(configHome)
	relative, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, errors.New("persistent setting path escaped the configuration root")
	}
	parts := strings.Split(relative, string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, 0) {
			return nil, errors.New("persistent setting path has an unsafe component")
		}
	}
	return parts, nil
}

func validatePersistentDirectoryFD(fd int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	metadata := fmt.Sprintf("observed type=%#o, mode=%04o, uid=%d, links=%d", stat.Mode&unix.S_IFMT, stat.Mode&0777, stat.Uid, stat.Nlink)
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return sourceError("", 0, "", "persistent settings directory", fmt.Sprintf("directory has unsafe type; %s; required type=%#o", metadata, unix.S_IFDIR), nil)
	}
	if stat.Mode&0o022 != 0 {
		return sourceError("", 0, "", "persistent settings directory", "directory is group/world writable; "+metadata+"; required writable mask=0000 (mask=0022)", nil)
	}
	if stat.Uid != uint32(os.Getuid()) {
		return sourceError("", 0, "", "persistent settings directory", fmt.Sprintf("directory is not operator-owned; %s; required uid=%d", metadata, os.Getuid()), nil)
	}
	return nil
}

func readPersistentSnapshotAt(parent int, name string) (PersistentFileSnapshot, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return PersistentFileSnapshot{}, nil
	}
	if err != nil {
		return PersistentFileSnapshot{}, sourceAccessError("", "persistent settings", err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return PersistentFileSnapshot{}, sourceAccessError("", "persistent settings", err)
	}
	identity := PersistentFileIdentity{
		Device: uint64(stat.Dev), Inode: stat.Ino, Mode: stat.Mode,
		UID: stat.Uid, GID: stat.Gid, Links: uint64(stat.Nlink),
	}
	metadata := fmt.Sprintf("observed type=%#o, mode=%04o, uid=%d, links=%d", identity.Mode&unix.S_IFMT, identity.Mode&0777, identity.UID, identity.Links)
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return PersistentFileSnapshot{}, sourceError("", 0, "", "persistent settings", fmt.Sprintf("target must be a regular non-symlink file; %s; required type=%#o", metadata, unix.S_IFREG), nil)
	}
	if stat.Mode&0o022 != 0 {
		return PersistentFileSnapshot{}, sourceError("", 0, "", "persistent settings", "target is group/world writable; "+metadata+"; required writable mask=0000 (mask=0022)", nil)
	}
	if stat.Uid != uint32(os.Getuid()) {
		return PersistentFileSnapshot{}, sourceError("", 0, "", "persistent settings", fmt.Sprintf("target is not operator-owned; %s; required uid=%d", metadata, os.Getuid()), nil)
	}
	if stat.Nlink != 1 {
		return PersistentFileSnapshot{}, sourceError("", 0, "", "persistent settings", "target has hard links; "+metadata+"; required links=1", nil)
	}
	if stat.Size > 8<<20 {
		return PersistentFileSnapshot{}, sourceError("", 0, "", "persistent settings", fmt.Sprintf("target exceeds its size bound; observed bytes=%d, required maximum=%d", stat.Size, 8<<20), nil)
	}
	content, err := io.ReadAll(io.LimitReader(file, (8<<20)+1))
	if err != nil {
		return PersistentFileSnapshot{}, sourceAccessError("", "persistent settings", err)
	}
	if len(content) > 8<<20 {
		return PersistentFileSnapshot{}, sourceError("", 0, "", "persistent settings", fmt.Sprintf("target exceeds its size bound; observed bytes=%d, required maximum=%d", len(content), 8<<20), nil)
	}
	return PersistentFileSnapshot{
		Exists: true, Content: content, Identity: identity,
	}, nil
}

func preparePersistentPendingAt(parent int, name string, desired []byte, mode uint32) error {
	fd, err := unix.Openat(
		parent, name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if errors.Is(err, unix.EEXIST) {
		pending, readErr := readPersistentSnapshotAt(parent, name)
		if readErr != nil {
			return readErr
		}
		if !pending.Exists || (!bytes.Equal(pending.Content, desired) || pending.Identity.Mode&0o777 != mode) {
			return ErrPersistentTargetStale
		}
		return nil
	}
	if err != nil {
		return err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = unix.Close(fd)
		}
	}()
	if err := unix.Fchmod(fd, mode); err != nil {
		return err
	}
	for remaining := desired; len(remaining) > 0; {
		written, writeErr := unix.Write(fd, remaining)
		if writeErr != nil {
			return writeErr
		}
		remaining = remaining[written:]
	}
	if err := unix.Fsync(fd); err != nil {
		return err
	}
	if err := unix.Close(fd); err != nil {
		return err
	}
	closeOnError = false
	return unix.Fsync(parent)
}

func samePersistentFileSnapshotExact(left, right PersistentFileSnapshot) bool {
	return left.Exists == right.Exists && left.Identity == right.Identity &&
		bytes.Equal(left.Content, right.Content)
}
