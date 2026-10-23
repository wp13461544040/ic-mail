package hmesync

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

type renewalContextClient struct {
	accountTestClient
	refresh func(context.Context, apple.AccountSession) (apple.AccountSession, error)
}

type renewalAccountListRepository struct {
	*store.Store
	accounts []domain.Account
}

func (r *renewalAccountListRepository) ListAccounts(context.Context) ([]domain.Account, error) {
	return r.accounts, nil
}

func (c *renewalContextClient) RefreshAccountSession(ctx context.Context, session apple.AccountSession) (apple.AccountSession, error) {
	return c.refresh(ctx, session)
}

func renewalDueSession(account domain.Account, now time.Time) apple.AccountSession {
	return apple.AccountSession{
		AppleID: account.Email, Region: apple.RegionGlobal, APIKey: "fixture",
		AuthenticatedAt: now.Add(-5 * time.Minute), UpdatedAt: now.Add(-5 * time.Minute),
	}
}

func addRenewalAccount(t *testing.T, db *store.Store, template domain.Account, index int) domain.Account {
	t.Helper()
	template.ID = 0
	template.Name = fmt.Sprintf("renewal-%d", index)
	template.Email = fmt.Sprintf("renewal-%d@icloud.com", index)
	template.IMAPUsername = template.Email
	account, err := db.CreateAccount(context.Background(), template)
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func saveRenewalSession(t *testing.T, s *Service, account domain.Account, session apple.AccountSession) {
	t.Helper()
	if err := s.saveAccountManagementSession(context.Background(), account.ID, session); err != nil {
		t.Fatal(err)
	}
}

func startRenewalScheduler(t *testing.T, s *Service, ticks <-chan time.Time) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runAccountSessionRenewal(ctx, nil, ticks)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("renewal scheduler did not exit")
		}
	})
	return cancel, done
}

func waitRenewalCall(t *testing.T, calls <-chan string) string {
	t.Helper()
	select {
	case id := <-calls:
		return id
	case <-time.After(3 * time.Second):
		t.Fatal("renewal call did not start")
		return ""
	}
}

func tickRenewalScheduler(t *testing.T, ticks chan<- time.Time) {
	t.Helper()
	select {
	case ticks <- time.Now():
	case <-time.After(3 * time.Second):
		t.Fatal("renewal scheduler did not read local tick")
	}
}

