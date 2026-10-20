package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// nestedRaw follows a path of exact keys (map decoding, last wins) and array
// indexes through raw JSON.
func nestedRaw(t *testing.T, raw []byte, path ...string) []byte {
	t.Helper()
	for _, step := range path {
		if index, err := strconv.Atoi(step); err == nil {
			var items []json.RawMessage
			if json.Unmarshal(raw, &items) != nil || index >= len(items) {
				t.Fatalf("no element %d in %s", index, raw)
			}
			raw = items[index]
			continue
		}
		value, ok := oracleMapLast(raw, step)
		if !ok {
			t.Fatalf("no member %s in %s", step, raw)
		}
		raw = []byte(value)
	}
	return raw
}

var nestedBypassProbes = []struct {
	name, body string
	path       []string // object whose member every reader must read alike
	key, want  string
}{
	{"stream_options include_usage", `{"model":"test-model","stream":false,"stream_options":{"include_usage":false,"include_usage":true},"messages":[]}`,
		[]string{"stream_options"}, "include_usage", "true"},
	{"stream_options variant", `{"model":"test-model","stream_options":{"include_usage":true,"Include_Usage":false},"messages":[]}`,
		[]string{"stream_options"}, "include_usage", "true"},
	{"prompt_cache_options ttl", `{"model":"test-model","prompt_cache_options":{"ttl":"1h","ttl":"5m"},"messages":[]}`,
		[]string{"prompt_cache_options"}, "ttl", `"5m"`},
	{"prompt_cache_options TTL variant", `{"model":"test-model","prompt_cache_options":{"TTL":"1h","ttl":"5m"},"messages":[]}`,
		[]string{"prompt_cache_options"}, "ttl", `"5m"`},
	{"message cache_control", `{"model":"test-model","messages":[{"role":"user","cache_control":{"type":"ephemeral"},"content":"hi","cache_control":null}]}`,
		[]string{"messages", "0"}, "cache_control", "null"},
	{"content block cache_control variant", `{"model":"test-model","messages":[{"role":"user","content":[{"type":"text","text":"hi","Cache_Control":{"type":"ephemeral"}}]}]}`,
		[]string{"messages", "0", "content", "0"}, "cache_control", ""},
	{"system block cache_control", `{"model":"test-model","system":[{"type":"text","text":"s","cache_control":null,"cache_control":{"type":"ephemeral"}}],"messages":[]}`,
		[]string{"system", "0"}, "cache_control", `{"type":"ephemeral"}`},
	{"tool cache_control", `{"model":"test-model","tools":[{"name":"f","cache_control":{"type":"ephemeral"},"CACHE_CONTROL":null}],"messages":[]}`,
		[]string{"tools", "0"}, "cache_control", `{"type":"ephemeral"}`},
	{"top-level cache_control", `{"model":"test-model","cache_control":{"type":"ephemeral"},"cache_control":null,"messages":[]}`,
		nil, "cache_control", "null"},
	{"cache_control type", `{"model":"test-model","messages":[{"role":"user","content":"hi","cache_control":{"type":"none","type":"ephemeral"}}]}`,
		[]string{"messages", "0", "cache_control"}, "type", `"ephemeral"`},
	{"message content duplicate", `{"model":"test-model","messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}],"content":"plain"}]}`,
		[]string{"messages", "0"}, "content", `"plain"`},
}

// Nested members the proxy reads for pricing (include_usage, the Kimi cache
// TTL, Qwen and Anthropic cache_control) must read the same in every parser
// upstream as in the proxy: the last exact member.
func TestNestedPricingMembersUnambiguousUpstream(t *testing.T) {
	for _, probe := range nestedBypassProbes {
		t.Run(probe.name, func(t *testing.T) {
			s, captured, principal := newNormalizeHTTPService(t, []config.ModelMapping{{Upstream: "test-model"}})
			if w := serveNormalize(s, principal, "/v1/chat/completions", probe.body); w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			object := nestedRaw(t, captured.last(t), probe.path...)
			got := assertSingleReading(t, object, probe.key)
			if got.value != probe.want {
				t.Fatalf("upstream %s = %q, want %q in %s", probe.key, got.value, probe.want, object)
			}
		})
	}
}

