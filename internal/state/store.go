package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/safefs"
)

var ErrNotFound = errors.New("project state not found")

type FileStore struct {
	directory string
}

func NewFileStore(directory string) (*FileStore, error) {
	if !filepath.IsAbs(directory) {
		return nil, errors.New("state directory must be absolute")
	}
	return &FileStore{directory: filepath.Clean(directory)}, nil
}

func (store *FileStore) Directory() string { return store.directory }

// RepairLegacyPermissions tightens valid records created by the original Bash
// store, whose shell redirection inherited 0666 & umask (commonly 0664). It
// never relaxes permissions and validates the record through an O_NOFOLLOW file
// descriptor before changing that same descriptor to mode 0600.
func (store *FileStore) RepairLegacyPermissions(ctx context.Context) (bool, error) {
	if _, err := os.Lstat(store.directory); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	lock, err := store.lock(ctx, true)
	if err != nil {
		return false, err
	}
	defer unlock(lock)

	entries, err := os.ReadDir(store.directory)
	if err != nil {
		return false, fmt.Errorf("read state directory: %w", err)
	}
	changed := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		repaired, err := repairLegacyRecord(filepath.Join(store.directory, entry.Name()), id)
		if err != nil {
			return false, fmt.Errorf("repair legacy project state: %w", err)
		}
		changed = changed || repaired
	}
	if changed {
		if err := syncDirectory(store.directory); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func (store *FileStore) List(ctx context.Context) ([]domain.ProjectRecord, error) {
	lock, err := store.lock(ctx, false)
	if err != nil {
		return nil, err
	}
	if lock != nil {
		defer unlock(lock)
	}
	return store.listUnlocked()
}

// ListReadOnly reads project records without creating the state directory or
// lock file. Records are published atomically, so it is suitable for a
// pre-confirmation observation that must not repair or migrate local state.
func (store *FileStore) ListReadOnly(context.Context) ([]domain.ProjectRecord, error) {
	var failures []error
	exists, err := validateReadOnlyStateDirectory(store.directory)
	if err != nil {
		return nil, err
	}
	if !exists {
		return []domain.ProjectRecord{}, nil
	}
	entries, err := os.ReadDir(store.directory)
	if err != nil {
		return nil, fmt.Errorf("read state directory: %w", err)
	}
	records := make([]domain.ProjectRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		record, readErr := readRecordReadOnly(filepath.Join(store.directory, entry.Name()), id)
		if readErr != nil {
			failures = append(failures, fmt.Errorf("invalid project state %s: %w", entry.Name(), readErr))
			continue
		}
		records = append(records, record)
	}
	if err := domain.CollectRefusals(failures...); err != nil {
		return nil, err
	}
	sort.Slice(records, func(left, right int) bool { return records[left].ProjectID < records[right].ProjectID })
	return records, nil
}

func (store *FileStore) listUnlocked() ([]domain.ProjectRecord, error) {
	entries, err := os.ReadDir(store.directory)
	if errors.Is(err, os.ErrNotExist) {
		return []domain.ProjectRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state directory: %w", err)
	}
	records := make([]domain.ProjectRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		record, err := readRecord(filepath.Join(store.directory, entry.Name()), id)
		if err != nil {
			return nil, fmt.Errorf("invalid project state: %w", err)
		}
		records = append(records, record)
	}
	sort.Slice(records, func(left, right int) bool { return records[left].ProjectID < records[right].ProjectID })
	return records, nil
}

func (store *FileStore) Get(ctx context.Context, id string) (domain.ProjectRecord, error) {
	if !domain.SafeID(id) {
		return domain.ProjectRecord{}, fmt.Errorf("invalid project ID %q", id)
	}
	lock, err := store.lock(ctx, false)
	if err != nil {
		return domain.ProjectRecord{}, err
	}
	if lock != nil {
		defer unlock(lock)
	}
	record, err := readRecord(store.path(id), id)
	if errors.Is(err, os.ErrNotExist) {
		return domain.ProjectRecord{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return domain.ProjectRecord{}, fmt.Errorf("invalid project state: %w", err)
	}
	return record, err
}

func readRecordReadOnly(path, expectedID string) (domain.ProjectRecord, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return domain.ProjectRecord{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return domain.ProjectRecord{}, fmt.Errorf("project state is not a regular file: %s", path)
	}
	// A read-only reader trusts only the owner; mode is repaired by writers.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return domain.ProjectRecord{}, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return domain.ProjectRecord{}, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return domain.ProjectRecord{}, fmt.Errorf("project state changed while opening: %s", path)
	}
	stat, ok := openedInfo.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return domain.ProjectRecord{}, fmt.Errorf("project state is not owned by the operator: %s", path)
	}
	if openedInfo.Size() > 1024*1024 {
		return domain.ProjectRecord{}, fmt.Errorf("project state exceeds size limit: %s", path)
	}
	return decodeRecord(file, path, expectedID)
}

