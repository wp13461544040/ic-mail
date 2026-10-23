package hmesync

import (
	"context"
	"testing"
	"time"

	"icloud-api/internal/apple"
)

func TestAccountSessionHealthDistinguishesBackoffFromRejectedLogin(t *testing.T) {
	s, _, account := independentAccountFixture(t, true)
	ctx := context.Background()
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	for _, test := range []struct {
		name     string
		failures int
		rejected bool
		expired  bool
		status   string
		health   string
	}{
		{"healthy", 0, false, false, StatusAuthenticated, "healthy"},
		{"temporary-outage", 2, false, false, StatusAuthenticated, "retry_wait"},
		{"expired-during-backoff", 2, false, true, StatusExpired, "retry_wait"},
		{"explicit-rejection", 2, true, false, StatusExpired, "login_required"},
		{"expired-without-retry", 0, false, true, StatusExpired, "login_required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			managed := apple.AccountSession{
				AppleID: account.Email, Region: apple.RegionGlobal, APIKey: "fixture-key", SCNT: "fixture-scnt",
				AuthenticatedAt: now.Add(-time.Hour), RefreshedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute),
				RefreshFailures: test.failures, RefreshRejected: test.rejected,
			}
			if test.failures > 0 {
				managed.RefreshAfter = now.Add(4 * time.Minute)
			}
			if test.expired {
				managed.ExpiresAt = now.Add(-time.Second)
			}
			if err := s.saveAccountManagementSession(ctx, account.ID, managed); err != nil {
				t.Fatal(err)
			}
			info, err := s.GetAccountSession(ctx, account.ID)
			if err != nil || info.Status != test.status || info.Health != test.health {
				t.Fatalf("status=%s health=%s err=%v", info.Status, info.Health, err)
			}
			if info.LastRefreshedAt == nil || !info.LastRefreshedAt.Equal(managed.RefreshedAt) {
				t.Fatal("last successful renewal was lost")
			}
			if test.rejected {
				if info.NextRefreshAt != nil || info.NextRetryAt != nil {
					t.Fatal("rejected credentials advertised an automatic retry")
				}
			} else if test.failures > 0 && (info.NextRetryAt == nil || !info.NextRetryAt.Equal(managed.RefreshAfter)) {
				t.Fatal("persisted retry deadline was not exposed")
			}
		})
	}
}
