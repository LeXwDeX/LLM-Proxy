package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Issue #151 A5: saving an unrelated target must not retire a warmed lane that
// holds a conversation, so its pooled continuation keeps working.
func TestResponsesWSUnrelatedTargetSaveKeepsContinuation(t *testing.T) {
	u, endpoint := newWSLifecycleUpstreamServer(t, wsModeComplete)
	s := newWSTestService(t, endpoint, true)
	s.SetUsageRecorder(&syncWSRecorder{})
	s.responsesWS.prewarm = false
	if w := wsTestRequest(t, s, "client-a", "/v1/responses", `{"model":"openai-test","input":"hi","stream":true}`); w.Code != 200 {
		t.Fatalf("first request: %d %s", w.Code, w.Body.String())
	}
	other := config.Target{Name: "other", EndpointType: "openai", Endpoint: "http://127.0.0.1:1", APIKey: "other-key",
		ModelMappings: []config.ModelMapping{{Upstream: "other-model", Fallback: "other-alias"}}}
	if err := s.UpdateTargets([]config.Target{wsLifecycleTarget(endpoint, "upstream-test-key"), other}); err != nil {
		t.Fatal(err)
	}
	w := wsTestRequest(t, s, "client-a", "/v1/responses", `{"model":"openai-test","input":"again","stream":true,"previous_response_id":"resp_1"}`)
	if w.Code != 200 {
		t.Fatalf("continuation after unrelated save: %d %s", w.Code, w.Body.String())
	}
	if u.upgrades.Load() != 1 {
		t.Fatalf("upgrades=%d, want the continuation on the original socket", u.upgrades.Load())
	}
	if previous := u.snapshotPreviousIDs(); len(previous) != 2 || previous[1] != "resp_1" {
		t.Fatalf("previous_response_id sequence: %q", previous)
	}
}

// A configuration save still forgets an unsupported upgrade, so a fixed
// upstream is probed again (re-probe-after-save contract).
func TestResponsesWSTargetSaveReprobesUnsupported(t *testing.T) {
	var gets atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			http.Error(w, "upgrade required", http.StatusUpgradeRequired)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"http_response","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer u.Close()
	s := newWSTestService(t, u.URL, true)
	for i := 0; i < 2; i++ {
		wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi"}`)
	}
	if gets.Load() != 1 {
		t.Fatalf("GET=%d before save", gets.Load())
	}
	if err := s.UpdateTargets([]config.Target{wsLifecycleTarget(u.URL, "upstream-test-key")}); err != nil {
		t.Fatal(err)
	}
	wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi"}`)
	if gets.Load() != 2 {
		t.Fatalf("save kept stale unsupported result: GET=%d", gets.Load())
	}
}

// Blocking a key retires every lane opened with it, including one holding a
// conversation, so the blocked credential never carries another turn.
func TestResponsesWSBlockKeyRetiresConversationLanes(t *testing.T) {
	u := &wsPrewarmUpstream{}
	server := httptest.NewServer(http.HandlerFunc(u.serve))
	defer server.Close()
	s := wsTwoKeyService(t, server.URL)
	s.responsesWS.prewarm = false
	if w := wsTestRequest(t, s, "client", "/v1/responses", wsPrewarmBody); w.Code != 200 {
		t.Fatalf("first: %d", w.Code)
	}
	if err := s.BlockKey("openai", u.keyIndexOf(t, 1)); err != nil {
		t.Fatal(err)
	}
	s.responsesWS.mu.Lock()
	lanes := len(s.responsesWS.entries)
	s.responsesWS.mu.Unlock()
	if lanes != 0 {
		t.Fatalf("lanes=%d, want the blocked key's conversation lane retired", lanes)
	}
	waitForWS(t, "the blocked key's socket to close", func() bool { return u.active.Load() == 0 })
}

