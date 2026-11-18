package config

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
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

// AssignedSettingNames lists the settings an operator file assigns. A
// profile-only assignment is discarded as the loader discards it.
func AssignedSettingNames(path string) ([]string, error) {
	values := environment{}
	seen := map[string]struct{}{}
	var result []string
	if err := applyEnvFileObservedRetired(path, values, func(name, _ string, _ int) {
		if _, exists := seen[name]; exists {
			return
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}, discardPolicy{ignored: IsProfileOnlySetting}, nil); err != nil {
		return nil, err
	}
	sort.Strings(result)
	return result, nil
}

func applyEnvFile(path string, values environment) error {
	return applyEnvFileObserved(path, values, nil)
}

func applyEnvFileObserved(path string, values environment, observer assignmentObserver, readers ...func(string) ([]byte, error)) error {
	return applyEnvFileObservedRetired(path, values, observer, discardPolicy{}, nil, readers...)
}

func applyEnvFileObservedRetired(path string, values environment, observer assignmentObserver, policy discardPolicy, discarded func(name, value string, line int), readers ...func(string) ([]byte, error)) error {
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
	return applyEnvReaderObservedRetired(input, path, values, observer, policy, discarded)
}

// discardPolicy names discarded inputs: retired settings always, plus those
// retired or ignored selects. A discarded assignment is evaluated in
// isolation: neither it nor any default it performs changes values or reaches
// the observer, so later records never see it. An ignored one also never
// refuses: its evaluation error is dropped and its unevaluated text kept, and
// a required check of its name is ignored.
// report, when set, receives each discarded record's assignment.
type discardPolicy struct {
	retired, ignored func(string) bool
	report           func(name, value string)
}

func (policy discardPolicy) discards(name string) bool {
	return isRetiredSetting(name) || policy.retired != nil && policy.retired(name) || policy.ignores(name)
}

func (policy discardPolicy) ignores(name string) bool {
	return policy.ignored != nil && policy.ignored(name)
}

// operatorDiscardPolicy is the policy for operator-authored files: they may
// still contain profile-only settings, which only environment profiles declare.
func operatorDiscardPolicy(report func(name, value string), catalogs ...Catalog) discardPolicy {
	return discardPolicy{ignored: IsProfileOnlySetting, report: report, retired: func(name string) bool {
		for _, settings := range catalogs {
			if settings.retiredSetting(name) {
				return true
			}
		}
		return false
	}}
}

func applyEnvReaderObserved(reader io.Reader, source string, values environment, observer assignmentObserver) error {
	return applyEnvReaderObservedRetired(reader, source, values, observer, discardPolicy{}, nil)
}

// applyEnvReaderObservedRetired evaluates every record under policy.
// discarded, when set, receives each discarded assignment with its record line.
func applyEnvReaderObservedRetired(reader io.Reader, source string, values environment, observer assignmentObserver, policy discardPolicy, discarded func(name, value string, line int)) error {
	discardRetiredSettings(values)

	return scanEnvRecords(reader, source, "scalar settings", func(record envRecord) error {
		if record.text == "" {
			return nil
		}
		evaluation := envEvaluation{values: values, policy: policy}
		if observer != nil {
			evaluation.observe = func(name, value string) { observer(name, value, record.line) }
		}
		if discarded != nil {
			evaluation.policy.report = func(name, value string) { discarded(name, value, record.line) }
		}
		return evaluation.applyRecord(record.text)
	})
}

type envRecord struct {
	raw  string
	text string
	line int
}

// scanEnvRecords preserves raw bytes for edits and supplies the same
// evaluation text and line numbers to each consumer. Blank and comment lines
// outside an assignment have empty evaluation text.
func scanEnvRecords(reader io.Reader, source, purpose string, visit func(envRecord) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if end := bytes.IndexByte(data, '\n'); end >= 0 {
			return end + 1, data[:end+1], nil
		}
		if atEOF && len(data) != 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
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

type envEvaluation struct {
	values  environment
	observe func(name, value string)
	policy  discardPolicy

	// possible visits defaults that may run over a different input layer.
	possible bool
}

func (evaluation envEvaluation) applyRecord(record string) error {
	if strings.HasPrefix(record, ":") {
		expression := strings.TrimSpace(strings.TrimPrefix(record, ":"))
		value, expand := decodeValue(expression)
		if !expand {
			return nil
		}
		_, err := evaluation.expandValue(value)
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
	discard := evaluation.policy.discards(name)
	if discard {
		// Validate the old expression without allowing its assignments or nested
		// defaults to affect current settings, provenance, or sync exports.
		evaluation.values = cloneEnvironment(evaluation.values)
		evaluation.observe = nil
	}
	raw := strings.TrimSpace(record[separator+1:])
	value, expand := decodeValue(raw)
	if expand && !(discard && evaluation.possible) {
		expanded, err := evaluation.expandValue(value)
		if err != nil && !(discard && evaluation.policy.ignores(name)) {
			return err
		}
		if err == nil {
			value = expanded
		}
	}
	if discard {
		if evaluation.policy.report != nil {
			evaluation.policy.report(name, value)
		}
		return nil
	}
	evaluation.assign(name, value)
	return nil
}

func (evaluation envEvaluation) assign(name, value string) {
	evaluation.values[name] = value
	if evaluation.observe != nil {
		evaluation.observe(name, value)
	}
}

func decodeValue(value string) (string, bool) {
	if len(value) < 2 {
		return value, true
	}
	if value[0] == '\'' && value[len(value)-1] == '\'' {
		return strings.ReplaceAll(value[1:len(value)-1], escapedEnvApostrophe, "'"), false
	}
	if value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1], true
	}
	return value, true
}

func (evaluation envEvaluation) expandValue(value string) (string, error) {
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
		if value[index+1] == '{' {
			end, err := parameterEnd(value, index+2)
			if err != nil {
				return "", err
			}
			expanded, err := evaluation.expandParameter(value[index+2 : end])
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
		result.WriteString(evaluation.values[value[index+1:end]])
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

func (evaluation envEvaluation) expandParameter(expression string) (string, error) {
	name, operator, fallback := splitParameter(expression)
	if !ValidVariable(name) {
		return "", sourceError("", 0, "", "scalar settings", "invalid parameter name", nil)
	}
	discard := evaluation.policy.discards(name)
	if discard {
		// A default of a discarded name stays inside this expansion.
		if evaluation.possible {
			return "", nil
		}
		evaluation.values = cloneEnvironment(evaluation.values)
		evaluation.observe = nil
	}
	current := evaluation.values[name]
	switch operator {
	case "":
		return current, nil
	case ":-", ":=", ":?":
		if current != "" {
			return current, nil
		}
		if operator == ":?" && (evaluation.possible || evaluation.policy.ignores(name)) {
			// A required check evaluates its message only on failure.
			// For an ignored name the check itself is skipped.
			return "", nil
		}
		expanded, err := evaluation.expandValue(fallback)
		if err != nil {
			// A different input layer can skip an invalid fallback.
			if evaluation.possible {
				return "", nil
			}
			return "", err
		}
		if operator == ":?" {
			if expanded == "" {
				expanded = name + " is required"
			}
			return "", sourceError("", 0, recognizedSettingKey(Catalog{}, name), "scalar settings", "required parameter is unset", errors.New(expanded))
		}
		if operator == ":=" {
			evaluation.assign(name, expanded)
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
