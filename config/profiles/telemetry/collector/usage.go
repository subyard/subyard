package collector

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
)

var usageIDs = strings.Fields("agent_run_id session_id turn_id native_agent_id parent_native_agent_id request_id model")
var usageCounters = strings.Fields("input_tokens cache_read_tokens cache_write_tokens output_tokens reasoning_tokens tool_output_bytes")

func sanitizeUsage(r Record) (Record, int64, error) {
	clean := make(map[string]any, len(r.Body))
	for _, key := range usageFieldNames {
		field := usageSchema.Fields[key]
		value, exists := r.Body[key]
		if !exists && !field.Required && !(r.Kind == "tool_result" && field.ToolRequired) {
			continue
		}
		validated, ok := field.validate(value)
		if !ok {
			return r, 0, fmt.Errorf("invalid usage %s", key)
		}
		clean[key] = validated
	}
	removed := int64(len(r.Body) - len(clean))
	r.Body = clean
	return r, removed, nil
}

// One native log stream per client. Metrics aggregate several calls and
// Codex traces also emit response.completed, without a shared response ID.
func nativeUsage(ctx context.Context, log plog.LogRecord, resource pcommon.Map, allow map[string]bool) (Envelope, bool) {
	attrs := log.Attributes()
	readString := func(values pcommon.Map, key string) string {
		if v, ok := values.Get(key); ok && allow[key] && v.Type() == pcommon.ValueTypeStr && safeName.MatchString(v.Str()) {
			return v.Str()
		}
		return ""
	}
	attr := func(key string) string { return readString(attrs, key) }
	counter := func(key string) (int64, bool) {
		v, ok := attrs.Get(key)
		if !ok || forbidden(key) {
			return 0, false
		}
		field := usageSchema.Attributes[key]
		var value any
		switch v.Type() {
		case pcommon.ValueTypeInt:
			value = v.Int()
		case pcommon.ValueTypeStr:
			if allow[key] {
				value = v.Str()
			}
		}
		n, valid := field.validate(value)
		if !valid {
			return 0, false
		}
		return n.(int64), true
	}

	kind, client, sessionKey, turnKey := "model_usage", "claude", "session.id", "prompt.id"
	counterKeys := []string{"input_tokens", "cache_read_tokens", "cache_creation_tokens", "output_tokens", ""}
	event := attr("event.name")
	switch event {
	case "api_request", "claude_code.api_request", "tool_result", "claude_code.tool_result":
		// Older Claude records use the short event name.
		if event == "tool_result" || event == "claude_code.tool_result" {
			kind = "tool_result"
		}
	case "codex.sse_event":
		if attr("event.kind") != "response.completed" {
			return Envelope{}, false
		}
		client, sessionKey, turnKey = "codex", "conversation.id", "turn.id"
		counterKeys = []string{"input_token_count", "cached_token_count", "cache_write_token_count", "output_token_count", "reasoning_token_count"}
	default:
		return Envelope{}, false
	}

	body := map[string]any{"client": client}
	put := func(key, value string) {
		if value != "" {
			body[key] = value
		}
	}
	session := attr(sessionKey)
	if session == "" {
		session = readString(resource, sessionKey)
	}
	put("session_id", session)
	put("turn_id", attr(turnKey))
	put("model", attr("model"))
	run, _ := ctx.Value(runContextKey{}).(string)
	put("agent_run_id", run)
	request := ""
	if client == "claude" {
		request = attr("request_id")
		put("native_agent_id", attr("agent_id"))
		put("parent_native_agent_id", attr("parent_agent_id"))
	}
	stamp := log.Timestamp()
	if stamp == 0 {
		// Codex's Rust tracing exporter sets observed time on the log.
		stamp = log.ObservedTimestamp()
	}
	if stamp == 0 {
		if value, exists := attrs.Get("event.timestamp"); exists && allow["event.timestamp"] {
			if parsed, ok := usageTimestamp(value.Str()); ok && parsed.UnixNano() > 0 {
				stamp = pcommon.NewTimestampFromTime(parsed)
			}
		}
	}
	body["request_identity"] = "provider"
	if request == "" {
		if kind == "tool_result" || stamp == 0 || session == "" {
			return Envelope{}, false
		}
		// Delivery preserves the source log timestamp and Claude sequence.
		// Exclude the run from the key so imports and resumed runs can't
		// charge the same historical log a second time.
		sequence, _ := counter("event.sequence")
		request = fmt.Sprintf("%x", sha256.Sum256(fmt.Appendf(nil, "%s/%d/%d", session, stamp, sequence)))
		body["request_identity"] = "native_log"
	}
	body["request_id"] = request

	if kind == "tool_result" {
		size, ok := counter("tool_result_size_bytes")
		tool := attr("tool_use_id")
		if !ok || tool == "" {
			return Envelope{}, false
		}
		body["tool_use_id"], body["tool_result_bytes"] = tool, size
	} else {
		found := false
		for i, key := range counterKeys {
			if n, ok := counter(key); ok {
				body[usageCounters[i]] = n
				found = true
			}
		}
		if !found {
			return Envelope{}, false
		}
	}
	sourceTime := stamp.AsTime().UTC().Format(timestampLayout)
	clean, _, err := sanitizeUsage(Record{Kind: kind, Body: body})
	if err != nil {
		return Envelope{}, false
	}
	raw, _ := json.Marshal(clean.Body)
	return Envelope{
		EventID:  fmt.Sprintf("usage:%x", sha256.Sum256(append(raw, sourceTime...))),
		SourceID: "collector:native-usage", SourceVersion: "1", SourceTime: sourceTime,
		Kind: kind, Encoding: "json", PolicyVersion: PolicyVersion, Body: raw,
	}, true
}

// Agree on each field independently. A late observation can fill an absent
// field, while incompatible evidence leaves that field unknown.
func usageViewSQL() string {
	sql := `CREATE VIEW IF NOT EXISTS model_usage AS
SELECT json_extract(body,'$.client') AS client,
json_extract(body,'$.request_identity') AS request_identity,
json_extract(body,'$.request_id') AS request_id,
min(nullif(source_time,'1970-01-01T00:00:00.000000000Z')) AS source_time`
	for _, key := range slices.Concat(usageIDs, []string{"trigger"}, usageCounters) {
		if key != "request_id" {
			field := "json_extract(body,'$." + key + "')"
			sql += ", CASE WHEN min(" + field + ")=max(" + field + ") THEN min(" + field + ") END AS " + key
		}
	}
	return sql + `
FROM records WHERE kind='model_usage'
GROUP BY client,request_identity,request_id;
DROP VIEW IF EXISTS model_tool_results;
CREATE VIEW model_tool_results AS
SELECT json_extract(body,'$.client') AS client,
json_extract(body,'$.request_identity') AS request_identity,
json_extract(body,'$.request_id') AS request_id,
json_extract(body,'$.tool_use_id') AS tool_use_id,
CASE WHEN min(json_extract(body,'$.tool_result_bytes'))=max(json_extract(body,'$.tool_result_bytes'))
	THEN min(json_extract(body,'$.tool_result_bytes')) END AS tool_result_bytes
FROM records WHERE kind='tool_result'
GROUP BY client,request_identity,request_id,tool_use_id;`
}
