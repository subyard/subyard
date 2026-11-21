package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

const projectNameMigrationSchema = 1

type projectNameMigration struct {
	Schema  int                          `json:"schema"`
	Targets []projectNameMigrationTarget `json:"targets"`
}

type projectNameMigrationTarget struct {
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
	SourceKey string `json:"sourceKey,omitempty"`
}

// MigrateLegacyNames makes the human selector unique without changing legacy
// project IDs or workspace paths. A durable target map is published before any
// record changes, so an interrupted pass resumes the same mapping.
func (store *FileStore) MigrateLegacyNames(ctx context.Context) (bool, error) {
	lock, err := store.lock(ctx, true)
	if err != nil {
		return false, err
	}
	defer unlock(lock)

	resumed, err := store.resumeProjectNameMigration()
	if err != nil {
		return false, err
	}
	records, err := store.listUnlocked()
	if err != nil {
		return false, err
	}
	targets := planProjectNameMigration(records)
	if len(targets) == 0 {
		return resumed, nil
	}
	if err := validateCanonicalMigrationTargets(records, targets); err != nil {
		return false, err
	}
	journal := projectNameMigration{
		Schema: projectNameMigrationSchema, Targets: targets,
	}
	if err := store.writeProjectNameMigration(journal); err != nil {
		return false, err
	}
	if _, err := store.applyProjectNameMigration(journal); err != nil {
		return false, err
	}
	if err := store.removeProjectNameMigration(); err != nil {
		return false, err
	}
	return true, nil
}

// LegacyNameMigrationPending reports whether MigrateLegacyNames would change
// state without publishing or applying a migration journal.
func (store *FileStore) LegacyNameMigrationPending(ctx context.Context) (bool, error) {
	lock, err := store.lock(ctx, false)
	if err != nil {
		return false, err
	}
	if lock != nil {
		defer unlock(lock)
	}
	records, err := store.listUnlocked()
	if err != nil {
		return false, err
	}
	journal, exists, err := store.readProjectNameMigration()
	if err != nil {
		return false, err
	}
	if exists {
		if err := validateCanonicalMigrationTargets(records, journal.Targets); err != nil {
			return false, err
		}
		return true, nil
	}
	targets := planProjectNameMigration(records)
	if err := validateCanonicalMigrationTargets(records, targets); err != nil {
		return false, err
	}
	return len(targets) > 0, nil
}

func planProjectNameMigration(records []domain.ProjectRecord) []projectNameMigrationTarget {
	sort.SliceStable(records, func(i, j int) bool {
		left, leftOK := importedTime(records[i].ImportedAt)
		right, rightOK := importedTime(records[j].ImportedAt)
		if leftOK != rightOK {
			return leftOK
		}
		if leftOK && !left.Equal(right) {
			return left.Before(right)
		}
		return records[i].ProjectID < records[j].ProjectID
	})

	bases := make([]string, len(records))
	keepers := make(map[string]int, len(records))
	idOwners := make(map[string][]string, len(records))
	for index, record := range records {
		base := record.Name
		if !domain.SafeProjectName(base) {
			base = record.ProjectID
			if len(base) > 50 {
				base = base[:50]
			}
		}
		bases[index] = base
		key := domain.ProjectNameKey(base)
		if _, found := keepers[key]; !found {
			keepers[key] = index
		}
		idKey := domain.ProjectNameKey(record.ProjectID)
		idOwners[idKey] = append(idOwners[idKey], record.ProjectID)
	}
	for index, record := range records {
		if record.IdentityVersion == 2 {
			keepers[domain.ProjectNameKey(bases[index])] = index
		}
	}

	// Protect every future keeper name so an earlier duplicate cannot consume
	// an already-unique name such as Demo-2.
	protected := make(map[string]string, len(keepers))
	for _, index := range keepers {
		record := records[index]
		if !hasForeignProjectID(idOwners[domain.ProjectNameKey(bases[index])], record.ProjectID) {
			protected[domain.ProjectNameKey(bases[index])] = record.ProjectID
		}
	}

	assigned := make(map[string]string, len(records))
	targets := make([]projectNameMigrationTarget, 0, len(records))
	for index, record := range records {
		base := bases[keepers[domain.ProjectNameKey(bases[index])]]
		name := base
		if keepers[domain.ProjectNameKey(base)] != index ||
			projectNameUnavailable(name, record.ProjectID, idOwners, protected, assigned) {
			name = nextAvailableProjectName(
				base,
				func(candidate string) bool {
					return !projectNameUnavailable(
						candidate, record.ProjectID, idOwners, protected, assigned,
					)
				},
			)
		}
		assigned[domain.ProjectNameKey(name)] = record.ProjectID
		sourceKey := record.SourceKey
		if sourceKey == "" && record.HostPath != "" {
			sourceKey = SourceKey(record.HostPath)
		}
		if record.Name != name || record.SourceKey != sourceKey {
			targets = append(targets, projectNameMigrationTarget{
				ProjectID: record.ProjectID, Name: name, SourceKey: sourceKey,
			})
		}
	}
	return targets
}

