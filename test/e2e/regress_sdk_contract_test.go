//go:build e2e

package e2e

import (
	"encoding/json"
	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// #166/#167: the real proxy binary records thinking as a subset, not extra cost.
func TestSDKContractClaudeAccounting(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			alias := uniq("sdk-claude")
			tg := target(t, map[string]any{"endpoint_type": "claude", "endpoint": w.urlA + "/anthropic/v1/messages", "api_key": uniq("sk-sdk"), "model_mappings": []any{map[string]any{"upstream": "claude-mock", "fallback": alias}}})
			name, key := client(t, []string{tg}, nil)
			code, b := w.adminDo("POST", "/admin/data/pricing-templates", map[string]any{"target": tg, "model": "claude-mock", "mode": "flat", "base_rates": map[string]any{"input_per_1m_tokens": 2, "output_per_1m_tokens": 4}})
			if code != 201 {
				t.Fatal(code, string(b))
			}
			resp, raw := call(t, key, "POST", "/v1/messages", expand(messagesBody("[[fake usage.in=10 usage.out=20 usage.reasoning=5]]", stream), alias), nil)
			if resp.StatusCode != 200 || !strings.Contains(string(raw), "thinking_tokens") {
				t.Fatal(resp.StatusCode, string(raw))
			}
			ev := usageFor(t, name, 1)[0]
			checkBilled(t, ev, billed{in: 10, out: 20, reasoning: 5})
			if ev.CostIncomplete {
				t.Fatal(ev)
			}
			code, b = w.adminDo("GET", "/admin/data/usage/aggregate?group_by=hour&client_name="+url.QueryEscape(name), nil)
			cost, ok := dig(decode(t, b), "totals/estimated_cost").(float64)
			if code != 200 || !ok || math.Abs(cost-0.0001) > 1e-12 {
				t.Fatal("double charged thinking", string(b))
			}
		})
	}
}

// #165: terminal protocol fault must not turn into a successful REST response or replay.
func TestSDKContractInvalidWSTerminal(t *testing.T) {
	for _, shape := range []string{"null", "42"} {
		t.Run(shape, func(t *testing.T) {
			alias := uniq("sdk-ws")
			upkey := uniq("sk-sdk-ws")
			tg := target(t, map[string]any{"endpoint_type": "openai", "endpoint": w.urlA + "/openai/v1", "api_key": upkey, "supports_responses": true, "prefer_websocket": true, "model_mappings": []any{map[string]any{"upstream": "mock-ws", "fallback": alias}}})
			_, key := client(t, []string{tg}, nil)
			before := w.fakeA.LastSeq()
			resp, raw := call(t, key, "POST", "/v1/responses", map[string]any{"model": alias, "input": wsInput("[[fake ws_terminal_response=" + shape + "]]"), "stream": false}, nil)
			if resp.StatusCode != 502 {
				t.Fatal(resp.StatusCode, string(raw))
			}
			turns := 0
			for _, r := range w.fakeA.RequestsSince(before) {
				if r.Key == fakellm.KeyFingerprint(upkey) && r.WebSocket && r.Status != 101 {
					turns++
				}
			}
			if turns != 1 {
				t.Fatalf("generation replayed: turns=%d", turns)
			}
		})
	}
}

// #164: actual HTTP upstream receives exact large numbers despite target custom_body.
func TestSDKContractCustomBodyPrecision(t *testing.T) {
	seen := make(chan []byte, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen <- b
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	alias := uniq("sdk-custom")
	tg := target(t, map[string]any{"endpoint_type": "openai", "endpoint": up.URL + "/v1", "api_key": uniq("sk-sdk-custom"), "custom_body": map[string]any{"enable_thinking": false}, "model_mappings": []any{map[string]any{"upstream": "mock", "fallback": alias}}})
	_, key := client(t, []string{tg}, nil)
	resp, raw := call(t, key, "POST", "/v1/chat/completions", map[string]any{"model": alias, "messages": []any{}, "seed": json.Number("9007199254740993"), "extension": map[string]any{"number": json.Number("9007199254740993"), "zero": 0, "null": nil}}, nil)
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode, string(raw))
	}
	b := <-seen
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil || string(obj["seed"]) != "9007199254740993" || !strings.Contains(string(obj["extension"]), "9007199254740993") || string(obj["enable_thinking"]) != "false" {
		t.Fatal(string(b))
	}
}
