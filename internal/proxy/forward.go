// forward.go — HTTP 转发、协议认证注入、响应写回与重试逻辑。
package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/errorlog"
	appmiddleware "github.com/LeXwDeX/LLM-Proxy/internal/middleware"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
	"github.com/LeXwDeX/LLM-Proxy/internal/tracestore"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

const headerAzureAuthorization = "X-Azure-Authorization"

// hopHeaders 使用 Canonical MIME Header 格式，与 http.Header 存储格式一致，
// 避免 copyHeaders 中每个 header 都做 strings.ToLower 分配。
var hopHeaders = map[string]struct{}{
	"Connection":          {},
	"Proxy-Connection":    {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailers":            {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

type forwardAttemptError struct {
	status          int
	retryable       bool
	err             error
	startedAt       time.Time // forwardRequest 进入的时刻；用于 errorlog duration 统计
	upstreamURL     string    // 已构造的上游 URL（去 query）；buildURL 后填充，便于错误日志归因
	upstreamFullURL string    // 完整 URL（含 query），用于核验 buildURL 输出（如 azure api-version）
	// skipMute=true 时，service.go 的 failover 分支不调用 handleForwardError（不 mute 目标）。
	// 仅 all-keys-exhausted 用：keypool 冷却自管恢复，mute 会误伤显式目标（评审回归修复）。
	skipMute bool
	// noUsage: this attempt may have reached the upstream without a response
	// body. The request loop records it only if it is the final outcome.
	noUsage *pendingNoUsage
}

func (e *forwardAttemptError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *forwardAttemptError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// OpenAI uses x-request-id; Claude uses request-id. Keep one trace identity
// without changing either vendor's original response header.
func upstreamResponseRequestID(headers http.Header) string {
	if id := headers.Get("X-Request-Id"); id != "" {
		return id
	}
	return headers.Get("Request-Id")
}

// forwardRequest sends one attempt. upstreamModel is the resolved upstream
// model the caller bills with (ensureModelAllowed); a sent attempt's pending
// zero-usage event uses it, as normal events do, never the wire body's model
// (which carries the target's ModelPrefix).
func (s *Service) forwardRequest(r *http.Request, state *targetState, body []byte, upstreamModel string, lease *probeLease) (*http.Response, context.CancelFunc, int, error) {
	startedAt := time.Now()
	if err := r.Context().Err(); err != nil {
		return nil, nil, -1, &forwardAttemptError{status: classifyTransportError(err), err: err}
	}
	target := state.Target()
	if target == nil {
		return nil, nil, -1, &forwardAttemptError{
			status:    http.StatusServiceUnavailable, // 配置缺失：服务不可用（5xx）
			retryable: false,
			err:       errors.New("target not configured"),
			startedAt: startedAt,
		}
	}

	// S2 节点级在途计数：进入转发前登记并发槽位（选路与占用之间的竞态窗口内
	// 少量越界候选在此被硬上限拒绝，类比 keypool 全耗尽的 retryable+skipMute 语义，
	// 由外层 failover 到下一目标；不计失败静默——容量自管恢复）。
	if !state.acquireSlot() {
		return nil, nil, -1, &forwardAttemptError{
			status:    http.StatusServiceUnavailable,
			retryable: true,
			skipMute:  true,
			err:       fmt.Errorf("proxy: target %q at max concurrency %d", target.Name, target.MaxConcurrency),
			startedAt: startedAt,
		}
	}
	nodeReleased := false
	defer func() {
		if !nodeReleased {
			state.releaseSlot()
		}
	}()

	// 选择 key：如果有 key 池则按客户端亲和选（轮询分配 + 记忆绑定），否则用 target.APIKey
	apiKey := target.APIKey
	keyIndex := -1
	if state.keyPool != nil {
		clientKey := ""
		if principal, ok := auth.PrincipalFromContext(r.Context()); ok && principal != nil {
			clientKey = principal.AccessKey
		}
		// 亲和键：IP + 客户端 access_key，区分同 token 不同机器/用户
		affinityID := clientIP(r) + "|" + clientKey
		selected, idx := state.keyPool.selectKeyForClient(affinityID)
		if selected == "" {
			if err := r.Context().Err(); err != nil {
				return nil, nil, -1, &forwardAttemptError{status: classifyTransportError(err), err: err}
			}
			// 所有 key 不可用，尝试触发唤醒模型
			if state.keyPool.tryWakeUp(time.Now()) {
				s.logger.Warn("[keypool] all keys exhausted, triggering wake-up model",
					"request_id", appmiddleware.RequestIDFromContext(r.Context()),
					"target", target.Name,
				)
				// 执行唤醒探测
				recoveredIdx := s.wakeUpProbe(r, state)
				state.keyPool.wakeUpComplete()
				if recoveredIdx >= 0 {
					// 唤醒成功，重新选择 key
					selected, idx = state.keyPool.selectKeyForClient(affinityID)
					if selected != "" {
						s.logger.Info("[keypool] wake-up successful, key recovered",
							"request_id", appmiddleware.RequestIDFromContext(r.Context()),
							"target", target.Name,
							"key_index", idx,
						)
						apiKey = selected
						keyIndex = idx
						goto keySelected
					}
				}
			}
			return nil, nil, -1, &forwardAttemptError{
				status:    http.StatusTooManyRequests, // WP-1: 全池耗尽统一 429（区分限额/限流由调用方处理）
				retryable: true,                       // LOW-67: 本 target 所有 key 耗尽，允许 service.go failover 到其他目标
				skipMute:  true,                       // keypool 冷却自管恢复；不 mute 目标，避免误伤显式目标
				err:       fmt.Errorf("proxy: all API keys exhausted for target %q", target.Name),
				startedAt: startedAt,
			}
		}
		apiKey = selected
		keyIndex = idx
	keySelected:
		clientName := ""
		if principal, ok := auth.PrincipalFromContext(r.Context()); ok && principal != nil {
			clientName = principal.Name
		}
		s.logger.Debug("[keypool] key selected",
			"request_id", appmiddleware.RequestIDFromContext(r.Context()),
			"target", target.Name,
			"client", clientName,
			"client_ip", clientIP(r),
			"key_index", idx,
			"key", maskAPIKey(selected),
		)
	}

	// #3 在途计数：选中 key 后登记该 key 的并发请求，转发结束时释放。
	// 释放保证：要么经由返回的 resp.Body.Close()（成功路径），要么经由下方 defer
	// 在出错返回（无可用 resp）时兜底，二者由 sync.Once 去重，确保不漏放也不重复放。
	var inflightRelease func()
	if state.keyPool != nil && keyIndex >= 0 {
		kp := state.keyPool
		ki := keyIndex
		kp.acquireInFlight(ki)
		var once sync.Once
		inflightRelease = func() { once.Do(func() { kp.releaseInFlight(ki) }) }
	}
	inflightHandedOff := false
	defer func() {
		if inflightRelease != nil && !inflightHandedOff {
			inflightRelease()
		}
	}()

	forwardURL, err := s.buildURL(target, r.URL)
	if err != nil {
		return nil, nil, keyIndex, &forwardAttemptError{
			status:    http.StatusServiceUnavailable, // URL 构建失败（配置/路由问题）：服务不可用（5xx）
			retryable: false,
			err:       err,
			startedAt: startedAt,
		}
	}
	fullURL := forwardURL.String()
	upstreamURL := stripURLQuery(fullURL)
	upstreamFullURL := fullURL

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.getRequestTimeout())
	// Records whether an HTTP generation may have reached the upstream, for
	// the client-gone usage settlement (client_gone_usage.go).
	sendState := &generationSendState{}
	ctx = context.WithValue(ctx, generationSendStateKey{}, sendState)

	req, err := http.NewRequestWithContext(ctx, r.Method, fullURL, bodyReader)
	if err != nil {
		cancel()
		return nil, nil, keyIndex, &forwardAttemptError{
			status:          http.StatusBadRequest,
			retryable:       false,
			err:             err,
			startedAt:       startedAt,
			upstreamURL:     upstreamURL,
			upstreamFullURL: upstreamFullURL,
		}
	}

	if len(body) > 0 {
		req.ContentLength = int64(len(body))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	} else if r.Body == nil || r.Body == http.NoBody {
		req.Body = http.NoBody
	}

	copyHeaders(req.Header, r.Header)
	req.Header.Del("Authorization")
	req.Header.Del(headerProxyTarget)
	req.Header.Del(headerAzureAuthorization)
	req.Header.Del("api-key")
	// Client credentials belong to this proxy, regardless of the selected
	// upstream protocol. Inject only the target's credential below.
	req.Header.Del("x-api-key")
	req.Header.Del("x-goog-api-key")
	// Observed responses are never negotiated as br: br decoding draws on a
	// shared, bounded budget a client could exhaust to go unbilled. An
	// upstream that sends br anyway still goes through the br observer.
	if values := req.Header.Values("Accept-Encoding"); len(values) > 0 && s.usageObservationEnabled() {
		if rewritten, changed := withoutBrotliAcceptEncoding(values); changed {
			req.Header.Set("Accept-Encoding", rewritten)
		}
	}

	// 透传 request_id 到上游：多跳 llms-proxy（如 代理 A → 代理 B → 上游）共享同一 id，
	// 全链日志可凭单一 id 追溯。入站中间件已是"入站 X-Request-ID 优先"（无则生成），
	// 故 Set context 中的 id 可让最外层入口生成的 id 贯穿所有下游跳。
	// 对 Claude/OpenAI 等原厂上游无害（它们忽略此头）。
	if reqID := appmiddleware.RequestIDFromContext(r.Context()); reqID != "" {
		req.Header.Set(appmiddleware.HeaderRequestID, reqID)
	}

	// Inject upstream credentials based on provider profile.
	authStrategy := s.resolveAuthStrategy(target, r)
	if authStrategy == nil {
		cancel()
		return nil, nil, keyIndex, &forwardAttemptError{
			status:          http.StatusInternalServerError,
			retryable:       false,
			err:             fmt.Errorf("proxy: unsupported endpoint type %q for target %q", target.EndpointType, target.Name),
			startedAt:       startedAt,
			upstreamURL:     upstreamURL,
			upstreamFullURL: upstreamFullURL,
		}
	}

	// Provider-specific credential rules (e.g. Azure without key or bearer) live
	// in the auth strategy; reject before any request bytes are prepared.
	if validator, ok := authStrategy.(interface{ ValidateCredential(string) error }); ok {
		if err := validator.ValidateCredential(apiKey); err != nil {
			cancel()
			return nil, nil, keyIndex, &forwardAttemptError{
				status: http.StatusBadRequest, retryable: false, err: err,
				startedAt: startedAt, upstreamURL: upstreamURL, upstreamFullURL: upstreamFullURL,
			}
		}
	}

	if err := injectTargetRequestHeaders(req, target, authStrategy, apiKey, r.URL.Path); err != nil {
		cancel()
		return nil, nil, keyIndex, &forwardAttemptError{
			status:          http.StatusInternalServerError,
			retryable:       false,
			err:             fmt.Errorf("proxy: auth injection failed for target %q: %w", target.Name, err),
			startedAt:       startedAt,
			upstreamURL:     upstreamURL,
			upstreamFullURL: upstreamFullURL,
		}
	}
	req.Host = target.Endpoint.Host

	// ─── httptrace 埋点：仅对大请求（>100KB）启用，避免每个请求分配 10 个闭包 ───
	reqID := appmiddleware.RequestIDFromContext(r.Context())
	reqBodySize := len(body)
	traceStart := time.Now()
	traceEnabled := reqBodySize > 100*1024
	var (
		dnsStart, dnsDone    time.Time
		connStart, connDone  time.Time
		tlsStart, tlsDone    time.Time
		gotConn              time.Time
		wroteHeaders         time.Time
		wroteRequest         time.Time
		gotFirstResponseByte time.Time
		connReused           bool
		connRemoteAddr       string
		tlsVersion           string
		httpProto            string
	)
	if traceEnabled {
		trace := &httptrace.ClientTrace{
			DNSStart:          func(_ httptrace.DNSStartInfo) { dnsStart = time.Now() },
			DNSDone:           func(_ httptrace.DNSDoneInfo) { dnsDone = time.Now() },
			ConnectStart:      func(_, _ string) { connStart = time.Now() },
			ConnectDone:       func(_, _ string, _ error) { connDone = time.Now() },
			TLSHandshakeStart: func() { tlsStart = time.Now() },
			TLSHandshakeDone: func(state tls.ConnectionState, _ error) {
				tlsDone = time.Now()
				switch state.Version {
				case tls.VersionTLS13:
					tlsVersion = "TLS1.3"
				case tls.VersionTLS12:
					tlsVersion = "TLS1.2"
				default:
					tlsVersion = fmt.Sprintf("0x%04x", state.Version)
				}
				httpProto = state.NegotiatedProtocol // "h2" or "http/1.1"
			},
			GotConn: func(info httptrace.GotConnInfo) {
				gotConn = time.Now()
				connReused = info.Reused
				if info.Conn != nil {
					connRemoteAddr = info.Conn.RemoteAddr().String()
				}
			},
			WroteHeaders:         func() { wroteHeaders = time.Now() },
			WroteRequest:         func(_ httptrace.WroteRequestInfo) { wroteRequest = time.Now() },
			GotFirstResponseByte: func() { gotFirstResponseByte = time.Now() },
		}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	}

	// Project pricing fields of the assembled body while the request is sent
	// and response headers are pending; writeResponse joins it. The goroutine
	// ends with this attempt's context (usage_pricing_context.go).
	req = req.WithContext(startPricingRequest(req.Context(), body))

	var resp *http.Response
	wsHandled := false
	if session, ok := r.Context().Value(responsesWebsocketContextKey{}).(*responsesWebsocketSession); ok {
		resp, err = session.do(req, target, s)
	} else {
		resp, wsHandled, err = s.tryResponsesWS(r, req, target, body)
		if !wsHandled {
			resp, err = s.doGenerationHTTP(req)
		}
	}
	// A completed native rejection proves no response.create was sent. Give
	// only these responses to the existing HTTP key loop. Partial captures and
	// transport errors retain the defensive, non-replayable error path below.
	var nativeHandshakeErr *responsesWebsocketHandshakeError
	if errors.As(err, &nativeHandshakeErr) && nativeHandshakeErr.complete {
		keyFailure := s.observeResponsesKeyFailure(state, target, extractModel(r, body), ResolveProtocol(r.URL.Path), keyIndex, nativeHandshakeErr.status, nativeHandshakeErr.body)
		rejection := &responsesWebsocketRejectionBody{
			Reader: bytes.NewReader(nativeHandshakeErr.body), envelope: nativeHandshakeErr.body,
			unhealthy: !keyFailure && isUpstreamFailureStatus(nativeHandshakeErr.status),
			settleHealth: func(unhealthy bool) {
				if unhealthy {
					state.MarkFailure(time.Now(), s.quietPeriod)
				} else {
					state.MarkSuccess(time.Now())
				}
				state.releaseProbe(lease)
			},
		}
		header := nativeHandshakeErr.header.Clone()
		if header == nil {
			header = make(http.Header)
		}
		header.Set("Content-Length", strconv.Itoa(len(nativeHandshakeErr.body)))
		resp = &http.Response{StatusCode: nativeHandshakeErr.status, Header: header, Body: rejection, ContentLength: int64(len(nativeHandshakeErr.body)), Request: req}
		err = nil
	}

	// 提取上游响应头中的关键标识，用于问题排查时关联百炼等上游的 request_id
	var upstreamRequestID string
	if resp != nil {
		upstreamRequestID = upstreamResponseRequestID(resp.Header)
	}

	// 对大请求（>100KB）输出详细 trace 日志；对慢请求（>5s）输出简要日志
	totalDuration := time.Since(traceStart)
	if traceEnabled {
		logFields := []any{
			"request_id", reqID,
			"upstream_request_id", upstreamRequestID,
			"target", targetName(target),
			"method", r.Method,
			"path", r.URL.Path,
			"req_bytes", reqBodySize,
			"total_ms", totalDuration.Milliseconds(),
			"conn_reused", connReused,
			"remote_addr", connRemoteAddr,
			"tls_version", tlsVersion,
			"http_proto", httpProto,
		}
		if !dnsStart.IsZero() && !dnsDone.IsZero() {
			logFields = append(logFields, "dns_ms", dnsDone.Sub(dnsStart).Milliseconds())
		}
		if !connStart.IsZero() && !connDone.IsZero() {
			logFields = append(logFields, "tcp_connect_ms", connDone.Sub(connStart).Milliseconds())
		}
		if !tlsStart.IsZero() && !tlsDone.IsZero() {
			logFields = append(logFields, "tls_handshake_ms", tlsDone.Sub(tlsStart).Milliseconds())
		}
		if !gotConn.IsZero() {
			logFields = append(logFields, "get_conn_ms", gotConn.Sub(traceStart).Milliseconds())
		}
		if !wroteHeaders.IsZero() && !gotConn.IsZero() {
			logFields = append(logFields, "write_headers_ms", wroteHeaders.Sub(gotConn).Milliseconds())
		}
		if !wroteRequest.IsZero() && !wroteHeaders.IsZero() {
			logFields = append(logFields, "write_body_ms", wroteRequest.Sub(wroteHeaders).Milliseconds())
		}
		if !gotFirstResponseByte.IsZero() && !wroteRequest.IsZero() {
			logFields = append(logFields, "wait_response_ms", gotFirstResponseByte.Sub(wroteRequest).Milliseconds())
		}
		if resp != nil {
			logFields = append(logFields, "status", resp.StatusCode)
		}
		if err != nil {
			logFields = append(logFields, "error", errorlog.ErrorCategory(err))
		}
		s.logger.Warn("upstream request trace", logFields...)
	} else if totalDuration > 5*time.Second {
		s.logger.Warn("upstream slow request",
			"request_id", reqID,
			"upstream_request_id", upstreamRequestID,
			"target", targetName(target),
			"method", r.Method,
			"path", r.URL.Path,
			"req_bytes", reqBodySize,
			"total_ms", totalDuration.Milliseconds(),
		)
	}

	// 每个请求都记录 proxy → upstream request_id 映射，便于向上游厂商报障。
	// 降为 DEBUG：高吞吐场景下逐请求 INFO 会产生大量 I/O，仅在排查时按需开启。
	if upstreamRequestID != "" {
		s.logger.Debug("upstream request id mapping",
			"request_id", reqID,
			"upstream_request_id", upstreamRequestID,
			"target", targetName(target),
			"path", r.URL.Path,
			"total_ms", totalDuration.Milliseconds(),
		)
	}

	if err != nil {
		// Classify before cancel(): the attempt context must still show
		// whether the parent or the attempt timeout ended it.
		preSend := isPreSendTransportError(ctx, err)
		cancel()
		internalStatus := classifyTransportError(err)
		var sentErr *responsesWSSentError
		wsSent := errors.As(err, &sentErr)
		var noUsage *pendingNoUsage
		if wsSent || sendState.mayHaveSent.Load() {
			// The generation may have reached the upstream but no response body
			// was observed. The request loop settles it through the shared rule:
			// recorded only if this attempt is the final outcome, otherwise
			// announced as retried. It never makes the attempt retryable.
			tokens := usageTokens{}
			if wsSent {
				tokens.diagnostic = "websocket_generation_interrupted_before_usage"
				tokens.responseStatus = "incomplete"
			}
			settle := noOfficialUsage{cause: transportNoUsageCause(r, err), tokens: tokens, model: upstreamModel, transport: clientGoneTransportHTTP, phase: clientGonePhaseBeforeHeaders}
			if wsSent {
				settle.transport = clientGoneTransportWS
			}
			if _, native := r.Context().Value(responsesWebsocketContextKey{}).(*responsesWebsocketSession); native {
				settle.phase = clientGonePhaseWSTurn
			}
			noUsage = &pendingNoUsage{
				settle: settle, target: target, state: state, keyIndex: keyIndex, status: internalStatus,
				path: r.URL.Path, rawPath: r.URL.RawPath, protocol: ResolveProtocol(r.URL.Path),
			}
		}
		var handshakeErr *responsesWSHandshakeError
		transportStatus := internalStatus
		// A timeout or cancellation may follow a sent request: never retry it, to
		// avoid duplicate inference or charges. A timeout proven to occur before
		// any request byte (dial, TLS handshake) is an unreachable target and
		// fails over like other network errors.
		retryable := (!wsHandled || errors.As(err, &handshakeErr)) &&
			((transportStatus != http.StatusGatewayTimeout && transportStatus != StatusClientClosedRequest) || preSend)
		var websocketErr *responsesWebsocketError
		if errors.As(err, &websocketErr) {
			retryable = false
			transportStatus = websocketErr.status
			if session, ok := r.Context().Value(responsesWebsocketContextKey{}).(*responsesWebsocketSession); ok {
				session.failure = websocketErr.Error()
			}
		}
		var nativeHandshakeErr *responsesWebsocketHandshakeError
		if errors.As(err, &nativeHandshakeErr) {
			// An incomplete/oversized capture is not a classification envelope.
			// Do not invent key feedback from its status or partial JSON.
			retryable = false
			transportStatus = nativeHandshakeErr.status
			if !isUpstreamFailureStatus(transportStatus) {
				// Deterministic request/key rejections confirm reachability.
				// Every completed handshake must also end a half-open probe.
				state.MarkSuccess(time.Now())
			} else {
				// Health feedback is independent of replay permission: an upstream
				// failure ends the probe with normal cooldown but never replays WS.
				state.MarkFailure(time.Now(), s.quietPeriod)
			}
			state.releaseProbe(lease)
			if session, ok := r.Context().Value(responsesWebsocketContextKey{}).(*responsesWebsocketSession); ok {
				session.failure = nativeHandshakeErr.Error()
			}
		}
		if sentErr != nil {
			internalCause := "other_error"
			switch internalStatus {
			case StatusClientClosedRequest:
				internalCause = "canceled"
			case http.StatusGatewayTimeout:
				internalCause = "timeout"
			}
			clientName := ""
			if principal, ok := auth.PrincipalFromContext(r.Context()); ok && principal != nil {
				clientName = principal.Name
			}
			clientStatusChannel := "http_response"
			if websocketErr != nil {
				clientStatusChannel = "websocket_error_event"
			}
			// Usage keeps the transport cause. The client contract can differ:
			// native WS uses an error event, not a new HTTP response after upgrade.
			// Proposed does not claim delivery (the client may have disconnected).
			s.logger.Warn("websocket generation transport failure",
				"request_id", reqID,
				"target", targetName(target),
				"client", clientName,
				"model", extractModel(r, body),
				"failure_stage", sentErr.stage,
				"internal_cause", internalCause,
				"internal_status", internalStatus,
				"client_proposed_status", transportStatus,
				"client_status_channel", clientStatusChannel,
				"possibly_sent", true,
				"replayed", false,
			)
		}
		return nil, nil, keyIndex, &forwardAttemptError{
			noUsage:         noUsage,
			status:          transportStatus,
			retryable:       retryable,
			err:             err,
			startedAt:       startedAt,
			upstreamURL:     upstreamURL,
			upstreamFullURL: upstreamFullURL,
		}
	}

	// key 池耗尽检测：检查上游响应是否表示 key 额度耗尽
	wsResponseBody := responseWSBody(resp)
	if state.keyPool != nil && keyIndex >= 0 && resp != nil && resp.StatusCode >= 400 &&
		responseWebsocketRejection(resp) == nil &&
		(wsResponseBody == nil || !wsResponseBody.generationSent) {
		checkBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if readErr != nil {
			category := "read_error"
			switch {
			case errors.Is(readErr, context.Canceled):
				category = "context_canceled"
			case errors.Is(readErr, context.DeadlineExceeded):
				category = "deadline_exceeded"
			case errors.Is(readErr, io.ErrUnexpectedEOF):
				category = "unexpected_eof"
			}
			s.logger.Warn("partial_error_body",
				"request_id", appmiddleware.RequestIDFromContext(r.Context()),
				"target", target.Name, "status", resp.StatusCode,
				"bytes_read", len(checkBody), "error_category", category)
		}
		// 重拼完整 body（peek 出的字节 + 剩余流）：非耗尽类错误（普通 400/500）
		// 会经 writeResponse 原样透传给客户端，若只保留前 4KB 会与原始
		// Content-Length/gzip 头错配，静默损坏 >4KB 的错误响应。
		restoredBody := struct {
			io.Reader
			io.Closer
		}{
			Reader: io.MultiReader(bytes.NewReader(checkBody), resp.Body),
			Closer: resp.Body,
		}
		if wsBody := responseWSBody(resp); wsBody != nil {
			restoredBody.Reader = io.MultiReader(bytes.NewReader(checkBody), wsBody.ReadCloser)
			restoredBody.Closer = wsBody.ReadCloser
			wsBody.ReadCloser = restoredBody
		} else {
			resp.Body = restoredBody
		}

		// 如果响应体是 gzip 压缩的，先解压再做模式匹配
		exhaustionBody := checkBody
		encoding := responseContentEncoding(resp.Header)
		if encoding != "" && !strings.EqualFold(encoding, "identity") {
			// Unknown or stacked encodings cannot supply a body classification.
			// Existing HTTP-status rules still apply to an empty envelope.
			exhaustionBody = nil
		}
		if strings.EqualFold(encoding, "gzip") {
			if gr, err := gzip.NewReader(bytes.NewReader(checkBody)); err == nil {
				if decompressed, err := io.ReadAll(io.LimitReader(gr, 8192)); err == nil {
					exhaustionBody = decompressed
				}
				gr.Close()
			}
		}
		if wsBody := responseWSBody(resp); wsBody != nil {
			wsBody.setOutcome(true, resp.StatusCode, exhaustionBody)
		}

		if exhausted, code := targetKeyExhausted(target, ResolveProtocol(r.URL.Path), resp.StatusCode, exhaustionBody); exhausted {
			state.keyPool.markExhaustedWithError(keyIndex, code, string(exhaustionBody))
		}
	}

	// Only the official non-stream ChatCompletionResp defines base_resp.
	// SSE, other request protocols and non-200 responses use standard rules.
	// Early returns precede the inFlight/node handoff; function defers release both.
	if !wsHandled && state.keyPool != nil && keyIndex >= 0 && resp != nil && resp.StatusCode == 200 &&
		targetVendor(target) == usageAdapterMinimax && requestSchemaForPath(r.URL.Path) == config.RequestSchemaOpenAIChat &&
		strings.EqualFold(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]), "application/json") && responseContentEncoding(resp.Header) == "" {
		peekSource := &minimaxPeekReader{Reader: resp.Body}
		prefix, _ := io.ReadAll(io.LimitReader(peekSource, 4096))
		replay := io.MultiReader(bytes.NewReader(prefix), resp.Body)
		code, ok := decodeMinimaxBaseResp(prefix)
		// An interrupted peek still owns an HTTP 200 response. Replay its
		// prefix through writeResponse so known or incomplete usage settles.
		if reason := minimaxReasonForCode(code); ok && reason != "" && peekSource.err == nil && ctx.Err() == nil {
			resp.Body.Close()
			cancel()
			state.keyPool.markExhaustedWithError(keyIndex, reason, string(prefix))
			return nil, nil, keyIndex, &forwardAttemptError{status: minimaxErrorStatus(reason), retryable: true, skipMute: true, err: errMinimaxInBandError, startedAt: startedAt, upstreamURL: upstreamURL, upstreamFullURL: upstreamFullURL}
		}
		resp.Body = struct {
			io.Reader
			io.Closer
		}{replay, resp.Body}
	}

	// 智谱 GLM 限流检测：HTTP 200 + SSE event:error + rate_limit_error（非 429）。
	// 智谱 Claude 兼容端点（/api/anthropic）的额度/速率限流返回 200 而非 429，错误藏在 SSE 流里，
	// 三种 code：1308（5h 额度）、1310（周/月额度）、1302（速率/QPS）。此处读取首个完整 SSE data 事件（最多 4KB）
	// 做匹配识别 + 提取 code，命中后：
	//   1. markExhausted 当前 key：额度类（1308/1310）用 message 里的精确重置时间，速率类（1302）用 60s
	//   2. 关闭 resp，返回 errGLMRateLimited 让上层 key 切换循环感知并切下一 key
	//   3. 冷却到期后 recoverFromCooldownLocked 自动恢复 key，闭环成立
	//
	// inFlight 生命周期说明：
	//   GLM 块在下方 #3 inFlight handoff 块之前执行，命中时直接 resp.Body.Close()
	//   并提前 return，inflightHandedOff 保持 false。函数级 defer（本函数顶部：
	//   if inflightRelease != nil && !inflightHandedOff { inflightRelease() }）在
	//   return 时触发兜底释放，因此当前 key 的 inFlight 计数在这里被正确 -1。
	//   service.go 重新 forwardRequest 时，新的 acquire/handoff 流程独立进行。
	if !wsHandled && state.keyPool != nil && keyIndex >= 0 && resp != nil && resp.StatusCode == 200 &&
		usesGLMErrorPatch(target) && responseIsIdentityEncoding(resp.Header) &&
		strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		peeked, replay := peekGLMFirstEvent(resp.Body)
		if limited, code, resetAt := detectGLMRateLimitInBody(peeked); limited {
			resp.Body.Close()
			cancel()
			reason := glmReasonForCode(code)
			cooldownEnd := glmCooldownForCode(code, resetAt, time.Now())
			state.keyPool.markExhaustedWithCooldown(keyIndex, reason, cooldownEnd, string(peeked))
			s.logger.Warn("[keypool] GLM rate limit detected in 200+SSE",
				"request_id", appmiddleware.RequestIDFromContext(r.Context()),
				"target", target.Name,
				"key_index", keyIndex,
				"code", diagnosticGLMCode(code),
				"reason", reason,
				"cooldown_end", cooldownEnd,
			)
			return nil, nil, keyIndex, &forwardAttemptError{
				status:          http.StatusTooManyRequests,
				retryable:       true,
				skipMute:        true, // GLM 限流永不 mute target：keypool 冷却自管，全 key 耗尽时让外层 failover
				err:             errGLMRateLimited,
				startedAt:       startedAt,
				upstreamURL:     upstreamURL,
				upstreamFullURL: upstreamFullURL,
			}
		}
		// 非限流：恢复 body（peek 出来的字节 + 剩余流），保证流式响应完整透传。
		resp.Body = struct {
			io.Reader
			io.Closer
		}{
			Reader: replay,
			Closer: resp.Body,
		}
	}

	// #3 将在途计数的释放移交给响应体：调用方关闭 resp.Body（重试循环或 writeResponse
	// 的 defer）时释放，覆盖流式响应的完整生命周期。节点级槽位释放（S2）与 key 级
	// 在途释放共用同一次 Close 触发，二者均由 handoff 标记豁免函数级 defer 兜底。
	if resp != nil {
		releaseNodeSlot := func() { state.releaseSlot() }
		if inflightRelease != nil {
			prevRelease := inflightRelease
			inflightRelease = func() {
				prevRelease()
				releaseNodeSlot()
			}
		} else {
			inflightRelease = releaseNodeSlot
		}
		resp.Body = &releaseOnCloseBody{ReadCloser: resp.Body, release: inflightRelease}
		inflightHandedOff = true
		nodeReleased = true
	}

	return resp, cancel, keyIndex, nil
}

