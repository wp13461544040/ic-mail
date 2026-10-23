package store

import (
	"context"
	"database/sql"
	"fmt"
)

func migrateAccountAutoLogin(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS account_auto_login_configs (
			account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			enabled BOOLEAN NOT NULL DEFAULT FALSE,
			region TEXT NOT NULL CHECK(region IN ('cn', 'global')),
			credentials_ciphertext TEXT NOT NULL DEFAULT '',
			revision BIGINT NOT NULL CHECK(revision > 0),
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL,
			CHECK(NOT enabled OR credentials_ciphertext <> '')
		)`,
		`CREATE TABLE IF NOT EXISTS account_auto_login_states (
			account_id BIGINT NOT NULL REFERENCES account_auto_login_configs(account_id) ON DELETE CASCADE,
			channel TEXT NOT NULL CHECK(channel IN ('apple_account', 'icloud_web')),
			config_revision BIGINT NOT NULL CHECK(config_revision > 0),
			status TEXT NOT NULL,
			failures INTEGER NOT NULL DEFAULT 0 CHECK(failures >= 0),
			last_error_code TEXT NOT NULL DEFAULT '',
			last_attempt_at BIGINT,
			last_success_at BIGINT,
			next_attempt_at BIGINT,
			PRIMARY KEY(account_id, channel)
		)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate account auto login: %w", err)
		}
	}
	return nil
}
