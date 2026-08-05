package bot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	appbsky "github.com/bluesky-social/indigo/api/bsky"
	"github.com/bluesky-social/indigo/atproto/atclient"
	lexutil "github.com/bluesky-social/indigo/lex/util"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const testDID = "did:plc:testbot"

type fakeLexClient struct {
	do func(method, endpoint string, body, out any) error
}

func (f *fakeLexClient) LexDo(_ context.Context, method, _ string, endpoint string, _ map[string]any, body, out any) error {
	return f.do(method, endpoint, body, out)
}

func testBot(client lexutil.LexClient) *Bot {
	return &Bot{
		Client: client,
		Repo:   testDID,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestPostRecordContents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var input *comatproto.RepoCreateRecord_Input
		client := &fakeLexClient{do: func(_, endpoint string, body, _ any) error {
			if endpoint != "com.atproto.repo.createRecord" {
				t.Fatalf("unexpected endpoint %q", endpoint)
			}
			input = body.(*comatproto.RepoCreateRecord_Input)
			return nil
		}}

		if err := testBot(client).post(context.Background()); err != nil {
			t.Fatal(err)
		}

		if input.Collection != postCollection || input.Repo != testDID || input.Rkey != nil {
			t.Fatalf("unexpected create input: %+v", input)
		}

		post, ok := input.Record.Val.(*appbsky.FeedPost)
		if !ok {
			t.Fatalf("record is %T", input.Record.Val)
		}

		if post.Text != "Hello, World! I am a bot. This post was created at 2000-01-01T00:00:00Z" ||
			post.CreatedAt != "2000-01-01T00:00:00Z" {
			t.Fatalf("unexpected post: %+v", post)
		}
	})
}

func TestPostInstrumentation(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() {
		if err := tracerProvider.Shutdown(context.Background()); err != nil {
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

	instruments, err := NewInstruments("atproto-dev/internal/bot", tracerProvider, meterProvider)
	if err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("create failed")
	client := &fakeLexClient{do: func(_, _ string, _, _ any) error { return wantErr }}
	app := testBot(client)
	app.Telemetry = instruments

	if err := app.post(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("post error = %v, want %v", err, wantErr)
	}

	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].Name() != "bot.create_post" {
		t.Fatalf("ended spans = %v, want bot.create_post", spans)
	}
	if spans[0].Status().Code != codes.Error {
		t.Fatalf("span status = %v, want error", spans[0].Status())
	}
	if got := spans[0].InstrumentationScope().Name; got != "atproto-dev/internal/bot" {
		t.Fatalf("span scope = %q, want atproto-dev/internal/bot", got)
	}

	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatal(err)
	}
	if len(metrics.ScopeMetrics) != 1 || metrics.ScopeMetrics[0].Scope.Name != "atproto-dev/internal/bot" {
		t.Fatalf("metric scopes = %+v, want atproto-dev/internal/bot", metrics.ScopeMetrics)
	}

	wantNames := map[string]bool{"bot.operation.count": false, "bot.operation.duration": false}
	for _, metric := range metrics.ScopeMetrics[0].Metrics {
		if _, ok := wantNames[metric.Name]; ok {
			wantNames[metric.Name] = true
		}

		switch data := metric.Data.(type) {
		case metricdata.Sum[int64]:
			assertOperationAttributes(t, data.DataPoints[0].Attributes)
		case metricdata.Histogram[float64]:
			assertOperationAttributes(t, data.DataPoints[0].Attributes)
		}
	}

	for name, found := range wantNames {
		if !found {
			t.Errorf("metric %q not collected", name)
		}
	}
}

func assertOperationAttributes(t *testing.T, attrs attribute.Set) {
	t.Helper()

	operation, ok := attrs.Value("operation")
	if !ok || operation.AsString() != "bot.create_post" {
		t.Fatalf("operation attribute = %v, want bot.create_post", operation)
	}

	outcome, ok := attrs.Value("outcome")
	if !ok || outcome.AsString() != "error" {
		t.Fatalf("outcome attribute = %v, want error", outcome)
	}

	if attrs.Len() != 2 {
		t.Fatalf("attribute count = %d, want 2", attrs.Len())
	}
}

func TestLabelProfilePreservesFieldsAndLabels(t *testing.T) {
	description := "Existing profile"
	var put *comatproto.RepoPutRecord_Input
	client := &fakeLexClient{do: func(_, endpoint string, body, out any) error {
		switch endpoint {
		case "com.atproto.repo.getRecord":
			result := out.(*comatproto.RepoGetRecord_Output)
			result.Cid = stringPointer("bafy-profile")
			result.Value = &lexutil.LexiconTypeDecoder{Val: &appbsky.ActorProfile{
				Description: &description,
				Labels: &appbsky.ActorProfile_Labels{LabelDefs_SelfLabels: &comatproto.LabelDefs_SelfLabels{
					Values: []*comatproto.LabelDefs_SelfLabel{{Val: "porn"}},
				}},
			}}
			return nil
		case "com.atproto.repo.putRecord":
			put = body.(*comatproto.RepoPutRecord_Input)
			return nil
		default:
			t.Fatalf("unexpected endpoint %q", endpoint)
			return nil
		}
	}}

	if err := testBot(client).LabelProfile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if put == nil || put.Rkey != "self" || put.SwapRecord == nil || *put.SwapRecord != "bafy-profile" {
		t.Fatalf("unexpected put input: %+v", put)
	}

	profile := put.Record.Val.(*appbsky.ActorProfile)
	if profile.Description == nil || *profile.Description != description {
		t.Fatal("description was not preserved")
	}

	labels := profile.Labels.LabelDefs_SelfLabels.Values
	if len(labels) != 2 || labels[0].Val != "porn" || labels[1].Val != "bot" {
		t.Fatalf("unexpected labels: %+v", labels)
	}
}

