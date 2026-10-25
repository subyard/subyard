package collector

import (
	"bytes"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
)

func TestOTLPMetricUnitsSurviveCapturePolicy(t *testing.T) {
	units := []string{"%", "{token}", "{request}", "{connection}/s", "[degF]", "mm[Hg]", "s{CPU}", "10*3", "kg.m/s2", "(m/s)/s", "s", "ms", "By", "By/s", "1", "1/s", "Cel", "m/s2", strings.Repeat("a", 160)}
	kinds := []pmetric.MetricType{pmetric.MetricTypeGauge, pmetric.MetricTypeSum, pmetric.MetricTypeHistogram, pmetric.MetricTypeExponentialHistogram, pmetric.MetricTypeSummary}
	for _, kind := range kinds {
		t.Run(kind.String(), func(t *testing.T) {
			for _, unit := range units {
				r := unitMetric(t, kind, unit)
				raw, err := r.MarshalProto()
				if err != nil {
					t.Fatal(err)
				}
				e := Envelope{Encoding: "otlp/metrics", Protobuf: raw}
				if err := SanitizeEnvelope(&e, DefaultPolicy()); err != nil {
					t.Fatal(err)
				}
				clean := pmetricotlp.NewExportRequest()
				if err := clean.UnmarshalProto(e.Protobuf); err != nil {
					t.Fatal(err)
				}
				metric := clean.Metrics().ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0)
				if metric.Unit() != unit || metric.Type() != kind || clean.Metrics().DataPointCount() != 1 {
					t.Fatalf("unit/type/point changed: unit=%q got=%q kind=%s", unit, metric.Unit(), metric.Type())
				}
				if bytes.Contains(e.Protobuf, []byte("PRIVATE")) || bytes.Contains(e.Protobuf, []byte("private-operation")) || e.Redactions == 0 {
					t.Fatal("unit syntax allowance widened sensitive metadata or name policy")
				}
			}
		})
	}
}

func TestOTLPMetricUnitBoundsAndOtherStringPolicies(t *testing.T) {
	for _, unit := range []string{"contains space", "line\nbreak", "\x00", "\x1f", "\x7f", "µs", strings.Repeat("a", 161)} {
		r := unitMetric(t, pmetric.MetricTypeGauge, unit)
		raw, err := r.MarshalProto()
		if err != nil {
			t.Fatal(err)
		}
		e := Envelope{Encoding: "otlp/metrics", Protobuf: raw}
		if err := SanitizeEnvelope(&e, DefaultPolicy()); err != nil {
			t.Fatal(err)
		}
		clean := pmetricotlp.NewExportRequest()
		if err := clean.UnmarshalProto(e.Protobuf); err != nil {
			t.Fatal(err)
		}
		if got := clean.Metrics().ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Unit(); got != "" {
			t.Fatalf("unsafe/overbound unit survived: %q", got)
		}
	}
	// The new syntax applies only to units; arbitrary names and schema URLs
	// retain their existing restrictions.
	clean, _, err := sanitizeJSON([]byte(`{"unit":"{request}","severityText":"{request}","name":"{request}","schemaUrl":"https://example.invalid/schema"}`), DefaultPolicy())
	if err != nil || !bytes.Contains(clean, []byte(`"unit":"{request}"`)) || bytes.Contains(clean, []byte(`"severityText":"{request}"`)) || bytes.Contains(clean, []byte(`"name":"{request}"`)) || bytes.Contains(clean, []byte("example.invalid")) {
		t.Fatalf("other string policy changed: %s %v", clean, err)
	}
}

func unitMetric(t *testing.T, kind pmetric.MetricType, unit string) pmetricotlp.ExportRequest {
	t.Helper()
	r := pmetricotlp.NewExportRequest()
	resource := r.Metrics().ResourceMetrics().AppendEmpty()
	resource.Resource().Attributes().PutStr("Authorization", "PRIVATE")
	metric := resource.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	metric.SetName("private-operation with spaces")
	metric.SetDescription("PRIVATE")
	metric.SetUnit(unit)
	switch kind {
	case pmetric.MetricTypeGauge:
		metric.SetEmptyGauge().DataPoints().AppendEmpty().Attributes().PutStr("tool.arguments", "PRIVATE")
	case pmetric.MetricTypeSum:
		metric.SetEmptySum().DataPoints().AppendEmpty().Attributes().PutStr("tool.arguments", "PRIVATE")
	case pmetric.MetricTypeHistogram:
		metric.SetEmptyHistogram().DataPoints().AppendEmpty().Attributes().PutStr("tool.arguments", "PRIVATE")
	case pmetric.MetricTypeExponentialHistogram:
		metric.SetEmptyExponentialHistogram().DataPoints().AppendEmpty().Attributes().PutStr("tool.arguments", "PRIVATE")
	case pmetric.MetricTypeSummary:
		metric.SetEmptySummary().DataPoints().AppendEmpty().Attributes().PutStr("tool.arguments", "PRIVATE")
	default:
		t.Fatal("unsupported fixture metric type")
	}
	return r
}
