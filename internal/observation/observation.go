// Package observation validates bounded JSON without lossy Unicode decoding.
package observation

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

const MaxInput = 4 << 20
const maxDepth = 128

// ValidateJSON rejects duplicate decoded object keys, invalid Unicode, trailing
// documents and inputs outside the receipt reader's depth, size and number limits.
func ValidateJSON(data []byte) error {
	if len(data) == 0 || len(data) > MaxInput || !utf8.Valid(data) || !validEscapes(data) {
		return errors.New("invalid or unbounded JSON input")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := validateValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing JSON input")
	}
	return nil
}

func validateValue(decoder *json.Decoder, depth int) error {
	if depth > maxDepth {
		return errors.New("JSON depth exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return errors.New("invalid JSON")
	}
	if delimiter, ok := token.(json.Delim); ok {
		switch delimiter {
		case '{':
			keys := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return errors.New("invalid JSON object")
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("invalid JSON key")
				}
				if _, duplicate := keys[key]; duplicate {
					return errors.New("duplicate JSON key")
				}
				keys[key] = struct{}{}
				if err := validateValue(decoder, depth+1); err != nil {
					return err
				}
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
				return errors.New("invalid JSON object")
			}
		case '[':
			for decoder.More() {
				if err := validateValue(decoder, depth+1); err != nil {
					return err
				}
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
				return errors.New("invalid JSON array")
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
	} else if number, ok := token.(json.Number); ok {
		return validateNumber(string(number))
	}
	return nil
}

// Keep both exponent limits of the historical reader without constructing a
// canonical number. Zero bypasses the normalized bound, as it has no exponent.
func validateNumber(text string) error {
	if len(text) > 1024 {
		return errors.New("JSON number exceeds limit")
	}
	text = strings.TrimPrefix(text, "-")
	mantissa, exponentText, exponential := strings.Cut(strings.ToLower(text), "e")
	var exponent int64
	if exponential {
		var err error
		exponent, err = strconv.ParseInt(exponentText, 10, 64)
		if err != nil || exponent < -1000000 || exponent > 1000000 {
			return errors.New("JSON exponent exceeds limit")
		}
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	exponent -= int64(len(fraction))
	digits := strings.TrimLeft(whole+fraction, "0")
	if digits == "" {
		return nil
	}
	exponent += int64(len(digits) - len(strings.TrimRight(digits, "0")))
	if exponent < -1000000 || exponent > 1000000 {
		return errors.New("normalized JSON exponent exceeds limit")
	}
	return nil
}

// encoding/json replaces lone UTF-16 surrogates. Reject them before decoding so
// distinct invalid native bytes cannot collapse into a valid protected string.
func validEscapes(data []byte) bool {
	quoted := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			quoted = !quoted
			continue
		}
		if !quoted || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return false
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return false
		}
		code, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if code >= 0xdc00 && code <= 0xdfff {
			return false
		}
		if code >= 0xd800 && code <= 0xdbff {
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return !quoted
}
