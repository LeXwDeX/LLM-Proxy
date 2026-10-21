// no_official_usage.go — protocol-independent usage settlement for a request
// that may have reached the upstream but ended without the protocol's official
// final usage.
//
// Interruptions stay transparent: a client cancellation cancels the upstream
// at once (the attempt context is derived from r.Context()), and nothing is
// ever retried, replayed or failed over because of this settlement. It only
// decides what the attempt's single usage event says:
//
//   - official final usage observed: record it as is. If the client leaves
//     or the upstream is interrupted afterwards, the official snapshot is
//     still recorded as a complete event, marked
//     client_disconnected_after_final_usage or interrupted_after_final_usage
//     (no zero-usage log or counter);
//   - otherwise every token is 0 (partial values are not official), status
//     incomplete, cost incomplete, no pricing context, and the existing
//     specific diagnostic gains a cause marker:
//     client_disconnected_no_usage when the client left, otherwise
//     upstream_interrupted_no_usage; plus one Warn log and a counter;
//   - unchanged: a complete response that carries no usage (missing_usage),
//     official usage with a value conflict, non-2xx rejections, and requests
//     provably never sent (queueing, dial, TLS) which record nothing.
//
// "Official final usage" is read from usageStream's found/finalUsage/terminal
// state (usageStream.officialFinalUsage).
package proxy

import (
	"compress/flate"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	appmiddleware "github.com/LeXwDeX/LLM-Proxy/internal/middleware"
)

const (
	diagnosticClientDisconnectedNoUsage = "client_disconnected_no_usage"
	// diagnosticUpstreamInterruptedNoUsage: the upstream response ended, for a
	// reason other than the client leaving, before official final usage.
	diagnosticUpstreamInterruptedNoUsage = "upstream_interrupted_no_usage"
)

// Causes of a missing official final usage (the log "cause" field).
const (
	noUsageCauseClientGone        = "client_gone"
	noUsageCauseUpstreamError     = "upstream_error"
	noUsageCauseTimeout           = "timeout"
	noUsageCauseObservationFailed = "observation_failed"
	noUsageCauseMissingTerminal   = "missing_terminal"
	noUsageCauseMissingFinalUsage = "missing_final_usage"
	noUsageCauseClaudeOverlap     = "claude_overlap"
)

// Where the response ended, for the settlement log.
const (
	clientGonePhaseBeforeHeaders = "before_headers"
	clientGonePhaseStreaming     = "streaming"
	clientGonePhaseWSTurn        = "ws_turn"
)

// Upstream transport of the interrupted generation.
const (
	clientGoneTransportHTTP = "http"
	clientGoneTransportSSE  = "sse"
	clientGoneTransportWS   = "ws"
)

// noOfficialUsage describes one attempt that ended without official usage.
type noOfficialUsage struct {
	cause string
	// usage is the closed response observer, nil when no body was observed.
	usage *usageStream
	// tokens carries the existing path's metadata (adapter, diagnostics).
	tokens    usageTokens
	model     string
	transport string
	phase     string
	// willRetry: another attempt follows; log and count, record nothing.
	willRetry bool
	// quiet: already logged and counted (announced as retried earlier).
	quiet bool
	// protocol overrides the log protocol (a pending attempt's own); empty
	// means the request path's protocol.
	protocol config.ProtocolType
}

// pendingNoUsage is a sent attempt that ended before response headers. The
// request loop owns it: a retried attempt is only announced (log + counter),
// and the request's final outcome records at most one usage event.
type pendingNoUsage struct {
	settle    noOfficialUsage
	target    *Target
	state     *targetState
	keyIndex  int
	status    int
	announced bool
	// path/rawPath/protocol are the sent attempt's own request path (after its
	// target's model rewrite). A later attempt may rewrite r.URL.Path for
	// another target, so the settlement never reads r.URL at request end.
	path     string
	rawPath  string
	protocol config.ProtocolType
}

// attemptRequest returns a shallow request copy carrying the pending
// attempt's path, so the event and log describe that attempt. r is unchanged.
func (p *pendingNoUsage) attemptRequest(r *http.Request) *http.Request {
	attempt := r.WithContext(r.Context())
	u := *r.URL
	u.Path, u.RawPath = p.path, p.rawPath
	attempt.URL = &u
	return attempt
}

// announceNoUsageRetry logs and counts a sent attempt that will be retried.
func (s *Service) announceNoUsageRetry(r *http.Request, p *pendingNoUsage) {
	if p == nil || p.announced {
		return
	}
	in := p.settle
	in.willRetry = true
	in.protocol = p.protocol
	s.settleNoOfficialUsage(p.attemptRequest(r), p.target, in)
	p.announced = true
}

