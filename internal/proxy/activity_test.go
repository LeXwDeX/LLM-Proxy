package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/gorilla/websocket"
)

func activityTestTime() time.Time { return time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC) }

func (t *activityTracker) beginAt(name string, now time.Time) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.beginLocked(name, now)
}
func (t *activityTracker) endAt(key string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.endLocked(key, now)
}
func (t *activityTracker) snapshotAt(now time.Time) ActivitySnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked(now)
}

//go:noinline
func activityQueuedSnapshot(tracker *activityTracker, done chan<- ActivitySnapshot) {
	done <- tracker.snapshot()
}

func waitActivitySnapshotLock(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	stack := make([]byte, 1<<20)
	for time.Now().Before(deadline) {
		n := runtime.Stack(stack, true)
		for _, g := range strings.Split(string(stack[:n]), "\n\n") {
			if strings.Contains(g, "activityQueuedSnapshot(") && strings.Contains(g, "Mutex).") {
				return
			}
		}
		runtime.Gosched()
	}
	t.Fatal("snapshot did not wait on tracker lock")
}

func TestActivityClockSampledInsideLock(t *testing.T) {
	at := activityTestTime()
	tracker := newActivityTracker(at)
	key := tracker.beginAt("queued", at.Add(time.Second))
	tracker.mu.Lock()
	held := true
	defer func() {
		if held {
			tracker.mu.Unlock()
		}
	}()
	called := make(chan struct{}, 1)
	tracker.clock = func() time.Time { called <- struct{}{}; return at.Add(2 * time.Second) }
	done := make(chan ActivitySnapshot, 1)
	go activityQueuedSnapshot(tracker, done)
	waitActivitySnapshotLock(t)
	// Hold the lock while replacing the clock with the new wall time. A reader
	// sampled outside the lock would carry stale time into advance and reset since.
	tracker.snapshotLocked(at.Add(3 * time.Second))
	tracker.clock = func() time.Time { called <- struct{}{}; return at.Add(4 * time.Second) }
	tracker.mu.Unlock()
	held = false
	select {
	case snapshot := <-done:
		if !snapshot.To.Equal(at.Add(4*time.Second)) || !snapshot.Since.Equal(at) || snapshot.ActiveRequests != 1 {
			t.Fatalf("queued sampling reset history: %+v", snapshot)
		}
	case <-time.After(time.Second):
		t.Fatal("queued snapshot did not finish")
	}
	tracker.endAt(key, at.Add(5*time.Second))
}

func activityPeakAt(t *testing.T, s ActivitySnapshot, at time.Time) *int {
	t.Helper()
	for i, interval := range s.Intervals {
		if !at.Before(interval.From) && at.Before(interval.To) {
			return s.Peaks[i]
		}
	}
	t.Fatalf("missing interval for %v", at)
	return nil
}

func TestActivityShortRequestsAndIndependentPeaks(t *testing.T) {
	at := activityTestTime()
	tracker := newActivityTracker(at)
	a := tracker.beginAt(" Alice ", at.Add(time.Second))
	tracker.endAt(a, at.Add(2*time.Second))
	b := tracker.beginAt("Bob", at.Add(3*time.Second))
	tracker.endAt(b, at.Add(4*time.Second))
	s := tracker.snapshotAt(at.Add(9 * time.Second))
	peak := activityPeakAt(t, s, at.Add(5*time.Second))
	if peak == nil || *peak != 1 || s.ActiveRequests != 0 || s.ActiveClients != 0 {
		t.Fatalf("wrong global peak or release: %+v peak=%v", s, peak)
	}
	last := len(s.Intervals) - 1
	if *s.Clients["alice"].Peaks[last] != 1 || *s.Clients["bob"].Peaks[last] != 1 {
		t.Fatal("client peaks missing")
	}
	if !s.Intervals[last].Partial {
		t.Fatal("current bucket must be partial")
	}
	key := tracker.beginAt("ALICE", at.Add(9*time.Second))
	s = tracker.snapshotAt(at.Add(9 * time.Second))
	if len(s.Clients) != 2 || s.Clients["alice"].ActiveRequests != 1 || s.ActiveClients != 1 {
		t.Fatal("client names were not normalized")
	}
	tracker.endAt(key, at.Add(9*time.Second))
}

