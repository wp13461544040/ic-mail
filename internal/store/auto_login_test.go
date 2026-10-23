package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/domain"
)

func createAutoLoginTestAccount(t *testing.T, db *Store) domain.Account {
	t.Helper()
	account, err := db.CreateAccount(context.Background(), domain.Account{
		Name: "Auto login", Email: "auto-login@icloud.com", IMAPHost: "imap.mail.me.com", IMAPPort: 993,
		IMAPUsername: "auto-login@icloud.com", PasswordCiphertext: "imap-ciphertext", Enabled: true,
		AutoLogin: &domain.AccountAutoLoginConfig{Enabled: true, Region: "global", CredentialsCiphertext: "login-envelope"},
	})
	if err != nil {
		t.Fatalf("create account with auto login: %v", err)
	}
	return account
}

func TestAccountAutoLoginConfigTransactionalCASAndCursorPreservation(t *testing.T) {
	ctx := context.Background()
	db := openAccountVersionTestStore(t)
	account := createAutoLoginTestAccount(t, db)
	config, err := db.GetAccountAutoLoginConfig(ctx, account.ID)
	if err != nil || config.Revision != 1 || !config.Enabled || config.Region != "global" || config.CredentialsCiphertext != "login-envelope" {
		t.Fatalf("initial config = %#v, %v", config, err)
	}
	beforeMailboxVersion := account.MailboxConfigVersion
	now := time.Now().UTC()
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO imap_sync_states(account_id,uid_validity,last_uid,updated_at) VALUES(?,91,42,?)`, account.ID, now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	state := domain.AccountAutoLoginState{AccountID: account.ID, Channel: domain.AutoLoginChannelAppleAccount,
		ConfigRevision: 1, Status: "backoff", Failures: 2, LastErrorCode: "RETRY", LastAttemptAt: &now, NextAttemptAt: &now}
	if err := db.SaveAccountAutoLoginState(ctx, state); err != nil {
		t.Fatal(err)
	}
	config.Region, config.CredentialsCiphertext = "cn", "replacement-envelope"
	account.AutoLogin = &config
	updated, err := db.UpdateAccount(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	if updated.MailboxConfigVersion != beforeMailboxVersion {
		t.Fatalf("auto login configuration changed IMAP config revision: %d -> %d", beforeMailboxVersion, updated.MailboxConfigVersion)
	}
	cursor, err := db.GetIMAPSyncState(ctx, account.ID)
	if err != nil || cursor.UIDValidity != 91 || cursor.LastUID != 42 {
		t.Fatalf("IMAP cursor changed: %#v, %v", cursor, err)
	}
	if _, err := db.GetAccountAutoLoginState(ctx, account.ID, state.Channel); !errors.Is(err, ErrNotFound) {
		t.Fatalf("configuration update retained old backoff: %v", err)
	}
	if err := db.SaveAccountAutoLoginState(ctx, state); !errors.Is(err, ErrAutoLoginConfigChanged) {
		t.Fatalf("stale worker state accepted: %v", err)
	}
	configAfter, err := db.GetAccountAutoLoginConfig(ctx, account.ID)
	if err != nil || configAfter.Revision != 2 || configAfter.Region != "cn" || configAfter.CredentialsCiphertext != "replacement-envelope" {
		t.Fatalf("updated config = %#v, %v", configAfter, err)
	}
	updated.Name = "must roll back"
	updated.AutoLogin = &config // Still revision 1.
	if _, err := db.UpdateAccount(ctx, updated); !errors.Is(err, ErrAutoLoginConfigChanged) {
		t.Fatalf("stale config update = %v", err)
	}
	unchanged, err := db.GetAccount(ctx, account.ID)
	if err != nil || unchanged.Name != account.Name {
		t.Fatalf("CAS failure partially updated account: %#v, %v", unchanged, err)
	}
	unchanged.Name = "plain edit"
	if _, err := db.UpdateAccount(ctx, unchanged); err != nil {
		t.Fatal(err)
	}
	unchangedConfig, err := db.GetAccountAutoLoginConfig(ctx, account.ID)
	if err != nil || unchangedConfig.Revision != 2 {
		t.Fatalf("nil config mutated saved config: %#v, %v", unchangedConfig, err)
	}
}

func TestAccountAutoLoginChannelsDisableAndIdentityChange(t *testing.T) {
	ctx := context.Background()
	db := openAccountVersionTestStore(t)
	account := createAutoLoginTestAccount(t, db)
	now := time.Now().UTC()
	for i, channel := range []string{domain.AutoLoginChannelAppleAccount, domain.AutoLoginChannelICloudWeb} {
		state := domain.AccountAutoLoginState{AccountID: account.ID, Channel: channel, ConfigRevision: 1,
			Status: "backoff", Failures: i + 1, LastErrorCode: "RETRY", LastSuccessAt: &now, NextAttemptAt: &now}
		if err := db.SaveAccountAutoLoginState(ctx, state); err != nil {
			t.Fatal(err)
		}
		got, err := db.GetAccountAutoLoginState(ctx, account.ID, channel)
		if err != nil || got.Failures != i+1 || got.LastSuccessAt == nil || !got.LastSuccessAt.Equal(now) || got.LastAttemptAt != nil {
			t.Fatalf("channel state = %#v, %v", got, err)
		}
	}
	configs, err := db.ListEnabledAccountAutoLoginConfigs(ctx)
	if err != nil || len(configs) != 1 {
		t.Fatalf("enabled configs = %d, %v", len(configs), err)
	}
	account.Enabled = false
	if _, err := db.UpdateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	configs, err = db.ListEnabledAccountAutoLoginConfigs(ctx)
	if err != nil || len(configs) != 0 {
		t.Fatalf("disabled account listed = %d, %v", len(configs), err)
	}
	state := domain.AccountAutoLoginState{AccountID: account.ID, Channel: domain.AutoLoginChannelAppleAccount, ConfigRevision: 1, Status: "idle"}
	if err := db.SaveAccountAutoLoginState(ctx, state); !errors.Is(err, ErrAutoLoginConfigChanged) {
		t.Fatalf("disabled account worker wrote state: %v", err)
	}
	account.Enabled = true
	if _, err := db.UpdateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	account.Email = "replacement@icloud.com"
	if _, err := db.UpdateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	config, err := db.GetAccountAutoLoginConfig(ctx, account.ID)
	if err != nil || config.Enabled || config.CredentialsCiphertext != "" || config.Revision != 2 {
		t.Fatalf("identity change retained credentials = %#v, %v", config, err)
	}
	if err := db.SaveAccountAutoLoginState(ctx, state); !errors.Is(err, ErrAutoLoginConfigChanged) {
		t.Fatalf("old identity worker wrote state: %v", err)
	}
}

func TestAccountAutoLoginValidationAndAtomicCreate(t *testing.T) {
	ctx := context.Background()
	db := openAccountVersionTestStore(t)
	for _, config := range []domain.AccountAutoLoginConfig{
		{Enabled: true, Region: "global"},
		{Enabled: true, Region: "elsewhere", CredentialsCiphertext: "ciphertext"},
		{Enabled: true, Region: "cn", CredentialsCiphertext: "ciphertext", Revision: 1},
		{Enabled: true, Region: "cn", CredentialsCiphertext: "ciphertext", AccountID: 999},
	} {
		_, err := db.CreateAccount(ctx, domain.Account{Name: "rollback", Email: "rollback@icloud.com", IMAPHost: "imap.mail.me.com", IMAPPort: 993, IMAPUsername: "rollback@icloud.com", Enabled: true, AutoLogin: &config})
		if !errors.Is(err, ErrInvalidAutoLoginConfig) && !errors.Is(err, ErrAutoLoginConfigChanged) {
			t.Fatalf("invalid config create = %v", err)
		}
		var count int
		if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("invalid config left account row: count=%d err=%v", count, err)
		}
	}
	account, err := db.CreateAccount(ctx, domain.Account{Name: "off", Email: "off@icloud.com", IMAPHost: "imap.mail.me.com", IMAPPort: 993, IMAPUsername: "off@icloud.com", Enabled: true,
		AutoLogin: &domain.AccountAutoLoginConfig{Region: "cn"}})
	if err != nil {
		t.Fatalf("disabled config with no secret: %v", err)
	}
	state := domain.AccountAutoLoginState{AccountID: account.ID, Channel: domain.AutoLoginChannelAppleAccount, ConfigRevision: 1, Status: "idle"}
	if err := db.SaveAccountAutoLoginState(ctx, state); !errors.Is(err, ErrAutoLoginConfigChanged) {
		t.Fatalf("disabled config state accepted: %v", err)
	}
	state.Channel = "unexpected"
	if err := db.SaveAccountAutoLoginState(ctx, state); !errors.Is(err, ErrInvalidAutoLoginState) {
		t.Fatalf("unknown channel = %v", err)
	}
	config, err := db.GetAccountAutoLoginConfig(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	config.Enabled, config.CredentialsCiphertext = true, "ciphertext"
	account.AutoLogin = &config
	account.MailboxType, account.EmailSuffix = domain.MailboxTypeCustom, "example.test"
	if _, err := db.UpdateAccount(ctx, account); !errors.Is(err, ErrInvalidAutoLoginConfig) {
		t.Fatalf("custom mailbox enabled auto login: %v", err)
	}
}

func TestAccountAutoLoginMigrationReopenAndDeleteCascade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "auto-login.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	account := createAutoLoginTestAccount(t, db)
	state := domain.AccountAutoLoginState{AccountID: account.ID, Channel: domain.AutoLoginChannelICloudWeb, ConfigRevision: 1, Status: "idle"}
	if err := db.SaveAccountAutoLoginState(ctx, state); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopen migration: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	config, err := db.GetAccountAutoLoginConfig(ctx, account.ID)
	if err != nil || config.Revision != 1 || config.CredentialsCiphertext != "login-envelope" {
		t.Fatalf("reopen config = %#v, %v", config, err)
	}
	got, err := db.GetAccountAutoLoginState(ctx, account.ID, state.Channel)
	if err != nil || got.Status != "idle" {
		t.Fatalf("reopen state = %#v, %v", got, err)
	}
	if err := db.DeleteAccount(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"account_auto_login_configs", "account_auto_login_states"} {
		var count int
		if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("cascade %s = %d, %v", table, count, err)
		}
	}
	if err := db.SaveAccountAutoLoginState(ctx, state); !errors.Is(err, ErrAutoLoginConfigChanged) {
		t.Fatalf("deleted account accepted old worker state: %v", err)
	}
}

func TestAccountAutoLoginPrivateJSON(t *testing.T) {
	config := domain.AccountAutoLoginConfig{AccountID: 123, Enabled: true, Region: "cn", CredentialsCiphertext: "private-envelope", Revision: 8}
	state := domain.AccountAutoLoginState{AccountID: 123, Channel: domain.AutoLoginChannelAppleAccount, ConfigRevision: 8, Status: "backoff", LastErrorCode: "private-detail"}
	for _, value := range []any{config, state} {
		encoded, err := json.Marshal(value)
		if err != nil || string(encoded) != "{}" {
			t.Fatalf("private config/state serialization = %s, %v", encoded, err)
		}
	}
	encoded, err := json.Marshal(domain.Account{AutoLogin: &config})
	if err != nil || strings.Contains(string(encoded), "private-envelope") || strings.Contains(string(encoded), "AutoLogin") {
		t.Fatalf("private config leaked through account serialization: %s, %v", encoded, err)
	}
}

func TestAccountAutoLoginPostgresMigrationCASAndState(t *testing.T) {
	dsn := os.Getenv("ICLOUD_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("ICLOUD_TEST_POSTGRES_URL is not set")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if (parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1") ||
		!strings.Contains(strings.ToLower(strings.TrimPrefix(parsed.Path, "/")), "test") {
		t.Fatal("expected disposable loopback test database")
	}
	ctx := context.Background()
	db, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	account, err := db.CreateAccount(ctx, domain.Account{Name: "Postgres auto login",
		Email: fmt.Sprintf("pg-auto-login-%d@icloud.com", time.Now().UnixNano()), Enabled: true,
		IMAPHost: "imap.mail.me.com", IMAPPort: 993, IMAPUsername: "fixture@icloud.com",
		AutoLogin: &domain.AccountAutoLoginConfig{Enabled: true, Region: "global", CredentialsCiphertext: "encrypted-envelope"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteAccount(ctx, account.ID) })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("repeat postgres migration: %v", err)
	}
	config, err := db.GetAccountAutoLoginConfig(ctx, account.ID)
	if err != nil || config.Revision != 1 {
		t.Fatalf("postgres config = %#v, %v", config, err)
	}
	state := domain.AccountAutoLoginState{AccountID: account.ID, Channel: domain.AutoLoginChannelAppleAccount,
		ConfigRevision: 1, Status: "idle"}
	if err := db.SaveAccountAutoLoginState(ctx, state); err != nil {
		t.Fatal(err)
	}
	account.AutoLogin = &config
	if _, err := db.UpdateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateAccount(ctx, account); !errors.Is(err, ErrAutoLoginConfigChanged) {
		t.Fatalf("postgres stale config accepted: %v", err)
	}
	if err := db.SaveAccountAutoLoginState(ctx, state); !errors.Is(err, ErrAutoLoginConfigChanged) {
		t.Fatalf("postgres stale state accepted: %v", err)
	}
	if err := db.DeleteAccount(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetAccountAutoLoginConfig(ctx, account.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("postgres config not cascaded: %v", err)
	}
}
