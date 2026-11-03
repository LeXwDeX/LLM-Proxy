package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/errorlog"
	appmiddleware "github.com/LeXwDeX/LLM-Proxy/internal/middleware"
	coderwebsocket "github.com/coder/websocket"
	"github.com/gorilla/websocket"
)

// Session limits are variables so tests can shorten them.
var (
	// Idle sessions are closed between turns; nginx in front allows 1800s.
	responsesWebsocketIdleTimeout = 15 * time.Minute
	// Server pings keep NAT and intermediary state alive while a client thinks.
	responsesWebsocketPingInterval = 30 * time.Second
	// Concurrent native sessions per client credential.
	responsesWebsocketPerClient = 64
	// A session with no pong and no frame from the client for
	// max(4 x ping interval, this floor) is half-open and is ended (#153 B11).
	responsesWebsocketPongTimeoutFloor = 2 * time.Minute
	// After an idle or drain close frame, wait this long for the client's
	// close before dropping TCP (#153 B12).
	responsesWebsocketCloseWait = time.Second
	// Between turns an upstream socket idle this long is pinged, so provider
	// idle cutoffs do not end a session that may still be continued.
	responsesWebsocketUpstreamPingAfter   = 30 * time.Second
	responsesWebsocketUpstreamPingTimeout = 10 * time.Second
)

const (
	responsesWebsocketMaxMessage       = 8 << 20
	responsesWebsocketHandshakeMaxBody = 64 << 10
)

type responsesWebsocketContextKey struct{}

type responsesWebsocketSession struct {
	conn          *responsesWebsocketUpstream
	identity      [32]byte
	target        string
	established   bool
	failure       string
	failureCode   string
	turnFailed    bool
	turnStatus    int
	turnErrorBody []byte
	turnHandshake bool // completed pre-generation rejection delivered this turn
	httpFallback  bool
	httpSession   bool
	// copy diagnostics for the terminal 502/504 warning. forward records them
	// after the response copy; they never affect routing or accounting.
	copyErrKnown    bool
	copyErrCategory string
	usageKnown      bool
	usageTerminal   bool
	closeCode       int
	closeReason     string
}

// recordCopyOutcome keeps the copy failure category, the upstream close
// details when the error carries them, and whether usage observed a terminal
// event. It is the only writer of these fields before the terminal warning.
// Every field is reset first: a later turn on the same session must not inherit
// a previous turn's close details or usage state.
func (s *responsesWebsocketSession) recordCopyOutcome(copyErr error, usage *usageStream) {
	s.copyErrKnown = true
	s.copyErrCategory = errorlog.ErrorCategory(copyErr)
	s.usageKnown = false
	s.usageTerminal = false
	s.closeCode = 0
	s.closeReason = ""
	var closeErr coderwebsocket.CloseError
	if errors.As(copyErr, &closeErr) {
		s.closeCode, s.closeReason = int(closeErr.Code), closeErr.Reason
	}
	if usage != nil {
		s.usageKnown = true
		s.usageTerminal = usage.terminal
	}
}

func (s *responsesWebsocketSession) copyErrorDiagnostic() string {
	if !s.copyErrKnown {
		return "unknown"
	}
	if s.copyErrCategory == "" {
		return "none"
	}
	return s.copyErrCategory
}

func (s *responsesWebsocketSession) usageTerminalDiagnostic() string {
	if !s.usageKnown {
		return "unknown"
	}
	if s.usageTerminal {
		return "true"
	}
	return "false"
}

func (s *responsesWebsocketSession) closeCodeDiagnostic() string {
	if s.closeCode == 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d", s.closeCode)
}

// closeReasonDiagnostic exposes only a length and a short hash. The reason is
// arbitrary upstream text and must not enter ordinary logs.
func (s *responsesWebsocketSession) closeReasonDiagnostic() (length, hash string) {
	if s.closeReason == "" {
		return "unknown", "unknown"
	}
	sum := sha256.Sum256([]byte(s.closeReason))
	return fmt.Sprintf("%d", len(s.closeReason)), hex.EncodeToString(sum[:6])
}

// websocketEventTypeDiagnostic keeps only known Responses event names. Any
// other upstream string is reported as "other" so it cannot carry text.
func websocketEventTypeDiagnostic(eventType string) string {
	switch eventType {
	case "response.created", "response.in_progress", "response.completed", "response.failed", "response.incomplete",
		"response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done",
		"response.output_text.delta", "response.output_text.done", "response.output_text.annotation.added",
		"response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.refusal.delta", "response.refusal.done", "error":
		return eventType
	case "":
		return "unknown"
	default:
		return "other"
	}
}

// websocketSequenceDiagnostic accepts only a bounded JSON number. Any other
// JSON value (object, string, oversized number) is reported as unknown so
// upstream text cannot reach the warning.
func websocketSequenceDiagnostic(raw []byte) string {
	if len(raw) == 0 || len(raw) > 20 {
		return "unknown"
	}
	for _, b := range raw {
		switch {
		case b >= '0' && b <= '9':
		case b == '-', b == '+', b == '.', b == 'e', b == 'E':
		default:
			return "unknown"
		}
	}
	return string(raw)
}

// responsesWebsocketError forbids failover after an ambiguous send and for local
// session constraints. Completed upgrade rejections use the ordinary key loop.
type responsesWebsocketError struct {
	status int
	err    error
	// code is the client-facing error code; empty means proxy_request_failed.
	code string
	// message is client-facing text for proxy-authored errors. Transport
	// errors leave it empty: their text can name the upstream host, port and
	// path, so the client gets a fixed message with the error category only.
	message string
}

func newResponsesWebsocketLocalError(status int, code, message string) *responsesWebsocketError {
	return &responsesWebsocketError{status: status, err: errors.New(message), code: code, message: message}
}

func (e *responsesWebsocketError) clientMessage() string {
	if e.message != "" {
		return e.message
	}
	return "upstream request failed and was not replayed (" + errorlog.ErrorCategory(e.err) + ")"
}

func (e *responsesWebsocketError) Error() string { return e.err.Error() }
func (e *responsesWebsocketError) Unwrap() error { return e.err }

