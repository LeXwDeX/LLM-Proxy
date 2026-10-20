// sanitize_test.go — sanitizeRequestBodyForAzure / ensureModelAllowed / normalizeAllowed 单元测试。
// 为后续性能优化（延迟 Azure sanitize、ensureModelAllowed 传参、normalizeAllowed 缓存）提供行为锁定。
package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// ---------- sanitizeRequestBodyForAzure ----------

func TestSanitizeRequestBodyForAzure_ChatCompletions_StripsUnknownFields(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"custom_field":"remove-me","foo":"bar"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	result, stripped := sanitizeRequestBodyForAzure(req, body)
	if len(stripped) == 0 {
		t.Fatal("expected some fields to be stripped")
	}

	var parsed map[string]any
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	// Whitelisted fields must be preserved
	if _, ok := parsed["model"]; !ok {
		t.Error("expected 'model' to be preserved")
	}
	if _, ok := parsed["messages"]; !ok {
		t.Error("expected 'messages' to be preserved")
	}
	if _, ok := parsed["temperature"]; !ok {
		t.Error("expected 'temperature' to be preserved")
	}

	// Non-whitelisted fields must be stripped
	if _, ok := parsed["custom_field"]; ok {
		t.Error("expected 'custom_field' to be stripped")
	}
	if _, ok := parsed["foo"]; ok {
		t.Error("expected 'foo' to be stripped")
	}
}

