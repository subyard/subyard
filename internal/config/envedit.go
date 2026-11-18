package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
)

var ErrPersistentTargetStale = errors.New("persistent configuration target is stale")

type PersistentFileSnapshot struct {
	Exists   bool
	Content  []byte
	Identity PersistentFileIdentity
}

// PersistentFileIdentity binds a protected observation to the exact inode and
// security metadata that was inspected. It is deliberately excluded from the
// desired semantic fingerprint: a successful atomic replacement has a new
// inode, while an unexpected same-bytes replacement must still fail CAS.
type PersistentFileIdentity struct {
	Device uint64
	Inode  uint64
	Mode   uint32
	UID    uint32
	GID    uint32
	Links  uint64
}

// PersistentAssignment describes one assignment observed in exact file order.
// Direct is false for assignments produced by a declarative parameter default
// rather than by the record's left-hand side. Dynamic marks any assignment
// whose value depended on expansion. Migration classifiers use both fields to
// reject ambiguous ownership instead of guessing from the effective value.
type PersistentAssignment struct {
	Name    string
	Value   string
	Line    int
	Direct  bool
	Dynamic bool
}

// ReadPersistentFileSnapshot observes an operator-owned persistent file
// without creating the configuration root or any metadata.
func ReadPersistentFileSnapshot(
	configHome string,
	path string,
) (PersistentFileSnapshot, error) {
	if !filepath.IsAbs(configHome) || !filepath.IsAbs(path) {
		return PersistentFileSnapshot{}, errors.New("persistent setting paths must be absolute")
	}
	configHome = filepath.Clean(configHome)
	path = filepath.Clean(path)
	relative, err := filepath.Rel(configHome, path)
	if err != nil || relative == "." || filepath.IsAbs(relative) ||
		relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return PersistentFileSnapshot{}, errors.New("persistent setting path escaped the configuration root")
	}
	file, exists, err := openPersistentFileAt(configHome, relative)
	if err != nil {
		var source *SourceError
		if errors.As(err, &source) {
			return PersistentFileSnapshot{}, err
		}
		return PersistentFileSnapshot{}, sourceAccessError(path, "persistent settings", err)
	}
	if !exists {
		return PersistentFileSnapshot{}, nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return PersistentFileSnapshot{}, sourceAccessError(path, "persistent settings", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return PersistentFileSnapshot{}, sourceError(path, 0, "", "persistent settings", "target must be a regular non-symlink file; "+FileMetadata(info)+"; required type=regular", nil)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return PersistentFileSnapshot{}, sourceError(path, 0, "", "persistent settings", "target is group/world writable; "+FileMetadata(info)+"; required writable mask=0000 (mask=0022)", nil)
	}
	if info.Size() > 8<<20 {
		return PersistentFileSnapshot{}, sourceError(path, 0, "", "persistent settings", fmt.Sprintf("target exceeds its size bound; observed bytes=%d, required maximum=%d", info.Size(), 8<<20), nil)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return PersistentFileSnapshot{}, sourceError(path, 0, "", "persistent settings", fmt.Sprintf("target is not operator-owned; %s; required uid=%d", FileMetadata(info), os.Getuid()), nil)
	}
	if stat.Nlink != 1 {
		return PersistentFileSnapshot{}, sourceError(path, 0, "", "persistent settings", fmt.Sprintf("target has hard links; observed links=%d, required links=1", stat.Nlink), nil)
	}
	content, err := io.ReadAll(io.LimitReader(file, (8<<20)+1))
	if err != nil {
		return PersistentFileSnapshot{}, sourceAccessError(path, "persistent settings", err)
	}
	if len(content) > 8<<20 {
		return PersistentFileSnapshot{}, sourceError(path, 0, "", "persistent settings", fmt.Sprintf("target exceeds its size bound; observed bytes=%d, required maximum=%d", len(content), 8<<20), nil)
	}
	return PersistentFileSnapshot{
		Exists: true, Content: content, Identity: persistentFileIdentity(stat),
	}, nil
}