// errGLMRateLimited 标识智谱 GLM 限流（HTTP 200 + SSE rate_limit_error，code 1308/1310/1302）。
// forwardRequest 已 markExhausted 当前 key 并返回此错误，service.go 的内层 key 循环感知后
// 直接切下一 key（不走 sleep 退避——已 markExhausted，退避无意义）。
// 与普通 429 retryable error 区分：GLM 限流是 key 级窗口耗尽，已标记，直接切 key。
var errGLMRateLimited = errors.New("glm rate limit detected in 200+SSE response")

// releaseOnCloseBody 包裹上游响应体，在 Close 时触发一次性的资源释放回调
// （用于 #3 在途计数释放），随后关闭底层响应体。
type releaseOnCloseBody struct {
	io.ReadCloser
	release func()
}

// Saved native handshake bytes retain their full classification envelope even
// through the slot-release wrapper. Every upstream attempt settles health once:
// discarded responses settle on Close; delivered responses use the final
// account-convergence decision. Client metrics belong only to writeResponse.
type responsesWebsocketRejectionBody struct {
	*bytes.Reader
	envelope     []byte
	unhealthy    bool
	settleHealth func(bool)
	once         sync.Once
}

func (b *responsesWebsocketRejectionBody) settle(unhealthy bool) {
	b.once.Do(func() { b.settleHealth(unhealthy) })
}

