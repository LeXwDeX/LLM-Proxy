package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func fixture(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(newHandler(settings{Name: "mock-a", Model: "mock-model", LegacyMock: true, WSIdleTimeout: time.Second}, "test-revision"))
	t.Cleanup(s.Close)
	return s
}

func request(t *testing.T, s *httptest.Server, method, path, body string) (int, http.Header, []byte) {
	t.Helper()
	r, err := http.NewRequest(method, s.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Ws-Smoke", "yes")
	resp, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, b
}

func object(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("JSON %q: %v", b, err)
	}
	return m
}

func stats(t *testing.T, s *httptest.Server) map[string]any {
	t.Helper()
	code, hdr, b := request(t, s, "GET", "/_stats", "")
	if code != 200 || hdr.Get("X-Mock-Instance") != "mock-a" {
		t.Fatalf("stats status/header: %d %v", code, hdr)
	}
	return object(t, b)
}

func TestLegacyControlAndCounters(t *testing.T) {
	s := fixture(t)
	m := stats(t, s)
	for _, field := range []string{"total", "success", "failed", "ws_connections", "ws_requests", "ws_active", "ws_header_ok", "http_header_ok"} {
		if m[field] != float64(0) {
			t.Fatalf("%s = %v", field, m[field])
		}
	}
	for field, keys := range map[string][]string{"ws_tiers": {"default", "auto", "flex", "priority", "ultrafast"}, "ws_models": {"mock-model", "ws-smoke"}, "http_tiers": {"default", "priority", "ultrafast"}, "http_models": {"mock-model", "ws-smoke"}} {
		for _, key := range keys {
			if m[field].(map[string]any)[key] != float64(0) {
				t.Fatalf("%s/%s missing", field, key)
			}
		}
	}
	for _, mode := range []struct {
		name   string
		status int
	}{{"fail500", 500}, {"rate429", 429}, {"ok", 200}} {
		code, hdr, b := request(t, s, "POST", "/_control", `{"mode":"`+mode.name+`","delay_ms":0}`)
		if code != 200 || object(t, b)["mode"] != mode.name || hdr.Get("X-Mock-Instance") != "mock-a" {
			t.Fatalf("control: %s", b)
		}
		code, _, b = request(t, s, "GET", "/v1/models", "")
		if code != 200 || object(t, b)["data"].([]any)[0].(map[string]any)["id"] != "mock-model" {
			t.Fatalf("models probe: %s", b)
		}
		code, hdr, b = request(t, s, "POST", "/v1/chat/completions", `{"model":"mock-model","messages":[{"role":"user","content":"验收"}]}`)
		if code != mode.status || hdr.Get("X-Mock-Instance") != "mock-a" {
			t.Fatalf("%s status=%d body=%s", mode.name, code, b)
		}
		if mode.name == "rate429" && (hdr.Get("Retry-After") != "1" || object(t, b)["error"].(map[string]any)["type"] != "rate_limit_error") {
			t.Fatalf("rate429: %v %s", hdr, b)
		}
	}
	m = stats(t, s)
	if m["total"] != float64(3) || m["success"] != float64(1) || m["failed"] != float64(2) {
		t.Fatalf("GET probes must not count: %v", m)
	}
	if m["paths"].(map[string]any)["/v1/chat/completions"] != float64(3) {
		t.Fatalf("path counters: %v", m)
	}
}

