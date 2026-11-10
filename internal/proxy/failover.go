// failover.go — unified non-2xx failover policy.
//
// The cost router already orders candidates by estimated price. This file adds
// the missing half of that contract: a candidate that provably did not process
// the request may be dequeued and the next (possibly more expensive) candidate
// is tried. Everything that may already have produced upstream work stays a
// terminal response — replaying it could duplicate inference and billing.
package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

// maxCachedUpstreamErrorBody bounds the bytes retained from an upstream 4xx so
// a single request can never retain an unbounded error page. A response above
// the limit is delivered as-is instead of being dequeued.
const maxCachedUpstreamErrorBody = 64 << 10

type failoverAction int

const (
	failoverTerminate failoverAction = iota
	failoverDequeue
)

// classifyNon2xxAttempt is the single policy entry point for upstream HTTP
// failures. It answers only one question: may this attempt be abandoned and
// the next cost-ordered candidate be tried?
//
//	allowFallback=false  explicit target: never cross targets.
//	status < 400          success: not this path.
//	status >= 500        may already have been computed and billed: terminal.
//	401/402/429          keypool scope: the key loop already handled them.
//	408                  may follow a fully sent request: terminal.
//	other 4xx            upstream rejected without processing: dequeue.
func classifyNon2xxAttempt(status int, allowFallback bool) failoverAction {
	if !allowFallback || status < 400 || status >= 500 {
		return failoverTerminate
	}
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden,
		http.StatusTooManyRequests, http.StatusRequestTimeout:
		// 401/402/429 belong to the keypool state machine. 403 is an
		// account/permission rejection: replaying it across candidates can
		// amplify a client-side authorization failure and trip account locks.
		return failoverTerminate
	}
	return failoverDequeue
}

// deterministicClientErrorCodes list upstream error codes that describe the
// request body itself. Every candidate rejects them identically, so crossing
// targets only repeats the same 4xx. Only codes with sample evidence belong
// here; target-specific 4xx must stay retryable.
var deterministicClientErrorCodes = map[string]struct{}{
	"invalid_function_parameters": {},
}

// deterministicUpstreamClientError decodes the controlled error envelope and
// reports a whitelisted deterministic client error code. An unparseable body
// is not proven deterministic and keeps the existing dequeue behaviour.
func deterministicUpstreamClientError(resp *http.Response, body []byte) (string, bool) {
	fields, ok, _ := upstreamErrorDiagnostics(resp, body)
	if !ok {
		return "", false
	}
	code := strings.ToLower(strings.TrimSpace(fields.Code))
	if _, deterministic := deterministicClientErrorCodes[code]; !deterministic {
		return "", false
	}
	return code, true
}

// cachedUpstreamError is the terminal 4xx response of one attempt. It is kept
// per client request and replayed only when every candidate has been tried.
// Header and body must be replayed together: mixing the fields of different
// attempts would silently corrupt the response.
type cachedUpstreamError struct {
	status            int
	header            http.Header
	body              []byte
	target            *Target
	upstreamModel     string
	upstreamRequestID string
}

// readUpstreamErrorForCache reads at most maxCachedUpstreamErrorBody+1 bytes.
// The returned read bytes always belong to the caller: when ok is false the
// caller must restore resp.Body with them before handing the response to
// writeResponse. That keeps an oversized or unreadable error terminal.
func readUpstreamErrorForCache(resp *http.Response, target *Target, upstreamModel string) (cached *cachedUpstreamError, read []byte, ok bool) {
	if resp == nil || resp.Body == nil {
		return nil, nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCachedUpstreamErrorBody+1))
	if err != nil || len(body) > maxCachedUpstreamErrorBody {
		return nil, body, false
	}
	return &cachedUpstreamError{
		status:            resp.StatusCode,
		header:            resp.Header.Clone(),
		body:              body,
		target:            target,
		upstreamModel:     upstreamModel,
		upstreamRequestID: upstreamResponseRequestID(resp.Header),
	}, body, true
}

// writeCachedUpstreamError replays the first dequeued 4xx verbatim, including
// its original encoding headers and request-id, so the client sees the real
// upstream error rather than a synthesized proxy status.
func writeCachedUpstreamError(w http.ResponseWriter, cached *cachedUpstreamError) {
	if cached == nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	for key, values := range cached.header {
		if _, skip := hopHeaders[key]; skip {
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	if cached.target != nil {
		w.Header().Set("X-Proxy-Target", cached.target.Name)
		w.Header().Set("X-Azure-Target", cached.target.Name)
		if cached.target.Pool != "" {
			w.Header().Set(headerProxyPool, cached.target.Pool)
		}
	}
	if cached.upstreamRequestID != "" {
		w.Header().Set("X-Upstream-Request-Id", cached.upstreamRequestID)
	}
	w.WriteHeader(cached.status)
	_, _ = w.Write(cached.body)
}

// replayReadCloser restores a partially read body while keeping the original
// closer, so the node/key occupancy release attached to the upstream body is
// still invoked exactly once.
type replayReadCloser struct {
	io.Reader
	io.Closer
}

func restoreUpstreamErrorBody(resp *http.Response, read []byte) {
	if resp == nil || resp.Body == nil {
		return
	}
	resp.Body = &replayReadCloser{
		Reader: io.MultiReader(bytes.NewReader(read), resp.Body),
		Closer: resp.Body,
	}
}