// A rejected upgrade has an ordinary HTTP error envelope. Preserve its fields
// for the ordinary key loop, but only after a complete bounded capture.
type responsesWebsocketHandshakeError struct {
	status   int
	body     []byte
	header   http.Header
	complete bool
	err      error
	// upstreamStatus is the status the upstream actually returned.
	upstreamStatus int
}

// clientMessage omits the read error, whose text can name the upstream.
func (e *responsesWebsocketHandshakeError) clientMessage() string {
	return fmt.Sprintf("upstream WebSocket handshake rejected (HTTP %d)", e.upstreamStatus)
}

func (e *responsesWebsocketHandshakeError) Error() string { return e.err.Error() }
func (e *responsesWebsocketHandshakeError) Unwrap() error { return e.err }

func (s *Service) serveResponsesWebsocket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != responsesPath {
		http.Error(w, "WebSocket is supported only on GET /v1/responses", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	lifecycle := &responsesWebsocketLifecycle{cancel: cancel, done: make(chan struct{})}
	if principal, ok := auth.PrincipalFromContext(r.Context()); ok && principal != nil {
		lifecycle.client = principal.Name + "\x00" + principal.AccessKey
	}
	drain, status := s.registerResponsesWebsocket(lifecycle)
	switch status {
	case http.StatusServiceUnavailable:
		cancel()
		http.Error(w, "service is shutting down", status)
		return
	case http.StatusTooManyRequests:
		cancel()
		http.Error(w, "too many concurrent Responses WebSocket sessions for this client", status)
		return
	}
	defer s.unregisterResponsesWebsocket(lifecycle)
	defer cancel()
	conn, err := (&websocket.Upgrader{HandshakeTimeout: 10 * time.Second}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	lifecycle.conn.Store(conn)
	if ctx.Err() != nil {
		return
	}
	conn.SetReadLimit(responsesWebsocketMaxMessage)
	session := &responsesWebsocketSession{}
	defer func() {
		if session.conn != nil {
			session.conn.Close()
		}
	}()
	// lastHeard (Unix ns) advances on every pong and every client frame. A
	// client silent past the liveness window WHILE A TURN IS IN PROGRESS is
	// half-open: it gets a 1001 close frame and the session ends, so it does
	// not hold its per-client slot or upstream work. Between turns silence is
	// not evidence (many WS libraries answer pings only while the application
	// reads); there only the 15-minute idle timer applies. No hard read deadline.
	var lastHeard atomic.Int64
	var inTurn atomic.Bool
	lastHeard.Store(time.Now().UnixNano())
	conn.SetPongHandler(func(string) error {
		lastHeard.Store(time.Now().UnixNano())
		return nil
	})
	livenessWindow := 4 * s.websocketPingInterval
	if livenessWindow < s.websocketPongTimeoutFloor {
		livenessWindow = s.websocketPongTimeoutFloor
	}
	idle := time.NewTimer(s.websocketIdleTimeout)
	defer idle.Stop()
	// WriteControl is safe alongside the single data writer (the turn loop).
	closeWith := func(code int, reason string) {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	}
	go func() {
		ticker := time.NewTicker(s.websocketPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if inTurn.Load() && now.Sub(time.Unix(0, lastHeard.Load())) > livenessWindow {
					closeWith(websocket.CloseGoingAway, "client unresponsive")
					cancel()
					return
				}
				// WriteControl may run concurrently with the turn writer. A slow
				// client can delay it past the deadline; skip that ping only.
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			}
		}
	}()
	// gracefulClose is used only for the drain (1001) and idle (1000) closes:
	// after the close frame it waits briefly for the client's close (the reader
	// then ends and cancels ctx) or ctx, instead of dropping TCP at once.
	gracefulClose := func(code int, reason string) {
		closeWith(code, reason)
		wait := time.NewTimer(responsesWebsocketCloseWait)
		defer wait.Stop()
		select {
		case <-ctx.Done():
		case <-wait.C:
		}
	}
	messages := make(chan []byte, 1)
	var fields []jsonMember // reused across turns; payload owns the bytes
	// Read independently so a disconnected client cancels an active inference.
	go func() {
		defer cancel()
		for {
			kind, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			lastHeard.Store(time.Now().UnixNano())
			if kind != websocket.TextMessage {
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseUnsupportedData, "expected JSON text events"), time.Now().Add(time.Second))
				return
			}
			select {
			case messages <- payload:
			case <-ctx.Done():
				return
			default:
				// Control writes may safely run alongside the response writer.
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "only one queued sequential turn is supported"), time.Now().Add(time.Second))
				return
			}
		}
	}()
	for {
		// A drain lets the turn in progress finish, then ends the session.
		select {
		case <-drain:
			gracefulClose(websocket.CloseGoingAway, "server is restarting")
			return
		default:
		}
		inTurn.Store(false) // every turn, including rejected messages, ends here
		idle.Reset(s.websocketIdleTimeout)
		select {
		case <-ctx.Done():
			return
		case <-drain:
			gracefulClose(websocket.CloseGoingAway, "server is restarting")
			return
		case <-idle.C:
			gracefulClose(websocket.CloseNormalClosure, "idle timeout")
			return
		case payload := <-messages:
			// The liveness window starts with the turn, not with older silence.
			lastHeard.Store(time.Now().UnixNano())
			inTurn.Store(true)
			// A drain that raced with this message wins: nothing was sent upstream.
			select {
			case <-drain:
				gracefulClose(websocket.CloseGoingAway, "server is restarting")
				return
			default:
			}
			// One validating scan; the forwarded body keeps every other
			// member's original bytes, order and escaping.
			// Protected members are normalized before any decision, so the
			// checks below, ACL, billing and upstream all read one value.
			payload, _ = s.normalizeRequestMembers(r, payload)
			var ok bool
			fields, ok = scanJSONObject(fields[:0], payload)
			if !ok {
				if writeResponsesWebsocketError(conn, 400, "invalid_request", "expected a JSON object") != nil {
					return
				}
				continue
			}
			if eventType, _ := jsonString(jsonLast(fields, "type")); eventType != "response.create" {
				if writeResponsesWebsocketError(conn, 400, "unsupported_event", "only sequential response.create events are supported") != nil {
					return
				}
				continue
			}
			if jsonHas(fields, "stream_id") {
				if writeResponsesWebsocketError(conn, 400, "unsupported_event", "named streams and parallel responses are not supported") != nil {
					return
				}
				continue
			}
			if string(jsonLast(fields, "background")) == "true" {
				if writeResponsesWebsocketError(conn, 400, "unsupported_background", "background responses are not supported over WebSocket") != nil {
					return
				}
				continue
			}
			body := jsonRebuild(fields, nil, func(m jsonMember) ([]byte, bool) {
				return m.span, !m.named("type") && !m.named("background") && !m.named("stream")
			}, []byte(`"stream":true`))
			previousID, _ := jsonString(jsonLast(fields, "previous_response_id"))
			request := r.Clone(context.WithValue(ctx, responsesWebsocketContextKey{}, session))
			request.Method = http.MethodPost
			request.Body = io.NopCloser(bytes.NewReader(body))
			request.ContentLength = int64(len(body))
			request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
			request.Header.Set("Content-Type", "application/json")
			request.Header.Del(appmiddleware.HeaderRequestID)
			request.Header.Del("Content-Length")
			request.Header.Del("Content-Encoding")
			request.Header.Del("Connection")
			request.Header.Del("Upgrade")
			for name := range request.Header {
				if strings.HasPrefix(strings.ToLower(name), "sec-websocket-") {
					request.Header.Del(name)
				}
			}
			// Connection-local lineage must not silently jump targets after affinity,
			// key-pool, or configuration changes. Normal ACL and admission run again.
			if previousID != "" && session.target != "" {
				request.Header.Set(headerProxyTarget, session.target)
			}
			principal, status := auth.RevalidateRequest(request)
			if status != 0 {
				if writeResponsesWebsocketError(conn, status, "authentication_failed", "client authentication or IP access was revoked") != nil {
					return
				}
				continue
			}
			request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
			session.failure = ""
			session.failureCode = ""
			session.httpFallback = false
			session.turnFailed = false
			session.turnStatus = 0
			session.turnErrorBody = nil
			session.turnHandshake = false
			writer := &responsesWebsocketWriter{conn: conn, session: session, header: make(http.Header)}
			appmiddleware.RequestID()(http.HandlerFunc(s.ServeHTTP)).ServeHTTP(writer, request)
			if ctx.Err() != nil || writer.clientErr {
				return
			}
			if writer.err != nil {
				// The adapter rejected upstream content. The client socket is
				// healthy: end this turn with an error event and keep the session.
				if writer.rejectCode == "" {
					return
				}
				message := "upstream sent an invalid event; it was not replayed"
				if writer.rejectCode == "upstream_event_too_large" {
					message = "upstream event exceeds the native WebSocket message limit; it was not replayed"
				}
				if writeResponsesWebsocketError(conn, http.StatusBadGateway, writer.rejectCode, message) != nil {
					return
				}
				continue
			}
			if writer.status >= 400 {
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if conn.WriteJSON(responsesWebsocketTurnErrorEvent(session, writer)) != nil {
					return
				}
			} else if !writer.terminal {
				status, code, message := 502, "upstream_disconnected", "upstream did not finish the response; it was not replayed"
				if session.turnStatus == http.StatusGatewayTimeout {
					status, code, message = 504, "upstream_timeout", "Responses generation timed out; it was not replayed"
				}
				// One terminal diagnostic per interrupted turn. Fields come from
				// state already held here; unavailable values stay "unknown".
				// Close reasons are hashed and event metadata is restricted to a
				// bounded JSON number, so upstream text, prompts, response bodies,
				// keys and Authorization never enter the log.
				reasonLength, reasonHash := session.closeReasonDiagnostic()
				s.logger.Warn("responses websocket ended without a terminal event",
					"request_id", websocketDiagnosticValue(writer.header.Get(appmiddleware.HeaderRequestID)),
					"target", websocketDiagnosticValue(session.target),
					"status", status,
					"code", code,
					"forwarded", writer.forwarded,
					"last_event_type", websocketDiagnosticValue(writer.lastEventType),
					"last_event_sequence", websocketDiagnosticValue(writer.lastEventSequence),
					"upstream_close_code", session.closeCodeDiagnostic(),
					"upstream_close_reason_present", session.closeReason != "",
					"upstream_close_reason_length", reasonLength,
					"upstream_close_reason_hash", reasonHash,
					"copy_error", session.copyErrorDiagnostic(),
					"usage_terminal", session.usageTerminalDiagnostic(),
				)
				if writeResponsesWebsocketError(conn, status, code, message) != nil {
					return
				}
			}
		}
	}
}

