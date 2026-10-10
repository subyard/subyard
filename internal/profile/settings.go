package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

func (field Field) Parse(value string) (any, error) {
	switch field.Kind {
	case "numeric-string":
		if value != "" && len(value) <= 64 && strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
			return value, nil
		}
	case "positive-integer":
		number, err := strconv.ParseInt(value, 10, 64)
		if err == nil && number > 0 {
			return number, nil
		}
	}
	return nil, errors.New("enter a positive numeric identifier")
}

func (setup Setup) Decode(data []byte) (map[string]any, error) {
	values := map[string]any{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&values) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid profile settings JSON")
	}
	if setup.SyncFields && values["use_credential_settings"] == true && len(values) == 1 {
		return values, nil
	}
	allowed := map[string]bool{}
	for _, field := range setup.Fields {
		allowed[field.Name] = true
		value, exists := values[field.Name]
		if !exists {
			return nil, errors.New("required profile field is missing")
		}
		text := ""
		switch field.Kind {
		case "numeric-string":
			text, _ = value.(string)
		case "positive-integer":
			if number, ok := value.(json.Number); ok {
				text = number.String()
			}
		}
		parsed, err := field.Parse(text)
		if err != nil {
			return nil, err
		}
		values[field.Name] = parsed
	}
	if setup.KeyOverrideField != "" {
		allowed[setup.KeyOverrideField] = true
		if value, exists := values[setup.KeyOverrideField]; exists {
			path, ok := value.(string)
			if !ok || path != "" && (!filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n")) {
				return nil, errors.New("credential override must be an absolute path")
			}
		}
	}
	for key := range values {
		if !allowed[key] {
			return nil, errors.New("unknown profile settings field")
		}
	}
	return values, nil
}
