package middleware

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestResponseRecorder_ImplementsHijacker verifies that responseRecorder exposes
// the http.Hijacker interface so that the AccessLogger middleware wrapper does not
// block Hijacker passthrough for scenarios requiring underlying connection access.
func TestResponseRecorder_ImplementsHijacker(t *testing.T) {
	// Use httptest.NewServer to get a real HTTP connection whose underlying
	// ResponseWriter implements http.Hijacker (as opposed to httptest.NewRecorder).
	done := make(chan string, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Wrap with responseRecorder exactly as AccessLogger does.
		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}

		// 1. The type assertion for http.Hijacker must succeed.
		// responseRecorder wraps http.ResponseWriter (interface) and asserts
		// w.(http.Hijacker), so we replicate that exact pattern here.
		var wIface http.ResponseWriter = rec
		hj, ok := wIface.(http.Hijacker)
		if !ok {
			done <- "FAIL: responseRecorder does not implement http.Hijacker"
			return
		}

		// 2. Hijack must return a usable connection.
		conn, _, err := hj.Hijack()
		if err != nil {
			done <- "FAIL: Hijack returned error: " + err.Error()
			return
		}
		defer conn.Close()

		// 3. The hijacked connection must be writable.
		_, err = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 8\r\n\r\nHijacked"))
		if err != nil {
			done <- "FAIL: conn.Write failed: " + err.Error()
			return
		}

		done <- "OK"
	}))
	defer server.Close()

	// Send a request to trigger the handler.
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if string(body) != "Hijacked" {
		t.Fatalf("expected body %q, got %q", "Hijacked", string(body))
	}

	// Verify the handler-side assertion succeeded.
	result := <-done
	if result != "OK" {
		t.Fatal(result)
	}
}

type recordCollector struct {
	mu      sync.Mutex
	records []map[string]any
}

func (c *recordCollector) Enabled(context.Context, slog.Level) bool { return true }

func (c *recordCollector) Handle(_ context.Context, r slog.Record) error {
	fields := make(map[string]any)
	fields["message"] = r.Message
	r.Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value.Any()
		return true
	})

	c.mu.Lock()
	c.records = append(c.records, fields)
	c.mu.Unlock()
	return nil
}

func (c *recordCollector) WithAttrs([]slog.Attr) slog.Handler { return c }

func (c *recordCollector) WithGroup(string) slog.Handler { return c }

func TestRequestIDUsesExistingHeader(t *testing.T) {
	handler := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(RequestIDFromContext(r.Context())))
	}))

	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.Header.Set(HeaderRequestID, "abc123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(HeaderRequestID); got != "abc123" {
		t.Fatalf("expected response request id header %q, got %q", "abc123", got)
	}
	if got := rec.Body.String(); got != "abc123" {
		t.Fatalf("expected body %q, got %q", "abc123", got)
	}
}

func TestRequestIDGeneratesWhenMissing(t *testing.T) {
	handler := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(RequestIDFromContext(r.Context())))
	}))

	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	got := rec.Header().Get(HeaderRequestID)
	if got == "" {
		t.Fatal("expected request id header to be set")
	}
	if body := rec.Body.String(); body != got {
		t.Fatalf("expected body %q to match header %q", body, got)
	}
}

func TestRecovererConvertsPanicTo500(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	handler := Recoverer(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", rec.Code)
	}
}

func TestAccessLoggerEmitsRecord(t *testing.T) {
	collector := &recordCollector{}
	logger := slog.New(collector)
	handler := AccessLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest(http.MethodGet, "http://example.com/test?x=1", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	collector.mu.Lock()
	defer collector.mu.Unlock()
	if len(collector.records) != 1 {
		t.Fatalf("expected 1 log record, got %d", len(collector.records))
	}
	record := collector.records[0]
	if record["message"] != "http_request" {
		t.Fatalf("expected message http_request, got %v", record["message"])
	}
	switch v := record["status"].(type) {
	case int:
		if v != http.StatusCreated {
			t.Fatalf("expected status %d, got %v", http.StatusCreated, record["status"])
		}
	case int64:
		if int(v) != http.StatusCreated {
			t.Fatalf("expected status %d, got %v", http.StatusCreated, record["status"])
		}
	default:
		t.Fatalf("unexpected status type %T (%v)", record["status"], record["status"])
	}
	switch v := record["bytes"].(type) {
	case int64:
		if v != 2 {
			t.Fatalf("expected bytes 2, got %v", record["bytes"])
		}
	case int:
		if v != 2 {
			t.Fatalf("expected bytes 2, got %v", record["bytes"])
		}
	default:
		t.Fatalf("unexpected bytes type %T (%v)", record["bytes"], record["bytes"])
	}
}
