package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var ErrPersistentTargetStale = errors.New("persistent configuration target is stale")

type PersistentFileSnapshot struct {
	Exists  bool
	Content []byte
}

// WritePersistentAssignment updates one assignment without rewriting unrelated
// comments or records. A nil value removes every assignment for name.
func WritePersistentAssignment(
	configHome string,
	path string,
	name string,
	value *string,
) error {
	return writePersistentAssignment(configHome, path, name, value, nil)
}

// WritePersistentAssignmentIfUnchanged updates an assignment only when the
// complete target still matches the bytes and existence observed at preview.
func WritePersistentAssignmentIfUnchanged(
	configHome string,
	path string,
	name string,
	value *string,
	expected PersistentFileSnapshot,
) error {
	return writePersistentAssignment(configHome, path, name, value, &expected)
}

func writePersistentAssignment(
	configHome string,
	path string,
	name string,
	value *string,
	expected *PersistentFileSnapshot,
) error {
	if err := ensurePersistentRoot(configHome); err != nil {
		return err
	}
	unlock, err := LockRoot(configHome, true)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := persistentFileSnapshot(path)
	if err != nil {
		return err
	}
	if expected != nil && !samePersistentFileSnapshot(current, *expected) {
		return ErrPersistentTargetStale
	}
	content := current.Content
	updated, err := EditPersistentAssignmentContent(path, content, name, value)
	if err != nil {
		return err
	}
	if value == nil && len(updated) == 0 {
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
			return errors.New("persistent setting target is not a regular file")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
			return errors.New("persistent setting target has unsafe ownership or hard links")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		return syncPersistentDirectory(filepath.Dir(path))
	}
	return writePersistentFile(configHome, path, updated)
}

// EditPersistentAssignmentContent returns the complete persistent settings
// content after applying one assignment change without writing it.
func EditPersistentAssignmentContent(
	path string,
	content []byte,
	name string,
	value *string,
) ([]byte, error) {
	return editAssignmentContent(path, content, name, value)
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
func WritePersistentFileIfUnchanged(
	configHome string,
	path string,
	expected PersistentFileSnapshot,
	content []byte,
) error {
	if err := ensurePersistentRoot(configHome); err != nil {
		return err
	}
	unlock, err := LockRoot(configHome, true)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := persistentFileSnapshot(path)
	if err != nil {
		return err
	}
	if !samePersistentFileSnapshot(current, expected) {
		return ErrPersistentTargetStale
	}
	return writePersistentFile(configHome, path, content)
}

func persistentFileSnapshot(path string) (PersistentFileSnapshot, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return PersistentFileSnapshot{}, nil
	}
	if err != nil {
		return PersistentFileSnapshot{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return PersistentFileSnapshot{}, errors.New(
			"persistent setting target must be a bounded regular non-symlink file",
		)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return PersistentFileSnapshot{}, err
	}
	return PersistentFileSnapshot{Exists: true, Content: content}, nil
}

func samePersistentFileSnapshot(current, expected PersistentFileSnapshot) bool {
	return current.Exists == expected.Exists && bytes.Equal(current.Content, expected.Content)
}

// CreatePersistentFile atomically stores a new persistent file and refuses to
// replace an existing target.
func CreatePersistentFile(configHome, path string, content []byte) error {
	if err := ensurePersistentRoot(configHome); err != nil {
		return err
	}
	unlock, err := LockRoot(configHome, true)
	if err != nil {
		return err
	}
	defer unlock()
	return persistFile(configHome, path, content, false)
}

type envEditRecord struct {
	content []byte
	name    string
}

func editAssignmentContent(
	path string,
	content []byte,
	name string,
	value *string,
) ([]byte, error) {
	records, err := parseEnvEditRecords(path, content)
	if err != nil {
		return nil, err
	}
	replacement := []byte(name + "=" + quotePersistentValue(dereference(value)) + "\n")
	last := -1
	for index := range records {
		if records[index].name == name {
			last = index
		}
	}
	var result bytes.Buffer
	for index, record := range records {
		if record.name != name {
			result.Write(record.content)
			continue
		}
		if value != nil && index == last {
			result.Write(replacement)
		}
	}
	if value != nil && last == -1 {
		if result.Len() != 0 && result.Bytes()[result.Len()-1] != '\n' {
			result.WriteByte('\n')
		}
		result.Write(replacement)
	}
	return result.Bytes(), nil
}

func parseEnvEditRecords(path string, content []byte) ([]envEditRecord, error) {
	lines := bytes.SplitAfter(content, []byte{'\n'})
	values := environment{}
	var records []envEditRecord
	var current bytes.Buffer
	quote := byte(0)
	lineNumber := 0
	recordLine := 0
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		lineNumber++
		if current.Len() == 0 {
			recordLine = lineNumber
		}
		current.Write(line)
		scan := strings.TrimSuffix(string(line), "\n")
		quote = scanQuote(scan, quote)
		if quote != 0 {
			continue
		}
		raw := append([]byte(nil), current.Bytes()...)
		trimmed := strings.TrimSpace(string(raw))
		record := envEditRecord{content: raw}
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			var assigned string
			if err := applyRecord(trimmed, values, func(name, _ string) {
				assigned = name
			}); err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, recordLine, err)
			}
			record.name = assigned
		}
		records = append(records, record)
		current.Reset()
	}
	if quote != 0 || current.Len() != 0 {
		return nil, fmt.Errorf("%s:%d: unterminated quoted assignment", path, recordLine)
	}
	return records, nil
}

func quotePersistentValue(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func dereference(value *string) string {
	if value == nil {
		return ""
	}
	return *value
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
	if info, err := os.Lstat(path); err == nil {
		if !replace {
			return errors.New("persistent setting target already exists")
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("persistent setting target is not a regular file")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
			return errors.New("persistent setting target has unsafe ownership or hard links")
		}
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
	if err := file.Chmod(0o600); err != nil {
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
		return fmt.Errorf("persistent setting directory is unsafe: %s", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("persistent setting directory is not operator-owned: %s", path)
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
