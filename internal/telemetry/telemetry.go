package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const defaultShutdownTimeout = 5 * time.Second

// OTLP signal endpoint environment variables, shared with the test suite.
const (
	otlpTracesEndpointEnv  = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	otlpMetricsEndpointEnv = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	otlpLogsEndpointEnv    = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"
)

// Config describes process telemetry. OTLP is enabled only when a common
// endpoint or all three signal-specific endpoints are explicitly configured.
type Config struct {
	ServiceName        string
	Stdout             slog.Handler
	ShutdownTimeout    time.Duration
	ResourceAttributes []attribute.KeyValue
	// Ownership of SpanProcessors transfers to New, including when New fails.
	SpanProcessors []sdktrace.SpanProcessor
}

// Telemetry owns OpenTelemetry providers, propagation, and stdout logging.
type Telemetry struct {
	stdout         slog.Handler
	loggerProvider *sdklog.LoggerProvider
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider
	propagator     propagation.TextMapPropagator
	timeout        time.Duration
	shutdown       func(context.Context) error
	once           sync.Once
	shutdownErr    error
}

// New configures stdout logging and, when explicitly enabled, creates
// OTLP/gRPC providers for logs, traces, and metrics.
func New(ctx context.Context, config Config) (*Telemetry, error) {
	if config.ShutdownTimeout <= 0 {
		config.ShutdownTimeout = defaultShutdownTimeout
	}
	if config.ServiceName == "" {
		return nil, shutdownProcessorsAfterFailure(ctx, config, errors.New("telemetry service name is required"))
	}
	if config.Stdout == nil {
		config.Stdout = slog.NewTextHandler(os.Stderr, nil)
	}

	propagator := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	)

	enabled, err := otlpEnabled()
	if err != nil {
		return nil, shutdownProcessorsAfterFailure(ctx, config, err)
	}
	if !enabled {
		return &Telemetry{
			stdout:         config.Stdout,
			tracerProvider: tracenoop.NewTracerProvider(),
			meterProvider:  metricnoop.NewMeterProvider(),
			propagator:     propagator,
			timeout:        config.ShutdownTimeout,
			shutdown:       spanProcessorShutdown(config.SpanProcessors),
		}, nil
	}
	return newOTLPProvider(ctx, config, propagator)
}

func spanProcessorShutdown(processors []sdktrace.SpanProcessor) func(context.Context) error {
	if len(processors) == 0 {
		return nil
	}
	return func(ctx context.Context) error {
		var err error
		for _, processor := range processors {
			if processor != nil {
				err = errors.Join(err, processor.Shutdown(ctx))
			}
		}
		return err
	}
}

func shutdownProcessorsAfterFailure(ctx context.Context, config Config, setupErr error) error {
	shutdown := spanProcessorShutdown(config.SpanProcessors)
	if shutdown == nil {
		return setupErr
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), config.ShutdownTimeout)
	defer cancel()
	return errors.Join(setupErr, shutdown(shutdownCtx))
}