func openPersistentFileAt(
	configHome string,
	relative string,
) (*os.File, bool, error) {
	rootFD, err := syscall.Open(
		configHome,
		syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC,
		0,
	)
	if err != nil {
		return nil, false, sourceAccessError(configHome, "persistent settings directory", err)
	}
	current := os.NewFile(uintptr(rootFD), configHome)
	if err := validateOpenedPersistentDirectory(current, configHome); err != nil {
		current.Close()
		return nil, false, err
	}
	parts := strings.Split(relative, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		fd, openErr := syscall.Openat(
			int(current.Fd()),
			part,
			syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC,
			0,
		)
		if errors.Is(openErr, syscall.ENOENT) {
			current.Close()
			return nil, false, nil
		}
		if openErr != nil {
			current.Close()
			return nil, false, sourceAccessError(filepath.Join(current.Name(), part), "persistent settings directory", openErr)
		}
		next := os.NewFile(uintptr(fd), filepath.Join(current.Name(), part))
		if err := validateOpenedPersistentDirectory(next, next.Name()); err != nil {
			next.Close()
			current.Close()
			return nil, false, err
		}
		current.Close()
		current = next
	}
	fd, err := syscall.Openat(
		int(current.Fd()),
		parts[len(parts)-1],
		// All callers require regular files. Do not wait for a FIFO writer
		// before the caller can fstat and reject the unexpected leaf type.
		syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK,
		0,
	)
	current.Close()
	if errors.Is(err, syscall.ENOENT) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(configHome, relative)), true, nil
}

func validateOpenedPersistentDirectory(file *os.File, path string) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return sourceError(path, 0, "", "persistent settings directory", "directory has unsafe type; "+FileMetadata(info)+"; required type=directory", nil)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return sourceError(path, 0, "", "persistent settings directory", "directory is group/world writable; "+FileMetadata(info)+"; required writable mask=0000 (mask=0022)", nil)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return sourceError(path, 0, "", "persistent settings directory", fmt.Sprintf("directory is not operator-owned; %s; required uid=%d", FileMetadata(info), os.Getuid()), nil)
	}
	return nil
}

// ParsePersistentAssignments parses bounded in-memory env content while
// preserving duplicates, line provenance, and implicit/dynamic assignments.
func ParsePersistentAssignments(
	path string,
	content []byte,
) ([]PersistentAssignment, error) {
	return (Catalog{}).ParsePersistentAssignments(path, content)
}

// ParsePersistentAssignments applies the owning catalog's discarded-input policy.
func (settings Catalog) ParsePersistentAssignments(path string, content []byte) ([]PersistentAssignment, error) {
	if len(content) > 8<<20 {
		return nil, errors.New("persistent setting content exceeds its size bound")
	}
	values := environment{}
	var assignments []PersistentAssignment
	err := scanEnvRecords(bytes.NewReader(content), path, "persistent settings", func(record envRecord) error {
		if record.text == "" {
			return nil
		}
		direct, dynamic := persistentDirectAssignment(record.text)
		observe := func(name, value string) {
			isDirect := direct != "" && name == direct
			assignments = append(assignments, PersistentAssignment{
				Name: name, Value: value, Line: record.line,
				Direct: isDirect, Dynamic: dynamic || !isDirect,
			})
		}
		// Report discarded direct assignments so config unset can remove them.
		return applyRecordRetired(record.text, values, observe, operatorDiscardPolicy(observe, settings))
	})
	if err != nil {
		return nil, err
	}
	return assignments, nil
}

func persistentDirectAssignment(record string) (string, bool) {
	name, raw := envRecordAssignment(record)
	_, expands := decodeValue(raw)
	return name, name == "" || expands && strings.ContainsRune(raw, '$')
}

// WritePersistentAssignment updates one assignment without rewriting unrelated
// comments or records. A nil value removes every assignment for name.
func WritePersistentAssignment(
	configHome string,
	path string,
	name string,
	value *string,
) error {
	return writePersistentAssignment(configHome, path, name, value, nil, nil, Catalog{})
}

