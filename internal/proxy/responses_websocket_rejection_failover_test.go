package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/gorilla/websocket"
)

// wsRejectionPair is a rejecting upstream plus a live WebSocket upstream. The
// native turn is driven through Service.ServeHTTP with a session, exactly like
// the downstream adapter does, so each test controls the candidate order.
type wsRejectionPair struct {
	service *Service
	dead    *httptest.Server
	live    *httptest.Server
	rejects atomic.Int32 // handshakes the rejecting upstream answered
	creates atomic.Int32 // response.create frames the live upstream received
	lives   atomic.Int32 // handshakes the live upstream saw
}

func newWSRejectionPair(t *testing.T, status int, body string, deadKeys ...string) *wsRejectionPair {
	t.Helper()
	p := &wsRejectionPair{}
	p.dead = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.rejects.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(p.dead.Close)
	p.live = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.lives.Add(1)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		p.creates.Add(1)
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "live", "status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}})
	}))
	t.Cleanup(p.live.Close)
	p.service, _, _ = websocketTestProxy(t, p.dead.URL)
	if len(deadKeys) == 0 {
		deadKeys = []string{"dead-key"}
	}
	dead := config.Target{Name: "test-target", EndpointType: "openai", Endpoint: p.dead.URL, APIKey: deadKeys[0], APIKeys: deadKeys[1:], MaxConcurrency: 1, ModelMappings: []config.ModelMapping{{Upstream: "DeadCase", Fallback: "codex-fast"}}}
	live := config.Target{Name: "live", EndpointType: "openai", Endpoint: p.live.URL, APIKey: "live-key", MaxConcurrency: 1, ModelMappings: []config.ModelMapping{{Upstream: "LiveCase", Fallback: "codex-fast"}}}
	if err := p.service.ApplyConfig(&config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{dead, live}}); err != nil {
		t.Fatal(err)
	}
	return p
}

// turn runs one native WS turn (no explicit target unless header is set) with
// the rejecting target first in line.
func (p *wsRejectionPair) turn(t *testing.T, explicitTarget string) *httptest.ResponseRecorder {
	t.Helper()
	session := &responsesWebsocketSession{}
	defer func() {
		if session.conn != nil {
			_ = session.conn.Close()
		}
	}()
	req := httptest.NewRequest(http.MethodPost, responsesPath, strings.NewReader(`{"model":"codex-fast","input":"hi"}`))
	if explicitTarget != "" {
		req.Header.Set(headerProxyTarget, explicitTarget)
	}
	p.service.affinity.Set(affinityKey(clientIP(req), "tester", "codex-fast"), "test-target", time.Now())
	ctx := auth.WithPrincipal(context.WithValue(req.Context(), responsesWebsocketContextKey{}, session), newAllowAllPrincipal(t, "local-client"))
	rec := httptest.NewRecorder()
	p.service.ServeHTTP(rec, req.WithContext(ctx))
	waitWebsocketSlotsReleased(t, p.service)
	return rec
}

func (p *wsRejectionPair) deadStatus(t *testing.T) TargetStatus {
	t.Helper()
	return targetStatusByName(t, p.service, "test-target")
}

