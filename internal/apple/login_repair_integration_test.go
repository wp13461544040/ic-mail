package apple

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

const repairIntegrationPhoneOptions = `{"repairType":"phone_number_requirement","phoneNumberRepair":true,"allowSkipPhoneNumberVerification":true,"requiredSteps":["phoneNumber","phoneNumberVerification"],"currentSteps":["phoneNumber"],"remainingSteps":["phoneNumber","phoneNumberVerification"],"requiresTerms":false,"requiresPassword":false,"privacyConsentRequired":false}`

type repairIntegrationFixture struct {
	t             *testing.T
	base          *authFixture
	entry         string
	mode          string
	active        bool
	steps         []string
	optionsCalls  int
	completeCalls int
	trustCalls    int
	exchangeCalls int
}

func (f *repairIntegrationFixture) account() bool {
	return f.entry == "SignInAccount" || f.entry == "VerifyAccountCode"
}

func (f *repairIntegrationFixture) signIn() bool {
	return f.entry == "SignIn" || f.entry == "SignInAccount"
}

func (f *repairIntegrationFixture) widget() string {
	if f.account() {
		return accountWidget
	}
	return defaultWidgetKey
}

func (f *repairIntegrationFixture) response(status int, body string) *http.Response {
	return accountResp(status, body)
}

func (f *repairIntegrationFixture) requireHeader(r *http.Request, key, want string) {
	f.t.Helper()
	if r.Header.Get(key) != want {
		f.t.Fatalf("%s: header %s did not match its synthetic checkpoint", r.URL.Path, key)
	}
}

func (f *repairIntegrationFixture) requireCookie(r *http.Request, name string) {
	f.t.Helper()
	if _, err := r.Cookie(name); err != nil {
		f.t.Fatalf("%s: missing synthetic cookie %s", r.URL.Path, name)
	}
}

func (f *repairIntegrationFixture) challenge() *http.Response {
	r := f.response(http.StatusPreconditionFailed, `{"securityCode":{"valid":true}}`)
	r.Header.Set("scnt", "auth-checkpoint-scnt")
	r.Header.Set("X-Apple-ID-Session-Id", "auth-checkpoint-id")
	r.Header.Set("X-Apple-Repair-Session-Token", "repair-initial")
	r.Header.Set("X-Apple-OAuth-Context", "oauth-fixture")
	rv := "3"
	if f.account() {
		rv = "11"
	}
	location := "https://appleid.apple.com/widget/account/repair?" + url.Values{
		"widgetKey": {f.widget()}, "rv": {rv}, "language": {"zh_CN_CHN"},
	}.Encode()
	switch f.mode {
	case "terms":
		return f.response(http.StatusPreconditionFailed, `{"requiresTerms":true}`)
	case "wrong-host":
		location = strings.Replace(location, "appleid.apple.com", "example.invalid", 1)
	case "wrong-path":
		location = strings.Replace(location, "/widget/account/repair", "/widget/account/other", 1)
	case "invalid-code":
		r.Body = io.NopCloser(strings.NewReader(`{"securityCode":{"valid":false}}`))
	case "fragment", "fragment-redirect":
		location += "#%2FphoneNumber%3Fstate%3Dfixture"
	}
	r.Header.Set("Location", location)
	r.Header.Add("Set-Cookie", "repair_challenge=fixture; Domain=.apple.com; Path=/; Secure; HttpOnly")
	return r
}

