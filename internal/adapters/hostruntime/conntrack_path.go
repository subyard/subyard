package hostruntime

import (
	"os"
	"path/filepath"
)

// FindConntrack checks absolute PATH entries first, then the supplied system
// directories. Ignoring relative entries prevents boot from running a binary
// from its current working directory.
func FindConntrack(path string, systemDirectories ...string) (string, bool) {
	for _, directory := range append(filepath.SplitList(path), systemDirectories...) {
		if !filepath.IsAbs(directory) {
			continue
		}
		candidate := filepath.Join(directory, "conntrack")
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, true
		}
	}
	return "", false
}
