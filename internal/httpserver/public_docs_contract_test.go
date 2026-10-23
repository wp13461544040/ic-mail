package httpserver

import (
	"net/http"
	"strings"
	"testing"
)

func TestPublicDocsCoversCurrentPickupPoolAndRetirement(t *testing.T) {
	env := newAdminAPITestEnv(t)
	env.server.cfg.AdminPath = "/fixture-private-admin"
	router, err := env.server.Router()
	if err != nil {
		t.Fatal(err)
	}
	response := serveV2Request(router, http.MethodGet, "/docs/", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("docs HTTP status=%d", response.Code)
	}
	body := response.Body.String()
	for _, contract := range []string{
		"2026-10-22-automatic-login-events", "v0.2.8", "ICLOUD_API_OTP_RETURN_LATEST_ONLY", "/api/v1/pool/claim",
		"/leases/LEASE_ID/commit", "wait_seconds=15", "LEASE_CLOSED", "root+tag",
		"/aliases/batch/jobs/OPERATION_ID/reconcile", "X-CSRF-Token", "IDEMPOTENCY_CONFLICT",
		"resume=false", "APPLE_DELETE_UNCERTAIN", "retiring", "retired", "IMAP 网络读取可并行",
		"被拦截刷新不扣额度、不重置恢复时间", "X-Retry-After-Ms", "/oauth2/v2.0/token",
		"自动创建与通道恢复", "旧通道每小时最多 5 次", "自动恢复 19＋4", "历史关闭项不会在升级时批量重开",
		"服务默认每批最多处理 128 条 UID", "保留归档身份与取码记录", "手动与批量创建按实际使用的通道确认原候选",
		"503 PICKUP_BUSY", "分页及重启继续原窗口", "并发本身不返回 429",
		"自动保活与过期重登", "不每 30 秒扫描", "重复事件不重置退避", "密码和接码链接加密保存",
	} {
		if !strings.Contains(body, contract) {
			t.Errorf("public docs missing contract %q", contract)
		}
	}
	for _, forbidden := range []string{"/accounts/available", "<script", "src=\"https://", "href=\"https://", env.server.cfg.AdminPath} {
		if forbidden != "" && strings.Contains(body, forbidden) {
			t.Errorf("public docs contains unshipped/private/external resource marker %q", forbidden)
		}
	}
}