func (f *repairIntegrationFixture) RoundTrip(r *http.Request) (*http.Response, error) {
	path := r.URL.Path
	trigger := path == "/appleauth/auth/signin/complete" && f.signIn() ||
		path == "/appleauth/auth/verify/trusteddevice/securitycode" && !f.signIn() && r.Method == http.MethodPost
	if trigger {
		domain := "3"
		if f.account() {
			domain = "11"
		}
		f.requireHeader(r, "X-Apple-Domain-Id", domain)
		f.active = true
		if f.mode != "normal" {
			return f.challenge(), nil
		}
		resp := f.response(http.StatusOK, `{}`)
		resp.Header.Set("X-Apple-Session-Token", "session-token")
		return resp, nil
	}
	if f.active {
		step := r.Method + " " + path
		if path == "/account/manage/repair/options" {
			step += " " + r.Header.Get("X-Apple-Skip-Repair-Attributes")
		}
		f.steps = append(f.steps, step)
		if len(f.steps) > 12 {
			f.t.Fatal("repair flow exceeded its bounded synthetic request budget")
		}
	}
	switch path {
	case "/widget/account/repair":
		if r.URL.Fragment != "" || r.URL.RawFragment != "" {
			f.t.Fatal("browser fragment reached the widget HTTP request")
		}
		expectedHost := "appleid.apple.com"
		if f.mode == "fragment-redirect" {
			if r.URL.Host == "appleid.apple.com" {
				redirect := f.response(http.StatusFound, "")
				redirect.Header.Set("Location", "https://account.apple.com/widget/account/repair?"+r.URL.RawQuery+"#fixture")
				redirect.Header.Add("Set-Cookie", "repair_migrated=fixture; Domain=.apple.com; Path=/; Secure; HttpOnly")
				return redirect, nil
			}
			expectedHost = "account.apple.com"
			f.requireCookie(r, "repair_migrated")
			f.requireHeader(r, "Referer", "https://idmsa.apple.com/")
		}
		if r.Method != http.MethodGet || r.URL.Host != expectedHost || r.URL.Query().Get("widgetKey") != f.widget() {
			f.t.Fatal("repair navigation did not preserve its validated widget")
		}
		if r.Header.Get("scnt") != "" || r.Header.Get("X-Apple-ID-Session-Id") != "" || r.Header.Get("X-Apple-Repair-Session-Token") != "" {
			f.t.Fatal("auth checkpoints leaked into widget navigation headers")
		}
		f.requireCookie(r, "repair_challenge")
		body := `<script type="application/json" id="boot_args">{"direct":{"sessionId":"widget-only-id","scnt":"widget-initial-scnt","origin":"https://idmsa.apple.com","widgetKey":"` + f.widget() + `"}}</script>`
		if f.mode == "malformed-widget" {
			body = `<script type="application/json" id="boot_args">{"direct":</script>`
		}
		resp := f.response(http.StatusOK, body)
		resp.Header.Set("scnt", "widget-initial-scnt")
		if f.mode == "widget-checkpoint-mismatch" {
			resp.Header.Set("scnt", "different-widget-scnt")
		}
		resp.Header.Add("Set-Cookie", "repair_widget=fixture; Domain=.apple.com; Path=/; Secure; HttpOnly")
		return resp, nil
	case "/account/manage/repair/options":
		f.optionsCalls++
		if r.Method != http.MethodGet || r.URL.RawQuery != "" || r.Body != nil && r.Body != http.NoBody {
			f.t.Fatal("repair options must remain a bodyless GET without query state")
		}
		f.requireHeader(r, "X-Apple-ID-Session-Id", "widget-only-id")
		f.requireHeader(r, "X-Apple-OAuth-Context", "oauth-fixture")
		f.requireHeader(r, "X-Apple-Widget-Key", f.widget())
		f.requireCookie(r, "repair_widget")
		if r.Header.Get("X-Apple-Api-Key") != "" || r.Header.Get("X-Apple-Repair-Session-Token") != "" {
			f.t.Fatal("repair options carried unrelated credentials")
		}
		if f.optionsCalls == 1 {
			f.requireHeader(r, "scnt", "widget-initial-scnt")
			f.requireHeader(r, "X-Apple-Session-Token", "repair-initial")
			f.requireHeader(r, "X-Apple-Skip-Repair-Attributes", "[]")
			if f.mode == "rate-limit" || f.mode == "upstream-failure" {
				status := http.StatusTooManyRequests
				if f.mode == "upstream-failure" {
					status = http.StatusServiceUnavailable
				}
				return f.response(status, `{}`), nil
			}
			body := repairIntegrationPhoneOptions
			if f.mode == "unknown-repair" {
				body = strings.Replace(body, "phone_number_requirement", "unknown_repair", 1)
			}
			if f.mode == "skip-disabled" {
				body = strings.Replace(body, `"allowSkipPhoneNumberVerification":true`, `"allowSkipPhoneNumberVerification":false`, 1)
			}
			resp := f.response(http.StatusOK, body)
			resp.Header.Set("scnt", "widget-rotated-scnt")
			resp.Header.Set("X-Apple-Session-Token", "repair-middle")
			if f.mode == "options-missing-checkpoint" {
				resp.Header.Del("X-Apple-Session-Token")
			}
			return resp, nil
		}
		if f.optionsCalls != 2 {
			f.t.Fatal("repair options were replayed")
		}
		f.requireHeader(r, "scnt", "widget-rotated-scnt")
		f.requireHeader(r, "X-Apple-Session-Token", "repair-middle")
		f.requireHeader(r, "X-Apple-Skip-Repair-Attributes", `["phoneNumber"]`)
		resp := f.response(http.StatusOK, `{"type":"hsa2","repairAttribute":"complete","phoneNumberRequirementGracePeriodEnded":false}`)
		if f.mode == "skip-incomplete" {
			resp.Body = io.NopCloser(strings.NewReader(`{"type":"hsa2","repairAttribute":"phoneNumber","phoneNumberRequirementGracePeriodEnded":false}`))
		}
		if f.mode == "skip-rate-limit" {
			resp.StatusCode = http.StatusTooManyRequests
			resp.Header.Set("Retry-After", "120")
		}
		resp.Header.Set("scnt", "widget-final-scnt")
		resp.Header.Set("X-Apple-Session-Token", "repair-latest")
		if f.mode == "skip-missing-token" {
			resp.Header.Del("X-Apple-Session-Token")
		}
		return resp, nil
	case "/appleauth/auth/repair/complete":
		f.completeCalls++
		if f.completeCalls != 1 || r.Method != http.MethodPost || r.Body != nil && r.Body != http.NoBody {
			f.t.Fatal("repair completion must be submitted once without a body")
		}
		f.requireHeader(r, "scnt", "auth-checkpoint-scnt")
		f.requireHeader(r, "X-Apple-ID-Session-Id", "auth-checkpoint-id")
		f.requireHeader(r, "X-Apple-Repair-Session-Token", "repair-latest")
		f.requireCookie(r, "repair_challenge")
		f.requireCookie(r, "repair_widget")
		if f.mode == "complete-interrupted" {
			return nil, errors.New("synthetic completion interruption")
		}
		resp := f.response(http.StatusNoContent, "")
		if f.mode == "complete-unexpected-status" {
			resp.StatusCode = http.StatusOK
		}
		resp.Header.Set("scnt", "auth-completed-scnt")
		resp.Header.Set("X-Apple-ID-Session-Id", "auth-completed-id")
		if !f.account() {
			resp.Header.Set("X-Apple-Session-Token", "session-token")
		}
		resp.Header.Add("Set-Cookie", "repair_completed=fixture; Domain=.apple.com; Path=/; Secure; HttpOnly")
		return resp, nil
	case "/appleauth/auth/2sv/trust":
		f.trustCalls++
		if f.mode != "normal" {
			f.requireHeader(r, "scnt", "auth-completed-scnt")
			f.requireHeader(r, "X-Apple-ID-Session-Id", "auth-completed-id")
			f.requireCookie(r, "repair_completed")
		}
		resp := f.response(http.StatusNoContent, "")
		resp.Header.Set("X-Apple-TwoSV-Trust-Token", "trust-token")
		resp.Header.Add("Set-Cookie", "repair_trusted=fixture; Domain=.apple.com; Path=/; Secure; HttpOnly")
		return resp, nil
	case "/setup/ws/1/accountLogin":
		f.exchangeCalls++
		if f.mode != "normal" {
			var payload struct {
				TrustToken string `json:"trustToken"`
			}
			body, err := io.ReadAll(r.Body)
			if err != nil || json.Unmarshal(body, &payload) != nil || payload.TrustToken != "trust-token" {
				f.t.Fatal("web exchange omitted the repaired session trust token")
			}
			r.Body = io.NopCloser(strings.NewReader(string(body)))
		}
		return f.base.RoundTrip(r)
	case "/account/manage/section/privacy":
		return f.response(http.StatusOK, `<html/>`), nil
	case "/bootstrap/portal":
		return f.response(http.StatusOK, `{"timeOutInterval":30}`), nil
	case "/account/manage/gs/ws/token":
		if f.active {
			f.exchangeCalls++
		}
		resp := f.response(http.StatusOK, `{"timeOutInterval":30}`)
		resp.Header.Set("scnt", "management-scnt")
		return resp, nil
	case "/account/manage":
		return f.response(http.StatusOK, `{"apiKey":"fixture-api-key"}`), nil
	case "/account/manage/forwardemail":
		return f.response(http.StatusOK, `{}`), nil
	case "/appleauth/auth/authorize/signin":
		if !f.account() {
			return f.base.RoundTrip(r)
		}
		resp := f.response(http.StatusOK, `<html/>`)
		resp.Header.Set("X-Apple-HC-Bits", "1")
		resp.Header.Set("X-Apple-HC-Challenge", "fixture-proof")
		return resp, nil
	case "/appleauth/auth/verify/device/key/challenge":
		return f.response(http.StatusOK, `{}`), nil
	case "/appleauth/auth/federate":
		if f.account() {
			return f.response(http.StatusOK, `{}`), nil
		}
		return f.base.RoundTrip(r)
	case "/appleauth/auth/signin/init":
		return f.base.RoundTrip(r)
	default:
		return nil, fmt.Errorf("unexpected synthetic request: %s %s", r.Method, path)
	}
}

