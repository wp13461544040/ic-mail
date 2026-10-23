package domain

import "time"

const (
	AutoLoginChannelAppleAccount = "apple_account"
	AutoLoginChannelICloudWeb    = "icloud_web"
)

// AccountAutoLoginConfig is private service configuration. CredentialsCiphertext
// is an opaque encrypted envelope, never a public account response field.
type AccountAutoLoginConfig struct {
	AccountID             int64     `json:"-"`
	Enabled               bool      `json:"-"`
	Region                string    `json:"-"`
	CredentialsCiphertext string    `json:"-"`
	Revision              int64     `json:"-"`
	CreatedAt             time.Time `json:"-"`
	UpdatedAt             time.Time `json:"-"`
}

// AccountAutoLoginState tracks independent channel backoff across restarts.
type AccountAutoLoginState struct {
	AccountID      int64      `json:"-"`
	Channel        string     `json:"-"`
	ConfigRevision int64      `json:"-"`
	Status         string     `json:"-"`
	Failures       int        `json:"-"`
	LastErrorCode  string     `json:"-"`
	LastAttemptAt  *time.Time `json:"-"`
	LastSuccessAt  *time.Time `json:"-"`
	NextAttemptAt  *time.Time `json:"-"`
}
