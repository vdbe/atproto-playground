package bot

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Instruments records signals for a bounded set of bot operations.
type Instruments struct {
	tracer   trace.Tracer
	count    metric.Int64Counter
	duration metric.Float64Histogram
	attrs    []attribute.KeyValue
}

// WithAttributes returns instruments that add attrs to every metric record.
// The returned value shares the concurrency-safe OpenTelemetry instruments.
func (i *Instruments) WithAttributes(attrs ...attribute.KeyValue) *Instruments {
	if i == nil {
		return nil
	}
	bound := *i
	bound.attrs = append(append([]attribute.KeyValue(nil), i.attrs...), attrs...)
	return &bound
}

// NewInstruments creates bot instruments under the supplied scope.
func NewInstruments(scope string, tracerProvider trace.TracerProvider, meterProvider metric.MeterProvider) (*Instruments, error) {
	meter := meterProvider.Meter(scope)
	count, err := meter.Int64Counter("bot.operation.count", metric.WithDescription("Completed bot operations"))
	if err != nil {
		return nil, err
	}

	duration, err := meter.Float64Histogram("bot.operation.duration", metric.WithUnit("s"), metric.WithDescription("Bot operation duration"))
	if err != nil {
		return nil, err
	}

	return &Instruments{
		tracer:   tracerProvider.Tracer(scope),
		count:    count,
		duration: duration,
	}, nil
}

// StartOperation starts a named bot span and returns an idempotent completion
// function that records one duration and count with bounded outcome attributes.
//
//nolint:spancheck // The span is ended exactly once inside the returned finish closure; spancheck cannot trace it.
func (i *Instruments) StartOperation(ctx context.Context, name string) (outCtx context.Context, finish func(error)) {
	if i == nil {
		return ctx, func(error) {}
	}

	// The span is ended exactly once by the finish closure returned below.
	ctx, span := i.tracer.Start(ctx, name)
	started := time.Now()

	var once sync.Once
	finish = func(err error) {
		once.Do(func() {
			outcome := "success"
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					outcome = "canceled"
				} else {
					outcome = "error"
					span.RecordError(err)
					span.SetStatus(codes.Error, "operation failed")
				}
			}

			attributes := append([]attribute.KeyValue(nil), i.attrs...)
			attributes = append(attributes,
				attribute.String("operation", name),
				attribute.String("outcome", outcome),
			)
			attrs := metric.WithAttributes(attributes...)
			i.count.Add(ctx, 1, attrs)
			i.duration.Record(ctx, time.Since(started).Seconds(), attrs)

			span.End()
		})
	}
	return ctx, finish
}
