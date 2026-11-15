package migration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func safeRelativePath(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\n\r\t") {
		return false
	}
	clean := filepath.Clean(path)
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func validateOwnedSafeMode(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("source-install input is not operator-owned: %s", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("source-install input is group/world writable: %s", path)
	}
	return nil
}
