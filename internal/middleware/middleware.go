package middleware

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/LLM-Proxy/internal/errorlog"
)

func init() {
	// EnableRandPool buffers random bytes to avoid per-request crypto/rand syscalls.
	// Safe for concurrent use; trades a small amount of memory for reduced syscall overhead.
	uuid.EnableRandPool()
}

type contextKey string

const (
	requestIDKey    contextKey = "request_id"
	HeaderRequestID            = "X-Request-ID"
)

// RequestID attaches a unique request identifier to the context and response headers.
func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqID := r.Header.Get(HeaderRequestID)
			if reqID == "" {
				reqID = uuid.NewString()
			}

			ctx := context.WithValue(r.Context(), requestIDKey, reqID)
			w.Header().Set(HeaderRequestID, reqID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Recoverer ensures panics are logged and converted into HTTP 500 responses.
func Recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &panicSafeWriter{ResponseWriter: w}
			defer func() {
				if rec := recover(); rec != nil {
					// 保持标准 abort 语义：http.ErrAbortHandler 不记为错误，直接上抛。
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					logger.Error("panic recovered",
						"panic", rec,
						"request_id", RequestIDFromContext(r.Context()),
						"method", r.Method,
						"path", r.URL.Path,
					)
					// 旁路：写结构化错误日志便于事后 grep。failure 不阻塞响应。
					errorlog.Write(errorlog.Entry{
						TraceID:  RequestIDFromContext(r.Context()),
						Kind:     errorlog.KindProxyPanic,
						Method:   r.Method,
						Path:     r.URL.Path,
						ClientIP: remoteIP(r),
						Error:    fmt.Sprintf("%v", rec),
					})
					// m1：仅当响应头尚未发出才写 500。若已部分写出（如 SSE 流中途
					// panic），再写会触发 superfluous WriteHeader 并把错误文本混入流尾。
					if !sw.wrote {
						http.Error(sw, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
					}
				}
			}()

			next.ServeHTTP(sw, r)
		})
	}
}

// AccessLogger logs request/response information through the provided logger.
func AccessLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()

			next.ServeHTTP(rec, r)

			duration := time.Since(start)
			logger.Info("http_request",
				"request_id", RequestIDFromContext(r.Context()),
				"remote_ip", remoteIP(r),
				"method", r.Method,
				"path", sanitizedURI(r),
				"status", rec.status,
				"bytes", rec.bytesWritten,
				"duration_ms", duration.Milliseconds(),
			)
		})
	}
}

// RequestIDFromContext retrieves the request ID if available.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

type responseRecorder struct {
	http.ResponseWriter
	status       int
	bytesWritten int64
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytesWritten += int64(n)
	return n, err
}

// Flush implements http.Flusher so that SSE streaming works correctly
// through the middleware chain. Without this, streamingWriter cannot
// detect the Flusher interface and SSE frames accumulate in the buffer.
func (r *responseRecorder) Flush() {
	if fl, ok := r.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// Hijack implements http.Hijacker so that responseRecorder passes through the
// AccessLogger middleware while still supporting Hijacker interface for scenarios
// that require hijacking the underlying connection.
func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := r.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Unwrap returns the underlying ResponseWriter, allowing http.NewResponseController
// and similar utilities to discover interfaces on the original writer.
func (r *responseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// ClientIP extracts the client IP from the request.
// It checks X-Forwarded-For first (taking the first IP in the chain),
// then falls back to RemoteAddr. This is the single shared implementation
// used by both the middleware layer and the proxy forwarding layer.
func ClientIP(r *http.Request) string {
	return remoteIP(r)
}

// remoteIP extracts the client IP, honoring X-Forwarded-For only when the direct
// peer is trusted (env TRUST_PROXY_XFF=true, or RemoteAddr is loopback/private —
// the typical reverse-proxy topology). Otherwise any client could spoof XFF and
// poison the client_ip recorded in access/audit/error logs.
func remoteIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	if trustProxyXFF || isPrivateOrLoopback(host) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[0]); ip != "" {
				return ip
			}
		}
	}
	return host
}

// trustProxyXFF force-trusts X-Forwarded-For regardless of peer address, for
// deployments behind a public-facing reverse proxy on a non-private network.
var trustProxyXFF = strings.EqualFold(strings.TrimSpace(os.Getenv("TRUST_PROXY_XFF")), "true")

func isPrivateOrLoopback(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

// sensitiveQueryKeys are query parameter names whose values are credentials and
// must be redacted before logging the request URI.
var sensitiveQueryKeys = map[string]struct{}{
	"key": {}, "api-key": {}, "api_key": {}, "apikey": {},
	"access_token": {}, "token": {}, "x-goog-api-key": {}, "authorization": {},
}

// sanitizedURI returns the request path with sensitive query values redacted.
// Auth accepts credentials via ?key=/?api-key= (Gemini SDK default), so logging
// the raw RequestURI would leak the client access_key into access.log/stdout.
func sanitizedURI(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return r.URL.Path
	}
	q := r.URL.Query()
	changed := false
	for k := range q {
		if _, ok := sensitiveQueryKeys[strings.ToLower(k)]; ok {
			q.Set(k, "REDACTED")
			changed = true
		}
	}
	if !changed {
		return r.URL.RequestURI()
	}
	return r.URL.Path + "?" + q.Encode()
}

// panicSafeWriter tracks whether any part of the response was written, so
// Recoverer can avoid a superfluous WriteHeader after a mid-response panic.
// It forwards Flush/Hijack/Unwrap so SSE streaming and connection hijacking
// keep working through the middleware chain.
type panicSafeWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *panicSafeWriter) WriteHeader(status int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *panicSafeWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *panicSafeWriter) Flush() {
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (w *panicSafeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (w *panicSafeWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

