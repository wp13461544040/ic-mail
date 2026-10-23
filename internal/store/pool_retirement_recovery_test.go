package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"icloud-api/internal/store"
)

func retirementRecoveryFixture(t *testing.T) (*store.Store, store.PoolAliasRetirementRecoverySnapshot) {
	t.Helper()
	db, client, lease := usedPoolLease(t)
	ctx := context.Background()
	alias, err := db.GetAlias(ctx, lease.AliasID)
	if err != nil {
		t.Fatal(err)
	}
	input := store.PoolAliasRetirementRecoverySnapshot{OperationID: "pool-retirement-recovery-0001", ClientID: client.ID, Project: client.Project, AliasID: alias.ID, AccountID: alias.AccountID, LeaseID: lease.ID, Address: lease.Address,
		EvidenceRef: "immutable-backup-and-receipt sha256:" + strings.Repeat("a", 64)}
	if _, err := db.DB().ExecContext(ctx, `DELETE FROM aliases WHERE id=?`, alias.ID); err != nil {
		t.Fatal(err)
	}
	return db, input
}

func TestPoolAliasRetirementRecoveryClosesOrphanAndRequiresReconciliation(t *testing.T) {
	db, input := retirementRecoveryFixture(t)
	ctx := context.Background()
	job, recovered, err := db.RecoverPoolAliasRetirementSnapshot(ctx, input)
	if err != nil || !recovered || job.Status != "review" || job.Items[0].State != "review" {
		t.Fatalf("recovery recovered=%v status=%s err=%v", recovered, job.Status, err)
	}
	lease, err := db.GetPoolLease(ctx, input.LeaseID, input.ClientID)
	if err != nil || lease.State != "retiring" || lease.AliasID != 0 {
		t.Fatalf("recovered lease state=%s alias_id=%d err=%v", lease.State, lease.AliasID, err)
	}
	for _, action := range []string{"commit", "release", "renew"} {
		if _, err := db.ActPoolLease(ctx, input.LeaseID, input.ClientID, action, 600); !errors.Is(err, store.ErrPoolClosed) {
			t.Fatalf("recovered lease %s err=%v", action, err)
		}
	}
	replay, recovered, err := db.RecoverPoolAliasRetirementSnapshot(ctx, input)
	if err != nil || recovered || !replay.UpdatedAt.Equal(job.UpdatedAt) {
		t.Fatalf("recovery replay recovered=%v err=%v", recovered, err)
	}
	audits, err := db.ListAuditLogs(ctx, 10, 0)
	if err != nil || len(audits) != 1 || audits[0].ResourceID != input.OperationID || audits[0].Detail != "sha256:"+strings.Repeat("a", 64) || strings.Contains(audits[0].Detail, input.Address) || strings.Contains(audits[0].Detail, "immutable-backup") {
		t.Fatalf("recovery audit count=%d err=%v", len(audits), err)
	}
	items := []store.PoolAliasRetirementItemInput{{AliasID: input.AliasID, LeaseID: input.LeaseID}}
	if _, acquired, err := db.AcquirePoolAliasRetirementReconcile(ctx, input.ClientID, input.OperationID, items); err != nil || !acquired {
		t.Fatalf("recovered reconcile acquired=%v err=%v", acquired, err)
	}
	if err := db.PreparePoolAliasRetirementResume(ctx, input.ClientID, input.OperationID, 0); !errors.Is(err, store.ErrPoolRetirementConflict) {
		t.Fatalf("missing recovered alias resume err=%v", err)
	}
	if err := db.RecordPoolAliasRetirementReconcileReview(ctx, input.ClientID, input.OperationID, 0, "APPLE_DELETE_UNCERTAIN", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := db.CompletePoolAliasRetirementReconcile(ctx, input.ClientID, input.OperationID); err != nil {
		t.Fatal(err)
	}
	lease, err = db.GetPoolLease(ctx, input.LeaseID, input.ClientID)
	if err != nil || lease.State != "retiring" {
		t.Fatalf("unknown recovered state=%s err=%v", lease.State, err)
	}
	if _, acquired, err := db.AcquirePoolAliasRetirementReconcile(ctx, input.ClientID, input.OperationID, items); err != nil || !acquired {
		t.Fatalf("confirmed acquire acquired=%v err=%v", acquired, err)
	}
	// The caller supplies a successful full-directory absence observation.
	if err := db.FinalizeReconciledPoolAliasRetirement(ctx, input.ClientID, input.OperationID, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.CompletePoolAliasRetirementReconcile(ctx, input.ClientID, input.OperationID); err != nil {
		t.Fatal(err)
	}
	if replay, recovered, err := db.RecoverPoolAliasRetirementSnapshot(ctx, input); err != nil || recovered || replay.Items[0].State != "retired" {
		t.Fatalf("retired recovery replay recovered=%v err=%v", recovered, err)
	}
}

func TestPoolAliasRetirementRecoveryChangedSnapshotConflicts(t *testing.T) {
	db, input := retirementRecoveryFixture(t)
	ctx := context.Background()
	if _, _, err := db.RecoverPoolAliasRetirementSnapshot(ctx, input); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*store.PoolAliasRetirementRecoverySnapshot){
		func(v *store.PoolAliasRetirementRecoverySnapshot) { v.AliasID++ },
		func(v *store.PoolAliasRetirementRecoverySnapshot) { v.AccountID++ },
		func(v *store.PoolAliasRetirementRecoverySnapshot) { v.LeaseID = "different-receipt-lease" },
		func(v *store.PoolAliasRetirementRecoverySnapshot) { v.Address = "different@fixture.invalid" },
		func(v *store.PoolAliasRetirementRecoverySnapshot) { v.ClientID = "different-client" },
		func(v *store.PoolAliasRetirementRecoverySnapshot) { v.Project = "different-project" },
		func(v *store.PoolAliasRetirementRecoverySnapshot) {
			v.EvidenceRef = "sha256:" + strings.Repeat("b", 64)
		},
	} {
		changed := input
		change(&changed)
		if _, recovered, err := db.RecoverPoolAliasRetirementSnapshot(ctx, changed); !errors.Is(err, store.ErrPoolRequestConflict) || recovered {
			t.Fatalf("changed snapshot recovered=%v err=%v", recovered, err)
		}
	}
}

func TestPoolAliasRetirementRecoveryExistingOriginalJobIsReadOnlyReplay(t *testing.T) {
	db, client, lease := usedPoolLease(t)
	ctx := context.Background()
	alias, err := db.GetAlias(ctx, lease.AliasID)
	if err != nil {
		t.Fatal(err)
	}
	operationID := "pool-retirement-original-replay-0001"
	items := []store.PoolAliasRetirementItemInput{{AliasID: alias.ID, LeaseID: lease.ID}}
	original, _, err := db.StartPoolAliasRetirement(ctx, client.ID, operationID, "fixture", items)
	if err != nil {
		t.Fatal(err)
	}
	input := store.PoolAliasRetirementRecoverySnapshot{OperationID: operationID, ClientID: client.ID, Project: client.Project, AliasID: alias.ID, AccountID: alias.AccountID, LeaseID: lease.ID, Address: lease.Address, EvidenceRef: "sha256:" + strings.Repeat("c", 64)}
	job, recovered, err := db.RecoverPoolAliasRetirementSnapshot(ctx, input)
	if err != nil || recovered || job.Status != original.Status || !job.UpdatedAt.Equal(original.UpdatedAt) {
		t.Fatalf("original replay recovered=%v status=%s err=%v", recovered, job.Status, err)
	}
	audits, err := db.ListAuditLogs(ctx, 10, 0)
	if err != nil || len(audits) != 0 {
		t.Fatalf("original replay wrote audit count=%d err=%v", len(audits), err)
	}
}

func TestPoolAliasRetirementRecoveryPreflightIsAtomic(t *testing.T) {
	for _, scenario := range []string{"evidence-missing", "evidence-malformed", "project", "address", "disabled-client", "existing-alias", "other-live-address", "other-retirement-owner"} {
		t.Run(scenario, func(t *testing.T) {
			db, input := retirementRecoveryFixture(t)
			ctx := context.Background()
			original := input
			var err error
			switch scenario {
			case "evidence-missing":
				input.EvidenceRef = ""
			case "evidence-malformed":
				input.EvidenceRef = "sha256:invalid"
			case "project":
				input.Project = "different-project"
			case "address":
				input.Address = "different@fixture.invalid"
			case "disabled-client":
				_, err = db.UpdatePoolClient(ctx, input.ClientID, false, false)
			case "existing-alias":
				_, err = db.DB().ExecContext(ctx, `INSERT INTO aliases(id,account_id,address,label,api_key_hash,api_key_prefix,credential_mode,enabled,last_sync_status,last_sync_error,created_at,updated_at) VALUES(?,?,?,'',?,'fixture','legacy',TRUE,'pending','',1,1)`, input.AliasID, input.AccountID, input.Address, []byte(strings.Repeat("x", 32)))
			case "other-live-address":
				_, err = db.DB().ExecContext(ctx, `INSERT INTO pool_leases(id,alias_id,address,client_id,request_id,state,created_at,expires_at,updated_at) SELECT 'competing-receipt-lease',NULL,address,client_id,request_id,'used',created_at,expires_at,updated_at FROM pool_leases WHERE id=?`, input.LeaseID)
			case "other-retirement-owner":
				_, err = db.DB().ExecContext(ctx, `INSERT INTO pool_alias_retirement_jobs(operation_id,client_id,project,fingerprint,status,request_id,created_at,updated_at) VALUES('different-retirement-operation',?,?,'fixture','review','fixture',1,1)`, input.ClientID, input.Project)
				if err == nil {
					_, err = db.DB().ExecContext(ctx, `INSERT INTO pool_alias_retirement_job_items(operation_id,ordinal,alias_id,lease_id,account_id,address,state,updated_at) VALUES('different-retirement-operation',0,?,?,?,?,'review',1)`, input.AliasID, input.LeaseID, input.AccountID, input.Address)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, recovered, err := db.RecoverPoolAliasRetirementSnapshot(ctx, input); err == nil || recovered {
				t.Fatalf("preflight accepted scenario=%s recovered=%v err=%v", scenario, recovered, err)
			}
			if _, err := db.GetPoolAliasRetirementJob(ctx, original.OperationID, original.ClientID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("preflight created job err=%v", err)
			}
			lease, err := db.GetPoolLease(ctx, original.LeaseID, original.ClientID)
			if err != nil || lease.State != "used" {
				t.Fatalf("preflight changed lease state=%s err=%v", lease.State, err)
			}
			audits, err := db.ListAuditLogs(ctx, 10, 0)
			if err != nil || len(audits) != 0 {
				t.Fatalf("preflight wrote audit count=%d err=%v", len(audits), err)
			}
		})
	}
}