// WritePersistentAssignmentIfUnchanged updates an assignment only when the
// complete target still matches the bytes and existence observed at preview.
// Optional guards run under the configuration root lock before publication.
func (settings Catalog) WritePersistentAssignmentIfUnchanged(
	configHome string,
	path string,
	name string,
	value *string,
	expected PersistentFileSnapshot,
	guards ...func() error,
) error {
	return writePersistentAssignment(configHome, path, name, value, &expected, guards, settings)
}

func writePersistentAssignment(
	configHome string,
	path string,
	name string,
	value *string,
	expected *PersistentFileSnapshot,
	guards []func() error,
	settings Catalog,
) error {
	if err := ensurePersistentRoot(configHome); err != nil {
		return err
	}
	unlock, err := LockRoot(configHome, true)
	if err != nil {
		return err
	}
	defer unlock()
	if err := checkLocalSettingsTargetWritable(configHome, path); err != nil {
		return err
	}
	current, err := persistentFileSnapshot(path)
	if err != nil {
		return err
	}
	if expected != nil && !samePersistentFileSnapshot(current, *expected) {
		return ErrPersistentTargetStale
	}
	for _, guard := range guards {
		if err := guard(); err != nil {
			return err
		}
	}
	content := current.Content
	updated, err := settings.EditPersistentAssignmentContent(path, content, name, value)
	if err != nil {
		return err
	}
	if value == nil && len(updated) == 0 {
		return removePersistentFile(configHome, path)
	}
	return writePersistentFile(configHome, path, updated)
}

func removePersistentFile(configHome, path string) error {
	relative, err := filepath.Rel(configHome, path)
	if err != nil || relative == "." || filepath.IsAbs(relative) ||
		relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("persistent setting path escaped the configuration root")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return sourceError(path, 0, "", "persistent settings", "target must be a regular non-symlink file; "+FileMetadata(info)+"; required type=regular", nil)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		return sourceError(path, 0, "", "persistent settings", persistentOwnershipReason(info, stat), nil)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncPersistentDirectory(filepath.Dir(path))
}

// EditPersistentAssignmentContent returns the complete persistent settings
// content after applying one assignment change without writing it.
func EditPersistentAssignmentContent(
	path string,
	content []byte,
	name string,
	value *string,
) ([]byte, error) {
	return (Catalog{}).EditPersistentAssignmentContent(path, content, name, value)
}

// EditPersistentAssignmentContent uses the catalog without reading or writing native state.
func (settings Catalog) EditPersistentAssignmentContent(path string, content []byte, name string, value *string) ([]byte, error) {
	return (PersistentScalarEdits{{Name: name, Value: value}}).LayerContent(path, content)
}

// WritePersistentFile atomically stores a validated persistent file setting.
func WritePersistentFile(configHome, path string, content []byte) error {
	unlock, err := LockRoot(configHome, true)
	if err != nil {
		return err
	}
	defer unlock()
	return writePersistentFile(configHome, path, content)
}

// WritePersistentFileIfUnchanged replaces a persistent file only when the
// complete target still matches the bytes and existence observed at preview.
// Optional guards run under the configuration root lock before publication.
func WritePersistentFileIfUnchanged(
	configHome string,
	path string,
	expected PersistentFileSnapshot,
	content []byte,
	guards ...func() error,
) error {
	return ApplyPersistentFileIfUnchanged(configHome, path, expected, PersistentFileSnapshot{Exists: true, Content: content}, guards...)
}

// ApplyPersistentFileIfUnchanged publishes the exact validated draft, or removes
// the target if the draft is absent.
func ApplyPersistentFileIfUnchanged(
	configHome string,
	path string,
	expected, desired PersistentFileSnapshot,
	guards ...func() error,
) error {
	if err := ensurePersistentRoot(configHome); err != nil {
		return err
	}
	unlock, err := LockRoot(configHome, true)
	if err != nil {
		return err
	}
	defer unlock()
	if err := checkLocalSettingsTargetWritable(configHome, path); err != nil {
		return err
	}
	current, err := persistentFileSnapshot(path)
	if err != nil {
		return err
	}
	if !samePersistentFileSnapshot(current, expected) {
		return ErrPersistentTargetStale
	}
	for _, guard := range guards {
		if err := guard(); err != nil {
			return err
		}
	}
	if !desired.Exists {
		return removePersistentFile(configHome, path)
	}
	return writePersistentFile(configHome, path, desired.Content)
}

