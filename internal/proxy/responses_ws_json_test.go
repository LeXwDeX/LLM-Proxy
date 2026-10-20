package proxy

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// wsJSONSamples covers the shapes the WS paths edit: nested values, escapes,
// duplicates, odd whitespace and HTML characters that json.Marshal would escape.
var wsJSONSamples = []string{
	`{}`,
	` { "a" : 1 } `,
	`{"type":"response.create","model":"m","input":"<b>&amp;</b>","stream":false,"background":false}`,
	`{"a":[1,2,{"b":null}],"c":{"d":[true,false]},"e":-0.5e+10,"f":"\u00e9\"\\\/\b\f\n\r\t"}`,
	"{\n  \"type\": \"response.output_text.delta\",\n  \"delta\": \"x\"\n}",
	`{"type":"a","type":"b","stream_id":"s","stream_id":null}`,
	`{"\u0074ype":"escaped key","k":"\u2028\u2029","\u003c":"\ud83d\ude00"}`,
	`{"emoji":"😀","raw":"é","n":0,"m":1E2,"o":12.25}`,
	`{"deep":[[[[[[[[{"x":[[]]}]]]]]]]]}`,
}

var wsJSONInvalid = []string{
	``, `null`, `[]`, `"s"`, `1`, `{`, `}`, `{"a"}`, `{"a":}`, `{"a":1,}`, `{,"a":1}`,
	`{"a":1}x`, `{"a":01}`, `{"a":1.}`, `{"a":.5}`, `{"a":-}`, `{"a":1e}`, `{"a":tru}`,
	`{"a":"\x"}`, `{"a":"\u12"}`, "{\"a\":\"\x01\"}", `{"a":[1,]}`, `{"a":[,1]}`, `{a:1}`,
	`{"a":1 "b":2}`, `{"a":{"b":1}`, `{"a":"unterminated}`, `{"a":+1}`, `{"a":NaN}`,
}

func wsJSONIsObject(data []byte) bool {
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	return json.Valid(data) && len(trimmed) > 0 && trimmed[0] == '{'
}

// The single-pass scanner must accept exactly the objects encoding/json accepts.
func TestWSScanJSONObjectMatchesEncodingJSON(t *testing.T) {
	check := func(data []byte) {
		t.Helper()
		members, ok := scanJSONObject(nil, data)
		if want := wsJSONIsObject(data); ok != want {
			t.Fatalf("scan(%q) ok=%v, encoding/json=%v", data, ok, want)
		}
		if !ok {
			return
		}
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("decode %q: %v", data, err)
		}
		// Every member key/value is the raw token encoding/json reads, and the
		// last duplicate wins exactly as with map decoding.
		got := map[string]string{}
		for _, m := range members {
			var key string
			if err := json.Unmarshal(m.key, &key); err != nil {
				t.Fatalf("member key %q: %v", m.key, err)
			}
			if !bytes.HasPrefix(m.span, m.key) || !bytes.HasSuffix(m.span, m.value) {
				t.Fatalf("span %q does not cover key %q and value %q", m.span, m.key, m.value)
			}
			got[key] = string(m.value)
		}
		if len(got) != len(decoded) {
			t.Fatalf("members %v, encoding/json %v", got, decoded)
		}
		for key, raw := range decoded {
			if got[key] != string(raw) {
				t.Fatalf("member %q = %s, encoding/json %s", key, got[key], raw)
			}
		}
	}
	for _, sample := range wsJSONSamples {
		check([]byte(sample))
	}
	for _, sample := range wsJSONInvalid {
		check([]byte(sample))
	}
	// Deterministic mutations around the valid samples: drop, duplicate and
	// replace single bytes, which exercises every scanner state boundary.
	rng := rand.New(rand.NewSource(154))
	alphabet := []byte(`{}[]":,\ -+.eE0123456789tfnaulrsx` + "\n\t\x00\x7f\xff")
	for i := 0; i < 20000; i++ {
		base := []byte(wsJSONSamples[rng.Intn(len(wsJSONSamples))])
		if len(base) == 0 {
			continue
		}
		mutated := append([]byte(nil), base...)
		for n := rng.Intn(3) + 1; n > 0 && len(mutated) > 0; n-- {
			at := rng.Intn(len(mutated))
			switch rng.Intn(3) {
			case 0:
				mutated = append(mutated[:at], mutated[at+1:]...)
			case 1:
				mutated = append(mutated[:at], append([]byte{alphabet[rng.Intn(len(alphabet))]}, mutated[at:]...)...)
			default:
				mutated[at] = alphabet[rng.Intn(len(alphabet))]
			}
		}
		check(mutated)
	}
	deep := strings.Repeat("[", 10001) + strings.Repeat("]", 10001)
	check([]byte(`{"a":` + deep + `}`))
	check([]byte(`{"a":` + deep[1:len(deep)-1] + `}`))
	check([]byte(`{"a":` + deep[2:len(deep)-2] + `}`))
}

func FuzzWSScanJSONObject(f *testing.F) {
	for _, sample := range append(append([]string{}, wsJSONSamples...), wsJSONInvalid...) {
		f.Add([]byte(sample))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if _, ok := scanJSONObject(nil, data); ok != wsJSONIsObject(data) {
			t.Fatalf("scan(%q) ok=%v", data, ok)
		}
	})
}

func wsJSONSemantics(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("invalid JSON %q: %v", data, err)
	}
	return decoded
}

