package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

const (
	autoLoginAPITestPassword = " fixture-apple-login-password "
	autoLoginAPITestCodeURL  = "https://auto-login-no-network.invalid/code?key=fixture-private-code-key"
)

type autoLoginAPINotifier struct {
	HMESyncService
	ids []int64
}

func (n *autoLoginAPINotifier) NotifyAutomaticLogin(id int64) { n.ids = append(n.ids, id) }

func TestAdminAPIAccountAutoLoginNotifiesOnlyAfterAcceptedWrite(t *testing.T) {
	env := newAdminAPITestEnv(t)
	notifier := &autoLoginAPINotifier{}
	env.server.SetHMESyncService(notifier)
	cookie, csrf, _ := env.createSession(t, "auto-login-events-admin", "fixture-password")
	account := autoLoginAPITestCreate(t, env, cookie, csrf, "auto-login-events@icloud.com", map[string]any{
		"enabled": true, "region": "global", "password": autoLoginAPITestPassword, "code_url": autoLoginAPITestCodeURL,
	})
	if len(notifier.ids) != 1 || notifier.ids[0] != account.ID {
		t.Fatal("created account did not publish its event")
	}
	response := autoLoginAPITestUpdate(t, env, cookie, csrf, account, map[string]any{"enabled": false})
	if response.Code != http.StatusOK || len(notifier.ids) != 2 || notifier.ids[1] != account.ID {
		t.Fatal("disabling automatic login did not publish its event")
	}
	response = autoLoginAPITestUpdate(t, env, cookie, "", account, map[string]any{"enabled": true})
	if response.Code != http.StatusForbidden || len(notifier.ids) != 2 {
		t.Fatal("rejected write published an automatic login event")
	}
	response = autoLoginAPITestUpdate(t, env, cookie, csrf, account, map[string]any{"enabled": true, "region": "invalid"})
	if response.Code != http.StatusBadRequest || len(notifier.ids) != 2 {
		t.Fatal("invalid configuration published an automatic login event")
	}
}

func autoLoginAPITestAccountBody(email string, config map[string]any) map[string]any {
	body := map[string]any{
		"name": "Automatic login fixture", "email": email, "imap_username": email,
		"imap_password": "fixture-imap-app-password",
	}
	if config != nil {
		body["auto_login"] = config
	}
	return body
}

