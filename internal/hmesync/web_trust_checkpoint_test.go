package hmesync

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

type webTrustCheckpointRepository struct {
	*fakeRepository
	t      *testing.T
	locker *fakeLocker
}

func (r *webTrustCheckpointRepository) UpsertAppleWebSession(ctx context.Context, record domain.AppleWebSession) (domain.AppleWebSession, error) {
	if r.locker.held.Load() == 0 {
		r.t.Fatal("trust checkpoint was written outside the account lock")
	}
	return r.fakeRepository.UpsertAppleWebSession(ctx, record)
}

func failedWebTrustFixture(t *testing.T) (*Service, *fakeRepository, *fakeAppleClient, *fakeLocker) {
	t.Helper()
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 7, Email: "owner@icloud.com", IMAPUsername: "owner@icloud.com"}, now)
	client, locker := &fakeAppleClient{}, &fakeLocker{}
	checked := &webTrustCheckpointRepository{fakeRepository: repo, t: t, locker: locker}
	return newTestService(t, checked, client, locker, func() time.Time { return now }), repo, client, locker
}

func failedWebTrustSession() apple.Session {
	return apple.Session{
		AppleID: "owner@icloud.com", Region: apple.RegionGlobal,
		ClientID: "stable-browser", TrustToken: "new-browser-trust",
		SessionToken: "partial-service-token", SCNT: "partial-scnt", SessionID: "partial-session-id",
		DSID: "partial-dsid", HSATrustedBrowser: true,
		Cookies: []apple.PersistentCookie{{Name: "partial", Value: "fixture", Domain: "icloud.com", Path: "/"}},
	}
}

func assertFailedWebTrustCheckpoint(t *testing.T, s *Service, repo *fakeRepository) {
	t.Helper()
	record := repo.mustSession(t, 7)
	if record.Authenticated || record.LastValidatedAt != nil {
		t.Fatal("partial authentication was published")
	}
	got, err := s.decryptSession(record)
	want := apple.Session{AppleID: "owner@icloud.com", Region: apple.RegionGlobal, ClientID: "stable-browser", TrustToken: "new-browser-trust"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("checkpoint retained service authentication fields: %v", err)
	}
	if _, _, err := s.loadSession(context.Background(), 7); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("checkpoint became a usable Web session: %v", err)
	}
}

func TestStartAuthFailureRetainsTrustForNextExplicitLogin(t *testing.T) {
	s, repo, client, locker := failedWebTrustFixture(t)
	upstream := errors.New("fixture accountLogin failure")
	calls := 0
	client.signIn = func(_ context.Context, _ string, _ string, _ apple.Region, previous *apple.Session) (apple.Session, bool, error) {
		assertNetworkOutsideAccountLock(t, locker)
		calls++
		if calls == 1 && previous != nil {
			t.Fatal("unexpected previous login")
		}
		if calls == 2 && (previous == nil || previous.TrustToken != "new-browser-trust" || previous.ClientID != "stable-browser") {
			t.Fatal("explicit login did not receive the trust checkpoint")
		}
		return failedWebTrustSession(), false, upstream
	}
	for i := 0; i < 2; i++ {
		if _, err := s.StartAuth(context.Background(), 1, 7, "owner@icloud.com", "fixture-password", apple.RegionGlobal); !errors.Is(err, upstream) || Code(err) != CodeUpstreamError {
			t.Fatalf("lost original login failure: %v", err)
		}
		if calls != i+1 {
			t.Fatal("login was retried automatically")
		}
		assertFailedWebTrustCheckpoint(t, s, repo)
	}
	if repo.upserts.Load() != 1 {
		t.Fatal("unchanged trust rewrote the checkpoint")
	}
}

