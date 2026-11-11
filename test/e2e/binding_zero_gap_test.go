//go:build e2e

package e2e

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Issue #202: clients that open a connection per request and send the next
// one the moment they have read the previous response (no pause, no
// keep-alive) must keep landing on the bound target: the binding is written
// before the response headers are committed, not after the handler is done.

// zeroGapRun sends rounds requests back to back, each on its own closed
// connection, and counts the targets that served them.
func zeroGapRun(t *testing.T, alias, key, session string, stream bool, rounds int) map[string]int {
	t.Helper()
	seen := map[string]int{}
	for i := 0; i < rounds; i++ {
		body := `{"model":"` + alias + `","stream":` + map[bool]string{true: "true", false: "false"}[stream] + `,"messages":[{"role":"user","content":"hi"}]}`
		req, _ := http.NewRequest(http.MethodPost, w.base+"/v1/chat/completions", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Connection", "close")
		if session != "" {
			req.Header.Set("X-Session-Id", session)
		}
		resp, err := closingClient().Do(req)
		if err != nil {
			t.Error(err)
			return seen
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close() // closes the connection at once
		if resp.StatusCode != 200 || (stream && !strings.Contains(string(raw), "[DONE]")) {
			t.Errorf("request %d: %d %s", i, resp.StatusCode, raw)
			return seen
		}
		seen[resp.Header.Get("X-Proxy-Target")]++
	}
	return seen
}

func TestBindingZeroGapShortConnectionsE2E(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, session := range []bool{true, false} {
			name := map[bool]string{false: "json", true: "sse"}[stream] + "/" + map[bool]string{true: "session", false: "ip"}[session]
			t.Run(name, func(t *testing.T) {
				p := pool(t)
				alias := uniq("e2e-zero-gap")
				keyedTarget(t, w.urlA, "/openai/v1", alias, []string{uniq("sk-zero-a")}, map[string]any{"pool": p})
				keyedTarget(t, w.urlB, "/openai/v1", alias, []string{uniq("sk-zero-b")}, map[string]any{"pool": p})
				_, key := client(t, nil, []string{p})
				sid := ""
				if session {
					sid = uniq("sess")
				}
				if seen := zeroGapRun(t, alias, key, sid, stream, 8); len(seen) != 1 {
					t.Fatalf("zero-gap requests alternated between targets %v: the binding was not visible yet when the next request came", seen)
				}
			})
		}
	}
}