func runRepairIntegrationEntry(t *testing.T, entry, mode string) (*repairIntegrationFixture, Session, AccountSession, error) {
	t.Helper()
	f := &repairIntegrationFixture{t: t, base: newAuthFixture(t, http.StatusOK), entry: entry, mode: mode}
	c, err := NewClient(Config{Transport: roundTripFunc(f.RoundTrip), Random: deterministicRandom()})
	if err != nil {
		t.Fatal(err)
	}
	var web Session
	var account AccountSession
	var pending bool
	switch entry {
	case "SignIn":
		web, pending, err = c.SignIn(context.Background(), f.base.appleID, f.base.password, RegionGlobal, nil)
	case "SignInAccount":
		account, pending, err = c.SignInAccount(context.Background(), f.base.appleID, f.base.password, RegionGlobal, nil)
	case "VerifyCode":
		web, err = c.VerifyCode(context.Background(), Session{AppleID: f.base.appleID, Region: RegionGlobal, FrameID: "fixture-frame", ClientID: "fixture-client", SCNT: "earlier-auth-scnt", SessionID: "earlier-auth-id"}, "123456")
	case "VerifyAccountCode":
		account, err = c.VerifyAccountCode(context.Background(), AccountSession{AppleID: f.base.appleID, Region: RegionGlobal, FrameID: "fixture-frame", SCNT: "earlier-auth-scnt", SessionID: "earlier-auth-id"}, "123456")
	default:
		t.Fatal("unknown synthetic entry")
	}
	if pending {
		t.Fatal("completed synthetic repair still requested a second factor")
	}
	return f, web, account, err
}

