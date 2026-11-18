package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/config"
)

func (cli *CLI) runConfigGitAuthoring(ctx context.Context, loaded config.Loaded, requests []configAuthoringRequest, content []byte) int {
	return cli.runPreparedConfigMutation(ctx, loaded, []string{requests[0].action}, requests[0].assumeYes, func(prepared *preparedCommand) error {
		return prepared.prepareConfigPush(ctx, configSyncPushOptions{message: configGitAuthoringMessage(requests), authoring: requests, content: content})
	})
}

// exportSelectedConfig edits only the explicitly selected source setting. Local
// overrides, other hosts and runtime state never participate in this export.
func (cli *CLI) exportSelectedConfig(loaded config.Loaded, candidate, hostID string, requests []configAuthoringRequest, content []byte) ([]string, error) {
	request := requests[0]
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
		if request.action == "set" {
			content, err = cli.catalog.EditPersistentAssignmentsContent(path, snapshot.Content, configSetAssignments(requests))
		} else {
			content, err = cli.catalog.EditPersistentAssignmentContent(path, snapshot.Content, request.name, nil)
		}
		if err != nil || bytes.Equal(content, snapshot.Content) {
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
