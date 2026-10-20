//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
	"github.com/gorilla/websocket"
)

// Request bodies with duplicate or case-variant routing members must reach
// the upstream with exactly the one value the proxy authorized and billed,
// whatever the upstream's JSON parser (first match, last match, or
// case-insensitive struct decoding). The proxy normalizes at entry; fakellm
// records every folded member it actually received.

// upstreamMember asserts the record holds exactly one member folding to
// name, spelled exactly name, with raw value want; want "" means none.
func upstreamMember(t *testing.T, rec fakellm.Record, name, want string) {
	t.Helper()
	var found []fakellm.TopMember
	for _, m := range rec.TopMembers {
		if strings.EqualFold(m.Key, name) {
			found = append(found, m)
		}
	}
	if want == "" {
		if len(found) != 0 {
			t.Fatalf("upstream received %s members %+v, want none", name, found)
		}
		return
	}
	if len(found) != 1 || found[0].Key != name || found[0].Value != want {
		t.Fatalf("upstream received %s members %+v, want exactly %s=%s", name, found, name, want)
	}
}

// newRecords returns the fake's new non-upgrade records since seq.
func newRecords(fake *fakellm.Server, seq int64) []fakellm.Record {
	var out []fakellm.Record
	for _, r := range fake.RequestsSince(seq) {
		if r.Status != http.StatusSwitchingProtocols {
			out = append(out, r)
		}
	}
	return out
}

