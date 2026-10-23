package store_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestCreationCandidatesSQLitePersistenceAndIsolation(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "candidates.db")
	checkCreationCandidatePersistence(t, dsn)
}

func TestCreationCandidatesPostgresPersistenceAndIsolation(t *testing.T) {
	dsn := os.Getenv("ICLOUD_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("ICLOUD_TEST_POSTGRES_URL is not set")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() != "127.0.0.1" || !strings.Contains(u.Path, "test") {
		t.Fatal("expected disposable loopback test database")
	}
	checkCreationCandidatePersistence(t, dsn)
}

func checkCreationCandidatePersistence(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	suffix := fmt.Sprint(time.Now().UnixNano())
	a := createAccount(t, ctx, db, "candidate isolation", "candidate-"+suffix+"@icloud.com")
	other := createAccount(t, ctx, db, "candidate other", "other-"+suffix+"@icloud.com")
	session := domain.AppleWebSession{AccountID: a.ID, AppleID: a.Email, Ciphertext: "fixture", Authenticated: true}
	create := func(channel, remoteID string) domain.Alias {
		t.Helper()
		address := fmt.Sprintf("candidate-%s-%d@icloud.com", suffix, time.Now().UnixNano())
		alias, _, err := db.CreateAliasWithPendingAPIKey(ctx, session, domain.Alias{AccountID: a.ID, Address: address,
			APIKeyHash: []byte(address), APIKeyPrefix: "fixture", CreationChannel: channel, CreationRemoteID: remoteID}, "encrypted-fixture")
		if err != nil {
			t.Fatal(err)
		}
		return alias
	}
	acc := create("apple_account", "exact-id")
	web := create("icloud_web", "")
	until := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Microsecond)
	if err := db.PauseAppleChannelCreation(ctx, a.ID, "apple_account", until); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for channel, id := range map[string]int64{"apple_account": acc.ID, "icloud_web": web.ID} {
		pending, err := db.GetPendingAutoAliasConfirmationForChannel(ctx, a.ID, channel)
		if err != nil || pending.ID != id || pending.CreationChannel != channel {
			t.Fatalf("channel provenance lost: %s %v", channel, err)
		}
		if channel == "apple_account" && pending.CreationRemoteID != "exact-id" {
			t.Fatal("remote ID lost on reopen")
		}
		if _, err := db.GetPendingAutoAliasConfirmationForChannel(ctx, other.ID, channel); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("pending candidate crossed account boundary")
		}
	}
	if got, err := db.AppleCreationChannelPausedUntil(ctx, a.ID, "apple_account"); err != nil || !got.Equal(until) {
		t.Fatal("persistent cooldown changed on migration")
	}
	if got, err := db.AppleCreationChannelPausedUntil(ctx, a.ID, "icloud_web"); err != nil || !got.IsZero() {
		t.Fatal("ACC pause leaked into Web")
	}
	for _, attempt := range []struct {
		accountID, aliasID int64
		channel, id        string
	}{
		{other.ID, acc.ID, "apple_account", "exact-id"},
		{a.ID, acc.ID, "icloud_web", "exact-id"},
		{a.ID, acc.ID, "apple_account", "different-id"},
	} {
		if err := db.SaveAliasCreationCandidateDetail(ctx, attempt.accountID, attempt.aliasID, attempt.channel, attempt.id); err == nil {
			t.Fatal("candidate identity mismatch accepted")
		}
	}
	if err := db.SaveAliasCreationCandidateDetail(ctx, a.ID, acc.ID, "apple_account", "exact-id"); err != nil {
		t.Fatal(err)
	}
	// Legacy candidates with no provenance continue to gate both channels.
	if _, _, err := db.ConfirmPendingAutoAlias(ctx, session, acc.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ConfirmPendingAutoAlias(ctx, session, web.ID); err != nil {
		t.Fatal(err)
	}
	legacy := create("", "")
	for _, channel := range []string{"apple_account", "icloud_web"} {
		p, err := db.GetPendingAutoAliasConfirmationForChannel(ctx, a.ID, channel)
		if err != nil || p.ID != legacy.ID || p.CreationChannel != "" {
			t.Fatalf("legacy unknown candidate bypassed: %v", err)
		}
	}
}
