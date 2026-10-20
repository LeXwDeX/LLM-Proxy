package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// #159 F3+F5: a 429 that proves the key itself is unusable (account balance
// exhausted, GLM arrears, GLM plan expired) must not be retried on the same key
// and must take the key out of rotation for longer than a transient cooldown.

// keyedUpstream answers with failBody/429 for failKey and a normal chat
// completion for any other key, counting hits per key.
type keyedUpstream struct {
	mu   sync.Mutex
	hits map[string]int
}

func (u *keyedUpstream) count(key string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[key]
}

func newKeyedUpstream(t *testing.T, failKey, failBody string) (*keyedUpstream, *httptest.Server) {
	t.Helper()
	u := &keyedUpstream{hits: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key == "" {
			key = r.Header.Get("api-key")
		}
		u.mu.Lock()
		u.hits[key]++
		u.mu.Unlock()
		if key == failKey {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(failBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	}))
	t.Cleanup(srv.Close)
	return u, srv
}

func serveKeyExhaustionRequest(t *testing.T, target config.Target, model string) (*Service, *httptest.ResponseRecorder) {
	t.Helper()
	cfg := &config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{target},
		Logging: config.LoggingConfig{Level: "error", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)
	return service, rr
}

func TestService429AccountExhaustionSkipsSameKeyRetry(t *testing.T) {
	cases := []struct {
		name       string
		adapter    string
		model      string
		body       string
		wantReason string
	}{
		{"credit_balance_exhausted", "", "gpt-4o",
			`{"error":{"code":"credit_balance_exhausted","type":"insufficient_quota","message":"Credit balance exhausted"}}`, "quota_exceeded_api"},
		{"glm_1113_arrears", config.UsageAdapterGLM, "glm-4.6",
			`{"error":{"code":"1113","message":"[1113][您的账户已欠费，请充值后重试]"}}`, glmAccountArrears},
		{"glm_1309_plan_expired", config.UsageAdapterGLM, "glm-4.6",
			`{"error":{"code":"1309","message":"[1309][您的 GLM Coding Plan 套餐已到期]"}}`, glmPlanExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, srv := newKeyedUpstream(t, "k0", tc.body)
			service, rr := serveKeyExhaustionRequest(t, config.Target{
				Name: "pool", Endpoint: srv.URL, UsageAdapter: tc.adapter, APIKey: "k0", APIKeys: []string{"k1"},
			}, tc.model)
			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200 from the healthy key, got %d body=%s", rr.Code, rr.Body.String())
			}
			if n := up.count("k0"); n != 1 {
				t.Fatalf("exhausted key hit %d times, want 1 (no same-key retry)", n)
			}
			state, _ := service.targetByName("pool")
			e := state.keyPool.entries[0]
			if !e.exhausted || e.exhaustReason != tc.wantReason {
				t.Fatalf("key0 exhausted=%v reason=%q, want reason %q", e.exhausted, e.exhaustReason, tc.wantReason)
			}
			if time.Until(e.cooldownEnd) < time.Hour {
				t.Fatalf("key0 cooldown ends in %s, want a long-term disable", time.Until(e.cooldownEnd))
			}
			if !isAccountLevelExhaustion(e.exhaustReason) || !isHardFailureReason(e.exhaustReason) {
				t.Fatalf("reason %q must be account-level and hard failure", e.exhaustReason)
			}
		})
	}
}

// Ordinary throttling keeps the bounded same-key retry (red line: do not widen
// or remove the key-pool exception).
func TestService429PlainRateLimitStillRetriesSameKey(t *testing.T) {
	up, srv := newKeyedUpstream(t, "k0", `{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"Rate limit reached"}}`)
	_, rr := serveKeyExhaustionRequest(t, config.Target{
		Name: "pool", Endpoint: srv.URL, APIKey: "k0", APIKeys: []string{"k1"},
	}, "gpt-4o")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if n := up.count("k0"); n != 2 {
		t.Fatalf("plain 429 key hit %d times, want 2 (one same-key retry)", n)
	}
}