// responsesWebsocketTurnErrorEvent converts a turn the HTTP pipeline answered
// with status >= 400 into one native error event. A recorded session failure
// keeps its proxy error; otherwise a JSON error object (upstream fallback or
// handshake rejections, and proxy-local quota/limit responses alike) passes
// through unchanged. Retry-After is exposed as retry_after (seconds).
func responsesWebsocketTurnErrorEvent(session *responsesWebsocketSession, writer *responsesWebsocketWriter) map[string]any {
	var event map[string]any
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	switch {
	case session.failure != "":
		code := session.failureCode
		if code == "" {
			code = "proxy_request_failed"
		}
		event = responsesWebsocketProxyErrorEvent(writer.status, code, session.failure)
	case json.Unmarshal(writer.buffer.Bytes(), &envelope) == nil && len(envelope.Error) > 0 &&
		(session.httpFallback || session.turnHandshake || envelope.Error[0] == '{'):
		event = map[string]any{"type": "error", "status": writer.status, "error": envelope.Error}
	default:
		message := strings.TrimSpace(writer.buffer.String())
		if message == "" {
			message = http.StatusText(writer.status)
		}
		event = responsesWebsocketProxyErrorEvent(writer.status, "proxy_request_failed", message)
	}
	if seconds, err := strconv.Atoi(strings.TrimSpace(writer.header.Get("Retry-After"))); err == nil && seconds >= 0 {
		event["retry_after"] = seconds
	}
	return event
}

