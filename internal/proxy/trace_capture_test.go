package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/errorlog"
	"github.com/LeXwDeX/LLM-Proxy/internal/tracestore"
)

func newTraceCaptureService(t testing.TB, traceEnabled bool, status int, contentType, body string) (*Service, *auth.Principal, *tracestore.Store) {
	t.Helper()
	s, err := NewService(&config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{Name: "t", EndpointType: config.EndpointTypeOpenAI, Endpoint: "http://upstream.invalid", APIKey: "fixture", UsageAdapter: "standard"}},
	}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	store := tracestore.New(tracestore.Config{Enabled: traceEnabled, RingBufferSize: 4, MaxBodySize: 8 << 20, ChannelBuffer: 4}, newTestLogger())
	t.Cleanup(func() { _ = store.Close() })
	s.traceStore = store
	s.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		_, _ = io.Copy(io.Discard, r.Body)
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	store2 := auth.NewStore()
	if err := store2.LoadFromConfig(testAuthClients("tracer", "trace-token")); err != nil {
		t.Fatal(err)
	}
	principal, _ := store2.Authenticate("trace-token")
	return s, principal, store
}

func serveTraceCapture(s *Service, principal *auth.Principal) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestResponseCaptureOnlyWhenNeeded(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		trace                         bool
		status                        int
		upstreamWS, nativeWS, capture bool
	}{
		{"2xx without trace", false, 200, false, false, false},
		{"3xx without trace", false, 304, false, false, false},
		{"2xx with trace", true, 200, false, false, true},
		{"4xx without trace", false, 404, false, false, true},
		{"5xx without trace", false, 503, false, false, true},
		{"upstream WS", false, 200, true, false, true},
		{"native WS turn", false, 200, false, true, true},
	} {
		if got := responseCaptureNeeded(tc.trace, tc.status, tc.upstreamWS, tc.nativeWS); got != tc.capture {
			t.Errorf("%s: capture=%v want %v", tc.name, got, tc.capture)
		}
	}
}

type countingDiscardWriter struct {
	header http.Header
	n      int
}

func (w *countingDiscardWriter) Header() http.Header         { return w.header }
func (w *countingDiscardWriter) WriteHeader(int)             {}
func (w *countingDiscardWriter) Flush()                      {}
func (w *countingDiscardWriter) Write(p []byte) (int, error) { w.n += len(p); return len(p), nil }

// With trace disabled a 2xx response is not captured at all. An opaque body
// keeps usage observation cheap, so the 2 MiB head/tail capture would
// dominate this run's allocations.
func TestTraceDisabledSkipsResponseCapture(t *testing.T) {
	wire := strings.Repeat("x", 4<<20)
	s, principal, store := newTraceCaptureService(t, false, http.StatusOK, "application/octet-stream", wire)
	serve := func() int {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
		w := &countingDiscardWriter{header: make(http.Header)}
		s.ServeHTTP(w, r)
		return w.n
	}
	serve() // warm up pools
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	forwarded := serve()
	runtime.ReadMemStats(&after)
	if forwarded != len(wire) {
		t.Fatalf("forwarded %d bytes, want %d", forwarded, len(wire))
	}
	if store.Get("") != nil {
		t.Fatal("disabled store recorded a trace")
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("trace disabled: 4 MiB 2xx response allocated %d bytes; the 2 MiB capture was not skipped", allocated)
	}
}

func TestRecordTraceDisabledDoesNotCopy(t *testing.T) {
	s := &Service{traceStore: tracestore.New(tracestore.Config{Enabled: false}, nil)}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Content-Type", "application/json")
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Request: r}
	body := []byte(strings.Repeat("x", 1<<20))
	allocs := testing.AllocsPerRun(10, func() {
		s.recordTrace(r, &Target{Name: "t"}, resp, body, body, time.Now(), 0, "m", usageTokens{})
	})
	if allocs != 0 {
		t.Fatalf("disabled recordTrace allocated %.0f objects", allocs)
	}
	(&Service{}).recordTrace(r, nil, resp, body, body, time.Now(), 0, "m", usageTokens{}) // nil store: no panic
}