func TestVerifyAuthFailureRetainsTrustWithoutReplayingCode(t *testing.T) {
	s, repo, client, locker := failedWebTrustFixture(t)
	upstream := errors.New("fixture accountLogin failure after trust")
	client.signIn = func(context.Context, string, string, apple.Region, *apple.Session) (apple.Session, bool, error) {
		// A token carried by an in-memory challenge is not a durable checkpoint.
		return apple.Session{AppleID: "owner@icloud.com", Region: apple.RegionGlobal, ClientID: "stable-browser", TrustToken: "new-browser-trust"}, true, nil
	}
	client.verify = func(context.Context, apple.Session, string) (apple.Session, error) {
		assertNetworkOutsideAccountLock(t, locker)
		return failedWebTrustSession(), upstream
	}
	started, err := s.StartAuth(context.Background(), 1, 7, "owner@icloud.com", "fixture-password", apple.RegionGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyAuth(context.Background(), 1, 7, started.ChallengeID, "123456"); !errors.Is(err, upstream) {
		t.Fatalf("lost original verification failure: %v", err)
	}
	assertFailedWebTrustCheckpoint(t, s, repo)
	if _, err := s.VerifyAuth(context.Background(), 1, 7, started.ChallengeID, "123456"); !errors.Is(err, ErrFlowExpired) || client.verifyCalls.Load() != 1 {
		t.Fatalf("completed verification was replayed: %v", err)
	}
}

func TestFailedWebTrustPreservesWorkingSession(t *testing.T) {
	s, repo, client, _ := failedWebTrustFixture(t)
	working := failedWebTrustSession()
	working.TrustToken, working.SessionToken, working.DSID = "old-trust", "working-service-token", "working-dsid"
	working.ValidatedAt = repo.now
	storeSessionExactly(t, s, repo, 7, working)
	before := repo.mustSession(t, 7)
	upstream := errors.New("fixture exchange failure")
	client.signIn = func(context.Context, string, string, apple.Region, *apple.Session) (apple.Session, bool, error) {
		return failedWebTrustSession(), false, upstream
	}
	if _, err := s.StartAuth(context.Background(), 1, 7, working.AppleID, "fixture-password", apple.RegionGlobal); !errors.Is(err, upstream) {
		t.Fatal(err)
	}
	after := repo.mustSession(t, 7)
	got, err := s.decryptSession(after)
	working.TrustToken = "new-browser-trust"
	if err != nil || !reflect.DeepEqual(got, working) || !after.Authenticated || !reflect.DeepEqual(after.LastValidatedAt, before.LastValidatedAt) {
		t.Fatalf("working session changed beyond browser trust: %v", err)
	}
}

func TestFailedWebTrustRejectsIdentityRegionAndBrowserMismatch(t *testing.T) {
	for _, scenario := range []string{"returned account", "returned region", "stored account", "stored region", "working browser", "account changed"} {
		t.Run(scenario, func(t *testing.T) {
			s, repo, client, locker := failedWebTrustFixture(t)
			failed := failedWebTrustSession()
			stored := failed
			stored.TrustToken = "old-trust"
			switch scenario {
			case "returned account":
				failed.AppleID = "other@icloud.com"
			case "returned region":
				failed.Region = apple.RegionChina
			case "stored account":
				stored.AppleID = "other@icloud.com"
			case "stored region":
				stored.Region = apple.RegionChina
			case "working browser":
				stored.ClientID = "another-browser"
			}
			storeSessionExactly(t, s, repo, 7, stored)
			before := repo.mustSession(t, 7)
			upstream := errors.New("fixture exchange failure")
			client.signIn = func(context.Context, string, string, apple.Region, *apple.Session) (apple.Session, bool, error) {
				assertNetworkOutsideAccountLock(t, locker)
				if scenario == "account changed" {
					repo.mu.Lock()
					repo.account.IMAPUsername = "changed@icloud.com"
					repo.mu.Unlock()
				}
				return failed, false, upstream
			}
			_, err := s.StartAuth(context.Background(), 1, 7, "owner@icloud.com", "fixture-password", apple.RegionGlobal)
			if !errors.Is(err, upstream) || !(errors.Is(err, ErrAccountMismatch) || errors.Is(err, ErrAccountChanged)) {
				t.Fatalf("mismatch or original failure was lost: %v", err)
			}
			if !reflect.DeepEqual(before, repo.mustSession(t, 7)) {
				t.Fatal("mismatched trust changed the stored session")
			}
		})
	}
}

func TestFailedWebTrustWriteFailureRemainsVisible(t *testing.T) {
	s, repo, client, _ := failedWebTrustFixture(t)
	upstream, writeErr := errors.New("fixture exchange failure"), errors.New("fixture checkpoint write failure")
	repo.upsertSessionErr = writeErr
	client.signIn = func(context.Context, string, string, apple.Region, *apple.Session) (apple.Session, bool, error) {
		return failedWebTrustSession(), false, upstream
	}
	_, err := s.StartAuth(context.Background(), 1, 7, "owner@icloud.com", "fixture-password", apple.RegionGlobal)
	if !errors.Is(err, upstream) || !errors.Is(err, writeErr) || !strings.Contains(err.Error(), "save Apple browser trust checkpoint") {
		t.Fatalf("checkpoint failure was hidden: %v", err)
	}
	if repo.sessionCount() != 0 {
		t.Fatal("failed checkpoint write published a session")
	}
}

func TestVerifyAuthTrustWriteFailureRetainsBothErrors(t *testing.T) {
	s, repo, client, _ := failedWebTrustFixture(t)
	upstream, writeErr := errors.New("fixture exchange failure"), errors.New("fixture checkpoint write failure")
	client.signIn = func(context.Context, string, string, apple.Region, *apple.Session) (apple.Session, bool, error) {
		return apple.Session{AppleID: "owner@icloud.com", Region: apple.RegionGlobal, ClientID: "stable-browser"}, true, nil
	}
	client.verify = func(context.Context, apple.Session, string) (apple.Session, error) {
		return failedWebTrustSession(), upstream
	}
	started, err := s.StartAuth(context.Background(), 1, 7, "owner@icloud.com", "fixture-password", apple.RegionGlobal)
	if err != nil {
		t.Fatal(err)
	}
	repo.upsertSessionErr = writeErr
	_, err = s.VerifyAuth(context.Background(), 1, 7, started.ChallengeID, "123456")
	if !errors.Is(err, upstream) || !errors.Is(err, writeErr) || client.verifyCalls.Load() != 1 {
		t.Fatalf("verification checkpoint lost a failure or retried: %v", err)
	}
}

func TestFailedWebTrustDoesNotSaveAbsentTrust(t *testing.T) {
	s, repo, client, _ := failedWebTrustFixture(t)
	upstream := errors.New("fixture failure before trust")
	client.signIn = func(context.Context, string, string, apple.Region, *apple.Session) (apple.Session, bool, error) {
		failed := failedWebTrustSession()
		failed.TrustToken = ""
		return failed, false, upstream
	}
	if _, err := s.StartAuth(context.Background(), 1, 7, "owner@icloud.com", "fixture-password", apple.RegionGlobal); !errors.Is(err, upstream) {
		t.Fatal(err)
	}
	if repo.upserts.Load() != 0 || repo.sessionCount() != 0 {
		t.Fatal("failure before browser trust created a checkpoint")
	}
}

func TestFailedWebTrustPreservesLegacyManagementCheckpoint(t *testing.T) {
	s, repo, client, _ := failedWebTrustFixture(t)
	stored := failedWebTrustSession()
	stored.TrustToken = "old-trust"
	stored.Account = &apple.AccountSession{AppleID: stored.AppleID, Region: stored.Region, APIKey: "legacy-management-key"}
	storeSessionExactly(t, s, repo, 7, stored)
	s.expireSession(context.Background(), 7)
	upstream := errors.New("fixture exchange failure")
	client.signIn = func(context.Context, string, string, apple.Region, *apple.Session) (apple.Session, bool, error) {
		failed := failedWebTrustSession()
		failed.Account = &apple.AccountSession{APIKey: "unaccepted-management-key"}
		return failed, false, upstream
	}
	if _, err := s.StartAuth(context.Background(), 1, 7, stored.AppleID, "fixture-password", apple.RegionGlobal); !errors.Is(err, upstream) {
		t.Fatal(err)
	}
	record := repo.mustSession(t, 7)
	retained, err := s.decryptSession(record)
	if err != nil || record.Authenticated || !reflect.DeepEqual(retained.Account, stored.Account) || retained.SessionToken != "" || len(retained.Cookies) != 0 {
		t.Fatalf("legacy management checkpoint was lost or replaced: %v", err)
	}
}

func TestFailedWebTrustPreservesIndependentManagementSession(t *testing.T) {
	s, db, account := independentAccountFixture(t, true)
	ctx := context.Background()
	managed := apple.AccountSession{AppleID: account.Email, Region: apple.RegionGlobal, APIKey: "independent-key", TrustToken: "independent-trust", AuthenticatedAt: s.now()}
	if err := s.saveAccountManagementSession(ctx, account.ID, managed); err != nil {
		t.Fatal(err)
	}
	before, err := db.GetAppleAccountSession(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	upstream := errors.New("fixture exchange failure")
	s.client = &fakeAppleClient{signIn: func(context.Context, string, string, apple.Region, *apple.Session) (apple.Session, bool, error) {
		failed := failedWebTrustSession()
		failed.AppleID = account.Email
		return failed, false, upstream
	}}
	if _, err := s.StartAuth(ctx, 1, account.ID, account.Email, "fixture-password", apple.RegionGlobal); !errors.Is(err, upstream) {
		t.Fatal(err)
	}
	after, err := db.GetAppleAccountSession(ctx, account.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("independent management session was modified: %v", err)
	}
	if retained := s.previousSession(ctx, account.ID, account.Email, apple.RegionGlobal); retained == nil || retained.TrustToken != "new-browser-trust" || retained.Account != nil {
		t.Fatal("Web checkpoint included management authentication")
	}
	if err := s.ClearAuth(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetAppleWebSession(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("explicit logout retained failed-login trust: %v", err)
	}
}
