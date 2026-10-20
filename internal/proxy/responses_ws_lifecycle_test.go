package proxy

// Responses WebSocket 生命周期独立回归测试。
// 仅使用本地 mock 上游，不调用真实模型；复用 responses_ws_test.go 的
// newWSTestService / wsTestRequest helper，不复制其实现。
// 覆盖：非流式 JSON final、客户端隔离与 previous_response_id 边界、
// terminal 失败事件不计成功、UpdateTargets 连接退役、生成中取消、并发不串包。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/coder/websocket"
)

const (
	wsLifecycleMarkerA = "ws-lifecycle-AAA"
	wsLifecycleMarkerB = "ws-lifecycle-BBB"
)

// syncWSRecorder 是并发安全的用量记录器；并发请求测试不能使用无锁的 adapterEventRecorder。
type syncWSRecorder struct {
	mu     sync.Mutex
	events []usage.Event
}

func (r *syncWSRecorder) Record(e usage.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *syncWSRecorder) snapshot() []usage.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]usage.Event(nil), r.events...)
}

// wsUpstreamMode 控制 mock 上游对每个 response.create 的事件脚本。
type wsUpstreamMode int

const (
	wsModeComplete            wsUpstreamMode = iota // created → delta → completed(完整 usage)
	wsModeIncompleteWithUsage                       // created → incomplete(带 final usage)
	wsModeFailedNoUsage                             // created → failed(无 usage)
	wsModeHoldAfterCreated                          // created 后挂起，模拟生成中的上游
	wsModeEchoInput                                 // 把 input 回显进 delta 与 completed output
)

// wsLifecycleUpstream 是可复用的本地 mock 上游：全部行为通过计数器暴露，
// server goroutine 内不调用 t.Fatal/t.Errorf，避免测试结束后触发 panic。
type wsLifecycleUpstream struct {
	mode wsUpstreamMode
	// authKey 是该 mock 接受的上游 Bearer key；默认与 newWSTestService 的固定 key 一致，
	// UpdateTargets 场景的新 endpoint 使用自己的 key。
	authKey string

	upgrades, posts, creates, badCreates atomic.Int32

	mu          sync.Mutex
	previousIDs []string // 每个 response.create 携带的 previous_response_id（缺省为 ""）
	authHeaders []string // 每次握手使用的 Authorization 头
}

func (u *wsLifecycleUpstream) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		u.posts.Add(1)
		http.Error(w, "HTTP was used", 500)
		return
	}
	if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer "+u.authKey {
		http.Error(w, "wrong handshake", 400)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	u.upgrades.Add(1)
	for {
		_, msg, err := c.Read(r.Context())
		if err != nil {
			return
		}
		var payload map[string]any
		if json.Unmarshal(msg, &payload) != nil || payload["type"] != "response.create" || payload["model"] != "UpstreamCase" || payload["stream"] != nil {
			u.badCreates.Add(1)
			return
		}
		previous, _ := payload["previous_response_id"].(string)
		u.mu.Lock()
		u.previousIDs = append(u.previousIDs, previous)
		u.mu.Unlock()
		n := u.creates.Add(1)
		input := wsFirstInputText(payload["input"])
		for _, event := range u.eventScript(fmt.Sprintf("resp_%d", n), input) {
			if c.Write(r.Context(), websocket.MessageText, []byte(event)) != nil {
				return
			}
		}
		if u.mode == wsModeHoldAfterCreated {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
	}
}

func (u *wsLifecycleUpstream) eventScript(id, input string) []string {
	created := `{"type":"response.created","response":{"id":"` + id + `","status":"in_progress"}}`
	switch u.mode {
	case wsModeHoldAfterCreated:
		return []string{created}
	case wsModeIncompleteWithUsage:
		return []string{created,
			`{"type":"response.incomplete","response":{"id":"` + id + `","model":"UpstreamCase","status":"incomplete","usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":2}}}}`}
	case wsModeFailedNoUsage:
		return []string{created,
			`{"type":"response.failed","response":{"id":"` + id + `","model":"UpstreamCase","status":"failed","error":{"code":"server_error","message":"generation failed"}}}`}
	case wsModeEchoInput:
		return []string{created,
			`{"type":"response.output_text.delta","delta":"` + input + `","item_id":"msg_1"}`,
			`{"type":"response.output_text.delta","delta":"` + input + `","item_id":"msg_1"}`,
			`{"type":"response.completed","response":{"id":"` + id + `","model":"UpstreamCase","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"` + input + `"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`}
	default:
		return []string{created,
			`{"type":"response.output_text.delta","delta":"hi","item_id":"msg_1"}`,
			`{"type":"response.completed","response":{"id":"` + id + `","model":"UpstreamCase","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}}}`}
	}
}

