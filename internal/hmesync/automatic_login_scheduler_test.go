package hmesync

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/autologincode"
	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

type automaticSchedulerRepository struct {
	*store.Store
	lists     chan struct{}
	reads     chan int64
	listCount atomic.Int32
	readCount atomic.Int32
}

func (r *automaticSchedulerRepository) ListEnabledAccountAutoLoginConfigs(ctx context.Context) ([]domain.AccountAutoLoginConfig, error) {
	r.listCount.Add(1)
	configs, err := r.Store.ListEnabledAccountAutoLoginConfigs(ctx)
	select {
	case r.lists <- struct{}{}:
	default:
	}
	return configs, err
}

func (r *automaticSchedulerRepository) GetAccountAutoLoginConfig(ctx context.Context, id int64) (domain.AccountAutoLoginConfig, error) {
	r.readCount.Add(1)
	config, err := r.Store.GetAccountAutoLoginConfig(ctx, id)
	select {
	case r.reads <- id:
	default:
	}
	return config, err
}

type automaticSchedulerCodeFetcher struct {
	fetch func(context.Context, string) (autologincode.Result, error)
}

func (f automaticSchedulerCodeFetcher) Fetch(ctx context.Context, url string) (autologincode.Result, error) {
	return f.fetch(ctx, url)
}

