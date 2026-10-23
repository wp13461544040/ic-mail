package store_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestReconcileAppleDirectory(t *testing.T) { runDirectoryReconciliation(t, "") }

func TestReconcileAppleDirectoryPostgres(t *testing.T) {
	dsn := os.Getenv("ICLOUD_DIRECTORY_TEST_DSN")
	if dsn == "" {
		t.Skip("ICLOUD_DIRECTORY_TEST_DSN is not set")
	}
	if !strings.Contains(dsn, "127.0.0.1") || !strings.Contains(dsn, "test") {
		t.Fatal("expected disposable loopback test database")
	}
	runDirectoryReconciliation(t, dsn)
}

func runDirectoryReconciliation(t *testing.T, dsn string) {
	db, _, account, aliases, client := poolFixture(t, 3, dsn)
	ctx := context.Background()
	leases, err := db.ClaimPool(ctx, client.ID, store.PoolClaim{RequestID: "directory-test-used-0001", Count: 2, TTLSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	for i := range leases {
		leases[i], err = db.ActPoolLease(ctx, leases[i].ID, client.ID, "commit", 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	operation := "directory-retirement-0001"
	if _, _, err := db.StartPoolAliasRetirement(ctx, client.ID, operation, "fixture", []store.PoolAliasRetirementItemInput{{AliasID: leases[1].AliasID, LeaseID: leases[1].ID}}); err != nil {
		t.Fatal(err)
	}
	var keep domain.Alias
	for _, alias := range aliases {
		if alias.ID != leases[0].AliasID && alias.ID != leases[1].AliasID {
			keep = alias
		}
	}
	started := time.Now().Add(time.Second)
	for attempt := range 2 {
		removed, err := db.ReconcileAppleDirectory(ctx, account, started, []string{keep.Address})
		want := 2
		if attempt > 0 {
			want = 0
		}
		if err != nil || removed != want {
			t.Fatalf("removed=%d want=%d err=%v", removed, want, err)
		}
	}
	for _, lease := range leases {
		got, err := db.GetPoolLease(ctx, lease.ID, client.ID)
		if err != nil || got.State != "retired" || got.AliasID != 0 || got.Address != lease.Address || got.RequestID != lease.RequestID {
			t.Fatalf("lease history not closed/preserved: %+v err=%v", got, err)
		}
		if _, err := db.GetAlias(ctx, lease.AliasID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing alias remains: %v", err)
		}
		for _, action := range []string{"renew", "release", "commit"} {
			if _, err := db.ActPoolLease(ctx, lease.ID, client.ID, action, 600); !errors.Is(err, store.ErrPoolClosed) {
				t.Fatalf("retired %s: %v", action, err)
			}
		}
	}
	job, err := db.GetPoolAliasRetirementJob(ctx, operation, client.ID)
	if err != nil || job.Status != "completed" || job.Items[0].State != "retired" || job.Items[0].ResultCode != "APPLE_DIRECTORY_REMOVED" {
		t.Fatalf("job not reconciled: %+v err=%v", job, err)
	}
	got, err := db.GetAccount(ctx, account.ID)
	if err != nil || got.AliasCount != 1 {
		t.Fatalf("single real count=%d err=%v", got.AliasCount, err)
	}
	var members int
	if err := db.DB().QueryRowContext(ctx, `SELECT count(*) FROM pool_members`).Scan(&members); err != nil || members != 1 {
		t.Fatalf("members=%d err=%v", members, err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileAppleDirectoryRollbackAndIdentity(t *testing.T) {
	db, _, account, aliases, client := poolFixture(t, 2, "")
	ctx := context.Background()
	leases, err := db.ClaimPool(ctx, client.ID, store.PoolClaim{RequestID: "directory-rollback-0001", Count: 2, TTLSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	for i := range leases {
		leases[i], err = db.ActPoolLease(ctx, leases[i].ID, client.ID, "commit", 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	wrong := account
	wrong.IMAPUsername = "changed@example.com"
	if _, err := db.ReconcileAppleDirectory(ctx, wrong, time.Now(), nil); !errors.Is(err, store.ErrAccountIdentityLocked) {
		t.Fatalf("identity accepted: %v", err)
	}
	if _, err := db.DB().ExecContext(ctx, `CREATE TRIGGER reject_directory_delete BEFORE DELETE ON aliases WHEN OLD.id=`+strconv.FormatInt(aliases[1].ID, 10)+` BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReconcileAppleDirectory(ctx, account, time.Now(), nil); err == nil {
		t.Fatal("expected rollback")
	}
	for _, lease := range leases {
		got, err := db.GetPoolLease(ctx, lease.ID, client.ID)
		if err != nil || got.State != "used" || got.AliasID != lease.AliasID {
			t.Fatal("partial lease retirement escaped transaction")
		}
	}
	got, err := db.GetAccount(ctx, account.ID)
	if err != nil || got.AliasCount != 2 {
		t.Fatal("partial deletion escaped transaction")
	}
}

func TestReconcileAppleDirectoryProtectsOtherAccountsNewAndPending(t *testing.T) {
	db, _, account, aliases, _ := poolFixture(t, 1, "")
	ctx := context.Background()
	other := createAccount(t, ctx, db, "other", "other-directory@icloud.com")
	if _, err := db.CreateAlias(ctx, domain.Alias{AccountID: other.ID, Address: "unrelated@icloud.com", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	web := domain.AppleWebSession{AccountID: account.ID, AppleID: account.Email, Ciphertext: "fixture", Authenticated: true}
	pending, _, err := db.CreateAliasWithPendingAPIKey(ctx, web, domain.Alias{AccountID: account.ID, Address: "young-pending@icloud.com", CreationChannel: "apple_account", APIKeyHash: []byte("fixture"), APIKeyPrefix: "fixture"}, "fixture-original-key")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := db.CreateAlias(ctx, domain.Alias{AccountID: account.ID, Address: "created-after-read@icloud.com", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	removed, err := db.ReconcileAppleDirectory(ctx, account, started, nil)
	if err != nil || removed != 1 {
		t.Fatalf("cleanup=%d err=%v", removed, err)
	}
	if _, err := db.GetAlias(ctx, aliases[0].ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("phantom not removed")
	}
	if _, err := db.GetAlias(ctx, pending.ID); err != nil {
		t.Fatal("young candidate removed before confirmation")
	}
	got, err := db.GetAccount(ctx, other.ID)
	if err != nil || got.AliasCount != 1 {
		t.Fatal("cross-account cleanup")
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE aliases SET created_at=? WHERE id=?`, started.Add(-25*time.Hour).UnixNano(), pending.ID); err != nil {
		t.Fatal(err)
	}
	removed, err = db.ReconcileAppleDirectory(ctx, account, started, nil)
	if err != nil || removed != 1 {
		t.Fatalf("stale candidate not removed: %d %v", removed, err)
	}
}
