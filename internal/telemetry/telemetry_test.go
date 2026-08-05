package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	logglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type logExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (e *logExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range records {
		e.records = append(e.records, records[i].Clone())
	}
	return nil
}

func (*logExporter) Shutdown(context.Context) error   { return nil }
func (*logExporter) ForceFlush(context.Context) error { return nil }

func TestFanoutCorrelatesStdoutAndPreservesIntrinsicLogContext(t *testing.T) {
	var stdout bytes.Buffer
	exporter := &logExporter{}
	loggerProvider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	t.Cleanup(func() {
		if err := loggerProvider.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down logger provider: %v", err)
		}
	})
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() {
		if err := tracerProvider.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down tracer provider: %v", err)
		}
	})

	handler := newFanoutHandler(
		slog.NewJSONHandler(&stdout, nil),
		otelslog.NewHandler("test", otelslog.WithLoggerProvider(loggerProvider)),
	)
	ctx, span := tracerProvider.Tracer("test").Start(context.Background(), "operation")
	slog.New(handler).InfoContext(ctx, "correlated", "key", "value")
	spanContext := span.SpanContext()
	span.End()

	line := stdout.String()
	if !strings.Contains(line, `"trace_id":"`+spanContext.TraceID().String()+`"`) ||
		!strings.Contains(line, `"span_id":"`+spanContext.SpanID().String()+`"`) {
		t.Fatalf("stdout log is not correlated: %s", line)
	}
	if len(exporter.records) != 1 {
		t.Fatalf("exported %d records, want 1", len(exporter.records))
	}
	record := exporter.records[0]
	if record.TraceID() != spanContext.TraceID() || record.SpanID() != spanContext.SpanID() {
		t.Fatalf("OTLP context = %s/%s, want %s/%s", record.TraceID(), record.SpanID(), spanContext.TraceID(), spanContext.SpanID())
	}
	record.WalkAttributes(func(attr attribute.KeyValue) bool {
		if attr.Key == "trace_id" || attr.Key == "span_id" {
			t.Fatalf("correlation ID duplicated as OTLP attribute: %s", attr.Key)
		}
		return true
	})
}

func TestLoggerUsesScopeAndPreservesTypedOTLPAttributes(t *testing.T) {
	var stdout bytes.Buffer
	exporter := &logExporter{}
	loggerProvider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	t.Cleanup(func() {
		if err := loggerProvider.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down logger provider: %v", err)
		}
	})
	telemetry := &Telemetry{
		stdout:         slog.NewTextHandler(&stdout, nil),
		loggerProvider: loggerProvider,
	}

	telemetry.Logger("example/application").Info("created item", "count", 3, "cached", true)

	if len(exporter.records) != 1 {
		t.Fatalf("exported %d records, want 1", len(exporter.records))
	}
	record := exporter.records[0]
	if got := record.InstrumentationScope().Name; got != "example/application" {
		t.Fatalf("scope = %q, want example/application", got)
	}
	if got := record.Body().AsString(); got != "created item" {
		t.Fatalf("body = %q, want created item", got)
	}
	attrs := make(map[string]attribute.Value)
	record.WalkAttributes(func(attr attribute.KeyValue) bool {
		attrs[string(attr.Key)] = attr.Value
		return true
	})
	if got := attrs["count"].AsInt64(); got != 3 {
		t.Fatalf("count = %d, want 3", got)
	}
	if got := attrs["cached"].AsBool(); !got {
		t.Fatal("cached = false, want true")
	}
	if line := stdout.String(); !strings.Contains(line, "msg=\"created item\"") || !strings.Contains(line, "count=3") {
		t.Fatalf("stdout is not text output: %s", line)
	}
}