func TestLegacyProtocols(t *testing.T) {
	s := fixture(t)
	for _, tc := range []struct {
		path, body, marker string
		stream             bool
	}{
		{"/v1/chat/completions", `{"model":"ws-smoke","service_tier":"ultrafast","messages":[{"role":"user","content":"验收"}]}`, `"choices"`, false},
		{"/v1/chat/completions", `{"model":"mock-model","stream":true,"messages":[{"role":"user","content":"验收"}]}`, "data: [DONE]", true},
		{"/v1/messages", `{"model":"mock-model","max_tokens":32,"messages":[{"role":"user","content":"验收"}]}`, `"content"`, false},
		{"/v1/messages", `{"model":"mock-model","stream":true,"messages":[{"role":"user","content":"验收"}]}`, "event: message_stop", true},
		{"/anthropic/v1/messages", `{"model":"mock-model","messages":[{"role":"user","content":"验收"}]}`, `"content"`, false},
		{"/v1beta/models/mock-model:generateContent", `{"contents":[{"parts":[{"text":"验收"}]}]}`, `"usageMetadata"`, false},
		{"/v1beta/models/mock-model:streamGenerateContent", `{"contents":[{"parts":[{"text":"验收"}]}]}`, `"finishReason":"STOP"`, true},
		{"/v1/models/mock-model:streamGenerateContent", `{"contents":[{"parts":[{"text":"验收"}]}]}`, `"finishReason":"STOP"`, true},
		{"/v1/responses", `{"model":"mock-model","input":"smoke","service_tier":"ultrafast"}`, `"usage"`, false},
		{"/v1/responses", `{"model":"mock-model","input":"smoke","stream":true}`, `"type":"response.completed"`, true},
	} {
		t.Run(tc.path+tc.marker, func(t *testing.T) {
			code, h, b := request(t, s, "POST", tc.path, tc.body)
			if code != 200 || h.Get("X-Mock-Instance") != "mock-a" || !bytes.Contains(b, []byte(tc.marker)) {
				t.Fatalf("%d %v %s", code, h, b)
			}
			if tc.stream && !strings.HasPrefix(h.Get("Content-Type"), "text/event-stream") {
				t.Fatalf("not SSE: %v", h)
			}
			if !tc.stream && tc.path == "/v1/chat/completions" {
				m := object(t, b)
				if m["model"] != "mock-model" || m["service_tier"] != "ultrafast" {
					t.Fatalf("chat model/tier: %v", m)
				}
			}
			if !tc.stream && tc.path == "/v1/responses" {
				m := object(t, b)
				u := m["usage"].(map[string]any)
				if m["model"] != "mock-model" || m["service_tier"] != "ultrafast" || u["input_tokens"] != float64(10) || u["output_tokens"] != float64(5) {
					t.Fatalf("fixed Responses fixture: %v", m)
				}
			}
		})
	}
	m := stats(t, s)
	if m["http_models"].(map[string]any)["ws-smoke"] != float64(1) || m["http_tiers"].(map[string]any)["ultrafast"] != float64(2) || m["http_header_ok"] != float64(10) {
		t.Fatalf("HTTP smoke counters: %v", m)
	}
	if m["paths"].(map[string]any)["/anthropic/v1/messages"] != float64(1) {
		t.Fatalf("dual protocol path: %v", m)
	}
}

func TestLegacyDelayAndTimeoutCancellation(t *testing.T) {
	s := fixture(t)
	request(t, s, "POST", "/_control", `{"mode":"ok","delay_ms":80}`)
	start := time.Now()
	request(t, s, "POST", "/v1/chat/completions", `{}`)
	if time.Since(start) < 80*time.Millisecond {
		t.Fatal("configured delay was ignored")
	}
	request(t, s, "POST", "/_control", `{"mode":"timeout","delay_ms":80}`)
	start = time.Now()
	request(t, s, "POST", "/v1/chat/completions", `{}`)
	if time.Since(start) < 5080*time.Millisecond {
		t.Fatal("timeout must add five seconds to delay_ms")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := s.Client().Do(r)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("timeout request ignored cancellation")
	}
	request(t, s, "POST", "/_control", `{"mode":"ok","delay_ms":0}`)
	code, _, _ := request(t, s, "POST", "/v1/chat/completions", `{}`)
	if code != 200 {
		t.Fatal("reset did not recover")
	}
}

