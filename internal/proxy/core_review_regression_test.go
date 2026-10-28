package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/admission"
	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

type coreReviewTransport func(*http.Request) (*http.Response, error)

func (f coreReviewTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type coreReviewTimeout struct{ message string }

func (e coreReviewTimeout) Error() string { return e.message }
func (coreReviewTimeout) Timeout() bool   { return true }
func (coreReviewTimeout) Temporary() bool { return true }

func TestCoreReviewNetworkTimeoutRetriesOnlyBeforeSend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		preSend bool
	}{
		// The request may already be at the upstream: never retry or mute.
		{"response headers", coreReviewTimeout{"net/http: timeout awaiting response headers"}, false},
		// Nothing was sent yet: fail over and mark the unreachable target.
		{"tls handshake", coreReviewTimeout{"net/http: TLS handshake timeout"}, true},
		{"dial", &net.OpError{Op: "dial", Net: "tcp", Err: coreReviewTimeout{"i/o timeout"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, err := NewService(&config.Config{Targets: []config.Target{
				{Name: "a", EndpointType: config.EndpointTypeOpenAI, Endpoint: "https://a.invalid", APIKey: "fixture"},
				{Name: "b", EndpointType: config.EndpointTypeOpenAI, Endpoint: "https://b.invalid", APIKey: "fixture"},
			}}, newTestLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			// The first attempt fails, whichever target was chosen; any later
			// attempt succeeds.
			var order []string
			svc.httpClient.Transport = coreReviewTransport(func(r *http.Request) (*http.Response, error) {
				order = append(order, strings.TrimSuffix(r.URL.Host, ".invalid"))
				if len(order) > 1 {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)), Request: r}, nil
				}
				return nil, tc.err
			})
			store := auth.NewStore()
			if err := store.LoadFromConfig(testAuthClients("tester", "fixture")); err != nil {
				t.Fatal(err)
			}
			principal, _ := store.Authenticate("fixture")
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
			req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
			rr := httptest.NewRecorder()
			svc.ServeHTTP(rr, req)
			statuses := map[string]TargetStatus{}
			for _, state := range svc.TargetStatuses(time.Now()) {
				statuses[state.Name] = state
			}
			if !tc.preSend {
				if rr.Code != 504 || len(order) != 1 || svc.MetricsSnapshot().TotalRetries != 0 {
					t.Fatalf("post-send timeout retried: status=%d attempts=%v metrics=%+v", rr.Code, order, svc.MetricsSnapshot())
				}
				for _, state := range statuses {
					if state.Muted || state.TotalFailedRequests != 0 {
						t.Fatalf("post-send timeout changed target health: %+v", state)
					}
				}
				return
			}
			if rr.Code != 200 || len(order) != 2 || order[0] == order[1] {
				t.Fatalf("pre-send timeout did not fail over to another target: status=%d attempts=%v body=%s", rr.Code, order, rr.Body.String())
			}
			if failed := statuses[order[0]]; !failed.Muted && failed.TotalFailedRequests == 0 {
				t.Fatalf("unreachable target was not marked unhealthy: %+v", failed)
			}
		})
	}
}