func projectNameUnavailable(
	name, projectID string,
	idOwners map[string][]string,
	protected, assigned map[string]string,
) bool {
	key := domain.ProjectNameKey(name)
	if hasForeignProjectID(idOwners[key], projectID) {
		return true
	}
	if owner, found := protected[key]; found && owner != projectID {
		return true
	}
	if owner, found := assigned[key]; found && owner != projectID {
		return true
	}
	return false
}

func hasForeignProjectID(owners []string, projectID string) bool {
	for _, owner := range owners {
		if owner != projectID {
			return true
		}
	}
	return false
}

func nextAvailableProjectName(base string, available func(string) bool) string {
	for suffix := 2; ; suffix++ {
		tail := fmt.Sprintf("-%d", suffix)
		keep := 50 - len(tail)
		candidateBase := base
		if len(candidateBase) > keep {
			candidateBase = candidateBase[:keep]
		}
		candidate := candidateBase + tail
		if available(candidate) {
			return candidate
		}
	}
}

func (store *FileStore) resumeProjectNameMigration() (bool, error) {
	journal, exists, err := store.readProjectNameMigration()
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	if _, err := store.applyProjectNameMigration(journal); err != nil {
		return false, err
	}
	if err := store.removeProjectNameMigration(); err != nil {
		return false, err
	}
	return true, nil
}

