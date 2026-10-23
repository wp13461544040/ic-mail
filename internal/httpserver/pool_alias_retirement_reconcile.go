package httpserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/apple"
	"icloud-api/internal/hmesync"
	"icloud-api/internal/store"
)

type hmeRetirementReconciler interface {
	ReconcileRetirementAliases(context.Context, []hmesync.AliasRetirementTarget) ([]hmesync.AliasRetirementObservation, error)
}

type poolRetirementReconcileRequest struct {
	Items  []store.PoolAliasRetirementItemInput `json:"items"`
	Resume *bool                                `json:"resume"`
}

func (s *Server) adminAPIPoolAliasRetirementReconcile(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var input poolRetirementReconcileRequest
	if !decodeAdminAPIJSON(c, &input) {
		return
	}
	if input.Resume == nil {
		writeAdminAPIError(c, http.StatusBadRequest, "INVALID_POOL_REQUEST", "请明确指定仅核验或恢复删除")
		return
	}
	resume := *input.Resume
	client := poolClient(c)
	job, err := s.store.GetPoolAliasRetirementJob(c.Request.Context(), c.Param("operationID"), client.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeAdminAPIError(c, http.StatusNotFound, "NOT_FOUND", "原删除任务不存在")
		return
	}
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	if !poolAliasRetirementItemsMatch(job, input.Items) {
		writeAdminAPIError(c, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "恢复项目与原删除工单不一致")
		return
	}
	allRetired := job.Status == store.PoolAliasRetirementJobCompleted
	for _, item := range job.Items {
		allRetired = allRetired && item.State == "retired"
	}
	if allRetired {
		writeAdminAPIData(c, http.StatusAccepted, poolAliasRetirementDTO(job))
		return
	}
	reconciler, ok := s.hmeSync.(hmeRetirementReconciler)
	if !ok {
		writeAdminAPIError(c, http.StatusServiceUnavailable, "POOL_RETIREMENT_UNAVAILABLE", "原工单核验服务尚未就绪")
		return
	}
	runtime := &s.poolAliasRetirementJobs
	runtime.mu.Lock()
	if runtime.ctx == nil || runtime.stopping || runtime.ctx.Err() != nil {
		runtime.mu.Unlock()
		writeAdminAPIError(c, http.StatusServiceUnavailable, "POOL_RETIREMENT_UNAVAILABLE", "后台删除服务尚未启动或正在关闭")
		return
	}
	if _, active := runtime.active[job.OperationID]; active {
		if mode, reconciling := runtime.reconcileResume[job.OperationID]; reconciling && mode != resume {
			runtime.mu.Unlock()
			writeAdminAPIError(c, http.StatusConflict, "RETIREMENT_ACTION_CONFLICT", "原工单正在执行不同的核验操作，请等待结束")
			return
		}
		runtime.mu.Unlock()
		writeAdminAPIData(c, http.StatusAccepted, poolAliasRetirementDTO(job))
		return
	}
	job, acquired, err := s.store.AcquirePoolAliasRetirementReconcile(c.Request.Context(), client.ID, job.OperationID, input.Items)
	if err != nil {
		runtime.mu.Unlock()
		s.poolAliasRetirementError(c, err)
		return
	}
	if !acquired {
		runtime.mu.Unlock()
		writeAdminAPIData(c, http.StatusAccepted, poolAliasRetirementDTO(job))
		return
	}
	runtime.active[job.OperationID] = struct{}{}
	runtime.reconcileResume[job.OperationID] = resume
	runtime.wg.Add(1)
	parent := runtime.ctx
	runtime.mu.Unlock()
	go func() {
		defer func() {
			runtime.mu.Lock()
			delete(runtime.active, job.OperationID)
			delete(runtime.reconcileResume, job.OperationID)
			runtime.mu.Unlock()
			runtime.wg.Done()
		}()
		s.runPoolAliasRetirementReconcile(parent, client, job, resume, reconciler)
	}()
	writeAdminAPIData(c, http.StatusAccepted, poolAliasRetirementDTO(job))
}

