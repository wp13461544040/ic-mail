package apple

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ListAccountAliases reads the Account management directory, including its
// explicitly inactive entries. Account IDs are opaque and retained verbatim;
// they are not interchangeable with Web directory anonymous IDs or detail URLs.
// A missing entry is not evidence that an alias was deleted.
func (c *Client) ListAccountAliases(ctx context.Context, s AccountSession) (ListResult, AccountSession, error) {
	if s.APIKey == "" || s.SCNT == "" || s.RefreshRejected {
		return ListResult{}, s, ErrInvalidSession
	}
	now := time.Now()
	if s.RefreshAfter.After(now) && !now.Before(s.ExpiresAt) {
		return ListResult{}, s, &Error{Op: "account refresh backoff", Kind: ErrService,
			StatusCode: http.StatusServiceUnavailable, Retryable: true, RetryAfter: s.RefreshAfter.Sub(now)}
	}
	if s.NeedsRefresh(now) || !now.Before(s.ExpiresAt) {
		var err error
		s, err = c.RefreshAccountSession(ctx, s)
		if err != nil {
			return ListResult{}, s, err
		}
	}
	r, err := c.accountManagementCall(ctx, &s, http.MethodGet, "/account/manage/email/private", nil)
	if err != nil {
		return ListResult{}, s, err
	}
	list, err := decodeAccountDirectory(r.body)
	if err != nil {
		return ListResult{}, s, r.operationError("account directory response", ErrInvalidResponse, err)
	}
	return list, s, nil
}

func decodeAccountDirectory(body []byte) (ListResult, error) {
	fields, err := decodeJSONObject(body, "account directory")
	if err != nil {
		return ListResult{}, err
	}
	if containsPaginationMarker(body) {
		return ListResult{}, errors.New("unexpected account directory pagination")
	}
	forward, err := decodeJSONString(fields["forwardToEmailAddress"], "forwardToEmailAddress")
	if err != nil {
		return ListResult{}, err
	}
	forward, err = normalizeHMEAddress(forward)
	if err != nil {
		return ListResult{}, errors.New("invalid account forwarding address")
	}
	list := ListResult{Aliases: []Alias{}, SelectedForwardTo: forward, ForwardToEmails: []string{forward}}
	seenIDs := make(map[string]struct{})
	seenAddresses := make(map[string]struct{})
	for _, group := range []struct {
		name   string
		active bool
	}{{"privateEmailList", true}, {"inactivePrivateEmailList", false}} {
		raw, present := fields[group.name]
		trimmed := bytes.TrimSpace(raw)
		if !present || len(trimmed) == 0 || trimmed[0] != '[' {
			return ListResult{}, fmt.Errorf("%s must be an explicit array", group.name)
		}
		var entries []json.RawMessage
		if json.Unmarshal(raw, &entries) != nil {
			return ListResult{}, fmt.Errorf("%s must be an array", group.name)
		}
		for index, entry := range entries {
			// Apple mixes Sign in with Apple relay addresses into this directory.
			// They are not manageable iCloud HME aliases and must not poison it.
			var identity struct {
				EmailAddress string `json:"emailAddress"`
			}
			if json.Unmarshal(entry, &identity) == nil {
				address, addressErr := normalizeHMEAddress(identity.EmailAddress)
				if addressErr == nil && strings.HasSuffix(address, "@privaterelay.appleid.com") {
					continue
				}
			}
			alias, err := decodeAccountDirectoryAlias(entry, forward, group.active)
			if err != nil {
				return ListResult{}, fmt.Errorf("%s[%d]: %w", group.name, index, err)
			}
			if _, duplicate := seenIDs[alias.AnonymousID]; duplicate {
				return ListResult{}, errors.New("duplicate account directory ID")
			}
			if _, duplicate := seenAddresses[alias.HME]; duplicate {
				return ListResult{}, errors.New("duplicate account directory address")
			}
			seenIDs[alias.AnonymousID] = struct{}{}
			seenAddresses[alias.HME] = struct{}{}
			list.Aliases = append(list.Aliases, alias)
		}
	}
	return list, nil
}

func decodeAccountDirectoryAlias(body []byte, forward string, active bool) (Alias, error) {
	fields, err := decodeJSONObject(body, "account alias")
	if err != nil {
		return Alias{}, err
	}
	id, err := decodeJSONString(fields["id"], "id")
	if err != nil || !validAccountDirectoryID(id) {
		return Alias{}, errors.New("invalid account directory ID")
	}
	address, err := decodeJSONString(fields["emailAddress"], "emailAddress")
	if err != nil || !validAccountAliasAddress(address) {
		return Alias{}, errors.New("invalid account alias address")
	}
	address, err = normalizeHMEAddress(address)
	if err != nil {
		return Alias{}, errors.New("invalid account alias address")
	}
	if raw, present := fields["active"]; present {
		trimmed := bytes.TrimSpace(raw)
		if !bytes.Equal(trimmed, []byte("true")) && !bytes.Equal(trimmed, []byte("false")) {
			return Alias{}, errors.New("active must be a boolean")
		}
		if bytes.Equal(trimmed, []byte("true")) != active {
			return Alias{}, errors.New("active conflicts with account directory group")
		}
	}
	alias := Alias{Origin: "APPLE_ACCOUNT", AnonymousID: id, HME: address, IsActive: active, ForwardToEmail: forward}
	for _, field := range []struct {
		name   string
		target *string
	}{{"label", &alias.Label}, {"note", &alias.Note}, {"forwardToEmail", &alias.ForwardToEmail}} {
		if raw, present := fields[field.name]; present {
			trimmed := bytes.TrimSpace(raw)
			if len(trimmed) == 0 || trimmed[0] != '"' || json.Unmarshal(raw, field.target) != nil {
				return Alias{}, fmt.Errorf("%s must be a string", field.name)
			}
		}
	}
	// The per-alias forwarding target may differ from the account default.
	if alias.ForwardToEmail == "" {
		alias.ForwardToEmail = forward
	} else if alias.ForwardToEmail, err = normalizeHMEAddress(alias.ForwardToEmail); err != nil {
		return Alias{}, errors.New("invalid alias forwarding address")
	}
	return alias, nil
}

func validAccountDirectoryID(value string) bool {
	if len(value) == 0 || len(value) > 1024 || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.') {
			return false
		}
	}
	return true
}
