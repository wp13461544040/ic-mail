package hmesync

import (
	"context"
	"errors"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

type directoryReconcileRepository interface {
	ListAliasesByAccount(context.Context, int64) ([]domain.Alias, error)
	ReconcileAppleDirectory(context.Context, domain.Account, time.Time, []string) (int, error)
}

// Consult the other complete directory only when a deletion would be needed.
// A positive entry in either channel wins over absence in the other. Failure of
// the cross-check disables cleanup for this sync, never becoming an empty list.
func (s *Service) directoryForReconciliation(ctx context.Context, account domain.Account, startedAt time.Time, directory syncDirectory) (syncDirectory, bool, error) {
	repo, ok := s.repo.(directoryReconcileRepository)
	if !ok {
		return directory, false, nil
	}
	local, err := repo.ListAliasesByAccount(ctx, account.ID)
	if err != nil {
		return directory, false, err
	}
	addresses := make(map[string]bool, len(directory.list.Aliases))
	for _, alias := range directory.list.Aliases {
		addresses[domain.NormalizeEmail(alias.HME)] = true
	}
	missing := false
	for _, alias := range local {
		if alias.CreatedAt.After(startedAt) || (!alias.Enabled && alias.LastSyncError == domain.AppleAliasConfirmationPending && alias.CreatedAt.After(startedAt.Add(-24*time.Hour))) {
			continue
		}
		if !addresses[domain.NormalizeEmail(alias.Address)] {
			missing = true
			break
		}
	}
	if !missing {
		return directory, true, nil
	}
	// A fallback means Web validation failed. Account absence alone must not
	// erase an address which may still be positive in the unavailable channel.
	if directory.fromAccount {
		return directory, false, ErrSessionExpired
	}
	if _, ok := s.client.(accountDirectoryLister); !ok {
		return directory, true, nil
	}
	if _, ok := s.repo.(accountSessionRepository); !ok {
		return directory, true, nil
	}
	managed, err := s.readAccountManagementSession(ctx, account.ID)
	if errors.Is(err, store.ErrNotFound) || Code(err) == CodeAccountSessionExpired || err == nil && (managed == nil || managed.RefreshRejected) {
		return directory, true, nil
	}
	if err != nil {
		return directory, false, err
	}
	secondary, err := s.readAccountDirectoryForSync(ctx, account, ErrLoginRequired)
	if err != nil {
		return directory, false, err
	}
	if _, _, err := filterAliases(secondary.list, account.Email); err != nil {
		return directory, false, err
	}
	merged := directory
	merged.list.Aliases = append([]apple.Alias(nil), directory.list.Aliases...)
	for _, alias := range secondary.list.Aliases {
		address := domain.NormalizeEmail(alias.HME)
		if !addresses[address] {
			merged.list.Aliases = append(merged.list.Aliases, alias)
			addresses[address] = true
		}
	}
	if _, _, err := filterAliases(merged.list, account.Email); err != nil {
		return directory, false, err
	}
	return merged, true, nil
}
