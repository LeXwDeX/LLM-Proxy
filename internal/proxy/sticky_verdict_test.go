package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Review round 2: the sticky verdict also covers a full sticky target, never
// resends a request to a target, caps IP-bound holds, and names the
// substitute's error code for diagnosis.

func stickyBody(t *testing.T, body []byte) (typ, substitute string) {
	t.Helper()
	var payload struct {
		Error struct {
			Type       string `json:"type"`
			Substitute string `json:"substitute_error_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	return payload.Error.Type, payload.Error.Substitute
}

func TestStickyVerdictFullStickyTargetNeverReplaysSubstitute4xx(t *testing.T) {
	for _, binding := range []string{"session", "ip"} {
		t.Run(binding, func(t *testing.T) {
			env := newIncidentEnv(t, "120", http.StatusBadRequest)
			session := ""
			if binding == "session" {
				session = "sess-full"
			}
			env.bind(session)
			st := env.state("bailian")
			st.Target().MaxConcurrency = 1
			fill(t, st, 1) // the sticky target is at its hard capacity
			start := time.Now()
			rr := env.do("deepseek", session)
			if time.Since(start) > 2*time.Second {
				t.Fatalf("a full target must not be waited for (took %v)", time.Since(start))
			}
			assertStickyRetry(t, env, rr.Header(), rr.Code, rr.Body.Bytes(), "400", 1, 1)
			if _, code := stickyBody(t, rr.Body.Bytes()); code != "invalid_request_error" {
				t.Fatalf("substitute_error_code = %q, want the substitute's error type", code)
			}
			if m := env.svc.MetricsSnapshot(); m.TotalInProxyWait != 0 || m.TotalStickyRetryHinted != 1 {
				t.Fatalf("waits=%d hinted=%d, want 0/1", m.TotalInProxyWait, m.TotalStickyRetryHinted)
			}
		})
	}
}

func TestStickyVerdictNoResendToTheSameTarget(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.svc.quietPeriod = 20 * time.Second // the network cooldown is short enough to be waited out
	env.bind("sess-net")
	env.bailian.srv.Close() // connection refused: the sticky target fails before sending
	start := time.Now()
	rr := env.do("deepseek", "sess-net")
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %v: the sticky target was waited for and tried again", time.Since(start))
	}
	assertStickyRetry(t, env, rr.Header(), rr.Code, rr.Body.Bytes(), "400", 15, 21)
	// One failed send to the sticky target, never a second one.
	if got := env.state("bailian").Stats().TotalFailure; got != 1 {
		t.Fatalf("sticky target failures = %d, want exactly one send", got)
	}
	if m := env.svc.MetricsSnapshot(); m.TotalInProxyWait != 0 {
		t.Fatalf("in-proxy waits = %d, want 0 (an attempted target is never waited for)", m.TotalInProxyWait)
	}
}

func TestStickyVerdictHoldLimitByBindingType(t *testing.T) {
	cases := []struct {
		name     string
		session  string
		limit    time.Duration
		wantHint bool
	}{
		{"session 5 min", "sess-cap", 5 * time.Minute, true},
		{"session beyond 10 min", "sess-cap", 11 * time.Minute, false},
		{"ip 100 s", "", 100 * time.Second, true},
		{"ip 150 s", "", 150 * time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newIncidentEnv(t, "120", http.StatusBadRequest)
			env.bind(tc.session)
			env.limitModel("bailian", incidentDeepseekUpstream, tc.limit)
			rr := env.do("deepseek", tc.session)
			if tc.wantHint {
				ra := int(tc.limit / time.Second)
				assertStickyRetry(t, env, rr.Header(), rr.Code, rr.Body.Bytes(), "400", ra-5, ra+1)
				return
			}
			if rr.Code != http.StatusBadRequest || rr.Header().Get("X-Proxy-Target") != "official" {
				t.Fatalf("status=%d target=%q, want the substitute's 400 replayed beyond the hold limit", rr.Code, rr.Header().Get("X-Proxy-Target"))
			}
		})
	}
}

// Without a cached substitute answer the terminal answer of a held request is
// still the sticky retry (no X-Upstream-Status, no substitute code), not a bare
// 503 or an upstream_rejected answer.
func TestStickyVerdictWithoutSubstituteOmitsUpstreamStatus(t *testing.T) {
	env := newIncidentEnv(t, "", http.StatusBadRequest)
	if rr := env.do("qwen-max", "sess-q"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("setup: qwen request status = %d", rr.Code) // binds nothing: the 429 is not a success
	}
	env.bailian.setRespond(nil)
	env.state("bailian").clearModelLimit(incidentQwenUpstream)
	if rr := env.do("qwen-max", "sess-q"); rr.Code != http.StatusOK {
		t.Fatalf("bind: qwen status = %d", rr.Code)
	}
	env.limitModel("bailian", incidentQwenUpstream, 90*time.Second)
	rr := env.do("qwen-max", "sess-q")
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d Retry-After=%q", rr.Code, rr.Header().Get("Retry-After"))
	}
	if rr.Header().Get("X-Upstream-Status") != "" {
		t.Fatalf("X-Upstream-Status = %q, want none without a substitute answer", rr.Header().Get("X-Upstream-Status"))
	}
	if typ, sub := stickyBody(t, rr.Body.Bytes()); typ != "upstream_temporarily_unavailable" || sub != "" {
		t.Fatalf("body type=%q substitute=%q", typ, sub)
	}
	ra, _ := strconv.Atoi(rr.Header().Get("Retry-After"))
	if ra < 85 {
		t.Fatalf("Retry-After = %d", ra)
	}
}

// After a wait for the sticky target, the round starts with that target alone:
// it is not left to the load balancer among the other candidates.
func TestStickyVerdictRetriesStickyTargetFirst(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest, func(cfg *config.Config) {
		cfg.Targets = append(cfg.Targets, config.Target{Name: "third", EndpointType: "openai", Endpoint: "http://127.0.0.1:1", APIKey: "k-t",
			ModelMappings: []config.ModelMapping{{Upstream: "ds-third", Fallback: "deepseek"}}})
	})
	env.bind("sess-first")
	attempted := map[string]struct{}{"official": {}}
	pick := func(retrySticky bool) string {
		trace := selectionTrace{retrySticky: retrySticky}
		state, lease, _, err := env.svc.selectTargetTraced(env.principal, "192.0.2.7", "bailian", "", nil, attempted, "deepseek", "/v1/chat/completions", time.Now(), &trace)
		if err != nil {
			t.Fatalf("selection: %v", err)
		}
		state.releaseProbe(lease)
		state.releaseModelProbe(trace.modelProbe)
		return state.Target().Name
	}
	for i := 0; i < 10; i++ {
		if got := pick(true); got != "bailian" {
			t.Fatalf("retrySticky round %d picked %q, want the sticky bailian", i, got)
		}
	}
	other := false
	for i := 0; i < 10; i++ {
		if pick(false) != "bailian" {
			other = true
		}
	}
	if !other {
		t.Fatal("the plain round never left the sticky target: the check above proves nothing")
	}
	// An attempted sticky target is never re-selected, retrySticky or not.
	attempted["bailian"] = struct{}{}
	for i := 0; i < 5; i++ {
		if got := pick(true); got == "bailian" {
			t.Fatal("a target that was already sent this request was selected again")
		}
	}
}

// A substitute multi-key target whose every key is rate limited for the model
// has answered 429 for this request; that must neither suppress the sticky
// verdict nor be resent to.
func TestStickyVerdictNotSuppressedBySubstituteMultiKey429(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusTooManyRequests, func(cfg *config.Config) {
		for i := range cfg.Targets {
			if cfg.Targets[i].Name == "official" {
				cfg.Targets[i].APIKeys = []string{"k-o2"}
			}
		}
	})
	env.bind("sess-mk")
	env.limitModel("bailian", incidentDeepseekUpstream, 90*time.Second)
	rr := env.do("deepseek", "sess-mk")
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d body=%s, want the sticky retry hint (not the substitute's 429)", rr.Code, rr.Body.String())
	}
	if typ, _ := stickyBody(t, rr.Body.Bytes()); typ != "upstream_temporarily_unavailable" {
		t.Fatalf("body = %s", rr.Body.String())
	}
	if rr.Header().Get("X-Upstream-Status") != "" {
		t.Fatalf("X-Upstream-Status = %q, want none (no cached substitute answer)", rr.Header().Get("X-Upstream-Status"))
	}
	// Every key of the substitute was limited for the model, and it cooled for it.
	if cds := env.state("official").modelCooldowns(time.Now()); len(cds) != 1 || cds[0].Model != incidentOfficialUpstream {
		t.Fatalf("official model cooldowns = %+v", cds)
	}
	for i, n := range env.state("official").keyPool.inFlight {
		if n != 0 {
			t.Fatalf("key %d in flight = %d after the request", i, n)
		}
	}
}