// recordPendingNoUsage writes the zero-usage event of the final sent attempt,
// with that attempt's path and protocol.
func (s *Service) recordPendingNoUsage(r *http.Request, p *pendingNoUsage) {
	if p == nil {
		return
	}
	in := p.settle
	in.quiet = p.announced
	in.protocol = p.protocol
	attempt := p.attemptRequest(r)
	tokens, _ := s.settleNoOfficialUsage(attempt, p.target, in)
	s.recordParsedUsage(attempt, p.target, p.status, in.model, tokens, "", p.state, p.keyIndex)
}

// clientGone reports whether the request's own client went away. A parent
// deadline is not a disconnect; it keeps its 504/failure semantics.
func clientGone(r *http.Request) bool {
	return r != nil && errors.Is(r.Context().Err(), context.Canceled)
}

// responseNoUsageCause classifies an observed response after its copy ended.
// It returns "" when the existing tokens stand: an official value conflict,
// official final usage with a clean end, or a response completed normally
// without usage. A client that left, or an interruption, settles even after
// official final usage; settleNoOfficialUsage then records the official
// snapshot with an after_final_usage marker.
//
// A terminal event or frame (completed, failed, incomplete, error) without
// usage is a complete response without usage on every transport: HTTP, the
// upstream WebSocket pool and native WebSocket turns alike. Only an
// interruption without a terminal settles, with its actual cause.
func responseNoUsageCause(r *http.Request, u *usageStream, copyErr error, clientWriteFailed, claudeOverlap bool) string {
	if u == nil || u.finalConflict {
		return ""
	}
	if copyErr != nil && !claudeOverlap && (clientWriteFailed || clientGone(r)) {
		return noUsageCauseClientGone
	}
	if u.finalSeen && (claudeOverlap || (copyErr == nil && u.err != nil) || decodeIntegrityError(copyErr)) {
		// Status quo: the overlap guard keeps its own diagnostic, and an
		// integrity failure (a body that arrived whole but failed observation,
		// or a transport-decoded body with a bad checksum/header/data) is not
		// an interruption; its decoded usage is not promoted to complete.
		// Truncation (unexpected EOF, network errors) is an interruption.
		return ""
	}
	// Bodies that are neither JSON nor SSE never carry observable usage; only
	// a transport interruption is worth settling for them.
	observable := u.streaming || u.jsonResponse
	switch {
	case claudeOverlap:
		return noUsageCauseClaudeOverlap
	case errors.Is(copyErr, context.DeadlineExceeded) || (copyErr != nil && errors.Is(r.Context().Err(), context.DeadlineExceeded)):
		return noUsageCauseTimeout
	case copyErr != nil:
		return noUsageCauseUpstreamError
	case !observable:
		return ""
	case u.err != nil:
		return noUsageCauseObservationFailed
	case u.streaming && !u.terminal:
		return noUsageCauseMissingTerminal
	case u.finalSeen:
		// Official final usage and a clean end: the existing tokens stand.
		return ""
	case u.found:
		return noUsageCauseMissingFinalUsage
	}
	return ""
}

// decodeIntegrityError reports a copy error raised by transport decoding that
// proves the decoded bytes are corrupt (normalizeAutomaticGzip), as opposed
// to a truncated or interrupted stream.
func decodeIntegrityError(err error) bool {
	if err == nil {
		return false
	}
	var corrupt flate.CorruptInputError
	return errors.Is(err, gzip.ErrChecksum) || errors.Is(err, gzip.ErrHeader) || errors.As(err, &corrupt)
}

// transportNoUsageCause classifies an attempt that failed before a response
// body was observed (sent HTTP request or WS create).
func transportNoUsageCause(r *http.Request, err error) string {
	switch {
	case clientGone(r):
		return noUsageCauseClientGone
	case classifyTransportError(err) == http.StatusGatewayTimeout || errors.Is(r.Context().Err(), context.DeadlineExceeded):
		return noUsageCauseTimeout
	}
	return noUsageCauseUpstreamError
}