func TestSanitizeRequestBodyForAzure_Responses_StripsUnknownFields(t *testing.T) {
	body := []byte(`{"model":"gpt-5.2","input":"hello","prompt_cache_key":"sess-a","prompt_cache_retention":"24h","unknown":"x"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	result, stripped := sanitizeRequestBodyForAzure(req, body)
	if len(stripped) == 0 {
		t.Fatal("expected some fields to be stripped")
	}

	var parsed map[string]any
	json.Unmarshal(result, &parsed)

	// Whitelisted
	if _, ok := parsed["model"]; !ok {
		t.Error("expected 'model' preserved")
	}
	if _, ok := parsed["input"]; !ok {
		t.Error("expected 'input' preserved")
	}
	if _, ok := parsed["prompt_cache_key"]; !ok {
		t.Error("expected 'prompt_cache_key' preserved")
	}

	if got := parsed["prompt_cache_retention"]; got != "24h" {
		t.Errorf("expected cache retention preserved, got %v", got)
	}
	// Unknown fields are still stripped.
	if _, ok := parsed["unknown"]; ok {
		t.Error("expected 'unknown' stripped")
	}
}

func TestSanitizeRequestBodyForAzure_Embeddings_StripsUnknownFields(t *testing.T) {
	body := []byte(`{"model":"text-embedding-3-small","input":"hello","dimensions":512,"extra":"remove"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", nil)

	result, stripped := sanitizeRequestBodyForAzure(req, body)
	if len(stripped) == 0 {
		t.Fatal("expected some fields to be stripped")
	}

	var parsed map[string]any
	json.Unmarshal(result, &parsed)

	if _, ok := parsed["model"]; !ok {
		t.Error("expected 'model' preserved")
	}
	if _, ok := parsed["input"]; !ok {
		t.Error("expected 'input' preserved")
	}
	if _, ok := parsed["dimensions"]; !ok {
		t.Error("expected 'dimensions' preserved")
	}
	if _, ok := parsed["extra"]; ok {
		t.Error("expected 'extra' stripped")
	}
}

func TestSanitizeRequestBodyForAzure_NonMatchingPath_ReturnsOriginal(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","custom":"keep"}`)
	paths := []string{
		"/v1/images/generations",
		"/v1/audio/transcriptions",
		"/v1/models",
		"/v1/files",
		"/some/random/path",
	}
	for _, p := range paths {
		req := httptest.NewRequest(http.MethodPost, p, nil)
		result, stripped := sanitizeRequestBodyForAzure(req, body)
		if len(stripped) != 0 {
			t.Errorf("path %q: expected no stripping, got %v", p, stripped)
		}
		if string(result) != string(body) {
			t.Errorf("path %q: expected original body returned", p)
		}
	}
}

func TestSanitizeRequestBodyForAzure_NonModifyingMethod_ReturnsOriginal(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","custom":"keep"}`)
	methods := []string{http.MethodGet, http.MethodDelete, http.MethodHead, http.MethodOptions}
	for _, m := range methods {
		req := httptest.NewRequest(m, "/v1/chat/completions", nil)
		result, stripped := sanitizeRequestBodyForAzure(req, body)
		if len(stripped) != 0 {
			t.Errorf("method %q: expected no stripping, got %v", m, stripped)
		}
		if string(result) != string(body) {
			t.Errorf("method %q: expected original body returned", m)
		}
	}
}

func TestSanitizeRequestBodyForAzure_AllFieldsWhitelisted_ReturnsOriginal(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[],"temperature":0.5,"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	result, stripped := sanitizeRequestBodyForAzure(req, body)
	if len(stripped) != 0 {
		t.Errorf("expected no stripping when all fields whitelisted, got %v", stripped)
	}
	// When nothing stripped, should return original body (not re-marshaled)
	if string(result) != string(body) {
		t.Error("expected original body when nothing stripped")
	}
}

func TestSanitizeRequestBodyForAzure_EmptyBody_ReturnsOriginal(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	result, stripped := sanitizeRequestBodyForAzure(req, nil)
	if len(stripped) != 0 {
		t.Error("expected no stripping for nil body")
	}
	if result != nil {
		t.Error("expected nil result for nil body")
	}

	result, stripped = sanitizeRequestBodyForAzure(req, []byte{})
	if len(stripped) != 0 {
		t.Error("expected no stripping for empty body")
	}
}

func TestSanitizeRequestBodyForAzure_NilRequest_ReturnsOriginal(t *testing.T) {
	body := []byte(`{"model":"gpt-4o"}`)
	result, stripped := sanitizeRequestBodyForAzure(nil, body)
	if len(stripped) != 0 {
		t.Error("expected no stripping for nil request")
	}
	if string(result) != string(body) {
		t.Error("expected original body for nil request")
	}
}

func TestSanitizeRequestBodyForAzure_InvalidJSON_ReturnsOriginal(t *testing.T) {
	body := []byte(`this is not json at all`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	result, stripped := sanitizeRequestBodyForAzure(req, body)
	if len(stripped) != 0 {
		t.Error("expected no stripping for invalid JSON")
	}
	if string(result) != string(body) {
		t.Error("expected original body for invalid JSON")
	}
}

func TestSanitizeRequestBodyForAzure_PUT_Patch_AlsoSanitize(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[],"custom":"remove"}`)
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		req := httptest.NewRequest(method, "/v1/chat/completions", nil)
		result, stripped := sanitizeRequestBodyForAzure(req, body)
		if len(stripped) == 0 {
			t.Errorf("method %q: expected stripping", method)
		}
		var parsed map[string]any
		json.Unmarshal(result, &parsed)
		if _, ok := parsed["custom"]; ok {
			t.Errorf("method %q: expected 'custom' stripped", method)
		}
	}
}

func TestSanitizeRequestBodyForAzure_ChatCompletions_FullWhitelist(t *testing.T) {
	// Verify all documented whitelist fields are actually preserved
	allFields := map[string]any{
		"audio":                nil,
		"data_sources":         nil,
		"frequency_penalty":    0.5,
		"function_call":        "auto",
		"functions":            []any{},
		"logit_bias":           map[string]any{},
		"logprobs":             false,
		"max_completion_tokens": 100,
		"max_tokens":           200,
		"messages":             []any{},
		"metadata":             map[string]any{},
		"modalities":           []any{"text"},
		"model":                "gpt-4o",
		"n":                    1,
		"parallel_tool_calls":  true,
		"prediction":           map[string]any{},
		"presence_penalty":     0.0,
		"prompt_cache_key":     "sess",
		"reasoning_effort":     "medium",
		"response_format":      map[string]any{},
		"seed":                 42,
		"stop":                 "END",
		"store":                false,
		"stream":               true,
		"stream_options":       map[string]any{},
		"temperature":          0.7,
		"tool_choice":          "auto",
		"tools":                []any{},
		"top_logprobs":         5,
		"top_p":                0.9,
		"user":                 "test-user",
		"user_security_context": "ctx",
	}
	body, _ := json.Marshal(allFields)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	result, stripped := sanitizeRequestBodyForAzure(req, body)
	if len(stripped) != 0 {
		t.Errorf("expected all fields whitelisted, but stripped: %v", stripped)
	}

	var parsed map[string]any
	json.Unmarshal(result, &parsed)
	for key := range allFields {
		if _, ok := parsed[key]; !ok {
			t.Errorf("expected whitelisted field %q to be preserved", key)
		}
	}
}

func TestSanitizeRequestBodyForAzure_DeploymentPath_Matches(t *testing.T) {
	// Azure deployment paths like /openai/deployments/xxx/chat/completions should also match
	body := []byte(`{"model":"gpt-4o","messages":[],"custom":"remove"}`)
	paths := []string{
		"/openai/deployments/gpt-4o/chat/completions",
		"/openai/deployments/gpt-4o/responses",
		"/openai/deployments/text-embedding-3-small/embeddings",
	}
	for _, p := range paths {
		req := httptest.NewRequest(http.MethodPost, p, nil)
		_, stripped := sanitizeRequestBodyForAzure(req, body)
		if len(stripped) == 0 {
			t.Errorf("path %q: expected stripping", p)
		}
	}
}

// ---------- ensureModelAllowed ----------

func TestEnsureModelAllowed_NoRestrictions(t *testing.T) {
	s := &Service{}
	target := &Target{Name: "t1"}
	if _, err := s.ensureModelAllowed(target, "gpt-4o"); err != nil {
		t.Errorf("expected nil error for no restrictions, got %v", err)
	}
}

func TestEnsureModelAllowed_NilTarget(t *testing.T) {
	s := &Service{}
	if _, err := s.ensureModelAllowed(nil, "gpt-4o"); err != nil {
		t.Errorf("expected nil error for nil target, got %v", err)
	}
}

func TestEnsureModelAllowed_ModelAllowed(t *testing.T) {
	s := &Service{}
	target := &Target{
		Name:            "t1",
		ModelMappings:   []config.ModelMapping{{Upstream: "gpt-4o"}, {Upstream: "gpt-3.5-turbo"}},
		allowedModelIdx: map[string]string{"gpt-4o": "gpt-4o", "gpt-3.5-turbo": "gpt-3.5-turbo"},
	}
	if _, err := s.ensureModelAllowed(target, "gpt-4o"); err != nil {
		t.Errorf("expected nil error for allowed model, got %v", err)
	}
}

func TestEnsureModelAllowed_ModelNotAllowed(t *testing.T) {
	s := &Service{}
	target := &Target{
		Name:            "t1",
		ModelMappings:   []config.ModelMapping{{Upstream: "gpt-4o"}},
		allowedModelIdx: map[string]string{"gpt-4o": "gpt-4o"},
	}
	_, err := s.ensureModelAllowed(target, "gpt-3.5-turbo")
	if err == nil {
		t.Fatal("expected error for disallowed model")
	}
}

func TestEnsureModelAllowed_EmptyModel_WithRestrictions(t *testing.T) {
	s := &Service{}
	target := &Target{
		Name:            "t1",
		ModelMappings:   []config.ModelMapping{{Upstream: "gpt-4o"}},
		allowedModelIdx: map[string]string{"gpt-4o": "gpt-4o"},
	}
	_, err := s.ensureModelAllowed(target, "")
	if err == nil {
		t.Fatal("expected error for empty model with restrictions")
	}
}

// ---------- normalizeAllowed ----------

func TestNormalizeAllowed_Basic(t *testing.T) {
	result := normalizeAllowed([]string{"Primary", "SECONDARY", "tertiary"})
	expected := []string{"primary", "secondary", "tertiary"}
	for _, e := range expected {
		if _, ok := result[e]; !ok {
			t.Errorf("expected %q in normalized set", e)
		}
	}
	if len(result) != 3 {
		t.Errorf("expected 3 entries, got %d", len(result))
	}
}

func TestNormalizeAllowed_EmptyList(t *testing.T) {
	result := normalizeAllowed([]string{})
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}
}

func TestNormalizeAllowed_SkipsEmptyStrings(t *testing.T) {
	result := normalizeAllowed([]string{"", "  ", "valid"})
	if len(result) != 1 {
		t.Errorf("expected 1 entry, got %d", len(result))
	}
	if _, ok := result["valid"]; !ok {
		t.Error("expected 'valid' in result")
	}
}

func TestNormalizeAllowed_TrimsWhitespace(t *testing.T) {
	result := normalizeAllowed([]string{"  target-a  ", "target-b"})
	if _, ok := result["target-a"]; !ok {
		t.Error("expected 'target-a' (trimmed) in result")
	}
	if _, ok := result["target-b"]; !ok {
		t.Error("expected 'target-b' in result")
	}
}

// ---------- modelAllowed ----------

func TestModelAllowed_NilTarget(t *testing.T) {
	if modelAllowed(nil, "gpt-4o") {
		t.Error("expected false for nil target")
	}
}

func TestModelAllowed_NoRestrictions(t *testing.T) {
	target := &Target{Name: "t1"}
	if !modelAllowed(target, "any-model") {
		t.Error("expected true when no restrictions")
	}
}

func TestModelAllowed_EmptyModel_WithRestrictions(t *testing.T) {
	target := &Target{
		Name:            "t1",
		ModelMappings:   []config.ModelMapping{{Upstream: "gpt-4o"}},
		allowedModelIdx: map[string]string{"gpt-4o": "gpt-4o"},
	}
	if modelAllowed(target, "") {
		t.Error("expected false for empty model with restrictions")
	}
}

func TestModelAllowed_CaseInsensitive(t *testing.T) {
	target := &Target{
		Name:            "t1",
		ModelMappings:   []config.ModelMapping{{Upstream: "GPT-4o"}},
		allowedModelIdx: map[string]string{"gpt-4o": "GPT-4o"},
	}
	if !modelAllowed(target, "gpt-4o") {
		t.Error("expected case-insensitive match")
	}
}

func TestModelAllowed_FallbackToLinearScan(t *testing.T) {
	// When allowedModelIdx is nil (no mappings configured), any model is allowed,
	// including empty model (since there are no restrictions).
	target := &Target{
		Name: "t1",
	}
	if !modelAllowed(target, "gpt-4o") {
		t.Error("expected any model allowed when no restrictions")
	}
	// With mappings configured, unknown models are rejected.
	target = &Target{
		Name:            "t1",
		ModelMappings:   []config.ModelMapping{{Upstream: "gpt-4o"}, {Upstream: "gpt-3.5-turbo"}},
		allowedModelIdx: map[string]string{"gpt-4o": "gpt-4o", "gpt-3.5-turbo": "gpt-3.5-turbo"},
	}
	if !modelAllowed(target, "gpt-4o") {
		t.Error("expected known model allowed")
	}
	if modelAllowed(target, "gpt-5") {
		t.Error("expected unknown model rejected when restrictions exist")
	}
}

// TestBuildModelIndex_CommaFallback verifies that a comma-separated fallback
// string expands into one index entry per alias, all resolving to the same
// upstream (with original case preserved).
func TestBuildModelIndex_CommaFallback(t *testing.T) {
	idx := buildModelIndex([]config.ModelMapping{
		{Upstream: "GPT-4o", Fallback: "gpt-4, gpt4-turbo, my-gpt"},
	})

	want := map[string]string{
		"gpt-4o":     "GPT-4o",
		"gpt-4":      "GPT-4o",
		"gpt4-turbo": "GPT-4o",
		"my-gpt":     "GPT-4o",
	}
	if len(idx) != 4 {
		t.Fatalf("expected 4 entries (1 upstream + 3 aliases), got %d: %v", len(idx), idx)
	}
	for key, wantUp := range want {
		if got, ok := idx[key]; !ok {
			t.Errorf("missing key %q", key)
		} else if got != wantUp {
			t.Errorf("idx[%q] = %q, want %q", key, got, wantUp)
		}
	}
}

// TestBuildModelIndex_CommaFallbackEmptyAndWhitespace confirms empty entries
// between commas are skipped and surrounding whitespace is trimmed.
func TestBuildModelIndex_CommaFallbackEmptyAndWhitespace(t *testing.T) {
	idx := buildModelIndex([]config.ModelMapping{
		{Upstream: "gpt-4o", Fallback: " gpt-4 , , gpt4 ,"},
	})
	// Expect upstream + gpt-4 + gpt4 only; empty entries skipped.
	if len(idx) != 3 {
		t.Fatalf("expected 3 entries, got %d: %v", len(idx), idx)
	}
	if idx["gpt-4"] != "gpt-4o" {
		t.Errorf("idx[gpt-4] = %q", idx["gpt-4"])
	}
	if _, ok := idx[""]; ok {
		t.Error("empty-string key should not be present")
	}
}

// TestResolveUpstreamModel_CommaFallback confirms every alias resolves to the
// upstream name (original case) and an unknown alias is rejected.
func TestResolveUpstreamModel_CommaFallback(t *testing.T) {
	target := &Target{
		Name:            "t1",
		allowedModelIdx: buildModelIndex([]config.ModelMapping{{Upstream: "GPT-4o", Fallback: "gpt-4,gpt4-turbo"}}),
	}
	for _, alias := range []string{"gpt-4o", "GPT-4O", "gpt-4", "GPT-4", "gpt4-turbo"} {
		up, ok := resolveUpstreamModel(target, alias)
		if !ok {
			t.Errorf("alias %q: expected ok", alias)
		}
		if up != "GPT-4o" {
			t.Errorf("alias %q: upstream = %q, want GPT-4o", alias, up)
		}
	}
	if _, ok := resolveUpstreamModel(target, "unknown"); ok {
		t.Error("unknown alias should not resolve")
	}
}