func responsesWebsocketProxyErrorEvent(status int, code, message string) map[string]any {
	return map[string]any{"type": "error", "status": status, "error": map[string]string{"type": "proxy_error", "code": code, "message": message}}
}

func writeResponsesWebsocketError(conn *websocket.Conn, status int, code, message string) error {
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteJSON(responsesWebsocketProxyErrorEvent(status, code, message))
}

func websocketDiagnosticValue(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

// responsesWebsocketWriter converts the existing response writer's SSE back to
// native events. Usage observation and durable accounting stay in writeResponse.
type responsesWebsocketWriter struct {
	conn     *websocket.Conn
	session  *responsesWebsocketSession
	header   http.Header
	status   int
	buffer   bytes.Buffer
	terminal bool
	err      error
	line     []byte
	data     bytes.Buffer
	eventLen int
	skipLF   bool
	started  bool
	members  []jsonMember // reused per event
	// forwarded and the last event fields feed the terminal 502/504 warning.
	forwarded         bool
	lastEventType     string
	lastEventSequence string
	// clientErr marks a failed write to the downstream socket, as opposed to
	// upstream content the adapter rejected.
	clientErr bool
	// rejectCode names the upstream content the adapter rejected (an event or
	// error body over the native message limit, or invalid event JSON).
	rejectCode string
}

func (w *responsesWebsocketWriter) Header() http.Header { return w.header }
func (w *responsesWebsocketWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *responsesWebsocketWriter) Flush()                      {}
func (w *responsesWebsocketWriter) downstreamWriteFailed() bool { return w.clientErr }
func (w *responsesWebsocketWriter) Write(data []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.status == 0 {
		w.status = 200
	}
	if w.status >= 400 {
		if len(data) > responsesWebsocketMaxMessage-w.buffer.Len() {
			w.err = errors.New("upstream error exceeds native WebSocket message limit")
			w.rejectCode = "upstream_event_too_large"
			return 0, w.err
		}
		w.buffer.Write(data)
		return len(data), nil
	}
	for _, b := range data {
		// CR terminates a line immediately; swallow its optional LF even when
		// it arrives in a later Write. Bare CR and LF are both valid delimiters.
		if w.skipLF {
			w.skipLF = false
			if b == '\n' {
				continue
			}
		}
		w.eventLen++
		if w.eventLen > responsesWebsocketMaxMessage {
			w.err = errors.New("SSE event exceeds native WebSocket message limit")
			w.rejectCode = "upstream_event_too_large"
			return 0, w.err
		}
		if b != '\r' && b != '\n' {
			w.line = append(w.line, b)
			continue
		}
		w.skipLF = b == '\r'
		if err := w.finishSSELine(); err != nil {
			w.err = err
			return 0, err
		}
		w.line = w.line[:0]
	}
	return len(data), nil
}

func (w *responsesWebsocketWriter) finishSSELine() error {
	line := w.line
	if !w.started {
		w.started = true
		// The first line accumulates across Writes, including a fragmented BOM.
		line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
	}
	if len(line) == 0 {
		w.eventLen = 0
		defer w.data.Reset()
		// SSE joins data fields with LF and removes the final LF at dispatch.
		event := bytes.TrimSuffix(w.data.Bytes(), []byte("\n"))
		if len(event) == 0 || bytes.Equal(bytes.TrimSpace(event), []byte("[DONE]")) {
			return nil
		}
		terminal, valid := responsesWebsocketEventTerminal(&w.members, event)
		if !valid {
			w.rejectCode = "upstream_invalid_event"
			return errors.New("invalid JSON in upstream SSE event")
		}
		if terminal {
			w.terminal = true
			if w.session != nil {
				w.session.turnFailed, w.session.turnStatus, w.session.turnErrorBody = responsesWebsocketOutcome(event)
			}
		}
		w.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := w.conn.WriteMessage(websocket.TextMessage, event); err != nil {
			w.clientErr = true
			return err
		}
		w.forwarded = true
		eventType, _ := jsonStructString(w.members, "type")
		w.lastEventType = websocketEventTypeDiagnostic(eventType)
		w.lastEventSequence = websocketSequenceDiagnostic(jsonLast(w.members, "sequence_number"))
		return nil
	}
	field, value, found := bytes.Cut(line, []byte(":"))
	if !bytes.Equal(field, []byte("data")) {
		return nil // Comments, event/id/retry, and unknown fields are metadata.
	}
	if found && len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	w.data.Write(value)
	w.data.WriteByte('\n')
	return nil
}

// httpFallbackTurn sends one native turn as an ordinary HTTP POST because the
// upstream rejected (or is known to reject) WebSocket upgrades. No create frame
// was sent, so this is not a replay.
func (session *responsesWebsocketSession) httpFallbackTurn(s *Service, req *http.Request, body []byte, identity [32]byte, target *Target) (*http.Response, error) {
	session.httpFallback = true
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = nil
	// This proxy consumes the fallback body to produce WS frames.
	// Only advertise encodings that both frames and usage can decode.
	if encodings := req.Header.Values("Accept-Encoding"); len(encodings) != 0 {
		var supported []string
		for _, encoding := range strings.Split(strings.Join(encodings, ","), ",") {
			encoding = strings.TrimSpace(encoding)
			coding, _, _ := strings.Cut(encoding, ";")
			coding = strings.TrimSpace(coding)
			if strings.EqualFold(coding, "gzip") || strings.EqualFold(coding, "identity") {
				supported = append(supported, encoding)
			}
		}
		if len(supported) == 0 {
			supported = []string{"identity"}
		}
		req.Header.Set("Accept-Encoding", strings.Join(supported, ", "))
	}
	fallback, err := s.doGenerationHTTP(req)
	if err != nil {
		// A failure provably before any request byte (dial, TLS handshake)
		// fails over like the native dial path; anything later may have reached
		// the upstream and is never replayed.
		if isPreSendTransportError(req.Context(), err) {
			session.httpFallback = false
			return nil, err
		}
		return nil, &responsesWebsocketError{status: classifyTransportError(err), err: err}
	}
	// Native frames and usage must consume the same decoded stream.
	// Explicit Accept-Encoding or a custom transport can prevent the
	// HTTP client from performing its usual automatic decompression.
	encoding := responseContentEncoding(fallback.Header)
	if fallback.Body != nil && strings.EqualFold(encoding, "gzip") {
		decoded := &responsesWebsocketGzipBody{source: fallback.Body, ctx: req.Context()}
		// A custom transport's body may ignore the request context.
		// Close its source to release a blocked gzip header/read.
		decoded.stopCancel = context.AfterFunc(req.Context(), func() { _ = decoded.closeSource() })
		fallback.Body = decoded
		fallback.Header.Del("Content-Encoding")
		fallback.Header.Del("Content-Length")
		fallback.ContentLength = -1
		fallback.Uncompressed = true
	} else if fallback.Body != nil && encoding != "" && !strings.EqualFold(encoding, "identity") {
		// A native WebSocket cannot emit undecoded HTTP bytes as valid frames.
		// Retain metadata and report this only on Read, after the POST was sent:
		// turning it into a pre-send error could incorrectly replay generation.
		rejected := &responsesWebsocketGzipBody{source: fallback.Body, ctx: req.Context(), initErr: errors.New("unsupported content encoding in native WebSocket HTTP fallback")}
		rejected.stopCancel = context.AfterFunc(req.Context(), func() { _ = rejected.closeSource() })
		fallback.Body = rejected
	}
	// HTTP continuations still belong to this upstream identity.
	// Keep their lineage even though there is no upstream WS socket.
	session.identity = identity
	session.target = target.Name
	session.established = true
	session.httpSession = true
	return fallback, nil
}

// responsesWebsocketEventTerminal validates one event object in a single scan
// and reports whether its type is terminal, exactly as the struct decoding it
// replaces (struct{ Type string `json:"type"` }): type keys match
// case-insensitively in order, null leaves the value unchanged, and any other
// non-string type fails the decode, so {"type":1,"Type":"error"} is not
// terminal. members is reused scratch. Valid JSON that is not an object stays
// valid and non-terminal, as before.
func responsesWebsocketEventTerminal(members *[]jsonMember, data []byte) (terminal, valid bool) {
	var ok bool
	if *members, ok = scanJSONObject((*members)[:0], data); !ok {
		return false, json.Valid(data)
	}
	typ, decoded := jsonStructString(*members, "type")
	if !decoded {
		return false, true
	}
	switch typ {
	case "response.completed", "response.failed", "response.incomplete", "error":
		return true, true
	}
	return false, true
}

// doResponsesWebsocket receives the fully assembled ordinary upstream request.
// The connection is local to one authenticated client session, never global.
func (session *responsesWebsocketSession) do(req *http.Request, target *Target, s *Service) (*http.Response, error) {
	session.httpFallback = false
	if target.EndpointType != config.EndpointTypeOpenAI && target.EndpointType != config.EndpointTypeDualProtocol {
		return nil, newResponsesWebsocketLocalError(400, "", "Responses WebSocket requires an openai or dual_protocol target")
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, &responsesWebsocketError{status: 400, err: err}
	}
	payload, ok := scanJSONObject(nil, body)
	if !ok {
		return nil, newResponsesWebsocketLocalError(400, "", "Responses WebSocket request body is not a JSON object")
	}
	if jsonHas(payload, "stream_id") {
		return nil, newResponsesWebsocketLocalError(400, "", "named streams and parallel responses are not supported")
	}
	previousID, _ := jsonString(jsonLast(payload, "previous_response_id"))
	// Edit only the transport members; the rest keeps its original bytes.
	wire := jsonRebuild(payload, []byte(`"type":"response.create"`), func(m jsonMember) ([]byte, bool) {
		return m.span, !m.named("type") && !m.named("stream") && !m.named("background")
	}, nil)
	headers := req.Header.Clone()
	headers.Del("Content-Type")
	headers.Del("Content-Length")
	headers.Del("Accept-Encoding")
	fingerprintHeaders := headers.Clone()
	fingerprintHeaders.Del(appmiddleware.HeaderRequestID)
	encodedHeaders, _ := json.Marshal(fingerprintHeaders)
	identity := sha256.Sum256(append([]byte(target.Name+"\n"+req.URL.String()+"\n"), encodedHeaders...))
	// Frames that arrived after the previous terminal belong to that turn.
	if session.conn != nil && !session.conn.discardStale() {
		session.conn.Close()
	}
	// Reserving the turn stops idle pings; a socket a failed ping is closing
	// counts as gone, exactly like one the upstream closed.
	if session.conn != nil && (session.conn.closed() || !session.conn.beginTurn()) {
		session.conn.Close()
		session.conn = nil
	}
	turn, handedOff := session.conn, false
	defer func() {
		if turn != nil && !handedOff {
			turn.endTurn()
		}
	}()
	if session.established && previousID != "" && (session.identity != identity || (session.conn == nil && !session.httpSession)) {
		return nil, newResponsesWebsocketLocalError(409, "websocket_session_changed", "WebSocket session changed; restart with full input and without previous_response_id")
	}
	if session.conn != nil && session.identity != identity {
		session.conn.Close()
		session.conn = nil
	}
	if session.conn == nil {
		upstreamURL := *req.URL
		switch upstreamURL.Scheme {
		case "http":
			upstreamURL.Scheme = "ws"
		case "https":
			upstreamURL.Scheme = "wss"
		default:
			return nil, newResponsesWebsocketLocalError(400, "", "unsupported upstream WebSocket URL scheme")
		}
		unsupported, gen := s.responsesWS.knownUnsupported(identity)
		if unsupported {
			return session.httpFallbackTurn(s, req, body, identity, target)
		}
		client := s.httpClient
		if client == nil {
			client = http.DefaultClient
		}
		dialClient := *client
		baseTransport := client.Transport
		if baseTransport == nil {
			baseTransport = http.DefaultTransport
		}
		transport := &responsesWSHandshakeTransport{base: baseTransport, owned: s.automaticGzipTransport}
		dialClient.Transport = transport
		// Preserve the native handshake's no-redirect behavior. The assembled
		// authentication and Host must stay with the selected upstream identity.
		dialClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		timeout := 5 * time.Second
		if client.Timeout > 0 && client.Timeout < timeout {
			timeout = client.Timeout
		}
		// Own this timeout so Dial does not cancel a retained rejection body
		// before we consume it. A successful socket owns its own lifetime.
		dialClient.Timeout = 0
		dialCtx, cancel := context.WithTimeout(req.Context(), timeout)
		defer cancel()
		traceCtx, observed := s.responsesWS.observeNativeHandshake(dialCtx)
		conn, response, err := coderwebsocket.Dial(traceCtx, upstreamURL.String(), &coderwebsocket.DialOptions{HTTPClient: &dialClient, HTTPHeader: headers, Host: req.Host})
		observed()
		transport.restore(response)
		if err != nil {
			if response != nil && response.Body != nil {
				defer func() {
					if response.Body != nil {
						_ = response.Body.Close()
					}
				}()
			}
			if err := req.Context().Err(); err != nil {
				return nil, err // the attempt or parent ended: never a failover
			}
			if dialCtx.Err() != nil {
				// Only the handshake's own timeout expired: nothing was sent.
				return nil, errResponsesWSHandshakeTimeout
			}
			if response != nil {
				status := response.StatusCode
				if status == 400 || status == 404 || status == 405 || status == 426 || status == 501 {
					// Release the failed handshake before starting the POST. Its
					// cancellation must not own the independent fallback request.
					if response.Body != nil {
						_ = response.Body.Close()
						response.Body = nil
					}
					cancel()
					if responsesWSUpgradeUnsupported(status) {
						s.responsesWS.markUnsupported(identity, gen)
					}
					return session.httpFallbackTurn(s, req, body, identity, target)
				}
				if status < 400 {
					status = http.StatusBadGateway
				}
				handshake := &responsesWebsocketHandshakeError{status: status, header: response.Header.Clone(), complete: response.Body == nil, upstreamStatus: response.StatusCode, err: fmt.Errorf("upstream WebSocket handshake rejected (HTTP %d)", response.StatusCode)}
				if response.Body != nil {
					// A configured transport may not honor context cancellation. Own
					// the original body's close once, independently of the saved body.
					source := response.Body
					var closeOnce sync.Once
					closeSource := func() { closeOnce.Do(func() { _ = source.Close() }) }
					stopClose := context.AfterFunc(dialCtx, closeSource)
					defer func() { stopClose(); closeSource() }()
					response.Body = nil // the capture now owns the original body
					errorBody, readErr := io.ReadAll(io.LimitReader(source, responsesWebsocketHandshakeMaxBody+1))
					// Bound both the original capture and any decoded envelope. This
					// also bounds gzip headers supplied by a custom transport.
					if readErr == nil && len(errorBody) <= responsesWebsocketHandshakeMaxBody && strings.EqualFold(strings.TrimSpace(responseContentEncoding(handshake.header)), "gzip") {
						decoded, decodeErr := gzip.NewReader(bytes.NewReader(errorBody))
						readErr = decodeErr
						if decodeErr == nil {
							errorBody, readErr = io.ReadAll(io.LimitReader(decoded, responsesWebsocketHandshakeMaxBody+1))
							_ = decoded.Close()
							handshake.header.Del("Content-Encoding")
						}
					}
					encoding := responseContentEncoding(handshake.header)
					if encoding != "" && !strings.EqualFold(encoding, "identity") {
						// gzip was cleared only after successful decoding above.
						// Otherwise the raw prefix is not a classification envelope.
						readErr = errors.Join(readErr, errors.New("unsupported or invalid WebSocket handshake content encoding"))
					}
					if dialCtx.Err() != nil {
						return nil, dialCtx.Err()
					}
					if readErr != nil {
						// An incomplete JSON prefix is not a classification envelope.
						handshake.err = fmt.Errorf("%w: reading error body: %w", handshake.err, readErr)
					} else if len(errorBody) > responsesWebsocketHandshakeMaxBody {
						handshake.err = fmt.Errorf("%w: error body exceeds %d bytes", handshake.err, responsesWebsocketHandshakeMaxBody)
					} else {
						handshake.body = errorBody
						handshake.complete = true
					}
				}
				return nil, handshake
			}
			return nil, err
		}
		conn.SetReadLimit(responsesWebsocketMaxMessage)
		session.conn = newResponsesWebsocketUpstream(conn, s.responsesWS)
		session.conn.beginTurn()
		turn = session.conn
		session.identity = identity
		session.target = target.Name
		session.established = true
		session.httpSession = false
	}
	conn := session.conn
	if err := req.Context().Err(); err != nil {
		return nil, err // nothing written; the turn ends without a generation
	}
	stop := context.AfterFunc(req.Context(), func() { conn.Close() })
	if err := conn.conn.Write(req.Context(), coderwebsocket.MessageText, wire); err != nil {
		stop()
		conn.Close()
		session.conn = nil
		// The frame may have partly reached the upstream: account it as an
		// incomplete generation, exactly like the pooled path, and never replay.
		return nil, &responsesWebsocketError{status: 502, err: &responsesWSSentError{err: fmt.Errorf("WebSocket send failed; response was not replayed: %w", err), stage: "send"},
			message: "WebSocket send failed; response was not replayed (" + errorlog.ErrorCategory(err) + ")"}
	}
	reader := &responsesWebsocketBody{session: session, conn: conn, ctx: req.Context(), stop: stop}
	handedOff = true // the body ends the turn when it closes
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader, Request: req}, nil
}

