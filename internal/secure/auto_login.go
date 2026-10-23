package secure

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"icloud-api/internal/domain"
)

const autoLoginAAD = "icloud-api/account-auto-login/v1/"

// AccountAutoLoginCredentials is only decrypted for a login attempt. Neither
// the password nor the bearer-like code URL belongs in an HTTP response.
type AccountAutoLoginCredentials struct {
	Password string `json:"password"`
	CodeURL  string `json:"code_url"`
}

func (AccountAutoLoginCredentials) String() string { return "[redacted auto-login credentials]" }

func (AccountAutoLoginCredentials) LogValue() slog.Value { return slog.StringValue("[redacted]") }

func (c *Cipher) EncryptAccountAutoLogin(email string, credentials AccountAutoLoginCredentials) (string, error) {
	email = domain.NormalizeEmail(email)
	if email == "" || credentials.Password == "" || credentials.CodeURL == "" {
		return "", errors.New("automatic login credentials incomplete")
	}
	payload, err := json.Marshal(credentials)
	if err != nil {
		return "", errors.New("encode automatic login credentials")
	}
	defer clear(payload)
	return c.encrypt("al1", autoLoginAAD+email, string(payload))
}

func (c *Cipher) DecryptAccountAutoLogin(email, ciphertext string) (AccountAutoLoginCredentials, error) {
	email = domain.NormalizeEmail(email)
	if email == "" {
		return AccountAutoLoginCredentials{}, errors.New("automatic login account required")
	}
	plaintext, err := c.decrypt("al1", autoLoginAAD+email, ciphertext)
	if err != nil {
		return AccountAutoLoginCredentials{}, errors.New("decrypt automatic login credentials")
	}
	defer func() { plaintext = "" }()
	var credentials AccountAutoLoginCredentials
	if json.Unmarshal([]byte(plaintext), &credentials) != nil || credentials.Password == "" || strings.TrimSpace(credentials.CodeURL) == "" {
		return AccountAutoLoginCredentials{}, errors.New("invalid automatic login credentials")
	}
	return credentials, nil
}
