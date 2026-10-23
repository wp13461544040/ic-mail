package httpserver

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"icloud-api/internal/autologincode"
	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

type adminAPIAutoLoginInput struct {
	Enabled  *bool  `json:"enabled"`
	Region   string `json:"region"`
	Password string `json:"password"`
	CodeURL  string `json:"code_url"`
}

type autoLoginSettingsLocker interface {
	WithAutoLoginSettingsLock(context.Context, int64, func() error) error
}

func (s *Server) withAutoLoginSettingsLock(ctx context.Context, id int64, update func() error) error {
	if service, ok := s.hmeSync.(autoLoginSettingsLocker); ok {
		return service.WithAutoLoginSettingsLock(ctx, id, update)
	}
	return update()
}

func (s *Server) withAccountAutoLoginUpdateLock(ctx context.Context, id int64, update func() error) error {
	return s.withAutoLoginSettingsLock(ctx, id, func() error {
		return s.withAccountLock(ctx, id, update)
	})
}

func (s *Server) notifyAutomaticLogin(id int64) {
	if service, ok := s.hmeSync.(interface{ NotifyAutomaticLogin(int64) }); ok {
		service.NotifyAutomaticLogin(id)
	}
}

func (s *Server) prepareAccountAutoLogin(ctx context.Context, input *adminAPIAutoLoginInput, account, previous domain.Account) (*domain.AccountAutoLoginConfig, string, error) {
	if input == nil {
		return nil, "", nil
	}
	defer func() { input.Password, input.CodeURL = "", "" }()
	if input.Enabled == nil {
		return nil, "请明确指定自动保活服务是否开启", nil
	}
	var existing domain.AccountAutoLoginConfig
	if account.ID > 0 {
		var err error
		existing, err = s.store.GetAccountAutoLoginConfig(ctx, account.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, "", err
		}
	}
	region := strings.TrimSpace(input.Region)
	if region == "" && !*input.Enabled {
		region = existing.Region
		if region == "" {
			region = "global"
		}
	}
	if region != "global" && region != "cn" {
		return nil, "请选择 Apple 账户区域", nil
	}
	icloud := domain.NormalizeMailboxType(account.MailboxType) == domain.MailboxTypeICloud
	if *input.Enabled && !icloud {
		return nil, "自动保活服务仅适用于 iCloud 主号", nil
	}
	identityUnchanged := account.ID > 0 && domain.NormalizeEmail(account.Email) == domain.NormalizeEmail(previous.Email) &&
		domain.NormalizeMailboxType(previous.MailboxType) == domain.NormalizeMailboxType(account.MailboxType)
	config := domain.AccountAutoLoginConfig{
		AccountID: account.ID, Enabled: *input.Enabled, Region: region,
		Revision: existing.Revision,
	}
	if identityUnchanged {
		config.CredentialsCiphertext = existing.CredentialsCiphertext
	}
	if !config.Enabled {
		if existing.Revision > 0 && !existing.Enabled && config.Region == existing.Region && config.CredentialsCiphertext == existing.CredentialsCiphertext {
			return nil, "", nil
		}
		return &config, "", nil
	}
	if len(input.Password) > 1024 || len(input.CodeURL) > 4096 || strings.ContainsRune(input.Password, 0) {
		return nil, "请检查 Apple 账户密码和接码链接长度", nil
	}
	codeURL := strings.TrimSpace(input.CodeURL)
	if existing.Enabled && identityUnchanged && region == existing.Region && input.Password == "" && codeURL == "" {
		return nil, "", nil // Ordinary account edits retain retry deadlines.
	}
	credentials := secure.AccountAutoLoginCredentials{Password: input.Password, CodeURL: codeURL}
	defer func() { credentials = secure.AccountAutoLoginCredentials{} }()
	if config.CredentialsCiphertext != "" && (credentials.Password == "" || credentials.CodeURL == "") {
		stored, err := s.cipher.DecryptAccountAutoLogin(account.Email, config.CredentialsCiphertext)
		if err != nil {
			return nil, "已保存的自动保活凭据读取异常，请重新填写密码和接码链接", nil
		}
		if credentials.Password == "" {
			credentials.Password = stored.Password
		}
		if credentials.CodeURL == "" {
			credentials.CodeURL = stored.CodeURL
		}
		stored = secure.AccountAutoLoginCredentials{}
	}
	if credentials.Password == "" || credentials.CodeURL == "" {
		return nil, "开启自动保活时，请填写 Apple 账户密码、双重验证接码链接和账户区域", nil
	}
	if autologincode.ValidateURL(credentials.CodeURL) != nil {
		return nil, "接码链接格式无效，请使用指向公网服务的完整 HTTP 或 HTTPS 链接", nil
	}
	ciphertext, err := s.cipher.EncryptAccountAutoLogin(account.Email, credentials)
	if err != nil {
		return nil, "", errors.New("encrypt automatic login credentials")
	}
	config.CredentialsCiphertext = ciphertext
	return &config, "", nil
}

var autoLoginPublicCode = regexp.MustCompile(`^[A-Z0-9_]{1,80}$`)

func (s *Server) adminAPIGetAccountAutoLogin(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, ok := adminAPIParseID(c)
	if !ok {
		return
	}
	if _, err := s.store.GetAccount(c.Request.Context(), id); err != nil {
		s.writeAdminAPIStoreReadError(c, err)
		return
	}
	config, err := s.store.GetAccountAutoLoginConfig(c.Request.Context(), id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		config.Region = "global"
	}
	channels := make([]gin.H, 0, 2)
	for _, channel := range []string{domain.AutoLoginChannelAppleAccount, domain.AutoLoginChannelICloudWeb} {
		state, err := s.store.GetAccountAutoLoginState(c.Request.Context(), id, channel)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.writeAdminAPIInternalError(c, err)
			return
		}
		status := "idle"
		if !config.Enabled {
			status = "disabled"
		} else {
			switch state.Status {
			case "logging_in", "waiting_code", "authenticated", "retry_wait", "needs_action":
				status = state.Status
			}
		}
		code := state.LastErrorCode
		if !autoLoginPublicCode.MatchString(code) {
			code = ""
		}
		channels = append(channels, gin.H{
			"channel": channel, "status": status, "last_error_code": code,
			"last_attempt_at": adminAPIOptionalTime(state.LastAttemptAt),
			"last_success_at": adminAPIOptionalTime(state.LastSuccessAt),
			"next_attempt_at": adminAPIOptionalTime(state.NextAttemptAt),
		})
	}
	configured := config.CredentialsCiphertext != ""
	writeAdminAPIData(c, http.StatusOK, gin.H{
		"enabled": config.Enabled, "region": config.Region,
		"password_configured": configured, "code_url_configured": configured, "channels": channels,
	})
}
