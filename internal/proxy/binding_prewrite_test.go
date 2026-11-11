package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Issue #202: the edge proxy opens a connection per request and a client may
// send the next request the instant it has read the previous response. The
// binding must already exist by then, so it is written before the 2xx headers
// are committed (and rolled back when the response is not delivered in full)
// instead of after writeResponse returns. slowUsageRecorder stretches the
// post-copy work of writeResponse so the old write-after window is wide.

func TestBindingVisibleToZeroGapShortConnections(t *testing.T) {
	for _, tc := range []struct {
		name, session string
		stream        bool
		readUntil     string
	}{
		{"json/session", "zero-gap-1", false, ""},
		{"json/ip", "", false, ""},
		{"sse/session", "zero-gap-2", true, "[DONE]"},
		{"sse/ip", "", true, "[DONE]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBindingFixture(t)
			first := f.call(t, tc.session, tc.stream, "one", tc.readUntil)
			for i := 1; i < 6; i++ {
				// No idle(): the next request leaves while the previous
				// handler is still busy after its last byte.
				if got := f.call(t, tc.session, tc.stream, "again", tc.readUntil); got != first {
					t.Fatalf("request %d went to %q, want the bound %q", i, got, first)
				}
			}
			f.idle(t)
		})
	}
}

// headerProbe is a ResponseRecorder that runs a hook the moment the response
// headers are committed, so a test can look at the bindings at that instant.
type headerProbe struct {
	*httptest.ResponseRecorder
	onHeader func()
	fired    bool
}

func (h *headerProbe) WriteHeader(code int) {
	if !h.fired {
		h.fired = true
		if h.onHeader != nil {
			h.onHeader()
		}
	}
	h.ResponseRecorder.WriteHeader(code)
}

func (h *headerProbe) Write(p []byte) (int, error) {
	if !h.fired {
		h.WriteHeader(http.StatusOK)
	}
	return h.ResponseRecorder.Write(p)
}

const (
	prewriteTestModel   = "deepseek-chat"
	prewriteTestSession = "sess-prewrite"
)

// prewriteUpstream answers every request with the given status. kind "json"
// is a complete chat completion, "sse" a complete stream, "truncated" a 200
// stream that ends without a terminal event, "error" the body as is.
func prewriteUpstream(t *testing.T, kind string, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch kind {
		case "sse", "truncated":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
			if kind == "sse" {
				fmt.Fprint(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\ndata: [DONE]\n\n")
			}
		case "json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func prewriteKeys(p *auth.Principal) (ip, sess string) {
	return affinityKey("192.0.2.1", p.Name, prewriteTestModel), sessionAffinityKey(p.Name, prewriteTestSession, prewriteTestModel)
}

// servePrewrite sends one request (RemoteAddr 192.0.2.1, with a session) and
// calls atHeaders at the moment its headers are committed.
func servePrewrite(s *Service, p *auth.Principal, atHeaders func()) *headerProbe {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"`+prewriteTestModel+`","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Session-Id", prewriteTestSession)
	req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	w := &headerProbe{ResponseRecorder: httptest.NewRecorder(), onHeader: atHeaders}
	s.ServeHTTP(w, req)
	return w
}

// The bindings exist before the first byte of a 2xx response is committed.
func TestBindingsWrittenBeforeHeadersCommit(t *testing.T) {
	for _, kind := range []string{"json", "sse"} {
		t.Run(kind, func(t *testing.T) {
			up := prewriteUpstream(t, kind, 200, "")
			s, p := newRejectionService(t, rejectionTarget("only", up.URL, "k"))
			ipKey, sessKey := prewriteKeys(p)
			var ipAt, sessAt string
			w := servePrewrite(s, p, func() {
				ipAt, _ = s.affinity.Get(ipKey, time.Now())
				sessAt, _ = s.affinity.Get(sessKey, time.Now())
			})
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d", w.Code)
			}
			if ipAt != "only" || sessAt != "only" {
				t.Fatalf("at header commit: ip binding %q, session binding %q, want both %q", ipAt, sessAt, "only")
			}
			// Delivered in full: they stay.
			if got, _ := s.affinity.Get(ipKey, time.Now()); got != "only" {
				t.Fatalf("ip binding after delivery = %q", got)
			}
			if got, _ := s.affinity.Get(sessKey, time.Now()); got != "only" {
				t.Fatalf("session binding after delivery = %q", got)
			}
		})
	}
}

