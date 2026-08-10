//go:build integration

package integration_test

import (
	"atproto-dev/internal/bot"
	"atproto-dev/internal/integrationtest"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	appbsky "github.com/bluesky-social/indigo/api/bsky"
	"github.com/bluesky-social/indigo/atproto/atclient"
	lexutil "github.com/bluesky-social/indigo/lex/util"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	postCollection        = "app.bsky.feed.post"
	profileCollection     = "app.bsky.actor.profile"
	profileRKey           = "self"
	workerShutdownTimeout = 15 * time.Second
)

type decoratedClient struct {
	client lexutil.LexClient
	do     func(context.Context, string, string, string, map[string]any, any, any) error
}

func (client *decoratedClient) LexDo(ctx context.Context, method, encoding, endpoint string, params map[string]any, body, out any) error {
	if client.do != nil {
		return client.do(ctx, method, encoding, endpoint, params, body, out)
	}

	return client.client.LexDo(ctx, method, encoding, endpoint, params, body, out)
}

func TestRunPostsAgainstPDS(t *testing.T) {
	t.Parallel()

	test := integrationSuite.Setup(t)
	account := integrationtest.CreateAccount(t, test)
	app := newBot(t, test, account, account.Client)

	const interval = 150 * time.Millisecond
	runCtx, cancel := context.WithCancel(test.Context)
	actionCtx, finishRun := test.StartOperation(runCtx, "test.action.bot_run", trace.WithAttributes(
		attribute.Int64("test.bot.interval_ms", interval.Milliseconds()),
	))
	done := make(chan error, 1)
	started := time.Now()
	go func() {
		err := app.Run(actionCtx, interval)
		finishRun(err)
		done <- err
	}()

	joined := false
	defer func() {
		cancel()
		if joined {
			return
		}
		select {
		case <-done:
		case <-time.After(workerShutdownTimeout):
			t.Errorf("Bot.Run goroutine did not stop during cleanup")
		}
	}()

	observeCtx, finishObserve := test.StartOperation(test.Context, "test.observe.posts", trace.WithAttributes(
		attribute.String("atproto.collection", postCollection),
		attribute.Int("test.observation.minimum_count", 2),
	))
	pollCtx, stopPolling := context.WithTimeout(observeCtx, 10*time.Second)
	_, attempts, observationErr := observePosts(pollCtx, account.Client, account.DID, started, interval, 2)
	stopPolling()
	trace.SpanFromContext(observeCtx).SetAttributes(attribute.Int("test.observation.poll_attempts", attempts))
	finishObserve(observationErr)

	cancel()
	var runErr error
	select {
	case runErr = <-done:
		joined = true
	case <-time.After(workerShutdownTimeout):
		t.Fatal("Bot.Run did not return after cancellation")
	}
	if observationErr != nil {
		t.Fatal(observationErr)
	}
	if runErr != nil {
		t.Fatalf("Bot.Run returned after cancellation: %v", runErr)
	}
}

