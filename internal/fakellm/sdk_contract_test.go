package fakellm

import (
	"encoding/json"
	"github.com/gorilla/websocket"
	"strings"
	"testing"
)

func TestSDKContractClaudeUsage(t *testing.T) {
	_, ts := newTestServer(t)
	for _, stream := range []bool{false, true} {
		body := `{"model":"mock","messages":[],"stream":false}`
		if stream {
			body = strings.Replace(body, "false", "true", 1)
		}
		_, raw := post(t, ts.URL+"/anthropic/v1/messages", body, map[string]string{"X-Fake-Scenario": "usage.in=10 usage.out=20 usage.reasoning=5"})
		check := func(m map[string]any, want int) {
			t.Helper()
			details, _ := m["output_tokens_details"].(map[string]any)
			if details["thinking_tokens"] != float64(want) {
				t.Fatal("thinking usage", m)
			}
			tools, _ := m["server_tool_use"].(map[string]any)
			if len(tools) != 2 || tools["web_fetch_requests"] != float64(0) || tools["web_search_requests"] != float64(0) {
				t.Fatal("tool usage", m)
			}
		}
		if !stream {
			check(decode(t, raw)["usage"].(map[string]any), 5)
		} else {
			found := false
			for _, ev := range readSSE(t, raw) {
				var p map[string]any
				json.Unmarshal([]byte(ev.data), &p)
				switch p["type"] {
				case "message_start":
					check(p["message"].(map[string]any)["usage"].(map[string]any), 0)
				case "message_delta":
					check(p["usage"].(map[string]any), 5)
					found = true
				}
			}
			if !found {
				t.Fatal("no final usage")
			}
		}
	}
}
func TestSDKContractWSTerminalFixture(t *testing.T) {
	fake, ts := newTestServer(t)
	for _, shape := range []string{"null", "42", "[]"} {
		t.Run(shape, func(t *testing.T) {
			directive, err := ParseDirective("ws_terminal_response=" + shape)
			if err != nil {
				t.Fatal(err)
			}
			var merged Scenario
			merged.merge(directive)
			b, _ := json.Marshal(merged)
			if !strings.Contains(string(b), "ws_terminal_response") {
				t.Fatal("merge lost fixture")
			}
			fake.SetScenario(directive)
			c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/v1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.WriteJSON(map[string]any{"type": "response.create", "model": "mock", "input": []any{map[string]any{"role": "user", "content": "hello"}}})
			for {
				_, b, err := c.ReadMessage()
				if err != nil {
					t.Fatal(err)
				}
				var ev map[string]json.RawMessage
				json.Unmarshal(b, &ev)
				if string(ev["type"]) == `"response.completed"` {
					if string(ev["response"]) != shape {
						t.Fatal(string(b))
					}
					break
				}
			}
			_, b = post(t, ts.URL+"/v1/responses", `{"model":"mock","input":"hello"}`, nil)
			if decode(t, b)["object"] != "response" {
				t.Fatal("HTTP changed", string(b))
			}
		})
	}
	if _, err := ParseDirective("ws_terminal_response=invalid"); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestSDKContractWSTerminalInvalidScenario(t *testing.T) {
	fake, ts := newTestServer(t)
	fake.SetScenario(&Scenario{WSTerminalResponse: "invalid"})
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.WriteJSON(map[string]any{"type": "response.create", "input": []any{}}); err != nil {
		t.Fatal(err)
	}
	var ev map[string]any
	if err = c.ReadJSON(&ev); err != nil {
		t.Fatal(err)
	}
	if ev["type"] != "error" || ev["error"].(map[string]any)["code"] != "fake_directive" {
		t.Fatal(ev)
	}
}
