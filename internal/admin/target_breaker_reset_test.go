package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
)

// Exercise the admin consumer through persistence, ApplyConfig and a real local
// upstream. No failure state is injected into the proxy's private internals.
type adminBreakerFixture struct {
	h       http.Handler
	service *proxy.Service
	calls   atomic.Int64
	fail    atomic.Bool
	lastKey atomic.Value
	url     string
}

func newAdminBreakerFixture(t *testing.T) *adminBreakerFixture {
	t.Helper()
	f := &adminBreakerFixture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.lastKey.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if f.fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"fake unavailable","type":"server_error"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"fake","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
	}))
	t.Cleanup(upstream.Close)
	f.url = upstream.URL
	f.h, _, _, f.service = newTargetUpdateTestHandlerWithService(t, t.TempDir(), []string{"sk-fake-before"})
	t.Cleanup(func() { _ = f.service.Close() })
	f.update(t, "target-1", map[string]any{
		"endpoint_type": "openai", "endpoint": upstream.URL,
		"model_mappings": []map[string]any{{"upstream": "qwen-max"}},
	})
	return f
}

func (f *adminBreakerFixture) update(t *testing.T, name string, body map[string]any) {
	t.Helper()
	if rec := putTarget(t, f.h, name, body); rec.Code != http.StatusOK {
		t.Fatalf("PUT target %s: HTTP %d %s", name, rec.Code, rec.Body.String())
	}
}

func (f *adminBreakerFixture) mutate(t *testing.T, path string) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/"+path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT %s: HTTP %d %s", path, rec.Code, rec.Body.String())
	}
}

func (f *adminBreakerFixture) request(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions",
		strings.NewReader(`{"model":"qwen-max","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer k1")
	req.RemoteAddr = "192.0.2.1:1234"
	h := f.h.(*Handler)
	rec := httptest.NewRecorder()
	auth.Middleware(h.authStore, h.logger)(f.service).ServeHTTP(rec, req)
	return rec
}

func (f *adminBreakerFixture) status(t *testing.T) proxy.TargetStatus {
	t.Helper()
	for _, status := range f.service.TargetStatuses(time.Now()) {
		if status.Name == "target-1" {
			return status
		}
	}
	t.Fatal("target-1 missing from runtime")
	return proxy.TargetStatus{}
}

func (f *adminBreakerFixture) trip(t *testing.T) proxy.TargetStatus {
	t.Helper()
	f.fail.Store(true)
	if rec := f.request(t); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("first fake failure: HTTP %d %s", rec.Code, rec.Body.String())
	}
	f.fail.Store(false)
	status := f.status(t)
	if !status.Muted || status.Phase != "muted" || status.ConsecutiveFailures == 0 {
		t.Fatalf("upstream failure did not mute target: %+v", status)
	}
	f.assertMuted(t, status)
	return status
}

func (f *adminBreakerFixture) assertMuted(t *testing.T, previous proxy.TargetStatus) {
	t.Helper()
	current := f.status(t)
	if !current.Muted || current.Phase != "muted" || !current.MutedUntil.Equal(previous.MutedUntil) || current.ConsecutiveFailures != previous.ConsecutiveFailures {
		t.Fatalf("unrelated edit changed breaker: before=%+v after=%+v", previous, current)
	}
	before := f.calls.Load()
	if rec := f.request(t); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "all targets muted") {
		t.Fatalf("expected local muted rejection: HTTP %d %s", rec.Code, rec.Body.String())
	}
	if f.calls.Load() != before {
		t.Fatal("muted target made an upstream request")
	}
}

func (f *adminBreakerFixture) assertReadyAndForward(t *testing.T, previous proxy.TargetStatus) {
	t.Helper()
	current := f.status(t)
	if current.Paused || current.Muted || current.Phase != "ready" || !current.MutedUntil.IsZero() || current.ConsecutiveFailures != 0 {
		t.Fatalf("expected immediately ready target: %+v", current)
	}
	if current.TotalFailedRequests != previous.TotalFailedRequests || current.BreakerTrips != previous.BreakerTrips {
		t.Fatalf("reset lost accumulated counters: before=%+v after=%+v", previous, current)
	}
	before := f.calls.Load()
	if rec := f.request(t); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"usage"`) {
		t.Fatalf("request after reset: HTTP %d %s", rec.Code, rec.Body.String())
	}
	if f.calls.Load() != before+1 {
		t.Fatalf("expected one actual upstream request, calls=%d before=%d", f.calls.Load(), before)
	}
}

