package configmaterial

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

const (
	ModeObserve = "observe"
	ModeApply   = "apply"

	guestStateRoot = "/var/lib/subyard/config-materialization"
)

//go:embed guest_json.py
var guestJSONProgram string

// Tomli-W 1.2.0, unmodified upstream writer; see TOMLI_W_LICENSE.
//
//go:embed tomli_w.py
var guestTOMLWriter string

//go:embed TOMLI_W_LICENSE
var guestTOMLWriterLicense string

type JSONObservation struct {
	Converged   bool   `json:"converged"`
	Fingerprint string `json:"fingerprint"`
}

type Observation = JSONObservation

func ParseObservation(payload []byte) (Observation, error) {
	return ParseJSONObservation(payload)
}

// TOML syntax is validated by the guest's standard-library parser before any
// mutation. Bind the exact template bytes without requiring a host Python runtime.
func DesiredDigestFor(format string, payload []byte) (string, error) {
	switch format {
	case "json":
		return DesiredDigest(payload)
	case "toml":
		digest := sha256.Sum256(payload)
		return hex.EncodeToString(digest[:]), nil
	default:
		return "", errors.New("unsupported materialization format")
	}
}

func Request(format, mode, developer, destination string, uid int, payload []byte) (ports.InstanceExecRequest, error) {
	if uid <= 0 {
		uid = 1000
	}
	return materializationRequest(format, mode, developer, destination, uid, payload,
		guestStateRoot, 0, "/home/"+developer)
}

func DesiredDigest(payload []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	value, err := decodeJSONValue(decoder)
	if err != nil {
		return "", errors.New("invalid desired JSON")
	}
	if _, ok := value.(map[string]any); !ok {
		return "", errors.New("desired JSON root must be an object")
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return "", errors.New("invalid desired JSON")
	}
	canonical, err := canonicalJSON(value)
	if err != nil {
		return "", errors.New("invalid desired JSON")
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func decodeJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return token, nil
	}
	switch delimiter {
	case '{':
		result := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("object key is not a string")
			}
			if _, duplicate := result[key]; duplicate {
				return nil, errors.New("duplicate object key")
			}
			value, err := decodeJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			result[key] = value
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return nil, errors.New("unterminated object")
		}
		return result, nil
	case '[':
		result := []any{}
		for decoder.More() {
			value, err := decodeJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return nil, errors.New("unterminated array")
		}
		return result, nil
	default:
		return nil, errors.New("invalid delimiter")
	}
}

func canonicalJSON(value any) ([]byte, error) {
	var output bytes.Buffer
	var write func(any) error
	write = func(current any) error {
		switch typed := current.(type) {
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			output.WriteByte('{')
			for index, key := range keys {
				if index != 0 {
					output.WriteByte(',')
				}
				encoded, err := jsonString(key)
				if err != nil {
					return err
				}
				output.Write(encoded)
				output.WriteByte(':')
				if err := write(typed[key]); err != nil {
					return err
				}
			}
			output.WriteByte('}')
		case []any:
			output.WriteByte('[')
			for index, item := range typed {
				if index != 0 {
					output.WriteByte(',')
				}
				if err := write(item); err != nil {
					return err
				}
			}
			output.WriteByte(']')
		case string:
			encoded, err := jsonString(typed)
			if err != nil {
				return err
			}
			output.Write(encoded)
		case json.Number:
			number, err := canonicalNumber(string(typed))
			if err != nil {
				return err
			}
			output.WriteString(number)
		case bool:
			output.WriteString(strconv.FormatBool(typed))
		case nil:
			output.WriteString("null")
		default:
			return errors.New("unsupported JSON value")
		}
		return nil
	}
	if err := write(value); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func jsonString(value string) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(output.Bytes(), []byte("\n")), nil
}