func TestTraceEnabledRecordsBodiesUnchanged(t *testing.T) {
	wire := "data: {\"choices\":[{\"delta\":{\"content\":\"<b>&\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"
	s, principal, store := newTraceCaptureService(t, true, http.StatusOK, "text/event-stream", wire)
	if w := serveTraceCapture(s, principal); w.Body.String() != wire {
		t.Fatalf("forwarded body changed: %q", w.Body.String())
	}
	record := store.Get("")
	if record == nil || record.ResponseBody != wire || record.RequestBody != `{"model":"m","stream":true}` {
		t.Fatalf("trace record = %+v", record)
	}
	if record.InputTokens != 3 || record.OutputTokens != 1 {
		t.Fatalf("trace tokens = %d/%d", record.InputTokens, record.OutputTokens)
	}
}

func readErrorLogEntries(t *testing.T, path string) []errorlog.Entry {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var entries []errorlog.Entry
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var entry errorlog.Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	return entries
}

// 4xx/5xx error logs retain captured size, not response content, with trace off.
func TestUpstreamErrorLogBodyRedactedWithoutTrace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream-error.log")
	errorlog.Init(path)
	t.Cleanup(func() { _ = errorlog.Close() })
	small := `{"error":{"message":"model <x> not found & gone","type":"invalid_request_error"}}`
	large := `{"error":"` + strings.Repeat("e", 3<<20) + `"}`
	for _, tc := range []struct {
		status    int
		body      string
		respBytes int
	}{
		{http.StatusNotFound, small, len(small)},
		{http.StatusBadGateway, large, 2 << 20}, // head/tail capture budget
	} {
		s, principal, _ := newTraceCaptureService(t, false, tc.status, "application/json", tc.body)
		if w := serveTraceCapture(s, principal); w.Code != tc.status || w.Body.String() != tc.body {
			t.Fatalf("status=%d forwarded %d bytes", w.Code, w.Body.Len())
		}
	}
	entries := readErrorLogEntries(t, path)
	if len(entries) != 2 {
		t.Fatalf("error log entries = %d", len(entries))
	}
	if entries[0].UpstreamStatus != 404 || entries[0].RespExcerpt != "[redacted]" || entries[0].RespBytes != len(small) {
		t.Fatalf("4xx entry = %+v", entries[0])
	}
	if entries[1].UpstreamStatus != 502 || entries[1].RespExcerpt != "[redacted]" || entries[1].RespBytes != 2<<20 {
		t.Fatalf("5xx entry status=%d bytes=%d excerpt=%q", entries[1].UpstreamStatus, entries[1].RespBytes, entries[1].RespExcerpt)
	}
}

type flushSignalWriter struct {
	*goneClientWriter
	flushed chan struct{}
}

func (w *flushSignalWriter) Flush() {
	select {
	case w.flushed <- struct{}{}:
	default:
	}
}

// SSE response headers reach the client before the first upstream event.
func TestSSEHeadersFlushedBeforeFirstEvent(t *testing.T) {
	release := make(chan struct{})
	s, err := NewService(&config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{Name: "t", EndpointType: config.EndpointTypeOpenAI, Endpoint: "http://upstream.invalid", APIKey: "fixture", UsageAdapter: "standard"}},
	}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reader, writer := io.Pipe()
		go func() {
			<-release
			_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"))
			_ = writer.Close()
		}()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader, Request: r}, nil
	})}
	principal := newAllowAllPrincipal(t, "fixture-sse-flush")
	flushed := make(chan struct{}, 1)
	w := &flushSignalWriter{goneClientWriter: newGoneClientWriter(), flushed: flushed}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
	done := make(chan struct{})
	go func() { defer close(done); s.ServeHTTP(w, r) }()
	select {
	case <-flushed:
	case <-time.After(3 * time.Second):
		close(release)
		<-done
		t.Fatal("SSE headers were not flushed before the first event")
	}
	close(release)
	<-done
}