func TestActivitySustainedRequestsAndBoundedHistory(t *testing.T) {
	at := activityTestTime()
	tracker := newActivityTracker(at)
	key := tracker.beginAt("held", at.Add(time.Second))
	// No monitor calls during this interval: each intermediate bucket must carry baseline 1.
	tracker.endAt(key, at.Add(35*time.Second))
	s := tracker.snapshotAt(at.Add(45 * time.Second))
	for _, sec := range []int{5, 15, 25, 34} {
		peak := activityPeakAt(t, s, at.Add(time.Duration(sec)*time.Second))
		if peak == nil || *peak != 1 {
			t.Fatalf("sustained peak missing at %d", sec)
		}
	}
	if p := activityPeakAt(t, s, at.Add(44*time.Second)); p == nil || *p != 0 {
		t.Fatal("idle bucket should be known zero")
	}
	s = tracker.snapshotAt(at.Add(7*time.Minute + 5*time.Second))
	if !s.Complete || len(s.Intervals) != 31 || s.Peaks[0] != nil || len(s.Clients) != 0 || len(tracker.buckets) > 31 {
		t.Fatalf("window trim or idle cleanup: %+v", s)
	}
	if !s.Intervals[0].Partial {
		t.Fatal("trimmed oldest interval must be partial")
	}
}

func TestActivityRestartClockRollbackAndForwardJump(t *testing.T) {
	at := activityTestTime()
	tracker := newActivityTracker(at)
	key := tracker.beginAt("ongoing", at.Add(time.Second))
	s := tracker.snapshotAt(at.Add(2 * time.Second))
	if s.Complete || s.Peaks[0] != nil || !s.Since.Equal(at) {
		t.Fatal("startup history was represented as observed zero")
	}
	back := at.Add(-time.Minute + time.Second)
	s = tracker.snapshotAt(back)
	if !s.Since.Equal(back) || s.ActiveRequests != 1 || s.ActiveClients != 1 || s.Complete {
		t.Fatal("clock rollback lost inflight or completeness")
	}
	s = tracker.snapshotAt(at.AddDate(1, 0, 0))
	if !s.Complete || s.ActiveRequests != 1 || len(tracker.buckets) > 31 {
		t.Fatal("large forward jump was not bounded")
	}
	for _, peak := range s.Peaks {
		if peak != nil && *peak != 1 {
			t.Fatal("forward jump lost sustained baseline")
		}
	}
	tracker.endAt(key, at.AddDate(1, 0, 0))
	restarted := newActivityTracker(at.AddDate(1, 0, 0)).snapshotAt(at.AddDate(1, 0, 0))
	if restarted.Complete || restarted.ActiveRequests != 0 || len(restarted.Clients) != 0 {
		t.Fatal("restart inherited runtime history")
	}
}

func TestActivityConcurrentLifecycle(t *testing.T) {
	at := activityTestTime()
	tracker := newActivityTracker(at)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				key := tracker.beginAt("shared", at.Add(time.Second))
				tracker.snapshotAt(at.Add(time.Second))
				tracker.endAt(key, at.Add(time.Second))
			}
		}()
	}
	wg.Wait()
	s := tracker.snapshotAt(at.Add(2 * time.Second))
	if s.ActiveRequests != 0 || s.ActiveClients != 0 || s.Clients["shared"].ActiveRequests != 0 {
		t.Fatal("concurrent lifecycle leaked inflight")
	}
}

