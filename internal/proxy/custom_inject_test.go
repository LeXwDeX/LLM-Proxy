package proxy

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestValidateCustomHeaders(t *testing.T) {
	t.Run("valid headers pass", func(t *testing.T) {
		got := validateCustomHeaders(map[string]string{
			"X-DashScope-DataInspection": `{"input":"disable"}`,
			"X-Custom-Foo":               "bar",
		})
		if got != "" {
			t.Errorf("expected no blocked header, got %q", got)
		}
	})
	t.Run("blocked authorization", func(t *testing.T) {
		got := validateCustomHeaders(map[string]string{
			"Authorization": "Bearer evil",
		})
		if got == "" {
			t.Error("expected Authorization to be blocked")
		}
	})
	t.Run("blocked case-insensitive", func(t *testing.T) {
		got := validateCustomHeaders(map[string]string{
			"content-type": "text/plain",
		})
		if got == "" {
			t.Error("expected content-type to be blocked (case-insensitive)")
		}
	})
	t.Run("nil map passes", func(t *testing.T) {
		got := validateCustomHeaders(nil)
		if got != "" {
			t.Errorf("nil map should pass, got %q", got)
		}
	})
}

func TestValidateCustomBody(t *testing.T) {
	t.Run("valid fields pass", func(t *testing.T) {
		got := validateCustomBody(map[string]any{
			"enable_search": true,
			"result_format": "message",
		})
		if got != "" {
			t.Errorf("expected no blocked field, got %q", got)
		}
	})
	t.Run("blocked model", func(t *testing.T) {
		got := validateCustomBody(map[string]any{"model": "evil"})
		if got == "" {
			t.Error("expected model to be blocked")
		}
	})
	t.Run("blocked messages", func(t *testing.T) {
		got := validateCustomBody(map[string]any{"messages": []any{}})
		if got == "" {
			t.Error("expected messages to be blocked")
		}
	})
	t.Run("nil map passes", func(t *testing.T) {
		got := validateCustomBody(nil)
		if got != "" {
			t.Errorf("nil map should pass, got %q", got)
		}
	})
}

func TestInjectCustomHeaders(t *testing.T) {
	req, _ := http.NewRequest("POST", "http://example.com/test", nil)
	req.Header.Set("X-Existing", "old")

	t.Run("injects custom headers", func(t *testing.T) {
		injectCustomHeaders(req, map[string]string{
			"X-DashScope-DataInspection": `{"input":"disable","output":"disable"}`,
			"X-Custom":                   "value",
		})
		if got := req.Header.Get("X-DashScope-DataInspection"); got != `{"input":"disable","output":"disable"}` {
			t.Errorf("custom header not injected: %q", got)
		}
		if got := req.Header.Get("X-Custom"); got != "value" {
			t.Errorf("custom header not injected: %q", got)
		}
	})

	t.Run("skips blacklisted headers", func(t *testing.T) {
		req2, _ := http.NewRequest("POST", "http://example.com/test", nil)
		injectCustomHeaders(req2, map[string]string{
			"Authorization": "Bearer should-not-inject",
		})
		if got := req2.Header.Get("Authorization"); got == "Bearer should-not-inject" {
			t.Error("blacklisted header should not be injected")
		}
	})
}

func TestInjectCustomBody(t *testing.T) {
	t.Run("merges custom fields", func(t *testing.T) {
		body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
		result := injectCustomBody([]byte(body), map[string]any{
			"enable_search": true,
			"temperature":   0.7,
		})
		var parsed map[string]any
		if err := json.Unmarshal(result, &parsed); err != nil {
			t.Fatalf("invalid JSON result: %v", err)
		}
		if _, ok := parsed["enable_search"]; !ok {
			t.Error("enable_search not injected")
		}
		if parsed["model"] != "gpt-4" {
			t.Errorf("model changed unexpectedly: %v", parsed["model"])
		}
	})

	t.Run("skips blacklisted fields", func(t *testing.T) {
		body := `{"model":"gpt-4","messages":[]}`
		result := injectCustomBody([]byte(body), map[string]any{
			"model":    "overridden-model",
			"messages": []any{},
			"custom":   "ok",
		})
		var parsed map[string]any
		if err := json.Unmarshal(result, &parsed); err != nil {
			t.Fatalf("invalid JSON result: %v", err)
		}
		if parsed["model"] != "gpt-4" {
			t.Errorf("model was overwritten: %v", parsed["model"])
		}
		if _, ok := parsed["custom"]; !ok {
			t.Error("non-blacklisted field should be injected")
		}
	})

	t.Run("empty body returns original", func(t *testing.T) {
		result := injectCustomBody([]byte(""), map[string]any{"foo": "bar"})
		if string(result) != "" {
			t.Errorf("empty body should be returned as-is, got %q", string(result))
		}
	})

	t.Run("nil fields returns original", func(t *testing.T) {
		body := `{"model":"gpt-4"}`
		result := injectCustomBody([]byte(body), nil)
		if string(result) != body {
			t.Errorf("nil fields should return original body")
		}
	})
}
