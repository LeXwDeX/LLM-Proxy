package proxy

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/gorilla/websocket"
)

// Every candidate of the requested model cools after an upstream 429: the
// request never reaches an upstream, and the client is told it is rate
// limited (429 + Retry-After), not that the service is down (503). Any other
// blocker (5xx/network cooldown, full target, probe in flight, account
// rejection) keeps the 503.

type rateLimitedBody struct {
	Error struct {
		Type       string `json:"type"`
		Code       string `json:"code"`
		Message    string `json:"message"`
		RetryAfter int    `json:"retry_after"`
	} `json:"error"`
}

// assertAllTargetsRateLimited checks the 429 the proxy itself generates when
// every candidate is cooling after an upstream 429.
func assertAllTargetsRateLimited(t *testing.T, rr *httptest.ResponseRecorder, minRetry, maxRetry int) rateLimitedBody {
	t.Helper()
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body=%s, want 429 while every candidate cools after an upstream 429", rr.Code, rr.Body.String())
	}
	ra, err := strconv.Atoi(rr.Header().Get("Retry-After"))
	if err != nil || ra < minRetry || ra > maxRetry {
		t.Fatalf("Retry-After = %q, want %d..%d", rr.Header().Get("Retry-After"), minRetry, maxRetry)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var payload rateLimitedBody
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body %q is not JSON: %v", rr.Body.String(), err)
	}
	if payload.Error.Type != "upstream_rate_limited" || payload.Error.Code != "upstream_rate_limited" || payload.Error.RetryAfter != ra || payload.Error.Message == "" {
		t.Fatalf("body = %s, want a proxy-generated upstream_rate_limited error carrying retry_after=%d", rr.Body.String(), ra)
	}
	return payload
}

func TestAllTargetsModelCoolingAnswers429(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.limitModel("bailian", incidentQwenUpstream, 90*time.Second)
	before := env.bailian.total()
	rr := env.do("qwen-max", "")
	assertAllTargetsRateLimited(t, rr, 80, 91)
	if env.bailian.total() != before {
		t.Fatal("a cooling model reached the upstream")
	}
	if env.state("bailian").Phase(time.Now()) != phaseReady {
		t.Fatal("a model 429 must not mute the target")
	}
}

func TestAllTargetsTargetLevel429Answers429(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	st := env.state("bailian")
	// No upstream model: the 429 cools the whole target.
	env.svc.markTargetFailure(st, time.Now(), http.StatusTooManyRequests, http.Header{"Retry-After": {"120"}}, false, "")
	if st.Phase(time.Now()) != phaseMuted {
		t.Fatal("setup: the target must be muted by the 429")
	}
	before := env.bailian.total()
	rr := env.do("qwen-max", "")
	assertAllTargetsRateLimited(t, rr, 100, 121)
	if env.bailian.total() != before {
		t.Fatal("a muted target reached the upstream")
	}
}

// Both candidates of one model cool after a 429, at different levels and
// with different ends: the earliest end drives Retry-After.
func TestAllTargetsMixedLevel429UsesEarliestRetryAfter(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.limitModel("bailian", incidentDeepseekUpstream, 100*time.Second)
	env.svc.markTargetFailure(env.state("official"), time.Now(), http.StatusTooManyRequests, http.Header{"Retry-After": {"60"}}, false, "")
	rr := env.do("deepseek", "")
	assertAllTargetsRateLimited(t, rr, 50, 61)
}

func TestAllTargetsSelectionErrorFlagsRateLimited(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.limitModel("bailian", incidentQwenUpstream, 90*time.Second)
	_, _, _, err := env.svc.selectTarget(env.principal, "192.0.2.7", "", "", nil, nil, "qwen-max", "/v1/chat/completions", time.Now())
	se, ok := err.(*selectionError)
	if !ok || se.Status != http.StatusServiceUnavailable || !se.RateLimited || se.RetryAt.IsZero() {
		t.Fatalf("err = %#v, want a 503-class selectionError flagged RateLimited with a retry time", err)
	}
}

// A target muted by a non-429 failure is a service problem, not a rate limit.
func TestAllTargetsMutedBy5xxStays503(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	st := env.state("bailian")
	env.svc.markTargetFailure(st, time.Now(), http.StatusServiceUnavailable, http.Header{"Retry-After": {"120"}}, false, "")
	st.lastEarlyProbe.Store(time.Now().UnixNano()) // use up the early-probe slot
	rr := env.do("qwen-max", "")
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d Retry-After=%q body=%s, want the plain 503", rr.Code, rr.Header().Get("Retry-After"), rr.Body.String())
	}
	_, _, _, err := env.svc.selectTarget(env.principal, "192.0.2.7", "", "", nil, nil, "qwen-max", "/v1/chat/completions", time.Now())
	if se, ok := err.(*selectionError); !ok || se.RateLimited {
		t.Fatalf("err = %#v, want RateLimited=false for a 503 cooldown", err)
	}
}

