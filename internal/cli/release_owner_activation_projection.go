package cli

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

func (c *releaseOwnerComponents) preparedSourceFiles() *config.ProspectiveFiles {
	return &config.ProspectiveFiles{Root: c.context.ConfigHome, PinnedRoot: c.root, ReadSnapshot: c.ReadSnapshot, ReadPreparedInput: func(path string) (config.PersistentFileSnapshot, bool, error) {
		if c.activationDraft != nil && c.activationDraft.projection.Sources[path] != "" {
			snapshot, matched, err := c.activationDraft.readPreparedInput(path)
			if err != nil || snapshot.Exists {
				return snapshot, matched, err
			}
			relative, err := filepath.Rel(c.context.ConfigHome, path)
			if err != nil || relative == ".." || strings.HasPrefix(relative, "../") {
				return config.PersistentFileSnapshot{}, false, err
			}
			snapshot, err = config.ReadPersistentSnapshotAt(c.root, filepath.ToSlash(relative))
			snapshot.Identity = config.PersistentFileIdentity{}
			return snapshot, true, err
		}
		if reader, ok := c.prospective.(releasetransition.V2PreparedInputReader); ok {
			return reader.ReadPreparedInput(path)
		}
		return config.PersistentFileSnapshot{}, false, nil
	}}
}

func (c *releaseOwnerComponents) ProjectActivation(ctx context.Context, before releaseowner.OwnerBefore) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	original := releasetransition.OwnerRegistrationObservation{State: releasetransition.OwnerRegistrationState(before.State), Registration: releasetransition.Fingerprint(before.Registration), Overrides: releasetransition.Fingerprint(before.Overrides), Controller: releasetransition.Fingerprint(before.Controller), SharedImages: before.SharedImages}
	draft, err := c.cli.prepareSourceActivationDraft(c.context.ConfigHome, releasetransition.V2SourceActivationView{Settings: c, OwnerRegistration: &original})
	if err != nil {
		return nil, err
	}
	c.activationDraft = draft
	c.activationOwner = &original
	return slices.Clone(draft.names), nil
}

func (c *releaseOwnerComponents) activationFiles() *config.ProspectiveFiles {
	if c.activationDraft != nil && !c.activationActual {
		return c.activationDraft.inputs
	}
	return c.view()
}

func (c *releaseOwnerComponents) activationYards() []string {
	if c.activationDraft != nil {
		return c.activationDraft.names
	}
	return c.yards
}

func (c *releaseOwnerComponents) loadActivation(yard string) (config.Loaded, error) {
	if loaded, ok := c.loaded[yard]; ok {
		return loaded, nil
	}
	if c.activationDraft == nil || c.activationActual {
		return c.load(yard)
	}
	root := c.context.PhysicalRepositoryRoot
	if root == "" {
		root = c.context.RepositoryRoot
	}
	operator := c.cli.env["SUBYARD_OPERATOR_HOME"]
	if operator == "" {
		operator = c.cli.env["HOME"]
	}
	if operator == "" {
		operator = c.context.ConfigHome
	}
	return config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: operator, YardName: yard, DisablePrivate: true, ConfigLocked: true, ProspectiveFiles: c.activationDraft.inputs, FileOverrides: c.activationDraft.overrides, Environment: map[string]string{"SUBYARD_HOME": c.context.DataHome, "SUBYARD_CONFIG_HOME": c.context.ConfigHome, "SUBYARD_CONFIG_DIR": root + "/config"}})
}
