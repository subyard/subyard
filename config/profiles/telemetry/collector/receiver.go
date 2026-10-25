package collector

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componentstatus"
	"go.opentelemetry.io/collector/config/configauth"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/config/configmiddleware"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/extension/extensionmiddleware"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/otlpreceiver"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Options struct {
	Listen, AuthFile, Spool string
	Policy                  Policy
	Receiver                ReceiverFunc
}
type LocalAuth = localAuth
type ReceiverFunc func(context.Context, *Store, Options, *LocalAuth) error
type runContextKey struct{}
type localAuth struct {
	store *Store
	token string
	slots chan struct{}
}

func (a *localAuth) Start(context.Context, component.Host) error { return nil }
func (a *localAuth) Shutdown(context.Context) error              { return nil }
func header(sources map[string][]string, key string) []string {
	for k, v := range sources {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return nil
}
func (a *localAuth) Authenticate(ctx context.Context, sources map[string][]string) (context.Context, error) {
	values := header(sources, "authorization")
	if len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), []byte("Bearer "+a.token)) != 1 {
		return ctx, errors.New("telemetry authentication required")
	}
	runs := header(sources, "x-subyard-agent-run-id")
	if len(runs) > 1 {
		return ctx, errors.New("invalid run context")
	}
	if len(runs) == 1 {
		if !validID(runs[0]) {
			return ctx, errors.New("invalid run context")
		}
		ctx = context.WithValue(ctx, runContextKey{}, runs[0])
	}
	return ctx, nil
}
func (a *localAuth) GetHTTPHandler(context.Context) (extensionmiddleware.WrapHTTPHandlerFunc, error) {
	return func(_ context.Context, next http.Handler) (http.Handler, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case a.slots <- struct{}{}:
				defer func() { <-a.slots }()
				if run, ok := r.Context().Value(runContextKey{}).(string); ok && !a.store.RegisteredRun(r.Context(), run) {
					w.Header().Set("Retry-After", "1")
					http.Error(w, "run registration pending", http.StatusServiceUnavailable)
					return
				}
				next.ServeHTTP(w, r)
			default:
				w.Header().Set("Retry-After", "1")
				http.Error(w, "telemetry capacity exceeded", http.StatusServiceUnavailable)
			}
		}), nil
	}, nil
}
func readToken(path string) (string, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		return "", errors.New("auth file must be regular with mode 0600")
	}
	if owner, ok := st.Sys().(*syscall.Stat_t); !ok || int(owner.Uid) != os.Geteuid() || owner.Nlink != 1 || st.Size() > 1024 {
		return "", errors.New("unsafe auth file ownership, links or size")
	}
	data, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if len(token) < 32 || len(token) > 512 || strings.ContainsAny(token, " \r\n\t") {
		return "", errors.New("invalid auth file")
	}
	return token, nil
}
func validateListen(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("OTLP listener must be loopback")
		}
	}
	return nil
}
func receiverConfig(options Options, limit int64) *otlpreceiver.Config {
	cfg := otlpreceiver.NewFactory().CreateDefaultConfig().(*otlpreceiver.Config)
	cfg.Protocols.GRPC = configoptional.None[configgrpc.ServerConfig]()
	h := cfg.Protocols.HTTP.GetOrInsertDefault()
	h.ServerConfig.NetAddr.Endpoint = options.Listen
	h.ServerConfig.ReadHeaderTimeout = 5 * time.Second
	h.ServerConfig.ReadTimeout = 15 * time.Second
	h.ServerConfig.WriteTimeout = 30 * time.Second
	h.ServerConfig.MaxRequestBodySize = limit
	h.ServerConfig.IncludeMetadata = false
	h.ServerConfig.CompressionAlgorithms = []string{"", "gzip"}
	id := component.MustNewID("local")
	h.ServerConfig.Auth = configoptional.Some(confighttp.AuthConfig{Config: configauth.Config{AuthenticatorID: id}})
	h.ServerConfig.Middlewares = []configmiddleware.Config{{ID: id}}
	return cfg
}