func (store *FileStore) readProjectNameMigration() (projectNameMigration, bool, error) {
	path := store.projectNameMigrationPath()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return projectNameMigration{}, false, nil
	}
	if err != nil {
		return projectNameMigration{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 64<<20 {
		return projectNameMigration{}, false, errors.New("invalid project name migration journal")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return projectNameMigration{}, false, err
	}
	var journal projectNameMigration
	if err := json.Unmarshal(payload, &journal); err != nil {
		return projectNameMigration{}, false, fmt.Errorf(
			"decode project name migration journal: %w", err,
		)
	}
	if err := validateProjectNameMigration(journal); err != nil {
		return projectNameMigration{}, false, err
	}
	return journal, true, nil
}

// ConvergeObserved registers an already-existing data-plane workspace without
// allowing its stale metadata to overwrite an owner record. Unlike normal
// admission, there is no physical mutation to protect; the migration journal
// atomically allocates a collision-free legacy selector and then publishes the
// observed record.
func (store *FileStore) ConvergeObserved(
	ctx context.Context,
	observed domain.ProjectRecord,
	sshHost string,
) error {
	lock, err := store.lock(ctx, true)
	if err != nil {
		return err
	}
	defer unlock(lock)
	if _, err := readRecord(store.path(observed.ProjectID), observed.ProjectID); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	reservations, err := store.readReservations(time.Now().UTC())
	if err != nil {
		return err
	}
	for _, reservation := range reservations {
		if domain.ProjectNamesEqual(reservation.ProjectID, observed.ProjectID) {
			return fmt.Errorf("%w; retry project metadata convergence", ErrAdmissionPending)
		}
	}

	record := domain.ProjectRecord{
		Schema: observed.Schema, IdentityVersion: observed.IdentityVersion,
		ProjectID: observed.ProjectID, Name: observed.Name,
		YardPath: observed.YardPath, Mode: observed.Mode, SSHHost: sshHost,
		ImportedAt: observed.ImportedAt, Target: observed.Target,
		RegistrySource: "yard",
	}
	if record.Target != "" && record.Target != "yard" {
		record.Profile = record.Target
	}
	if err := record.Validate(record.ProjectID); err != nil {
		return err
	}
	records, err := store.listUnlocked()
	if err != nil {
		return err
	}
	plannedRecords := append(append([]domain.ProjectRecord(nil), records...), record)
	for _, reservation := range reservations {
		plannedRecords = append(plannedRecords, domain.ProjectRecord{
			Schema: 1, IdentityVersion: 2,
			ProjectID: reservation.ProjectID, Name: reservation.Name,
			YardPath: YardPath(reservation.ProjectID), Mode: reservation.Mode,
			SSHHost: sshHost,
		})
	}
	targets := planProjectNameMigration(plannedRecords)
	if len(targets) == 0 {
		return store.putUnlocked(record)
	}
	if err := validateCanonicalMigrationTargets(plannedRecords, targets); err != nil {
		return err
	}
	journal := projectNameMigration{
		Schema: projectNameMigrationSchema, Targets: targets,
	}
	if err := store.writeProjectNameMigration(journal); err != nil {
		return err
	}
	if _, err := store.applyProjectNameMigration(journal); err != nil {
		return err
	}
	for _, target := range targets {
		if target.ProjectID == record.ProjectID {
			record.Name = target.Name
			record.SourceKey = target.SourceKey
			break
		}
	}
	if err := store.putUnlocked(record); err != nil {
		return err
	}
	return store.removeProjectNameMigration()
}

func validateCanonicalMigrationTargets(
	records []domain.ProjectRecord,
	targets []projectNameMigrationTarget,
) error {
	versions := make(map[string]int, len(records))
	for _, record := range records {
		versions[record.ProjectID] = record.IdentityVersion
	}
	for _, target := range targets {
		if versions[target.ProjectID] == 2 && target.Name != target.ProjectID {
			return fmt.Errorf(
				"canonical project %q conflicts with owner identity",
				target.ProjectID,
			)
		}
	}
	return nil
}

func validateProjectNameMigration(journal projectNameMigration) error {
	if journal.Schema != projectNameMigrationSchema || len(journal.Targets) == 0 {
		return errors.New("invalid project name migration journal")
	}
	seen := make(map[string]bool, len(journal.Targets))
	for _, target := range journal.Targets {
		if !domain.SafeID(target.ProjectID) || !domain.SafeProjectName(target.Name) ||
			seen[target.ProjectID] {
			return errors.New("invalid project name migration target")
		}
		if target.SourceKey != "" &&
			(len(target.SourceKey) != 64 ||
				strings.Trim(target.SourceKey, "0123456789abcdef") != "") {
			return errors.New("invalid project name migration source key")
		}
		seen[target.ProjectID] = true
	}
	return nil
}

func (store *FileStore) applyProjectNameMigration(
	journal projectNameMigration,
) (bool, error) {
	records, err := store.listUnlocked()
	if err != nil {
		return false, err
	}
	byID := make(map[string]domain.ProjectRecord, len(records))
	for _, record := range records {
		byID[record.ProjectID] = record
	}
	changed := false
	for _, target := range journal.Targets {
		record, found := byID[target.ProjectID]
		if !found || record.Name == target.Name && record.SourceKey == target.SourceKey {
			continue
		}
		record.Name = target.Name
		record.SourceKey = target.SourceKey
		if err := store.putUnlocked(record); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

func (store *FileStore) writeProjectNameMigration(journal projectNameMigration) error {
	if err := validateProjectNameMigration(journal); err != nil {
		return err
	}
	payload, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	return writeStateFile(store.projectNameMigrationPath(), payload, ".name-migration.tmp.*", false)
}

func (store *FileStore) removeProjectNameMigration() error {
	if err := os.Remove(store.projectNameMigrationPath()); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(store.directory)
}

func (store *FileStore) projectNameMigrationPath() string {
	return filepath.Join(store.directory, ".name-migration")
}

func importedTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, value)
	return parsed, err == nil
}
