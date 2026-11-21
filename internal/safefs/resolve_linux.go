package safefs

import (
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// EntryID identifies a named entry in a directory, including when the parent
// is reached through another pathname or a bind mount.
type EntryID struct {
	Dev, Ino uint64
	Name     string
}

func EntryIdentity(path string) (EntryID, error) {
	parent, name, err := openParent(path, 0, BestEffort)
	if err != nil {
		return EntryID{}, err
	}
	defer parent.close()
	var stat unix.Stat_t
	if err := unix.Fstat(parent.fd, &stat); err != nil {
		return EntryID{}, pathError("fstat", parent.path, err)
	}
	return EntryID{Dev: stat.Dev, Ino: stat.Ino, Name: name}, nil
}

// ResolveFile follows final symlinks for a user-controlled path. If the input
// names an absent entry or any non-symlink, it is returned unchanged.
//
// The kernel resolves every directory: each link is read through a descriptor
// of the directory that holds it, and only the last component of its text is
// split off. Text that names a directory returns that directory, which the
// caller refuses by type. A missing or unusable target directory fails as
// "open <input>: errno".
func ResolveFile(path string) (string, error) { return resolveFile(path, false) }

// ResolveFileForCreate is ResolveFile for a writer that creates the target's
// missing parent. A missing target directory is accepted when the link text is
// clean, so it names the same entry lexically and in the kernel once created.
func ResolveFileForCreate(path string) (string, error) { return resolveFile(path, true) }

func resolveFile(input string, createsParent bool) (string, error) {
	directory, name := splitFile(input)
	fd, err := unix.Open(directory, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err == unix.ENOENT {
		return input, nil
	}
	if err != nil {
		return "", pathError("readlink", input, err)
	}
	defer func() { _ = unix.Close(fd) }()
	followed := false
	text := make([]byte, unix.PathMax)
	for range 40 {
		n, err := unix.Readlinkat(fd, name, text)
		if err == unix.EINVAL || err == unix.ENOENT {
			if !followed {
				return input, nil
			}
			return canonicalEntry(fd, name)
		}
		if err != nil {
			return "", pathError("readlink", input, err)
		}
		followed = true
		target := string(text[:n])
		targetDirectory, targetName := splitFile(target)
		if targetName == "" || targetName == "." || targetName == ".." {
			// The text names a directory. The caller refuses it by type.
			targetDirectory, targetName = target, ""
		}
		next, err := unix.Openat(fd, targetDirectory, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err == unix.ENOENT && createsParent && targetName != "" && filepath.Clean(target) == target {
			if filepath.IsAbs(target) {
				return target, nil
			}
			return canonicalEntry(fd, target)
		}
		if err != nil {
			return "", pathError("open", input, err)
		}
		_ = unix.Close(fd)
		fd = next
		if targetName == "" {
			return canonicalEntry(fd, "")
		}
		name = targetName
	}
	return "", pathError("readlink", input, unix.ELOOP)
}

// canonicalEntry names an entry of the directory that fd holds.
func canonicalEntry(fd int, name string) (string, error) {
	directory, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, name), nil
}

func splitFile(path string) (directory, name string) {
	directory, name = filepath.Split(path)
	if directory == "" {
		directory = "."
	}
	return directory, name
}