func TestServiceActivityLifecycleRelease(t *testing.T) {
	for _, mode := range []string{"success", "failure", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestModelService(t, config.EndpointTypeOpenAI, "http://activity.invalid")
			defer s.Close()
			entered, release := make(chan struct{}), make(chan struct{})
			s.httpClient.Transport = statsRoundTrip(func(r *http.Request) (*http.Response, error) {
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				code := 200
				if mode == "failure" {
					code = 503
				}
				return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)), Request: r}, nil
			})
			req := statsRequest(t, "/v1/chat/completions")
			ctx, cancel := context.WithCancel(req.Context())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); s.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx)) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("upstream not reached")
			}
			if snapshot := s.ActivitySnapshot(); snapshot.ActiveRequests != 1 || snapshot.ActiveClients != 1 || snapshot.Clients["tester"].ActiveRequests != 1 {
				t.Fatal("accepted request missing from runtime")
			}
			if mode == "canceled" {
				cancel()
			} else {
				close(release)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("request did not terminate")
			}
			if snapshot := s.ActivitySnapshot(); snapshot.ActiveRequests != 0 || snapshot.ActiveClients != 0 {
				t.Fatal("request lifecycle leaked runtime count")
			}
		})
	}
}

func TestActivityWebsocketSnapshotNamesOnly(t *testing.T) {
	s := &Service{websocketClients: map[string]int{"Alice\x00secret-one": 1, "ALICE\x00secret-two": 2, "Bob\x00secret-three": 1}}
	snapshot := s.WebsocketActivitySnapshot()
	if len(snapshot) != 2 || snapshot["alice"].Sessions != 3 || snapshot["bob"].Sessions != 1 {
		t.Fatalf("credential identities were not merged: %+v", snapshot)
	}
	for key, item := range snapshot {
		if strings.Contains(key, "secret") || strings.Contains(item.Name, "secret") || strings.ContainsRune(item.Name, 0) {
			t.Fatal("credential leaked into snapshot")
		}
	}
}

func waitActivityCondition(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("activity condition did not settle")
}

func TestServiceActivityNativeWebsocketTurns(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var event map[string]any
		if conn.ReadJSON(&event) != nil {
			return
		}
		close(entered)
		<-release
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "activity-response", "model": "Codex-Upstream", "status": "completed", "usage": map[string]any{"input_tokens": 10, "output_tokens": 2}}})
		_, _, _ = conn.ReadMessage()
	}))
	t.Cleanup(upstream.Close)
	s, server, _ := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	if snapshot := s.ActivitySnapshot(); snapshot.ActiveRequests != 0 || len(snapshot.Clients) != 0 {
		t.Fatal("native upgrade counted as a business request")
	}
	if sessions := s.WebsocketActivitySnapshot(); sessions["tester"].Sessions != 1 {
		t.Fatal("idle websocket session missing")
	}
	if err := conn.WriteJSON(map[string]any{"type": "response.unknown"}); err != nil {
		t.Fatal(err)
	}
	readWebsocketEvent(t, conn)
	if snapshot := s.ActivitySnapshot(); snapshot.ActiveRequests != 0 || len(snapshot.Clients) != 0 {
		t.Fatal("invalid WS message counted as a request")
	}
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("native generation did not reach upstream")
	}
	if snapshot := s.ActivitySnapshot(); snapshot.ActiveRequests != 1 || snapshot.ActiveClients != 1 {
		t.Fatal("native generation missing from in-flight runtime")
	}
	releaseOnce.Do(func() { close(release) })
	if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
		t.Fatalf("unexpected generation terminal: %v", event)
	}
	waitActivityCondition(t, func() bool { return s.ActivitySnapshot().ActiveRequests == 0 })
	if sessions := s.WebsocketActivitySnapshot(); sessions["tester"].Sessions != 1 {
		t.Fatal("completed turn closed idle transport session")
	}
	conn.Close()
	waitActivityCondition(t, func() bool { return len(s.WebsocketActivitySnapshot()) == 0 })
}