func (b *responsesWebsocketRejectionBody) Close() error {
	b.settle(b.unhealthy)
	return nil
}

func responseWebsocketRejection(resp *http.Response) *responsesWebsocketRejectionBody {
	if resp == nil {
		return nil
	}
	body := resp.Body
	if released, ok := body.(*releaseOnCloseBody); ok {
		body = released.ReadCloser
	}
	rejection, _ := body.(*responsesWebsocketRejectionBody)
	return rejection
}

func (b *releaseOnCloseBody) Close() error {
	if b.release != nil {
		b.release()
	}
	return b.ReadCloser.Close()
}

// writeResponse 写回上游响应给客户端，并记录用量、trace。
// upstreamModel 必须是已解析的上游模型名（来自 ensureModelAllowed），
// 用于生成粘连 key、记录用量事件和 trace——确保内部存储的模型名是上游真实名，而非客户端 alias。
func (s *Service) writeResponse(
	w http.ResponseWriter,
	r *http.Request,
	state *targetState,
	resp *http.Response,
	cancel context.CancelFunc,
	attempt int,
	upstreamModel string,
	requestBody []byte,
	startedAt time.Time,
	keyIndex int,
	accountLevelFailure bool, // MED-3: 收敛透传的账户级 4xx 应按失败计（mute + 不计 success）
	lease *probeLease,
) requeststats.Outcome {
	defer deferCancel(cancel)
	defer resp.Body.Close()

	// 提取上游 request_id，用于回传给客户端做问题排查关联
	upstreamRequestID := upstreamResponseRequestID(resp.Header)

	target := state.Target()
	wsBody := responseWSBody(resp)
	nativeRejection := responseWebsocketRejection(resp)
	guardClaudeStream := wsBody == nil && claudeStreamGuardEnabled(r, resp)

	for key, values := range resp.Header {
		if _, skip := hopHeaders[key]; skip {
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	if target != nil {
		w.Header().Set("X-Proxy-Target", target.Name)
		w.Header().Set("X-Azure-Target", target.Name) // backward compat
		// 池成员才带 X-Proxy-Pool；无池目标不出现该头（红线）。
		if target.Pool != "" {
			w.Header().Set(headerProxyPool, target.Pool)
		}
	}
	if upstreamRequestID != "" {
		w.Header().Set("X-Upstream-Request-Id", upstreamRequestID)
	}
	if guardClaudeStream {
		// A terminated stream has a different length from the upstream body.
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(resp.StatusCode)

	recordOutcome := func(failed, unhealthy bool) {
		if failed || isUpstreamFailureStatus(resp.StatusCode) || accountLevelFailure {
			if nativeRejection != nil {
				nativeRejection.settle(unhealthy)
			} else if unhealthy {
				state.MarkFailure(time.Now(), s.quietPeriod)
			} else {
				// Request errors and normal truncation confirm reachability,
				// just as HTTP 4xx does, and release any half-open probe lease.
				state.MarkSuccess(time.Now())
			}
			stats := state.Stats()
			s.metrics.totalFailures.Add(1)
			s.logger.Warn("target response error",
				"request_id", appmiddleware.RequestIDFromContext(r.Context()),
				"target", targetName(target),
				"status_code", resp.StatusCode,
				"consecutive_failures", stats.ConsecutiveFailure,
				"total_failures", stats.TotalFailure,
				"muted_until", stats.MutedUntil,
			)
		} else {
			state.MarkSuccess(time.Now())
			// 真实成功：重置该 key 的连续 429 限流指数退避计数（限流周期已过）
			if state.keyPool != nil && keyIndex >= 0 {
				state.keyPool.markKeySuccess(keyIndex)
				// WP-3：重置连续失败计数（consecutiveFails，RETRY≥2 降级依据）
				state.keyPool.resetFails(keyIndex)
			}
			// WP-2: AIMD additive increase — signal a successful upstream response
			// to allow the admission controller to slowly raise its request rate.
			if am := s.admissionManager.Load(); am != nil {
				am.OnSuccess(target.Name)
			}
			s.metrics.totalSuccess.Add(1)
			if attempt > 1 {
				s.logger.Info("request succeeded after failover",
					"request_id", appmiddleware.RequestIDFromContext(r.Context()),
					"target", targetName(target),
					"attempt", attempt,
					"status_code", resp.StatusCode,
				)
			}
		}
		state.releaseProbe(lease)

	}
	websocketSession, websocketResponse := r.Context().Value(responsesWebsocketContextKey{}).(*responsesWebsocketSession)
	if websocketResponse && (websocketSession.httpFallback || nativeRejection != nil) {
		websocketSession.turnStatus = resp.StatusCode
		websocketSession.turnFailed = resp.StatusCode >= 400
		websocketSession.turnHandshake = nativeRejection != nil
	}
	if !websocketResponse && wsBody == nil {
		recordOutcome(false, true)
	}

	writer := newStreamingWriter(w)
	// Send SSE response headers now instead of with the first event, so the
	// client sees the stream open while the upstream is still generating.
	// Native WS turns write frames, not HTTP headers, and are left alone.
	if !websocketResponse && writer.flusher != nil && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		writer.flusher.Flush()
	}
	traceEnabled := s.traceStore.Enabled()
	var capture *limitedCaptureWriter
	if responseCaptureNeeded(traceEnabled, resp.StatusCode, wsBody != nil, websocketResponse) {
		capture = &limitedCaptureWriter{limit: 2 * 1024 * 1024}
	}
	capturedBody := func() []byte {
		if capture == nil {
			return nil
		}
		return capture.Bytes()
	}
	var observer io.Writer
	if capture != nil {
		observer = capture
	}
	var usageReader *usageStream
	observeUsage := responseObservesUsage(resp)
	billingObservation := s.currentUsageRecorder() != nil || s.traceStore != nil
	// The same gate as the request's br removal (usageObservationEnabled).
	if observeUsage && s.usageObservationEnabled() {
		parser := newTargetUsageParser(r, target, upstreamModel, requestBody, resp.Request)
		if config.NormalizeUsageAdapter(parser.usageAdapter) == usageAdapterMinimax && (resp.StatusCode != http.StatusOK || requestSchemaForPath(r.URL.Path) != config.RequestSchemaOpenAIChat) {
			// MiniMax only defines base_resp on HTTP 200 ChatCompletionResp.
			parser.usageAdapter = config.UsageAdapterStandard
		}
		usageReader = newUsageStream(resp.Header.Get("Content-Type"), responseContentEncoding(resp.Header), parser)
		defer usageReader.Close()
		if capture != nil {
			observer = io.MultiWriter(capture, usageReader)
		} else {
			observer = usageReader
		}
	}

	var downstream io.Writer = writer
	var claudeGuard *claudeStreamGuard
	if guardClaudeStream {
		claudeGuard = &claudeStreamGuard{writer: writer}
		downstream = claudeGuard
	}
	_, copyErr := copyClientFirst(downstream, resp.Body, observer)
	if copyErr == nil && claudeGuard != nil {
		copyErr = claudeGuard.flush()
	}
	claudeOverlap := errors.Is(copyErr, errClaudeStreamOverlap)
	// A slow or vanished client is not upstream health evidence.
	clientWriteFailed := copyErr != nil && !claudeOverlap && (writer.downstreamWriteFailed() || (claudeGuard != nil && claudeGuard.downstreamFailure))
	if wsBody != nil && wsBody.local {
		// The proxy answered without contacting the upstream: release a held
		// half-open probe but leave the breaker streak untouched.
		state.releaseProbe(lease)
		s.metrics.totalFailures.Add(1)
	} else if wsBody != nil {
		if copyErr != nil {
			wsBody.failed.Store(true)
			if errors.Is(copyErr, context.Canceled) || errors.Is(copyErr, context.DeadlineExceeded) || r.Context().Err() != nil || clientWriteFailed {
				wsBody.noMute.Store(true)
			}
		}
		status, errorBody := wsBody.outcome()
		unhealthy := !wsBody.noMute.Load() || accountLevelFailure
		if !wsBody.generationSent {
			// A rejected upgrade already went through the key-pool loop, which
			// marked the key and notified admission control; key-level failures
			// stay off the target unless every key failed the same way.
			if state.keyPool != nil && keyIndex >= 0 && state.keyPool.exhaustReasonAt(keyIndex) != "" {
				unhealthy = accountLevelFailure
			}
		} else if s.observeResponsesKeyFailure(state, target, upstreamModel, ResolveProtocol(r.URL.Path), keyIndex, status, errorBody) {
			unhealthy = (copyErr != nil && !wsBody.noMute.Load()) || accountLevelFailure
		}
		// Streaming headers may already be committed, but accounting and traces
		// must retain the terminal's actual failure status.
		if status >= 400 {
			resp.StatusCode = status
		}
		recordOutcome(wsBody.failed.Load(), unhealthy)
	}
	if websocketResponse && nativeRejection != nil {
		websocketSession.turnErrorBody = nativeRejection.envelope
		recordOutcome(true, nativeRejection.unhealthy || accountLevelFailure)
	} else if websocketResponse {
		if websocketSession.httpFallback && resp.StatusCode >= 400 {
			websocketSession.turnErrorBody = capturedBody()
		}
		// The native frame is already delivered. Keep its deterministic error
		// status for usage, diagnostics and traces rather than the SSE adapter's
		// synthetic 200. Incomplete responses without an HTTP status stay 200.
		if websocketSession.turnStatus >= 400 {
			resp.StatusCode = websocketSession.turnStatus
		}
		switch {
		case errors.Is(copyErr, context.Canceled):
			s.metrics.totalClientCanceled.Add(1)
		case errors.Is(copyErr, context.DeadlineExceeded):
			websocketSession.turnStatus = http.StatusGatewayTimeout
			resp.StatusCode = http.StatusGatewayTimeout
			s.metrics.totalFailures.Add(1)
		default:
			status := websocketSession.turnStatus
			upstreamCopyFailed := copyErr != nil && !clientWriteFailed
			unhealthy := upstreamCopyFailed || isUpstreamFailureStatus(status)
			// Observe key-specific failures once, after sending. Never replay a
			// WS create: only the next client turn may choose another key.
			if s.observeResponsesKeyFailure(state, target, upstreamModel, ResolveProtocol(r.URL.Path), keyIndex, status, websocketSession.turnErrorBody) {
				unhealthy = upstreamCopyFailed
			}
			recordOutcome(websocketSession.turnFailed || copyErr != nil, unhealthy)
		}
	}
	if claudeOverlap {
		s.logger.Warn("claude stream overlapping message_start",
			"request_id", appmiddleware.RequestIDFromContext(r.Context()),
			"target", targetName(target),
			"open_message_id", claudeGuard.openID,
			"new_message_id", claudeGuard.newID,
		)
	} else if err := copyErr; err != nil &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		s.logger.Warn("stream copy failed",
			"request_id", appmiddleware.RequestIDFromContext(r.Context()),
			"target", targetName(target),
			"error", errorlog.ErrorCategory(err),
		)
	}

	var tokens usageTokens
	if usageReader != nil {
		usageReader.Close()
		tokens = usageReader.tokens
		if !usageReader.found || copyErr != nil || usageReader.err != nil {
			tokens.status = "incomplete"
			tokens.costIncomplete = true
			if tokens.diagnostic == "" {
				tokens.diagnostic = "missing_usage_or_interrupted_response"
			}
		}
		if claudeOverlap {
			tokens.diagnostic = "claude_stream_overlapping_message"
		}
		responseModel := usageReader.model
		// Without official final usage (client left, upstream interrupted,
		// observation failed, ...) settle through the shared rule. The
		// upstream is already canceled or finished; nothing is replayed.
		if cause := responseNoUsageCause(r, usageReader, copyErr, clientWriteFailed, claudeOverlap); cause != "" && !(wsBody != nil && wsBody.local) {
			settle := noOfficialUsage{cause: cause, usage: usageReader, tokens: tokens, model: upstreamModel, transport: clientGoneTransportHTTP, phase: clientGonePhaseStreaming}
			if usageReader.streaming {
				settle.transport = clientGoneTransportSSE
			}
			if wsBody != nil || (websocketResponse && !websocketSession.httpFallback && nativeRejection == nil) {
				settle.transport = clientGoneTransportWS
			}
			if websocketResponse {
				settle.phase = clientGonePhaseWSTurn
			}
			tokens, responseModel = s.settleNoOfficialUsage(r, target, settle)
		}
		if billingObservation {
			s.recordParsedUsage(r, target, resp.StatusCode, upstreamModel, tokens, responseModel, state, keyIndex)
		}
	}

	// 上游 4xx/5xx：写结构化错误日志（旁路 access/error log，便于事后 grep）。
	// 触发条件覆盖 4xx 与 5xx 完整范围；用户主动 abort 由 io.Copy err 分支拦截不入此路径。
	if resp.StatusCode >= 400 {
		writeUpstreamErrorLog(r, target, resp, capturedBody(), startedAt)
	}

	// DEBUG 模式：记录完整 trace（请求/响应 META + body）
	if traceEnabled {
		s.recordTrace(r, target, resp, requestBody, capturedBody(), startedAt, keyIndex, upstreamModel, tokens)
	}
	lifecycleFailed := accountLevelFailure
	if usageReader != nil {
		// Missing usage is deliberately not a request failure. SSE requires a
		// terminal even when EOF is clean at an event boundary.
		lifecycleFailed = lifecycleFailed || (usageReader.streaming && (!usageReader.terminal || usageReader.err != nil)) || usageReader.responseStatus == "failed" || usageReader.responseStatus == "incomplete" || usageReader.responseStatus == "blocked"
	}
	if wsBody != nil {
		lifecycleFailed = lifecycleFailed || wsBody.local || wsBody.failed.Load()
	}
	if websocketResponse {
		lifecycleFailed = lifecycleFailed || websocketSession.turnFailed
		if nativeWriter, ok := w.(*responsesWebsocketWriter); ok {
			lifecycleFailed = lifecycleFailed || !nativeWriter.terminal
		}
	}
	return responseCompletion(r, resp.StatusCode, copyErr, clientWriteFailed, lifecycleFailed)
}

// Observe a Responses key failure without replaying the current generation.
// Both native WS and the HTTP adapter use the same provider-aware ordering.
func (s *Service) observeResponsesKeyFailure(state *targetState, target *Target, model string, protocol config.ProtocolType, keyIndex, status int, body []byte) bool {
	if state.keyPool == nil || keyIndex < 0 {
		return false
	}
	if exhausted, code := targetKeyExhausted(target, protocol, status, body); exhausted {
		state.keyPool.markExhaustedWithError(keyIndex, code, string(body))
		return true
	}
	if status != http.StatusTooManyRequests {
		return false
	}
	if usesGLMErrorPatch(target) {
		state.keyPool.markRateLimitedWithBody(keyIndex, body)
	} else {
		state.keyPool.markExhaustedWithError(keyIndex, "rate_limited", string(body))
	}
	if am := s.admissionManager.Load(); am != nil {
		am.OnLimit(target.Name)
	}
	return true
}

// writeUpstreamErrorLog 在 writeResponse 写完响应体后落一条 NDJSON 错误日志。
// Body bytes are counted for diagnostics but never stored in default logs.
func writeUpstreamErrorLog(r *http.Request, target *Target, resp *http.Response, body []byte, startedAt time.Time) {
	kind := errorlog.KindUpstream4xx
	if resp.StatusCode >= 500 {
		kind = errorlog.KindUpstream5xx
	}

	entry := errorlog.Entry{
		TraceID:        appmiddleware.RequestIDFromContext(r.Context()),
		Kind:           kind,
		Method:         r.Method,
		Path:           r.URL.Path,
		ClientIP:       clientIP(r),
		UpstreamStatus: resp.StatusCode,
		DurationMS:     time.Since(startedAt).Milliseconds(),
		ReqBytes:       int(r.ContentLength),
		RespBytes:      len(body),
	}
	if len(body) > 0 {
		entry.RespExcerpt = "[redacted]"
	}
	if target != nil {
		entry.Target = target.Name
		entry.EndpointType = target.EndpointType
		if target.Endpoint != nil {
			entry.UpstreamURL = stripURLQuery(target.Endpoint.String())
		}
	}
	// The errorlog sink removes userinfo and query from the actual request URL.
	if resp != nil && resp.Request != nil && resp.Request.URL != nil {
		entry.UpstreamFullURL = resp.Request.URL.String()
	}
	errorlog.Write(entry)
}

// recordTrace 记录完整的请求/响应 trace（DEBUG 模式）。
// recordTrace 记录完整的请求/响应 trace（DEBUG 模式）。
// upstreamModel 必须是真实的上游模型名（非客户端 alias），确保 TraceRecord.Model
// 存储的是上游实际模型名，便于事后排查时与上游日志对齐。
func (s *Service) recordTrace(r *http.Request, target *Target, resp *http.Response, requestBody, responseBody []byte, startedAt time.Time, keyIndex int, upstreamModel string, tokens usageTokens) {
	// A disabled or closed store keeps nothing: skip copying bodies and headers.
	if !s.traceStore.Enabled() {
		return
	}

	principal, _ := auth.PrincipalFromContext(r.Context())
	clientName := ""
	clientAccessKey := ""
	if principal != nil {
		clientName = principal.Name
		clientAccessKey = auth.MaskClientKey(principal.AccessKey)
	}

	// 收集请求头（脱敏）
	requestHeaders := make(map[string]string)
	for key := range r.Header {
		if isSensitiveTraceHeader(key, false) {
			requestHeaders[key] = "***"
		} else {
			requestHeaders[key] = r.Header.Get(key)
		}
	}

	// 收集上游响应头（脱敏）
	upstreamHeaders := make(map[string]string)
	if resp != nil {
		for key := range resp.Header {
			if isSensitiveTraceHeader(key, true) {
				upstreamHeaders[key] = "***"
			} else {
				upstreamHeaders[key] = resp.Header.Get(key)
			}
		}
	}

	// 构建上游 URL（脱敏 query 中的敏感参数）
	upstreamURL := ""
	if resp != nil && resp.Request != nil && resp.Request.URL != nil {
		upstreamURL = sanitizeTraceURLQuery(resp.Request.URL)
	}

	// 提取 key mask
	keyMask := ""
	if target != nil && keyIndex >= 0 {
		keys := target.APIKeys
		if len(keys) == 0 && target.APIKey != "" {
			keys = []string{target.APIKey}
		}
		if keyIndex < len(keys) {
			keyMask = maskAPIKey(keys[keyIndex])
		}
	}

	// Decode only the captured observation; forwarding keeps original bytes.
	respBodyStr := string(responseBody)
	if resp != nil && len(responseBody) > 0 {
		encoding := strings.TrimSpace(responseContentEncoding(resp.Header))
		// Identity already holds readable bytes. Store.Record owns its limit
		// semantics, including nonpositive values meaning no truncation.
		if encoding != "" && !strings.EqualFold(encoding, "identity") {
			if decoded, err := observationReader(bytes.NewReader(responseBody), encoding); err == nil {
				body, readErr := io.ReadAll(io.LimitReader(decoded, int64(s.traceStore.MaxBodySize())))
				_ = decoded.Close()
				// Captures can end mid-frame. Preserve readable partial output.
				if len(body) > 0 || readErr == nil {
					respBodyStr = string(body)
				}
			}
		}
	}

	record := &tracestore.TraceRecord{
		// META: 请求侧
		TraceID:         appmiddleware.RequestIDFromContext(r.Context()),
		Timestamp:       startedAt,
		ClientName:      clientName,
		ClientIP:        clientIP(r),
		ClientAccessKey: clientAccessKey,
		Method:          r.Method,
		Path:            r.URL.Path,
		QueryParams:     sanitizeTraceRawQuery(r.URL.RawQuery),
		RequestHeaders:  requestHeaders,

		// META: 路由决策
		Target:       targetName(target),
		EndpointType: targetEndpointType(target),
		Model:        upstreamModel,
		KeyIndex:     keyIndex,
		KeyMask:      keyMask,

		// META: 上游
		UpstreamURL:       upstreamURL,
		UpstreamRequestID: upstreamResponseRequestID(resp.Header),
		UpstreamStatus:    resp.StatusCode,
		UpstreamHeaders:   upstreamHeaders,

		// META: 结果
		StatusCode:          resp.StatusCode,
		DurationMS:          time.Since(startedAt).Milliseconds(),
		InputTokens:         tokens.InputTokens,
		OutputTokens:        tokens.OutputTokens,
		CachedTokens:        tokens.CachedTokens,
		CacheCreationTokens: tokens.CacheCreationTokens,

		// 内容
		RequestBody:  string(requestBody),
		ResponseBody: respBodyStr,
	}

	s.traceStore.Record(record)
}

func isSensitiveTraceHeader(key string, includeSetCookie bool) bool {
	switch strings.ToLower(key) {
	case "authorization", "api-key", "x-api-key", "x-goog-api-key", "x-azure-authorization", "proxy-authorization", "cookie":
		return true
	case "set-cookie":
		return includeSetCookie
	default:
		return false
	}
}

func sanitizeTraceRawQuery(raw string) string {
	if raw == "" {
		return ""
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return sanitizeTraceInvalidRawQuery(raw)
	}
	for param := range q {
		if isSensitiveTraceQuery(param) {
			q.Set(param, "***")
		}
	}
	return q.Encode()
}

func sanitizeTraceInvalidRawQuery(raw string) string {
	parts := strings.Split(raw, "&")
	for i, part := range parts {
		key, _, _ := strings.Cut(part, "=")
		if isSensitiveTraceQuery(key) || isSensitiveTraceQuery(decodedTraceQueryKey(key)) {
			parts[i] = key + "=***"
		}
	}
	return strings.Join(parts, "&")
}

func decodedTraceQueryKey(key string) string {
	decoded, err := url.QueryUnescape(key)
	if err != nil {
		return key
	}
	return decoded
}

func sanitizeTraceURLQuery(u *url.URL) string {
	if u == nil {
		return ""
	}
	copy := *u
	copy.RawQuery = sanitizeTraceRawQuery(copy.RawQuery)
	return copy.String()
}

func isSensitiveTraceQuery(key string) bool {
	switch strings.ToLower(key) {
	case "api-key", "api_key", "apikey", "key", "access_token", "token":
		return true
	default:
		return false
	}
}

// targetEndpointType 返回 target 的 endpoint type。
func targetEndpointType(target *Target) string {
	if target == nil {
		return ""
	}
	return target.EndpointType
}

// recordUsageEvent 记录用量事件。
// upstreamModel 必须是已解析的上游模型名（来自 ensureModelAllowed 或 writeResponse 透传），
// 不再调用 catalog.ResolveAlias 做二次规范化——调用方已保证传入的是真实上游名。
// 仅当 caller 传入空串时（极端边界），以响应体中的 model 字段作为兜底。
func (s *Service) recordUsageEvent(r *http.Request, target *Target, statusCode int, upstreamModel string, contentType string, body []byte, state *targetState, keyIndex int) usageTokens {
	parser := newTargetUsageParser(r, target, upstreamModel, nil, nil)
	tokens, parsedModel, found := parser.extractTokens(contentType, body)
	if found {
		s.recordParsedUsage(r, target, statusCode, upstreamModel, tokens, parsedModel, state, keyIndex)
	}
	return tokens
}

// Key balancing measures observed work, including nonbillable attempts. Each
// partition is counted once; reasoning, TTLs and image details are subsets.
func reportedBundleTokenLoad(tokens usageTokens) int64 {
	var total int64
	add := func(counts ...int64) {
		for _, n := range counts {
			if n > math.MaxInt64-total {
				total = math.MaxInt64
				return
			}
			if n > 0 {
				total += n
			}
		}
	}
	add(tokens.InputTokens, tokens.OutputTokens, tokens.CachedTokens, tokens.CacheCreationTokens, tokens.ToolUsePromptTokens)
	for _, c := range tokens.components {
		add(c.InputTokens, c.OutputTokens, c.CachedTokens, c.CacheCreationTokens, c.ToolUsePromptTokens)
	}
	return total
}

func (s *Service) recordParsedUsage(r *http.Request, target *Target, statusCode int, upstreamModel string, tokens usageTokens, parsedModel string, state *targetState, keyIndex int) {
	recorder := s.currentUsageRecorder()
	if recorder == nil || r == nil {
		return
	}
	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok || principal == nil {
		return
	}
	endpointType := targetEndpointType(target)

	// 回写 token 用量到 keyPool，用于 token 感知调度
	if state != nil && state.keyPool != nil && keyIndex >= 0 {
		totalTokens := reportedBundleTokenLoad(tokens)
		state.keyPool.recordTokens(keyIndex, totalTokens)
	}

	if s.logger != nil && (len(tokens.unadmittedEchoes) != 0 || tokens.unadmittedDropped != 0) {
		requestID := appmiddleware.RequestIDFromContext(r.Context())
		targetName := ""
		if target != nil {
			targetName = target.Name
		}
		// Attribution keeps the echoed spelling and never infers an identity.
		// The parser bounds and deduplicates these per request.
		for _, echo := range tokens.unadmittedEchoes {
			s.logger.Warn("回显附属模型不在准入集合内 / echoed supplementary model not in admission set",
				"request_id", requestID,
				"target", targetName,
				"iteration_type", echo.iterationType,
				"model", echo.model,
			)
		}
		if tokens.unadmittedDropped != 0 {
			s.logger.Warn("回显附属模型超出单请求日志上限，已汇总 / echoed supplementary models over per-request log limit; summarized",
				"request_id", requestID,
				"target", targetName,
				"logged", len(tokens.unadmittedEchoes),
				"dropped", tokens.unadmittedDropped,
			)
		}
	}

	if tokens.billingModel != "" {
		upstreamModel = tokens.billingModel
	}
	upstreamModel = strings.ToLower(strings.TrimSpace(upstreamModel))
	if upstreamModel == "" {
		upstreamModel = parsedModel // 仅在 caller 未传时兜底到响应体中的 model
	}

	evt := usage.Event{
		Components:              tokens.components,
		IterationType:           tokens.iterationType,
		IterationIndex:          tokens.iterationIndex,
		NonBillable:             tokens.nonBillable,
		Timestamp:               time.Now().UTC(),
		PricingContext:          tokens.pricingContext,
		ClientName:              principal.Name,
		EndpointType:            endpointType,
		Model:                   upstreamModel,
		InputTokens:             tokens.InputTokens,
		OutputTokens:            tokens.OutputTokens,
		CachedTokens:            tokens.CachedTokens,
		CacheCreationTokens:     tokens.CacheCreationTokens,
		ExplicitCacheReadTokens: tokens.ExplicitCacheReadTokens,
		ReasoningTokens:         tokens.ReasoningTokens,
		CacheCreation5mTokens:   tokens.CacheCreation5mTokens,
		CacheCreation1hTokens:   tokens.CacheCreation1hTokens,
		ToolUsePromptTokens:     tokens.ToolUsePromptTokens,
		ImageInputTokens:        tokens.ImageInputTokens,
		CachedImageInputTokens:  tokens.CachedImageInputTokens,
		ImageOutputTokens:       tokens.ImageOutputTokens,
		ObservedFields:          uint16(tokens.fields),
		UsageStatus:             tokens.status,
		UsageAdapter:            tokens.adapter,
		UsageProtocol:           string(ResolveProtocol(r.URL.Path)),
		ResponseModel:           parsedModel,
		CostIncomplete:          tokens.costIncomplete,
		UsageDiagnostic:         tokens.diagnostic,
		ResponseStatus:          tokens.responseStatus,
		FinishReason:            tokens.finishReason,
		RequestID:               appmiddleware.RequestIDFromContext(r.Context()),
		StatusCode:              statusCode,
		Path:                    r.URL.Path,
	}
	if target != nil {
		evt.Target = target.Name
	}

	var recordErr error
	if qm := s.quotaManager.Load(); qm != nil {
		recordErr = qm.RecordUsage(recorder, evt)
	} else {
		usage.CheckBundlePrices(&evt, nil)
		recordErr = recorder.Record(evt)
	}
	if recordErr != nil {
		s.metrics.totalUsageRecordFailures.Add(1)
		s.logger.Error("usage accounting could not be persisted; inspect the usage database and recovery journal",
			"request_id", appmiddleware.RequestIDFromContext(r.Context()),
			"client", principal.Name,
			"error", recordErr,
		)
	}
}

func (s *Service) handleForwardError(r *http.Request, state *targetState, err error, status int) {
	if status == 0 {
		status = http.StatusServiceUnavailable // 默认服务不可用（5xx），非网关错误
	}
	state.MarkFailure(time.Now(), s.quietPeriod)
	stats := state.Stats()

	s.logger.Warn("forward request failed",
		"request_id", appmiddleware.RequestIDFromContext(r.Context()),
		"target", state.Target().Name,
		"error", errorlog.ErrorCategory(err),
		"status", status,
		"consecutive_failures", stats.ConsecutiveFailure,
		"total_failures", stats.TotalFailure,
		"muted_until", stats.MutedUntil,
	)
}

// writeForwardErrorLog 在 forwardRequest 失败（网络错误 / 配置错误 / Azure 缺凭据 等）时落错误日志。
// 由 service.go 在拿到 forwardAttemptError 后统一调用，传入完整 startedAt / upstreamURL。
func writeForwardErrorLog(r *http.Request, state *targetState, fe *forwardAttemptError) {
	if fe == nil {
		return
	}
	target := state.Target()

	entry := errorlog.Entry{
		TraceID:         appmiddleware.RequestIDFromContext(r.Context()),
		Kind:            errorlog.KindUpstreamNetErr,
		Method:          r.Method,
		Path:            r.URL.Path,
		ClientIP:        clientIP(r),
		UpstreamStatus:  0, // net error 时上游无响应
		UpstreamURL:     fe.upstreamURL,
		UpstreamFullURL: fe.upstreamFullURL,
		ReqBytes:        int(r.ContentLength),
	}
	if !fe.startedAt.IsZero() {
		entry.DurationMS = time.Since(fe.startedAt).Milliseconds()
	}
	if fe.err != nil {
		entry.Error = errorlog.ErrorCategory(fe.err)
	}
	if target != nil {
		entry.Target = target.Name
		entry.EndpointType = target.EndpointType
	}
	errorlog.Write(entry)
}

// clientIP extracts the client IP from the request.
// Delegates to the shared middleware.ClientIP implementation to avoid duplication.
func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	return appmiddleware.ClientIP(r)
}

