package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestSharedPickupQueueReservesReadsEvenWhenWaitingIngressIsFull(t *testing.T) {
	env := newAdminAPITestEnv(t)
	router := gin.New()
	router.Use(env.server.sharedPickupAdmissionWithCapacity(4, 2, 2, time.Second))
	entered := make(chan string, 8)
	release := make(chan struct{})
	var once sync.Once
	var workers sync.WaitGroup
	t.Cleanup(func() { once.Do(func() { close(release) }); workers.Wait() })
	router.GET("/api/v1/pool/leases/:leaseID/code", func(c *gin.Context) {
		entered <- c.Param("leaseID")
		select {
		case <-release:
			c.Status(http.StatusOK)
		case <-c.Request.Context().Done():
		}
	})
	router.GET("/api/v1/otp", func(c *gin.Context) { c.Status(http.StatusOK) })
	start := func(id string, ctx context.Context) <-chan *httptest.ResponseRecorder {
		result := make(chan *httptest.ResponseRecorder, 1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/pool/leases/"+id+"/code?wait_seconds=15", nil).WithContext(ctx)
			r := httptest.NewRecorder()
			router.ServeHTTP(r, req)
			result <- r
		}()
		return result
	}
	for _, id := range []string{"first", "second"} {
		start(id, context.Background())
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("waiter did not enter")
		}
	}
	// One queued wait fills the remaining long-wait ticket. A second one must
	// report busy while ordinary reads still have reserved ingress capacity.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	third, fourth := start("third", ctx), start("fourth", ctx)
	var queued <-chan *httptest.ResponseRecorder
	var overflow *httptest.ResponseRecorder
	select {
	case overflow = <-third:
		queued = fourth
	case overflow = <-fourth:
		queued = third
	case <-time.After(time.Second):
		t.Fatal("waiting ingress did not fill")
	}
	if overflow.Code != http.StatusServiceUnavailable || overflow.Header().Get("X-RateLimit-Reason") != "wait_queue" {
		t.Fatalf("waiting overflow: %d %s", overflow.Code, overflow.Body.String())
	}
	if r := serveV2Request(router, http.MethodGet, "/api/v1/otp", "", nil); r.Code != http.StatusOK {
		t.Fatalf("queued long waits blocked ordinary pickup: %d", r.Code)
	}
	cancel()
	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("queued cancellation did not return")
	}
	once.Do(func() { close(release) })
	workers.Wait()
	if r := serveV2Request(router, http.MethodGet, "/api/v1/pool/leases/after/code?wait_seconds=15", "", nil); r.Code != http.StatusOK {
		t.Fatalf("cancelled queue leaked tickets: %d", r.Code)
	}
}

func TestSharedPickupQueueWaitsForSlotAndReportsBusyWithout429(t *testing.T) {
	for _, freeSlot := range []bool{true, false} {
		t.Run(map[bool]string{true: "resumes", false: "timeout"}[freeSlot], func(t *testing.T) {
			env := newAdminAPITestEnv(t)
			router := gin.New()
			router.Use(env.server.sharedPickupAdmissionWithCapacity(1, 1, 1, 500*time.Millisecond))
			entered := make(chan struct{}, 4)
			release := make(chan struct{})
			var once sync.Once
			var workers sync.WaitGroup
			t.Cleanup(func() { once.Do(func() { close(release) }); workers.Wait() })
			router.GET("/api/v1/otp", func(c *gin.Context) { entered <- struct{}{}; <-release; c.Status(200) })
			start := func() <-chan *httptest.ResponseRecorder {
				result := make(chan *httptest.ResponseRecorder, 1)
				workers.Add(1)
				go func() { defer workers.Done(); result <- serveV2Request(router, http.MethodGet, "/api/v1/otp", "", nil) }()
				return result
			}
			first := start()
			<-entered
			second, third := start(), start()
			var pending <-chan *httptest.ResponseRecorder
			var overflow *httptest.ResponseRecorder
			select {
			case overflow = <-second:
				pending = third
			case overflow = <-third:
				pending = second
			case <-time.After(time.Second):
				t.Fatal("queue overflow did not return")
			}
			if overflow.Code != 503 || overflow.Header().Get("X-RateLimit-Reason") != "global_queue" {
				t.Fatalf("queue overflow: %d %s", overflow.Code, overflow.Body.String())
			}
			if freeSlot {
				once.Do(func() { close(release) })
			}
			select {
			case r := <-pending:
				if freeSlot && r.Code != 200 {
					t.Fatalf("queued request did not resume: %d", r.Code)
				}
				if !freeSlot && (r.Code != 503 || r.Header().Get("X-RateLimit-Reason") != "global_concurrency" || !strings.Contains(r.Body.String(), "PICKUP_BUSY")) {
					t.Fatalf("capacity returned incorrect status: %d %s", r.Code, r.Body.String())
				}
			case <-time.After(time.Second):
				t.Fatal("bounded queue did not complete")
			}
			once.Do(func() { close(release) })
			<-first
		})
	}
}

