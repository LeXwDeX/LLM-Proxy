//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
)

// A pre-commit 4xx proves the candidate rejected the request without
// processing it; the cost queue must try the next candidate.
func TestPoolFailoverOn400(t *testing.T) {
	p := pool(t)
	alias := uniq("e2e-4xx")
	kA, kB := uniq("sk-4xx-a"), uniq("sk-4xx-b")
	keyedTarget(t, w.urlA, "/openai/v1", alias, []string{kA}, map[string]any{"pool": p})
	keyedTarget(t, w.urlB, "/openai/v1", alias, []string{kB}, map[string]any{"pool": p})
	w.fakeA.SetKeyScenario(kA, &fakellm.Scenario{Status: http.StatusBadRequest, Body: `{"error":{"code":"bad_a","message":"from-a"}}`})
	defer w.fakeA.SetKeyScenario(kA, nil)

	routedToA := false
	for attempt := 0; attempt < 16; attempt++ {
		_, key := client(t, nil, []string{p})
		fromA, fromB := w.fakeA.LastSeq(), w.fakeB.LastSeq()
		resp, body := chat(t, key, alias, "hi")
		if hits(w.fakeA, kA, fromA) == 0 {
			continue
		}
		routedToA = true
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("client saw %d %s; want 200 after 4xx failover", resp.StatusCode, body)
		}
		if hits(w.fakeB, kB, fromB) == 0 {
			t.Fatalf("candidate B was not called after A returned 400")
		}
		break
	}
	if !routedToA {
		t.Fatalf("no fresh client was routed to the 4xx target first")
	}
}