func TestLabelProfileAlreadyCorrectDoesNotUpdatePDS(t *testing.T) {
	t.Parallel()

	test := integrationSuite.Setup(t)
	account := integrationtest.CreateAccount(t, test)
	if err := profileSeed(test, account, &appbsky.ActorProfile{Labels: selfLabels("bot")}, "labeled"); err != nil {
		t.Fatal(err)
	}
	var before *comatproto.RepoGetRecord_Output
	if err := operation(test.Context, test, "test.observe.profile", []attribute.KeyValue{attribute.String("test.observation.phase", "before")}, func(ctx context.Context) (err error) {
		before, err = getProfileRecord(ctx, account.Client, account.DID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var headBefore string
	if err := operation(test.Context, test, "test.observe.repository_head", []attribute.KeyValue{attribute.String("test.observation.phase", "before")}, func(ctx context.Context) (err error) {
		headBefore, err = repoHead(ctx, account.Client, account.DID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var putCalls atomic.Int32
	client := &decoratedClient{client: account.Client}
	client.do = func(ctx context.Context, method, encoding, endpoint string, params map[string]any, body, out any) error {
		if endpoint == "com.atproto.repo.putRecord" {
			putCalls.Add(1)
		}
		return client.client.LexDo(ctx, method, encoding, endpoint, params, body, out)
	}
	if err := newBot(t, test, account, client).LabelProfile(test.Context); err != nil {
		t.Fatal(err)
	}

	var after *comatproto.RepoGetRecord_Output
	if err := operation(test.Context, test, "test.observe.profile", []attribute.KeyValue{attribute.String("test.observation.phase", "after")}, func(ctx context.Context) (err error) {
		after, err = getProfileRecord(ctx, account.Client, account.DID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var headAfter string
	if err := operation(test.Context, test, "test.observe.repository_head", []attribute.KeyValue{attribute.String("test.observation.phase", "after")}, func(ctx context.Context) (err error) {
		headAfter, err = repoHead(ctx, account.Client, account.DID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if putCalls.Load() != 0 {
		t.Fatalf("LabelProfile made %d repo.putRecord requests, want 0", putCalls.Load())
	}
	if dereference(before.Cid) != dereference(after.Cid) || headBefore != headAfter {
		t.Fatalf("idempotent label changed the repository: cid %s -> %s, head %s -> %s", dereference(before.Cid), dereference(after.Cid), headBefore, headAfter)
	}
}

func TestLabelProfileRejectsConcurrentProfileEditAgainstPDS(t *testing.T) {
	t.Parallel()

	test := integrationSuite.Setup(t)
	account := integrationtest.CreateAccount(t, test)
	displayName := "Original"
	if err := profileSeed(test, account, &appbsky.ActorProfile{DisplayName: &displayName}, "present"); err != nil {
		t.Fatal(err)
	}

	scenarioErr := runConcurrentProfileEdit(test, account, displayName)
	if scenarioErr != nil {
		t.Fatal(scenarioErr)
	}
}

func TestLabelProfilePreservesExistingFieldsAgainstPDS(t *testing.T) {
	t.Parallel()

	test := integrationSuite.Setup(t)
	account := integrationtest.CreateAccount(t, test)
	displayName, description := "Integration Bot", "Existing description"
	if err := profileSeed(test, account, &appbsky.ActorProfile{
		DisplayName: &displayName,
		Description: &description,
		Labels:      selfLabels("graphic-media"),
	}, "present"); err != nil {
		t.Fatal(err)
	}

	if err := newBot(t, test, account, account.Client).LabelProfile(test.Context); err != nil {
		t.Fatal(err)
	}

	var profile *appbsky.ActorProfile
	if err := operation(test.Context, test, "test.observe.profile", []attribute.KeyValue{attribute.String("test.observation.phase", "final")}, func(ctx context.Context) (err error) {
		profile, err = getProfile(ctx, account.Client, account.DID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if profile.DisplayName == nil || *profile.DisplayName != displayName || profile.Description == nil || *profile.Description != description {
		t.Fatalf("existing profile fields were not preserved: %+v", profile)
	}

	labels := profile.Labels.LabelDefs_SelfLabels.Values
	if len(labels) != 2 || labels[0].Val != "graphic-media" || labels[1].Val != "bot" || countLabel(profile, "bot") != 1 {
		t.Fatalf("profile labels = %+v, want graphic-media then bot once", labels)
	}
}

func TestLabelProfileCreatesMissingProfileAgainstPDS(t *testing.T) {
	t.Parallel()

	test := integrationSuite.Setup(t)
	account := integrationtest.CreateAccount(t, test)
	if err := operation(test.Context, test, "test.fixture.profile.absent", []attribute.KeyValue{
		attribute.String("atproto.collection", profileCollection),
		attribute.String("test.profile.initial_state", "absent"),
	}, func(ctx context.Context) error {
		return ensureProfileMissing(ctx, account.Client, account.DID)
	}); err != nil {
		t.Fatal(err)
	}

	app := newBot(t, test, account, account.Client)
	if err := app.LabelProfile(test.Context); err != nil {
		t.Fatal(err)
	}

	var profile *appbsky.ActorProfile
	if err := operation(test.Context, test, "test.observe.profile", []attribute.KeyValue{attribute.String("test.observation.phase", "after")}, func(ctx context.Context) (err error) {
		profile, err = getProfile(ctx, account.Client, account.DID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if countLabel(profile, "bot") != 1 {
		t.Fatalf("created profile does not contain one bot label: %+v", profile)
	}

	var headBefore string
	if err := operation(test.Context, test, "test.observe.repository_head", []attribute.KeyValue{attribute.String("test.observation.phase", "before")}, func(ctx context.Context) (err error) {
		headBefore, err = repoHead(ctx, account.Client, account.DID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.LabelProfile(test.Context); err != nil {
		t.Fatal(err)
	}

	var headAfter string
	if err := operation(test.Context, test, "test.observe.repository_head", []attribute.KeyValue{attribute.String("test.observation.phase", "after")}, func(ctx context.Context) (err error) {
		headAfter, err = repoHead(ctx, account.Client, account.DID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if headBefore != headAfter {
		t.Fatalf("second LabelProfile changed repository head: %s -> %s", headBefore, headAfter)
	}
}

func runConcurrentProfileEdit(test *integrationtest.Test, account *integrationtest.Account, displayName string) (scenarioErr error) {
	actionCtx, finish := test.StartOperation(test.Context, "test.action.concurrent_profile_edit", trace.WithAttributes(
		attribute.String("atproto.collection", profileCollection),
		attribute.Bool("test.concurrent_edit.expected_error", true),
	))
	fetched := make(chan struct{})
	release := make(chan struct{})

	var releaseOnce sync.Once
	releaseEdit := func() { releaseOnce.Do(func() { close(release) }) }
	var fetchedCID, submittedSwap string
	client := &decoratedClient{client: account.Client}
	client.do = func(ctx context.Context, method, encoding, endpoint string, params map[string]any, body, out any) error {
		err := client.client.LexDo(ctx, method, encoding, endpoint, params, body, out)
		if endpoint == "com.atproto.repo.getRecord" && err == nil {
			record, ok := out.(*comatproto.RepoGetRecord_Output)
			if !ok {
				return fmt.Errorf("getRecord output is %T", out)
			}
			fetchedCID = dereference(record.Cid)
			close(fetched)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if endpoint == "com.atproto.repo.putRecord" {
			input, ok := body.(*comatproto.RepoPutRecord_Input)
			if !ok {
				return fmt.Errorf("putRecord input is %T", body)
			}
			submittedSwap = dereference(input.SwapRecord)
		}
		return err
	}

	result := make(chan error, 1)
	started, joined := false, false
	defer func() {
		releaseEdit()
		if started && !joined {
			select {
			case <-result:
			case <-time.After(workerShutdownTimeout):
				if scenarioErr == nil {
					scenarioErr = errors.New("LabelProfile goroutine did not stop during cleanup")
				}
			}
		}
		finish(scenarioErr)
	}()

	app := &bot.Bot{Client: client, Repo: account.DID, Logger: test.Logger, Telemetry: botTelemetry.WithAttributes(test.Attributes()...)}
	go func() { result <- app.LabelProfile(actionCtx) }()
	started = true

	select {
	case <-fetched:
	case <-time.After(workerShutdownTimeout):
		return errors.New("LabelProfile did not complete its real getRecord request")
	}

	current, err := getProfileRecord(actionCtx, account.Client, account.DID)
	if err != nil {
		return fmt.Errorf("read profile before competing edit: %w", err)
	}
	competingDescription := "Competing profile edit"
	if err := putProfile(actionCtx, account.Client, account.DID, &appbsky.ActorProfile{
		DisplayName: &displayName, Description: &competingDescription,
	}, current.Cid); err != nil {
		return fmt.Errorf("write competing profile edit: %w", err)
	}
	releaseEdit()

	var labelErr error
	select {
	case labelErr = <-result:
		joined = true
	case <-time.After(workerShutdownTimeout):
		return errors.New("LabelProfile did not return after the competing edit")
	}
	if submittedSwap != fetchedCID {
		return fmt.Errorf("LabelProfile submitted swap CID %q, want fetched CID %q", submittedSwap, fetchedCID)
	}
	var apiErr *atclient.APIError
	if !errors.As(labelErr, &apiErr) || apiErr.Name != "InvalidSwap" {
		return fmt.Errorf("LabelProfile error = %w, want real InvalidSwap", labelErr)
	}

	var profile *appbsky.ActorProfile
	if err := operation(actionCtx, test, "test.observe.profile", []attribute.KeyValue{attribute.String("test.observation.phase", "final")}, func(ctx context.Context) (err error) {
		profile, err = getProfile(ctx, account.Client, account.DID)
		return err
	}); err != nil {
		return err
	}
	if profile.Description == nil || *profile.Description != competingDescription {
		return fmt.Errorf("competing profile edit was not preserved: %+v", profile)
	}
	if countLabel(profile, "bot") != 0 {
		return errors.New("stale bot-label write reached the repository")
	}

	return nil
}

func observePosts(ctx context.Context, client lexutil.LexClient, did string, started time.Time, interval time.Duration, minimum int) ([]*comatproto.RepoListRecords_Record, int, error) {
	var records []*comatproto.RepoListRecords_Record
	attempts := 0
	for len(records) < minimum {
		attempts++
		listed, err := comatproto.RepoListRecords(ctx, client, postCollection, "", 20, did, true)
		if err != nil {
			return nil, attempts, fmt.Errorf("list bot posts: %w", err)
		}

		records = listed.Records
		if len(records) < minimum {
			select {
			case <-ctx.Done():
				return nil, attempts, fmt.Errorf("wait for %d real PDS posts: %w", minimum, ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
	}

	type observedPost struct {
		uri, cid string
		created  time.Time
	}
	posts := make([]observedPost, 0, len(records))
	seenURI := make(map[string]bool)
	seenCID := make(map[string]bool)
	for _, record := range records {
		if record.Value == nil {
			return nil, attempts, errors.New("post record has no value")
		}

		post, ok := record.Value.Val.(*appbsky.FeedPost)
		if !ok {
			return nil, attempts, fmt.Errorf("post record value is %T", record.Value.Val)
		}

		created, err := time.Parse(time.RFC3339Nano, post.CreatedAt)
		if err != nil {
			return nil, attempts, fmt.Errorf("parse post timestamp %q: %w", post.CreatedAt, err)
		}

		wantText := "Hello, World! I am a bot. This post was created at " + post.CreatedAt
		if post.Text != wantText {
			return nil, attempts, fmt.Errorf("post text = %q, want %q", post.Text, wantText)
		}
		if seenURI[record.Uri] || seenCID[record.Cid] {
			return nil, attempts, fmt.Errorf("posts are not distinct: uri=%s cid=%s", record.Uri, record.Cid)
		}

		seenURI[record.Uri], seenCID[record.Cid] = true, true
		posts = append(posts, observedPost{uri: record.Uri, cid: record.Cid, created: created})
	}

	sort.Slice(posts, func(i, j int) bool { return posts[i].created.Before(posts[j].created) })

	if posts[0].created.Before(started.Add(-time.Second)) || posts[0].created.After(started.Add(2*time.Second)) {
		return nil, attempts, fmt.Errorf("first post was not immediate: started=%s created=%s", started, posts[0].created)
	}

	if posts[1].created.Sub(posts[0].created) < interval/2 {
		return nil, attempts, fmt.Errorf("second post did not repeat after the interval: timestamps=%s, %s", posts[0].created, posts[1].created)
	}

	return records, attempts, nil
}

func operation(ctx context.Context, test *integrationtest.Test, name string, attrs []attribute.KeyValue, fn func(context.Context) error) error {
	return test.Step(ctx, name, func(ctx context.Context) error {
		trace.SpanFromContext(ctx).SetAttributes(attrs...)

		return fn(ctx)
	})
}

func profileSeed(test *integrationtest.Test, account *integrationtest.Account, profile *appbsky.ActorProfile, initialState string) error {
	return operation(test.Context, test, "test.fixture.profile.seed", []attribute.KeyValue{
		attribute.String("atproto.collection", profileCollection),
		attribute.String("test.profile.initial_state", initialState),
	}, func(ctx context.Context) error {
		return putProfile(ctx, account.Client, account.DID, profile, nil)
	})
}

func newBot(t *testing.T, test *integrationtest.Test, account *integrationtest.Account, client lexutil.LexClient) *bot.Bot {
	t.Helper()

	return &bot.Bot{Client: client, Repo: account.DID, Logger: test.Logger, Telemetry: botTelemetry.WithAttributes(test.Attributes()...)}
}

func putProfile(ctx context.Context, client lexutil.LexClient, did string, profile *appbsky.ActorProfile, swap *string) error {
	_, err := comatproto.RepoPutRecord(ctx, client, &comatproto.RepoPutRecord_Input{
		Collection: profileCollection, Repo: did, Rkey: profileRKey,
		Record: &lexutil.LexiconTypeDecoder{Val: profile}, SwapRecord: swap,
	})

	return err
}

func getProfileRecord(ctx context.Context, client lexutil.LexClient, did string) (*comatproto.RepoGetRecord_Output, error) {
	record, err := comatproto.RepoGetRecord(ctx, client, "", profileCollection, did, profileRKey)
	if err != nil {
		return nil, err
	}

	return record, nil
}

func getProfile(ctx context.Context, client lexutil.LexClient, did string) (*appbsky.ActorProfile, error) {
	record, err := getProfileRecord(ctx, client, did)
	if err != nil {
		return nil, err
	}

	if record.Value == nil {
		return nil, errors.New("profile record has no value")
	}

	profile, ok := record.Value.Val.(*appbsky.ActorProfile)
	if !ok {
		return nil, fmt.Errorf("profile record value is %T", record.Value.Val)
	}

	return profile, nil
}

func ensureProfileMissing(ctx context.Context, client lexutil.LexClient, did string) error {
	record, err := comatproto.RepoGetRecord(ctx, client, "", profileCollection, did, profileRKey)
	if err == nil {
		_, err = comatproto.RepoDeleteRecord(ctx, client, &comatproto.RepoDeleteRecord_Input{
			Collection: profileCollection, Repo: did, Rkey: profileRKey, SwapRecord: record.Cid,
		})
		return err
	}

	var apiErr *atclient.APIError
	if !errors.As(err, &apiErr) || apiErr.Name != "RecordNotFound" {
		return err
	}

	return nil
}

func repoHead(ctx context.Context, client lexutil.LexClient, did string) (string, error) {
	head, err := comatproto.SyncGetLatestCommit(ctx, client, did)
	if err != nil {
		return "", err
	}

	return head.Cid, nil
}

func selfLabels(values ...string) *appbsky.ActorProfile_Labels {
	labels := make([]*comatproto.LabelDefs_SelfLabel, 0, len(values))
	for _, value := range values {
		labels = append(labels, &comatproto.LabelDefs_SelfLabel{Val: value})
	}

	return &appbsky.ActorProfile_Labels{LabelDefs_SelfLabels: &comatproto.LabelDefs_SelfLabels{Values: labels}}
}

func countLabel(profile *appbsky.ActorProfile, value string) int {
	if profile.Labels == nil || profile.Labels.LabelDefs_SelfLabels == nil {
		return 0
	}

	count := 0
	for _, label := range profile.Labels.LabelDefs_SelfLabels.Values {
		if label != nil && label.Val == value {
			count++
		}
	}

	return count
}

func dereference(value *string) string {
	if value == nil {
		return ""
	}

	return *value
}
