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
	"github.com/coder/websocket"
)

func TestResponsesWSNonstreamWaitsForTerminalStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		terminal   string
		wantStatus int
		hold       bool
		wantUsage  bool
	}{
		{"late_error", `{"type":"error","status":500,"error":{"type":"server_error","message":"generation failed"}}`, 500, false, false},
		{"failed_with_usage", `{"type":"response.failed","response":{"id":"r1","status":"failed","error":{"type":"server_error"},"usage":{"input_tokens":8,"output_tokens":5}}}`, 500, false, true},
		{"timeout", "", 504, true, false},
		{"disconnect", "", 502, false, false},
		{"success", `{"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":8,"output_tokens":5}}}`, 200, false, true},
		{"incomplete_with_usage", `{"type":"response.incomplete","response":{"id":"r1","status":"incomplete","usage":{"input_tokens":8,"output_tokens":5}}}`, 200, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var creates, posts atomic.Int32
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					http.Error(w, "unexpected replay", 500)
					return
				}
				c, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer c.CloseNow()
				if _, _, err = c.Read(r.Context()); err != nil {
					return
				}
				creates.Add(1)
				_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"r1","status":"in_progress"}}`))
				if tc.hold {
					_, _, _ = c.Read(r.Context())
					return
				}
				if tc.terminal != "" {
					_ = c.Write(r.Context(), websocket.MessageText, []byte(tc.terminal))
				}
			}))
			defer u.Close()
			s := newWSTestService(t, u.URL, true)
			if tc.hold {
				s.requestTimeout.Store(int64(50 * time.Millisecond))
			}
			rec := &syncWSRecorder{}
			s.SetUsageRecorder(rec)
			w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi"}`)
			if w.Code != tc.wantStatus {
				t.Errorf("nonstream returned status=%d want=%d body=%q", w.Code, tc.wantStatus, w.Body.String())
			}
			if creates.Load() != 1 || posts.Load() != 0 {
				t.Errorf("generation replayed creates=%d posts=%d", creates.Load(), posts.Load())
			}
			events := rec.snapshot()
			if len(events) != 1 {
				t.Errorf("generation missing usage event: %+v", events)
			} else if tc.wantUsage {
				if events[0].InputTokens != 8 || events[0].OutputTokens != 5 || events[0].CostIncomplete {
					t.Errorf("terminal usage lost: %+v", events[0])
				}
			} else if !events[0].CostIncomplete {
				t.Errorf("missing usage not recorded incomplete: %+v", events[0])
			}
			if s.TargetStatuses(time.Now())[0].InFlight != 0 {
				t.Error("request capacity leaked")
			}
		})
	}
}

