package hmesync

import (
	"context"
	"errors"
	"net/http"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

// Optional for repository adapters; the production Store always implements it.
type appleCreationBudgetRepository interface {
	ClaimAppleCreationAttempt(context.Context, int64, time.Time) error
	PauseAppleCreation(context.Context, int64, time.Time) error
}

type appleChannelCreationBudgetRepository interface {
	ClaimAppleChannelCreationAttempt(context.Context, int64, string, time.Time) error
	PauseAppleChannelCreation(context.Context, int64, string, time.Time) error
}

type creationChannelWaitError struct{ cause error }

func (e *creationChannelWaitError) Error() string               { return e.cause.Error() }
func (e *creationChannelWaitError) Unwrap() error               { return e.cause }
func (e *creationChannelWaitError) CreationChannelScoped() bool { return true }

type channelPendingRepository interface {
	GetPendingAutoAliasConfirmationForChannel(context.Context, int64, string) (domain.PendingAliasAPIKey, error)
}

type candidateDetailRepository interface {
	SaveAliasCreationCandidateDetail(context.Context, int64, int64, string, string) error
}

func (s *Service) creationChannelPausedUntil(ctx context.Context, id int64, channel string) (time.Time, error) {
	s.operationMu.Lock()
	until := s.creationCooldowns[id][channel]
	s.operationMu.Unlock()
	if repo, ok := s.repo.(interface {
		AppleCreationChannelPausedUntil(context.Context, int64, string) (time.Time, error)
	}); ok {
		persisted, err := repo.AppleCreationChannelPausedUntil(ctx, id, channel)
		if err != nil {
			return until, err
		}
		if persisted.After(until) {
			until = persisted
		}
	}
	return until, nil
}

// Completion and detail throttles have the same scope as their creation channel.
func (s *Service) scopeCreationThrottle(ctx context.Context, id int64, channel string, err error) error {
	if !apple.IsRateLimited(err) && !errors.Is(err, ErrRateLimited) {
		return err
	}
	var scoped interface{ CreationChannelScoped() bool }
	if errors.As(err, &scoped) && scoped.CreationChannelScoped() {
		return err
	}
	until := s.now().Add(max(domain.AppleCreationRateLimitCooldown, apple.RetryDelay(err)))
	s.operationMu.Lock()
	if s.creationCooldowns == nil {
		s.creationCooldowns = make(map[int64]map[string]time.Time)
	}
	if s.creationCooldowns[id] == nil {
		s.creationCooldowns[id] = make(map[string]time.Time)
	}
	if until.After(s.creationCooldowns[id][channel]) {
		s.creationCooldowns[id][channel] = until
	}
	s.operationMu.Unlock()
	if budget, ok := s.repo.(appleChannelCreationBudgetRepository); ok {
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), autoCreatePersistTimeout)
		defer cancel()
		if persistErr := budget.PauseAppleChannelCreation(persistCtx, id, channel, until); persistErr != nil {
			err = errors.Join(wrapPersistenceError(persistErr), err)
		}
	}
	return &creationChannelWaitError{cause: err}
}

func creationCooldownError(until, now time.Time) error {
	return mapAppleError(&creationChannelWaitError{cause: &apple.Error{Op: "creation channel cooling down", Kind: apple.ErrService,
		StatusCode: http.StatusTooManyRequests, RetryAfter: until.Sub(now)}}, false)
}
