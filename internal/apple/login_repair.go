package apple

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// Repair is a separate widget session. Its tokens must never replace IDMSA's
// accepted authentication checkpoint before /repair/complete succeeds.
func loginRepairLocation(response responseData, widget string) (*url.URL, bool) {
	if response.status != http.StatusPreconditionFailed || (widget != accountWidget && widget != defaultWidgetKey) {
		return nil, false
	}
	for _, name := range []string{"scnt", "X-Apple-ID-Session-Id", "X-Apple-Repair-Session-Token", "X-Apple-OAuth-Context"} {
		if strings.TrimSpace(response.header.Get(name)) == "" {
			return nil, false
		}
	}
	u, err := url.Parse(response.header.Get("Location"))
	if err != nil || u.Scheme != "https" || u.Host != "appleid.apple.com" || u.User != nil || u.Path != "/widget/account/repair" || u.RawPath != "" {
		return nil, false
	}
	// Apple may include browser navigation state after '#'. It is not part
	// of the HTTP target; keep it out of both the request and session state.
	u.Fragment, u.RawFragment = "", ""
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q["widgetKey"]) != 1 || q.Get("widgetKey") != widget || len(q["rv"]) != 1 {
		return nil, false
	}
	rv := "3"
	if widget == accountWidget {
		rv = "11"
	}
	if q.Get("rv") != rv {
		return nil, false
	}
	for key := range q {
		if key != "widgetKey" && key != "rv" && key != "language" && key != "v" {
			return nil, false
		}
	}
	return u, true
}

func repairCodeWasAccepted(response responseData) bool {
	var body struct {
		SecurityCode struct {
			Valid bool `json:"valid"`
		} `json:"securityCode"`
	}
	return json.Unmarshal(response.body, &body) == nil && body.SecurityCode.Valid
}

// The legacy public widget now migrates to account.apple.com. Permit that
// single bodyless navigation only; authentication and API redirects stay errors.
func checkRepairWidgetRedirect(next *http.Request, via []*http.Request) error {
	if len(via) != 1 || via[0].Method != http.MethodGet || next.Method != http.MethodGet {
		return http.ErrUseLastResponse
	}
	from, to := via[0].URL, next.URL
	if from.Scheme != "https" || from.Host != "appleid.apple.com" || from.User != nil || from.Path != "/widget/account/repair" || from.RawPath != "" ||
		to.Scheme != "https" || to.Host != "account.apple.com" || to.User != nil || to.Path != from.Path || to.RawPath != "" {
		return http.ErrUseLastResponse
	}
	fromQuery, fromErr := url.ParseQuery(from.RawQuery)
	toQuery, toErr := url.ParseQuery(to.RawQuery)
	if fromErr != nil || toErr != nil || fromQuery.Encode() != toQuery.Encode() {
		return http.ErrUseLastResponse
	}
	to.Fragment, to.RawFragment = "", ""
	// Only public navigation headers cross hosts. The client's isolated jar
	// supplies cookies according to the destination's domain and path.
	next.Header = make(http.Header)
	for _, name := range []string{"Accept", "User-Agent", "Referer"} {
		if value := via[0].Header.Get(name); value != "" {
			next.Header.Set(name, value)
		}
	}
	return nil
}

// Only fixed categories and presence flags may reach the operation log. Never
// include a Location, header value, query key supplied by Apple, or body text.
func loginRepairDiagnostic(response responseData, widget string) string {
	flags := ""
	for _, name := range []string{"scnt", "X-Apple-ID-Session-Id", "X-Apple-Repair-Session-Token", "X-Apple-OAuth-Context"} {
		if strings.TrimSpace(response.header.Get(name)) == "" {
			flags += "0"
		} else {
			flags += "1"
		}
	}
	location := "accepted"
	u, err := url.Parse(response.header.Get("Location"))
	switch {
	case response.header.Get("Location") == "":
		location = "missing"
	case err != nil:
		location = "malformed"
	case u.Scheme == "" && u.Host == "":
		location = "relative_url"
	case u.Scheme != "https":
		location = "scheme"
	case u.User != nil:
		location = "userinfo"
	case u.RawPath != "":
		location = "rawpath"
	case u.Host != "appleid.apple.com":
		location = "host"
	case u.Path != "/widget/account/repair":
		location = "path"
	default:
		q, parseErr := url.ParseQuery(u.RawQuery)
		rv := "3"
		if widget == accountWidget {
			rv = "11"
		}
		switch {
		case parseErr != nil:
			location = "query_malformed"
		case len(q["widgetKey"]) != 1 || q.Get("widgetKey") != widget:
			location = "widget"
		case len(q["rv"]) != 1 || q.Get("rv") != rv:
			location = "rv"
		default:
			for key := range q {
				if key != "widgetKey" && key != "rv" && key != "language" && key != "v" {
					location = "query_extra"
				}
			}
		}
	}
	return fmt.Sprintf("Apple login repair gate: headers=%s location=%s code_accepted=%t", flags, location, repairCodeWasAccepted(response))
}

