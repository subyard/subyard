package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

// SourceRecordRelativePath is shared with the versioned configuration owner.
const SourceRecordRelativePath = ".sync/source.json"

// YardIntegrationWrite is a bounded prepared edit of the selected local yard
// settings file. A Git-only registration remains the source of inherited settings.
type YardIntegrationWrite struct {
	Path         string
	Before       PersistentFileSnapshot
	Content      []byte
	SourcePath   string
	SourceBefore PersistentFileSnapshot
	root         string
	yard         string
}

func PlanYardIntegrationWrite(loaded Loaded, requested []string) (*YardIntegrationWrite, error) {
	name := loaded.Context.YardName
	if name == "" {
		name = "default"
	}
	if !domain.SafeName(name) {
		return nil, errors.New("unsafe yard name")
	}
	root := loaded.Context.Paths.ConfigHome
	if err := CheckLocalSettingsWritable(root); err != nil {
		return nil, err
	}
	path, err := LocalYardSettingsFile(filepath.Join(root, "yards"), name)
	if err != nil {
		return nil, err
	}
	plan := &YardIntegrationWrite{Path: path, root: root, yard: name}
	plan.Before, err = readOptionalIntegrationSnapshot(root, path)
	if err != nil {
		return nil, err
	}
	if name != "default" && !plan.Before.Exists {
		plan.SourcePath, err = FindYardRegistrationFile(root, name)
		if err != nil {
			return nil, err
		}
		if plan.SourcePath == path {
			return nil, ErrPersistentTargetStale
		}
		plan.SourceBefore, err = ReadPersistentFileSnapshot(root, plan.SourcePath)
		if err != nil {
			return nil, err
		}
		if !plan.SourceBefore.Exists {
			return nil, ErrPersistentTargetStale
		}
	}
	value := strings.Join(requested, " ")
	plan.Content, err = EditPersistentAssignmentContent(path, plan.Before.Content, "CODING_TOOL_INTEGRATIONS", &value)
	return plan, err
}

func (plan *YardIntegrationWrite) Changed() bool {
	return plan != nil && (!plan.Before.Exists || !bytes.Equal(plan.Before.Content, plan.Content))
}

// Check revalidates the selected layout, target and inherited registration.
func (plan *YardIntegrationWrite) Check() error {
	if plan == nil {
		return nil
	}
	if err := CheckLocalSettingsWritable(plan.root); err != nil {
		return err
	}
	path, err := LocalYardSettingsFile(filepath.Join(plan.root, "yards"), plan.yard)
	if err != nil {
		return err
	}
	if path != plan.Path {
		return ErrPersistentTargetStale
	}
	current, err := readOptionalIntegrationSnapshot(plan.root, plan.Path)
	if err != nil {
		return err
	}
	if !samePersistentFileSnapshotExact(current, plan.Before) {
		return ErrPersistentTargetStale
	}
	if plan.SourcePath != "" {
		source, err := FindYardRegistrationFile(plan.root, plan.yard)
		if err != nil {
			return err
		}
		if source != plan.SourcePath {
			return ErrPersistentTargetStale
		}
		current, err := ReadPersistentFileSnapshot(plan.root, source)
		if err != nil {
			return err
		}
		if !samePersistentFileSnapshotExact(current, plan.SourceBefore) {
			return ErrPersistentTargetStale
		}
	}
	return nil
}

// Apply retains the requested selection if later runtime reconciliation fails.
func (plan *YardIntegrationWrite) Apply() error {
	if !plan.Changed() {
		return plan.Check()
	}
	if err := ensurePersistentRoot(plan.root); err != nil {
		return err
	}
	return compareAndSwapPersistentFileGuarded(plan.root, plan.Path, plan.Before, plan.Content, plan.Check, nil, true)
}

func readOptionalIntegrationSnapshot(root, path string) (PersistentFileSnapshot, error) {
	snapshot, err := ReadPersistentFileSnapshot(root, path)
	if errors.Is(err, os.ErrNotExist) {
		return PersistentFileSnapshot{}, nil
	}
	return snapshot, err
}
