// middleware_requestid_test.go — RequestID 中间件回归测试。
// 为后续性能优化（#10: uuid.NewString → 更快 ID 生成）提供行为锁定。
package middleware

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
)

// ---------- 格式验证 ----------

func TestRequestID_FormatIsValidUUID(t *testing.T) {
	handler := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(RequestIDFromContext(r.Context())))
	}))

	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	id := rec.Body.String()
	// UUID v4 format: 8-4-4-4-12 hex chars
	uuidRegex := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	if !uuidRegex.MatchString(id) {
		t.Errorf("generated request ID %q does not match UUID v4 format", id)
	}
}

// ---------- 唯一性 ----------

func TestRequestID_Uniqueness(t *testing.T) {
	handler := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(RequestIDFromContext(r.Context())))
	}))

	seen := make(map[string]bool)
	const iterations = 1000

	for i := 0; i < iterations; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		id := rec.Body.String()
		if id == "" {
			t.Fatal("expected non-empty request ID")
		}
		if seen[id] {
			t.Fatalf("duplicate request ID %q after %d iterations", id, i)
		}
		seen[id] = true
	}
}

// ---------- 并发安全 ----------

func TestRequestID_ConcurrentSafety(t *testing.T) {
	handler := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(RequestIDFromContext(r.Context())))
	}))

	const goroutines = 50
	const requestsPerGoroutine = 100

	var mu sync.Mutex
	allIDs := make(map[string]bool)
	var wg sync.WaitGroup

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			localIDs := make([]string, 0, requestsPerGoroutine)
			for i := 0; i < requestsPerGoroutine; i++ {
				req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				id := rec.Body.String()
				if id == "" {
					t.Error("empty request ID")
					return
				}
				localIDs = append(localIDs, id)
			}

			mu.Lock()
			for _, id := range localIDs {
				if allIDs[id] {
					t.Errorf("duplicate request ID %q across goroutines", id)
				}
				allIDs[id] = true
			}
			mu.Unlock()
		}()
	}

	wg.Wait()

	expected := goroutines * requestsPerGoroutine
	if len(allIDs) != expected {
		t.Errorf("expected %d unique IDs, got %d", expected, len(allIDs))
	}
}

// ---------- 客户端提供 ID 时保留原样 ----------

func TestRequestID_PreservesClientProvidedID(t *testing.T) {
	handler := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(RequestIDFromContext(r.Context())))
	}))

	clientIDs := []string{
		"custom-id-123",
		"abc",
		"not-a-uuid-but-valid",
		"12345678-1234-1234-1234-123456789abc",
		"",  // empty should trigger generation
	}

	for _, clientID := range clientIDs {
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		if clientID != "" {
			req.Header.Set(HeaderRequestID, clientID)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		got := rec.Body.String()
		if clientID != "" {
			if got != clientID {
				t.Errorf("expected client ID %q preserved, got %q", clientID, got)
			}
		} else {
			if got == "" {
				t.Error("expected generated ID when client sends empty")
			}
		}
	}
}

// ---------- Response header 与 context 一致 ----------

func TestRequestID_HeaderMatchesContext(t *testing.T) {
	handler := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxID := RequestIDFromContext(r.Context())
		headerID := r.Header.Get(HeaderRequestID)
		// Context ID should be set
		if ctxID == "" {
			t.Error("expected request ID in context")
		}
		// Note: the middleware sets the response header, not the request header
		_ = headerID
		w.Write([]byte(ctxID))
	}))

	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	bodyID := rec.Body.String()
	headerID := rec.Header().Get(HeaderRequestID)
	if bodyID != headerID {
		t.Errorf("context ID %q != response header ID %q", bodyID, headerID)
	}
}