// One candidate rate limited, the other cooling for another reason: not
// "everything is rate limited", so the 503 stays.
func TestAllTargetsMixedCooldownReasonsStay503(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.limitModel("bailian", incidentDeepseekUpstream, 100*time.Second)
	official := env.state("official")
	env.svc.markTargetFailure(official, time.Now(), http.StatusServiceUnavailable, http.Header{"Retry-After": {"120"}}, false, "")
	official.lastEarlyProbe.Store(time.Now().UnixNano())
	rr := env.do("deepseek", "")
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d body=%s, want the plain 503 for mixed causes", rr.Code, rr.Body.String())
	}
}

// A candidate that is merely full is not rate limited either.
func TestAllTargetsRateLimitedPlusFullStays503(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest, func(cfg *config.Config) { cfg.Targets[1].MaxConcurrency = 1 })
	env.limitModel("bailian", incidentDeepseekUpstream, 100*time.Second)
	official := env.state("official")
	official.inFlight.Add(1)
	defer official.inFlight.Add(-1)
	rr := env.do("deepseek", "")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want 503 when a candidate is full", rr.Code, rr.Body.String())
	}
}

// An account rejection keeps its own surfaced status (401 shows as 503),
// never the 429 of a rate limit.
func TestAllTargetsAccountRejectionKeepsItsStatus(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	st := env.state("bailian")
	st.markAccountRejection(time.Now(), http.StatusPaymentRequired, "billing_error", true, time.Minute, time.Hour, time.Hour)
	rr := env.do("qwen-max", "")
	if rr.Code != http.StatusPaymentRequired {
		t.Fatalf("status=%d body=%s, want the upstream's 402", rr.Code, rr.Body.String())
	}
}

// A request held by its sticky target whose target is rate limited gets the
// 429 retry hint instead of the 503 one; the substitute's 4xx stays hidden.
func TestStickyHeldRateLimitedTargetAnswers429(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.bind("sess-ds")
	env.limitModel("bailian", incidentDeepseekUpstream, 90*time.Second)
	rr := env.do("deepseek", "sess-ds")
	payload := assertAllTargetsRateLimited(t, rr, 80, 91)
	if got := rr.Header().Get("X-Upstream-Status"); got != "400" {
		t.Fatalf("X-Upstream-Status = %q, want the substitute's 400 kept for diagnosis", got)
	}
	if rr.Header().Get("X-Proxy-Target") != "" || containsAny(rr.Body.String(), "thinking mode", "reasoning content") {
		t.Fatalf("the substitute leaked into the hint: %v %s", rr.Header(), rr.Body.String())
	}
	if payload.Error.Message == "" {
		t.Fatal("empty message")
	}
}

// A forward of this very request failed with a transport error: the pool is
// not merely rate limited, so the remaining 429 cooldown must not turn the
// answer into a 429.
func TestAllTargetsNetworkFailureThenRateLimitedStays503(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.limitModel("bailian", incidentDeepseekUpstream, 100*time.Second)
	env.official.srv.Close() // connection refused: the request fails before sending
	rr := env.do("deepseek", "")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want 503 after a transport failure of this request", rr.Code, rr.Body.String())
	}
}

// A model 429 cooldown hides a target-level cooldown of another cause (here an
// upstream 503): the target is not only rate limited.
func TestAllTargetsModelCooldownDoesNotMaskTargetFailure(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	st := env.state("bailian")
	env.limitModel("bailian", incidentQwenUpstream, 100*time.Second)
	env.svc.markTargetFailure(st, time.Now(), http.StatusServiceUnavailable, http.Header{"Retry-After": {"120"}}, false, "")
	st.lastEarlyProbe.Store(time.Now().UnixNano())
	rr := env.do("qwen-max", "")
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d body=%s, want the plain 503 (target also muted by a 503)", rr.Code, rr.Body.String())
	}
	// The same, with a rejected account.
	st.ClearMute()
	st.markAccountRejection(time.Now(), http.StatusPaymentRequired, "billing_error", true, time.Minute, time.Hour, time.Hour)
	_, _, _, err := env.svc.selectTarget(env.principal, "192.0.2.7", "", "", nil, nil, "qwen-max", "/v1/chat/completions", time.Now())
	if se, ok := err.(*selectionError); !ok || se.RateLimited {
		t.Fatalf("err = %#v, want RateLimited=false when the target is also muted by an account rejection", err)
	}
}

