package store

import (
	"context"
	"fmt"
	"net/mail"
	"time"

	"icloud-api/internal/domain"
)

// ReconcileAppleDirectory removes local addresses absent from a freshly read,
// complete Apple directory. The caller validates the directory and serializes
// Apple operations. No remote requests run while the Pool/account rows are held.
func (s *Store) ReconcileAppleDirectory(ctx context.Context, expected domain.Account, startedAt time.Time, addresses []string) (int, error) {
	if expected.ID < 1 || startedAt.IsZero() {
		return 0, fmt.Errorf("invalid directory reconciliation identity")
	}
	present := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		address = domain.NormalizeEmail(address)
		parsed, err := mail.ParseAddress(address)
		if err != nil || parsed.Name != "" || domain.NormalizeEmail(parsed.Address) != address {
			return 0, fmt.Errorf("invalid directory reconciliation address")
		}
		if _, duplicate := present[address]; duplicate {
			return 0, fmt.Errorf("duplicate directory reconciliation address")
		}
		present[address] = struct{}{}
	}
	tx, err := s.poolTx(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	version, err := s.lockAccountVersionForUpdate(ctx, tx, expected.ID)
	if err != nil {
		return 0, err
	}
	current, err := scanAccount(s.txQueryRowContext(ctx, tx, `SELECT `+accountColumns+` FROM accounts a WHERE a.id=?`, expected.ID))
	if err != nil {
		return 0, err
	}
	if !current.Enabled {
		return 0, ErrAccountDisabled
	}
	if domain.NormalizeMailboxType(current.MailboxType) != domain.MailboxTypeICloud ||
		current.Email != expected.Email || current.IMAPUsername != expected.IMAPUsername || current.MailboxConfigVersion != expected.MailboxConfigVersion {
		return 0, ErrAccountIdentityLocked
	}
	rows, err := s.txQueryContext(ctx, tx, `SELECT id,address FROM aliases WHERE account_id=? AND created_at<=?
		AND NOT (enabled=FALSE AND last_sync_error=? AND created_at>?) ORDER BY id`,
		expected.ID, timestamp(startedAt), domain.AppleAliasConfirmationPending, timestamp(startedAt.Add(-24*time.Hour)))
	if err != nil {
		return 0, err
	}
	type target struct {
		id      int64
		address string
	}
	var missing []target
	for rows.Next() {
		var item target
		if err := rows.Scan(&item.id, &item.address); err != nil {
			rows.Close()
			return 0, err
		}
		if _, exists := present[domain.NormalizeEmail(item.address)]; !exists {
			missing = append(missing, item)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	now := timestamp(s.now())
	for _, item := range missing {
		var conflicts int
		if err := s.txQueryRowContext(ctx, tx, `SELECT COUNT(*) FROM pool_leases WHERE alias_id=? AND address<>?`, item.id, item.address).Scan(&conflicts); err != nil {
			return 0, err
		}
		if conflicts != 0 {
			return 0, ErrPoolRetirementConflict
		}
		if err := s.txQueryRowContext(ctx, tx, `SELECT COUNT(*) FROM pool_alias_retirement_job_items i
			JOIN pool_alias_retirement_jobs j ON j.operation_id=i.operation_id
			LEFT JOIN pool_leases l ON l.id=i.lease_id
			WHERE i.alias_id=? AND i.state<>'retired' AND (i.account_id<>? OR i.address<>?
			OR l.id IS NULL OR l.alias_id IS NULL OR l.alias_id<>i.alias_id OR l.address<>i.address OR l.client_id<>j.client_id)`,
			item.id, expected.ID, item.address).Scan(&conflicts); err != nil {
			return 0, err
		}
		if conflicts != 0 {
			return 0, ErrPoolRetirementConflict
		}
		// Close all live capabilities before cascading the alias and pool member.
		if _, err := s.txExecContext(ctx, tx, `UPDATE pool_leases SET state='retired',updated_at=?
			WHERE alias_id=? AND state IN ('leased','used','retiring')`, now, item.id); err != nil {
			return 0, err
		}
		if _, err := s.txExecContext(ctx, tx, `UPDATE pool_alias_retirement_job_items SET state='retired',
			result_code='APPLE_DIRECTORY_REMOVED',result_message='Apple 完整目录已确认地址不存在，本地记录已清理',updated_at=?
			WHERE alias_id=? AND account_id=? AND address=? AND state<>'retired'`, now, item.id, expected.ID, item.address); err != nil {
			return 0, err
		}
		if _, err := s.txExecContext(ctx, tx, `UPDATE pool_alias_retirement_jobs SET status='completed',updated_at=?
			WHERE operation_id IN (SELECT operation_id FROM pool_alias_retirement_job_items WHERE alias_id=? AND account_id=?)
			AND NOT EXISTS (SELECT 1 FROM pool_alias_retirement_job_items i WHERE i.operation_id=pool_alias_retirement_jobs.operation_id AND i.state<>'retired')`,
			now, item.id, expected.ID); err != nil {
			return 0, err
		}
		if _, err := s.txExecContext(ctx, tx, `DELETE FROM aliases WHERE id=? AND account_id=?`, item.id, expected.ID); err != nil {
			return 0, err
		}
	}
	if len(missing) > 0 {
		if _, err := s.bumpAccountVersionTx(ctx, tx, expected.ID, version); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(missing), nil
}
