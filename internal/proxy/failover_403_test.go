package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

const forbidden403Body = `{"error":{"message":"Forbidden: account suspended","type":"permission_error"}}`

func newForbiddenFailoverService(t *testing.T) (*Service, *auth.Principal, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var forbiddenHits, okHits atomic.Int32
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forbiddenHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(forbidden403Body))
	}))
	t.Cleanup(forbidden.Close)
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mock":"ok"}`))
	}))
	t.Cleanup(ok.Close)

	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{
			{Name: "cheap-403", Endpoint: forbidden.URL, ResourcePathPrefix: "/", APIKey: "k-403"},
			{Name: "next-ok", Endpoint: ok.URL, ResourcePathPrefix: "/", APIKey: "k-ok"},
		},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	s, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s.quietPeriod = time.Minute
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, found := store.Authenticate("token")
	if !found {
		t.Fatal("expected authenticated principal")
	}
	return s, principal, &forbiddenHits, &okHits
}

func serveForbiddenFailover(s *Service, principal *auth.Principal, explicitTarget string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`))
	if explicitTarget != "" {
		req.Header.Set("X-Proxy-Target", explicitTarget)
	}
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	return rr
}

// An upstream 403 is the provider rejecting the proxy's own account on that
// target; the client's authorization was already checked locally. The
// request must move to the next candidate, and the rejecting target must be
// muted so the following requests go straight to the next candidate.
func TestUpstream403FailsOverAndMutesTarget(t *testing.T) {
	s, principal, forbiddenHits, okHits := newForbiddenFailoverService(t)

	routedTo403 := false
	for i := 0; i < 8 && !routedTo403; i++ {
		before := forbiddenHits.Load()
		rr := serveForbiddenFailover(s, principal, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: status=%d body=%s, want 200 after 403 failover", i, rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("X-Proxy-Target"); got != "next-ok" {
			t.Fatalf("request %d: served by %q, want next-ok", i, got)
		}
		routedTo403 = forbiddenHits.Load() > before
	}
	if !routedTo403 {
		t.Fatal("no request reached the 403 target first")
	}
	if got := s.metrics.totalDequeued.Load(); got != 1 {
		t.Fatalf("totalDequeued = %d, want 1", got)
	}

	hitsAfterFailover := forbiddenHits.Load()
	for i := 0; i < 5; i++ {
		rr := serveForbiddenFailover(s, principal, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("follow-up %d: status=%d", i, rr.Code)
		}
	}
	if got := forbiddenHits.Load(); got != hitsAfterFailover {
		t.Fatalf("403 target hit %d more times while it should be muted", got-hitsAfterFailover)
	}
	if okHits.Load() < 6 {
		t.Fatalf("next-ok hits = %d, want >= 6", okHits.Load())
	}
	// The mute is a fixed one-hour cooldown (quiet_period here is 1 min), it
	// records why the target is cooling, and it is not permanent.
	now := time.Now()
	var found bool
	for _, st := range s.TargetStatuses(now) {
		if st.Name != "cheap-403" {
			continue
		}
		found = true
		if !st.Muted || st.MuteReason != muteReasonUpstream403 || st.MuteCode != "permission_error" {
			t.Fatalf("cheap-403 status muted=%v reason=%q code=%q", st.Muted, st.MuteReason, st.MuteCode)
		}
		if remaining := st.MutedUntil.Sub(now); remaining < 59*time.Minute || remaining > time.Hour {
			t.Fatalf("cheap-403 muted for %v, want ~1h", remaining)
		}
		if !strings.Contains(st.Phase, "muted") {
			t.Fatalf("phase = %q, want muted", st.Phase)
		}
	}
	if !found {
		t.Fatal("cheap-403 status missing")
	}
}

func TestMarkFailureForFixedCooldownRecovers(t *testing.T) {
	st := newTargetState(&Target{Name: "t"}, newTestLogger())
	now := time.Now()
	st.MarkFailureFor(now, time.Hour, http.StatusForbidden, "permission_error")
	if !st.IsMuted(now.Add(59 * time.Minute)) {
		t.Fatal("should stay muted within the hour")
	}
	// An ordinary failure during the 403 cooldown must not shorten it.
	st.MarkFailure(now.Add(time.Minute), time.Minute)
	if got := st.Stats(); got.MutedUntil.Before(now.Add(time.Hour)) || got.MuteReason != muteReasonUpstream403 {
		t.Fatalf("ordinary failure shortened 403 cooldown: %+v", got)
	}
	if st.IsMuted(now.Add(time.Hour + time.Second)) {
		t.Fatal("must become eligible again after one hour (not permanent)")
	}
	if got := st.Phase(now.Add(time.Hour + time.Second)); got != phaseHalfOpen {
		t.Fatalf("phase after cooldown = %q, want half-open probe", got)
	}
	st.MarkSuccess(now.Add(2 * time.Hour))
	if got := st.Stats(); got.MuteReason != "" || got.MuteCode != "" {
		t.Fatalf("success must clear mute reason: %+v", got)
	}
}

// Counterexample: an explicit X-Proxy-Target never crosses targets and keeps
// passing the real upstream 403 through without muting the target.
func TestUpstream403ExplicitTargetPassesThrough(t *testing.T) {
	s, principal, forbiddenHits, okHits := newForbiddenFailoverService(t)
	for i := 0; i < 2; i++ {
		rr := serveForbiddenFailover(s, principal, "cheap-403")
		if rr.Code != http.StatusForbidden || rr.Body.String() != forbidden403Body {
			t.Fatalf("request %d: status=%d body=%q, want original 403", i, rr.Code, rr.Body.String())
		}
	}
	if forbiddenHits.Load() != 2 || okHits.Load() != 0 {
		t.Fatalf("hits 403=%d ok=%d, want 2/0", forbiddenHits.Load(), okHits.Load())
	}
	if got := s.metrics.totalDequeued.Load(); got != 0 {
		t.Fatalf("totalDequeued = %d, want 0", got)
	}
}
