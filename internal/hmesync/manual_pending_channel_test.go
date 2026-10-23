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

func TestInteractivePendingUsesSelectedChannel(t *testing.T) {
	for _, tc := range []struct {
		name, requested, pendingChannel string
		probe, expired, wantCreate      bool
	}{
		{"manual auto after ACC expiry", "auto", "apple_account", true, true, true},
		{"batch auto after ACC expiry", "auto", "apple_account", false, true, true},
		{"explicit Web with live ACC", "icloud_web", "apple_account", true, false, true},
		{"explicit ACC with Web candidate", "apple_account", "icloud_web", true, false, true},
		{"same ACC candidate blocks generation", "auto", "apple_account", true, false, false},
		{"same Web candidate blocks generation", "auto", "icloud_web", true, true, false},
		{"legacy unknown candidate gates Web", "auto", "", true, true, false},
		{"legacy unknown candidate gates explicit Web", "icloud_web", "", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, db, account := independentAccountFixture(t, true)
			now := time.Now().UTC()
			s.now = func() time.Time { return now }
			managed := apple.AccountSession{AppleID: account.Email, APIKey: "fixture", SCNT: "fixture", ExpiresAt: now.Add(time.Hour)}
			if tc.expired {
				managed.RefreshRejected = true
				managed.ExpiresAt = now.Add(-time.Hour)
			}
			record, err := s.saveSession(ctx, account.ID, apple.Session{AppleID: account.Email, Region: apple.RegionGlobal,
				DSID: "fixture-dsid", Account: &managed})
			if err != nil {
				t.Fatal(err)
			}
			pending, _, err := db.CreateAliasWithPendingAPIKey(ctx, record, domain.Alias{AccountID: account.ID,
				Address: "unresolved-original@icloud.com", APIKeyHash: []byte("fixture"), APIKeyPrefix: "fixture",
				CreationChannel: tc.pendingChannel}, "encrypted-fixture")
			if err != nil {
				t.Fatal(err)
			}
			client := &pendingChannelClient{accountTestClient: s.client.(*accountTestClient)}
			var directory []apple.Alias
			creates := 0
			client.validate = func(_ context.Context, web apple.Session) (apple.Session, error) { return web, nil }
			client.list = func(_ context.Context, web apple.Session) (apple.ListResult, apple.Session, error) {
				return apple.ListResult{SelectedForwardTo: account.Email, Aliases: directory}, web, nil
			}
			created := apple.Alias{HME: "independent-channel@icloud.com", IsActive: true, ForwardToEmail: account.Email}
			client.create = func(_ context.Context, web apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
				if tc.requested == "apple_account" {
					t.Fatal("explicit ACC request used Web")
				}
				creates++
				directory = append(directory, created)
				return created, web, nil
			}
			client.accountCreate = func(_ context.Context, acc apple.AccountSession) (apple.Alias, apple.AccountSession, error) {
				if tc.requested != "apple_account" {
					t.Fatal("expired or pending ACC generated another candidate")
				}
				creates++
				directory = append(directory, created)
				return created, acc, nil
			}
			client.complete = func(_ context.Context, acc apple.AccountSession, address, _, _ string) (apple.Alias, apple.AccountSession, error) {
				if address != pending.Address || tc.wantCreate || tc.expired {
					t.Fatal("completed an unrelated or expired candidate")
				}
				return apple.Alias{}, acc, &apple.Error{Kind: apple.ErrService, StatusCode: 503}
			}
			s = newTestService(t, db, client, &fakeLocker{}, func() time.Time { return now })
			s.autoCreateConfirmationDelays = nil
			var got domain.Alias
			if tc.probe {
				got, err = s.ProbeAliasWithChannel(ctx, account.ID, tc.requested)
			} else {
				got, err = s.CreateAliasWithChannel(ctx, account.ID, tc.requested)
			}
			if tc.wantCreate {
				if err != nil || creates != 1 || !got.Enabled || got.Address != created.HME {
					t.Fatalf("independent channel failed: created=%d enabled=%t err=%v", creates, got.Enabled, err)
				}
			} else if !errors.Is(err, ErrAliasConfirmationPending) || creates != 0 {
				t.Fatalf("pending result was replayed: creates=%d err=%v", creates, err)
			}
			retained, err := db.GetPendingAutoAliasConfirmation(ctx, account.ID)
			if err != nil || retained.ID != pending.ID || retained.CreationChannel != tc.pendingChannel {
				t.Fatalf("original pending identity changed: %v", err)
			}
		})
	}
}

