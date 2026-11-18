// Package collector implements the optional local telemetry data plane.
package collector

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Subyard/Subyard/config/profiles/telemetry"
)

const RecordSchema = "subyard.telemetry.record/v1"
const ExportSchema = "subyard.telemetry.export/v1"
const timestampLayout = "2006-01-02T15:04:05.000000000Z"
const unknownSourceTime = "0001-01-01T00:00:00.000000000Z"

type Record struct {
	Schema        string         `json:"schema,omitempty"`
	SchemaVersion int            `json:"schema_version,omitempty"`
	SourceID      string         `json:"source_id"`
	EventID       string         `json:"event_id"`
	SourceVersion string         `json:"source_version"`
	SourceTime    string         `json:"source_time"`
	Kind          string         `json:"kind"`
	Body          map[string]any `json:"body"`

	authenticated bool
	redactions    int64
	capturePolicy string
	inputID       string
	filtered      bool
}

func (r *Record) UnmarshalJSON(data []byte) error {
	var input map[string]json.RawMessage
	if err := json.Unmarshal(data, &input); err != nil {
		return err
	}
	body, err := decodeBody(input["body"])
	if err != nil {
		return err
	}
	*r = Record{
		Schema: jsonString(input["schema"]), SourceID: jsonString(input["source_id"]), EventID: jsonString(input["event_id"]),
		SourceVersion: jsonString(input["source_version"]), SourceTime: jsonString(input["source_time"]),
		Kind: jsonString(input["kind"]), Body: body,
	}
	_ = json.Unmarshal(input["schema_version"], &r.SchemaVersion)
	return nil
}

func jsonString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

