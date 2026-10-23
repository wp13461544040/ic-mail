package hmesync

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

type directoryTestClient struct {
	*fakeAppleClient
	read func(context.Context, apple.AccountSession) (apple.ListResult, apple.AccountSession, error)
}

func (c *directoryTestClient) ListAccountAliases(ctx context.Context, session apple.AccountSession) (apple.ListResult, apple.AccountSession, error) {
	return c.read(ctx, session)
}

func directoryServiceFixture(t *testing.T) (*Service, *store.Store, domain.Account, *fakeLocker, apple.AccountSession) {
	t.Helper()
	s, db, account := independentAccountFixture(t, true)
	credentialCipher, ok := s.cipher.(*secure.Cipher)
	if !ok {
		t.Fatal("fixture must use the production credential cipher")
	}
	db.ConfigureAliasCredentialFactory(func(id, version int64) (domain.AliasCredentialMaterial, error) {
		_, material, err := secure.NewAliasCredentialMaterial(credentialCipher, id, version)
		return material, err
	})
	db.ConfigureAliasCredentialRevealFactory(func(id int64, ciphertext string) (string, error) {
		credentials, err := credentialCipher.DecryptAliasCredentials(id, ciphertext)
		return credentials.APIKey, err
	})
	locker := &fakeLocker{}
	s.locker = locker
	managed := apple.AccountSession{AppleID: "login@example.com", Region: apple.RegionGlobal,
		SCNT: "fixture-scnt", APIKey: "fixture-key", AuthenticatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.persistAccountManagementSession(context.Background(), account.ID, identityOf(account), managed); err != nil {
		t.Fatal(err)
	}
	return s, db, account, locker, managed
}

func saveDirectoryWeb(t *testing.T, s *Service, account domain.Account) {
	t.Helper()
	_, err := s.saveSession(context.Background(), account.ID, apple.Session{
		AppleID: "login@example.com", Region: apple.RegionGlobal, DSID: "fixture-dsid", TrustToken: "fixture-trust",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func ownedDirectory(account domain.Account) apple.ListResult {
	return apple.ListResult{SelectedForwardTo: account.Email, ForwardToEmails: []string{account.Email}, Aliases: []apple.Alias{
		{AnonymousID: "opaque-account-id.em", HME: "active@icloud.com", ForwardToEmail: account.Email, IsActive: true, Origin: "APPLE_ACCOUNT"},
		{AnonymousID: "inactive-account-id", HME: "off@icloud.com", ForwardToEmail: account.Email, IsActive: false, Origin: "APPLE_ACCOUNT"},
	}}
}

func TestSyncAccountDirectoryFallbackPreservesRealWebStatus(t *testing.T) {
	for _, trigger := range []string{"list authentication", "missing web", "expired cookie", "expired record", "validate rejection", "validate misdirected"} {
		t.Run(trigger, func(t *testing.T) {
			ctx := context.Background()
			s, db, account, locker, _ := directoryServiceFixture(t)
			if trigger != "missing web" {
				saveDirectoryWeb(t, s, account)
			}
			var webCheckpoint string
			if trigger == "validate misdirected" {
				record, err := db.GetAppleWebSession(ctx, account.ID)
				if err != nil {
					t.Fatal(err)
				}
				webCheckpoint = record.Ciphertext
			}
			expectedStatus := StatusAuthenticated
			switch trigger {
			case "missing web":
				expectedStatus = StatusLoginRequired
			case "expired cookie":
				_, web, err := s.loadSession(ctx, account.ID)
				if err != nil {
					t.Fatal(err)
				}
				web.Cookies = []apple.PersistentCookie{{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "fixture", Domain: ".icloud.com", Path: "/", Expires: time.Now().Add(-time.Minute)}}
				if _, err := s.saveSession(ctx, account.ID, web); err != nil {
					t.Fatal(err)
				}
			case "expired record":
				record, err := db.GetAppleWebSession(ctx, account.ID)
				if err != nil {
					t.Fatal(err)
				}
				record.Authenticated = false
				if _, err := db.UpsertAppleWebSession(ctx, record); err != nil {
					t.Fatal(err)
				}
				expectedStatus = StatusExpired
			case "validate rejection":
				expectedStatus = StatusExpired
			}
			validates, webReads, accountReads := 0, 0, 0
			s.client = &directoryTestClient{fakeAppleClient: &fakeAppleClient{
				validate: func(_ context.Context, web apple.Session) (apple.Session, error) {
					assertNetworkOutsideAccountLock(t, locker)
					validates++
					if trigger == "validate rejection" {
						return web, &apple.Error{Kind: apple.ErrInvalidSession, StatusCode: http.StatusUnauthorized}
					}
					if trigger == "validate misdirected" {
						return web, &apple.Error{Kind: apple.ErrService, StatusCode: http.StatusMisdirectedRequest}
					}
					if trigger == "expired cookie" {
						web.Cookies = nil
					}
					return web, nil
				},
				list: func(_ context.Context, web apple.Session) (apple.ListResult, apple.Session, error) {
					assertNetworkOutsideAccountLock(t, locker)
					webReads++
					return apple.ListResult{}, web, &apple.Error{Kind: apple.ErrHMEAuthentication, StatusCode: http.StatusForbidden}
				},
			}, read: func(_ context.Context, managed apple.AccountSession) (apple.ListResult, apple.AccountSession, error) {
				assertNetworkOutsideAccountLock(t, locker)
				accountReads++
				if managed.AppleID != "login@example.com" || managed.AppleID == account.Email {
					t.Fatal("Account login identity was replaced with the forwarding mailbox")
				}
				managed.SCNT = "rotated-fixture"
				managed.UpdatedAt = time.Now().UTC()
				return ownedDirectory(account), managed, nil
			}}
			result, err := s.SyncAliases(ctx, account.ID)
			if err != nil || accountReads != 1 || result.Summary.Total != 2 || result.Summary.CreatedCount != 2 || result.Summary.InactiveCount != 1 {
				t.Fatalf("fallback: account reads=%d total=%d created=%d err=%v", accountReads, result.Summary.Total, result.Summary.CreatedCount, err)
			}
			if result.Session.Status != expectedStatus {
				t.Fatalf("Web status=%q want=%q", result.Session.Status, expectedStatus)
			}
			if trigger == "validate misdirected" {
				record, err := db.GetAppleWebSession(ctx, account.ID)
				if err != nil || record.Ciphertext != webCheckpoint {
					t.Fatal("421 fallback changed the retained Web checkpoint")
				}
			}
			if trigger == "missing web" || trigger == "expired record" {
				if validates != 0 || webReads != 0 {
					t.Fatal("missing or expired Web state reached the Web network client")
				}
			} else if validates != 1 || ((trigger == "validate rejection" || trigger == "validate misdirected") && webReads != 0) || (trigger != "validate rejection" && trigger != "validate misdirected" && webReads != 1) {
				t.Fatal("unexpected Web validation and directory request order")
			}
			if trigger == "missing web" {
				if _, err := db.GetAppleWebSession(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatal("Account directory read fabricated a Web session")
				}
			}
			managed, err := s.readAccountManagementSession(ctx, account.ID)
			if err != nil || managed.SCNT != "rotated-fixture" {
				t.Fatal("Account directory session rotation was not independently saved")
			}
		})
	}
}

func TestSyncAccountDirectoryKeepsWebPriority(t *testing.T) {
	s, _, account, locker, _ := directoryServiceFixture(t)
	saveDirectoryWeb(t, s, account)
	s.client = &directoryTestClient{fakeAppleClient: &fakeAppleClient{
		validate: func(_ context.Context, web apple.Session) (apple.Session, error) {
			assertNetworkOutsideAccountLock(t, locker)
			return web, nil
		},
		list: func(_ context.Context, web apple.Session) (apple.ListResult, apple.Session, error) {
			assertNetworkOutsideAccountLock(t, locker)
			return ownedDirectory(account), web, nil
		},
	}, read: func(context.Context, apple.AccountSession) (apple.ListResult, apple.AccountSession, error) {
		t.Fatal("working Web directory reached Account fallback")
		return apple.ListResult{}, apple.AccountSession{}, nil
	}}
	result, err := s.SyncAliases(context.Background(), account.ID)
	if err != nil || result.Session.Status != StatusAuthenticated || result.Summary.Total != 2 {
		t.Fatalf("Web-first sync: err=%v", err)
	}
}

func TestSyncAccountDirectoryDoesNotFallbackForOtherWebErrors(t *testing.T) {
	errorsToCheck := []struct {
		name string
		err  error
	}{
		{"throttle", &apple.Error{Kind: apple.ErrService, StatusCode: 429, RetryAfter: time.Minute}},
		{"misdirected throttle", &apple.Error{Kind: apple.ErrService, StatusCode: 421, ServiceCode: "-41015"}},
		{"misdirected cancellation", &apple.Error{Kind: apple.ErrService, StatusCode: 421, Err: context.Canceled}},
		{"outage", &apple.Error{Kind: apple.ErrService, StatusCode: 503}},
		{"schema", &apple.Error{Kind: apple.ErrInvalidResponse, StatusCode: 200}},
		{"unavailable", apple.ErrHMEUnavailable},
		{"authentication with throttle", &apple.Error{Kind: apple.ErrHMEAuthentication, StatusCode: 429}},
		{"authentication with outage", &apple.Error{Kind: apple.ErrHMEAuthentication, StatusCode: 503}},
		{"session with outage", &apple.Error{Kind: apple.ErrInvalidSession, StatusCode: 503}},
	}
	for _, stage := range []string{"validate", "list"} {
		for _, test := range errorsToCheck {
			t.Run(stage+"/"+test.name, func(t *testing.T) {
				s, db, account, _, _ := directoryServiceFixture(t)
				saveDirectoryWeb(t, s, account)
				s.client = &directoryTestClient{fakeAppleClient: &fakeAppleClient{
					validate: func(_ context.Context, web apple.Session) (apple.Session, error) {
						if stage == "validate" {
							return web, test.err
						}
						return web, nil
					},
					list: func(_ context.Context, web apple.Session) (apple.ListResult, apple.Session, error) {
						return apple.ListResult{}, web, test.err
					},
				}, read: func(context.Context, apple.AccountSession) (apple.ListResult, apple.AccountSession, error) {
					t.Fatal("non-authentication Web failure reached Account fallback")
					return apple.ListResult{}, apple.AccountSession{}, nil
				}}
				if _, err := s.SyncAliases(context.Background(), account.ID); !errors.Is(err, test.err) {
					t.Fatalf("Web failure classification lost: %v", err)
				}
				aliases, err := db.ListAliasesByAccount(context.Background(), account.ID)
				if err != nil || len(aliases) != 0 {
					t.Fatal("failed Web directory imported aliases")
				}
			})
		}
	}
}

func TestSyncAccountDirectoryPreservesOriginalErrorWithoutCapabilityOrSession(t *testing.T) {
	for _, missing := range []string{"capability", "session", "both sessions"} {
		t.Run(missing, func(t *testing.T) {
			s, db, account, _, _ := directoryServiceFixture(t)
			web := &fakeAppleClient{
				validate: func(_ context.Context, web apple.Session) (apple.Session, error) { return web, nil },
				list: func(_ context.Context, web apple.Session) (apple.ListResult, apple.Session, error) {
					return apple.ListResult{}, web, apple.ErrHMEAuthentication
				},
			}
			s.client = web
			if missing != "both sessions" {
				saveDirectoryWeb(t, s, account)
			}
			if missing != "capability" {
				if err := db.DeleteAppleAccountSession(context.Background(), account.ID); err != nil {
					t.Fatal(err)
				}
				s.client = &directoryTestClient{fakeAppleClient: web, read: func(context.Context, apple.AccountSession) (apple.ListResult, apple.AccountSession, error) {
					t.Fatal("missing Account session reached directory request")
					return apple.ListResult{}, apple.AccountSession{}, nil
				}}
			}
			_, err := s.SyncAliases(context.Background(), account.ID)
			wantCode := CodeHMEAuthentication
			if missing == "both sessions" {
				wantCode = CodeLoginRequired
			}
			if Code(err) != wantCode {
				t.Fatalf("original Web code=%q want=%q", Code(err), wantCode)
			}
		})
	}
}

func TestSyncAccountDirectoryChecksLoginIdentityAndForwardOwnership(t *testing.T) {
	for _, mismatch := range []string{"apple id", "region", "unknown region", "forwarding", "empty unowned directory"} {
		t.Run(mismatch, func(t *testing.T) {
			s, db, account, _, original := directoryServiceFixture(t)
			s.client = &directoryTestClient{fakeAppleClient: &fakeAppleClient{}, read: func(_ context.Context, managed apple.AccountSession) (apple.ListResult, apple.AccountSession, error) {
				list := ownedDirectory(account)
				managed.SCNT = "rotated-fixture"
				managed.UpdatedAt = time.Now().UTC()
				switch mismatch {
				case "apple id":
					managed.AppleID = "other@example.com"
				case "region":
					managed.Region = apple.RegionChina
				case "unknown region":
					managed.Region = "unknown"
				case "forwarding":
					list.SelectedForwardTo = "other@example.com"
					list.ForwardToEmails = []string{"other@example.com"}
					for index := range list.Aliases {
						list.Aliases[index].ForwardToEmail = "other@example.com"
					}
				case "empty unowned directory":
					list = apple.ListResult{}
				}
				return list, managed, nil
			}}
			if _, err := s.SyncAliases(context.Background(), account.ID); !errors.Is(err, ErrAccountMismatch) {
				t.Fatalf("directory mismatch classification: %v", err)
			}
			aliases, err := db.ListAliasesByAccount(context.Background(), account.ID)
			if err != nil || len(aliases) != 0 {
				t.Fatal("foreign Account directory published aliases")
			}
			if mismatch == "apple id" || mismatch == "region" || mismatch == "unknown region" {
				managed, err := s.readAccountManagementSession(context.Background(), account.ID)
				if err != nil || managed.SCNT != original.SCNT {
					t.Fatal("foreign returned identity replaced Account credentials")
				}
			}
		})
	}
}

func TestSyncAccountDirectoryFallbackDoesNotDeleteWithoutWebCrosscheck(t *testing.T) {
	s, db, account, _, _ := directoryServiceFixture(t)
	reads := 0
	web := &fakeAppleClient{}
	s.client = &directoryTestClient{fakeAppleClient: web, read: func(_ context.Context, managed apple.AccountSession) (apple.ListResult, apple.AccountSession, error) {
		reads++
		list := ownedDirectory(account)
		if reads > 1 {
			list.Aliases = []apple.Alias{}
		}
		return list, managed, nil
	}}
	if _, err := s.SyncAliases(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	before, err := db.ListAliasesByAccount(context.Background(), account.ID)
	if err != nil || len(before) != 2 {
		t.Fatal("first directory did not import expected aliases")
	}
	result, err := s.SyncAliases(context.Background(), account.ID)
	if err != nil || result.Summary.Total != 0 {
		t.Fatalf("empty owned projection: %v", err)
	}
	after, err := db.ListAliasesByAccount(context.Background(), account.ID)
	if err != nil || len(after) != len(before) || result.Summary.RemovedCount != 0 || result.Summary.DirectoryWarning != "APPLE_DIRECTORY_RECONCILIATION_PENDING" || web.deactivateCalls.Load() != 0 || web.deleteCalls.Load() != 0 {
		t.Fatal("fallback without Web confirmation removed addresses or made remote writes")
	}
}

func TestSyncAccountDirectoryFailureRetainsAcceptedAccountCheckpoint(t *testing.T) {
	s, db, account, locker, _ := directoryServiceFixture(t)
	s.client = &directoryTestClient{fakeAppleClient: &fakeAppleClient{}, read: func(_ context.Context, managed apple.AccountSession) (apple.ListResult, apple.AccountSession, error) {
		assertNetworkOutsideAccountLock(t, locker)
		managed.SCNT = "accepted-fixture"
		managed.UpdatedAt = time.Now().UTC()
		return apple.ListResult{}, managed, &apple.Error{Kind: apple.ErrService, StatusCode: 503, RetryAfter: 90 * time.Second}
	}}
	_, err := s.SyncAliases(context.Background(), account.ID)
	if !errors.Is(err, ErrUpstream) || apple.RetryDelay(err) != 90*time.Second {
		t.Fatalf("Account failure metadata: %v", err)
	}
	managed, err := s.readAccountManagementSession(context.Background(), account.ID)
	if err != nil || managed.SCNT != "accepted-fixture" {
		t.Fatal("accepted Account checkpoint was lost after read failure")
	}
	if _, err := db.GetAppleWebSession(context.Background(), account.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("failed Account request fabricated Web authentication")
	}
}

func TestSyncAccountDirectoryOnlyExplicitAuthenticationFailureRequiresLogin(t *testing.T) {
	for _, test := range []struct {
		name     string
		upstream error
		rejected bool
	}{
		{"expired", &apple.Error{Kind: apple.ErrInvalidSession, StatusCode: 401}, true},
		{"throttled", &apple.Error{Kind: apple.ErrService, StatusCode: 429}, false},
		{"unavailable", &apple.Error{Kind: apple.ErrService, StatusCode: 503}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _, account, _, _ := directoryServiceFixture(t)
			s.client = &directoryTestClient{fakeAppleClient: &fakeAppleClient{}, read: func(_ context.Context, _ apple.AccountSession) (apple.ListResult, apple.AccountSession, error) {
				return apple.ListResult{}, apple.AccountSession{}, test.upstream
			}}
			if _, err := s.SyncAliases(context.Background(), account.ID); err == nil {
				t.Fatal("expected fixture directory failure")
			}
			stored, err := s.readAccountManagementSession(context.Background(), account.ID)
			if err != nil || stored.RefreshRejected != test.rejected {
				t.Fatalf("directory failure re-login eligibility: rejected=%v want=%v err=%v", stored.RefreshRejected, test.rejected, err)
			}
		})
	}
}

func TestSyncAccountDirectoryRejectsAccountIdentityRace(t *testing.T) {
	s, db, account, locker, _ := directoryServiceFixture(t)
	s.client = &directoryTestClient{fakeAppleClient: &fakeAppleClient{}, read: func(ctx context.Context, managed apple.AccountSession) (apple.ListResult, apple.AccountSession, error) {
		assertNetworkOutsideAccountLock(t, locker)
		changed := account
		changed.IMAPUsername = "changed@icloud.com"
		if _, err := db.UpdateAccount(ctx, changed); err != nil {
			t.Fatal(err)
		}
		return ownedDirectory(account), managed, nil
	}}
	if _, err := s.SyncAliases(context.Background(), account.ID); !errors.Is(err, ErrAccountChanged) {
		t.Fatalf("account identity race: %v", err)
	}
	aliases, err := db.ListAliasesByAccount(context.Background(), account.ID)
	if err != nil || len(aliases) != 0 {
		t.Fatal("stale Account directory published after account identity changed")
	}
}

func TestSyncAccountDirectoryChecksFailedReturnedIdentityBeforeSaving(t *testing.T) {
	for _, changed := range []string{"apple id", "region"} {
		t.Run(changed, func(t *testing.T) {
			s, _, account, _, original := directoryServiceFixture(t)
			s.client = &directoryTestClient{fakeAppleClient: &fakeAppleClient{}, read: func(_ context.Context, managed apple.AccountSession) (apple.ListResult, apple.AccountSession, error) {
				managed.SCNT = "foreign-fixture"
				managed.UpdatedAt = time.Now().UTC()
				if changed == "apple id" {
					managed.AppleID = "foreign@example.com"
				} else {
					managed.Region = apple.RegionChina
				}
				return apple.ListResult{}, managed, &apple.Error{Kind: apple.ErrService, StatusCode: 503}
			}}
			if _, err := s.SyncAliases(context.Background(), account.ID); !errors.Is(err, ErrAccountMismatch) {
				t.Fatalf("failed returned session identity: %v", err)
			}
			managed, err := s.readAccountManagementSession(context.Background(), account.ID)
			if err != nil || managed.SCNT != original.SCNT {
				t.Fatal("failed foreign Account checkpoint replaced saved credentials")
			}
		})
	}
}

type directoryPublicationLocker struct {
	base   *fakeLocker
	before func()
}

func (l *directoryPublicationLocker) WithAccountLock(ctx context.Context, id int64, operation func() error) error {
	l.before()
	return l.base.WithAccountLock(ctx, id, operation)
}

func TestSyncAccountDirectoryRechecksIdentityAtAliasPublication(t *testing.T) {
	s, db, account, locker, _ := directoryServiceFixture(t)
	locks := 0
	s.locker = &directoryPublicationLocker{base: locker, before: func() {
		locks++
		if locks == 2 {
			changed := account
			changed.IMAPUsername = "changed@icloud.com"
			if _, err := db.UpdateAccount(context.Background(), changed); err != nil {
				t.Fatal(err)
			}
		}
	}}
	s.client = &directoryTestClient{fakeAppleClient: &fakeAppleClient{}, read: func(_ context.Context, managed apple.AccountSession) (apple.ListResult, apple.AccountSession, error) {
		assertNetworkOutsideAccountLock(t, locker)
		return ownedDirectory(account), managed, nil
	}}
	if _, err := s.SyncAliases(context.Background(), account.ID); !errors.Is(err, ErrAccountChanged) || locks != 2 {
		t.Fatalf("publication identity race: locks=%d err=%v", locks, err)
	}
	aliases, err := db.ListAliasesByAccount(context.Background(), account.ID)
	if err != nil || len(aliases) != 0 {
		t.Fatal("Account directory published after the final identity check became stale")
	}
}