type repairBoot struct {
	Direct struct {
		SessionID string `json:"sessionId"`
		SCNT      string `json:"scnt"`
		Origin    string `json:"origin"`
		WidgetKey string `json:"widgetKey"`
	} `json:"direct"`
}

func decodeRepairBoot(body []byte, widget string) (repairBoot, bool) {
	z := html.NewTokenizer(bytes.NewReader(body))
	var boot repairBoot
	found := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			return boot, found && z.Err() == io.EOF
		case html.StartTagToken:
			token := z.Token()
			if token.Data != "script" {
				continue
			}
			id, mime := "", ""
			for _, attr := range token.Attr {
				if attr.Key == "id" {
					id = attr.Val
				}
				if attr.Key == "type" {
					mime = attr.Val
				}
			}
			if id != "boot_args" || mime != "application/json" {
				continue
			}
			if found || z.Next() != html.TextToken || json.Unmarshal(z.Text(), &boot) != nil {
				return repairBoot{}, false
			}
			if boot.Direct.SessionID == "" || boot.Direct.SCNT == "" || boot.Direct.Origin != "https://idmsa.apple.com" || boot.Direct.WidgetKey != widget {
				return repairBoot{}, false
			}
			found = true
		}
	}
}

type phoneRepairOptions struct {
	RepairType                       string   `json:"repairType"`
	PhoneNumberRepair                bool     `json:"phoneNumberRepair"`
	AllowSkipPhoneNumberVerification bool     `json:"allowSkipPhoneNumberVerification"`
	RequiredSteps                    []string `json:"requiredSteps"`
	CurrentSteps                     []string `json:"currentSteps"`
	RemainingSteps                   []string `json:"remainingSteps"`
	RequiresPassword                 bool     `json:"requiresPassword"`
	RequiresTerms                    *bool    `json:"requiresTerms"`
	PrivacyConsentRepair             bool     `json:"privacyConsentRepair"`
	PrivacyConsentRequired           bool     `json:"privacyConsentRequired"`
	CountrySpecificConsentRequired   bool     `json:"countrySpecificPrivacyConsentRequired"`
	CrossBorderConsentRequired       bool     `json:"appleCrossBorderPrivacyConsentRequired"`
	GCBDConsentRequired              bool     `json:"gcbdCrossBorderPrivacyConsentRequired"`
	ADIConsentRequired               bool     `json:"adiToAspPrivacyConsentRequired"`
	RequiredConsentCount             int      `json:"requiredConsentCount"`
}

func (options phoneRepairOptions) hasOtherActions() bool {
	return options.RequiresPassword || options.RequiresTerms != nil && *options.RequiresTerms || options.PrivacyConsentRepair || options.PrivacyConsentRequired || options.CountrySpecificConsentRequired || options.CrossBorderConsentRequired || options.GCBDConsentRequired || options.ADIConsentRequired || options.RequiredConsentCount != 0
}

func maySkipPhoneRepair(body []byte) bool {
	var options phoneRepairOptions
	if json.Unmarshal(body, &options) != nil || options.RepairType != "phone_number_requirement" || !options.PhoneNumberRepair || !options.AllowSkipPhoneNumberVerification || options.RequiresTerms == nil || options.hasOtherActions() {
		return false
	}
	phoneRequired := false
	for _, step := range options.RequiredSteps {
		phoneRequired = phoneRequired || step == "phoneNumber"
	}
	for _, steps := range [][]string{options.RequiredSteps, options.CurrentSteps, options.RemainingSteps} {
		for _, step := range steps {
			if step != "phoneNumber" && step != "phoneNumberVerification" {
				return false
			}
		}
	}
	return phoneRequired
}

func repairIsComplete(body []byte) bool {
	var result struct {
		phoneRepairOptions
		Type       string `json:"type"`
		Attribute  string `json:"repairAttribute"`
		GraceEnded *bool  `json:"phoneNumberRequirementGracePeriodEnded"`
	}
	return json.Unmarshal(body, &result) == nil && result.Type == "hsa2" && result.Attribute == "complete" && result.GraceEnded != nil && !*result.GraceEnded && len(result.RequiredSteps) == 0 && len(result.CurrentSteps) == 0 && len(result.RemainingSteps) == 0 && !result.hasOtherActions()
}

