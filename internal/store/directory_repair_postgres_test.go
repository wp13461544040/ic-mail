package store_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestDirectoryRepairPostgres(t *testing.T) {
	dsn := os.Getenv("ICLOUD_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("ICLOUD_TEST_POSTGRES_URL is not set")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() != "127.0.0.1" || !strings.Contains(u.Path, "test") {
		t.Fatal("expected disposable loopback test database")
	}
	ctx := context.Background()
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	suffix := fmt.Sprint(time.Now().UnixNano())
	account := createAccount(t, ctx, db, "directory repair", "repair-"+suffix+"@icloud.com")
	session := domain.AppleWebSession{AccountID: account.ID, AppleID: account.Email, Ciphertext: "fixture", Authenticated: true}
	alias, _, err := db.CreateAliasWithPendingAPIKey(ctx, session, domain.Alias{AccountID: account.ID, Address: "candidate-" + suffix + "@icloud.com", CreationChannel: "apple_account", APIKeyHash: []byte("fixture"), APIKeyPrefix: "fixture"}, "original-ciphertext")
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(time.Second)
	for range 2 {
		if err := db.ReviewStaleAccountAliasCandidate(ctx, account.ID, alias.ID, alias.Address, cutoff); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetAccount(ctx, account.ID)
	if err != nil || got.AliasCount != 1 {
		t.Fatalf("PostgreSQL count=%d err=%v", got.AliasCount, err)
	}
	if _, err := db.GetPendingAutoAliasConfirmation(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("PostgreSQL review still gates: %v", err)
	}
	confirmed, err := db.ConfirmReviewedAliasFromDirectory(ctx, account.ID, alias.Address)
	if err != nil || confirmed.ID != alias.ID || !confirmed.Enabled {
		t.Fatalf("PostgreSQL restore: %v", err)
	}
	keys, err := db.ListPendingAliasAPIKeysByAccount(ctx, account.ID)
	if err != nil || len(keys) != 1 || keys[0].APIKeyCiphertext != "original-ciphertext" {
		t.Fatal("PostgreSQL lost original key")
	}
}