type Envelope struct {
	Schema        string `json:"schema"`
	OriginID      string `json:"origin_id"`
	EventID       string `json:"event_id"`
	Sequence      int64  `json:"sequence"`
	ReceivedTime  string `json:"received_time"`
	SourceID      string `json:"source_id"`
	SourceVersion string `json:"source_version"`
	SourceTime    string `json:"source_time,omitempty"`
	Kind          string `json:"kind"`
	Encoding      string `json:"encoding"`
	PolicyVersion string `json:"policy_version"`
	// PolicyVersion is the source capture claim; imports independently record
	// the trusted local policy applied before the imported bytes are stored.
	ImportPolicyVersion string          `json:"import_policy_version,omitempty"`
	StoreProof          string          `json:"store_proof,omitempty"`
	Redactions          int64           `json:"redactions"`
	Body                json.RawMessage `json:"body,omitempty"`
	Protobuf            []byte          `json:"protobuf,omitempty"`

	// Only internal capture and an authenticated producer can set local.
	local   bool
	replay  bool
	native  bool
	inputID string
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

var legacyID = recordPatterns["legacy_id"]
var versionPattern = recordPatterns["version"]

func canonicalTimestamp(value string) string {
	if !recordPatterns["timestamp"].MatchString(value) || strings.HasPrefix(value, "0000-") {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return ""
	}
	utc := parsed.UTC()
	if utc.Year() < 1 || utc.Year() > 9999 {
		return ""
	}
	return utc.Format(timestampLayout)
}

func sanitizeSourceVersion(version string) string {
	if version == nativeSourceVersion || versionPattern.MatchString(version) {
		return version
	}
	return ""
}

var recordPatterns, bodyRules = loadRecordPolicy()

type bodyRule struct {
	kind    string
	choices []string
}

func loadRecordPolicy() (map[string]*regexp.Regexp, map[string]bodyRule) {
	patterns := map[string]*regexp.Regexp{}
	rules := map[string]bodyRule{}
	for line := range strings.Lines(strings.TrimSpace(telemetry.RecordPolicy)) {
		parts := strings.Fields(line)
		if parts[0] == "pattern" {
			patterns[parts[1]] = regexp.MustCompile("^(?:" + parts[2] + ")$")
			continue
		}
		rule := bodyRule{kind: parts[0]}
		if rule.kind == "enum" || rule.kind == "names" {
			rule.choices = parts[2:]
			rules[parts[1]] = rule
			continue
		}
		for _, field := range parts[1:] {
			rules[field] = rule
		}
	}
	return patterns, rules
}

func (id identity) field(rule bodyRule, value any) (any, bool) {
	if rule.kind == "" {
		return nil, false
	}
	if value == nil {
		return nil, true
	}
	switch rule.kind {
	case "names":
		list, ok := value.([]any)
		if !ok || len(list) > 128 {
			return nil, false
		}
		fields := []any{}
		for _, item := range list {
			if name, ok := item.(string); ok && slices.Contains(rule.choices, name) {
				fields = append(fields, name)
			}
		}
		return fields, true
	case "missing":
		switch v := value.(type) {
		case []any:
			if len(v) > 128 {
				return nil, false
			}
			fields := []any{}
			for _, field := range v {
				if name, ok := field.(string); ok {
					if _, known := bodyRules[name]; known {
						fields = append(fields, name)
					}
				}
			}
			return fields, true
		case string:
			if v == "broker_attribution" || v == "final_tail" {
				return v, true
			}
			return id.field(bodyRule{kind: "uint"}, v)
		default:
			return id.field(bodyRule{kind: "uint"}, v)
		}
	case "bool":
		_, ok := value.(bool)
		return value, ok
	case "uint", "int":
		var number string
		switch v := value.(type) {
		case string:
			number = v // Historical producer counters were sometimes decimal strings.
		case json.Number:
			number = string(v)
		case int:
			number = strconv.Itoa(v)
		case int64:
			number = strconv.FormatInt(v, 10)
		case uint64:
			number = strconv.FormatUint(v, 10)
		default:
			return nil, false
		}
		if !recordPatterns["decimal"].MatchString(number) {
			return nil, false
		}
		if rule.kind == "uint" {
			n, err := strconv.ParseUint(strings.TrimPrefix(number, "+"), 10, 64)
			return n, err == nil
		}
		n, err := strconv.ParseInt(number, 10, 64)
		return n, err == nil
	}
	s, ok := value.(string)
	if !ok {
		return nil, false
	}
	switch rule.kind {
	case "enum":
		return s, slices.Contains(rule.choices, s)
	case "time":
		s = canonicalTimestamp(s)
		return s, s != ""
	case "id":
		s = id(s)
		return s, s != ""
	case "version":
		return s, versionPattern.MatchString(s)
	default:
		return nil, false
	}
}

var kinds = map[string]bool{"run": true, "session": true, "relation": true, "scope": true, "process": true, "sample": true, "allocation": true, "association": true, "gap": true}

func (id identity) body(input map[string]any) (map[string]any, int64) {
	out := make(map[string]any, len(bodyRules))
	var removed int64
	for name, value := range input {
		clean, ok := id.field(bodyRules[name], value)
		if ok {
			out[name] = clean
		}
		if list, isList := value.([]any); isList {
			accepted, _ := clean.([]any)
			removed += int64(len(list) - len(accepted))
			if !ok && len(list) == 0 {
				removed++
			}
		} else if !ok {
			removed++
		}
	}
	return out, removed
}

func (id identity) record(r Record) (Record, int64, error) {
	r.SourceID, r.EventID = id(r.SourceID), id(r.EventID)
	return id.cleanRecord(r)
}

func (id identity) cleanRecord(r Record) (Record, int64, error) {
	if r.Schema != RecordSchema && !(r.Schema == "" && r.SchemaVersion == 1) {
		return Record{}, 0, errors.New("unsupported record schema")
	}
	clean := Record{
		Schema: RecordSchema, SourceID: r.SourceID, EventID: r.EventID,
		SourceVersion: sanitizeSourceVersion(r.SourceVersion), SourceTime: canonicalTimestamp(r.SourceTime),
		Kind: r.Kind,
	}
	if clean.SourceID == "" || clean.EventID == "" || !kinds[clean.Kind] || r.Body == nil {
		return Record{}, 0, errors.New("invalid record identity or kind")
	}
	body, removed := id.body(r.Body)
	if clean.SourceTime == "" {
		clean.SourceTime = unknownSourceTime
		removed++
	}
	clean.Body = body
	if clean.SourceVersion != r.SourceVersion {
		removed++
	}
	if clean.Kind == "run" && stringValue(body, "agent_run_id") == "" {
		return Record{}, 0, errors.New("run identity missing")
	}
	switch clean.Kind {
	case "scope", "process", "sample", "allocation", "association":
		if stringValue(body, "object_id") == "" {
			return Record{}, 0, errors.New("object identity missing")
		}
	}
	return clean, removed, nil
}

// decodeRecord authenticates the exact inner JSON sent by a producer. Passing
// through already digested identifiers requires this proof, not an ID prefix.
func (key identityKey) decodeRecord(data []byte) (Record, error) {
	var signed struct {
		Record    json.RawMessage `json:"record"`
		Signature string          `json:"signature"`
	}
	if err := json.Unmarshal(data, &signed); err != nil {
		return Record{}, err
	}
	if signed.Record == nil {
		var r Record
		err := json.Unmarshal(data, &r)
		return r, err
	}
	signature, err := hex.DecodeString(signed.Signature)
	if err != nil || !hmac.Equal(signature, key.digest("producer-record/v1\x00"+string(signed.Record))) {
		return Record{}, errInvalidRecord
	}
	var r Record
	if err = json.Unmarshal(signed.Record, &r); err != nil {
		return Record{}, err
	}
	var count struct {
		Redactions    int64  `json:"redactions"`
		PolicyVersion string `json:"policy_version"`
		InputID       string `json:"input_id"`
		Filtered      bool   `json:"filtered"`
	}
	if err = json.Unmarshal(signed.Record, &count); err != nil {
		return Record{}, err
	}
	r.authenticated = true
	r.inputID, r.filtered = count.InputID, count.Filtered
	r.redactions = max(0, min(count.Redactions, 1<<31))
	// Older signed outbox entries did not identify the producer's allowlist.
	r.capturePolicy = PolicyVersion
	if capturePolicyVersion.MatchString(count.PolicyVersion) {
		r.capturePolicy = count.PolicyVersion
	}
	return r, nil
}

func decodeBody(data []byte) (map[string]any, error) {
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	err := dec.Decode(&body)
	return body, err
}