// Initialize during Read so a malformed compressed response remains a copy
// failure after the POST was sent, never a pre-send error eligible for replay.
type responsesWebsocketGzipBody struct {
	source     io.ReadCloser
	ctx        context.Context
	stopCancel func() bool
	sourceOnce sync.Once
	sourceErr  error
	mu         sync.Mutex
	reader     *gzip.Reader
	initErr    error
	closed     bool
	closeOnce  sync.Once
	closeErr   error
}

func (b *responsesWebsocketGzipBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	if b.reader == nil && b.initErr == nil {
		b.reader, b.initErr = gzip.NewReader(b.source)
	}
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	if b.initErr != nil {
		return 0, b.initErr
	}
	n, err := b.reader.Read(p)
	if canceled := b.ctx.Err(); canceled != nil {
		return n, canceled
	}
	return n, err
}

func (b *responsesWebsocketGzipBody) Close() error {
	b.closeOnce.Do(func() {
		if b.stopCancel != nil {
			b.stopCancel()
		}
		// Closing the source first releases a Read blocked while initializing
		// or decompressing. Lock only after that read can finish.
		b.closeErr = b.closeSource()
		b.mu.Lock()
		defer b.mu.Unlock()
		b.closed = true
		if b.reader != nil {
			b.closeErr = errors.Join(b.closeErr, b.reader.Close())
		}
	})
	return b.closeErr
}

