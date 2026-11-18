package collector

import (
	"context"
	"encoding/json"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

var sessionAttributeNames = []string{"session.id", "session_id", "conversation.id", "conversation_id", "thread.id"}

func (p wirePolicy) sessionAttributes(attributes pcommon.Map, sessions map[string]bool) {
	for _, name := range sessionAttributeNames {
		if value, ok := attributes.Get(name); ok && value.Type() == pcommon.ValueTypeStr {
			if clean, keep := p.clean(p.attributeRule(name), value.Str()); keep {
				sessions[clean.(string)] = true
			}
		}
	}
}

func (p wirePolicy) metricSessions(metric pmetric.Metric, sessions map[string]bool) {
	switch metric.Type() {
	case pmetric.MetricTypeGauge:
		points := metric.Gauge().DataPoints()
		for i := 0; i < points.Len(); i++ {
			p.sessionAttributes(points.At(i).Attributes(), sessions)
		}
	case pmetric.MetricTypeSum:
		points := metric.Sum().DataPoints()
		for i := 0; i < points.Len(); i++ {
			p.sessionAttributes(points.At(i).Attributes(), sessions)
		}
	case pmetric.MetricTypeHistogram:
		points := metric.Histogram().DataPoints()
		for i := 0; i < points.Len(); i++ {
			p.sessionAttributes(points.At(i).Attributes(), sessions)
		}
	case pmetric.MetricTypeExponentialHistogram:
		points := metric.ExponentialHistogram().DataPoints()
		for i := 0; i < points.Len(); i++ {
			p.sessionAttributes(points.At(i).Attributes(), sessions)
		}
	case pmetric.MetricTypeSummary:
		points := metric.Summary().DataPoints()
		for i := 0; i < points.Len(); i++ {
			p.sessionAttributes(points.At(i).Attributes(), sessions)
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
