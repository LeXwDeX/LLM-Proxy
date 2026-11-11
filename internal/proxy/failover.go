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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// maxCachedUpstreamErrorBody bounds the bytes retained from an upstream 4xx so
// a single request can never retain an unbounded error page. A response above
// the limit is delivered as-is instead of being dequeued.
const maxCachedUpstreamErrorBody = 64 << 10

// upstreamForbiddenCooldown is how long a target stays muted after an
// upstream 403 dequeue. It is a cooldown, not a permanent block: afterwards
// the normal half-open probe lets traffic back in.
const upstreamForbiddenCooldown = time.Hour

type failoverAction int

const (
	failoverTerminate failoverAction = iota
	failoverDequeue
	// failoverDequeueIfAccountRejection dequeues only when the bounded error
	// body proves an account-level rejection (accountRejection).
	failoverDequeueIfAccountRejection
)

// classifyNon2xxAttempt is the single policy entry point for upstream HTTP
// failures. It answers only one question: may this attempt be abandoned and
// the next cost-ordered candidate be tried?
//
//	allowFallback=false  explicit target: never cross targets.
//	status < 400          success: not this path.
//	status >= 500        may already have been computed and billed: terminal.
//	408                  may follow a fully sent request: terminal.
//	401/402              dequeue when accountRejectionDetail calls it a
//	                     rejection (every proxy-credential 401; an account-level
//	                     402); the key loop has already tried the other keys.
//	403                  the provider rejected the proxy's own account on this
//	                     target (client auth is checked locally): dequeue,
//	                     except a content-moderation rejection or the client's
//	                     own credential (caller checks).
//	429                  multi-key targets fail over through the key loop once
//	                     every key is exhausted; a single-key target has no
//	                     key loop, so its 429 dequeues directly.
//	other 4xx            upstream rejected without processing: dequeue.
func classifyNon2xxAttempt(status int, allowFallback, keyPooled bool) failoverAction {
	if !allowFallback || status < 400 || status >= 500 {
		return failoverTerminate
	}
	switch status {
	case http.StatusRequestTimeout:
		return failoverTerminate
	case http.StatusUnauthorized, http.StatusPaymentRequired:
		return failoverDequeueIfAccountRejection
	case http.StatusTooManyRequests:
		if keyPooled {
			return failoverTerminate
		}
	}
	return failoverDequeue
}

// accountRejectionDetail classifies an upstream 401/402/403 as a rejection of
// this target's account and says whether it is confirmed. body is the decoded
// error body when bodyOK.
//
//	403  a rejection unless it is content moderation; confirmed only when the
//	     body carries a structured envelope (code or type). An HTML/WAF page or
//	     a bare 403 is unconfirmed.
//	402  a rejection when the existing key classification calls it account
//	     level (OpenRouter in-flight/weight 402 are not); always confirmed.
//	401  always a rejection of the proxy credential; confirmed when it matches
//	     a known credential rule (invalid_token), otherwise unconfirmed.
//
// Unconfirmed rejections still fail over, but cool briefly first and only
// escalate to the fixed hour when they repeat (unverifiedRejectionCooldown).
func accountRejectionDetail(target *Target, protocol config.ProtocolType, status int, body []byte, bodyOK bool) (rejected, confirmed bool) {
	if !bodyOK {
		body = nil
	}
	switch status {
	case http.StatusForbidden:
		if upstreamContentModeration(target, body) {
			return false, false
		}
		return true, structuredRejection(body)
	case http.StatusPaymentRequired:
		exhausted, reason := targetKeyExhausted(target, protocol, status, body)
		// Without a readable body a vendor 402 (e.g. OpenRouter in-flight)
		// cannot be told apart: cool briefly first.
		return exhausted && isAccountLevelExhaustion(reason), bodyOK
	case http.StatusUnauthorized:
		exhausted, reason := targetKeyExhausted(target, protocol, status, body)
		return true, exhausted && isAccountLevelExhaustion(reason)
	}
	return false, false
}

// accountRejection is the yes/no form of accountRejectionDetail.
func accountRejection(target *Target, protocol config.ProtocolType, status int, body []byte) bool {
	rejected, _ := accountRejectionDetail(target, protocol, status, body, true)
	return rejected
}

