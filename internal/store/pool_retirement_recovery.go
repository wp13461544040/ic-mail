package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/mail"
	"strings"

	"icloud-api/internal/domain"
)

// PoolAliasRetirementRecoverySnapshot is supplied only by an offline operator
// after verifying an immutable backup and the original durable receipt. The
// store validates bindings, not the provenance or contents of that evidence.
// Never bind this type or recovery method to a public HTTP route.
type PoolAliasRetirementRecoverySnapshot struct {
	OperationID string
	ClientID    string
	Project     string
	AliasID     int64
	AccountID   int64
	LeaseID     string
	Address     string
	EvidenceRef string
}

const poolAliasRetirementRecoveryAction = "pool.retirement.recover_snapshot"

func poolAliasRetirementRecoveryDigest(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	index := strings.LastIndex(ref, "sha256:")
	if index < 0 || len(ref) > 4096 {
		return "", ErrPoolInput
	}
	digest := ref[index+len("sha256:"):]
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 || len(digest) != 64 {
		return "", ErrPoolInput
	}
	return "sha256:" + strings.ToLower(digest), nil
}

// RecoverPoolAliasRetirementSnapshot closes an orphan used lease and restores
// its original one-item retirement job. It proves no Apple outcome and performs
// no remote mutation. Only normal full-directory reconciliation may retire it.
// EvidenceRef must end in sha256:<64 hex characters>; only that digest is kept.
func (s *Store) RecoverPoolAliasRetirementSnapshot(ctx context.Context, input PoolAliasRetirementRecoverySnapshot) (PoolAliasRetirementJob, bool, error) {
	items := []PoolAliasRetirementItemInput{{AliasID: input.AliasID, LeaseID: input.LeaseID}}
	if err := validatePoolAliasRetirementInput(input.OperationID, items); err != nil {
		return PoolAliasRetirementJob{}, false, err
	}
	parsed, err := mail.ParseAddress(input.Address)
	if input.AccountID < 1 || input.ClientID == "" || input.Project == "" || input.LeaseID != strings.TrimSpace(input.LeaseID) || err != nil || parsed.Name != "" || parsed.Address != input.Address || domain.NormalizeEmail(input.Address) != input.Address {
		return PoolAliasRetirementJob{}, false, ErrPoolInput
	}
	digest, err := poolAliasRetirementRecoveryDigest(input.EvidenceRef)
	if err != nil {
		return PoolAliasRetirementJob{}, false, err
	}
	tx, err := s.poolTx(ctx)
	if err != nil {
		return PoolAliasRetirementJob{}, false, err
	}
	defer tx.Rollback()
	existing, existingErr := s.poolAliasRetirementJobTx(ctx, tx, input.OperationID)
	if existingErr == nil {
		if existing.ClientID != input.ClientID || existing.Project != input.Project || len(existing.Items) != 1 {
			return PoolAliasRetirementJob{}, false, ErrPoolRequestConflict
		}
		item := existing.Items[0]
		if item.AliasID != input.AliasID || item.AccountID != input.AccountID || item.LeaseID != input.LeaseID || item.Address != input.Address {
			return PoolAliasRetirementJob{}, false, ErrPoolRequestConflict
		}
		var recordedDigest string
		if err := s.txQueryRowContext(ctx, tx, `SELECT detail FROM audit_logs WHERE action=? AND resource_type='pool_retirement_operation' AND resource_id=? ORDER BY id LIMIT 1`, poolAliasRetirementRecoveryAction, input.OperationID).Scan(&recordedDigest); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// A normal original job already carries the full snapshot. This
				// operator repair has no work to do and never rewrites that job.
				return existing, false, tx.Commit()
			}
			return PoolAliasRetirementJob{}, false, err
		}
		if recordedDigest != digest {
			return PoolAliasRetirementJob{}, false, ErrPoolRequestConflict
		}
		return existing, false, tx.Commit()
	}
	if !errors.Is(existingErr, ErrNotFound) {
		return PoolAliasRetirementJob{}, false, existingErr
	}
	var project string
	var enabled bool
	if err := s.txQueryRowContext(ctx, tx, `SELECT project,enabled FROM pool_clients WHERE id=?`, input.ClientID).Scan(&project, &enabled); err != nil {
		return PoolAliasRetirementJob{}, false, err
	}
	if project != input.Project {
		return PoolAliasRetirementJob{}, false, ErrPoolRetirementProject
	}
	if !enabled {
		return PoolAliasRetirementJob{}, false, ErrPoolClosed
	}
	lease, err := scanPoolLease(s.txQueryRowContext(ctx, tx, poolLeaseSelect+` WHERE l.id=?`, input.LeaseID))
	if err != nil {
		return PoolAliasRetirementJob{}, false, ErrPoolRetirementLeaseNotFound
	}
	if lease.ClientID != input.ClientID || lease.Project != input.Project || lease.Address != input.Address || lease.AliasID != 0 || lease.State != "used" {
		return PoolAliasRetirementJob{}, false, ErrPoolRetirementConflict
	}
	if _, err := s.lockAccountVersionForUpdate(ctx, tx, input.AccountID); err != nil {
		return PoolAliasRetirementJob{}, false, err
	}
	var count int
	if err := s.txQueryRowContext(ctx, tx, `SELECT COUNT(*) FROM aliases WHERE id=? OR LOWER(address)=?`, input.AliasID, input.Address).Scan(&count); err != nil {
		return PoolAliasRetirementJob{}, false, err
	}
	if count != 0 {
		return PoolAliasRetirementJob{}, false, ErrPoolRetirementConflict
	}
	if err := s.txQueryRowContext(ctx, tx, `SELECT COUNT(*) FROM pool_leases WHERE id<>? AND LOWER(address)=? AND state IN ('leased','used','retiring')`, input.LeaseID, input.Address).Scan(&count); err != nil {
		return PoolAliasRetirementJob{}, false, err
	}
	if count != 0 {
		return PoolAliasRetirementJob{}, false, ErrPoolRetirementConflict
	}
	// Even completed attempts retain their original identity. A receipt already
	// belonging to another job must be reconciled through that existing job.
	if err := s.txQueryRowContext(ctx, tx, `SELECT COUNT(*) FROM pool_alias_retirement_job_items WHERE lease_id=? OR alias_id=? OR LOWER(address)=?`, input.LeaseID, input.AliasID, input.Address).Scan(&count); err != nil {
		return PoolAliasRetirementJob{}, false, err
	}
	if count != 0 {
		return PoolAliasRetirementJob{}, false, ErrPoolRetirementConflict
	}
	fingerprint, err := poolAliasRetirementFingerprint(items)
	if err != nil {
		return PoolAliasRetirementJob{}, false, err
	}
	now := s.now().UTC()
	job := PoolAliasRetirementJob{OperationID: input.OperationID, ClientID: input.ClientID, Project: input.Project, Status: PoolAliasRetirementJobReview, RequestID: "operator-snapshot-recovery", CreatedAt: now, UpdatedAt: now,
		Items: []PoolAliasRetirementJobItem{{Ordinal: 0, AliasID: input.AliasID, AccountID: input.AccountID, LeaseID: input.LeaseID, Address: input.Address, State: "review", ResultCode: "OPERATOR_SNAPSHOT_RECOVERED", ResultMessage: "原退役快照已据证恢复，等待 Apple 完整目录核验"}}}
	if _, err := s.txExecContext(ctx, tx, `INSERT INTO pool_alias_retirement_jobs(operation_id,client_id,project,fingerprint,status,request_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, job.OperationID, job.ClientID, job.Project, fingerprint, job.Status, job.RequestID, timestamp(now), timestamp(now)); err != nil {
		return PoolAliasRetirementJob{}, false, err
	}
	item := job.Items[0]
	if _, err := s.txExecContext(ctx, tx, `INSERT INTO pool_alias_retirement_job_items(operation_id,ordinal,alias_id,lease_id,account_id,address,state,result_code,result_message,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, job.OperationID, item.Ordinal, item.AliasID, item.LeaseID, item.AccountID, item.Address, item.State, item.ResultCode, item.ResultMessage, timestamp(now)); err != nil {
		return PoolAliasRetirementJob{}, false, err
	}
	result, err := s.txExecContext(ctx, tx, `UPDATE pool_leases SET state='retiring',updated_at=? WHERE id=? AND client_id=? AND address=? AND alias_id IS NULL AND state='used'`, timestamp(now), input.LeaseID, input.ClientID, input.Address)
	if err != nil {
		return PoolAliasRetirementJob{}, false, err
	}
	if err := requireAffected(result, "recovered orphan retirement lease"); err != nil {
		return PoolAliasRetirementJob{}, false, ErrPoolRetirementConflict
	}
	if _, err := s.createAuditLogTx(ctx, tx, domain.AuditLog{Username: "offline-operator", Action: poolAliasRetirementRecoveryAction, ResourceType: "pool_retirement_operation", ResourceID: input.OperationID, Result: "review", Detail: digest, CreatedAt: now}); err != nil {
		return PoolAliasRetirementJob{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return PoolAliasRetirementJob{}, false, err
	}
	return job, true, nil
}