func (u *wsLifecycleUpstream) snapshotPreviousIDs() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.previousIDs...)
}

func (u *wsLifecycleUpstream) hasAuthHeader(value string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, h := range u.authHeaders {
		if h == value {
			return true
		}
	}
	return false
}

// noteAuth 在请求进入 serve 之前记录 Authorization，供断言核对新 endpoint 的 key。
func (u *wsLifecycleUpstream) noteAuth(r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.authHeaders = append(u.authHeaders, r.Header.Get("Authorization"))
}

func newWSLifecycleUpstreamServerWithKey(t *testing.T, mode wsUpstreamMode, authKey string) (*wsLifecycleUpstream, string) {
	t.Helper()
	u := &wsLifecycleUpstream{mode: mode, authKey: authKey}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.noteAuth(r)
		u.serve(w, r)
	}))
	t.Cleanup(srv.Close)
	return u, srv.URL
}

func newWSLifecycleUpstreamServer(t *testing.T, mode wsUpstreamMode) (*wsLifecycleUpstream, string) {
	return newWSLifecycleUpstreamServerWithKey(t, mode, "upstream-test-key")
}

// newWSLifecycleHarness 组装"mock 上游 + Service + 用量记录器"的最小测试环境。
func newWSLifecycleHarness(t *testing.T, mode wsUpstreamMode) (*Service, *syncWSRecorder, *wsLifecycleUpstream) {
	t.Helper()
	u, endpoint := newWSLifecycleUpstreamServer(t, mode)
	s := newWSTestService(t, endpoint, true)
	rec := &syncWSRecorder{}
	s.SetUsageRecorder(rec)
	return s, rec, u
}

func wsPrincipalFor(t *testing.T, name string) *auth.Principal {
	t.Helper()
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients(name, name)); err != nil {
		t.Fatal(err)
	}
	principal, ok := store.Authenticate(name)
	if !ok {
		t.Fatalf("authenticate client %q failed", name)
	}
	return principal
}

// wsServeRequest 以指定 principal（可选 ctx）发起一次代理请求；可在 goroutine 中使用，
// 不得调用 t 的任何方法，结果由主 goroutine 断言。
func wsServeRequest(s *Service, principal *auth.Principal, ctx context.Context, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if ctx != nil {
		r = r.WithContext(ctx)
	}
	r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func wsWaitForZeroInflight(t *testing.T, s *Service) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.TargetStatuses(time.Now())[0].InFlight == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("target still reports InFlight=%d after deadline", s.TargetStatuses(time.Now())[0].InFlight)
}

func wsRequireNoProtocolViolations(t *testing.T, u *wsLifecycleUpstream, wantCreates int32) {
	t.Helper()
	if u.badCreates.Load() != 0 || u.posts.Load() != 0 || u.creates.Load() != wantCreates {
		t.Fatalf("upstream violations: badCreates=%d posts=%d creates=%d wantCreates=%d",
			u.badCreates.Load(), u.posts.Load(), u.creates.Load(), wantCreates)
	}
}

// 场景 1：非流式请求（无 stream 字段）经 WS 收到 response.completed 后，
// 代理返回单个 JSON final 对象（含 id 与 usage），且用量完整入账。
func TestResponsesWSNonStreamJSONFinalWithUsage(t *testing.T) {
	s, rec, u := newWSLifecycleHarness(t, wsModeComplete)
	// Exact upstream handshake counts below; background spare lanes are covered
	// by responses_ws_prewarm_test.go.
	s.responsesWS.prewarm = false
	w := wsTestRequest(t, s, "client-a", "/v1/responses", `{"model":"openai-test","input":"hi"}`)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type=%q, want application/json", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"id":"resp_1"`) || !strings.Contains(body, `"input_tokens":10`) || strings.Contains(body, "event: ") {
		t.Fatalf("unexpected non-stream body: %s", body)
	}
	events := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("usage events=%d, want 1: %+v", len(events), events)
	}
	e := events[0]
	if e.InputTokens != 8 || e.CachedTokens != 2 || e.OutputTokens != 5 || e.CostIncomplete {
		t.Fatalf("accounting: %+v", e)
	}
	if metrics := s.MetricsSnapshot(); metrics.TotalSuccess != 1 {
		t.Fatalf("TotalSuccess=%d, want 1", metrics.TotalSuccess)
	}
	wsRequireNoProtocolViolations(t, u, 1)
	if u.upgrades.Load() != 1 {
		t.Fatalf("upgrades=%d, want 1", u.upgrades.Load())
	}
	wsWaitForZeroInflight(t, s)
}

