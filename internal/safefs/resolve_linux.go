package safefs

import (
	"errors"
	"os"
	"path/filepath"

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
func ResolveFile(path string) (string, error) {
	input := path
	for range 40 {
		target, err := os.Readlink(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.EINVAL) {
				return "", err
			}
			if path == input {
				return path, nil
			}
			directory, name := splitFile(path)
			resolved, err := filepath.EvalSymlinks(directory)
			if errors.Is(err, os.ErrNotExist) {
				return path, nil
			}
			if err != nil {
				return "", err
			}
			return filepath.Join(resolved, name), nil
		}
		if !filepath.IsAbs(target) {
			directory, _ := splitFile(path)
			resolved, err := filepath.EvalSymlinks(directory)
			if err != nil {
				return "", err
			}
			target = resolved + string(os.PathSeparator) + target
		}
		path = target
	}
	return "", pathError("readlink", path, unix.ELOOP)
}

func splitFile(path string) (directory, name string) {
	directory, name = filepath.Split(path)
	if directory == "" {
		directory = "."
	}
	return directory, name
}
