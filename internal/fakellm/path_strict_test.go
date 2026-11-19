package fakellm

import (
	"net/http"
	"strings"
	"testing"
)

func TestMalformedUpstreamPath(t *testing.T) {
	malformed := []string{
		// 接口路径之后又拼了一段客户端路径（endpoint 被填成完整接口地址）。
		"/v2/chat/completions/v1/chat/completions",
		"/v2/chat/completions/v1/responses",
		"/v2/chat/completions/chat/completions",
		"/v2/Chat/Completions/V1/chat/completions",
		"/anthropic/v1/messages/v1/messages",
		"/anthropic/v1/messages/v1/messages/count_tokens",
		"/v1/responses/v1/responses",
		"/v1/embeddings/v1/embeddings",
		"/v1/images/generations/v1/images/generations",
		// 版本段叠加（endpoint 与前缀或客户端路径各带一个版本段）。
		"/v2/v2/chat/completions",
		"/api/paas/v4/v1/chat/completions",
		"/v1/v1beta/models/gemini-x:generateContent",
	}
	for _, p := range malformed {
		if malformedUpstreamPath(p) == "" {
			t.Errorf("malformedUpstreamPath(%q) = \"\", want a reason", p)
		}
	}
	wellFormed := []string{
		"/", "/v1/chat/completions", "/chat/completions", "/qianfan/v2/chat/completions",
		"/v2/openai/v1/chat/completions", "/v1beta/openai/chat/completions",
		"/compatible-mode/v1/responses", "/v1/responses/resp_123", "/v1/responses/resp_123/cancel",
		"/v1/responses/resp_123/input_items", "/v1/responses/input_tokens",
		"/anthropic/v1/messages", "/v1/messages/count_tokens", "/v1/messages/batches",
		"/v1/chat/completions/chatcmpl_1/messages", "/v1/completions", "/v1/embeddings",
		"/v1/images/generations", "/v1/images/edits", "/v1/models", "/v1/models/gpt-x",
		"/v1beta/models/gemini-x:generateContent", "/openai/deployments/d/chat/completions",
		"/openai/v1/responses", "/v1/files", "/_control",
	}
	for _, p := range wellFormed {
		if reason := malformedUpstreamPath(p); reason != "" {
			t.Errorf("malformedUpstreamPath(%q) = %q, want well-formed", p, reason)
		}
	}
}

// 真实上游对拼接错误的路径返回 404；fake 也必须如此，否则代理的路径缺陷在内部测试里
// 永远是 200（Issue #232：/v2/chat/completions/v1/chat/completions 曾被后缀匹配应答）。
func TestMalformedPathGets404AndKeepsScenario(t *testing.T) {
	fake, ts := newTestServer(t)
	fake.SetScenario(&Scenario{Status: 429, Times: 1})

	resp, body := post(t, ts.URL+"/qianfan/v2/chat/completions/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d body %s, want 404", resp.StatusCode, body)
	}
	if got := strings.TrimSpace(string(body)); got != "404 page not found" {
		t.Errorf("body %q, want the plain gateway 404", got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type %q, want text/plain", ct)
	}
	recs := fake.Requests()
	if len(recs) != 1 || recs[0].Status != 404 || recs[0].Outcome != "error" || !strings.HasSuffix(recs[0].Path, "/v1/chat/completions") {
		t.Fatalf("record %+v, want one 404 error record", recs)
	}

	// 一次性场景不被路径错误的请求消耗：下一个正常请求仍拿到 429。
	resp, body = post(t, ts.URL+"/qianfan/v2/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 429 {
		t.Fatalf("well-formed request: status %d body %s, want the pending 429 scenario", resp.StatusCode, body)
	}
}

func TestLegacyMockKeepsLenientPaths(t *testing.T) {
	fake := New(Options{Name: "legacy", LegacyMock: true})
	if fake.rejectsMalformedPaths() {
		t.Fatal("legacy mock must keep its lenient path contract")
	}
	if !New(Options{Name: "t"}).rejectsMalformedPaths() {
		t.Fatal("default server must reject malformed paths")
	}
}
