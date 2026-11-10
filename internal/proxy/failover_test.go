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
		want          failoverAction
	}{
		{"400 dequeue", http.StatusBadRequest, true, failoverDequeue},
		{"403 account rejection", http.StatusForbidden, true, failoverTerminate},
		{"404 dequeue", http.StatusNotFound, true, failoverDequeue},
		{"409 dequeue", http.StatusConflict, true, failoverDequeue},
		{"413 dequeue", http.StatusRequestEntityTooLarge, true, failoverDequeue},
		{"415 dequeue", http.StatusUnsupportedMediaType, true, failoverDequeue},
		{"422 dequeue", http.StatusUnprocessableEntity, true, failoverDequeue},
		{"401 key scope", http.StatusUnauthorized, true, failoverTerminate},
		{"402 key scope", http.StatusPaymentRequired, true, failoverTerminate},
		{"429 key scope", http.StatusTooManyRequests, true, failoverTerminate},
		{"408 may have processed", http.StatusRequestTimeout, true, failoverTerminate},
		{"500 may have processed", http.StatusInternalServerError, true, failoverTerminate},
		{"502 may have processed", http.StatusBadGateway, true, failoverTerminate},
		{"503 may have processed", http.StatusServiceUnavailable, true, failoverTerminate},
		{"529 may have processed", 529, true, failoverTerminate},
		{"200 success", http.StatusOK, true, failoverTerminate},
		{"400 explicit target", http.StatusBadRequest, false, failoverTerminate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyNon2xxAttempt(tc.status, tc.allowFallback); got != tc.want {
				t.Fatalf("classifyNon2xxAttempt(%d, %v) = %v, want %v", tc.status, tc.allowFallback, got, tc.want)
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
