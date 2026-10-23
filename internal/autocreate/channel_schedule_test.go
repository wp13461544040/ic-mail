package autocreate

import (
	"context"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

type channelWaitTestError struct{ error }

func (channelWaitTestError) CreationChannelScoped() bool { return true }
func (e channelWaitTestError) Unwrap() error             { return e.error }

func TestCreationRateLimitDefaultAndLegacyStatus(t *testing.T) {
	if appleRateLimitCooldown != time.Hour {
		t.Fatalf("default pause=%s", appleRateLimitCooldown)
	}
	for _, status := range []string{"APPLE_RATE_LIMITED", failureMessage(&apple.Error{Kind: apple.ErrService, StatusCode: 429}), "Apple 请求被限流，当前周期剩余计划槽已跳过，冷却后会继续执行"} {
		if !IsRateLimitStatus(status) {
			t.Fatalf("unrecognized stored throttle: %s", status)
		}
	}
}

func TestChannelScheduleDistributesNineteenAndFourAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	clock := newTestClock(time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	counts := map[string]int{}
	var webSlots []int
	attempts := 0
	creator := func(ctx context.Context, _ int64) (domain.Alias, error) {
		channel := domain.ScheduledCreationChannel(ctx)
		counts[channel]++
		if channel == "icloud_web" {
			webSlots = append(webSlots, attempts)
		}
		attempts++
		return domain.Alias{ID: 1, Address: "fixture@icloud.com"}, nil
	}
	newManager := func() *Manager {
		m, err := New(repo, creator, nil, WithClock(clock.Now), WithRandom(&testRandom{state: 3}))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := newManager()
	if _, err := m.SetEnabled(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	for range 23 {
		// A fresh manager must derive channel order from the durable plan.
		m = newManager()
		s, err := m.GetSchedule(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		clock.Set(*s.NextRunAt)
		m.runDue(ctx)
	}
	if counts["apple_account"] != 19 || counts["icloud_web"] != 4 {
		t.Fatalf("counts=%v", counts)
	}
	for i, want := range []int{5, 11, 17, 22} {
		if webSlots[i] != want {
			t.Fatalf("Web slot=%v", webSlots)
		}
	}
}

func TestChannelWaitPreservesNextSlotInsteadOfPausingAccount(t *testing.T) {
	ctx := context.Background()
	clock := newTestClock(time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	m, err := New(repo, func(context.Context, int64) (domain.Alias, error) {
		return domain.Alias{}, channelWaitTestError{&apple.Error{Kind: apple.ErrService, StatusCode: 429, RetryAfter: time.Hour}}
	}, nil, WithClock(clock.Now), WithRandom(&testRandom{state: 4}))
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.SetEnabled(ctx, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	want := s.PlannedAt[1]
	clock.Set(*s.NextRunAt)
	m.runDue(ctx)
	s, err = m.GetSchedule(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Enabled || s.NextRunAt == nil || !s.NextRunAt.Equal(want) {
		t.Fatalf("channel pause shifted whole account: %+v", s)
	}
}

func TestWebOnlyScheduleSurvivesRestartsAndRecoversDualChannels(t *testing.T) {
	ctx := context.Background()
	clock := newTestClock(time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	availability := domain.CreationChannelAvailability{ICloudWeb: true}
	counts := map[string]int{}
	newManager := func() *Manager {
		m, err := New(repo, func(ctx context.Context, _ int64) (domain.Alias, error) {
			counts[domain.ScheduledCreationChannel(ctx)]++
			if domain.WebOnlyCreation(ctx) == availability.AppleAccount {
				t.Fatal("incorrect degraded budget context")
			}
			return domain.Alias{Address: "fixture@icloud.com"}, nil
		}, nil, WithClock(clock.Now), WithRandom(&testRandom{state: 7}),
			WithChannelAvailability(func(context.Context, int64) (domain.CreationChannelAvailability, error) { return availability, nil }))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := newManager()
	if _, err := m.SetEnabled(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	runCycle := func() {
		for range CreationsPerCycle {
			m = newManager()
			s, err := m.GetSchedule(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			clock.Set(*s.NextRunAt)
			m.runDue(ctx)
			current, _ := m.GetSchedule(ctx, 1)
			slot := CreationsPerCycle - len(s.PlannedAt)
			if !availability.ICloudWeb || !availability.AppleAccount && !domain.WebOnlyCreationSlot(slot) {
				if (s.LastAttemptedAt == nil) != (current.LastAttemptedAt == nil) || s.LastAttemptedAt != nil && !s.LastAttemptedAt.Equal(*current.LastAttemptedAt) {
					t.Fatal("skipped slot recorded a remote attempt")
				}
			}
			if !current.Enabled || current.NextRunAt == nil {
				t.Fatal("opt-in lost")
			}
		}
	}
	runCycle()
	if counts["icloud_web"] != 5 || counts["apple_account"] != 0 {
		t.Fatalf("degraded counts=%v", counts)
	}
	availability.AppleAccount = true
	counts = map[string]int{}
	runCycle()
	if counts["icloud_web"] != 4 || counts["apple_account"] != 19 {
		t.Fatalf("recovered counts=%v", counts)
	}
	availability = domain.CreationChannelAvailability{}
	counts = map[string]int{}
	runCycle()
	if len(counts) != 0 {
		t.Fatalf("offline requests=%v", counts)
	}
	if _, err := m.SetEnabled(ctx, 1, false); err != nil {
		t.Fatal(err)
	}
	availability = domain.CreationChannelAvailability{AppleAccount: true, ICloudWeb: true}
	clock.Advance(2 * time.Hour)
	m.runDue(ctx)
	s, _ := m.GetSchedule(ctx, 1)
	if s.Enabled || len(counts) != 0 {
		t.Fatal("manual disable was reversed")
	}
}

func TestSessionExpiryKeepsOptInWithChannelPreflight(t *testing.T) {
	for _, code := range []string{"APPLE_LOGIN_REQUIRED", "APPLE_SESSION_EXPIRED", "APPLE_ACCOUNT_LOGIN_REQUIRED", "APPLE_ACCOUNT_SESSION_EXPIRED", "ACCOUNT_CHANGED", "APPLE_ACCOUNT_MISMATCH"} {
		t.Run(code, func(t *testing.T) {
			clock := newTestClock(time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
			repo := newFakeRepository()
			m, err := New(repo, func(context.Context, int64) (domain.Alias, error) {
				return domain.Alias{}, testDiagnosticError{code: code}
			}, nil, WithClock(clock.Now), WithRandom(&testRandom{state: 7}),
				WithChannelAvailability(func(context.Context, int64) (domain.CreationChannelAvailability, error) {
					return domain.CreationChannelAvailability{AppleAccount: true, ICloudWeb: true}, nil
				}))
			if err != nil {
				t.Fatal(err)
			}
			s := enableForTest(t, m, 1)
			clock.Set(*s.NextRunAt)
			m.runDue(context.Background())
			s, _ = m.GetSchedule(context.Background(), 1)
			wantEnabled := creationSessionError(testDiagnosticError{code: code})
			if s.Enabled != wantEnabled {
				t.Fatalf("enabled=%t want=%t", s.Enabled, wantEnabled)
			}
		})
	}
}