func TestResponsesWSReclaimsIdleIdentityAtCapacity(t *testing.T) {
	var upgrades, creates atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-test-key" {
			http.Error(w, "wrong credential", 401)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		upgrades.Add(1)
		for {
			if _, _, err = c.Read(r.Context()); err != nil {
				return
			}
			n := creates.Add(1)
			_ = c.Write(r.Context(), websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"r%d","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`, n)))
		}
	}))
	defer u.Close()
	s := newWSTestService(t, u.URL, true)
	// Exact upstream handshake counts below; background spare lanes are covered
	// by responses_ws_prewarm_test.go.
	s.responsesWS.prewarm = false
	principal := wsPrincipalFor(t, "client")
	latestTrace := ""
	for i := 1; i <= 8; i++ {
		latestTrace = fmt.Sprintf("00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-%016x-01", i)
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"openai-test","input":"hi"}`))
		r.Header.Set("Traceparent", latestTrace)
		r.Header.Set("X-Session-ID", fmt.Sprint(i))
		r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("serial request %d failed: status=%d body=%s", i, w.Code, w.Body.String())
		}
		s.responsesWS.mu.Lock()
		lanes := len(s.responsesWS.entries)
		s.responsesWS.mu.Unlock()
		if lanes > responsesWSPerClient {
			t.Fatalf("pool grew beyond owner capacity: %d", lanes)
		}
	}
	if upgrades.Load() != 8 {
		t.Fatalf("different header identities reused an incompatible handshake: upgrades=%d", upgrades.Load())
	}
	// The latest identity and its response cache remain available after eviction.
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"openai-test","input":"continue","previous_response_id":"r8"}`))
	r.Header.Set("Traceparent", latestTrace)
	r.Header.Set("X-Session-ID", "8")
	r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || upgrades.Load() != 8 || creates.Load() != 9 {
		t.Fatalf("latest continuation lost: status=%d upgrades=%d creates=%d body=%s", w.Code, upgrades.Load(), creates.Load(), w.Body.String())
	}
	// A different handshake identity must not borrow the latest cache.
	r = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"openai-test","input":"continue","previous_response_id":"r9"}`))
	r.Header.Set("Traceparent", latestTrace)
	r.Header.Set("X-Session-ID", "other")
	r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 400 || creates.Load() != 9 {
		t.Fatalf("continuation crossed header identity: status=%d creates=%d", w.Code, creates.Load())
	}
}

func TestResponsesWSLongActiveGenerationRemainsContinuable(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	defer func() {
		select {
		case <-finish:
		default:
			close(finish)
		}
	}()
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if _, _, err = c.Read(r.Context()); err != nil {
			return
		}
		close(started)
		select {
		case <-finish:
		case <-r.Context().Done():
			return
		}
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`))
		if _, _, err = c.Read(r.Context()); err != nil {
			return
		}
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"r2","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`))
	}))
	defer u.Close()
	s := newWSTestService(t, u.URL, true)
	s.requestTimeout.Store(int64(time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := wsPrincipalFor(t, "client")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- wsServeRequest(s, p, ctx, "/v1/responses", `{"model":"openai-test","input":"long generation"}`)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("generation not started")
	}
	s.responsesWS.mu.Lock()
	var lane *responsesWSConn
	for e := range s.responsesWS.entries {
		lane = e
		e.used = time.Now().Add(-responsesWSIdle - time.Minute)
	}
	s.responsesWS.mu.Unlock()
	// Evaluate the same reaper policy without waiting for its 15-second ticker.
	s.responsesWS.reapAt(time.Now())
	s.responsesWS.mu.Lock()
	retired := lane.retired
	s.responsesWS.mu.Unlock()
	close(finish)
	w := <-done
	follow := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"continue","previous_response_id":"r1"}`)
	if retired || w.Code != 200 || follow.Code != 200 || !strings.Contains(follow.Body.String(), `"id":"r2"`) {
		t.Fatalf("active generation lost cache: retired=%v completed=%d continuation=%d body=%s", retired, w.Code, follow.Code, follow.Body.String())
	}
}

func TestResponsesWSNonstreamTerminalFailuresNeverReplay(t *testing.T) {
	for _, tc := range []struct {
		name, event string
		status      int
		first       bool
	}{
		{"first_error", `{"type":"error","status":500,"error":{"type":"server_error"}}`, 500, true},
		{"first_quota", `{"type":"error","status":429,"error":{"type":"insufficient_quota","code":"organization_spend_limit_exceeded"}}`, 429, true},
		{"late_quota", `{"type":"response.failed","status":429,"response":{"id":"r1","status":"failed","error":{"type":"insufficient_quota","code":"organization_spend_limit_exceeded"}}}`, 429, false},
		{"late_rate", `{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded"}}`, 429, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var creates, posts atomic.Int32
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					return
				}
				c, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer c.CloseNow()
				if _, _, err := c.Read(r.Context()); err != nil {
					return
				}
				creates.Add(1)
				if !tc.first {
					_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"r1","status":"in_progress"}}`))
				}
				_ = c.Write(r.Context(), websocket.MessageText, []byte(tc.event))
			}))
			defer u.Close()
			s := newWSTestService(t, u.URL, true)
			rec := &syncWSRecorder{}
			s.SetUsageRecorder(rec)
			w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi"}`)
			if w.Code != tc.status || creates.Load() != 1 || posts.Load() != 0 || s.metrics.totalKeyRetries.Load() != 0 || s.metrics.totalTargetRetries.Load() != 0 {
				t.Fatalf("terminal replayed: status=%d creates=%d posts=%d key_retries=%d target_retries=%d body=%s", w.Code, creates.Load(), posts.Load(), s.metrics.totalKeyRetries.Load(), s.metrics.totalTargetRetries.Load(), w.Body.String())
			}
			if events := rec.snapshot(); len(events) != 1 || !events[0].CostIncomplete {
				t.Fatalf("sent generation missing incomplete usage: %+v", events)
			}
		})
	}
}

