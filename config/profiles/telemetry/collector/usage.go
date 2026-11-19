package collector

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

const usageSource = "collector:usage"

// Native counter names stay unchanged. Codex tool_token_count is the total of
// the other Codex counters and must never be added to them.
var usageCounters = strings.Fields("input_tokens output_tokens cache_read_tokens cache_creation_tokens cache_creation_5m_tokens cache_creation_1h_tokens cost_usd cost_usd_micros input_token_count output_token_count cached_token_count cache_write_token_count reasoning_token_count tool_token_count tool_result_size_bytes tool_input_size_bytes output_length output_line_count prompt_length")

// usageEvents maps a qualified native event to its client and usage event.
var usageEvents = map[string][2]string{
	"claude_code.api_request": {"claude", "model_request"}, "claude_code.tool_result": {"claude", "tool_output"},
	"claude_code.user_prompt": {"claude", "user_prompt"}, "codex.sse_event": {"codex", "model_request"},
	"codex.tool_result": {"codex", "tool_output"}, "codex.user_prompt": {"codex", "user_prompt"},
}

// usageEnvelopes derives usage records and durable session bindings from a
// sanitized OTLP logs request. It reads only retained IDs, categories and
// counters. IDs are deterministic, so a re-delivered export or a replay adds
// nothing. Metrics and spans repeat the same usage and are ignored.
func usageEnvelopes(signal map[string]any) []Envelope {
	var out []Envelope
	seen := map[string]bool{}
	add := func(kind, id string, sourceTime string, body map[string]any) {
		if seen[id] {
			return
		}
		seen[id] = true
		for name, value := range body {
			if value == "" {
				delete(body, name)
			}
		}
		raw, _ := json.Marshal(body)
		out = append(out, Envelope{EventID: id, SourceID: usageSource, SourceVersion: "1", SourceTime: sourceTime,
			Kind: kind, Encoding: "json", PolicyVersion: PolicyVersion, Body: raw})
	}
	resources, _ := signal["resourceLogs"].([]any)
	for _, resource := range resources {
		resourceAttrs := otlpAttributes(mapValue(mapValue(resource)["resource"]))
		run, parent := resourceAttrs["subyard.agent_run.id"], resourceAttrs["subyard.parent_session.id"]
		for _, scope := range listValue(mapValue(resource)["scopeLogs"]) {
			for _, item := range listValue(mapValue(scope)["logRecords"]) {
				record := mapValue(item)
				attrs := otlpAttributes(record)
				at := firstNonempty(nonzero(stringValue(record, "timeUnixNano")), nonzero(stringValue(record, "observedTimeUnixNano")))
				sourceTime := unknownSourceTime
				if ns, err := strconv.ParseInt(at, 10, 64); err == nil {
					sourceTime = time.Unix(0, ns).UTC().Format(timestampLayout)
				}
				if child, parent := attrs["conversation.id"], attrs["parent.conversation.id"]; child != "" && parent != "" && child != parent {
					add("relation", bindingID("codex", child, parent), sourceTime, map[string]any{"client": "codex",
						"session_id": child, "session_attributes": []string{"conversation.id"}, "parent_session_id": parent,
						"relation": "child_thread", "quality": "explicit", "source": nativeSessionSource})
				}
				kind, known := usageEvents[qualifiedEventName(attrs["event.name"])]
				client, event := kind[0], kind[1]
				sessionKey := map[string]string{"claude": "session.id", "codex": "conversation.id"}[client]
				session := attrs[sessionKey]
				if !known || session == "" || (client == "codex" && event == "model_request" && attrs["event.kind"] != "response.completed") {
					continue
				}
				body := map[string]any{"client": client, "event": event, "session_id": session, "session_attributes": []string{sessionKey}}
				put := func(field, value string) {
					if value != "" {
						body[field] = value
					}
				}
				put("agent_run_id", run)
				put("model", attrs["model"])
				put("query_source", attrs["query_source"])
				put("native_agent_id", attrs["agent_id"])
				put("parent_native_agent_id", attrs["parent_agent_id"])
				put("turn_id", attrs["prompt.id"])
				put("tool_use_id", firstNonempty(attrs["tool_use_id"], attrs["call_id"]))
				put("event_sequence", attrs["event.sequence"])
				for _, name := range usageCounters {
					if value, ok := attrs[name]; ok && value != "" {
						body[name] = json.Number(value)
					}
				}
				// A provider ID names the request itself; otherwise the native
				// log's own time and sequence identify it across deliveries.
				key, identity := map[string]string{"model_request": attrs["request_id"], "tool_output": stringValue(body, "tool_use_id"),
					"user_prompt": attrs["prompt.id"]}[event], "provider"
				if event == "model_request" {
					put("request_id", key)
				}
				if key == "" {
					if at == "" {
						continue
					}
					key, identity = at+"/"+attrs["event.sequence"], "native_log"
				}
				body["request_identity"] = identity
				add("usage", "usage:"+digest(client, session, event, identity, key), sourceTime, body)
				if agent := attrs["agent_id"]; agent != "" {
					add("relation", bindingID(client, session, agent, attrs["parent_agent_id"]), sourceTime, map[string]any{"client": client,
						"session_id": session, "session_attributes": []string{sessionKey}, "native_agent_id": agent,
						"parent_native_agent_id": attrs["parent_agent_id"], "relation": "subagent", "quality": "explicit", "source": nativeSessionSource})
				}
				if parent != "" && parent != session {
					add("relation", bindingID(client, session, "parent", parent), sourceTime, map[string]any{"client": client,
						"session_id": session, "session_attributes": []string{sessionKey}, "parent_session_id": parent,
						"relation": "parent_session", "quality": "explicit", "source": nativeSessionSource})
				}
			}
		}
	}
	return out
}

