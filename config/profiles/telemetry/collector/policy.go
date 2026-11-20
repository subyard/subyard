package collector

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
)

const PolicyVersion = "subyard.telemetry.capture/v1"

var capturePolicyVersion = regexp.MustCompile(`^subyard\.telemetry\.capture/v1(?::[0-9a-f]{64})?$`)

func policyFingerprint(p Policy) string {
	data, _ := json.Marshal(p)
	// Bind the closed schema as well as the configurable subset.
	return fmt.Sprintf("%s:%x", PolicyVersion, sha256.Sum256(append([]byte("otlp/closed-fields/v3\x00"), data...)))
}

type Policy struct {
	Schema     string   `json:"schema"`
	Attributes []string `json:"attributes"`
	SpanNames  []string `json:"span_names"`
	EventNames []string `json:"event_names"`
}

var wireVocabulary = map[string][]string{
	"sdk":        strings.Fields("opentelemetry opentelemetry-rust opentelemetry-js"),
	"language":   strings.Fields("rust nodejs javascript typescript python go"),
	"agent":      strings.Fields("codex claude other"),
	"provider":   strings.Fields("openai anthropic azure"),
	"operation":  strings.Fields("chat text_completion generate_content embeddings execute_tool invoke_agent"),
	"evidence":   strings.Fields("authenticated-header registered-resource-attribute"),
	"tokenType":  strings.Fields("input output cacheRead cacheCreation cached_input reasoning_output total cache_read cache_creation cache_creation_5m cache_creation_1h reasoning tool"),
	"unit":       strings.Fields("1 % s ms us ns By KiBy MiBy GiBy By/s 1/s USD tokens {token} {request} {session} {line} {commit}"),
	"scopeName":  strings.Fields("codex codex_otel codex-otel claude-code claude_code"),
	"spanName":   strings.Fields("agent.interaction llm_request tool_execution claude_code.interaction claude_code.llm_request claude_code.tool codex.turn codex.api_request"),
	"eventName":  strings.Fields("claude_code.user_prompt claude_code.api_request claude_code.api_error claude_code.tool_result claude_code.tool_decision codex.api_request codex.sse_event codex.user_prompt codex.tool_result codex.tool_decision codex.conversation_starts codex.conversation_start codex.websocket_connect codex.websocket_request codex.websocket_event codex.turn_ttft codex.turn_cost codex.startup_phase codex.sandbox_outcome codex.agent_response claude_code.session_start"),
	"metricName": strings.Fields("claude_code.session.count claude_code.lines_of_code.count claude_code.pull_request.count claude_code.commit.count claude_code.cost.usage claude_code.token.usage claude_code.code_edit_tool.decision claude_code.active_time.total codex.api_request codex.api_request.duration_ms codex.sse_event codex.sse_event.duration_ms codex.tool.call codex.tool.call.duration_ms codex.turn.cost_microusd codex.websocket.request codex.websocket.request.duration_ms codex.turn.e2e_duration_ms codex.turn.ttft.duration_ms codex.turn.ttfm.duration_ms codex.turn.token_usage codex.turn.tool.call codex.turn.memory codex.turn.network_proxy codex.thread.started codex.conversation.turn.count codex.websocket.event codex.websocket.event.duration_ms gen_ai.client.token.usage gen_ai.client.operation.duration codex.turn.duration_ms codex.api_request.duration codex.turn.token.usage"),
}

var attributeRules = func() map[string]string {
	rules := map[string]string{
		"service.name": "label", "service.version": "version",
		"telemetry.sdk.name": "sdk", "telemetry.sdk.version": "version", "telemetry.sdk.language": "language",
		"agent.name": "agent", "agent.version": "version",
		"subyard.agent_run.id": "id", "subyard.agent_run.evidence": "evidence",
		"gen_ai.system": "provider", "gen_ai.provider.name": "provider", "gen_ai.operation.name": "operation",
		"event.name": "eventName", "type": "tokenType", "token_type": "tokenType",
		"model": "label", "gen_ai.request.model": "label", "gen_ai.response.model": "label",
		"tool_name": "label", "tool_namespace": "label", "gen_ai.tool.name": "label", "slug": "label", "app.version": "label",
		"duration_ms": "decimal", "ttft_ms": "decimal", "cost_usd": "decimal", "success": "bool",
		"host.id": "label", "subyard.host.id": "label", "subyard.yard.id": "label", "subyard.project.id": "label",
		"subyard.worktree.id": "label", "subyard.task.id": "label",
		// Usage attribution: identifiers, categories and counters only.
		"request_id": "id", "prompt.id": "id", "agent_id": "id", "parent_agent_id": "id", "tool_use_id": "id", "call_id": "id",
		"parent.conversation.id": "id", "parent.turn.id": "id", "subyard.parent_session.id": "id",
		"query_source": "label", "agent.type": "label", "event.kind": "label",
	}
	for _, name := range sessionAttributeNames {
		rules[name] = "id"
	}
	for _, name := range strings.Fields("gen_ai.usage.input_tokens gen_ai.usage.output_tokens gen_ai.usage.cache_read.input_tokens gen_ai.usage.reasoning.output_tokens prompt_tokens completion_tokens input_tokens output_tokens cached_input_tokens reasoning_output_tokens prompt_length cache_read_tokens cache_creation_tokens cache_creation_5m_tokens cache_creation_1h_tokens cost_usd_micros input_token_count output_token_count cached_token_count cache_write_token_count reasoning_token_count tool_token_count event.sequence tool_result_size_bytes tool_input_size_bytes output_length output_line_count") {
		rules[name] = "counter"
	}
	return rules
}()

