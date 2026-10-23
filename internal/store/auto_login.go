package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"

	"icloud-api/internal/domain"
)

var (
	ErrAutoLoginConfigChanged = errors.New("auto login configuration changed")
	ErrInvalidAutoLoginConfig = errors.New("invalid auto login configuration")
	ErrInvalidAutoLoginState  = errors.New("invalid auto login state")
)

const autoLoginConfigColumns = `account_id, enabled, region, credentials_ciphertext, revision, created_at, updated_at`

func scanAutoLoginConfig(row interface{ Scan(...any) error }) (domain.AccountAutoLoginConfig, error) {
	var config domain.AccountAutoLoginConfig
	var created, updated int64
	err := row.Scan(&config.AccountID, &config.Enabled, &config.Region, &config.CredentialsCiphertext, &config.Revision, &created, &updated)
	if err != nil {
		return domain.AccountAutoLoginConfig{}, err
	}
	config.CreatedAt, config.UpdatedAt = timeFromTimestamp(created), timeFromTimestamp(updated)
	return config, nil
}

func (s *Store) GetAccountAutoLoginConfig(ctx context.Context, accountID int64) (domain.AccountAutoLoginConfig, error) {
	return scanAutoLoginConfig(s.queryRowContext(ctx, `SELECT `+autoLoginConfigColumns+` FROM account_auto_login_configs WHERE account_id = ?`, accountID))
}