type fullCapacityPendingRepository struct{ *store.Store }

func (r fullCapacityPendingRepository) CountEnabledAliasesByAccount(context.Context, int64) (int, error) {
	return domain.MaxEnabledAliasesPerAccount, nil
}

func (r fullCapacityPendingRepository) CountAliasCreationOccupancy(context.Context, int64) (int, error) {
	return domain.MaxEnabledAliasesPerAccount, nil
}

func TestIndependentPendingDoesNotBypassCapacity(t *testing.T) {
	ctx := context.Background()
	s, db, account := independentAccountFixture(t, true)
	record, err := s.saveSession(ctx, account.ID, apple.Session{AppleID: account.Email, Region: apple.RegionGlobal, DSID: "fixture-dsid"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = db.CreateAliasWithPendingAPIKey(ctx, record, domain.Alias{AccountID: account.ID,
		Address: "acc-pending@icloud.com", APIKeyHash: []byte("fixture"), APIKeyPrefix: "fixture",
		CreationChannel: "apple_account"}, "encrypted-fixture")
	if err != nil {
		t.Fatal(err)
	}
	s.repo = fullCapacityPendingRepository{db}
	s.client.(*accountTestClient).validate = func(context.Context, apple.Session) (apple.Session, error) {
		t.Fatal("capacity rejection must precede Apple requests")
		return apple.Session{}, nil
	}
	if _, err := s.ProbeAliasWithChannel(ctx, account.ID, "auto"); !errors.Is(err, store.ErrAliasLimit) {
		t.Fatalf("unrelated candidate bypassed capacity: %v", err)
	}
}

func TestWebOnlyScheduleWithExpiredACCCandidate(t *testing.T) {
	ctx := context.Background()
	s, db, account := independentAccountFixture(t, true)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	managed := apple.AccountSession{AppleID: account.Email, APIKey: "fixture", RefreshRejected: true, ExpiresAt: now.Add(-time.Hour)}
	record, err := s.saveSession(ctx, account.ID, apple.Session{AppleID: account.Email, Region: apple.RegionGlobal,
		DSID: "fixture-dsid", Account: &managed})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = db.CreateAliasWithPendingAPIKey(ctx, record, domain.Alias{AccountID: account.ID,
		Address: "expired-acc-candidate@icloud.com", APIKeyHash: []byte("fixture"), APIKeyPrefix: "fixture",
		CreationChannel: "apple_account"}, "encrypted-fixture")
	if err != nil {
		t.Fatal(err)
	}
	client := s.client.(*accountTestClient)
	client.validate = func(_ context.Context, web apple.Session) (apple.Session, error) { return web, nil }
	var directory []apple.Alias
	client.list = func(_ context.Context, web apple.Session) (apple.ListResult, apple.Session, error) {
		return apple.ListResult{SelectedForwardTo: account.Email, Aliases: directory}, web, nil
	}
	client.accountCreate = func(context.Context, apple.AccountSession) (apple.Alias, apple.AccountSession, error) {
		t.Fatal("expired ACC was used by Web-only schedule")
		return apple.Alias{}, managed, nil
	}
	client.create = func(_ context.Context, web apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
		a := apple.Alias{HME: fmt.Sprintf("web-only-%d@icloud.com", len(directory)), IsActive: true, ForwardToEmail: account.Email}
		directory = append(directory, a)
		return a, web, nil
	}
	manager, err := autocreate.New(db, s.CreateAutoAlias, slog.New(slog.NewTextHandler(io.Discard, nil)),
		autocreate.WithClock(s.now), autocreate.WithRandomFunc(func(int) int { return 0 }),
		autocreate.WithWait(func(context.Context, time.Duration) bool { return false }),
		autocreate.WithChannelAvailability(s.CreationChannelAvailability))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetEnabled(ctx, account.ID, true); err != nil {
		t.Fatal(err)
	}
	for range domain.AppleCreationHourlyLimit {
		plan, err := manager.GetSchedule(ctx, account.ID)
		if err != nil || !plan.Enabled || plan.NextRunAt == nil {
			t.Fatalf("Web-only plan stopped: %v", err)
		}
		now = *plan.NextRunAt
		manager.Run(ctx)
	}
	if len(directory) != domain.ICloudWebOnlyCreationHourlyLimit {
		t.Fatalf("Web-only creations=%d, want 5", len(directory))
	}
}