func TestLegacyWebSocketSmoke(t *testing.T) {
	s := fixture(t)
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(s.URL, "http")+"/v1/responses?target=synthetic", http.Header{"X-Ws-Smoke": {"yes"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if resp.Header.Get("X-Mock-Instance") != "mock-a" {
		t.Fatalf("WS instance header: %v", resp.Header)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	previous := ""
	for i := 0; i < 2; i++ {
		var input any = "legacy smoke prompt"
		if i == 1 {
			input = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "synthetic follow-up"}}}}
		}
		if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "mock-model", "service_tier": "ultrafast", "previous_response_id": previous, "input": input}); err != nil {
			t.Fatal(err)
		}
		created, delta := false, false
		for {
			var ev map[string]any
			if err := conn.ReadJSON(&ev); err != nil {
				t.Fatal(err)
			}
			switch ev["type"] {
			case "error":
				t.Fatalf("unexpected error: %v", ev)
			case "response.created":
				created = true
			case "response.output_text.delta":
				delta = true
			case "response.completed":
				m := ev["response"].(map[string]any)
				u := m["usage"].(map[string]any)
				if !created || !delta || m["model"] != "mock-model" || m["service_tier"] != "ultrafast" || u["input_tokens"] != float64(10) || u["output_tokens"] != float64(5) {
					t.Fatalf("WS smoke contract: %v", ev)
				}
				if previous != "" && m["previous_response_id"] != previous {
					t.Fatalf("lineage: %v", m)
				}
				previous = m["id"].(string)
			}
			if ev["type"] == "response.completed" {
				break
			}
		}
	}
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(time.Second))
	conn.Close()
	deadline := time.Now().Add(time.Second)
	for {
		m := stats(t, s)
		if m["ws_active"] == float64(0) {
			if m["ws_connections"] != float64(1) || m["ws_requests"] != float64(2) || m["ws_header_ok"] != float64(1) || m["ws_tiers"].(map[string]any)["ultrafast"] != float64(2) || m["ws_models"].(map[string]any)["mock-model"] != float64(2) {
				t.Fatalf("WS counters: %v", m)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("WS active leaked")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLegacyWebSocketLineageIsolation(t *testing.T) {
	for _, name := range []string{"older-turn-on-same-connection", "stored-turn-from-another-connection"} {
		t.Run(name, func(t *testing.T) {
			s := fixture(t)
			dial := func() *websocket.Conn {
				t.Helper()
				conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(s.URL, "http")+"/v1/responses", nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = conn.Close() })
				return conn
			}
			send := func(conn *websocket.Conn, previous string) {
				t.Helper()
				if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
				if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "mock-model", "input": []any{}, "previous_response_id": previous}); err != nil {
					t.Fatal(err)
				}
			}
			complete := func(conn *websocket.Conn, previous string) string {
				t.Helper()
				send(conn, previous)
				for {
					var ev map[string]any
					if err := conn.ReadJSON(&ev); err != nil {
						t.Fatal(err)
					}
					if ev["type"] == "error" {
						t.Fatalf("valid continuation rejected: %v", ev)
					}
					if ev["type"] == "response.completed" {
						response := ev["response"].(map[string]any)
						id, ok := response["id"].(string)
						if !ok || id == "" || id == previous || response["previous_response_id"] != previous {
							t.Fatalf("completed lineage: %v", response)
						}
						return id
					}
				}
			}

			owner := dial()
			storedID := complete(owner, "")
			// Prove this is a stored response, not merely an unknown ID.
			code, _, body := request(t, s, "GET", "/v1/responses/"+storedID, "")
			if code != http.StatusOK || object(t, body)["id"] != storedID {
				t.Fatalf("response was not stored: status=%d body=%s", code, body)
			}
			conn := owner
			latestID := ""
			if name == "older-turn-on-same-connection" {
				latestID = complete(conn, storedID)
			} else {
				conn = dial()
				latestID = complete(conn, "")
			}
			send(conn, storedID)
			var ev map[string]any
			if err := conn.ReadJSON(&ev); err != nil {
				t.Fatal(err)
			}
			errorDetails, ok := ev["error"].(map[string]any)
			if ev["type"] != "error" || ev["status"] != float64(http.StatusConflict) || !ok || errorDetails["code"] != "invalid_previous_response_id" {
				t.Fatalf("invalid lineage must be rejected: %v", ev)
			}
			// The error must not close the connection or replace its latest ID.
			complete(conn, latestID)
		})
	}
}

