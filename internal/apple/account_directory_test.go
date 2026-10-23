package apple

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func accountDirectorySession() AccountSession {
	return AccountSession{SCNT: "fixture-scnt", APIKey: "fixture-key", ExpiresAt: time.Now().Add(time.Hour)}
}

func accountDirectoryBody(active, inactive string) string {
	return fmt.Sprintf(`{"forwardToEmailAddress":"owner@example.com","privateEmailList":%s,"inactivePrivateEmailList":%s}`, active, inactive)
}

func TestListAccountAliasesReadsBothGroupsAndRotatesSession(t *testing.T) {
	requests := 0
	c := accountClient(t, func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != http.MethodGet || r.URL.String() != "https://appleid.apple.com/account/manage/email/private" {
			t.Fatal("unexpected account directory request")
		}
		if r.Header.Get("X-Apple-Api-Key") != "fixture-key" || r.Header.Get("scnt") != "fixture-scnt" ||
			r.Header.Get("Origin") != accountPortal || r.Header.Get("Referer") != accountPortal+"/" {
			t.Fatal("missing expected account request headers")
		}
		if r.Body != nil {
			body, err := io.ReadAll(r.Body)
			if err != nil || len(body) != 0 {
				t.Fatal("directory GET included a request body")
			}
		}
		response := accountResp(http.StatusOK, accountDirectoryBody(
			`[{"id":"active-id.em","emailAddress":"ACTIVE@icloud.com","active":true,"label":"Shopping","note":"fixture note"},{"id":"another-id","emailAddress":"another@icloud.com","forwardToEmail":"other@example.com"}]`,
			`[{"id":"inactive-id","emailAddress":"off@icloud.com","active":false},{"id":"inactive-no-flag","emailAddress":"off2@icloud.com","forwardToEmail":""}]`))
		response.Header.Set("scnt", "rotated-fixture")
		response.Header.Add("Set-Cookie", "directory=fixture; Secure; Path=/")
		return response, nil
	})
	list, session, err := c.ListAccountAliases(context.Background(), accountDirectorySession())
	if err != nil || requests != 1 || len(list.Aliases) != 4 {
		t.Fatalf("directory read: requests=%d aliases=%d err=%v", requests, len(list.Aliases), err)
	}
	if list.SelectedForwardTo != "owner@example.com" || !reflect.DeepEqual(list.ForwardToEmails, []string{"owner@example.com"}) {
		t.Fatal("account default forwarding target was not retained")
	}
	active, other, inactive, inactiveNoFlag := list.Aliases[0], list.Aliases[1], list.Aliases[2], list.Aliases[3]
	if active.AnonymousID != "active-id.em" || active.HME != "active@icloud.com" || !active.IsActive ||
		active.ForwardToEmail != "owner@example.com" || active.Label != "Shopping" || active.Note != "fixture note" || active.Origin != "APPLE_ACCOUNT" {
		t.Fatal("active alias fields were not preserved")
	}
	if !other.IsActive || other.AnonymousID != "another-id" || other.ForwardToEmail != "other@example.com" {
		t.Fatal("per-alias forwarding target or active group was ignored")
	}
	if inactive.IsActive || inactiveNoFlag.IsActive || inactiveNoFlag.ForwardToEmail != "owner@example.com" {
		t.Fatal("inactive membership or forwarding fallback was ignored")
	}
	if session.SCNT != "rotated-fixture" || session.UpdatedAt.IsZero() || len(session.Cookies) != 1 {
		t.Fatal("accepted account session rotation was lost")
	}
}

func TestListAccountAliasesAcceptsExplicitEmptyDirectory(t *testing.T) {
	c := accountClient(t, func(*http.Request) (*http.Response, error) {
		return accountResp(http.StatusOK, accountDirectoryBody(`[]`, `[]`)), nil
	})
	list, _, err := c.ListAccountAliases(context.Background(), accountDirectorySession())
	if err != nil || list.Aliases == nil || len(list.Aliases) != 0 || list.SelectedForwardTo != "owner@example.com" {
		t.Fatalf("explicit empty directory: err=%v", err)
	}
}

