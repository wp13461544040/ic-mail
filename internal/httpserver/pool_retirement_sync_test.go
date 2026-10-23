package httpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/hmesync"
	"icloud-api/internal/store"
	"icloud-api/internal/syncer"
)

type retirementSyncFetcher struct {
	entered chan struct{}
	release chan struct{}
	aliasID int64
}

func (f *retirementSyncFetcher) FetchIncremental(ctx context.Context, account domain.Account, _ string, _ []domain.Alias, _ *domain.IMAPSyncState, _ map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
	close(f.entered)
	select {
	case <-ctx.Done():
		return domain.MailboxSyncResult{}, ctx.Err()
	case <-f.release:
	}
	now := time.Now().UTC()
	return domain.MailboxSyncResult{
		State:     domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1, LastUID: 2, UpdatedAt: now},
		TargetUID: 2,
		ArchivedMessages: []domain.ArchivedMessage{{
			AccountID: account.ID, UIDValidity: 1, UID: 2, InternalDate: now,
			RawMIME: []byte("To: survivor+task@example.test\r\n\r\nVerification code 123456"),
			OTP:     "123456", AliasIDs: []int64{f.aliasID},
		}},
	}, nil
}

func (f *retirementSyncFetcher) FetchActiveIncremental(ctx context.Context, account domain.Account, password string, aliases []domain.Alias, previous *domain.IMAPSyncState, positions map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
	return f.FetchIncremental(ctx, account, password, aliases, previous, positions)
}

