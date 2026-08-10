package integrationtest

import (
	"atproto-dev/internal/telemetry"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const testScope = "atproto-dev/test/integration"

var (
	runIDOnce       sync.Once
	runID           string
	revisionOnce    sync.Once
	revision        string
	instanceIDOnce  sync.Once
	instanceIDValue string
)

type testIdentityKey struct{}

// Suite owns process-wide integration telemetry and shared HTTP connections.
type Suite struct {
	observability  *telemetry.Telemetry
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider
	propagator     propagation.TextMapPropagator
	logger         *slog.Logger
	tracer         trace.Tracer
	count          metric.Int64Counter
	duration       metric.Float64Histogram
	transport      http.RoundTripper
	baseTransport  *http.Transport
	pds            string
	adminPassword  string
	close          func() error
	closeOnce      sync.Once
	closeErr       error
}

// Test owns one integration test's context, identity, root trace, and client.
type Test struct {
	Context    context.Context
	PDS        string
	HTTPClient *http.Client
	Logger     *slog.Logger

	tracer        trace.Tracer
	traceProvider trace.TracerProvider
	identity      []attribute.KeyValue
	adminPassword string
}

// NewSuite validates process configuration and creates process-wide telemetry.
func NewSuite(ctx context.Context) (*Suite, error) {
	pds, err := configuredPDS()
	if err != nil {
		return nil, err
	}

	adminPassword := strings.TrimSpace(os.Getenv("PDS_ADMIN_PASSWORD"))
	if adminPassword == "" {
		return nil, errors.New("PDS_ADMIN_PASSWORD is required to create and clean up integration accounts")
	}

	identityProcessor := &identitySpanProcessor{}
	observability, err := telemetry.New(ctx, telemetry.Config{
		ServiceName: "it",
		ResourceAttributes: []attribute.KeyValue{
			attribute.String("run_id", RunID()),
			attribute.String("vcs.revision", SourceRevision()),
			attribute.String("service.instance.id", instanceID()),
			attribute.String("test.suite.name", testScope),
		},
		SpanProcessors: []sdktrace.SpanProcessor{identityProcessor},
	})
	if err != nil {
		return nil, fmt.Errorf("initialize integration telemetry: %w", err)
	}

	meter := observability.MeterProvider().Meter(testScope)
	count, err := meter.Int64Counter("integration.test.count")
	if err != nil {
		//nolint:contextcheck // Telemetry shutdown intentionally owns a fresh bounded context.
		return nil, closeAfterInitializationFailure(observability, "create integration test counter", err)
	}

	duration, err := meter.Float64Histogram("integration.test.duration", metric.WithUnit("s"))
	if err != nil {
		//nolint:contextcheck // Telemetry shutdown intentionally owns a fresh bounded context.
		return nil, closeAfterInitializationFailure(observability, "create integration test duration", err)
	}

	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		//nolint:contextcheck // Telemetry shutdown intentionally owns a fresh bounded context.
		return nil, closeAfterInitializationFailure(observability, "create shared HTTP transport", errors.New("http.DefaultTransport has unexpected type"))
	}

	baseTransport := defaultTransport.Clone()
	instrumentedTransport := otelhttp.NewTransport(baseTransport,
		otelhttp.WithTracerProvider(observability.TracerProvider()),
		otelhttp.WithMeterProvider(observability.MeterProvider()),
		otelhttp.WithPropagators(observability.Propagator()),
	)

	transport := &identityRoundTripper{next: instrumentedTransport}
	suite := &Suite{
		observability:  observability,
		tracerProvider: observability.TracerProvider(),
		meterProvider:  observability.MeterProvider(),
		propagator:     observability.Propagator(),
		logger:         observability.Logger(testScope),
		tracer:         observability.TracerProvider().Tracer(testScope),
		count:          count,
		duration:       duration,
		transport:      transport,
		baseTransport:  baseTransport,
		pds:            pds,
		adminPassword:  adminPassword,
	}

	//nolint:contextcheck // Telemetry shutdown intentionally owns a fresh bounded context.
	suite.close = func() error {
		baseTransport.CloseIdleConnections()
		return observability.Shutdown()
	}
	return suite, nil
}

func closeAfterInitializationFailure(observability *telemetry.Telemetry, operation string, err error) error {
	return errors.Join(fmt.Errorf("%s: %w", operation, err), observability.Shutdown())
}

// Setup creates one test identity and root trace using the suite providers.
func (suite *Suite) Setup(t *testing.T) *Test {
	t.Helper()

	return suite.setupTest(t, t.Name())
}

func (suite *Suite) setupTest(t *testing.T, name string) *Test {
	t.Helper()

	identity := testIdentity(name)
	ctx := context.WithValue(context.Background(), testIdentityKey{}, identity)
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	t.Cleanup(cancel)

	ctx, span := suite.tracer.Start(ctx, name, trace.WithNewRoot())
	t.Cleanup(func() { span.End() })

	logger := suite.logger.With(
		"test.name", name,
		"test.name.fingerprint", testNameFingerprint(name),
	)
	started := time.Now()
	logger.InfoContext(ctx, "integration test started", "run_id", RunID())

	t.Cleanup(func() {
		outcome := "success"
		if t.Failed() {
			outcome = "failure"
			span.SetStatus(codes.Error, "test failed")
		}
		attrs := append([]attribute.KeyValue(nil), identity...)
		attrs = append(attrs, attribute.String("outcome", outcome))
		options := metric.WithAttributes(attrs...)
		suite.count.Add(ctx, 1, options)
		suite.duration.Record(ctx, time.Since(started).Seconds(), options)
		logger.InfoContext(ctx, "integration test ended", "run_id", RunID(), "outcome", outcome, "duration", time.Since(started))
	})

	test := &Test{
		Context:       ctx,
		PDS:           suite.pds,
		HTTPClient:    &http.Client{Timeout: requestTimeout, Transport: suite.transport},
		Logger:        logger,
		tracer:        suite.tracer,
		traceProvider: suite.tracerProvider,
		identity:      identity,
		adminPassword: suite.adminPassword,
	}
	requireHealthy(t, test)

	return test
}