// stripURLQuery 去掉 URL 中的 query string，避免 errorlog 泄露 api key 等查询参数。
func stripURLQuery(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i]
	}
	return raw
}

// responseObservesUsage reports whether writeResponse observes and records
// usage for resp: a 2xx response, or a WS response whose generation was sent.
// Any other response (a passed-through non-2xx) records no usage event.
func responseObservesUsage(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true
	}
	wsBody := responseWSBody(resp)
	return wsBody != nil && wsBody.generationSent
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if _, skip := hopHeaders[key]; skip {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

type streamingWriter struct {
	http.ResponseWriter
	flusher  http.Flusher
	writeErr error
}

// downstreamWriteFailed reports whether the client side rejected a write.
// Writers that also validate upstream content (the native WS adapter) say
// which of their errors came from the client connection.
func (w *streamingWriter) downstreamWriteFailed() bool {
	if reporter, ok := w.ResponseWriter.(interface{ downstreamWriteFailed() bool }); ok {
		return reporter.downstreamWriteFailed()
	}
	return w.writeErr != nil
}

// responseCaptureNeeded reports whether writeResponse keeps the 2 MiB
// head/tail capture. It feeds traces, 4xx/5xx error logs and WS turn error
// bodies. WS outcomes can turn a 200 into a terminal error after copying, so
// WS responses always capture; other responses below 400 without trace never
// read the capture and skip it.
func responseCaptureNeeded(traceEnabled bool, status int, upstreamWS, nativeWS bool) bool {
	return traceEnabled || status >= 400 || upstreamWS || nativeWS
}

// copyBufferPool recycles the 32 KiB response copy buffers (io.Copy's size).
var copyBufferPool = sync.Pool{New: func() any {
	buffer := make([]byte, 32*1024)
	return &buffer
}}

// copyClientFirst copies src to the client and then hands every chunk to the
// observer (usage/capture), so observation never delays bytes to the client.
// The observer still receives a chunk whose client write failed: usage it
// carries stays known. Copy semantics (short writes, errors) match io.Copy.
func copyClientFirst(dst io.Writer, src io.Reader, observer io.Writer) (written int64, err error) {
	bufferPointer := copyBufferPool.Get().(*[]byte)
	defer copyBufferPool.Put(bufferPointer)
	buffer := *bufferPointer
	for {
		nr, readErr := src.Read(buffer)
		if nr > 0 {
			nw, writeErr := dst.Write(buffer[:nr])
			if nw < 0 || nr < nw {
				nw = 0
				if writeErr == nil {
					writeErr = errors.New("invalid write result")
				}
			}
			written += int64(nw)
			if observer != nil {
				if _, observeErr := observer.Write(buffer[:nr]); observeErr != nil && writeErr == nil {
					return written, observeErr
				}
			}
			if writeErr != nil {
				return written, writeErr
			}
			if nr != nw {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return written, nil
			}
			return written, readErr
		}
	}
}

func newStreamingWriter(w http.ResponseWriter) *streamingWriter {
	sw := &streamingWriter{ResponseWriter: w}
	if fl, ok := w.(http.Flusher); ok {
		sw.flusher = fl
	}
	return sw
}

func (w *streamingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err != nil {
		w.writeErr = err
	} else if w.flusher != nil {
		w.flusher.Flush()
	}
	return n, err
}

// StatusClientClosedRequest 表示客户端在响应返回前主动断开连接（nginx 惯例码 499）。
// Go 标准库未定义此码；代理用它把"客户端断连"与"上游真超时(504)"区分开，
// 避免下游断开被误记为代理/上游故障（多跳代理链中尤其关键）。
const StatusClientClosedRequest = 499

// isPreSendTransportError reports a network failure that provably happened
// before the request was written: a dial error or net/http's TLS handshake
// timeout. ctx is the attempt context (derived from the parent request): when
// it has ended, the parent's cancellation or deadline (or the attempt
// timeout) caused the failure, which never qualifies (#163). Otherwise a dial
// error qualifies even when it matches context.DeadlineExceeded, as the
// dialer's own timeout does (#156).
func isPreSendTransportError(ctx context.Context, err error) bool {
	if err == nil || (ctx != nil && ctx.Err() != nil) || errors.Is(err, context.Canceled) {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return strings.Contains(err.Error(), "TLS handshake timeout")
}

func classifyTransportError(err error) int {
	// 客户端主动断开（context.Canceled）：上游链路本身没超时，不该记 504/502。
	// 注意：per-attempt ctx 派生自 r.Context()，客户端 TCP 断开会触发 Canceled；
	// 代理自身 deadline 到期是 DeadlineExceeded，走下面的 504 分支。
	if errors.Is(err, context.Canceled) {
		return StatusClientClosedRequest
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return http.StatusGatewayTimeout
	}

	return http.StatusBadGateway
}

func targetName(t *Target) string {
	if t == nil {
		return ""
	}
	return t.Name
}

// replaceModelInURLPath matches the URL model segment case-insensitively and
// replaces it with the exact configured upstream spelling. It recognizes
// Azure's /deployments/{model}/ shape and Gemini's /models/{model}: shape.
// Other paths are returned unchanged.
func replaceModelInURLPath(path, upstream, model string) string {
	if upstream == model || path == "" {
		return path
	}
	pathLower := strings.ToLower(path)

	// Azure: /openai/deployments/{model}/...
	const deploymentsSegment = "/deployments/"
	if idx := strings.Index(pathLower, deploymentsSegment); idx >= 0 {
		modelStart := idx + len(deploymentsSegment)
		rest := path[modelStart:]
		restLower := pathLower[modelStart:]
		slashIdx := strings.Index(restLower, "/")
		var seg string
		if slashIdx < 0 {
			seg = rest
		} else {
			seg = rest[:slashIdx]
		}
		if len(seg) > 0 && strings.EqualFold(seg, model) {
			// Preserve any query/fragment after segment: rebuild up to modelStart+slashIdx
			if slashIdx < 0 {
				return path[:modelStart] + upstream
			}
			return path[:modelStart] + upstream + path[modelStart+slashIdx:]
		}
	}

	// Gemini: /v1beta/models/{model}:{action} (also /v1alpha/models/, /v1/models/)
	const modelsSegment = "/models/"
	if idx := strings.Index(pathLower, modelsSegment); idx >= 0 {
		modelStart := idx + len(modelsSegment)
		rest := path[modelStart:]
		restLower := pathLower[modelStart:]
		colonIdx := strings.Index(restLower, ":")
		slashIdx := strings.Index(restLower, "/")
		endIdx := len(rest)
		if colonIdx >= 0 && (slashIdx < 0 || colonIdx < slashIdx) {
			endIdx = colonIdx
		} else if slashIdx >= 0 {
			endIdx = slashIdx
		}
		seg := rest[:endIdx]
		if len(seg) > 0 && strings.EqualFold(seg, model) {
			return path[:modelStart] + upstream + path[modelStart+endIdx:]
		}
	}

	return path
}

// isUpstreamFailureStatus 判定上游响应状态码是否应触发 MarkFailure（进而 mute + 下次请求 fallback）。
//
// 触发条件：
//   - 5xx：上游服务端错误（502/503/504 等），通用故障切换的信号；
//   - 429：上游过载/限流（OpenAI/Azure 在模型容量瓶颈时常返回此码，含 "Engine is overloaded"）；
//   - 408：上游请求超时（典型见于网宿/Azure 长耗时图像生成接口）。
//
// 4xx 其他状态（400/401/403/404 等）属于客户端请求问题，不切换 target。
func isUpstreamFailureStatus(statusCode int) bool {
	if statusCode >= 500 {
		return true
	}
	switch statusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return false
}