// A 2xx answer that is not delivered in full takes its prewritten bindings
// back: none stays, and a previous binding is restored as it was.
func TestPrewrittenBindingsRolledBackOnIncompleteResponse(t *testing.T) {
	up := prewriteUpstream(t, "truncated", 200, "")
	s, p := newRejectionService(t, rejectionTarget("only", up.URL, "k"))
	ipKey, sessKey := prewriteKeys(p)

	var sessAt string
	servePrewrite(s, p, func() { sessAt, _ = s.affinity.Get(sessKey, time.Now()) })
	if sessAt != "only" {
		t.Fatalf("binding at header commit = %q, want the prewrite", sessAt)
	}
	if got, ok := s.affinity.Get(sessKey, time.Now()); ok {
		t.Fatalf("session binding %q survived an incomplete stream", got)
	}
	if got, ok := s.affinity.Get(ipKey, time.Now()); ok {
		t.Fatalf("ip binding %q survived an incomplete stream", got)
	}

	// The session was bound to a target that no longer exists; this request
	// rebinds it to "only", fails, and the old binding must be back untouched.
	start := time.Now()
	s.affinity.SetWithTTL(sessKey, "ghost", start, sessionAffinityTTL)
	s.affinity.mu.RLock()
	original := s.affinity.entries[sessKey]
	s.affinity.mu.RUnlock()
	servePrewrite(s, p, nil)
	s.affinity.mu.RLock()
	restored := s.affinity.entries[sessKey]
	s.affinity.mu.RUnlock()
	if restored != original {
		t.Fatalf("session binding = %+v, want the original %+v restored", restored, original)
	}
}

// A request that overwrote nothing the others own: if a concurrent request
// rewrote the key between the prewrite and the rollback, that binding stays.
func TestRollbackDoesNotOverwriteConcurrentRewrite(t *testing.T) {
	up := prewriteUpstream(t, "truncated", 200, "")
	s, p := newRejectionService(t, rejectionTarget("only", up.URL, "k"))
	ipKey, sessKey := prewriteKeys(p)
	servePrewrite(s, p, func() {
		now := time.Now()
		s.affinity.SetWithTTL(sessKey, "other", now, sessionAffinityTTL)
		s.affinity.SetWithTTL(ipKey, "other", now, 0)
	})
	if got, _ := s.affinity.Get(sessKey, time.Now()); got != "other" {
		t.Fatalf("session binding = %q, the concurrent rewrite was rolled over", got)
	}
	if got, _ := s.affinity.Get(ipKey, time.Now()); got != "other" {
		t.Fatalf("ip binding = %q, the concurrent rewrite was rolled over", got)
	}
}

// Responses that are not a 2xx pass-through never reach the prewrite and bind
// nothing.
func TestNoBindingForNon2xxResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"500", http.StatusInternalServerError, `{"error":{"message":"boom"}}`},
		{"429", http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`},
		{"400 passthrough", http.StatusBadRequest, `{"error":{"code":"bad_request"}}`},
		{"moderation 403", http.StatusForbidden, `{"error":{"code":"content_policy_violation","message":"blocked"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := prewriteUpstream(t, "error", tc.status, tc.body)
			s, p := newRejectionService(t, rejectionTarget("only", up.URL, "k"))
			ipKey, sessKey := prewriteKeys(p)
			var atHeaders int
			w := servePrewrite(s, p, func() {
				s.affinity.mu.RLock()
				atHeaders = len(s.affinity.entries)
				s.affinity.mu.RUnlock()
			})
			if w.Code != tc.status {
				t.Fatalf("status=%d, want %d", w.Code, tc.status)
			}
			if atHeaders != 0 {
				t.Fatalf("%d bindings written before a %d response", atHeaders, tc.status)
			}
			for _, k := range []string{ipKey, sessKey} {
				if got, ok := s.affinity.Get(k, time.Now()); ok {
					t.Fatalf("binding %q written for a %d response", got, tc.status)
				}
			}
		})
	}
}

