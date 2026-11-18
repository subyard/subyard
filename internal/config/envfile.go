package config

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

type environment map[string]string

const escapedEnvApostrophe = `'\''`

type assignmentObserver func(name, value string, line int)

func ReadAssignments(path string) (map[string]string, error) {
	return ReadAssignmentsOver(path, nil)
}

func ReadAssignmentsOver(path string, base map[string]string) (map[string]string, error) {
	values := make(environment, len(base))
	for name, value := range base {
		values[name] = value
	}
	if err := applyEnvFile(path, values); err != nil {
		return nil, err
	}
	result := make(map[string]string, len(values))
	for name, value := range values {
		result[name] = value
	}
	return result, nil
}

// ReadLiteralAssignments reads a user settings file without scope validation.
func ReadLiteralAssignments(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, sourceAccessError(path, "scalar settings", err)
	}
	defer file.Close()
	values := environment{}
	if err := applyLiteralEnv(file, path, values, nil, discardPolicy{}, nil); err != nil {
		return nil, err
	}
	return values, nil
}

func applyEnvFile(path string, values environment) error {
	return applyEnvFileObserved(path, values, nil)
}

func applyEnvFileObserved(path string, values environment, observer assignmentObserver, readers ...func(string) ([]byte, error)) error {
	var input io.Reader
	if len(readers) != 0 && readers[0] != nil {
		payload, err := readers[0](path)
		if err != nil {
			return sourceAccessError(path, "scalar settings", err)
		}
		input = bytes.NewReader(payload)
	} else {
		file, err := os.Open(path)
		if err != nil {
			return sourceAccessError(path, "scalar settings", err)
		}
		defer file.Close()
		input = file
	}
	return applyEnvReaderObserved(input, path, values, observer)
}

// discardPolicy keeps retired settings and operator-only discards out of the resolved environment.
type discardPolicy struct {
	retired, ignored func(string) bool
	batch            *localBatch
	unavailable      *bool
	literal          bool
}

func (policy discardPolicy) discards(name string) bool {
	return isRetiredSetting(name) || policy.retired != nil && policy.retired(name) || policy.ignores(name)
}

func (policy discardPolicy) ignores(name string) bool {
	return policy.ignored != nil && policy.ignored(name)
}

func applyEnvReaderObserved(reader io.Reader, source string, values environment, observer assignmentObserver) error {
	return applyShippedEnv(reader, source, values, observer, discardPolicy{})
}

func applyShippedEnv(reader io.Reader, source string, values environment, observer assignmentObserver, policy discardPolicy) error {
	discardRetiredSettings(values)

	return scanEnvRecords(reader, source, "scalar settings", false, func(record envRecord) error {
		if record.text == "" {
			return nil
		}
		return applyRecordRetired(record.text, values, func(name, value string) {
			if observer != nil {
				observer(name, value, record.line)
			}
		}, policy)
	})
}

type envRecord struct {
	raw  string
	text string
	line int
}

var errSettingExpansion = errors.New("settings files accept only literal values. Replace the parameter expansion or setting reference with its value")

