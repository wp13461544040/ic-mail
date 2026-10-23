package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestReviewCandidatePreservesCredentialsAndConfirmsOriginal(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Review candidate", "review-candidate@icloud.com")
	session := domain.AppleWebSession{AccountID: account.ID, AppleID: account.Email,
		Ciphertext: "as1.review", Authenticated: true}
	created, _, err := db.CreateAliasWithPendingAPIKey(ctx, session, domain.Alias{
		AccountID: account.ID, Address: "review-original@icloud.com", CreationChannel: "apple_account",
		APIKeyHash: []byte("original-key"), APIKeyPrefix: "original",
	}, "original-encrypted-key")
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().UTC().Add(time.Second)
	for range 2 {
		if err := db.ReviewStaleAccountAliasCandidate(ctx, account.ID, created.ID, created.Address, cutoff); err != nil {
			t.Fatalf("review (including idempotent repeat): %v", err)
		}
	}
	for _, channel := range []string{"apple_account", "icloud_web"} {
		if _, err := db.GetPendingAutoAliasConfirmationForChannel(ctx, account.ID, channel); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("review still gates %s: %v", channel, err)
		}
	}
	if _, err := db.GetPendingAutoAliasConfirmation(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("review still gates global pending lookup: %v", err)
	}
	reviews, err := db.ListReviewPendingAliases(ctx, account.ID)
	if err != nil || len(reviews) != 1 || reviews[0].ID != created.ID || reviews[0].Enabled ||
		reviews[0].APIKeyCiphertext != "original-encrypted-key" || reviews[0].CreationChannel != "apple_account" ||
		reviews[0].LastSyncError != domain.AppleAliasConfirmationPending {
		t.Fatalf("review lost original pending state: count=%d err=%v", len(reviews), err)
	}
	if count, err := db.CountAliasCreationOccupancy(ctx, account.ID); err != nil || count != 1 {
		t.Fatalf("review occupancy=%d err=%v", count, err)
	}
	if keys, err := db.ListPendingAliasAPIKeysByAccount(ctx, account.ID); err != nil || len(keys) != 0 {
		t.Fatalf("review credentials became issuable: count=%d err=%v", len(keys), err)
	}
	confirmed, _, err := db.ConfirmPendingAutoAlias(ctx, session, created.ID)
	if err != nil || confirmed.ID != created.ID || !confirmed.Enabled {
		t.Fatalf("confirm original review: id=%d err=%v", confirmed.ID, err)
	}
	if reviews, err := db.ListReviewPendingAliases(ctx, account.ID); err != nil || len(reviews) != 0 {
		t.Fatalf("confirmation retained review: count=%d err=%v", len(reviews), err)
	}
	keys, err := db.ListPendingAliasAPIKeysByAccount(ctx, account.ID)
	if err != nil || len(keys) != 1 || keys[0].APIKeyCiphertext != "original-encrypted-key" {
		t.Fatalf("confirmation changed original key: count=%d err=%v", len(keys), err)
	}
}

func TestReviewCandidateCASRejectsChangedOrIneligibleState(t *testing.T) {
	for _, scenario := range []string{"wrong-address", "wrong-account", "newer", "web", "remote-id", "confirmed"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			db := openTestStore(t)
			account := createAccount(t, ctx, db, "Review CAS", "review-cas@icloud.com")
			session := domain.AppleWebSession{AccountID: account.ID, AppleID: account.Email,
				Ciphertext: "as1.review-cas", Authenticated: true}
			channel := "apple_account"
			if scenario == "web" {
				channel = "icloud_web"
			}
			created, _, err := db.CreateAliasWithPendingAPIKey(ctx, session, domain.Alias{
				AccountID: account.ID, Address: "review-cas-alias@icloud.com", CreationChannel: channel,
				APIKeyHash: []byte("original-key"), APIKeyPrefix: "original",
			}, "encrypted-key")
			if err != nil {
				t.Fatal(err)
			}
			id, address, cutoff := account.ID, created.Address, time.Now().UTC().Add(time.Second)
			switch scenario {
			case "wrong-address":
				address = "different@icloud.com"
			case "wrong-account":
				id = createAccount(t, ctx, db, "Other", "review-other@icloud.com").ID
			case "newer":
				cutoff = created.CreatedAt.Add(-time.Hour)
			case "remote-id":
				if err := db.SaveAliasCreationCandidateDetail(ctx, account.ID, created.ID, channel, "completed-id"); err != nil {
					t.Fatal(err)
				}
			case "confirmed":
				if _, _, err := db.ConfirmPendingAutoAlias(ctx, session, created.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.ReviewStaleAccountAliasCandidate(ctx, id, created.ID, address, cutoff); !errors.Is(err, store.ErrAliasConfirmationPending) {
				t.Fatalf("ineligible review error=%v", err)
			}
			if _, err := db.GetAlias(ctx, created.ID); err != nil {
				t.Fatalf("CAS rejection removed candidate: %v", err)
			}
			if reviews, err := db.ListReviewPendingAliases(ctx, account.ID); err != nil || len(reviews) != 0 {
				t.Fatalf("CAS rejection added review: count=%d err=%v", len(reviews), err)
			}
		})
	}
}

func TestConfirmReviewedAliasFromDirectoryPreservesOriginalCredentials(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Directory review", "directory-review@icloud.com")
	created, _, err := db.CreateAliasWithPendingAPIKey(ctx, domain.AppleWebSession{
		AccountID: account.ID, AppleID: account.Email, Ciphertext: "as1.original", Authenticated: true,
	}, domain.Alias{AccountID: account.ID, Address: "directory-review-alias@icloud.com",
		CreationChannel: "apple_account", APIKeyHash: []byte("original-key"), APIKeyPrefix: "original",
	}, "original-encrypted-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReviewStaleAccountAliasCandidate(ctx, account.ID, created.ID, created.Address, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ConfirmReviewedAliasFromDirectory(ctx, account.ID, "different@icloud.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("non-exact directory confirmation: %v", err)
	}
	confirmed, err := db.ConfirmReviewedAliasFromDirectory(ctx, account.ID, created.Address)
	if err != nil || confirmed.ID != created.ID || !confirmed.Enabled || confirmed.LastSyncError != "" {
		t.Fatalf("directory confirmation: id=%d err=%v", confirmed.ID, err)
	}
	if reviews, err := db.ListReviewPendingAliases(ctx, account.ID); err != nil || len(reviews) != 0 {
		t.Fatalf("directory confirmation retained review: count=%d err=%v", len(reviews), err)
	}
	keys, err := db.ListPendingAliasAPIKeysByAccount(ctx, account.ID)
	if err != nil || len(keys) != 1 || keys[0].APIKeyCiphertext != "original-encrypted-key" {
		t.Fatalf("directory confirmation changed credentials: count=%d err=%v", len(keys), err)
	}
	if _, err := db.ConfirmReviewedAliasFromDirectory(ctx, account.ID, created.Address); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("already confirmed alias accepted again: %v", err)
	}
}