func (b *responsesWebsocketGzipBody) closeSource() error {
	b.sourceOnce.Do(func() { b.sourceErr = b.source.Close() })
	return b.sourceErr
}

type responsesWebsocketBody struct {
	session  *responsesWebsocketSession
	conn     *responsesWebsocketUpstream
	ctx      context.Context
	stop     func() bool
	pending  *bytes.Reader
	terminal bool
	members  []jsonMember // reused per event
}

func (b *responsesWebsocketBody) Read(p []byte) (int, error) {
	if b.pending != nil && b.pending.Len() > 0 {
		return b.pending.Read(p)
	}
	if b.terminal {
		return 0, io.EOF
	}
	var event responsesWebsocketEvent
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case next, ok := <-b.conn.events:
		if !ok {
			if err := b.ctx.Err(); err != nil {
				return 0, err
			}
			if err := b.conn.readError(); err != nil {
				// Keep io.ErrUnexpectedEOF for existing classification while
				// carrying the close code/reason (or network error) through.
				return 0, fmt.Errorf("%w: upstream WebSocket read failed: %w", io.ErrUnexpectedEOF, err)
			}
			return 0, io.ErrUnexpectedEOF
		}
		event = next
	}
	if event.kind != websocket.TextMessage {
		return 0, errors.New("invalid upstream WebSocket event")
	}
	// One validating scan replaces json.Valid, Compact and a decode. Event
	// bytes are forwarded as received; only multi-line JSON is compacted so
	// that it stays one SSE data line.
	terminal, valid := responsesWebsocketEventTerminal(&b.members, event.data)
	if !valid {
		return 0, errors.New("invalid upstream WebSocket event")
	}
	data := jsonSingleLine(event.data)
	b.terminal = terminal
	if b.terminal {
		b.session.turnFailed, b.session.turnStatus, b.session.turnErrorBody = responsesWebsocketOutcome(data)
	}
	b.pending = bytes.NewReader(append(append([]byte("data: "), data...), []byte("\n\n")...))
	return b.pending.Read(p)
}
func (b *responsesWebsocketBody) Close() error {
	b.stop()
	b.conn.endTurn()
	if !b.terminal || b.ctx.Err() != nil {
		b.conn.Close()
		b.session.conn = nil
	}
	return nil
}

