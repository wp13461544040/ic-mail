package apple

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const skippablePhoneOptions = `{"repairType":"phone_number_requirement","phoneNumberRepair":true,"allowSkipPhoneNumberVerification":true,"requiresPassword":false,"requiresTerms":false,"privacyConsentRequired":false,"requiredSteps":["phoneNumber","phoneNumberVerification"],"currentSteps":["phoneNumber"],"remainingSteps":["phoneNumber","phoneNumberVerification"]}`

func TestPhoneRepairSkipRequiresExplicitNarrowOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"permitted phone repair", skippablePhoneOptions, true},
		{"unknown type", strings.Replace(skippablePhoneOptions, "phone_number_requirement", "other_repair", 1), false},
		{"mandatory phone", strings.Replace(skippablePhoneOptions, `"allowSkipPhoneNumberVerification":true`, `"allowSkipPhoneNumberVerification":false`, 1), false},
		{"terms", strings.Replace(skippablePhoneOptions, `"requiresTerms":false`, `"requiresTerms":true`, 1), false},
		{"missing terms flag", strings.Replace(skippablePhoneOptions, `"requiresTerms":false,`, "", 1), false},
		{"password required", strings.Replace(skippablePhoneOptions, `"requiresPassword":false`, `"requiresPassword":true`, 1), false},
		{"privacy consent", strings.Replace(skippablePhoneOptions, `"privacyConsentRequired":false`, `"privacyConsentRequired":true`, 1), false},
		{"additional step", strings.Replace(skippablePhoneOptions, `"currentSteps":["phoneNumber"]`, `"currentSteps":["phoneNumber","birthday"]`, 1), false},
		{"missing required steps", strings.Replace(skippablePhoneOptions, `"requiredSteps":["phoneNumber","phoneNumberVerification"]`, `"requiredSteps":[]`, 1), false},
		{"new consent", strings.TrimSuffix(skippablePhoneOptions, "}") + `,"requiredConsentCount":1}`, false},
		{"cross border consent", strings.TrimSuffix(skippablePhoneOptions, "}") + `,"appleCrossBorderPrivacyConsentRequired":true}`, false},
		{"malformed", `{`, false},
		{"empty", `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := maySkipPhoneRepair([]byte(tc.body)); got != tc.want {
				t.Fatalf("skip = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPhoneRepairCompletionRequiresAffirmativeReceipt(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"type":"hsa2","repairAttribute":"complete","phoneNumberRequirementGracePeriodEnded":false}`, true},
		{`{"type":"hsa2","repairAttribute":"phoneNumber","phoneNumberRequirementGracePeriodEnded":false}`, false},
		{`{"type":"hsa2","repairAttribute":"complete","phoneNumberRequirementGracePeriodEnded":true}`, false},
		{`{"type":"hsa2","repairAttribute":"complete"}`, false},
		{`{"type":"other","repairAttribute":"complete","phoneNumberRequirementGracePeriodEnded":false}`, false},
		{`{"type":"hsa2","repairAttribute":"complete","phoneNumberRequirementGracePeriodEnded":false,"remainingSteps":["terms"]}`, false},
		{`{"type":"hsa2","repairAttribute":"complete","phoneNumberRequirementGracePeriodEnded":false,"requiresTerms":true}`, false},
		{`{"type":"hsa2","repairAttribute":"complete","phoneNumberRequirementGracePeriodEnded":false,"currentSteps":["terms"]}`, false},
		{`{"type":"hsa2","repairAttribute":"complete","phoneNumberRequirementGracePeriodEnded":false,"requiresPassword":true}`, false},
		{`{"type":"hsa2","repairAttribute":"complete","phoneNumberRequirementGracePeriodEnded":false,"privacyConsentRepair":true}`, false},
		{`{"type":"hsa2","repairAttribute":"complete","phoneNumberRequirementGracePeriodEnded":false,"privacyConsentRequired":true}`, false},
		{`{}`, false},
	} {
		if got := repairIsComplete([]byte(tc.body)); got != tc.want {
			t.Fatalf("receipt %s: complete = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestLoginRepairLocationIsPinnedToCurrentWidget(t *testing.T) {
	base := "https://appleid.apple.com/widget/account/repair?widgetKey=" + accountWidget + "&rv=11&language=zh_CN_CHN"
	h := make(http.Header)
	for _, name := range []string{"scnt", "X-Apple-ID-Session-Id", "X-Apple-Repair-Session-Token", "X-Apple-OAuth-Context"} {
		h.Set(name, "synthetic-checkpoint")
	}
	for _, tc := range []struct {
		name string
		url  string
		want bool
	}{
		{"expected", base, true},
		{"plain HTTP", strings.Replace(base, "https:", "http:", 1), false},
		{"foreign host", strings.Replace(base, "appleid.apple.com", "elsewhere.example", 1), false},
		{"suffix host", strings.Replace(base, "appleid.apple.com", "appleid.apple.com.example", 1), false},
		{"port", strings.Replace(base, "appleid.apple.com", "appleid.apple.com:443", 1), false},
		{"userinfo", strings.Replace(base, "appleid.apple.com", "user@appleid.apple.com", 1), false},
		{"other path", strings.Replace(base, "/widget/account/repair", "/account/manage", 1), false},
		{"other widget", strings.Replace(base, accountWidget, defaultWidgetKey, 1), false},
		{"other domain id", strings.Replace(base, "rv=11", "rv=3", 1), false},
		{"duplicate widget", base + "&widgetKey=" + accountWidget, false},
		{"fragment", base + "#other", true},
		{"encoded fragment", base + "#%2FphoneNumber%3Fstate%3Dfixture", true},
		{"foreign host with fragment", strings.Replace(base, "appleid.apple.com", "elsewhere.example", 1) + "#other", false},
		{"other widget with fragment", strings.Replace(base, accountWidget, defaultWidgetKey, 1) + "#other", false},
		{"unknown query", base + "&next=https://elsewhere.example", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := h.Clone()
			headers.Set("Location", tc.url)
			u, ok := loginRepairLocation(responseData{status: 412, header: headers}, accountWidget)
			if ok != tc.want {
				t.Fatalf("accepted = %v, want %v", ok, tc.want)
			}
			if ok && (u.Fragment != "" || u.RawFragment != "") {
				t.Fatal("repair navigation retained a browser-only fragment")
			}
		})
	}
	h.Set("Location", base)
	for _, missing := range []string{"scnt", "X-Apple-ID-Session-Id", "X-Apple-Repair-Session-Token", "X-Apple-OAuth-Context"} {
		copy := h.Clone()
		copy.Del(missing)
		if _, ok := loginRepairLocation(responseData{status: 412, header: copy}, accountWidget); ok {
			t.Fatalf("accepted missing %s", missing)
		}
	}
	if _, ok := loginRepairLocation(responseData{status: 429, header: h}, accountWidget); ok {
		t.Fatal("rate limit accepted as repair")
	}
}

func TestLoginRepairDiagnosticExcludesResponseMaterial(t *testing.T) {
	base := "https://appleid.apple.com/widget/account/repair?widgetKey=" + accountWidget + "&rv=11"
	for _, tc := range []struct {
		name, location, classification string
	}{
		{"missing", "", "missing"},
		{"valid", base, "accepted"},
		{"malformed", "%secret", "malformed"},
		{"host", "https://secret.example/widget/account/repair", "host"},
		{"path", "https://appleid.apple.com/secret", "path"},
		{"query", base + "&secret=private-value", "query_extra"},
		{"widget", strings.Replace(base, accountWidget, "private-value", 1), "widget"},
		{"rv", strings.Replace(base, "rv=11", "rv=private-value", 1), "rv"},
		{"userinfo", "https://private-value@appleid.apple.com/widget/account/repair", "userinfo"},
		{"relative", "/widget/account/repair?secret=private-value", "relative_url"},
		{"scheme", "http://appleid.apple.com/widget/account/repair", "scheme"},
		{"fragment", base + "#private-value", "accepted"},
		{"escaped path", strings.Replace(base, "/widget/", "/%77idget/", 1), "rawpath"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := make(http.Header)
			h.Set("scnt", "private-value")
			h.Set("X-Apple-ID-Session-Id", "private-value")
			h.Set("X-Apple-Repair-Session-Token", "private-value")
			h.Set("Location", tc.location)
			r := responseData{status: 412, header: h, body: []byte(`{"securityCode":{"valid":true},"secret":"private-value"}`)}
			want := "Apple login repair gate: headers=1110 location=" + tc.classification + " code_accepted=true"
			if got := loginRepairDiagnostic(r, accountWidget); got != want {
				t.Fatalf("diagnostic contained unexpected material: %q", got)
			}
		})
	}
}