func TestSettingsFlagsAndEnvironment(t *testing.T) {
	env := map[string]string{"MOCK_NAME": "mock-b", "MOCK_MODEL": "custom-model", "MOCK_PORT": "9100", "MOCK_DELAY_MS": "300", "MOCK_WS_IDLE_TIMEOUT_S": "90"}
	cfg, err := parseSettings(nil, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "0.0.0.0:9100" || cfg.Name != "mock-b" || cfg.Model != "custom-model" || cfg.Delay != 300*time.Millisecond || cfg.WSIdleTimeout != 60*time.Second {
		t.Fatalf("env config: %+v", cfg)
	}
	cfg, err = parseSettings([]string{"-addr", "127.0.0.1:0", "-name", "mock-c", "-model", "flag-model", "-delay-ms", "5", "-ws-idle-timeout-s", "2", "-legacy-mock"}, func(k string) string { return env[k] })
	if err != nil || cfg.Addr != "127.0.0.1:0" || cfg.Name != "mock-c" || cfg.Model != "flag-model" || cfg.Delay != 5*time.Millisecond || cfg.WSIdleTimeout != 2*time.Second || !cfg.LegacyMock {
		t.Fatalf("flag override: %+v %v", cfg, err)
	}
	for _, k := range []string{"MOCK_PORT", "MOCK_DELAY_MS", "MOCK_WS_IDLE_TIMEOUT_S"} {
		env[k] = "invalid"
		if _, err := parseSettings(nil, func(n string) string { return env[n] }); err == nil {
			t.Fatalf("invalid %s accepted", k)
		}
		delete(env, k)
	}
}

func TestRevision(t *testing.T) {
	s := fixture(t)
	code, h, b := request(t, s, "GET", "/healthz", "")
	m := object(t, b)
	if code != 200 || h.Get("X-Mock-Instance") != "mock-a" || m["status"] != "ok" || m["instance"] != "mock-a" || m["revision"] != "test-revision" {
		t.Fatalf("health revision: %d %v %s", code, h, b)
	}
}

// This exercises the real entrypoint and linker injection, not only a handler.
func TestStandaloneRevision(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "fakellm")
	build := exec.Command("go", "build", "-ldflags", "-X main.revision=h1-build-fixture", "-o", bin, ".")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-version")
	b, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(b)) != "h1-build-fixture" {
		t.Fatalf("binary revision: %v %s", err, b)
	}
	serverCtx, serverCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer serverCancel()
	server := exec.CommandContext(serverCtx, bin, "-addr", "127.0.0.1:0", "-legacy-mock")
	server.Env = []string{"MOCK_NAME=mock-b", "MOCK_MODEL=standalone-model"}
	stderr, err := server.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { serverCancel(); _ = server.Wait() }()
	line, err := bufio.NewReader(stderr).ReadString('\n')
	if err != nil {
		t.Fatalf("startup: %v", err)
	}
	_, addr, found := strings.Cut(line, "listening on ")
	if !found {
		t.Fatalf("startup log did not expose address: %s", line)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + strings.TrimSpace(addr) + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	b, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	m := object(t, b)
	if m["revision"] != "h1-build-fixture" || m["instance"] != "mock-b" {
		t.Fatalf("running artifact identity: %v", m)
	}
	if err = server.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err = server.Wait(); err != nil {
		t.Fatalf("graceful termination: %v", err)
	}
}

