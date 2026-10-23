package hmesync

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"log/slog"
	"time"

	"icloud-api/internal/autologincode"
	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

type automaticLoginRepository interface {
	GetAccountAutoLoginConfig(context.Context, int64) (domain.AccountAutoLoginConfig, error)
	ListEnabledAccountAutoLoginConfigs(context.Context) ([]domain.AccountAutoLoginConfig, error)
	GetAccountAutoLoginState(context.Context, int64, string) (domain.AccountAutoLoginState, error)
	SaveAccountAutoLoginState(context.Context, domain.AccountAutoLoginState) error
}

type automaticLoginCipher interface {
	DecryptAccountAutoLogin(string, string) (secure.AccountAutoLoginCredentials, error)
}

type AutomaticLoginCodeFetcher interface {
	Fetch(context.Context, string) (autologincode.Result, error)
}

func WithAutomaticLoginCodeFetcher(fetcher AutomaticLoginCodeFetcher) Option {
	return func(s *Service) { s.autoLoginCodeFetcher = fetcher }
}

// WithAutoLoginSettingsLock serializes credential changes with password/code
// submission. Waiting for a provider's new code never holds this lock.
func (s *Service) WithAutoLoginSettingsLock(ctx context.Context, id int64, update func() error) error {
	release, err := s.acquireOperation(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	return update()
}

// RunAutomaticLogin restores durable work once, then handles account events and
// individual deadlines. It never periodically scans accounts for expired sessions.
func (s *Service) RunAutomaticLogin(ctx context.Context, logger *slog.Logger) {
	s.runAutomaticLogin(ctx, logger, nil)
}

func (s *Service) automaticLoginDue(ctx context.Context, config domain.AccountAutoLoginConfig) bool {
	for _, channel := range []string{"apple_account", "icloud_web"} {
		if s.automaticChannelDue(ctx, config, channel) {
			return true
		}
	}
	return false
}

func (s *Service) automaticChannelDue(ctx context.Context, config domain.AccountAutoLoginConfig, channel string) bool {
	if !config.Enabled {
		return false
	}
	var info SessionInfo
	var err error
	if channel == "apple_account" {
		info, err = s.GetAccountSession(ctx, config.AccountID)
	} else {
		info, err = s.GetSession(ctx, config.AccountID)
	}
	if err != nil && Code(err) != CodeAccountSessionExpired {
		return false
	}
	if err == nil && info.Status == StatusAuthenticated {
		s.observeHealthyAutomaticChannel(ctx, config, channel)
		return false
	}
	state, err := s.repo.(automaticLoginRepository).GetAccountAutoLoginState(ctx, config.AccountID, channel)
	if err != nil {
		return errors.Is(err, store.ErrNotFound)
	}
	return state.ConfigRevision == config.Revision && state.Status != "needs_action" &&
		(state.NextAttemptAt == nil || !state.NextAttemptAt.After(s.now()))
}

func (s *Service) observeHealthyAutomaticChannel(ctx context.Context, config domain.AccountAutoLoginConfig, channel string) {
	repo := s.repo.(automaticLoginRepository)
	state, err := repo.GetAccountAutoLoginState(ctx, config.AccountID, channel)
	if err != nil || state.ConfigRevision != config.Revision || state.Status == "authenticated" ||
		state.LastErrorCode == CodeCredentialsInvalid || state.LastErrorCode == codeAutoLoginCredentials {
		return
	}
	now := s.now()
	state.Status, state.Failures, state.LastErrorCode, state.LastSuccessAt, state.NextAttemptAt = "authenticated", 0, "", &now, nil
	_ = repo.SaveAccountAutoLoginState(ctx, state)
}

func automaticLoginDelay(failures int) time.Duration {
	delay := min(5*time.Minute*time.Duration(1<<min(max(failures-1, 0), 3)), 30*time.Minute)
	var random [2]byte
	if _, err := rand.Read(random[:]); err == nil {
		delay += time.Duration(binary.BigEndian.Uint16(random[:])%61) * time.Second
	}
	return delay
}

func (s *Service) waitAutomaticLogin(ctx context.Context, delay time.Duration) error {
	if s.autoLoginWait != nil {
		return s.autoLoginWait(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
