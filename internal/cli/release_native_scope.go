package cli

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

// coldInstallationEntries is everything a cold installation may keep in its
// configuration home: release bookkeeping written by the first-install
// transition and host-wide settings. Any other entry, such as the owner HostID,
// yard registrations and config files, Git settings, applied-state records or
// keys, is yard state. A nil child set admits a regular file.
var coldInstallationEntries = map[string]map[string]bool{
	"config.env":         nil,
	"migrations":         {"update.lock": true},
	"overrides":          {"host": true, "shared": true},
	"release-transition": {"v2": true},
}

// coldInstallation proves that this installation owns no native yard state,
// so release activation has nothing to observe in Incus. Native yards are
// created only by init, which records owner state before creating them;
// a native instance without that record is foreign until init adopts it.
// Every unreadable or unexpected fact makes the installation warm.
func coldInstallation(configHome, runtimeRoot string) bool {
	if configHome == "" || !filepath.IsAbs(configHome) {
		return false
	}
	entries, err := os.ReadDir(configHome)
	if errors.Is(err, os.ErrNotExist) {
		entries, err = nil, nil
	}
	if err != nil {
		return false
	}
	for _, entry := range entries {
		allowed, known := coldInstallationEntries[entry.Name()]
		if !known {
			return false
		}
		if allowed == nil {
			if !entry.Type().IsRegular() {
				return false
			}
			continue
		}
		if !entry.IsDir() {
			return false
		}
		children, err := os.ReadDir(filepath.Join(configHome, entry.Name()))
		if err != nil {
			return false
		}
		for _, child := range children {
			if !allowed[child.Name()] {
				return false
			}
		}
	}
	if runtimeRoot != "" {
		// A retained previous release proves history beyond the first install.
		if _, err := os.Lstat(filepath.Join(runtimeRoot, "previous")); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		return false
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return false
	}
	if !snapshot.Exists {
		return true
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil {
		return false
	}
	// Only a first install activates its own release with no predecessor.
	releases := journal.Releases
	return releases.Previous == nil && releases.From == releases.Target &&
		journal.Goal.Direction == releasetransition.DirectionActivateTarget
}