// structuredRejection reports whether an error body carries a controlled
// envelope with a code or type, i.e. a deliberate API rejection rather than
// an HTML error page or an empty body.
func structuredRejection(body []byte) bool {
	fields, ok := decodeUpstreamErrorEnvelope(body)
	if !ok {
		return false
	}
	if strings.TrimSpace(fields.Code) != "" || strings.TrimSpace(fields.Type) != "" || strings.TrimSpace(fields.Status) != "" {
		return true
	}
	// A numeric code (Gemini, OpenRouter) is structured too; the string field
	// above only holds string codes.
	var raw struct {
		Error struct {
			Code json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &raw) != nil {
		return false
	}
	code := strings.TrimSpace(string(raw.Error.Code))
	return code != "" && code != "null" && code != `""`
}

// contentModerationCodes are structured error codes that describe the
// request content, not the account. Contract assumption from public error
// vocabularies (OpenAI content_policy_violation / moderation_blocked, Azure
// content_filter); every candidate is expected to reject the same content.
var contentModerationCodes = map[string]struct{}{
	"content_filter":           {},
	"content_policy_violation": {},
	"moderation_blocked":       {},
}

// upstreamContentModeration recognises a 403 caused by the request content.
// Only the controlled envelope code/type is read; the OpenRouter shape
// (metadata.reasons / flagged_input) is honoured only when that Patch is
// explicitly selected, never by host or message text.
func upstreamContentModeration(target *Target, body []byte) bool {
	fields, ok := decodeUpstreamErrorEnvelope(body)
	if ok {
		for _, v := range []string{fields.Code, fields.Type} {
			if _, hit := contentModerationCodes[strings.ToLower(strings.TrimSpace(v))]; hit {
				return true
			}
		}
	}
	if targetVendor(target) == usageAdapterOpenRouter {
		var envelope struct {
			Error struct {
				Metadata struct {
					Reasons      []string        `json:"reasons"`
					FlaggedInput json.RawMessage `json:"flagged_input"`
				} `json:"metadata"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &envelope) == nil {
			md := envelope.Error.Metadata
			if len(md.Reasons) > 0 || (len(md.FlaggedInput) > 0 && string(md.FlaggedInput) != "null") {
				return true
			}
		}
	}
	return false
}

// upstreamRejectionMutesTarget reports whether a dequeued 4xx describes the
// target's account rather than the request. Only proven account rejections
// reach the dequeue with 401/402, and moderation 403s never do, so every
// dequeued 401/402/403 mutes the target through the circuit breaker: later
// requests skip it until a half-open probe succeeds, instead of hitting the
// same rejection first on every request. Other dequeued 4xx confirm
// reachability and stay healthy. The mute lasts upstreamForbiddenCooldown,
// independent of quiet_period, and is not permanent.
func upstreamRejectionMutesTarget(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
		return true
	}
	return false
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
	// nativeHandshake marks a rejection of a native WS upstream handshake, so a
	// final replay keeps the native error event shape.
	nativeHandshake bool
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
	w.WriteHeader(clientFacingStatus(w.Header(), cached.status, false))
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
	// A saved native handshake rejection keeps its type (writeResponse and the
	// settle handoff recognize it) and simply gets its bytes back.
	if rejection := responseWebsocketRejection(resp); rejection != nil {
		rest, _ := io.ReadAll(rejection.Reader)
		rejection.Reader = bytes.NewReader(append(append([]byte(nil), read...), rest...))
		return
	}
	// A native WS turn body carries generation/failure state that
	// responseWSBody must still recognize after the replay. Wrap its reader in
	// place so the type and its release handoff survive; a plain resp.Body
	// replacement would hide both from writeResponse.
	if wsBody := responseWSBody(resp); wsBody != nil {
		wsBody.ReadCloser = &replayReadCloser{
			Reader: io.MultiReader(bytes.NewReader(read), wsBody.ReadCloser),
			Closer: wsBody.ReadCloser,
		}
		return
	}
	resp.Body = &replayReadCloser{
		Reader: io.MultiReader(bytes.NewReader(read), resp.Body),
		Closer: resp.Body,
	}
}

// decodedUpstreamErrorBody decodes a bounded cached error body according to
// its Content-Encoding. Unknown, stacked or broken encodings report false so
// callers fall back to status-only rules instead of guessing.
func decodedUpstreamErrorBody(resp *http.Response, body []byte) ([]byte, bool) {
	if len(body) > maxCachedUpstreamErrorBody {
		return nil, false
	}
	if resp == nil || responseIsIdentityEncoding(resp.Header) {
		return body, true
	}
	reader, err := observationReader(bytes.NewReader(body), responseContentEncoding(resp.Header))
	if err != nil {
		return nil, false
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxCachedUpstreamErrorBody+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || len(data) > maxCachedUpstreamErrorBody {
		return nil, false
	}
	return data, true
}

// upstreamRejectionCode returns the controlled error code (or type) of a
// cached upstream rejection for display. It never returns the message, and
// caps the length so an odd envelope cannot bloat runtime state.
func upstreamRejectionCode(resp *http.Response, body []byte) string {
	fields, ok, _ := upstreamErrorDiagnostics(resp, body)
	if !ok {
		return ""
	}
	code := strings.TrimSpace(fields.Code)
	if code == "" {
		code = strings.TrimSpace(fields.Type)
	}
	if len(code) > 64 {
		code = code[:64]
	}
	return code
}

// writeUpstreamRejectionCooldown answers a request whose every candidate is
// cooling after an account-level upstream rejection. It returns the original
// upstream status (401/402/403) with a proxy-generated error envelope that
// names the cooling target, the controlled upstream code and the recovery
// time; the upstream body itself is not stored or replayed.
func writeUpstreamRejectionCooldown(w http.ResponseWriter, selErr *selectionError, now time.Time) {
	retryAfter := int(selErr.MutedUntil.Sub(now).Seconds())
	if retryAfter < 1 {
		retryAfter = 1
	}
	payload := map[string]any{
		"error": map[string]any{
			"type":        "upstream_rejected",
			"code":        selErr.MuteCode,
			"status":      selErr.MuteStatus,
			"target":      selErr.MuteTarget,
			"muted_until": selErr.MutedUntil.UTC().Format(time.RFC3339),
			"message": fmt.Sprintf("all targets are cooling after upstream %d; target %q retries after %s",
				selErr.MuteStatus, selErr.MuteTarget, selErr.MutedUntil.UTC().Format(time.RFC3339)),
		},
	}
	body, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	if selErr.MuteTarget != "" {
		w.Header().Set("X-Proxy-Target", selErr.MuteTarget)
	}
	w.WriteHeader(clientFacingStatus(w.Header(), selErr.MuteStatus, false))
	_, _ = w.Write(body)
}

// attemptUsesClientCredential reports whether this attempt forwarded the
// client's own upstream credential (bearer passthrough) instead of a proxy
// key. Such a 401/403 judges the client's token, not the target's account: it
// must not cool proxy keys, mute the target for everyone, or cross targets.
func (s *Service) attemptUsesClientCredential(target *Target, r *http.Request) bool {
	if s == nil || target == nil || r == nil || !target.AllowBearer || s.providerRegistry == nil {
		return false
	}
	profile := s.providerRegistry.Lookup(target.EndpointType)
	if profile == nil {
		return false
	}
	az, ok := profile.AuthForTarget(target, r).(*AzureAuth)
	return ok && az.AllowBearer && az.AzureAuthValue != ""
}

// classifyDeferredKeyRejection completes the key classification that
// forwardRequest defers for a 403 whose body exceeded its 4 KiB pre-read. Only
// a complete body (at most maxCachedUpstreamErrorBody, read without error) is
// judged: a content-moderation 403 describes the request and cools nothing; any
// other 403 rejects this key's credential and cools it (confirmed or
// unconfirmed, as markKeyAccountRejection decides). A longer or unreadable body
// keeps the old behaviour (left to the cross-target dequeue), as does a key that
// is already marked, the client's own credential and a native handshake
// rejection (classified on its complete envelope already). The body is always
// restored for the response.
func (s *Service) classifyDeferredKeyRejection(state *targetState, target *Target, r *http.Request, resp *http.Response, keyIndex int) {
	if state == nil || state.keyPool == nil || keyIndex < 0 || resp == nil || resp.StatusCode != http.StatusForbidden ||
		responseWebsocketRejection(resp) != nil || responseWSBody(resp) != nil ||
		s.attemptUsesClientCredential(target, r) || state.keyPool.exhaustReasonAt(keyIndex) != "" {
		return
	}
	cached, read, ok := readUpstreamErrorForCache(resp, target, "")
	restoreUpstreamErrorBody(resp, read)
	if !ok {
		return
	}
	body, decoded := decodedUpstreamErrorBody(resp, cached.body)
	if !decoded || upstreamContentModeration(target, body) {
		return
	}
	s.markKeyAccountRejection(state, target, ResolveProtocol(r.URL.Path), keyIndex, resp.StatusCode, body, false)
}

// targetHasServingKeys reports whether a multi-key target still has a key
// that can serve now or soon (only a transient rate limit or quota window). An account rejection that converged or ran out on
// some keys then only dequeues this request; the key pool keeps managing the
// rejected keys and the target stays in rotation for the healthy ones.
func targetHasServingKeys(state *targetState) bool {
	return state != nil && state.keyPool != nil && state.keyPool.hasServingOrTransientKey()
}

// headerUpstreamStatus carries the real upstream status when the client sees
// a different one (clientFacingStatus).
const headerUpstreamStatus = "X-Upstream-Status"

// clientFacingStatus maps a 401 that rejected the proxy's own upstream key to
// 503: client SDKs and CLIs read 401 as "your key is invalid" and may log the
// user out, while the client's own credential is fine. The upstream status is
// kept in X-Upstream-Status and the body is unchanged. A 401 on the client's
// own forwarded credential (bearer passthrough) is the client's and stays 401.
func clientFacingStatus(h http.Header, status int, clientCredential bool) int {
	if status != http.StatusUnauthorized || clientCredential {
		return status
	}
	if h != nil {
		h.Set(headerUpstreamStatus, strconv.Itoa(status))
	}
	return http.StatusServiceUnavailable
}
