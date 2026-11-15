package reconcileruntime

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall"
)

func nativeInstalledFilesLabeled(paths []string, label func(string) string) (string, error) {
	facts := map[string]any{}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			facts[label(path)] = "absent"
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", errors.New("unsafe installed host power file")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return "", errors.New("host power file identity unavailable")
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		facts[label(path)] = []any{stat.Dev, stat.Ino, stat.Uid, stat.Gid, stat.Mode, stat.Size, stat.Mtim, stat.Ctim, fmt.Sprintf("%x", sha256.Sum256(payload))}
	}
	raw, err := json.Marshal(facts)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}
