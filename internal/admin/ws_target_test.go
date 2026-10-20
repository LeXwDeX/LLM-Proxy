package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// wsCreateTarget POSTs a create-target body and returns the recorder.
func wsCreateTarget(t *testing.T, h http.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal create body: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/targets", bytes.NewReader(payload)))
	return rec
}

// wsTargetListField returns the prefer_websocket value found in the
// list-targets JSON response for the named target. It decodes into generic
// maps on purpose: the test must compile (and fail, not break the build)
// before config.Target gains the backing field.
func wsTargetListField(t *testing.T, h http.Handler, name string) (any, bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/targets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list targets expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Targets []map[string]any `json:"targets"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode list targets: %v", err)
	}
	for _, m := range resp.Targets {
		if m["name"] == name {
			v, ok := m["prefer_websocket"]
			return v, ok
		}
	}
	t.Fatalf("target %q not found in list response", name)
	return nil, false
}

// wsTargetStoreField marshals a stored config.Target and reads
// prefer_websocket from the JSON document persisted in bbolt.
func wsTargetStoreField(t *testing.T, target config.Target) map[string]any {
	t.Helper()
	raw, err := json.Marshal(target)
	if err != nil {
		t.Fatalf("marshal stored target: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal stored target: %v", err)
	}
	return m
}

// wsBoolTrue reports whether the document carries an explicit true. A key
// that is absent (omitempty on the zero value) or false counts as off, so the
// assertions do not depend on the exact json tag of the config field.
func wsBoolTrue(doc map[string]any) bool {
	v, ok := doc["prefer_websocket"]
	return ok && v == true
}

// wsListBoolTrue reports the prefer_websocket value from the list response:
// the handler always emits the key, but missing keys count as off.
func wsListBoolTrue(t *testing.T, h http.Handler, name string) bool {
	t.Helper()
	v, ok := wsTargetListField(t, h, name)
	if !ok {
		t.Fatalf("list response missing prefer_websocket key for %q", name)
	}
	return v == true
}

// wsCreateOpenAITarget seeds a target of the given endpoint type with
// prefer_websocket set as requested, and asserts the create call succeeded.
func wsCreateOpenAITarget(t *testing.T, h http.Handler, epType string, preferWS bool) {
	t.Helper()
	rec := wsCreateTarget(t, h, map[string]any{
		"name":             "ws-target",
		"endpoint_type":    epType,
		"usage_adapter":    config.UsageAdapterStandard,
		"endpoint":         "https://api.openai.com",
		"api_key":          "sk-primary-real-0001",
		"model_mappings":   []map[string]any{{"upstream": "gpt-4o"}},
		"prefer_websocket": preferWS,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create target expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestTargetPreferWebSocketCreatePersistsAndReadsBack(t *testing.T) {
	tempDir := t.TempDir()
	h, _, stores := newTargetUpdateTestHandler(t, tempDir, []string{"sk-primary-real-0001"})
	wsCreateOpenAITarget(t, h, config.EndpointTypeOpenAI, true)

	if !wsListBoolTrue(t, h, "ws-target") {
		t.Fatalf("list prefer_websocket expected true")
	}

	stored := wsTargetStoreField(t, mustGetTarget(t, stores.targetStore, "ws-target"))
	if !wsBoolTrue(stored) {
		t.Fatalf("store prefer_websocket expected true, got doc=%v", stored)
	}
}

func TestTargetPreferWebSocketUpdateOmittedPreserves(t *testing.T) {
	tempDir := t.TempDir()
	h, _, stores := newTargetUpdateTestHandler(t, tempDir, []string{"sk-primary-real-0001"})
	wsCreateOpenAITarget(t, h, config.EndpointTypeOpenAI, true)

	// PUT omits prefer_websocket and keeps endpoint_type openai: value stays true.
	body := targetUpdateBody("__existing_key_index__:0", nil)
	body["endpoint_type"] = config.EndpointTypeOpenAI
	rec := putTarget(t, h, "ws-target", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	if !wsListBoolTrue(t, h, "ws-target") {
		t.Fatalf("list prefer_websocket expected preserved true")
	}
	stored := wsTargetStoreField(t, mustGetTarget(t, stores.targetStore, "ws-target"))
	if !wsBoolTrue(stored) {
		t.Fatalf("store prefer_websocket expected preserved true, got doc=%v", stored)
	}
}

func TestTargetPreferWebSocketUpdateExplicitFalseClears(t *testing.T) {
	tempDir := t.TempDir()
	h, _, stores := newTargetUpdateTestHandler(t, tempDir, []string{"sk-primary-real-0001"})
	wsCreateOpenAITarget(t, h, config.EndpointTypeOpenAI, true)

	body := targetUpdateBody("__existing_key_index__:0", nil)
	body["endpoint_type"] = config.EndpointTypeOpenAI
	body["prefer_websocket"] = false
	rec := putTarget(t, h, "ws-target", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	if wsListBoolTrue(t, h, "ws-target") {
		t.Fatalf("list prefer_websocket expected false after explicit false")
	}
	stored := wsTargetStoreField(t, mustGetTarget(t, stores.targetStore, "ws-target"))
	if wsBoolTrue(stored) {
		t.Fatalf("store prefer_websocket expected false after explicit false, got doc=%v", stored)
	}
}

func TestTargetPreferWebSocketNonOpenAITypesNormalized(t *testing.T) {
	tempDir := t.TempDir()
	h, _, stores := newTargetUpdateTestHandler(t, tempDir, []string{"sk-primary-real-0001"})

	// Create a claude target with prefer_websocket=true: server must normalize to false.
	wsCreateOpenAITarget(t, h, config.EndpointTypeClaude, true)
	if wsListBoolTrue(t, h, "ws-target") {
		t.Fatalf("claude target prefer_websocket expected normalized false")
	}
	stored := wsTargetStoreField(t, mustGetTarget(t, stores.targetStore, "ws-target"))
	if wsBoolTrue(stored) {
		t.Fatalf("claude target store prefer_websocket expected false, got doc=%v", stored)
	}

	// Switch the same target to openai with prefer_websocket=true: allowed again.
	body := targetUpdateBody("__existing_key_index__:0", nil)
	body["endpoint_type"] = config.EndpointTypeOpenAI
	body["prefer_websocket"] = true
	rec := putTarget(t, h, "ws-target", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !wsListBoolTrue(t, h, "ws-target") {
		t.Fatalf("openai target prefer_websocket expected true after switch")
	}

	// Switch back to a non-openai type: stale true must be cleared.
	body = targetUpdateBody("__existing_key_index__:0", nil)
	body["endpoint_type"] = config.EndpointTypeAzureOpenAI
	body["prefer_websocket"] = true
	rec = putTarget(t, h, "ws-target", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if wsListBoolTrue(t, h, "ws-target") {
		t.Fatalf("azure target prefer_websocket expected cleared false")
	}
	stored = wsTargetStoreField(t, mustGetTarget(t, stores.targetStore, "ws-target"))
	if wsBoolTrue(stored) {
		t.Fatalf("azure target store prefer_websocket expected false, got doc=%v", stored)
	}
}

func TestTargetWriteRejectsUnknownAuthModeAndImageOperation(t *testing.T) {
	h, _, stores := newTargetUpdateTestHandler(t, t.TempDir(), []string{"sk-primary-real-0001"})
	base := map[string]any{"name": "enum-target", "endpoint_type": config.EndpointTypeClaude, "endpoint": "https://enum.invalid", "api_key": "sk-enum-0001", "usage_adapter": config.UsageAdapterStandard}
	for _, field := range []struct{ key, value string }{{"auth_mode", "x-api-key"}, {"image_operation", "edit"}} {
		body := map[string]any{}
		for k, v := range base {
			body[k] = v
		}
		body[field.key] = field.value
		if rec := wsCreateTarget(t, h, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("create accepted %s=%q: %d %s", field.key, field.value, rec.Code, rec.Body.String())
		}
	}
	body := map[string]any{}
	for k, v := range base {
		body[k] = v
	}
	body["auth_mode"] = " Bearer "
	if rec := wsCreateTarget(t, h, body); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("create rejected a case variant of a valid auth mode: %d %s", rec.Code, rec.Body.String())
	}
	if got := mustGetTarget(t, stores.targetStore, "enum-target").AuthMode; got != config.AuthModeBearer {
		t.Fatalf("auth mode stored as %q, want normalized %q", got, config.AuthModeBearer)
	}
	for _, field := range []struct{ key, value string }{{"auth_mode", "x-api-key"}, {"image_operation", "edit"}} {
		payload, _ := json.Marshal(map[string]any{field.key: field.value})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/targets/enum-target", bytes.NewReader(payload)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("update accepted %s=%q: %d %s", field.key, field.value, rec.Code, rec.Body.String())
		}
	}
}
