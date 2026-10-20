package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// extractModelLegacyJSON is the pre-optimization body branch of extractModel:
// decode the whole object into map[string]any. It is the equivalence oracle.
func extractModelLegacyJSON(body []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err == nil {
		if m, ok := payload["model"].(string); ok {
			return strings.TrimSpace(m)
		}
	}
	return ""
}

var extractModelEquivalenceCases = []struct {
	name, body, want string
}{
	{"plain", `{"model":"qwen-max"}`, "qwen-max"},
	{"trimmed", `{"model":"  qwen-max \t"}`, "qwen-max"},
	{"duplicate last wins", `{"model":"first","model":"second"}`, "second"},
	{"duplicate last non-string", `{"model":"first","model":1}`, ""},
	{"duplicate last string after number", `{"model":1,"model":"second"}`, "second"},
	{"number model", `{"model":123}`, ""},
	{"null model", `{"model":null}`, ""},
	{"bool model", `{"model":true}`, ""},
	{"object model", `{"model":{"name":"x"}}`, ""},
	{"array model", `{"model":["x"]}`, ""},
	{"capitalized key", `{"Model":"qwen-max"}`, ""},
	{"upper key", `{"MODEL":"qwen-max"}`, ""},
	{"both cases", `{"Model":"wrong","model":"right"}`, "right"},
	{"escaped key", "{\"mod\\u0065l\":\"escaped-key\"}", "escaped-key"},
	{"escaped capital key", "{\"\\u004dodel\":\"escaped-key\"}", ""},
	{"escaped value", "{\"model\":\"qwen\\u002dmax\\n\"}", "qwen-max"},
	{"invalid utf8 value", "{\"model\":\"q\xffw\"}", "q�w"},
	{"nested model ignored", `{"metadata":{"model":"nested"}}`, ""},
	{"invalid json", `{"model":"qwen-max",}`, ""},
	{"truncated", `{"model":"qwen-max"`, ""},
	{"trailing data", `{"model":"qwen-max"} {}`, ""},
	{"top-level array", `[{"model":"qwen-max"}]`, ""},
	{"top-level string", `"qwen-max"`, ""},
	{"top-level null", `null`, ""},
	{"empty object", `{}`, ""},
	{"whitespace", " \n{ \"model\" : \"qwen-max\" }\r\n", "qwen-max"},
	{"large integer", `{"model":"m","seed":9007199254740993}`, "m"},
	{"float overflow", `{"model":"m","x":1e400}`, ""},
	{"nested float overflow", `{"model":"m","x":[{"y":-2.5E+309}]}`, ""},
	{"float underflow", `{"model":"m","x":1e-400}`, "m"},
	{"exponent in string", `{"model":"m","x":"1e400"}`, "m"},
	{"escaped quote before number text", `{"model":"m","x":"\"1e400\\"}`, "m"},
	{"long mantissa overflow", `{"model":"m","x":` + strings.Repeat("9", 320) + `}`, ""},
	{"long mantissa fits", `{"model":"m","x":` + strings.Repeat("9", 300) + `}`, "m"},
	{"bom", "\xef\xbb\xbf{\"model\":\"m\"}", ""},
	{"empty", ``, ""},
}

func TestExtractModelJSONEquivalence(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Content-Type", "application/json")
	for _, tc := range extractModelEquivalenceCases {
		t.Run(tc.name, func(t *testing.T) {
			legacy := extractModelLegacyJSON([]byte(tc.body))
			if legacy != tc.want {
				t.Fatalf("oracle = %q, table want %q", legacy, tc.want)
			}
			if got := extractModel(r, []byte(tc.body)); got != legacy {
				t.Fatalf("extractModel = %q, legacy = %q", got, legacy)
			}
		})
	}
}

// The body branch must not materialize every message as map[string]any.
func TestExtractModelJSONAllocationBudget(t *testing.T) {
	body := perfChatBody(256 << 10)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Content-Type", "application/json")
	allocs := testing.AllocsPerRun(5, func() {
		if extractModel(r, body) != "qwen-max" {
			t.Fatal("model lost")
		}
	})
	if allocs > 64 {
		t.Fatalf("extractModel allocated %.0f objects for a 256 KiB body, want <= 64", allocs)
	}
}