func TestPoolRetirementPublicationRunsDuringActiveSync(t *testing.T) {
	for _, reconcile := range []bool{false, true} {
		name := "delete_receipt"
		if reconcile {
			name = "directory_reconcile"
		}
		t.Run(name, func(t *testing.T) {
			env := newAdminAPITestEnv(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, _, lease := poolAliasRetirementUsedLease(t, env)
			retiringAlias, err := env.store.GetAlias(ctx, lease.AliasID)
			if err != nil {
				t.Fatal(err)
			}
			survivor, _ := createV2AliasFixture(t, env, retiringAlias.AccountID, "survivor@example.test")
			if err := env.store.ConfigureMailArchive(t.TempDir(), 1<<20); err != nil {
				t.Fatal(err)
			}
			account, err := env.store.GetAccount(ctx, retiringAlias.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			if err := env.store.ApplyMailboxSync(ctx, account.ID, account.UpdatedAt, []domain.Alias{retiringAlias, survivor}, domain.MailboxSyncResult{
				Reset: true, State: domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1, LastUID: 1, UpdatedAt: time.Now().UTC()},
			}, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			job, _, err := env.store.StartPoolAliasRetirement(ctx, client.ID, "retirement-sync-publication-0001", "fixture", []store.PoolAliasRetirementItemInput{{AliasID: lease.AliasID, LeaseID: lease.ID}})
			if err != nil {
				t.Fatal(err)
			}
			if reconcile {
				if err := env.store.MarkPoolAliasRetirementReview(ctx, job.OperationID, 0, "APPLE_DELETE_UNCERTAIN", "fixture"); err != nil {
					t.Fatal(err)
				}
				job, err = env.store.GetPoolAliasRetirementJob(ctx, job.OperationID, client.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			fetcher := &retirementSyncFetcher{entered: make(chan struct{}), release: make(chan struct{}), aliasID: survivor.ID}
			defer func() {
				select {
				case <-fetcher.release:
				default:
					close(fetcher.release)
				}
			}()
			manager := syncer.New(env.store, env.cipher, fetcher, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute, 2)
			publicationEntered := make(chan struct{}, 1)
			env.server.SetAccountLocker(func(ctx context.Context, accountID int64, operation func() error) error {
				publicationEntered <- struct{}{}
				return manager.WithAccountLock(ctx, accountID, operation)
			})
			synced := make(chan error, 1)
			syncDone := make(chan struct{})
			go func() {
				defer close(syncDone)
				synced <- manager.SyncActiveAccount(ctx, account.ID)
			}()
			defer func() { cancel(); <-syncDone }()
			select {
			case <-fetcher.entered:
			case <-ctx.Done():
				t.Fatal("active sync did not enter fixture fetch")
			}
			fake := &fakeHMESyncService{deleteAliases: func(context.Context, []int64) ([]hmesync.AliasDeletionOutcome, error) {
				return []hmesync.AliasDeletionOutcome{{AliasID: lease.AliasID}}, nil
			}}
			env.server.SetHMESyncService(fake)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				if reconcile {
					env.server.runPoolAliasRetirementReconcile(ctx, client, job, false, &fakeRetirementReconciler{
						fakeHMESyncService: fake,
						observe: func(context.Context, []hmesync.AliasRetirementTarget) ([]hmesync.AliasRetirementObservation, error) {
							return []hmesync.AliasRetirementObservation{{AliasID: lease.AliasID, Present: false}}, nil
						},
					})
				} else {
					env.server.runPoolAliasRetirementJob(ctx, client, job)
				}
			}()
			defer func() { cancel(); <-finished }()
			select {
			case <-finished:
			case <-ctx.Done():
				t.Fatal("retirement waited for unrelated IMAP network work")
			}
			current, err := env.store.GetPoolLease(ctx, lease.ID, client.ID)
			if err != nil || current.State != "retired" || current.AliasID != 0 {
				t.Fatalf("lease changed during active fetch: state=%s err=%v", current.State, err)
			}
			if _, err := env.store.GetAlias(ctx, lease.AliasID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("retired alias remained during active fetch: %v", err)
			}
			otherCtx, cancelOther := context.WithTimeout(ctx, time.Second)
			err = manager.WithAccountLock(otherCtx, account.ID+1, func() error { return nil })
			cancelOther()
			if err != nil {
				t.Fatalf("publication blocked another account: %v", err)
			}
			close(fetcher.release)
			if err := <-synced; err != nil {
				t.Fatalf("active sync was discarded by retirement: %v", err)
			}
			select {
			case <-finished:
			case <-ctx.Done():
				t.Fatal("retirement did not finish after active sync")
			}
			state, err := env.store.GetIMAPSyncState(ctx, account.ID)
			if err != nil || state.LastUID != 2 {
				t.Fatalf("sync cursor did not advance: uid=%d err=%v", state.LastUID, err)
			}
			otps, err := env.store.ListAliasOTPs(ctx, survivor.ID, 1)
			if err != nil || len(otps) != 1 || otps[0].OTP != "123456" {
				t.Fatalf("surviving alias OTP not published: count=%d err=%v", len(otps), err)
			}
			current, err = env.store.GetPoolLease(ctx, lease.ID, client.ID)
			if err != nil || current.State != "retired" || current.AliasID != 0 {
				t.Fatalf("retirement did not finalize: state=%s err=%v", current.State, err)
			}
			if _, err := env.store.GetAlias(ctx, lease.AliasID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("retired alias still exists: %v", err)
			}
		})
	}
}

func TestPoolRetirementPublicationLockFailureKeepsReview(t *testing.T) {
	env := newAdminAPITestEnv(t)
	ctx := context.Background()
	client, _, lease := poolAliasRetirementUsedLease(t, env)
	job, _, err := env.store.StartPoolAliasRetirement(ctx, client.ID, "retirement-publication-lock-0001", "fixture", []store.PoolAliasRetirementItemInput{{AliasID: lease.AliasID, LeaseID: lease.ID}})
	if err != nil {
		t.Fatal(err)
	}
	deleteCalls := 0
	env.server.SetHMESyncService(&fakeHMESyncService{deleteAliases: func(context.Context, []int64) ([]hmesync.AliasDeletionOutcome, error) {
		deleteCalls++
		return []hmesync.AliasDeletionOutcome{{AliasID: lease.AliasID}}, nil
	}})
	env.server.SetAccountLocker(func(context.Context, int64, func() error) error { return context.DeadlineExceeded })
	env.server.runPoolAliasRetirementJob(ctx, client, job)
	current, err := env.store.GetPoolAliasRetirementJob(ctx, job.OperationID, client.ID)
	if err != nil || current.Status != store.PoolAliasRetirementJobReview || current.Items[0].ResultCode != "APPLE_DELETE_UNCERTAIN" {
		t.Fatalf("publication lock failure lost review: status=%s err=%v", current.Status, err)
	}
	currentLease, err := env.store.GetPoolLease(ctx, lease.ID, client.ID)
	if err != nil || currentLease.State != "retiring" || currentLease.AliasID != lease.AliasID {
		t.Fatalf("publication lock failure reopened lease: state=%s err=%v", currentLease.State, err)
	}
	if _, err := env.store.GetAlias(ctx, lease.AliasID); err != nil || deleteCalls != 1 {
		t.Fatalf("publication lock failure removed alias or replayed Apple: calls=%d err=%v", deleteCalls, err)
	}
}