func bindingID(parts ...string) string { return "binding:" + digest(parts...) }

func digest(parts ...string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(parts, "\x00"))))
}

func nonzero(value string) string {
	if strings.Trim(value, "0") == "" {
		return ""
	}
	return value
}

func mapValue(value any) map[string]any { m, _ := value.(map[string]any); return m }
func listValue(value any) []any         { l, _ := value.([]any); return l }

// otlpAttributes flattens sanitized OTLP JSON attributes to their scalar text.
func otlpAttributes(node map[string]any) map[string]string {
	out := map[string]string{}
	for _, item := range listValue(node["attributes"]) {
		attr := mapValue(item)
		for _, value := range mapValue(attr["value"]) {
			switch v := value.(type) {
			case string:
				out[stringValue(attr, "key")] = v
			case json.Number:
				out[stringValue(attr, "key")] = string(v)
			}
		}
	}
	return out
}

// usageSchema adds read-only projections over the unchanged records table.
// It runs on every writer open and never changes the store version.
var usageSchema = `CREATE INDEX IF NOT EXISTS records_usage ON records(kind) WHERE kind='usage';
CREATE VIEW IF NOT EXISTS model_requests AS SELECT origin_id,event_id,source_time,` + usageColumns + ` FROM records WHERE kind='usage' AND json_extract(body,'$.event')='model_request';
CREATE VIEW IF NOT EXISTS tool_outputs AS SELECT origin_id,event_id,source_time,` + usageColumns + ` FROM records WHERE kind='usage' AND json_extract(body,'$.event')='tool_output';
CREATE VIEW IF NOT EXISTS model_turn_triggers AS SELECT origin_id,event_id,source_time,client,session_id,native_agent_id,request_id,
	CASE previous WHEN 'user_prompt' THEN 'new_prompt' WHEN 'tool_output' THEN 'tool_result' ELSE 'unknown' END AS cause
	FROM (SELECT origin_id,event_id,source_time,json_extract(body,'$.event') AS event,json_extract(body,'$.client') AS client,
		json_extract(body,'$.session_id') AS session_id,json_extract(body,'$.native_agent_id') AS native_agent_id,
		json_extract(body,'$.request_id') AS request_id,
		lag(json_extract(body,'$.event')) OVER (PARTITION BY origin_id,json_extract(body,'$.client'),json_extract(body,'$.session_id'),
			coalesce(json_extract(body,'$.native_agent_id'),'') ORDER BY source_time,json_extract(body,'$.event_sequence'),sequence) AS previous
		FROM records WHERE kind='usage') WHERE event='model_request';`

var usageColumns = func() string {
	columns := slices.Concat(strings.Fields("client session_id agent_run_id native_agent_id parent_native_agent_id turn_id request_id request_identity tool_use_id event_sequence query_source model"), usageCounters)
	for i, name := range columns {
		columns[i] = "json_extract(body,'$." + name + "') AS " + name
	}
	return strings.Join(columns, ",")
}()