func (c *Client) repairPhoneRequirement(ctx context.Context, auth AccountSession, challenge responseData, authHeaders http.Header) (AccountSession, responseData, error) {
	widget := authHeaders.Get("X-Apple-Widget-Key")
	u, ok := loginRepairLocation(challenge, widget)
	if !ok {
		return auth, responseData{}, challenge.operationError(loginRepairDiagnostic(challenge, widget), ErrTermsRequired, nil)
	}
	// Copy IDMSA's accepted headers, not the earlier request's headers.
	auth.SCNT = challenge.header.Get("scnt")
	auth.SessionID = challenge.header.Get("X-Apple-ID-Session-Id")
	authHeaders = authHeaders.Clone()
	authHeaders.Set("scnt", auth.SCNT)
	authHeaders.Set("X-Apple-ID-Session-Id", auth.SessionID)
	if value := challenge.header.Get("X-Apple-Auth-Attributes"); value != "" {
		auth.AuthAttributes = value
		authHeaders.Set("X-Apple-Auth-Attributes", value)
	}
	q := u.Query()
	if q.Get("v") == "" {
		q.Set("v", "0.3.4")
	}
	u.RawQuery = q.Encode()
	widgetSession := AccountSession{Cookies: append([]PersistentCookie(nil), auth.Cookies...)}
	h := make(http.Header)
	h.Set("Accept", "text/html,application/xhtml+xml")
	h.Set("User-Agent", authHeaders.Get("User-Agent"))
	h.Set("Referer", "https://idmsa.apple.com/")
	r, err := c.accountCall(ctx, &widgetSession, http.MethodGet, u.String(), nil, h, false)
	if err != nil {
		return auth, r, err
	}
	boot, ok := decodeRepairBoot(r.body, widget)
	if !ok {
		return auth, r, r.operationError("decode Apple repair widget", ErrInvalidResponse, nil)
	}
	if (r.header.Get("scnt") != "" && r.header.Get("scnt") != boot.Direct.SCNT) ||
		(r.header.Get("X-Apple-ID-Session-Id") != "" && r.header.Get("X-Apple-ID-Session-Id") != boot.Direct.SessionID) {
		return auth, r, r.operationError("Apple repair widget checkpoint mismatch", ErrInvalidResponse, nil)
	}
	widgetSession.SCNT, widgetSession.SessionID = boot.Direct.SCNT, boot.Direct.SessionID
	widgetSession.SessionToken = challenge.header.Get("X-Apple-Repair-Session-Token")
	options := func(skip string) (responseData, error) {
		h := make(http.Header)
		h.Set("Accept", "application/json, text/plain, */*")
		h.Set("Content-Type", "application/json")
		h.Set("User-Agent", authHeaders.Get("User-Agent"))
		h.Set("Referer", accountManage+"/")
		h.Set("X-Apple-Widget-Key", widget)
		h.Set("X-Apple-I-FD-Client-Info", authHeaders.Get("X-Apple-I-FD-Client-Info"))
		h.Set("X-Apple-I-TimeZone", "Asia/Shanghai")
		h.Set("X-Apple-Privacy-Consent", "true")
		h.Set("X-Apple-ID-Session-Id", widgetSession.SessionID)
		h.Set("scnt", widgetSession.SCNT)
		h.Set("X-Apple-Session-Token", widgetSession.SessionToken)
		h.Set("X-Apple-OAuth-Context", challenge.header.Get("X-Apple-OAuth-Context"))
		h.Set("X-Apple-Skip-Repair-Attributes", skip)
		if widget == defaultWidgetKey {
			h.Set("X-Apple-Offer-Security-Upgrade", "1")
		}
		return c.accountCall(ctx, &widgetSession, http.MethodGet, accountManage+"/account/manage/repair/options", nil, h, false)
	}
	r, err = options("[]")
	if err != nil {
		return auth, r, err
	}
	if !maySkipPhoneRepair(r.body) {
		return auth, r, r.operationError("Apple repair requires account action", ErrTermsRequired, nil)
	}
	if r.header.Get("scnt") == "" || r.header.Get("X-Apple-Session-Token") == "" {
		return auth, r, r.operationError("Apple repair options checkpoint", ErrInvalidResponse, nil)
	}
	r, err = options(`["phoneNumber"]`)
	if err != nil {
		return auth, r, err
	}
	if !repairIsComplete(r.body) {
		return auth, r, r.operationError("Apple repair remains incomplete", ErrTermsRequired, nil)
	}
	if r.header.Get("X-Apple-Session-Token") == "" {
		return auth, r, r.operationError("Apple repair completion checkpoint", ErrInvalidResponse, nil)
	}
	// Complete against IDMSA with its original SCNT/session ID and the latest
	// widget token. Never persist widget OAuth or repair tokens as login state.
	auth.Cookies = widgetSession.Cookies
	authHeaders.Set("X-Apple-Repair-Session-Token", widgetSession.SessionToken)
	r, err = c.accountCall(ctx, &auth, http.MethodPost, accountAuth+"/repair/complete", nil, authHeaders, false)
	if err == nil && r.status != http.StatusNoContent {
		err = r.operationError("complete Apple phone repair", ErrInvalidResponse, nil)
	}
	return auth, r, err
}

func (op *operation) repairPhoneRequirement(ctx context.Context, challenge responseData) error {
	s := op.session
	auth := AccountSession{SCNT: s.SCNT, SessionID: s.SessionID, SessionToken: s.SessionToken, TrustToken: s.TrustToken, AuthAttributes: s.AuthAttributes, Cookies: op.jar.Export()}
	repaired, response, err := op.owner.repairPhoneRequirement(ctx, auth, challenge, op.authHeaders())
	if err != nil {
		return err
	}
	jar, err := NewPersistentJar(repaired.Cookies)
	if err != nil {
		return err
	}
	op.jar, op.http.Jar = jar, jar
	op.capture(response.header)
	return nil
}
