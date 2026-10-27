package collector

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
)

const PolicyVersion = "subyard.telemetry.capture/v1"

var capturePolicyVersion = regexp.MustCompile(`^subyard\.telemetry\.capture/v1(?::[0-9a-f]{64})?$`)

func policyFingerprint(p Policy) string {
	data, _ := json.Marshal(p)
	return fmt.Sprintf("%s:%x", p.Schema, sha256.Sum256(data))
}

type Policy struct {
	Schema     string   `json:"schema"`
	Attributes []string `json:"attributes"`
	SpanNames  []string `json:"span_names"`
	EventNames []string `json:"event_names"`
}

func DefaultPolicy() Policy {
	return Policy{Schema: PolicyVersion, Attributes: strings.Fields(`service.name service.version telemetry.sdk.name telemetry.sdk.version telemetry.sdk.language deployment.environment.name session.id session_id conversation.id conversation_id thread.id agent.name agent.version agent.id subyard.agent_run.id subyard.agent_run.evidence gen_ai.system gen_ai.provider.name gen_ai.request.model gen_ai.response.model gen_ai.operation.name gen_ai.usage.input_tokens gen_ai.usage.output_tokens gen_ai.usage.cache_read.input_tokens gen_ai.usage.reasoning.output_tokens model model.name event.name tool.name terminal.type prompt_tokens completion_tokens input_tokens output_tokens cached_input_tokens reasoning_output_tokens`), SpanNames: []string{}, EventNames: []string{}}
}
func LoadPolicy(path string) (Policy, error) {
	if path == "" {
		return DefaultPolicy(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Policy{}, err
	}
	var p Policy
	if err = json.Unmarshal(data, &p); err != nil {
		return p, err
	}
	if p.Schema != PolicyVersion {
		return p, errors.New("unsupported capture policy")
	}
	for _, k := range p.Attributes {
		if forbidden(k) {
			return p, errors.New("capture policy cannot permit sensitive attribute")
		}
	}
	return p, nil
}
func forbidden(key string) bool {
	k := strings.ToLower(key)
	// This standardized deployment label is safe metadata, not an environment
	// variable dump. It is also part of our default configurable allowlist.
	if k == "deployment.environment.name" {
		return false
	}
	usageCounter := strings.HasSuffix(k, "tokens") || strings.HasSuffix(k, "token_count") || strings.HasSuffix(k, "tokens_count")
	if strings.Contains(k, "token") && !usageCounter {
		return true
	}
	for _, s := range []string{"auth", "token.", "api_key", "secret", "password", "capabilit", "argv", "command_line", "environment", "http.request.header", "http.response.header", "prompt", "response.body", "tool.arguments", "tool.result", "input.messages", "output.messages"} {
		if strings.Contains(k, s) && !(s == "prompt" && usageCounter) {
			return true
		}
	}
	return false
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,160}$`)

// UCUM uses printable ASCII punctuation for units, expressions and annotations.
// Keep the existing metadata length bound without treating units as names.
var safeUnit = regexp.MustCompile(`^[\x21-\x7e]{1,160}$`)
var safeNumeric = regexp.MustCompile(`^-?[0-9]+$`)
var safeHex = regexp.MustCompile(`^[a-fA-F0-9]*$`)

// Sanitization uses official OTLP JSON readers/writers. Numeric strings remain
// exact, including uint64 timestamps/counters; supported structures are untouched.
func sanitizeJSON(data []byte, p Policy) ([]byte, int64, error) {
	var root any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, 0, err
	}
	allow := map[string]bool{}
	for _, k := range p.Attributes {
		if !forbidden(k) {
			allow[k] = true
		}
	}
	var n int64
	names := func(list []string, value string) bool {
		for _, name := range list {
			if value == name && safeName.MatchString(value) {
				return true
			}
		}
		return false
	}
	var walk func(any, string, bool, string) any
	walk = func(value any, key string, attribute bool, class string) any {
		switch v := value.(type) {
		case map[string]any:
			out := make(map[string]any, len(v))
			for k, x := range v {
				if attribute && (k == "arrayValue" || k == "kvlistValue" || k == "bytesValue") {
					n++
					continue
				}
				if k == "attributes" || k == "filteredAttributes" {
					list, _ := x.([]any)
					kept := []any{}
					for _, a := range list {
						m, ok := a.(map[string]any)
						if !ok {
							n++
							continue
						}
						name, _ := m["key"].(string)
						value, _ := m["value"].(map[string]any)
						_, intValue := value["intValue"]
						_, doubleValue := value["doubleValue"]
						_, boolValue := value["boolValue"]
						numeric := intValue || doubleValue || boolValue
						if (!allow[name] && !numeric) || forbidden(name) {
							n++
							continue
						}
						kept = append(kept, walk(m, k, true, class))
					}
					out[k] = kept
					continue
				}
				if k == "body" || k == "traceState" || k == "message" || k == "description" {
					n++
					continue
				}
				nextClass := class
				switch k {
				case "spans":
					nextClass = "span"
				case "events":
					nextClass = "event"
				case "scope":
					nextClass = "scope"
				case "metrics":
					nextClass = "metric"
				}
				out[k] = walk(x, k, attribute, nextClass)
			}
			return out
		case []any:
			out := make([]any, len(v))
			for i, x := range v {
				out[i] = walk(x, key, attribute, class)
			}
			return out
		case string:
			keep := false
			switch key {
			case "key":
				keep = attribute
			case "stringValue":
				keep = attribute && safeName.MatchString(v)
			case "name":
				keep = safeName.MatchString(v)
				if class == "span" {
					keep = names(p.SpanNames, v)
				}
				if class == "event" {
					keep = names(p.EventNames, v)
				}
			case "eventName":
				keep = names(p.EventNames, v)
			case "unit":
				keep = safeUnit.MatchString(v)
			case "severityText":
				keep = safeName.MatchString(v)
			case "version":
				keep = safeName.MatchString(v)
			case "schemaUrl":
				keep = strings.HasPrefix(v, "https://opentelemetry.io/schemas/") && safeName.MatchString(strings.TrimPrefix(v, "https://"))
			case "traceId", "spanId", "parentSpanId":
				keep = len(v) <= 32 && safeHex.MatchString(v)
			case "timeUnixNano", "startTimeUnixNano", "endTimeUnixNano", "observedTimeUnixNano", "asInt", "intValue", "count", "zeroCount", "bucketCounts", "positive", "negative":
				keep = safeNumeric.MatchString(v)
			case "asDouble", "doubleValue", "sum", "min", "max", "value", "quantile", "explicitBounds", "zeroThreshold":
				keep = v == "NaN" || v == "Infinity" || v == "-Infinity"
			}
			if keep {
				return v
			}
			n++
			return ""
		default:
			return v
		}
	}
	clean := walk(root, "", false, "")
	out, err := json.Marshal(clean)
	return out, n, err
}
func SanitizeEnvelope(e *Envelope, p Policy) error {
	var data []byte
	var err error
	switch e.Encoding {
	case "otlp/logs":
		r := plogotlp.NewExportRequest()
		if err = r.UnmarshalProto(e.Protobuf); err != nil {
			return err
		}
		data, err = r.MarshalJSON()
		if err != nil {
			return err
		}
		data, n, err := sanitizeJSON(data, p)
		if err != nil {
			return err
		}
		clean := plogotlp.NewExportRequest()
		if err = clean.UnmarshalJSON(data); err != nil {
			return err
		}
		e.Protobuf, err = clean.MarshalProto()
		e.Redactions += n
	case "otlp/metrics":
		r := pmetricotlp.NewExportRequest()
		if err = r.UnmarshalProto(e.Protobuf); err != nil {
			return err
		}
		data, err = r.MarshalJSON()
		if err != nil {
			return err
		}
		data, n, err := sanitizeJSON(data, p)
		if err != nil {
			return err
		}
		clean := pmetricotlp.NewExportRequest()
		if err = clean.UnmarshalJSON(data); err != nil {
			return err
		}
		e.Protobuf, err = clean.MarshalProto()
		e.Redactions += n
	case "otlp/traces":
		r := ptraceotlp.NewExportRequest()
		if err = r.UnmarshalProto(e.Protobuf); err != nil {
			return err
		}
		data, err = r.MarshalJSON()
		if err != nil {
			return err
		}
		data, n, err := sanitizeJSON(data, p)
		if err != nil {
			return err
		}
		clean := ptraceotlp.NewExportRequest()
		if err = clean.UnmarshalJSON(data); err != nil {
			return err
		}
		e.Protobuf, err = clean.MarshalProto()
		e.Redactions += n
	default:
		return errors.New("unsupported OTLP encoding")
	}
	e.PolicyVersion = policyFingerprint(p)
	return err
}
