package minimal

import (
	"context"
	"fmt"
	"github.com/Subyard/Subyard/config/profiles/telemetry/collector"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/confmap/provider/yamlprovider"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/otlpreceiver"
	"go.opentelemetry.io/collector/service/telemetry"
)

type logExporter struct{ consumer.Logs }

func (logExporter) Start(context.Context, component.Host) error { return nil }
func (logExporter) Shutdown(context.Context) error              { return nil }

type metricExporter struct{ consumer.Metrics }

func (metricExporter) Start(context.Context, component.Host) error { return nil }
func (metricExporter) Shutdown(context.Context) error              { return nil }

type traceExporter struct{ consumer.Traces }

func (traceExporter) Start(context.Context, component.Host) error { return nil }
func (traceExporter) Shutdown(context.Context) error              { return nil }

// The comparison changes lifecycle/pipeline wiring only. Both modes call the
// exact same consumers, capture policy and durable writer.
func Serve(ctx context.Context, s *collector.Store, options collector.Options, auth *collector.LocalAuth) error {
	logs, metrics, traces, err := s.Consumers(options.Policy)
	if err != nil {
		return err
	}
	exp := exporter.NewFactory(component.MustNewType("sqlite"), func() component.Config { return &struct{}{} }, exporter.WithLogs(func(context.Context, exporter.Settings, component.Config) (exporter.Logs, error) {
		return logExporter{logs}, nil
	}, component.StabilityLevelDevelopment), exporter.WithMetrics(func(context.Context, exporter.Settings, component.Config) (exporter.Metrics, error) {
		return metricExporter{metrics}, nil
	}, component.StabilityLevelDevelopment), exporter.WithTraces(func(context.Context, exporter.Settings, component.Config) (exporter.Traces, error) {
		return traceExporter{traces}, nil
	}, component.StabilityLevelDevelopment))
	ext := extension.NewFactory(component.MustNewType("local"), func() component.Config { return &struct{}{} }, func(context.Context, extension.Settings, component.Config) (extension.Extension, error) {
		return auth, nil
	}, component.StabilityLevelDevelopment)
	config := fmt.Sprintf(`extensions:
  local: {}
receivers:
  otlp:
    protocols:
      http:
        endpoint: %q
        auth: {authenticator: local}
        middlewares: [{id: local}]
        include_metadata: false
        compression_algorithms: ["", gzip]
        max_request_body_size: %d
        read_header_timeout: 5s
        read_timeout: 15s
        write_timeout: 30s
exporters:
  sqlite: {}
service:
  extensions: [local]
  pipelines:
    logs: {receivers: [otlp], exporters: [sqlite]}
    metrics: {receivers: [otlp], exporters: [sqlite]}
    traces: {receivers: [otlp], exporters: [sqlite]}
`, options.Listen, s.Limits().RequestBytes)
	col, err := otelcol.NewCollector(otelcol.CollectorSettings{Factories: func() (otelcol.Factories, error) {
		return otelcol.Factories{Receivers: map[component.Type]receiver.Factory{component.MustNewType("otlp"): otlpreceiver.NewFactory()}, Exporters: map[component.Type]exporter.Factory{exp.Type(): exp}, Extensions: map[component.Type]extension.Factory{ext.Type(): ext}, Telemetry: telemetry.NewFactory(func() component.Config { return &struct{}{} })}, nil
	}, DisableGracefulShutdown: true, ConfigProviderSettings: otelcol.ConfigProviderSettings{ResolverSettings: confmap.ResolverSettings{URIs: []string{"yaml:" + config}, ProviderFactories: []confmap.ProviderFactory{yamlprovider.NewFactory()}}}})
	if err != nil {
		return err
	}
	return col.Run(ctx)
}
