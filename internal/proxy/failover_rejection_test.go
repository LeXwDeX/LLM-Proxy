package proxy

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
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// rejectionUpstream answers every request with status/body, except requests
// authenticated with one of okKeys, which succeed.
type rejectionUpstream struct {
	srv    *httptest.Server
	hits   atomic.Int32
	okHits atomic.Int32
}

func newRejectionUpstream(t *testing.T, status int, body string, okKeys ...string) *rejectionUpstream {
	t.Helper()
	u := &rejectionUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		cred := r.Header.Get("Authorization") + "|" + r.Header.Get("api-key")
		for _, k := range okKeys {
			if strings.Contains(cred, k) {
				u.okHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"mock":"ok"}`))
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func newRejectionService(t *testing.T, targets ...config.Target) (*Service, *auth.Principal) {
	t.Helper()
	cfg := &config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: targets,
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
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected authenticated principal")
	}
	return s, principal
}

func rejectionTarget(name, url string, keys ...string) config.Target {
	t := config.Target{Name: name, Endpoint: url, ResourcePathPrefix: "/", APIKey: keys[0]}
	if len(keys) > 1 {
		t.APIKeys = keys[1:]
	}
	return t
}

func serveRejection(s *Service, principal *auth.Principal) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	return rr
}

// serveUntilHit sends requests until the rejecting upstream is reached and
// returns that response.
func serveUntilHit(t *testing.T, s *Service, principal *auth.Principal, u *rejectionUpstream) *httptest.ResponseRecorder {
	t.Helper()
	for i := 0; i < 16; i++ {
		before := u.hits.Load()
		rr := serveRejection(s, principal)
		if u.hits.Load() > before {
			return rr
		}
	}
	t.Fatal("no request reached the rejecting target")
	return nil
}

func targetStatusByName(t *testing.T, s *Service, name string) TargetStatus {
	t.Helper()
	for _, st := range s.TargetStatuses(time.Now()) {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("target %s missing", name)
	return TargetStatus{}
}

func assertRejectionMuted(t *testing.T, s *Service, name string, status int) {
	t.Helper()
	st := targetStatusByName(t, s, name)
	if !st.Muted || st.MuteStatus != status || st.MuteReason != muteReasonForStatus(status) {
		t.Fatalf("%s muted=%v status=%d reason=%q, want %d cooldown", name, st.Muted, st.MuteStatus, st.MuteReason, status)
	}
	if remaining := time.Until(st.MutedUntil); remaining < 59*time.Minute || remaining > time.Hour {
		t.Fatalf("%s cooldown %v, want ~1h", name, remaining)
	}
}

func assertNotMuted(t *testing.T, s *Service, name string) {
	t.Helper()
	if st := targetStatusByName(t, s, name); st.Muted {
		t.Fatalf("%s muted (reason=%q) but must stay healthy", name, st.MuteReason)
	}
}

// A single-key target has no key pool, so a 402 (DeepSeek "Insufficient
// Balance") used to pass straight through. It must now fail over and cool
// the target for an hour.
func TestSingleKey402FailsOverAndCools(t *testing.T) {
	bad := newRejectionUpstream(t, http.StatusPaymentRequired, `{"error":{"message":"Insufficient Balance","type":"unknown_error"}}`)
	ok := newRejectionUpstream(t, http.StatusOK, ``, "k-ok")
	s, p := newRejectionService(t, rejectionTarget("cheap", bad.srv.URL, "k-bad"), rejectionTarget("next", ok.srv.URL, "k-ok"))

	rr := serveUntilHit(t, s, p, bad)
	if rr.Code != http.StatusOK || rr.Header().Get("X-Proxy-Target") != "next" {
		t.Fatalf("status=%d target=%q body=%s, want 200 from next", rr.Code, rr.Header().Get("X-Proxy-Target"), rr.Body.String())
	}
	assertRejectionMuted(t, s, "cheap", http.StatusPaymentRequired)
	if got := s.MetricsSnapshot().UpstreamRejectionMuted["402"]; got != 1 {
		t.Fatalf("402 mute counter = %d, want 1", got)
	}
}

// A recognised credential 401 on a single-key target fails over and cools.
func TestSingleKeyInvalidToken401FailsOver(t *testing.T) {
	bad := newRejectionUpstream(t, http.StatusUnauthorized, `{"error":{"code":"invalid_api_key","message":"Incorrect API key provided"}}`)
	ok := newRejectionUpstream(t, http.StatusOK, ``, "k-ok")
	s, p := newRejectionService(t, rejectionTarget("cheap", bad.srv.URL, "k-bad"), rejectionTarget("next", ok.srv.URL, "k-ok"))

	rr := serveUntilHit(t, s, p, bad)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 after 401 failover", rr.Code, rr.Body.String())
	}
	assertRejectionMuted(t, s, "cheap", http.StatusUnauthorized)
}

// An unrecognised 401 still rejects the proxy credential: it fails over, but
// the target cools only briefly first; a repeat within the hour escalates.
func TestSingleKeyUnknown401FailsOverWithShortCooldown(t *testing.T) {
	bad := newRejectionUpstream(t, http.StatusUnauthorized, `<html>401 Authorization Required</html>`)
	ok := newRejectionUpstream(t, http.StatusOK, ``, "k-ok")
	s, p := newRejectionService(t, rejectionTarget("cheap", bad.srv.URL, "k-bad"), rejectionTarget("next", ok.srv.URL, "k-ok"))

	rr := serveUntilHit(t, s, p, bad)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q, want failover", rr.Code, rr.Body.String())
	}
	st := targetStatusByName(t, s, "cheap")
	if remaining := time.Until(st.MutedUntil); !st.Muted || st.MuteStatus != 401 || remaining > defaultUnverifiedRejectionCooldown || remaining < defaultUnverifiedRejectionCooldown-time.Minute {
		t.Fatalf("first unconfirmed 401 cooldown: %+v remaining=%v", st, remaining)
	}
	// The cooldown ends; the next unconfirmed 401 within the hour escalates.
	state, _ := s.targetByName("cheap")
	state.mu.Lock()
	state.mutedUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()
	s.affinity = newAffinityMap() // the client is now sticky to "next"
	rr = serveUntilHit(t, s, p, bad)
	if rr.Code != http.StatusOK {
		t.Fatalf("second: status=%d", rr.Code)
	}
	assertRejectionMuted(t, s, "cheap", http.StatusUnauthorized)
}

// An unstructured 403 (HTML/WAF page) also cools briefly first.
func TestUnstructured403CoolsBrieflyFirst(t *testing.T) {
	bad := newRejectionUpstream(t, http.StatusForbidden, `<html>Access denied</html>`)
	ok := newRejectionUpstream(t, http.StatusOK, ``, "k-ok")
	s, p := newRejectionService(t, rejectionTarget("cheap", bad.srv.URL, "k-bad"), rejectionTarget("next", ok.srv.URL, "k-ok"))
	if rr := serveUntilHit(t, s, p, bad); rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	st := targetStatusByName(t, s, "cheap")
	if remaining := time.Until(st.MutedUntil); !st.Muted || remaining > defaultUnverifiedRejectionCooldown {
		t.Fatalf("unstructured 403 cooldown: %+v remaining=%v", st, remaining)
	}
}

// A single-key 429 has no key loop: the current request moves to the next
// candidate, and the (target, upstream model) pair takes the classified 429
// cooldown. The target itself keeps serving its other models (model_cooldown_*).
func TestSingleKey429FailsOverWithOrdinaryCooldown(t *testing.T) {
	bad := newRejectionUpstream(t, http.StatusTooManyRequests, `{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`)
	ok := newRejectionUpstream(t, http.StatusOK, ``, "k-ok")
	s, p := newRejectionService(t, rejectionTarget("cheap", bad.srv.URL, "k-bad"), rejectionTarget("next", ok.srv.URL, "k-ok"))

	rr := serveUntilHit(t, s, p, bad)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 after 429 failover", rr.Code, rr.Body.String())
	}
	st := targetStatusByName(t, s, "cheap")
	if st.Muted || st.Phase != phaseReady {
		t.Fatalf("cheap muted=%v phase=%s: a 429 cools the model, not the target", st.Muted, st.Phase)
	}
	if len(st.ModelCooldowns) != 1 {
		t.Fatalf("model cooldowns = %+v, want exactly the 429-ed model", st.ModelCooldowns)
	}
	cd := st.ModelCooldowns[0]
	if cd.Model != "deepseek-chat" || cd.Reason != "upstream_429" || cd.Status != http.StatusTooManyRequests {
		t.Fatalf("model cooldown = %+v, want the classified 429 cooldown of deepseek-chat", cd)
	}
	// Half the 1-minute quiet period, not the 1h account-rejection cooldown.
	if remaining := time.Until(cd.Until); remaining > 31*time.Second || remaining < 29*time.Second {
		t.Fatalf("429 cooldown %v, want ~30s", remaining)
	}
}

// A content-moderation 403 describes the request: pass it through, keep the
// target healthy, and do not try other candidates.
func TestModeration403PassesThroughWithoutMute(t *testing.T) {
	const body = `{"error":{"code":"content_policy_violation","message":"blocked"}}`
	bad := newRejectionUpstream(t, http.StatusForbidden, body)
	ok := newRejectionUpstream(t, http.StatusOK, ``, "k-ok")
	s, p := newRejectionService(t, rejectionTarget("cheap", bad.srv.URL, "k-bad"), rejectionTarget("next", ok.srv.URL, "k-ok"))

	rr := serveUntilHit(t, s, p, bad)
	if rr.Code != http.StatusForbidden || rr.Body.String() != body {
		t.Fatalf("status=%d body=%q, want original moderation 403", rr.Code, rr.Body.String())
	}
	assertNotMuted(t, s, "cheap")
}

func TestUpstreamContentModerationShapes(t *testing.T) {
	openrouter := &Target{Name: "or", UsageAdapter: usageAdapterOpenRouter}
	standard := &Target{Name: "std", UsageAdapter: "standard"}
	orModeration := []byte(`{"error":{"message":"Moderation rejected","metadata":{"reasons":["moderation"],"flagged_input":"x"}}}`)
	cases := []struct {
		name   string
		target *Target
		body   string
		want   bool
	}{
		{"content_filter code", standard, `{"error":{"code":"content_filter"}}`, true},
		{"moderation type", standard, `{"error":{"type":"moderation_blocked"}}`, true},
		{"account suspended", standard, `{"error":{"type":"permission_error","message":"content_filter"}}`, false},
		{"openrouter moderation with patch", openrouter, string(orModeration), true},
		{"openrouter shape without patch", standard, string(orModeration), false},
		{"openrouter budget 403", openrouter, `{"error":{"metadata":{"limit_source":"openrouter_key_limit"}}}`, false},
		{"not json", standard, `forbidden`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstreamContentModeration(tc.target, []byte(tc.body)); got != tc.want {
				t.Fatalf("upstreamContentModeration = %v, want %v", got, tc.want)
			}
		})
	}
}

// A multi-key target where only one account is rejected: the key loop cools
// that key and serves the request with the next key; the target stays up.
func TestMultiKey403CoolsOnlyTheKey(t *testing.T) {
	up := newRejectionUpstream(t, http.StatusForbidden, `{"error":{"type":"permission_error","message":"account suspended"}}`, "k-good")
	other := newRejectionUpstream(t, http.StatusOK, ``, "k-other")
	s, p := newRejectionService(t, rejectionTarget("multi", up.srv.URL, "k-bad", "k-good"), rejectionTarget("other", other.srv.URL, "k-other"))

	var reachedBad bool
	for i := 0; i < 24 && !reachedBad; i++ {
		before := up.hits.Load() - up.okHits.Load()
		rr := serveRejection(s, p)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: status=%d body=%s", i, rr.Code, rr.Body.String())
		}
		reachedBad = up.hits.Load()-up.okHits.Load() > before
		if reachedBad && rr.Header().Get("X-Proxy-Target") != "multi" {
			t.Fatalf("served by %q, want multi via its healthy key", rr.Header().Get("X-Proxy-Target"))
		}
	}
	if !reachedBad {
		t.Fatal("no request used the rejected key")
	}
	assertNotMuted(t, s, "multi")
	var cooled bool
	for _, st := range s.targetSnapshot() {
		if st.Target().Name != "multi" {
			continue
		}
		for _, ks := range st.keyPool.status() {
			if ks.ExhaustReason == keyReasonForbidden {
				cooled = true
				if remaining := time.Until(ks.CooldownEnd); remaining < 59*time.Minute || remaining > time.Hour {
					t.Fatalf("key cooldown %v, want ~1h", remaining)
				}
				if ks.Status == "" {
					t.Fatal("key status label missing")
				}
			}
		}
	}
	if !cooled {
		t.Fatal("rejected key was not cooled with reason forbidden")
	}
}

// Every key of a multi-key target rejected: the key loop converges, the
// target cools for an hour and the request moves to the next candidate.
func TestMultiKeyAll403FailsOverAndCoolsTarget(t *testing.T) {
	up := newRejectionUpstream(t, http.StatusForbidden, `{"error":{"type":"permission_error","message":"account suspended"}}`)
	other := newRejectionUpstream(t, http.StatusOK, ``, "k-other")
	s, p := newRejectionService(t, rejectionTarget("multi", up.srv.URL, "k-b1", "k-b2"), rejectionTarget("other", other.srv.URL, "k-other"))

	rr := serveUntilHit(t, s, p, up)
	if rr.Code != http.StatusOK || rr.Header().Get("X-Proxy-Target") != "other" {
		t.Fatalf("status=%d target=%q body=%s, want 200 from other", rr.Code, rr.Header().Get("X-Proxy-Target"), rr.Body.String())
	}
	assertRejectionMuted(t, s, "multi", http.StatusForbidden)
}

// When every candidate is cooling after a rejection, the client sees the
// real upstream status and reason instead of a bare 503.
func TestAllCandidatesCoolingReturnsRealRejection(t *testing.T) {
	const body = `{"error":{"code":"insufficient_quota","type":"permission_error","message":"account suspended"}}`
	bad := newRejectionUpstream(t, http.StatusForbidden, body)
	s, p := newRejectionService(t, rejectionTarget("only", bad.srv.URL, "k-bad"))

	first := serveRejection(s, p)
	if first.Code != http.StatusForbidden || first.Body.String() != body {
		t.Fatalf("first: status=%d body=%q, want the real upstream 403", first.Code, first.Body.String())
	}
	second := serveRejection(s, p)
	if bad.hits.Load() != 1 {
		t.Fatalf("cooling target was called again: hits=%d", bad.hits.Load())
	}
	if second.Code != http.StatusForbidden {
		t.Fatalf("second: status=%d body=%s, want 403 with cooldown reason", second.Code, second.Body.String())
	}
	if ra := second.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After = %q", ra)
	}
	var got struct {
		Error struct {
			Type       string `json:"type"`
			Code       string `json:"code"`
			Status     int    `json:"status"`
			Target     string `json:"target"`
			MutedUntil string `json:"muted_until"`
		} `json:"error"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, second.Body.String())
	}
	if got.Error.Type != "upstream_rejected" || got.Error.Code != "insufficient_quota" || got.Error.Status != 403 || got.Error.Target != "only" || got.Error.MutedUntil == "" {
		t.Fatalf("cooldown envelope = %+v", got.Error)
	}
	if strings.Contains(second.Body.String(), "account suspended") {
		t.Fatal("cooldown response must not replay the upstream message")
	}
}

