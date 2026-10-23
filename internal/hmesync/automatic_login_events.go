package hmesync

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

// NotifyAutomaticLogin coalesces account changes without doing database or
// network I/O on the caller. Eligibility is checked again before any login.
func (s *Service) NotifyAutomaticLogin(id int64) {
	if id < 1 {
		return
	}
	s.autoLoginEventMu.Lock()
	defer s.autoLoginEventMu.Unlock()
	s.initAutomaticLoginEvents()
	s.autoLoginEvents[id] = struct{}{}
	select {
	case s.autoLoginWake <- struct{}{}:
	default:
	}
}

// Caller holds autoLoginEventMu.
func (s *Service) initAutomaticLoginEvents() {
	if s.autoLoginWake == nil {
		s.autoLoginWake = make(chan struct{}, 1)
		s.autoLoginEvents = make(map[int64]struct{})
	}
}

func (s *Service) takeAutomaticLoginEvents() map[int64]struct{} {
	s.autoLoginEventMu.Lock()
	defer s.autoLoginEventMu.Unlock()
	ids := s.autoLoginEvents
	s.autoLoginEvents = make(map[int64]struct{})
	return ids
}

// deadlineWake is a test clock wakeup; production uses only the one-shot timer.
func (s *Service) runAutomaticLogin(ctx context.Context, logger *slog.Logger, deadlineWake <-chan time.Time) {
	repo, ok := s.repo.(automaticLoginRepository)
	if !ok {
		return
	}
	if _, ok := s.cipher.(automaticLoginCipher); !ok {
		return
	}
	s.autoLoginEventMu.Lock()
	s.initAutomaticLoginEvents()
	wake := s.autoLoginWake
	s.autoLoginEventMu.Unlock()
	active := make(map[int64]bool)
	deadlines := make(map[int64]time.Time)
	type completion struct {
		id, revision int64
	}
	done := make(chan completion)
	var workers sync.WaitGroup
	defer workers.Wait()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	if !timer.Stop() {
		<-timer.C
	}

	reconcile := func(id, completedRevision int64) {
		if ctx.Err() != nil || active[id] {
			return
		}
		delete(deadlines, id)
		config, next, err := s.nextAutomaticLogin(ctx, id)
		if err != nil {
			// Retry only this failed read, never rescan all accounts.
			deadlines[id] = s.now().Add(5 * time.Minute)
			return
		}
		if next.IsZero() {
			return
		}
		if next.After(s.now()) {
			deadlines[id] = next
			return
		}
		if completedRevision == config.Revision {
			// Both channels were just considered. An unreserved preparation
			// failure is retried with backoff, never in a tight completion loop.
			deadlines[id] = s.now().Add(5 * time.Minute)
			return
		}
		active[id] = true
		workers.Add(1)
		go func() {
			defer workers.Done()
			callCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
			defer cancel()
			for _, channel := range []string{domain.AutoLoginChannelAppleAccount, domain.AutoLoginChannelICloudWeb} {
				if callCtx.Err() != nil {
					break
				}
				s.runAutomaticLoginChannel(callCtx, config, channel, logger)
			}
			select {
			case done <- completion{id: id, revision: config.Revision}:
			case <-ctx.Done():
			}
		}()
	}
	bootstrap := func() {
		configs, err := repo.ListEnabledAccountAutoLoginConfigs(ctx)
		if err != nil {
			// A failed startup recovery has its own deadline; after success this
			// is removed permanently. Zero is not an account ID.
			deadlines[0] = s.now().Add(5 * time.Minute)
			if ctx.Err() == nil && logger != nil {
				logger.Warn("恢复自动登录保活计划暂未完成", "operation", "apple_auto_login_restore")
			}
			return
		}
		delete(deadlines, 0)
		for _, config := range configs {
			reconcile(config.AccountID, 0)
		}
	}
	bootstrap()
	for ctx.Err() == nil {
		var earliest time.Time
		for _, next := range deadlines {
			if earliest.IsZero() || next.Before(earliest) {
				earliest = next
			}
		}
		var timerC <-chan time.Time
		if !earliest.IsZero() {
			timer.Reset(max(earliest.Sub(s.now()), 0))
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
			for id := range s.takeAutomaticLoginEvents() {
				reconcile(id, 0)
			}
		case result := <-done:
			delete(active, result.id)
			reconcile(result.id, result.revision)
		case <-timerC:
		case <-deadlineWake:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		// Snapshot due IDs before reconciliation mutates the deadline map.
		var due []int64
		for id, next := range deadlines {
			if !next.After(s.now()) {
				due = append(due, id)
			}
		}
		for _, id := range due {
			if id == 0 {
				bootstrap()
			} else {
				reconcile(id, 0)
			}
		}
	}
}

// nextAutomaticLogin returns an exact known expiry/retry deadline, or now for
// an eligible expired channel. Healthy accounts with no expiry have no timer.
func (s *Service) nextAutomaticLogin(ctx context.Context, id int64) (domain.AccountAutoLoginConfig, time.Time, error) {
	repo := s.repo.(automaticLoginRepository)
	config, err := repo.GetAccountAutoLoginConfig(ctx, id)
	if errors.Is(err, store.ErrNotFound) || err == nil && !config.Enabled {
		return config, time.Time{}, nil
	}
	if err != nil {
		return config, time.Time{}, err
	}
	account, err := s.repo.GetAccount(ctx, id)
	if errors.Is(err, store.ErrNotFound) || err == nil && (!account.Enabled || domain.NormalizeMailboxType(account.MailboxType) != domain.MailboxTypeICloud) {
		return config, time.Time{}, nil
	}
	if err != nil {
		return config, time.Time{}, err
	}
	now := s.now()
	interactive := s.automaticInteractiveDeadline(id)
	var next time.Time
	for _, channel := range []string{domain.AutoLoginChannelAppleAccount, domain.AutoLoginChannelICloudWeb} {
		var info SessionInfo
		if channel == domain.AutoLoginChannelAppleAccount {
			info, err = s.GetAccountSession(ctx, id)
		} else {
			info, err = s.GetSession(ctx, id)
		}
		if err != nil && Code(err) != CodeAccountSessionExpired {
			return config, time.Time{}, err
		}
		var candidate time.Time
		if err == nil && info.Status == StatusAuthenticated {
			s.observeHealthyAutomaticChannel(ctx, config, channel)
			if info.ExpiresAt != nil && info.ExpiresAt.After(now) {
				candidate = *info.ExpiresAt
			}
		} else {
			state, stateErr := repo.GetAccountAutoLoginState(ctx, id, channel)
			if stateErr != nil && !errors.Is(stateErr, store.ErrNotFound) {
				return config, time.Time{}, stateErr
			}
			if stateErr == nil && (state.ConfigRevision != config.Revision || state.Status == "needs_action") {
				continue
			}
			candidate = now
			if state.NextAttemptAt != nil && state.NextAttemptAt.After(candidate) {
				candidate = *state.NextAttemptAt
			}
			if interactive.After(candidate) {
				candidate = interactive
			}
		}
		if !candidate.IsZero() && (next.IsZero() || candidate.Before(next)) {
			next = candidate
		}
	}
	return config, next, nil
}

func (s *Service) automaticInteractiveDeadline(id int64) time.Time {
	s.challengeMu.Lock()
	defer s.challengeMu.Unlock()
	var deadline time.Time
	if flow, ok := s.accountAuthChallenges[id]; ok && flow.expires.After(s.now()) {
		deadline = flow.expires
	}
	if key, ok := s.accountFlows[id]; ok {
		if flow, ok := s.challenges[key]; ok && flow.expiresAt.After(s.now()) && flow.expiresAt.After(deadline) {
			deadline = flow.expiresAt
		}
	}
	return deadline
}
