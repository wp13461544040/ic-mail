package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"icloud-api/internal/hmesync"
	"icloud-api/internal/store"
)

type fakeRetirementReconciler struct {
	*fakeHMESyncService
	observe func(context.Context, []hmesync.AliasRetirementTarget) ([]hmesync.AliasRetirementObservation, error)
}

func TestPoolRetirementReconcileActiveModeConflictAndCancellation(t *testing.T) {
	env := newAdminAPITestEnv(t)
	startTestPoolAliasRetirementJobs(t, env)
	client, key, lease := poolAliasRetirementUsedLease(t, env)
	cookie, csrf, _ := env.createSession(t, "reconcile-active-admin", "password")
	op := "retirement-reconcile-active-0001"
	items := []store.PoolAliasRetirementItemInput{{AliasID: lease.AliasID, LeaseID: lease.ID}}
	if _, _, err := env.store.StartPoolAliasRetirement(context.Background(), client.ID, op, "fixture", items); err != nil {
		t.Fatal(err)
	}
	if err := env.store.MarkPoolAliasRetirementReview(context.Background(), op, 0, "APPLE_DELETE_UNCERTAIN", "fixture"); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	env.server.SetHMESyncService(&fakeRetirementReconciler{fakeHMESyncService: &fakeHMESyncService{}, observe: func(_ context.Context, targets []hmesync.AliasRetirementTarget) ([]hmesync.AliasRetirementObservation, error) {
		reads.Add(1)
		close(entered)
		<-release
		return []hmesync.AliasRetirementObservation{{AliasID: targets[0].AliasID, Err: context.Canceled}}, nil
	}})
	t.Cleanup(func() { close(release) })
	path := "/admin/api/v1/pool/aliases/batch/jobs/" + op + "/reconcile"
	for index, resume := range []bool{false, false, true} {
		r := callPoolAliasRetirement(t, env, http.MethodPost, path, key, map[string]any{"items": items, "resume": resume}, cookie, csrf)
		if index == 2 {
			if r.Code != 409 || adminAPITestErrorCode(t, r) != "RETIREMENT_ACTION_CONFLICT" {
				t.Fatalf("mode conflict=%d", r.Code)
			}
		} else if r.Code != 202 {
			t.Fatalf("same mode=%d", r.Code)
		}
		if index == 0 {
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("worker did not start")
			}
		}
	}
	if reads.Load() != 1 {
		t.Fatalf("duplicate directory reads=%d", reads.Load())
	}
}

func (f *fakeRetirementReconciler) ReconcileRetirementAliases(ctx context.Context, targets []hmesync.AliasRetirementTarget) ([]hmesync.AliasRetirementObservation, error) {
	return f.observe(ctx, targets)
}

