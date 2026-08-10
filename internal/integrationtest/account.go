package integrationtest

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

var accountSequence atomic.Uint64

// Account is a temporary authenticated account on the real PDS.
type Account struct {
	Client   *atclient.APIClient
	DID      string
	Handle   string
	Password string
	Email    string
}

// CreateAccount creates a unique real account and immediately registers admin cleanup.
func CreateAccount(t *testing.T, test *Test) *Account {
	t.Helper()

	sequence := accountSequence.Add(1)
	seed := fmt.Sprintf("%s:%s:%s:%d", t.Name(), RunID(), instanceID(), sequence)
	digest := sha256.Sum256([]byte(seed))
	handle := fmt.Sprintf("it-%x-%d.test", digest[:4], sequence)
	password := fmt.Sprintf("%x", sha256.Sum256([]byte("password:"+handle+":"+RunID())))
	email := strings.TrimSuffix(handle, ".test") + "@integration.test"

	client := atclient.NewAPIClient(test.PDS)
	client.Client = test.HTTPClient

	operationCtx, finishCreate := test.StartOperation(test.Context, "test.fixture.account.create")
	ctx, cancel := context.WithTimeout(operationCtx, requestTimeout)
	defer cancel()

	created, err := comatproto.ServerCreateAccount(ctx, client, &comatproto.ServerCreateAccount_Input{
		Email:    &email,
		Handle:   handle,
		Password: &password,
	})
	if err != nil {
		finishCreate(err)
		t.Fatalf("create PDS account %s: %v", handle, err)
	}

	t.Cleanup(func() {
		cleanupRoot := context.WithoutCancel(test.Context)
		operationCtx, finishDelete := test.StartOperation(cleanupRoot, "test.fixture.account.delete")
		cleanupCtx, cleanupCancel := context.WithTimeout(operationCtx, 10*time.Second)
		defer cleanupCancel()
		admin := atclient.NewAdminClient(test.PDS, test.adminPassword)
		admin.Client = test.HTTPClient
		cleanupErr := comatproto.AdminDeleteAccount(cleanupCtx, admin, &comatproto.AdminDeleteAccount_Input{Did: created.Did})
		finishDelete(cleanupErr)
		if cleanupErr != nil {
			t.Errorf("delete integration account %s: %v", created.Did, cleanupErr)
		}
	})

	did, err := syntax.ParseDID(created.Did)
	if err != nil {
		finishCreate(err)
		t.Fatalf("parse created account DID: %v", err)
	}

	client.Auth = &atclient.PasswordAuth{Session: atclient.PasswordSessionData{
		AccessToken: created.AccessJwt, RefreshToken: created.RefreshJwt, AccountDID: did, Host: client.Host,
	}}
	client.AccountDID = &did
	client.Headers.Set("User-Agent", "atproto-dev-integration-test/1.0")

	account := &Account{Client: client, DID: created.Did, Handle: created.Handle, Password: password, Email: email}
	finishCreate(nil)

	return account
}