func automaticSchedulerAccount(t *testing.T, db *store.Store, index int, enabled bool) domain.Account {
	t.Helper()
	cipher, err := secure.NewCipher([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	email := fmt.Sprintf("scheduler-%d@icloud.com", index)
	ciphertext, err := cipher.EncryptAccountAutoLogin(email, secure.AccountAutoLoginCredentials{
		Password: "fixture-password", CodeURL: fmt.Sprintf("https://codes.example.test/%d", index),
	})
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateAccount(context.Background(), domain.Account{
		Name: "scheduler fixture", Email: email, IMAPHost: "imap.mail.me.com", IMAPPort: 993,
		IMAPUsername: email, PasswordCiphertext: "fixture-imap", Enabled: true,
		AutoLogin: &domain.AccountAutoLoginConfig{Enabled: enabled, Region: "global", CredentialsCiphertext: ciphertext},
	})
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func automaticSchedulerWait[T any](t *testing.T, events <-chan T, description string) T {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
		var zero T
		return zero
	}
}

func automaticSchedulerStart(t *testing.T, service *Service, repo *automaticSchedulerRepository) chan<- time.Time {
	t.Helper()
	repo.reads = make(chan int64, 256)
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.runAutomaticLogin(ctx, nil, ticks)
	}()
	t.Cleanup(func() {
		cancel()
		automaticSchedulerWait(t, done, "scheduler shutdown and worker cancellation")
	})
	automaticSchedulerWait(t, repo.lists, "initial scheduler refresh")
	return ticks
}

func automaticSchedulerFlush(t *testing.T, service *Service, repo *automaticSchedulerRepository) {
	t.Helper()
	const barrierID = int64(1 << 60)
	// A second event passes through the next select after the entire first
	// coalesced batch, regardless of map iteration order within that batch.
	for round := 0; round < 2; round++ {
		service.NotifyAutomaticLogin(barrierID)
		for automaticSchedulerWait(t, repo.reads, "account event barrier") != barrierID {
		}
	}
}

func automaticSchedulerTick(t *testing.T, ticks chan<- time.Time, service *Service, repo *automaticSchedulerRepository, now time.Time) {
	t.Helper()
	select {
	case ticks <- now:
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler did not accept the next tick")
	}
	automaticSchedulerFlush(t, service, repo)
	if got := repo.listCount.Load(); got != 1 {
		t.Fatalf("deadline/event handling rescanned enabled accounts: lists=%d", got)
	}
}

func TestAutomaticLoginSchedulerDeduplicatesEventsWithoutBlockingOtherAccounts(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	automaticSchedulerAccount(t, db, 1, true)
	automaticSchedulerAccount(t, db, 2, true)
	repo := &automaticSchedulerRepository{Store: db, lists: make(chan struct{}, 16)}
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	service := newTestService(t, repo, &fakeAppleClient{}, &fakeLocker{}, now)
	started := make(chan string, 8)
	var fetches atomic.Int32
	service.autoLoginCodeFetcher = automaticSchedulerCodeFetcher{fetch: func(ctx context.Context, url string) (autologincode.Result, error) {
		fetches.Add(1)
		started <- url
		<-ctx.Done()
		return autologincode.Result{}, ctx.Err()
	}}
	ticks := automaticSchedulerStart(t, service, repo)
	first := automaticSchedulerWait(t, started, "first account code baseline")
	second := automaticSchedulerWait(t, started, "second account while first remains blocked")
	if first == second {
		t.Fatalf("one account started twice while another was blocked: %q", first)
	}
	// Both reserved deadlines are now in the past. The in-flight account gate
	// must still prevent another worker on each subsequent scheduler tick.
	clock.Add(int64(time.Hour))
	for i := 0; i < 5; i++ {
		service.NotifyAutomaticLogin(1)
		service.NotifyAutomaticLogin(2)
		automaticSchedulerTick(t, ticks, service, repo, now())
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("cross-tick duplicate worker reached code provider: fetches=%d want=2", got)
	}
}

func TestAutomaticLoginSchedulerDisabledConfigurationAndHealthyChannelsAvoidNetwork(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "inactive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	disabled := automaticSchedulerAccount(t, db, 1, false)
	healthy := automaticSchedulerAccount(t, db, 2, true)
	repo := &automaticSchedulerRepository{Store: db, lists: make(chan struct{}, 16)}
	now := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	var appleCalls, codeCalls atomic.Int32
	client := &accountTestClient{
		accountSignIn: func(session apple.AccountSession) (apple.AccountSession, bool, error) {
			appleCalls.Add(1)
			return session, false, errors.New("unexpected fixture Apple request")
		},
		fakeAppleClient: fakeAppleClient{signIn: func(context.Context, string, string, apple.Region, *apple.Session) (apple.Session, bool, error) {
			appleCalls.Add(1)
			return apple.Session{}, false, errors.New("unexpected fixture Apple request")
		}},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	service.autoLoginCodeFetcher = automaticSchedulerCodeFetcher{fetch: func(context.Context, string) (autologincode.Result, error) {
		codeCalls.Add(1)
		return autologincode.Result{}, autologincode.ErrNoCode
	}}
	managed := apple.AccountSession{AppleID: healthy.Email, Region: apple.RegionGlobal,
		APIKey: "fixture-key", AuthenticatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := service.persistAccountManagementSession(context.Background(), healthy.ID, identityOf(healthy), managed); err != nil {
		t.Fatal(err)
	}
	if _, err := service.saveSession(context.Background(), healthy.ID, apple.Session{AppleID: healthy.Email,
		Region: apple.RegionGlobal, SessionToken: "fixture-web-token", Cookies: []apple.PersistentCookie{
			{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "fixture-cookie", Expires: now.Add(time.Hour)},
		}}); err != nil {
		t.Fatal(err)
	}
	if info, err := service.GetAccountSession(context.Background(), healthy.ID); err != nil || info.Status != StatusAuthenticated {
		t.Fatalf("management fixture not healthy: %#v %v", info, err)
	}
	if info, err := service.GetSession(context.Background(), healthy.ID); err != nil || info.Status != StatusAuthenticated {
		t.Fatalf("web fixture not healthy: %#v %v", info, err)
	}
	for _, account := range []domain.Account{disabled, healthy} {
		config, err := db.GetAccountAutoLoginConfig(context.Background(), account.ID)
		if err != nil {
			t.Fatal(err)
		}
		if service.automaticLoginDue(context.Background(), config) {
			t.Fatalf("disabled configuration or healthy channel scheduled a login for account %d", account.ID)
		}
	}
	ticks := automaticSchedulerStart(t, service, repo)
	automaticSchedulerFlush(t, service, repo)
	before := repo.readCount.Load()
	for i := 0; i < 4; i++ {
		automaticSchedulerTick(t, ticks, service, repo, now)
	}
	if got := repo.readCount.Load() - before; got != 8 {
		t.Fatalf("idle deadline wakeups queried accounts: %d reads (only 8 test barrier reads expected)", got)
	}
	service.NotifyAutomaticLogin(disabled.ID)
	automaticSchedulerFlush(t, service, repo)
	if got := appleCalls.Load(); got != 0 {
		t.Fatalf("disabled/healthy accounts reached Apple: %d calls", got)
	}
	if got := codeCalls.Load(); got != 0 {
		t.Fatalf("disabled/healthy accounts polled verification provider: %d calls", got)
	}
}

func TestAutomaticLoginSchedulerRestartHonorsReservedRetryDeadlines(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	account := automaticSchedulerAccount(t, db, 1, true)
	config, err := db.GetAccountAutoLoginConfig(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	service := newTestService(t, db, &fakeAppleClient{}, &fakeLocker{}, now)
	var latestDeadline time.Time
	for _, channel := range []string{domain.AutoLoginChannelAppleAccount, domain.AutoLoginChannelICloudWeb} {
		attempt, err := service.prepareAutomaticLogin(ctx, config, channel)
		if err != nil || attempt == nil {
			t.Fatalf("reserve %s attempt: %v", channel, err)
		}
		state, err := db.GetAccountAutoLoginState(ctx, account.ID, channel)
		if err != nil || state.NextAttemptAt == nil || state.Status != "logging_in" || state.Failures != 1 {
			t.Fatalf("missing durable reservation for %s: %#v %v", channel, state, err)
		}
		if state.NextAttemptAt.After(latestDeadline) {
			latestDeadline = *state.NextAttemptAt
		}
		attempt.credentials = secure.AccountAutoLoginCredentials{}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := &automaticSchedulerRepository{Store: db, lists: make(chan struct{}, 16)}
	service = newTestService(t, repo, &fakeAppleClient{}, &fakeLocker{}, now)
	if service.automaticLoginDue(ctx, config) {
		t.Fatal("reopened scheduler ignored an in-progress attempt's durable retry deadline")
	}
	started := make(chan struct{}, 8)
	var fetches atomic.Int32
	service.autoLoginCodeFetcher = automaticSchedulerCodeFetcher{fetch: func(ctx context.Context, _ string) (autologincode.Result, error) {
		fetches.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		return autologincode.Result{}, ctx.Err()
	}}
	ticks := automaticSchedulerStart(t, service, repo)
	for i := 0; i < 3; i++ {
		service.NotifyAutomaticLogin(account.ID)
		automaticSchedulerTick(t, ticks, service, repo, now())
	}
	if got := fetches.Load(); got != 0 {
		t.Fatalf("restart ignored reserved deadline: %d verification-provider requests", got)
	}
	for _, channel := range []string{domain.AutoLoginChannelAppleAccount, domain.AutoLoginChannelICloudWeb} {
		state, err := db.GetAccountAutoLoginState(ctx, account.ID, channel)
		if err != nil || state.Failures != 1 || state.NextAttemptAt == nil || state.NextAttemptAt.After(latestDeadline) {
			t.Fatalf("repeated events changed the reserved retry: channel=%s err=%v", channel, err)
		}
	}
	clock.Store(latestDeadline.Add(time.Second).UnixNano())
	automaticSchedulerTick(t, ticks, service, repo, now())
	automaticSchedulerWait(t, started, "retry after the durable deadline")
	if got := fetches.Load(); got != 1 {
		t.Fatalf("deadline expiry started %d attempts, want 1", got)
	}
}

func TestAutomaticLoginSchedulerConfigurationEventAndDisable(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "configuration.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	account := automaticSchedulerAccount(t, db, 1, false)
	repo := &automaticSchedulerRepository{Store: db, lists: make(chan struct{}, 16)}
	now := time.Now().UTC()
	s := newTestService(t, repo, &fakeAppleClient{}, &fakeLocker{}, func() time.Time { return now })
	started := make(chan struct{}, 4)
	s.autoLoginCodeFetcher = automaticSchedulerCodeFetcher{fetch: func(ctx context.Context, _ string) (autologincode.Result, error) {
		started <- struct{}{}
		<-ctx.Done()
		return autologincode.Result{}, ctx.Err()
	}}
	automaticSchedulerStart(t, s, repo)
	config, err := db.GetAccountAutoLoginConfig(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	config.Enabled = true
	account.AutoLogin = &config
	if _, err := db.UpdateAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	s.NotifyAutomaticLogin(account.ID)
	automaticSchedulerWait(t, started, "newly enabled account event without a scan")
	if repo.listCount.Load() != 1 {
		t.Fatal("enabling one account rescanned all configurations")
	}
}

func TestAutomaticLoginSchedulerRealOneShotDeadline(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "deadline.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	account := automaticSchedulerAccount(t, db, 1, true)
	config, err := db.GetAccountAutoLoginConfig(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(150 * time.Millisecond)
	for _, channel := range []string{domain.AutoLoginChannelAppleAccount, domain.AutoLoginChannelICloudWeb} {
		if err := db.SaveAccountAutoLoginState(context.Background(), domain.AccountAutoLoginState{
			AccountID: account.ID, Channel: channel, ConfigRevision: config.Revision,
			Status: "retry_wait", Failures: 1, NextAttemptAt: &deadline,
		}); err != nil {
			t.Fatal(err)
		}
	}
	repo := &automaticSchedulerRepository{Store: db, lists: make(chan struct{}, 16)}
	s := newTestService(t, repo, &fakeAppleClient{}, &fakeLocker{}, time.Now)
	started := make(chan time.Time, 2)
	s.autoLoginCodeFetcher = automaticSchedulerCodeFetcher{fetch: func(ctx context.Context, _ string) (autologincode.Result, error) {
		started <- time.Now()
		<-ctx.Done()
		return autologincode.Result{}, ctx.Err()
	}}
	automaticSchedulerStart(t, s, repo)
	if at := automaticSchedulerWait(t, started, "one-shot deadline without any account event"); at.Before(deadline) {
		t.Fatal("retry started before the durable deadline")
	}
	if repo.listCount.Load() != 1 {
		t.Fatal("retry timer performed a full configuration scan")
	}
}

func TestAutomaticLoginNextDeadlineUsesExpiryAndInteractiveChallenge(t *testing.T) {
	s, db, account, _, _ := automaticLoginFixture(t)
	ctx := context.Background()
	now := s.now()
	expires := now.Add(time.Hour)
	if err := s.saveAccountManagementSession(ctx, account.ID, apple.AccountSession{
		AppleID: account.Email, Region: apple.RegionGlobal, APIKey: "fixture-key",
		AuthenticatedAt: now, ExpiresAt: expires,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.saveSession(ctx, account.ID, apple.Session{AppleID: account.Email,
		Region: apple.RegionGlobal, SessionToken: "fixture-web", Cookies: []apple.PersistentCookie{
			{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "fixture", Expires: expires.Add(time.Hour)},
		}}); err != nil {
		t.Fatal(err)
	}
	if _, next, err := s.nextAutomaticLogin(ctx, account.ID); err != nil || !next.Equal(expires) {
		t.Fatalf("expected exact earliest session expiry: next=%v err=%v", next, err)
	}
	if err := db.DeleteAppleAccountSession(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	challengeExpiry := now.Add(3 * time.Minute)
	s.accountAuthChallenges = map[int64]accountAuthChallenge{account.ID: {expires: challengeExpiry}}
	if _, next, err := s.nextAutomaticLogin(ctx, account.ID); err != nil || !next.Equal(challengeExpiry) {
		t.Fatalf("manual challenge did not defer automatic login: next=%v err=%v", next, err)
	}
	delete(s.accountAuthChallenges, account.ID)
	if _, next, err := s.nextAutomaticLogin(ctx, account.ID); err != nil || !next.Equal(now) {
		t.Fatalf("missing session was not eligible: next=%v err=%v", next, err)
	}
	account.Enabled = false
	if _, err := db.UpdateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	if _, next, err := s.nextAutomaticLogin(ctx, account.ID); err != nil || !next.IsZero() {
		t.Fatalf("disabled account retained a login timer: next=%v err=%v", next, err)
	}
}

func TestAutomaticLoginSchedulerUpstreamAuthenticationEventHonorsSwitch(t *testing.T) {
	for _, test := range []struct {
		name      string
		enabled   bool
		status    int
		kind      error
		wantLogin bool
	}{
		{"expired_enabled", true, 401, apple.ErrInvalidSession, true},
		{"expired_disabled", false, 401, apple.ErrInvalidSession, false},
		{"rate_limit", true, 429, apple.ErrService, false},
		{"upstream_unavailable", true, 503, apple.ErrService, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "upstream.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			account := automaticSchedulerAccount(t, db, 1, test.enabled)
			repo := &automaticSchedulerRepository{Store: db, lists: make(chan struct{}, 16)}
			now := time.Now().UTC()
			client := &accountTestClient{accountRefresh: func(current apple.AccountSession) (apple.AccountSession, error) {
				return current, &apple.Error{Kind: test.kind, StatusCode: test.status}
			}}
			s := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
			if err := s.saveAccountManagementSession(context.Background(), account.ID, apple.AccountSession{
				AppleID: account.Email, Region: apple.RegionGlobal, APIKey: "fixture-key", SCNT: "fixture-scnt",
				AuthenticatedAt: now, ExpiresAt: now.Add(time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.saveSession(context.Background(), account.ID, apple.Session{
				AppleID: account.Email, Region: apple.RegionGlobal, SessionToken: "fixture-web",
			}); err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{}, 2)
			var fetches atomic.Int32
			s.autoLoginCodeFetcher = automaticSchedulerCodeFetcher{fetch: func(ctx context.Context, _ string) (autologincode.Result, error) {
				fetches.Add(1)
				started <- struct{}{}
				<-ctx.Done()
				return autologincode.Result{}, ctx.Err()
			}}
			automaticSchedulerStart(t, s, repo)
			automaticSchedulerFlush(t, s, repo)
			if fetches.Load() != 0 {
				t.Fatal("healthy startup attempted a login")
			}
			if err := s.KeepAliveAccountSession(context.Background(), account.ID); err == nil {
				t.Fatal("expected fixture upstream failure")
			}
			automaticSchedulerFlush(t, s, repo)
			if test.wantLogin {
				automaticSchedulerWait(t, started, "explicit upstream expiry event")
			} else if fetches.Load() != 0 {
				t.Fatal("disabled account or non-authentication failure triggered login")
			}
			if repo.listCount.Load() != 1 {
				t.Fatal("upstream event rescanned unrelated accounts")
			}
		})
	}
}