// 场景 2：previous_response_id 只能被同一客户端在同一 target 的活跃连接上解析；
// 其他客户端携带受害者的 previous_response_id 必须得到 400，且不触碰受害者连接。
func TestResponsesWSPreviousIDCrossClientIsolation(t *testing.T) {
	s, _, u := newWSLifecycleHarness(t, wsModeComplete)
	// Exact upstream handshake counts below; background spare lanes are covered
	// by responses_ws_prewarm_test.go.
	s.responsesWS.prewarm = false
	if w := wsTestRequest(t, s, "client-a", "/v1/responses", `{"model":"openai-test","input":"hi","stream":true}`); w.Code != 200 {
		t.Fatalf("first request: %d %s", w.Code, w.Body.String())
	}
	w := wsTestRequest(t, s, "client-b", "/v1/responses", `{"model":"openai-test","input":"hi","stream":true,"previous_response_id":"resp_1"}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "previous_response_not_found") {
		t.Fatalf("cross-client previous: %d %s", w.Code, w.Body.String())
	}
	// 受害者连接未被复用写入：仍只有 client-a 的第一次 create，且没有任何 HTTP 回放。
	wsRequireNoProtocolViolations(t, u, 1)
	if u.upgrades.Load() != 1 {
		t.Fatalf("upgrades=%d, want 1", u.upgrades.Load())
	}
}

func TestResponsesWSRequestFailureDoesNotMuteHealthyTarget(t *testing.T) {
	for _, tc := range []struct {
		name, terminal string
		first, muted   bool
	}{
		{"incomplete", `{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":8,"output_tokens":5}}}`, false, false},
		{"invalid_request", `{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"type":"invalid_request_error","code":"invalid_input"},"usage":{"input_tokens":8,"output_tokens":5}}}`, false, false},
		{"first_error_400", `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"invalid_input"}}`, true, false},
		{"server_error", `{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"type":"server_error"},"usage":{"input_tokens":8,"output_tokens":5}}}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var creates, posts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					w.WriteHeader(500)
					return
				}
				c, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer c.CloseNow()
				for {
					if _, _, err := c.Read(r.Context()); err != nil {
						return
					}
					n := creates.Add(1)
					if n == 1 && !tc.first {
						c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`))
					}
					terminal := tc.terminal
					if n > 1 {
						terminal = `{"type":"response.completed","response":{"id":"resp_2","status":"completed","usage":{"input_tokens":8,"output_tokens":5}}}`
					}
					if c.Write(r.Context(), websocket.MessageText, []byte(terminal)) != nil {
						return
					}
				}
			}))
			defer upstream.Close()
			s := newWSTestService(t, upstream.URL, true)
			recorder := &syncWSRecorder{}
			s.SetUsageRecorder(recorder)
			wsTestRequest(t, s, "client-a", "/v1/responses", `{"model":"openai-test","input":"hi","stream":true}`)
			wsWaitForZeroInflight(t, s)
			state, _ := s.targetByName("openai")
			if state.IsMuted(time.Now()) != tc.muted || s.metrics.totalSuccess.Load() != 0 || s.metrics.totalFailures.Load() != 1 {
				t.Fatalf("muted=%t want=%t metrics=%+v", state.IsMuted(time.Now()), tc.muted, s.MetricsSnapshot())
			}
			if !tc.first {
				if events := recorder.snapshot(); len(events) != 1 || events[0].InputTokens != 8 || events[0].OutputTokens != 5 || events[0].CostIncomplete {
					t.Fatalf("lost terminal usage: %+v", events)
				}
			}
			if !tc.muted {
				if w := wsTestRequest(t, s, "client-b", "/v1/responses", `{"model":"openai-test","input":"valid","stream":true}`); w.Code != 200 || !strings.Contains(w.Body.String(), "response.completed") {
					t.Fatalf("healthy target unavailable after request error: %d", w.Code)
				}
			}
			wsWaitForZeroInflight(t, s)
			want := int32(1)
			if !tc.muted {
				want = 2
			}
			if creates.Load() != want || posts.Load() != 0 {
				t.Fatalf("generation replay or lost request: creates=%d want=%d posts=%d", creates.Load(), want, posts.Load())
			}
		})
	}
}

