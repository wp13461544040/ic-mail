package hmesync

import (
	"context"
	"errors"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

type reviewCandidateClient struct {
	*pendingChannelClient
	read func(context.Context, apple.AccountSession) (apple.ListResult, apple.AccountSession, error)
}

type failingDirectoryReconcileStore struct{ *store.Store }

func (f failingDirectoryReconcileStore) ReconcileAppleDirectory(context.Context, domain.Account, time.Time, []string) (int, error) {
	return 0, context.Canceled
}

func TestDirectoryReconciliationFailureKeepsImportedCredentials(t *testing.T) {
	s, db, account, _, _ := directoryServiceFixture(t)
	saveDirectoryWeb(t, s, account)
	client := s.client.(*accountTestClient)
	client.validate = func(_ context.Context, web apple.Session) (apple.Session, error) { return web, nil }
	client.list = func(_ context.Context, web apple.Session) (apple.ListResult, apple.Session, error) {
		return ownedDirectory(account), web, nil
	}
	s.repo = failingDirectoryReconcileStore{db}
	result, err := s.SyncAliases(context.Background(), account.ID)
	if err != nil || len(result.Created) != 2 || result.Summary.DirectoryWarning != "APPLE_DIRECTORY_RECONCILIATION_PENDING" {
		t.Fatalf("import result lost after reconciliation failure: created=%d err=%v", len(result.Created), err)
	}
	for _, created := range result.Created {
		if created.APIKey == "" {
			t.Fatal("imported credential lost")
		}
	}
}

func (c *reviewCandidateClient) ListAccountAliases(ctx context.Context, session apple.AccountSession) (apple.ListResult, apple.AccountSession, error) {
	return c.read(ctx, session)
}

func TestStaleAccountCandidateReviewDoesNotReplayCompletion(t *testing.T) {
	for _, scenario := range []string{"absent", "account-present", "read-fails", "young", "wrong-identity"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s, db, account := independentAccountFixture(t, true)
			cipher := s.cipher.(*secure.Cipher)
			db.ConfigureAliasCredentialFactory(func(id, version int64) (domain.AliasCredentialMaterial, error) {
				_, material, err := secure.NewAliasCredentialMaterial(cipher, id, version)
				return material, err
			})
			db.ConfigureAliasCredentialRevealFactory(func(id int64, ciphertext string) (string, error) {
				credentials, err := cipher.DecryptAliasCredentials(id, ciphertext)
				return credentials.APIKey, err
			})
			now := time.Now().UTC()
			managed := apple.AccountSession{AppleID: account.Email, Region: apple.RegionGlobal, APIKey: "fixture", SCNT: "fixture", ExpiresAt: now.Add(time.Hour)}
			record, err := s.saveSession(ctx, account.ID, apple.Session{AppleID: account.Email, Region: apple.RegionGlobal, DSID: "fixture-dsid", Account: &managed})
			if err != nil {
				t.Fatal(err)
			}
			pending, _, err := db.CreateAliasWithPendingAPIKey(ctx, record, domain.Alias{AccountID: account.ID, Address: "old-candidate@icloud.com", CreationChannel: "apple_account", APIKeyHash: []byte("fixture"), APIKeyPrefix: "fixture"}, "original-encrypted-key")
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "young" {
				now = now.Add(25 * time.Hour)
				managed.ExpiresAt = now.Add(time.Hour)
			}
			client := &reviewCandidateClient{pendingChannelClient: &pendingChannelClient{accountTestClient: s.client.(*accountTestClient)}}
			directory := []apple.Alias{}
			created := apple.Alias{HME: "fresh-candidate@icloud.com", IsActive: true, ForwardToEmail: account.Email, Origin: "APPLE_ACCOUNT"}
			creates, completes, reads := 0, 0, 0
			client.validate = func(_ context.Context, web apple.Session) (apple.Session, error) {
				web.Account = &managed
				return web, nil
			}
			client.list = func(_ context.Context, web apple.Session) (apple.ListResult, apple.Session, error) {
				return apple.ListResult{SelectedForwardTo: account.Email, Aliases: directory}, web, nil
			}
			client.read = func(_ context.Context, acc apple.AccountSession) (apple.ListResult, apple.AccountSession, error) {
				reads++
				if scenario == "read-fails" {
					return apple.ListResult{}, acc, &apple.Error{Kind: apple.ErrService, StatusCode: 503}
				}
				if scenario == "wrong-identity" {
					acc.AppleID = "other@icloud.com"
				}
				items := []apple.Alias{}
				if scenario == "account-present" {
					items = append(items, apple.Alias{HME: pending.Address, IsActive: true, ForwardToEmail: account.Email})
				}
				return apple.ListResult{SelectedForwardTo: account.Email, Aliases: items}, acc, nil
			}
			client.accountCreate = func(_ context.Context, acc apple.AccountSession) (apple.Alias, apple.AccountSession, error) {
				creates++
				directory = append(directory, created)
				return created, acc, nil
			}
			client.complete = func(_ context.Context, acc apple.AccountSession, _, _, _ string) (apple.Alias, apple.AccountSession, error) {
				completes++
				return apple.Alias{}, acc, &apple.Error{Kind: apple.ErrService, StatusCode: 503}
			}
			s = newTestService(t, db, client, &fakeLocker{}, func() time.Time { return now })
			s.autoCreateConfirmationDelays = nil
			got, err := s.ProbeAliasWithChannel(ctx, account.ID, "apple_account")
			switch scenario {
			case "absent":
				if err != nil || creates != 1 || completes != 0 || got.Address != created.HME {
					t.Fatalf("fresh create=%d complete=%d err=%v", creates, completes, err)
				}
				reviews, e := db.ListReviewPendingAliases(ctx, account.ID)
				if e != nil || len(reviews) != 1 || reviews[0].ID != pending.ID || reviews[0].APIKeyCiphertext != "original-encrypted-key" {
					t.Fatal("lost original review")
				}
				// A later positive directory observation restores the same ID/key.
				directory = append(directory, apple.Alias{HME: pending.Address, IsActive: true, ForwardToEmail: account.Email})
				if _, e := s.SyncAliases(ctx, account.ID); e != nil {
					t.Fatal(e)
				}
				original, e := db.GetAlias(ctx, pending.ID)
				if e != nil || !original.Enabled {
					t.Fatal("original review not recovered")
				}
				if reviews, e := db.ListReviewPendingAliases(ctx, account.ID); e != nil || len(reviews) != 0 {
					t.Fatal("review not cleared")
				}
			case "account-present":
				if err != nil || got.ID != pending.ID || creates != 0 || completes != 0 {
					t.Fatalf("positive directory not restored: %v", err)
				}
			case "read-fails", "wrong-identity":
				if err == nil || creates != 0 || completes != 0 {
					t.Fatal("failed read replayed completion or creation")
				}
			case "young":
				if !errors.Is(err, ErrAliasConfirmationPending) || reads != 0 || creates != 0 {
					t.Fatal("young candidate quarantined")
				}
			}
		})
	}
}
