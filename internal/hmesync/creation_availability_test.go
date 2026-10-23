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
)

func TestCreationAvailabilityAndRoutingUseIndependentSessionState(t *testing.T) {
	s, db, account := independentAccountFixture(t, true)
	ctx := context.Background()
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	managed := apple.AccountSession{AppleID: "login@example.com", Region: apple.RegionGlobal, APIKey: "fixture", AuthenticatedAt: now, ExpiresAt: now.Add(time.Hour), RefreshRejected: true, UpdatedAt: now}
	web := apple.Session{AppleID: managed.AppleID, Region: apple.RegionGlobal, DSID: "fixture-dsid", SessionToken: "fixture-web", Account: &managed}
	if _, err := s.saveSession(ctx, account.ID, web); err != nil {
		t.Fatal(err)
	}
	if err := s.saveAccountManagementSession(ctx, account.ID, managed); err != nil {
		t.Fatal(err)
	}
	status, err := s.CreationChannelAvailability(ctx, account.ID)
	if err != nil || status.AppleAccount || !status.ICloudWeb {
		t.Fatalf("degrade status=%v err=%v", status, err)
	}
	client := s.client.(*accountTestClient)
	webCalls := 0
	client.accountCreate = func(context.Context, apple.AccountSession) (apple.Alias, apple.AccountSession, error) {
		t.Fatal("rejected ACC reached Apple")
		return apple.Alias{}, apple.AccountSession{}, nil
	}
	client.create = func(ctx context.Context, current apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
		if !domain.WebOnlyCreation(ctx) {
			t.Fatal("Web-only budget missing")
		}
		webCalls++
		return apple.Alias{HME: "fixture@icloud.com"}, current, nil
	}
	for i := range 5 {
		now = now.Add(12 * time.Minute)
		_, current, err := s.loadSession(ctx, account.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.createRemoteAliasWithChannel(ctx, account.ID, current, client, "auto"); err != nil {
			t.Fatalf("Web attempt %d: %v", i, err)
		}
	}
	if webCalls != 5 {
		t.Fatalf("Web calls=%d", webCalls)
	}
	// A new durable login replaces the rejected checkpoint without changing Web.
	managed.RefreshRejected = false
	managed.UpdatedAt = now.Add(time.Second)
	managed.ExpiresAt = now.Add(time.Hour)
	if err := s.saveAccountManagementSession(ctx, account.ID, managed); err != nil {
		t.Fatal(err)
	}
	status, err = s.CreationChannelAvailability(ctx, account.ID)
	if err != nil || !status.AppleAccount || !status.ICloudWeb {
		t.Fatalf("relogin status=%v err=%v", status, err)
	}
	if err := s.ClearAuth(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	status, err = s.CreationChannelAvailability(ctx, account.ID)
	if err != nil || status.ICloudWeb {
		t.Fatalf("Web logout status=%v err=%v", status, err)
	}
	if _, err := db.GetAccount(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCreationUsabilityRejectsExpiredButAllowsTransientRefreshFailure(t *testing.T) {
	now := time.Now()
	s := apple.AccountSession{APIKey: "fixture", ExpiresAt: now.Add(time.Hour), RefreshFailures: 2, RefreshAfter: now.Add(time.Minute)}
	if !accountCreationSessionUsable(&s, now) {
		t.Fatal("transient refresh failure logged out ACC")
	}
	s.ExpiresAt = now
	if accountCreationSessionUsable(&s, now) {
		t.Fatal("expired checkpoint selected")
	}
}

func TestAutoCreationDegradeAndReloginEndToEndOffline(t *testing.T) {
	s, db, account := independentAccountFixture(t, true)
	ctx := context.Background()
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	managed := apple.AccountSession{AppleID: account.Email, Region: apple.RegionGlobal, APIKey: "fixture", AuthenticatedAt: now, ExpiresAt: now.Add(8 * time.Hour), UpdatedAt: now, RefreshRejected: true}
	web := apple.Session{AppleID: account.Email, Region: apple.RegionGlobal, DSID: "fixture-dsid", SessionToken: "fixture-web", Account: &managed}
	if _, err := s.saveSession(ctx, account.ID, web); err != nil {
		t.Fatal(err)
	}
	if err := s.saveAccountManagementSession(ctx, account.ID, managed); err != nil {
		t.Fatal(err)
	}
	client := s.client.(*accountTestClient)
	var directory []apple.Alias
	counts := map[string]int{}
	create := func(channel string) apple.Alias {
		counts[channel]++
		a := apple.Alias{HME: fmt.Sprintf("offline-%d@icloud.com", len(directory)+1), IsActive: true, ForwardToEmail: account.Email}
		directory = append(directory, a)
		return a
	}
	client.validate = func(_ context.Context, web apple.Session) (apple.Session, error) { return web, nil }
	client.list = func(_ context.Context, web apple.Session) (apple.ListResult, apple.Session, error) {
		return apple.ListResult{SelectedForwardTo: account.Email, Aliases: directory}, web, nil
	}
	client.create = func(_ context.Context, web apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
		return create("icloud_web"), web, nil
	}
	client.accountCreate = func(_ context.Context, managed apple.AccountSession) (apple.Alias, apple.AccountSession, error) {
		if managed.RefreshRejected {
			t.Fatal("rejected ACC used")
		}
		return create("apple_account"), managed, nil
	}
	newManager := func() *autocreate.Manager {
		m, err := autocreate.New(db, s.CreateAutoAlias, slog.New(slog.NewTextHandler(io.Discard, nil)),
			autocreate.WithClock(s.now), autocreate.WithRandomFunc(func(int) int { return 0 }),
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
	runCycle := func() {
		for range domain.AppleCreationHourlyLimit {
			m = newManager()
			schedule, err := m.GetSchedule(ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			now = *schedule.NextRunAt
			m.Run(ctx)
			schedule, err = m.GetSchedule(ctx, account.ID)
			if err != nil || !schedule.Enabled || schedule.LastError != "" {
				t.Fatalf("creation schedule failed: err=%v enabled=%t diagnostic=%s", err, schedule.Enabled, schedule.LastError)
			}
		}
	}
	runCycle()
	if counts["icloud_web"] != 5 || counts["apple_account"] != 0 {
		t.Fatalf("offline degraded creates=%v", counts)
	}
	aliases, err := db.CountEnabledAliasesByAccount(ctx, account.ID)
	if err != nil || aliases != 5 {
		t.Fatalf("confirmed local aliases=%d err=%v", aliases, err)
	}
	managed.RefreshRejected = false
	managed.UpdatedAt = now.Add(time.Second)
	if err := s.saveAccountManagementSession(ctx, account.ID, managed); err != nil {
		t.Fatal(err)
	}
	counts = map[string]int{}
	runCycle()
	if counts["icloud_web"] != 4 || counts["apple_account"] != 19 {
		t.Fatalf("offline recovered creates=%v", counts)
	}
	aliases, err = db.CountEnabledAliasesByAccount(ctx, account.ID)
	if err != nil || aliases != 28 {
		t.Fatalf("confirmed local aliases=%d err=%v", aliases, err)
	}
}

func TestRejectedAccountCreationPersistsExpiryWithoutLoggingOutWeb(t *testing.T) {
	s, _, account := independentAccountFixture(t, true)
	ctx := context.Background()
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	managed := apple.AccountSession{AppleID: account.Email, Region: apple.RegionGlobal, APIKey: "fixture", AuthenticatedAt: now, ExpiresAt: now.Add(time.Hour)}
	web := apple.Session{AppleID: account.Email, Region: apple.RegionGlobal, DSID: "fixture-dsid", SessionToken: "fixture-web", Account: &managed}
	if _, err := s.saveSession(ctx, account.ID, web); err != nil {
		t.Fatal(err)
	}
	if err := s.saveAccountManagementSession(ctx, account.ID, managed); err != nil {
		t.Fatal(err)
	}
	client := s.client.(*accountTestClient)
	client.accountCreate = func(_ context.Context, managed apple.AccountSession) (apple.Alias, apple.AccountSession, error) {
		return apple.Alias{}, managed, apple.ErrInvalidSession
	}
	client.create = func(context.Context, apple.Session, string, string) (apple.Alias, apple.Session, error) {
		t.Fatal("invalid ACC result replayed in same call")
		return apple.Alias{}, apple.Session{}, nil
	}
	_, _, err := s.createRemoteAliasWithChannel(ctx, account.ID, web, client, "auto")
	if Code(err) != CodeAccountSessionExpired || !errors.Is(err, ErrUpstream) {
		t.Fatalf("expiry diagnostic=%v", err)
	}
	status, err := s.CreationChannelAvailability(ctx, account.ID)
	if err != nil || status.AppleAccount || !status.ICloudWeb {
		t.Fatalf("expiry not isolated: %v %v", status, err)
	}
}
