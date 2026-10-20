package fakellm

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	fake := New(Options{Name: "t"})
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	return fake, ts
}

func post(t *testing.T, url, body string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp, b
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	return m
}

// dig walks a/b/0/c paths in decoded JSON.
func dig(v any, path string) any {
	for _, seg := range strings.Split(path, "/") {
		switch x := v.(type) {
		case map[string]any:
			v = x[seg]
		case []any:
			i := 0
			for _, r := range seg {
				i = i*10 + int(r-'0')
			}
			if i >= len(x) {
				return nil
			}
			v = x[i]
		default:
			return nil
		}
	}
	return v
}

type sseEvent struct {
	name string
	data string
}

func readSSE(t *testing.T, b []byte) []sseEvent {
	t.Helper()
	var out []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.data != "" || cur.name != "" {
				out = append(out, cur)
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, "event: "):
			cur.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
	return out
}

func TestChatUsageIsDeterministicAndOverridable(t *testing.T) {
	_, ts := newTestServer(t)
	body := `{"model":"m","messages":[{"role":"user","content":"hello world"}]}`
	_, a := post(t, ts.URL+"/v1/chat/completions", body, nil)
	_, b := post(t, ts.URL+"/v1/chat/completions", body, nil)
	ma, mb := decode(t, a), decode(t, b)
	if dig(ma, "choices/0/message/content") != dig(mb, "choices/0/message/content") {
		t.Fatalf("reply not deterministic: %v vs %v", dig(ma, "choices/0/message/content"), dig(mb, "choices/0/message/content"))
	}
	if got := dig(ma, "usage/prompt_tokens"); got != float64(EstimateTokens("hello world ")) {
		t.Fatalf("prompt_tokens = %v", got)
	}
	_, c := post(t, ts.URL+"/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"[[fake usage.in=100 usage.out=20 usage.cached=30 usage.reasoning=5]]"}]}`, nil)
	mc := decode(t, c)
	for path, want := range map[string]float64{"usage/prompt_tokens": 100, "usage/completion_tokens": 20, "usage/total_tokens": 120,
		"usage/prompt_tokens_details/cached_tokens": 30, "usage/completion_tokens_details/reasoning_tokens": 5} {
		if got := dig(mc, path); got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
}

func TestChatStreamUsagePlacementPerVendor(t *testing.T) {
	_, ts := newTestServer(t)
	cases := []struct {
		path, extra string
		wantUsage   bool
		usageOnLast bool // usage on a trailing chunk with empty choices
	}{
		{"/openai/v1/chat/completions", "", false, false},
		{"/openai/v1/chat/completions", `,"stream_options":{"include_usage":true}`, true, true},
		{"/compatible-mode/v1/chat/completions", `,"stream_options":{"include_usage":true}`, true, true},
		{"/compatible-mode/v1/chat/completions", "", false, false},
		{"/deepseek/chat/completions", "", true, false},
		{"/api/paas/v4/chat/completions", "", true, false},
		{"/openrouter/api/v1/chat/completions", "", true, false},
		{"/v1/chat/completions", "", true, false}, // legacy mock behaviour
	}
	for _, tc := range cases {
		_, b := post(t, ts.URL+tc.path, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]`+tc.extra+`}`, nil)
		evs := readSSE(t, b)
		if len(evs) < 3 || evs[len(evs)-1].data != "[DONE]" {
			t.Fatalf("%s: stream not terminated: %q", tc.path, b)
		}
		var usageEv map[string]any
		for _, ev := range evs[:len(evs)-1] {
			m := decode(t, []byte(ev.data))
			if m["usage"] != nil {
				usageEv = m
			}
		}
		if (usageEv != nil) != tc.wantUsage {
			t.Fatalf("%s%s: usage present=%v want %v", tc.path, tc.extra, usageEv != nil, tc.wantUsage)
		}
		if tc.usageOnLast && len(usageEv["choices"].([]any)) != 0 {
			t.Fatalf("%s: include_usage chunk must have empty choices", tc.path)
		}
	}
}

func TestVendorUsageFields(t *testing.T) {
	_, ts := newTestServer(t)
	d := `[[fake usage.in=100 usage.out=10 usage.cached=40 usage.cw5m=20]]`
	_, b := post(t, ts.URL+"/deepseek/chat/completions", `{"messages":[{"role":"user","content":"`+d+`"}]}`, nil)
	m := decode(t, b)
	if dig(m, "usage/prompt_cache_hit_tokens") != 40.0 || dig(m, "usage/prompt_cache_miss_tokens") != 60.0 {
		t.Fatalf("deepseek usage = %v", m["usage"])
	}
	_, b = post(t, ts.URL+"/compatible-mode/v1/chat/completions", `{"messages":[{"role":"user","content":"`+d+`"}]}`, nil)
	if got := dig(decode(t, b), "usage/prompt_tokens_details/cache_creation_input_tokens"); got != 20.0 {
		t.Fatalf("qwen cache_creation_input_tokens = %v", got)
	}
	_, b = post(t, ts.URL+"/openrouter/api/v1/chat/completions", `{"messages":[{"role":"user","content":"`+d+`"}]}`, nil)
	if got := dig(decode(t, b), "usage/prompt_tokens_details/cache_write_tokens"); got != 20.0 {
		t.Fatalf("openrouter cache_write_tokens = %v", got)
	}
	_, b = post(t, ts.URL+"/v1/messages", `{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":"[[fake usage.in=100 usage.out=10 usage.cached=40 usage.cw5m=20 usage.cw1h=10]]"}]}`, nil)
	m = decode(t, b)
	for path, want := range map[string]float64{"usage/input_tokens": 30, "usage/cache_read_input_tokens": 40,
		"usage/cache_creation_input_tokens": 30, "usage/cache_creation/ephemeral_5m_input_tokens": 20, "usage/cache_creation/ephemeral_1h_input_tokens": 10} {
		if got := dig(m, path); got != want {
			t.Errorf("anthropic %s = %v, want %v", path, got, want)
		}
	}
	_, b = post(t, ts.URL+"/v1beta/models/gemini-x:generateContent", `{"contents":[{"parts":[{"text":"[[fake usage.in=50 usage.out=10 usage.reasoning=4 usage.cached=5]]"}]}]}`, nil)
	m = decode(t, b)
	if dig(m, "usageMetadata/candidatesTokenCount") != 6.0 || dig(m, "usageMetadata/thoughtsTokenCount") != 4.0 || dig(m, "usageMetadata/cachedContentTokenCount") != 5.0 {
		t.Fatalf("gemini usage = %v", m["usageMetadata"])
	}
}

func TestErrorPresetsUseProviderEnvelopes(t *testing.T) {
	_, ts := newTestServer(t)
	cases := []struct {
		path, preset string
		status       int
		check        func(m map[string]any) bool
	}{
		{"/v1/chat/completions", "invalid_key", 401, func(m map[string]any) bool { return dig(m, "error/code") == "invalid_api_key" }},
		{"/v1/messages", "billing", 402, func(m map[string]any) bool {
			return m["type"] == "error" && dig(m, "error/type") == "billing_error"
		}},
		{"/v1beta/models/g:generateContent", "gemini_invalid_key", 400, func(m map[string]any) bool { return dig(m, "error/status") == "API_KEY_INVALID" }},
		{"/api/paas/v4/chat/completions", "glm_1308", 429, func(m map[string]any) bool {
			return dig(m, "error/code") == "1308" && strings.Contains(dig(m, "error/message").(string), "重置")
		}},
		{"/api/anthropic/v1/messages", "glm_1113", 429, func(m map[string]any) bool {
			return m["type"] == "error" && dig(m, "error/code") == "1113"
		}},
		{"/compatible-mode/v1/chat/completions", "qwen_arrearage", 400, func(m map[string]any) bool { return dig(m, "error/code") == "Arrearage" }},
		{"/deepseek/chat/completions", "deepseek_402", 402, func(m map[string]any) bool { return dig(m, "error/message") == "Insufficient Balance" }},
		{"/openrouter/api/v1/chat/completions", "openrouter_402", 402, func(m map[string]any) bool { return dig(m, "error/code") == 402.0 }},
		{"/kimi/v1/chat/completions", "kimi_quota", 429, func(m map[string]any) bool { return dig(m, "error/type") == "exceeded_current_quota_error" }},
		{"/minimax/v1/chat/completions", "minimax_1008", 200, func(m map[string]any) bool { return dig(m, "base_resp/status_code") == 1008.0 }},
	}
	for _, tc := range cases {
		resp, b := post(t, ts.URL+tc.path, `{"model":"m","messages":[],"contents":[]}`, map[string]string{"X-Fake-Scenario": "preset=" + tc.preset})
		if resp.StatusCode != tc.status || !tc.check(decode(t, b)) {
			t.Errorf("%s %s: status %d body %s", tc.path, tc.preset, resp.StatusCode, b)
		}
	}
}

func TestKeyRulesConsumeTimesAndLegacyControl(t *testing.T) {
	fake, ts := newTestServer(t)
	fake.SetKeyScenario("sk-bad", &Scenario{Status: 429, RetryAfter: "1", Times: 2})
	for i, want := range []int{429, 429, 200} {
		resp, _ := post(t, ts.URL+"/v1/chat/completions", `{"messages":[]}`, map[string]string{"Authorization": "Bearer sk-bad"})
		if resp.StatusCode != want {
			t.Fatalf("request %d: status %d want %d", i, resp.StatusCode, want)
		}
		if want == 429 && resp.Header.Get("Retry-After") != "1" {
			t.Fatalf("missing Retry-After")
		}
	}
	resp, _ := post(t, ts.URL+"/v1/chat/completions", `{"messages":[]}`, map[string]string{"Authorization": "Bearer sk-good"})
	if resp.StatusCode != 200 {
		t.Fatalf("other key affected: %d", resp.StatusCode)
	}
	post(t, ts.URL+"/_control", `{"mode":"fail500"}`, nil)
	resp, _ = post(t, ts.URL+"/v1/chat/completions", `{"messages":[]}`, nil)
	if resp.StatusCode != 500 {
		t.Fatalf("legacy fail500: %d", resp.StatusCode)
	}
	_, stats := post(t, ts.URL+"/_control", `{"mode":"ok"}`, nil)
	m := decode(t, stats)
	if m["mode"] != "ok" || m["instance"] != "t" || m["total"].(float64) < 5 {
		t.Fatalf("stats = %v", m)
	}
	recs := fake.Requests()
	if recs[0].Auth != "bearer" || recs[0].Key != KeyFingerprint("sk-bad") {
		t.Fatalf("journal = %+v", recs[0])
	}
}

func TestStreamFaults(t *testing.T) {
	_, ts := newTestServer(t)
	// cut_after drops the connection without a terminator.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"stream":true,"messages":[{"role":"user","content":"[[fake cut_after=2]]"}]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil || strings.Contains(string(b), "[DONE]") {
		t.Fatalf("cut stream finished cleanly: err=%v body=%q", err, b)
	}
	// abort fails before any response.
	_, err = http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"x":"[[fake abort]]"}`))
	if err == nil {
		t.Fatal("abort produced a response")
	}
	// GLM historical 200 + SSE rate limit.
	resp2, b2 := post(t, ts.URL+"/api/anthropic/v1/messages", `{"stream":true,"x":"[[fake sse_error preset=glm_1308]]"}`, nil)
	if resp2.StatusCode != 200 || !strings.Contains(string(b2), "event: error") || !strings.Contains(string(b2), `"type":"rate_limit_error"`) || !strings.Contains(string(b2), `"code":"1308"`) {
		t.Fatalf("sse_error: %d %s", resp2.StatusCode, b2)
	}
	// Responses mid-stream failure.
	_, b3 := post(t, ts.URL+"/v1/responses", `{"stream":true,"input":"[[fake stream_error=1]]"}`, nil)
	evs := readSSE(t, b3)
	if last := evs[len(evs)-1]; last.name != "response.failed" {
		t.Fatalf("last event %q", last.name)
	}
	// Anthropic in-stream error.
	_, b4 := post(t, ts.URL+"/v1/messages", `{"stream":true,"messages":[{"role":"user","content":"[[fake stream_error=1]]"}]}`, nil)
	if !strings.Contains(string(b4), "event: error") || strings.Contains(string(b4), "message_stop") {
		t.Fatalf("anthropic stream_error: %s", b4)
	}
}

func TestResponsesHTTPStoreAndPrevious(t *testing.T) {
	_, ts := newTestServer(t)
	_, b := post(t, ts.URL+"/v1/responses", `{"model":"m","input":"hi","service_tier":"priority"}`, nil)
	first := decode(t, b)
	if first["status"] != "completed" || first["service_tier"] != "priority" || dig(first, "usage/input_tokens") == nil {
		t.Fatalf("response = %s", b)
	}
	id := first["id"].(string)
	resp, b := post(t, ts.URL+"/v1/responses", `{"model":"m","input":"next","previous_response_id":"`+id+`"}`, nil)
	if resp.StatusCode != 200 || decode(t, b)["previous_response_id"] != id {
		t.Fatalf("continuation: %d %s", resp.StatusCode, b)
	}
	resp, _ = post(t, ts.URL+"/v1/responses", `{"input":"x","previous_response_id":"resp_missing"}`, nil)
	if resp.StatusCode != 400 {
		t.Fatalf("unknown previous: %d", resp.StatusCode)
	}
	get, err := http.Get(ts.URL + "/v1/responses/" + id)
	if err != nil || get.StatusCode != 200 {
		t.Fatalf("retrieve: %v %v", err, get)
	}
	get.Body.Close()
	_, b = post(t, ts.URL+"/v1/responses", `{"model":"m","input":"hi","stream":true}`, nil)
	evs := readSSE(t, b)
	if evs[0].name != "response.created" || evs[len(evs)-1].name != "response.completed" {
		t.Fatalf("stream events: first %q last %q", evs[0].name, evs[len(evs)-1].name)
	}
	if dig(decode(t, []byte(evs[len(evs)-1].data)), "response/usage/output_tokens") == nil {
		t.Fatalf("completed lacks usage")
	}
}

func TestResponsesWebSocket(t *testing.T) {
	fake, ts := newTestServer(t)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer sk-ws"}, "X-Ws-Smoke": {"yes"}})
	if err != nil {
		t.Fatalf("dial: %v %v", err, resp)
	}
	defer conn.Close()
	turn := func(payload string) []map[string]any {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
			t.Fatal(err)
		}
		var evs []map[string]any
		for {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			var ev map[string]any
			if err := conn.ReadJSON(&ev); err != nil {
				t.Fatalf("read: %v", err)
			}
			evs = append(evs, ev)
			switch ev["type"] {
			case "response.completed", "response.failed", "error":
				return evs
			}
		}
	}
	evs := turn(`{"type":"response.create","model":"mock-model","input":"hi"}`)
	if last := evs[len(evs)-1]; last["type"] != "error" || last["status"] != 400.0 {
		t.Fatalf("string input over WS must be rejected like the real upstream: %v", last)
	}
	evs = turn(`{"type":"response.create","model":"mock-model","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"service_tier":"ultrafast"}`)
	done := evs[len(evs)-1]
	if done["type"] != "response.completed" {
		t.Fatalf("turn 1 ended with %v", done)
	}
	id := dig(done, "response/id").(string)
	evs = turn(`{"type":"response.create","model":"mock-model","input":[],"previous_response_id":"` + id + `","store":false}`)
	if evs[len(evs)-1]["type"] != "response.completed" {
		t.Fatalf("turn 2 = %v", evs[len(evs)-1])
	}
	evs = turn(`{"type":"response.create","input":[],"previous_response_id":"nope"}`)
	if last := evs[len(evs)-1]; last["type"] != "error" || last["status"] != 400.0 {
		t.Fatalf("bad previous = %v", last)
	}
	evs = turn(`{"type":"response.create","input":[{"role":"user","content":"[[fake preset=rate_limit]]"}]}`)
	if last := evs[len(evs)-1]; last["type"] != "error" || last["status"] != 429.0 || dig(last, "error/type") != "rate_limit_error" {
		t.Fatalf("error frame = %v", last)
	}
	_, stats := post(t, ts.URL+"/_stats", "", nil)
	_ = stats
	conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		active, reqs, tiers, hdr := fake.stats.WSActive, fake.stats.WSRequests, fake.stats.WSTiers["ultrafast"], fake.stats.WSHeaderOK
		fake.mu.Unlock()
		if active == 0 {
			if reqs != 4 || tiers != 1 || hdr != 1 { // the rejected string-input frame is not counted
				t.Fatalf("ws stats reqs=%d tiers=%d hdr=%d", reqs, tiers, hdr)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Handshake rejection.
	_, resp, err = websocket.DefaultDialer.Dial(wsURL, http.Header{"X-Fake-Scenario": {"ws_status=426"}})
	if err == nil || resp == nil || resp.StatusCode != 426 {
		t.Fatalf("ws_status: err=%v resp=%v", err, resp)
	}
}

func TestImagesOfficialShapes(t *testing.T) {
	_, ts := newTestServer(t)
	_, b := post(t, ts.URL+"/v1/images/generations", `{"model":"gpt-image-1","prompt":"cat","n":2}`, nil)
	m := decode(t, b)
	if len(m["data"].([]any)) != 2 || dig(m, "usage/input_tokens") == nil || dig(m, "usage/output_tokens") != 544.0 {
		t.Fatalf("gpt-image = %s", b)
	}
	_, b = post(t, ts.URL+"/v1/images/generations", `{"model":"dall-e-3","prompt":"cat"}`, nil)
	if m := decode(t, b); m["usage"] != nil {
		t.Fatalf("dall-e must not report usage: %s", b)
	}
	_, b = post(t, ts.URL+"/v1beta/models/gemini-2.5-flash-image:generateContent", `{"contents":[{"parts":[{"text":"cat"}]}]}`, nil)
	if !strings.Contains(string(b), "inlineData") || !strings.Contains(string(b), `"modality":"IMAGE"`) {
		t.Fatalf("gemini image = %s", b)
	}
}

func TestDirectiveErrorsAreLoud(t *testing.T) {
	_, ts := newTestServer(t)
	resp, b := post(t, ts.URL+"/v1/chat/completions", `{"x":"[[fake bogus=1]]"}`, nil)
	if resp.StatusCode != 400 || !strings.Contains(string(b), "unknown key") {
		t.Fatalf("bad directive: %d %s", resp.StatusCode, b)
	}
}

func TestLegacyModesOnlyAffectPOSTLikeMockProvider(t *testing.T) {
	_, ts := newTestServer(t)
	post(t, ts.URL+"/_control", `{"mode":"fail500"}`, nil)
	get, err := http.Get(ts.URL + "/v1/models")
	if err != nil || get.StatusCode != 200 {
		t.Fatalf("GET /v1/models under fail500: %v %v", err, get)
	}
	get.Body.Close()
	resp, b := post(t, ts.URL+"/v1/chat/completions", `{"messages":[]}`, nil)
	if resp.StatusCode != 500 || dig(decode(t, b), "error/type") != "mock_error" {
		t.Fatalf("POST under fail500: %d %s", resp.StatusCode, b)
	}
}

func TestQueryCredentialsAreRedacted(t *testing.T) {
	fake, ts := newTestServer(t)
	post(t, ts.URL+"/v1beta/models/g:generateContent?key=secret-value&alt=json", `{"contents":[]}`, nil)
	recs := fake.Requests()
	rec := recs[len(recs)-1]
	if strings.Contains(rec.Query, "secret-value") || rec.Key != KeyFingerprint("secret-value") {
		t.Fatalf("record leaks or misses key: %+v", rec)
	}
}
