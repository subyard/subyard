// Package testimpact defines the strict, Git-independent change-set model.
package testimpact

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"unicode/utf8"
)

type ChangeSet struct {
	SchemaVersion int
	Changes       []Change
}

type Change struct {
	Status     string
	Similarity *int
	OldPath    *string
	NewPath    *string
	OldMode    string
	NewMode    string
}

type changeSetWire struct {
	SchemaVersion json.RawMessage `json:"schema_version"`
	Changes       json.RawMessage `json:"changes"`
}

type changeWire struct {
	Status     json.RawMessage `json:"status"`
	Similarity json.RawMessage `json:"similarity"`
	OldPath    json.RawMessage `json:"old_path"`
	NewPath    json.RawMessage `json:"new_path"`
	OldMode    json.RawMessage `json:"old_mode"`
	NewMode    json.RawMessage `json:"new_mode"`
}

// DecodeChangeSet reads one canonical, schema-version 1 change-set document.
func DecodeChangeSet(reader io.Reader) (ChangeSet, error) {
	if reader == nil {
		return ChangeSet{}, errors.New("change set reader is nil")
	}
	source, err := io.ReadAll(reader)
	if err != nil {
		return ChangeSet{}, fmt.Errorf("read change set: %w", err)
	}
	if !utf8.Valid(source) {
		return ChangeSet{}, errors.New("change set contains invalid UTF-8")
	}
	if bytes.IndexByte(source, 0) >= 0 {
		return ChangeSet{}, errors.New("change set contains NUL")
	}
	if err := rejectUnpairedUnicodeSurrogateEscapes(source); err != nil {
		return ChangeSet{}, fmt.Errorf("change set contains %w", err)
	}
	if err := rejectDuplicateMembers(source); err != nil {
		return ChangeSet{}, err
	}

	var wire changeSetWire
	if err := decodeStrict(source, &wire); err != nil {
		return ChangeSet{}, fmt.Errorf("decode change set: %w", err)
	}
	if !present(wire.SchemaVersion) || !present(wire.Changes) {
		return ChangeSet{}, errors.New("change set must include schema_version and changes")
	}
	if isNull(wire.SchemaVersion) || isNull(wire.Changes) {
		return ChangeSet{}, errors.New("change set fields cannot be null")
	}

	var schemaVersion int
	if err := decodeStrict(wire.SchemaVersion, &schemaVersion); err != nil {
		return ChangeSet{}, fmt.Errorf("decode schema_version: %w", err)
	}
	if schemaVersion != 1 {
		return ChangeSet{}, fmt.Errorf("unsupported schema_version %d", schemaVersion)
	}

	var records []json.RawMessage
	if err := decodeStrict(wire.Changes, &records); err != nil {
		return ChangeSet{}, fmt.Errorf("decode changes: %w", err)
	}

	changes := make([]Change, 0, len(records))
	exact := make(map[changeKey]struct{}, len(records))
	newPaths := make(map[string]struct{}, len(records))
	oldPaths := make(map[string]oldPathUse, len(records))
	for index, record := range records {
		change, err := decodeChange(record)
		if err != nil {
			return ChangeSet{}, fmt.Errorf("decode changes[%d]: %w", index, err)
		}
		if err := ValidateChange(change); err != nil {
			return ChangeSet{}, fmt.Errorf("validate changes[%d]: %w", index, err)
		}

		recordKey := makeChangeKey(change)
		if _, exists := exact[recordKey]; exists {
			return ChangeSet{}, fmt.Errorf("changes[%d] duplicates another change", index)
		}
		exact[recordKey] = struct{}{}

		if err := registerIdentities(change, newPaths, oldPaths); err != nil {
			return ChangeSet{}, fmt.Errorf("changes[%d] conflicts with another change: %w", index, err)
		}
		changes = append(changes, change)
	}

	slices.SortFunc(changes, compareChange)
	return ChangeSet{SchemaVersion: schemaVersion, Changes: changes}, nil
}

func rejectUnpairedUnicodeSurrogateEscapes(source []byte) error {
	inString := false
	for index := 0; index < len(source); index++ {
		switch source[index] {
		case '"':
			inString = !inString
		case '\\':
			if !inString || index+1 >= len(source) {
				continue
			}
			index++
			if source[index] != 'u' || index+4 >= len(source) {
				continue
			}
			value, ok := decodeHexQuad(source[index+1 : index+5])
			if !ok {
				continue
			}
			index += 4
			switch {
			case value >= 0xd800 && value <= 0xdbff:
				if index+6 >= len(source) || source[index+1] != '\\' || source[index+2] != 'u' {
					return errors.New("an unpaired Unicode surrogate escape")
				}
				low, ok := decodeHexQuad(source[index+3 : index+7])
				if !ok || low < 0xdc00 || low > 0xdfff {
					return errors.New("an unpaired Unicode surrogate escape")
				}
				index += 6
			case value >= 0xdc00 && value <= 0xdfff:
				return errors.New("an unpaired Unicode surrogate escape")
			}
		}
	}
	return nil
}