// Exhausting the queue must deliver the first real upstream 4xx, not a
// synthesized 503.
func TestAllCandidates4xxReplaysFirstError(t *testing.T) {
	beforeDequeued := metricInt(t, "requests/dequeued_total")
	beforeExhausted := metricInt(t, "requests/dequeue_exhausted_total")
	beforeReplayed := metricInt(t, "requests/cached_error_replayed_total")
	p := pool(t)
	alias := uniq("e2e-4xxall")
	kA, kB := uniq("sk-4all-a"), uniq("sk-4all-b")
	tgA := keyedTarget(t, w.urlA, "/openai/v1", alias, []string{kA}, map[string]any{"pool": p})
	keyedTarget(t, w.urlB, "/openai/v1", alias, []string{kB}, map[string]any{"pool": p})
	w.fakeA.SetKeyScenario(kA, &fakellm.Scenario{Status: http.StatusBadRequest, Body: `{"error":{"code":"bad_a","message":"from-a"}}`})
	defer w.fakeA.SetKeyScenario(kA, nil)
	w.fakeB.SetKeyScenario(kB, &fakellm.Scenario{Status: http.StatusUnprocessableEntity, Body: `{"error":{"code":"bad_b","message":"from-b"}}`})
	defer w.fakeB.SetKeyScenario(kB, nil)

	for attempt := 0; attempt < 16; attempt++ {
		_, key := client(t, nil, []string{p})
		fromA, fromB := w.fakeA.LastSeq(), w.fakeB.LastSeq()
		resp, body := chat(t, key, alias, "hi")
		if hits(w.fakeA, kA, fromA) == 0 {
			continue
		}
		if hits(w.fakeB, kB, fromB) == 0 {
			t.Fatalf("candidate B was not called before exhaustion")
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("client saw %d %s; want the first 400", resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "from-a") {
			t.Fatalf("replayed body is not the first upstream error: %s", body)
		}
		if got := resp.Header.Get("X-Proxy-Target"); got != tgA {
			t.Fatalf("X-Proxy-Target = %q, want %q", got, tgA)
		}
		if got := metricInt(t, "requests/dequeued_total"); got != beforeDequeued+2 {
			t.Fatalf("dequeued_total = %d, want %d (A and B)", got, beforeDequeued+2)
		}
		if got := metricInt(t, "requests/dequeue_exhausted_total"); got != beforeExhausted+1 {
			t.Fatalf("dequeue_exhausted_total = %d, want %d", got, beforeExhausted+1)
		}
		if got := metricInt(t, "requests/cached_error_replayed_total"); got != beforeReplayed+1 {
			t.Fatalf("cached_error_replayed_total = %d, want %d", got, beforeReplayed+1)
		}
		return
	}
	t.Fatalf("no fresh client was routed to the 4xx target first")
}

// An explicit X-Proxy-Target disables cross-target failover.
func TestExplicitTarget400DoesNotCross(t *testing.T) {
	p := pool(t)
	alias := uniq("e2e-4xxexp")
	kA, kB := uniq("sk-4exp-a"), uniq("sk-4exp-b")
	tgA := keyedTarget(t, w.urlA, "/openai/v1", alias, []string{kA}, map[string]any{"pool": p})
	keyedTarget(t, w.urlB, "/openai/v1", alias, []string{kB}, map[string]any{"pool": p})
	w.fakeA.SetKeyScenario(kA, &fakellm.Scenario{Status: http.StatusBadRequest, Body: `{"error":{"code":"bad_a","message":"from-a"}}`})
	defer w.fakeA.SetKeyScenario(kA, nil)

	_, key := client(t, nil, []string{p})
	fromB := w.fakeB.LastSeq()
	body := map[string]any{"model": alias, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	resp, raw := call(t, key, "POST", "/v1/chat/completions", body, map[string]string{"X-Proxy-Target": tgA})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("explicit target saw %d %s; want 400", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "from-a") {
		t.Fatalf("explicit target error body = %s", raw)
	}
	if hits(w.fakeB, kB, fromB) != 0 {
		t.Fatalf("explicit target crossed to B")
	}
}

// A 400 attempt must not create a usage event; only the successful candidate
// owns the single bundle, and Event.Model is that candidate's upstream name.
func Test400DequeueWritesOnlySuccessfulUsage(t *testing.T) {
	p := pool(t)
	alias := uniq("e2e-4usage")
	kA, kB := uniq("sk-4use-a"), uniq("sk-4use-b")
	keyedTarget(t, w.urlA, "/openai/v1", alias, []string{kA}, map[string]any{
		"pool":           p,
		"model_mappings": []any{map[string]any{"upstream": "up-a-" + alias, "fallback": alias}},
	})
	keyedTarget(t, w.urlB, "/openai/v1", alias, []string{kB}, map[string]any{
		"pool":           p,
		"model_mappings": []any{map[string]any{"upstream": "up-b-" + alias, "fallback": alias}},
	})
	w.fakeA.SetKeyScenario(kA, &fakellm.Scenario{Status: http.StatusBadRequest, Body: `{"error":{"code":"bad_a","message":"from-a"}}`})
	defer w.fakeA.SetKeyScenario(kA, nil)

	for attempt := 0; attempt < 16; attempt++ {
		clientName, key := client(t, nil, []string{p})
		fromA := w.fakeA.LastSeq()
		resp, body := chat(t, key, alias, "hi")
		if hits(w.fakeA, kA, fromA) == 0 {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("client saw %d %s", resp.StatusCode, body)
		}
		events := usageFor(t, clientName, 1)
		if len(events) != 1 {
			t.Fatalf("usage events = %d, want exactly 1", len(events))
		}
		if events[0].Model != "up-b-"+alias {
			t.Fatalf("usage model = %q, want B upstream %q", events[0].Model, "up-b-"+alias)
		}
		return
	}
	t.Fatalf("no fresh client was routed to the 4xx target first")
}

func TestDequeueMetrics(t *testing.T) {
	beforeDequeued := metricInt(t, "requests/dequeued_total")
	before4xx := metricInt(t, "requests/dequeue_4xx_total")
	beforeReplayed := metricInt(t, "requests/cached_error_replayed_total")

	p := pool(t)
	alias := uniq("e2e-4metric")
	kA, kB := uniq("sk-4met-a"), uniq("sk-4met-b")
	keyedTarget(t, w.urlA, "/openai/v1", alias, []string{kA}, map[string]any{"pool": p})
	keyedTarget(t, w.urlB, "/openai/v1", alias, []string{kB}, map[string]any{"pool": p})
	w.fakeA.SetKeyScenario(kA, &fakellm.Scenario{Status: http.StatusBadRequest, Body: `{"error":{"code":"bad_a","message":"from-a"}}`})
	defer w.fakeA.SetKeyScenario(kA, nil)

	for attempt := 0; attempt < 16; attempt++ {
		_, key := client(t, nil, []string{p})
		fromA := w.fakeA.LastSeq()
		resp, _ := chat(t, key, alias, "hi")
		if hits(w.fakeA, kA, fromA) == 0 {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected failover success, got %d", resp.StatusCode)
		}
		break
	}

	if got := metricInt(t, "requests/dequeued_total"); got != beforeDequeued+1 {
		t.Fatalf("dequeued_total = %d, want %d", got, beforeDequeued+1)
	}
	if got := metricInt(t, "requests/dequeue_4xx_total"); got != before4xx+1 {
		t.Fatalf("dequeue_4xx_total = %d, want %d", got, before4xx+1)
	}
	if got := metricInt(t, "requests/cached_error_replayed_total"); got != beforeReplayed {
		t.Fatalf("cached_error_replayed_total changed on a successful failover: %d -> %d", beforeReplayed, got)
	}
}

func metricInt(t *testing.T, path string) int64 {
	t.Helper()
	metrics := readMetrics(t)
	v := dig(metrics, path)
	if v == nil {
		return 0
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("metric %s = %#v, want number", path, v)
	}
	return int64(f)
}

func claudeTarget(t *testing.T, fakeURL, alias, key string, pool string, upstream string) string {
	t.Helper()
	return target(t, map[string]any{
		"endpoint_type": "claude",
		"endpoint":      fakeURL + "/anthropic",
		"api_key":       key,
		"pool":          pool,
		"model_mappings": []any{
			map[string]any{"upstream": upstream, "fallback": alias},
		},
	})
}

// A 4xx on an SSE request is still pre-commit: the client must see only the
// second candidate's stream, never a partial first response.
func TestSSE400FailsOverBeforeCommit(t *testing.T) {
	p := pool(t)
	alias := uniq("e2e-4sse")
	kA, kB := uniq("sk-4sse-a"), uniq("sk-4sse-b")
	claudeTarget(t, w.urlA, alias, kA, p, "up-a-"+alias)
	claudeTarget(t, w.urlB, alias, kB, p, "up-b-"+alias)
	w.fakeA.SetKeyScenario(kA, &fakellm.Scenario{Status: http.StatusBadRequest, Body: `{"type":"error","error":{"type":"invalid_request_error","message":"from-a"}}`})
	defer w.fakeA.SetKeyScenario(kA, nil)

	body := map[string]any{"model": alias, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for attempt := 0; attempt < 16; attempt++ {
		_, key := client(t, nil, []string{p})
		fromA, fromB := w.fakeA.LastSeq(), w.fakeB.LastSeq()
		resp, raw := call(t, key, "POST", "/v1/messages", body, nil)
		if hits(w.fakeA, kA, fromA) == 0 {
			continue
		}
		if hits(w.fakeB, kB, fromB) == 0 {
			t.Fatalf("SSE candidate B was not called after A returned 400")
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("SSE client saw %d %.200s", resp.StatusCode, raw)
		}
		if !strings.Contains(string(raw), "message_start") {
			t.Fatalf("SSE body is not a stream from B: %.200s", raw)
		}
		return
	}
	t.Fatalf("no fresh client was routed to the SSE 4xx target first")
}

// count_tokens is read-only and idempotent, so a pre-commit 4xx is dequeued.
func TestCountTokens400FailsOver(t *testing.T) {
	p := pool(t)
	alias := uniq("e2e-4ct")
	kA, kB := uniq("sk-4ct-a"), uniq("sk-4ct-b")
	claudeTarget(t, w.urlA, alias, kA, p, "up-a-"+alias)
	claudeTarget(t, w.urlB, alias, kB, p, "up-b-"+alias)
	w.fakeA.SetKeyScenario(kA, &fakellm.Scenario{Status: http.StatusBadRequest, Body: `{"type":"error","error":{"type":"invalid_request_error","message":"from-a"}}`})
	defer w.fakeA.SetKeyScenario(kA, nil)

	body := map[string]any{"model": alias,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for attempt := 0; attempt < 16; attempt++ {
		_, key := client(t, nil, []string{p})
		fromA, fromB := w.fakeA.LastSeq(), w.fakeB.LastSeq()
		resp, raw := call(t, key, "POST", "/v1/messages/count_tokens", body, nil)
		if hits(w.fakeA, kA, fromA) == 0 {
			continue
		}
		if hits(w.fakeB, kB, fromB) == 0 {
			t.Fatalf("count_tokens candidate B was not called after A returned 400")
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("count_tokens client saw %d %.200s", resp.StatusCode, raw)
		}
		return
	}
	t.Fatalf("no fresh client was routed to the count_tokens 4xx target first")
}

func imageTarget(t *testing.T, fakeURL, alias, key, pool string) string {
	t.Helper()
	return target(t, map[string]any{
		"endpoint_type": "openai_image",
		"endpoint":      fakeURL + "/openai/v1/images/generations",
		"api_key":       key,
		"pool":          pool,
		"model_mappings": []any{
			map[string]any{"upstream": "gpt-image-1", "fallback": alias},
		},
	})
}

// Image generation: a 4xx rejection may be retried on the next candidate, but
// a 5xx may already have produced (and billed) an image and must stay terminal.
func TestImageFailoverPolicy(t *testing.T) {
	t.Run("400 dequeues", func(t *testing.T) {
		p := pool(t)
		alias := uniq("e2e-4img")
		kA, kB := uniq("sk-4img-a"), uniq("sk-4img-b")
		imageTarget(t, w.urlA, alias, kA, p)
		imageTarget(t, w.urlB, alias, kB, p)
		w.fakeA.SetKeyScenario(kA, &fakellm.Scenario{Status: http.StatusBadRequest, Body: `{"error":{"code":"bad_a","message":"from-a"}}`})
		defer w.fakeA.SetKeyScenario(kA, nil)

		body := map[string]any{"model": alias, "prompt": "cat"}
		for attempt := 0; attempt < 16; attempt++ {
			_, key := client(t, nil, []string{p})
			fromA, fromB := w.fakeA.LastSeq(), w.fakeB.LastSeq()
			resp, raw := call(t, key, "POST", "/v1/images/generations", body, nil)
			if hits(w.fakeA, kA, fromA) == 0 {
				continue
			}
			if hits(w.fakeB, kB, fromB) == 0 {
				t.Fatalf("image candidate B was not called after A returned 400")
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("image client saw %d %.200s", resp.StatusCode, raw)
			}
			return
		}
		t.Fatalf("no fresh client was routed to the image 4xx target first")
	})

	t.Run("500 stays terminal", func(t *testing.T) {
		p := pool(t)
		alias := uniq("e2e-5img")
		kA, kB := uniq("sk-5img-a"), uniq("sk-5img-b")
		imageTarget(t, w.urlA, alias, kA, p)
		imageTarget(t, w.urlB, alias, kB, p)
		w.fakeA.SetKeyScenario(kA, &fakellm.Scenario{Status: http.StatusInternalServerError, Body: `{"error":{"code":"server_a","message":"broke"}}`})
		defer w.fakeA.SetKeyScenario(kA, nil)

		body := map[string]any{"model": alias, "prompt": "cat"}
		for attempt := 0; attempt < 16; attempt++ {
			_, key := client(t, nil, []string{p})
			fromA, fromB := w.fakeA.LastSeq(), w.fakeB.LastSeq()
			resp, raw := call(t, key, "POST", "/v1/images/generations", body, nil)
			if hits(w.fakeA, kA, fromA) == 0 {
				continue
			}
			if hits(w.fakeB, kB, fromB) != 0 {
				t.Fatalf("image 5xx was replayed on B")
			}
			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("image 5xx client saw %d %.200s", resp.StatusCode, raw)
			}
			return
		}
		t.Fatalf("no fresh client was routed to the image 5xx target first")
	})
}

// #187: a deterministic client error is rejected identically by every
// candidate. The first 400 is replayed verbatim and B is never called.
func TestDeterministicClientErrorSkipsFailover(t *testing.T) {
	beforeDequeued := metricInt(t, "requests/dequeued_total")
	before4xx := metricInt(t, "requests/dequeue_4xx_total")
	p := pool(t)
	alias := uniq("e2e-4xxdet")
	kA, kB := uniq("sk-4det-a"), uniq("sk-4det-b")
	tgA := keyedTarget(t, w.urlA, "/openai/v1", alias, []string{kA}, map[string]any{"pool": p})
	keyedTarget(t, w.urlB, "/openai/v1", alias, []string{kB}, map[string]any{"pool": p})
	upstreamBody := `{"error":{"code":"invalid_function_parameters","type":"invalid_request_error","param":"tools[0].parameters","message":"from-a"}}`
	w.fakeA.SetKeyScenario(kA, &fakellm.Scenario{Status: http.StatusBadRequest, Body: upstreamBody})
	defer w.fakeA.SetKeyScenario(kA, nil)

	for attempt := 0; attempt < 16; attempt++ {
		_, key := client(t, nil, []string{p})
		fromA, fromB := w.fakeA.LastSeq(), w.fakeB.LastSeq()
		resp, body := chat(t, key, alias, "hi")
		if hits(w.fakeA, kA, fromA) == 0 {
			continue
		}
		if got := hits(w.fakeA, kA, fromA); got != 1 {
			t.Fatalf("candidate A was called %d times, want 1", got)
		}
		if hits(w.fakeB, kB, fromB) != 0 {
			t.Fatalf("candidate B was called after a deterministic client error")
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("client saw %d %s; want the original 400", resp.StatusCode, body)
		}
		if string(body) != upstreamBody {
			t.Fatalf("client body = %s, want the original upstream error", body)
		}
		if got := resp.Header.Get("X-Proxy-Target"); got != tgA {
			t.Fatalf("X-Proxy-Target = %q, want %q", got, tgA)
		}
		if got := resp.Header.Get("X-Upstream-Request-Id"); got == "" {
			t.Fatalf("X-Upstream-Request-Id missing from the replayed 4xx")
		}
		if got := metricInt(t, "requests/dequeued_total"); got != beforeDequeued {
			t.Fatalf("dequeued_total = %d, want %d (no dequeue)", got, beforeDequeued)
		}
		if got := metricInt(t, "requests/dequeue_4xx_total"); got != before4xx {
			t.Fatalf("dequeue_4xx_total = %d, want %d (no dequeue)", got, before4xx)
		}
		return
	}
	t.Fatalf("no fresh client was routed to the deterministic-error target first")
}
