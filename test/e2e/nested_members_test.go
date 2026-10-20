//go:build e2e

package e2e

import "testing"

// Nested pricing members (stream_options.include_usage, the Kimi
// prompt_cache_options.ttl, cache_control) reach the upstream as the single
// member the proxy reads: duplicates and case variants are removed and every
// other byte is kept.
func TestNestedPricingMembersNormalized(t *testing.T) {
	name := target(t, map[string]any{"endpoint_type": "openai", "endpoint": w.urlA, "api_key": "sk-up-nested",
		"model_mappings": []any{map[string]any{"upstream": "e2e-nest-model"}}})
	_, key := client(t, []string{name}, nil)
	seq := w.fakeA.LastSeq()
	resp, body := call(t, key, "POST", "/v1/chat/completions", `{"model":"e2e-nest-model",`+
		`"stream_options":{"include_usage":false,"Include_Usage":false,"include_usage":true},`+
		`"prompt_cache_options":{"ttl":"1h","TTL":"1h","ttl":"5m"},`+
		`"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"},"cache_control":{"type":"none","Type":"x","type":"ephemeral"}}],`+
		`"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	rec := lastNewRecord(t, w.fakeA, seq)
	upstreamMember(t, rec, "stream_options", `{"include_usage":true}`)
	upstreamMember(t, rec, "prompt_cache_options", `{"ttl":"5m"}`)
	upstreamMember(t, rec, "system", `[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}]`)

	// Conflict-free nested members are forwarded byte for byte.
	seq = w.fakeA.LastSeq()
	resp, body = call(t, key, "POST", "/v1/chat/completions", `{"model":"e2e-nest-model","stream_options":{ "include_usage" : true },`+
		`"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	rec = lastNewRecord(t, w.fakeA, seq)
	upstreamMember(t, rec, "stream_options", `{ "include_usage" : true }`)
	upstreamMember(t, rec, "system", `[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}]`)
}
