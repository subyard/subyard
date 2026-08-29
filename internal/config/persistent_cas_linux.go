package config

import (
	"bytes"
	"errors"
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
	return compareAndSwapPersistentFile(configHome, path, expected, desired, nil)
}

func compareAndSwapPersistentFile(
	configHome string,
	path string,
	expected PersistentFileSnapshot,
	desired []byte,
	fault func(string) error,
) error {
	if !expected.Exists || expected.Identity == (PersistentFileIdentity{}) {
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
		return err
	}
	defer unix.Close(root)
	current := root
	if err := validatePersistentDirectoryFD(current); err != nil {
		return err
	}
	if err := unix.Flock(root, unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(root, unix.LOCK_UN)
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(
			current, part,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		if openErr != nil {
			if current != root {
				_ = unix.Close(current)
			}
			return openErr
		}
		if err := validatePersistentDirectoryFD(next); err != nil {
			_ = unix.Close(next)
			if current != root {
				_ = unix.Close(current)
			}
			return err
		}
		if current != root {
			_ = unix.Close(current)
		}
		current = next
	}
	if current != root {
		defer unix.Close(current)
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
	if err := preparePersistentPendingAt(current, pending, desired); err != nil {
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
	if err := unix.Renameat(current, pending, current, name); err != nil {
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
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o022 != 0 ||
		stat.Uid != uint32(os.Getuid()) {
		return errors.New("persistent setting directory has unsafe type, mode, or ownership")
	}
	return nil
}

func readPersistentSnapshotAt(parent int, name string) (PersistentFileSnapshot, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return PersistentFileSnapshot{}, nil
	}
	if err != nil {
		return PersistentFileSnapshot{}, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return PersistentFileSnapshot{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o022 != 0 ||
		stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 || stat.Size > 8<<20 {
		return PersistentFileSnapshot{}, errors.New(
			"persistent setting target has unsafe type, mode, ownership, links, or size",
		)
	}
	content, err := io.ReadAll(io.LimitReader(file, (8<<20)+1))
	if err != nil {
		return PersistentFileSnapshot{}, err
	}
	if len(content) > 8<<20 {
		return PersistentFileSnapshot{}, errors.New("persistent setting content exceeds its size bound")
	}
	return PersistentFileSnapshot{
		Exists:  true,
		Content: content,
		Identity: PersistentFileIdentity{
			Device: uint64(stat.Dev), Inode: stat.Ino, Mode: stat.Mode,
			UID: stat.Uid, GID: stat.Gid, Links: uint64(stat.Nlink),
		},
	}, nil
}

func preparePersistentPendingAt(parent int, name string, desired []byte) error {
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
		if !pending.Exists || !bytes.Equal(pending.Content, desired) {
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
