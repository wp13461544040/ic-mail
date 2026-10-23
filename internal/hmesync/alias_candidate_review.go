package hmesync

import (
	"context"
	"errors"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

type candidateReviewRepository interface {
	ReviewStaleAccountAliasCandidate(context.Context, int64, int64, string, time.Time) error
	ListReviewPendingAliases(context.Context, int64) ([]domain.PendingAliasAPIKey, error)
	ConfirmReviewedAliasFromDirectory(context.Context, int64, string) (domain.Alias, error)
}

const staleAccountCandidateReviewAge = 24 * time.Hour

// Two successful, independent reads permit a stale, unissued candidate to stop
// blocking creation. They do not prove deletion: its identity and credentials
// remain reserved, and no completion request is replayed on this path.
// The caller owns the account operation lock and provides its checkpoint path.
func (s *Service) reviewOldAccountCandidate(ctx context.Context, account domain.Account, pending domain.PendingAliasAPIKey, web apple.ListResult, session apple.Session, persist func(context.Context, apple.AccountSession) error) (bool, *apple.Alias, apple.Session, error) {
	repo, ok := s.repo.(candidateReviewRepository)
	lister, canList := s.client.(accountDirectoryLister)
	if !ok || !canList || pending.CreationChannel != "apple_account" || pending.CreationRemoteID != "" ||
		pending.CreatedAt.IsZero() || pending.CreatedAt.After(s.now().Add(-staleAccountCandidateReviewAge)) || session.Account == nil {
		return false, nil, session, nil
	}
	if _, present := findAppleAlias(web.Aliases, pending.Address); present {
		return false, nil, session, nil
	}
	started := s.now()
	list, managed, err := lister.ListAccountAliases(ctx, *session.Account)
	if err == nil || managed.UpdatedAt.After(session.Account.UpdatedAt) {
		if identityErr := validateAccountDirectorySession(*session.Account, managed); identityErr != nil {
			return false, nil, session, identityErr
		}
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), autoCreatePersistTimeout)
		persistErr := persist(persistCtx, managed)
		cancel()
		if persistErr != nil {
			return false, nil, session, wrapPersistenceError(persistErr)
		}
	}
	if err != nil {
		return false, nil, session, wrapError(CodeAliasConfirmationPending, ErrAliasConfirmationPending, mapAppleError(err, false))
	}
	if _, _, err := filterAliases(list, account.Email); err != nil {
		return false, nil, session, err
	}
	session.Account = &managed
	if positive, present := findAppleAlias(list.Aliases, pending.Address); present {
		return false, &positive, session, nil
	}
	if s.now().Sub(started) > time.Minute {
		return false, nil, session, wrapError(CodeAliasConfirmationPending, ErrAliasConfirmationPending, nil)
	}
	if err := ctx.Err(); err != nil {
		return false, nil, session, err
	}
	current, err := s.repo.GetAccount(ctx, account.ID)
	if err != nil {
		return false, nil, session, err
	}
	if !current.Enabled || !sameIdentity(identityOf(current), identityOf(account)) {
		return false, nil, session, wrapError(CodeAccountChanged, ErrAccountChanged, nil)
	}
	err = repo.ReviewStaleAccountAliasCandidate(ctx, account.ID, pending.ID, pending.Address, s.now().Add(-staleAccountCandidateReviewAge))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrAliasConfirmationPending) {
			return false, nil, session, wrapError(CodeAliasConfirmationPending, ErrAliasConfirmationPending, err)
		}
		return false, nil, session, wrapPersistenceError(err)
	}
	return true, nil, session, nil
}

func (s *Service) confirmReviewedDirectoryAliases(ctx context.Context, accountID int64, directory []apple.Alias) (int, error) {
	repo, ok := s.repo.(candidateReviewRepository)
	if !ok {
		return 0, nil
	}
	pending, err := repo.ListReviewPendingAliases(ctx, accountID)
	if err != nil {
		return 0, err
	}
	confirmed := 0
	for _, item := range pending {
		remote, present := findAppleAlias(directory, item.Address)
		if !present || !remote.IsActive {
			continue
		}
		// Directory entries have already passed the exact forwarding filter.
		if _, err := repo.ConfirmReviewedAliasFromDirectory(ctx, accountID, item.Address); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return confirmed, err
		}
		confirmed++
	}
	return confirmed, nil
}