func (s *Store) ListEnabledAccountAutoLoginConfigs(ctx context.Context) ([]domain.AccountAutoLoginConfig, error) {
	rows, err := s.queryContext(ctx, `SELECT c.account_id, c.enabled, c.region, c.credentials_ciphertext, c.revision, c.created_at, c.updated_at
		FROM account_auto_login_configs c JOIN accounts a ON a.id = c.account_id
		WHERE c.enabled = TRUE AND a.enabled = TRUE
		AND COALESCE((SELECT mailbox_type FROM account_mailbox_settings WHERE account_id = a.id), 'icloud') = 'icloud'
		ORDER BY c.account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	configs := make([]domain.AccountAutoLoginConfig, 0)
	for rows.Next() {
		config, err := scanAutoLoginConfig(rows)
		if err != nil {
			return nil, err
		}
		configs = append(configs, config)
	}
	return configs, rows.Err()
}

// The caller holds the account row lock (or is creating the account) so state
// writers and configuration changes use one lock order on both databases.
func (s *Store) saveAccountAutoLoginConfigTx(ctx context.Context, tx *sql.Tx, accountID int64, config domain.AccountAutoLoginConfig, mailboxType string) error {
	if config.AccountID != 0 && config.AccountID != accountID {
		return ErrInvalidAutoLoginConfig
	}
	if config.Region != "cn" && config.Region != "global" || config.Revision < 0 ||
		config.Enabled && (strings.TrimSpace(config.CredentialsCiphertext) == "" || mailboxType != domain.MailboxTypeICloud) {
		return ErrInvalidAutoLoginConfig
	}
	var currentRevision int64
	err := s.txQueryRowContext(ctx, tx, `SELECT revision FROM account_auto_login_configs WHERE account_id = ?`, accountID).Scan(&currentRevision)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if config.Revision != currentRevision || currentRevision == math.MaxInt64 {
		return ErrAutoLoginConfigChanged
	}
	now := timestamp(s.now())
	if currentRevision == 0 {
		_, err = s.txExecContext(ctx, tx, `INSERT INTO account_auto_login_configs
			(account_id, enabled, region, credentials_ciphertext, revision, created_at, updated_at)
			VALUES(?, ?, ?, ?, 1, ?, ?)`, accountID, config.Enabled, config.Region, config.CredentialsCiphertext, now, now)
	} else {
		var result sql.Result
		result, err = s.txExecContext(ctx, tx, `UPDATE account_auto_login_configs
			SET enabled = ?, region = ?, credentials_ciphertext = ?, revision = ?, updated_at = ?
			WHERE account_id = ? AND revision = ?`, config.Enabled, config.Region, config.CredentialsCiphertext, currentRevision+1, now, accountID, currentRevision)
		if err == nil {
			var affected int64
			affected, err = result.RowsAffected()
			if err == nil && affected != 1 {
				return ErrAutoLoginConfigChanged
			}
		}
	}
	if err != nil {
		return fmt.Errorf("save account auto login config: %w", err)
	}
	_, err = s.txExecContext(ctx, tx, `DELETE FROM account_auto_login_states WHERE account_id = ?`, accountID)
	return err
}

func (s *Store) invalidateAccountAutoLoginIdentityTx(ctx context.Context, tx *sql.Tx, accountID int64) error {
	var revision int64
	err := s.txQueryRowContext(ctx, tx, `SELECT revision FROM account_auto_login_configs WHERE account_id = ?`, accountID).Scan(&revision)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if revision == math.MaxInt64 {
		return ErrAutoLoginConfigChanged
	}
	if _, err := s.txExecContext(ctx, tx, `UPDATE account_auto_login_configs SET enabled = FALSE,
		credentials_ciphertext = '', revision = ?, updated_at = ? WHERE account_id = ?`, revision+1, timestamp(s.now()), accountID); err != nil {
		return err
	}
	_, err = s.txExecContext(ctx, tx, `DELETE FROM account_auto_login_states WHERE account_id = ?`, accountID)
	return err
}

func validAutoLoginChannel(channel string) bool {
	return channel == domain.AutoLoginChannelAppleAccount || channel == domain.AutoLoginChannelICloudWeb
}

func (s *Store) GetAccountAutoLoginState(ctx context.Context, accountID int64, channel string) (domain.AccountAutoLoginState, error) {
	if !validAutoLoginChannel(channel) {
		return domain.AccountAutoLoginState{}, ErrInvalidAutoLoginState
	}
	var state domain.AccountAutoLoginState
	var attempt, success, next sql.NullInt64
	err := s.queryRowContext(ctx, `SELECT account_id, channel, config_revision, status, failures, last_error_code,
		last_attempt_at, last_success_at, next_attempt_at FROM account_auto_login_states WHERE account_id = ? AND channel = ?`, accountID, channel).
		Scan(&state.AccountID, &state.Channel, &state.ConfigRevision, &state.Status, &state.Failures, &state.LastErrorCode, &attempt, &success, &next)
	if err != nil {
		return domain.AccountAutoLoginState{}, err
	}
	state.LastAttemptAt, state.LastSuccessAt, state.NextAttemptAt = timePtr(attempt), timePtr(success), timePtr(next)
	return state, nil
}

func (s *Store) SaveAccountAutoLoginState(ctx context.Context, state domain.AccountAutoLoginState) error {
	if !validAutoLoginChannel(state.Channel) || state.ConfigRevision <= 0 || state.Failures < 0 ||
		strings.TrimSpace(state.Status) == "" || strings.ContainsRune(state.Status, 0) || strings.ContainsRune(state.LastErrorCode, 0) {
		return ErrInvalidAutoLoginState
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := s.lockAccountVersionForUpdate(ctx, tx, state.AccountID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrAutoLoginConfigChanged
		}
		return err
	}
	var eligible bool
	if err := s.txQueryRowContext(ctx, tx, `SELECT EXISTS(SELECT 1 FROM account_auto_login_configs c
		JOIN accounts a ON a.id = c.account_id WHERE c.account_id = ? AND c.revision = ?
		AND c.enabled = TRUE AND a.enabled = TRUE
		AND COALESCE((SELECT mailbox_type FROM account_mailbox_settings WHERE account_id = a.id), 'icloud') = 'icloud')`,
		state.AccountID, state.ConfigRevision).Scan(&eligible); err != nil {
		return err
	}
	if !eligible {
		return ErrAutoLoginConfigChanged
	}
	_, err = s.txExecContext(ctx, tx, `INSERT INTO account_auto_login_states
		(account_id, channel, config_revision, status, failures, last_error_code, last_attempt_at, last_success_at, next_attempt_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, channel) DO UPDATE SET config_revision = excluded.config_revision,
		status = excluded.status, failures = excluded.failures, last_error_code = excluded.last_error_code,
		last_attempt_at = excluded.last_attempt_at, last_success_at = excluded.last_success_at, next_attempt_at = excluded.next_attempt_at`,
		state.AccountID, state.Channel, state.ConfigRevision, state.Status, state.Failures, state.LastErrorCode,
		nullableTimestamp(state.LastAttemptAt), nullableTimestamp(state.LastSuccessAt), nullableTimestamp(state.NextAttemptAt))
	if err != nil {
		return err
	}
	return tx.Commit()
}