// Blocking a key leaves lanes of other credentials continuable.
func TestResponsesWSBlockKeyKeepsOtherCredentialLanes(t *testing.T) {
	p := newResponsesWSPool()
	defer p.close()
	keep := &responsesWSConn{responseID: "resp_keep", dial: &responsesWSDialSpec{headers: http.Header{"Authorization": {"Bearer key-b"}}}}
	drop := &responsesWSConn{responseID: "resp_drop", dial: &responsesWSDialSpec{headers: http.Header{"Authorization": {"Bearer key-a"}}}}
	busy := &responsesWSConn{busy: true, dial: &responsesWSDialSpec{headers: http.Header{"Authorization": {"Bearer key-a"}}}}
	p.entries[keep], p.entries[drop], p.entries[busy] = struct{}{}, struct{}{}, struct{}{}
	p.retireLanesForCredentials([]string{"key-a"})
	if _, ok := p.entries[keep]; !ok || keep.retired {
		t.Fatal("lane of another credential was retired")
	}
	if _, ok := p.entries[drop]; ok {
		t.Fatal("idle conversation lane of the blocked key survived")
	}
	if _, ok := p.entries[busy]; !ok || !busy.retired {
		t.Fatal("busy lane of the blocked key must stay owned by its request but be retired on release")
	}
}

// invalidateForConfig keeps conversation and busy lanes, drops fresh idle
// lanes and in-flight spares, and forgets unsupported/backoff memories.
func TestResponsesWSInvalidateForConfigKeepsConversations(t *testing.T) {
	p := newResponsesWSPool()
	defer p.close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conversation := &responsesWSConn{responseID: "resp_1"}
	busy := &responsesWSConn{busy: true}
	fresh := &responsesWSConn{}
	spare := &responsesWSConn{prewarmed: true}
	dialing := &responsesWSConn{busy: true, prewarming: true, cancelDial: cancel}
	for _, e := range []*responsesWSConn{conversation, busy, fresh, spare, dialing} {
		p.entries[e] = struct{}{}
	}
	p.unsupported = map[[32]byte]time.Time{{1}: time.Now()}
	p.prewarmBackoff = map[[32]byte]time.Time{{1}: time.Now()}
	gen := p.generation
	p.invalidateForConfig()
	if p.generation == gen || p.unsupported != nil || p.prewarmBackoff != nil {
		t.Fatalf("generation/memories not reset: gen=%d unsupported=%v backoff=%v", p.generation, p.unsupported, p.prewarmBackoff)
	}
	for _, e := range []*responsesWSConn{conversation, busy} {
		if _, ok := p.entries[e]; !ok || e.retired {
			t.Fatalf("lane %+v retired by configuration save", e)
		}
	}
	for _, e := range []*responsesWSConn{fresh, spare, dialing} {
		if _, ok := p.entries[e]; ok {
			t.Fatalf("lane %+v survived configuration save", e)
		}
	}
	if ctx.Err() == nil {
		t.Fatal("in-flight spare dial was not canceled")
	}
}

// Issue #151 A15: the breaker-reset log line is written after the service's
// configuration write lock is released.
func TestTargetBreakerResetLoggedOutsideConfigLock(t *testing.T) {
	s := newWSTestService(t, "http://127.0.0.1:1", true)
	h := &lockProbeHandler{try: func() bool {
		if s.mu.TryRLock() {
			s.mu.RUnlock()
			return true
		}
		return false
	}}
	s.logger = slog.New(h)
	if err := s.UpdateTargets([]config.Target{wsLifecycleTarget("http://127.0.0.1:2", "upstream-test-key")}); err != nil {
		t.Fatal(err)
	}
	if h.seen.Load() != 1 {
		t.Fatalf("breaker reset logged %d times, want 1", h.seen.Load())
	}
	if h.underLock.Load() != 0 {
		t.Fatal("breaker reset was logged while the configuration write lock was held")
	}
}

type lockProbeHandler struct {
	try             func() bool
	seen, underLock atomic.Int32
}

func (h *lockProbeHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *lockProbeHandler) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, "target breaker state reset") {
		h.seen.Add(1)
		if !h.try() {
			h.underLock.Add(1)
		}
	}
	return nil
}
func (h *lockProbeHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *lockProbeHandler) WithGroup(string) slog.Handler      { return h }
