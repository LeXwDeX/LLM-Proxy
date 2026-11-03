package proxy

import (
	"context"
	"fmt"
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

// invalidateForConfig keeps conversation and busy lanes of live targets and
// keys, drops fresh idle lanes and in-flight spares, and forgets
// unsupported/backoff memories.
func TestResponsesWSInvalidateForConfigKeepsConversations(t *testing.T) {
	p := newResponsesWSPool()
	defer p.close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	liveDial := &responsesWSDialSpec{target: "openai", headers: http.Header{"Authorization": {"Bearer key-a"}}}
	conversation := &responsesWSConn{responseID: "resp_1", dial: liveDial}
	busy := &responsesWSConn{busy: true, dial: liveDial}
	fresh := &responsesWSConn{dial: liveDial}
	spare := &responsesWSConn{prewarmed: true, dial: liveDial}
	dialing := &responsesWSConn{busy: true, prewarming: true, cancelDial: cancel, dial: liveDial}
	for _, e := range []*responsesWSConn{conversation, busy, fresh, spare, dialing} {
		p.entries[e] = struct{}{}
	}
	p.unsupported = map[[32]byte]time.Time{{1}: time.Now()}
	p.prewarmBackoff = map[[32]byte]time.Time{{1}: time.Now()}
	gen := p.generation
	p.invalidateForConfig(map[string][]string{"openai": {"key-a", "key-b"}})
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

// A lane busy during a configuration save may hold a credential the save
// removed. On release it must not reserve a spare and dial a new upstream
// handshake with that credential, while its conversation stays continuable
// (A5). A busy pre-save lane without a conversation is retired on release.
func TestResponsesWSBusyLaneAcrossSaveDoesNotPrewarmOldCredentials(t *testing.T) {
	u := &wsPrewarmUpstream{}
	server := httptest.NewServer(http.HandlerFunc(u.serve))
	defer server.Close()
	s := newWSTestService(t, server.URL, true)
	if w := wsTestRequest(t, s, "client", "/v1/responses", wsPrewarmBody); w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	waitForWS(t, "the first spare", func() bool { return s.MetricsSnapshot().ResponsesWS.PrewarmDialOK == 1 })
	var conversation *responsesWSConn
	s.responsesWS.mu.Lock()
	for e := range s.responsesWS.entries {
		if e.responseID == "resp_1" {
			conversation = e
		}
	}
	if conversation == nil {
		s.responsesWS.mu.Unlock()
		t.Fatal("no lane holds resp_1")
	}
	conversation.busy = true // a request owns the lane while the save lands
	s.responsesWS.mu.Unlock()
	if err := s.UpdateTargets([]config.Target{wsLifecycleTarget(server.URL, "upstream-test-key")}); err != nil {
		t.Fatal(err)
	}
	attempts := u.attempts.Load()
	s.responsesWS.release(conversation, true)
	time.Sleep(100 * time.Millisecond)
	if got := u.attempts.Load(); got != attempts {
		t.Fatalf("release of a pre-save lane dialed %d new handshake(s) with its old credentials", got-attempts)
	}
	s.responsesWS.mu.Lock()
	for e := range s.responsesWS.entries {
		if e.prewarming {
			s.responsesWS.mu.Unlock()
			t.Fatal("a pre-save lane reserved a spare after the save")
		}
	}
	s.responsesWS.mu.Unlock()
	w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"again","previous_response_id":"resp_1"}`)
	if w.Code != 200 {
		t.Fatalf("continuation of a pre-save lane: %d %s", w.Code, w.Body.String())
	}
	u.mu.Lock()
	continuedOn := u.continuedOn["resp_1"]
	u.mu.Unlock()
	if continuedOn != 1 {
		t.Fatalf("continuation ran on handshake %d, want the original socket 1", continuedOn)
	}
}

// A busy pre-save lane without a conversation is retired when released, like
// the idle fresh lanes the save already retired.
func TestResponsesWSReleaseRetiresPreSaveFreshLane(t *testing.T) {
	p := newResponsesWSPool()
	defer p.close()
	dial := &responsesWSDialSpec{client: http.DefaultClient, url: "ws://127.0.0.1:1", target: "openai", headers: http.Header{"Authorization": {"Bearer key-a"}}}
	fresh := &responsesWSConn{busy: true, gen: p.generation, dial: dial}
	conversation := &responsesWSConn{busy: true, gen: p.generation, owner: [32]byte{2}, dial: dial}
	p.entries[fresh], p.entries[conversation] = struct{}{}, struct{}{}
	p.invalidateForConfig(map[string][]string{"openai": {"key-a"}})
	conversation.responseID = "resp_1"
	p.release(fresh, true)
	p.release(conversation, true)
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.entries[fresh]; ok {
		t.Fatal("pre-save fresh lane survived release")
	}
	if _, ok := p.entries[conversation]; !ok || conversation.retired {
		t.Fatal("pre-save conversation lane must stay continuable")
	}
	if len(p.entries) != 1 {
		t.Fatalf("entries=%d, want no spare reserved for a pre-save lane", len(p.entries))
	}
}

// wsLanesOf snapshots the pool's lanes under its lock.
func wsLanesOf(s *Service) []*responsesWSConn {
	s.responsesWS.mu.Lock()
	defer s.responsesWS.mu.Unlock()
	lanes := make([]*responsesWSConn, 0, len(s.responsesWS.entries))
	for e := range s.responsesWS.entries {
		lanes = append(lanes, e)
	}
	return lanes
}

func wsOtherTarget() config.Target {
	return config.Target{Name: "other", EndpointType: "openai", Endpoint: "http://127.0.0.1:1", APIKey: "other-key",
		ModelMappings: []config.ModelMapping{{Upstream: "other-model", Fallback: "other-alias"}}}
}

// Deleting a target closes its idle conversation lane at once: no remaining
// configuration can select it, so it must not stay connected until the reaper.
func TestResponsesWSDeletedTargetClosesIdleConversationLane(t *testing.T) {
	u := &wsPrewarmUpstream{}
	server := httptest.NewServer(http.HandlerFunc(u.serve))
	defer server.Close()
	s := newWSTestService(t, server.URL, true)
	s.responsesWS.prewarm = false
	if w := wsTestRequest(t, s, "client", "/v1/responses", wsPrewarmBody); w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	if lanes := wsLanesOf(s); len(lanes) != 1 || lanes[0].responseID == "" {
		t.Fatalf("want one conversation lane before the save, got %d", len(lanes))
	}
	if err := s.UpdateTargets([]config.Target{wsOtherTarget()}); err != nil {
		t.Fatal(err)
	}
	if lanes := wsLanesOf(s); len(lanes) != 0 {
		t.Fatalf("lanes=%d, want the deleted target's conversation lane retired", len(lanes))
	}
	waitForWS(t, "the deleted target's upstream socket to close", func() bool { return u.active.Load() == 0 })
}

// A lane busy when its target is deleted stays owned by its request and
// closes when released.
func TestResponsesWSDeletedTargetClosesBusyLaneOnRelease(t *testing.T) {
	u := &wsPrewarmUpstream{}
	server := httptest.NewServer(http.HandlerFunc(u.serve))
	defer server.Close()
	s := newWSTestService(t, server.URL, true)
	s.responsesWS.prewarm = false
	if w := wsTestRequest(t, s, "client", "/v1/responses", wsPrewarmBody); w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	lanes := wsLanesOf(s)
	if len(lanes) != 1 {
		t.Fatalf("lanes=%d before the save", len(lanes))
	}
	lane := lanes[0]
	s.responsesWS.mu.Lock()
	lane.busy = true // a request owns the lane while the save lands
	s.responsesWS.mu.Unlock()
	if err := s.UpdateTargets([]config.Target{wsOtherTarget()}); err != nil {
		t.Fatal(err)
	}
	s.responsesWS.mu.Lock()
	_, present := s.responsesWS.entries[lane]
	retired := lane.retired
	s.responsesWS.mu.Unlock()
	if !present || !retired {
		t.Fatalf("busy lane of a deleted target: present=%v retired=%v, want owned by its request and retired", present, retired)
	}
	if u.active.Load() != 1 {
		t.Fatal("busy lane was closed under its request")
	}
	s.responsesWS.release(lane, true)
	if lanes := wsLanesOf(s); len(lanes) != 0 {
		t.Fatalf("lanes=%d after release, want the deleted target's lane gone", len(lanes))
	}
	waitForWS(t, "the released lane's upstream socket to close", func() bool { return u.active.Load() == 0 })
}

// Removing one key from a surviving target retires that key's conversation
// lanes; lanes on its remaining keys stay and continue on their socket.
func TestResponsesWSRemovedKeyRetiresOnlyItsConversationLanes(t *testing.T) {
	u := &wsPrewarmUpstream{}
	server := httptest.NewServer(http.HandlerFunc(u.serve))
	defer server.Close()
	s := wsTwoKeyService(t, server.URL)
	s.responsesWS.prewarm = false
	// Separate clients own separate lanes; continue until both keys carry one.
	seen := map[string]bool{}
	for i := 0; i < 8 && len(seen) < 2; i++ {
		if w := wsTestRequest(t, s, fmt.Sprintf("client-%d", i), "/v1/responses", wsPrewarmBody); w.Code != 200 {
			t.Fatalf("request %d: %d %s", i, w.Code, w.Body.String())
		}
		u.mu.Lock()
		seen[u.authOf[int32(i+1)]] = true
		u.mu.Unlock()
	}
	if len(seen) != 2 {
		t.Fatalf("lanes did not cover both keys: %v", seen)
	}
	var keptID string
	for _, e := range wsLanesOf(s) {
		if e.dial != nil && headersCarryCredential(e.dial.headers, []string{"prewarm-key-a"}) && e.responseID != "" {
			keptID = e.responseID
		}
	}
	if keptID == "" {
		t.Fatal("no conversation lane on the kept key")
	}
	activeBefore := u.active.Load()
	removed := int32(0)
	for _, e := range wsLanesOf(s) {
		if e.dial != nil && headersCarryCredential(e.dial.headers, []string{"prewarm-key-b"}) {
			removed++
		}
	}
	target := config.Target{Name: "openai", EndpointType: "openai", Endpoint: server.URL, APIKey: "prewarm-key-a",
		PreferWebSocket: true, ModelMappings: []config.ModelMapping{{Upstream: "UpstreamCase", Fallback: "openai-test"}}}
	if err := s.UpdateTargets([]config.Target{target}); err != nil {
		t.Fatal(err)
	}
	for _, e := range wsLanesOf(s) {
		if e.dial == nil || headersCarryCredential(e.dial.headers, []string{"prewarm-key-b"}) {
			t.Fatal("a lane of the removed key survived the save")
		}
	}
	waitForWS(t, "the removed key's sockets to close", func() bool { return u.active.Load() == activeBefore-removed })
	u.mu.Lock()
	producedOn := u.producedOn[keptID]
	owner := -1
	for attempt, auth := range u.authOf {
		if attempt == producedOn && strings.HasSuffix(auth, "prewarm-key-a") {
			owner = int(attempt)
		}
	}
	u.mu.Unlock()
	if owner < 0 {
		t.Fatalf("response %s was not produced on a kept-key handshake", keptID)
	}
	var client string
	for i := 0; i < 8; i++ {
		if int32(i+1) == producedOn {
			client = fmt.Sprintf("client-%d", i)
		}
	}
	attempts := u.attempts.Load()
	w := wsTestRequest(t, s, client, "/v1/responses", `{"model":"openai-test","input":"again","previous_response_id":"`+keptID+`"}`)
	if w.Code != 200 {
		t.Fatalf("continuation on the kept key: %d %s", w.Code, w.Body.String())
	}
	u.mu.Lock()
	continuedOn := u.continuedOn[keptID]
	u.mu.Unlock()
	if continuedOn != producedOn || u.attempts.Load() != attempts {
		t.Fatalf("continuation ran on handshake %d (attempts %d→%d), want original socket %d", continuedOn, attempts, u.attempts.Load(), producedOn)
	}
}

// invalidateForConfig retires lanes of a deleted target or a removed key,
// conversation or not: idle ones at once, busy ones on release.
func TestResponsesWSInvalidateForConfigRetiresDeadIdentities(t *testing.T) {
	p := newResponsesWSPool()
	defer p.close()
	dial := func(target, key string) *responsesWSDialSpec {
		return &responsesWSDialSpec{target: target, headers: http.Header{"Authorization": {"Bearer " + key}}}
	}
	kept := &responsesWSConn{responseID: "resp_kept", dial: dial("openai", "key-a")}
	keptBusy := &responsesWSConn{busy: true, dial: dial("openai", "key-a")}
	removedKey := &responsesWSConn{responseID: "resp_b", dial: dial("openai", "key-b")}
	deleted := &responsesWSConn{responseID: "resp_gone", dial: dial("gone", "key-a")}
	deletedBusy := &responsesWSConn{busy: true, responseID: "resp_gone_busy", dial: dial("gone", "key-a")}
	removedKeyBusy := &responsesWSConn{busy: true, dial: dial("openai", "key-b")}
	for _, e := range []*responsesWSConn{kept, keptBusy, removedKey, deleted, deletedBusy, removedKeyBusy} {
		p.entries[e] = struct{}{}
	}
	p.invalidateForConfig(map[string][]string{"openai": {"key-a"}})
	for _, e := range []*responsesWSConn{kept, keptBusy} {
		if _, ok := p.entries[e]; !ok || e.retired {
			t.Fatalf("live lane %+v retired", e)
		}
	}
	for _, e := range []*responsesWSConn{removedKey, deleted} {
		if _, ok := p.entries[e]; ok {
			t.Fatalf("idle lane %+v of a dead identity survived", e)
		}
	}
	for _, e := range []*responsesWSConn{deletedBusy, removedKeyBusy} {
		if _, ok := p.entries[e]; !ok || !e.retired {
			t.Fatalf("busy lane %+v must stay owned by its request and be retired", e)
		}
		p.release(e, true)
		if _, ok := p.entries[e]; ok {
			t.Fatalf("busy lane %+v of a dead identity survived release", e)
		}
	}
}
