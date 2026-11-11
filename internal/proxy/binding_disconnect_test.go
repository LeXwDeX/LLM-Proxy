package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// A client that closes the connection the moment it has read the whole
// response (Connection: close, as the production edge proxy does without
// upstream keep-alive) must still get its session and IP bindings written: the
// response was delivered in full. A client that leaves mid-response must not.

type slowUsageRecorder struct{ delay time.Duration }

func (r slowUsageRecorder) Record(usage.Event) error {
	time.Sleep(r.delay) // post-copy work during which the client's close is noticed
	return nil
}

type bindingFixture struct {
	svc    *Service
	proxy  *httptest.Server
	mu     sync.Mutex
	hold   chan struct{} // when set, SSE stalls after its first event until closed
	stream bool
	// linger keeps the connection open this long after the response was read.
	linger time.Duration
}

func newBindingFixture(t *testing.T) *bindingFixture {
	t.Helper()
	f := &bindingFixture{hold: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-f.hold:
		default:
			close(f.hold)
		}
	})
	var targets []config.Target
	for _, name := range []string{"t1", "t2"} {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"stream":true`) {
				w.Header().Set("Content-Type", "text/event-stream")
				fl := w.(http.Flusher)
				fmt.Fprint(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
				fl.Flush()
				if strings.Contains(string(body), "stall") {
					select {
					case <-f.hold:
					case <-r.Context().Done():
						return
					}
				}
				fmt.Fprint(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
				fl.Flush()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
		}))
		t.Cleanup(up.Close)
		targets = append(targets, config.Target{Name: name, EndpointType: "openai", Endpoint: up.URL, APIKey: "k-" + name})
	}
	svc, err := NewService(&config.Config{Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 10}, Targets: targets}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	svc.SetUsageRecorder(slowUsageRecorder{delay: 250 * time.Millisecond})
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatal(err)
	}
	principal, _ := store.Authenticate("token")
	f.svc = svc
	f.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		svc.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	}))
	t.Cleanup(f.proxy.Close)
	return f
}

// call sends one request on its own connection (Connection: close). It reads
// until fullyRead says the response is complete and then closes at once;
// readUntil=="" reads only the header and the first event (a mid-stream leave).
func (f *bindingFixture) call(t *testing.T, session string, stream bool, content, readUntil string) (target string) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(f.proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"model":"m","stream":%v,"messages":[{"role":"user","content":%q}]}`, stream, content)
	fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nConnection: close\r\nContent-Type: application/json\r\nX-Session-Id: %s\r\nContent-Length: %d\r\n\r\n%s", session, len(body), body)
	br := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var contentLength = -1
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading headers: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		low := strings.ToLower(line)
		if strings.HasPrefix(low, "x-proxy-target:") {
			target = strings.TrimSpace(line[len("x-proxy-target:"):])
		}
		if strings.HasPrefix(low, "content-length:") {
			contentLength, _ = strconv.Atoi(strings.TrimSpace(line[len("content-length:"):]))
		}
	}
	if stream {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("reading the stream: %v", err)
			}
			if readUntil == "" || strings.Contains(line, readUntil) {
				break
			}
		}
	} else if contentLength >= 0 {
		if _, err := io.CopyN(io.Discard, br, int64(contentLength)); err != nil {
			t.Fatalf("reading the body: %v", err)
		}
	}
	time.Sleep(f.linger)
	conn.Close() // immediately (linger 0): nothing more is read
	return target
}

func (f *bindingFixture) idle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.svc.metrics.activeRequests.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a request never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *bindingFixture) bindings() int {
	f.svc.affinity.mu.RLock()
	defer f.svc.affinity.mu.RUnlock()
	return len(f.svc.affinity.entries)
}

func TestBindingWrittenWhenClientClosesAfterFullResponse(t *testing.T) {
	for _, tc := range []struct {
		name, session string
		stream        bool
		readUntil     string
	}{
		{"json/session", "sess-1", false, ""},
		{"json/ip", "", false, ""},
		{"sse/session", "sess-2", true, "[DONE]"},
		{"sse/ip", "", true, "[DONE]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBindingFixture(t)
			first := f.call(t, tc.session, tc.stream, "one", tc.readUntil)
			f.idle(t)
			if f.bindings() == 0 {
				t.Fatal("no binding was written although the response was delivered in full")
			}
			for i := 0; i < 4; i++ {
				if got := f.call(t, tc.session, tc.stream, "again", tc.readUntil); got != first {
					t.Fatalf("request %d went to %q, want the bound %q", i, got, first)
				}
				f.idle(t)
			}
			if m := f.svc.MetricsSnapshot(); m.TotalSuccess == 0 {
				t.Fatal("successes not counted")
			}
		})
	}
}

// The counter-example: leaving in the middle of a response binds nothing.
func TestNoBindingWhenClientLeavesMidStream(t *testing.T) {
	f := newBindingFixture(t)
	f.call(t, "sess-mid", true, "stall", "") // reads the first event only, then closes
	f.idle(t)
	if n := f.bindings(); n != 0 {
		t.Fatalf("%d bindings written for a response the client abandoned mid-stream", n)
	}
}

// Control: a client that lingers until the server finished gets the binding
// today, which pins the failure above on the close racing the post-copy work.
func TestBindingWrittenWhenClientLingers(t *testing.T) {
	f := newBindingFixture(t)
	f.linger = 500 * time.Millisecond
	f.call(t, "sess-linger", false, "one", "")
	f.idle(t)
	if f.bindings() == 0 {
		t.Fatal("no binding for a lingering client")
	}
}

func TestResponseDeliveredInFull(t *testing.T) {
	canceled := context.Canceled
	cases := []struct {
		name                               string
		status                             int
		contentLength, copied              int64
		copyErr                            error
		writeFailed, lifecycleFailed, term bool
		want                               bool
	}{
		{"complete", 200, 10, 10, nil, false, false, false, true},
		{"complete unknown length", 200, -1, 10, nil, false, false, false, true},
		{"cancel after the declared length", 200, 10, 10, canceled, false, false, false, true},
		{"cancel after the stream terminal", 200, -1, 40, canceled, false, false, true, true},
		{"cancel without proof of completeness", 200, 10, 4, canceled, false, false, false, false},
		{"cancel, unknown length, no terminal", 200, -1, 4, canceled, false, false, false, false},
		{"client write failed", 200, 10, 4, io.ErrClosedPipe, true, false, false, false},
		{"copy error", 200, 10, 4, io.ErrUnexpectedEOF, false, false, false, false},
		{"lifecycle failed", 200, 10, 10, nil, false, true, false, false},
		{"upstream error status", 500, 10, 10, nil, false, false, false, false},
		{"upstream 4xx", 400, 10, 10, nil, false, false, false, false},
	}
	for _, tc := range cases {
		if got := responseDeliveredInFull(tc.status, tc.contentLength, tc.copied, tc.copyErr, tc.writeFailed, tc.lifecycleFailed, tc.term); got != tc.want {
			t.Errorf("%s: delivered = %v, want %v", tc.name, got, tc.want)
		}
	}
}