func TestGLMAccountLevelCodes(t *testing.T) {
	now := time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC)
	for code, reason := range map[string]string{"1113": glmAccountArrears, "1309": glmPlanExpired} {
		if got := glmReasonForCode(code); got != reason {
			t.Errorf("glmReasonForCode(%s) = %q, want %q", code, got, reason)
		}
		if got := glmCooldownForCode(code, time.Time{}, now); !got.Equal(permanentCooldownEnd) {
			t.Errorf("glmCooldownForCode(%s) = %v, want permanent", code, got)
		}
		if isGLMRateLimitReason(reason) || isTransientGLMReason(reason) || isRecoverableAfterCooldown(reason) {
			t.Errorf("reason %q must not be treated as a recoverable rate limit", reason)
		}

		pool := newTestKeyPool([]string{"key-0"})
		pool.markRateLimitedWithBody(0, []byte(`{"error":{"code":"`+code+`","message":"x"}}`))
		e := pool.entries[0]
		if e.exhaustReason != reason || !e.cooldownEnd.Equal(permanentCooldownEnd) || e.consecutiveRateLimit != 0 {
			t.Errorf("429 path code %s: reason=%q cooldown=%v consecutive=%d", code, e.exhaustReason, e.cooldownEnd, e.consecutiveRateLimit)
		}
		// Survives a key-pool rebuild (config reload) like other permanent reasons.
		rebuilt := newTestKeyPool([]string{"key-0"})
		rebuilt.restoreExhaustedKeys(pool.exhaustedKeys())
		if r := rebuilt.entries[0]; !r.exhausted || r.exhaustReason != reason || !r.cooldownEnd.Equal(permanentCooldownEnd) {
			t.Errorf("code %s after rebuild: exhausted=%v reason=%q cooldown=%v", code, r.exhausted, r.exhaustReason, r.cooldownEnd)
		}
		// Shown in the admin key list as a failure, not as a GLM rate limit.
		if label := keyStatusLabelForReason(reason); !strings.HasPrefix(label, "失效") {
			t.Errorf("code %s label = %q, want a 失效 label", code, label)
		}
	}
	// The 2026-08-14 incident guard stays: other unknown codes keep the short cooldown.
	if got := glmReasonForCode("1311"); got != glmRateLimitUnknown {
		t.Errorf("1311 reason = %q, want unknown", got)
	}
}

// After the operator recharges/renews, the admin wake-up must recover a GLM
// account-level key even though the GLM /models probe path does not exist.
func TestWakeUpRecoversGLMAccountLevelKeyWithoutProbe(t *testing.T) {
	var probes int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		probes++
		mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{Name: "glm", Endpoint: srv.URL + "/api/paas/v4", UsageAdapter: config.UsageAdapterGLM,
			APIKey: "k0", APIKeys: []string{"k1", "k2"}}},
		Logging: config.LoggingConfig{Level: "error", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	state, _ := service.targetByName("glm")
	state.keyPool.markRateLimitedWithBody(0, []byte(`{"error":{"code":"1113","message":"欠费"}}`))
	state.keyPool.markRateLimitedWithBody(1, []byte(`{"error":{"code":"1309","message":"套餐到期"}}`))
	state.keyPool.markRateLimitedWithBody(2, []byte(`{"error":{"code":"1308","message":"5h"}}`))

	recovered, err := service.WakeUpKeys("glm")
	if err != nil {
		t.Fatalf("WakeUpKeys: %v", err)
	}
	if recovered != 2 {
		t.Fatalf("recovered = %d, want 2 (1113 and 1309)", recovered)
	}
	for i := 0; i < 2; i++ {
		if state.keyPool.entries[i].exhausted {
			t.Errorf("key %d still exhausted after wake-up", i)
		}
	}
	// GLM quota-class keys keep waiting for their reset (unchanged behaviour).
	if !state.keyPool.entries[2].exhausted {
		t.Error("1308 key must stay exhausted")
	}
	mu.Lock()
	defer mu.Unlock()
	if probes != 0 {
		t.Errorf("wake-up issued %d /models probes for GLM keys, want 0", probes)
	}
}