func TestLoginRepairFourEntryPointsKeepWidgetStateSeparate(t *testing.T) {
	for _, entry := range []string{"SignIn", "SignInAccount", "VerifyCode", "VerifyAccountCode"} {
		t.Run(entry, func(t *testing.T) {
			f, web, account, err := runRepairIntegrationEntry(t, entry, "repair")
			if err != nil {
				t.Fatal(err)
			}
			want := []string{
				"GET /widget/account/repair",
				"GET /account/manage/repair/options []",
				`GET /account/manage/repair/options ["phoneNumber"]`,
				"POST /appleauth/auth/repair/complete",
				"GET /appleauth/auth/2sv/trust",
			}
			if f.account() {
				want = append(want, "GET /account/manage/gs/ws/token", "GET /account/manage", "GET /account/manage/forwardemail")
			} else {
				want = append(want, "POST /setup/ws/1/accountLogin")
			}
			if !reflect.DeepEqual(f.steps, want) {
				t.Fatalf("repair request sequence = %v, want %v", f.steps, want)
			}
			if f.optionsCalls != 2 || f.completeCalls != 1 || f.trustCalls != 1 || f.exchangeCalls != 1 {
				t.Fatalf("repair counts = %d/%d/%d/%d, want 2/1/1/1", f.optionsCalls, f.completeCalls, f.trustCalls, f.exchangeCalls)
			}
			var stored any = web
			if f.account() {
				stored = account
			}
			encoded, err := json.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			for _, ephemeral := range []string{"widget-only-id", "widget-initial-scnt", "widget-rotated-scnt", "widget-final-scnt", "repair-initial", "repair-middle", "repair-latest", "oauth-fixture"} {
				if strings.Contains(string(encoded), ephemeral) {
					t.Fatal("persisted login contains temporary widget state")
				}
			}
			var cookies []PersistentCookie
			if f.account() {
				var restored AccountSession
				if json.Unmarshal(encoded, &restored) != nil || restored.TrustToken != "trust-token" || restored.APIKey != "fixture-api-key" {
					t.Fatal("account JSON round trip lost the trusted management checkpoint")
				}
				cookies = restored.Cookies
			} else {
				var restored Session
				if json.Unmarshal(encoded, &restored) != nil || restored.TrustToken != "trust-token" || restored.DSID == "" {
					t.Fatal("web JSON round trip lost the trusted web checkpoint")
				}
				cookies = restored.Cookies
			}
			jar, err := NewPersistentJar(cookies)
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse("https://idmsa.apple.com/appleauth/auth")
			found := map[string]bool{}
			for _, cookie := range jar.Cookies(u) {
				found[cookie.Name] = true
			}
			for _, name := range []string{"repair_challenge", "repair_widget", "repair_completed", "repair_trusted"} {
				if !found[name] {
					t.Fatalf("JSON round trip lost synthetic cookie %s", name)
				}
			}
		})
	}
}

