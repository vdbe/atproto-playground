package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"atproto-dev/internal/bot"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestRequiredURLAcceptsOnlyOrigins(t *testing.T) {
	t.Setenv("TEST_PDS", "https://pds.example.com/")
	got, err := requiredURL("TEST_PDS")
	if err != nil {
		t.Fatal(err)
	}

	if got != "https://pds.example.com" {
		t.Fatalf("requiredURL() = %q, want origin without trailing slash", got)
	}

	for _, value := range []string{
		"pds.example.com",
		"https://user@pds.example.com",
		"https://pds.example.com/prefix",
		"https://pds.example.com?debug=true",
	} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("TEST_PDS", value)
			if _, err := requiredURL("TEST_PDS"); err == nil {
				t.Fatalf("requiredURL() accepted %q", value)
			}
		})
	}
}

func TestLoginRequestIsLinkedToLoginSpan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/xrpc/com.atproto.server.createSession" {
			t.Fatalf("unexpected login path %q", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"accessJwt":"access","refreshJwt":"refresh","did":"did:plc:testbot","handle":"bot.test"}`)
	}))
	t.Cleanup(server.Close)

	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() {
		if err := tracerProvider.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down tracer provider: %v", err)
		}
	})

	instruments, err := bot.NewInstruments("test", tracerProvider, metricnoop.NewMeterProvider())
	if err != nil {
		t.Fatal(err)
	}

	httpClient := &http.Client{Transport: otelhttp.NewTransport(
		http.DefaultTransport,
		otelhttp.WithTracerProvider(tracerProvider),
	)}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, err := login(context.Background(), server.URL, "bot.test", "password", httpClient, instruments, logger); err != nil {
		t.Fatal(err)
	}

	spans := recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("ended spans = %d, want login and HTTP request", len(spans))
	}

	var loginSpan, requestSpan sdktrace.ReadOnlySpan
	for _, span := range spans {
		if span.Name() == "bot.login" {
			loginSpan = span
		} else {
			requestSpan = span
		}
	}

	if loginSpan == nil || requestSpan == nil {
		t.Fatalf("ended spans = %v, want login and HTTP request", spans)
	}

	if requestSpan.Parent().SpanID() != loginSpan.SpanContext().SpanID() ||
		requestSpan.SpanContext().TraceID() != loginSpan.SpanContext().TraceID() {
		t.Fatal("HTTP request span is not a child of the login span")
	}
}
