package httpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestPickupBusyLogsAreSampledWithoutHidingSyncFailures(t *testing.T) {
	env := newAdminAPITestEnv(t)
	var output bytes.Buffer
	env.server.logger = slog.New(slog.NewJSONHandler(&output, nil))
	router := gin.New()
	router.Use(env.server.requestContext())
	router.GET("/api/v1/otp", func(c *gin.Context) {
		env.server.rejectMailboxPickup(c, "global_queue", "fixture busy", time.Second)
	})
	router.GET("/api/v1/mail/latest", func(c *gin.Context) {
		env.server.writeAPIError(c, http.StatusServiceUnavailable, "SYNC_UNAVAILABLE", "fixture upstream failure")
	})
	for i := 0; i < 3; i++ {
		serveV2Request(router, http.MethodGet, "/api/v1/otp", "", nil)
		serveV2Request(router, http.MethodGet, "/api/v1/mail/latest", "", nil)
	}
	warnings, failures := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		switch entry["level"] {
		case "WARN":
			warnings++
			if entry["limit_reason"] != "global_queue" || entry["retry_after_ms"] != "1000" {
				t.Fatal("busy log lost its cause or retry hint")
			}
		case "ERROR":
			failures++
		}
	}
	if warnings != 1 || failures != 3 {
		t.Fatalf("busy logs=%d upstream failure logs=%d, want 1/3", warnings, failures)
	}
}

func TestRequestSamplerAllowAndSuppression(t *testing.T) {
	s := newRequestSampler()
	now := time.Unix(100, 0)
	if ok, n := s.allow("GET /x", now); !ok || n != 0 {
		t.Fatalf("first = %v,%d", ok, n)
	}
	for i := 0; i < 3; i++ {
		if ok, _ := s.allow("GET /x", now.Add(time.Second)); ok {
			t.Fatal("duplicate allowed")
		}
	}
	if ok, n := s.allow("GET /x", now.Add(61*time.Second)); !ok || n != 3 {
		t.Fatalf("next window = %v,%d", ok, n)
	}
}

func TestRequestSamplerBoundedAndConcurrent(t *testing.T) {
	s := newRequestSampler()
	now := time.Unix(200, 0)
	for i := 0; i < 3000; i++ {
		s.allow(fmt.Sprintf("k-%d", i), now)
	}
	if len(s.items) > 2048 {
		t.Fatalf("sampler size %d", len(s.items))
	}
	s = newRequestSampler()
	var wg sync.WaitGroup
	allowed := 0
	var mu sync.Mutex
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _ := s.allow("same", now)
			if ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 1 {
		t.Fatalf("concurrent allowed = %d", allowed)
	}
}

func TestHttpOperationRouteMap(t *testing.T) {
	cases := []struct{ method, path, want string }{
		{"DELETE", "/admin/api/v1/accounts/:id", "删除主号"},
		{"GET", "/admin/api/v1/accounts", "读取主号列表"},
		{"POST", "/admin/api/v1/pool/members", "添加邮箱池成员"},
		{"POST", "/api/v1/pool/claim", "领取邮箱"},
		{"GET", "/admin/api/v1/unknown", "其他请求"},
	}
	for _, tc := range cases {
		if got := httpOperation(tc.path, tc.method); got != tc.want {
			t.Errorf("%s %s = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}
