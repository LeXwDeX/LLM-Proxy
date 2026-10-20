package proxy

import "testing"

// parseUsageMap cache-hit billing regression tests.
//
// Background: OpenAI / DeepSeek / Azure / Gemini report prompt_tokens / promptTokenCount
// that INCLUDE cache_hit tokens. Billing must charge the cache-hit portion at CacheRead
// price, not InputPrice (miss) price. parseUsageMap therefore subtracts cached from
// input for these formats so downstream EstimateEventCost computes the miss correctly.
//
// Claude is unaffected: its input_tokens already excludes cache tokens, and the Claude
// branch (cache_read_input_tokens / cache_creation_input_tokens) runs before the
// subtraction guard, setting cacheRead>0 which skips the subtraction.

func TestParseUsageMap_OpenAIPromptTokensExcludesCacheHit(t *testing.T) {
	// OpenAI format: prompt_tokens includes cache_hit.
	m := map[string]any{
		"prompt_tokens":     100,
		"completion_tokens": 50,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 80,
		},
	}
	tok, ok := parseUsageMap(m)
	if !ok {
		t.Fatal("expected ok")
	}
	if tok.InputTokens != 20 {
		t.Errorf("InputTokens: got %d, want 20 (prompt_tokens - cached)", tok.InputTokens)
	}
	if tok.CachedTokens != 80 {
		t.Errorf("CachedTokens: got %d, want 80", tok.CachedTokens)
	}
	if tok.OutputTokens != 50 {
		t.Errorf("OutputTokens: got %d, want 50", tok.OutputTokens)
	}
}

func TestParseUsageMap_DeepSeekFormatExcludesCacheHit(t *testing.T) {
	// DeepSeek format mirrors OpenAI (prompt_tokens_details.cached_tokens).
	m := map[string]any{
		"prompt_tokens":     68011,
		"completion_tokens": 449,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 66816,
		},
	}
	tok, ok := parseUsageMap(m)
	if !ok {
		t.Fatal("expected ok")
	}
	if tok.InputTokens != 1195 {
		t.Errorf("InputTokens: got %d, want 1195", tok.InputTokens)
	}
	if tok.CachedTokens != 66816 {
		t.Errorf("CachedTokens: got %d, want 66816", tok.CachedTokens)
	}
}

func TestParseUsageMap_ClaudeInputNotDoublySubtracted(t *testing.T) {
	// Claude format: input_tokens already excludes cache; must NOT be subtracted again.
	m := map[string]any{
		"input_tokens":                20,
		"output_tokens":               100,
		"cache_read_input_tokens":     80,
		"cache_creation_input_tokens": 10,
	}
	tok, ok := parseUsageMap(m)
	if !ok {
		t.Fatal("expected ok")
	}
	if tok.InputTokens != 20 {
		t.Errorf("Claude InputTokens: got %d, want 20 (not subtracted)", tok.InputTokens)
	}
	if tok.CachedTokens != 80 {
		t.Errorf("CachedTokens: got %d, want 80", tok.CachedTokens)
	}
	if tok.CacheCreationTokens != 10 {
		t.Errorf("CacheCreationTokens: got %d, want 10", tok.CacheCreationTokens)
	}
}

func TestParseUsageMap_GeminiPromptTokenCountExcludesCache(t *testing.T) {
	// Gemini native format: promptTokenCount includes cache.
	m := map[string]any{
		"promptTokenCount":        100,
		"candidatesTokenCount":    50,
		"cachedContentTokenCount": 80,
	}
	tok, ok := parseUsageMap(m)
	if !ok {
		t.Fatal("expected ok")
	}
	if tok.InputTokens != 20 {
		t.Errorf("InputTokens: got %d, want 20", tok.InputTokens)
	}
	if tok.CachedTokens != 80 {
		t.Errorf("CachedTokens: got %d, want 80", tok.CachedTokens)
	}
}

func TestParseUsageMap_NoCacheHitUnchanged(t *testing.T) {
	// No cache_hit present: input must not be reduced.
	m := map[string]any{
		"prompt_tokens":     100,
		"completion_tokens": 50,
	}
	tok, ok := parseUsageMap(m)
	if !ok {
		t.Fatal("expected ok")
	}
	if tok.InputTokens != 100 {
		t.Errorf("InputTokens: got %d, want 100 (no cache)", tok.InputTokens)
	}
	if tok.CachedTokens != 0 {
		t.Errorf("CachedTokens: got %d, want 0", tok.CachedTokens)
	}
}

func TestParseUsageMap_DeepSeekNativeCacheHitFields(t *testing.T) {
	// DeepSeek native format: prompt_cache_hit_tokens / prompt_cache_miss_tokens.
	// prompt_tokens = hit + miss; hit must be billed at CacheRead price, miss at Input.
	// (Real-world example from DeepSeek API docs.)
	m := map[string]any{
		"prompt_tokens":            68011, // = 66816 hit + 1195 miss
		"completion_tokens":        449,
		"prompt_cache_hit_tokens":  66816,
		"prompt_cache_miss_tokens": 1195,
	}
	tok, ok := (usageParser{usageAdapter: "deepseek", model: "deepseek-chat"}).parseMap(m, "")
	if !ok {
		t.Fatal("expected ok")
	}
	if tok.InputTokens != 1195 {
		t.Errorf("InputTokens: got %d, want 1195 (prompt_tokens - hit)", tok.InputTokens)
	}
	if tok.CachedTokens != 66816 {
		t.Errorf("CachedTokens: got %d, want 66816", tok.CachedTokens)
	}
	if tok.OutputTokens != 449 {
		t.Errorf("OutputTokens: got %d, want 449", tok.OutputTokens)
	}
}

func TestParseUsageMap_DeepSeekNativeCacheHitZeroHit(t *testing.T) {
	// DeepSeek with no cache hit: hit=0 should not reduce input.
	m := map[string]any{
		"prompt_tokens":            1000,
		"completion_tokens":        200,
		"prompt_cache_hit_tokens":  0,
		"prompt_cache_miss_tokens": 1000,
	}
	tok, ok := (usageParser{usageAdapter: "deepseek", model: "deepseek-chat"}).parseMap(m, "")
	if !ok {
		t.Fatal("expected ok")
	}
	if tok.InputTokens != 1000 {
		t.Errorf("InputTokens: got %d, want 1000 (no hit)", tok.InputTokens)
	}
	if tok.CachedTokens != 0 {
		t.Errorf("CachedTokens: got %d, want 0", tok.CachedTokens)
	}
}

func TestParseUsageMap_FullCacheHitInputZero(t *testing.T) {
	// Boundary: every prompt token is a cache hit (input == cached).
	m := map[string]any{
		"prompt_tokens":     100,
		"completion_tokens": 50,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 100,
		},
	}
	tok, ok := parseUsageMap(m)
	if !ok {
		t.Fatal("expected ok")
	}
	if tok.InputTokens != 0 {
		t.Errorf("InputTokens: got %d, want 0 (full cache hit)", tok.InputTokens)
	}
	if tok.CachedTokens != 100 {
		t.Errorf("CachedTokens: got %d, want 100", tok.CachedTokens)
	}
}
