package store_test

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestWebMailProjectionUpgradesToIMAPMIME(t *testing.T) {
	for _, targeted := range []bool{false, true} {
		name := "account"
		if targeted {
			name = "alias"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db, account, alias, _ := aliasIMAPSyncFixture(t)
			account, err := db.GetAccount(ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC()
			result := aliasIMAPSyncResult(alias, 91, 9, at)
			projection := []byte("Message-ID: <same-message@example.test>\r\nX-Mail-Archive-Source: icloud-web-text-parts\r\nContent-Type: text/plain\r\n\r\nweb text")
			result.ArchivedMessages[0].MessageID = "<same-message@example.test>"
			result.ArchivedMessages[0].RawMIME = projection
			result.ArchivedMessages[0].BodyTruncated = true
			if err := db.ApplyAliasWebMailboxSync(ctx, account.UpdatedAt, alias, result, at); err != nil {
				t.Fatal(err)
			}
			before := oneArchivedMailboxMessage(t, ctx, db, alias.ID)
			full := []byte("Message-ID: <same-message@example.test>\r\nContent-Type: text/plain\r\nX-Upstream-Fixture: complete\r\n\r\ncomplete MIME including attachment fixture")
			file, err := os.CreateTemp(db.MailArchiveTempDir(), "imap-fixture-*")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(full); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			result.ArchivedMessages[0].RawMIME = nil
			result.ArchivedMessages[0].RawMIMEPath = file.Name()
			result.ArchivedMessages[0].RawSize = int64(len(full))
			result.ArchivedMessages[0].BodyTruncated = false
			result.Reset = true
			current, err := db.GetAccount(ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if targeted {
				err = db.ApplyAliasMailboxSync(ctx, current.UpdatedAt, alias, result, at)
			} else {
				err = db.ApplyObservedMailboxSync(ctx, current, []domain.Alias{alias}, result, at)
			}
			if err != nil {
				t.Fatalf("upgrade of same UID from web projection to full MIME failed: %v", err)
			}
			after := oneArchivedMailboxMessage(t, ctx, db, alias.ID)
			if after.ID != before.ID || after.MailboxUID != before.MailboxUID || after.BodyTruncated || after.ContentBytes != int64(len(full)) {
				t.Fatal("upgrade changed mailbox identity or kept projection metadata")
			}
			content, err := db.ReadArchivedContent(after)
			if err != nil || !bytes.Equal(content, full) {
				t.Fatal("complete IMAP MIME was not published")
			}
			oldPath := filepath.Join(db.MailArchiveTempDir(), "..", filepath.FromSlash(before.ContentPath))
			if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
				t.Fatal("superseded projection file was not cleaned after commit")
			}
			result.ArchivedMessages[0].RawMIMEPath = ""
			result.ArchivedMessages[0].RawMIME = full
			result.Reset = false
			current, err = db.GetAccount(ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if targeted {
				err = db.ApplyAliasMailboxSync(ctx, current.UpdatedAt, alias, result, at)
			} else {
				err = db.ApplyObservedMailboxSync(ctx, current, []domain.Alias{alias}, result, at)
			}
			if err != nil {
				t.Fatalf("full MIME replay failed: %v", err)
			}
			replayed := oneArchivedMailboxMessage(t, ctx, db, alias.ID)
			if replayed.ID != after.ID || replayed.MailboxUID != after.MailboxUID || replayed.ContentPath != after.ContentPath {
				t.Fatal("replay changed archive identity")
			}
			current, err = db.GetAccount(ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			result.ArchivedMessages[0].RawMIME = projection
			result.ArchivedMessages[0].BodyTruncated = true
			if err := db.ApplyAliasWebMailboxSync(ctx, current.UpdatedAt, alias, result, at); err != nil {
				t.Fatalf("Web replay after full MIME: %v", err)
			}
			kept := oneArchivedMailboxMessage(t, ctx, db, alias.ID)
			if kept.ContentPath != after.ContentPath || kept.ContentSHA256 != after.ContentSHA256 || kept.BodyTruncated {
				t.Fatal("Web replay downgraded full MIME")
			}
		})
	}
}

func TestArchiveTransportUpgradeKeepsConflictAndRollbackBoundaries(t *testing.T) {
	for _, scenario := range []string{"different_message_id", "empty_message_id", "original_mime", "corrupt_projection", "stale_account", "duplicate_uid"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			db, account, alias, _ := aliasIMAPSyncFixture(t)
			account, err := db.GetAccount(ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC()
			result := aliasIMAPSyncResult(alias, 91, 9, at)
			projection := []byte("Message-ID: <same@example.test>\r\nX-Mail-Archive-Source: icloud-web-text-parts\r\n\r\nweb text")
			if scenario == "original_mime" {
				projection = []byte("Message-ID: <same@example.test>\r\n\r\noriginal immutable MIME")
			}
			result.ArchivedMessages[0].MessageID = "<same@example.test>"
			result.ArchivedMessages[0].RawMIME = projection
			if err := db.ApplyAliasWebMailboxSync(ctx, account.UpdatedAt, alias, result, at); err != nil {
				t.Fatal(err)
			}
			before := oneArchivedMailboxMessage(t, ctx, db, alias.ID)
			oldPath := filepath.Join(db.MailArchiveTempDir(), "..", filepath.FromSlash(before.ContentPath))
			if scenario == "corrupt_projection" {
				projection = append([]byte(nil), projection...)
				projection[len(projection)-1] ^= 1
				if err := os.WriteFile(oldPath, projection, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			current, err := db.GetAccount(ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			result.ArchivedMessages[0].RawMIME = []byte("Message-ID: <same@example.test>\r\n\r\ncomplete IMAP MIME with different length")
			switch scenario {
			case "different_message_id":
				result.ArchivedMessages[0].MessageID = "<different@example.test>"
			case "empty_message_id":
				result.ArchivedMessages[0].MessageID = ""
			case "stale_account":
				current.UpdatedAt = current.UpdatedAt.Add(-time.Second)
			case "duplicate_uid":
				duplicate := result.ArchivedMessages[0]
				duplicate.RawMIME = append(append([]byte(nil), duplicate.RawMIME...), []byte("different")...)
				result.ArchivedMessages = append(result.ArchivedMessages, duplicate)
			}
			result.Reset = true
			err = db.ApplyMailboxSync(ctx, account.ID, current.UpdatedAt, []domain.Alias{alias}, result, at)
			if scenario == "stale_account" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("conflicting or corrupt archive was accepted")
			}
			after := oneArchivedMailboxMessage(t, ctx, db, alias.ID)
			if after.ContentPath != before.ContentPath || after.ContentSHA256 != before.ContentSHA256 || after.MailboxUID != before.MailboxUID {
				t.Fatal("rejected upgrade modified committed archive identity")
			}
			stored, err := os.ReadFile(oldPath)
			if err != nil || !bytes.Equal(stored, projection) {
				t.Fatal("rejected upgrade changed previous file")
			}
			files, err := filepath.Glob(filepath.Join(filepath.Dir(oldPath), "*-imap-*.eml"))
			if err != nil || len(files) != 0 {
				t.Fatal("failed upgrade left staged MIME files")
			}
		})
	}
}

func TestPostgresWebMailProjectionUpgrade(t *testing.T) {
	dsn := os.Getenv("ICLOUD_ARCHIVE_UPGRADE_TEST_DSN")
	if dsn == "" {
		t.Skip("set ICLOUD_ARCHIVE_UPGRADE_TEST_DSN to an isolated fixture database")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() != "127.0.0.1" || !strings.Contains(u.Path, "test") {
		t.Fatal("expected isolated loopback test database")
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ConfigureMailArchive(t.TempDir(), 1<<20); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	account := createAccount(t, ctx, db, "archive upgrade fixture", "archive-upgrade@icloud.com")
	alias := createLegacyAlias(t, ctx, db, account.ID, "archive-upgrade-alias@icloud.com", bytes.Repeat([]byte{0x71}, 32))
	account, err = db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	result := aliasIMAPSyncResult(alias, 91, 9, at)
	result.ArchivedMessages[0].MessageID = "<pg-upgrade@example.test>"
	result.ArchivedMessages[0].RawMIME = []byte("Message-ID: <pg-upgrade@example.test>\r\nX-Mail-Archive-Source: icloud-web-text-parts\r\n\r\nprojection")
	if err := db.ApplyAliasWebMailboxSync(ctx, account.UpdatedAt, alias, result, at); err != nil {
		t.Fatal(err)
	}
	before := oneArchivedMailboxMessage(t, ctx, db, alias.ID)
	account, err = db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	full := []byte("Message-ID: <pg-upgrade@example.test>\r\n\r\nfull MIME archive fixture with attachment")
	result.ArchivedMessages[0].RawMIME = full
	if err := db.ApplyObservedMailboxSync(ctx, account, []domain.Alias{alias}, result, at); err != nil {
		t.Fatal(err)
	}
	after := oneArchivedMailboxMessage(t, ctx, db, alias.ID)
	content, err := db.ReadArchivedContent(after)
	if err != nil || !bytes.Equal(content, full) || before.ID != after.ID || before.MailboxUID != after.MailboxUID {
		t.Fatal("PostgreSQL upgrade did not preserve identity and publish complete content")
	}
}
