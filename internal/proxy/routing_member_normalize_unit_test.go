package proxy

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestNormalizeProtectedMembersRules(t *testing.T) {
	for _, tc := range []struct{ name, body, want, report string }{
		{"no change keeps bytes", "{ \"model\" : \"m\",\n \"x\":[1, 2] }", "{ \"model\" : \"m\",\n \"x\":[1, 2] }", ""},
		{"unprotected duplicates untouched", `{"x":1,"x":2,"X":3}`, `{"x":1,"x":2,"X":3}`, ""},
		{"last exact wins", `{"model":"a", "model":"b"}`, `{ "model":"b"}`, "model:1"},
		{"variants removed around exact", `{"Model":"a","model":"b" , "MODEL":"c","x":"<&>"}`, `{"model":"b","x":"<&>"}`, "model:2"},
		{"variant only removed", `{"Model":"secret","input":"hi"}`, `{"input":"hi"}`, "model:1"},
		{"all members removed", ` { "Model" : 1 } `, ` { } `, "model:1"},
		{"escaped exact key kept", "{\"model\":\"a\",\"mod\\u0065l\":\"b\"}", "{\"mod\\u0065l\":\"b\"}", "model:1"},
		{"unicode fold variants", "{\"stream\":true,\"ſtream\":false,\"max_tokens\":1,\"max_to\u212aens\":9}", `{"stream":true,"max_tokens":1}`, "stream:1,max_tokens:1"},
		{"several members", `{"Type":"x","service_tier":"a","service_tier":"b","type":"y","Input":[]}`, `{"service_tier":"b","type":"y"}`, "service_tier:1,type:1,input:1"},
		{"invalid JSON untouched", `{"model":"a","model":"b",}`, `{"model":"a","model":"b",}`, ""},
		{"non-object untouched", `[{"model":"a","model":"b"}]`, `[{"model":"a","model":"b"}]`, ""},
		{"nested untouched", `{"metadata":{"model":"a","Model":"b"}}`, `{"metadata":{"model":"a","Model":"b"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			got, report := normalizeProtectedMembers(body)
			if string(got) != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
			var parts []string
			for _, entry := range report {
				parts = append(parts, entry.Member+":"+strconv.Itoa(entry.Removed))
			}
			if strings.Join(parts, ",") != tc.report {
				t.Fatalf("report %v, want %s", parts, tc.report)
			}
			if tc.report == "" && len(body) > 0 && &got[0] != &body[0] {
				t.Fatal("a body needing no change was copied")
			}
		})
	}
}

var normalizeCorpus = []string{
	`{"model":"gpt-secret","model":"test-model"}`, `{"model":"test-model","Model":"gpt-secret"}`,
	`{"MODEL":"a","Model":"b","model":"c","mOdEl":"d"}`, `{"Model":"only"}`, "{\"\\u006dodel\":\"x\",\"model\":\"y\"}",
	`{"service_tier":"flex","service_tier":"priority"}`, "{\"service_tier\":\"flex\",\"ſervice_tier\":\"priority\"}",
	`{"type":1,"Type":"error"}`, `{"stream":true,"Stream":false,"stream_options":{},"Stream_Options":null}`,
	`{"input":"a","Input":["b"],"instructions":"x","INSTRUCTIONS":"y"}`, `{"model":null,"Model":"x"}`,
	"{\"model\":\"a\",\"mod\xffl\":\"b\"}", `{"a":1}`, `{}`, `null`, `[]`, `{"model":"a"`,
}

// After normalization the three reader kinds agree on every protected
// member and with the value the proxy itself used (map-last = extractModel).
func checkNormalizedReadersAgree(t *testing.T, body []byte) {
	t.Helper()
	normalized, _ := normalizeProtectedMembers(body)
	if _, ok := scanJSONObject(nil, body); !ok {
		if !bytes.Equal(normalized, body) {
			t.Fatalf("invalid or non-object body changed: %q", body)
		}
		return
	}
	if !json.Valid(normalized) {
		t.Fatalf("normalization produced invalid JSON from %q: %q", body, normalized)
	}
	for _, key := range protectedRequestMembers {
		before, beforeOK := oracleMapLast(body, key)
		mapValue, mapOK := oracleMapLast(normalized, key)
		firstValue, firstOK := oracleFirst(normalized, key)
		foldValue, foldOK := oracleStructFold(normalized, key)
		if mapOK != firstOK || mapOK != foldOK || mapValue != firstValue || mapValue != foldValue {
			t.Fatalf("%q -> %q: %s map=%q first=%q fold=%q", body, normalized, key, mapValue, firstValue, foldValue)
		}
		if beforeOK != mapOK || before != mapValue {
			t.Fatalf("%q -> %q: %s changed from proxy value %q to %q", body, normalized, key, before, mapValue)
		}
	}
	if got, want := extractModelFromJSON(normalized), extractModelFromJSON(body); got != want {
		t.Fatalf("%q: extractModel changed from %q to %q", body, want, got)
	}
	// Unprotected members are untouched: same bytes in the same order.
	var keptBefore, keptAfter []string
	before, _ := scanJSONObject(nil, body)
	after, _ := scanJSONObject(nil, normalized)
	for _, m := range before {
		if protectedMemberIndex(m) < 0 {
			keptBefore = append(keptBefore, string(m.span))
		}
	}
	for _, m := range after {
		if protectedMemberIndex(m) < 0 {
			keptAfter = append(keptAfter, string(m.span))
		}
	}
	if !reflect.DeepEqual(keptBefore, keptAfter) {
		t.Fatalf("%q: unprotected members changed: %v -> %v", body, keptBefore, keptAfter)
	}
	if again, report := normalizeProtectedMembers(normalized); report != nil || !bytes.Equal(again, normalized) {
		t.Fatalf("normalization not idempotent on %q", normalized)
	}
}

func TestNormalizedReadersAgree(t *testing.T) {
	for _, body := range normalizeCorpus {
		checkNormalizedReadersAgree(t, []byte(body))
	}
}

func FuzzNormalizeProtectedMembers(f *testing.F) {
	for _, body := range normalizeCorpus {
		f.Add([]byte(body))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		checkNormalizedReadersAgree(t, body)
	})
}

// --- L6: terminal event typing follows the old struct decoding ---

func legacyWebsocketTerminal(data []byte) bool {
	var event struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &event) != nil {
		return false
	}
	switch event.Type {
	case "response.completed", "response.failed", "response.incomplete", "error":
		return true
	}
	return false
}

var terminalEventCorpus = []string{
	`{"type":"error"}`, `{"type":1,"Type":"error"}`, `{"Type":"error","type":1}`, `{"type":"error","Type":null}`,
	`{"type":null}`, `{"TYPE":"response.completed"}`, `{"type":"response.completed","type":"x"}`,
	`{"type":"x","tyPe":"response.failed"}`, `{"type":["error"]}`, `{"type":{"a":1}}`, `{"other":1}`,
	`[]`, `"error"`, `{"type":"error"`, "{\"\\u0074ype\":\"error\"}",
}

func checkTerminalLikeLegacy(t *testing.T, data []byte) {
	t.Helper()
	var members []jsonMember
	terminal, valid := responsesWebsocketEventTerminal(&members, data)
	if valid != json.Valid(data) {
		t.Fatalf("%q: valid=%v", data, valid)
	}
	if valid && terminal != legacyWebsocketTerminal(data) {
		t.Fatalf("%q: terminal=%v, struct decoding=%v", data, terminal, legacyWebsocketTerminal(data))
	}
}

func TestResponsesWebsocketTerminalMatchesStructDecoding(t *testing.T) {
	for _, data := range terminalEventCorpus {
		checkTerminalLikeLegacy(t, []byte(data))
	}
}

func FuzzResponsesWebsocketTerminal(f *testing.F) {
	for _, data := range terminalEventCorpus {
		f.Add([]byte(data))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		checkTerminalLikeLegacy(t, data)
	})
}

// --- custom_body, metrics and log ---

func TestCustomBodyRejectsProtectedVariants(t *testing.T) {
	if got := ValidateCustomBody(map[string]any{"Service_Tier": "priority"}); got != "Service_Tier" {
		t.Fatalf("variant accepted: %q", got)
	}
	if got := ValidateCustomBody(map[string]any{"service_tier": "priority", "temperature": 1}); got != "" {
		t.Fatalf("exact member rejected: %q", got)
	}
	body := injectCustomBody([]byte(`{"service_tier":"flex"}`), map[string]any{"Service_Tier": "priority", "Temperature": 1})
	assertSingleReading(t, body, "service_tier")
	if got, _ := oracleStructFold(body, "service_tier"); got != `"flex"` {
		t.Fatalf("variant injected: %s", body)
	}
}

func TestRequestNormalizationCountsAndLogsNamesOnly(t *testing.T) {
	var logs bytes.Buffer
	s, captured, principal := newNormalizeHTTPService(t, []config.ModelMapping{{Upstream: "test-model"}})
	s.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	serveNormalize(s, principal, "/v1/chat/completions", `{"model":"test-model","Model":"gpt-secret-value","messages":[]}`)
	serveNormalize(s, principal, "/v1/chat/completions", `{"model":"test-model","messages":[]}`)
	if got := s.MetricsSnapshot().TotalRequestMembersNormalized; got != 1 {
		t.Fatalf("normalized counter = %d, want 1", got)
	}
	out := logs.String()
	if !strings.Contains(out, "request members normalized") || !strings.Contains(out, "model:1") {
		t.Fatalf("missing normalization log: %s", out)
	}
	if strings.Contains(out, "gpt-secret-value") {
		t.Fatalf("log leaked a member value: %s", out)
	}
	// The unchanged second request reached upstream byte for byte.
	if got := captured.last(t); string(got) != `{"model":"test-model","messages":[]}` {
		t.Fatalf("unchanged body rewritten: %s", got)
	}
}
