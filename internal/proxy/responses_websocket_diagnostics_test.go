package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	gorillaws "github.com/gorilla/websocket"
)

const responsesWebsocketTerminalDiagnostic = "responses websocket ended without a terminal event"

func captureWebsocketDiagnostics(t *testing.T, service *Service) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	service.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	return &logs
}

func assertLogField(t *testing.T, logs *bytes.Buffer, want string) {
	t.Helper()
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("diagnostic log missing %q:\n%s", want, logs.String())
	}
}

func assertLogDuration(t *testing.T, logs *bytes.Buffer) {
	t.Helper()
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, responsesWebsocketTerminalDiagnostic) {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode diagnostic log: %v", err)
		}
		duration, ok := entry["duration_ms"].(float64)
		if !ok || duration < 0 {
			t.Fatalf("diagnostic duration_ms=%v", entry["duration_ms"])
		}
		return
	}
	t.Fatalf("diagnostic log missing terminal entry:\n%s", logs.String())
}

func TestWebsocketBoundedDiagnosticValue(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "empty", value: "", want: "unknown"},
		{name: "short", value: "codex-fast", want: "codex-fast"},
		{name: "long ascii", value: strings.Repeat("a", 129), want: strings.Repeat("a", 128)},
		{name: "utf8 boundary", value: strings.Repeat("a", 127) + "界", want: strings.Repeat("a", 127)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := websocketBoundedDiagnosticValue(tc.value, 128); got != tc.want {
				t.Fatalf("bounded value=%q want %q", got, tc.want)
			}
		})
	}
}

func TestResponsesWebsocketDisconnectWithoutTerminalIsDiagnosed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&gorillaws.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		var request map[string]any
		if conn.ReadJSON(&request) != nil {
			return
		}
		if err := conn.WriteJSON(map[string]any{
			"type":            "response.output_text.delta",
			"sequence_number": 3,
			"delta":           "fixture response secret",
		}); err != nil {
			t.Error(err)
			return
		}
		// Give the frame time to reach the proxy before the close frame.
		time.Sleep(50 * time.Millisecond)
		_ = conn.WriteControl(gorillaws.CloseMessage, gorillaws.FormatCloseMessage(gorillaws.CloseGoingAway, "fixture upstream bye"), time.Now().Add(time.Second))
	}))
	defer upstream.Close()
	service, server, _ := websocketTestProxy(t, upstream.URL)
	logs := captureWebsocketDiagnostics(t, service)
	conn := dialWebsocketTest(t, server)
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "fixture prompt secret"}); err != nil {
		t.Fatal(err)
	}
	if event := readWebsocketEvent(t, conn); event["type"] != "response.output_text.delta" {
		t.Fatalf("forwarded event=%v", event)
	}
	event := readWebsocketEvent(t, conn)
	detail, _ := event["error"].(map[string]any)
	if event["type"] != "error" || event["status"] != float64(502) || detail["code"] != "upstream_disconnected" ||
		detail["message"] != "upstream did not finish the response; it was not replayed" {
		t.Fatalf("public error changed: %v", event)
	}
	waitWebsocketSlotsReleased(t, service)
	assertLogField(t, logs, `"msg":"`+responsesWebsocketTerminalDiagnostic+`"`)
	assertLogField(t, logs, `"status":502`)
	assertLogField(t, logs, `"code":"upstream_disconnected"`)
	assertLogField(t, logs, `"model":"codex-fast"`)
	assertLogField(t, logs, `"failure_stage":"wait_terminal_event"`)
	assertLogDuration(t, logs)
	assertLogField(t, logs, `"forwarded":true`)
	assertLogField(t, logs, `"last_event_type":"response.output_text.delta"`)
	assertLogField(t, logs, `"last_event_sequence":"3"`)
	assertLogField(t, logs, `"upstream_close_code":"1001"`)
	assertLogField(t, logs, `"upstream_close_reason_present":true`)
	assertLogField(t, logs, `"upstream_close_reason_length":"20"`)
	assertLogField(t, logs, `"copy_error":"unexpected_eof"`)
	assertLogField(t, logs, `"usage_terminal":"false"`)
	if strings.Contains(logs.String(), `"request_id":""`) {
		t.Fatalf("diagnostic log lacks a request id:\n%s", logs.String())
	}
	for _, secret := range []string{"fixture prompt secret", "fixture response secret", "fixture upstream bye", "sk-test-key"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("diagnostic log leaked %q:\n%s", secret, logs.String())
		}
	}
}