func TestLoginRepairFragmentContinuesAllFourEntryPoints(t *testing.T) {
	for _, entry := range []string{"SignIn", "SignInAccount", "VerifyCode", "VerifyAccountCode"} {
		for _, mode := range []string{"fragment", "fragment-redirect"} {
			t.Run(entry+"/"+mode, func(t *testing.T) {
				f, _, _, err := runRepairIntegrationEntry(t, entry, mode)
				if err != nil {
					t.Fatal(err)
				}
				want := []string{
					"GET /widget/account/repair",
					"GET /account/manage/repair/options []",
					`GET /account/manage/repair/options ["phoneNumber"]`,
					"POST /appleauth/auth/repair/complete",
					"GET /appleauth/auth/2sv/trust",
				}
				if mode == "fragment-redirect" {
					want = append([]string{"GET /widget/account/repair"}, want...)
				}
				if len(f.steps) < len(want) || !reflect.DeepEqual(f.steps[:len(want)], want) || f.optionsCalls != 2 || f.completeCalls != 1 || f.trustCalls != 1 || f.exchangeCalls != 1 {
					t.Fatalf("fragment did not complete repair, trust and session exchange: %v", f.steps)
				}
			})
		}
	}
}

func TestLoginRepairFailuresStopBeforeTrustAndExchange(t *testing.T) {
	cases := []struct {
		mode     string
		options  int
		complete int
	}{
		{"terms", 0, 0},
		{"wrong-host", 0, 0},
		{"wrong-path", 0, 0},
		{"malformed-widget", 0, 0},
		{"widget-checkpoint-mismatch", 0, 0},
		{"unknown-repair", 1, 0},
		{"skip-disabled", 1, 0},
		{"options-missing-checkpoint", 1, 0},
		{"rate-limit", 1, 0},
		{"upstream-failure", 1, 0},
		{"skip-incomplete", 2, 0},
		{"skip-rate-limit", 2, 0},
		{"skip-missing-token", 2, 0},
		{"complete-interrupted", 2, 1},
		{"complete-unexpected-status", 2, 1},
	}
	for _, entry := range []string{"SignIn", "SignInAccount", "VerifyCode", "VerifyAccountCode"} {
		for _, tc := range cases {
			t.Run(entry+"/"+tc.mode, func(t *testing.T) {
				f, _, _, err := runRepairIntegrationEntry(t, entry, tc.mode)
				if err == nil {
					t.Fatal("synthetic repair failure unexpectedly succeeded")
				}
				if tc.mode == "skip-rate-limit" && (!IsRateLimited(err) || RetryDelay(err).Seconds() != 120) {
					t.Fatal("repair throttle lost its Retry-After")
				}
				if f.optionsCalls != tc.options || f.completeCalls != tc.complete || f.trustCalls != 0 || f.exchangeCalls != 0 {
					t.Fatalf("failure request counts = %d/%d/%d/%d, want %d/%d/0/0", f.optionsCalls, f.completeCalls, f.trustCalls, f.exchangeCalls, tc.options, tc.complete)
				}
			})
		}
	}
	for _, entry := range []string{"VerifyCode", "VerifyAccountCode"} {
		t.Run(entry+"/invalid-code", func(t *testing.T) {
			f, _, _, err := runRepairIntegrationEntry(t, entry, "invalid-code")
			if err == nil || len(f.steps) != 0 {
				t.Fatal("unaccepted verification code entered the repair flow")
			}
		})
	}
}

func TestLoginWithoutRepairKeepsExistingRequestFlow(t *testing.T) {
	for _, entry := range []string{"SignIn", "SignInAccount", "VerifyCode", "VerifyAccountCode"} {
		t.Run(entry, func(t *testing.T) {
			f, _, _, err := runRepairIntegrationEntry(t, entry, "normal")
			if err != nil {
				t.Fatal(err)
			}
			if f.optionsCalls != 0 || f.completeCalls != 0 || f.exchangeCalls != 1 {
				t.Fatal("normal login unexpectedly entered repair or repeated its exchange")
			}
			wantTrust := 1
			if f.signIn() {
				wantTrust = 0
			}
			if f.trustCalls != wantTrust {
				t.Fatal("normal login changed its existing trust behavior")
			}
			for _, step := range f.steps {
				if strings.Contains(step, "/repair") {
					t.Fatal("normal login added a repair request")
				}
			}
		})
	}
}