var ownerCursor = regexp.MustCompile(`^[0-9]{20}-[0-9a-f]{16}$`)

func selectedStrings(supplied, allowed []string) []string {
	out := make([]string, 0, len(allowed))
	for _, value := range allowed {
		if slices.Contains(supplied, value) {
			out = append(out, value)
		}
	}
	return out
}

func DefaultPolicy() Policy {
	attributes := make([]string, 0, len(attributeRules))
	for name := range attributeRules {
		attributes = append(attributes, name)
	}
	slices.Sort(attributes)
	return Policy{Schema: PolicyVersion, Attributes: attributes, SpanNames: slices.Clone(wireVocabulary["spanName"]), EventNames: slices.Clone(wireVocabulary["eventName"])}
}

func LoadPolicy(path string) (Policy, error) {
	if path == "" {
		return DefaultPolicy(), nil
	}
	var p Policy
	data, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal(data, &p); err != nil {
		return p, err
	}
	if p.Schema != PolicyVersion {
		return p, errors.New("unsupported capture policy")
	}
	return p, nil
}

// Field rules are scoped to their OTLP message. A leading * denotes a
// repeated field. The pdata round trip supplies canonical OTLP JSON, including
// decimal text for 64-bit integers and hex for trace and span IDs.
var wireMessages = map[string]string{
	"otlp/logs":            "resourceLogs:*resourceLogs",
	"otlp/metrics":         "resourceMetrics:*resourceMetrics",
	"otlp/traces":          "resourceSpans:*resourceSpans",
	"resourceLogs":         "resource:resource scopeLogs:*scopeLogs",
	"resourceMetrics":      "resource:resource scopeMetrics:*scopeMetrics",
	"resourceSpans":        "resource:resource scopeSpans:*scopeSpans",
	"scopeLogs":            "scope:scope logRecords:*logRecord",
	"scopeMetrics":         "scope:scope metrics:*metric",
	"scopeSpans":           "scope:scope spans:*span",
	"resource":             "attributes:*attribute droppedAttributesCount:number",
	"scope":                "name:scopeName version:version attributes:*attribute droppedAttributesCount:number",
	"logRecord":            "timeUnixNano:number observedTimeUnixNano:number severityNumber:number attributes:*attribute droppedAttributesCount:number flags:number traceId:traceID spanId:spanID eventName:eventName",
	"span":                 "traceId:traceID spanId:spanID parentSpanId:spanID name:spanName kind:number startTimeUnixNano:number endTimeUnixNano:number attributes:*attribute droppedAttributesCount:number events:*event droppedEventsCount:number links:*link droppedLinksCount:number status:status flags:number",
	"event":                "timeUnixNano:number name:eventName attributes:*attribute droppedAttributesCount:number",
	"link":                 "traceId:traceID spanId:spanID attributes:*attribute droppedAttributesCount:number flags:number",
	"status":               "code:number",
	"metric":               "name:metricName unit:unit metadata:*attribute gauge:gauge sum:sum histogram:histogram exponentialHistogram:exponentialHistogram summary:summary",
	"gauge":                "dataPoints:*numberPoint",
	"sum":                  "dataPoints:*numberPoint aggregationTemporality:number isMonotonic:bool",
	"histogram":            "dataPoints:*histogramPoint aggregationTemporality:number",
	"exponentialHistogram": "dataPoints:*exponentialPoint aggregationTemporality:number",
	"summary":              "dataPoints:*summaryPoint",
	"numberPoint":          "attributes:*attribute startTimeUnixNano:number timeUnixNano:number flags:number exemplars:*exemplar asInt:number asDouble:number",
	"histogramPoint":       "attributes:*attribute startTimeUnixNano:number timeUnixNano:number flags:number exemplars:*exemplar count:number sum:number min:number max:number bucketCounts:*number explicitBounds:*number",
	"exponentialPoint":     "attributes:*attribute startTimeUnixNano:number timeUnixNano:number flags:number exemplars:*exemplar count:number sum:number min:number max:number scale:number zeroCount:number zeroThreshold:number positive:buckets negative:buckets",
	"summaryPoint":         "attributes:*attribute startTimeUnixNano:number timeUnixNano:number flags:number count:number sum:number quantileValues:*quantile",
	"exemplar":             "filteredAttributes:*attribute timeUnixNano:number asInt:number asDouble:number traceId:traceID spanId:spanID",
	"buckets":              "offset:number bucketCounts:*number",
	"quantile":             "quantile:number value:number",
}

