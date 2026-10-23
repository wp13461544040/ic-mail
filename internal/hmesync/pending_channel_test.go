package hmesync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/autocreate"
	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

type pendingChannelClient struct {
	*accountTestClient
	complete func(context.Context, apple.AccountSession, string, string, string) (apple.Alias, apple.AccountSession, error)
	detail   func(context.Context, apple.AccountSession, apple.Alias) (apple.Alias, apple.AccountSession, error)
}

func (c *pendingChannelClient) CompleteAccountAlias(ctx context.Context, s apple.AccountSession, address, label, note string) (apple.Alias, apple.AccountSession, error) {
	return c.complete(ctx, s, address, label, note)
}

func (c *pendingChannelClient) GetAccountAlias(ctx context.Context, s apple.AccountSession, a apple.Alias) (apple.Alias, apple.AccountSession, error) {
	return c.detail(ctx, s, a)
}

// Run real scheduler + SQLite publication with only the Apple protocol replaced.
// The original pending address survives service restarts while Web slots proceed.
func TestPendingCompletionThrottleKeepsWebSlotsAcrossRestarts(t *testing.T) {
	s, db, account := independentAccountFixture(t, true)
	ctx := context.Background()
	now := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	managed := apple.AccountSession{AppleID: account.Email, APIKey: "fixture", SCNT: "fixture", ExpiresAt: now.Add(8 * time.Hour)}
	if _, err := s.saveSession(ctx, account.ID, apple.Session{AppleID: account.Email, Region: apple.RegionGlobal, DSID: "fixture-dsid", Account: &managed}); err != nil {
		t.Fatal(err)
	}
	if err := s.saveAccountManagementSession(ctx, account.ID, managed); err != nil {
		t.Fatal(err)
	}
	client := &pendingChannelClient{accountTestClient: s.client.(*accountTestClient)}
	accountCreates, webCreates, completes, details := 0, 0, 0, 0
	var directory []apple.Alias
	client.validate = func(_ context.Context, a apple.Session) (apple.Session, error) { return a, nil }
	client.list = func(_ context.Context, a apple.Session) (apple.ListResult, apple.Session, error) {
		return apple.ListResult{SelectedForwardTo: account.Email, Aliases: directory}, a, nil
	}
	client.accountCreate = func(_ context.Context, a apple.AccountSession) (apple.Alias, apple.AccountSession, error) {
		accountCreates++
		return apple.Alias{HME: "original-pending@icloud.com"}, a, &apple.Error{Kind: apple.ErrService, StatusCode: 503}
	}
	client.complete = func(_ context.Context, a apple.AccountSession, address, _, _ string) (apple.Alias, apple.AccountSession, error) {
		completes++
		if address != "original-pending@icloud.com" {
			t.Fatal("completion changed candidate")
		}
		return apple.Alias{}, a, &apple.Error{Kind: apple.ErrService, StatusCode: 429, RetryAfter: 2 * time.Hour}
	}
	client.detail = func(context.Context, apple.AccountSession, apple.Alias) (apple.Alias, apple.AccountSession, error) {
		details++
		t.Fatal("completion has not supplied an ID")
		return apple.Alias{}, managed, nil
	}
	client.create = func(_ context.Context, a apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
		webCreates++
		created := apple.Alias{HME: fmt.Sprintf("web-slot-%d@icloud.com", webCreates), IsActive: true, ForwardToEmail: account.Email}
		directory = append(directory, created)
		return created, a, nil
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	newManager := func() *autocreate.Manager {
		// A fresh service must read channel cooldown and pending provenance from DB.
		s = newTestService(t, db, client, &fakeLocker{}, func() time.Time { return now })
		s.autoCreateConfirmationDelays = nil
		m, err := autocreate.New(db, s.CreateAutoAlias, logger, autocreate.WithClock(s.now),
			autocreate.WithRandomFunc(func(int) int { return 0 }),
			autocreate.WithWait(func(context.Context, time.Duration) bool { return false }),
			autocreate.WithChannelAvailability(s.CreationChannelAvailability))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := newManager()
	if _, err := m.SetEnabled(ctx, account.ID, true); err != nil {
		t.Fatal(err)
	}
	var cooldown time.Time
	for slot := range domain.AppleCreationHourlyLimit {
		m = newManager()
		plan, err := m.GetSchedule(ctx, account.ID)
		if err != nil {
			t.Fatal(err)
		}
		now = *plan.NextRunAt
		m.Run(ctx)
		after, err := m.GetSchedule(ctx, account.ID)
		if err != nil || !after.Enabled || after.NextRunAt == nil {
			t.Fatalf("slot %d lost schedule: %v", slot, err)
		}
		if slot != domain.AppleCreationHourlyLimit-1 && !after.NextRunAt.Equal(plan.PlannedAt[1]) {
			t.Fatalf("slot %d changed whole schedule", slot)
		}
		until, err := db.AppleCreationChannelPausedUntil(ctx, account.ID, "apple_account")
		if err != nil {
			t.Fatal(err)
		}
		if slot == 1 {
			cooldown = now.Add(2 * time.Hour)
		}
		if slot > 0 && !until.Equal(cooldown) {
			t.Fatal("cooldown was shortened or extended by later polls")
		}
	}
	if accountCreates != 1 || completes != 1 || details != 0 || webCreates != 4 {
		t.Fatalf("remote calls: account=%d complete=%d detail=%d web=%d", accountCreates, completes, details, webCreates)
	}
	if count, err := db.CountEnabledAliasesByAccount(ctx, account.ID); err != nil || count != 4 {
		t.Fatalf("Web aliases were not published: count=%d err=%v", count, err)
	}
	pending, err := db.GetPendingAutoAliasConfirmationForChannel(ctx, account.ID, "apple_account")
	if err != nil || pending.Address != "original-pending@icloud.com" || pending.CreationChannel != "apple_account" {
		t.Fatalf("lost original pending: %v", err)
	}
	if _, err := db.GetPendingAutoAliasConfirmationForChannel(ctx, account.ID, "icloud_web"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ACC candidate blocked Web: %v", err)
	}
	// Exhausted Web quota must not fall back to generating another ACC address
	// while ACC still owns an unresolved candidate.
	now = now.Add(domain.AppleCreationMinInterval)
	if _, err := s.CreateAutoAlias(domain.WithScheduledCreationSlot(ctx, 5), account.ID); err == nil || accountCreates != 1 {
		t.Fatalf("fallback bypassed pending candidate: %v", err)
	}
	// Published in the Web directory, the original candidate can be confirmed
	// read-only during ACC cooldown without repeating completion or generation.
	directory = append(directory, apple.Alias{HME: pending.Address, IsActive: true, ForwardToEmail: account.Email})
	if _, err := s.CreateAutoAlias(domain.WithScheduledCreationSlot(ctx, 0), account.ID); err != nil {
		t.Fatalf("read-only pending confirmation during cooldown: %v", err)
	}
	if completes != 1 || accountCreates != 1 {
		t.Fatal("confirmation replayed creation")
	}
}

func TestPendingDetailCheckpointAvoidsCompletionReplay(t *testing.T) {
	s, db, account := independentAccountFixture(t, true)
	ctx := context.Background()
	now := time.Now().UTC()
	managed := apple.AccountSession{AppleID: account.Email, APIKey: "fixture", SCNT: "fixture", ExpiresAt: now.Add(8 * time.Hour)}
	if _, err := s.saveSession(ctx, account.ID, apple.Session{AppleID: account.Email, Region: apple.RegionGlobal, DSID: "fixture-dsid", Account: &managed}); err != nil {
		t.Fatal(err)
	}
	client := &pendingChannelClient{accountTestClient: s.client.(*accountTestClient)}
	client.validate = func(_ context.Context, a apple.Session) (apple.Session, error) { return a, nil }
	client.list = func(_ context.Context, a apple.Session) (apple.ListResult, apple.Session, error) {
		return apple.ListResult{SelectedForwardTo: account.Email}, a, nil
	}
	creates, details := 0, 0
	client.accountCreate = func(_ context.Context, a apple.AccountSession) (apple.Alias, apple.AccountSession, error) {
		creates++
		return apple.Alias{HME: "detail-pending@icloud.com", AnonymousID: "exact-id"}, a, nil
	}
	client.complete = func(context.Context, apple.AccountSession, string, string, string) (apple.Alias, apple.AccountSession, error) {
		t.Fatal("known ID replayed completion")
		return apple.Alias{}, managed, nil
	}
	client.detail = func(_ context.Context, a apple.AccountSession, alias apple.Alias) (apple.Alias, apple.AccountSession, error) {
		details++
		if alias.AnonymousID != "exact-id" || alias.HME != "detail-pending@icloud.com" {
			t.Fatal("detail changed original candidate")
		}
		if details == 1 {
			return alias, a, &apple.Error{Kind: apple.ErrService, StatusCode: 429, RetryAfter: 2 * time.Hour}
		}
		alias.IsActive, alias.ForwardToEmail = true, account.Email
		return alias, a, nil
	}
	newService := func() *Service {
		s := newTestService(t, db, client, &fakeLocker{}, func() time.Time { return now })
		s.autoCreateConfirmationDelays = nil
		return s
	}
	s = newService()
	if _, err := s.CreateAutoAlias(ctx, account.ID); !errors.Is(err, ErrAliasConfirmationPending) {
		t.Fatalf("first detail failure: %v", err)
	}
	s = newService()
	if _, err := s.CreateAutoAlias(ctx, account.ID); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("lost persistent detail cooldown: %v", err)
	}
	if creates != 1 || details != 1 {
		t.Fatal("poll repeated upstream work during cooldown")
	}
	now = now.Add(2 * time.Hour)
	created, err := s.CreateAutoAlias(ctx, account.ID)
	if err != nil || !created.Enabled || creates != 1 || details != 2 {
		t.Fatalf("detail recovery: creates=%d details=%d err=%v", creates, details, err)
	}
}

func TestLegacyPendingCompletionThrottleKeepsAccountSchedule(t *testing.T) {
	s, db, account := independentAccountFixture(t, true)
	ctx := context.Background()
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	managed := apple.AccountSession{AppleID: account.Email, APIKey: "fixture", SCNT: "fixture", ExpiresAt: now.Add(8 * time.Hour)}
	web := apple.Session{AppleID: account.Email, Region: apple.RegionGlobal, DSID: "fixture-dsid", Account: &managed}
	record, err := s.saveSession(ctx, account.ID, web)
	if err != nil {
		t.Fatal(err)
	}
	pending, _, err := db.CreateAliasWithPendingAPIKey(ctx, record, domain.Alias{AccountID: account.ID,
		Address: "legacy-pending@icloud.com", APIKeyHash: []byte("fixture"), APIKeyPrefix: "fixture"}, "encrypted-fixture")
	if err != nil {
		t.Fatal(err)
	}
	client := &pendingChannelClient{accountTestClient: s.client.(*accountTestClient)}
	client.validate = func(_ context.Context, a apple.Session) (apple.Session, error) { return a, nil }
	client.list = func(_ context.Context, a apple.Session) (apple.ListResult, apple.Session, error) {
		return apple.ListResult{SelectedForwardTo: account.Email}, a, nil
	}
	calls := 0
	client.complete = func(_ context.Context, a apple.AccountSession, address, _, _ string) (apple.Alias, apple.AccountSession, error) {
		calls++
		if address != pending.Address {
			t.Fatal("legacy candidate changed")
		}
		return apple.Alias{}, a, &apple.Error{Kind: apple.ErrService, StatusCode: 429}
	}
	for range 2 {
		s = newTestService(t, db, client, &fakeLocker{}, func() time.Time { return now })
		_, err := s.CreateAutoAlias(ctx, account.ID)
		var scoped interface{ CreationChannelScoped() bool }
		if !errors.As(err, &scoped) || !scoped.CreationChannelScoped() || !errors.Is(err, ErrRateLimited) {
			t.Fatalf("legacy completion throttle lost scope: %v", err)
		}
	}
	if calls != 1 {
		t.Fatal("legacy completion retried during cooldown")
	}
	for _, channel := range []string{"apple_account", "icloud_web"} {
		p, err := db.GetPendingAutoAliasConfirmationForChannel(ctx, account.ID, channel)
		if err != nil || p.ID != pending.ID || p.CreationChannel != "" {
			t.Fatal("legacy unknown result stopped gating a channel")
		}
	}
	now = now.Add(time.Hour)
	client.complete = func(_ context.Context, a apple.AccountSession, address, _, _ string) (apple.Alias, apple.AccountSession, error) {
		calls++
		return apple.Alias{HME: address, AnonymousID: "legacy-exact-id"}, a, nil
	}
	detailCalls := 0
	client.detail = func(_ context.Context, a apple.AccountSession, candidate apple.Alias) (apple.Alias, apple.AccountSession, error) {
		detailCalls++
		if candidate.AnonymousID != "legacy-exact-id" || candidate.HME != pending.Address {
			t.Fatal("resumed detail lost exact identity")
		}
		if detailCalls == 1 {
			return candidate, a, &apple.Error{Kind: apple.ErrService, StatusCode: 503}
		}
		candidate.IsActive, candidate.ForwardToEmail = true, account.Email
		return candidate, a, nil
	}
	if _, err := s.CreateAutoAlias(ctx, account.ID); !errors.Is(err, ErrAliasConfirmationPending) {
		t.Fatalf("unknown resumed detail: %v", err)
	}
	s = newTestService(t, db, client, &fakeLocker{}, func() time.Time { return now })
	if _, err := s.CreateAutoAlias(ctx, account.ID); err != nil || calls != 2 || detailCalls != 2 {
		t.Fatalf("resumed completion replayed: complete=%d detail=%d err=%v", calls, detailCalls, err)
	}
}

func TestSharedDirectoryThrottleTakesPrecedenceOverDetailThrottle(t *testing.T) {
	now := time.Now().UTC()
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	client := &detailPendingClient{}
	client.validate = func(_ context.Context, a apple.Session) (apple.Session, error) { return a, nil }
	lists := 0
	client.list = func(_ context.Context, a apple.Session) (apple.ListResult, apple.Session, error) {
		lists++
		if lists > 1 {
			return apple.ListResult{}, a, &apple.Error{Kind: apple.ErrService, StatusCode: 429, RetryAfter: 3 * time.Hour}
		}
		return apple.ListResult{SelectedForwardTo: "primary@icloud.com"}, a, nil
	}
	client.create = func(_ context.Context, a apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
		return apple.Alias{HME: "shared-pending@icloud.com", AnonymousID: "exact-id"}, a, nil
	}
	client.detail = func(_ context.Context, a apple.AccountSession, candidate apple.Alias) (apple.Alias, apple.AccountSession, error) {
		return candidate, a, &apple.Error{Kind: apple.ErrService, StatusCode: 429}
	}
	s := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, s, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal,
		Account: &apple.AccountSession{AppleID: "owner@example.com", APIKey: "fixture", SCNT: "fixture"}})
	_, err := s.CreateAutoAlias(context.Background(), 3)
	var scoped interface{ CreationChannelScoped() bool }
	if !apple.IsRateLimited(err) || errors.As(err, &scoped) || apple.RetryDelay(err) != 3*time.Hour || repo.pending == nil {
		t.Fatalf("shared directory cooldown masked by channel error: %v", err)
	}
}