func canonicalNumber(value string) (string, error) {
	match := regexp.MustCompile(`^(-?)([0-9]+)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$`).FindStringSubmatch(value)
	if match == nil {
		return "", errors.New("invalid JSON number")
	}
	exponent := 0
	if match[4] != "" {
		parsed, err := strconv.Atoi(match[4])
		if err != nil || parsed > 100000 || parsed < -100000 {
			return "", errors.New("JSON number exponent is out of range")
		}
		exponent = parsed
	}
	digits := strings.TrimLeft(match[2]+match[3], "0")
	if digits == "" {
		return "0", nil
	}
	scale := len(match[3]) - exponent
	for scale > 0 && strings.HasSuffix(digits, "0") {
		digits = strings.TrimSuffix(digits, "0")
		scale--
	}
	var normalized string
	if scale <= 0 {
		normalized = digits + strings.Repeat("0", -scale)
	} else if len(digits) > scale {
		normalized = digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	} else {
		normalized = "0." + strings.Repeat("0", scale-len(digits)) + digits
	}
	return match[1] + normalized, nil
}

func JSONRequest(
	mode string,
	developer string,
	destination string,
	uid int,
	payload []byte,
) (ports.InstanceExecRequest, error) {
	if uid <= 0 {
		uid = 1000
	}
	return jsonRequest(mode, developer, destination, uid, payload,
		guestStateRoot, 0, "/home/"+developer)
}

func jsonRequest(
	mode string,
	developer string,
	destination string,
	uid int,
	payload []byte,
	stateRoot string,
	stateUID int,
	allowedHome string,
) (ports.InstanceExecRequest, error) {
	return materializationRequest("json", mode, developer, destination, uid, payload, stateRoot, stateUID, allowedHome)
}

func materializationRequest(format, mode, developer, destination string, uid int, payload []byte, stateRoot string, stateUID int, allowedHome string) (ports.InstanceExecRequest, error) {
	if mode != ModeObserve && mode != ModeApply {
		return ports.InstanceExecRequest{}, errors.New("invalid JSON materialization mode")
	}
	if !domain.SafeName(developer) || uid <= 0 {
		return ports.InstanceExecRequest{}, errors.New("invalid JSON materialization identity")
	}
	cleanHome := filepath.Clean(allowedHome)
	cleanDestination := filepath.Clean(destination)
	if !filepath.IsAbs(cleanHome) || !filepath.IsAbs(cleanDestination) ||
		cleanDestination == cleanHome ||
		!strings.HasPrefix(cleanDestination, cleanHome+string(filepath.Separator)) ||
		cleanDestination != destination {
		return ports.InstanceExecRequest{}, errors.New("invalid JSON materialization destination")
	}
	digest, err := DesiredDigestFor(format, payload)
	if err != nil {
		return ports.InstanceExecRequest{}, err
	}
	program := strings.ReplaceAll(guestJSONProgram, "@STATE_ROOT@", stateRoot)
	program = strings.ReplaceAll(program, "@STATE_UID@", fmt.Sprint(stateUID))
	program = strings.ReplaceAll(program, "@FORMAT@", format)
	if format == "toml" {
		// Execute the embedded, self-contained writer in its own namespace so its
		// helpers cannot collide with ownership or validation functions.
		licensedWriter := "# " + strings.ReplaceAll(guestTOMLWriterLicense, "\n", "\n# ") + "\n" + guestTOMLWriter
		writer, _ := json.Marshal(licensedWriter)
		program = "_toml_writer = {}\nexec(" + string(writer) + ", _toml_writer)\n" + program
	}
	return ports.InstanceExecRequest{
		Command: []string{
			"python3", "-B", "-c", program,
			mode, developer, cleanDestination, fmt.Sprint(uid), digest, cleanHome,
		},
		Stdin: payload,
	}, nil
}

func ParseJSONObservation(payload []byte) (JSONObservation, error) {
	type encodedObservation struct {
		Converged   *bool  `json:"converged"`
		Fingerprint string `json:"fingerprint"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var encoded encodedObservation
	if err := decoder.Decode(&encoded); err != nil {
		return JSONObservation{}, errors.New("invalid JSON materialization observation")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF ||
		encoded.Converged == nil || len(encoded.Fingerprint) != sha256.Size*2 {
		return JSONObservation{}, errors.New("invalid JSON materialization observation")
	}
	if _, err := hex.DecodeString(encoded.Fingerprint); err != nil {
		return JSONObservation{}, errors.New("invalid JSON materialization observation")
	}
	return JSONObservation{Converged: *encoded.Converged, Fingerprint: encoded.Fingerprint}, nil
}