func (s *Server) runPoolAliasRetirementReconcile(parent context.Context, client store.PoolClient, job store.PoolAliasRetirementJob, resume bool, reconciler hmeRetirementReconciler) {
	ctx, cancel := context.WithTimeout(parent, poolAliasRetirementJobTimeout)
	defer cancel()
	finished := false
	defer func() {
		if recover() != nil {
			s.logger.Error("原退役工单核验中断", "operation", "pool_retirement_reconcile")
		}
		if !finished {
			// Review items also need a durable interrupted job after cancellation.
			s.interruptPoolAliasRetirementJob(job, client, "JOB_INTERRUPTED", "原退役工单核验中断，远端结果待核对")
		}
	}()
	targets := make([]hmesync.AliasRetirementTarget, 0, len(job.Items))
	for _, item := range job.Items {
		if item.State != "retired" {
			targets = append(targets, hmesync.AliasRetirementTarget{AliasID: item.AliasID, AccountID: item.AccountID, Address: item.Address})
		}
	}
	observations, runErr := reconciler.ReconcileRetirementAliases(ctx, targets)
	byAlias := make(map[int64]hmesync.AliasRetirementObservation, len(observations))
	for _, observation := range observations {
		byAlias[observation.AliasID] = observation
	}
	toResume := false
	for _, item := range job.Items {
		if item.State == "retired" {
			continue
		}
		observation, found := byAlias[item.AliasID]
		if !found || observation.Err != nil || runErr != nil {
			code := "APPLE_DELETE_UNCERTAIN"
			if observation.Err != nil {
				code = poolAliasRetirementOutcomeCode(observation.Err)
			}
			var upstream *apple.Error
			if errors.As(observation.Err, &upstream) {
				s.logger.Warn("原退役工单 Apple 核验尚未确认", "operation", "pool_retirement_reconcile", "code", code,
					"apple_operation", upstream.Op, "http_status", upstream.StatusCode, "service_code", upstream.ServiceCode,
					"retry_after_seconds", apple.RetryDelay(observation.Err).Seconds())
			}
			s.recordPoolRetirementReconcileReview(ctx, client, job, item.Ordinal, code, "Apple 完整目录核验尚未确认，原租约保持关闭")
			continue
		}
		if !observation.Present {
			if err := s.withAccountLock(ctx, item.AccountID, func() error {
				return s.store.FinalizeReconciledPoolAliasRetirement(ctx, client.ID, job.OperationID, item.Ordinal)
			}); err != nil {
				s.recordPoolRetirementReconcileReview(ctx, client, job, item.Ordinal, "POOL_RETIREMENT_CONFLICT", "远端目标已不存在，本地原绑定需核对")
			}
			continue
		}
		if !resume {
			s.recordPoolRetirementReconcileReview(ctx, client, job, item.Ordinal, "APPLE_ALIAS_STILL_PRESENT", "Apple 完整目录确认原隐藏邮箱仍存在，等待显式继续")
			continue
		}
		if err := s.store.PreparePoolAliasRetirementResume(ctx, client.ID, job.OperationID, item.Ordinal); err != nil {
			s.recordPoolRetirementReconcileReview(ctx, client, job, item.Ordinal, "POOL_RETIREMENT_CONFLICT", "原删除绑定已变化，保持待核对")
			continue
		}
		toResume = true
	}
	if ctx.Err() != nil {
		return
	}
	if toResume {
		current, err := s.store.GetPoolAliasRetirementJob(ctx, job.OperationID, client.ID)
		if err != nil {
			return
		}
		s.runPoolAliasRetirementJob(ctx, client, current)
		finished = true
		return
	}
	persist, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	if err := s.store.CompletePoolAliasRetirementReconcile(persist, client.ID, job.OperationID); err != nil {
		return
	}
	finished = true
}

func (s *Server) recordPoolRetirementReconcileReview(ctx context.Context, client store.PoolClient, job store.PoolAliasRetirementJob, ordinal int, code, message string) {
	if err := s.store.RecordPoolAliasRetirementReconcileReview(ctx, client.ID, job.OperationID, ordinal, code, message); err != nil {
		s.logger.Warn("原退役核验结果保存失败", "operation", "pool_retirement_reconcile", "code", "POOL_RETIREMENT_CONFLICT")
	}
}