func isResponsesWebsocketUpgrade(r *http.Request) bool { return websocket.IsWebSocketUpgrade(r) }

// Keep reading even between turns so upstream ping/close control frames are
// handled. The bounded queue supplies one consumer, with context-driven close
// releasing readers blocked either on the socket or on backpressure.
type responsesWebsocketEvent struct {
	kind int
	data []byte
}
type responsesWebsocketUpstream struct {
	conn      *coderwebsocket.Conn
	events    chan responsesWebsocketEvent
	done      chan struct{}
	closeOnce sync.Once
	// state is upstreamIdle between turns, upstreamTurn while a turn owns the
	// socket and upstreamPingFailed once a failed idle ping is closing it.
	state atomic.Int32
	// lastActive is the time (Unix ns) of the last frame read or turn end.
	lastActive atomic.Int64
	// readErr is the terminal socket read error (including close code/reason)
	// for the 502/504 diagnostic. Written once before events closes.
	errMu   sync.Mutex
	readErr error
}

const (
	upstreamIdle int32 = iota
	upstreamTurn
	upstreamPingFailed
)

// newResponsesWebsocketUpstream starts the socket's reader and, when the pool
// configures it, its between-turn keepalive.
func newResponsesWebsocketUpstream(conn *coderwebsocket.Conn, pool *responsesWSPool) *responsesWebsocketUpstream {
	upstream := &responsesWebsocketUpstream{conn: conn, events: make(chan responsesWebsocketEvent, 1), done: make(chan struct{})}
	upstream.lastActive.Store(time.Now().UnixNano())
	if pool != nil && pool.nativePingAfter > 0 {
		go upstream.keepalive(pool.nativePingAfter, pool.nativePingTimeout, func() { pool.stats.nativePingFail.Add(1) })
	}
	go func() {
		defer close(upstream.events)
		defer upstream.Close()
		for {
			kind, data, err := conn.Read(context.Background())
			if err != nil {
				upstream.errMu.Lock()
				upstream.readErr = err
				upstream.errMu.Unlock()
				return
			}
			upstream.lastActive.Store(time.Now().UnixNano())
			select {
			case upstream.events <- responsesWebsocketEvent{int(kind), data}:
			case <-upstream.done:
				return
			}
		}
	}()
	return upstream
}
func (u *responsesWebsocketUpstream) Close() error {
	u.closeOnce.Do(func() { close(u.done); u.conn.CloseNow() })
	return nil
}

func (u *responsesWebsocketUpstream) readError() error {
	u.errMu.Lock()
	defer u.errMu.Unlock()
	return u.readErr
}

// beginTurn reserves the socket for a turn. It fails once a failed idle ping
// has claimed the socket for closing.
func (u *responsesWebsocketUpstream) beginTurn() bool {
	return u.state.CompareAndSwap(upstreamIdle, upstreamTurn)
}

func (u *responsesWebsocketUpstream) endTurn() {
	u.lastActive.Store(time.Now().UnixNano())
	u.state.CompareAndSwap(upstreamTurn, upstreamIdle)
}

