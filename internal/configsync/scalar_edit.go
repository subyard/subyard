package configsync

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

// EditScalarSettings prepares a batch against the original Git source bytes.
// The caller holds the configuration root lock while preparing the edit.
func (plan Plan) EditScalarSettings(scope config.SettingScope, yard string, edits config.PersistentScalarEdits) (string, []byte, error) {
	root := plan.SourceRoot
	host := filepath.Join(root, "hosts", plan.HostID)
	path := filepath.Join(host, "config.env")
	switch scope {
	case config.ScopeShared:
		path = filepath.Join(root, "shared", "config.env")
	case config.ScopeYard:
		path = filepath.Join(host, "yards", yard, "config.env")
	}
	before, err := readSourceFile(root, path, maxConfigBytes, false)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	label, err := filepath.Rel(root, path)
	if err != nil {
		return "", nil, err
	}
	content, editErr := edits.LayerContent(label, before)
	refusals := []error{editErr}
	checked := map[string]bool{path: true}
	for _, name := range slices.Sorted(maps.Keys(plan.scalarLayers)) {
		if scope == config.ScopeYard && name != yard {
			continue
		}
		for _, layer := range plan.scalarLayers[name] {
			if !layer.Present || checked[layer.Path] {
				continue
			}
			var payload []byte
			label := layer.Path
			var err error
			switch layer.Role {
			case "Git scalar settings":
				payload, err = readSourceFile(root, layer.Path, maxConfigBytes, false)
				if err == nil {
					label, err = filepath.Rel(root, layer.Path)
				}
			case "scalar settings":
				var snapshot config.PersistentFileSnapshot
				snapshot, err = config.ReadPersistentFileSnapshot(plan.options.ConfigHome, layer.Path)
				payload = snapshot.Content
			default:
				continue
			}
			checked[layer.Path] = true
			if err == nil {
				err = edits.CheckLayer(label, payload)
			}
			refusals = append(refusals, err)
		}
	}
	if err := domain.CollectRefusals(refusals...); err != nil {
		return "", nil, err
	}
	if bytes.Equal(content, before) {
		return "", nil, nil
	}
	return path, content, nil
}
