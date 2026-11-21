package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
// Link text is resolved by the kernel, never cleaned lexically: "missing/../x"
// fails as open(2) would instead of naming an existing "x". A target whose
// directory cannot be opened fails with that errno, reported for the input path.
func ResolveFile(path string) (string, error) {
	input := path
	for range 40 {
		target, err := os.Readlink(path)
		if err != nil {
			if !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOTDIR) {
				return "", err
			}
			if path == input {
				if errors.Is(err, unix.ENOTDIR) {
					return "", err
				}
				return path, nil
			}
			directory, name := splitFile(path)
			if name == "" || name == "." || name == ".." {
				return "", pathError("open", input, unix.EISDIR)
			}
			resolved, err := kernelDirectory(directory)
			if err != nil {
				return "", pathError("open", input, err)
			}
			return filepath.Join(resolved, name), nil
		}
		if !filepath.IsAbs(target) {
			directory, _ := splitFile(path)
			target = strings.TrimSuffix(directory, string(os.PathSeparator)) + string(os.PathSeparator) + target
		}
		path = target
	}
	return "", pathError("readlink", path, unix.ELOOP)
}

// kernelDirectory opens a directory path as the kernel resolves it and returns
// its canonical name.
func kernelDirectory(path string) (string, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	return os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
}

func splitFile(path string) (directory, name string) {
	directory, name = filepath.Split(path)
	if directory == "" {
		directory = "."
	}
	return directory, name
}
