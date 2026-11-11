package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Production incident: qwen4-max-preview is rate limited by the Bailian
// subscription. The 429 must cool that model only; a deepseek session bound to
// the same target must keep its target instead of being pushed to the official
// channel (which rejects it with a 400 the client cannot survive).
func TestModelRateLimitKeepsOtherModelSessionOnTarget(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)

	first := env.do("deepseek", "sess-ds")
	if first.Code != http.StatusOK || first.Header().Get("X-Proxy-Target") != "bailian" {
		t.Fatalf("setup: deepseek session status=%d target=%q", first.Code, first.Header().Get("X-Proxy-Target"))
	}

	qwen := env.do("qwen-max", "")
	if qwen.Code != http.StatusTooManyRequests {
		t.Fatalf("qwen-max status = %d, want the upstream's 429 (no alternative target)", qwen.Code)
	}
	st := env.state("bailian")
	if st.IsMuted(time.Now()) || st.Phase(time.Now()) != phaseReady {
		t.Fatalf("a model 429 must not mute the target: muted=%v phase=%s", st.IsMuted(time.Now()), st.Phase(time.Now()))
	}
	if cds := st.modelCooldowns(time.Now()); len(cds) != 1 || cds[0].Model != incidentQwenUpstream {
		t.Fatalf("model cooldowns = %+v, want only %s", cds, incidentQwenUpstream)
	}

	// The deepseek session keeps its target; the official channel is never touched.
	for i := 0; i < 3; i++ {
		rr := env.do("deepseek", "sess-ds")
		if rr.Code != http.StatusOK || rr.Header().Get("X-Proxy-Target") != "bailian" {
			t.Fatalf("deepseek #%d status=%d target=%q body=%s, want 200 from bailian", i, rr.Code, rr.Header().Get("X-Proxy-Target"), rr.Body.String())
		}
	}
	if env.official.total() != 0 {
		t.Fatalf("official channel received %d requests, want 0", env.official.total())
	}
	// Another model's successes never cut the qwen cooldown short.
	if _, ok := st.modelLimitedUntil(incidentQwenUpstream, time.Now()); !ok {
		t.Fatal("deepseek success cleared the qwen cooldown")
	}
}

func TestModelCooldownBlocksOnlyThatModelsRequests(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	if rr := env.do("qwen-max", ""); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("setup 429 status = %d", rr.Code)
	}
	before := env.bailian.hitsFor(incidentQwenUpstream)
	rr := env.do("qwen-max", "")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("limited model status = %d, want 503 while the model cools", rr.Code)
	}
	if got := env.bailian.hitsFor(incidentQwenUpstream); got != before {
		t.Fatalf("a cooling model reached the upstream (%d -> %d)", before, got)
	}
	ra, err := strconv.Atoi(rr.Header().Get("Retry-After"))
	if err != nil || ra < 100 || ra > 121 {
		t.Fatalf("Retry-After = %q, want the remaining cooldown (~120s)", rr.Header().Get("Retry-After"))
	}
	// Another model of the same target is untouched.
	if rr := env.do("deepseek", ""); rr.Code != http.StatusOK {
		t.Fatalf("other model status = %d", rr.Code)
	}
}

func TestModelCooldownEndsAtDeadlineAndSuccessClearsIt(t *testing.T) {
	env := newIncidentEnv(t, "", http.StatusBadRequest)
	st := env.state("bailian")
	env.svc.markTargetFailure(st, time.Now(), 429, nil, false, incidentQwenUpstream)
	// Shorten to a few ms: the cooldown ends on its own.
	st.mu.Lock()
	ml := st.modelLimits[incidentQwenUpstream]
	ml.until = time.Now().Add(40 * time.Millisecond)
	st.modelLimits[incidentQwenUpstream] = ml
	st.mu.Unlock()
	// The upstream recovered.
	env.bailian.setRespond(nil)
	if _, _, _, err := env.svc.selectTarget(env.principal, "192.0.2.7", "", "", nil, nil, "qwen-max", "/v1/chat/completions", time.Now()); err == nil {
		t.Fatal("qwen-max must be unavailable while the model cools")
	}
	time.Sleep(60 * time.Millisecond)
	rr := env.do("qwen-max", "")
	if rr.Code != http.StatusOK || rr.Header().Get("X-Proxy-Target") != "bailian" {
		t.Fatalf("after the deadline status=%d target=%q, want 200 from bailian", rr.Code, rr.Header().Get("X-Proxy-Target"))
	}
	st.mu.RLock()
	_, kept := st.modelLimits[incidentQwenUpstream]
	st.mu.RUnlock()
	if kept {
		t.Fatal("the model's own success must clear its limit entry and streak")
	}
}

