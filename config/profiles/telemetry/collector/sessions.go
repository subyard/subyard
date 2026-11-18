package collector

import (
	"encoding/json"
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
			for session, names := range sessions {
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

func filterSessionRelation(r *Record, policy Policy) (bool, int64) {
	if r.Kind != "session" && r.Kind != "relation" {
		return true, 0
	}
	if stringValue(r.Body, "session_id") == "" {
		return r.Kind != "session" && stringValue(r.Body, "relation") != "native_session" &&
			!strings.HasPrefix(stringValue(r.Body, "source"), "native-otlp-attribute"), 0
	}
	var removed int64
	if names, ok := r.Body["session_attributes"].([]any); ok {
		before := len(names)
		names = slices.DeleteFunc(names, func(name any) bool { return !slices.Contains(policy.Attributes, name.(string)) })
		if len(names) == 0 {
			return false, 0
		}
		removed += int64(before - len(names))
		r.Body["session_attributes"] = names
	} else {
		// Legacy relations do not identify the contributing attribute.
		for _, name := range sessionAttributeNames {
			if !slices.Contains(policy.Attributes, name) {
				return false, 0
			}
		}
	}
	if !slices.Contains(policy.Attributes, "subyard.agent_run.id") {
		for _, field := range []string{"agent_run_id", "relation"} {
			if _, exists := r.Body[field]; exists {
				delete(r.Body, field)
				removed++
			}
		}
		r.Kind = "session"
	}
	if !slices.Contains(policy.Attributes, "subyard.agent_run.id") || !slices.Contains(policy.Attributes, "subyard.agent_run.evidence") {
		const source = "native-otlp-attribute"
		if old, exists := r.Body["source"]; exists && old != source {
			removed++
		}
		r.Body["source"] = source
	}
	return true, removed
}
