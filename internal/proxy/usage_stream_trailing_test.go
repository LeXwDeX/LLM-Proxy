package proxy

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// A usage payload is exactly one JSON value: only JSON whitespace may follow
// it. This contract is independent of the encoding/json implementation
// (legacy or jsonv2-backed) and of how the trailing bytes are inspected.
func TestReadUsagePayloadTrailingData(t *testing.T) {
	const payload = `{"usage":{"prompt_tokens":3,"completion_tokens":1}}`
	cases := []struct {
		name    string
		trailer string
		wantErr bool
	}{
		{"none", "", false},
		{"json_whitespace", " \t\r\n \n", false},
		{"second_object", ` {"usage":{}}`, true},
		{"second_scalar", "\n1", true},
		{"garbage", " trailing", true},
		{"comma", ",", true},
		{"colon", " :", true},
		{"closing_brace", "}", true},
		{"nul", "\x00", true},
		{"bom", "\xef\xbb\xbf", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := readUsagePayload(strings.NewReader(payload + c.trailer))
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, c.wantErr)
			}
			if !c.wantErr {
				usage, _ := got["usage"].(map[string]any)
				if usage == nil || usage["completion_tokens"] == nil {
					t.Fatalf("payload lost: %#v", got)
				}
			}
		})
	}
}

// A read error after a complete value is still reported, as before.
func TestReadUsagePayloadTrailingReadError(t *testing.T) {
	boom := errors.New("decode failed")
	reader := io.MultiReader(strings.NewReader(`{"usage":{}}`+"  "), &errReader{err: boom})
	if _, err := readUsagePayload(reader); !errors.Is(err, boom) {
		t.Fatalf("err=%v, want %v", err, boom)
	}
}

type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }

// SSE events follow the same single-value rule; whitespace after the JSON
// value inside an event is not an observation failure.
func TestUsageStreamSSEEventTrailingData(t *testing.T) {
	usage := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`
	cases := []struct {
		name    string
		event   string
		wantErr bool
	}{
		{"clean", "data: " + usage + "\n\n", false},
		{"trailing_spaces", "data: " + usage + "   \n\n", false},
		{"trailing_data_line", "data: " + usage + "\ndata: \n\n", false},
		{"second_value", "data: " + usage + " {}\n\n", true},
		{"garbage", "data: " + usage + " x\n\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stream := newUsageStream("text/event-stream", "", usageParser{protocol: config.ProtocolOpenAIChat})
			_, _ = stream.Write([]byte(c.event + "data: [DONE]\n\n"))
			stream.Close()
			if (stream.err != nil) != c.wantErr {
				t.Fatalf("err=%v, wantErr=%v", stream.err, c.wantErr)
			}
			if !stream.terminal {
				t.Fatalf("terminal lost")
			}
			if !c.wantErr && (!stream.found || stream.tokens.OutputTokens != 2) {
				t.Fatalf("usage lost: found=%v tokens=%+v", stream.found, stream.tokens)
			}
		})
	}
}