func scanLiteralEnv(reader io.Reader, path, role string, visit func(record envRecord, name, value string)) error {
	var refusals []error
	err := scanEnvRecords(reader, path, role, true, func(record envRecord) error {
		if record.text == "" {
			visit(record, "", "")
			return nil
		}
		name, raw := envRecordAssignment(record.text)
		value, expand := decodeValue(raw)
		var err error
		if expand {
			value, err = expandValue(value, nil, nil, discardPolicy{literal: true})
		} else {
			value = strings.ReplaceAll(value, escapedEnvApostrophe, "'")
			if strings.ContainsRune(strings.ReplaceAll(raw[1:len(raw)-1], escapedEnvApostrophe, ""), '\'') {
				err = sourceError("", 0, "", role, "invalid single-quoted value. Replace it with one literal value", nil)
			}
		}
		if err == nil {
			switch {
			case name == "":
				err = sourceError("", 0, "", role, "only variable assignments are allowed", nil)
			case !ValidVariable(name):
				err = sourceError("", 0, "", role, "invalid variable name", nil)
			}
		}
		if err != nil {
			refusals = append(refusals, sourceContext(path, record.line, recordSettingKey(record.text), role, err))
		} else {
			visit(record, name, value)
		}
		return nil
	})
	return domain.CollectRefusals(append(refusals, err)...)
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

func applyLiteralEnv(reader io.Reader, path string, values environment, observe assignmentObserver, policy discardPolicy, discard assignmentObserver) error {
	discardRetiredSettings(values)
	return scanLiteralEnv(reader, path, "scalar settings", func(record envRecord, name, value string) {
		if name != "" {
			applyLiteralAssignment(name, value, record.line, values, observe, policy, discard)
		}
	})
}

func applyLiteralAssignment(name, value string, line int, values environment, observe assignmentObserver, policy discardPolicy, discard assignmentObserver) {
	if policy.discards(name) {
		if discard != nil {
			discard(name, value, line)
		}
		return
	}
	values[name] = value
	if policy.batch != nil {
		policy.batch.setAvailable(name, true)
	}
	if observe != nil {
		observe(name, value, line)
	}
}

// scanEnvRecords preserves raw bytes for edits and supplies the same
// evaluation text and line numbers to each consumer. Blank and comment lines
// outside an assignment have empty evaluation text.
func scanEnvRecords(reader io.Reader, source, purpose string, preserveEndings bool, visit func(envRecord) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	if preserveEndings {
		scanner.Split(scanEnvLine)
	}
	var raw strings.Builder
	var quote byte
	lineNumber, recordLine := 0, 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		body := strings.TrimSuffix(line, "\n")
		if raw.Len() == 0 {
			trimmed := strings.TrimSpace(body)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				if err := visit(envRecord{raw: line, line: lineNumber}); err != nil {
					return err
				}
				continue
			}
			recordLine = lineNumber
		} else if !preserveEndings {
			raw.WriteByte('\n')
		}
		raw.WriteString(line)
		quote = scanQuote(body, quote)
		if quote != 0 {
			continue
		}
		record := envRecord{raw: raw.String(), text: strings.TrimSpace(raw.String()), line: recordLine}
		if err := visit(record); err != nil {
			return sourceContext(source, record.line, recordSettingKey(record.text), purpose, err)
		}
		raw.Reset()
	}
	if err := scanner.Err(); err != nil {
		return sourceError(source, lineNumber+1, "", purpose, "cannot read config within the line size bound", err)
	}
	if raw.Len() != 0 {
		return sourceError(source, recordLine, recordSettingKey(raw.String()), purpose, "unterminated quoted assignment", nil)
	}
	return nil
}

func scanEnvLine(data []byte, atEOF bool) (int, []byte, error) {
	if end := bytes.IndexByte(data, '\n'); end >= 0 {
		return end + 1, data[:end+1], nil
	}
	if atEOF && len(data) != 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func scanQuote(line string, quote byte) byte {
	escaped := false
	for index := 0; index < len(line); index++ {
		char := line[index]
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote == 0 && (char == '\'' || char == '"') {
			quote = char
			continue
		}
		if quote == char {
			quote = 0
		}
	}
	return quote
}

func applyRecordRetired(record string, values environment, observer func(name, value string), policy discardPolicy) error {
	unavailable := false
	policy.unavailable = &unavailable
	record = strings.TrimSpace(record)
	if strings.HasPrefix(record, ":") {
		expression := strings.TrimSpace(strings.TrimPrefix(record, ":"))
		value, expand := decodeValue(expression)
		if !expand {
			return nil
		}
		_, err := expandValue(value, values, observer, policy)
		return err
	}
	if strings.HasPrefix(record, "export ") {
		record = strings.TrimSpace(strings.TrimPrefix(record, "export "))
	}
	separator := strings.IndexByte(record, '=')
	if separator < 1 {
		return sourceError("", 0, "", "scalar settings", "only variable assignments are allowed", nil)
	}
	name := strings.TrimSpace(record[:separator])
	if !ValidVariable(name) {
		return sourceError("", 0, "", "scalar settings", "invalid variable name", nil)
	}
	discard := policy.discards(name)
	if discard {
		// Validate the old expression without allowing its assignments or nested
		// defaults to affect current settings, provenance, or sync exports.
		values = cloneEnvironment(values)
		observer = nil
		policy.batch = nil
	}
	raw := strings.TrimSpace(record[separator+1:])
	value, expand := decodeValue(raw)
	if expand {
		var err error
		value, err = expandValue(value, values, observer, policy)
		if err != nil {
			return err
		}
	}
	if discard {
		return nil
	}
	values[name] = value
	if policy.batch != nil {
		policy.batch.setAvailable(name, !unavailable)
	}
	if observer != nil {
		observer(name, value)
	}
	return nil
}

func decodeValue(value string) (string, bool) {
	if len(value) < 2 {
		return value, true
	}
	if value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1], false
	}
	if value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1], true
	}
	return value, true
}

