package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// PersistentBinding contains no setting values. It binds exact bytes and security
// identity for a data-only precondition under a caller-owned config-root lock.
type PersistentBinding struct {
	Exists   bool                   `json:"exists"`
	Digest   string                 `json:"digest"`
	Identity PersistentFileIdentity `json:"identity"`
}
type PersistentPrecondition struct {
	Relative string            `json:"relative"`
	Binding  PersistentBinding `json:"binding"`
}
type PersistentDirectoryBinding struct {
	Relative string                 `json:"relative"`
	Identity PersistentFileIdentity `json:"identity"`
	Entries  []string               `json:"entries"`
}

func BindPersistentSnapshot(s PersistentFileSnapshot) PersistentBinding {
	sum := sha256.Sum256(s.Content)
	return PersistentBinding{s.Exists, hex.EncodeToString(sum[:]), s.Identity}
}
func rootParts(relative string) ([]string, error) {
	if relative == "" || filepath.IsAbs(relative) || filepath.Clean(relative) != relative || relative == "." {
		return nil, errors.New("invalid rooted configuration target")
	}
	parts := strings.Split(relative, "/")
	for _, p := range parts {
		if p == ".." || p == "" || strings.ContainsRune(p, 0) {
			return nil, errors.New("invalid rooted configuration component")
		}
	}
	return parts, nil
}
func openPersistentDirectoryAt(root int, relative string) (int, error) {
	if err := validatePersistentDirectoryFD(root); err != nil {
		return -1, err
	}
	parts := []string{}
	var err error
	if relative != "." {
		parts, err = rootParts(relative)
		if err != nil {
			return -1, err
		}
	}
	fd, err := unix.Openat(root, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range parts {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return -1, e
		}
		fd = next
		if e = validatePersistentDirectoryFD(fd); e != nil {
			unix.Close(fd)
			return -1, e
		}
	}
	return fd, nil
}

// ReadPersistentSnapshotAt traverses every component relative to the pinned FD.
func ReadPersistentSnapshotAt(root *os.File, relative string) (PersistentFileSnapshot, error) {
	parts, err := rootParts(relative)
	if err != nil {
		return PersistentFileSnapshot{}, err
	}
	parent := "."
	if len(parts) > 1 {
		parent = strings.Join(parts[:len(parts)-1], "/")
	}
	fd, err := openPersistentDirectoryAt(int(root.Fd()), parent)
	if errors.Is(err, unix.ENOENT) {
		return PersistentFileSnapshot{}, nil
	}
	if err != nil {
		return PersistentFileSnapshot{}, err
	}
	defer unix.Close(fd)
	return readPersistentSnapshotAt(fd, parts[len(parts)-1])
}
func ReadPersistentDirectoryAt(root *os.File, relative string) (PersistentDirectoryBinding, error) {
	fd, err := openPersistentDirectoryAt(int(root.Fd()), relative)
	if err != nil {
		return PersistentDirectoryBinding{}, err
	}
	f := os.NewFile(uintptr(fd), relative)
	defer f.Close()
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return PersistentDirectoryBinding{}, err
	}
	entries, err := f.Readdirnames(-1)
	if err != nil {
		return PersistentDirectoryBinding{}, err
	}
	slices.Sort(entries)
	return PersistentDirectoryBinding{relative, PersistentFileIdentity{Device: uint64(st.Dev), Inode: st.Ino, Mode: st.Mode, UID: st.Uid, GID: st.Gid, Links: uint64(st.Nlink)}, entries}, nil
}

// CompareAndSwapLocalSettingsAt borrows a pinned config-root FD whose exclusive
// flock is owned by the caller. It never acquires/releases that lock or reopens
// the root by path. Preconditions and target CAS share that exact root/lock.
// Cooperating writers must use the same root lock; flock cannot fence arbitrary
// external writers. The final precondition check narrows, but cannot remove, that
// external race. Existing path-based callers retain their existing behavior.
func CompareAndSwapLocalSettingsAt(root *os.File, relative string, expected PersistentBinding, desired []byte, files []PersistentPrecondition, directories []PersistentDirectoryBinding, excluded []string) error {
	if len(desired) > 8<<20 {
		return errors.New("persistent setting content exceeds its size bound")
	}
	parts, err := rootParts(relative)
	if err != nil {
		return err
	}
	guard := func() error {
		for _, p := range files {
			s, e := ReadPersistentSnapshotAt(root, p.Relative)
			if e != nil {
				return e
			}
			if BindPersistentSnapshot(s) != p.Binding {
				return ErrPersistentTargetStale
			}
		}
		for _, d := range directories {
			got, e := ReadPersistentDirectoryAt(root, d.Relative)
			if e != nil {
				return e
			}
			got.Entries = slices.DeleteFunc(got.Entries, func(name string) bool {
				return slices.Contains(excluded, filepath.ToSlash(filepath.Join(d.Relative, name)))
			})
			if got.Identity != d.Identity || !slices.Equal(got.Entries, d.Entries) {
				return ErrPersistentTargetStale
			}
		}
		return nil
	}
	if err = guard(); err != nil {
		return err
	}
	current, err := ReadPersistentSnapshotAt(root, relative)
	if err != nil {
		return err
	}
	if BindPersistentSnapshot(current) != expected {
		return ErrPersistentTargetStale
	}
	// Recheck all captured input bytes on the same pinned root immediately before
	// pending publication, in addition to the CAS's exact target identity checks.
	return compareAndSwapPersistentFileAt(int(root.Fd()), "<pinned configuration root>", parts, current, desired, guard, func(point string) error {
		if point == "before-publish" {
			return guard()
		}
		return nil
	}, true)
}