func TestRepairWidgetMigrationRedirectIsNarrowAndStripsHeaders(t *testing.T) {
	base := "https://appleid.apple.com/widget/account/repair?widgetKey=" + accountWidget + "&rv=11"
	target := strings.Replace(base, "appleid.apple.com", "account.apple.com", 1)
	for _, tc := range []struct {
		name, from, to, method string
		count                  int
		want                   bool
	}{
		{"migration", base, target + "#fixture", "GET", 1, true},
		{"other host", base, strings.Replace(target, "account.apple.com", "example.invalid", 1), "GET", 1, false},
		{"lookalike host", base, strings.Replace(target, "account.apple.com", "account.apple.com.example", 1), "GET", 1, false},
		{"other path", base, strings.Replace(target, "/widget/account/repair", "/account/manage", 1), "GET", 1, false},
		{"other query", base, target + "&next=fixture", "GET", 1, false},
		{"http", base, strings.Replace(target, "https:", "http:", 1), "GET", 1, false},
		{"userinfo", base, strings.Replace(target, "account.apple.com", "fixture@account.apple.com", 1), "GET", 1, false},
		{"port", base, strings.Replace(target, "account.apple.com", "account.apple.com:443", 1), "GET", 1, false},
		{"post", base, target, "POST", 1, false},
		{"loop", base, target, "GET", 2, false},
		{"auth route", "https://idmsa.apple.com/appleauth/auth/repair/complete", target, "GET", 1, false},
		{"management route", "https://appleid.apple.com/account/manage", "https://account.apple.com/account/manage", "GET", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fromURL, _ := url.Parse(tc.from)
			toURL, _ := url.Parse(tc.to)
			from := &http.Request{Method: tc.method, URL: fromURL, Header: make(http.Header)}
			from.Header.Set("Accept", "text/html")
			from.Header.Set("scnt", "private-checkpoint")
			from.Header.Set("Cookie", "private-checkpoint")
			to := &http.Request{Method: tc.method, URL: toURL, Header: from.Header.Clone()}
			via := make([]*http.Request, tc.count)
			for i := range via {
				via[i] = from
			}
			err := checkRepairWidgetRedirect(to, via)
			if (err == nil) != tc.want {
				t.Fatalf("redirect accepted = %v, want %v", err == nil, tc.want)
			}
			if tc.want && (to.Header.Get("scnt") != "" || to.Header.Get("Cookie") != "" || to.Header.Get("Accept") != "text/html" || to.URL.Fragment != "") {
				t.Fatal("migration leaked headers or lost public navigation state")
			}
		})
	}
}