func TestNormalizeNestedMembersRules(t *testing.T) {
	for _, tc := range []struct{ name, body, want, report string }{
		{"no conflict keeps bytes", `{"model":"m","stream_options":{"include_usage":true},"messages":[{"role":"user","content":[{"type":"text","text":"a\"b","cache_control":{"type":"ephemeral"}}]}],"tools":[{"cache_control":{"type":"ephemeral"}}]}`, "", ""},
		{"other nested duplicates untouched", `{"metadata":{"a":1,"a":2},"messages":[{"name":"x","name":"y","content":[{"text":"a","text":"b"}]}],"response_format":{"type":"a","type":"b"}}`, "", ""},
		{"include_usage", `{"stream_options":{"include_usage":false,"x":1,"include_usage":true}}`, `{"stream_options":{"x":1,"include_usage":true}}`, "stream_options.include_usage:1"},
		{"include_usage first kept", `{"stream_options": { "include_usage" : true , "Include_Usage":false }}`, `{"stream_options": { "include_usage" : true }}`, "stream_options.include_usage:1"},
		{"variant only", `{"prompt_cache_options":{ "TTL":"1h" }}`, `{"prompt_cache_options":{ }}`, "prompt_cache_options.ttl:1"},
		{"leading drops keep spacing", `{"prompt_cache_options":{"ttl":"1h", "Ttl":"x",  "ttl":"5m"}}`, `{"prompt_cache_options":{  "ttl":"5m"}}`, "prompt_cache_options.ttl:2"},
		{"message and block", `{"messages":[{"cache_control":1,"content":[{"cache_control":2},{"cache_control":3,"cache_control":4}],"cache_control":5}]}`,
			`{"messages":[{"content":[{"cache_control":2},{"cache_control":4}],"cache_control":5}]}`, "messages[].content[].cache_control:1,messages[].cache_control:1"},
		{"kept cache_control normalized inside", `{"system":[{"cache_control":{"type":"a","Type":"b","ttl":"5m","ttl":"1h"}}]}`,
			`{"system":[{"cache_control":{"type":"a","ttl":"1h"}}]}`, "system[].cache_control.type:1,system[].cache_control.ttl:1"},
		{"dropped cache_control not inspected", `{"tools":[{"cache_control":{"type":"a","type":"b"},"cache_control":null}]}`, `{"tools":[{"cache_control":null}]}`, "tools[].cache_control:1"},
		{"system string untouched", `{"system":"cache_control","messages":"x"}`, "", ""},
		{"top level with nested", `{"model":"a","stream_options":{"include_usage":true,"include_usage":false},"model":"b"}`, `{"stream_options":{"include_usage":false},"model":"b"}`, "model:1,stream_options.include_usage:1"},
		{"escaped variant", "{\"stream_options\":{\"include_usage\":true,\"\\u0069nclude_usage\":false}}", "{\"stream_options\":{\"\\u0069nclude_usage\":false}}", "stream_options.include_usage:1"},
		{"nested in dropped top-level not inspected", `{"messages":[{"cache_control":1,"cache_control":2}],"messages":[]}`, `{"messages":[]}`, "messages:1"},
		{"invalid JSON untouched", `{"stream_options":{"include_usage":true,"include_usage":false}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			got, report := normalizeProtectedMembers(body)
			want := tc.want
			if tc.report == "" {
				want = tc.body
				if len(got) > 0 && &got[0] != &body[0] {
					t.Fatal("a body needing no change was copied")
				}
			}
			if string(got) != want {
				t.Fatalf("got  %s\nwant %s", got, want)
			}
			var parts []string
			for _, entry := range report {
				parts = append(parts, entry.Member+":"+strconv.Itoa(entry.Removed))
			}
			if strings.Join(parts, ",") != tc.report {
				t.Fatalf("report %v, want %s", parts, tc.report)
			}
		})
	}
}

// checkNestedNormalization: the map decoding of the body, minus the case
// variants removed at the normalized paths, must not change; the edit only
// deletes bytes and is idempotent.
func checkNestedNormalization(t *testing.T, body []byte) {
	t.Helper()
	normalized, report := normalizeProtectedMembers(body)
	if report == nil {
		if !bytes.Equal(normalized, body) {
			t.Fatalf("changed without report: %q", body)
		}
		return
	}
	if !json.Valid(normalized) {
		t.Fatalf("normalization broke %q into %q", body, normalized)
	}
	// Only deletions: normalized is a subsequence of body.
	j := 0
	for i := 0; i < len(body) && j < len(normalized); i++ {
		if body[i] == normalized[j] {
			j++
		}
	}
	if j != len(normalized) {
		t.Fatalf("%q -> %q is not a deletion", body, normalized)
	}
	var before, after any
	if json.Unmarshal(body, &before) == nil && json.Unmarshal(normalized, &after) == nil {
		if !reflect.DeepEqual(canonicalizeRequest(before), after) {
			t.Fatalf("map reading changed:\n%q\n%q", body, normalized)
		}
	}
	if again, report := normalizeProtectedMembers(normalized); report != nil || !bytes.Equal(again, normalized) {
		t.Fatalf("not idempotent on %q: %v", normalized, report)
	}
}

// canonicalizeRequest drops, from a map-decoded request, the case variants
// normalization removes, mirroring the normalized paths.
func canonicalizeRequest(value any) any {
	dropVariants := func(object map[string]any, names ...string) {
		for key := range object {
			for _, name := range names {
				if strings.EqualFold(key, name) && key != name {
					delete(object, key)
				}
			}
		}
	}
	cacheControl := func(value any) {
		if object, ok := value.(map[string]any); ok {
			dropVariants(object, "type", "ttl")
		}
	}
	block := func(value any) {
		if object, ok := value.(map[string]any); ok {
			dropVariants(object, "cache_control")
			cacheControl(object["cache_control"])
		}
	}
	blocks := func(value any) {
		if items, ok := value.([]any); ok {
			for _, item := range items {
				block(item)
			}
		}
	}
	root, ok := value.(map[string]any)
	if !ok {
		return value
	}
	dropVariants(root, protectedRequestMembers...)
	if object, ok := root["stream_options"].(map[string]any); ok {
		dropVariants(object, "include_usage", "include_obfuscation")
	}
	if object, ok := root["prompt_cache_options"].(map[string]any); ok {
		dropVariants(object, "ttl")
	}
	cacheControl(root["cache_control"])
	blocks(root["system"])
	blocks(root["tools"])
	if messages, ok := root["messages"].([]any); ok {
		for _, message := range messages {
			if object, ok := message.(map[string]any); ok {
				dropVariants(object, "cache_control", "content")
				cacheControl(object["cache_control"])
				blocks(object["content"])
			}
		}
	}
	return root
}

// benchmarkRequest builds a conflict-free chat body of about size bytes:
// many messages with text blocks, some cache-marked, plus tools.
func benchmarkRequest(size int) []byte {
	var b bytes.Buffer
	b.WriteString(`{"model":"test-model","stream":true,"stream_options":{"include_usage":true},"prompt_cache_options":{"ttl":"1h"},`)
	b.WriteString(`"system":[{"type":"text","text":"You are helpful.","cache_control":{"type":"ephemeral"}}],`)
	b.WriteString(`"tools":[{"name":"lookup","description":"Look up \"things\".","input_schema":{"type":"object","properties":{"q":{"type":"string"}}},"cache_control":{"type":"ephemeral"}}],"messages":[`)
	text := strings.Repeat(`Hello \"world\" 你好 你好, line\n`, 40)
	for i := 0; b.Len() < size; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"role":"user","content":[{"type":"text","text":"` + text + `"}`)
		if i%10 == 0 {
			b.WriteString(`,{"type":"text","text":"tail","cache_control":{"type":"ephemeral","ttl":"5m"}}`)
		}
		b.WriteString(`]}`)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

// Before: the top-level pass alone (the previous normalizeProtectedMembers).
// After: top level plus the nested pricing members, on a conflict-free body.
func BenchmarkNormalizeProtectedMembers1MB(b *testing.B) {
	body := benchmarkRequest(1 << 20)
	if !json.Valid(body) {
		b.Fatal("invalid benchmark body")
	}
	if out, report := normalizeProtectedMembers(body); report != nil || &out[0] != &body[0] {
		b.Fatal("benchmark body needs normalization")
	}
	b.Run("before_top_level_only", func(b *testing.B) {
		b.SetBytes(int64(len(body)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			members, _ := scanJSONObject(nil, body)
			normalizeScannedMembers(body, members)
		}
	})
	b.Run("after_with_nested", func(b *testing.B) {
		b.SetBytes(int64(len(body)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			normalizeProtectedMembers(body)
		}
	})
}

var nestedFuzzCorpus = []string{
	`{"stream_options":{"include_usage":false,"Include_Usage":true,"include_usage":true}}`,
	`{"prompt_cache_options":{"ttl":"1h","TTL":"5m"},"cache_control":1,"Cache_Control":2}`,
	`{"messages":[{"cache_control":{"type":"a","TYPE":"b"},"content":[{"cache_control":1,"cache_control":2}],"Content":"x"}]}`,
	`{"system":[{"cache_control":null},{"CACHE_CONTROL":{}}],"tools":[{"cache_control":{"ttl":"1h","ttl":"5m"}}]}`,
	` { "messages" : [ { "cache_control" : 1 , "cache_control" : 2 } ] } `,
}

func TestNestedNormalizationProperties(t *testing.T) {
	for _, body := range nestedFuzzCorpus {
		checkNestedNormalization(t, []byte(body))
	}
}

func FuzzNestedNormalization(f *testing.F) {
	for _, body := range nestedFuzzCorpus {
		f.Add([]byte(body))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		checkNestedNormalization(t, body)
	})
}
