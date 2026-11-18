package configsync

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

// EditScalarSettings prepares the entire batch against the registered owner's
// source and local layers. The caller holds the configuration root lock from
// the baseline plan through its replacement. An empty path means no change.
func (plan Plan) EditScalarSettings(scope config.SettingScope, yard string, edits config.PersistentScalarEdits) (string, []byte, error) {
	root := plan.SourceRoot
	host := filepath.Join(root, "hosts", plan.HostID)
	path := filepath.Join(host, "config.env")
	target := "config.env"
	switch scope {
	case config.ScopeShared:
		path = filepath.Join(root, "shared", "config.env")
		target = "overrides/shared/config.env"
	case config.ScopeYard:
		path = filepath.Join(host, "yards", yard, "config.env")
		target = filepath.ToSlash(filepath.Join("yards", yard, "config.env"))
	}
	key := gitSettingsPath(target)
	file := plan.desired[key]

	options := plan.options
	options.inspectionOnly = true
	options.scalarEditPath = path
	catalog, err := config.LoadCatalog(options.RepositoryRoot)
	if err != nil {
		return "", nil, err
	}
	source := sourceSnapshot{root: root, hostID: plan.HostID, files: maps.Clone(plan.desired)}
	for key := range source.files {
		if name, ok := deletedYardDefinition(key); ok {
			source.yardNames = append(source.yardNames, name)
		}
	}
	affected := func(loaded config.Loaded) bool { return scope != config.ScopeYard || loaded.Context.YardName == yard }

	var before []config.Loaded
	if err := validateCandidate(options, source, plan.previous, catalog, &before); err != nil {
		return "", nil, err
	}
	before = slices.DeleteFunc(before, func(loaded config.Loaded) bool { return !affected(loaded) })
	content, err := edits.LayerContent(path, file.Content, before)
	if err != nil || bytes.Equal(content, file.Content) {
		return "", nil, err
	}
	draft, err := os.CreateTemp("/tmp", "subyard-config-draft-*")
	if err != nil {
		return "", nil, err
	}
	defer os.Remove(draft.Name())
	_, err = draft.Write(content)
	if closeErr := draft.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", nil, err
	}
	file.SourcePath = path
	source.files[key] = file
	options.validationOverrides = map[string]string{path: draft.Name()}
	var after []config.Loaded
	validationErr := validateCandidate(options, source, plan.previous, catalog, &after)
	baselines := make(map[string]config.Loaded, len(before))
	for _, loaded := range before {
		baselines[loaded.Context.YardName] = loaded
	}
	refusals := []error{validationErr}
	for _, loaded := range after {
		if baseline, ok := baselines[loaded.Context.YardName]; ok {
			refusals = append(refusals, edits.CheckLayer(path, baseline, loaded))
		}
	}
	if err := domain.CollectRefusals(refusals...); err != nil {
		return "", nil, err
	}
	return path, content, nil
}
