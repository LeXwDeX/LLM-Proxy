package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Issue #112: config loading must keep custom_body values as their original
// JSON text. Decoding into any would turn integers beyond 2^53 into float64.
func TestIssue112LoadPreservesCustomBodyJSONText(t *testing.T) {
	want := map[string]string{
		"seed":        "12345678901234567890",
		"temperature": "0.7",
		"label":       `"ok"`,
		"nested":      `{"id":9007199254740993}`,
		"enabled":     "true",
		"empty":       "null",
	}
	cfg := DefaultConfig()
	cfg.Targets = []Target{{
		Name:         "precision",
		EndpointType: EndpointTypeOpenAI,
		Endpoint:     "https://api.openai.com",
		APIKey:       "sk-test-key",
		UsageAdapter: UsageAdapterStandard,
		CustomBody: map[string]any{
			"seed":        json.RawMessage(want["seed"]),
			"temperature": json.RawMessage(want["temperature"]),
			"label":       json.RawMessage(want["label"]),
			"nested":      json.RawMessage(want["nested"]),
			"enabled":     json.RawMessage(want["enabled"]),
			"empty":       json.RawMessage(want["empty"]),
		},
	}}
	path := filepath.Join(t.TempDir(), "config.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	assertCustomBodyText(t, loaded.Targets[0].CustomBody, want)

	cloned := loaded.Clone()
	assertCustomBodyText(t, cloned.Targets[0].CustomBody, want)
}

func assertCustomBodyText(t *testing.T, body map[string]any, want map[string]string) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal custom_body: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("decode custom_body: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("custom_body keys = %v, want %v", got, want)
	}
	for key, text := range want {
		if string(got[key]) != text {
			t.Errorf("custom_body[%q] = %s, want %s", key, got[key], text)
		}
	}
}
