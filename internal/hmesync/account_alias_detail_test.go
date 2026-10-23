package hmesync

import (
	"context"
	"errors"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

type detailPendingClient struct {
	resumingPendingAppleClient
	detail func(context.Context, apple.AccountSession, apple.Alias) (apple.Alias, apple.AccountSession, error)
}

func (c *detailPendingClient) GetAccountAlias(ctx context.Context, s apple.AccountSession, a apple.Alias) (apple.Alias, apple.AccountSession, error) {
	return c.detail(ctx, s, a)
}

func (c *detailPendingClient) SignInAccount(context.Context, string, string, apple.Region, *apple.AccountSession) (apple.AccountSession, bool, error) {
	panic("unexpected sign in")
}

func (c *detailPendingClient) VerifyAccountCode(context.Context, apple.AccountSession, string) (apple.AccountSession, error) {
	panic("unexpected verification")
}

func (c *detailPendingClient) CreateAccountAlias(ctx context.Context, s apple.AccountSession, label, note string) (apple.Alias, apple.AccountSession, error) {
	a, _, err := c.create(ctx, apple.Session{Account: &s}, label, note)
	return a, s, err
}

func TestCreationConfirmsAccountDetailWhenListsOmitAddress(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pending bool
		age     time.Duration
		forward string
		readErr error
	}{
		{"new", false, 0, "primary@icloud.com", nil},
		{"inactive_completion_active_detail", false, 0, "primary@icloud.com", nil},
		{"recent_pending", true, time.Minute, "primary@icloud.com", nil},
		{"old_pending", true, time.Hour, "primary@icloud.com", nil},
		{"other_forward", true, time.Minute, "other@icloud.com", nil},
		{"unknown_old_detail", true, time.Hour, "", &apple.Error{Kind: apple.ErrService, StatusCode: 503}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC)
			repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
			if tc.pending {
				repo.pending = &domain.Alias{ID: 77, AccountID: 3, Address: "root@icloud.com", Label: "test", CreatedAt: now.Add(-tc.age), LastSyncError: domain.AppleAliasConfirmationPending}
			}
			lists, details, completes := 0, 0, 0
			candidate := apple.Alias{HME: "root@icloud.com", AnonymousID: "remote-id", IsActive: true, Origin: "APPLE_ACCOUNT"}
			if tc.name == "inactive_completion_active_detail" {
				candidate.IsActive = false
			}
			client := &detailPendingClient{}
			client.validate = func(_ context.Context, s apple.Session) (apple.Session, error) { return s, nil }
			client.list = func(_ context.Context, s apple.Session) (apple.ListResult, apple.Session, error) {
				lists++
				return apple.ListResult{SelectedForwardTo: "primary@icloud.com"}, s, nil
			}
			client.create = func(_ context.Context, s apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
				return candidate, s, nil
			}
			client.complete = func(_ context.Context, s apple.AccountSession, _, _, _ string) (apple.Alias, apple.AccountSession, error) {
				completes++
				return candidate, s, nil
			}
			client.detail = func(_ context.Context, s apple.AccountSession, a apple.Alias) (apple.Alias, apple.AccountSession, error) {
				details++
				s.SCNT = "rotated"
				a.ForwardToEmail = tc.forward
				a.IsActive = true
				return a, s, tc.readErr
			}
			service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
			service.autoCreateConfirmationDelays = nil
			storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal, Account: &apple.AccountSession{AppleID: "owner@example.com", APIKey: "key", SCNT: "scnt"}})
			created, err := service.CreateAutoAlias(context.Background(), 3)
			if details != 1 {
				t.Fatalf("detail calls=%d err=%v creates=%d", details, err, client.createCalls.Load())
			}
			if tc.readErr != nil {
				if !errors.Is(err, ErrAliasConfirmationPending) || repo.pending == nil || repo.creates.Load() != 0 {
					t.Fatalf("unknown completion lost pending or reserved again: %v", err)
				}
			} else if tc.forward != "primary@icloud.com" {
				if !errors.Is(err, ErrAccountMismatch) || repo.confirms.Load() != 0 {
					t.Fatalf("wrong forwarding accepted: %v", err)
				}
			} else if err != nil || !created.Enabled || repo.confirms.Load() != 1 || lists != 1 {
				t.Fatalf("created=%#v lists=%d err=%v", created, lists, err)
			}
			if tc.pending && (client.createCalls.Load() != 0 || completes != 1) {
				t.Fatal("pending recovery reserved another address")
			}
			saved, err := service.decryptSession(repo.mustSession(t, 3))
			if err != nil || saved.Account.SCNT != "rotated" {
				t.Fatal("detail session checkpoint lost")
			}
		})
	}
}

func TestOldPendingUnknownCompletionNeverReservesReplacement(t *testing.T) {
	now := time.Now().UTC()
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	repo.pending = &domain.Alias{ID: 77, AccountID: 3, Address: "root@icloud.com", CreatedAt: now.Add(-time.Hour), LastSyncError: domain.AppleAliasConfirmationPending}
	client := &resumingPendingAppleClient{}
	client.validate = func(_ context.Context, s apple.Session) (apple.Session, error) { return s, nil }
	client.list = func(_ context.Context, s apple.Session) (apple.ListResult, apple.Session, error) {
		return apple.ListResult{}, s, nil
	}
	client.complete = func(_ context.Context, s apple.AccountSession, _, _, _ string) (apple.Alias, apple.AccountSession, error) {
		return apple.Alias{HME: "root@icloud.com"}, s, &apple.Error{Kind: apple.ErrService, StatusCode: 503}
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal, Account: &apple.AccountSession{AppleID: "owner@example.com", APIKey: "key", SCNT: "scnt"}})
	_, err := service.CreateAutoAlias(context.Background(), 3)
	if !errors.Is(err, ErrAliasConfirmationPending) || repo.pending == nil || repo.creates.Load() != 0 || client.createCalls.Load() != 0 {
		t.Fatalf("unknown completion lost original candidate: %v", err)
	}
	var upstream *apple.Error
	if !errors.As(err, &upstream) || upstream.StatusCode != 503 {
		t.Fatal("completion diagnostic lost")
	}
}