func FuzzExtractModelJSONEquivalence(f *testing.F) {
	for _, tc := range extractModelEquivalenceCases {
		f.Add([]byte(tc.body))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if got, want := extractModelFromJSON(body), extractModelLegacyJSON(body); got != want {
			t.Fatalf("body %q: got %q want %q", body, got, want)
		}
	})
}

// --- top-level index ---

var topLevelIndexCorpus = []string{
	`{}`, ` { } `, `{"a":1}`, `{"a":1,"a":2}`, `{"a" : [1, {"b":null}] , "c":"x\"y"}`,
	"{\"mod\\u0065l\":\"x\",\"model\":\"y\"}", `{"a":-0.5e+10,"b":0,"c":-0,"d":1E2}`, `{"a":01}`, `{"a":-}`, `{"a":1.}`,
	`{"a":.5}`, `{"a":1e}`, `{"a":+1}`, `{"a":tru}`, `{"a":nul}`, `{"a":"\x"}`, "{\"a\":\"\\u12\"}",
	"{\"a\":\"\\ud83d\\ude00\\/\\b\\f\\r\\t\"}", "{\"a\":\"\\uZZZZ\"}", "{\"a\\\"b\":1}",
	"{\"a\":\"\x01\"}", "{\"a\":\"\x7f\xff\"}", `{"a":1,}`, `{,}`, `{"a"}`, `{"a":1 "b":2}`,
	`{"a":[1,]}`, `{"a":[,1]}`, `[]`, `null`, `"s"`, `1`, ``, ` `, `{"a":1}x`, `{"a":1}{}`,
	`{"a":"b"`, `{"a":{"b":[[]]}}`, "\t{\"a\":true}\n", `{"a":false,"b":null,"c":true}`,
	`{"a":"<script>&</script>"}`, `{"a":"😀"}`, `{1:2}`, `{"a":1}}`, `{"a":[}`,
}

func nestedJSON(depth int) string {
	return `{"a":` + strings.Repeat("[", depth-1) + strings.Repeat("]", depth-1) + `}`
}

// checkTopLevelIndexAgainstStdlib holds the merged scanner to encoding/json:
// same validity, and every member's offsets, key and value tokens and exact
// lookups agree with map decoding.
func checkTopLevelIndexAgainstStdlib(t *testing.T, body []byte) {
	t.Helper()
	members, ok := scanJSONObject(nil, body)
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	want := json.Valid(body) && len(trimmed) > 0 && trimmed[0] == '{'
	if ok != want {
		t.Fatalf("body %q: scan ok=%v, stdlib valid object=%v", body, ok, want)
	}
	if !ok {
		return
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body %q: stdlib rejected a scanned object: %v", body, err)
	}
	last := map[string][]byte{}
	for _, member := range members {
		if !bytes.Equal(member.key, body[member.keyStart:member.keyStart+len(member.key)]) ||
			!bytes.Equal(member.value, body[member.valueStart:member.valueEnd]) ||
			!bytes.Equal(member.span, body[member.keyStart:member.valueEnd]) {
			t.Fatalf("body %q: member offsets disagree with its bytes", body)
		}
		var key string
		if err := json.Unmarshal(member.key, &key); err != nil {
			t.Fatalf("body %q: key %q invalid: %v", body, member.key, err)
		}
		if !member.named(key) || !member.folds(key) || member.folds(strings.ToUpper(key)) != strings.EqualFold(key, strings.ToUpper(key)) {
			t.Fatalf("body %q: key %q lookup", body, key)
		}
		last[key] = member.value
	}
	if len(last) != len(decoded) {
		t.Fatalf("body %q: scanned keys %d, stdlib keys %d", body, len(last), len(decoded))
	}
	for key, value := range decoded {
		if !bytes.Equal(last[key], value) || !bytes.Equal(jsonLast(members, key), value) {
			t.Fatalf("body %q key %q: scanned value %q, stdlib %q", body, key, last[key], value)
		}
	}
}

