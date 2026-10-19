package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ProspectiveFiles reads a validated planned result inside one persistent root.
// A zero identity denotes a virtual file whose source security was checked by
// its planning owner; existing files retain their protected snapshot metadata.
type ProspectiveFiles struct {
	Root         string
	Yards        []string
	ReadSnapshot func(string) (PersistentFileSnapshot, error)
	// ReadPreparedInput keeps uncaptured paths on their native reader. Captured
	// snapshots retain the same bounds and identity validation as ReadSnapshot.
	ReadPreparedInput func(string) (PersistentFileSnapshot, bool, error)
	// PinnedRoot is supplied only by the native component owner. It keeps all
	// persistent identity revalidation on the same opened root used for capture
	// and guarded CAS. Ordinary callers retain path-based validation.
	PinnedRoot *os.File
}

func (files *ProspectiveFiles) snapshot(path string) (PersistentFileSnapshot, bool, error) {
	if files == nil {
		return PersistentFileSnapshot{}, false, nil
	}
	root := filepath.Clean(files.Root)
	if root != files.Root || !filepath.IsAbs(root) || root == "/" || files.ReadSnapshot == nil {
		return PersistentFileSnapshot{}, false, errors.New("invalid prospective configuration root")
	}
	clean := filepath.Clean(path)
	if strings.HasPrefix(path, root+string(filepath.Separator)) && clean != path {
		return PersistentFileSnapshot{}, true, errors.New("invalid prospective configuration path")
	}
	if !strings.HasPrefix(clean, root+string(filepath.Separator)) {
		return PersistentFileSnapshot{}, false, nil
	}
	if clean != path {
		return PersistentFileSnapshot{}, true, errors.New("invalid prospective configuration path")
	}
	var snapshot PersistentFileSnapshot
	var err error
	if files.ReadPreparedInput != nil {
		var matched bool
		snapshot, matched, err = files.ReadPreparedInput(path)
		if !matched && err == nil {
			return PersistentFileSnapshot{}, false, nil
		}
	} else {
		snapshot, err = files.ReadSnapshot(path)
	}
	if err != nil {
		return PersistentFileSnapshot{}, true, err
	}
	identity := snapshot.Identity
	if len(snapshot.Content) > 32<<20 || (!snapshot.Exists && (len(snapshot.Content) != 0 || identity != (PersistentFileIdentity{}))) {
		return PersistentFileSnapshot{}, true, errors.New("invalid prospective configuration snapshot")
	}
	if identity != (PersistentFileIdentity{}) {
		var current PersistentFileSnapshot
		var err error
		if files.PinnedRoot != nil {
			current, err = ReadPersistentSnapshotAt(files.PinnedRoot, strings.TrimPrefix(path, root+string(filepath.Separator)))
		} else {
			current, err = ReadPersistentFileSnapshot(root, path)
		}
		if err != nil {
			return PersistentFileSnapshot{}, true, err
		}
		if !samePersistentFileSnapshot(current, snapshot) {
			return PersistentFileSnapshot{}, true, errors.New("prospective configuration snapshot changed")
		}
	}
	return snapshot, true, nil
}

func (files *ProspectiveFiles) fallbackReset(root, yard string) (bool, error) {
	path := YardResetMarkerPath(root, yard)
	marker, handled, err := files.snapshot(path)
	if err != nil {
		return false, err
	}
	if !handled {
		return YardFallbackReset(root, yard)
	}
	if !marker.Exists {
		return false, nil
	}
	if marker.Identity.Mode&0o777 != 0o600 || string(marker.Content) != yardResetMarkerContent {
		return false, errors.New("unsafe prospective yard reset marker")
	}
	return true, nil
}
func (files *ProspectiveFiles) pendingTransaction(root string) (bool, error) {
	value, handled, err := files.snapshot(filepath.Join(root, ".sync", "transaction.json"))
	if err != nil {
		return false, err
	}
	if !handled {
		return PendingConfigurationTransaction(root)
	}
	return value.Exists, nil
}

func (files *ProspectiveFiles) readFile(path string) ([]byte, error) {
	snapshot, handled, err := files.snapshot(path)
	if err != nil {
		return nil, err
	}
	if !handled {
		return os.ReadFile(path)
	}
	if !snapshot.Exists {
		return nil, os.ErrNotExist
	}
	return snapshot.Content, nil
}

// ReadAsset preserves logical source paths and ordinary shipped/external policy.
func (files *ProspectiveFiles) ReadAsset(asset MaterializedAsset) ([]byte, error) {
	snapshot, handled, err := files.snapshot(asset.Source)
	if err != nil {
		return nil, err
	}
	if !handled {
		return asset.ReadSource()
	}
	if !snapshot.Exists {
		return nil, os.ErrNotExist
	}
	return snapshot.Content, nil
}

// Snapshot returns a validated projected file and whether the path belongs to
// this view. Callers keep their ordinary ownership checks for external paths.
func (files *ProspectiveFiles) Snapshot(path string) (PersistentFileSnapshot, bool, error) {
	return files.snapshot(path)
}