func newOTLPProvider(ctx context.Context, config Config, propagator propagation.TextMapPropagator) (*Telemetry, error) {
	res, err := newResource(config)
	if err != nil {
		return nil, shutdownProcessorsAfterFailure(ctx, config, fmt.Errorf("create telemetry resource: %w", err))
	}

	traceExporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, shutdownProcessorsAfterFailure(ctx, config, fmt.Errorf("create OTLP trace exporter: %w", err))
	}

	traceOptions := make([]sdktrace.TracerProviderOption, 0, len(config.SpanProcessors)+2)
	for _, processor := range config.SpanProcessors {
		if processor != nil {
			traceOptions = append(traceOptions, sdktrace.WithSpanProcessor(processor))
		}
	}
	traceOptions = append(traceOptions, sdktrace.WithBatcher(traceExporter), sdktrace.WithResource(res))
	traceProvider := sdktrace.NewTracerProvider(traceOptions...)
	cleanup := func(ctx context.Context) error { return traceProvider.Shutdown(ctx) }

	metricExporter, err := otlpmetricgrpc.New(ctx)
	if err != nil {
		shutdownOnFailure(ctx, config.ShutdownTimeout, cleanup)
		return nil, fmt.Errorf("create OTLP metric exporter: %w", err)
	}

	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
		sdkmetric.WithResource(res),
	)
	cleanup = joinShutdown(meterProvider.Shutdown, cleanup)

	logExporter, err := otlploggrpc.New(ctx)
	if err != nil {
		shutdownOnFailure(ctx, config.ShutdownTimeout, cleanup)
		return nil, fmt.Errorf("create OTLP log exporter: %w", err)
	}

	loggerProvider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
		sdklog.WithResource(res),
	)
	cleanup = joinShutdown(loggerProvider.Shutdown, cleanup)

	return &Telemetry{
		stdout:         config.Stdout,
		loggerProvider: loggerProvider,
		tracerProvider: traceProvider,
		meterProvider:  meterProvider,
		propagator:     propagator,
		timeout:        config.ShutdownTimeout,
		shutdown:       cleanup,
	}, nil
}

func newResource(config Config) (*resource.Resource, error) {
	attributes := append([]attribute.KeyValue(nil), config.ResourceAttributes...)
	attributes = append(attributes, semconv.ServiceName(config.ServiceName))
	return resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, attributes...),
	)
}

// Logger returns a slog logger whose OTLP records use scope as their native
// instrumentation scope. All records are also written once to stdout.
func (t *Telemetry) Logger(scope string) *slog.Logger {
	if t.loggerProvider == nil {
		return slog.New(t.stdout)
	}
	return slog.New(newFanoutHandler(t.stdout, otelslog.NewHandler(
		scope, otelslog.WithLoggerProvider(t.loggerProvider),
	)))
}

// TracerProvider returns the configured provider for application-owned spans.
func (t *Telemetry) TracerProvider() trace.TracerProvider { return t.tracerProvider }

// MeterProvider returns the configured provider for application-owned metrics.
func (t *Telemetry) MeterProvider() metric.MeterProvider { return t.meterProvider }

// Propagator returns the configured W3C trace-context and baggage propagator.
func (t *Telemetry) Propagator() propagation.TextMapPropagator { return t.propagator }

// Shutdown flushes all providers once using its own bounded context.
func (t *Telemetry) Shutdown() error {
	t.once.Do(func() {
		if t.shutdown == nil {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), t.timeout)
		defer cancel()
		t.shutdownErr = t.shutdown(ctx)
	})

	return t.shutdownErr
}

func otlpEnabled() (bool, error) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")), "true") {
		return false, nil
	}

	commonConfigured := false
	if endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")); endpoint != "" {
		if err := validateOTLPEndpoint("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint); err != nil {
			return false, err
		}
		commonConfigured = true
	}

	configured := 0
	for _, name := range []string{
		otlpTracesEndpointEnv,
		otlpMetricsEndpointEnv,
		otlpLogsEndpointEnv,
	} {
		if endpoint := strings.TrimSpace(os.Getenv(name)); endpoint != "" {
			if err := validateOTLPEndpoint(name, endpoint); err != nil {
				return false, err
			}

			configured++
		}
	}

	if commonConfigured {
		return true, nil
	}

	if configured != 0 && configured != 3 {
		return false, errors.New("configure OTEL_EXPORTER_OTLP_ENDPOINT or all three signal-specific OTLP endpoints")
	}

	return configured == 3, nil
}

func validateOTLPEndpoint(name, endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s must be an http or https URL with a host, for example http://localhost:4317", name)
	}

	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s must not contain a query string or fragment", name)
	}

	return nil
}

func joinShutdown(first, second func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		return errors.Join(first(ctx), second(ctx))
	}
}

func shutdownOnFailure(ctx context.Context, timeout time.Duration, shutdown func(context.Context) error) {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	if err := shutdown(shutdownCtx); err != nil {
		slog.Default().Error("telemetry shutdown after setup failure", "err", err)
	}
}
