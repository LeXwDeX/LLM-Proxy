package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
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
	turnFailed    bool
	turnStatus    int
	turnErrorBody []byte
	turnHandshake bool // completed pre-generation rejection delivered this turn
	httpFallback  bool
	httpSession   bool
}

// responsesWebsocketError forbids failover after an ambiguous send and for local
// session constraints. Completed upgrade rejections use the ordinary key loop.
type responsesWebsocketError struct {
	status int
	err    error
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
	go func() {
		ticker := time.NewTicker(s.websocketPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// WriteControl may run concurrently with the turn writer. A slow
				// client can delay it past the deadline; skip that ping only.
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			}
		}
	}()
	idle := time.NewTimer(s.websocketIdleTimeout)
	defer idle.Stop()
	closeWith := func(code int, reason string) {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
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
			closeWith(websocket.CloseGoingAway, "server is restarting")
			return
		default:
		}
		idle.Reset(s.websocketIdleTimeout)
		select {
		case <-ctx.Done():
			return
		case <-drain:
			closeWith(websocket.CloseGoingAway, "server is restarting")
			return
		case <-idle.C:
			closeWith(websocket.CloseNormalClosure, "idle timeout")
			return
		case payload := <-messages:
			// A drain that raced with this message wins: nothing was sent upstream.
			select {
			case <-drain:
				closeWith(websocket.CloseGoingAway, "server is restarting")
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
			session.httpFallback = false
			session.turnFailed = false
			session.turnStatus = 0
			session.turnErrorBody = nil
			session.turnHandshake = false
			writer := &responsesWebsocketWriter{conn: conn, session: session, header: make(http.Header)}
			appmiddleware.RequestID()(http.HandlerFunc(s.ServeHTTP)).ServeHTTP(writer, request)
			if writer.err != nil || ctx.Err() != nil {
				return
			}
			if writer.status >= 400 {
				var envelope struct {
					Error json.RawMessage `json:"error"`
				}
				if (session.httpFallback || session.turnHandshake) && json.Unmarshal(writer.buffer.Bytes(), &envelope) == nil && len(envelope.Error) > 0 {
					conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
					if conn.WriteJSON(map[string]any{"type": "error", "status": writer.status, "error": envelope.Error}) != nil {
						return
					}
					continue
				}
				message := strings.TrimSpace(writer.buffer.String())
				if session.failure != "" {
					message = session.failure
				}
				if writeResponsesWebsocketError(conn, writer.status, "proxy_request_failed", message) != nil {
					return
				}
			} else if !writer.terminal {
				status, code, message := 502, "upstream_disconnected", "upstream did not finish the response; it was not replayed"
				if session.turnStatus == http.StatusGatewayTimeout {
					status, code, message = 504, "upstream_timeout", "Responses generation timed out; it was not replayed"
				}
				if writeResponsesWebsocketError(conn, status, code, message) != nil {
					return
				}
			}
		}
	}
}

func writeResponsesWebsocketError(conn *websocket.Conn, status int, code, message string) error {
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteJSON(map[string]any{"type": "error", "status": status, "error": map[string]string{"type": "proxy_error", "code": code, "message": message}})
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
	// clientErr marks a failed write to the downstream socket, as opposed to
	// upstream content the adapter rejected.
	clientErr bool
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
		return nil, &responsesWebsocketError{classifyTransportError(err), err}
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
		return nil, &responsesWebsocketError{400, errors.New("Responses WebSocket requires an openai or dual_protocol target")}
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, &responsesWebsocketError{400, err}
	}
	payload, ok := scanJSONObject(nil, body)
	if !ok {
		return nil, &responsesWebsocketError{400, errors.New("Responses WebSocket request body is not a JSON object")}
	}
	if jsonHas(payload, "stream_id") {
		return nil, &responsesWebsocketError{400, errors.New("named streams and parallel responses are not supported")}
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
		return nil, &responsesWebsocketError{409, errors.New("WebSocket session changed; restart with full input and without previous_response_id")}
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
			return nil, &responsesWebsocketError{400, errors.New("unsupported upstream WebSocket URL scheme")}
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
			if dialCtx.Err() != nil {
				return nil, dialCtx.Err()
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
				handshake := &responsesWebsocketHandshakeError{status: status, header: response.Header.Clone(), complete: response.Body == nil, err: fmt.Errorf("upstream WebSocket handshake rejected (HTTP %d)", response.StatusCode)}
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
		return nil, &responsesWebsocketError{502, &responsesWSSentError{err: fmt.Errorf("WebSocket send failed; response was not replayed: %w", err), stage: "send"}}
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
