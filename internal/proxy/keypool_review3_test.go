package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Review round 3 on multi-key targets: bounded same-key retry, GLM and
// encoding carve-outs, key affinity and wake-up order.

// newModelPool serves a two-model target (qwen -> qwen-up, deepseek -> ds-up)
// with the given keys; the handler sees the upstream model and the key.
func newModelPool(t *testing.T, adapter string, handler func(w http.ResponseWriter, model, key string), keys ...string) (*Service, func(ctx context.Context, model string) *httptest.ResponseRecorder) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key == "" {
			key = r.Header.Get("api-key")
		}
		handler(w, body.Model, key)
	}))
	t.Cleanup(srv.Close)
	svc, err := NewService(&config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 10},
		Targets: []config.Target{{Name: "pool", Endpoint: srv.URL, UsageAdapter: adapter, APIKey: keys[0], APIKeys: keys[1:], ModelMappings: []config.ModelMapping{
			{Upstream: "qwen-up", Fallback: "qwen"}, {Upstream: "ds-up", Fallback: "deepseek"}}}},
	}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	svc.SetInProxyWaitLimits(-1, 0)
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatal(err)
	}
	principal, _ := store.Authenticate("token")
	return svc, func(ctx context.Context, model string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		rr := httptest.NewRecorder()
		svc.ServeHTTP(rr, req.WithContext(auth.WithPrincipal(ctx, principal)))
		return rr
	}
}

func okChat(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
}

// N1: the same-key retry is bounded per request. A success of another model on
// the same key must not reset what this request already spent.
func TestMultiKeySameKeyRetryIsBoundedPerRequest(t *testing.T) {
	var qwenHits atomic.Int32
	_, send := newModelPool(t, "", func(w http.ResponseWriter, model, key string) {
		if model == "qwen-up" {
			qwenHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded","message":"slow"}}`))
			return
		}
		okChat(w)
	}, "k0", "k1")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // deepseek keeps succeeding on the same client's keys
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				send(context.Background(), "deepseek")
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	rr := send(ctx, "qwen")
	close(stop)
	wg.Wait()
	if ctx.Err() != nil {
		t.Fatalf("the qwen request was still retrying after %v (status %d, qwen hits %d)", time.Since(start), rr.Code, qwenHits.Load())
	}
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream 429", rr.Code)
	}
	if n := qwenHits.Load(); n > 4 {
		t.Fatalf("qwen upstream hits = %d, want at most 4 (two keys, one same-key retry each)", n)
	}
}

// N3: a GLM-selected target keeps the key-level rate limit for every 429, also
// when the body cannot be read (an encoding the proxy does not decode).
func TestGLMTargetNonIdentity429StaysKeyLevel(t *testing.T) {
	svc, send := newModelPool(t, config.UsageAdapterGLM, func(w http.ResponseWriter, model, key string) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "br")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("not-brotli"))
	}, "k0", "k1")
	send(context.Background(), "qwen")
	state, _ := svc.targetByName("pool")
	state.keyPool.mu.Lock()
	defer state.keyPool.mu.Unlock()
	exhausted := 0
	for i, e := range state.keyPool.entries {
		if len(e.modelLimits) != 0 {
			t.Fatalf("key %d has model limits %v: a GLM target must stay key-level", i, e.modelLimits)
		}
		if e.exhausted {
			exhausted++
		}
	}
	if exhausted == 0 {
		t.Fatal("no key was rate limited at key level")
	}
}

// N5: skipping a key for a limited model must not move the client's key binding.
func TestKeyPoolModelOverflowDoesNotRebindClient(t *testing.T) {
	p := newModelTestPool(t, "k0", "k1", "k2")
	_, bound := p.selectKeyForClientModel("client", "deepseek")
	p.markRateLimitedFor(bound, "qwen", nil, false)
	if _, idx := p.selectKeyForClientModel("client", "qwen"); idx == bound || idx < 0 {
		t.Fatalf("qwen selected key %d, want another key than the limited %d", idx, bound)
	}
	if _, idx := p.selectKeyForClientModel("client", "deepseek"); idx != bound {
		t.Fatalf("deepseek selected key %d after a qwen overflow, want the original key %d", idx, bound)
	}
	// A client without a binding that meets a limited key is not bound by that pick either.
	p2 := newModelTestPool(t, "k0", "k1")
	p2.markRateLimitedFor(0, "qwen", nil, false)
	if _, idx := p2.selectKeyForClientModel("fresh", "qwen"); idx != 1 {
		t.Fatalf("fresh client qwen key = %d", idx)
	}
	p2.mu.Lock()
	_, bound2 := p2.clientAffinity["fresh"]
	p2.mu.Unlock()
	if bound2 {
		t.Fatal("a pick that skipped a model-limited key must not create the client's binding")
	}
}

// N7: with an exhausted key that can be woken, the wake-up path runs before the
// target is declared limited for the model.
func TestAllKeysModelLimitedWakesExhaustedKeyFirst(t *testing.T) {
	svc, send := newModelPool(t, "", func(w http.ResponseWriter, model, key string) {
		if key == "k0" {
			okChat(w) // k0 serves both the wake-up probe (GET) and the request
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}, "k0", "k1")
	state, _ := svc.targetByName("pool")
	state.keyPool.markExhaustedWithError(0, "probeable_reason", "") // wakeable, not a hard failure
	if !state.keyPool.markModelRateLimited(1, "qwen-up", nil) {
		t.Fatal("setup")
	}
	rr := send(context.Background(), "qwen")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the exhausted key should have been woken before the target was limited for the model", rr.Code)
	}
	if cds := state.modelCooldowns(time.Now()); len(cds) != 0 {
		t.Fatalf("model cooldowns = %+v, want none", cds)
	}
}
