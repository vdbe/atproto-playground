//go:build integration

package integration_test

import (
	"atproto-dev/internal/bot"
	"atproto-dev/internal/integrationtest"
	"context"
	"fmt"
	"os"
	"testing"
)

var (
	integrationSuite *integrationtest.Suite
	botTelemetry     *bot.Instruments
)

func TestMain(m *testing.M) {
	var err error
	integrationSuite, err = integrationtest.NewSuite(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "initialize integration suite: %v\n", err)
		os.Exit(1)
	}
	botTelemetry, err = bot.NewInstruments(
		"atproto-dev/internal/bot",
		integrationSuite.TracerProvider(),
		integrationSuite.MeterProvider(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "initialize bot telemetry: %v\n", err)
		if closeErr := integrationSuite.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "close integration suite after setup failure: %v\n", closeErr)
		}
		os.Exit(1)
	}

	code := m.Run()
	if err := integrationSuite.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "close integration suite: %v\n", err)
		code = 1
	}
	os.Exit(code)
}