// keepalive pings the socket while no turn owns it and it has been silent for
// after. A ping that fails closes the socket only if no turn has started in
// the meantime; the next turn then dials anew or answers 409 as before. A turn
// in progress is never pinged or interrupted.
func (u *responsesWebsocketUpstream) keepalive(after, timeout time.Duration, failed func()) {
	timer := time.NewTimer(after)
	defer timer.Stop()
	for {
		select {
		case <-u.done:
			return
		case <-timer.C:
		}
		wait := after
		if u.state.Load() == upstreamIdle {
			if idle := time.Since(time.Unix(0, u.lastActive.Load())); idle < after {
				wait = after - idle
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				err := u.conn.Ping(ctx)
				cancel()
				if err != nil && !u.closed() && u.state.CompareAndSwap(upstreamIdle, upstreamPingFailed) {
					failed()
					u.Close()
					return
				}
			}
		}
		timer.Reset(wait)
	}
}

// discardStale empties events queued between turns. It reports false once the
// upstream reader has ended.
func (u *responsesWebsocketUpstream) discardStale() bool {
	for {
		select {
		case _, ok := <-u.events:
			if !ok {
				return false
			}
		default:
			return true
		}
	}
}

func (u *responsesWebsocketUpstream) closed() bool {
	select {
	case <-u.done:
		return true
	default:
		return false
	}
}

// Server.Shutdown does not own hijacked connections. Track them separately and
// wait for canceled turns to finish accounting before the trace store closes.
type responsesWebsocketLifecycle struct {
	client string
	cancel context.CancelFunc
	conn   atomic.Pointer[websocket.Conn]
	done   chan struct{}
}

// The caller holds s.websocketMu.
func (s *Service) websocketDrainLocked() chan struct{} {
	if s.websocketDrain == nil {
		s.websocketDrain = make(chan struct{})
	}
	return s.websocketDrain
}

// registerResponsesWebsocket returns the drain signal, or a rejection status.
func (s *Service) registerResponsesWebsocket(lifecycle *responsesWebsocketLifecycle) (<-chan struct{}, int) {
	s.websocketMu.Lock()
	defer s.websocketMu.Unlock()
	if s.websocketsClosing {
		return nil, http.StatusServiceUnavailable
	}
	if s.websocketSessions == nil {
		s.websocketSessions = make(map[*responsesWebsocketLifecycle]struct{})
		s.websocketClients = make(map[string]int)
	}
	if s.websocketClients[lifecycle.client] >= s.websocketPerClient {
		return nil, http.StatusTooManyRequests
	}
	s.websocketClients[lifecycle.client]++
	s.websocketSessions[lifecycle] = struct{}{}
	return s.websocketDrainLocked(), 0
}
func (s *Service) unregisterResponsesWebsocket(lifecycle *responsesWebsocketLifecycle) {
	s.websocketMu.Lock()
	defer s.websocketMu.Unlock()
	delete(s.websocketSessions, lifecycle)
	if s.websocketClients[lifecycle.client]--; s.websocketClients[lifecycle.client] <= 0 {
		delete(s.websocketClients, lifecycle.client)
	}
	close(lifecycle.done)
}

// beginResponsesWebsocketDrain refuses new sessions and asks existing ones to
// close after their current turn.
func (s *Service) beginResponsesWebsocketDrain() []*responsesWebsocketLifecycle {
	s.websocketMu.Lock()
	defer s.websocketMu.Unlock()
	if !s.websocketsClosing {
		s.websocketsClosing = true
		close(s.websocketDrainLocked())
	}
	sessions := make([]*responsesWebsocketLifecycle, 0, len(s.websocketSessions))
	for lifecycle := range s.websocketSessions {
		sessions = append(sessions, lifecycle)
	}
	return sessions
}

// DrainResponsesWebsockets lets active native turns finish and record usage,
// then closes their sessions with 1001. It returns ctx.Err() if ctx ends first;
// Close then cancels whatever remains.
func (s *Service) DrainResponsesWebsockets(ctx context.Context) error {
	for _, lifecycle := range s.beginResponsesWebsocketDrain() {
		select {
		case <-lifecycle.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *Service) closeResponsesWebsockets() error {
	sessions := s.beginResponsesWebsocketDrain()
	for _, lifecycle := range sessions {
		lifecycle.cancel()
		if conn := lifecycle.conn.Load(); conn != nil {
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "server is shutting down"), time.Now().Add(100*time.Millisecond))
			conn.Close()
		}
	}
	// Canceled turns still record their usage; never skip that to exit sooner.
	for _, lifecycle := range sessions {
		<-lifecycle.done
	}
	return nil
}

// An unfinished turn is not evidence of a broken target. Classify only explicit
// HTTP-equivalent status/error envelopes, never generated text or messages.
func responsesWebsocketOutcome(data []byte) (bool, int, []byte) {
	var event struct {
		Type     string          `json:"type"`
		Status   int             `json:"status"`
		Error    json.RawMessage `json:"error"`
		Response struct {
			Error json.RawMessage `json:"error"`
		} `json:"response"`
	}
	_ = json.Unmarshal(data, &event)
	failed := event.Type != "response.completed"
	if !failed || event.Type == "response.incomplete" {
		return failed, 0, nil
	}
	body := data
	if len(event.Error) == 0 && len(event.Response.Error) > 0 {
		body, _ = json.Marshal(struct {
			Error json.RawMessage `json:"error"`
		}{event.Response.Error})
	}
	if event.Status >= 400 {
		return failed, event.Status, body
	}
	fields := decodeUpstreamError(body)
	switch strings.ToLower(fields.Type) {
	case "invalid_request_error":
		return failed, 400, body
	case "authentication_error":
		return failed, 401, body
	case "permission_error":
		return failed, 403, body
	case "not_found_error":
		return failed, 404, body
	case "rate_limit_error":
		return failed, 429, body
	case "server_error", "api_error", "internal_server_error", "overloaded_error":
		return failed, 500, body
	}
	switch strings.ToLower(fields.Code) {
	case "rate_limit_exceeded":
		return failed, 429, body
	case "server_error", "internal_server_error":
		return failed, 500, body
	}
	return failed, 0, body
}
