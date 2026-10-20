package proxy

import (
	"math"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// Images uses input/output keys. Parsing checks dimensions; accounting checks
// whether the selected historical prices cover those image dimensions.
func TestOpenAIImageUsageKeys(t *testing.T) {
	cases := []struct {
		name, body string
		in, out    int64
		status     string
		incomplete bool
		diagnostic string
	}{
		{"generation", `{"created":1,"data":[],"usage":{"input_tokens":50,"output_tokens":272,"total_tokens":322,"input_tokens_details":{"text_tokens":50,"image_tokens":0}}}`,
			50, 272, usageStatusComplete, false, ""},
		{"generation-no-details", `{"usage":{"input_tokens":12,"output_tokens":1056}}`,
			12, 1056, usageStatusComplete, false, ""},
		{"edit-image-input", `{"usage":{"input_tokens":90,"output_tokens":272,"input_tokens_details":{"text_tokens":10,"image_tokens":80}}}`,
			90, 272, usageStatusComplete, false, ""},
		{"compatible-prompt-keys", `{"usage":{"prompt_tokens":5,"completion_tokens":7}}`,
			5, 7, usageStatusComplete, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := usageParser{protocol: config.ProtocolOpenAIImage, endpointType: config.EndpointTypeOpenAIImage, model: "gpt-image-1"}
			got, _, _ := p.extractTokens("application/json", []byte(tc.body))
			if got.InputTokens != tc.in || got.OutputTokens != tc.out || got.ImageOutputTokens != tc.out || got.status != tc.status || got.costIncomplete != tc.incomplete {
				t.Fatalf("got in=%d out=%d status=%q incomplete=%v diag=%q", got.InputTokens, got.OutputTokens, got.status, got.costIncomplete, got.diagnostic)
			}
			if tc.diagnostic != "" && got.diagnostic != tc.diagnostic {
				t.Fatalf("diagnostic %q want %q", got.diagnostic, tc.diagnostic)
			}
		})
	}
}

// Chat Completions keeps its prompt/completion keys.
func TestOpenAIChatUsageKeysUnchanged(t *testing.T) {
	p := usageParser{protocol: config.ProtocolOpenAIChat}
	got, _, _ := p.extractTokens("application/json", []byte(`{"usage":{"prompt_tokens":5,"completion_tokens":7}}`))
	if got.InputTokens != 5 || got.OutputTokens != 7 || got.status != usageStatusComplete {
		t.Fatalf("got %+v", got)
	}
}

// Image streams end with image_generation.completed / image_edit.completed,
// which carries the usage; there is no [DONE] sentinel.
func TestOpenAIImageStreamTerminal(t *testing.T) {
	for _, prefix := range []string{"image_generation", "image_edit"} {
		t.Run(prefix, func(t *testing.T) {
			u := newUsageStream("text/event-stream", "", usageParser{protocol: config.ProtocolOpenAIImage, model: "gpt-image-1"})
			_, _ = u.Write([]byte("event: " + prefix + ".partial_image\ndata: {\"type\":\"" + prefix + ".partial_image\",\"b64_json\":\"AA==\",\"partial_image_index\":0}\n\n" +
				"event: " + prefix + ".completed\ndata: {\"type\":\"" + prefix + ".completed\",\"b64_json\":\"AA==\",\"usage\":{\"input_tokens\":50,\"output_tokens\":272,\"input_tokens_details\":{\"text_tokens\":50,\"image_tokens\":0}}}\n\n"))
			u.Close()
			if got := u.tokens; got.InputTokens != 50 || got.OutputTokens != 272 || got.ImageOutputTokens != 272 || got.status != usageStatusComplete || got.costIncomplete {
				t.Fatalf("got in=%d out=%d status=%q incomplete=%v diag=%q", got.InputTokens, got.OutputTokens, got.status, got.costIncomplete, got.diagnostic)
			}
		})
	}
	// Truncated before completed: still incomplete.
	u := newUsageStream("text/event-stream", "", usageParser{protocol: config.ProtocolOpenAIImage})
	_, _ = u.Write([]byte("event: image_generation.partial_image\ndata: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"AA==\"}\n\n"))
	u.Close()
	if u.tokens.status == usageStatusComplete {
		t.Fatalf("truncated image stream counted complete: %+v", u.tokens)
	}
}

func TestImageOutputWithoutDetailsFinalEventAndCost(t *testing.T) {
	imageOut := 20.0
	rates := usage.CostTable{"model": {InputPer1MTokens: 2, OutputPer1MTokens: 4, ImageOutputPer1MTokens: &imageOut}}
	for _, path := range []string{"/v1/images/generations", "/v1/images/edits", "/v1/images/variations", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(path+map[bool]string{false: "/json", true: "/sse"}[stream], func(t *testing.T) {
				req := httptest.NewRequest("POST", path, nil)
				store := auth.NewStore()
				if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
					t.Fatal(err)
				}
				principal, _ := store.Authenticate("token")
				req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
				p := newTargetUsageParser(req, nil, "model", nil, nil)
				body := `{"usage":{"input_tokens":12,"output_tokens":1056}}`
				if p.protocol == config.ProtocolOpenAIChat {
					body = strings.NewReplacer("input_tokens", "prompt_tokens", "output_tokens", "completion_tokens").Replace(body)
				}
				contentType := "application/json"
				if stream {
					contentType = "text/event-stream"
					switch p.protocol {
					case config.ProtocolOpenAIImage:
						typ := "image_generation.completed"
						if strings.HasSuffix(path, "/edits") {
							typ = "image_edit.completed"
						}
						body = `{"type":"` + typ + `",` + strings.TrimPrefix(body, "{")
					case config.ProtocolOpenAIResponses:
						body = `{"type":"response.completed","response":` + body + `}`
					}
					body = "data: " + body + "\n\ndata: [DONE]\n\n"
				}
				var tokens usageTokens
				var ok bool
				if stream {
					tokens, _, ok = p.extractSSE([]byte(body))
				} else {
					tokens, _, ok = p.extractTokens(contentType, []byte(body))
				}
				if !ok || tokens.costIncomplete {
					t.Fatalf("tokens=%+v found=%v", tokens, ok)
				}
				recorder := &adapterEventRecorder{}
				svc := &Service{}
				svc.SetUsageRecorder(recorder)
				svc.recordParsedUsage(req, nil, 200, "model", tokens, "", nil, -1)
				if len(recorder.events) != 1 {
					t.Fatalf("events=%+v", recorder.events)
				}
				evt := recorder.events[0]
				wantImage, wantCost := int64(0), .004248
				if p.protocol == config.ProtocolOpenAIImage {
					wantImage, wantCost = 1056, .021144
				}
				if evt.ImageOutputTokens != wantImage || evt.InputTokens != 12 || evt.OutputTokens != 1056 || math.Abs(usage.EstimateEventCost(evt, rates)-wantCost) > 1e-10 {
					t.Fatalf("event=%+v cost=%v want=%v", evt, usage.EstimateEventCost(evt, rates), wantCost)
				}
			})
		}
	}
}
