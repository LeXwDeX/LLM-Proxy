package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func serveRejectionWith(s *Service, p *auth.Principal, mutate func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	if mutate != nil {
		mutate(req)
	}
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	return rr
}

// H1: a client stuck to A whose request fails over to B in the same request
// keeps its binding on A.
func TestIPAffinityNotHijackedAfterForwardFailover(t *testing.T) {
	bad := newRejectionUpstream(t, http.StatusBadRequest, `{"error":{"code":"bad_a"}}`)
	ok := newRejectionUpstream(t, http.StatusOK, ``, "k-ok")
	s, p := newRejectionService(t, rejectionTarget("A", bad.srv.URL, "k-bad"), rejectionTarget("B", ok.srv.URL, "k-ok"))
	key := affinityKey("192.0.2.1", p.Name, "deepseek-chat")
	s.affinity.Set(key, "a", time.Now())
	rr := serveRejection(s, p) // httptest RemoteAddr is 192.0.2.1
	if rr.Code != http.StatusOK || rr.Header().Get("X-Proxy-Target") != "B" {
		t.Fatalf("status=%d target=%q", rr.Code, rr.Header().Get("X-Proxy-Target"))
	}
	if got, _ := s.affinity.Get(key, time.Now()); got != "a" {
		t.Fatalf("IP binding moved to %q, want it kept on a", got)
	}
}

// H2: a failed final response never creates a binding.
func TestAffinityNotWrittenOnFailedResponse(t *testing.T) {
	bad := newRejectionUpstream(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	s, p := newRejectionService(t, rejectionTarget("only", bad.srv.URL, "k-bad"))
	rr := serveRejectionWith(s, p, func(r *http.Request) { r.Header.Set("X-Session-Id", "sess-1") })
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rr.Code)
	}
	if got, ok := s.affinity.Get(affinityKey("192.0.2.1", p.Name, "deepseek-chat"), time.Now()); ok {
		t.Fatalf("IP binding written on a failed response: %q", got)
	}
	if got, ok := s.affinity.Get(sessionAffinityKey(p.Name, "sess-1", "deepseek-chat"), time.Now()); ok {
		t.Fatalf("session binding written on a failed response: %q", got)
	}
}

// H3: one candidate cools after a 403 while the other is only full or only
// cooling for an ordinary failure: that is a temporary 503, not a 403.
func TestAllUnavailableMixedReturns503(t *testing.T) {
	for _, mode := range []string{"capacity", "ordinary-cooldown"} {
		t.Run(mode, func(t *testing.T) {
			a := newRejectionUpstream(t, http.StatusForbidden, `{"error":{"type":"permission_error"}}`)
			b := newRejectionUpstream(t, http.StatusOK, ``, "k-b")
			tB := rejectionTarget("B", b.srv.URL, "k-b")
			tB.MaxConcurrency = 1
			s, p := newRejectionService(t, rejectionTarget("A", a.srv.URL, "k-a"), tB)
			now := time.Now()
			stA, _ := s.targetByName("A")
			stA.MarkFailureFor(now, time.Hour, http.StatusForbidden, "")
			stB, _ := s.targetByName("B")
			if mode == "capacity" {
				stB.inFlight.Store(1)
			} else {
				stB.MarkFailure(now.Add(time.Hour), time.Minute) // cools longer than A
				// B's early-probe slot was just used, so this request sees the
				// cooldowns (an available early probe is covered elsewhere).
				stB.lastEarlyProbe.Store(time.Now().UnixNano())
			}
			rr := serveRejection(s, p)
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("mixed unavailability returned %d %s, want 503", rr.Code, rr.Body.String())
			}
		})
	}
}

// H5: with bearer passthrough the upstream judged the client's own token, so
// a 403 neither cools the proxy key/target nor crosses targets.
func TestBearerPassthrough403DoesNotMuteTargetOrKey(t *testing.T) {
	const body = `{"error":{"type":"permission_error","message":"client token denied"}}`
	a := newRejectionUpstream(t, http.StatusForbidden, body)
	b := newRejectionUpstream(t, http.StatusOK, ``, "k-b")
	tA := rejectionTarget("A", a.srv.URL, "k-a1", "k-a2")
	tA.EndpointType = config.EndpointTypeAzureOpenAI
	tA.AllowBearer = true
	tB := rejectionTarget("B", b.srv.URL, "k-b")
	s, p := newRejectionService(t, tA, tB)
	var rr *httptest.ResponseRecorder
	for i := 0; i < 16; i++ {
		before := a.hits.Load()
		rr = serveRejectionWith(s, p, func(r *http.Request) { r.Header.Set(headerAzureAuthorization, "Bearer client-token") })
		if a.hits.Load() > before {
			break
		}
	}
	if a.hits.Load() == 0 {
		t.Fatal("no request reached A")
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want the client's 403 passed through", rr.Code)
	}
	assertNotMuted(t, s, "A")
	stA, _ := s.targetByName("A")
	for _, ks := range stA.keyPool.status() {
		if ks.ExhaustReason == keyReasonForbidden {
			t.Fatal("proxy key cooled for the client's own token")
		}
	}
}