func TestResponsesWebsocketHTTPFallbackMissingTerminalIsDiagnosed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gorillaws.IsWebSocketUpgrade(r) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		event, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "sequence_number": 7, "delta": "fixture fallback secret"})
		fmt.Fprintf(w, "data: %s\n\n", event)
	}))
	defer upstream.Close()
	service, server, _ := websocketTestProxy(t, upstream.URL)
	logs := captureWebsocketDiagnostics(t, service)
	conn := dialWebsocketTest(t, server)
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "fixture prompt secret"}); err != nil {
		t.Fatal(err)
	}
	if event := readWebsocketEvent(t, conn); event["type"] != "response.output_text.delta" {
		t.Fatalf("forwarded event=%v", event)
	}
	event := readWebsocketEvent(t, conn)
	detail, _ := event["error"].(map[string]any)
	if event["type"] != "error" || event["status"] != float64(502) || detail["code"] != "upstream_disconnected" ||
		detail["message"] != "upstream did not finish the response; it was not replayed" {
		t.Fatalf("public error changed: %v", event)
	}
	waitWebsocketSlotsReleased(t, service)
	assertLogField(t, logs, `"msg":"`+responsesWebsocketTerminalDiagnostic+`"`)
	assertLogField(t, logs, `"status":502`)
	assertLogField(t, logs, `"model":"codex-fast"`)
	assertLogField(t, logs, `"failure_stage":"wait_terminal_event"`)
	assertLogDuration(t, logs)
	assertLogField(t, logs, `"forwarded":true`)
	assertLogField(t, logs, `"last_event_type":"response.output_text.delta"`)
	assertLogField(t, logs, `"last_event_sequence":"7"`)
	assertLogField(t, logs, `"upstream_close_code":"unknown"`)
	assertLogField(t, logs, `"upstream_close_reason_present":false`)
	assertLogField(t, logs, `"upstream_close_reason_length":"unknown"`)
	assertLogField(t, logs, `"upstream_close_reason_hash":"unknown"`)
	assertLogField(t, logs, `"copy_error":"none"`)
	assertLogField(t, logs, `"usage_terminal":"false"`)
	if strings.Contains(logs.String(), "fixture fallback secret") || strings.Contains(logs.String(), "fixture prompt secret") {
		t.Fatalf("diagnostic log leaked response content:\n%s", logs.String())
	}
}

func TestResponsesWebsocketTimeoutIsDiagnosed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&gorillaws.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		var request map[string]any
		if conn.ReadJSON(&request) != nil {
			return
		}
		conn.ReadMessage()
	}))
	defer upstream.Close()
	service, server, _ := websocketTestProxy(t, upstream.URL)
	logs := captureWebsocketDiagnostics(t, service)
	service.requestTimeout.Store(int64(150 * time.Millisecond))
	conn := dialWebsocketTest(t, server)
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"}); err != nil {
		t.Fatal(err)
	}
	event := readWebsocketEvent(t, conn)
	detail, _ := event["error"].(map[string]any)
	if event["type"] != "error" || event["status"] != float64(504) || detail["code"] != "upstream_timeout" ||
		detail["message"] != "Responses generation timed out; it was not replayed" {
		t.Fatalf("public error changed: %v", event)
	}
	waitWebsocketSlotsReleased(t, service)
	assertLogField(t, logs, `"msg":"`+responsesWebsocketTerminalDiagnostic+`"`)
	assertLogField(t, logs, `"status":504`)
	assertLogField(t, logs, `"code":"upstream_timeout"`)
	assertLogField(t, logs, `"model":"codex-fast"`)
	assertLogField(t, logs, `"failure_stage":"wait_first_event"`)
	assertLogDuration(t, logs)
	assertLogField(t, logs, `"forwarded":false`)
	assertLogField(t, logs, `"last_event_type":"unknown"`)
	assertLogField(t, logs, `"copy_error":"deadline_exceeded"`)
	assertLogField(t, logs, `"usage_terminal":"false"`)
}