func lastNewRecord(t *testing.T, fake *fakellm.Server, seq int64) fakellm.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if recs := newRecords(fake, seq); len(recs) > 0 {
			return recs[len(recs)-1]
		}
		if time.Now().After(deadline) {
			t.Fatal("fake upstream received no request")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRoutingMembersNormalizedHTTP(t *testing.T) {
	plain := target(t, map[string]any{"endpoint_type": "openai", "endpoint": w.urlA,
		"api_key": "sk-up-norm", "model_mappings": []any{map[string]any{"upstream": "e2e-norm-plain"}}})
	aliased := target(t, map[string]any{"endpoint_type": "openai", "endpoint": w.urlA,
		"api_key": "sk-up-norm", "model_mappings": []any{map[string]any{"upstream": "GPT-NORM", "fallback": "e2e-norm-alias"}}})
	_, key := client(t, []string{plain, aliased}, nil)
	msgs := `"messages":[{"role":"user","content":"hi [[fake usage.in=5 usage.out=2]]"}]`
	for _, tc := range []struct {
		name, body, model, tier string
	}{
		{"duplicate model secret first", `{"model":"gpt-secret","model":"e2e-norm-plain",` + msgs + `}`, `"e2e-norm-plain"`, ""},
		{"capitalized variant", `{"model":"e2e-norm-plain","Model":"gpt-secret",` + msgs + `}`, `"e2e-norm-plain"`, ""},
		{"upper variant on alias", `{"MODEL":"gpt-secret","model":"e2e-norm-alias",` + msgs + `}`, `"GPT-NORM"`, ""},
		{"duplicate service_tier", `{"model":"e2e-norm-plain","service_tier":"flex","service_tier":"priority",` + msgs + `}`, `"e2e-norm-plain"`, `"priority"`},
		{"variant service_tier only", `{"model":"e2e-norm-alias","Service_Tier":"priority",` + msgs + `}`, `"GPT-NORM"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seq := w.fakeA.LastSeq()
			resp, body := call(t, key, "POST", "/v1/chat/completions", tc.body, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			rec := lastNewRecord(t, w.fakeA, seq)
			upstreamMember(t, rec, "model", tc.model)
			upstreamMember(t, rec, "service_tier", tc.tier)
		})
	}
	// ACL: a model the client may not use cannot ride in as a variant or a
	// duplicate; the proxy decides on the exact last member and upstream
	// never sees anything else.
	for _, body := range []string{
		`{"Model":"gpt-secret",` + msgs + `}`,
		`{"model":"gpt-secret","Model":"e2e-norm-plain",` + msgs + `}`,
		`{"model":"e2e-norm-plain","model":"gpt-secret",` + msgs + `}`,
	} {
		seq := w.fakeA.LastSeq()
		resp, out := call(t, key, "POST", "/v1/chat/completions", body, nil)
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Fatalf("unauthorized model accepted: %d %s", resp.StatusCode, out)
		}
		if recs := newRecords(w.fakeA, seq); len(recs) != 0 {
			t.Fatalf("rejected request reached upstream: %+v", recs)
		}
	}
}

func TestRoutingMembersNormalizedNativeWebSocket(t *testing.T) {
	_, _, key := wsTarget(t, nil)
	wsURL := "ws" + strings.TrimPrefix(w.base, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + key}})
	if err != nil {
		t.Fatalf("dial: %v %v", err, resp)
	}
	defer conn.Close()
	input := `"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]`
	for _, tc := range []struct{ name, frame, tier string }{
		{"duplicate model", `{"type":"response.create","model":"gpt-secret","model":"e2e-ws",` + input + `}`, ""},
		{"variant model", `{"type":"response.create","model":"e2e-ws","Model":"gpt-secret",` + input + `}`, ""},
		{"duplicate tier and type variant", `{"type":"response.create","Type":"response.cancel","model":"e2e-ws","service_tier":"flex","service_tier":"priority",` + input + `}`, `"priority"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seq := w.fakeA.LastSeq()
			if err := conn.WriteMessage(websocket.TextMessage, []byte(tc.frame)); err != nil {
				t.Fatal(err)
			}
			for {
				_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				var ev map[string]any
				if err := conn.ReadJSON(&ev); err != nil {
					t.Fatalf("ws read: %v", err)
				}
				if ev["type"] == "response.completed" {
					break
				}
				if ev["type"] == "error" || ev["type"] == "response.failed" {
					t.Fatalf("turn failed: %v", ev)
				}
			}
			rec := lastNewRecord(t, w.fakeA, seq)
			if !rec.WebSocket {
				t.Fatalf("expected an upstream WS turn: %+v", rec)
			}
			upstreamMember(t, rec, "model", `"gpt-ws"`)
			upstreamMember(t, rec, "type", `"response.create"`)
			upstreamMember(t, rec, "service_tier", tc.tier)
		})
	}
	// The unauthorized model cannot be selected through a variant.
	seq := w.fakeA.LastSeq()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-secret","Model":"e2e-ws",`+input+`}`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var ev map[string]any
	if err := conn.ReadJSON(&ev); err != nil || ev["type"] != "error" {
		t.Fatalf("unauthorized WS model: %v %v", ev, err)
	}
	if recs := newRecords(w.fakeA, seq); len(recs) != 0 {
		t.Fatalf("rejected turn reached upstream: %+v", recs)
	}
}

func TestRoutingMembersNormalizedPooledWebSocket(t *testing.T) {
	_, _, key := wsTarget(t, map[string]any{"prefer_websocket": true})
	input := `"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]`
	for _, body := range []string{
		`{"model":"gpt-secret","model":"e2e-ws",` + input + `}`,
		`{"model":"e2e-ws","Model":"gpt-secret","Input":"other",` + input + `}`,
		`{"model":"e2e-ws","service_tier":"flex","SERVICE_TIER":"priority",` + input + `}`,
	} {
		seq := w.fakeA.LastSeq()
		resp, out := call(t, key, "POST", "/v1/responses", body, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d: %s", resp.StatusCode, out)
		}
		rec := lastNewRecord(t, w.fakeA, seq)
		if !rec.WebSocket {
			t.Fatalf("expected the pooled upstream WS: %+v", rec)
		}
		upstreamMember(t, rec, "model", `"gpt-ws"`)
		upstreamMember(t, rec, "type", `"response.create"`)
		if strings.Contains(body, `"service_tier":"flex"`) {
			upstreamMember(t, rec, "service_tier", `"flex"`)
		}
		var inputs int
		for _, m := range rec.TopMembers {
			if strings.EqualFold(m.Key, "input") {
				inputs++
			}
		}
		if inputs != 1 {
			t.Fatalf("upstream input members %+v", rec.TopMembers)
		}
	}
}
