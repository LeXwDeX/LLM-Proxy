package fakellm

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// streamOnce opens a Chat stream and returns the response plus the time until
// the first data line arrives and the times of the first two data lines.
func streamOnce(t *testing.T, url string, body string, sc *Scenario, fake *Server) (*http.Response, time.Duration, time.Duration) {
	t.Helper()
	if sc != nil {
		fake.SetScenario(sc)
	}
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	scanner := bufio.NewScanner(resp.Body)
	var first, second time.Duration
	for scanner.Scan() {
		if !strings.HasPrefix(scanner.Text(), "data: ") {
			continue
		}
		if first == 0 {
			first = time.Since(start)
			continue
		}
		second = time.Since(start)
		break
	}
	resp.Body.Close()
	return resp, first, second
}

// A zero FirstContentDelayMS keeps the existing behavior: the first event is
// written without any pause after the header.
func TestFirstContentDelayZeroKeepsStreamFast(t *testing.T) {
	fake, ts := newTestServer(t)
	_, first, _ := streamOnce(t, ts.URL+"/v1/chat/completions",
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil, fake)
	if first == 0 {
		t.Fatal("stream produced no data event")
	}
	if first > 500*time.Millisecond {
		t.Fatalf("first event took %v with a zero first-content delay", first)
	}
}

// A non-zero FirstContentDelayMS pauses after the response header and before
// the first event, and only there: the second event follows immediately.
func TestFirstContentDelayPausesBeforeFirstEvent(t *testing.T) {
	fake, ts := newTestServer(t)
	_, first, second := streamOnce(t, ts.URL+"/v1/chat/completions",
		`{"model":"m","stream":true,"chunks":4,"messages":[{"role":"user","content":"hi"}]}`,
		&Scenario{FirstContentDelayMS: 250}, fake)
	defer fake.SetScenario(nil)
	if first < 180*time.Millisecond {
		t.Fatalf("first event after %v, want the 250ms first-content delay", first)
	}
	if first > 2*time.Second {
		t.Fatalf("first event after %v, want about 250ms", first)
	}
	if gap := second - first; gap > 150*time.Millisecond {
		t.Fatalf("second event %v after the first; the delay must apply once", gap)
	}
}

// The delay is applied once per stream even when the scenario also delays
// chunks, and the response header is already available before it elapses.
func TestFirstContentDelayAfterHeader(t *testing.T) {
	fake, ts := newTestServer(t)
	defer fake.SetScenario(nil)
	fake.SetScenario(&Scenario{FirstContentDelayMS: 300, ChunkDelayMS: 5})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"chunks":2,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()
	if got := time.Since(start); got > 200*time.Millisecond {
		t.Fatalf("response header took %v, want it before the first-content delay", got)
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			break
		}
	}
	if got := time.Since(start); got < 240*time.Millisecond {
		t.Fatalf("first event after %v, want the 300ms first-content delay", got)
	}
}

// A client that goes away during the pause ends the handler as canceled, just
// like a delayed chunk.
func TestFirstContentDelayClientCancel(t *testing.T) {
	fake, ts := newTestServer(t)
	fake.SetScenario(&Scenario{FirstContentDelayMS: 3000})
	defer fake.SetScenario(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_, err = bufio.NewReader(resp.Body).ReadString('\n')
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("the canceled stream produced no error")
	}
	if got := time.Since(start); got > 2*time.Second {
		t.Fatalf("cancellation took %v, want well under the 3s delay", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		recs := fake.Requests()
		if len(recs) == 1 && recs[0].Outcome == "canceled" && recs[0].Status == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("journal = %+v, want one canceled record", recs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Non-streaming replies never open an SSE stream, so the delay does not apply.
func TestFirstContentDelayDoesNotAffectNonStream(t *testing.T) {
	fake, ts := newTestServer(t)
	fake.SetScenario(&Scenario{FirstContentDelayMS: 1000})
	defer fake.SetScenario(nil)
	start := time.Now()
	resp, raw := post(t, ts.URL+"/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, raw)
	}
	if got := time.Since(start); got > 500*time.Millisecond {
		t.Fatalf("non-stream reply took %v, want the first-content delay ignored", got)
	}
}

// The request directive and the /_control scenario both reach the field.
func TestFirstContentDelayDirectiveAndControl(t *testing.T) {
	fake, ts := newTestServer(t)
	sc, err := ParseDirective("first_content_delay_ms=120")
	if err != nil || sc == nil || sc.FirstContentDelayMS != 120 {
		t.Fatalf("directive = %+v err=%v, want first_content_delay_ms=120", sc, err)
	}
	// The directive is carried inside the request body by the fake plugin.
	_, first, _ := streamOnce(t, ts.URL+"/v1/chat/completions",
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"[[fake first_content_delay_ms=250]]"}]}`, nil, fake)
	if first < 180*time.Millisecond {
		t.Fatalf("directive delay: first event after %v", first)
	}
	defer fake.SetScenario(nil)
	resp, raw := post(t, ts.URL+"/_control", `{"scenario":{"first_content_delay_ms":250}}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("control: HTTP %d %s", resp.StatusCode, raw)
	}
	_, first, _ = streamOnce(t, ts.URL+"/v1/chat/completions",
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil, fake)
	if first < 180*time.Millisecond {
		t.Fatalf("control delay: first event after %v", first)
	}
}
