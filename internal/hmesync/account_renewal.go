package hmesync

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

// RunAccountSessionRenewal checks local deadlines; only due, enabled accounts
// make management requests. It never reads mail or creates an address.
func (s *Service) RunAccountSessionRenewal(ctx context.Context, logger *slog.Logger) {
	// Leave scheduling headroom around the four-minute renewal deadline.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	s.runAccountSessionRenewal(ctx, logger, ticker.C)
}

func (s *Service) runAccountSessionRenewal(ctx context.Context, logger *slog.Logger, ticks <-chan time.Time) {
	repo, ok := s.repo.(interface {
		ListAccounts(context.Context) ([]domain.Account, error)
	})
	if !ok {
		return
	}
	completed := make(chan int64)
	var workers sync.WaitGroup
	defer workers.Wait()
	active := make(map[int64]bool)
	start := func(id int64) {
		active[id] = true
		workers.Add(1)
		go func() {
			defer workers.Done()
			callCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
			err := s.keepAliveAccountSession(callCtx, id, true, logger)
			cancel()
			if err != nil && ctx.Err() == nil {
				logAccountRenewalError(logger, id, err)
			}
			select {
			case completed <- id:
			case <-ctx.Done():
			}
		}()
	}
	refresh := func() {
		accounts, err := repo.ListAccounts(ctx)
		if err != nil && ctx.Err() == nil && logger != nil {
			logger.Warn("读取 Apple Account 续期计划暂未完成", "operation", "apple_account_session_renew_list")
		}
		if err != nil {
			return
		}
		for _, account := range accounts {
			if ctx.Err() != nil {
				return
			}
			if active[account.ID] || !account.Enabled || domain.NormalizeMailboxType(account.MailboxType) != domain.MailboxTypeICloud {
				continue
			}
			// Only due accounts get a short-lived task. Each account owns its
			// operation wait and timeout, independently of all other accounts.
			session, err := s.readAccountManagementSession(ctx, account.ID)
			if err != nil {
				if !errors.Is(err, store.ErrNotFound) && ctx.Err() == nil {
					logAccountRenewalError(logger, account.ID, err)
				}
				continue
			}
			if accountSessionInfo(session).Status == StatusAuthenticated && session.NeedsRefresh(s.now()) {
				start(account.ID)
			}
		}
	}
	refresh()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			refresh()
		case id := <-completed:
			delete(active, id)
		}
	}
}

func logAccountRenewalError(logger *slog.Logger, id int64, err error) {
	if logger == nil {
		return
	}
	code := Code(mapAppleError(err, false))
	if errors.Is(err, apple.ErrInvalidSession) {
		code = CodeAccountSessionExpired
	}
	attrs := []any{"account_id", id, "operation", "apple_account_session_renew", "error_code", code}
	var upstream *apple.Error
	if errors.As(err, &upstream) {
		attrs = append(attrs, "http_status", upstream.StatusCode, "upstream_operation", upstream.Op, "service_code", upstream.ServiceCode)
	}
	logger.Warn("Apple Account 会话续期暂未完成", attrs...)
}