func TestSharedPickupQueueFullIngressReleasesWaitingTicket(t *testing.T) {
	env := newAdminAPITestEnv(t)
	router := gin.New()
	router.Use(env.server.sharedPickupAdmissionWithCapacity(1, 1, 0, time.Second))
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	var workers sync.WaitGroup
	t.Cleanup(func() { once.Do(func() { close(release) }); workers.Wait() })
	router.GET("/api/v1/otp", func(c *gin.Context) { entered <- struct{}{}; <-release; c.Status(200) })
	router.GET("/api/v1/pool/leases/:leaseID/code", func(c *gin.Context) { c.Status(200) })
	workers.Add(1)
	go func() { defer workers.Done(); serveV2Request(router, http.MethodGet, "/api/v1/otp", "", nil) }()
	<-entered
	// With no ingress queue, the first waiting request gets a waiter ticket
	// and then fails at the full global ticket. That ticket must be released.
	r := serveV2Request(router, http.MethodGet, "/api/v1/pool/leases/first/code?wait_seconds=15", "", nil)
	if r.Code != 503 || r.Header().Get("X-RateLimit-Reason") != "global_queue" {
		t.Fatalf("full global ingress: %d %s", r.Code, r.Body.String())
	}
	once.Do(func() { close(release) })
	workers.Wait()
	if r := serveV2Request(router, http.MethodGet, "/api/v1/pool/leases/after/code?wait_seconds=15", "", nil); r.Code != 200 {
		t.Fatalf("failed global ingress leaked waiter capacity: %d", r.Code)
	}
}

func TestSharedPickupQueueCancellationAfterWaitingSlotDoesNotLeakCapacity(t *testing.T) {
	env := newAdminAPITestEnv(t)
	router := gin.New()
	router.Use(env.server.sharedPickupAdmissionWithCapacity(1, 1, 2, time.Second))
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	var workers sync.WaitGroup
	t.Cleanup(func() { once.Do(func() { close(release) }); workers.Wait() })
	router.GET("/api/v1/otp", func(c *gin.Context) { entered <- struct{}{}; <-release; c.Status(200) })
	called := false
	router.GET("/api/v1/pool/leases/:leaseID/code", func(c *gin.Context) { called = true; c.Status(200) })
	workers.Add(1)
	go func() { defer workers.Done(); serveV2Request(router, http.MethodGet, "/api/v1/otp", "", nil) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pool/leases/cancel/code?wait_seconds=15", nil).WithContext(ctx)
	router.ServeHTTP(httptest.NewRecorder(), req)
	if ctx.Err() != context.DeadlineExceeded || called {
		t.Fatal("request did not cancel while queued behind ordinary slot")
	}
	once.Do(func() { close(release) })
	workers.Wait()
	if r := serveV2Request(router, http.MethodGet, "/api/v1/pool/leases/after/code?wait_seconds=15", "", nil); r.Code != 200 || !called {
		t.Fatalf("cancelled second-stage queue leaked capacity: %d", r.Code)
	}
}
