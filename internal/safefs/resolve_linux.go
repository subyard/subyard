package safefs

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ResolveFile resolves a user-controlled file path before staging beside its
// target. The final target file may be absent.
func ResolveFile(path string) (string, error) {
	for range 40 {
		directory, name := filepath.Split(path)
		if directory == "" {
			directory = "."
		}
		resolved, err := filepath.EvalSymlinks(directory)
		if err == nil {
			path = filepath.Join(resolved, name)
		} else if errors.Is(err, os.ErrNotExist) {
			return path, nil
		} else {
			return "", err
		}
		target, err := os.Readlink(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.EINVAL) {
				return path, nil
			}
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = resolved + string(os.PathSeparator) + target
		}
		path = target
	}
	return "", pathError("readlink", path, unix.ELOOP)
}
