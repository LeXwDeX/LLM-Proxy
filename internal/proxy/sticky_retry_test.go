package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// A: when waiting is not possible, a request held by its sticky target gets a
// retryable 503 + Retry-After instead of a substitute target's 4xx.

func assertStickyRetry(t *testing.T, env *incidentEnv, rrHeader http.Header, code int, body []byte, wantSubstitute string, minRetry, maxRetry int) {
	t.Helper()
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s, want the sticky retry 503", code, body)
	}
	ra, err := strconv.Atoi(rrHeader.Get("Retry-After"))
	if err != nil || ra < minRetry || ra > maxRetry {
		t.Fatalf("Retry-After = %q, want %d..%d", rrHeader.Get("Retry-After"), minRetry, maxRetry)
	}
	if got := rrHeader.Get("X-Upstream-Status"); got != wantSubstitute {
		t.Fatalf("X-Upstream-Status = %q, want %q", got, wantSubstitute)
	}
	if rrHeader.Get("X-Proxy-Target") != "" || rrHeader.Get("X-Upstream-Request-Id") != "" {
		t.Fatalf("the substitute target leaked into the hint: %v", rrHeader)
	}
	var payload struct {
		Error struct {
			Type       string `json:"type"`
			Message    string `json:"message"`
			RetryAfter int    `json:"retry_after"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Error.Type != "upstream_temporarily_unavailable" || payload.Error.RetryAfter != ra {
		t.Fatalf("body = %s (%v), want a proxy-generated upstream_temporarily_unavailable error", body, err)
	}
	if string(body) != "" && containsAny(string(body), "thinking mode", "reasoning content") {
		t.Fatalf("the substitute's upstream body was replayed: %s", body)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestStickyRetryHintForSessionBinding(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.bind("sess-ds")
	env.limitModel("bailian", incidentDeepseekUpstream, 90*time.Second)

	rr := env.do("deepseek", "sess-ds")
	assertStickyRetry(t, env, rr.Header(), rr.Code, rr.Body.Bytes(), "400", 85, 91)
	m := env.svc.MetricsSnapshot()
	if m.TotalDequeueExhausted != 1 || m.TotalCachedErrorReplayed != 0 || m.TotalStickyRetryHinted != 1 {
		t.Fatalf("exhausted=%d replayed=%d hinted=%d, want 1/0/1", m.TotalDequeueExhausted, m.TotalCachedErrorReplayed, m.TotalStickyRetryHinted)
	}
	// The substitute was asked exactly once and the binding did not move.
	if got := env.official.hitsFor(incidentOfficialUpstream); got != 1 {
		t.Fatalf("official hits = %d, want 1", got)
	}
	if target, _ := env.svc.affinity.Get(sessionAffinityKey("tester", "sess-ds", "deepseek"), time.Now()); target != "bailian" {
		t.Fatalf("session binding moved to %q", target)
	}
	// After the cooldown the client is back on the original target.
	env.state("bailian").clearModelLimit(incidentDeepseekUpstream)
	again := env.do("deepseek", "sess-ds")
	if again.Code != http.StatusOK || again.Header().Get("X-Proxy-Target") != "bailian" {
		t.Fatalf("after recovery status=%d target=%q", again.Code, again.Header().Get("X-Proxy-Target"))
	}
	if target, _ := env.svc.affinity.Get(sessionAffinityKey("tester", "sess-ds", "deepseek"), time.Now()); target != "bailian" {
		t.Fatalf("binding after recovery = %q", target)
	}
}

func TestStickyRetryHintForIPBinding(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.bind("") // no session header: IP affinity only
	env.limitModel("bailian", incidentDeepseekUpstream, 90*time.Second)
	rr := env.do("deepseek", "")
	assertStickyRetry(t, env, rr.Header(), rr.Code, rr.Body.Bytes(), "400", 85, 91)
}

func TestStickyRetryAfterPreSendNetworkFailureOfStickyTarget(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.bind("sess-ds")
	env.bailian.srv.Close() // connection refused: provably nothing was sent
	rr := env.do("deepseek", "sess-ds")
	// bailian is tried first (sticky), fails before sending and cools; the
	// official channel then rejects with a 400: the client is asked to retry.
	assertStickyRetry(t, env, rr.Header(), rr.Code, rr.Body.Bytes(), "400", 30, 61)
	if got := env.official.hitsFor(incidentOfficialUpstream); got != 1 {
		t.Fatalf("official hits = %d, want 1", got)
	}
}

func TestStickyRetryNativeWebSocketEventCarriesRetryAfter(t *testing.T) {
	rec := newResponseRecorder()
	writeStickyRetry(rec, http.StatusBadRequest, "bad_a", time.Now().Add(12*time.Second), time.Now())
	writer := &responsesWebsocketWriter{status: rec.status, header: rec.header}
	writer.buffer.Write(rec.body.Bytes())
	event := responsesWebsocketTurnErrorEvent(&responsesWebsocketSession{}, writer)
	if event["status"] != http.StatusServiceUnavailable {
		t.Fatalf("event status = %v", event["status"])
	}
	if ra, ok := event["retry_after"].(int); !ok || ra < 11 || ra > 13 {
		t.Fatalf("retry_after = %v, want ~12", event["retry_after"])
	}
	raw, _ := json.Marshal(event["error"])
	var errObj struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &errObj)
	if errObj.Type != "upstream_temporarily_unavailable" {
		t.Fatalf("event error = %s", raw)
	}
}

// Counter-examples: every other situation keeps the upstream's own answer.

func TestStickyRetryNotForUnboundRequest(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.limitModel("bailian", incidentDeepseekUpstream, 90*time.Second)
	// No session and no IP binding exists: the first real 4xx is the answer.
	rr := env.do("deepseek", "")
	if rr.Code != http.StatusBadRequest || rr.Header().Get("X-Proxy-Target") != "official" {
		t.Fatalf("status=%d target=%q, want the official 400 replayed", rr.Code, rr.Header().Get("X-Proxy-Target"))
	}
	if m := env.svc.MetricsSnapshot(); m.TotalCachedErrorReplayed != 1 || m.TotalStickyRetryHinted != 0 {
		t.Fatalf("replayed=%d hinted=%d, want 1/0", m.TotalCachedErrorReplayed, m.TotalStickyRetryHinted)
	}
}

func TestStickyRetryNotWhenStickyTargetIsAccountCooled(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.bind("sess-ds")
	env.state("bailian").MarkFailureFor(time.Now(), time.Hour, http.StatusPaymentRequired, "")
	rr := env.do("deepseek", "sess-ds")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the 400 (an account rejection is not a transient hold)", rr.Code)
	}
}

func TestStickyRetryNotWhenStickyTargetPausedOrDeleted(t *testing.T) {
	for _, mode := range []string{"paused", "deleted"} {
		t.Run(mode, func(t *testing.T) {
			env := newIncidentEnv(t, "120", http.StatusBadRequest)
			env.bind("sess-ds")
			env.limitModel("bailian", incidentDeepseekUpstream, 90*time.Second)
			switch mode {
			case "paused":
				env.state("bailian").Target().Paused = true
			case "deleted":
				if err := env.svc.UpdateTargets([]config.Target{{Name: "official", EndpointType: "openai", Endpoint: env.official.srv.URL, APIKey: "k-o", ModelMappings: []config.ModelMapping{{Upstream: incidentOfficialUpstream, Fallback: "deepseek"}}}}); err != nil {
					t.Fatal(err)
				}
			}
			rr := env.do("deepseek", "sess-ds")
			// A paused or removed sticky target is no longer a legal candidate;
			// the request is served by whatever else exists (here: a 400).
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s, want the official 400", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestStickyRetryNotForExplicitTarget(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.bind("sess-ds")
	rr := env.doCtx(t.Context(), "deepseek", "sess-ds", func(r *http.Request) { r.Header.Set(headerProxyTarget, "official") })
	if rr.Code != http.StatusBadRequest || rr.Header().Get("X-Proxy-Target") != "official" {
		t.Fatalf("explicit target status=%d target=%q, want the passthrough 400", rr.Code, rr.Header().Get("X-Proxy-Target"))
	}
}

func TestStickyOwn4xxIsReplayedNotHinted(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.bind("sess-ds")
	// The sticky target itself now rejects deepseek requests with its own 400.
	env.bailian.setRespond(func(model string) (int, http.Header, string) {
		return http.StatusBadRequest, nil, `{"error":{"message":"bailian says bad request","type":"invalid_request_error"}}`
	})
	rr := env.do("deepseek", "sess-ds")
	if rr.Code != http.StatusBadRequest || rr.Header().Get("X-Proxy-Target") != "bailian" {
		t.Fatalf("status=%d target=%q body=%s, want the sticky target's own 400", rr.Code, rr.Header().Get("X-Proxy-Target"), rr.Body.String())
	}
	if !containsAny(rr.Body.String(), "bailian says bad request") {
		t.Fatalf("body = %s", rr.Body.String())
	}
	if m := env.svc.MetricsSnapshot(); m.TotalStickyRetryHinted != 0 {
		t.Fatalf("hinted = %d, want 0", m.TotalStickyRetryHinted)
	}
}

func TestStickyOwn4xxReplacesSubstituteCachedAnswer(t *testing.T) {
	// The substitute answers first (the sticky target was cooling), then the
	// sticky target recovers during the wait and gives its own 400: the client
	// gets the sticky target's, as a whole (status, target header and body).
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.bind("sess-ds")
	env.bailian.setRespond(func(model string) (int, http.Header, string) {
		h := http.Header{}
		h.Set("X-Upstream-Request-Id", "bailian-req")
		return http.StatusBadRequest, h, `{"error":{"message":"bailian says bad request","type":"invalid_request_error"}}`
	})
	env.limitModel("bailian", incidentDeepseekUpstream, 200*time.Millisecond)
	rr := env.do("deepseek", "sess-ds")
	if rr.Code != http.StatusBadRequest || rr.Header().Get("X-Proxy-Target") != "bailian" || rr.Header().Get("X-Upstream-Request-Id") != "bailian-req" {
		t.Fatalf("status=%d target=%q reqid=%q body=%s, want bailian's own 400 as a whole", rr.Code, rr.Header().Get("X-Proxy-Target"), rr.Header().Get("X-Upstream-Request-Id"), rr.Body.String())
	}
	if !containsAny(rr.Body.String(), "bailian says bad request") || containsAny(rr.Body.String(), "thinking mode") {
		t.Fatalf("mixed body: %s", rr.Body.String())
	}
}

func TestStickyDeterministicErrorStaysTerminal(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.official.setRespond(func(model string) (int, http.Header, string) {
		return http.StatusBadRequest, nil, `{"error":{"code":"invalid_function_parameters","message":"bad tool schema"}}`
	})
	env.bind("sess-ds")
	env.limitModel("bailian", incidentDeepseekUpstream, 90*time.Second)
	rr := env.do("deepseek", "sess-ds")
	if rr.Code != http.StatusBadRequest || !containsAny(rr.Body.String(), "invalid_function_parameters") {
		t.Fatalf("status=%d body=%s, want the deterministic 400 passed through", rr.Code, rr.Body.String())
	}
}

func TestStickyFirstHop429Stays429(t *testing.T) {
	// The sticky target answers a deepseek request with 429 and a substitute
	// then rejects with 400: the client is told 429 (rate limited), never the
	// substitute's 400 and never a rewritten status.
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.bind("sess-ds")
	env.bailian.setRespond(func(model string) (int, http.Header, string) {
		h := http.Header{}
		h.Set("Retry-After", "120")
		return http.StatusTooManyRequests, h, `{"error":{"message":"slow down","code":"rate_limit"}}`
	})
	rr := env.do("deepseek", "sess-ds")
	if rr.Code != http.StatusTooManyRequests || rr.Header().Get("X-Proxy-Target") != "bailian" {
		t.Fatalf("status=%d target=%q, want bailian's 429", rr.Code, rr.Header().Get("X-Proxy-Target"))
	}
	if m := env.svc.MetricsSnapshot(); m.TotalStickyRetryHinted != 0 {
		t.Fatalf("hinted = %d, want 0", m.TotalStickyRetryHinted)
	}
}

func TestStickyRetryNotBeyondTenMinutes(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.bind("sess-ds")
	env.limitModel("bailian", incidentDeepseekUpstream, 11*time.Minute)
	rr := env.do("deepseek", "sess-ds")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the 400 replayed when the sticky target is out for over %v", rr.Code, retryAfterMax)
	}
}

func TestPlainUnavailable503CarriesRetryAfter(t *testing.T) {
	env := newIncidentEnv(t, "", http.StatusBadRequest)
	// Every candidate of qwen-max cools at target level (an upstream 502).
	env.svc.markTargetFailure(env.state("bailian"), time.Now(), 502, nil, false, "")
	env.state("bailian").lastEarlyProbe.Store(time.Now().UnixNano()) // no early probe: the all-cooling 503 is under test
	rr := env.do("qwen-max", "")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rr.Code)
	}
	ra, err := strconv.Atoi(rr.Header().Get("Retry-After"))
	if err != nil || ra < 55 || ra > 61 {
		t.Fatalf("Retry-After = %q, want the remaining cooldown (~60s)", rr.Header().Get("Retry-After"))
	}
}