func TestNativeWSAccountRejectionFailsOverAndMutesTarget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		keys   []string
		hits   int32 // handshakes the rejecting upstream answers before the switch
	}{
		{"single403", 403, `{"error":{"type":"permission_error","code":"policy_denied"}}`, nil, 1},
		{"multiKey403", 403, `{"error":{"type":"permission_error","code":"policy_denied"}}`, []string{"dead-a", "dead-b"}, 2},
		{"emptyBody403", 403, ``, nil, 1},
		{"single402", 402, `{"error":{"type":"billing_error"}}`, nil, 1},
		{"single401", 401, `{"error":{"code":"invalid_api_key"}}`, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newWSRejectionPair(t, tc.status, tc.body, tc.keys...)
			rec := p.turn(t, "")
			if rec.Code != http.StatusOK || p.creates.Load() != 1 || p.rejects.Load() != tc.hits {
				t.Fatalf("no same-turn switch: status=%d creates=%d rejects=%d body=%s", rec.Code, p.creates.Load(), p.rejects.Load(), rec.Body.String())
			}
			if tc.body == "" {
				// An empty 403 is an unconfirmed rejection: short cooldown first.
				st := p.deadStatus(t)
				if remaining := time.Until(st.MutedUntil); !st.Muted || st.MuteStatus != tc.status || remaining > defaultUnverifiedRejectionCooldown || remaining < defaultUnverifiedRejectionCooldown-time.Minute {
					t.Fatalf("unconfirmed 403 cooldown: %+v remaining=%v", st, remaining)
				}
			} else {
				assertRejectionMuted(t, p.service, "test-target", tc.status)
			}
			assertNotMuted(t, p.service, "live")
			state, _ := p.service.targetByName("test-target")
			if state.probeSlotBusy(time.Now()) {
				t.Error("rejected handshake left the probe lease occupied")
			}
			if len(tc.keys) > 1 {
				state.keyPool.mu.Lock()
				if tc.status == 403 {
					for i, entry := range state.keyPool.entries {
						if !entry.exhausted || entry.exhaustReason != keyReasonForbidden || time.Until(entry.cooldownEnd) < 59*time.Minute {
							t.Errorf("key %d not cooled for an hour: %+v", i, entry)
						}
					}
				}
				state.keyPool.mu.Unlock()
			}
			// Settled once: one fixed-cooldown failure, no success for the
			// rejecting target, and one client-level success for the live turn.
			if got := p.service.metrics.totalDequeued.Load(); got != 1 {
				t.Errorf("totalDequeued=%d, want 1", got)
			}
			if got := p.service.MetricsSnapshot().UpstreamRejectionMuted[fmt.Sprint(tc.status)]; got != 1 {
				t.Errorf("rejection mute counted %d times, want 1", got)
			}
			if p.service.metrics.totalSuccess.Load() != 1 || p.service.metrics.totalFailures.Load() != 0 {
				t.Errorf("client outcome counted wrongly: success=%d failure=%d", p.service.metrics.totalSuccess.Load(), p.service.metrics.totalFailures.Load())
			}
			// The cooldown pays off at once: the next turn skips the target.
			before := p.rejects.Load()
			if rec := p.turn(t, ""); rec.Code != http.StatusOK || p.rejects.Load() != before || p.creates.Load() != 2 {
				t.Fatalf("muted target was probed again: status=%d rejects=%d->%d creates=%d", rec.Code, before, p.rejects.Load(), p.creates.Load())
			}
		})
	}
}

func TestNativeWSAccountRejectionWithoutCandidateReplaysRealError(t *testing.T) {
	var rejects atomic.Int32
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rejects.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"type":"permission_error","code":"policy_denied"}}`)
	}))
	defer dead.Close()
	service, server, _ := websocketTestProxy(t, dead.URL)
	state, _ := service.targetByName("test-target")
	state.keyPool = nil
	conn := dialWebsocketTest(t, server)
	_ = conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	event := readWebsocketEvent(t, conn)
	detail, _ := event["error"].(map[string]any)
	if event["type"] != "error" || event["status"] != float64(403) || detail["code"] != "policy_denied" {
		t.Fatalf("real 403 was not replayed as the native error event: %v", event)
	}
	waitWebsocketSlotsReleased(t, service)
	assertRejectionMuted(t, service, "test-target", 403)
	if rejects.Load() != 1 || service.metrics.totalFailures.Load() != 1 || service.metrics.totalSuccess.Load() != 0 {
		t.Fatalf("rejects=%d failures=%d successes=%d, want one rejection and one failed turn", rejects.Load(), service.metrics.totalFailures.Load(), service.metrics.totalSuccess.Load())
	}
}

func TestNativeWSModeration403PassesThroughWithoutMute(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":"content_policy_violation","message":"blocked"}}`,
		`{"error":{"type":"content_filter"}}`,
	} {
		p := newWSRejectionPair(t, 403, body, "dead-a", "dead-b")
		rec := p.turn(t, "")
		if rec.Code != http.StatusForbidden || p.lives.Load() != 0 || p.creates.Load() != 0 {
			t.Fatalf("moderation 403 crossed targets: status=%d live=%d body=%s", rec.Code, p.lives.Load(), rec.Body.String())
		}
		if p.rejects.Load() != 1 {
			t.Fatalf("moderation 403 switched keys: rejects=%d", p.rejects.Load())
		}
		assertNotMuted(t, p.service, "test-target")
		state, _ := p.service.targetByName("test-target")
		state.keyPool.mu.Lock()
		for i, entry := range state.keyPool.entries {
			if entry.exhausted {
				t.Errorf("key %d cooled by a moderation 403: %+v", i, entry)
			}
		}
		state.keyPool.mu.Unlock()
	}
}

func TestNativeWSExplicitTargetRejectionDoesNotCrossTargets(t *testing.T) {
	p := newWSRejectionPair(t, 403, `{"error":{"type":"permission_error"}}`)
	rec := p.turn(t, "test-target")
	if rec.Code != http.StatusForbidden || p.lives.Load() != 0 {
		t.Fatalf("explicit target failed over: status=%d live=%d", rec.Code, p.lives.Load())
	}
	assertNotMuted(t, p.service, "test-target")
}

