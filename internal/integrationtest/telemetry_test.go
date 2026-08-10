package integrationtest

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestSuiteSeparatesConcurrentTestTelemetry(t *testing.T) {
	suite, recorder, _, logs := newInstrumentedSuite(t)
	first := suite.setupTest(t, "TestFirst")
	second := suite.setupTest(t, "TestSecond")
	firstRoot := trace.SpanContextFromContext(first.Context)
	secondRoot := trace.SpanContextFromContext(second.Context)
	if firstRoot.TraceID() == secondRoot.TraceID() {
		t.Fatal("test roots share a trace ID")
	}

	server := newTestServer(t)
	tests := []*Test{first, second}
	errorsByTest := make(chan error, len(tests))
	var wait sync.WaitGroup
	for _, test := range tests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsByTest <- test.Step(test.Context, "test.observe.http", func(ctx context.Context) error {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, http.NoBody)
				if err != nil {
					return err
				}
				resp, err := test.HTTPClient.Do(req)
				if err != nil {
					return err
				}
				return resp.Body.Close()
			})
		}()
	}

	wait.Wait()
	close(errorsByTest)
	for err := range errorsByTest {
		if err != nil {
			t.Fatal(err)
		}
	}

	wantName := map[trace.TraceID]string{firstRoot.TraceID(): "TestFirst", secondRoot.TraceID(): "TestSecond"}
	clientSpans := 0
	for _, span := range recorder.Ended() {
		name, ok := wantName[span.SpanContext().TraceID()]
		if !ok {
			continue
		}
		assertIdentity(t, span.Attributes(), name)
		if span.SpanKind() == trace.SpanKindClient {
			clientSpans++
			if span.Parent().TraceID() != span.SpanContext().TraceID() {
				t.Fatal("HTTP span escaped its test trace")
			}
		}
	}

	if clientSpans != 4 {
		t.Fatalf("HTTP client spans = %d, want 4 readiness and observed requests", clientSpans)
	}

	if output := logs.String(); !strings.Contains(output, `test.name=TestFirst`) || !strings.Contains(output, `test.name=TestSecond`) {
		t.Fatalf("logs lack test identities: %s", output)
	}
}

func TestSuiteIntegrationMetricsIncludeIdentity(t *testing.T) {
	suite, _, reader, logs := newInstrumentedSuite(t)
	const name = "TestMetricIdentity"
	t.Run("records", func(t *testing.T) {
		test := suite.setupTest(t, name)
		test.Logger.InfoContext(test.Context, "identity log")
	})

	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatal(err)
	}

	seen := 0
	for _, scope := range metrics.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			if instrument.Name != "integration.test.count" && instrument.Name != "integration.test.duration" {
				continue
			}
			switch data := instrument.Data.(type) {
			case metricdata.Sum[int64]:
				assertMetricIdentity(t, data.DataPoints[0].Attributes, name)
				seen++
			case metricdata.Histogram[float64]:
				assertMetricIdentity(t, data.DataPoints[0].Attributes, name)
				seen++
			}
		}
	}

	if seen != 2 {
		t.Fatalf("identity metrics = %d, want 2", seen)
	}

	if output := logs.String(); !strings.Contains(output, `test.name=TestMetricIdentity`) || !strings.Contains(output, "identity log") {
		t.Fatalf("identity log missing fields: %s", output)
	}
}

func TestOperationRecordsErrorAndCompletesOnce(t *testing.T) {
	test, recorder, rootCtx, finishRoot := newInstrumentedTest(t)
	wantErr := errors.New("operation failed")
	_, finish := test.StartOperation(rootCtx, "test.action.failure")

	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			finish(wantErr)
		}()
	}
	wait.Wait()
	finishRoot()

	operation := namedSpan(t, recorder.Ended(), "test.action.failure")
	if operation.Status().Code != codes.Error || len(operation.Events()) != 1 {
		t.Fatalf("operation status/events = %v/%v, want one recorded error", operation.Status(), operation.Events())
	}

	if countNamedSpans(recorder.Ended(), "test.action.failure") != 1 {
		t.Fatal("operation completed more than once")
	}
}

func TestCleanupOperationRetainsRootAfterCancellation(t *testing.T) {
	test, recorder, rootCtx, finishRoot := newInstrumentedTest(t)
	server := newTestServer(t)
	client := testHTTPClient(test)
	sourceCtx, cancel := context.WithCancel(rootCtx)
	cancel()

	cleanupCtx := context.WithoutCancel(sourceCtx)
	err := test.Step(cleanupCtx, "test.fixture.account.delete", func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, server.URL, http.NoBody)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	})
	if err != nil {
		t.Fatal(err)
	}

	finishRoot()
	assertSpanChain(t, recorder.Ended(), "test.root", "test.fixture.account.delete")
}