func TestLegacySSEUsage(t *testing.T) {
	s := fixture(t)
	for _, tc := range []struct{ path, body, field string }{
		{"/v1/chat/completions", `{"model":"mock-model","stream":true,"messages":[{"role":"user","content":"synthetic"}]}`, "usage"},
		{"/v1/messages", `{"model":"mock-model","stream":true,"messages":[{"role":"user","content":"synthetic"}]}`, "usage"},
		{"/v1beta/models/mock-model:streamGenerateContent", `{"contents":[{"parts":[{"text":"synthetic"}]}]}`, "usageMetadata"},
		{"/v1/responses", `{"model":"mock-model","input":"synthetic","stream":true}`, "response"},
	} {
		_, _, b := request(t, s, "POST", tc.path, tc.body)
		seen := false
		for _, line := range strings.Split(string(b), "\n") {
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok || data == "[DONE]" {
				continue
			}
			m := object(t, []byte(data))
			if tc.path == "/v1/responses" {
				if m["type"] != "response.completed" {
					continue
				}
				u := m["response"].(map[string]any)["usage"].(map[string]any)
				seen = u["input_tokens"] == float64(10) && u["output_tokens"] == float64(5) && u["total_tokens"] == float64(15)
			} else if u, ok := m[tc.field].(map[string]any); ok {
				if tc.field == "usageMetadata" {
					seen = u["totalTokenCount"].(float64) > 0
				} else {
					for _, field := range []string{"total_tokens", "output_tokens"} {
						if n, ok := u[field].(float64); ok && n > 0 {
							seen = true
						}
					}
				}
			}
		}
		if !seen {
			t.Fatalf("%s omitted required SSE usage: %s", tc.path, b)
		}
	}
}

func TestLegacyWebSocketBoundaries(t *testing.T) {
	for _, tc := range []string{"oversize", "incomplete-oversize-header", "binary", "idle", "warmup-and-errors"} {
		t.Run(tc, func(t *testing.T) {
			s := fixture(t)
			code, h, _ := request(t, s, "GET", "/v1/responses", "")
			if code != 426 || h.Get("Upgrade") != "websocket" {
				t.Fatalf("upgrade required: %d %v", code, h)
			}
			request(t, s, "POST", "/_control", `{"mode":"fail500","delay_ms":500}`)
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(s.URL, "http")+"/v1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			switch tc {
			case "oversize":
				// A valid client frame header includes its four masking bytes.
				// Gorilla reads those before enforcing its message size limit.
				frame := make([]byte, 14)
				frame[0] = 0x81
				frame[1] = 0xff
				binary.BigEndian.PutUint64(frame[2:10], (2<<20)+1)
				if _, err := conn.NetConn().Write(frame); err != nil {
					t.Fatal(err)
				}
				_, _, err = conn.ReadMessage()
				if !websocket.IsCloseError(err, 1009) {
					t.Fatalf("oversize close: %v", err)
				}
			case "incomplete-oversize-header":
				// Record the non-compatible malformed fixture from the Python
				// unittest: without mask bytes Gorilla waits for a full header.
				frame := make([]byte, 10)
				frame[0] = 0x81
				frame[1] = 0xff
				binary.BigEndian.PutUint64(frame[2:], (2<<20)+1)
				if _, err := conn.NetConn().Write(frame); err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				_, _, err = conn.ReadMessage()
				if !websocket.IsCloseError(err, 1006) || time.Since(start) > 2*time.Second {
					t.Fatalf("incomplete header must close on idle timeout: %v", err)
				}
			case "binary":
				if err := conn.WriteMessage(websocket.BinaryMessage, []byte("binary")); err != nil {
					t.Fatal(err)
				}
				_, _, err = conn.ReadMessage()
				if !websocket.IsCloseError(err, 1003) {
					t.Fatalf("binary close: %v", err)
				}
			case "idle":
				start := time.Now()
				_, _, err = conn.ReadMessage()
				if err == nil || time.Since(start) > 2*time.Second {
					t.Fatalf("idle timeout not enforced: %v", err)
				}
			case "warmup-and-errors":
				pong := false
				conn.SetPongHandler(func(s string) error { pong = s == "ping"; return nil })
				if err := conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				for _, data := range []string{`{`, `{"type":"unsupported"}`, `{"type":"response.create","previous_response_id":"unknown","input":[]}`} {
					if err := conn.WriteMessage(websocket.TextMessage, []byte(data)); err != nil {
						t.Fatal(err)
					}
					var ev map[string]any
					if err := conn.ReadJSON(&ev); err != nil {
						t.Fatal(err)
					}
					if ev["type"] != "error" {
						t.Fatalf("invalid event: %v", ev)
					}
					if strings.Contains(data, "unknown") && (ev["status"] != float64(409) || ev["error"].(map[string]any)["code"] != "invalid_previous_response_id") {
						t.Fatalf("lineage rejection: %v", ev)
					}
				}
				if !pong {
					t.Fatal("ping not answered")
				}
				if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "mock-model", "generate": false, "input": []any{}}); err != nil {
					t.Fatal(err)
				}
				for {
					var ev map[string]any
					if err := conn.ReadJSON(&ev); err != nil {
						t.Fatal(err)
					}
					if ev["type"] == "error" || ev["type"] == "response.output_text.delta" {
						t.Fatalf("warmup: %v", ev)
					}
					if ev["type"] == "response.completed" {
						m := ev["response"].(map[string]any)
						u := m["usage"].(map[string]any)
						if u["input_tokens"] != float64(0) || u["output_tokens"] != float64(0) || len(m["output"].([]any)) != 0 {
							t.Fatalf("warmup usage: %v", m)
						}
						break
					}
				}
			}
		})
	}
}

