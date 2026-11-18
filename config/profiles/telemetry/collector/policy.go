package collector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	return fmt.Sprintf("%s:%x", PolicyVersion, sha256.Sum256(append([]byte("otlp/closed-fields/v2\x00"), data...)))
}

type Policy struct {
	Schema     string   `json:"schema"`
	Attributes []string `json:"attributes"`
	SpanNames  []string `json:"span_names"`
	EventNames []string `json:"event_names"`
}

var wireVocabulary = map[string][]string{
	"service":    strings.Fields("codex codex_cli_rs codex_exec claude-code claude_code"),
	"sdk":        strings.Fields("opentelemetry opentelemetry-rust opentelemetry-js"),
	"language":   strings.Fields("rust nodejs javascript typescript python go"),
	"agent":      strings.Fields("codex claude other"),
	"provider":   strings.Fields("openai anthropic azure"),
	"operation":  strings.Fields("chat text_completion generate_content embeddings execute_tool invoke_agent"),
	"evidence":   strings.Fields("authenticated-header registered-resource-attribute"),
	"tokenType":  strings.Fields("input output cacheRead cacheCreation cached_input reasoning_output total"),
	"unit":       strings.Fields("1 % s ms us ns By KiBy MiBy GiBy By/s 1/s USD tokens {token} {request} {session} {line} {commit}"),
	"scopeName":  strings.Fields("codex codex_otel codex-otel claude-code claude_code"),
	"spanName":   strings.Fields("agent.interaction llm_request tool_execution claude_code.interaction claude_code.llm_request claude_code.tool codex.turn codex.api_request"),
	"eventName":  strings.Fields("claude_code.user_prompt claude_code.api_request claude_code.api_error claude_code.tool_result claude_code.tool_decision codex.api_request codex.sse_event codex.user_prompt codex.tool_result codex.conversation_starts"),
	"metricName": strings.Fields("claude_code.session.count claude_code.lines_of_code.count claude_code.pull_request.count claude_code.commit.count claude_code.cost.usage claude_code.token.usage claude_code.code_edit_tool.decision claude_code.active_time.total codex.api_request codex.api_request.duration_ms codex.turn.e2e_duration_ms codex.turn.ttft.duration_ms codex.turn.ttfm.duration_ms codex.turn.token_usage codex.turn.tool.call codex.turn.memory codex.turn.network_proxy codex.thread.started codex.conversation.turn.count codex.websocket.event codex.websocket.event.duration_ms gen_ai.client.token.usage gen_ai.client.operation.duration"),
}

var attributeRules = func() map[string]string {
	rules := map[string]string{
		"service.name": "service", "service.version": "version",
		"telemetry.sdk.name": "sdk", "telemetry.sdk.version": "version", "telemetry.sdk.language": "language",
		"agent.name": "agent", "agent.version": "version",
		"subyard.agent_run.id": "id", "subyard.agent_run.evidence": "evidence",
		"gen_ai.system": "provider", "gen_ai.provider.name": "provider", "gen_ai.operation.name": "operation",
		"event.name": "eventName", "type": "tokenType", "token_type": "tokenType",
	}
	for _, name := range sessionAttributeNames {
		rules[name] = "id"
	}
	for _, name := range strings.Fields("gen_ai.usage.input_tokens gen_ai.usage.output_tokens gen_ai.usage.cache_read.input_tokens gen_ai.usage.reasoning.output_tokens prompt_tokens completion_tokens input_tokens output_tokens cached_input_tokens reasoning_output_tokens") {
		rules[name] = "counter"
	}
	return rules
}()

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

var wireFields = func() map[string]map[string]string {
	out := make(map[string]map[string]string, len(wireMessages))
	for kind, fields := range wireMessages {
		out[kind] = map[string]string{}
		for _, field := range strings.Fields(fields) {
			name, rule, _ := strings.Cut(field, ":")
			out[kind][name] = rule
		}
	}
	return out
}()

type wirePolicy struct {
	key         identityKey
	id          identity
	rekeyBinary bool
	policy      Policy
	removed     int64
}

func (p *wirePolicy) clean(rule string, value any) (any, bool) {
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
	if fields, known := wireFields[rule]; known {
		input, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		out := make(map[string]any, len(fields))
		for name, child := range input {
			if clean, keep := p.clean(fields[name], child); keep {
				out[name] = clean
			} else {
				p.removed++
			}
		}
		return out, true
	}
	switch rule {
	case "attribute":
		return p.attribute(value)
	case "number":
		// Numeric wire fields came from the typed pdata encoder.
		return value, true
	case "counter":
		text, _ := value.(string)
		n, err := strconv.ParseInt(text, 10, 64)
		return value, err == nil && n >= 0
	case "bool":
		_, ok := value.(bool)
		return value, ok
	}
	s, ok := value.(string)
	if !ok {
		return nil, false
	}
	switch rule {
	case "version":
		return s, versionPattern.MatchString(s)
	case "id":
		s = p.id(s)
		return s, s != ""
	case "traceID":
		return p.binaryID(s, 16)
	case "spanID":
		return p.binaryID(s, 8)
	case "spanName":
		return s, slices.Contains(wireVocabulary[rule], s) && slices.Contains(p.policy.SpanNames, s)
	case "eventName":
		return s, slices.Contains(wireVocabulary[rule], s) && slices.Contains(p.policy.EventNames, s)
	default:
		return s, slices.Contains(wireVocabulary[rule], s)
	}
}

func (p *wirePolicy) binaryID(value string, size int) (string, bool) {
	if value == "" || value == strings.Repeat("0", size*2) || !p.rekeyBinary {
		return value, true
	}
	sum := p.key.digest("otlp-binary-id\x00" + value)
	return hex.EncodeToString(sum[:size]), true
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
	valueType := "stringValue"
	if rule == "counter" {
		valueType = "intValue"
	}
	if len(wireValue) != 1 {
		return nil, false
	}
	clean, keep := p.clean(rule, wireValue[valueType])
	if !keep {
		return nil, false
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

func (key identityKey) sanitizeOTLP(e *Envelope, p Policy, id identity, rekeyBinary bool) ([]Envelope, error) {
	policy := wirePolicy{key: key, id: id, policy: p, rekeyBinary: rekeyBinary}
	var relations []Envelope
	protobuf, err := transformOTLP(e.Encoding, e.Protobuf, func(body map[string]any) any {
		clean, _ := policy.clean(e.Encoding, body)
		if e.native {
			relations = sessionEnvelopes(clean.(map[string]any))
		}
		return clean
	})
	e.Protobuf = protobuf
	e.PolicyVersion = policyFingerprint(p)
	for i := range relations {
		relations[i].PolicyVersion = e.PolicyVersion
	}
	e.Redactions += policy.removed
	return relations, err
}