func TestPoolRetirementReconcileDualAuthAndExternalDeletion(t *testing.T) {
	env := newAdminAPITestEnv(t)
	startTestPoolAliasRetirementJobs(t, env)
	client, key, lease := poolAliasRetirementUsedLease(t, env)
	cookie, csrf, _ := env.createSession(t, "retirement-reconcile-admin", "password")
	op := "retirement-reconcile-auth-0001"
	items := []store.PoolAliasRetirementItemInput{{AliasID: lease.AliasID, LeaseID: lease.ID}}
	if _, _, err := env.store.StartPoolAliasRetirement(context.Background(), client.ID, op, "fixture", items); err != nil {
		t.Fatal(err)
	}
	if err := env.store.MarkPoolAliasRetirementReview(context.Background(), op, 0, "APPLE_UPSTREAM_ERROR", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := env.store.DeleteAlias(context.Background(), lease.AliasID); err != nil {
		t.Fatal(err)
	}
	var reads, deletes atomic.Int32
	env.server.SetHMESyncService(&fakeRetirementReconciler{fakeHMESyncService: &fakeHMESyncService{deleteAliases: func(context.Context, []int64) ([]hmesync.AliasDeletionOutcome, error) {
		deletes.Add(1)
		return nil, errors.New("unexpected delete")
	}}, observe: func(_ context.Context, targets []hmesync.AliasRetirementTarget) ([]hmesync.AliasRetirementObservation, error) {
		reads.Add(1)
		return []hmesync.AliasRetirementObservation{{AliasID: targets[0].AliasID, Present: false}}, nil
	}})
	path := "/admin/api/v1/pool/aliases/batch/jobs/" + op + "/reconcile"
	body := map[string]any{"items": items, "resume": false}
	for _, tc := range []struct {
		key    string
		cookie *http.Cookie
		csrf   string
		status int
	}{{key, nil, "", 401}, {"bad", cookie, csrf, 401}, {key, cookie, "", 403}} {
		if r := callPoolAliasRetirement(t, env, http.MethodPost, path, tc.key, body, tc.cookie, tc.csrf); r.Code != tc.status {
			t.Fatalf("auth=%d want %d", r.Code, tc.status)
		}
	}
	_, otherKey, err := env.store.CreatePoolClient(context.Background(), "other-reconcile-project")
	if err != nil {
		t.Fatal(err)
	}
	if r := callPoolAliasRetirement(t, env, http.MethodPost, path, otherKey, body, cookie, csrf); r.Code != 404 {
		t.Fatalf("foreign=%d", r.Code)
	}
	changed := map[string]any{"resume": false, "items": []store.PoolAliasRetirementItemInput{{AliasID: lease.AliasID + 1, LeaseID: lease.ID}}}
	if r := callPoolAliasRetirement(t, env, http.MethodPost, path, key, map[string]any{"items": items}, cookie, csrf); r.Code != 400 {
		t.Fatalf("missing explicit mode=%d", r.Code)
	}
	if r := callPoolAliasRetirement(t, env, http.MethodPost, path, key, changed, cookie, csrf); r.Code != 409 || adminAPITestErrorCode(t, r) != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("changed=%d %s", r.Code, r.Body.String())
	}
	r := callPoolAliasRetirement(t, env, http.MethodPost, path, key, body, cookie, csrf)
	if r.Code != 202 {
		t.Fatalf("reconcile=%d %s", r.Code, r.Body.String())
	}
	for _, field := range []string{"api_key", "cookie", "imap_password", "refresh_token", "address", "account_id"} {
		if strings.Contains(r.Body.String(), field) {
			t.Fatalf("response exposes %s", field)
		}
	}
	job := waitPoolAliasRetirementJob(t, env, op, client.ID)
	if job.Status != "completed" || job.Items[0].State != "retired" {
		t.Fatalf("result=%s/%s", job.Status, job.Items[0].State)
	}
	got, err := env.store.GetPoolLease(context.Background(), lease.ID, client.ID)
	if err != nil || got.State != "retired" || got.AliasID != 0 {
		t.Fatalf("lease=%s err=%v", got.State, err)
	}
	if r := callPoolAliasRetirement(t, env, http.MethodPost, path, key, body, cookie, csrf); r.Code != 202 {
		t.Fatalf("replay=%d", r.Code)
	}
	if reads.Load() != 1 || deletes.Load() != 0 {
		t.Fatalf("reads=%d deletes=%d", reads.Load(), deletes.Load())
	}
}

func TestPoolRetirementReconcilePresenceNeedsExplicitResume(t *testing.T) {
	for _, tc := range []struct {
		name      string
		present   bool
		remoteErr error
		resume    bool
		want      string
		deletes   int32
	}{{"present-read-only", true, nil, false, "review", 0}, {"unknown", false, hmesync.ErrUpstream, true, "review", 0}, {"resume", true, nil, true, "retired", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			env := newAdminAPITestEnv(t)
			startTestPoolAliasRetirementJobs(t, env)
			client, key, lease := poolAliasRetirementUsedLease(t, env)
			cookie, csrf, _ := env.createSession(t, "reconcile-test-admin", "password")
			op := "retirement-reconcile-state-0001"
			items := []store.PoolAliasRetirementItemInput{{AliasID: lease.AliasID, LeaseID: lease.ID}}
			if _, _, err := env.store.StartPoolAliasRetirement(context.Background(), client.ID, op, "fixture", items); err != nil {
				t.Fatal(err)
			}
			if err := env.store.MarkPoolAliasRetirementReview(context.Background(), op, 0, "APPLE_DELETE_UNCERTAIN", "fixture"); err != nil {
				t.Fatal(err)
			}
			var deleted atomic.Int32
			env.server.SetHMESyncService(&fakeRetirementReconciler{fakeHMESyncService: &fakeHMESyncService{deleteAliases: func(_ context.Context, ids []int64) ([]hmesync.AliasDeletionOutcome, error) {
				deleted.Add(1)
				return []hmesync.AliasDeletionOutcome{{AliasID: ids[0]}}, nil
			}}, observe: func(_ context.Context, targets []hmesync.AliasRetirementTarget) ([]hmesync.AliasRetirementObservation, error) {
				return []hmesync.AliasRetirementObservation{{AliasID: targets[0].AliasID, Present: tc.present, Err: tc.remoteErr}}, nil
			}})
			path := "/admin/api/v1/pool/aliases/batch/jobs/" + op + "/reconcile"
			if r := callPoolAliasRetirement(t, env, http.MethodPost, path, key, map[string]any{"items": items, "resume": tc.resume}, cookie, csrf); r.Code != 202 {
				t.Fatalf("response=%d %s", r.Code, r.Body.String())
			}
			job := waitPoolAliasRetirementJob(t, env, op, client.ID)
			if job.Items[0].State != tc.want || deleted.Load() != tc.deletes {
				t.Fatalf("state=%s deletes=%d", job.Items[0].State, deleted.Load())
			}
		})
	}
}
