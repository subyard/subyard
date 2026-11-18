package config

import (
	"bytes"
	"fmt"
	"maps"
	"slices"

	"github.com/Subyard/Subyard/internal/domain"
)

// PersistentScalarEdit uses a canonical setting name. A nil value unsets it.
type PersistentScalarEdit struct {
	Name  string
	Value *string
}

// PersistentScalarEdits retains request order when appending new records.
type PersistentScalarEdits []PersistentScalarEdit

func (edits PersistentScalarEdits) requestedValues() map[string]*string {
	values := make(map[string]*string, len(edits))
	for _, edit := range edits {
		values[edit.Name] = edit.Value
	}
	return values
}

// LayerContent edits records using the assignments observed in each yard's
// production load of this layer. A direct assignment is always editable,
// including an input that the loader discards.
func (edits PersistentScalarEdits) LayerContent(path string, content []byte, before []Loaded) ([]byte, error) {
	assignments := make([][]observedAssignment, 0, len(before))
	for _, loaded := range before {
		assignments = append(assignments, loaded.scalarEditAssignments)
	}
	return edits.editContent(path, content, assignments, true)
}

func (edits PersistentScalarEdits) editContent(path string, content []byte, assignments [][]observedAssignment, keepSame bool) ([]byte, error) {
	requestedValues := edits.requestedValues()
	byLine := map[int][]string{}
	last := map[string]int{}
	var records []envRecord
	literals := map[int]string{}
	err := scanEnvRecords(bytes.NewReader(content), path, "persistent settings", func(record envRecord) error {
		records = append(records, record)
		direct, dynamic := persistentDirectAssignment(record.text)
		name := canonicalSettingName(direct)
		if _, requested := requestedValues[name]; requested {
			byLine[record.line] = append(byLine[record.line], name)
			last[name] = record.line
			if direct == name && !dynamic {
				literals[record.line] = name
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	unchanged := make(map[string]bool, len(edits))
	for name := range requestedValues {
		unchanged[name] = keepSame && len(assignments) != 0
	}
	for _, layer := range assignments {
		effective := map[string]observedAssignment{}
		for _, assignment := range layer {
			name := canonicalSettingName(assignment.name)
			if _, requested := requestedValues[name]; requested {
				byLine[assignment.line] = append(byLine[assignment.line], name)
				last[name] = max(last[name], assignment.line)
				effective[name] = assignment
			}
		}
		for name, value := range requestedValues {
			assignment, exists := effective[name]
			if value == nil {
				unchanged[name] = unchanged[name] && last[name] == 0
			} else {
				unchanged[name] = unchanged[name] && exists && literals[assignment.line] == name && assignment.value == *value
			}
		}
	}

	var result bytes.Buffer
	write := func(name string) {
		if value := requestedValues[name]; value != nil {
			if result.Len() != 0 && result.Bytes()[result.Len()-1] != '\n' {
				result.WriteByte('\n')
			}
			fmt.Fprintf(&result, "%s=%s\n", name, quotePersistentValue(*value))
		}
	}
	for _, record := range records {
		names := slices.DeleteFunc(byLine[record.line], func(name string) bool { return unchanged[name] })
		if len(names) == 0 {
			result.WriteString(record.raw)
			continue
		}
		slices.Sort(names)
		for _, name := range slices.Compact(names) {
			if last[name] == record.line {
				write(name)
			}
		}
	}
	for _, edit := range edits {
		if !unchanged[edit.Name] && last[edit.Name] == 0 {
			write(edit.Name)
		}
	}
	if result.Len() > 8<<20 {
		return nil, sourceError(path, 0, "", "persistent settings", "edited content exceeds its size bound", nil)
	}
	return result.Bytes(), nil
}

// CheckLayer compares effective values and presence across production loads of
// one yard. It also checks that conditional records in the edited layer did
// not reintroduce an unset or override the replacement.
func (edits PersistentScalarEdits) CheckLayer(path string, before, after Loaded) error {
	const role = "persistent settings"
	requestedValues := edits.requestedValues()
	var refusals []error
	last := map[string]observedAssignment{}
	for _, assignment := range after.scalarEditAssignments {
		if value, requested := requestedValues[assignment.name]; requested {
			if value == nil {
				refusals = append(refusals, sourceError(path, assignment.line, assignment.name, role, "setting is still assigned by a preserved record; simplify conditional assignments before authoring", nil))
			}
			last[assignment.name] = assignment
		}
	}
	for _, name := range slices.Sorted(maps.Keys(last)) {
		if value, assignment := requestedValues[name], last[name]; value != nil && assignment.value != *value {
			refusals = append(refusals, sourceError(path, assignment.line, name, role, "a preserved record changes the requested setting; simplify conditional assignments before authoring", nil))
		}
	}
	requestChanged := false
	for name := range requestedValues {
		if !sameSettingValue(before.Environment, after.Environment, name) {
			requestChanged = true
		}
	}
	check := func(key string) {
		if _, requested := requestedValues[key]; requested {
			return
		}
		if _, setting := before.Catalog.LookupSetting(key); !setting {
			return
		}
		if sameSettingValue(before.Environment, after.Environment, key) {
			return
		}
		// An unchanged input can have a new built-in result, such as an auto
		// port derived from the requested SSH_PORT.
		if requestChanged && sameSettingValue(before.scalarEditValues, after.scalarEditValues, key) {
			return
		}
		// Selecting a yard template also selects its shipped defaults. Any
		// assignment in an operator layer remains subject to preservation.
		if _, template := requestedValues["YARD_TEMPLATE"]; template && !sameSettingValue(before.Environment, after.Environment, "YARD_TEMPLATE") &&
			!hasOperatorAssignment(before.Settings[key]) && !hasOperatorAssignment(after.Settings[key]) {
			return
		}
		reason := "edit changes effective setting " + key + " in yard " + after.Context.YardName + "; simplify dependent assignments before authoring"
		refusals = append(refusals, sourceError(path, 0, "", role, reason, nil))
	}
	for _, key := range slices.Sorted(maps.Keys(before.Environment)) {
		check(key)
	}
	for _, key := range slices.Sorted(maps.Keys(after.Environment)) {
		if _, existed := before.Environment[key]; !existed {
			check(key)
		}
	}
	return domain.CollectRefusals(refusals...)
}

func sameSettingValue(left, right map[string]string, name string) bool {
	before, wasPresent := left[name]
	after, present := right[name]
	return wasPresent == present && before == after
}

func hasOperatorAssignment(setting SettingTrace) bool {
	return slices.ContainsFunc(setting.Resolutions, func(resolution SettingResolution) bool {
		return resolution.Status != "unset" && (resolution.Role == "scalar settings" || resolution.Role == "Git scalar settings")
	})
}