func TestLegacyFaultEnvelopes(t *testing.T) {
	s := fixture(t)
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1beta/models/mock-model:generateContent"} {
		for _, mode := range []string{"fail500", "rate429"} {
			request(t, s, "POST", "/_control", `{"mode":"`+mode+`","delay_ms":0}`)
			code, h, b := request(t, s, "POST", path, `{"model":"mock-model","stream":true}`)
			m := object(t, b)
			e := m["error"].(map[string]any)
			if mode == "rate429" {
				// Python returns this OpenAI-shaped error even on Gemini paths.
				if code != 429 || h.Get("Retry-After") != "1" || e["type"] != "rate_limit_error" {
					t.Fatalf("%s 429: %v %s", path, h, b)
				}
			} else {
				if code != 500 {
					t.Fatalf("%s 500: %s", path, b)
				}
				switch path {
				case "/v1/messages":
					if m["type"] != "error" || e["type"] != "api_error" {
						t.Fatalf("Anthropic legacy error: %s", b)
					}
				case "/v1beta/models/mock-model:generateContent":
					if e["code"] != float64(500) || e["status"] != "INTERNAL" {
						t.Fatalf("Gemini legacy error: %s", b)
					}
				default:
					if e["type"] != "mock_error" {
						t.Fatalf("OpenAI legacy error: %s", b)
					}
				}
			}
		}
	}
}

func TestDefaultHandlerKeepsDeterministicMode(t *testing.T) {
	s := httptest.NewServer(newHandler(settings{Name: "fake", Model: "mock-model"}, "dev"))
	defer s.Close()
	conn, handshake, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(s.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, present := handshake.Header[http.CanonicalHeaderKey("X-Mock-Instance")]; present {
		t.Fatalf("default WS handshake must not include X-Mock-Instance: %v", handshake.Header)
	}
	_, _, b := request(t, s, "POST", "/v1/chat/completions", `{"model":"custom-upstream","messages":[{"role":"user","content":"synthetic"}]}`)
	if object(t, b)["model"] != "custom-upstream" {
		t.Fatalf("default model echo changed: %s", b)
	}
	_, h, b := request(t, s, "POST", "/v1beta/models/mock-model:streamGenerateContent", `{"contents":[{"parts":[{"text":"synthetic"}]}]}`)
	if h.Get("Content-Type") != "application/json" || !bytes.HasPrefix(b, []byte("[")) {
		t.Fatalf("default Gemini stream changed: %v %s", h, b)
	}
	_, _, b = request(t, s, "POST", "/v1/responses", `{"model":"mock-model","input":"[[fake usage.in=42 usage.out=7]]"}`)
	u := object(t, b)["usage"].(map[string]any)
	if u["input_tokens"] != float64(42) || u["output_tokens"] != float64(7) {
		t.Fatalf("default scenario changed: %v", u)
	}
}
