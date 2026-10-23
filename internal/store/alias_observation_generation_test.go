package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"icloud-api/internal/domain"
)

func TestAliasObservationRejectsEnableABAWithFrozenClock(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.now = func() time.Time { return time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC) }
	ctx := context.Background()
	account, err := db.CreateAccount(ctx, domain.Account{Email: "primary@icloud.com", IMAPHost: "imap.mail.me.com", IMAPPort: 993, IMAPUsername: "primary@icloud.com", PasswordCiphertext: "fixture", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	alias, err := db.CreateAlias(ctx, domain.Alias{AccountID: account.ID, Address: "root@icloud.com", APIKeyHash: []byte("01234567890123456789012345678901"), Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	account, err = db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAliasEnabled(ctx, alias.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAliasEnabled(ctx, alias.ID, true); err != nil {
		t.Fatal(err)
	}
	result := domain.MailboxSyncResult{Reset: true, State: domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1, LastUID: 1, UpdatedAt: db.now()}}
	if err := db.ApplyObservedAliasMailboxSync(ctx, account, alias, result, db.now(), false); !errors.Is(err, ErrAliasMailboxSyncStale) {
		t.Fatalf("enable ABA accepted stale alias observation: %v", err)
	}
}
