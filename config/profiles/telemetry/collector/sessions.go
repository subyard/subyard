package collector

import (
	"context"
	"encoding/json"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

func sessionAttributes(attributes pcommon.Map, sessions map[string]bool, p Policy) {
	for _, key := range []string{"session.id", "session_id", "conversation.id", "conversation_id", "thread.id"} {
		if value, ok := attributes.Get(key); ok && value.Type() == pcommon.ValueTypeStr && validID(value.Str()) {
			for _, allowed := range p.Attributes {
				if allowed == key {
					sessions[value.Str()] = true
					break
				}
			}
		}
	}
}
func metricSessions(metric pmetric.Metric, sessions map[string]bool, policy Policy) {
	switch metric.Type() {
	case pmetric.MetricTypeGauge:
		p := metric.Gauge().DataPoints()
		for i := 0; i < p.Len(); i++ {
			sessionAttributes(p.At(i).Attributes(), sessions, policy)
		}
	case pmetric.MetricTypeSum:
		p := metric.Sum().DataPoints()
		for i := 0; i < p.Len(); i++ {
			sessionAttributes(p.At(i).Attributes(), sessions, policy)
		}
	case pmetric.MetricTypeHistogram:
		p := metric.Histogram().DataPoints()
		for i := 0; i < p.Len(); i++ {
			sessionAttributes(p.At(i).Attributes(), sessions, policy)
		}
	case pmetric.MetricTypeExponentialHistogram:
		p := metric.ExponentialHistogram().DataPoints()
		for i := 0; i < p.Len(); i++ {
			sessionAttributes(p.At(i).Attributes(), sessions, policy)
		}
	case pmetric.MetricTypeSummary:
		p := metric.Summary().DataPoints()
		for i := 0; i < p.Len(); i++ {
			sessionAttributes(p.At(i).Attributes(), sessions, policy)
		}
	}
}
func sessionEnvelopes(ctx context.Context, sessions map[string]bool) []Envelope {
	entries := []Envelope{}
	run, _ := ctx.Value(runContextKey{}).(string)
	for session := range sessions {
		kind := "session"
		body := map[string]any{"session_id": session, "quality": "explicit", "source": "native-otlp-attribute"}
		if run != "" {
			kind = "relation"
			body["agent_run_id"] = run
			body["relation"] = "native_session"
			if evidence, ok := ctx.Value(runEvidenceKey{}).(string); ok {
				body["source"] = "native-otlp-attribute/" + evidence
			}
		}
		raw, _ := json.Marshal(body)
		entries = append(entries, Envelope{SourceID: "collector:otlp-correlation", SourceVersion: "1", SourceTime: time.Now().UTC().Format(timestampLayout), Kind: kind, Encoding: "json", PolicyVersion: PolicyVersion, Body: raw})
	}
	return entries
}
