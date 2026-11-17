package config

import (
	"bytes"
	"fmt"
	"path/filepath"
)

// PowerPathsWrite records command power paths as desired host configuration.
// Like integration selection, it is retained if later reconciliation fails.
type PowerPathsWrite struct {
	Path        string
	Before      PersistentFileSnapshot
	Content     []byte
	Assignments []string
}

func (catalog Catalog) PlanCommandPowerPathsWrite(loaded Loaded, environment map[string]string) (*PowerPathsWrite, error) {
	names := []string{"SUBYARD_POWER_RECONCILER_PATH", "SUBYARD_POWER_UNIT_PATH"}
	if environment[names[0]] == "" && environment[names[1]] == "" {
		return nil, nil
	}
	root := loaded.Context.Paths.ConfigHome
	if err := CheckLocalSettingsWritable(root); err != nil {
		return nil, err
	}
	path := filepath.Join(root, "config.env")
	before, err := readOptionalIntegrationSnapshot(root, path)
	if err != nil {
		return nil, err
	}
	content := before.Content
	var assignments []string
	for _, name := range names {
		if environment[name] == "" {
			continue
		}
		value := loaded.Environment[name]
		assignments = append(assignments, fmt.Sprintf("%s=%q", name, value))
		content, err = catalog.EditPersistentAssignmentContent(path, content, name, &value)
		if err != nil {
			return nil, err
		}
	}
	if bytes.Equal(content, before.Content) {
		return nil, nil
	}
	return &PowerPathsWrite{Path: path, Before: before, Content: content, Assignments: assignments}, nil
}

func (plan *PowerPathsWrite) Apply() error {
	if plan == nil {
		return nil
	}
	root := filepath.Dir(plan.Path)
	if err := ensurePersistentRoot(root); err != nil {
		return err
	}
	return CompareAndSwapLocalSettingsFile(root, plan.Path, plan.Before, plan.Content)
}