func TestRepairWidgetParsesOnlyBoundJSON(t *testing.T) {
	boot := `<script type="application/json" id="boot_args">{"direct":{"sessionId":"widget-session","scnt":"widget-scnt","origin":"https://idmsa.apple.com","widgetKey":"` + accountWidget + `"}}</script>`
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"valid", "<html>" + boot + "</html>", true},
		{"duplicate boot", boot + boot, false},
		{"missing boot", `<script>window.boot_args={}</script>`, false},
		{"wrong type", strings.Replace(boot, "application/json", "text/javascript", 1), false},
		{"wrong origin", strings.Replace(boot, "https://idmsa.apple.com", "https://elsewhere.example", 1), false},
		{"wrong widget", strings.Replace(boot, accountWidget, defaultWidgetKey, 1), false},
		{"missing session", strings.Replace(boot, `"widget-session"`, `""`, 1), false},
		{"bad JSON", strings.Replace(boot, `{"direct":`, `invalid`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := decodeRepairBoot([]byte(tc.body), accountWidget); ok != tc.want {
				t.Fatalf("accepted = %v, want %v", ok, tc.want)
			}
		})
	}
}

func TestWebLoginRejectsRemainingAccountActionsAfterTrust(t *testing.T) {
	for _, flag := range []string{"termsUpdateNeeded", "isRepairNeeded"} {
		t.Run(flag, func(t *testing.T) {
			calls := 0
			c := accountClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				switch r.URL.Path {
				case "/appleauth/auth/verify/trusteddevice/securitycode":
					return accountResp(204, ""), nil
				case "/appleauth/auth/2sv/trust":
					resp := accountResp(204, "")
					resp.Header.Set("X-Apple-TwoSV-Trust-Token", "synthetic-trust")
					return resp, nil
				case "/setup/ws/1/accountLogin":
					return accountResp(200, `{"dsInfo":{"dsid":"42","hsaVersion":2},"hsaTrustedBrowser":true,"`+flag+`":true}`), nil
				default:
					t.Fatal("unexpected request after account action")
					return nil, nil
				}
			})
			s, err := c.VerifyCode(context.Background(), Session{AppleID: "owner@example.com", Region: RegionGlobal, SessionToken: "synthetic-session", FrameID: "synthetic-frame"}, "123456")
			if !errors.Is(err, ErrTermsRequired) || s.DSID != "" || !s.ValidatedAt.IsZero() || s.TrustToken != "synthetic-trust" || calls != 3 {
				t.Fatal("unfinished account action became an authenticated session or lost trust")
			}
		})
	}
}