func TestLabelProfileRejectsConcurrentProfileEdit(t *testing.T) {
	const (
		fetchedCID = "bafy-profile"
		currentCID = "bafy-concurrent-profile"
	)

	writeApplied := false
	client := &fakeLexClient{do: func(_, endpoint string, body, out any) error {
		switch endpoint {
		case "com.atproto.repo.getRecord":
			result := out.(*comatproto.RepoGetRecord_Output)
			result.Cid = stringPointer(fetchedCID)
			result.Value = &lexutil.LexiconTypeDecoder{Val: &appbsky.ActorProfile{}}
			return nil
		case "com.atproto.repo.putRecord":
			put := body.(*comatproto.RepoPutRecord_Input)
			if put.SwapRecord != nil && *put.SwapRecord != currentCID {
				return &atclient.APIError{StatusCode: 400, Name: "InvalidSwap"}
			}
			writeApplied = true
			return nil
		default:
			t.Fatalf("unexpected endpoint %q", endpoint)
			return nil
		}
	}}

	err := testBot(client).LabelProfile(context.Background())
	var apiErr *atclient.APIError
	if !errors.As(err, &apiErr) || apiErr.Name != "InvalidSwap" {
		t.Fatalf("LabelProfile() error = %v, want InvalidSwap", err)
	}
	if writeApplied {
		t.Fatal("concurrent profile edit was overwritten")
	}
}

func TestLabelProfileIsIdempotent(t *testing.T) {
	putCalled := false
	client := &fakeLexClient{do: func(_, endpoint string, _, out any) error {
		switch endpoint {
		case "com.atproto.repo.getRecord":
			out.(*comatproto.RepoGetRecord_Output).Value = &lexutil.LexiconTypeDecoder{Val: &appbsky.ActorProfile{
				Labels: &appbsky.ActorProfile_Labels{LabelDefs_SelfLabels: &comatproto.LabelDefs_SelfLabels{
					Values: []*comatproto.LabelDefs_SelfLabel{{Val: "bot"}},
				}},
			}}
			return nil
		case "com.atproto.repo.putRecord":
			putCalled = true
			return nil
		default:
			t.Fatalf("unexpected endpoint %q", endpoint)
			return nil
		}
	}}

	if err := testBot(client).LabelProfile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if putCalled {
		t.Fatal("profile with an existing bot label should not be rewritten")
	}
}

func TestLabelProfileCreatesMissingProfile(t *testing.T) {
	var put *comatproto.RepoPutRecord_Input
	client := &fakeLexClient{do: func(_, endpoint string, body, out any) error {
		switch endpoint {
		case "com.atproto.repo.getRecord":
			return &atclient.APIError{StatusCode: 400, Name: "RecordNotFound"}
		case "com.atproto.repo.putRecord":
			put = body.(*comatproto.RepoPutRecord_Input)
			return nil
		default:
			t.Fatalf("unexpected endpoint %q", endpoint)
			return nil
		}
	}}

	if err := testBot(client).LabelProfile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if put == nil || put.SwapRecord != nil {
		t.Fatalf("unexpected put input: %+v", put)
	}

	profile := put.Record.Val.(*appbsky.ActorProfile)
	labels := profile.Labels.LabelDefs_SelfLabels.Values
	if len(labels) != 1 || labels[0].Val != "bot" {
		t.Fatalf("unexpected labels: %+v", labels)
	}
}

func TestRunPostsImmediatelyAndOnSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		var count atomic.Int32
		client := &fakeLexClient{do: func(_, endpoint string, _, _ any) error {
			if endpoint != "com.atproto.repo.createRecord" {
				t.Fatalf("unexpected endpoint %q", endpoint)
			}
			count.Add(1)
			return nil
		}}

		done := make(chan error, 1)
		go func() {
			done <- testBot(client).Run(ctx, time.Hour)
		}()

		synctest.Wait()
		time.Sleep(time.Hour)
		synctest.Wait()
		cancel()
		synctest.Wait()

		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if count.Load() != 2 {
			t.Fatalf("got %d posts, want 2", count.Load())
		}
	})
}

func TestRunStopsAfterImmediatePostWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var count atomic.Int32
	client := &fakeLexClient{do: func(_, _ string, _, _ any) error {
		count.Add(1)
		cancel()
		return nil
	}}

	if err := testBot(client).Run(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 1 {
		t.Fatalf("got %d posts, want 1", count.Load())
	}
}

func stringPointer(value string) *string {
	return &value
}
