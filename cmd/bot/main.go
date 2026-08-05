// Package main runs the ATProto development bot: it loads local configuration,
// logs in to a local PDS, labels the bot profile, and posts on a schedule.
package main

import (
	"atproto-dev/internal/bot"
	"atproto-dev/internal/telemetry"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/joho/godotenv"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	usage        = "usage: bot"
	httpTimeout  = 30 * time.Second
	postInterval = 3 * time.Hour
	commandScope = "atproto-dev/cmd/bot"
	botScope     = "atproto-dev/internal/bot"
)

func main() {
	if run() != nil {
		os.Exit(1)
	}
}

func run() (runErr error) {
	if usageRequested(os.Args) {
		fmt.Println(usage)
		return nil
	}

	logger := slog.Default()
	var observability *telemetry.Telemetry

	// Ensure that we always shut down observability and log the final outcome
	defer func() {
		if runErr != nil {
			logger.Error("bot stopped", "err", runErr)
		} else {
			logger.Info("bot stopped")
		}
		if observability != nil {
			if err := observability.Shutdown(); err != nil {
				runErr = errors.Join(runErr, err)
				logger.Error("telemetry shutdown failed", "err", err)
			}
		}
	}()

	if len(os.Args) != 1 {
		return fmt.Errorf("%s", usage)
	}

	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load .env: %w", err)
	}

	// Setup observability (tracing, metrics, logging)
	observability, err := telemetry.New(context.Background(), telemetry.Config{ServiceName: "atproto-bot"})
	if err != nil {
		return fmt.Errorf("initialize telemetry: %w", err)
	}
	logger = observability.Logger(commandScope)
	botLogger := observability.Logger(botScope)
	slog.SetDefault(logger)

	// Create instruments for the command and bot packages
	commandInstruments, err := newInstruments(observability, commandScope)
	if err != nil {
		return err
	}
	botInstruments, err := newInstruments(observability, botScope)
	if err != nil {
		return err
	}

	// Validate required environment variables for account login
	pds, handle, password, err := loadAccountConfig()
	if err != nil {
		return err
	}

	// Create a context that is canceled on SIGINT or SIGTERM for graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpClient := &http.Client{
		Timeout: httpTimeout,

		// Wrap the default transport with OpenTelemetry instrumentation for tracing HTTP requests
		Transport: otelhttp.NewTransport(
			http.DefaultTransport,
			otelhttp.WithTracerProvider(observability.TracerProvider()),
			otelhttp.WithMeterProvider(observability.MeterProvider()),
			otelhttp.WithPropagators(observability.Propagator()),
		),
	}
	client, err := login(ctx, pds, handle, password, httpClient, commandInstruments, logger)
	if err != nil {
		return fmt.Errorf("log in: %w", err)
	}
	client.Headers.Set("User-Agent", "atproto-dev-bot/1.0")
	defer func() {
		if auth, ok := client.Auth.(*atclient.PasswordAuth); ok {
			logout(auth, httpClient, commandInstruments, logger)
		}
	}()
	if client.AccountDID == nil {
		return errors.New("login response did not include an account DID")
	}

	app := &bot.Bot{
		Client:    client,
		Repo:      client.AccountDID.String(),
		Logger:    botLogger,
		Telemetry: botInstruments,
	}
	return runBot(ctx, app, logger)
}

// usageRequested reports whether the process was invoked with a help flag.
func usageRequested(args []string) bool {
	return len(args) == 2 && (args[1] == "-h" || args[1] == "--help")
}

// newInstruments creates bot telemetry instruments under the supplied scope.
func newInstruments(observability *telemetry.Telemetry, scope string) (*bot.Instruments, error) {
	instruments, err := bot.NewInstruments(scope, observability.TracerProvider(), observability.MeterProvider())
	if err != nil {
		return nil, fmt.Errorf("create %s telemetry: %w", scope, err)
	}
	return instruments, nil
}

// loadAccountConfig validates and returns the account environment variables.
func loadAccountConfig() (pds, handle, password string, err error) {
	pds, err = requiredURL("ACCOUNT_PDS")
	if err != nil {
		return "", "", "", err
	}
	handle, err = requiredEnv("ACCOUNT_HANDLE")
	if err != nil {
		return "", "", "", err
	}
	password, err = requiredEnv("ACCOUNT_PASSWORD")
	if err != nil {
		return "", "", "", err
	}
	return pds, handle, password, nil
}

// runBot labels the bot profile and runs the posting loop until the context is canceled.
func runBot(ctx context.Context, app *bot.Bot, logger *slog.Logger) error {
	if err := app.LabelProfile(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	logger.Info("bot started", "postInterval", postInterval)
	if err := app.Run(ctx, postInterval); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func login(ctx context.Context, pds, handle, password string, httpClient *http.Client, instruments *bot.Instruments, logger *slog.Logger) (client *atclient.APIClient, err error) {
	// Bound the entire login operation independently of the HTTP client's request timeout.
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	// Start a telemetry span for the login operation
	ctx, finish := instruments.StartOperation(ctx, "bot.login")
	defer func() { finish(err) }()

	client = atclient.NewAPIClient(pds)
	client.Client = httpClient
	session, err := comatproto.ServerCreateSession(ctx, client, &comatproto.ServerCreateSession_Input{
		Identifier: handle,
		Password:   password,
	})
	if err != nil {
		return nil, err
	}

	did, err := syntax.ParseDID(session.Did)
	if err != nil {
		return nil, err
	}

	client.Auth = &atclient.PasswordAuth{Session: atclient.PasswordSessionData{
		AccessToken:  session.AccessJwt,
		RefreshToken: session.RefreshJwt,
		AccountDID:   did,
		Host:         client.Host,
	}}
	client.AccountDID = &did

	logger.InfoContext(ctx, "logged in", "operation", "login", "outcome", "success")
	return client, nil
}

func logout(auth *atclient.PasswordAuth, client *http.Client, instruments *bot.Instruments, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ctx, finish := instruments.StartOperation(ctx, "bot.logout")

	err := auth.Logout(ctx, client)
	if err != nil {
		logger.WarnContext(ctx, "logout failed", "operation", "logout", "outcome", "error", "err", err)
	} else {
		logger.InfoContext(ctx, "logged out", "operation", "logout", "outcome", "success")
	}

	finish(err)
}

func requiredEnv(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}

	return value, nil
}

func requiredURL(name string) (string, error) {
	value, err := requiredEnv(name)
	if err != nil {
		return "", err
	}

	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("%s must be an HTTP(S) origin", name)
	}

	if parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%s must not contain credentials, a path, query string, or fragment", name)
	}

	parsed.Path = ""
	return parsed.String(), nil
}