// GetReadOnly reads one project record without creating the state directory or
// lock file. It deliberately performs no legacy permission or name repair.
func (store *FileStore) GetReadOnly(_ context.Context, id string) (domain.ProjectRecord, error) {
	if !domain.SafeID(id) {
		return domain.ProjectRecord{}, fmt.Errorf("invalid project ID %q", id)
	}
	exists, err := validateReadOnlyStateDirectory(store.directory)
	if err != nil {
		return domain.ProjectRecord{}, err
	}
	if !exists {
		return domain.ProjectRecord{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	record, err := readRecordReadOnly(store.path(id), id)
	if errors.Is(err, os.ErrNotExist) {
		return domain.ProjectRecord{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return domain.ProjectRecord{}, fmt.Errorf("invalid project state: %w", err)
	}
	return record, nil
}

func (store *FileStore) Put(ctx context.Context, record domain.ProjectRecord) error {
	if err := record.Validate(record.ProjectID); err != nil {
		return err
	}
	lock, err := store.lock(ctx, true)
	if err != nil {
		return err
	}
	defer unlock(lock)
	if err := store.validateProjectNameUnlocked(record, ""); err != nil {
		return err
	}
	return store.putUnlocked(record)
}

func (store *FileStore) validateProjectNameUnlocked(
	record domain.ProjectRecord,
	ignoredOperation string,
) error {
	if !domain.SafeProjectName(record.Name) {
		return fmt.Errorf("invalid project name %q", record.Name)
	}
	records, err := store.listUnlocked()
	if err != nil {
		return err
	}
	for _, candidate := range records {
		if candidate.ProjectID == record.ProjectID {
			continue
		}
		if domain.ProjectNamesEqual(candidate.ProjectID, record.Name) ||
			domain.ProjectNamesEqual(candidate.Name, record.Name) ||
			domain.ProjectNamesEqual(candidate.Name, record.ProjectID) {
			return fmt.Errorf(
				"project name %q conflicts with project %q",
				record.Name, candidate.ProjectID,
			)
		}
	}
	reservations, err := store.readReservations(time.Now().UTC())
	if err != nil {
		return err
	}
	for _, reservation := range reservations {
		if reservation.OperationID == ignoredOperation {
			continue
		}
		if domain.ProjectNamesEqual(reservation.Name, record.Name) ||
			domain.ProjectNamesEqual(reservation.Name, record.ProjectID) {
			return fmt.Errorf(
				"project name %q conflicts with pending operation %q",
				record.Name, reservation.OperationID,
			)
		}
	}
	return nil
}

func (store *FileStore) putUnlocked(record domain.ProjectRecord) error {
	pending, err := safefs.Create(store.path(record.ProjectID), safefs.Options{
		TempPattern: "." + record.ProjectID + ".json.tmp.*",
	})
	if err != nil {
		var pathErr *os.PathError
		var linkErr *os.LinkError
		switch {
		case errors.Is(err, safefs.ErrSymlink), errors.Is(err, safefs.ErrNotRegular), errors.As(err, &linkErr):
			// The target is checked before staging; it is a publication failure.
			return fmt.Errorf("publish state: %w", err)
		case errors.As(err, &pathErr) && pathErr.Op == "chmod":
			return fmt.Errorf("protect state candidate: %w", err)
		}
		return fmt.Errorf("create state candidate: %w", err)
	}
	defer pending.Abort()
	temporary := pending.File()
	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(record); err != nil {
		return fmt.Errorf("encode state candidate: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync state candidate: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close state candidate: %w", err)
	}
	if _, err := readRecord(pending.Name(), record.ProjectID); err != nil {
		return fmt.Errorf("validate state candidate: %w", err)
	}
	if err := pending.Publish(); err != nil {
		return fmt.Errorf("publish state: %w", err)
	}
	return stateDirectorySyncError(pending.SyncParent())
}

func (store *FileStore) Delete(ctx context.Context, id string) error {
	if !domain.SafeID(id) {
		return fmt.Errorf("invalid project ID %q", id)
	}
	lock, err := store.lock(ctx, true)
	if err != nil {
		return err
	}
	defer unlock(lock)
	if err := os.Remove(store.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove project state: %w", err)
	}
	return syncDirectory(store.directory)
}

func (store *FileStore) path(id string) string {
	return filepath.Join(store.directory, id+".json")
}

func (store *FileStore) lock(ctx context.Context, exclusive bool) (*safefs.LockFile, error) {
	if err := ensurePrivateDirectory(store.directory); err != nil {
		return nil, err
	}
	lock, err := safefs.OpenLock(filepath.Join(store.directory, ".lock"), 0o600)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	if err := lock.Acquire(ctx, !exclusive, 5*time.Millisecond); err != nil {
		lock.Release()
		return nil, fmt.Errorf("lock state: %w", err)
	}
	return lock, nil
}

func unlock(lock *safefs.LockFile) { lock.Release() }

func ensurePrivateDirectory(path string) error {
	if err := safefs.AdmitDir(path, 0o700); err != nil {
		return errors.New("state path is not a real directory")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("state path is not a real directory")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("state directory permissions are too broad: %o", info.Mode().Perm())
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	return os.Chmod(path, 0o700)
}

func validateReadOnlyStateDirectory(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, errors.New("state path is not a real directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return false, errors.New("state directory is not owned by the operator")
	}
	return true, nil
}

func readRecord(path, expectedID string) (domain.ProjectRecord, error) {
	_ = safefs.RepairPath(path, 0o077)
	info, err := os.Lstat(path)
	if err != nil {
		return domain.ProjectRecord{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return domain.ProjectRecord{}, fmt.Errorf("project state is not a regular file: %s", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return domain.ProjectRecord{}, fmt.Errorf("project state permissions are too broad: %o", info.Mode().Perm())
	}
	if info.Size() > 1024*1024 {
		return domain.ProjectRecord{}, fmt.Errorf("project state exceeds size limit: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return domain.ProjectRecord{}, err
	}
	defer file.Close()
	return decodeRecord(file, path, expectedID)
}

func repairLegacyRecord(path, expectedID string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("project state is not a regular file: %s", path)
	}
	permissions := info.Mode().Perm()
	if permissions&0o077 == 0 {
		return false, nil
	}
	// The legacy shell redirection started from 0666. Any broader mode of the
	// operator's own valid record becomes 0600.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return false, fmt.Errorf("project state changed while opening: %s", path)
	}
	stat, ok := openedInfo.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return false, fmt.Errorf("project state is not owned by the operator: %s", path)
	}
	if openedInfo.Size() > 1024*1024 {
		return false, fmt.Errorf("project state exceeds size limit: %s", path)
	}
	if _, err := decodeRecord(file, path, expectedID); err != nil {
		return false, err
	}
	if err := file.Chmod(0o600); err != nil {
		return false, fmt.Errorf("protect legacy project state: %w", err)
	}
	if err := file.Sync(); err != nil {
		return false, fmt.Errorf("sync legacy project state: %w", err)
	}
	return true, nil
}

func decodeRecord(reader io.Reader, path, expectedID string) (domain.ProjectRecord, error) {
	decoder := json.NewDecoder(io.LimitReader(reader, 1024*1024))
	var record domain.ProjectRecord
	if err := decoder.Decode(&record); err != nil {
		return domain.ProjectRecord{}, fmt.Errorf("decode project state %s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return domain.ProjectRecord{}, fmt.Errorf("project state %s has trailing data", path)
	}
	if err := record.Validate(expectedID); err != nil {
		return domain.ProjectRecord{}, fmt.Errorf("invalid project state %s: %w", path, err)
	}
	return record, nil
}

// ValidateFile checks an unpublished candidate using the same compatibility and
// permission boundary as normal reads. The path must be inside this store.
func (store *FileStore) ValidateFile(path, expectedID string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(store.directory, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("project state candidate escapes state directory")
	}
	_, err = readRecord(absolute, expectedID)
	return err
}

func syncDirectory(path string) error {
	return stateDirectorySyncError(safefs.SyncDir(path))
}

// writeStateFile durably publishes a private state file next to its target.
func writeStateFile(path string, payload []byte, pattern string, noReplace bool) error {
	pending, err := safefs.Create(path, safefs.Options{TempPattern: pattern, NoReplace: noReplace})
	if err != nil {
		return err
	}
	defer pending.Abort()
	if _, err := pending.File().Write(payload); err != nil {
		return err
	}
	if err := pending.Publish(); err != nil {
		return err
	}
	return stateDirectorySyncError(pending.SyncParent())
}

func stateDirectorySyncError(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && pathErr.Op == "sync" {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return err
}