func decodeHexQuad(source []byte) (uint16, bool) {
	if len(source) != 4 {
		return 0, false
	}
	var value uint16
	for _, character := range source {
		value <<= 4
		switch {
		case character >= '0' && character <= '9':
			value += uint16(character - '0')
		case character >= 'a' && character <= 'f':
			value += uint16(character-'a') + 10
		case character >= 'A' && character <= 'F':
			value += uint16(character-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

// ValidateChange verifies a change against the version 1 schema contract.
func ValidateChange(change Change) error {
	switch change.Status {
	case "A":
		if change.OldPath != nil || change.NewPath == nil {
			return errors.New("A requires a null old_path and a new_path")
		}
		if change.OldMode != "000000" || !nonZeroMode(change.NewMode) {
			return errors.New("A requires old_mode 000000 and a non-zero new_mode")
		}
		if change.Similarity != nil {
			return errors.New("A requires null similarity")
		}
		return validatePath(*change.NewPath)
	case "D":
		if change.OldPath == nil || change.NewPath != nil {
			return errors.New("D requires an old_path and a null new_path")
		}
		if !nonZeroMode(change.OldMode) || change.NewMode != "000000" {
			return errors.New("D requires a non-zero old_mode and new_mode 000000")
		}
		if change.Similarity != nil {
			return errors.New("D requires null similarity")
		}
		return validatePath(*change.OldPath)
	case "M", "T":
		if err := validateMatchingPaths(change); err != nil {
			return err
		}
		if !nonZeroMode(change.OldMode) || !nonZeroMode(change.NewMode) {
			return fmt.Errorf("%s requires non-zero modes", change.Status)
		}
		if change.Similarity != nil {
			return fmt.Errorf("%s requires null similarity", change.Status)
		}
		return nil
	case "R", "C":
		if err := validateDistinctPaths(change); err != nil {
			return err
		}
		if !nonZeroMode(change.OldMode) || !nonZeroMode(change.NewMode) {
			return fmt.Errorf("%s requires non-zero modes", change.Status)
		}
		if change.Similarity == nil || *change.Similarity < 0 || *change.Similarity > 100 {
			return fmt.Errorf("%s requires similarity from 0 through 100", change.Status)
		}
		return nil
	default:
		return fmt.Errorf("unsupported status %q", change.Status)
	}
}

func decodeChange(source json.RawMessage) (Change, error) {
	var wire changeWire
	if err := decodeStrict(source, &wire); err != nil {
		return Change{}, err
	}
	fields := []struct {
		name  string
		value json.RawMessage
	}{
		{"status", wire.Status},
		{"similarity", wire.Similarity},
		{"old_path", wire.OldPath},
		{"new_path", wire.NewPath},
		{"old_mode", wire.OldMode},
		{"new_mode", wire.NewMode},
	}
	for _, field := range fields {
		if !present(field.value) {
			return Change{}, fmt.Errorf("missing %s", field.name)
		}
	}

	status, err := requiredString(wire.Status, "status")
	if err != nil {
		return Change{}, err
	}
	oldMode, err := requiredString(wire.OldMode, "old_mode")
	if err != nil {
		return Change{}, err
	}
	newMode, err := requiredString(wire.NewMode, "new_mode")
	if err != nil {
		return Change{}, err
	}
	oldPath, err := nullableString(wire.OldPath, "old_path")
	if err != nil {
		return Change{}, err
	}
	newPath, err := nullableString(wire.NewPath, "new_path")
	if err != nil {
		return Change{}, err
	}
	similarity, err := nullableInt(wire.Similarity, "similarity")
	if err != nil {
		return Change{}, err
	}
	return Change{
		Status:     status,
		Similarity: similarity,
		OldPath:    oldPath,
		NewPath:    newPath,
		OldMode:    oldMode,
		NewMode:    newMode,
	}, nil
}

func requiredString(raw json.RawMessage, name string) (string, error) {
	if isNull(raw) {
		return "", fmt.Errorf("%s cannot be null", name)
	}
	var value string
	if err := decodeStrict(raw, &value); err != nil {
		return "", fmt.Errorf("decode %s: %w", name, err)
	}
	return value, nil
}

func nullableString(raw json.RawMessage, name string) (*string, error) {
	if isNull(raw) {
		return nil, nil
	}
	var value string
	if err := decodeStrict(raw, &value); err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	return &value, nil
}

func nullableInt(raw json.RawMessage, name string) (*int, error) {
	if isNull(raw) {
		return nil, nil
	}
	var value int
	if err := decodeStrict(raw, &value); err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	return &value, nil
}

func validateMatchingPaths(change Change) error {
	if change.OldPath == nil || change.NewPath == nil || *change.OldPath != *change.NewPath {
		return fmt.Errorf("%s requires equal old_path and new_path", change.Status)
	}
	return validatePath(*change.OldPath)
}

func validateDistinctPaths(change Change) error {
	if change.OldPath == nil || change.NewPath == nil || *change.OldPath == *change.NewPath {
		return fmt.Errorf("%s requires distinct old_path and new_path", change.Status)
	}
	if err := validatePath(*change.OldPath); err != nil {
		return err
	}
	return validatePath(*change.NewPath)
}

func validatePath(value string) error {
	if value == "" || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("invalid path %q", value)
	}
	if path.IsAbs(value) {
		return fmt.Errorf("path must be relative: %q", value)
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return fmt.Errorf("path traversal is not allowed: %q", value)
		}
	}
	return nil
}

func nonZeroMode(value string) bool {
	switch value {
	case "100644", "100755", "120000", "160000":
		return true
	default:
		return false
	}
}

func present(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) != 0
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func decodeStrict(source []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicateMembers(source []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return fmt.Errorf("scan change set JSON: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("change set has trailing JSON value")
		}
		return fmt.Errorf("scan change set JSON: %w", err)
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		members := make(map[string]struct{})
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := token.(string)
			if !ok {
				return errors.New("object member name is not a string")
			}
			if _, exists := members[name]; exists {
				return fmt.Errorf("duplicate object member %q", name)
			}
			members[name] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return nil
	}
	_, err = decoder.Token()
	return err
}

type changeKey struct {
	status           string
	similarity       int
	hasSimilarity    bool
	oldPath          string
	hasOldPath       bool
	newPath          string
	hasNewPath       bool
	oldMode, newMode string
}

func makeChangeKey(change Change) changeKey {
	key := changeKey{
		status:        change.Status,
		oldMode:       change.OldMode,
		newMode:       change.NewMode,
		hasOldPath:    change.OldPath != nil,
		hasNewPath:    change.NewPath != nil,
		hasSimilarity: change.Similarity != nil,
	}
	if change.OldPath != nil {
		key.oldPath = *change.OldPath
	}
	if change.NewPath != nil {
		key.newPath = *change.NewPath
	}
	if change.Similarity != nil {
		key.similarity = *change.Similarity
	}
	return key
}

type oldPathUse struct {
	copyCount     int
	nonCopyStatus string
}

func registerIdentities(change Change, newPaths map[string]struct{}, oldPaths map[string]oldPathUse) error {
	if change.NewPath != nil {
		if _, exists := newPaths[*change.NewPath]; exists {
			return fmt.Errorf("multiple changes produce %q", *change.NewPath)
		}
		newPaths[*change.NewPath] = struct{}{}
	}
	if change.OldPath == nil {
		return nil
	}

	oldPath := *change.OldPath
	use := oldPaths[oldPath]
	if change.Status == "C" {
		if use.nonCopyStatus != "" && use.nonCopyStatus != "M" && use.nonCopyStatus != "T" {
			return fmt.Errorf("copy cannot share old_path %q with %s", oldPath, use.nonCopyStatus)
		}
		use.copyCount++
		oldPaths[oldPath] = use
		return nil
	}

	if use.nonCopyStatus != "" {
		return fmt.Errorf("%s cannot share old_path %q with %s", change.Status, oldPath, use.nonCopyStatus)
	}
	if use.copyCount > 0 && change.Status != "M" && change.Status != "T" {
		return fmt.Errorf("%s cannot share old_path %q with copies", change.Status, oldPath)
	}
	use.nonCopyStatus = change.Status
	oldPaths[oldPath] = use
	return nil
}

func compareChange(left, right Change) int {
	if result := cmp.Compare(left.Status, right.Status); result != 0 {
		return result
	}
	if result := compareOptionalString(left.OldPath, right.OldPath); result != 0 {
		return result
	}
	if result := compareOptionalString(left.NewPath, right.NewPath); result != 0 {
		return result
	}
	if result := cmp.Compare(left.OldMode, right.OldMode); result != 0 {
		return result
	}
	if result := cmp.Compare(left.NewMode, right.NewMode); result != 0 {
		return result
	}
	return compareOptionalInt(left.Similarity, right.Similarity)
}

func compareOptionalString(left, right *string) int {
	if left == nil && right == nil {
		return 0
	}
	if left == nil {
		return -1
	}
	if right == nil {
		return 1
	}
	return cmp.Compare(*left, *right)
}

func compareOptionalInt(left, right *int) int {
	if left == nil && right == nil {
		return 0
	}
	if left == nil {
		return -1
	}
	if right == nil {
		return 1
	}
	return cmp.Compare(*left, *right)
}
