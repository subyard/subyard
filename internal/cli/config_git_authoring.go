package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
)

func (cli *CLI) runConfigGitAuthoring(ctx context.Context, loaded config.Loaded, requests []configAuthoringRequest, content []byte) int {
	return cli.runPreparedConfigMutation(ctx, loaded, []string{requests[0].action}, requests[0].assumeYes, func(prepared *preparedCommand) error {
		return prepared.prepareConfigPush(ctx, configSyncPushOptions{message: configGitAuthoringMessage(requests), authoring: requests, content: content})
	})
}

func exportSelectedScalars(baseline configsync.Plan, scope config.SettingScope, yard string, edits config.PersistentScalarEdits) ([]string, error) {
	path, content, err := baseline.EditScalarSettings(scope, yard, edits)
	if err != nil || path == "" {
		return nil, err
	}
	relative, err := filepath.Rel(baseline.SourceRoot, path)
	if err != nil {
		return nil, err
	}
	if err := writeExportedConfigFile(path, content, 0o600); err != nil {
		return nil, err
	}
	return []string{filepath.ToSlash(relative)}, nil
}

// exportSelectedFile edits only the selected source file setting.
func (cli *CLI) exportSelectedFile(loaded config.Loaded, candidate, hostID string, request configAuthoringRequest, content []byte) ([]string, error) {
	root := filepath.Join(candidate, "hosts", hostID)
	switch request.scope {
	case config.ScopeShared:
		root = filepath.Join(candidate, "shared")
	case config.ScopeYard:
		root = filepath.Join(root, "yards", loaded.Context.YardName)
	}
	var paths []string
	local, err := cli.configFileAuthoringPath(loaded, request)
	if err != nil {
		return nil, err
	}
	var localRoot string
	switch request.scope {
	case config.ScopeShared:
		localRoot = filepath.Join(loaded.Context.Paths.ConfigHome, "overrides", "shared")
	case config.ScopeHost:
		localRoot = filepath.Join(loaded.Context.Paths.ConfigHome, "overrides", "host")
	case config.ScopeYard:
		localRoot = filepath.Join(loaded.Context.Paths.ConfigHome, "yards", loaded.Context.YardName, "overrides")
	}
	relative, err := filepath.Rel(localRoot, local)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, "overrides", relative)
	// A named source yard needs a definition, even when its only selected
	// change is a file setting. Preserve it as an empty registration.
	if request.scope == config.ScopeYard && loaded.Context.YardName != "default" {
		definition := filepath.Join(root, "config.env")
		if _, err := os.Lstat(definition); errors.Is(err, os.ErrNotExist) {
			if err := writeExportedConfigFile(definition, nil, 0o600); err != nil {
				return nil, err
			}
			paths = append(paths, definition)
		} else if err != nil {
			return nil, err
		}
	}
	if err := writeExportedConfigFile(path, content, 0o600); err != nil {
		return nil, err
	}
	paths = append(paths, path)
	for index, path := range paths {
		relative, err := filepath.Rel(candidate, path)
		if err != nil {
			return nil, err
		}
		paths[index] = filepath.ToSlash(relative)
	}
	return paths, nil
}
