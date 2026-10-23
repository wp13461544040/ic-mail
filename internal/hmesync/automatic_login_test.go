package hmesync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/autologincode"
	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

type autoLoginFetcherFunc func(context.Context, string) (autologincode.Result, error)

func (f autoLoginFetcherFunc) Fetch(ctx context.Context, url string) (autologincode.Result, error) {
	return f(ctx, url)
}

type automaticLoginTestClient struct {
	fakeAppleClient
	accountSignIn func(context.Context, string, string, apple.Region, *apple.AccountSession) (apple.AccountSession, bool, error)
	accountVerify func(context.Context, apple.AccountSession, string) (apple.AccountSession, error)
}

func (c *automaticLoginTestClient) SignInAccount(ctx context.Context, id, password string, region apple.Region, previous *apple.AccountSession) (apple.AccountSession, bool, error) {
	return c.accountSignIn(ctx, id, password, region, previous)
}

func (c *automaticLoginTestClient) VerifyAccountCode(ctx context.Context, session apple.AccountSession, code string) (apple.AccountSession, error) {
	return c.accountVerify(ctx, session, code)
}

func (*automaticLoginTestClient) CreateAccountAlias(context.Context, apple.AccountSession, string, string) (apple.Alias, apple.AccountSession, error) {
	panic("automatic login must never create aliases")
}

