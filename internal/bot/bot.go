package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	appbsky "github.com/bluesky-social/indigo/api/bsky"
	"github.com/bluesky-social/indigo/atproto/atclient"
	lexutil "github.com/bluesky-social/indigo/lex/util"
)

const (
	postCollection    = "app.bsky.feed.post"
	profileCollection = "app.bsky.actor.profile"
)

// Bot contains the authenticated Bluesky behavior.
type Bot struct {
	Client    lexutil.LexClient
	Repo      string
	Logger    *slog.Logger
	Telemetry *Instruments
}

// LabelProfile preserves the current profile and self-labels while ensuring
// the bot self-label is present.
func (b *Bot) LabelProfile(ctx context.Context) (err error) {
	ctx, finish := b.Telemetry.StartOperation(ctx, "bot.label_profile")
	defer func() { finish(err) }()

	profile := &appbsky.ActorProfile{}
	var swapRecord *string
	record, err := comatproto.RepoGetRecord(ctx, b.Client, "", profileCollection, b.Repo, "self")
	if err == nil {
		if record.Value == nil {
			return fmt.Errorf("profile record contains no value")
		}

		decoded, ok := record.Value.Val.(*appbsky.ActorProfile)
		if !ok {
			return fmt.Errorf("profile record has unexpected type %T", record.Value.Val)
		}

		profile = decoded

		// Require the fetched version so a concurrent profile edit is not overwritten.
		swapRecord = record.Cid
	} else if !isAPIError(err, "RecordNotFound") {
		return fmt.Errorf("get profile: %w", err)
	}

	if profile.Labels != nil && profile.Labels.LabelDefs_SelfLabels != nil {
		for _, label := range profile.Labels.LabelDefs_SelfLabels.Values {
			if label != nil && label.Val == "bot" {
				b.logger().InfoContext(ctx, "profile already labeled as bot", "operation", "label_profile", "outcome", "success")
				return nil
			}
		}
	} else {
		profile.Labels = &appbsky.ActorProfile_Labels{
			LabelDefs_SelfLabels: &comatproto.LabelDefs_SelfLabels{},
		}
	}
	profile.Labels.LabelDefs_SelfLabels.Values = append(
		profile.Labels.LabelDefs_SelfLabels.Values,
		&comatproto.LabelDefs_SelfLabel{Val: "bot"},
	)

	// Push the updated profile record back to the repo.
	// This will fail if the profile record was concurrently updated (compare and exchange on the cid),
	// which is expected behavior.
	_, err = comatproto.RepoPutRecord(ctx, b.Client, &comatproto.RepoPutRecord_Input{
		Collection: profileCollection,
		Repo:       b.Repo,
		Rkey:       "self",
		Record:     &lexutil.LexiconTypeDecoder{Val: profile},
		SwapRecord: swapRecord,
	})
	if err != nil {
		return fmt.Errorf("put profile: %w", err)
	}

	b.logger().InfoContext(ctx, "labeled profile as bot", "operation", "label_profile", "outcome", "success")
	return nil
}

// Run posts immediately and then at each interval until the context is canceled.
// The initial post must succeed; later failures are logged and retried.
func (b *Bot) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("post interval must be positive")
	}

	if err := b.post(ctx); err != nil {
		return err
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// Continue posting even if a previous post failed, but log the error.
			// Context cancellation means the bot is shutting down, so don't log that as an error.
			if err := b.post(ctx); err != nil && !errors.Is(err, context.Canceled) {
				b.logger().ErrorContext(ctx, "scheduled post failed", "operation", "create_post", "outcome", "error", "err", err)
			}
		}
	}
}

func (b *Bot) post(ctx context.Context) (err error) {
	ctx, finish := b.Telemetry.StartOperation(ctx, "bot.create_post")
	defer func() { finish(err) }()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = comatproto.RepoCreateRecord(ctx, b.Client, &comatproto.RepoCreateRecord_Input{
		Collection: postCollection,
		Repo:       b.Repo,
		Record: &lexutil.LexiconTypeDecoder{Val: &appbsky.FeedPost{
			Text:      "Hello, World! I am a bot. This post was created at " + now,
			CreatedAt: now,
		}},
	})
	if err != nil {
		return fmt.Errorf("create post: %w", err)
	}

	b.logger().InfoContext(ctx, "created post", "operation", "create_post", "outcome", "success")
	return nil
}

func isAPIError(err error, name string) bool {
	var apiErr *atclient.APIError
	return errors.As(err, &apiErr) && apiErr.Name == name
}

func (b *Bot) logger() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.Default()
}
