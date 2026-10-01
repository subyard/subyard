package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/config"
)

func (cli *CLI) runConfigGitAuthoring(ctx context.Context, loaded config.Loaded, request configAuthoringRequest, content []byte) int {
	return cli.runConfigSyncPushRequest(ctx, loaded, configSyncPushOptions{
		message:   fmt.Sprintf("%s %s in %s settings", request.action, request.name, request.scope),
		authoring: &request, content: content,
	}, request.assumeYes)
}

// exportSelectedConfig edits only the explicitly selected source setting. Local
// overrides, other hosts and runtime state never participate in this export.
func (cli *CLI) exportSelectedConfig(loaded config.Loaded, candidate, hostID string, request configAuthoringRequest, content []byte) ([]string, error) {
	root := filepath.Join(candidate, "hosts", hostID)
	switch request.scope {
	case config.ScopeShared:
		root = filepath.Join(candidate, "shared")
	case config.ScopeYard:
		root = filepath.Join(root, "yards", loaded.Context.YardName)
	}
	path := filepath.Join(root, "config.env")
	var paths []string
	if request.action == "set" || request.action == "unset" {
		snapshot, err := readConfigAuthoringTarget(path)
		if err != nil {
			return nil, err
		}
		var value *string
		if request.action == "set" {
			value = &request.value
		}
		if !snapshot.Exists && value == nil {
			return nil, nil
		}
		content, err = config.EditPersistentAssignmentContent(path, snapshot.Content, request.name, value)
		if err != nil {
			return nil, err
		}
	} else {
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
		path = filepath.Join(root, "overrides", relative)
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
