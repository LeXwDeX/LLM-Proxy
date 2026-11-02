package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Issue #112: custom_body values must survive storage, the admin list response
// and the raw-text request field without passing through float64. Large
// integers are the observable failure; ordinary values must keep working too.

const issue112LargeInt = "12345678901234567890"
const issue112NestedInt = "9007199254740993"

func issue112PutRaw(t *testing.T, h http.Handler, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "http://example.com/data/targets/"+name, bytes.NewReader([]byte(body)))
	h.ServeHTTP(rec, req)
	return rec
}

func issue112PostRaw(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://example.com/data/targets", bytes.NewReader([]byte(body)))
	h.ServeHTTP(rec, req)
	return rec
}

func issue112ListRaw(t *testing.T, h http.Handler, role string) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://example.com/data/targets", nil)
	if role != "" {
		req = req.WithContext(context.WithValue(req.Context(), sessionContextKey{}, &Session{Username: role, Role: role}))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list targets status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func issue112AssertCustomBody(t *testing.T, body map[string]any, want map[string]string) {
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

func issue112AssertListedCustomBody(t *testing.T, raw []byte, want map[string]string) {
	t.Helper()
	var listed struct {
		Targets []struct {
			CustomBody    json.RawMessage   `json:"custom_body"`
			CustomBodyRaw map[string]string `json:"custom_body_raw"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listed.Targets) != 1 {
		t.Fatalf("list returned %d targets, want 1: %s", len(listed.Targets), raw)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(listed.Targets[0].CustomBody, &body); err != nil {
		t.Fatalf("decode listed custom_body: %v", err)
	}
	for key, text := range want {
		if string(body[key]) != text {
			t.Errorf("listed custom_body[%q] = %s, want %s", key, body[key], text)
		}
		if listed.Targets[0].CustomBodyRaw[key] != text {
			t.Errorf("listed custom_body_raw[%q] = %q, want %q", key, listed.Targets[0].CustomBodyRaw[key], text)
		}
	}
	if !bytes.Contains(raw, []byte(issue112LargeInt)) {
		t.Errorf("list response lost the large integer: %s", raw)
	}
}

func TestIssue112UpdateTargetPreservesCustomBodyJSONText(t *testing.T) {
	h, _, stores := newTargetUpdateTestHandler(t, t.TempDir(), []string{"sk-primary-0001"})
	want := map[string]string{
		"seed":        issue112LargeInt,
		"temperature": "0.7",
		"label":       `"ok"`,
		"nested":      `{"id":` + issue112NestedInt + `}`,
		"enabled":     "true",
		"empty":       "null",
	}
	payload := `{
		"allow_bearer_passthrough": false,
		"custom_body": {
			"seed": 12345678901234567890,
			"temperature": 0.7,
			"label": "ok",
			"nested": {"id": 9007199254740993},
			"enabled": true,
			"empty": null
		}
	}`
	rec := issue112PutRaw(t, h, "target-1", payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", rec.Code, rec.Body.String())
	}

	stored := mustGetTarget(t, stores.targetStore, "target-1")
	issue112AssertCustomBody(t, stored.CustomBody, want)

	issue112AssertListedCustomBody(t, issue112ListRaw(t, h, RoleAdmin), want)

	readonly := issue112ListRaw(t, h, RoleReadonly)
	var listed struct {
		Targets []map[string]json.RawMessage `json:"targets"`
	}
	if err := json.Unmarshal(readonly, &listed); err != nil || len(listed.Targets) != 1 {
		t.Fatalf("decode readonly list: %v, body = %s", err, readonly)
	}
	if string(listed.Targets[0]["custom_body_raw"]) != "null" {
		t.Errorf("readonly custom_body_raw = %s, want null", listed.Targets[0]["custom_body_raw"])
	}
	if strings.Contains(string(readonly), "12345678901234567890") {
		t.Errorf("readonly response leaked raw custom body text: %s", readonly)
	}
}

func TestIssue112RawCustomBodyRequestWinsOverLossyField(t *testing.T) {
	h, _, stores := newTargetUpdateTestHandler(t, t.TempDir(), []string{"sk-primary-0001"})
	want := map[string]string{
		"seed":        issue112LargeInt,
		"temperature": "0.7",
		"label":       `"ok"`,
		"nested":      `{"id":` + issue112NestedInt + `}`,
	}
	payload := `{
		"allow_bearer_passthrough": false,
		"custom_body": {"seed": 1},
		"custom_body_raw": {
			"seed": "12345678901234567890",
			"temperature": "0.7",
			"label": "\"ok\"",
			"nested": "{\"id\":9007199254740993}"
		}
	}`
	rec := issue112PutRaw(t, h, "target-1", payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", rec.Code, rec.Body.String())
	}

	stored := mustGetTarget(t, stores.targetStore, "target-1")
	issue112AssertCustomBody(t, stored.CustomBody, want)
	issue112AssertListedCustomBody(t, issue112ListRaw(t, h, RoleAdmin), want)
}

func TestIssue112RejectsInvalidOrBlockedRawCustomBody(t *testing.T) {
	h, _, stores := newTargetUpdateTestHandler(t, t.TempDir(), []string{"sk-primary-0001"})
	seed := issue112PutRaw(t, h, "target-1", `{"allow_bearer_passthrough": false, "custom_body": {"keep": "original"}}`)
	if seed.Code != http.StatusOK {
		t.Fatalf("seed update status = %d, body = %s", seed.Code, seed.Body.String())
	}
	for name, payload := range map[string]string{
		"invalid json": `{"allow_bearer_passthrough": false, "custom_body_raw": {"seed": "{not json"}}`,
		"blocked key":  `{"allow_bearer_passthrough": false, "custom_body_raw": {"Model": "\"x\""}}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := issue112PutRaw(t, h, "target-1", payload)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("update status = %d, want 400, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
	stored := mustGetTarget(t, stores.targetStore, "target-1")
	issue112AssertCustomBody(t, stored.CustomBody, map[string]string{"keep": `"original"`})
}

func TestIssue112CreateTargetPreservesRawCustomBody(t *testing.T) {
	h, _, stores := newTargetUpdateTestHandler(t, t.TempDir(), []string{"sk-primary-0001"})
	want := map[string]string{
		"seed":        issue112LargeInt,
		"temperature": "0.7",
		"label":       `"ok"`,
	}
	payload := `{
		"name": "raw-body",
		"endpoint_type": "openai",
		"usage_adapter": "standard",
		"endpoint": "https://api.openai.com",
		"api_key": "sk-primary-0001",
		"custom_body_raw": {
			"seed": "12345678901234567890",
			"temperature": "0.7",
			"label": "\"ok\""
		}
	}`
	rec := issue112PostRaw(t, h, payload)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	stored := mustGetTarget(t, stores.targetStore, "raw-body")
	issue112AssertCustomBody(t, stored.CustomBody, want)
}