func TestCoreReviewGLMFirstEventDoesNotWaitForStreamEnd(t *testing.T) {
	svc := newTestModelService(t, config.EndpointTypeClaude, "https://fixture.invalid", func(tg *config.Target) { tg.APIKeys = []string{"second"}; tg.UsageAdapter = config.UsageAdapterGLM })
	defer svc.Close()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	first := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"glm-5\",\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n")
	svc.httpClient.Transport = coreReviewTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader, Request: req}, nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"glm-5","stream":true}`))
	done := make(chan struct{})
	var resp *http.Response
	var cancel context.CancelFunc
	var fErr error
	go func() {
		resp, cancel, _, fErr = svc.forwardRequest(req, svc.targetOrder[0], []byte(`{"model":"glm-5","stream":true}`), "", nil)
		close(done)
	}()
	wrote := make(chan struct{})
	go func() { _, _ = writer.Write(first); close(wrote) }()
	select {
	case <-wrote:
	case <-time.After(time.Second):
		t.Fatal("first event was not consumed")
	}
	select {
	case <-done:
	case <-time.After(150 * time.Millisecond):
		writer.Close()
		<-done
		if cancel != nil {
			cancel()
		}
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatal("GLM first complete event blocked until stream EOF/4KB")
	}
	if fErr != nil {
		t.Fatal(fErr)
	}
	defer cancel()
	defer resp.Body.Close()
	got := make([]byte, len(first))
	if _, err := io.ReadFull(resp.Body, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, first) {
		t.Fatal("first SSE bytes changed")
	}
}

func TestCoreReviewTestModelHonorsUnavailableKeyPool(t *testing.T) {
	for _, reason := range []string{"blocked", "rate_limited"} {
		t.Run(reason, func(t *testing.T) {
			svc := newTestModelService(t, config.EndpointTypeOpenAI, "https://fixture.invalid", func(tg *config.Target) { tg.APIKeys = []string{"second"} })
			defer svc.Close()
			calls := 0
			svc.httpClient.Transport = coreReviewTransport(func(req *http.Request) (*http.Response, error) { calls++; return nil, errors.New("should not send") })
			pool := svc.targetOrder[0].keyPool
			for i := range pool.entries {
				if reason == "blocked" {
					pool.blockKey(i)
				} else {
					pool.markRateLimited(i)
				}
			}
			result, err := svc.TestModel("test-target", "test")
			if calls != 0 || err == nil || result != nil {
				t.Fatalf("unavailable pool probed: calls=%d result=%+v err=%v", calls, result, err)
			}
		})
	}
}

func TestCoreReviewAdmissionBindsPoolsAtInitialStartup(t *testing.T) {
	svc := newTestModelService(t, config.EndpointTypeOpenAI, "https://fixture.invalid", func(tg *config.Target) { tg.APIKeys = []string{"second"} })
	defer svc.Close()
	manager := admission.NewManager(newTestLogger(), admission.Options{RInit: 10, RMax: 10})
	svc.SetAdmissionManager(manager)
	pool := svc.targetOrder[0].keyPool
	if manager.Snapshot("test-target") == nil {
		t.Fatal("initial manager attachment did not register existing target")
	}
	pool.mu.Lock()
	callback := pool.onKeyChange
	pool.mu.Unlock()
	if callback == nil {
		t.Fatal("initial startup did not wire live key-pool changes")
	}
	applied := make(chan int, 8)
	pool.setOnKeyChange(func(active, total int) {
		callback(active, total)
		applied <- active
	})
	pool.blockKey(0)
	pool.blockKey(1)
	// Notifications apply in read order; wait until the final state (no active
	// key) has been applied instead of trusting the first one.
	deadline := time.After(time.Second)
	for done := false; !done; {
		select {
		case active := <-applied:
			done = active == 0
		case <-deadline:
			t.Fatal("startup key change did not notify admission with active=0")
		}
	}
	// Callbacks read the latest snapshot, so both keys must be unavailable now.
	acquired := 0
	for i := 0; i < 10; i++ {
		if manager.TryAcquire("test-target") {
			acquired++
		}
	}
	if acquired != 1 {
		t.Fatalf("all blocked keys must reduce admission burst to 1, got %d", acquired)
	}
}

func TestCoreReviewCooldownRecoveryNotifiesActiveKeys(t *testing.T) {
	pool := newTestKeyPool([]string{"fixture-a", "fixture-b"})
	for i := range pool.entries {
		pool.entries[i].exhausted = true
		pool.entries[i].exhaustReason = "rate_limited"
		pool.entries[i].cooldownEnd = time.Now().Add(time.Hour)
	}
	changed := make(chan int, 2)
	pool.setOnKeyChange(func(active, total int) { changed <- active })
	// Drain exhaustion notifications before the measured recovery.
	pool.mu.Lock()
	pool.entries[0].cooldownEnd = time.Now().Add(-time.Second)
	pool.mu.Unlock()
	for {
		select {
		case <-changed:
		default:
			goto drained
		}
	}
drained:
	if key, _ := pool.selectKeyForClient("fixture-client"); key == "" {
		t.Fatal("expired key failed to recover")
	}
	select {
	case active := <-changed:
		if active != 1 {
			t.Fatalf("recovery notified active=%d", active)
		}
	case <-time.After(time.Second):
		t.Fatal("cooldown recovery did not notify admission")
	}
}

func TestCoreReviewGLMPeekPreservesFramingAndDetectsFragmentedErrors(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n", "\r"} {
		t.Run(strings.ReplaceAll(newline, "\r", "CR"), func(t *testing.T) {
			prefix := strings.Join([]string{": heartbeat", "", "event: error", `data: {"type":"error","error":{"type":"rate_limit_error","code":"1308","message":"quota"}}`, "", ""}, newline)
			tail := strings.Join([]string{"data: [DONE]", "", ""}, newline)
			reader, writer := io.Pipe()
			defer reader.Close()
			go func() {
				defer writer.Close()
				for _, b := range []byte(prefix + tail) {
					if _, err := writer.Write([]byte{b}); err != nil {
						return
					}
				}
			}()
			peeked, replay := peekGLMFirstEvent(reader)
			limited, code, _ := detectGLMRateLimitInBody(peeked)
			if !limited || code != "1308" {
				t.Fatalf("fragmented first error not detected: limited=%v code=%s prefix=%q", limited, code, peeked)
			}
			got, err := io.ReadAll(replay)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != prefix+tail {
				t.Fatalf("SSE byte framing changed: got %q", got)
			}
		})
	}
	large := strings.Repeat("x", 8192)
	peeked, replay := peekGLMFirstEvent(strings.NewReader(large))
	if len(peeked) != 4096 {
		t.Fatalf("peek exceeded bounded budget: %d", len(peeked))
	}
	all, _ := io.ReadAll(replay)
	if string(all) != large {
		t.Fatal("bounded peek discarded unread bytes")
	}
}

func TestCoreReviewAdmissionIgnoresRetiredPoolCallbacks(t *testing.T) {
	svc := newTestModelService(t, config.EndpointTypeOpenAI, "https://fixture.invalid", func(tg *config.Target) { tg.APIKeys = []string{"second"} })
	defer svc.Close()
	manager := admission.NewManager(newTestLogger(), admission.Options{RInit: 10, RMax: 10})
	svc.SetAdmissionManager(manager)
	oldPool := svc.targetOrder[0].keyPool
	oldPool.mu.Lock()
	oldCallback := oldPool.onKeyChange
	oldPool.mu.Unlock()
	if oldCallback == nil {
		t.Fatal("pool callback missing")
	}
	cfg := &config.Config{Targets: []config.Target{{Name: "test-target", EndpointType: config.EndpointTypeOpenAI, Endpoint: "https://fixture.invalid", APIKey: "new-a", APIKeys: []string{"new-b", "new-c"}}}}
	if err := svc.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	oldCallback(0, 2) // A pre-reload request finished against the retired credentials.
	acquired := 0
	for i := 0; i < 10; i++ {
		if manager.TryAcquire("test-target") {
			acquired++
		}
	}
	if acquired < 2 {
		t.Fatalf("retired pool poisoned current admission capacity: %d", acquired)
	}
}

// Notifications must be applied in the order their state was read, so the last
// callback always carries the latest key-pool state.
func TestKeyPoolNotificationsApplyInReadOrder(t *testing.T) {
	pool := newTestKeyPool([]string{"fixture-a", "fixture-b"})
	var mu sync.Mutex
	var seen []int
	first := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	pool.setOnKeyChange(func(active, total int) {
		if calls.Add(1) == 1 {
			close(first)
			<-release // hold the first notification (read active=1) mid-callback
		}
		mu.Lock()
		seen = append(seen, active)
		mu.Unlock()
	})
	pool.blockKey(0)
	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("first notification never started")
	}
	pool.blockKey(1)                   // second notification reads active=0
	time.Sleep(100 * time.Millisecond) // let it run, or queue behind the first
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected 2 notifications, got %v", seen)
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if last := seen[len(seen)-1]; last != 0 {
		t.Fatalf("last applied notification must carry final state active=0, applied order=%v", seen)
	}
}