func expandValue(
	value string,
	values environment,
	observer func(name, value string),
	policy discardPolicy,
) (string, error) {
	if strings.Contains(value, "$(") || strings.ContainsRune(value, '`') {
		return "", sourceError("", 0, "", "scalar settings", "command substitution is not allowed in config", nil)
	}
	var result strings.Builder
	for index := 0; index < len(value); {
		if value[index] == '\\' && index+1 < len(value) {
			result.WriteByte(value[index+1])
			index += 2
			continue
		}
		if value[index] != '$' {
			result.WriteByte(value[index])
			index++
			continue
		}
		if index+1 >= len(value) {
			result.WriteByte('$')
			index++
			continue
		}
		if policy.literal && (value[index+1] == '{' || variableChar(value[index+1], true)) {
			return "", sourceError("", 0, "", "scalar settings", errSettingExpansion.Error(), errSettingExpansion)
		}
		if value[index+1] == '{' {
			end, err := parameterEnd(value, index+2)
			if err != nil {
				return "", err
			}
			expanded, err := expandParameter(value[index+2:end], values, observer, policy)
			if err != nil {
				return "", err
			}
			result.WriteString(expanded)
			index = end + 1
			continue
		}
		end := index + 1
		for end < len(value) && variableChar(value[end], end == index+1) {
			end++
		}
		if end == index+1 {
			result.WriteByte('$')
			index++
			continue
		}
		name := value[index+1 : end]
		if policy.batch != nil && !policy.batch.available(name) {
			*policy.unavailable = true
		}
		result.WriteString(values[name])
		index = end
	}
	return result.String(), nil
}

func parameterEnd(value string, start int) (int, error) {
	depth := 1
	for index := start; index < len(value); index++ {
		if value[index] == '$' && index+1 < len(value) && value[index+1] == '{' {
			depth++
			index++
			continue
		}
		if value[index] == '}' {
			depth--
			if depth == 0 {
				return index, nil
			}
		}
	}
	return 0, sourceError("", 0, "", "scalar settings", "unterminated parameter expansion", nil)
}

func expandParameter(
	expression string,
	values environment,
	observer func(name, value string),
	policy discardPolicy,
) (string, error) {
	name, operator, fallback := splitParameter(expression)
	if !ValidVariable(name) {
		return "", sourceError("", 0, "", "scalar settings", "invalid parameter name", nil)
	}
	discard := policy.discards(name)
	if discard {
		// A default of a discarded name stays inside this expansion.
		values = cloneEnvironment(values)
		observer = nil
		policy.batch = nil
	}
	current := values[name]
	if policy.batch != nil && !policy.batch.available(name) {
		*policy.unavailable = true
		return current, nil
	}
	switch operator {
	case "":
		return current, nil
	case ":-", ":=", ":?":
		if current != "" {
			return current, nil
		}
		unavailable := false
		fallbackPolicy := policy
		fallbackPolicy.unavailable = &unavailable
		expanded, err := expandValue(fallback, values, observer, fallbackPolicy)
		if err != nil {
			return "", err
		}
		*policy.unavailable = *policy.unavailable || unavailable
		if operator == ":?" {
			if unavailable {
				return expanded, nil
			}
			if expanded == "" {
				expanded = name + " is required"
			}
			return "", sourceError("", 0, recognizedSettingKey(Catalog{}, name), "scalar settings", "required parameter is unset", errors.New(expanded))
		}
		if operator == ":=" {
			values[name] = expanded
			if policy.batch != nil {
				policy.batch.setAvailable(name, !unavailable)
			}
			if observer != nil {
				observer(name, expanded)
			}
		}
		return expanded, nil
	default:
		return "", sourceError("", 0, recognizedSettingKey(Catalog{}, name), "scalar settings", "unsupported parameter operator", nil)
	}
}

func splitParameter(expression string) (string, string, string) {
	depth := 0
	for index := 0; index+1 < len(expression); index++ {
		switch expression[index] {
		case '{':
			depth++
		case '}':
			depth--
		case ':':
			if depth == 0 {
				operator := expression[index : index+2]
				if operator == ":-" || operator == ":=" || operator == ":?" {
					return expression[:index], operator, expression[index+2:]
				}
			}
		}
	}
	return expression, "", ""
}

func ValidVariable(value string) bool {
	if value == "" || !variableChar(value[0], true) {
		return false
	}
	for index := 1; index < len(value); index++ {
		if !variableChar(value[index], false) {
			return false
		}
	}
	return true
}

func variableChar(char byte, first bool) bool {
	return char == '_' || (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') ||
		(!first && char >= '0' && char <= '9')
}

// EvaluateAssignments evaluates a layer sequentially over a copy of base.
// Source is a diagnostic label; evaluation never opens it.
func EvaluateAssignments(reader io.Reader, source string, base map[string]string) (map[string]string, error) {
	values := cloneEnvironment(base)
	if err := applyEnvReaderObserved(reader, source, values, nil); err != nil {
		return nil, err
	}
	return values, nil
}
