package apple

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestAccountAliasDetailRequiresExactActiveExistingAddress(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		ok     bool
	}{
		{"verified", 200, `{"emailAddress":"root@icloud.com","id":"original-id","active":true,"exists":true,"forwardToEmail":"primary@icloud.com"}`, true},
		{"other_address", 200, `{"emailAddress":"other@icloud.com","id":"original-id","active":true,"exists":true,"forwardToEmail":"primary@icloud.com"}`, false},
		{"other_id", 200, `{"emailAddress":"root@icloud.com","id":"other-id","active":true,"exists":true,"forwardToEmail":"primary@icloud.com"}`, false},
		{"inactive", 200, `{"emailAddress":"root@icloud.com","id":"original-id","active":false,"exists":true,"forwardToEmail":"primary@icloud.com"}`, false},
		{"absent", 200, `{"emailAddress":"root@icloud.com","id":"original-id","active":true,"exists":false,"forwardToEmail":"primary@icloud.com"}`, false},
		{"missing_exists", 200, `{"emailAddress":"root@icloud.com","id":"original-id","active":true,"forwardToEmail":"primary@icloud.com"}`, false},
		{"missing_forward", 200, `{"emailAddress":"root@icloud.com","id":"original-id","active":true,"exists":true}`, false},
		{"expired", 401, `{}`, false},
		{"unknown", 503, `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := accountClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/account/manage/email/private/original-id.em" || r.Header.Get("X-Apple-Api-Key") != "key" {
					t.Fatal("unexpected detail request")
				}
				resp := accountResp(tc.status, tc.body)
				resp.Header.Set("scnt", "rotated")
				return resp, nil
			})
			a, s, err := c.GetAccountAlias(context.Background(), AccountSession{APIKey: "key", SCNT: "scnt"}, Alias{HME: "root@icloud.com", AnonymousID: "original-id", Origin: "APPLE_ACCOUNT"})
			if (err == nil) != tc.ok || calls != 1 {
				t.Fatalf("ok=%v calls=%d err=%v", tc.ok, calls, err)
			}
			if tc.ok && (!a.IsActive || a.ForwardToEmail != "primary@icloud.com" || s.SCNT != "rotated") {
				t.Fatal("verified detail or rotated session lost")
			}
			if !tc.ok && a.HME != "" {
				t.Fatal("unverified detail was returned")
			}
			if tc.status == 401 && !errors.Is(err, ErrInvalidSession) {
				t.Fatal("expired detail session not classified")
			}
		})
	}
}