func TestHandlerTargetIdentityChangeResetsBreaker(t *testing.T) {
	f := newAdminBreakerFixture(t)
	previous := f.trip(t)
	f.update(t, "target-1", map[string]any{"api_key": "sk-fake-after"})
	f.assertReadyAndForward(t, previous)
	if got := f.lastKey.Load(); got != "Bearer sk-fake-after" {
		t.Fatalf("upstream did not receive the new key: %v", got)
	}
}

func TestHandlerTargetNonIdentityUpdatesPreserveBreaker(t *testing.T) {
	for _, edit := range []string{"max_concurrency", "other_target"} {
		t.Run(edit, func(t *testing.T) {
			f := newAdminBreakerFixture(t)
			if edit == "other_target" {
				body := map[string]any{"name": "other", "endpoint_type": "openai", "usage_adapter": "standard", "endpoint": f.url, "api_key": "sk-fake-other", "model_mappings": []map[string]any{{"upstream": "glm-flash"}}}
				payload, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				rec := httptest.NewRecorder()
				f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/targets", bytes.NewReader(payload)))
				if rec.Code != http.StatusCreated {
					t.Fatalf("create other target: HTTP %d %s", rec.Code, rec.Body.String())
				}
			}
			previous := f.trip(t)
			if edit == "max_concurrency" {
				f.update(t, "target-1", map[string]any{"max_concurrency": 3})
			} else {
				f.update(t, "other", map[string]any{"api_key": "sk-fake-other-new"})
			}
			f.assertMuted(t, previous)
		})
	}
}

func TestHandlerTargetResumeResetsBreaker(t *testing.T) {
	for _, resume := range []string{"target_route", "target_put", "pool_route", "pool_put"} {
		t.Run(resume, func(t *testing.T) {
			f := newAdminBreakerFixture(t)
			isPool := strings.HasPrefix(resume, "pool_")
			if isPool {
				if rec := postPool(t, f.h, map[string]any{"name": "breaker-pool"}); rec.Code != http.StatusCreated {
					t.Fatalf("create pool: HTTP %d %s", rec.Code, rec.Body.String())
				}
				f.update(t, "target-1", map[string]any{"pool": "breaker-pool"})
			}
			previous := f.trip(t)
			path := "targets/target-1"
			if isPool {
				path = "pools/breaker-pool"
			}
			f.mutate(t, path+"/pause")
			if status := f.status(t); !status.Paused || !status.Muted || !status.MutedUntil.Equal(previous.MutedUntil) {
				t.Fatalf("pause should preserve breaker: %+v", status)
			}
			if resume == "target_put" {
				f.update(t, "target-1", map[string]any{"paused": false})
			} else if resume == "pool_put" {
				if rec := putPool(t, f.h, "breaker-pool", map[string]any{"paused": false}); rec.Code != http.StatusOK {
					t.Fatalf("resume pool: HTTP %d %s", rec.Code, rec.Body.String())
				}
			} else {
				f.mutate(t, path+"/resume")
			}
			f.assertReadyAndForward(t, previous)
		})
	}
}

func TestHandlerPoolResumePreservesBreakerWhenTargetStillPaused(t *testing.T) {
	f := newAdminBreakerFixture(t)
	if rec := postPool(t, f.h, map[string]any{"name": "breaker-pool"}); rec.Code != http.StatusCreated {
		t.Fatalf("create pool: HTTP %d %s", rec.Code, rec.Body.String())
	}
	f.update(t, "target-1", map[string]any{"pool": "breaker-pool"})
	previous := f.trip(t)
	f.mutate(t, "targets/target-1/pause")
	f.mutate(t, "pools/breaker-pool/pause")
	f.mutate(t, "pools/breaker-pool/resume")
	current := f.status(t)
	if !current.Paused || !current.Muted || !current.MutedUntil.Equal(previous.MutedUntil) || current.ConsecutiveFailures != previous.ConsecutiveFailures {
		t.Fatalf("pool resume reset a target that remains paused: %+v", current)
	}
	before := f.calls.Load()
	if rec := f.request(t); rec.Code < 400 {
		t.Fatalf("paused target accepted request: HTTP %d %s", rec.Code, rec.Body.String())
	}
	if f.calls.Load() != before {
		t.Fatal("still-paused target made an upstream request")
	}
	f.mutate(t, "targets/target-1/resume")
	f.assertReadyAndForward(t, previous)
}
