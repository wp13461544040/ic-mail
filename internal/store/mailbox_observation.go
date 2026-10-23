package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"icloud-api/internal/domain"
)

// The caller already holds the account row lock. Settings revisions also
// detect disable/re-enable and source switches that return to their old values.
func (s *Store) checkMailboxObservationTx(ctx context.Context, tx *sql.Tx, observed domain.Account) error {
	current, err := scanAccount(s.txQueryRowContext(ctx, tx, `SELECT `+accountColumns+` FROM accounts a WHERE a.id = ?`, observed.ID))
	if err != nil {
		return err
	}
	if !current.Enabled || current.MailboxConfigVersion != observed.MailboxConfigVersion ||
		current.MailTransportVersion != observed.MailTransportVersion ||
		current.Email != observed.Email || current.IMAPHost != observed.IMAPHost ||
		current.IMAPPort != observed.IMAPPort || current.IMAPUsername != observed.IMAPUsername ||
		current.PasswordCiphertext != observed.PasswordCiphertext ||
		current.MailboxType != observed.MailboxType || current.EmailSuffix != observed.EmailSuffix {
		return ErrAliasMailboxSyncStale
	}
	return nil
}

func (s *Store) filterMailboxRecipientsTx(ctx context.Context, tx *sql.Tx, accountID int64, observed []domain.Alias, result domain.MailboxSyncResult) ([]domain.Alias, domain.MailboxSyncResult, error) {
	rows, err := s.txQueryContext(ctx, tx, `SELECT a.id, a.address, a.created_at, COALESCE(r.revision, 0)
		FROM aliases a LEFT JOIN alias_receive_revisions r ON r.alias_id = a.id
		WHERE a.account_id = ? AND a.enabled = TRUE`, accountID)
	if err != nil {
		return nil, result, fmt.Errorf("recheck mailbox recipients: %w", err)
	}
	type identity struct {
		address   string
		version   int64
		createdAt int64
	}
	current := make(map[int64]identity)
	for rows.Next() {
		var id int64
		var value identity
		if err := rows.Scan(&id, &value.address, &value.createdAt, &value.version); err != nil {
			rows.Close()
			return nil, result, err
		}
		current[id] = value
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, result, err
	}
	kept := make(map[int64]bool)
	aliases := make([]domain.Alias, 0, len(observed))
	for _, alias := range observed {
		if value, ok := current[alias.ID]; ok && value.address == alias.Address && value.version == alias.ReceiveRevision && value.createdAt == timestamp(alias.CreatedAt) {
			kept[alias.ID] = true
			aliases = append(aliases, alias)
		}
	}
	messages := make([]domain.ArchivedMessage, 0, len(result.ArchivedMessages))
	for _, message := range result.ArchivedMessages {
		ids := make([]int64, 0, len(message.AliasIDs))
		for _, id := range message.AliasIDs {
			if kept[id] {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			message.AliasIDs = ids
			messages = append(messages, message)
		}
	}
	updates := make(map[int64]domain.LatestMessage)
	for id, update := range result.LegacySnapshotUpdates {
		if kept[id] {
			updates[id] = update
		}
	}
	result.ArchivedMessages, result.LegacySnapshotUpdates = messages, updates
	return aliases, result, nil
}

func (s *Store) ApplyObservedAliasMailboxSync(ctx context.Context, account domain.Account, alias domain.Alias, result domain.MailboxSyncResult, at time.Time, webmail bool) error {
	return s.applyAliasMailboxSync(ctx, account.UpdatedAt, alias, result, at, webmail, &account)
}