func TestAccountRenewalFourBusyAccountsDoNotBlockAnotherAccount(t *testing.T) {
	s, db, first := independentAccountFixture(t, true)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	for i := 0; i < 4; i++ {
		account := first
		if i > 0 {
			account = addRenewalAccount(t, db, first, i)
		}
		saveRenewalSession(t, s, account, renewalDueSession(account, now))
		release, err := s.acquireOperation(context.Background(), account.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	second := addRenewalAccount(t, db, first, 100)
	saveRenewalSession(t, s, second, renewalDueSession(second, now))
	// Four busy accounts must not occupy global slots ahead of this account.
	calls := make(chan string, 2)
	s.client = &renewalContextClient{refresh: func(ctx context.Context, session apple.AccountSession) (apple.AccountSession, error) {
		calls <- session.AppleID
		<-ctx.Done()
		return session, ctx.Err()
	}}
	cancel, done := startRenewalScheduler(t, s, nil)
	if got := waitRenewalCall(t, calls); got != second.Email {
		t.Fatalf("unexpected account refreshed: %s", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("operation-lock waiter did not cancel")
	}
}

func TestAccountRenewalSlowNetworkDoesNotBlockAnotherAccount(t *testing.T) {
	s, db, first := independentAccountFixture(t, true)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	second := addRenewalAccount(t, db, first, 2)
	for _, account := range []domain.Account{first, second} {
		saveRenewalSession(t, s, account, renewalDueSession(account, now))
	}
	calls := make(chan string, 2)
	s.client = &renewalContextClient{refresh: func(ctx context.Context, session apple.AccountSession) (apple.AccountSession, error) {
		calls <- session.AppleID
		<-ctx.Done()
		return session, ctx.Err()
	}}
	startRenewalScheduler(t, s, nil)
	seen := map[string]bool{waitRenewalCall(t, calls): true}
	seen[waitRenewalCall(t, calls)] = true
	if !seen[first.Email] || !seen[second.Email] {
		t.Fatalf("slow network call prevented another account from starting: %v", seen)
	}
}

func TestAccountRenewalRunsAllDueAccountsIndependently(t *testing.T) {
	s, db, first := independentAccountFixture(t, true)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	const accountCount = 32
	for i := 0; i < accountCount; i++ {
		account := first
		if i > 0 {
			account = addRenewalAccount(t, db, first, i)
		}
		saveRenewalSession(t, s, account, renewalDueSession(account, now))
	}
	calls := make(chan string, accountCount+1)
	var active, peak atomic.Int32
	s.client = &renewalContextClient{refresh: func(ctx context.Context, session apple.AccountSession) (apple.AccountSession, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for previous := peak.Load(); current > previous && !peak.CompareAndSwap(previous, current); previous = peak.Load() {
		}
		calls <- session.AppleID
		<-ctx.Done()
		return session, ctx.Err()
	}}
	ticks := make(chan time.Time)
	cancel, done := startRenewalScheduler(t, s, ticks)
	seen := make(map[string]bool)
	for i := 0; i < accountCount; i++ {
		id := waitRenewalCall(t, calls)
		if seen[id] {
			t.Fatal("account renewed twice in the same scan")
		}
		seen[id] = true
	}
	tickRenewalScheduler(t, ticks)
	tickRenewalScheduler(t, ticks)
	select {
	case id := <-calls:
		t.Fatalf("active account scheduled again: %s", id)
	case <-time.After(50 * time.Millisecond):
	}
	if peak.Load() != accountCount {
		t.Fatalf("peak calls = %d, want %d", peak.Load(), accountCount)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("active workers did not stop")
	}
	if active.Load() != 0 {
		t.Fatalf("scheduler returned with %d calls active", active.Load())
	}
}

func TestAccountRenewalDoesNotQueueSameAccountAcrossTicks(t *testing.T) {
	s, _, account := independentAccountFixture(t, true)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	saveRenewalSession(t, s, account, renewalDueSession(account, now))
	calls := make(chan string, 8)
	s.client = &renewalContextClient{refresh: func(ctx context.Context, session apple.AccountSession) (apple.AccountSession, error) {
		calls <- session.AppleID
		<-ctx.Done()
		return session, ctx.Err()
	}}
	ticks := make(chan time.Time)
	startRenewalScheduler(t, s, ticks)
	waitRenewalCall(t, calls)
	for i := 0; i < 5; i++ {
		tickRenewalScheduler(t, ticks)
	}
	s.operationMu.Lock()
	refs := s.operationLock[account.ID].refs
	s.operationMu.Unlock()
	if refs != 1 {
		t.Fatalf("same-account operation references = %d, want 1", refs)
	}
	select {
	case <-calls:
		t.Fatal("same account has concurrent refresh calls")
	default:
	}
}

func TestAccountRenewalSkipsIneligibleAccountsBeforeOperationLock(t *testing.T) {
	s, db, first := independentAccountFixture(t, true)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	var skipped []domain.Account
	for index, kind := range []string{"no-session", "disabled", "non-icloud", "not-due", "rejected", "backoff", "no-key", "no-authentication"} {
		account := first
		if index > 0 {
			account = addRenewalAccount(t, db, first, index)
		}
		session := renewalDueSession(account, now)
		switch kind {
		case "disabled":
			account.Enabled = false
		case "non-icloud":
			account.MailboxType = domain.MailboxTypeCustom
			account.EmailSuffix = "example.com"
		case "not-due":
			session.AuthenticatedAt = now
		case "rejected":
			session.RefreshRejected = true
		case "backoff":
			session.RefreshAfter = now.Add(time.Hour)
		case "no-key":
			session.APIKey = ""
		case "no-authentication":
			session.AuthenticatedAt = time.Time{}
		}
		if kind != "non-icloud" {
			if _, err := db.UpdateAccount(context.Background(), account); err != nil {
				t.Fatal(err)
			}
		}
		if kind != "no-session" {
			saveRenewalSession(t, s, account, session)
		}
		skipped = append(skipped, account)
		release, err := s.acquireOperation(context.Background(), account.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	eligible := addRenewalAccount(t, db, first, 100)
	saveRenewalSession(t, s, eligible, renewalDueSession(eligible, now))
	// Keep storage fixtures valid while supplying a non-iCloud account to the
	// scheduler's local eligibility check.
	s.repo = &renewalAccountListRepository{Store: db, accounts: append(skipped, eligible)}
	calls := make(chan string, 16)
	s.client = &renewalContextClient{refresh: func(ctx context.Context, session apple.AccountSession) (apple.AccountSession, error) {
		calls <- session.AppleID
		<-ctx.Done()
		return session, ctx.Err()
	}}
	ticks := make(chan time.Time)
	startRenewalScheduler(t, s, ticks)
	if got := waitRenewalCall(t, calls); got != eligible.Email {
		t.Fatalf("ineligible account reached upstream: %s", got)
	}
	tickRenewalScheduler(t, ticks)
	tickRenewalScheduler(t, ticks)
	s.operationMu.Lock()
	for _, account := range skipped {
		if refs := s.operationLock[account.ID].refs; refs != 1 {
			t.Errorf("ineligible account %d acquired an operation waiter: refs=%d", account.ID, refs)
		}
	}
	s.operationMu.Unlock()
}

func TestAccountRenewalCancellationWaitsForOwnCalls(t *testing.T) {
	s, _, account := independentAccountFixture(t, true)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	saveRenewalSession(t, s, account, renewalDueSession(account, now))
	calls := make(chan string, 1)
	canceled := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	s.client = &renewalContextClient{refresh: func(ctx context.Context, session apple.AccountSession) (apple.AccountSession, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 25*time.Second || time.Until(deadline) < 24*time.Second {
			t.Error("renewal call did not retain its 25-second timeout")
		}
		calls <- session.AppleID
		<-ctx.Done()
		close(canceled)
		<-release
		return session, ctx.Err()
	}}
	cancel, done := startRenewalScheduler(t, s, nil)
	waitRenewalCall(t, calls)
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("renewal call did not receive cancellation")
	}
	select {
	case <-done:
		t.Fatal("scheduler returned before its active call exited")
	case <-time.After(50 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler did not join its canceled worker")
	}
}