var wireFields = messageFields(wireMessages)

func messageFields(messages map[string]string) map[string]map[string]string {
	out := make(map[string]map[string]string, len(messages))
	for kind, fields := range messages {
		out[kind] = map[string]string{}
		for _, field := range strings.Fields(fields) {
			name, rule, _ := strings.Cut(field, ":")
			out[kind][name] = rule
		}
	}
	return out
}

var stateFields = func() map[string]map[string]string {
	fields := messageFields(stateMessages)
	maps.Copy(fields, messageFields(coreStateMessages))
	return fields
}()

func firstRequired(rule string) []string {
	if fields := coreStateRequired[rule]; fields != nil {
		return fields
	}
	return stateRequired[rule]
}

type wirePolicy struct {
	filter  valueFilter
	policy  Policy
	removed int64
}

func (p *wirePolicy) clean(rule string, value any) (any, bool) {
	if optional, ok := strings.CutPrefix(rule, "?"); ok {
		if value == nil {
			return nil, true
		}
		rule = optional
	}
	if field, ok := strings.CutPrefix(rule, "body:"); ok {
		return p.filter.field(bodyRules[field], value)
	}
	if value == nil {
		return nil, false
	}
	if element, ok := strings.CutPrefix(rule, "@"); ok {
		input, ok := value.(map[string]any)
		if !ok || len(input) > 128 {
			return nil, false
		}
		out := make(map[string]any, len(input))
		for key, child := range input {
			if clean, valid := p.clean(element, child); valid && p.filter.id(key) {
				out[key] = clean
			}
		}
		return out, true
	}
	if element, repeated := strings.CutPrefix(rule, "*"); repeated {
		input, ok := value.([]any)
		if !ok {
			return nil, false
		}
		out := make([]any, 0, len(input))
		for _, child := range input {
			if clean, keep := p.clean(element, child); keep {
				out = append(out, clean)
			} else {
				p.removed++
			}
		}
		return out, true
	}
	fields, known := wireFields[rule]
	if !known {
		fields, known = stateFields[rule]
	}
	if known {
		input, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		if part, scoped := recordScopes[rule]; scoped {
			child := wirePolicy{filter: maps.Clone(p.filter), policy: p.policy}
			if part == "" {
				child.filter.discover(input, false)
			} else {
				child.filter.discover(input[part], false)
			}
			out, keep := child.message(rule, fields, input)
			p.removed += child.removed
			return out, keep
		}
		return p.message(rule, fields, input)
	}
	switch rule {
	case "attribute":
		return p.attribute(value)
	case "number":
		// Numeric wire fields came from the typed pdata encoder.
		if text, ok := value.(string); ok && !p.filter.safe(text) {
			return nil, false
		}
		return value, true
	case "spanName", "eventName":
		s, ok := value.(string)
		name, allowed := qualifiedEventName(s), p.policy.EventNames
		if rule == "spanName" {
			name, allowed = s, p.policy.SpanNames
		}
		return s, ok && p.filter.safe(s) && slices.Contains(wireVocabulary[rule], name) && slices.Contains(allowed, name)
	case "counter":
		rule = "uint"
	}
	if choices, ok := wireVocabulary[rule]; ok {
		return p.filter.field(bodyRule{kind: "enum", choices: choices}, value)
	}
	return p.filter.field(bodyRule{kind: rule}, value)
}

// Claude sends the unprefixed event name in event.name; the prefixed name is
// only in the log body, which is dropped. Codex always sends the prefixed name.
func qualifiedEventName(name string) string {
	if name == "" || strings.Contains(name, ".") {
		return name
	}
	return "claude_code." + name
}

// Sensitive values found in a resource or scope apply to everything below it;
// values found in one log record, span or metric apply only to that record.
var recordScopes = map[string]string{
	"resourceLogs": "resource", "resourceMetrics": "resource", "resourceSpans": "resource",
	"scopeLogs": "scope", "scopeMetrics": "scope", "scopeSpans": "scope",
	"logRecord": "", "span": "", "metric": "",
}

