package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// PR #201 review.

// P1: with bearer passthrough the upstream judged the client's own token. A 402
// for it is the client's billing problem: passed through, no key cooled, no
// target cooldown, no failover (the same boundary as 401/403).
func TestBearerPassthrough402DoesNotMuteTargetOrKeyOrCrossTargets(t *testing.T) {
	const body = `{"error":{"message":"Insufficient Balance","type":"unknown_error"}}`
	a := newRejectionUpstream(t, http.StatusPaymentRequired, body)
	b := newRejectionUpstream(t, http.StatusOK, ``, "k-b")
	tA := rejectionTarget("A", a.srv.URL, "k-a1", "k-a2")
	tA.EndpointType = config.EndpointTypeAzureOpenAI
	tA.AllowBearer = true
	s, p := newRejectionService(t, tA, rejectionTarget("B", b.srv.URL, "k-b"))
	var rr *httptest.ResponseRecorder
	for i := 0; i < 16 && a.hits.Load() == 0; i++ {
		rr = serveRejectionWith(s, p, func(r *http.Request) { r.Header.Set(headerAzureAuthorization, "Bearer client-token") })
	}
	if a.hits.Load() == 0 {
		t.Fatal("no request reached A")
	}
	if rr.Code != http.StatusPaymentRequired || rr.Body.String() != body {
		t.Fatalf("status=%d body=%s, want the client's 402 passed through", rr.Code, rr.Body.String())
	}
	if b.hits.Load() != 0 || s.metrics.totalDequeued.Load() != 0 {
		t.Fatalf("the client's 402 failed over: B hits=%d dequeued=%d", b.hits.Load(), s.metrics.totalDequeued.Load())
	}
	assertNotMuted(t, s, "A")
	stA, _ := s.targetByName("A")
	for i, ks := range stA.keyPool.status() {
		if ks.Exhausted {
			t.Fatalf("proxy key %d cooled for the client's own token: %+v", i, ks)
		}
	}
}

// A proxy-credential 402 is unchanged: it still cools the target and fails over.
func TestProxyCredential402StillMutesAndFailsOver(t *testing.T) {
	a := newRejectionUpstream(t, http.StatusPaymentRequired, `{"error":{"message":"Insufficient Balance","type":"unknown_error"}}`)
	b := newRejectionUpstream(t, http.StatusOK, ``, "k-b")
	tA := rejectionTarget("A", a.srv.URL, "k-a")
	tA.EndpointType = config.EndpointTypeAzureOpenAI
	tA.AllowBearer = true
	s, p := newRejectionService(t, tA, rejectionTarget("B", b.srv.URL, "k-b"))
	rr := serveUntilHit(t, s, p, a) // no client credential header
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want failover to B", rr.Code)
	}
	assertRejectionMuted(t, s, "A", http.StatusPaymentRequired)
}

func padded403(code string) string {
	return `{"error":{"type":"permission_error","code":"` + code + `","message":"` + strings.Repeat("x", 8*1024) + `"}}`
}

// P2: a structured 403 whose body is larger than the 4 KiB pre-read is
// classified on the complete body before settlement, and the key loop moves on
// to the next key.
func TestDeferredLargeAccountRejection403RotatesKey(t *testing.T) {
	bad := newRejectionUpstream(t, http.StatusForbidden, padded403("forbidden"), "k-ok")
	s, p := newRejectionService(t, rejectionTarget("only", bad.srv.URL, "k-bad", "k-ok"))
	for i := 0; i < 8; i++ {
		rr := serveRejection(s, p)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: status=%d, want 200 from the usable key (the rejected key was not rotated away)", i, rr.Code)
		}
	}
	if bad.hits.Load() == bad.okHits.Load() && bad.okHits.Load() == 0 {
		t.Fatal("no request reached the upstream")
	}
	st, _ := s.targetByName("only")
	status := st.keyPool.status()
	if status[0].ExhaustReason != keyReasonForbidden || !status[0].Exhausted {
		t.Fatalf("rejected key = %+v, want cooled as a confirmed forbidden key", status[0])
	}
	if status[1].Exhausted {
		t.Fatalf("usable key = %+v", status[1])
	}
	// Once cooled it is not selected again: only the first request can have hit it.
	if rejects := bad.hits.Load() - bad.okHits.Load(); rejects > 1 {
		t.Fatalf("the rejected key was tried %d times, want at most once", rejects)
	}
	assertNotMuted(t, s, "only")
}

// Counter-example: a content-moderation 403 describes the request: no key
// rotation, no cooldown, the 403 passes through.
func TestDeferredLargeModeration403DoesNotRotateOrCool(t *testing.T) {
	up := newRejectionUpstream(t, http.StatusForbidden, padded403("content_policy_violation"), "k-ok")
	s, p := newRejectionService(t, rejectionTarget("only", up.srv.URL, "k-bad", "k-ok"))
	var got403 bool
	for i := 0; i < 6 && !got403; i++ {
		rr := serveRejection(s, p)
		if rr.Code == http.StatusForbidden {
			got403 = true
		}
	}
	if !got403 {
		t.Fatal("the moderation 403 never reached the client")
	}
	st, _ := s.targetByName("only")
	for i, ks := range st.keyPool.status() {
		if ks.Exhausted {
			t.Fatalf("key %d cooled by a moderation 403: %+v", i, ks)
		}
	}
	assertNotMuted(t, s, "only")
}