func persistentFileSnapshot(path string) (PersistentFileSnapshot, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return PersistentFileSnapshot{}, nil
	}
	if err != nil {
		return PersistentFileSnapshot{}, sourceAccessError(path, "persistent settings", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return PersistentFileSnapshot{}, sourceError(path, 0, "", "persistent settings", "target must be a regular non-symlink file; "+FileMetadata(info)+"; required type=regular", nil)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return PersistentFileSnapshot{}, sourceError(path, 0, "", "persistent settings", "target is group/world writable; "+FileMetadata(info)+"; required writable mask=0000 (mask=0022)", nil)
	}
	if info.Size() > 8<<20 {
		return PersistentFileSnapshot{}, sourceError(path, 0, "", "persistent settings", fmt.Sprintf("target exceeds its size bound; observed bytes=%d, required maximum=%d", info.Size(), 8<<20), nil)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		return PersistentFileSnapshot{}, sourceError(path, 0, "", "persistent settings", persistentOwnershipReason(info, stat), nil)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return PersistentFileSnapshot{}, err
	}
	return PersistentFileSnapshot{
		Exists: true, Content: content, Identity: persistentFileIdentity(stat),
	}, nil
}

func samePersistentFileSnapshot(current, expected PersistentFileSnapshot) bool {
	if current.Exists != expected.Exists || !bytes.Equal(current.Content, expected.Content) {
		return false
	}
	return !expected.Exists || expected.Identity == (PersistentFileIdentity{}) ||
		current.Identity == expected.Identity
}

func persistentFileIdentity(stat *syscall.Stat_t) PersistentFileIdentity {
	return PersistentFileIdentity{
		Device: uint64(stat.Dev), Inode: stat.Ino, Mode: stat.Mode,
		UID: stat.Uid, GID: stat.Gid, Links: uint64(stat.Nlink),
	}
}

// CreatePersistentFile atomically stores a new persistent file and refuses to
// replace an existing target. Optional guards run under the configuration root
// lock before publication, as with guarded assignment and file updates.
func CreatePersistentFile(configHome, path string, content []byte, guards ...func() error) error {
	if err := ensurePersistentRoot(configHome); err != nil {
		return err
	}
	unlock, err := LockRoot(configHome, true)
	if err != nil {
		return err
	}
	defer unlock()
	if err := checkLocalSettingsTargetWritable(configHome, path); err != nil {
		return err
	}
	for _, guard := range guards {
		if err := guard(); err != nil {
			return err
		}
	}
	return persistFile(configHome, path, content, false)
}

// EditProfileSelectionContent preserves the profile command's handling of
// standalone defaults and discarded profile settings.
func (settings Catalog) EditProfileSelectionContent(path string, content []byte, value string) ([]byte, error) {
	const name = "ENVIRONMENT_PROFILES"
	const role = "persistent settings"
	assignments, err := settings.ParsePersistentAssignments(path, content)
	if err != nil {
		return nil, err
	}
	defaults := map[int]bool{}
	for _, assignment := range assignments {
		if !assignment.Direct && assignment.Name == name {
			defaults[assignment.Line] = true
		}
	}
	var records []scalarEditRecord
	err = scanEnvRecords(bytes.NewReader(content), path, role, func(record envRecord) error {
		direct, dynamic := persistentDirectAssignment(record.text)
		if direct == "" && defaults[record.line] {
			_, raw := envRecordAssignment(record.text)
			expression, _ := decodeValue(raw)
			fallback, prefix := strings.CutPrefix(expression, "${ENVIRONMENT_PROFILES:=")
			fallback, suffix := strings.CutSuffix(fallback, "}")
			if prefix && suffix && !strings.ContainsAny(fallback, "$}") {
				direct, dynamic = name, false
			}
		}
		if dynamic && CanonicalSettingName(direct) == name {
			return sourceError(path, record.line, recordSettingKey(record.text), role, ErrDynamicScalarRecord.Error(), ErrDynamicScalarRecord)
		}
		records = append(records, scalarEditRecord{envRecord: record, name: direct})
		return nil
	})
	if err != nil {
		return nil, err
	}
	edits := PersistentScalarEdits{{Name: name, Value: &value}}
	draft, err := renderScalarEditRecords(path, edits.placeRecords(records))
	if err != nil {
		return nil, err
	}
	assignments, err = settings.ParsePersistentAssignments(path, draft)
	if err != nil {
		return nil, err
	}
	var last PersistentAssignment
	var refusals []error
	for _, assignment := range assignments {
		if CanonicalSettingName(assignment.Name) != name {
			continue
		}
		if assignment.Name != name || !assignment.Direct {
			refusals = append(refusals, sourceError(path, assignment.Line, name, role,
				"setting is still assigned by a preserved record; edit the file manually", nil))
		}
		last = assignment
	}
	if last.Value != value || last.Name == "" {
		refusals = append(refusals, sourceError(path, last.Line, name, role,
			"a preserved record changes the requested setting; edit the file manually", nil))
	}
	if err := domain.CollectRefusals(refusals...); err != nil {
		return nil, err
	}
	return draft, nil
}

