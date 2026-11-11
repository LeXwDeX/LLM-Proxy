//go:build e2e

package e2e

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The edge proxy in front of the service closes the connection after every
// response (no upstream keep-alive). A client that closes right after reading
// the whole response must still get its session and IP bindings.

func closingClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
}

func TestBindingSurvivesConnectionCloseE2E(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, session := range []bool{true, false} {
			name := map[bool]string{false: "json", true: "sse"}[stream] + "/" + map[bool]string{true: "session", false: "ip"}[session]
			t.Run(name, func(t *testing.T) {
				p := pool(t)
				alias := uniq("e2e-close")
				kA, kB := uniq("sk-close-a"), uniq("sk-close-b")
				keyedTarget(t, w.urlA, "/openai/v1", alias, []string{kA}, map[string]any{"pool": p})
				keyedTarget(t, w.urlB, "/openai/v1", alias, []string{kB}, map[string]any{"pool": p})
				_, key := client(t, nil, []string{p})
				sid := uniq("sess")
				seen := map[string]int{}
				for i := 0; i < 6; i++ {
					body := `{"model":"` + alias + `","stream":` + map[bool]string{true: "true", false: "false"}[stream] + `,"messages":[{"role":"user","content":"hi"}]}`
					req, _ := http.NewRequest(http.MethodPost, w.base+"/v1/chat/completions", bytes.NewBufferString(body))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer "+key)
					req.Header.Set("Connection", "close")
					if session {
						req.Header.Set("X-Session-Id", sid)
					}
					resp, err := closingClient().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					raw, _ := io.ReadAll(resp.Body)
					resp.Body.Close() // closes the connection at once
					if resp.StatusCode != 200 || (stream && !strings.Contains(string(raw), "[DONE]")) {
						t.Fatalf("request %d: %d %s", i, resp.StatusCode, raw)
					}
					seen[resp.Header.Get("X-Proxy-Target")]++
					time.Sleep(100 * time.Millisecond) // the handler may still be finishing after the close
				}
				if len(seen) != 1 {
					t.Fatalf("requests alternated between targets %v: the binding was not written when the client closed after the full response", seen)
				}
			})
		}
	}
}
