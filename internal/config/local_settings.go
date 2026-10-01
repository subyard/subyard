package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// CheckLocalSettingsWritable prevents authoring into the old shared local/Git
// storage before sync has transactionally separated its ownership.
func CheckLocalSettingsWritable(configHome string) error {
	pending, err := PendingConfigurationTransaction(configHome)
	if err != nil {
		return err
	}
	if pending {
		return errors.New("an interrupted configuration transaction requires recovery with yard config sync")
	}
	path := filepath.Join(configHome, ".sync", "manifest.json")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	snapshot, err := ReadPersistentFileSnapshot(configHome, path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !snapshot.Exists {
		return err
	}
	var manifest struct {
		SchemaVersion int               `json:"schemaVersion"`
		Files         []json.RawMessage `json:"files"`
	}
	if err := json.Unmarshal(snapshot.Content, &manifest); err != nil {
		return err
	}
	switch manifest.SchemaVersion {
	case 1:
		if len(manifest.Files) != 0 {
			return errors.New("run yard config sync to migrate Git settings before saving local settings")
		}
	case 2:
	default:
		return errors.New("unsupported configuration sync manifest schema")
	}
	return nil
}

// Runtime locks and profile-owned state do not participate in Git settings.
func checkLocalSettingsTargetWritable(configHome, path string) error {
	relative, err := filepath.Rel(configHome, path)
	if err != nil {
		return err
	}
	relative = filepath.ToSlash(relative)
	if relative == "config.env" || relative == "overrides/shared/config.env" ||
		(strings.HasPrefix(relative, "yards/") && filepath.Base(path) == "config.env") ||
		strings.HasPrefix(relative, "overrides/shared/agents/") ||
		strings.HasPrefix(relative, "overrides/host/agents/") ||
		(strings.HasPrefix(relative, "yards/") && strings.Contains(relative, "/overrides/agents/")) {
		return CheckLocalSettingsWritable(configHome)
	}
	return nil
}