func quotePersistentValue(value string) string {
	return "'" + strings.ReplaceAll(value, "'", escapedEnvApostrophe) + "'"
}

func writePersistentFile(configHome, path string, content []byte) error {
	return persistFile(configHome, path, content, true)
}

func persistFile(configHome, path string, content []byte, replace bool) error {
	relative, err := filepath.Rel(configHome, path)
	if err != nil || relative == "." || filepath.IsAbs(relative) ||
		relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("persistent setting path escaped the configuration root")
	}
	directory := filepath.Dir(path)
	if err := ensurePersistentDirectory(configHome, directory); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if info, err := os.Lstat(path); err == nil {
		if !replace {
			return errors.New("persistent setting target already exists")
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return sourceError(path, 0, "", "persistent settings", "target must be a regular non-symlink file; "+FileMetadata(info)+"; required type=regular", nil)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
			return sourceError(path, 0, "", "persistent settings", persistentOwnershipReason(info, stat), nil)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return sourceError(path, 0, "", "persistent settings", "target is group/world writable; "+FileMetadata(info)+"; required writable mask=0000 (mask=0022)", nil)
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	temp := file.Name()
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(temp)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if replace {
		if err := os.Rename(temp, path); err != nil {
			return err
		}
	} else {
		if err := os.Link(temp, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return errors.New("persistent setting target already exists")
			}
			return err
		}
		if err := os.Remove(temp); err != nil {
			return err
		}
	}
	keep = true
	return syncPersistentDirectory(directory)
}

func ensurePersistentRoot(configHome string) error {
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		return err
	}
	return validatePersistentDirectory(configHome)
}

func ensurePersistentDirectory(configHome, target string) error {
	relative, err := filepath.Rel(configHome, target)
	if err != nil || filepath.IsAbs(relative) || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("persistent setting directory escaped the configuration root")
	}
	current := configHome
	if err := validatePersistentDirectory(current); err != nil {
		return err
	}
	parts := strings.Split(relative, string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		if err := os.Mkdir(current, 0o700); err != nil &&
			!errors.Is(err, os.ErrExist) {
			return err
		}
		if err := validatePersistentDirectory(current); err != nil {
			return err
		}
	}
	return nil
}

func validatePersistentDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
		info.Mode().Perm()&0o022 != 0 {
		return sourceError(path, 0, "", "persistent settings directory", "directory is unsafe; "+FileMetadata(info)+"; required type=directory, writable mask=0000 (mask=0022)", nil)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return sourceError(path, 0, "", "persistent settings directory", fmt.Sprintf("directory is not operator-owned; %s; required uid=%d", FileMetadata(info), os.Getuid()), nil)
	}
	return nil
}

func syncPersistentDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func persistentOwnershipReason(info os.FileInfo, stat *syscall.Stat_t) string {
	links := "unavailable"
	if stat != nil {
		links = fmt.Sprintf("%d", stat.Nlink)
	}
	return fmt.Sprintf("target has unsafe ownership or hard links; %s, links=%s; required uid=%d, links=1", FileMetadata(info), links, os.Getuid())
}