func TestResponsesWSReclaimPreservesBusyCapacity(t *testing.T) {
	for _, mode := range []string{"owner_busy", "global_busy", "global_idle"} {
		t.Run(mode, func(t *testing.T) {
			var upgrades atomic.Int32
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upgrades.Add(1)
				c, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer c.CloseNow()
				_, _, _ = c.Read(r.Context())
			}))
			defer u.Close()
			p := newResponsesWSPool()
			defer p.close()
			owner := [32]byte{255}
			now := time.Now()
			var idle *responsesWSConn
			for i := 0; i < responsesWSMax; i++ {
				e := &responsesWSConn{owner: [32]byte{byte(i)}, created: now, used: now, busy: true}
				if mode == "owner_busy" && i < responsesWSPerClient {
					e.owner = owner
				}
				if mode != "global_busy" && i == responsesWSMax-1 {
					e.busy = false
					e.used = now.Add(-time.Minute)
					idle = e
				}
				p.entries[e] = struct{}{}
			}
			req, _ := http.NewRequest(http.MethodPost, u.URL+"/v1/responses", nil)
			e, resp, fallback, err := p.acquire(req.Context(), http.DefaultClient, req, owner, "")
			if mode == "global_idle" {
				if err != nil || resp != nil || fallback || e == nil || upgrades.Load() != 1 {
					t.Fatalf("idle global capacity not reclaimed: conn=%v response=%v fallback=%v err=%v upgrades=%d", e, resp, fallback, err, upgrades.Load())
				}
				p.mu.Lock()
				_, retained := p.entries[idle]
				lanes := len(p.entries)
				p.mu.Unlock()
				if retained || lanes != responsesWSMax {
					t.Fatalf("incorrect eviction: old_idle_retained=%v lanes=%d", retained, lanes)
				}
				p.release(e, false)
			} else {
				// Busy capacity is never stolen; the request falls back to HTTP before sending.
				if err != nil || !fallback || e != nil || resp != nil || upgrades.Load() != 0 {
					t.Fatalf("busy capacity guard bypassed: conn=%v response=%v fallback=%v err=%v upgrades=%d", e, resp, fallback, err, upgrades.Load())
				}
				p.mu.Lock()
				lanes := len(p.entries)
				p.mu.Unlock()
				if lanes != responsesWSMax {
					t.Fatalf("blocked request evicted another owner's idle cache: lanes=%d", lanes)
				}
			}
		})
	}
}

func TestResponsesWSReaperKeepsAgeLimitForBusyLanes(t *testing.T) {
	p := newResponsesWSPool()
	defer p.close()
	now := time.Now()
	active := &responsesWSConn{created: now.Add(-6 * time.Minute), used: now.Add(-6 * time.Minute), busy: true}
	aged := &responsesWSConn{created: now.Add(-responsesWSAge), used: now, busy: true}
	idle := &responsesWSConn{created: now.Add(-6 * time.Minute), used: now.Add(-6 * time.Minute)}
	for _, e := range []*responsesWSConn{active, aged, idle} {
		p.entries[e] = struct{}{}
	}
	p.reapAt(now)
	if active.retired || !aged.retired {
		t.Fatalf("busy retirement policy changed: active=%v aged=%v", active.retired, aged.retired)
	}
	if _, retained := p.entries[idle]; retained {
		t.Fatal("idle expired lane was retained")
	}
	if _, retained := p.entries[aged]; !retained {
		t.Fatal("busy aged lane was removed before release")
	}
	p.release(aged, true)
	if _, retained := p.entries[aged]; retained {
		t.Fatal("aged lane was reused after completing generation")
	}
}
