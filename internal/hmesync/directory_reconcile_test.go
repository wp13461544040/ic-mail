package hmesync

import (
	"context"
	"errors"
	"testing"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestSyncDirectoryReconcilesMissingAndPreservesOtherChannelPositive(t *testing.T) {
	for _, scenario := range []string{"success", "both-empty", "secondary-fails", "secondary-wrong-owner"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s, db, account, _, _ := directoryServiceFixture(t)
			saveDirectoryWeb(t, s, account)
			missing, err := db.CreateAlias(ctx, domain.Alias{AccountID: account.ID, Address: "phantom@icloud.com", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			onlyAccount, err := db.CreateAlias(ctx, domain.Alias{AccountID: account.ID, Address: "account-only@icloud.com", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			reads := 0
			s.client = &directoryTestClient{fakeAppleClient: &fakeAppleClient{
				validate: func(_ context.Context, web apple.Session) (apple.Session, error) { return web, nil },
				list: func(_ context.Context, web apple.Session) (apple.ListResult, apple.Session, error) {
					list := ownedDirectory(account)
					if scenario == "both-empty" {
						list.Aliases = []apple.Alias{}
					}
					return list, web, nil
				},
			}, read: func(_ context.Context, managed apple.AccountSession) (apple.ListResult, apple.AccountSession, error) {
				reads++
				if scenario == "secondary-fails" {
					return apple.ListResult{}, managed, &apple.Error{Kind: apple.ErrService, StatusCode: 503}
				}
				list := ownedDirectory(account)
				if scenario == "both-empty" {
					list.Aliases = []apple.Alias{}
					return list, managed, nil
				}
				if scenario == "secondary-wrong-owner" {
					list.SelectedForwardTo = "different@icloud.com"
					list.ForwardToEmails = nil
					list.Aliases = nil
					return list, managed, nil
				}
				list.Aliases = append(list.Aliases, apple.Alias{HME: onlyAccount.Address, AnonymousID: "account-only-id", ForwardToEmail: account.Email, IsActive: true})
				return list, managed, nil
			}}
			result, err := s.SyncAliases(ctx, account.ID)
			if err != nil || reads != 1 {
				t.Fatalf("sync err=%v reads=%d", err, reads)
			}
			if _, err := db.GetAlias(ctx, onlyAccount.ID); err != nil && scenario != "both-empty" {
				t.Fatal("positive entry in other channel removed")
			}
			_, absentErr := db.GetAlias(ctx, missing.ID)
			if scenario == "both-empty" {
				if !errors.Is(absentErr, store.ErrNotFound) || result.Summary.RemovedCount != 2 || result.Summary.Total != 0 || result.Summary.DirectoryWarning != "" {
					t.Fatalf("empty directories not reconciled: %+v err=%v", result.Summary, absentErr)
				}
			} else if scenario == "success" {
				if !errors.Is(absentErr, store.ErrNotFound) || result.Summary.RemovedCount != 1 || result.Summary.Total != 3 || result.Summary.DirectoryWarning != "" {
					t.Fatalf("missing not reconciled: %+v err=%v", result.Summary, absentErr)
				}
			} else {
				if absentErr != nil || result.Summary.RemovedCount != 0 || result.Summary.DirectoryWarning != "APPLE_DIRECTORY_RECONCILIATION_PENDING" {
					t.Fatalf("failed read removed address: %+v err=%v", result.Summary, absentErr)
				}
				if len(result.Created) != 2 || result.Created[0].APIKey == "" {
					t.Fatal("secondary failure discarded successfully imported credentials")
				}
			}
		})
	}
}