func TestListAccountAliasesIgnoresOnlySignInWithAppleRelayAddresses(t *testing.T) {
	for _, relay := range []string{"relay@privaterelay.appleid.com", "RELAY@PRIVATERELAY.APPLEID.COM"} {
		c := accountClient(t, func(*http.Request) (*http.Response, error) {
			return accountResp(http.StatusOK, accountDirectoryBody(
				`[{"id":"active-id","emailAddress":"active@icloud.com"},{"emailAddress":"`+relay+`","exists":false}]`,
				`[{"emailAddress":"`+relay+`"},{"id":"inactive-id","emailAddress":"off@icloud.com"}]`)), nil
		})
		list, _, err := c.ListAccountAliases(context.Background(), accountDirectorySession())
		if err != nil || len(list.Aliases) != 2 || list.Aliases[0].HME != "active@icloud.com" || list.Aliases[1].HME != "off@icloud.com" {
			t.Fatalf("mixed directory: count=%d err=%v", len(list.Aliases), err)
		}
	}
	for _, address := range []string{
		"relay@privaterelay.appleid.com.attacker.test", "relay@sub.privaterelay.appleid.com",
		"relay@notprivaterelay.appleid.com", "relay@example.com", "Name <relay@privaterelay.appleid.com>",
		"bad@@privaterelay.appleid.com", "", "bad..local@icloud.com",
	} {
		encoded, _ := json.Marshal(address)
		c := accountClient(t, func(*http.Request) (*http.Response, error) {
			return accountResp(http.StatusOK, accountDirectoryBody(`[{"id":"bad","emailAddress":`+string(encoded)+`}]`, `[]`)), nil
		})
		if _, _, err := c.ListAccountAliases(context.Background(), accountDirectorySession()); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("invalid or unrelated address accepted: %q", address)
		}
	}
}