// H6: two keys of A reject the account (the loop converges) while the third
// key is only briefly rate limited: the request moves to B, but A is not
// muted for an hour; it stays in rotation for the recovering key.
func TestConvergenceKeepsTargetWhenHealthyKeysRemain(t *testing.T) {
	a := newRejectionUpstream(t, http.StatusForbidden, `{"error":{"type":"permission_error"}}`, "k-rl")
	b := newRejectionUpstream(t, http.StatusOK, ``, "k-b")
	s, p := newRejectionService(t, rejectionTarget("A", a.srv.URL, "k-bad1", "k-bad2", "k-rl"), rejectionTarget("B", b.srv.URL, "k-b"))
	stA, _ := s.targetByName("A")
	stA.keyPool.markExhaustedWithError(2, "rate_limited", "")
	rr := serveUntilHit(t, s, p, a)
	if rr.Code != http.StatusOK || rr.Header().Get("X-Proxy-Target") != "B" {
		t.Fatalf("status=%d target=%q body=%s", rr.Code, rr.Header().Get("X-Proxy-Target"), rr.Body.String())
	}
	if s.metrics.totalDequeued.Load() != 1 {
		t.Fatalf("dequeued=%d, want the converged rejection to move on once", s.metrics.totalDequeued.Load())
	}
	assertNotMuted(t, s, "A")

	// Counterexample: with no recovering key left the target cools.
	stA.keyPool.markRecovered(2)
	stA.keyPool.markExhaustedWithCooldown(2, keyReasonForbidden, time.Now().Add(time.Hour), "")
	if targetHasServingKeys(stA) {
		t.Fatal("all keys account-rejected must not count as serving")
	}
}

// H3: a moderation 403 whose body the 4 KiB pre-read cannot judge (longer
// than 4 KiB) must not cool keys, converge, or cool the target.
func TestLongModeration403DoesNotCoolKeysOrTarget(t *testing.T) {
	body := `{"error":{"code":"content_policy_violation","message":"` + strings.Repeat("x", 6000) + `"}}`
	a := newRejectionUpstream(t, http.StatusForbidden, body)
	b := newRejectionUpstream(t, http.StatusOK, ``, "k-b")
	s, p := newRejectionService(t, rejectionTarget("A", a.srv.URL, "k-a1", "k-a2"), rejectionTarget("B", b.srv.URL, "k-b"))
	rr := serveUntilHit(t, s, p, a)
	if rr.Code != http.StatusForbidden || rr.Body.String() != body {
		t.Fatalf("status=%d, want the moderation 403 passed through", rr.Code)
	}
	assertNotMuted(t, s, "A")
	stA, _ := s.targetByName("A")
	for _, ks := range stA.keyPool.status() {
		if ks.Exhausted {
			t.Fatalf("moderation 403 cooled key %d: %+v", ks.Index, ks)
		}
	}
}

// Bearer passthrough: a client-token 402 neither cools the proxy key nor the target.
func TestBearerPassthrough402DoesNotCoolProxyKey(t *testing.T) {
	a := newRejectionUpstream(t, http.StatusPaymentRequired, `{"error":{"type":"billing_error","message":"client balance"}}`)
	tA := rejectionTarget("A", a.srv.URL, "k-a1", "k-a2")
	tA.EndpointType = config.EndpointTypeAzureOpenAI
	tA.AllowBearer = true
	s, p := newRejectionService(t, tA)
	_ = serveRejectionWith(s, p, func(r *http.Request) { r.Header.Set(headerAzureAuthorization, "Bearer client-token") })
	stA, _ := s.targetByName("A")
	for _, ks := range stA.keyPool.status() {
		if ks.Exhausted {
			t.Fatalf("client-token 402 cooled proxy key %d: %+v", ks.Index, ks)
		}
	}
	assertNotMuted(t, s, "A")
}

// M3: one candidate cools after an account rejection, another only after a
// transient failure; the transient one still gets its early probe.
func TestEarlyProbeWithAccountMutedSibling(t *testing.T) {
	a := newRejectionUpstream(t, http.StatusForbidden, `{"error":{"type":"permission_error"}}`)
	b := newRejectionUpstream(t, http.StatusOK, ``, "k-b")
	s, p := newRejectionService(t, rejectionTarget("A", a.srv.URL, "k-a"), rejectionTarget("B", b.srv.URL, "k-b"))
	now := time.Now()
	stA, _ := s.targetByName("A")
	stA.MarkFailureFor(now, time.Hour, http.StatusForbidden, "")
	stB, _ := s.targetByName("B")
	stB.MarkFailure(now, time.Minute)
	if rr := serveRejection(s, p); rr.Code != http.StatusOK || b.okHits.Load() != 1 {
		t.Fatalf("status=%d B hits=%d, want B probed early", rr.Code, b.okHits.Load())
	}
	if stB.IsMuted(time.Now()) {
		t.Fatal("successful early probe must recover B")
	}
}