// 场景 3：同客户端带 previous_response_id 的请求在同一 socket 上续接且透传原值；
// 不带 previous 的请求继续复用该 socket 且代理不得自动注入 previous_response_id。
func TestResponsesWSPreviousContinuesSameSocketWithoutInjection(t *testing.T) {
	s, _, u := newWSLifecycleHarness(t, wsModeComplete)
	// Exact upstream handshake counts below; background spare lanes are covered
	// by responses_ws_prewarm_test.go.
	s.responsesWS.prewarm = false
	if w := wsTestRequest(t, s, "client-a", "/v1/responses", `{"model":"openai-test","input":"hi","stream":true}`); w.Code != 200 {
		t.Fatalf("first request: %d %s", w.Code, w.Body.String())
	}
	if w := wsTestRequest(t, s, "client-a", "/v1/responses", `{"model":"openai-test","input":"again","stream":true,"previous_response_id":"resp_1"}`); w.Code != 200 {
		t.Fatalf("continuation: %d %s", w.Code, w.Body.String())
	}
	if w := wsTestRequest(t, s, "client-a", "/v1/responses", `{"model":"openai-test","input":"fresh","stream":true}`); w.Code != 200 {
		t.Fatalf("fresh request: %d %s", w.Code, w.Body.String())
	}
	wsRequireNoProtocolViolations(t, u, 3)
	// The continuation stays on the first socket; the fresh request must not
	// evict resp_2 from it while another lane still fits.
	if u.upgrades.Load() != 2 {
		t.Fatalf("upgrades=%d, want continuation on one socket and a fresh lane", u.upgrades.Load())
	}
	previous := u.snapshotPreviousIDs()
	if len(previous) != 3 || previous[0] != "" || previous[1] != "resp_1" || previous[2] != "" {
		t.Fatalf("previous_response_id sequence: %q", previous)
	}
}

// 场景 4：response.incomplete / response.failed 是 terminal 事件但不计成功；
// 带 final usage 的记录完整用量，缺 usage 的记录标记为用量不完整。
func TestResponsesWSFailedAndIncompleteTerminalsNotCountedSuccess(t *testing.T) {
	t.Run("incomplete_with_usage", func(t *testing.T) {
		s, rec, u := newWSLifecycleHarness(t, wsModeIncompleteWithUsage)
		w := wsTestRequest(t, s, "client-a", "/v1/responses", `{"model":"openai-test","input":"hi","stream":true}`)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "event: response.incomplete") {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		events := rec.snapshot()
		if len(events) != 1 || events[0].InputTokens != 8 || events[0].OutputTokens != 5 || events[0].CachedTokens != 2 || events[0].CostIncomplete {
			t.Fatalf("final usage must stay complete: %+v", events)
		}
		if metrics := s.MetricsSnapshot(); metrics.TotalSuccess != 0 {
			t.Fatalf("incomplete terminal counted as success: %+v", metrics)
		}
		wsRequireNoProtocolViolations(t, u, 1)
		wsWaitForZeroInflight(t, s)
	})
	t.Run("failed_without_usage", func(t *testing.T) {
		s, rec, u := newWSLifecycleHarness(t, wsModeFailedNoUsage)
		w := wsTestRequest(t, s, "client-a", "/v1/responses", `{"model":"openai-test","input":"hi","stream":true}`)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "event: response.failed") {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		events := rec.snapshot()
		if len(events) != 1 || events[0].CostIncomplete != true || events[0].InputTokens != 0 || events[0].OutputTokens != 0 {
			t.Fatalf("missing usage must be cost-incomplete: %+v", events)
		}
		if metrics := s.MetricsSnapshot(); metrics.TotalSuccess != 0 {
			t.Fatalf("failed terminal counted as success: %+v", metrics)
		}
		wsRequireNoProtocolViolations(t, u, 1)
		wsWaitForZeroInflight(t, s)
	})
}

func wsLifecycleTarget(endpoint, apiKey string) config.Target {
	return config.Target{
		Name: "openai", EndpointType: "openai", Endpoint: endpoint, APIKey: apiKey, PreferWebSocket: true,
		ModelMappings: []config.ModelMapping{{Upstream: "UpstreamCase", Fallback: "openai-test"}},
	}
}

