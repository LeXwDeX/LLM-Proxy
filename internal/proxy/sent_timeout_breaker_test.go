package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// A sent timeout is never retried. One or two in a row leave the target in
// rotation; the third consecutive one cools it (504 class). A success in
// between restarts the streak.
func TestConsecutiveSentTimeoutsCoolTarget(t *testing.T) {
	var slow atomic.Bool
	slow.Store(true)
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if slow.Load() {
			time.Sleep(200 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mock":"ok"}`))
	}))
	defer up.Close()
	cfg := &config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 1},
		Targets: []config.Target{{Name: "slow", Endpoint: up.URL, ResourcePathPrefix: "/", APIKey: "key"}},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	s, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	s.setRequestTimeout(50 * time.Millisecond)
	s.quietPeriod = time.Minute
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatal(err)
	}
	p, _ := store.Authenticate("token")
	send := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"test-model"}`))
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		return rr.Code
	}
	st, _ := s.targetByName("slow")

	for i := 0; i < 2; i++ {
		if code := send(); code != http.StatusGatewayTimeout {
			t.Fatalf("timeout %d: status %d", i, code)
		}
		if st.IsMuted(time.Now()) {
			t.Fatalf("target cooled after %d sent timeout(s)", i+1)
		}
	}
	// A success restarts the streak.
	slow.Store(false)
	if code := send(); code != http.StatusOK {
		t.Fatalf("success: %d", code)
	}
	slow.Store(true)
	for i := 0; i < 2; i++ {
		_ = send()
	}
	if st.IsMuted(time.Now()) {
		t.Fatal("streak did not restart after a success")
	}
	before := calls.Load()
	_ = send() // third in a row
	if !st.IsMuted(time.Now()) {
		t.Fatal("three consecutive sent timeouts must cool the target")
	}
	if got := st.Stats(); got.MuteReason != "upstream_504" {
		t.Fatalf("mute reason = %q, want upstream_504", got.MuteReason)
	}
	if calls.Load() != before+1 {
		t.Fatalf("sent timeout was retried: calls=%d", calls.Load()-before)
	}
}
