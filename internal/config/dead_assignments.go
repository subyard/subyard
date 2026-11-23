package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DeadAssignment is a narrowly proven obsolete checkout-overlay reference.
// No user expression is evaluated: only the two exact historical spellings of
// SUBYARD_CONFIG_DIR followed by a declared agent artifact path are recognized.
type DeadAssignment struct {
	Name, Path, Source string
	Line               int
}

func (a DeadAssignment) String() string {
	return fmt.Sprintf("%s:%d: %s is dead (private overlay path is missing); update will remove this line; file-settings source %s takes precedence", a.Path, a.Line, a.Name, a.Source)
}

// DeadAssignmentPlan keeps the original protected host snapshot and all proof
// inputs. The release activation owner binds it to the authorized plan.
type DeadAssignmentPlan struct {
	Path      string
	Expected  PersistentFileSnapshot
	Desired   []byte
	Removed   []DeadAssignment
	Overrides map[string]PersistentFileSnapshot
	Missing   []string
}

// PlanDeadAssignments does not change files. Uncertain references are retained
// for the ordinary literal parser to refuse. Projected files without a native
// identity cannot authorize deletion of operator settings.
func (settings Catalog) PlanDeadAssignments(home, configDir string, files *ProspectiveFiles) (DeadAssignmentPlan, error) {
	plan := DeadAssignmentPlan{Path: filepath.Join(home, "config.env"), Overrides: map[string]PersistentFileSnapshot{}}
	read := func(path string) (PersistentFileSnapshot, error) {
		if snapshot, handled, err := files.snapshot(path); handled || err != nil {
			return snapshot, err
		}
		return ReadPersistentFileSnapshot(home, path)
	}
	var err error
	plan.Expected, err = read(plan.Path)
	if errors.Is(err, os.ErrNotExist) {
		return plan, nil
	}
	if err != nil {
		return plan, err
	}
	plan.Desired = slices.Clone(plan.Expected.Content)
	if !plan.Expected.Exists || plan.Expected.Identity == (PersistentFileIdentity{}) {
		return plan, nil
	}
	// Resolve the config directory before interpreting /.., as the shell did.
	actualConfig, err := filepath.EvalSymlinks(configDir)
	if err != nil || !filepath.IsAbs(actualConfig) {
		return plan, nil
	}
	mappings := SyncableFileMappings(Loaded{Catalog: settings})
	counts := map[string]int{}
	var records []envRecord
	err = scanEnvRecords(bytes.NewReader(plan.Expected.Content), plan.Path, "scalar settings", true, func(record envRecord) error {
		records = append(records, record)
		name, _ := envRecordAssignment(record.text)
		counts[name]++
		return nil
	})
	if err != nil {
		// Leave malformed records to the ordinary collecting literal parser.
		return plan, nil
	}
	if counts["SUBYARD_CONFIG_DIR"] != 0 {
		return plan, nil
	}
	var desired bytes.Buffer
	for _, record := range records {
		name, raw := envRecordAssignment(record.text)
		removed := false
		for _, mapping := range mappings {
			if name != mapping.Name || counts[name] != 1 {
				continue
			}
			suffix := "/../private/agents/" + mapping.Relative
			if raw != `"$SUBYARD_CONFIG_DIR`+suffix+`"` && raw != `"${SUBYARD_CONFIG_DIR}`+suffix+`"` &&
				raw != `$SUBYARD_CONFIG_DIR`+suffix && raw != `${SUBYARD_CONFIG_DIR}`+suffix {
				continue
			}
			missing := filepath.Join(actualConfig, "..", "private", "agents", filepath.FromSlash(mapping.Relative))
			if !provablyMissing(missing) {
				continue
			}
			source := filepath.Join(home, "overrides", "host", "agents", filepath.FromSlash(mapping.Relative))
			override, readErr := read(source)
			if readErr != nil || !override.Exists || override.Identity == (PersistentFileIdentity{}) {
				continue
			}
			plan.Removed = append(plan.Removed, DeadAssignment{Name: name, Path: plan.Path, Line: record.line, Source: source})
			plan.Overrides[source] = override
			plan.Missing = append(plan.Missing, missing)
			removed = true
			break
		}
		if removed {
			continue
		}
		desired.WriteString(record.raw)
	}
	plan.Desired = desired.Bytes()
	return plan, nil
}

func provablyMissing(path string) bool {
	// Lstat each component: ENOENT after real directories proves absence;
	// symlinks, ENOTDIR and access errors leave the reference uncertain.
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(path, current), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return true
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return false
}

// Preview preserves original line numbers while keeping removed values out of
// the literal parser. Desired holds the actual bytes to publish.
func (plan DeadAssignmentPlan) Preview() []byte {
	if len(plan.Removed) == 0 {
		return plan.Expected.Content
	}
	lines := map[int]bool{}
	for _, assignment := range plan.Removed {
		lines[assignment.Line] = true
	}
	var filtered bytes.Buffer
	_ = scanEnvRecords(bytes.NewReader(plan.Expected.Content), plan.Path, "scalar settings", true, func(record envRecord) error {
		if lines[record.line] {
			filtered.WriteString(strings.Repeat("\n", strings.Count(record.raw, "\n")))
		} else {
			filtered.WriteString(record.raw)
		}
		return nil
	})
	return filtered.Bytes()
}

// Apply publishes one atomic edit, preserving owner and exact mode. Proof
// dependencies are checked again under the same root lock as the host CAS.
func (plan DeadAssignmentPlan) Apply(home string) error {
	if len(plan.Removed) == 0 {
		return nil
	}
	return compareAndSwapPersistentFileGuarded(home, plan.Path, plan.Expected, plan.Desired, func() error {
		if err := checkLocalSettingsTargetWritable(home, plan.Path); err != nil {
			return err
		}
		for path, expected := range plan.Overrides {
			current, err := ReadPersistentFileSnapshot(home, path)
			if err != nil {
				return err
			}
			if !samePersistentFileSnapshotExact(current, expected) {
				return ErrPersistentTargetStale
			}
		}
		for _, path := range plan.Missing {
			if !provablyMissing(path) {
				return ErrPersistentTargetStale
			}
		}
		return nil
	}, nil, true)
}

func (loaded Loaded) DeadAssignments() []DeadAssignment { return slices.Clone(loaded.dead) }
