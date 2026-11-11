package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestClassifyNon2xxAttempt(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		allowFallback bool
		keyPooled     bool
		want          failoverAction
	}{
		{"400 dequeue", http.StatusBadRequest, true, false, failoverDequeue},
		{"403 account rejection dequeues", http.StatusForbidden, true, false, failoverDequeue},
		{"403 multi-key dequeues after key loop", http.StatusForbidden, true, true, failoverDequeue},
		{"403 explicit target", http.StatusForbidden, false, false, failoverTerminate},
		{"404 dequeue", http.StatusNotFound, true, false, failoverDequeue},
		{"409 dequeue", http.StatusConflict, true, false, failoverDequeue},
		{"413 dequeue", http.StatusRequestEntityTooLarge, true, false, failoverDequeue},
		{"415 dequeue", http.StatusUnsupportedMediaType, true, false, failoverDequeue},
		{"422 dequeue", http.StatusUnprocessableEntity, true, false, failoverDequeue},
		{"401 only if account rejection", http.StatusUnauthorized, true, false, failoverDequeueIfAccountRejection},
		{"402 only if account rejection", http.StatusPaymentRequired, true, true, failoverDequeueIfAccountRejection},
		{"401 explicit target", http.StatusUnauthorized, false, false, failoverTerminate},
		{"429 single key dequeues", http.StatusTooManyRequests, true, false, failoverDequeue},
		{"429 key pool scope", http.StatusTooManyRequests, true, true, failoverTerminate},
		{"429 explicit target", http.StatusTooManyRequests, false, false, failoverTerminate},
		{"408 may have processed", http.StatusRequestTimeout, true, false, failoverTerminate},
		{"500 may have processed", http.StatusInternalServerError, true, false, failoverTerminate},
		{"502 may have processed", http.StatusBadGateway, true, false, failoverTerminate},
		{"503 may have processed", http.StatusServiceUnavailable, true, false, failoverTerminate},
		{"529 may have processed", 529, true, false, failoverTerminate},
		{"200 success", http.StatusOK, true, false, failoverTerminate},
		{"400 explicit target", http.StatusBadRequest, false, false, failoverTerminate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyNon2xxAttempt(tc.status, tc.allowFallback, tc.keyPooled); got != tc.want {
				t.Fatalf("classifyNon2xxAttempt(%d, %v, %v) = %v, want %v", tc.status, tc.allowFallback, tc.keyPooled, got, tc.want)
			}
		})
	}
}

