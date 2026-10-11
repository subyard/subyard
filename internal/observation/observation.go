// Package observation encodes validated semantic snapshots. Collection semantics
// are chosen by the owning producer, never inferred from JSON paths.
package observation

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const MaxInput = 4 << 20
const maxDepth = 128

// Value has a closed, immutable representation. Constructors copy collections;
// callers cannot obtain or mutate their backing storage.
type Value struct{ node *node }
type node struct {
	kind   byte
	text   string
	fields map[string]Value
	items  []Value
}

func Null() Value               { return Value{&node{kind: 'n'}} }
func Bool(value bool) Value     { return Value{&node{kind: 'b', text: strconv.FormatBool(value)}} }
func String(value string) Value { return Value{&node{kind: 's', text: value}} }
func Object(fields map[string]Value) Value {
	copy := make(map[string]Value, len(fields))
	for key, value := range fields {
		copy[key] = value
	}
	return Value{&node{kind: 'o', fields: copy}}
}
func Ordered(items []Value) Value  { return Value{&node{kind: 'a', items: slices.Clone(items)}} }
func Multiset(items []Value) Value { return Value{&node{kind: 'm', items: slices.Clone(items)}} }
func Number(text string) (Value, error) {
	// JSON's decoder validates the grammar, including leading zeros and exponents.
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return Value{}, errors.New("invalid JSON number")
	}
	number, ok := value.(json.Number)
	if !ok || decoder.Decode(new(any)) != io.EOF {
		return Value{}, errors.New("invalid JSON number")
	}
	canonical, err := canonicalNumber(string(number))
	if err != nil {
		return Value{}, err
	}
	return Value{&node{kind: 'd', text: canonical}}, nil
}

// Snapshot can only be constructed from explicitly typed values. Its zero value
// is invalid and cannot be fingerprinted.
type Snapshot struct{ canonical string }

func NewSnapshot(root Value) (Snapshot, error) {
	encoded, err := encode(root, 0)
	if err != nil {
		return Snapshot{}, err
	}
	if len(encoded) > MaxInput {
		return Snapshot{}, errors.New("semantic snapshot exceeds limit")
	}
	return Snapshot{string(encoded)}, nil
}
func (snapshot Snapshot) Bytes() []byte { return []byte(snapshot.canonical) }

type SemanticFingerprint struct{ hex string }

func (fingerprint SemanticFingerprint) String() string { return fingerprint.hex }
func Fingerprint(snapshot Snapshot) (SemanticFingerprint, error) {
	if snapshot.canonical == "" {
		return SemanticFingerprint{}, errors.New("unvalidated semantic snapshot")
	}
	return SemanticFingerprint{fmt.Sprintf("%x", sha256.Sum256([]byte(snapshot.canonical)))}, nil
}

// SourceDigest is a byte-exact proof, deliberately distinct from a semantic
// fingerprint. It cannot enter Fingerprint's snapshot boundary.
type SourceDigest struct{ hex string }

func DigestSource(data []byte) SourceDigest {
	return SourceDigest{fmt.Sprintf("%x", sha256.Sum256(data))}
}
func (digest SourceDigest) String() string { return digest.hex }

