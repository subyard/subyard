// Package collector implements the optional local telemetry data plane.
package collector

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
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
	Schema        string `json:"schema,omitempty"`
	SchemaVersion int    `json:"schema_version,omitempty"`
	OriginID      string `json:"origin_id,omitempty"`
	SourceID      string `json:"source_id"`
	EventID       string `json:"event_id"`
	SourceVersion string `json:"source_version"`
	SourceTime    string `json:"source_time"`
	Kind          string `json:"kind"`
	filter        valueFilter
	Filtered      bool           `json:"filtered,omitempty"`
	Redactions    int64          `json:"redactions,omitempty"`
	Body          map[string]any `json:"body"`
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
		OriginID: jsonString(input["origin_id"]),
		Schema:   jsonString(input["schema"]), SourceID: jsonString(input["source_id"]), EventID: jsonString(input["event_id"]),
		SourceVersion: jsonString(input["source_version"]), SourceTime: jsonString(input["source_time"]),
		Kind: jsonString(input["kind"]), Body: body,
	}
	_ = json.Unmarshal(input["schema_version"], &r.SchemaVersion)
	_ = json.Unmarshal(input["filtered"], &r.Filtered)
	_ = json.Unmarshal(input["redactions"], &r.Redactions)
	fields, err := decodeBody(data)
	if err != nil {
		return err
	}
	r.filter = valueFilter{}
	r.filter.discover(fields, false)
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
	Redactions          int64           `json:"redactions"`
	Body                json.RawMessage `json:"body,omitempty"`
	Protobuf            []byte          `json:"protobuf,omitempty"`

	replay   bool
	native   bool
	filtered bool
	invalid  bool
	filter   valueFilter
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

func (f valueFilter) field(rule bodyRule, value any) (any, bool) {
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
			if name, ok := item.(string); ok && f.safe(name) && slices.Contains(rule.choices, name) {
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
				if name, ok := field.(string); ok && f.safe(name) {
					if _, known := bodyRules[name]; known {
						fields = append(fields, name)
					}
				}
			}
			return fields, true
		case string:
			if f.safe(v) && (v == "broker_attribution" || v == "final_tail") {
				return v, true
			}
			return f.field(bodyRule{kind: "uint"}, v)
		default:
			return f.field(bodyRule{kind: "uint"}, v)
		}
	case "bool":
		_, ok := value.(bool)
		return value, ok
	case "decimal":
		text := numberText(value)
		n, err := strconv.ParseFloat(text, 64)
		return json.Number(strconv.FormatFloat(n, 'g', -1, 64)), f.safe(text) && err == nil && n >= 0 && !math.IsInf(n, 0) && !math.IsNaN(n)
	case "uint", "int":
		number := numberText(value)
		if !f.safe(number) || !recordPatterns["decimal"].MatchString(number) {
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
	if !ok || !f.safe(s) {
		return nil, false
	}
	switch rule.kind {
	case "enum":
		return s, slices.Contains(rule.choices, s)
	case "time":
		s = canonicalTimestamp(s)
		return s, s != ""
	case "id":
		return s, legacyID.MatchString(s)
	case "label":
		return s, s != ""
	case "version":
		return s, versionPattern.MatchString(s)
	case "hex32":
		return s, hex32.MatchString(s)
	case "cursor":
		return s, s == "-" || ownerCursor.MatchString(s)
	case "cutover_archive":
		return s, archiveName.MatchString(s)
	case "attribute_name":
		return s, attributeRules[s] != ""
	case "capture_version":
		return s, s == PolicyVersion
	case "reserved":
		return s, s == "reserved"
	case "known_span_name":
		return s, slices.Contains(wireVocabulary["spanName"], s)
	case "known_event_name":
		return s, slices.Contains(wireVocabulary["eventName"], s)
	case "traceID", "spanID":
		return s, true
	default:
		return nil, false
	}
}

var kinds = map[string]bool{"run": true, "session": true, "relation": true, "scope": true, "process": true, "sample": true, "allocation": true, "association": true, "gap": true}

var bodyFields = func() map[string]string {
	fields := make(map[string]string, len(bodyRules))
	for name := range bodyRules {
		fields[name] = "body:" + name
	}
	return fields
}()

func (f valueFilter) body(input map[string]any) (map[string]any, int64) {
	p := wirePolicy{filter: f}
	out, _ := p.message("", bodyFields, input)
	return out.(map[string]any), p.removed
}

func (f valueFilter) cleanRecord(r Record) (Record, int64, error) {
	if r.Schema != RecordSchema && !(r.Schema == "" && r.SchemaVersion == 1) {
		return Record{}, 0, errors.New("unsupported record schema")
	}
	clean := Record{
		Schema: RecordSchema, SourceID: r.SourceID, EventID: r.EventID,
		SourceVersion: sanitizeSourceVersion(r.SourceVersion), SourceTime: canonicalTimestamp(r.SourceTime),
		Kind: r.Kind, OriginID: r.OriginID,
	}
	if !f.id(clean.SourceID) || !f.id(clean.EventID) || (r.OriginID != "" && !f.id(r.OriginID)) || !f.safe(clean.Kind) || !kinds[clean.Kind] || r.Body == nil {
		return Record{}, 0, errors.New("invalid record identity or kind")
	}
	body, removed := f.body(r.Body)
	if !f.safe(r.SourceVersion) {
		clean.SourceVersion = ""
	}
	if !f.safe(r.SourceTime) {
		clean.SourceTime = ""
	}
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

func numberText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return string(v)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	}
	return ""
}

func decodeBody(data []byte) (map[string]any, error) {
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	err := dec.Decode(&body)
	return body, err
}
