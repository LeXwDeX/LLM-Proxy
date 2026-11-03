package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/gorilla/websocket"
)

func websocketLivenessProxy(t *testing.T, configure func(*Service)) (*Service, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unused", 500)
	}))
	t.Cleanup(upstream.Close)
	service, _, _ := websocketTestProxy(t, upstream.URL)
	configure(service)
	principal := newAllowAllPrincipal(t, "ws-liveness")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	}))
	t.Cleanup(server.Close)
	return service, server
}

func websocketSessionCount(s *Service) int {
	s.websocketMu.Lock()
	defer s.websocketMu.Unlock()
	return len(s.websocketSessions)
}

func dialLiveness(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+responsesPath, http.Header{"Authorization": {"Bearer proxy-client-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// Issue #153 B11: a half-open client that never answers pings is ended once
// no pong or frame arrived for the liveness window, releasing its slot.
func TestResponsesWebsocketHalfOpenClientEndsSession(t *testing.T) {
	s, server := websocketLivenessProxy(t, func(s *Service) {
		s.websocketPingInterval = 20 * time.Millisecond
		s.websocketPongTimeoutFloor = 100 * time.Millisecond
	})
	conn := dialLiveness(t, server)
	// Never read: gorilla answers pings only while reading, so no pong is sent.
	_ = conn
	waitForWS(t, "the session to register", func() bool { return websocketSessionCount(s) == 1 })
	started := time.Now()
	waitForWS(t, "the half-open session to end", func() bool { return websocketSessionCount(s) == 0 })
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond {
		t.Fatalf("session ended after %v, before the liveness window", elapsed)
	}
}

// A client that answers pings stays connected past the liveness window.
func TestResponsesWebsocketPongKeepsSessionAlive(t *testing.T) {
	s, server := websocketLivenessProxy(t, func(s *Service) {
		s.websocketPingInterval = 20 * time.Millisecond
		s.websocketPongTimeoutFloor = 100 * time.Millisecond
	})
	conn := dialLiveness(t, server)
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	waitForWS(t, "the session to register", func() bool { return websocketSessionCount(s) == 1 })
	time.Sleep(400 * time.Millisecond)
	if websocketSessionCount(s) != 1 {
		t.Fatal("a responsive client was disconnected")
	}
}

// Issue #153 B12: after an idle close frame the server waits briefly for the
// peer's close instead of dropping TCP at once, and ends as soon as it arrives.
func TestResponsesWebsocketIdleCloseWaitsForPeerClose(t *testing.T) {
	for _, tc := range []struct {
		name       string
		echo       bool
		minElapsed time.Duration
		maxElapsed time.Duration
	}{
		{"peer_silent", false, 500 * time.Millisecond, 3 * time.Second},
		{"peer_echoes", true, 0, 400 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, server := websocketLivenessProxy(t, func(s *Service) {
				s.websocketIdleTimeout = 50 * time.Millisecond
			})
			conn := dialLiveness(t, server)
			if !tc.echo {
				conn.SetCloseHandler(func(int, string) error { return nil })
			}
			waitForWS(t, "the session to register", func() bool { return websocketSessionCount(s) == 1 })
			_, _, err := conn.ReadMessage()
			var closeErr *websocket.CloseError
			if ce, ok := err.(*websocket.CloseError); ok {
				closeErr = ce
			}
			if closeErr == nil || closeErr.Code != websocket.CloseNormalClosure {
				t.Fatalf("expected idle close 1000, got %v", err)
			}
			received := time.Now()
			waitForWS(t, "the session to end", func() bool { return websocketSessionCount(s) == 0 })
			elapsed := time.Since(received)
			if elapsed < tc.minElapsed || elapsed > tc.maxElapsed {
				t.Fatalf("session ended %v after the close frame, want [%v, %v]", elapsed, tc.minElapsed, tc.maxElapsed)
			}
		})
	}
}
