package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

type environment map[string]string

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

func AssignedSettingNames(path string) ([]string, error) {
	values := environment{}
	seen := map[string]struct{}{}
	var result []string
	if err := applyEnvFileObserved(path, values, func(name, _ string, _ int) {
		if _, exists := seen[name]; exists {
			return
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}); err != nil {
		return nil, err
	}
	sort.Strings(result)
	return result, nil
}

func applyEnvFile(path string, values environment) error {
	return applyEnvFileObserved(path, values, nil)
}

func applyEnvFileObserved(path string, values environment, observer assignmentObserver) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	discardRetiredSettings(values)

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var record strings.Builder
	quote := byte(0)
	lineNumber := 0
	recordLine := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if record.Len() == 0 {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			recordLine = lineNumber
		} else {
			record.WriteByte('\n')
		}
		record.WriteString(line)
		quote = scanQuote(line, quote)
		if quote != 0 {
			continue
		}
		recordObserver := func(name, value string) {
			if observer != nil {
				observer(name, value, recordLine)
			}
		}
		if err := applyRecord(record.String(), values, recordObserver); err != nil {
			return fmt.Errorf("%s:%d: %w", path, recordLine, err)
		}
		record.Reset()
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	if quote != 0 || record.Len() != 0 {
		return fmt.Errorf("%s:%d: unterminated quoted assignment", path, recordLine)
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

func applyRecord(record string, values environment, observer func(name, value string)) error {
	record = strings.TrimSpace(record)
	if strings.HasPrefix(record, ":") {
		expression := strings.TrimSpace(strings.TrimPrefix(record, ":"))
		value, expand := decodeValue(expression)
		if !expand {
			return nil
		}
		_, err := expandValue(value, values, observer)
		return err
	}
	if strings.HasPrefix(record, "export ") {
		record = strings.TrimSpace(strings.TrimPrefix(record, "export "))
	}
	separator := strings.IndexByte(record, '=')
	if separator < 1 {
		return errors.New("only variable assignments are allowed")
	}
	name := strings.TrimSpace(record[:separator])
	if !ValidVariable(name) {
		return fmt.Errorf("invalid variable name %q", name)
	}
	if isRetiredSetting(name) {
		// Validate the old expression without allowing its assignments or nested
		// defaults to affect current settings, provenance, or sync exports.
		values = cloneEnvironment(values)
		observer = nil
	}
	raw := strings.TrimSpace(record[separator+1:])
	value, expand := decodeValue(raw)
	if !expand {
		values[name] = value
		if observer != nil {
			observer(name, value)
		}
		return nil
	}
	value, err := expandValue(value, values, observer)
	if err != nil {
		return err
	}
	values[name] = value
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
) (string, error) {
	if strings.Contains(value, "$(") || strings.ContainsRune(value, '`') {
		return "", errors.New("command substitution is not allowed in config")
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
			expanded, err := expandParameter(value[index+2:end], values, observer)
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
		result.WriteString(values[value[index+1:end]])
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
	return 0, errors.New("unterminated parameter expansion")
}

func expandParameter(
	expression string,
	values environment,
	observer func(name, value string),
) (string, error) {
	name, operator, fallback := splitParameter(expression)
	if !ValidVariable(name) {
		return "", fmt.Errorf("invalid parameter name %q", name)
	}
	if isRetiredSetting(name) {
		values = cloneEnvironment(values)
		observer = nil
	}
	current := values[name]
	switch operator {
	case "":
		return current, nil
	case ":-", ":=", ":?":
		if current != "" {
			return current, nil
		}
		expanded, err := expandValue(fallback, values, observer)
		if err != nil {
			return "", err
		}
		if operator == ":?" {
			if expanded == "" {
				expanded = name + " is required"
			}
			return "", errors.New(expanded)
		}
		if operator == ":=" {
			values[name] = expanded
			if observer != nil {
				observer(name, expanded)
			}
		}
		return expanded, nil
	default:
		return "", fmt.Errorf("unsupported parameter operator %q", operator)
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