func TestResponsesWebsocketCompletedTurnHasNoTerminalDiagnostic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&gorillaws.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		var request map[string]any
		if conn.ReadJSON(&request) != nil {
			return
		}
		conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "resp_1", "status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		}})
		conn.ReadMessage()
	}))
	defer upstream.Close()
	service, server, _ := websocketTestProxy(t, upstream.URL)
	logs := captureWebsocketDiagnostics(t, service)
	conn := dialWebsocketTest(t, server)
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"}); err != nil {
		t.Fatal(err)
	}
	if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
		t.Fatalf("event=%v", event)
	}
	waitWebsocketSlotsReleased(t, service)
	if strings.Contains(logs.String(), responsesWebsocketTerminalDiagnostic) || strings.Contains(logs.String(), "stream copy failed") {
		t.Fatalf("completed turn produced an error diagnostic:\n%s", logs.String())
	}
}

func TestResponsesWebsocketBodyPropagatesUpstreamCloseDetails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow()
		_ = conn.Close(coderws.StatusGoingAway, "fixture close reason")
	}))
	defer upstream.Close()
	conn, _, err := coderws.Dial(context.Background(), "ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	body := &responsesWebsocketBody{conn: newResponsesWebsocketUpstream(conn, nil), ctx: context.Background(), stop: func() bool { return false }}
	_, readErr := body.Read(make([]byte, 32))
	if !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("read error=%v, want io.ErrUnexpectedEOF", readErr)
	}
	var closeErr coderws.CloseError
	if !errors.As(readErr, &closeErr) {
		t.Fatalf("read error lost the close details: %v", readErr)
	}
	if closeErr.Code != coderws.StatusGoingAway || closeErr.Reason != "fixture close reason" {
		t.Fatalf("close details=%+v", closeErr)
	}
}

func TestResponsesWebsocketUntrustedEventMetadataIsRedacted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&gorillaws.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		var request map[string]any
		if conn.ReadJSON(&request) != nil {
			return
		}
		if err := conn.WriteJSON(map[string]any{
			"type":            "fixture secret type",
			"sequence_number": map[string]any{"prompt": "fixture nested secret"},
			"delta":           "fixture response secret",
		}); err != nil {
			t.Error(err)
			return
		}
		time.Sleep(50 * time.Millisecond)
		_ = conn.WriteControl(gorillaws.CloseMessage, gorillaws.FormatCloseMessage(gorillaws.CloseGoingAway, "fixture close reason"), time.Now().Add(time.Second))
	}))
	defer upstream.Close()
	service, server, _ := websocketTestProxy(t, upstream.URL)
	logs := captureWebsocketDiagnostics(t, service)
	conn := dialWebsocketTest(t, server)
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"}); err != nil {
		t.Fatal(err)
	}
	if event := readWebsocketEvent(t, conn); event["type"] != "fixture secret type" {
		t.Fatalf("forwarded event=%v", event)
	}
	if event := readWebsocketEvent(t, conn); event["type"] != "error" {
		t.Fatalf("terminal event=%v", event)
	}
	waitWebsocketSlotsReleased(t, service)
	assertLogField(t, logs, `"last_event_type":"other"`)
	assertLogField(t, logs, `"last_event_sequence":"unknown"`)
	assertLogField(t, logs, `"upstream_close_reason_present":true`)
	for _, secret := range []string{"fixture secret type", "fixture nested secret", "fixture response secret", "fixture close reason"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("diagnostic log leaked %q:\n%s", secret, logs.String())
		}
	}
}

func TestResponsesWebsocketSessionDiagnosticsResetPerTurn(t *testing.T) {
	session := &responsesWebsocketSession{}
	session.recordCopyOutcome(coderws.CloseError{Code: coderws.StatusGoingAway, Reason: "first turn"}, &usageStream{terminal: true})
	if session.closeCode != int(coderws.StatusGoingAway) || session.closeReason != "first turn" || !session.usageTerminal {
		t.Fatalf("first turn diagnostics=%+v", session)
	}
	session.recordCopyOutcome(context.DeadlineExceeded, &usageStream{terminal: false})
	if session.closeCode != 0 || session.closeReason != "" || session.usageKnown != true || session.usageTerminal {
		t.Fatalf("later turn inherited close diagnostics: %+v", session)
	}
	if session.copyErrorDiagnostic() != "deadline_exceeded" || session.usageTerminalDiagnostic() != "false" {
		t.Fatalf("later turn diagnostics=%s/%s", session.copyErrorDiagnostic(), session.usageTerminalDiagnostic())
	}
	session.recordCopyOutcome(nil, nil)
	if session.usageKnown || session.copyErrorDiagnostic() != "none" || session.usageTerminalDiagnostic() != "unknown" {
		t.Fatalf("clean turn diagnostics=%+v", session)
	}
}