func TestSuiteCloseIsIdempotentAndRunsAfterRoots(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	_, root := provider.Tracer(testScope).Start(context.Background(), "test.root")
	root.End()

	var calls atomic.Int32
	suite := &Suite{close: func() error {
		calls.Add(1)
		if countNamedSpans(recorder.Ended(), "test.root") != 1 {
			return errors.New("root was not ended before suite shutdown")
		}
		return provider.Shutdown(context.Background())
	}}

	if err := suite.Close(); err != nil {
		t.Fatal(err)
	}

	if err := suite.Close(); err != nil {
		t.Fatal(err)
	}

	if calls.Load() != 1 {
		t.Fatalf("close calls = %d, want 1", calls.Load())
	}
}

func TestConfiguredPDSValidation(t *testing.T) {
	t.Setenv("PDS_TEST_URL", "https://example.test/path")
	if _, err := configuredPDS(); err == nil {
		t.Fatal("PDS path should be rejected")
	}
}

func newInstrumentedSuite(t *testing.T) (*Suite, *tracetest.SpanRecorder, *sdkmetric.ManualReader, *bytes.Buffer) {
	t.Helper()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(&identitySpanProcessor{}),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down tracer provider: %v", err)
		}
	})

	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := meterProvider.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down meter provider: %v", err)
		}
	})

	meter := meterProvider.Meter(testScope)
	count, err := meter.Int64Counter("integration.test.count")
	if err != nil {
		t.Fatal(err)
	}

	duration, err := meter.Float64Histogram("integration.test.duration")
	if err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport has type %T", http.DefaultTransport)
	}
	base := defaultTransport.Clone()
	t.Cleanup(base.CloseIdleConnections)

	propagator := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
	suite := &Suite{
		tracerProvider: provider,
		meterProvider:  meterProvider,
		propagator:     propagator,
		logger:         slog.New(slog.NewTextHandler(&logs, nil)),
		tracer:         provider.Tracer(testScope),
		count:          count,
		duration:       duration,
		transport: &identityRoundTripper{next: otelhttp.NewTransport(base,
			otelhttp.WithTracerProvider(provider),
			otelhttp.WithMeterProvider(meterProvider),
			otelhttp.WithPropagators(propagator),
		)},
		pds:           "http://unused.test",
		adminPassword: "test-admin",
	}

	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(health.Close)

	suite.pds = health.URL
	return suite, recorder, reader, &logs
}

//nolint:spancheck // Root ownership is transferred to the idempotent cleanup returned below.
func newInstrumentedTest(t *testing.T) (*Test, *tracetest.SpanRecorder, context.Context, func()) {
	t.Helper()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(&identitySpanProcessor{}), sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down tracer provider: %v", err)
		}
	})

	tracer := provider.Tracer(testScope)
	ctx := context.WithValue(context.Background(), testIdentityKey{}, testIdentity("TestCleanup"))
	rootCtx, root := tracer.Start(ctx, "test.root")

	var once sync.Once
	finishRoot := func() { once.Do(func() { root.End() }) }

	t.Cleanup(finishRoot)

	return &Test{tracer: tracer, traceProvider: provider}, recorder, rootCtx, finishRoot
}

func testHTTPClient(test *Test) *http.Client {
	return &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport, otelhttp.WithTracerProvider(test.traceProvider))}
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(server.Close)
	return server
}

func assertIdentity(t *testing.T, attrs []attribute.KeyValue, name string) {
	t.Helper()

	set := attribute.NewSet(attrs...)
	value, ok := set.Value("test.name")
	if !ok || value.AsString() != name {
		t.Fatalf("test.name = %v, want %q", value, name)
	}

	value, ok = set.Value("test.name.fingerprint")
	if !ok || value.AsString() != testNameFingerprint(name) {
		t.Fatalf("test.name.fingerprint = %v, want %q", value, testNameFingerprint(name))
	}
}

func assertMetricIdentity(t *testing.T, attrs attribute.Set, name string) {
	t.Helper()

	value, ok := attrs.Value("test.name")
	if !ok || value.AsString() != name {
		t.Fatalf("metric test.name = %v, want %q", value, name)
	}

	if outcome, ok := attrs.Value("outcome"); !ok || outcome.AsString() != "success" {
		t.Fatalf("metric outcome = %v, want success", outcome)
	}
}

func assertSpanChain(t *testing.T, spans []sdktrace.ReadOnlySpan, rootName, operationName string) {
	t.Helper()

	root := namedSpan(t, spans, rootName)
	operation := namedSpan(t, spans, operationName)
	if operation.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Fatal("operation is not parented to root")
	}

	for _, span := range spans {
		if span.SpanKind() == trace.SpanKindClient {
			if span.Parent().SpanID() != operation.SpanContext().SpanID() || span.SpanContext().TraceID() != root.SpanContext().TraceID() {
				t.Fatal("HTTP span is not a child of the cleanup operation in the root trace")
			}
			return
		}
	}

	t.Fatal("HTTP span not found")
}

func namedSpan(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()

	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}

	t.Fatalf("span %q not found", name)
	return nil
}

func countNamedSpans(spans []sdktrace.ReadOnlySpan, name string) int {
	count := 0
	for _, span := range spans {
		if span.Name() == name {
			count++
		}
	}

	return count
}