// settleNoOfficialUsage is the single settlement rule for every path. It
// returns the tokens to record and the parsed response model.
func (s *Service) settleNoOfficialUsage(r *http.Request, target *Target, in noOfficialUsage) (usageTokens, string) {
	if in.usage != nil && in.usage.finalSeen {
		// Interrupted after the official final usage: record the official
		// snapshot. The interruption does not downgrade it (status and cost
		// completeness are the snapshot's own: complete for well-formed
		// official usage); only lifecycle state and a marker are added.
		official := in.usage.finalTokens
		official.responseStatus, official.finishReason = in.tokens.responseStatus, in.tokens.finishReason
		if hasTokenDiagnostic(in.usage.tokens, diagnosticInterruptedAfterFinalUsage) {
			appendTokenDiagnostic(&official, diagnosticInterruptedAfterFinalUsage)
		}
		if in.cause == noUsageCauseClientGone {
			appendTokenDiagnostic(&official, diagnosticClientDisconnectedAfterFinalUsage)
		} else {
			appendTokenDiagnostic(&official, diagnosticInterruptedAfterFinalUsage)
		}
		return official, in.usage.finalModel
	}
	zero := usageTokens{
		status:         usageStatusIncomplete,
		costIncomplete: true,
		adapter:        in.tokens.adapter,
		diagnostic:     in.tokens.diagnostic,
		responseStatus: in.tokens.responseStatus,
		finishReason:   in.tokens.finishReason,
	}
	clientLeft := in.cause == noUsageCauseClientGone
	if clientLeft {
		appendTokenDiagnostic(&zero, diagnosticClientDisconnectedNoUsage)
	} else {
		appendTokenDiagnostic(&zero, diagnosticUpstreamInterruptedNoUsage)
	}
	responseModel := ""
	if in.usage != nil {
		responseModel = in.usage.model
	}
	if in.quiet || s.currentUsageRecorder() == nil {
		return zero, responseModel
	}
	if principal, ok := auth.PrincipalFromContext(r.Context()); !ok || principal == nil {
		return zero, responseModel
	}
	protocol := in.protocol
	if protocol == "" {
		protocol = ResolveProtocol(r.URL.Path)
	}
	fields := []any{
		"request_id", appmiddleware.RequestIDFromContext(r.Context()),
		"target", targetName(target),
		"model", in.model,
		"protocol", string(protocol),
		"transport", in.transport,
		"phase", in.phase,
	}
	if in.willRetry {
		fields = append(fields, "will_retry", true)
	}
	if clientLeft {
		s.metrics.totalUsageClientGoneNoUsage.Add(1)
		s.logger.Warn("连接断开，未获取到计费字段，按 0 token 记录（不重放） / client disconnected before upstream usage was received; recorded zero usage (no replay)", fields...)
	} else {
		s.metrics.totalUsageUpstreamInterruptedNoUsage.Add(1)
		s.logger.Warn("上游中断，未获取到计费字段，按 0 token 记录（不重放） / upstream response ended before official usage was received; recorded zero usage (no replay)", append(fields, "cause", in.cause)...)
	}
	return zero, responseModel
}

// appendTokenDiagnostic keeps existing diagnostics, joined like usage.Event.
func appendTokenDiagnostic(t *usageTokens, reason string) {
	for _, existing := range strings.Split(t.diagnostic, ";") {
		if existing == reason {
			return
		}
	}
	if t.diagnostic != "" {
		t.diagnostic += ";"
	}
	t.diagnostic += reason
}

// generationSendState records whether an HTTP generation may have reached the
// upstream: once a connection is obtained, request bytes may be written.
// Before that (queueing, dial, TLS handshake) a failure or cancellation
// provably sent nothing, matching isPreSendTransportError.
type generationSendState struct{ mayHaveSent atomic.Bool }

type generationSendStateKey struct{}

func generationSendStateFrom(ctx context.Context) *generationSendState {
	state, _ := ctx.Value(generationSendStateKey{}).(*generationSendState)
	return state
}

// doGenerationHTTP sends a generation request over HTTP and marks the
// attempt's send state when a connection is obtained.
func (s *Service) doGenerationHTTP(req *http.Request) (*http.Response, error) {
	if state := generationSendStateFrom(req.Context()); state != nil {
		trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { state.mayHaveSent.Store(true) }}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	}
	return s.doHTTP(req)
}

const (
	// diagnosticClientDisconnectedAfterFinalUsage: the client left after the
	// official final usage was observed; the official values are recorded.
	diagnosticClientDisconnectedAfterFinalUsage = "client_disconnected_after_final_usage"
	// diagnosticInterruptedAfterFinalUsage: the upstream response was
	// interrupted after the official final usage; official values recorded.
	diagnosticInterruptedAfterFinalUsage = "interrupted_after_final_usage"
)