func TestExplicitTargetRefusesCoolingModel(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	if rr := env.do("qwen-max", ""); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("setup 429 status = %d", rr.Code)
	}
	before := env.bailian.total()
	rr := env.doCtx(t.Context(), "qwen-max", "", func(r *http.Request) { r.Header.Set(headerProxyTarget, "bailian") })
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("explicit target with a cooling model status = %d, want 503", rr.Code)
	}
	if env.bailian.total() != before {
		t.Fatal("explicit target request reached the cooling model upstream")
	}
	// The same explicit target still serves the other model.
	rr = env.doCtx(t.Context(), "deepseek", "", func(r *http.Request) { r.Header.Set(headerProxyTarget, "bailian") })
	if rr.Code != http.StatusOK {
		t.Fatalf("explicit target, healthy model status = %d", rr.Code)
	}
}

func TestModelCooldownDoesNotBreakOtherTargetSelection(t *testing.T) {
	// qwen cooling on the only target that serves it must not make selection
	// for deepseek skip it: the model check uses the request's own upstream.
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	st := env.state("bailian")
	env.svc.markTargetFailure(st, time.Now(), 429, nil, false, incidentQwenUpstream)
	state, _, kind, err := env.svc.selectTarget(env.principal, "192.0.2.7", "bailian", "", nil, nil, "deepseek", "/v1/chat/completions", time.Now())
	if err != nil || state == nil || state.Target().Name != "bailian" || kind != selectionAffinityHit {
		t.Fatalf("sticky bailian for deepseek: state=%v kind=%v err=%v", state, kind, err)
	}
	// And a sticky binding for the cooling model yields to the other member.
	_, _, _, err = env.svc.selectTarget(env.principal, "192.0.2.7", "bailian", "", nil, nil, "qwen-max", "/v1/chat/completions", time.Now())
	if err == nil {
		t.Fatal("sticky target with a cooling model must not be hit")
	}
}

// Multi-key targets: a generic 429 limits the (key, upstream model) pair, not
// the key. Another model keeps using the same keys; the bounded same-key retry
// of the key pool is unchanged; once every key is limited for the model, the
// target cools for that model exactly like a single-key target.
func TestMultiKeyTarget429LimitsOnlyTheKeyModelPair(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		hits[body.Model]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if body.Model == "qwen-up" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"Rate limit reached"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	}))
	t.Cleanup(srv.Close)
	svc, err := NewService(&config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 10},
		Targets: []config.Target{{Name: "pool", Endpoint: srv.URL, APIKey: "k0", APIKeys: []string{"k1"}, ModelMappings: []config.ModelMapping{
			{Upstream: "qwen-up", Fallback: "qwen"}, {Upstream: "ds-up", Fallback: "deepseek"}}}},
	}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	svc.SetInProxyWaitLimits(-1, 0) // the 503 while the model cools is under test
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatal(err)
	}
	principal, _ := store.Authenticate("token")
	send := func(model string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rr := httptest.NewRecorder()
		svc.ServeHTTP(rr, req)
		return rr
	}

	if rr := send("qwen"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("qwen status = %d, want the upstream 429 once every key is limited for it", rr.Code)
	}
	mu.Lock()
	qwenHits := hits["qwen-up"]
	mu.Unlock()
	if qwenHits != 4 {
		t.Fatalf("qwen upstream hits = %d, want 4 (two keys, one same-key retry each: the key-pool exception is unchanged)", qwenHits)
	}
	state, _ := svc.targetByName("pool")
	state.keyPool.mu.Lock()
	for i := range state.keyPool.entries {
		e := state.keyPool.entries[i]
		if e.exhausted || e.modelLimits["qwen-up"] == nil || !e.modelLimits["qwen-up"].until.After(time.Now()) {
			state.keyPool.mu.Unlock()
			t.Fatalf("key %d exhausted=%v limits=%v, want an active key limited for qwen-up only", i, e.exhausted, e.modelLimits)
		}
	}
	state.keyPool.mu.Unlock()
	if state.IsMuted(time.Now()) {
		t.Fatal("a generic 429 must not mute the target")
	}
	if cds := state.modelCooldowns(time.Now()); len(cds) != 1 || cds[0].Model != "qwen-up" {
		t.Fatalf("model cooldowns = %+v, want qwen-up (every key limited for it)", cds)
	}
	// The same keys keep serving the other model.
	if rr := send("deepseek"); rr.Code != http.StatusOK {
		t.Fatalf("deepseek status = %d, want 200 on the same keys", rr.Code)
	}
	// And qwen is refused locally, with Retry-After, while it cools.
	before := hits["qwen-up"]
	rr := send("qwen")
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("qwen while cooling: %d Retry-After=%q, want 503 with Retry-After", rr.Code, rr.Header().Get("Retry-After"))
	}
	mu.Lock()
	after := hits["qwen-up"]
	mu.Unlock()
	if after != before {
		t.Fatal("a cooling model reached the upstream")
	}
	// Admin view: which model is limited on which key.
	var limited int
	for _, ks := range svc.KeyPoolStatus("pool") {
		for _, ml := range ks.ModelLimits {
			if ml.Model == "qwen-up" {
				limited++
			}
		}
	}
	if limited != 2 {
		t.Fatalf("KeyStatus.ModelLimits report %d keys limited for qwen-up, want 2", limited)
	}
}
