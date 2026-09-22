package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// SourceRecordRelativePath is shared with the versioned configuration owner.
const SourceRecordRelativePath = ".sync/source.json"

// YardIntegrationWrite is a bounded prepared edit of the canonical yard config.
// Migration preserves the complete older registration before archiving a local
// flat file. A private compatibility source is copied and remains untouched.
type YardIntegrationWrite struct {
	Path         string
	Before       PersistentFileSnapshot
	Content      []byte
	SourcePath   string
	SourceBefore PersistentFileSnapshot
	FlatBefore   PersistentFileSnapshot
	root         string
	yard         string
}

func PlanYardIntegrationWrite(loaded Loaded, requested []string) (*YardIntegrationWrite, error) {
	name := loaded.Context.YardName
	if name == "" {
		name = "default"
	}
	root := loaded.Context.Paths.ConfigHome
	plan := &YardIntegrationWrite{Path: filepath.Join(root, "yards", name, "config.env"), root: root, yard: name}
	if err := plan.checkSourceAuthority(); err != nil {
		return nil, err
	}
	var err error
	plan.Before, err = readOptionalIntegrationSnapshot(root, plan.Path)
	if err != nil {
		return nil, err
	}
	content := plan.Before.Content
	if name != "default" {
		if !plan.Before.Exists {
			plan.SourcePath, err = FindYardRegistrationFile(loaded.Context.Paths.ConfigDir, root, name)
			if err != nil {
				return nil, err
			}
			if strings.HasPrefix(plan.SourcePath, root+string(filepath.Separator)) {
				plan.SourceBefore, err = ReadPersistentFileSnapshot(root, plan.SourcePath)
			} else {
				plan.SourceBefore, err = persistentFileSnapshot(plan.SourcePath)
			}
			if err != nil {
				return nil, err
			}
			if !plan.SourceBefore.Exists {
				return nil, ErrPersistentTargetStale
			}
			content = plan.SourceBefore.Content
		}
		flat := filepath.Join(root, "yards", name+".env")
		plan.FlatBefore, err = readOptionalIntegrationSnapshot(root, flat)
		if err != nil {
			return nil, err
		}
		if plan.FlatBefore.Exists {
			archive, err := readOptionalIntegrationSnapshot(root, filepath.Join(root, "recovery/yard-registrations", name+".env"))
			if err != nil {
				return nil, err
			}
			if archive.Exists {
				return nil, errors.New("yard registration recovery archive already exists")
			}
			if err := validateRegistrationRepairRecoveryDirectories(root); err != nil {
				return nil, err
			}
		}
	}
	value := strings.Join(requested, " ")
	plan.Content, err = EditPersistentAssignmentContent(plan.Path, content, "CODING_TOOL_INTEGRATIONS", &value)
	return plan, err
}

func (plan *YardIntegrationWrite) Changed() bool {
	return plan != nil && (!plan.Before.Exists || !bytes.Equal(plan.Before.Content, plan.Content) || plan.FlatBefore.Exists)
}

// Check revalidates target and compatibility input without creating state.
func (plan *YardIntegrationWrite) Check() error {
	if plan == nil {
		return nil
	}
	if err := plan.checkSourceAuthority(); err != nil {
		return err
	}
	current, err := readOptionalIntegrationSnapshot(plan.root, plan.Path)
	if err != nil {
		return err
	}
	if !samePersistentFileSnapshotExact(current, plan.Before) {
		return ErrPersistentTargetStale
	}
	if plan.SourcePath != "" {
		var current PersistentFileSnapshot
		if strings.HasPrefix(plan.SourcePath, plan.root+string(filepath.Separator)) {
			current, err = ReadPersistentFileSnapshot(plan.root, plan.SourcePath)
		} else {
			current, err = persistentFileSnapshot(plan.SourcePath)
		}
		if err != nil {
			return err
		}
		if !samePersistentFileSnapshotExact(current, plan.SourceBefore) {
			return ErrPersistentTargetStale
		}
	}
	if plan.yard != "default" {
		flat, err := readOptionalIntegrationSnapshot(plan.root, filepath.Join(plan.root, "yards", plan.yard+".env"))
		if err != nil {
			return err
		}
		if !samePersistentFileSnapshotExact(flat, plan.FlatBefore) {
			return ErrPersistentTargetStale
		}
	}
	return nil
}

// Apply retains the requested selection if later runtime reconciliation fails.
// An interrupted archive leaves the full old file available for the existing
// config repair-registration command and for the next prepared-write retry.
func (plan *YardIntegrationWrite) Apply() error {
	if plan == nil {
		return nil
	}
	if err := plan.Check(); err != nil {
		return err
	}
	if !plan.Changed() {
		return nil
	}
	if plan.Before.Exists {
		if !bytes.Equal(plan.Before.Content, plan.Content) {
			if err := compareAndSwapPersistentFileGuarded(plan.root, plan.Path, plan.Before, plan.Content, plan.checkSourceAuthority, nil); err != nil {
				return err
			}
		}
	} else {
		if err := ensurePersistentRoot(plan.root); err != nil {
			return err
		}
		unlock, err := LockRoot(plan.root, true)
		if err != nil {
			return err
		}
		if err = plan.Check(); err == nil {
			err = persistFile(plan.root, plan.Path, plan.Content, false)
		}
		unlock()
		if err != nil {
			return err
		}
	}
	if !plan.FlatBefore.Exists {
		return nil
	}
	repair, err := PlanYardRegistrationRepair(plan.root, plan.yard)
	if err != nil {
		return err
	}
	if !samePersistentFileSnapshotExact(repair.flat, plan.FlatBefore) || !bytes.Equal(repair.nested.Content, plan.Content) {
		return ErrPersistentTargetStale
	}
	return repair.applyGuarded(plan.checkSourceAuthority, nil)
}

func readOptionalIntegrationSnapshot(root, path string) (PersistentFileSnapshot, error) {
	snapshot, err := ReadPersistentFileSnapshot(root, path)
	if errors.Is(err, os.ErrNotExist) {
		return PersistentFileSnapshot{}, nil
	}
	return snapshot, err
}

func (plan *YardIntegrationWrite) checkSourceAuthority() error {
	source, err := readOptionalIntegrationSnapshot(plan.root, filepath.Join(plan.root, filepath.FromSlash(SourceRecordRelativePath)))
	if err != nil {
		return err
	}
	if source.Exists {
		return errors.New("configuration is source-managed; integration selection must be changed through the registered source")
	}
	return nil
}