func automaticLoginFixture(t *testing.T) (*Service, *store.Store, domain.Account, domain.AccountAutoLoginConfig, *automaticLoginTestClient) {
	t.Helper()
	s, db, account := independentAccountFixture(t, true)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	s.autoLoginWait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	cipher := s.cipher.(*secure.Cipher)
	secret, err := cipher.EncryptAccountAutoLogin(account.Email, secure.AccountAutoLoginCredentials{Password: " opaque Apple password ", CodeURL: "https://codes.example.test/read?secret=fixture"})
	if err != nil {
		t.Fatal(err)
	}
	account.AutoLogin = &domain.AccountAutoLoginConfig{Enabled: true, Region: "cn", CredentialsCiphertext: secret}
	if _, err := db.UpdateAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	account.AutoLogin = nil
	config, err := db.GetAccountAutoLoginConfig(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	client := &automaticLoginTestClient{}
	s.client = client
	return s, db, account, config, client
}

func TestAutomaticLoginBothChannelsUseSavedCredentialsAndNewCode(t *testing.T) {
	s, db, account, config, client := automaticLoginFixture(t)
	ctx := context.Background()
	passwordCalls, verifyCalls, fetches := 0, 0, 0
	check := func(id, password string, region apple.Region) {
		t.Helper()
		if id != account.Email || password != " opaque Apple password " || region != apple.RegionChina {
			t.Fatal("saved identity, opaque password or region changed")
		}
		passwordCalls++
	}
	client.accountSignIn = func(_ context.Context, id, password string, region apple.Region, _ *apple.AccountSession) (apple.AccountSession, bool, error) {
		check(id, password, region)
		return apple.AccountSession{AppleID: id, Region: region}, true, nil
	}
	client.accountVerify = func(_ context.Context, session apple.AccountSession, code string) (apple.AccountSession, error) {
		if code != "222222" {
			t.Fatal("new channel submitted cached code")
		}
		verifyCalls++
		session.APIKey, session.AuthenticatedAt, session.ExpiresAt = "fixture-key", s.now(), s.now().Add(time.Hour)
		return session, nil
	}
	client.signIn = func(_ context.Context, id, password string, region apple.Region, _ *apple.Session) (apple.Session, bool, error) {
		check(id, password, region)
		return apple.Session{AppleID: id, Region: region, DSID: "fixture-dsid", SessionToken: "challenge-token"}, true, nil
	}
	client.verify = func(_ context.Context, session apple.Session, code string) (apple.Session, error) {
		if code != "444444" {
			t.Fatal("old channel submitted cached code")
		}
		verifyCalls++
		session.SessionToken = "authenticated-token"
		return session, nil
	}
	s.autoLoginCodeFetcher = autoLoginFetcherFunc(func(_ context.Context, url string) (autologincode.Result, error) {
		if url != "https://codes.example.test/read?secret=fixture" {
			t.Fatal("saved code URL changed")
		}
		codes := []string{"111111", "111111", "222222", "333333", "333333", "444444"}
		if fetches >= len(codes) {
			t.Fatal("unexpected extra polling")
		}
		code := codes[fetches]
		fetches++
		return autologincode.Result{Code: code}, nil
	})
	for _, channel := range []string{"apple_account", "icloud_web"} {
		s.runAutomaticLoginChannel(ctx, config, channel, nil)
		state, err := db.GetAccountAutoLoginState(ctx, account.ID, channel)
		if err != nil || state.Status != "authenticated" || state.Failures != 0 || state.LastSuccessAt == nil {
			t.Fatalf("channel=%s state=%s code=%s err=%v", channel, state.Status, state.LastErrorCode, err)
		}
		// Healthy sessions must not start login or read the provider again.
		s.runAutomaticLoginChannel(ctx, config, channel, nil)
	}
	if passwordCalls != 2 || verifyCalls != 2 || fetches != 6 {
		t.Fatalf("unexpected call counts: password=%d verify=%d fetch=%d", passwordCalls, verifyCalls, fetches)
	}
	web, err := s.GetSession(ctx, account.ID)
	if err != nil || web.Status != StatusAuthenticated {
		t.Fatal("web channel was not restored")
	}
	managed, err := s.GetAccountSession(ctx, account.ID)
	if err != nil || managed.Status != StatusAuthenticated {
		t.Fatal("new channel was not preserved by later web login")
	}
}

func TestAutomaticLoginRejectsOldCodesAndPersistsRetry(t *testing.T) {
	s, db, account, config, client := automaticLoginFixture(t)
	passwordCalls, fetches := 0, 0
	client.accountSignIn = func(_ context.Context, id, _ string, region apple.Region, _ *apple.AccountSession) (apple.AccountSession, bool, error) {
		passwordCalls++
		return apple.AccountSession{AppleID: id, Region: region}, true, nil
	}
	client.accountVerify = func(context.Context, apple.AccountSession, string) (apple.AccountSession, error) {
		t.Fatal("old code must not be submitted")
		return apple.AccountSession{}, nil
	}
	s.autoLoginCodeFetcher = autoLoginFetcherFunc(func(context.Context, string) (autologincode.Result, error) {
		fetches++
		return autologincode.Result{Code: "111111"}, nil
	})
	s.runAutomaticLoginChannel(context.Background(), config, "apple_account", nil)
	state, err := db.GetAccountAutoLoginState(context.Background(), account.ID, "apple_account")
	if err != nil || state.LastErrorCode != codeAutoLoginCodeTimeout || state.NextAttemptAt == nil || !state.NextAttemptAt.After(s.now()) {
		t.Fatal("cached-code timeout did not persist a retry deadline")
	}
	s.runAutomaticLoginChannel(context.Background(), config, "apple_account", nil)
	if passwordCalls != 1 || fetches != 19 {
		t.Fatal("retry cooldown was bypassed")
	}
}

func TestAutomaticLoginManualLogoutOrConfigurationChangeCancelsWaitingCode(t *testing.T) {
	for _, action := range []string{"logout", "disable_config", "disable_account", "replace_password"} {
		t.Run(action, func(t *testing.T) {
			s, db, account, config, client := automaticLoginFixture(t)
			client.accountSignIn = func(_ context.Context, id, _ string, region apple.Region, _ *apple.AccountSession) (apple.AccountSession, bool, error) {
				return apple.AccountSession{AppleID: id, Region: region}, true, nil
			}
			client.accountVerify = func(context.Context, apple.AccountSession, string) (apple.AccountSession, error) {
				t.Fatal("stale task submitted code after user changed login intent")
				return apple.AccountSession{}, nil
			}
			fetches := 0
			s.autoLoginCodeFetcher = autoLoginFetcherFunc(func(context.Context, string) (autologincode.Result, error) {
				fetches++
				if fetches == 1 {
					return autologincode.Result{}, autologincode.ErrNoCode
				}
				return autologincode.Result{Code: "222222"}, nil
			})
			s.autoLoginWait = func(ctx context.Context, _ time.Duration) error {
				if action == "logout" {
					return s.ClearAccountAuth(ctx, account.ID)
				}
				account.AutoLogin = &config
				if action == "disable_config" {
					account.AutoLogin.Enabled = false
				} else if action == "disable_account" {
					account.Enabled = false
				} else {
					account.AutoLogin.CredentialsCiphertext = "replacement-ciphertext"
				}
				_, err := db.UpdateAccount(ctx, account)
				return err
			}
			s.runAutomaticLoginChannel(context.Background(), config, "apple_account", nil)
			if _, err := db.GetAppleAccountSession(context.Background(), account.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("stale task published a session")
			}
		})
	}
}

func TestAutomaticLoginFailureClassificationAndRetryAfter(t *testing.T) {
	for _, invalidPassword := range []bool{false, true} {
		s, db, account, config, client := automaticLoginFixture(t)
		client.accountSignIn = func(context.Context, string, string, apple.Region, *apple.AccountSession) (apple.AccountSession, bool, error) {
			if invalidPassword {
				return apple.AccountSession{}, false, apple.ErrAuthentication
			}
			return apple.AccountSession{}, false, &apple.Error{Kind: apple.ErrService, StatusCode: 429, RetryAfter: 2 * time.Hour}
		}
		s.autoLoginCodeFetcher = autoLoginFetcherFunc(func(context.Context, string) (autologincode.Result, error) {
			return autologincode.Result{}, autologincode.ErrNoCode
		})
		s.runAutomaticLoginChannel(context.Background(), config, "apple_account", nil)
		state, err := db.GetAccountAutoLoginState(context.Background(), account.ID, "apple_account")
		if err != nil {
			t.Fatal(err)
		}
		if invalidPassword {
			if state.Status != "needs_action" || state.NextAttemptAt != nil || state.LastErrorCode != CodeCredentialsInvalid {
				t.Fatal("invalid password did not stop automatic retries")
			}
		} else if state.Status != "retry_wait" || state.NextAttemptAt == nil || state.NextAttemptAt.Before(s.now().Add(2*time.Hour)) {
			t.Fatal("Apple Retry-After was shortened")
		}
	}
}

func TestAutomaticLoginLogsUpstreamFingerprintWithoutCauseOrCredentials(t *testing.T) {
	s, _, _, config, client := automaticLoginFixture(t)
	client.accountSignIn = func(context.Context, string, string, apple.Region, *apple.AccountSession) (apple.AccountSession, bool, error) {
		return apple.AccountSession{}, false, &apple.Error{Op: "account API", Kind: apple.ErrService,
			StatusCode: 503, ServiceCode: "-1", Err: errors.New("secret upstream response body")}
	}
	s.autoLoginCodeFetcher = autoLoginFetcherFunc(func(context.Context, string) (autologincode.Result, error) {
		return autologincode.Result{}, autologincode.ErrNoCode
	})
	var output bytes.Buffer
	s.runAutomaticLoginChannel(context.Background(), config, "apple_account", slog.New(slog.NewJSONHandler(&output, nil)))
	var event map[string]any
	if err := json.Unmarshal(output.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event["http_status"] != float64(503) || event["upstream_operation"] != "account API" || event["service_code"] != "-1" {
		t.Fatal("upstream diagnostic fingerprint missing")
	}
	for _, secret := range []string{"secret upstream response body", "opaque Apple password", "codes.example.test", "secret=fixture"} {
		if strings.Contains(output.String(), secret) {
			t.Fatal("login log leaked credential material or upstream response")
		}
	}
}

func TestAutomaticLoginCodeFreshness(t *testing.T) {
	now := time.Now()
	before := now.Add(-5 * time.Second)
	older := now.Add(-10 * time.Second)
	old := now.Add(-2 * time.Minute)
	future := now.Add(time.Hour)
	for _, tc := range []struct {
		name                string
		candidate, baseline autologincode.Result
		want                bool
	}{
		{"different without timestamps", autologincode.Result{Code: "222222"}, autologincode.Result{Code: "111111"}, true},
		{"same cached code", autologincode.Result{Code: "111111"}, autologincode.Result{Code: "111111"}, false},
		{"stale timestamp", autologincode.Result{Code: "222222", ReceivedAt: &old}, autologincode.Result{}, false},
		{"time reversal", autologincode.Result{Code: "222222", ReceivedAt: &older}, autologincode.Result{Code: "111111", ReceivedAt: &before}, false},
		{"same timestamp", autologincode.Result{Code: "222222", ReceivedAt: &before}, autologincode.Result{Code: "111111", ReceivedAt: &before}, false},
		{"future timestamp", autologincode.Result{Code: "222222", ReceivedAt: &future}, autologincode.Result{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := automaticCodeFresh(tc.candidate, tc.baseline, now, now); got != tc.want {
				t.Fatalf("fresh=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestAutomaticLoginOneChannelFailureDoesNotBlockOtherChannel(t *testing.T) {
	s, db, account, config, client := automaticLoginFixture(t)
	client.accountSignIn = func(context.Context, string, string, apple.Region, *apple.AccountSession) (apple.AccountSession, bool, error) {
		return apple.AccountSession{}, false, apple.ErrAuthentication
	}
	client.signIn = func(_ context.Context, id, _ string, region apple.Region, _ *apple.Session) (apple.Session, bool, error) {
		return apple.Session{AppleID: id, Region: region, DSID: "fixture-dsid", SessionToken: "fixture-token"}, false, nil
	}
	s.autoLoginCodeFetcher = autoLoginFetcherFunc(func(context.Context, string) (autologincode.Result, error) {
		return autologincode.Result{}, autologincode.ErrNoCode
	})
	ctx := context.Background()
	for _, channel := range []string{"apple_account", "icloud_web"} {
		s.runAutomaticLoginChannel(ctx, config, channel, nil)
	}
	failed, err := db.GetAccountAutoLoginState(ctx, account.ID, "apple_account")
	if err != nil || failed.Status != "needs_action" {
		t.Fatalf("first channel lost its own failure state: %s %v", failed.Status, err)
	}
	healthy, err := s.GetSession(ctx, account.ID)
	if err != nil || healthy.Status != StatusAuthenticated {
		t.Fatalf("second channel failed to recover independently: %s %v", healthy.Status, err)
	}
}

func TestAutomaticLoginLatePasswordReplyCannotPublishAfterSettingsChange(t *testing.T) {
	for _, action := range []string{"disable_account", "disable_config", "replace_password", "disable_then_reenable"} {
		t.Run(action, func(t *testing.T) {
			s, db, account, config, client := automaticLoginFixture(t)
			client.accountSignIn = func(ctx context.Context, id, _ string, region apple.Region, _ *apple.AccountSession) (apple.AccountSession, bool, error) {
				// Simulate an external store writer changing intent while Apple is responding.
				account.AutoLogin = &config
				switch action {
				case "disable_account", "disable_then_reenable":
					account.Enabled = false
				case "disable_config":
					account.AutoLogin.Enabled = false
				case "replace_password":
					account.AutoLogin.CredentialsCiphertext = "replacement-fixture"
				}
				if _, err := db.UpdateAccount(ctx, account); err != nil {
					t.Fatal(err)
				}
				if action == "disable_then_reenable" {
					account.Enabled, account.AutoLogin = true, nil
					if _, err := db.UpdateAccount(ctx, account); err != nil {
						t.Fatal(err)
					}
				}
				return apple.AccountSession{AppleID: id, Region: region, APIKey: "fixture-key", AuthenticatedAt: s.now(), ExpiresAt: s.now().Add(time.Hour)}, false, nil
			}
			s.autoLoginCodeFetcher = autoLoginFetcherFunc(func(context.Context, string) (autologincode.Result, error) {
				return autologincode.Result{}, autologincode.ErrNoCode
			})
			s.runAutomaticLoginChannel(context.Background(), config, "apple_account", nil)
			if _, err := db.GetAppleAccountSession(context.Background(), account.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("late successful password reply published a session after settings changed")
			}
		})
	}
}