func TestTopLevelJSONIndexMatchesStdlib(t *testing.T) {
	cases := append([]string{}, topLevelIndexCorpus...)
	// encoding/json allows nesting depth 10000 and rejects 10001.
	cases = append(cases, nestedJSON(10000), nestedJSON(10001))
	for _, body := range cases {
		checkTopLevelIndexAgainstStdlib(t, []byte(body))
	}
	if _, ok := scanJSONObject(nil, []byte(nestedJSON(10000))); !ok {
		t.Fatal("depth 10000 rejected")
	}
	if _, ok := scanJSONObject(nil, []byte(nestedJSON(10001))); ok {
		t.Fatal("depth 10001 accepted")
	}
}

func FuzzTopLevelJSONIndex(f *testing.F) {
	for _, body := range topLevelIndexCorpus {
		f.Add([]byte(body))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		checkTopLevelIndexAgainstStdlib(t, body)
	})
}

// --- byte-level rewrite ---

// decodeEquivalent compares two JSON documents as encoding/json sees them,
// keeping number literals exact.
func decodeEquivalent(t *testing.T, got, want []byte) {
	t.Helper()
	decode := func(raw []byte) any {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("invalid JSON %q: %v", raw, err)
		}
		return value
	}
	if g, w := decode(got), decode(want); !reflect.DeepEqual(g, w) {
		t.Fatalf("semantics differ:\n got  %s\n want %s", got, want)
	}
}

// checkRewriteAgainstLegacy compares a byte rewrite of body with the legacy
// re-marshal function applied to the normalized body: the rewrite first
// normalizes protected members, then edits. Where legacy changes nothing the
// rewrite must return the normalized bytes exactly.
func checkRewriteAgainstLegacy(t *testing.T, body []byte, got []byte, legacyFn func([]byte) []byte) {
	t.Helper()
	// rewriteTopLevelJSON normalizes top-level protected members for direct
	// callers; nested pricing members are normalized at request entry only
	// (normalizeProtectedMembers), so the oracle starts from the same state.
	normalized := body
	if members, ok := scanJSONObject(nil, body); ok {
		normalized, _ = normalizeScannedMembers(body, members)
	}
	legacy := legacyFn(normalized)
	if bytes.Equal(legacy, normalized) {
		if !bytes.Equal(got, normalized) {
			t.Fatalf("legacy kept %q unchanged, byte rewrite produced %q", normalized, got)
		}
		return
	}
	decodeEquivalent(t, got, legacy)
}

var modelRewriteCorpus = []string{
	`{"model":"alias"}`, `{"model":"ALIAS","messages":[]}`, `{"model":" alias "}`, `{"model":"other"}`,
	`{"model":null}`, `{"model":1}`, `{"Model":"alias"}`, `{"model":"alias","model":"alias"}`,
	`{"model":"other","model":"alias"}`, "{\"mod\\u0065l\":\"alias\"}", "{\"model\":\"al\\u0069as\"}",
	"{\"mod\\u0065l\":\"alias\",\"model\":\"alias\"}", "{\"model\":\"x\",\"\\u004dodel\":\"alias\"}",
	`{"messages":[{"model":"alias"}],"model":"alias"}`, ` { "model" : "alias" , "n" : 9007199254740993 } `,
	`{"model":"alias","x":"<b>&</b>","y":1.0e+2}`, `[{"model":"alias"}]`, `null`, `not-json`,
	`{"model":"alias",}`, `{"model":"alias"} trailing`, `{"x":1,"x":2,"model":"alias"}`,
}

func TestReplaceModelInBodyMatchesLegacySemantics(t *testing.T) {
	for _, body := range modelRewriteCorpus {
		got := replaceModelInBody([]byte(body), "Upstream-<Model>", "alias")
		checkRewriteAgainstLegacy(t, []byte(body), got, func(b []byte) []byte { return replaceModelInBodyLegacy(b, "Upstream-<Model>", "alias") })
	}
}

