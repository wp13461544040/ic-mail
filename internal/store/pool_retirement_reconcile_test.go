package store_test

import (
	"context"
	"errors"
	"testing"

	"icloud-api/internal/store"
)

func retirementReconcileFixture(t *testing.T) (*store.Store, store.PoolClient, store.PoolLease, string, []store.PoolAliasRetirementItemInput) {
	t.Helper()
	db, client, lease := usedPoolLease(t)
	operationID := "pool-retirement-reconcile-0001"
	items := []store.PoolAliasRetirementItemInput{{AliasID: lease.AliasID, LeaseID: lease.ID}}
	ctx := context.Background()
	if _, acquired, err := db.StartPoolAliasRetirement(ctx, client.ID, operationID, "fixture", items); err != nil || !acquired {
		t.Fatalf("start acquired=%v err=%v", acquired, err)
	}
	if err := db.MarkPoolAliasRetirementReview(ctx, operationID, 0, "APPLE_DELETE_UNCERTAIN", "fixture"); err != nil {
		t.Fatal(err)
	}
	return db, client, lease, operationID, items
}

func TestPoolAliasRetirementReconcileUnknownStaysClosedAndResumeIsExplicit(t *testing.T) {
	db, client, lease, operationID, items := retirementReconcileFixture(t)
	ctx := context.Background()
	job, acquired, err := db.AcquirePoolAliasRetirementReconcile(ctx, client.ID, operationID, items)
	if err != nil || !acquired || job.Status != "running" || job.Items[0].State != "review" {
		t.Fatalf("acquire acquired=%v err=%v status=%s", acquired, err, job.Status)
	}
	if _, acquired, err := db.AcquirePoolAliasRetirementReconcile(ctx, client.ID, operationID, items); err != nil || acquired {
		t.Fatalf("concurrent acquire acquired=%v err=%v", acquired, err)
	}
	current, err := db.GetPoolLease(ctx, lease.ID, client.ID)
	if err != nil || current.State != "retiring" {
		t.Fatalf("unknown lease state=%s err=%v", current.State, err)
	}
	for _, action := range []string{"commit", "release", "renew"} {
		if _, err := db.ActPoolLease(ctx, lease.ID, client.ID, action, 600); !errors.Is(err, store.ErrPoolClosed) {
			t.Fatalf("unknown %s err=%v", action, err)
		}
	}
	if err := db.CompletePoolAliasRetirementReconcile(ctx, client.ID, operationID); err != nil {
		t.Fatal(err)
	}
	job, err = db.GetPoolAliasRetirementJob(ctx, operationID, client.ID)
	if err != nil || job.Status != "review" || job.Items[0].State != "review" {
		t.Fatalf("unknown completion status=%s err=%v", job.Status, err)
	}
	if _, acquired, err := db.AcquirePoolAliasRetirementReconcile(ctx, client.ID, operationID, items); err != nil || !acquired {
		t.Fatalf("reacquire acquired=%v err=%v", acquired, err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := db.PreparePoolAliasRetirementResume(ctx, client.ID, operationID, 0); err != nil {
			t.Fatalf("prepare attempt=%d err=%v", attempt, err)
		}
	}
	var memberState string
	if err := db.DB().QueryRowContext(ctx, `SELECT state FROM pool_members WHERE alias_id=?`, lease.AliasID).Scan(&memberState); err != nil || memberState != "paused" {
		t.Fatalf("resumed member=%s err=%v", memberState, err)
	}
	if _, err := db.ClaimPool(ctx, client.ID, store.PoolClaim{RequestID: "reconcile-no-recycle-0001", Count: 1, TTLSeconds: 600}); !errors.Is(err, store.ErrPoolEmpty) {
		t.Fatalf("resumed claim err=%v", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := db.FinalizeReconciledPoolAliasRetirement(ctx, client.ID, operationID, 0); err != nil {
			t.Fatalf("finalize attempt=%d err=%v", attempt, err)
		}
	}
	if err := db.RecordPoolAliasRetirementReconcileReview(ctx, client.ID, operationID, 0, "APPLE_DELETE_UNCERTAIN", "stale read fixture"); err != nil {
		t.Fatalf("retired review replay err=%v", err)
	}
	job, err = db.GetPoolAliasRetirementJob(ctx, operationID, client.ID)
	if err != nil || job.Items[0].State != "retired" || job.Items[0].ResultCode != "" {
		t.Fatalf("retired review replay changed state err=%v", err)
	}
	if err := db.CompletePoolAliasRetirementReconcile(ctx, client.ID, operationID); err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := db.AcquirePoolAliasRetirementReconcile(ctx, client.ID, operationID, items); err != nil || acquired {
		t.Fatalf("completed replay acquired=%v err=%v", acquired, err)
	}
	retired, err := db.GetPoolLease(ctx, lease.ID, client.ID)
	if err != nil || retired.State != "retired" || retired.AliasID != 0 {
		t.Fatalf("retired state=%s alias_id=%d err=%v", retired.State, retired.AliasID, err)
	}
}

func TestPoolAliasRetirementReconcileRejectsProjectAndChangedRequest(t *testing.T) {
	db, client, _, operationID, items := retirementReconcileFixture(t)
	ctx := context.Background()
	other, _, err := db.CreatePoolClient(ctx, "reconcile-other-project")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AcquirePoolAliasRetirementReconcile(ctx, other.ID, operationID, items); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project acquire err=%v", err)
	}
	if err := db.PreparePoolAliasRetirementResume(ctx, other.ID, operationID, 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project resume err=%v", err)
	}
	if err := db.FinalizeReconciledPoolAliasRetirement(ctx, other.ID, operationID, 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project finalize err=%v", err)
	}
	changed := []store.PoolAliasRetirementItemInput{{AliasID: items[0].AliasID, LeaseID: "replacement-lease"}}
	if _, _, err := db.AcquirePoolAliasRetirementReconcile(ctx, client.ID, operationID, changed); !errors.Is(err, store.ErrPoolRequestConflict) {
		t.Fatalf("changed items err=%v", err)
	}
	if _, err := db.UpdatePoolClient(ctx, client.ID, false, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AcquirePoolAliasRetirementReconcile(ctx, client.ID, operationID, items); !errors.Is(err, store.ErrPoolClosed) {
		t.Fatalf("disabled client err=%v", err)
	}
}

func TestPoolAliasRetirementReconcileMissingAliasPreservesHistoricalLease(t *testing.T) {
	db, client, lease, operationID, items := retirementReconcileFixture(t)
	ctx := context.Background()
	if _, err := db.DB().ExecContext(ctx, `DELETE FROM aliases WHERE id=?`, lease.AliasID); err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := db.AcquirePoolAliasRetirementReconcile(ctx, client.ID, operationID, items); err != nil || !acquired {
		t.Fatalf("missing alias acquire acquired=%v err=%v", acquired, err)
	}
	if err := db.PreparePoolAliasRetirementResume(ctx, client.ID, operationID, 0); !errors.Is(err, store.ErrPoolRetirementConflict) {
		t.Fatalf("missing alias resume err=%v", err)
	}
	if err := db.FinalizeReconciledPoolAliasRetirement(ctx, client.ID, operationID, 0); err != nil {
		t.Fatal(err)
	}
	current, err := db.GetPoolLease(ctx, lease.ID, client.ID)
	if err != nil || current.State != "retired" || current.AliasID != 0 || current.Address != lease.Address {
		t.Fatalf("missing alias finalization state=%s alias_id=%d err=%v", current.State, current.AliasID, err)
	}
}

func TestPoolAliasRetirementReconcileRechecksBindingAfterAcquire(t *testing.T) {
	for _, mutate := range []string{
		`UPDATE pool_leases SET address='changed@fixture.invalid' WHERE id=?`,
		`UPDATE pool_leases SET alias_id=NULL WHERE id=?`,
	} {
		t.Run(mutate, func(t *testing.T) {
			db, client, lease, operationID, items := retirementReconcileFixture(t)
			ctx := context.Background()
			if _, acquired, err := db.AcquirePoolAliasRetirementReconcile(ctx, client.ID, operationID, items); err != nil || !acquired {
				t.Fatalf("acquire acquired=%v err=%v", acquired, err)
			}
			if _, err := db.DB().ExecContext(ctx, mutate, lease.ID); err != nil {
				t.Fatal(err)
			}
			if err := db.PreparePoolAliasRetirementResume(ctx, client.ID, operationID, 0); !errors.Is(err, store.ErrPoolRetirementConflict) {
				t.Fatalf("changed binding resume err=%v", err)
			}
			if err := db.FinalizeReconciledPoolAliasRetirement(ctx, client.ID, operationID, 0); !errors.Is(err, store.ErrPoolRetirementConflict) {
				t.Fatalf("changed binding finalize err=%v", err)
			}
			if _, err := db.GetAlias(ctx, lease.AliasID); err != nil {
				t.Fatalf("changed binding deleted alias err=%v", err)
			}
		})
	}
}

func TestPoolAliasRetirementReconcileUsedJobDoesNotStealNewAttempt(t *testing.T) {
	db, client, _, operationID, items := retirementReconcileFixture(t)
	ctx := context.Background()
	// A definitive failure restores used; a later operation can own that lease.
	if _, err := db.DB().ExecContext(ctx, `UPDATE pool_alias_retirement_job_items SET state='retiring' WHERE operation_id=?`, operationID); err != nil {
		t.Fatal(err)
	}
	if err := db.RestorePoolAliasRetirement(ctx, operationID, 0, "APPLE_DELETE_FAILED", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetPoolAliasRetirementJobStatus(ctx, operationID, client.ID, store.PoolAliasRetirementJobCompleted); err != nil {
		t.Fatal(err)
	}
	if _, accepted, err := db.StartPoolAliasRetirement(ctx, client.ID, "pool-retirement-new-attempt-0001", "fixture-new", items); err != nil || !accepted {
		t.Fatalf("new attempt accepted=%v err=%v", accepted, err)
	}
	if _, acquired, err := db.AcquirePoolAliasRetirementReconcile(ctx, client.ID, operationID, items); !errors.Is(err, store.ErrPoolRetirementConflict) || acquired {
		t.Fatalf("old acquire acquired=%v err=%v", acquired, err)
	}
	oldJob, err := db.GetPoolAliasRetirementJob(ctx, operationID, client.ID)
	if err != nil || oldJob.Status != "completed" || oldJob.Items[0].State != "used" {
		t.Fatalf("conflicting acquire reserved old job status=%s err=%v", oldJob.Status, err)
	}
	if err := db.PreparePoolAliasRetirementResume(ctx, client.ID, operationID, 0); !errors.Is(err, store.ErrPoolRetirementConflict) {
		t.Fatalf("old attempt steals lease err=%v", err)
	}
	if err := db.FinalizeReconciledPoolAliasRetirement(ctx, client.ID, operationID, 0); !errors.Is(err, store.ErrPoolRetirementConflict) {
		t.Fatalf("old attempt finalizes another owner lease err=%v", err)
	}
	newJob, err := db.GetPoolAliasRetirementJob(ctx, "pool-retirement-new-attempt-0001", client.ID)
	if err != nil || newJob.Status != "queued" || newJob.Items[0].State != "retiring" {
		t.Fatalf("new attempt changed status=%s err=%v", newJob.Status, err)
	}
}

func TestPoolAliasRetirementReconcileUsedFailureCanResumeOriginalJob(t *testing.T) {
	db, client, lease, operationID, items := retirementReconcileFixture(t)
	ctx := context.Background()
	if _, err := db.DB().ExecContext(ctx, `UPDATE pool_alias_retirement_job_items SET state='retiring' WHERE operation_id=?`, operationID); err != nil {
		t.Fatal(err)
	}
	if err := db.RestorePoolAliasRetirement(ctx, operationID, 0, "APPLE_DELETE_FAILED", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetPoolAliasRetirementJobStatus(ctx, operationID, client.ID, store.PoolAliasRetirementJobCompleted); err != nil {
		t.Fatal(err)
	}
	job, acquired, err := db.AcquirePoolAliasRetirementReconcile(ctx, client.ID, operationID, items)
	if err != nil || !acquired || job.Items[0].State != "used" {
		t.Fatalf("failed acquire acquired=%v err=%v", acquired, err)
	}
	current, err := db.GetPoolLease(ctx, lease.ID, client.ID)
	if err != nil || current.State != "used" {
		t.Fatalf("acquire implicitly resumed state=%s err=%v", current.State, err)
	}
	if err := db.PreparePoolAliasRetirementResume(ctx, client.ID, operationID, 0); err != nil {
		t.Fatal(err)
	}
	current, err = db.GetPoolLease(ctx, lease.ID, client.ID)
	if err != nil || current.State != "retiring" {
		t.Fatalf("explicit resume state=%s err=%v", current.State, err)
	}
}

func TestPoolAliasRetirementReconcileBatchIdentityAndAcquisitionAreAtomic(t *testing.T) {
	db, _, _, _, client := poolFixture(t, 2, "")
	ctx := context.Background()
	leases, err := db.ClaimPool(ctx, client.ID, store.PoolClaim{RequestID: "reconcile-batch-claim-0001", Count: 2, TTLSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	items := make([]store.PoolAliasRetirementItemInput, len(leases))
	for index, lease := range leases {
		if _, err := db.ActPoolLease(ctx, lease.ID, client.ID, "commit", 0); err != nil {
			t.Fatal(err)
		}
		items[index] = store.PoolAliasRetirementItemInput{AliasID: lease.AliasID, LeaseID: lease.ID}
	}
	operationID := "pool-retirement-reconcile-batch-0001"
	if _, accepted, err := db.StartPoolAliasRetirement(ctx, client.ID, operationID, "fixture", items); err != nil || !accepted {
		t.Fatalf("batch start accepted=%v err=%v", accepted, err)
	}
	if err := db.InterruptPoolAliasRetirementJobs(ctx); err != nil {
		t.Fatal(err)
	}
	reversed := []store.PoolAliasRetirementItemInput{items[1], items[0]}
	if _, _, err := db.AcquirePoolAliasRetirementReconcile(ctx, client.ID, operationID, reversed); !errors.Is(err, store.ErrPoolRequestConflict) {
		t.Fatalf("batch order err=%v", err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE pool_leases SET address='changed@fixture.invalid' WHERE id=?`, leases[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AcquirePoolAliasRetirementReconcile(ctx, client.ID, operationID, items); !errors.Is(err, store.ErrPoolRetirementConflict) {
		t.Fatalf("batch binding err=%v", err)
	}
	job, err := db.GetPoolAliasRetirementJob(ctx, operationID, client.ID)
	if err != nil || job.Status != "interrupted" || job.Items[0].State != "review" || job.Items[1].State != "review" {
		t.Fatalf("partial batch acquisition status=%s err=%v", job.Status, err)
	}
}

func TestPoolAliasRetirementReconcileBatchOtherOwnerRejectsWholeAcquisition(t *testing.T) {
	db, _, _, _, client := poolFixture(t, 2, "")
	ctx := context.Background()
	leases, err := db.ClaimPool(ctx, client.ID, store.PoolClaim{RequestID: "reconcile-owner-batch-0001", Count: 2, TTLSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	items := make([]store.PoolAliasRetirementItemInput, len(leases))
	for index, lease := range leases {
		if _, err := db.ActPoolLease(ctx, lease.ID, client.ID, "commit", 0); err != nil {
			t.Fatal(err)
		}
		items[index] = store.PoolAliasRetirementItemInput{AliasID: lease.AliasID, LeaseID: lease.ID}
	}
	operationID := "pool-retirement-owner-batch-0001"
	if _, accepted, err := db.StartPoolAliasRetirement(ctx, client.ID, operationID, "fixture", items); err != nil || !accepted {
		t.Fatalf("batch start accepted=%v err=%v", accepted, err)
	}
	for ordinal := range items {
		if err := db.RestorePoolAliasRetirement(ctx, operationID, ordinal, "APPLE_DELETE_FAILED", "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetPoolAliasRetirementJobStatus(ctx, operationID, client.ID, store.PoolAliasRetirementJobCompleted); err != nil {
		t.Fatal(err)
	}
	before, err := db.GetPoolAliasRetirementJob(ctx, operationID, client.ID)
	if err != nil {
		t.Fatal(err)
	}
	newOperationID := "pool-retirement-owner-new-0001"
	if _, accepted, err := db.StartPoolAliasRetirement(ctx, client.ID, newOperationID, "fixture-new", items[1:]); err != nil || !accepted {
		t.Fatalf("new owner accepted=%v err=%v", accepted, err)
	}
	if _, acquired, err := db.AcquirePoolAliasRetirementReconcile(ctx, client.ID, operationID, items); !errors.Is(err, store.ErrPoolRetirementConflict) || acquired {
		t.Fatalf("batch owner acquire acquired=%v err=%v", acquired, err)
	}
	after, err := db.GetPoolAliasRetirementJob(ctx, operationID, client.ID)
	if err != nil || after.Status != "completed" || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("batch owner mutated reservation status=%s err=%v", after.Status, err)
	}
	for _, item := range after.Items {
		if item.State != "used" || item.ResultCode != "APPLE_DELETE_FAILED" {
			t.Fatalf("batch owner mutated ordinal=%d state=%s", item.Ordinal, item.State)
		}
	}
	for index, wantState := range []string{"used", "retiring"} {
		current, err := db.GetPoolLease(ctx, leases[index].ID, client.ID)
		if err != nil || current.State != wantState {
			t.Fatalf("batch owner lease index=%d state=%s err=%v", index, current.State, err)
		}
	}
	newJob, err := db.GetPoolAliasRetirementJob(ctx, newOperationID, client.ID)
	if err != nil || newJob.Status != "queued" || newJob.Items[0].State != "retiring" {
		t.Fatalf("batch owner changed newer job status=%s err=%v", newJob.Status, err)
	}
}

func TestPoolAliasRetirementReconcileReviewRetainsRunningReservation(t *testing.T) {
	db, client, lease, operationID, items := retirementReconcileFixture(t)
	ctx := context.Background()
	if _, acquired, err := db.AcquirePoolAliasRetirementReconcile(ctx, client.ID, operationID, items); err != nil || !acquired {
		t.Fatalf("acquire acquired=%v err=%v", acquired, err)
	}
	if err := db.PreparePoolAliasRetirementResume(ctx, client.ID, operationID, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordPoolAliasRetirementReconcileReview(ctx, client.ID, operationID, 0, " APPLE_DELETE_UNCERTAIN ", " fixture "); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetPoolAliasRetirementJob(ctx, operationID, client.ID)
	if err != nil || job.Status != "running" || job.Items[0].State != "review" || job.Items[0].ResultCode != "APPLE_DELETE_UNCERTAIN" {
		t.Fatalf("review reservation status=%s err=%v", job.Status, err)
	}
	if _, acquired, err := db.AcquirePoolAliasRetirementReconcile(ctx, client.ID, operationID, items); err != nil || acquired {
		t.Fatalf("second executor acquired=%v err=%v", acquired, err)
	}
	current, err := db.GetPoolLease(ctx, lease.ID, client.ID)
	if err != nil || current.State != "retiring" {
		t.Fatalf("review lease state=%s err=%v", current.State, err)
	}
	other, _, err := db.CreatePoolClient(ctx, "reconcile-review-other")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordPoolAliasRetirementReconcileReview(ctx, other.ID, operationID, 0, "CHANGED", "fixture"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross project review err=%v", err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE pool_leases SET address='changed@fixture.invalid' WHERE id=?`, lease.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordPoolAliasRetirementReconcileReview(ctx, client.ID, operationID, 0, "CHANGED", "fixture"); !errors.Is(err, store.ErrPoolRetirementConflict) {
		t.Fatalf("changed binding review err=%v", err)
	}
}

func TestPoolAliasRetirementReconcileReviewPreservesDefinitiveUsedFailure(t *testing.T) {
	db, client, lease, operationID, items := retirementReconcileFixture(t)
	ctx := context.Background()
	if _, err := db.DB().ExecContext(ctx, `UPDATE pool_alias_retirement_job_items SET state='retiring' WHERE operation_id=?`, operationID); err != nil {
		t.Fatal(err)
	}
	if err := db.RestorePoolAliasRetirement(ctx, operationID, 0, "APPLE_DELETE_FAILED", "definitive fixture"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetPoolAliasRetirementJobStatus(ctx, operationID, client.ID, store.PoolAliasRetirementJobCompleted); err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := db.AcquirePoolAliasRetirementReconcile(ctx, client.ID, operationID, items); err != nil || !acquired {
		t.Fatalf("acquire acquired=%v err=%v", acquired, err)
	}
	if err := db.RecordPoolAliasRetirementReconcileReview(ctx, client.ID, operationID, 0, "APPLE_DELETE_UNCERTAIN", "latest read fixture"); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetPoolAliasRetirementJob(ctx, operationID, client.ID)
	if err != nil || job.Status != "running" || job.Items[0].State != "used" || job.Items[0].ResultCode != "APPLE_DELETE_FAILED" || job.Items[0].ResultMessage != "definitive fixture" {
		t.Fatalf("used failure changed status=%s err=%v", job.Status, err)
	}
	current, err := db.GetPoolLease(ctx, lease.ID, client.ID)
	if err != nil || current.State != "used" {
		t.Fatalf("used failure lease state=%s err=%v", current.State, err)
	}
	if err := db.SetPoolAliasRetirementJobStatus(ctx, operationID, client.ID, store.PoolAliasRetirementJobCompleted); err != nil {
		t.Fatal(err)
	}
	if err := db.CompletePoolAliasRetirementReconcile(ctx, client.ID, operationID); err != nil {
		t.Fatalf("worker already published completed err=%v", err)
	}
}

func TestPoolAliasRetirementReconcileCompleteAcceptsWorkerPublishedReview(t *testing.T) {
	db, client, _, operationID, _ := retirementReconcileFixture(t)
	if err := db.CompletePoolAliasRetirementReconcile(context.Background(), client.ID, operationID); err != nil {
		t.Fatalf("worker already published review err=%v", err)
	}
}

func TestPoolAliasRetirementReconcileConfirmedAbsenceRetiresUsedFailure(t *testing.T) {
	for _, missingAlias := range []bool{false, true} {
		name := "local-alias-present"
		if missingAlias {
			name = "local-alias-missing"
		}
		t.Run(name, func(t *testing.T) {
			db, client, lease, operationID, items := retirementReconcileFixture(t)
			ctx := context.Background()
			if _, err := db.DB().ExecContext(ctx, `UPDATE pool_alias_retirement_job_items SET state='retiring' WHERE operation_id=?`, operationID); err != nil {
				t.Fatal(err)
			}
			if err := db.RestorePoolAliasRetirement(ctx, operationID, 0, "APPLE_DELETE_FAILED", "definitive fixture"); err != nil {
				t.Fatal(err)
			}
			if err := db.SetPoolAliasRetirementJobStatus(ctx, operationID, client.ID, store.PoolAliasRetirementJobCompleted); err != nil {
				t.Fatal(err)
			}
			if missingAlias {
				if _, err := db.DB().ExecContext(ctx, `DELETE FROM aliases WHERE id=?`, lease.AliasID); err != nil {
					t.Fatal(err)
				}
			}
			if _, acquired, err := db.AcquirePoolAliasRetirementReconcile(ctx, client.ID, operationID, items); err != nil || !acquired {
				t.Fatalf("used absence acquire acquired=%v err=%v", acquired, err)
			}
			// The caller has a successful complete-directory absence observation;
			// publishing it retires used directly without preparing remote writes.
			for attempt := 0; attempt < 2; attempt++ {
				if err := db.FinalizeReconciledPoolAliasRetirement(ctx, client.ID, operationID, 0); err != nil {
					t.Fatalf("used absence finalize attempt=%d err=%v", attempt, err)
				}
			}
			if err := db.CompletePoolAliasRetirementReconcile(ctx, client.ID, operationID); err != nil {
				t.Fatal(err)
			}
			current, err := db.GetPoolLease(ctx, lease.ID, client.ID)
			if err != nil || current.State != "retired" || current.AliasID != 0 || current.Address != lease.Address {
				t.Fatalf("used absence lease state=%s alias_id=%d err=%v", current.State, current.AliasID, err)
			}
			job, err := db.GetPoolAliasRetirementJob(ctx, operationID, client.ID)
			if err != nil || job.Status != "completed" || job.Items[0].State != "retired" || job.Items[0].ResultCode != "" {
				t.Fatalf("used absence job status=%s err=%v", job.Status, err)
			}
			if _, err := db.GetAlias(ctx, lease.AliasID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("used absence retained alias err=%v", err)
			}
			if _, err := db.ClaimPool(ctx, client.ID, store.PoolClaim{RequestID: "used-absence-no-recycle-0001", Count: 1, TTLSeconds: 600}); !errors.Is(err, store.ErrPoolEmpty) {
				t.Fatalf("used absence recycled lease err=%v", err)
			}
		})
	}
}
