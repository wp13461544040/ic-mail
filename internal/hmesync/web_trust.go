package hmesync

import (
	"context"
	"encoding/json"
	"errors"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

// A failed service exchange can follow successful browser trust. Retain only
// that checkpoint, or rotate the trust on an existing working Web session.
func (s *Service) preserveFailedWebTrust(ctx context.Context, id int64, expected accountIdentity, appleID string, region apple.Region, failed apple.Session) error {
	if failed.TrustToken == "" {
		return nil
	}
	failedRegion, err := normalizeRegion(failed.Region)
	if err != nil || failedRegion != region || !sameEmail(failed.AppleID, appleID) {
		return wrapError(CodeAccountMismatch, ErrAccountMismatch, err)
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), autoCreatePersistTimeout)
	defer cancel()
	return s.locker.WithAccountLock(persistCtx, id, func() error {
		account, err := s.repo.GetAccount(persistCtx, id)
		if err != nil {
			return err
		}
		if !sameIdentity(identityOf(account), expected) {
			return wrapError(CodeAccountChanged, ErrAccountChanged, nil)
		}
		record, err := s.repo.GetAppleWebSession(persistCtx, id)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		trusted := apple.Session{AppleID: failed.AppleID, Region: region, ClientID: failed.ClientID, TrustToken: failed.TrustToken}
		if err == nil {
			stored, err := s.decryptSession(record)
			if err != nil {
				return err
			}
			if !sameEmail(stored.AppleID, appleID) || stored.Region != region {
				return wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
			}
			if stored.TrustToken == failed.TrustToken {
				return nil
			}
			if record.Authenticated {
				if stored.ClientID != "" && stored.ClientID != failed.ClientID {
					return wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
				}
				trusted = stored
				trusted.TrustToken = failed.TrustToken
				if trusted.ClientID == "" {
					trusted.ClientID = failed.ClientID
				}
			} else {
				// Legacy adapters keep the independent management session here.
				trusted.Account = stored.Account
				record.LastValidatedAt = nil
			}
		} else {
			record = domain.AppleWebSession{AccountID: id, AppleID: failed.AppleID, Region: publicRegion(string(region))}
		}
		payload, err := json.Marshal(trusted)
		if err != nil {
			return err
		}
		record.Ciphertext, err = s.cipher.EncryptAppleSession(string(payload))
		if err != nil {
			return err
		}
		_, err = s.repo.UpsertAppleWebSession(persistCtx, record)
		if err == nil {
			s.NotifyAutomaticLogin(id)
		}
		return err
	})
}

// Automatic expiry invalidates service access but retains the Apple-issued
// browser trust token for the next explicit login. Explicit logout still deletes it.
// The caller holds the account publication lock.
func (s *Service) expireWebSessionPreservingTrust(ctx context.Context, id int64) error {
	record, err := s.repo.GetAppleWebSession(ctx, id)
	if err != nil {
		return err
	}
	previous, err := s.decryptSession(record)
	if err != nil || previous.TrustToken == "" {
		return s.deleteWebSessionPreservingAccount(ctx, id)
	}
	if _, supported := s.repo.(accountSessionRepository); supported {
		managed, err := s.readAccountManagementSession(ctx, id)
		if err != nil && !errors.Is(err, store.ErrNotFound) && Code(err) != CodeAccountSessionExpired {
			return err
		}
		if err == nil {
			if err := s.saveAccountManagementSession(ctx, id, *managed); err != nil {
				return err
			}
		}
	}
	trusted := apple.Session{AppleID: previous.AppleID, Region: previous.Region, ClientID: previous.ClientID, TrustToken: previous.TrustToken}
	if _, supported := s.repo.(accountSessionRepository); !supported {
		trusted.Account = previous.Account
	}
	payload, err := json.Marshal(trusted)
	if err != nil {
		return err
	}
	record.Ciphertext, err = s.cipher.EncryptAppleSession(string(payload))
	if err != nil {
		return err
	}
	record.Authenticated = false
	_, err = s.repo.UpsertAppleWebSession(ctx, record)
	if err == nil {
		s.NotifyAutomaticLogin(id)
	}
	return err
}