// Ordinary (transient) breaker mutes with no other candidate: one early probe
// goes through per spacing interval; while it fails the cooldown neither
// doubles nor extends, and other requests get the ordinary 503.
func TestAllCandidatesOrdinaryMuteStays503(t *testing.T) {
	bad := newRejectionUpstream(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	s, p := newRejectionService(t, rejectionTarget("only", bad.srv.URL, "k-bad"))
	// The 30s 500 cooldown would be waited out; the wait has its own tests.
	s.inProxyWaitMaxOverride = time.Millisecond
	_ = serveRejection(s, p) // 500: cools
	until := targetStatusByName(t, s, "only").MutedUntil
	probe := serveRejection(s, p)
	if probe.Code != http.StatusInternalServerError || bad.hits.Load() != 2 {
		t.Fatalf("early probe: status=%d hits=%d, want one probe to the upstream", probe.Code, bad.hits.Load())
	}
	if got := targetStatusByName(t, s, "only"); !got.MutedUntil.Equal(until) || got.ConsecutiveFailures != 1 {
		t.Fatalf("failed early probe changed the cooldown: until %v -> %v streak=%d", until, got.MutedUntil, got.ConsecutiveFailures)
	}
	rr := serveRejection(s, p)
	if rr.Code != http.StatusServiceUnavailable || bad.hits.Load() != 2 {
		t.Fatalf("within the probe spacing: status=%d hits=%d, want 503 without an upstream call", rr.Code, bad.hits.Load())
	}
}

// A successful early probe ends the cooldown at once.
func TestEarlyProbeRecoversTransientCooldown(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"mock":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	s, p := newRejectionService(t, rejectionTarget("only", srv.URL, "k"))
	_ = serveRejection(s, p)
	fail.Store(false)
	if rr := serveRejection(s, p); rr.Code != http.StatusOK {
		t.Fatalf("early probe status=%d", rr.Code)
	}
	if st := targetStatusByName(t, s, "only"); st.Muted {
		t.Fatal("successful early probe must clear the cooldown")
	}
}

// Rate limits and account rejections are not probed early.
func TestEarlyProbeSkipsRateLimitAndAccountCooldowns(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusForbidden} {
		up := newRejectionUpstream(t, status, `{"error":{"type":"permission_error","message":"x"}}`)
		s, p := newRejectionService(t, rejectionTarget("only", up.srv.URL, "k"))
		s.inProxyWaitMaxOverride = time.Millisecond // a 30s 429 cooldown would be waited out, then retried
		_ = serveRejection(s, p)
		_ = serveRejection(s, p)
		if up.hits.Load() != 1 {
			t.Fatalf("status %d cooldown was probed early: hits=%d", status, up.hits.Load())
		}
	}
}

// Manual recovery clears the cooldown immediately.
func TestClearTargetMute(t *testing.T) {
	bad := newRejectionUpstream(t, http.StatusForbidden, `{"error":{"type":"permission_error"}}`)
	s, p := newRejectionService(t, rejectionTarget("only", bad.srv.URL, "k-bad"))
	_ = serveRejection(s, p)
	assertRejectionMuted(t, s, "only", http.StatusForbidden)
	if !s.ClearTargetMute("ONLY") {
		t.Fatal("ClearTargetMute returned false")
	}
	st := targetStatusByName(t, s, "only")
	if st.Muted || st.MuteReason != "" || st.ConsecutiveFailures != 0 || st.Phase != phaseReady {
		t.Fatalf("after clear: %+v", st)
	}
	if s.ClearTargetMute("missing") {
		t.Fatal("unknown target must report false")
	}
}