// 场景 5：UpdateTargets 后旧的空闲 socket 被关闭，新请求拨向新 endpoint 并使用新 key。
func TestResponsesWSUpdateTargetsClosesIdleSocketUsesNewEndpoint(t *testing.T) {
	u1, endpoint1 := newWSLifecycleUpstreamServer(t, wsModeComplete)
	u2, endpoint2 := newWSLifecycleUpstreamServerWithKey(t, wsModeComplete, "new-upstream-key")
	s := newWSTestService(t, endpoint1, true)
	// Exact upstream handshake counts below; background spare lanes are covered
	// by responses_ws_prewarm_test.go.
	s.responsesWS.prewarm = false
	s.SetUsageRecorder(&syncWSRecorder{})
	if w := wsTestRequest(t, s, "client-a", "/v1/responses", `{"model":"openai-test","input":"hi","stream":true}`); w.Code != 200 {
		t.Fatalf("request on old endpoint: %d %s", w.Code, w.Body.String())
	}
	if err := s.UpdateTargets([]config.Target{wsLifecycleTarget(endpoint2, "new-upstream-key")}); err != nil {
		t.Fatal(err)
	}
	w := wsTestRequest(t, s, "client-a", "/v1/responses", `{"model":"openai-test","input":"hi","stream":true}`)
	if w.Code != 200 {
		t.Fatalf("request on new endpoint: %d %s", w.Code, w.Body.String())
	}
	if u1.upgrades.Load() != 1 || u2.upgrades.Load() != 1 {
		t.Fatalf("upgrades old=%d new=%d, want 1/1", u1.upgrades.Load(), u2.upgrades.Load())
	}
	if !u2.hasAuthHeader("Bearer new-upstream-key") {
		t.Fatal("new endpoint did not receive the new upstream key")
	}
	if u2.creates.Load() != 1 {
		t.Fatalf("new endpoint creates=%d, want 1", u2.creates.Load())
	}
	if u1.posts.Load() != 0 || u2.posts.Load() != 0 {
		t.Fatalf("unexpected HTTP fallback: old=%d new=%d", u1.posts.Load(), u2.posts.Load())
	}
	if u1.badCreates.Load() != 0 || u2.badCreates.Load() != 0 {
		t.Fatalf("bad creates: old=%d new=%d", u1.badCreates.Load(), u2.badCreates.Load())
	}
}

// 场景 6：生成中客户端取消后，该次生成不得回放为 HTTP POST，且 target 的 InFlight 必须归零。
func TestResponsesWSCancelDuringGenerationNoHTTPReplayAndNoInflightLeak(t *testing.T) {
	s, _, u := newWSLifecycleHarness(t, wsModeHoldAfterCreated)
	principal := wsPrincipalFor(t, "client-a")
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	type wsReqResult struct {
		code int
	}
	done := make(chan wsReqResult, 1)
	go func() {
		w := wsServeRequest(s, principal, ctx, "/v1/responses", `{"model":"openai-test","input":"hi","stream":true}`)
		done <- wsReqResult{code: w.Code}
	}()
	// 等待生成确实开始（create 已到达上游），再等客户端取消返回。
	deadline := time.Now().Add(time.Second)
	for u.creates.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled request did not return in time")
	}
	if u.posts.Load() != 0 {
		t.Fatalf("canceled generation replayed over HTTP %d times", u.posts.Load())
	}
	if metrics := s.MetricsSnapshot(); metrics.TotalSuccess != 0 {
		t.Fatalf("canceled generation counted as success: %+v", metrics)
	}
	wsWaitForZeroInflight(t, s)
}

// 场景 7：同一客户端的两个并发 HTTP 请求各自拿到自己的响应流，事件不得串包。
func TestResponsesWSConcurrentRequestsNoPacketCrosstalk(t *testing.T) {
	s, rec, u := newWSLifecycleHarness(t, wsModeEchoInput)
	principal := wsPrincipalFor(t, "client-a")
	type wsMarkerResult struct {
		marker string
		code   int
		body   string
	}
	results := make(chan wsMarkerResult, 2)
	for _, marker := range []string{wsLifecycleMarkerA, wsLifecycleMarkerB} {
		marker := marker
		go func() {
			w := wsServeRequest(s, principal, nil, "/v1/responses",
				`{"model":"openai-test","input":"`+marker+`","stream":true}`)
			results <- wsMarkerResult{marker: marker, code: w.Code, body: w.Body.String()}
		}()
	}
	other := map[string]string{wsLifecycleMarkerA: wsLifecycleMarkerB, wsLifecycleMarkerB: wsLifecycleMarkerA}
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if r.code != 200 {
				t.Fatalf("request %q: status=%d body=%s", r.marker, r.code, r.body)
			}
			if !strings.Contains(r.body, r.marker) || strings.Contains(r.body, other[r.marker]) {
				t.Fatalf("response for %q crosstalk: %s", r.marker, r.body)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent requests timed out")
		}
	}
	if events := rec.snapshot(); len(events) != 2 {
		t.Fatalf("usage events=%d, want 2: %+v", len(events), events)
	}
	wsRequireNoProtocolViolations(t, u, 2)
	wsWaitForZeroInflight(t, s)
}
