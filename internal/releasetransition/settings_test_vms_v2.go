package releasetransition

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

const maximumSettingsV2Yards = 128

// Frozen typed recovery before-values; no transform code uses them.
type settingsV2BeforeValue struct {
	Setting string `json:"setting"`
	Present bool   `json:"present"`
	Value   string `json:"value,omitempty"`
}

func affectedSettingsV2ID(name string) bool {
	return name == "YARD_TEMPLATE" || name == "NESTED_E2E_VMS"
}

type posixV2SettingsSnapshotView struct{ configHome string }

func (view posixV2SettingsSnapshotView) ReadSnapshot(
	path string,
) (config.PersistentFileSnapshot, error) {
	return config.ReadPersistentFileSnapshot(view.configHome, path)
}

func (view posixV2SettingsSnapshotView) ListYards() ([]string, error) {
	yardsRoot := filepath.Join(view.configHome, "yards")
	directory, err := os.OpenFile(
		yardsRoot,
		os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW,
		0,
	)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 || !ok ||
		stat.Uid != uint32(os.Getuid()) {
		return nil, errors.New("persistent yards directory is unsafe")
	}
	entries, err := directory.ReadDir((maximumSettingsV2Yards * 2) + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > maximumSettingsV2Yards*2 {
		return nil, errors.New("too many persistent yard settings entries")
	}
	names := make(map[string]struct{})
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".env")
		if domain.SafeName(name) {
			names[name] = struct{}{}
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	if len(result) > maximumSettingsV2Yards {
		return nil, errors.New("too many persistent yards")
	}
	return result, nil
}
