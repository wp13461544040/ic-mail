package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/hmesync"
)

type accountAuthFake struct {
	fakeHMESyncService
	info *hmesync.SessionInfo
}

func (f *accountAuthFake) StartAccountAuth(context.Context, int64, int64, string, string, apple.Region) (hmesync.AuthResult, error) {
	return hmesync.AuthResult{Status: hmesync.StatusVerificationRequired, ChallengeID: "challenge"}, nil
}
func (f *accountAuthFake) VerifyAccountAuth(context.Context, int64, int64, string, string) (hmesync.AuthResult, error) {
	return hmesync.AuthResult{Status: hmesync.StatusAuthenticated}, nil
}
func (f *accountAuthFake) GetAccountSession(context.Context, int64) (hmesync.SessionInfo, error) {
	if f.info != nil {
		return *f.info, nil
	}
	return hmesync.SessionInfo{Status: hmesync.StatusAuthenticated}, nil
}
func (f *accountAuthFake) ClearAccountAuth(context.Context, int64) error { return nil }

func TestAppleAccountAuthEndpoints(t *testing.T) {
	env := newAdminAPITestEnv(t)
	acct := adminAPITestCreateAccount(t, env, "auth-test@icloud.com")
	path := fmt.Sprintf("/admin/api/v1/accounts/%d/apple-account-auth", acct.ID)
	if got := env.request(t, http.MethodGet, path, nil, "", nil, "").Code; got != http.StatusUnauthorized {
		t.Fatalf("anonymous GET=%d", got)
	}
	cookie, csrf, _ := env.createSession(t, "auth-test-admin", "password")
	env.server.SetHMESyncService(&accountAuthFake{})
	if got := env.request(t, http.MethodPost, path, []byte(`{"apple_id":"a@icloud.com","password":"p","region":"global"}`), "application/json", []*http.Cookie{cookie}, "").Code; got != http.StatusForbidden {
		t.Fatalf("missing csrf=%d", got)
	}
	if got := env.request(t, http.MethodPost, path, []byte(`{"apple_id":"a@icloud.com","password":"p","region":"global"}`), "application/json", []*http.Cookie{cookie}, csrf).Code; got != http.StatusAccepted {
		t.Fatalf("start=%d", got)
	}
	verify := path + "/verify"
	if got := env.request(t, http.MethodPost, verify, []byte(`{"challenge_id":"challenge","code":"123456"}`), "application/json", []*http.Cookie{cookie}, csrf).Code; got != http.StatusOK {
		t.Fatalf("verify=%d", got)
	}
	if got := env.request(t, http.MethodDelete, path, []byte(`{}`), "application/json", []*http.Cookie{cookie}, csrf).Code; got != http.StatusNoContent {
		t.Fatalf("delete=%d", got)
	}
}

func TestAppleAccountHealthResponseOnlyIncludesPublicSessionMetadata(t *testing.T) {
	env := newAdminAPITestEnv(t)
	account := adminAPITestCreateAccount(t, env, "health-fixture@icloud.com")
	cookie, _, _ := env.createSession(t, "health-admin", "password")
	next := time.Now().UTC().Add(time.Minute)
	env.server.SetHMESyncService(&accountAuthFake{info: &hmesync.SessionInfo{
		Status: hmesync.StatusExpired, Health: "retry_wait", RefreshFailures: 2, NextRetryAt: &next,
	}})
	path := fmt.Sprintf("/admin/api/v1/accounts/%d/apple-account-auth", account.ID)
	response := env.request(t, http.MethodGet, path, nil, "", []*http.Cookie{cookie}, "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET status=%d cache=%s", response.Code, response.Header().Get("Cache-Control"))
	}
	var body struct {
		Data struct {
			Session map[string]json.RawMessage `json:"apple_session"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if string(body.Data.Session["health"]) != `"retry_wait"` || string(body.Data.Session["refresh_failures"]) != "2" || body.Data.Session["next_retry_at"] == nil {
		t.Fatal("retry health metadata missing")
	}
	allowed := map[string]bool{"status": true, "apple_id": true, "region": true, "authenticated_at": true, "expires_at": true, "health": true, "last_refreshed_at": true, "next_refresh_at": true, "next_retry_at": true, "refresh_failures": true}
	for field := range body.Data.Session {
		if !allowed[field] {
			t.Fatalf("unexpected session field %q", field)
		}
	}
}