func TestDeterministicUpstreamClientError(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"whitelisted", `{"error":{"code":"invalid_function_parameters"}}`, "invalid_function_parameters"},
		{"whitelisted mixed case", `{"error":{"code":" Invalid_Function_Parameters "}}`, "invalid_function_parameters"},
		{"target-specific code", `{"error":{"code":"bad_a"}}`, ""},
		{"top-level code only", `{"code":"invalid_function_parameters"}`, ""},
		{"error is a string", `{"error":"invalid_function_parameters"}`, ""},
		{"non-json", `invalid_function_parameters`, ""},
		{"numeric code", `{"error":{"code":400}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}}}
			got, ok := deterministicUpstreamClientError(resp, []byte(tc.body))
			if ok != (tc.want != "") || got != tc.want {
				t.Fatalf("deterministicUpstreamClientError(%q) = (%q, %v), want (%q, %v)", tc.body, got, ok, tc.want, tc.want != "")
			}
		})
	}
}

func TestWriteCachedUpstreamErrorReplaysOrigin(t *testing.T) {
	body := []byte(`{"code":"InvalidParameter","message":"Request body format invalid"}`)
	cached := &cachedUpstreamError{
		status: http.StatusBadRequest,
		header: http.Header{
			"Content-Type":          {"application/json"},
			"Content-Encoding":      {"identity"},
			"X-Upstream-Request-Id": {"upstream-123"},
			"Connection":            {"close"}, // hop-by-hop must be dropped
		},
		body:              body,
		target:            &Target{Name: "DeepSeek-A", Pool: "p1"},
		upstreamRequestID: "upstream-123",
	}
	rec := newResponseRecorder()
	writeCachedUpstreamError(rec, cached)

	if rec.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.status)
	}
	if !bytes.Equal(rec.body.Bytes(), body) {
		t.Fatalf("body = %q, want %q", rec.body.String(), body)
	}
	if got := rec.header.Get("X-Proxy-Target"); got != "DeepSeek-A" {
		t.Fatalf("X-Proxy-Target = %q", got)
	}
	if got := rec.header.Get("X-Proxy-Pool"); got != "p1" {
		t.Fatalf("X-Proxy-Pool = %q", got)
	}
	if got := rec.header.Get("X-Upstream-Request-Id"); got != "upstream-123" {
		t.Fatalf("X-Upstream-Request-Id = %q", got)
	}
	if got := rec.header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.header.Get("Connection"); got != "" {
		t.Fatalf("hop-by-hop Connection leaked: %q", got)
	}
}

func TestReadUpstreamErrorForCacheBoundary(t *testing.T) {
	t.Run("at limit", func(t *testing.T) {
		body := bytes.Repeat([]byte("a"), maxCachedUpstreamErrorBody)
		resp := &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}
		cached, read, ok := readUpstreamErrorForCache(resp, &Target{Name: "A"}, "upstream-model")
		if !ok || cached == nil {
			t.Fatalf("exact-limit body must be cacheable")
		}
		if len(read) != len(body) || !bytes.Equal(cached.body, body) {
			t.Fatalf("cached body mismatch: read=%d cached=%d", len(read), len(cached.body))
		}
	})

	t.Run("over limit restores body", func(t *testing.T) {
		body := bytes.Repeat([]byte("b"), maxCachedUpstreamErrorBody+4096)
		resp := &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}
		cached, read, ok := readUpstreamErrorForCache(resp, &Target{Name: "A"}, "upstream-model")
		if ok || cached != nil {
			t.Fatalf("oversized body must not be cached")
		}
		restoreUpstreamErrorBody(resp, read)
		restored, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read restored body: %v", err)
		}
		if !bytes.Equal(restored, body) {
			t.Fatalf("restored body length = %d, want %d", len(restored), len(body))
		}
	})

	t.Run("read error restores partial bytes", func(t *testing.T) {
		resp := &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(&failingReader{data: []byte("partial")})}
		cached, read, ok := readUpstreamErrorForCache(resp, &Target{Name: "A"}, "upstream-model")
		if ok || cached != nil {
			t.Fatalf("unreadable body must not be cached")
		}
		if !strings.Contains(string(read), "partial") {
			t.Fatalf("partial bytes not returned: %q", read)
		}
	})
}

// A native WS turn body carries generation/failure state. Restoring the read
// bytes must keep that type and the slot release wrapper recognisable.
func TestRestoreUpstreamErrorBodyPreservesWebsocketState(t *testing.T) {
	ws := &responsesWSBody{ReadCloser: io.NopCloser(bytes.NewReader([]byte("rest")))}
	released := false
	resp := &http.Response{Body: &releaseOnCloseBody{ReadCloser: ws, release: func() { released = true }}}

	restoreUpstreamErrorBody(resp, []byte("head"))

	if got := responseWSBody(resp); got != ws {
		t.Fatalf("responseWSBody lost the native body type")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if string(body) != "headrest" {
		t.Fatalf("restored body = %q, want headrest", body)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close restored body: %v", err)
	}
	if !released {
		t.Fatalf("slot release was lost")
	}
}

type failingReader struct {
	data []byte
	done bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.ErrUnexpectedEOF
	}
	r.done = true
	n := copy(p, r.data)
	return n, io.ErrUnexpectedEOF
}

// responseRecorder is a minimal ResponseWriter; net/http/httptest would add
// its own header canonicalization assumptions to this package's tests.
type responseRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{header: make(http.Header)}
}

func (r *responseRecorder) Header() http.Header { return r.header }

func (r *responseRecorder) WriteHeader(status int) { r.status = status }

func (r *responseRecorder) Write(p []byte) (int, error) { return r.body.Write(p) }
