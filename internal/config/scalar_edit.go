package config

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

// ErrDynamicScalarRecord marks a record that requires a manual edit.
var ErrDynamicScalarRecord = errors.New("record uses a parameter expansion or setting reference; edit the file manually")

// PersistentScalarEdit names a setting. A nil value unsets it.
type PersistentScalarEdit struct {
	Name  string
	Value *string
}

// PersistentScalarEdits retains request order when appending new records.
type PersistentScalarEdits []PersistentScalarEdit

type scalarEditRecord struct {
	envRecord
	name string
	edit *PersistentScalarEdit
}

func (edits PersistentScalarEdits) readRecords(path string, content []byte) ([]scalarEditRecord, error) {
	const role = "persistent settings"
	if len(content) > 8<<20 {
		return nil, sourceError(path, 0, "", role, "target exceeds its size bound", nil)
	}
	selected := make(map[string]bool, len(edits))
	for _, edit := range edits {
		// Profile-only values are ignored in operator layers and may be removed.
		if edit.Value != nil || !IsProfileOnlySetting(edit.Name) {
			selected[CanonicalSettingName(edit.Name)] = true
		}
	}
	var records []scalarEditRecord
	var refusals []error
	err := scanEnvRecords(bytes.NewReader(content), path, role, func(record envRecord) error {
		name, raw := envRecordAssignment(record.text)
		if record.text != "" && !strings.HasPrefix(record.text, ":") && !ValidVariable(name) {
			refusals = append(refusals, sourceError(path, record.line, "", role, "only variable assignments are allowed", nil))
			return nil
		}
		records = append(records, scalarEditRecord{envRecord: record, name: name})
		value, expands := decodeValue(raw)
		if !expands {
			return nil
		}
		dynamic, readsTarget := false, false
		for index := 0; index < len(value); index++ {
			switch value[index] {
			case '\\':
				index++
			case '$':
				start := index + 1
				braced := start < len(value) && value[start] == '{'
				if braced {
					start++
				}
				// Keep scanning after the name so nested fallbacks are checked too.
				end := start
				for end < len(value) && variableChar(value[end], end == start) {
					end++
				}
				dynamic = dynamic || braced || end > start
				readsTarget = readsTarget || selected[CanonicalSettingName(value[start:end])]
			}
		}
		if readsTarget || dynamic && selected[CanonicalSettingName(name)] {
			refusals = append(refusals, sourceError(path, record.line, recordSettingKey(record.text), role,
				ErrDynamicScalarRecord.Error(), ErrDynamicScalarRecord))
		} else if !IsProfileOnlySetting(name) && (strings.Contains(value, "$(") || strings.ContainsRune(value, '`')) {
			refusals = append(refusals, sourceError(path, record.line, recordSettingKey(record.text), role,
				"command substitution is not allowed in config", nil))
		}
		return nil
	})
	return records, domain.CollectRefusals(append(refusals, err)...)
}

func envRecordAssignment(record string) (name, raw string) {
	if expression, ok := strings.CutPrefix(record, ":"); ok {
		return "", strings.TrimSpace(expression)
	}
	record = strings.TrimSpace(strings.TrimPrefix(record, "export "))
	if name, raw, ok := strings.Cut(record, "="); ok {
		return strings.TrimSpace(name), strings.TrimSpace(raw)
	}
	return "", ""
}

// CheckLayer rejects dynamic records related to the selected settings.
func (edits PersistentScalarEdits) CheckLayer(path string, content []byte) error {
	_, err := edits.readRecords(path, content)
	return err
}

func (tracker *settingTracker) checkScalarEditLayers(edits PersistentScalarEdits, paths ...string) error {
	var refusals []error
	for _, path := range paths {
		if path == "" {
			continue
		}
		if batch := tracker.batch; batch != nil && path == batch.path {
			if batch.baseline != nil {
				refusals = append(refusals, edits.CheckLayer(path, batch.baseline.Content))
			}
			continue
		}
		exists, err := tracker.fileExists(path)
		if err == nil && exists {
			if read := tracker.prospectiveReader(); read != nil {
				var content []byte
				content, err = read(path)
				if err == nil {
					err = edits.CheckLayer(path, content)
				}
			} else {
				err = edits.checkFile(path, tracker.fileSource(path))
			}
		}
		refusals = append(refusals, err)
	}
	return domain.CollectRefusals(refusals...)
}

func (edits PersistentScalarEdits) checkFile(path, source string) error {
	file, err := os.Open(source)
	if err != nil {
		return sourceAccessError(path, "persistent settings", err)
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, (8<<20)+1))
	if err != nil {
		return sourceAccessError(path, "persistent settings", err)
	}
	return edits.CheckLayer(path, content)
}

func (edits PersistentScalarEdits) editRecords(path string, content []byte) ([]scalarEditRecord, error) {
	records, err := edits.readRecords(path, content)
	if err != nil {
		return nil, err
	}
	return edits.placeRecords(records), nil
}

func (edits PersistentScalarEdits) placeRecords(records []scalarEditRecord) []scalarEditRecord {
	last := make(map[string]int, len(edits))
	replacements := make(map[string]*PersistentScalarEdit, len(edits))
	for _, edit := range edits {
		replacements[CanonicalSettingName(edit.Name)] = &edit
	}
	for index, record := range records {
		name := CanonicalSettingName(record.name)
		if replacements[name] != nil {
			last[name] = index
		}
	}
	result := make([]scalarEditRecord, 0, len(records)+len(edits))
	for index, record := range records {
		name := CanonicalSettingName(record.name)
		edit := replacements[name]
		if edit == nil {
			result = append(result, record)
		} else if edit.Value != nil && index == last[name] {
			record.name, record.edit = name, edit
			result = append(result, record)
		}
	}
	for _, edit := range edits {
		name := CanonicalSettingName(edit.Name)
		if _, exists := last[name]; !exists && edit.Value != nil {
			result = append(result, scalarEditRecord{name: name, edit: &edit})
		}
	}
	return result
}

// LayerContent updates selected literal assignments and preserves other records.
func (edits PersistentScalarEdits) LayerContent(path string, content []byte) ([]byte, error) {
	records, err := edits.editRecords(path, content)
	if err != nil {
		return nil, err
	}
	return renderScalarEditRecords(path, records)
}

func renderScalarEditRecords(path string, records []scalarEditRecord) ([]byte, error) {
	var result bytes.Buffer
	for _, record := range records {
		if record.edit == nil {
			result.WriteString(record.raw)
			continue
		}
		if result.Len() != 0 && result.Bytes()[result.Len()-1] != '\n' {
			result.WriteByte('\n')
		}
		result.WriteString(record.name + "=" + quotePersistentValue(*record.edit.Value))
		if strings.HasSuffix(record.raw, "\r\n") {
			result.WriteByte('\r')
		}
		result.WriteByte('\n')
	}
	if result.Len() > 8<<20 {
		return nil, sourceError(path, 0, "", "persistent settings", "edited content exceeds its size bound", nil)
	}
	return result.Bytes(), nil
}