func autoLoginAPITestCreate(t *testing.T, env *adminAPITestEnv, cookie *http.Cookie, csrf, email string, config map[string]any) domain.Account {
	t.Helper()
	response := env.request(t, http.MethodPost, "/admin/api/v1/accounts", adminAPITestJSON(t, autoLoginAPITestAccountBody(email, config)), "application/json", []*http.Cookie{cookie}, csrf)
	if response.Code != http.StatusCreated {
		t.Fatalf("create account status=%d", response.Code)
	}
	autoLoginAPITestNoSecrets(t, response.Body.String(), autoLoginAPITestPassword, autoLoginAPITestCodeURL)
	var envelope struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Data.ID <= 0 {
		t.Fatal("missing created account ID")
	}
	account, err := env.store.GetAccount(context.Background(), envelope.Data.ID)
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func autoLoginAPITestUpdate(t *testing.T, env *adminAPITestEnv, cookie *http.Cookie, csrf string, account domain.Account, config map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body := autoLoginAPITestAccountBody(account.Email, config)
	body["imap_password"], body["enabled"] = "", true
	return env.request(t, http.MethodPut, fmt.Sprintf("/admin/api/v1/accounts/%d", account.ID), adminAPITestJSON(t, body), "application/json", []*http.Cookie{cookie}, csrf)
}

func autoLoginAPITestNoSecrets(t *testing.T, text string, values ...string) {
	t.Helper()
	for _, value := range values {
		if value != "" && strings.Contains(text, value) {
			t.Fatal("response or log exposed a private value")
		}
	}
	for _, field := range []string{`"credentials_ciphertext":`, `"password":`, `"code_url":`, `"config_revision":`} {
		if strings.Contains(text, field) {
			t.Fatal("response exposed a private configuration field")
		}
	}
}

func autoLoginAPITestConfig(t *testing.T, env *adminAPITestEnv, accountID int64) domain.AccountAutoLoginConfig {
	t.Helper()
	config, err := env.store.GetAccountAutoLoginConfig(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestAdminAPIAccountAutoLoginRequiredFieldsRejectCreationAtomically(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, _ := env.createSession(t, "auto-login-required-admin", "fixture-password")
	tests := []struct {
		name   string
		config map[string]any
	}{
		{"all_missing", map[string]any{"enabled": true}},
		{"password_missing", map[string]any{"enabled": true, "region": "global", "code_url": autoLoginAPITestCodeURL}},
		{"code_url_missing", map[string]any{"enabled": true, "region": "global", "password": autoLoginAPITestPassword}},
		{"region_missing", map[string]any{"enabled": true, "password": autoLoginAPITestPassword, "code_url": autoLoginAPITestCodeURL}},
		{"region_invalid", map[string]any{"enabled": true, "region": "other", "password": autoLoginAPITestPassword, "code_url": autoLoginAPITestCodeURL}},
		{"enabled_missing", map[string]any{"region": "global", "password": autoLoginAPITestPassword, "code_url": autoLoginAPITestCodeURL}},
		{"private_loopback", map[string]any{"enabled": true, "region": "global", "password": autoLoginAPITestPassword, "code_url": "http://127.0.0.1:8818/code?key=fixture"}},
		{"private_network", map[string]any{"enabled": true, "region": "global", "password": autoLoginAPITestPassword, "code_url": "http://10.1.2.3/code"}},
		{"metadata_linklocal", map[string]any{"enabled": true, "region": "global", "password": autoLoginAPITestPassword, "code_url": "http://169.254.169.254/code"}},
		{"mapped_ipv6", map[string]any{"enabled": true, "region": "global", "password": autoLoginAPITestPassword, "code_url": "http://[::ffff:127.0.0.1]/code"}},
		{"url_userinfo", map[string]any{"enabled": true, "region": "global", "password": autoLoginAPITestPassword, "code_url": "https://user:secret@provider.invalid/code"}},
		{"url_fragment", map[string]any{"enabled": true, "region": "global", "password": autoLoginAPITestPassword, "code_url": "https://provider.invalid/code#secret"}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := env.request(t, http.MethodPost, "/admin/api/v1/accounts", adminAPITestJSON(t, autoLoginAPITestAccountBody(fmt.Sprintf("auto-login-reject-%d@icloud.com", index), test.config)), "application/json", []*http.Cookie{cookie}, csrf)
			if response.Code != http.StatusBadRequest || adminAPITestErrorCode(t, response) != "VALIDATION_FAILED" {
				t.Fatalf("invalid automatic login creation status=%d", response.Code)
			}
			autoLoginAPITestNoSecrets(t, response.Body.String(), autoLoginAPITestPassword, autoLoginAPITestCodeURL)
			accounts, err := env.store.ListAccounts(context.Background())
			if err != nil || len(accounts) != 0 {
				t.Fatal("failed creation left an account row")
			}
			var count int
			if err := env.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM account_auto_login_configs`).Scan(&count); err != nil || count != 0 {
				t.Fatal("failed creation left automatic login configuration")
			}
		})
	}
}

func TestAdminAPIAccountAutoLoginDisabledCredentialsAreOptional(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, _ := env.createSession(t, "auto-login-disabled-admin", "fixture-password")
	account := autoLoginAPITestCreate(t, env, cookie, csrf, "auto-login-disabled@icloud.com", map[string]any{"enabled": false})
	config := autoLoginAPITestConfig(t, env, account.ID)
	if config.Enabled || config.CredentialsCiphertext != "" || config.Region != "global" {
		t.Fatal("disabled configuration unexpectedly requires saved credentials")
	}
	response := env.request(t, http.MethodGet, fmt.Sprintf("/admin/api/v1/accounts/%d/auto-login", account.ID), nil, "", []*http.Cookie{cookie}, "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("disabled configuration GET status=%d", response.Code)
	}
	var payload struct {
		Data struct {
			Enabled            bool `json:"enabled"`
			PasswordConfigured bool `json:"password_configured"`
			CodeURLConfigured  bool `json:"code_url_configured"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload.Data.Enabled || payload.Data.PasswordConfigured || payload.Data.CodeURLConfigured {
		t.Fatal("disabled public flags are incorrect")
	}
	ordinary := autoLoginAPITestCreate(t, env, cookie, csrf, "auto-login-omitted@icloud.com", nil)
	if _, err := env.store.GetAccountAutoLoginConfig(context.Background(), ordinary.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("omitted configuration unexpectedly created automatic login settings")
	}
}

func TestAdminAPIAccountAutoLoginEncryptedSaveBlankEditOverwriteAndDisable(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, _ := env.createSession(t, "auto-login-edit-admin", "fixture-password")
	var logs strings.Builder
	env.server.logger = slog.New(slog.NewTextHandler(&logs, nil))
	account := autoLoginAPITestCreate(t, env, cookie, csrf, "auto-login-edit@icloud.com", map[string]any{
		"enabled": true, "region": "cn", "password": autoLoginAPITestPassword, "code_url": autoLoginAPITestCodeURL,
	})
	initial := autoLoginAPITestConfig(t, env, account.ID)
	if !initial.Enabled || initial.Region != "cn" || initial.Revision != 1 || initial.CredentialsCiphertext == "" || strings.Contains(initial.CredentialsCiphertext, autoLoginAPITestPassword) || strings.Contains(initial.CredentialsCiphertext, autoLoginAPITestCodeURL) {
		t.Fatal("credentials were not saved as an encrypted account-bound envelope")
	}
	credentials, err := env.cipher.DecryptAccountAutoLogin(account.Email, initial.CredentialsCiphertext)
	if err != nil || credentials.Password != autoLoginAPITestPassword || credentials.CodeURL != autoLoginAPITestCodeURL {
		t.Fatal("saved encrypted credentials differ from input")
	}
	if _, err := env.cipher.DecryptAccountAutoLogin("another-account@icloud.com", initial.CredentialsCiphertext); err == nil {
		t.Fatal("credentials envelope is not bound to the account")
	}
	now := time.Now().UTC().Truncate(time.Second)
	next := now.Add(20 * time.Minute)
	state := domain.AccountAutoLoginState{AccountID: account.ID, Channel: domain.AutoLoginChannelAppleAccount,
		ConfigRevision: initial.Revision, Status: "retry_wait", Failures: 3, LastErrorCode: "AUTO_LOGIN_CODE_SERVICE_UNAVAILABLE", LastAttemptAt: &now, NextAttemptAt: &next}
	if err := env.store.SaveAccountAutoLoginState(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	for _, configInput := range []map[string]any{nil, {"enabled": true, "region": "cn", "password": "", "code_url": ""}} {
		response := autoLoginAPITestUpdate(t, env, cookie, csrf, account, configInput)
		if response.Code != http.StatusOK {
			t.Fatalf("blank edit status=%d", response.Code)
		}
		unchanged := autoLoginAPITestConfig(t, env, account.ID)
		if unchanged.Revision != initial.Revision || unchanged.CredentialsCiphertext != initial.CredentialsCiphertext || !unchanged.UpdatedAt.Equal(initial.UpdatedAt) {
			t.Fatal("ordinary or blank edit changed configuration revision or credentials")
		}
		preserved, err := env.store.GetAccountAutoLoginState(context.Background(), account.ID, state.Channel)
		if err != nil || preserved.Failures != state.Failures || preserved.Status != state.Status || preserved.NextAttemptAt == nil || !preserved.NextAttemptAt.Equal(next) {
			t.Fatal("ordinary or blank edit reset the retry deadline")
		}
	}
	newPassword := "replacement-apple-login-password"
	response := autoLoginAPITestUpdate(t, env, cookie, csrf, account, map[string]any{"enabled": true, "region": "cn", "password": newPassword})
	if response.Code != http.StatusOK {
		t.Fatalf("password-only edit status=%d", response.Code)
	}
	afterPassword := autoLoginAPITestConfig(t, env, account.ID)
	credentials, err = env.cipher.DecryptAccountAutoLogin(account.Email, afterPassword.CredentialsCiphertext)
	if err != nil || credentials.Password != newPassword || credentials.CodeURL != autoLoginAPITestCodeURL || afterPassword.Revision != initial.Revision+1 || afterPassword.CredentialsCiphertext == initial.CredentialsCiphertext {
		t.Fatal("password overwrite did not preserve the saved code URL")
	}
	if _, err := env.store.GetAccountAutoLoginState(context.Background(), account.ID, state.Channel); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("credential replacement did not reset the previous retry state")
	}
	newCodeURL := "http://43.138.208.78:8818/api/Accounts/getCode?ID=fixture-replacement"
	response = autoLoginAPITestUpdate(t, env, cookie, csrf, account, map[string]any{"enabled": true, "region": "cn", "code_url": newCodeURL})
	if response.Code != http.StatusOK {
		t.Fatalf("code-URL-only edit status=%d", response.Code)
	}
	afterURL := autoLoginAPITestConfig(t, env, account.ID)
	credentials, err = env.cipher.DecryptAccountAutoLogin(account.Email, afterURL.CredentialsCiphertext)
	if err != nil || credentials.Password != newPassword || credentials.CodeURL != newCodeURL || afterURL.Revision != afterPassword.Revision+1 {
		t.Fatal("code URL overwrite did not preserve the saved password")
	}
	invalid := autoLoginAPITestUpdate(t, env, cookie, csrf, account, map[string]any{"enabled": true, "region": "cn", "code_url": "http://127.0.0.1/private"})
	if invalid.Code != http.StatusBadRequest || adminAPITestErrorCode(t, invalid) != "VALIDATION_FAILED" {
		t.Fatalf("private URL update status=%d", invalid.Code)
	}
	if unchanged := autoLoginAPITestConfig(t, env, account.ID); unchanged.Revision != afterURL.Revision || unchanged.CredentialsCiphertext != afterURL.CredentialsCiphertext {
		t.Fatal("rejected URL replacement modified saved credentials")
	}
	response = autoLoginAPITestUpdate(t, env, cookie, csrf, account, map[string]any{"enabled": false})
	if response.Code != http.StatusOK {
		t.Fatalf("disable status=%d", response.Code)
	}
	disabled := autoLoginAPITestConfig(t, env, account.ID)
	if disabled.Enabled || disabled.Region != "cn" || disabled.Revision != afterURL.Revision+1 || disabled.CredentialsCiphertext != afterURL.CredentialsCiphertext {
		t.Fatal("disable did not preserve encrypted settings for subsequent use")
	}
	autoLoginAPITestNoSecrets(t, response.Body.String()+logs.String(), autoLoginAPITestPassword, autoLoginAPITestCodeURL, newPassword, newCodeURL, initial.CredentialsCiphertext, afterURL.CredentialsCiphertext)
}

func TestAdminAPIAccountAutoLoginSessionAndCSRFBoundary(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, _ := env.createSession(t, "auto-login-security-admin", "fixture-password")
	account := adminAPITestCreateAccount(t, env, "auto-login-security@icloud.com")
	accountPath := fmt.Sprintf("/admin/api/v1/accounts/%d", account.ID)
	getPath := accountPath + "/auto-login"
	unauthenticated := env.request(t, http.MethodGet, getPath, nil, "", nil, "")
	if unauthenticated.Code != http.StatusUnauthorized || adminAPITestErrorCode(t, unauthenticated) != "AUTH_REQUIRED" {
		t.Fatal("automatic login metadata GET did not require administrator session")
	}
	body := autoLoginAPITestAccountBody(account.Email, map[string]any{"enabled": true, "region": "global", "password": autoLoginAPITestPassword, "code_url": autoLoginAPITestCodeURL})
	body["enabled"], body["imap_password"] = true, ""
	for _, test := range []struct {
		name    string
		cookies []*http.Cookie
		csrf    string
		status  int
		code    string
	}{
		{"no_session", nil, csrf, http.StatusUnauthorized, "AUTH_REQUIRED"},
		{"no_csrf", []*http.Cookie{cookie}, "", http.StatusForbidden, "CSRF_INVALID"},
		{"wrong_csrf", []*http.Cookie{cookie}, "different-token", http.StatusForbidden, "CSRF_INVALID"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := env.request(t, http.MethodPut, accountPath, adminAPITestJSON(t, body), "application/json", test.cookies, test.csrf)
			if response.Code != test.status || adminAPITestErrorCode(t, response) != test.code {
				t.Fatalf("authentication boundary status=%d", response.Code)
			}
			if _, err := env.store.GetAccountAutoLoginConfig(context.Background(), account.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("rejected request modified automatic login configuration")
			}
		})
	}
	allowedGET := env.request(t, http.MethodGet, getPath, nil, "", []*http.Cookie{cookie}, "")
	if allowedGET.Code != http.StatusOK {
		t.Fatalf("session-authenticated read unexpectedly required CSRF: status=%d", allowedGET.Code)
	}
	createBody := adminAPITestJSON(t, autoLoginAPITestAccountBody("auto-login-unauthenticated-create@icloud.com", map[string]any{
		"enabled": true, "region": "global", "password": autoLoginAPITestPassword, "code_url": autoLoginAPITestCodeURL,
	}))
	for _, test := range []struct {
		cookies []*http.Cookie
		csrf    string
		status  int
	}{
		{nil, csrf, http.StatusUnauthorized},
		{[]*http.Cookie{cookie}, "", http.StatusForbidden},
	} {
		response := env.request(t, http.MethodPost, "/admin/api/v1/accounts", createBody, "application/json", test.cookies, test.csrf)
		if response.Code != test.status {
			t.Fatalf("create authentication boundary status=%d", response.Code)
		}
	}
	if accounts, err := env.store.ListAccounts(context.Background()); err != nil || len(accounts) != 1 {
		t.Fatal("rejected authenticated creation left an account row")
	}
}

type autoLoginAPITripwireTransport struct{ calls *atomic.Int64 }

func (transport autoLoginAPITripwireTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	return nil, errors.New("offline automatic login API tripwire")
}

func TestAdminAPIAccountAutoLoginPublicGETIsLocalAndSecretFree(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, _ := env.createSession(t, "auto-login-public-admin", "fixture-password")
	account := autoLoginAPITestCreate(t, env, cookie, csrf, "auto-login-public@icloud.com", map[string]any{
		"enabled": true, "region": "global", "password": autoLoginAPITestPassword, "code_url": autoLoginAPITestCodeURL,
	})
	config := autoLoginAPITestConfig(t, env, account.ID)
	now, next := time.Now().UTC().Truncate(time.Second), time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	if err := env.store.SaveAccountAutoLoginState(context.Background(), domain.AccountAutoLoginState{
		AccountID: account.ID, Channel: domain.AutoLoginChannelAppleAccount, ConfigRevision: config.Revision,
		Status: "retry_wait", LastErrorCode: autoLoginAPITestCodeURL, LastAttemptAt: &now, NextAttemptAt: &next,
	}); err != nil {
		t.Fatal(err)
	}
	// Never allow this metadata read to accidentally contact a code provider.
	// Tests are sequential; parallel package tests start after cleanup restores globals.
	var outboundHTTP, outboundDNS atomic.Int64
	originalTransport, originalResolver := http.DefaultTransport, net.DefaultResolver
	http.DefaultTransport = autoLoginAPITripwireTransport{calls: &outboundHTTP}
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		outboundDNS.Add(1)
		return nil, errors.New("offline automatic login DNS tripwire")
	}}
	t.Cleanup(func() { http.DefaultTransport, net.DefaultResolver = originalTransport, originalResolver })
	path := fmt.Sprintf("/admin/api/v1/accounts/%d/auto-login", account.ID)
	for read := 0; read < 2; read++ {
		response := env.request(t, http.MethodGet, path, nil, "", []*http.Cookie{cookie}, "")
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("metadata read status=%d", response.Code)
		}
		autoLoginAPITestNoSecrets(t, response.Body.String(), autoLoginAPITestPassword, strings.TrimSpace(autoLoginAPITestPassword), autoLoginAPITestCodeURL, config.CredentialsCiphertext)
		var payload struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		allowed := map[string]bool{"enabled": true, "region": true, "password_configured": true, "code_url_configured": true, "channels": true}
		if len(payload.Data) != len(allowed) || string(payload.Data["enabled"]) != "true" || string(payload.Data["password_configured"]) != "true" || string(payload.Data["code_url_configured"]) != "true" {
			t.Fatal("public automatic login metadata flags or fields are incorrect")
		}
		for field := range payload.Data {
			if !allowed[field] {
				t.Fatalf("unexpected public metadata field %q", field)
			}
		}
		var channels []map[string]json.RawMessage
		if err := json.Unmarshal(payload.Data["channels"], &channels); err != nil || len(channels) != 2 {
			t.Fatal("expected both independent channel states")
		}
		allowedChannel := map[string]bool{"channel": true, "status": true, "last_error_code": true, "last_attempt_at": true, "last_success_at": true, "next_attempt_at": true}
		for _, channel := range channels {
			for field := range channel {
				if !allowedChannel[field] {
					t.Fatalf("unexpected public channel field %q", field)
				}
			}
			if string(channel["last_error_code"]) != `""` {
				t.Fatal("private or malformed persisted error code was exposed")
			}
		}
	}
	if outboundHTTP.Load() != 0 || outboundDNS.Load() != 0 {
		t.Fatal("metadata GET attempted an outbound request")
	}
	for _, target := range []string{"/admin/api/v1/accounts", fmt.Sprintf("/admin/api/v1/accounts/%d", account.ID)} {
		response := env.request(t, http.MethodGet, target, nil, "", []*http.Cookie{cookie}, "")
		if response.Code != http.StatusOK {
			t.Fatalf("ordinary account GET status=%d", response.Code)
		}
		autoLoginAPITestNoSecrets(t, response.Body.String(), autoLoginAPITestPassword, autoLoginAPITestCodeURL, config.CredentialsCiphertext)
	}
	if outboundHTTP.Load() != 0 || outboundDNS.Load() != 0 {
		t.Fatal("account metadata GET attempted an outbound request")
	}
	stored := autoLoginAPITestConfig(t, env, account.ID)
	if stored.Revision != config.Revision || stored.CredentialsCiphertext != config.CredentialsCiphertext {
		t.Fatal("metadata GET changed saved credentials")
	}
}
