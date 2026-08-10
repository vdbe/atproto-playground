// Package integrationtest provides real-PDS test setup for opt-in integration tests.
package integrationtest

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

const requestTimeout = 10 * time.Second

func configuredPDS() (string, error) {
	value := firstNonempty(os.Getenv("PDS_TEST_URL"), os.Getenv("ACCOUNT_PDS"), "http://localhost:2583")
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("PDS_TEST_URL or ACCOUNT_PDS must be an HTTP(S) origin, got %q", value)
	}
	if parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("PDS URL must not contain credentials, a path, query, or fragment: %q", value)
	}
	parsed.Path = ""
	return parsed.String(), nil
}

func requireHealthy(tb testing.TB, test *Test) {
	tb.Helper()

	operationCtx, finish := test.StartOperation(test.Context, "test.setup.pds.readiness")
	ctx, cancel := context.WithTimeout(operationCtx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, test.PDS+"/xrpc/_health", http.NoBody)
	if err != nil {
		finish(err)
		tb.Fatalf("create PDS health request: %v", err)
	}

	resp, err := test.HTTPClient.Do(req)
	if err != nil {
		finish(err)
		tb.Fatalf("PDS is not ready at %s/xrpc/_health; start deployments/compose.yaml: %v", test.PDS, err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			tb.Errorf("close PDS health response: %v", closeErr)
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err = fmt.Errorf("PDS health status %s", resp.Status)
		finish(err)
		tb.Fatalf("PDS is not ready at %s/xrpc/_health: %v", test.PDS, err)
	}

	finish(nil)
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	panic(fmt.Sprintf("firstNonempty called without a nonempty value: %q", values))
}