// The old request edits decoded every top-level member and re-encoded the
// object. The byte editor must produce the same JSON value without touching
// the untouched members' bytes (no HTML escaping, no key reordering).
func TestWSJSONEditsMatchMapRoundTrip(t *testing.T) {
	input := []byte(`{"z":"<tag> & </tag>","type":"response.create","background":false,"stream":false,"stream":true,"model":"m","a":{"y":1,"x":[2]}}`)
	members, ok := scanJSONObject(nil, input)
	if !ok {
		t.Fatal("sample rejected")
	}
	edited := jsonRebuild(members, []byte(`"type":"response.create"`), func(m jsonMember) ([]byte, bool) {
		return m.span, !m.named("stream") && !m.named("background") && !m.named("type")
	}, nil)
	var legacy map[string]json.RawMessage
	_ = json.Unmarshal(input, &legacy)
	delete(legacy, "stream")
	delete(legacy, "background")
	legacy["type"] = json.RawMessage(`"response.create"`)
	legacyWire, _ := json.Marshal(legacy)
	if !reflect.DeepEqual(wsJSONSemantics(t, edited), wsJSONSemantics(t, legacyWire)) {
		t.Fatalf("edit changed JSON semantics:\n got %s\nwant %s", edited, legacyWire)
	}
	if !bytes.Contains(edited, []byte(`"z":"<tag> & </tag>"`)) || !bytes.Contains(edited, []byte(`"a":{"y":1,"x":[2]}`)) || bytes.Contains(edited, []byte("\\u003c")) {
		t.Fatalf("untouched members were re-encoded: %s", edited)
	}
	if !bytes.HasPrefix(edited, []byte(`{"type":"response.create","z":`)) {
		t.Fatalf("member order not preserved: %s", edited)
	}
}

func TestWSJSONMemberLookup(t *testing.T) {
	members, ok := scanJSONObject(nil, []byte(`{"\u0074ype":"first","Type":"folded","type":"last","n":null}`))
	if !ok {
		t.Fatal("sample rejected")
	}
	if raw := jsonLast(members, "type"); string(raw) != `"last"` {
		t.Fatalf("exact lookup = %s", raw)
	}
	if raw := jsonLast(members, "missing"); raw != nil {
		t.Fatalf("missing lookup = %s", raw)
	}
	if s, ok := jsonString(jsonLast(members, "type")); !ok || s != "last" {
		t.Fatalf("string = %q %v", s, ok)
	}
	if _, ok := jsonString(jsonLast(members, "n")); ok {
		t.Fatal("null decoded as string")
	}
	if s, ok := jsonString([]byte(`"a\"<\u00e9"`)); !ok || s != "a\"<é" {
		t.Fatalf("escaped string = %q %v", s, ok)
	}
	escaped, ok := scanJSONObject(nil, []byte(`{"type":"plain","\u0074ype":"escaped"}`))
	if !ok || string(jsonLast(escaped, "type")) != `"escaped"` || !jsonHas(escaped, "type") {
		t.Fatalf("escaped key lookup = %s", jsonLast(escaped, "type"))
	}
	folded, ok := scanJSONObject(nil, []byte(`{"type":"x","TYPE":"response.completed"}`))
	if !ok || string(jsonLastFold(folded, "type")) != `"response.completed"` {
		t.Fatalf("folded lookup = %s", jsonLastFold(folded, "type"))
	}
}

// legacyWSRequestEdit is the pre-change pooled edit, kept to compare cost.
func legacyWSRequestEdit(body []byte) []byte {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	delete(payload, "stream")
	delete(payload, "background")
	delete(payload, "stream_id")
	delete(payload, "type")
	payload["type"] = json.RawMessage(`"response.create"`)
	wire, _ := json.Marshal(payload)
	return wire
}

func wsBenchmarkBody(size int) []byte {
	var text strings.Builder
	for text.Len() < size {
		text.WriteString(`if a < b && c > d { return "<html>" } `)
	}
	input, _ := json.Marshal([]map[string]any{{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": text.String()}}}})
	return []byte(`{"model":"gpt-test","stream":true,"store":false,"input":` + string(input) + `,"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`)
}

func BenchmarkWSRequestEditLegacy1MB(b *testing.B) {
	body := wsBenchmarkBody(1 << 20)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if legacyWSRequestEdit(body) == nil {
			b.Fatal("edit failed")
		}
	}
}

func BenchmarkWSRequestEditRaw1MB(b *testing.B) {
	body := wsBenchmarkBody(1 << 20)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	var members []jsonMember
	for i := 0; i < b.N; i++ {
		var ok bool
		members, ok = scanJSONObject(members[:0], body)
		if !ok {
			b.Fatal("scan failed")
		}
		_ = jsonRebuild(members, []byte(`"type":"response.create"`), func(m jsonMember) ([]byte, bool) {
			return m.span, !m.named("stream") && !m.named("background") && !m.named("stream_id") && !m.named("type")
		}, nil)
	}
}

const wsBenchmarkEvent = `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"if a < b && c > d","sequence_number":42}`

func BenchmarkWSEventPumpLegacy(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var data map[string]json.RawMessage
		if json.Unmarshal([]byte(wsBenchmarkEvent), &data) != nil {
			b.Fatal("decode")
		}
		var typ string
		_ = json.Unmarshal(data["type"], &typ)
		delete(data, "stream_id")
		encoded, _ := json.Marshal(data)
		_ = encoded
	}
}

func BenchmarkWSEventPumpRaw(b *testing.B) {
	b.ReportAllocs()
	var members []jsonMember
	event := []byte(wsBenchmarkEvent)
	for i := 0; i < b.N; i++ {
		var ok bool
		members, ok = scanJSONObject(members[:0], event)
		if !ok {
			b.Fatal("decode")
		}
		if _, ok := jsonString(jsonLast(members, "type")); !ok {
			b.Fatal("type")
		}
	}
}