// While a model's half-open probe is in flight the request waits for the probe:
// that is recovery, not a rate limit.
func TestAllTargetsProbeInFlightIsNotRateLimited(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	st := env.state("bailian")
	env.limitModel("bailian", incidentQwenUpstream, 30*time.Millisecond)
	time.Sleep(50 * time.Millisecond) // the cooldown ended, the entry stays: half-open
	if gate, _, lease := st.admitModel(incidentQwenUpstream, time.Now()); gate != modelOpen || lease == nil {
		t.Fatalf("setup: the first request must take the model probe, got gate=%v lease=%v", gate, lease)
	}
	_, _, _, err := env.svc.selectTarget(env.principal, "192.0.2.7", "", "", nil, nil, "qwen-max", "/v1/chat/completions", time.Now())
	se, ok := err.(*selectionError)
	if !ok || se.Status != http.StatusServiceUnavailable || se.RateLimited || !se.ProbeInFlight {
		t.Fatalf("err = %#v, want a 503 selectionError with a probe in flight and RateLimited=false", err)
	}
}

// The bound (sticky) target fails before sending, and the only other candidate
// cools after a 429: the bound upstream is down, not rate limited, so the
// sticky hint stays the 503 one.
func TestStickyTransportFailureWithRateLimitedSubstituteStays503(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.bind("sess-ds")
	env.bailian.srv.Close() // connection refused: provably nothing was sent
	env.limitModel("official", incidentOfficialUpstream, 100*time.Second)
	rr := env.do("deepseek", "sess-ds")
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d Retry-After=%q body=%s, want the sticky 503 hint", rr.Code, rr.Header().Get("Retry-After"), rr.Body.String())
	}
	if typ, _ := stickyBody(t, rr.Body.Bytes()); typ != "upstream_temporarily_unavailable" {
		t.Fatalf("body = %s, want upstream_temporarily_unavailable", rr.Body.String())
	}
}

// A native WS turn without any sticky binding ends with one 429 error event
// carrying the retry hint while every candidate of its model cools after a 429.
func TestNativeWebSocketTurnAllTargetsRateLimitedGets429Event(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.limitModel("bailian", incidentQwenUpstream, 90*time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.svc.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), env.principal)))
	}))
	t.Cleanup(server.Close)
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+responsesPath, http.Header{})
	if err != nil {
		if resp != nil {
			t.Fatalf("dial: %v (HTTP %d)", err, resp.StatusCode)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "qwen-max", "input": "hi"}); err != nil {
		t.Fatal(err)
	}
	event := readWebsocketEvent(t, conn)
	if event["type"] != "error" || event["status"] != float64(429) {
		t.Fatalf("event = %v, want a 429 error event", event)
	}
	if ra, ok := event["retry_after"].(float64); !ok || ra < 80 || ra > 91 {
		t.Fatalf("retry_after = %v, want about 90", event["retry_after"])
	}
	errObj, _ := event["error"].(map[string]any)
	if errObj["type"] != "upstream_rate_limited" {
		t.Fatalf("event error = %v, want upstream_rate_limited", errObj)
	}
}

// An explicit X-Proxy-Target names its target and never gets the pool-wide
// rate-limit answer: a cooling model there stays the plain 503.
func TestExplicitTargetWithRateLimitedModelStays503(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.limitModel("bailian", incidentQwenUpstream, 90*time.Second)
	rr := env.doCtx(t.Context(), "qwen-max", "", func(r *http.Request) { r.Header.Set(headerProxyTarget, "bailian") })
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("explicit target with a cooling model: status=%d body=%s, want 503", rr.Code, rr.Body.String())
	}
}

// The local 429 is logged, so it can be told from a forwarded upstream 429.
func TestAllTargetsRateLimitedIsLoggedAsLocal(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.limitModel("bailian", incidentQwenUpstream, 90*time.Second)
	var buf syncLogBuffer
	env.svc.logger = slog.New(slog.NewTextHandler(&buf, nil))
	if rr := env.do("qwen-max", ""); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429", rr.Code)
	}
	if lines := buf.lines("answered 429 locally"); len(lines) != 1 || !strings.Contains(lines[0], "sticky=false") {
		t.Fatalf("local-429 log lines = %q, want exactly one with sticky=false", lines)
	}
}
