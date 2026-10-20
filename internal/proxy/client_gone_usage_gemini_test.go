package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
)

// Gemini native: usage in a finishReason chunk is final; earlier cumulative
// usageMetadata without finishReason is partial and zeroed.
func TestClientGoneGeminiNative(t *testing.T) {
	for _, final := range []bool{true, false} {
		t.Run(map[bool]string{true: "finish_chunk", false: "partial_chunk"}[final], func(t *testing.T) {
			chunk := `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":3}}`
			if final {
				chunk = `{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":7}}`
			}
			upstreamCanceled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: "+chunk+"\n\n: chunk-sent\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(upstreamCanceled)
			}))
			defer upstream.Close()
			s, events, stats, logs := goneService(t, upstream.URL, config.EndpointTypeGemini, config.UsageAdapterStandard)
			w := newGoneClientWriter()
			cancel, done := serveGone(s, w, goneRequest(t, "/v1beta/models/test-model:streamGenerateContent?alt=sse", `{"contents":[{"parts":[{"text":"hi"}]}]}`))
			defer cancel()
			waitGoneBody(t, w, "chunk-sent")
			cancel()
			awaitGone(t, upstreamCanceled, "upstream cancellation")
			awaitGone(t, done, "request end")
			if final {
				got := events.snapshot()
				if len(got) != 1 || got[0].InputTokens != 11 || got[0].OutputTokens != 7 || got[0].UsageStatus != usageStatusComplete {
					t.Fatalf("official Gemini usage lost: %+v", got)
				}
			} else {
				assertZeroGoneEvent(t, s, events.snapshot(), logs, "streaming", "sse")
			}
			assertCompletions(t, stats, requeststats.Canceled)
		})
	}
}
