package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestActiveIMAPWindowCommitsWithCursorAndSurvivesReopen(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "active-window.db")
			if backend == "postgres" {
				dsn = os.Getenv("ICLOUD_MAIL_ACTIVE_WINDOW_TEST_DSN")
				if dsn == "" {
					t.Skip("set ICLOUD_MAIL_ACTIVE_WINDOW_TEST_DSN to an empty fixture database")
				}
			}
			ctx := context.Background()
			db, err := store.Open(dsn)
			if err != nil {
				t.Fatal("open active-window fixture failed")
			}
			t.Cleanup(func() { _ = db.Close() })
			accounts, err := db.ListAccounts(ctx)
			if err != nil || len(accounts) != 0 {
				t.Fatal("active-window test requires an empty fixture database")
			}
			archiveRoot := t.TempDir()
			if err := db.ConfigureMailArchive(archiveRoot, 1<<20); err != nil {
				t.Fatal(err)
			}
			account := createAccount(t, ctx, db, "Active window", "active-window@icloud.com")
			alias := createLegacyAlias(t, ctx, db, account.ID, "active-window-root@icloud.com", []byte("01234567890123456789012345678901"))
			at := time.Now().UTC().Truncate(time.Second)
			since := at.Add(-15 * time.Minute)
			result := domain.MailboxSyncResult{
				State: domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 91, LastUID: 128, UpdatedAt: at, ActiveWindowSince: since},
				Reset: true, HasMore: true,
			}
			apply := func(batch domain.MailboxSyncResult) error {
				account, err := db.GetAccount(ctx, account.ID)
				if err != nil {
					return err
				}
				return db.ApplyObservedMailboxSync(ctx, account, []domain.Alias{alias}, batch, batch.State.UpdatedAt)
			}
			if err := apply(result); err != nil {
				t.Fatal(err)
			}
			check := func(uid uint32, cutoff time.Time) {
				t.Helper()
				state, err := db.GetIMAPSyncState(ctx, account.ID)
				if err != nil || state.LastUID != uid || !state.ActiveWindowSince.Equal(cutoff) {
					t.Fatalf("cursor/window mismatch: uid=%d cutoff=%v error=%v", state.LastUID, state.ActiveWindowSince, err)
				}
			}
			check(128, since)
			failed := result
			failed.Reset = false
			failed.State.LastUID = 200
			failed.State.ActiveWindowSince = time.Unix(-1, 0)
			if err := apply(failed); err == nil {
				t.Fatal("invalid window unexpectedly committed")
			}
			check(128, since)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = store.Open(dsn)
			if err != nil {
				t.Fatal("reopen active-window fixture failed")
			}
			if err := db.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			if err := db.ConfigureMailArchive(archiveRoot, 1<<20); err != nil {
				t.Fatal(err)
			}
			check(128, since)
			result.Reset = false
			result.State.LastUID = 200
			result.State.UpdatedAt = at.Add(16 * time.Minute)
			if err := apply(result); err != nil {
				t.Fatal(err)
			}
			check(200, since)
			result.State.LastUID = 250
			result.HasMore = false
			result.State.ActiveWindowSince = time.Time{}
			if err := apply(result); err != nil {
				t.Fatal(err)
			}
			check(250, time.Time{})
			var count int
			if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM imap_active_windows`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("completed window remains: count=%d error=%v", count, err)
			}
			result.Reset, result.HasMore = true, true
			result.State.UIDValidity, result.State.LastUID = 92, 10
			result.State.ActiveWindowSince = at
			if err := apply(result); err != nil {
				t.Fatal(err)
			}
			check(10, at)
			if err := db.DeleteAccount(ctx, account.ID); err != nil {
				t.Fatal(err)
			}
			if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM imap_active_windows`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("deleted cursor left an active window: count=%d error=%v", count, err)
			}
		})
	}
}
