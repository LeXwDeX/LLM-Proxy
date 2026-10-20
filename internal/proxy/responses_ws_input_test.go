package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/coder/websocket"
)

// wsFirstInputText returns the text of the first input_text part of a WS
// create frame's list input, which is the only input form WS mode accepts.
func wsFirstInputText(input any) string {
	items, _ := input.([]any)
	if len(items) == 0 {
		return ""
	}
	item, _ := items[0].(map[string]any)
	parts, _ := item["content"].([]any)
	if len(parts) == 0 {
		return ""
	}
	part, _ := parts[0].(map[string]any)
	text, _ := part["text"].(string)
	return text
}

// F12 (#154): upstream WebSocket mode only accepts list input, so the pooled
// HTTP->WS path must wrap a plain-string input as one user message, while list
// input reaches the upstream unchanged.
func TestResponsesWSNormalizesStringInput(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"string", `{"model":"openai-test","input":"hi \"there\"\n"}`,
			`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi \"there\"\n"}]}]`},
		{"list", `{"model":"openai-test","input":[{"role":"user","content":"x","extra":{"k":1}}]}`,
			`[{"role":"user","content":"x","extra":{"k":1}}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frames := make(chan json.RawMessage, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					http.Error(w, "HTTP was used", 500)
					return
				}
				c, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer c.CloseNow()
				_, msg, err := c.Read(r.Context())
				if err != nil {
					return
				}
				var payload map[string]json.RawMessage
				if json.Unmarshal(msg, &payload) != nil {
					t.Errorf("bad create: %s", msg)
					return
				}
				frames <- payload["input"]
				_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`))
			}))
			defer upstream.Close()
			s := newWSTestService(t, upstream.URL, true)
			w := wsTestRequest(t, s, "client-a", "/v1/responses", tc.body)
			if w.Code != 200 {
				t.Fatalf("status %d %s", w.Code, w.Body.String())
			}
			raw := <-frames
			var got, want any
			if json.Unmarshal(raw, &got) != nil || json.Unmarshal([]byte(tc.want), &want) != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("input sent upstream:\n got %s\nwant %s", raw, tc.want)
			}
		})
	}
}