func TestDisabledProviderRetainsStdoutOnly(t *testing.T) {
	clearOTLPEnv(t)
	var stdout bytes.Buffer
	telemetry, err := New(context.Background(), Config{
		ServiceName: "test",
		Stdout:      slog.NewTextHandler(&stdout, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	telemetry.Logger("example/application").Info("local only")
	if !strings.Contains(stdout.String(), "local only") {
		t.Fatalf("missing stdout log: %s", stdout.String())
	}
	if err := telemetry.Shutdown(); err != nil {
		t.Fatal(err)
	}
}

func TestNewDoesNotReplaceGlobalTelemetry(t *testing.T) {
	clearOTLPEnv(t)
	tracerProvider := otel.GetTracerProvider()
	meterProvider := otel.GetMeterProvider()
	loggerProvider := logglobal.GetLoggerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(previousPropagator) })

	telemetry, err := New(context.Background(), Config{
		ServiceName: "test",
		Stdout:      slog.NewTextHandler(&bytes.Buffer{}, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := telemetry.Shutdown(); err != nil {
			t.Errorf("shut down telemetry: %v", err)
		}
	})

	if otel.GetTracerProvider() != tracerProvider {
		t.Fatal("New replaced the global tracer provider")
	}
	if otel.GetMeterProvider() != meterProvider {
		t.Fatal("New replaced the global meter provider")
	}
	if logglobal.GetLoggerProvider() != loggerProvider {
		t.Fatal("New replaced the global logger provider")
	}
	if _, ok := otel.GetTextMapPropagator().(propagation.TraceContext); !ok {
		t.Fatal("New replaced the global propagator")
	}
}

func TestNewProvidesTraceContextAndBaggagePropagation(t *testing.T) {
	clearOTLPEnv(t)
	telemetry, err := New(context.Background(), Config{
		ServiceName: "test",
		Stdout:      slog.NewTextHandler(&bytes.Buffer{}, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := telemetry.Shutdown(); err != nil {
			t.Errorf("shut down telemetry: %v", err)
		}
	})

	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1},
		SpanID:     trace.SpanID{2},
		TraceFlags: trace.FlagsSampled,
	})
	member, err := baggage.NewMember("tenant", "example")
	if err != nil {
		t.Fatal(err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatal(err)
	}
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)
	ctx = baggage.ContextWithBaggage(ctx, bag)
	carrier := propagation.MapCarrier{}
	telemetry.Propagator().Inject(ctx, carrier)

	if got := carrier.Get("traceparent"); got != "00-01000000000000000000000000000000-0200000000000000-01" {
		t.Fatalf("traceparent = %q", got)
	}
	if got := carrier.Get("baggage"); got != "tenant=example" {
		t.Fatalf("baggage = %q", got)
	}
}

func TestPartialSignalEndpointsFail(t *testing.T) {
	clearOTLPEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://localhost:4317")
	if _, err := otlpEnabled(); err == nil {
		t.Fatal("partial signal endpoints should fail")
	}
}

func TestOTLPEndpointValidation(t *testing.T) {
	clearOTLPEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	if _, err := otlpEnabled(); err == nil || !strings.Contains(err.Error(), "http://localhost:4317") {
		t.Fatalf("invalid endpoint error = %v, want actionable URL example", err)
	}

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4317")
	enabled, err := otlpEnabled()
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("valid endpoint should enable OTLP")
	}

	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "localhost:4317")
	if _, err := otlpEnabled(); err == nil {
		t.Fatal("invalid signal override should fail when the common endpoint is configured")
	}
}

func TestAllSignalEndpointsEnableOTLP(t *testing.T) {
	clearOTLPEnv(t)
	for _, name := range []string{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	} {
		t.Setenv(name, "http://localhost:4317")
	}
	enabled, err := otlpEnabled()
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("all signal-specific endpoints should enable OTLP")
	}
}

func TestShutdownIsBoundedAndIdempotent(t *testing.T) {
	var calls atomic.Int32
	telemetry := &Telemetry{
		timeout: 10 * time.Millisecond,
		shutdown: func(ctx context.Context) error {
			calls.Add(1)
			<-ctx.Done()
			return ctx.Err()
		},
	}
	if err := telemetry.Shutdown(); err == nil {
		t.Fatal("expected shutdown deadline error")
	}
	if err := telemetry.Shutdown(); err == nil {
		t.Fatal("expected cached shutdown error")
	}
	if calls.Load() != 1 {
		t.Fatalf("shutdown called %d times, want 1", calls.Load())
	}
}

func clearOTLPEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OTEL_SDK_DISABLED",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	} {
		t.Setenv(name, "")
	}
}
