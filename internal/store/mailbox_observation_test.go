package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestObservedMailboxSyncFiltersChangedRecipientsOnly(t *testing.T) {
	for _, mutation := range []string{"delete", "disable", "reenable", "address"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			db, _, first, second := aliasIMAPSyncFixture(t)
			account, err := db.GetAccount(ctx, first.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC()
			result := aliasIMAPSyncResult(first, 91, 10, at)
			result.ArchivedMessages[0].AliasIDs = []int64{first.ID, second.ID}
			result.ArchivedMessages[0].RawMIME = []byte("To: fixture@icloud.com\r\n\r\nfixture")
			withdrawn := result.ArchivedMessages[0]
			withdrawn.UID, withdrawn.AliasIDs = 9, []int64{second.ID}
			result.ArchivedMessages = append(result.ArchivedMessages, withdrawn)
			result.LegacySnapshotUpdates = map[int64]domain.LatestMessage{
				first.ID:  {AliasID: first.ID, UIDValidity: 91, SnapshotState: domain.SnapshotEmpty},
				second.ID: {AliasID: second.ID, UIDValidity: 91, SnapshotState: domain.SnapshotEmpty},
			}
			switch mutation {
			case "delete":
				err = db.DeleteAlias(ctx, second.ID)
			case "disable", "reenable":
				second.Enabled = false
				_, err = db.UpdateAlias(ctx, second)
				if err == nil && mutation == "reenable" {
					second.Enabled = true
					_, err = db.UpdateAlias(ctx, second)
				}
			case "address":
				_, err = db.DB().ExecContext(ctx, `UPDATE aliases SET address = ?, updated_at = updated_at + 1 WHERE id = ?`, "changed@icloud.com", second.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			newAlias := createLegacyAlias(t, ctx, db, account.ID, "newcomer@icloud.com", []byte("01234567890123456789012345678901"))
			// Use the identities actually observed before the remote read.
			originalSecond := second
			originalSecond.Address = "alias-imap-second@icloud.com"
			originalSecond.Enabled = true
			if err := db.ApplyObservedMailboxSync(ctx, account, []domain.Alias{first, originalSecond}, result, at); err != nil {
				t.Fatal(err)
			}
			state, err := db.GetIMAPSyncState(ctx, account.ID)
			if err != nil || state.LastUID != 10 {
				t.Fatalf("cursor did not publish: %v", err)
			}
			matches, err := filepath.Glob(filepath.Join(db.MailArchiveTempDir(), "..", "account-*", "uidv-*", "uid-9.eml"))
			if err != nil || len(matches) != 0 {
				t.Fatalf("withdrawn-only body remained on disk: count=%d error=%v", len(matches), err)
			}
			messages, err := db.ListArchivedMailboxMessages(ctx, first.ID)
			if err != nil || len(messages) != 1 {
				t.Fatalf("survivor archive count=%d error=%v", len(messages), err)
			}
			if mutation != "delete" {
				messages, err = db.ListArchivedMailboxMessages(ctx, second.ID)
				if err != nil || len(messages) != 0 {
					t.Fatalf("changed recipient inherited old observation: %v", err)
				}
			}
			fresh, err := db.GetAlias(ctx, newAlias.ID)
			if err != nil || fresh.LastSyncedAt != nil || fresh.LastSyncStatus != domain.SyncStatusPending {
				t.Fatalf("unobserved newcomer marked synced: %v", err)
			}
		})
	}
}

// Run only against an explicitly supplied, empty fixture database.
func TestObservedMailboxPostgresPublicationAndConfigBoundary(t *testing.T) {
	dsn := os.Getenv("ICLOUD_MAIL_OBSERVATION_TEST_DSN")
	if dsn == "" {
		t.Skip("set ICLOUD_MAIL_OBSERVATION_TEST_DSN to an empty fixture database")
	}
	ctx := context.Background()
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatal("open fixture PostgreSQL failed")
	}
	t.Cleanup(func() { _ = db.Close() })
	accounts, err := db.ListAccounts(ctx)
	if err != nil || len(accounts) != 0 {
		t.Fatal("observation test requires an empty fixture database")
	}
	archiveDir := t.TempDir()
	if err := db.ConfigureMailArchive(archiveDir, 1<<20); err != nil {
		t.Fatal(err)
	}
	account := createAccount(t, ctx, db, "PG observation", "observation-fixture@icloud.com")
	first := createLegacyAlias(t, ctx, db, account.ID, "first@icloud.com", []byte("01234567890123456789012345678901"))
	second := createLegacyAlias(t, ctx, db, account.ID, "second@icloud.com", []byte("12345678901234567890123456789012"))
	account, err = db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	changed := account
	changed.Name = "Name change must not discard mailbox observation"
	if _, err := db.UpdateAccount(ctx, changed); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAlias(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	if err := db.ApplyObservedMailboxSync(ctx, account, []domain.Alias{first, second}, aliasIMAPSyncResult(first, 91, 10, at), at); err != nil {
		t.Fatal(err)
	}
	state, err := db.GetIMAPSyncState(ctx, account.ID)
	if err != nil || state.LastUID != 10 {
		t.Fatalf("PostgreSQL publication failed: %v", err)
	}
	account, err = db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err = db.GetAlias(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAliasEnabled(ctx, first.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAliasEnabled(ctx, first.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyObservedAliasMailboxSync(ctx, account, first, aliasIMAPSyncResult(first, 91, 11, at), at, false); !errors.Is(err, store.ErrAliasMailboxSyncStale) {
		t.Fatalf("PostgreSQL alias enable ABA accepted stale batch: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(dsn)
	if err != nil {
		t.Fatal("reopen fixture PostgreSQL failed")
	}
	if err := db.ConfigureMailArchive(archiveDir, 1<<20); err != nil {
		t.Fatal(err)
	}
	first, err = db.GetAlias(ctx, first.ID)
	if err != nil || first.ReceiveRevision != 2 {
		t.Fatalf("PostgreSQL revision migration not durable/idempotent: revision=%d error=%v", first.ReceiveRevision, err)
	}
	changed = account
	changed.PasswordCiphertext = "rotated"
	if _, err := db.UpdateAccount(ctx, changed); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyObservedAliasMailboxSync(ctx, account, first, aliasIMAPSyncResult(first, 91, 11, at), at, false); !errors.Is(err, store.ErrAliasMailboxSyncStale) {
		t.Fatalf("PostgreSQL source ABA accepted stale batch: %v", err)
	}
	if _, err := db.GetIMAPSyncState(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("PostgreSQL stale batch restored cursor: %v", err)
	}
	account, err = db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountMailTransport(ctx, account.ID, store.MailTransportWebmail); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountMailTransport(ctx, account.ID, store.MailTransportIMAP); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyObservedMailboxSync(ctx, account, []domain.Alias{first}, aliasIMAPSyncResult(first, 91, 11, at), at); !errors.Is(err, store.ErrAliasMailboxSyncStale) {
		t.Fatalf("PostgreSQL transport ABA accepted stale batch: %v", err)
	}
}

func TestObservedMailboxSyncRejectsChangedSourceAndABA(t *testing.T) {
	for _, mutation := range []string{"password", "endpoint", "username", "disabled", "reenabled", "transport_roundtrip", "password_roundtrip"} {
		for _, targeted := range []bool{false, true} {
			name := mutation + "/shared"
			if targeted {
				name = mutation + "/targeted"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				db, _, alias, second := aliasIMAPSyncFixture(t)
				account, err := db.GetAccount(ctx, alias.AccountID)
				if err != nil {
					t.Fatal(err)
				}
				changed := account
				switch mutation {
				case "password", "password_roundtrip":
					changed.PasswordCiphertext = "rotated"
				case "endpoint":
					changed.IMAPHost = "other.example.test"
				case "username":
					changed.IMAPUsername = "other"
				case "disabled", "reenabled":
					changed.Enabled = false
				case "transport_roundtrip":
					if err := db.SetAccountMailTransport(ctx, account.ID, store.MailTransportWebmail); err != nil {
						t.Fatal(err)
					}
					if err := db.SetAccountMailTransport(ctx, account.ID, store.MailTransportIMAP); err != nil {
						t.Fatal(err)
					}
				}
				if mutation != "transport_roundtrip" {
					if _, err := db.UpdateAccount(ctx, changed); err != nil {
						t.Fatal(err)
					}
					if mutation == "reenabled" || mutation == "password_roundtrip" {
						if _, err := db.UpdateAccount(ctx, account); err != nil {
							t.Fatal(err)
						}
					}
				}
				at := time.Now().UTC()
				result := aliasIMAPSyncResult(alias, 91, 10, at)
				if targeted {
					err = db.ApplyObservedAliasMailboxSync(ctx, account, alias, result, at, false)
				} else {
					err = db.ApplyObservedMailboxSync(ctx, account, []domain.Alias{alias, second}, result, at)
				}
				if !errors.Is(err, store.ErrAliasMailboxSyncStale) {
					t.Fatalf("source mutation accepted old result: %v", err)
				}
				messages, err := db.ListArchivedMailboxMessages(ctx, alias.ID)
				if err != nil || len(messages) != 0 {
					t.Fatalf("stale source archived mail: %v", err)
				}
				if _, err := db.GetIMAPSyncState(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("stale source advanced cursor: %v", err)
				}
				if err := db.RecordObservedMailboxSyncFailure(ctx, account, "stale auth failure", at); err != nil {
					t.Fatal(err)
				}
				current, err := db.GetAccount(ctx, account.ID)
				if err != nil || current.LastSyncError == "stale auth failure" {
					t.Fatalf("stale source poisoned current health: %v", err)
				}
			})
		}
	}
}

func TestObservedAliasMailboxSyncSurvivesUnrelatedDeletion(t *testing.T) {
	ctx := context.Background()
	db, _, alias, other := aliasIMAPSyncFixture(t)
	account, err := db.GetAccount(ctx, alias.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAlias(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	if err := db.ApplyObservedAliasMailboxSync(ctx, account, alias, aliasIMAPSyncResult(alias, 91, 10, at), at, false); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordObservedMailboxSyncFailure(ctx, account, "current auth failure", at); err != nil {
		t.Fatal(err)
	}
	current, err := db.GetAccount(ctx, account.ID)
	if err != nil || current.LastSyncError != "current auth failure" {
		t.Fatalf("unrelated deletion suppressed auth pause: %v", err)
	}
}

func TestObservedMailboxStaleSourceDoesNotOverwriteCurrentArchive(t *testing.T) {
	for _, targeted := range []bool{false, true} {
		name := "shared"
		if targeted {
			name = "targeted"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db, _, alias, second := aliasIMAPSyncFixture(t)
			oldSource, err := db.GetAccount(ctx, alias.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			newSource := oldSource
			newSource.IMAPHost = "replacement.example.test"
			newSource, err = db.UpdateAccount(ctx, newSource)
			if err != nil {
				t.Fatal(err)
			}
			aliases, err := db.ListEnabledAliasesByAccount(ctx, alias.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC()
			current := aliasIMAPSyncResult(alias, 91, 10, at)
			current.ArchivedMessages[0].RawMIME = []byte("To: fixture@icloud.com\r\n\r\ncurrent source body")
			if err := db.ApplyObservedMailboxSync(ctx, newSource, aliases, current, at); err != nil {
				t.Fatal(err)
			}
			stale := aliasIMAPSyncResult(alias, 91, 10, at)
			stale.ArchivedMessages[0].RawMIME = []byte("To: fixture@icloud.com\r\n\r\nold source different body")
			if targeted {
				err = db.ApplyObservedAliasMailboxSync(ctx, oldSource, alias, stale, at, false)
			} else {
				err = db.ApplyObservedMailboxSync(ctx, oldSource, []domain.Alias{alias, second}, stale, at)
			}
			// Archive identity conflicts may reject the input before the source check.
			if err == nil {
				t.Fatal("old source accepted")
			}
			messages, err := db.ListArchivedMailboxMessages(ctx, alias.ID)
			if err != nil || len(messages) != 1 {
				t.Fatalf("current archive missing: %v", err)
			}
			body, err := db.ReadArchivedContent(messages[0])
			if err != nil || string(body) != string(current.ArchivedMessages[0].RawMIME) {
				t.Fatalf("stale source damaged current archive: %v", err)
			}
		})
	}
}