type receiverHost struct {
	auth     *localAuth
	failures chan error
}

func (h *receiverHost) GetExtensions() map[component.ID]component.Component {
	return map[component.ID]component.Component{component.MustNewID("local"): h.auth}
}
func (h *receiverHost) Report(event *componentstatus.Event) {
	if event.Status() == componentstatus.StatusFatalError {
		select {
		case h.failures <- errors.New("OTLP server failed"):
		default:
		}
	}
}

type signalConsumers struct {
	logs    consumer.Logs
	metrics consumer.Metrics
	traces  consumer.Traces
}

type runEvidenceKey struct{}

func (s *Store) resourceContext(ctx context.Context, attributes pcommon.Map) (context.Context, error) {
	headerRun, _ := ctx.Value(runContextKey{}).(string)
	resourceRun := ""
	if value, ok := attributes.Get("subyard.agent_run.id"); ok {
		if value.Type() != pcommon.ValueTypeStr || !validID(value.Str()) {
			return ctx, status.Error(codes.InvalidArgument, "invalid resource run context")
		}
		resourceRun = value.Str()
	}
	if headerRun != "" && resourceRun != "" && headerRun != resourceRun {
		return ctx, status.Error(codes.InvalidArgument, "conflicting resource and header run context")
	}
	run := headerRun
	evidence := "authenticated-header"
	if run == "" {
		run = resourceRun
		evidence = "registered-resource-attribute"
	}
	attributes.Remove("subyard.agent_run.evidence")
	if run != "" {
		if !s.RegisteredRun(ctx, run) {
			return ctx, status.Error(codes.Unavailable, "run registration pending")
		}
		attributes.PutStr("subyard.agent_run.id", run)
		attributes.PutStr("subyard.agent_run.evidence", evidence)
		ctx = context.WithValue(ctx, runContextKey{}, run)
		ctx = context.WithValue(ctx, runEvidenceKey{}, evidence)
	}
	return ctx, nil
}
func makeConsumers(s *Store, p Policy) (signalConsumers, error) {
	submit := func(ctx context.Context, kind string, data []byte, relations []Envelope) error {
		e := Envelope{Kind: "otlp/" + kind, Encoding: "otlp/" + kind, SourceID: "native-otlp", SourceVersion: "collector-pdata/1.69.0", Protobuf: data}
		if err := SanitizeEnvelope(&e, p); err != nil {
			return status.Error(codes.InvalidArgument, "invalid telemetry data")
		}
		entries := append([]Envelope{e}, relations...)
		err := s.submit(ctx, entries)
		if errors.Is(err, ErrCapacity) {
			return status.Error(codes.ResourceExhausted, ErrCapacity.Error())
		}
		if err != nil {
			return status.Error(codes.Unavailable, ErrStorage.Error())
		}
		return nil
	}
	logs, err := consumer.NewLogs(func(ctx context.Context, data plog.Logs) error {
		relations := []Envelope{}
		for i := 0; i < data.ResourceLogs().Len(); i++ {
			resource := data.ResourceLogs().At(i)
			resourceCtx, contextErr := s.resourceContext(ctx, resource.Resource().Attributes())
			if contextErr != nil {
				return contextErr
			}
			sessions := map[string]bool{}
			sessionAttributes(resource.Resource().Attributes(), sessions, p)
			for j := 0; j < resource.ScopeLogs().Len(); j++ {
				scope := resource.ScopeLogs().At(j)
				sessionAttributes(scope.Scope().Attributes(), sessions, p)
				for k := 0; k < scope.LogRecords().Len(); k++ {
					sessionAttributes(scope.LogRecords().At(k).Attributes(), sessions, p)
				}
			}
			relations = append(relations, sessionEnvelopes(resourceCtx, sessions)...)
		}
		raw, err := plogotlp.NewExportRequestFromLogs(data).MarshalProto()
		if err != nil {
			return err
		}
		return submit(ctx, "logs", raw, relations)
	})
	if err != nil {
		return signalConsumers{}, err
	}
	metrics, err := consumer.NewMetrics(func(ctx context.Context, data pmetric.Metrics) error {
		relations := []Envelope{}
		for i := 0; i < data.ResourceMetrics().Len(); i++ {
			resource := data.ResourceMetrics().At(i)
			resourceCtx, contextErr := s.resourceContext(ctx, resource.Resource().Attributes())
			if contextErr != nil {
				return contextErr
			}
			sessions := map[string]bool{}
			sessionAttributes(resource.Resource().Attributes(), sessions, p)
			for j := 0; j < resource.ScopeMetrics().Len(); j++ {
				scope := resource.ScopeMetrics().At(j)
				sessionAttributes(scope.Scope().Attributes(), sessions, p)
				for k := 0; k < scope.Metrics().Len(); k++ {
					metricSessions(scope.Metrics().At(k), sessions, p)
				}
			}
			relations = append(relations, sessionEnvelopes(resourceCtx, sessions)...)
		}
		raw, err := pmetricotlp.NewExportRequestFromMetrics(data).MarshalProto()
		if err != nil {
			return err
		}
		return submit(ctx, "metrics", raw, relations)
	})
	if err != nil {
		return signalConsumers{}, err
	}
	traces, err := consumer.NewTraces(func(ctx context.Context, data ptrace.Traces) error {
		relations := []Envelope{}
		for i := 0; i < data.ResourceSpans().Len(); i++ {
			resource := data.ResourceSpans().At(i)
			resourceCtx, contextErr := s.resourceContext(ctx, resource.Resource().Attributes())
			if contextErr != nil {
				return contextErr
			}
			sessions := map[string]bool{}
			sessionAttributes(resource.Resource().Attributes(), sessions, p)
			for j := 0; j < resource.ScopeSpans().Len(); j++ {
				scope := resource.ScopeSpans().At(j)
				sessionAttributes(scope.Scope().Attributes(), sessions, p)
				for k := 0; k < scope.Spans().Len(); k++ {
					sessionAttributes(scope.Spans().At(k).Attributes(), sessions, p)
				}
			}
			relations = append(relations, sessionEnvelopes(resourceCtx, sessions)...)
		}
		raw, err := ptraceotlp.NewExportRequestFromTraces(data).MarshalProto()
		if err != nil {
			return err
		}
		return submit(ctx, "traces", raw, relations)
	})
	return signalConsumers{logs, metrics, traces}, err
}
func serveOTLP(ctx context.Context, s *Store, options Options, auth *localAuth) error {
	if err := validateListen(options.Listen); err != nil {
		return err
	}
	consumers, err := makeConsumers(s, options.Policy)
	if err != nil {
		return err
	}
	factory := otlpreceiver.NewFactory()
	cfg := receiverConfig(options, s.limits.RequestBytes)
	settings := receiver.Settings{ID: component.MustNewID("otlp"), TelemetrySettings: component.TelemetrySettings{Logger: zap.NewNop(), TracerProvider: tracenoop.NewTracerProvider(), MeterProvider: noop.NewMeterProvider(), Resource: pcommon.NewResource()}}
	traces, err := factory.CreateTraces(ctx, settings, cfg, consumers.traces)
	if err != nil {
		return err
	}
	metrics, err := factory.CreateMetrics(ctx, settings, cfg, consumers.metrics)
	if err != nil {
		return err
	}
	logs, err := factory.CreateLogs(ctx, settings, cfg, consumers.logs)
	if err != nil {
		return err
	}
	host := &receiverHost{auth, make(chan error, 1)}
	components := []component.Component{traces, metrics, logs}
	for _, c := range components {
		if err = c.Start(ctx, host); err != nil {
			return fmt.Errorf("start OTLP: %w", err)
		}
	}
	select {
	case <-ctx.Done():
	case err = <-host.failures:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, c := range components {
		err = errors.Join(err, c.Shutdown(shutdown))
	}
	return err
}

func (s *Store) Consumers(p Policy) (consumer.Logs, consumer.Metrics, consumer.Traces, error) {
	c, err := makeConsumers(s, p)
	return c.logs, c.metrics, c.traces, err
}