func TestListAccountAliasesRejectsIncompleteOrConflictingResponses(t *testing.T) {
	entry := `{"id":"fixture-id","emailAddress":"one@icloud.com","active":true}`
	tests := []struct {
		name string
		body string
	}{
		{"html", `<html>Sign in</html>`},
		{"empty object", `{}`},
		{"null object", `null`},
		{"array root", `[]`},
		{"missing active list", `{"forwardToEmailAddress":"owner@example.com","inactivePrivateEmailList":[]}`},
		{"missing inactive list", `{"forwardToEmailAddress":"owner@example.com","privateEmailList":[]}`},
		{"null active list", accountDirectoryBody(`null`, `[]`)},
		{"null inactive list", accountDirectoryBody(`[]`, `null`)},
		{"object list", accountDirectoryBody(`{}`, `[]`)},
		{"missing forward", `{"privateEmailList":[],"inactivePrivateEmailList":[]}`},
		{"null forward", `{"forwardToEmailAddress":null,"privateEmailList":[],"inactivePrivateEmailList":[]}`},
		{"invalid forward", `{"forwardToEmailAddress":"invalid","privateEmailList":[],"inactivePrivateEmailList":[]}`},
		{"display name forward", `{"forwardToEmailAddress":"Owner <owner@example.com>","privateEmailList":[],"inactivePrivateEmailList":[]}`},
		{"null alias", accountDirectoryBody(`[null]`, `[]`)},
		{"array alias", accountDirectoryBody(`[[]]`, `[]`)},
		{"missing id", accountDirectoryBody(`[{"emailAddress":"one@icloud.com"}]`, `[]`)},
		{"number id", accountDirectoryBody(`[{"id":123,"emailAddress":"one@icloud.com"}]`, `[]`)},
		{"empty id", accountDirectoryBody(`[{"id":"","emailAddress":"one@icloud.com"}]`, `[]`)},
		{"path id", accountDirectoryBody(`[{"id":"../one","emailAddress":"one@icloud.com"}]`, `[]`)},
		{"whitespace id", accountDirectoryBody(`[{"id":" one ","emailAddress":"one@icloud.com"}]`, `[]`)},
		{"encoded id", accountDirectoryBody(`[{"id":"one%2Ftwo","emailAddress":"one@icloud.com"}]`, `[]`)},
		{"long id", accountDirectoryBody(fmt.Sprintf(`[{"id":"%s","emailAddress":"one@icloud.com"}]`, strings.Repeat("a", 1025)), `[]`)},
		{"missing address", accountDirectoryBody(`[{"id":"one"}]`, `[]`)},
		{"number address", accountDirectoryBody(`[{"id":"one","emailAddress":123}]`, `[]`)},
		{"foreign alias domain", accountDirectoryBody(`[{"id":"one","emailAddress":"one@example.com"}]`, `[]`)},
		{"invalid alias syntax", accountDirectoryBody(`[{"id":"one","emailAddress":"one@@icloud.com"}]`, `[]`)},
		{"invalid alias local part", accountDirectoryBody(`[{"id":"one","emailAddress":"a..b@icloud.com"}]`, `[]`)},
		{"string active", accountDirectoryBody(`[{"id":"one","emailAddress":"one@icloud.com","active":"true"}]`, `[]`)},
		{"null active", accountDirectoryBody(`[{"id":"one","emailAddress":"one@icloud.com","active":null}]`, `[]`)},
		{"active group conflict", accountDirectoryBody(`[{"id":"one","emailAddress":"one@icloud.com","active":false}]`, `[]`)},
		{"inactive group conflict", accountDirectoryBody(`[]`, `[`+entry+`]`)},
		{"invalid alias forward", accountDirectoryBody(`[{"id":"one","emailAddress":"one@icloud.com","forwardToEmail":"invalid"}]`, `[]`)},
		{"null alias forward", accountDirectoryBody(`[{"id":"one","emailAddress":"one@icloud.com","forwardToEmail":null}]`, `[]`)},
		{"null label", accountDirectoryBody(`[{"id":"one","emailAddress":"one@icloud.com","label":null}]`, `[]`)},
		{"object note", accountDirectoryBody(`[{"id":"one","emailAddress":"one@icloud.com","note":{}}]`, `[]`)},
		{"same entry twice", accountDirectoryBody(`[`+entry+`,`+entry+`]`, `[]`)},
		{"same id other address", accountDirectoryBody(`[`+entry+`,{"id":"fixture-id","emailAddress":"two@icloud.com"}]`, `[]`)},
		{"same address other id", accountDirectoryBody(`[`+entry+`,{"id":"other-id","emailAddress":"ONE@icloud.com"}]`, `[]`)},
		{"same id across groups", accountDirectoryBody(`[`+entry+`]`, `[{"id":"fixture-id","emailAddress":"two@icloud.com"}]`)},
		{"same address across groups", accountDirectoryBody(`[`+entry+`]`, `[{"id":"other-id","emailAddress":"ONE@icloud.com"}]`)},
		{"pagination marker", `{"forwardToEmailAddress":"owner@example.com","privateEmailList":[],"inactivePrivateEmailList":[],"nextCursor":"fixture"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			c := accountClient(t, func(*http.Request) (*http.Response, error) {
				requests++
				response := accountResp(http.StatusOK, test.body)
				response.Header.Set("scnt", "accepted-fixture")
				return response, nil
			})
			list, session, err := c.ListAccountAliases(context.Background(), accountDirectorySession())
			if !errors.Is(err, ErrInvalidResponse) || len(list.Aliases) != 0 || requests != 1 {
				t.Fatalf("invalid directory: requests=%d aliases=%d err=%v", requests, len(list.Aliases), err)
			}
			if session.SCNT != "accepted-fixture" {
				t.Fatal("accepted HTTP session checkpoint was lost after schema failure")
			}
		})
	}
}

func TestListAccountAliasesPreservesUpstreamErrorsAndCheckpoint(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		kind      error
		retryable bool
		rateLimit bool
	}{
		{"unauthenticated", 401, `{}`, ErrInvalidSession, false, false},
		{"forbidden", 403, `{}`, ErrInvalidSession, false, false},
		{"expired", 419, `{}`, ErrInvalidSession, false, false},
		{"terms", 412, `{}`, ErrTermsRequired, false, false},
		{"http throttle", 429, `{}`, ErrService, true, true},
		{"outage", 503, `{}`, ErrService, true, false},
		{"service throttle", 200, `{"errorCode":"-41015"}`, ErrService, false, true},
		{"service errors", 200, `{"serviceErrors":[{"code":"123","message":"fixture"}]}`, ErrService, false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			c := accountClient(t, func(*http.Request) (*http.Response, error) {
				requests++
				response := accountResp(test.status, test.body)
				response.Header.Set("Retry-After", "75")
				response.Header.Set("scnt", "unaccepted-fixture")
				response.Header.Add("Set-Cookie", "unaccepted=fixture; Secure; Path=/")
				return response, nil
			})
			original := accountDirectorySession()
			list, session, err := c.ListAccountAliases(context.Background(), original)
			var typed *Error
			if !errors.Is(err, test.kind) || !errors.As(err, &typed) || typed.StatusCode != test.status ||
				typed.Retryable != test.retryable || typed.RetryAfter != 75*time.Second || IsRateLimited(err) != test.rateLimit ||
				len(list.Aliases) != 0 || requests != 1 {
				t.Fatalf("upstream error metadata: requests=%d err=%v", requests, err)
			}
			if !reflect.DeepEqual(session, original) {
				t.Fatal("unaccepted response replaced the account checkpoint")
			}
		})
	}
}

func TestListAccountAliasesTransportFailureKeepsSession(t *testing.T) {
	requests := 0
	cause := errors.New("fixture disconnect")
	c := accountClient(t, func(*http.Request) (*http.Response, error) {
		requests++
		return nil, cause
	})
	original := accountDirectorySession()
	_, returned, err := c.ListAccountAliases(context.Background(), original)
	if !errors.Is(err, ErrService) || !errors.Is(err, cause) || requests != 1 || !reflect.DeepEqual(returned, original) {
		t.Fatalf("transport failure: requests=%d err=%v", requests, err)
	}
}

func TestListAccountAliasesPreflightRejectsMissingOrBackedOffSessions(t *testing.T) {
	for _, name := range []string{"missing key", "missing scnt", "rejected", "expired backoff"} {
		t.Run(name, func(t *testing.T) {
			session := accountDirectorySession()
			expected := ErrInvalidSession
			switch name {
			case "missing key":
				session.APIKey = ""
			case "missing scnt":
				session.SCNT = ""
			case "rejected":
				session.RefreshRejected = true
			case "expired backoff":
				session.ExpiresAt = time.Now().Add(-time.Minute)
				session.RefreshAfter = time.Now().Add(2 * time.Minute)
				expected = ErrService
			}
			c := accountClient(t, func(*http.Request) (*http.Response, error) {
				t.Fatal("preflight rejection made a network request")
				return nil, nil
			})
			_, returned, err := c.ListAccountAliases(context.Background(), session)
			if !errors.Is(err, expected) || !reflect.DeepEqual(returned, session) {
				t.Fatalf("session preflight: err=%v", err)
			}
			if name == "expired backoff" && (RetryDelay(err) <= time.Minute || RetryDelay(err) > 2*time.Minute) {
				t.Fatal("refresh backoff delay was lost")
			}
		})
	}
}

func TestListAccountAliasesRenewsBeforeReading(t *testing.T) {
	var paths []string
	c := accountClient(t, func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if r.Method != http.MethodGet {
			t.Fatal("account directory renewal made a write request")
		}
		switch r.URL.Path {
		case "/account/manage/gs/ws/token":
			response := accountResp(http.StatusOK, `{"timeOutInterval":15}`)
			response.Header.Set("scnt", "renewed-fixture")
			return response, nil
		case "/account/manage":
			return accountResp(http.StatusOK, `{"apiKey":"renewed-fixture-key"}`), nil
		case "/account/manage/forwardemail":
			return accountResp(http.StatusOK, `{}`), nil
		case "/account/manage/email/private":
			if r.Header.Get("scnt") != "renewed-fixture" || r.Header.Get("X-Apple-Api-Key") != "renewed-fixture-key" {
				t.Fatal("directory request did not use the renewed account checkpoint")
			}
			return accountResp(http.StatusOK, accountDirectoryBody(`[]`, `[]`)), nil
		default:
			t.Fatal("unexpected renewal path")
			return nil, nil
		}
	})
	session := accountDirectorySession()
	session.AuthenticatedAt = time.Now().Add(-10 * time.Minute)
	session.ExpiresAt = time.Now().Add(-time.Minute)
	_, returned, err := c.ListAccountAliases(context.Background(), session)
	wantPaths := []string{"/account/manage/gs/ws/token", "/account/manage", "/account/manage/forwardemail", "/account/manage/email/private"}
	if err != nil || !reflect.DeepEqual(paths, wantPaths) || returned.RefreshedAt.IsZero() || !returned.ExpiresAt.After(time.Now()) {
		t.Fatalf("renew-before-read: requests=%d err=%v", len(paths), err)
	}
}

func TestListAccountAliasesReturnsAcceptedRenewalCheckpointOnFailure(t *testing.T) {
	requests := 0
	c := accountClient(t, func(r *http.Request) (*http.Response, error) {
		requests++
		switch r.URL.Path {
		case "/account/manage/gs/ws/token":
			response := accountResp(http.StatusOK, `{"timeOutInterval":15}`)
			response.Header.Set("scnt", "accepted-renewal-fixture")
			return response, nil
		case "/account/manage":
			response := accountResp(http.StatusServiceUnavailable, `{}`)
			response.Header.Set("Retry-After", "90")
			return response, nil
		default:
			t.Fatal("directory was read after renewal failed")
			return nil, nil
		}
	})
	original := accountDirectorySession()
	original.ExpiresAt = time.Now().Add(-time.Minute)
	list, returned, err := c.ListAccountAliases(context.Background(), original)
	if !errors.Is(err, ErrService) || RetryDelay(err) != 90*time.Second || requests != 2 || len(list.Aliases) != 0 {
		t.Fatalf("renewal failure: requests=%d err=%v", requests, err)
	}
	if returned.SCNT != "accepted-renewal-fixture" || returned.APIKey != original.APIKey || returned.UpdatedAt.IsZero() {
		t.Fatal("accepted partial renewal checkpoint was lost")
	}
}
