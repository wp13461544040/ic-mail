package httpserver

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"icloud-api/internal/hmesync"
	mailfetch "icloud-api/internal/mail"
	"icloud-api/internal/store"
	"icloud-api/internal/syncer"
)

func TestPoolRetirementDuringTLSIMAPFetchKeepsTaggedSurvivor(t *testing.T) {
	env := newAdminAPITestEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, key, lease := poolAliasRetirementUsedLease(t, env)
	retiring, err := env.store.GetAlias(ctx, lease.AliasID)
	if err != nil {
		t.Fatal(err)
	}
	survivor, _ := createV2AliasFixture(t, env, retiring.AccountID, "survivor@example.test")
	literal, _ := createV2AliasFixture(t, env, retiring.AccountID, "survivor+literal@example.test")
	if err := env.store.ConfigureMailArchive(t.TempDir(), 64<<20); err != nil {
		t.Fatal(err)
	}
	user := imapmemserver.NewUser("fixture-owner@example.test", "fixture-password")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	backend := imapmemserver.New()
	backend.AddUser(user)
	trace := &demandIMAPTrace{}
	bodyEntered, releaseBody := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseBody) }) }
	defer release()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", demandFixtureTLS)
	if err != nil {
		t.Fatal(err)
	}
	server := imapserver.New(&imapserver.Options{Logger: log.New(io.Discard, "", 0),
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &demandIMAPSession{Session: backend.NewSession(), trace: trace, beforeBody: func() {
				enteredOnce.Do(func() { close(bodyEntered) })
				select {
				case <-releaseBody:
				case <-ctx.Done():
				}
			}}, nil, nil
		}})
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() { release(); _ = listener.Close(); _ = server.Close(); <-done })
	account, err := env.store.GetAccount(ctx, retiring.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	account.IMAPHost, account.IMAPPort, account.IMAPUsername = "127.0.0.1", listener.Addr().(*net.TCPAddr).Port, "fixture-owner@example.test"
	account.PasswordCiphertext, err = env.cipher.Encrypt("fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	if account, err = env.store.UpdateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ address, code string }{
		{retiring.Address, "111111"}, {"survivor+task@example.test", "222222"},
		{literal.Address, "333333"}, {"unknown+task@example.test", "444444"}, {"survivor+task@other.test", "555555"},
	} {
		raw := "From: sender@example.test\r\nX-Original-To: " + item.address + "\r\nDelivered-To: fixture-owner@example.test\r\nTo: " + item.address + "\r\nSubject: Verification code\r\nContent-Type: text/plain\r\n\r\nCode " + item.code + "\r\n"
		if _, err := user.Append("INBOX", strings.NewReader(raw), &imap.AppendOptions{Time: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	fetcher := mailfetch.NewFetcher()
	fetcher.ArchiveTempDir = env.store.MailArchiveTempDir()
	manager := syncer.New(env.store, env.cipher, fetcher, env.server.logger, time.Minute, 2)
	env.server.SetAccountLocker(manager.WithAccountLock)
	router, err := env.server.Router()
	if err != nil {
		t.Fatal(err)
	}
	synced := make(chan error, 1)
	go func() { synced <- manager.SyncActiveAccount(ctx, account.ID) }()
	select {
	case <-bodyEntered:
	case err := <-synced:
		t.Fatalf("IMAP finished without reading matched bodies: %v", err)
	case <-ctx.Done():
		t.Fatal("TLS IMAP did not start body read")
	}
	if err := env.store.TouchAliasAccess(ctx, survivor.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	survivor.Label = "metadata changes during fetch"
	if _, err := env.store.UpdateAlias(ctx, survivor); err != nil {
		t.Fatal(err)
	}
	job, _, err := env.store.StartPoolAliasRetirement(ctx, client.ID, "tls-imap-retirement-0001", "fixture", []store.PoolAliasRetirementItemInput{{AliasID: lease.AliasID, LeaseID: lease.ID}})
	if err != nil {
		t.Fatal(err)
	}
	response := serveV2Request(router, http.MethodGet, "/api/v1/pool/leases/"+lease.ID+"/code", "", map[string]string{"Authorization": "Bearer " + key})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "LEASE_CLOSED") {
		t.Fatalf("retiring lease remained readable: status=%d", response.Code)
	}
	env.server.SetHMESyncService(&fakeHMESyncService{deleteAliases: func(context.Context, []int64) ([]hmesync.AliasDeletionOutcome, error) {
		return []hmesync.AliasDeletionOutcome{{AliasID: lease.AliasID}}, nil
	}})
	retired := make(chan struct{})
	go func() { defer close(retired); env.server.runPoolAliasRetirementJob(ctx, client, job) }()
	select {
	case <-retired:
	case <-ctx.Done():
		t.Fatal("retirement blocked behind IMAP body read")
	}
	if _, err := env.store.GetAlias(ctx, lease.AliasID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("retirement did not finalize during IMAP network read")
	}
	release()
	if err := <-synced; err != nil {
		t.Fatalf("surviving batch discarded: %v", err)
	}
	for _, item := range []struct {
		id   int64
		code string
	}{{survivor.ID, "222222"}, {literal.ID, "333333"}} {
		rows, err := env.store.ListAliasOTPs(ctx, item.id, 10)
		if err != nil || len(rows) != 1 || rows[0].OTP != item.code {
			t.Fatalf("recipient routing failed: count=%d error=%v", len(rows), err)
		}
	}
	response = serveV2Request(router, http.MethodGet, "/api/v1/pool/leases/"+lease.ID+"/code", "", map[string]string{"Authorization": "Bearer " + key})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "LEASE_CLOSED") {
		t.Fatalf("retired lease reopened: status=%d", response.Code)
	}
	state, err := env.store.GetIMAPSyncState(ctx, account.ID)
	if err != nil || state.LastUID != 5 {
		t.Fatalf("cursor did not advance across unrelated headers: %v", err)
	}
	var archiveCount int
	if err := env.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM archived_messages WHERE account_id = ?`, account.ID).Scan(&archiveCount); err != nil || archiveCount != 2 {
		t.Fatalf("retired/unrelated mail leaked into archive: count=%d error=%v", archiveCount, err)
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.logins != 1 || len(trace.bodies) != 3 {
		t.Fatalf("unexpected upstream work: logins=%d bodies=%d", trace.logins, len(trace.bodies))
	}
}