func (p *wirePolicy) message(rule string, fields map[string]string, input map[string]any) (any, bool) {
	out := make(map[string]any, len(fields))
	for name, child := range input {
		if empty, ok := child.(map[string]any); ok && len(empty) == 0 && fields[name] == "" {
			// The pdata round trip writes an empty log body; it carries no data,
			// so replaying an export must not count it as another redaction.
			continue
		}
		if clean, keep := p.clean(fields[name], child); keep && p.filter.safe(name) {
			out[name] = clean
			if list, ok := child.([]any); ok && strings.HasPrefix(fields[name], "body:") {
				kept, _ := clean.([]any)
				p.removed += int64(len(list) - len(kept)) // Record lists drop unknown names in place.
			}
		} else {
			p.removed++
		}
	}
	for _, required := range firstRequired(rule) {
		if item, exists := out[required]; !exists || item == "" {
			return nil, false
		}
	}
	return out, true
}

func (p *wirePolicy) attributeRule(name string) string {
	if slices.Contains(p.policy.Attributes, name) {
		return attributeRules[name]
	}
	return ""
}

func (p *wirePolicy) attribute(value any) (any, bool) {
	attr, _ := value.(map[string]any)
	name, _ := attr["key"].(string)
	rule := p.attributeRule(name)
	if rule == "" {
		return nil, false
	}
	wireValue, _ := attr["value"].(map[string]any)
	if len(wireValue) != 1 || !p.filter.safe(name) {
		return nil, false
	}
	var valueType string
	var raw any
	for valueType, raw = range wireValue {
	}
	switch rule {
	case "counter", "decimal":
		if valueType != "intValue" && valueType != "doubleValue" && valueType != "stringValue" {
			return nil, false
		}
	case "bool":
		// Claude and Codex send success as the string "true" or "false".
		if text, ok := raw.(string); ok && valueType == "stringValue" && (text == "true" || text == "false") {
			raw, valueType = text == "true", "boolValue"
		}
		if valueType != "boolValue" {
			return nil, false
		}
	default:
		if valueType != "stringValue" {
			return nil, false
		}
	}
	clean, keep := p.clean(rule, raw)
	if !keep || clean == nil {
		return nil, false
	}
	if rule == "counter" {
		n := clean.(uint64)
		if n > math.MaxInt64 {
			return nil, false
		}
		clean, valueType = strconv.FormatUint(n, 10), "intValue"
	} else if rule == "decimal" {
		valueType = "doubleValue"
	}
	return map[string]any{"key": name, "value": map[string]any{valueType: clean}}, true

}

type otlpRequest interface {
	UnmarshalProto([]byte) error
	MarshalJSON() ([]byte, error)
	UnmarshalJSON([]byte) error
	MarshalProto() ([]byte, error)
}

func otlpRequests(encoding string) (input, output otlpRequest, err error) {
	switch encoding {
	case "otlp/logs":
		return plogotlp.NewExportRequest(), plogotlp.NewExportRequest(), nil
	case "otlp/metrics":
		return pmetricotlp.NewExportRequest(), pmetricotlp.NewExportRequest(), nil
	case "otlp/traces":
		return ptraceotlp.NewExportRequest(), ptraceotlp.NewExportRequest(), nil
	default:
		return nil, nil, errors.New("unsupported OTLP encoding")
	}
}

func transformOTLP(encoding string, protobuf []byte, transform func(map[string]any) any) ([]byte, error) {
	input, output, err := otlpRequests(encoding)
	if err != nil {
		return nil, err
	}
	if err := input.UnmarshalProto(protobuf); err != nil {
		return nil, err
	}
	data, err := input.MarshalJSON()
	if err != nil {
		return nil, err
	}
	body, err := decodeBody(data)
	if err != nil {
		return nil, err
	}
	data, err = json.Marshal(transform(body))
	if err != nil {
		return nil, err
	}
	if err = output.UnmarshalJSON(data); err != nil {
		return nil, err
	}
	return output.MarshalProto()
}

func sanitizeOTLP(e *Envelope, p Policy, f valueFilter) ([]Envelope, error) {
	policy := wirePolicy{filter: f, policy: p}
	var relations []Envelope
	protobuf, err := transformOTLP(e.Encoding, e.Protobuf, func(body map[string]any) any {
		clean, _ := policy.clean(e.Encoding, body)
		if e.native {
			relations = append(sessionEnvelopes(clean.(map[string]any)), usageEnvelopes(clean.(map[string]any))...)
		}
		return clean
	})
	e.Protobuf = protobuf
	e.PolicyVersion = policyFingerprint(p)
	for i := range relations {
		relations[i].OriginID = e.OriginID
		if relations[i].EventID == "" { // Usage and bindings carry their own deterministic IDs.
			relations[i].EventID = e.EventID + ":" + strconv.Itoa(i)
		}
		relations[i].ReceivedTime = e.ReceivedTime
		relations[i].PolicyVersion = e.PolicyVersion
	}
	e.Redactions += policy.removed
	return relations, err
}