func FuzzReplaceModelInBodyMatchesLegacy(f *testing.F) {
	for _, body := range modelRewriteCorpus {
		f.Add([]byte(body), "alias")
	}
	f.Fuzz(func(t *testing.T, body []byte, model string) {
		got := replaceModelInBody(body, "UP-model", model)
		if model == "" || model == "UP-model" {
			// No rewrite requested: the body is not even scanned.
			if !bytes.Equal(got, body) {
				t.Fatalf("no-op rewrite changed %q into %q", body, got)
			}
			return
		}
		checkRewriteAgainstLegacy(t, body, got, func(b []byte) []byte { return replaceModelInBodyLegacy(b, "UP-model", model) })
	})
}

// Only the top-level model value bytes change; everything else is the
// client's original bytes (order, whitespace, escapes, number spelling).
func TestReplaceModelInBodyPreservesOriginalBytes(t *testing.T) {
	for _, tc := range []struct{ name, body, upstream, want string }{
		{
			"order whitespace and escapes",
			"{\n  \"temperature\": 1.0E+0,\n  \"model\" : \"glm-flash\",\n  \"messages\": [{\"content\":\"<b>\\u00e9 & \\\"q\\\"</b>\"}],\n  \"seed\": 9007199254740993\n}",
			"GLM-5.3-FLASH",
			"{\n  \"temperature\": 1.0E+0,\n  \"model\" : \"GLM-5.3-FLASH\",\n  \"messages\": [{\"content\":\"<b>\\u00e9 & \\\"q\\\"</b>\"}],\n  \"seed\": 9007199254740993\n}",
		},
		{
			"nested model untouched",
			`{"metadata":{"model":"glm-flash"},"model":"GLM-FLASH","tools":[{"function":{"arguments":"{\"model\":\"glm-flash\"}"}}]}`,
			"glm-5.3-flash",
			`{"metadata":{"model":"glm-flash"},"model":"glm-5.3-flash","tools":[{"function":{"arguments":"{\"model\":\"glm-flash\"}"}}]}`,
		},
		{
			"upstream encoded like json.Marshal",
			`{"model":"alias","x":[]}`,
			`a<b>&"c"`,
			"{\"model\":\"a\\u003cb\\u003e\\u0026\\\"c\\\"\",\"x\":[]}",
		},
		{
			"duplicate unrelated members kept",
			`{"x":1,"model":"alias","x":2}`,
			"real",
			`{"x":1,"model":"real","x":2}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := "glm-flash"
			if strings.Contains(tc.body, `"alias"`) {
				model = "alias"
			}
			if got := replaceModelInBody([]byte(tc.body), tc.upstream, model); string(got) != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

var streamUsageCorpus = []string{
	`{"stream":true}`, `{"stream":true,"stream_options":{}}`, `{"stream":true,"stream_options":{ }}`,
	`{"stream":true,"stream_options":{"other":{"x":1}}}`, `{"stream":true,"stream_options":{"include_usage":false}}`,
	"{\"stream\":true,\"stream_options\":{\"include_\\u0075sage\":false}}", `{"stream":true,"stream_options":null}`,
	`{"stream":true,"stream_options":[]}`, `{"stream":true,"stream_options":"x"}`, `{"stream":false}`,
	`{"stream":"true"}`, `{"stream":true,"stream":false}`, `{"stream":false,"stream":true}`,
	`{"stream":true,"stream_options":{},"stream_options":{"include_usage":true}}`, "{\"str\\u0065am\":true}",
	"{\"stream\":true,\"stream_\\u006fptions\":{}}", `{"stream" : true}`,
	` { "stream" : true , "n" : 9007199254740993 } `, `{"stream":true,"stream_options":{"a":1,"a":2}}`,
	`{}`, `null`, `[]`, `not-json`, `{"stream":true,`, `{"stream":true} x`,
}

func TestRequestStreamUsageIfMissingMatchesLegacySemantics(t *testing.T) {
	for _, body := range streamUsageCorpus {
		got := requestStreamUsageIfMissing([]byte(body))
		checkRewriteAgainstLegacy(t, []byte(body), got, requestStreamUsageIfMissingLegacy)
	}
}

func FuzzRequestStreamUsageIfMissingMatchesLegacy(f *testing.F) {
	for _, body := range streamUsageCorpus {
		f.Add([]byte(body))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		checkRewriteAgainstLegacy(t, body, requestStreamUsageIfMissing(body), requestStreamUsageIfMissingLegacy)
	})
}

func TestRequestStreamUsageIfMissingInsertsBytesOnly(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"stream":true}`, `{"stream":true,"stream_options":{"include_usage":true}}`},
		{"{ \"stream\" : true ,\n \"seed\" : 9007199254740993 \n}", "{ \"stream\" : true ,\n \"seed\" : 9007199254740993,\"stream_options\":{\"include_usage\":true} \n}"},
		{`{"stream":true,"stream_options":{}}`, `{"stream":true,"stream_options":{"include_usage":true}}`},
		{`{"stream":true,"stream_options":{ }}`, `{"stream":true,"stream_options":{"include_usage":true }}`},
		{`{"stream_options":{"z":1, "a":{"b":2}},"stream":true}`, `{"stream_options":{"z":1, "a":{"b":2},"include_usage":true},"stream":true}`},
	} {
		if got := requestStreamUsageIfMissing([]byte(tc.body)); string(got) != tc.want {
			t.Fatalf("body %s\n got  %s\n want %s", tc.body, got, tc.want)
		}
	}
}

// One assembly pass applies both edits; unrelated bytes stay original.
func TestPrepareTargetBodyAppliesModelAndStreamUsageInOneScan(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Content-Type", "application/json")
	mapped := &Target{RequestStreamUsageIfMissing: true, allowedModelIdx: map[string]string{"qwen-max": "QWEN3.8-MAX"}}
	body := []byte(`{"stream":true, "model":"qwen-max", "seed":9007199254740993}`)
	got := prepareTargetBody(mapped, r, body, "qwen-max", "QWEN3.8-MAX")
	want := `{"stream":true, "model":"QWEN3.8-MAX", "seed":9007199254740993,"stream_options":{"include_usage":true}}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// Model as the last member: the insertion follows the replaced value.
	body = []byte(`{"stream":true,"model":"qwen-max"}`)
	got = prepareTargetBody(mapped, r, body, "qwen-max", "QWEN3.8-MAX")
	if want := `{"stream":true,"model":"QWEN3.8-MAX","stream_options":{"include_usage":true}}`; string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// Duplicate and variant routing members are normalized (last exact wins)
	// before the edits, keeping every other byte.
	body = []byte(`{"stream":true, "model":"x", "Stream":false, "model":"qwen-max"}`)
	got = prepareTargetBody(mapped, r, body, "qwen-max", "QWEN3.8-MAX")
	if want := `{"stream":true, "model":"QWEN3.8-MAX","stream_options":{"include_usage":true}}`; string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// replaceModelInBodyLegacy is the original re-marshal model rewrite, kept as
// the semantic oracle for the byte rewrite.
func replaceModelInBodyLegacy(body []byte, upstream, model string) []byte {
	if model == "" || upstream == model || len(body) == 0 {
		return body
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	var value string
	if json.Unmarshal(payload["model"], &value) != nil || !strings.EqualFold(strings.TrimSpace(value), model) {
		return body
	}
	payload["model"], _ = json.Marshal(upstream)
	result, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return result
}

// requestStreamUsageIfMissingLegacy is the original re-marshal opt-in policy,
// kept as the semantic oracle for the byte insertion.
func requestStreamUsageIfMissingLegacy(body []byte) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || !bytes.Equal(bytes.TrimSpace(fields["stream"]), []byte("true")) {
		return body
	}
	options := make(map[string]json.RawMessage)
	if raw, exists := fields["stream_options"]; exists {
		if json.Unmarshal(raw, &options) != nil || options == nil {
			return body
		}
		if _, exists := options["include_usage"]; exists {
			return body
		}
	}
	options["include_usage"] = json.RawMessage("true")
	raw, err := json.Marshal(options)
	if err != nil {
		return body
	}
	fields["stream_options"] = raw
	assembled, err := json.Marshal(fields)
	if err != nil {
		return body
	}
	return assembled
}
