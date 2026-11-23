package releasetransition

import (
	"bytes"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/config"
)

// Older settings migrations bind the effective host inputs. A proven dead
// assignment has no effect before or after the activation owner removes it.
// Its raw bytes and proof dependencies are bound by that owner's observation.
func effectiveSettingsSnapshot(home, configDir, path string, snapshot config.PersistentFileSnapshot, view V2SettingsSnapshotView) (config.PersistentFileSnapshot, error) {
	if path != filepath.Join(home, "config.env") || configDir == "" || (!bytes.Contains(snapshot.Content, []byte("$SUBYARD_CONFIG_DIR/../private/agents/")) && !bytes.Contains(snapshot.Content, []byte("${SUBYARD_CONFIG_DIR}/../private/agents/"))) {
		return snapshot, nil
	}
	catalog, err := config.LoadCatalog(filepath.Dir(configDir))
	if err != nil {
		return snapshot, err
	}
	var files *config.ProspectiveFiles
	if view != nil {
		files = &config.ProspectiveFiles{Root: home, ReadSnapshot: view.ReadSnapshot}
	}
	plan, err := catalog.PlanDeadAssignments(home, configDir, files)
	if err != nil {
		return snapshot, err
	}
	if !bytes.Equal(snapshot.Content, plan.Expected.Content) || snapshot.Exists != plan.Expected.Exists {
		return snapshot, config.ErrPersistentTargetStale
	}
	if len(plan.Removed) != 0 {
		snapshot.Content = plan.Desired
	}
	return snapshot, nil
}