// A binding to a non-preferred cost tier is still temporary when prewritten:
// it never slides and the end of the request does not renew it.
func TestPrewrittenOverflowBindingStaysTemporary(t *testing.T) {
	s, p, _ := cheapExpensiveService(t, config.PoolRoutingCost)
	cheap, _ := s.targetByName("cheap")
	cheap.MarkFailure(time.Now(), time.Hour)
	sessKey := sessionAffinityKey(p.Name, "sess-temp", costTestModel)
	ipKey := affinityKey("192.0.2.1", p.Name, costTestModel)

	var tempAtHeaders bool
	req := httptest.NewRequest(http.MethodPost, costTestPath, bytes.NewBufferString(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Session-Id", "sess-temp")
	req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	w := &headerProbe{ResponseRecorder: httptest.NewRecorder(), onHeader: func() {
		now := time.Now()
		tempAtHeaders = s.affinity.IsTemporary(sessKey, now) && s.affinity.IsTemporary(ipKey, now)
	}}
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !tempAtHeaders {
		t.Fatalf("status=%d, temporary bindings at header commit = %v", w.Code, tempAtHeaders)
	}
	for _, k := range []string{sessKey, ipKey} {
		if !s.affinity.IsTemporary(k, time.Now()) {
			t.Fatalf("binding %q is not temporary after delivery", k)
		}
		if _, ok := s.affinity.Get(k, time.Now().Add(temporarySessionAffinityTTL+time.Second)); ok {
			t.Fatalf("temporary binding %q outlived its absolute expiry", k)
		}
	}
}

func TestAffinityPrewriteAndRollback(t *testing.T) {
	now := time.Now()
	set := func(m *affinityMap, key, target string, at time.Time) affinityUndo {
		return m.Prewrite(key, at, func() { m.setWithTTLLocked(key, target, at, sessionAffinityTTL) })
	}
	live := func(m *affinityMap, key string) (string, bool) { return m.Get(key, now.Add(time.Second)) }

	t.Run("no previous entry is removed", func(t *testing.T) {
		m := newAffinityMap()
		u := set(m, "k", "b", now)
		m.Rollback(u)
		if got, ok := live(m, "k"); ok {
			t.Fatalf("binding %q left behind", got)
		}
	})
	t.Run("previous entry is restored exactly", func(t *testing.T) {
		m := newAffinityMap()
		m.SetWithTTL("k", "a", now.Add(-time.Hour), 90*time.Minute)
		before := m.entries["k"]
		u := set(m, "k", "b", now)
		if got, _ := live(m, "k"); got != "b" {
			t.Fatalf("prewrite not visible: %q", got)
		}
		m.Rollback(u)
		if m.entries["k"] != before {
			t.Fatalf("entry = %+v, want %+v", m.entries["k"], before)
		}
	})
	t.Run("expired previous entry is not resurrected", func(t *testing.T) {
		m := newAffinityMap()
		m.SetWithTTL("k", "a", now.Add(-48*time.Hour), time.Hour)
		u := set(m, "k", "b", now)
		m.Rollback(u)
		if _, ok := m.entries["k"]; ok {
			t.Fatal("an expired binding was restored")
		}
	})
	t.Run("a concurrent rewrite or refresh is kept", func(t *testing.T) {
		m := newAffinityMap()
		m.SetWithTTL("k", "a", now.Add(-time.Hour), sessionAffinityTTL)
		u := set(m, "k", "b", now)
		m.SetWithTTL("k", "c", now.Add(time.Millisecond), sessionAffinityTTL)
		m.Rollback(u)
		if got, _ := live(m, "k"); got != "c" {
			t.Fatalf("binding = %q, want the concurrent %q", got, "c")
		}
		v := set(m, "k2", "b", now)
		m.Touch("k2", "b", now.Add(time.Millisecond), sessionAffinityTTL)
		m.Rollback(v)
		if got, _ := live(m, "k2"); got != "b" {
			t.Fatalf("binding refreshed by another request was removed: %q", got)
		}
	})
	t.Run("two writes of one key undo newest first", func(t *testing.T) {
		m := newAffinityMap()
		m.SetWithTTL("k", "a", now.Add(-time.Hour), sessionAffinityTTL)
		before := m.entries["k"]
		u1 := set(m, "k", "b", now)
		u2 := set(m, "k", "c", now.Add(time.Millisecond))
		m.Rollback(u2)
		m.Rollback(u1)
		if m.entries["k"] != before {
			t.Fatalf("entry = %+v, want %+v", m.entries["k"], before)
		}
	})
	t.Run("a temporary binding is not renewed by a prewritten touch", func(t *testing.T) {
		m := newAffinityMap()
		m.SetTemporary("k", "b", now, sessionAffinityTTL, temporarySessionAffinityTTL)
		before := m.entries["k"]
		u := m.Prewrite("k", now.Add(time.Minute), func() { m.touchLocked("k", "b", now.Add(time.Minute), sessionAffinityTTL) })
		if u.wrote || m.entries["k"] != before {
			t.Fatalf("touch changed a temporary binding: wrote=%v entry=%+v", u.wrote, m.entries["k"])
		}
		w := m.Prewrite("fresh", now, func() {
			m.setTemporaryLocked("fresh", "b", now, sessionAffinityTTL, temporarySessionAffinityTTL)
		})
		if !m.IsTemporary("fresh", now) {
			t.Fatal("prewritten overflow binding is not temporary")
		}
		m.Rollback(w)
		if _, ok := m.entries["fresh"]; ok {
			t.Fatal("temporary prewrite not removed")
		}
	})
}