func TestNativeWSUnrecognisedAccountRejectionFailsOverWithShortCooldown(t *testing.T) {
	// A 401 without a known credential rule still rejects the proxy
	// credential: the turn moves on, and the target cools only briefly first.
	p := newWSRejectionPair(t, 401, `{"error":{"message":"nope"}}`)
	rec := p.turn(t, "")
	if rec.Code != http.StatusOK || p.lives.Load() != 1 {
		t.Fatalf("unknown 401 did not fail over: status=%d live=%d", rec.Code, p.lives.Load())
	}
	st := p.deadStatus(t)
	if remaining := time.Until(st.MutedUntil); !st.Muted || st.MuteStatus != 401 || remaining > defaultUnverifiedRejectionCooldown {
		t.Fatalf("unconfirmed 401 cooldown: %+v remaining=%v", st, remaining)
	}
}

func TestNativeWSHandshake403CoolsKeyAndUsesNextKey(t *testing.T) {
	var rejected, creates atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-test-key" {
			rejected.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":{"type":"permission_error","message":"denied"}}`)
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err = conn.ReadMessage(); err != nil {
			return
		}
		creates.Add(1)
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "ok", "status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}})
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()
	service, server, _ := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	_ = conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
		t.Fatalf("403 did not rotate to the healthy key: %v", event)
	}
	waitWebsocketSlotsReleased(t, service)
	state, _ := service.targetByName("test-target")
	state.keyPool.mu.Lock()
	bad, good := state.keyPool.entries[0], state.keyPool.entries[1]
	state.keyPool.mu.Unlock()
	if rejected.Load() != 1 || creates.Load() != 1 || !bad.exhausted || bad.exhaustReason != keyReasonForbidden || good.exhausted {
		t.Fatalf("rejected=%d creates=%d bad=%+v good=%+v", rejected.Load(), creates.Load(), bad, good)
	}
	if remaining := time.Until(bad.cooldownEnd); remaining < 59*time.Minute || remaining > time.Hour {
		t.Fatalf("key cooldown %v, want ~1h", remaining)
	}
	if state.IsMuted(time.Now()) || service.metrics.totalDequeued.Load() != 0 {
		t.Fatalf("key-level 403 muted or dequeued the target")
	}
}

// A 403 that arrives after the create was sent is a terminal result: never
// replayed on another key or target. It may only steer the next turn's key.
func TestNativeWSSentGeneration403IsNeverReplayed(t *testing.T) {
	var creates, liveHits atomic.Int32
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err = conn.ReadMessage(); err != nil {
			return
		}
		creates.Add(1)
		_ = conn.WriteJSON(map[string]any{"type": "error", "status": 403, "error": map[string]any{"type": "permission_error", "code": "policy_denied"}})
		_, _, _ = conn.ReadMessage()
	}))
	defer dead.Close()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { liveHits.Add(1) }))
	defer live.Close()
	service, server, recorder := websocketTestProxy(t, dead.URL)
	if err := service.ApplyConfig(&config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{
		{Name: "test-target", EndpointType: "openai", Endpoint: dead.URL, APIKey: "dead-a", APIKeys: []string{"dead-b"}, MaxConcurrency: 1, ModelMappings: []config.ModelMapping{{Upstream: "DeadCase", Fallback: "codex-fast"}}},
		{Name: "live", EndpointType: "openai", Endpoint: live.URL, APIKey: "live-key", MaxConcurrency: 1, ModelMappings: []config.ModelMapping{{Upstream: "LiveCase", Fallback: "codex-fast"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	service.affinity.Set(affinityKey("127.0.0.1", "tester", "codex-fast"), "test-target", time.Now())
	conn := dialWebsocketTest(t, server)
	_ = conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	if event := readWebsocketEvent(t, conn); event["type"] != "error" || event["status"] != float64(403) {
		t.Fatalf("terminal 403 event=%v", event)
	}
	waitWebsocketSlotsReleased(t, service)
	if creates.Load() != 1 || liveHits.Load() != 0 {
		t.Fatalf("sent generation was replayed: creates=%d live=%d", creates.Load(), liveHits.Load())
	}
	if got := service.metrics.totalDequeued.Load(); got != 0 {
		t.Fatalf("sent generation was dequeued %d times", got)
	}
	if st := targetStatusByName(t, service, "test-target"); st.MuteStatus == http.StatusForbidden {
		t.Fatalf("terminal 403 after a sent generation fixed-cooled the target: %+v", st)
	}
	if events := recorder.snapshot(); len(events) != 1 {
		t.Fatalf("generation not accounted exactly once: %+v", events)
	}
}
