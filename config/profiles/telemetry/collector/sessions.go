package collector

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"
)

var sessionAttributeNames = bodyRules["session_attributes"].choices

// visitOTLPAttributes walks all retained attribute positions within a resource,
// including metric points, exemplars, span events and span links.
func visitOTLPAttributes(value any, visit func(map[string]any)) {
	switch node := value.(type) {
	case map[string]any:
		for name, child := range node {
			if name == "attributes" || name == "filteredAttributes" || name == "metadata" {
				if attrs, ok := child.([]any); ok {
					for _, attr := range attrs {
						if field, ok := attr.(map[string]any); ok {
							visit(field)
						}
					}
				}
				continue
			}
			visitOTLPAttributes(child, visit)
		}
	case []any:
		for _, child := range node {
			visitOTLPAttributes(child, visit)
		}
	}
}

func sessionEnvelopes(signal map[string]any) []Envelope {
	var entries []Envelope
	for _, group := range signal {
		for _, resource := range group.([]any) {
			sessions := map[string][]string{}
			visitOTLPAttributes(resource, func(attr map[string]any) {
				name := stringValue(attr, "key")
				if slices.Contains(sessionAttributeNames, name) {
					value, _ := attr["value"].(map[string]any)
					session := stringValue(value, "stringValue")
					if session != "" && !slices.Contains(sessions[session], name) {
						sessions[session] = append(sessions[session], name)
					}
				}
			})
			var run, evidence string
			if info, ok := resource.(map[string]any)["resource"]; ok {
				visitOTLPAttributes(info, func(attr map[string]any) {
					value, _ := attr["value"].(map[string]any)
					switch attr["key"] {
					case "subyard.agent_run.id":
						run = stringValue(value, "stringValue")
					case "subyard.agent_run.evidence":
						evidence = stringValue(value, "stringValue")
					}
				})
			}
			for _, session := range slices.Sorted(maps.Keys(sessions)) {
				names := sessions[session]
				slices.Sort(names)
				kind := "session"
				body := map[string]any{"session_id": session, "session_attributes": names, "quality": "explicit", "source": "native-otlp-attribute"}
				if run != "" {
					kind = "relation"
					body["agent_run_id"] = run
					body["relation"] = "native_session"
					if evidence != "" {
						body["source"] = "native-otlp-attribute/" + evidence
					}
				}
				raw, _ := json.Marshal(body)
				entries = append(entries, Envelope{SourceID: "collector:otlp-correlation", SourceVersion: "1", SourceTime: time.Now().UTC().Format(timestampLayout), Kind: kind, Encoding: "json", PolicyVersion: PolicyVersion, Body: raw})
			}
		}
	}
	return entries
}

var sessionFields = []string{"session_id", "native_session_id", "session_attributes"}

const nativeSessionSource = "native-otlp-attribute"

func (r Record) hasSessionMetadata() bool {
	if r.Kind == "session" || r.Kind == "relation" || stringValue(r.Body, "relation") == "native_session" ||
		strings.HasPrefix(stringValue(r.Body, "source"), nativeSessionSource) {
		return true
	}
	for _, field := range sessionFields {
		if _, exists := r.Body[field]; exists {
			return true
		}
	}
	return false
}

func filterSessionRelation(r *Record, policy Policy) (bool, int64) {
	var removed int64
	remove := func(fields ...string) {
		for _, field := range fields {
			if _, exists := r.Body[field]; exists {
				delete(r.Body, field)
				removed++
			}
		}
	}
	relationKind := r.Kind == "session" || r.Kind == "relation"
	nativeRelation := stringValue(r.Body, "relation") == "native_session"
	nativeSource := strings.HasPrefix(stringValue(r.Body, "source"), nativeSessionSource)
	keepRun := slices.Contains(policy.Attributes, "subyard.agent_run.id")
	keepEvidence := slices.Contains(policy.Attributes, "subyard.agent_run.evidence")
	names, hasNames := r.Body["session_attributes"]
	list, hasList := names.([]any)
	var retained []any
	allowed := true
	if hasList {
		for _, name := range list {
			if slices.Contains(policy.Attributes, name.(string)) {
				retained = append(retained, name)
			}
		}
		allowed = len(retained) != 0
	} else {
		// Legacy records do not identify the contributing attribute.
		for _, name := range sessionAttributeNames {
			if !slices.Contains(policy.Attributes, name) {
				allowed = false
				break
			}
		}
	}
	primary, alias := stringValue(r.Body, "session_id"), stringValue(r.Body, "native_session_id")
	session := firstNonempty(primary, alias)
	ambiguous := primary != "" && alias != "" && primary != alias
	if !relationKind && (!allowed || !keepRun || ambiguous || (session == "" && hasNames)) {
		remove(sessionFields...)
		if nativeRelation {
			remove("relation")
		}
		if nativeSource {
			remove("source")
		}
		return true, removed
	}
	if relationKind && session == "" {
		return r.Kind != "session" && !nativeRelation && !nativeSource && !hasNames, 0
	}
	if !allowed || ambiguous {
		return false, 0
	}

	if primary == "" && alias != "" {
		r.Body["session_id"] = alias
		delete(r.Body, "native_session_id")
	} else {
		remove("native_session_id")
	}
	if hasList {
		r.Body["session_attributes"] = retained
		removed += int64(len(list) - len(retained))
	}
	if !keepRun {
		remove("agent_run_id", "parent_run_id", "relation")
		r.Kind = "session"
	}
	if (relationKind || nativeSource) && (!keepRun || !keepEvidence) {
		if old, exists := r.Body["source"]; exists && old != nativeSessionSource {
			removed++
		}
		r.Body["source"] = nativeSessionSource
	}
	return true, removed
}
