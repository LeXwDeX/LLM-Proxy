package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestInjectCustomBodyPreservesLargeJSONNumbers(t *testing.T) {
	const large = "9007199254740993"
	body := []byte(`{"model":"client-model","original":{"items":[{"id":` + large + `}]}}`)
	fields := map[string]any{
		"nested": map[string]any{
			"items": []any{map[string]any{"id": int64(9007199254740993)}},
		},
		"enabled":  false,
		"count":    0,
		"nullable": nil,
	}

	got := injectCustomBody(body, fields)
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("injectCustomBody returned invalid JSON: %v", err)
	}
	var original, nested map[string]json.RawMessage
	if err := json.Unmarshal(decoded["original"], &original); err != nil {
		t.Fatal(err)
	}
	if string(original["items"]) != `[{"id":`+large+`}]` {
		t.Errorf("existing large number changed: %s", original["items"])
	}
	if err := json.Unmarshal(decoded["nested"], &nested); err != nil {
		t.Fatal(err)
	}
	var nestedItems []json.RawMessage
	if err := json.Unmarshal(nested["items"], &nestedItems); err != nil {
		t.Fatal(err)
	}
	var nestedItem map[string]json.RawMessage
	if err := json.Unmarshal(nestedItems[0], &nestedItem); err != nil {
		t.Fatal(err)
	}
	if string(nestedItem["id"]) != large {
		t.Errorf("custom large number changed: %s", nestedItem["id"])
	}
	for key, want := range map[string]string{"enabled": "false", "count": "0", "nullable": "null"} {
		if string(decoded[key]) != want {
			t.Errorf("%s = %s, want %s", key, decoded[key], want)
		}
	}
}

func TestInjectCustomBodyFallbackAndNullInput(t *testing.T) {
	fields := map[string]any{"custom": true}
	for _, input := range []string{"not-json", ""} {
		if got := injectCustomBody([]byte(input), fields); string(got) != input {
			t.Errorf("input %q changed to %q", input, got)
		}
	}
	if got := injectCustomBody([]byte("null"), fields); string(got) != "null" {
		t.Errorf("JSON null input = %q, want unchanged null", got)
	}
	if got := injectCustomBody([]byte(`{"model":"original","stable":true}`), map[string]any{
		"model":  "override",
		"stable": false,
	}); string(got) != `{"model":"original","stable":false}` {
		t.Errorf("top-level override/blacklist behavior changed: %s", got)
	}
}

func TestPrepareTargetBodyPreservesLargeNumbersThroughModelRewrite(t *testing.T) {
	const large = "9007199254740993"
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Content-Type", "application/json")
	target := &Target{CustomBody: map[string]any{"custom": map[string]any{"big": int64(9007199254740993)}}}
	body := []byte(`{"model":"client-alias","value":{"big":` + large + `}}`)
	got := prepareTargetBody(target, r, body, "client-alias", "upstream-model")
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("prepareTargetBody returned invalid JSON: %v", err)
	}
	var value, custom map[string]json.RawMessage
	if err := json.Unmarshal(decoded["value"], &value); err != nil {
		t.Fatal(err)
	}
	if string(value["big"]) != large {
		t.Errorf("original request number changed through model rewrite: %s", value["big"])
	}
	if err := json.Unmarshal(decoded["custom"], &custom); err != nil {
		t.Fatal(err)
	}
	if string(custom["big"]) != large {
		t.Errorf("custom_body number changed: %s", custom["big"])
	}
}

func TestInjectCustomBodyPreservesRawMessageText(t *testing.T) {
	const large = "12345678901234567890"
	const nested = `{"id":9007199254740993}`
	got := injectCustomBody([]byte(`{"model":"m"}`), map[string]any{
		"seed":     json.RawMessage(large),
		"nested":   json.RawMessage(nested),
		"ordinary": json.RawMessage(`"ok"`),
		"enabled":  json.RawMessage("false"),
	})
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("injectCustomBody returned invalid JSON: %v", err)
	}
	for key, want := range map[string]string{
		"seed": large, "nested": nested, "ordinary": `"ok"`, "enabled": "false",
	} {
		if string(decoded[key]) != want {
			t.Errorf("custom_body[%q] = %s, want %s", key, decoded[key], want)
		}
	}
}
