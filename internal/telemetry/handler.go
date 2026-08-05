// Package telemetry configures OpenTelemetry logging, tracing, and metrics for
// the development stack, and provides a fanout handler that mirrors logs to
// stdout.
package telemetry

import (
	"context"
	"errors"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// fanoutHandler sends the original record to OpenTelemetry and a correlated
// copy to stdout so trace identifiers are not duplicated as OTLP attributes.
type fanoutHandler struct {
	stdout slog.Handler
	otel   slog.Handler
}

func newFanoutHandler(stdout, otel slog.Handler) slog.Handler {
	if otel == nil {
		return stdout
	}
	return &fanoutHandler{stdout: stdout, otel: otel}
}

func (h *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.stdout.Enabled(ctx, level) || h.otel.Enabled(ctx, level)
}

func (h *fanoutHandler) Handle(ctx context.Context, record slog.Record) error {
	var stdoutErr, otelErr error
	if h.stdout.Enabled(ctx, record.Level) {
		stdoutRecord := record.Clone()
		spanContext := trace.SpanContextFromContext(ctx)
		if spanContext.IsValid() {
			stdoutRecord.AddAttrs(
				slog.String("trace_id", spanContext.TraceID().String()),
				slog.String("span_id", spanContext.SpanID().String()),
			)
		}
		stdoutErr = h.stdout.Handle(ctx, stdoutRecord)
	}

	if h.otel.Enabled(ctx, record.Level) {
		otelErr = h.otel.Handle(ctx, record)
	}

	return errors.Join(stdoutErr, otelErr)
}

func (h *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &fanoutHandler{stdout: h.stdout.WithAttrs(attrs), otel: h.otel.WithAttrs(attrs)}
}

func (h *fanoutHandler) WithGroup(name string) slog.Handler {
	return &fanoutHandler{stdout: h.stdout.WithGroup(name), otel: h.otel.WithGroup(name)}
}
