//go:build integration

package integration_test

import (
	"atproto-dev/internal/integrationtest"
	"errors"
	"net/http"
	"net/mail"
	"testing"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

func TestIntegrationFrameworkAccountLifecycle(t *testing.T) {
	t.Parallel()

	test := integrationSuite.Setup(t)
	var accounts [2]*integrationtest.Account
	if !t.Run("create authenticate and clean up", func(t *testing.T) {
		first := integrationtest.CreateAccount(t, test)
		second := integrationtest.CreateAccount(t, test)

		for _, account := range []*integrationtest.Account{first, second} {
			validateIntegrationAccount(t, account)

			session, err := comatproto.ServerGetSession(test.Context, account.Client)
			if err != nil {
				t.Fatalf("get session for %s: %v", account.Handle, err)
			}
			if session.Did != account.DID || session.Handle != account.Handle {
				t.Fatalf("get session identity = DID %q, handle %q; want DID %q, handle %q", session.Did, session.Handle, account.DID, account.Handle)
			}
			if session.Email == nil || *session.Email != account.Email {
				t.Fatalf("get session email = %v, want %q", session.Email, account.Email)
			}
			if session.Active == nil || !*session.Active {
				t.Fatalf("get session active = %v, want true", session.Active)
			}

			unauthenticated := newUnauthenticatedClient(test)
			created, err := comatproto.ServerCreateSession(test.Context, unauthenticated, &comatproto.ServerCreateSession_Input{
				Identifier: account.Handle,
				Password:   account.Password,
			})
			if err != nil {
				t.Fatalf("create session for %s: %v", account.Handle, err)
			}
			if created.Did != account.DID || created.Handle != account.Handle {
				t.Fatalf("created session identity = DID %q, handle %q; want DID %q, handle %q", created.Did, created.Handle, account.DID, account.Handle)
			}
		}

		if first.DID == second.DID || first.Handle == second.Handle || first.Email == second.Email || first.Password == second.Password {
			t.Fatal("created accounts are not unique")
		}

		accounts = [2]*integrationtest.Account{first, second}
	}) {
		return
	}
	if accounts[0] == nil || accounts[1] == nil {
		t.Fatal("successful account lifecycle child did not capture both accounts")
	}

	unauthenticated := newUnauthenticatedClient(test)
	for _, account := range accounts {
		_, err := comatproto.RepoDescribeRepo(test.Context, unauthenticated, account.DID)
		var apiErr *atclient.APIError
		if !errors.As(err, &apiErr) || apiErr.Name != "RepoNotFound" || apiErr.StatusCode != http.StatusBadRequest {
			t.Errorf("describe deleted repo %s error = %v, want RepoNotFound with HTTP 400", account.DID, err)
		}
	}
}

func validateIntegrationAccount(t *testing.T, account *integrationtest.Account) {
	t.Helper()

	if account == nil || account.Client == nil || account.DID == "" || account.Handle == "" || account.Password == "" || account.Email == "" {
		t.Fatalf("created account has missing fields: %+v", account)
	}
	if _, err := syntax.ParseDID(account.DID); err != nil {
		t.Fatalf("parse account DID %q: %v", account.DID, err)
	}
	if _, err := syntax.ParseHandle(account.Handle); err != nil {
		t.Fatalf("parse account handle %q: %v", account.Handle, err)
	}

	address, err := mail.ParseAddress(account.Email)
	if err != nil || address.Address != account.Email {
		t.Fatalf("parse account email %q: address=%v err=%v", account.Email, address, err)
	}
}

func newUnauthenticatedClient(test *integrationtest.Test) *atclient.APIClient {
	client := atclient.NewAPIClient(test.PDS)
	client.Client = test.HTTPClient

	return client
}