func encode(value Value, depth int) ([]byte, error) {
	if value.node == nil || depth > maxDepth {
		return nil, errors.New("invalid semantic value or depth")
	}
	n := value.node
	switch n.kind {
	case 'n':
		return []byte("null"), nil
	case 'b', 'd':
		return []byte(n.text), nil
	case 's':
		if !utf8.ValidString(n.text) {
			return nil, errors.New("invalid Unicode string")
		}
		var out bytes.Buffer
		encoder := json.NewEncoder(&out)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(n.text); err != nil {
			return nil, err
		}
		return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
	case 'o':
		keys := make([]string, 0, len(n.fields))
		for key := range n.fields {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		out := []byte{'{'}
		for i, key := range keys {
			if i > 0 {
				out = append(out, ',')
			}
			name, err := encode(String(key), depth)
			if err != nil {
				return nil, err
			}
			item, err := encode(n.fields[key], depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, name...)
			out = append(out, ':')
			out = append(out, item...)
			if len(out) > MaxInput {
				return nil, errors.New("semantic snapshot exceeds limit")
			}
		}
		return append(out, '}'), nil
	case 'a', 'm':
		items := make([][]byte, len(n.items))
		total := 2
		for i, item := range n.items {
			encoded, err := encode(item, depth+1)
			if err != nil {
				return nil, err
			}
			items[i] = encoded
			total += len(encoded)
			if i > 0 {
				total++
			}
			if total > MaxInput {
				return nil, errors.New("semantic snapshot exceeds limit")
			}
		}
		if n.kind == 'm' {
			slices.SortFunc(items, bytes.Compare)
		}
		out := []byte{'['}
		for i, item := range items {
			if i > 0 {
				out = append(out, ',')
			}
			out = append(out, item...)
			if len(out) > MaxInput {
				return nil, errors.New("semantic snapshot exceeds limit")
			}
		}
		return append(out, ']'), nil
	default:
		return nil, errors.New("invalid semantic kind")
	}
}

func canonicalNumber(text string) (string, error) {
	if len(text) > 1024 {
		return "", errors.New("JSON number exceeds limit")
	}
	negative := strings.HasPrefix(text, "-")
	text = strings.TrimPrefix(text, "-")
	mantissa, exponentText, exponential := strings.Cut(strings.ToLower(text), "e")
	var exponent int64
	if exponential {
		var err error
		exponent, err = strconv.ParseInt(exponentText, 10, 64)
		if err != nil || exponent < -1000000 || exponent > 1000000 {
			return "", errors.New("JSON exponent exceeds limit")
		}
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	exponent -= int64(len(fraction))
	digits := strings.TrimLeft(whole+fraction, "0")
	if digits == "" {
		return "0", nil
	}
	trimmed := strings.TrimRight(digits, "0")
	exponent += int64(len(digits) - len(trimmed))
	digits = trimmed
	if negative {
		digits = "-" + digits
	}
	if exponent < -1000000 || exponent > 1000000 {
		return "", errors.New("normalized JSON exponent exceeds limit")
	}
	if exponent != 0 {
		digits += "e" + strconv.FormatInt(exponent, 10)
	}
	return digits, nil
}

// RawValue is validated JSON, but is not a semantic Value or Snapshot. A
// producer must explicitly construct every array as Ordered or Multiset.
type RawValue struct{ value any }

func (raw RawValue) Fields() (map[string]RawValue, bool) {
	fields, ok := raw.value.(map[string]RawValue)
	copy := make(map[string]RawValue, len(fields))
	for key, value := range fields {
		copy[key] = value
	}
	return copy, ok
}
func (raw RawValue) Elements() ([]RawValue, bool) {
	items, ok := raw.value.([]RawValue)
	return slices.Clone(items), ok
}
func (raw RawValue) Text() (string, bool)  { value, ok := raw.value.(string); return value, ok }
func (raw RawValue) Boolean() (bool, bool) { value, ok := raw.value.(bool); return value, ok }
func (raw RawValue) IsNull() bool          { return raw.value == nil }
func (raw RawValue) Scalar() (Value, bool) {
	switch value := raw.value.(type) {
	case nil:
		return Null(), true
	case bool:
		return Bool(value), true
	case string:
		return String(value), true
	case json.Number:
		canonical, err := Number(string(value))
		return canonical, err == nil
	default:
		return Value{}, false
	}
}

func Decode(data []byte) (RawValue, error) {
	if len(data) == 0 || len(data) > MaxInput || !utf8.Valid(data) || !validEscapes(data) {
		return RawValue{}, errors.New("invalid or unbounded JSON input")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeValue(decoder, 0)
	if err != nil {
		return RawValue{}, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return RawValue{}, errors.New("trailing JSON input")
	}
	return value, nil
}
func decodeValue(decoder *json.Decoder, depth int) (RawValue, error) {
	if depth > maxDepth {
		return RawValue{}, errors.New("JSON depth exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return RawValue{}, errors.New("invalid JSON")
	}
	if delimiter, ok := token.(json.Delim); ok {
		switch delimiter {
		case '{':
			fields := map[string]RawValue{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return RawValue{}, errors.New("invalid JSON object")
				}
				key, ok := keyToken.(string)
				if !ok {
					return RawValue{}, errors.New("invalid JSON key")
				}
				if _, duplicate := fields[key]; duplicate {
					return RawValue{}, errors.New("duplicate JSON key")
				}
				value, err := decodeValue(decoder, depth+1)
				if err != nil {
					return RawValue{}, err
				}
				fields[key] = value
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
				return RawValue{}, errors.New("invalid JSON object")
			}
			return RawValue{fields}, nil
		case '[':
			items := []RawValue{}
			for decoder.More() {
				value, err := decodeValue(decoder, depth+1)
				if err != nil {
					return RawValue{}, err
				}
				items = append(items, value)
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
				return RawValue{}, errors.New("invalid JSON array")
			}
			return RawValue{items}, nil
		default:
			return RawValue{}, errors.New("invalid JSON delimiter")
		}
	}
	if number, ok := token.(json.Number); ok {
		if _, err := canonicalNumber(string(number)); err != nil {
			return RawValue{}, err
		}
	}
	return RawValue{token}, nil
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