// Close shuts down suite-owned resources exactly once and caches the result.
func (suite *Suite) Close() error {
	suite.closeOnce.Do(func() {
		if suite.close != nil {
			suite.closeErr = suite.close()
		}
	})

	return suite.closeErr
}

// TracerProvider returns the shared provider for integration-owned instruments.
func (suite *Suite) TracerProvider() trace.TracerProvider { return suite.tracerProvider }

// MeterProvider returns the shared provider for integration-owned instruments.
func (suite *Suite) MeterProvider() metric.MeterProvider { return suite.meterProvider }

// Propagator returns the suite's explicitly configured propagator.
func (suite *Suite) Propagator() propagation.TextMapPropagator { return suite.propagator }

// Attributes returns a copy of the test's bounded telemetry identity.
func (test *Test) Attributes() []attribute.KeyValue {
	return append([]attribute.KeyValue(nil), test.identity...)
}

// StartOperation starts a semantic test operation and returns an idempotent,
// concurrency-safe completion function.
//
//nolint:spancheck // The span is ended exactly once by the returned completion function.
func (test *Test) StartOperation(ctx context.Context, name string, options ...trace.SpanStartOption) (operationCtx context.Context, finish func(error)) {
	operationCtx, span := test.tracer.Start(ctx, name, options...)
	var once sync.Once
	finish = func(err error) {
		once.Do(func() {
			if err != nil && !expectedContextError(ctx, err) {
				span.RecordError(err)
				span.SetStatus(codes.Error, "operation failed")
			}
			span.End()
		})
	}

	return operationCtx, finish
}

// Step runs fn in a semantic operation and always completes its span.
func (test *Test) Step(ctx context.Context, name string, fn func(context.Context) error) (err error) {
	ctx, finish := test.StartOperation(ctx, name)

	defer func() { finish(err) }()
	return fn(ctx)
}

func expectedContextError(ctx context.Context, err error) bool {
	return ctx.Err() != nil && errors.Is(err, ctx.Err())
}

func testIdentity(name string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("test.name", name),
		attribute.String("test.name.fingerprint", testNameFingerprint(name)),
	}
}

type identitySpanProcessor struct{}

func (*identitySpanProcessor) OnStart(parent context.Context, span sdktrace.ReadWriteSpan) {
	if attrs, ok := parent.Value(testIdentityKey{}).([]attribute.KeyValue); ok {
		span.SetAttributes(attrs...)
	}
}

func (*identitySpanProcessor) OnEnd(sdktrace.ReadOnlySpan)      {}
func (*identitySpanProcessor) Shutdown(context.Context) error   { return nil }
func (*identitySpanProcessor) ForceFlush(context.Context) error { return nil }

type identityRoundTripper struct {
	next http.RoundTripper
}

func (transport *identityRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	attrs, ok := request.Context().Value(testIdentityKey{}).([]attribute.KeyValue)
	if !ok {
		return transport.next.RoundTrip(request)
	}

	labeler := &otelhttp.Labeler{}
	labeler.Add(attrs...)

	ctx := otelhttp.ContextWithLabeler(request.Context(), labeler)
	return transport.next.RoundTrip(request.Clone(ctx))
}

// RunID identifies every signal emitted by this test process.
func RunID() string {
	runIDOnce.Do(func() {
		if override := strings.TrimSpace(os.Getenv("ATPROTO_TEST_RUN_ID")); override != "" {
			runID = override
			return
		}

		commit := SourceRevision()
		if len(commit) > 12 {
			commit = commit[:12]
		}
		if commit == "unknown" {
			commit = fmt.Sprintf("unknown-p%d", os.Getpid())
		}

		runID = commit + "-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	})

	return runID
}

// SourceRevision identifies the source revision independently of a test run.
func SourceRevision() string {
	revisionOnce.Do(func() {
		revision = buildRevision()

		if revision == "" {
			revision = strings.TrimSpace(os.Getenv("GITHUB_SHA"))
		}

		if revision == "" {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if output, err := exec.CommandContext(ctx, "git", "rev-parse", "HEAD").Output(); err == nil {
				revision = strings.TrimSpace(string(output))
			}
		}

		if revision == "" {
			revision = "unknown"
		}
	})

	return revision
}

func instanceID() string {
	instanceIDOnce.Do(func() {
		var random [8]byte
		if _, err := rand.Read(random[:]); err == nil {
			instanceIDValue = hex.EncodeToString(random[:])
			return
		}

		instanceIDValue = fmt.Sprintf("p%d-%d", os.Getpid(), time.Now().UnixNano())
	})

	return instanceIDValue
}

func buildRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}

	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return setting.Value
		}
	}

	return ""
}

func testNameFingerprint(name string) string {
	digest := sha256.Sum256([]byte(name))

	return hex.EncodeToString(digest[:4])
}
