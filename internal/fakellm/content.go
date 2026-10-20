package fakellm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Provider dialects. They only change field names and error envelopes; the
// numbers come from the same Usage.
const (
	FlavorOpenAI     = "openai"
	FlavorAzure      = "azure"
	FlavorAnthropic  = "anthropic"
	FlavorGemini     = "gemini"
	FlavorGLM        = "glm"
	FlavorDeepSeek   = "deepseek"
	FlavorQwen       = "qwen"
	FlavorOpenRouter = "openrouter"
	FlavorKimi       = "kimi"
)

// FlavorMinimax answers with MiniMax's base_resp envelope.
const FlavorMinimax = "minimax"

// vendorSegments map an explicit path segment to a dialect. Tests mount the
// fake behind e.g. http://fake/glm/api/paas/v4 so one instance can serve every
// vendor; the marker segment is not required by any route.
var vendorSegments = map[string]string{
	"openai": FlavorOpenAI, "azure": FlavorAzure, "anthropic": FlavorAnthropic, "claude": FlavorAnthropic,
	"gemini": FlavorGemini, "google": FlavorGemini, "glm": FlavorGLM, "zhipu": FlavorGLM, "bigmodel": FlavorGLM,
	"deepseek": FlavorDeepSeek, "qwen": FlavorQwen, "dashscope": FlavorQwen, "bailian": FlavorQwen,
	"openrouter": FlavorOpenRouter, "kimi": FlavorKimi, "moonshot": FlavorKimi, "minimax": FlavorMinimax,
}

// flavorForPath derives the dialect from the upstream path the proxy
// assembled (endpoint prefix + client path): first an explicit vendor
// segment, then the vendors' own distinctive path prefixes.
func flavorForPath(path string) string {
	p := strings.ToLower(path)
	if seg, _, ok := strings.Cut(strings.TrimPrefix(p, "/"), "/"); ok {
		if f, ok := vendorSegments[seg]; ok {
			return f
		}
	}
	switch {
	case strings.Contains(p, "/api/paas/") || strings.Contains(p, "/api/coding/paas/"):
		return FlavorGLM
	case strings.Contains(p, "/compatible-mode/") || strings.Contains(p, "/apps/anthropic"):
		return FlavorQwen
	case strings.Contains(p, "/openai/deployments/"):
		return FlavorAzure
	}
	return ""
}

// Usage is the canonical token count of one reply. Input includes cached and
// cache-write tokens; Output includes reasoning tokens.
type Usage struct {
	Input      int
	Output     int
	CachedRead int
	CacheWrite int // 5-minute / default writes
	CacheWr1h  int
	Reasoning  int
	Images     int
}

func (u Usage) Total() int { return u.Input + u.Output }

// uncachedInput is input minus cache reads and writes (Anthropic input_tokens).
func (u Usage) uncachedInput() int {
	n := u.Input - u.CachedRead - u.CacheWrite - u.CacheWr1h
	if n < 0 {
		return 0
	}
	return n
}

// EstimateTokens is the deterministic token count used for request text.
func EstimateTokens(text string) int {
	return len([]rune(text))/4 + 8
}

// computeUsage applies overrides to the deterministic default.
func computeUsage(promptText string, maxTokens int, spec *UsageSpec) Usage {
	out := 16
	if maxTokens > 0 && maxTokens < out {
		out = maxTokens
	}
	u := Usage{Input: EstimateTokens(promptText), Output: out}
	if spec != nil {
		set := func(dst *int, v *int) {
			if v != nil {
				*dst = *v
			}
		}
		set(&u.Input, spec.Input)
		set(&u.Output, spec.Output)
		set(&u.CachedRead, spec.CachedRead)
		set(&u.CacheWrite, spec.CacheWrite)
		set(&u.CacheWr1h, spec.CacheWr1h)
		set(&u.Reasoning, spec.Reasoning)
		set(&u.Images, spec.Images)
	}
	return u
}

// replyText is the fake answer: stable for identical request bytes.
func replyText(name string, raw []byte, override string) string {
	if override != "" {
		return override
	}
	sum := sha256.Sum256(raw)
	return "fake reply " + hex.EncodeToString(sum[:4]) + " from " + name
}

// splitText cuts text into n roughly equal deltas.
func splitText(text string, n int) []string {
	if n <= 0 {
		n = 3
	}
	r := []rune(text)
	if n > len(r) {
		n = len(r)
	}
	if n == 0 {
		return []string{""}
	}
	parts := make([]string, 0, n)
	step := (len(r) + n - 1) / n
	for i := 0; i < len(r); i += step {
		end := i + step
		if end > len(r) {
			end = len(r)
		}
		parts = append(parts, string(r[i:end]))
	}
	return parts
}

// promptText collects user-visible text from any request schema to size the
// deterministic input count. It is intentionally rough.
func promptText(req map[string]any) string {
	var b strings.Builder
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			b.WriteString(x)
			b.WriteByte(' ')
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, k := range []string{"text", "content", "input", "prompt", "parts", "contents", "messages", "system", "instructions"} {
				if e, ok := x[k]; ok {
					walk(e)
				}
			}
		}
	}
	for _, k := range []string{"system", "instructions", "messages", "input", "prompt", "contents", "systemInstruction"} {
		if v, ok := req[k]; ok {
			walk(v)
		}
	}
	return b.String()
}

func intField(req map[string]any, keys ...string) int {
	for _, k := range keys {
		if v, ok := req[k].(float64); ok {
			return int(v)
		}
	}
	return 0
}

// --- usage renderers -------------------------------------------------------

// openAIChatUsage renders Chat Completions usage. Every dialect keeps the
// standard fields; dialect-specific cache fields are added on top so one
// response is a superset parsers of that provider understand.
func openAIChatUsage(u Usage, flavor string, stream bool) map[string]any {
	prompt := map[string]any{"cached_tokens": u.CachedRead, "audio_tokens": 0}
	completion := map[string]any{"reasoning_tokens": u.Reasoning, "audio_tokens": 0,
		"accepted_prediction_tokens": 0, "rejected_prediction_tokens": 0}
	m := map[string]any{
		"prompt_tokens":             u.Input,
		"completion_tokens":         u.Output,
		"total_tokens":              u.Total(),
		"prompt_tokens_details":     prompt,
		"completion_tokens_details": completion,
	}
	switch flavor {
	case FlavorMinimax:
		// Official Chat Usage, checked 2026-10-13. Do not add OpenAI-only fields.
		m["total_characters"] = 0
		delete(prompt, "audio_tokens")
		delete(m, "completion_tokens_details")
	case FlavorDeepSeek:
		m["prompt_cache_hit_tokens"] = u.CachedRead
		m["prompt_cache_miss_tokens"] = u.Input - u.CachedRead
	case FlavorQwen:
		if w := u.CacheWrite + u.CacheWr1h; w > 0 {
			prompt["cache_creation_input_tokens"] = w
			prompt["cache_type"] = "ephemeral"
		}
	case FlavorOpenRouter:
		prompt["cache_write_tokens"] = u.CacheWrite + u.CacheWr1h
		m["cost"] = 0.0001
		m["is_byok"] = false
		m["cost_details"] = map[string]any{"upstream_inference_cost": nil}
	case FlavorKimi:
		m["cached_tokens"] = u.CachedRead
		prompt["cache_write_tokens"] = u.CacheWrite + u.CacheWr1h
	case FlavorGemini:
		// Gemini OpenAI compatibility (https://ai.google.dev/gemini-api/docs/openai)
		// reports reasoning and text as disjoint output parts. Shape follows a
		// redacted real response observed 2026-10-20 (contract assumption, not an
		// official example).
		m["prompt_tokens_details"] = map[string]any{"cached_tokens": u.CachedRead,
			"cache_write_tokens": u.CacheWrite + u.CacheWr1h, "text_tokens": u.Input}
		m["completion_tokens_details"] = map[string]any{"reasoning_tokens": u.Reasoning,
			"text_tokens": max(0, u.Output-u.Reasoning)}
	case FlavorGLM:
		if stream {
			// GLM documents cache details for non-stream replies only.
			delete(m, "prompt_tokens_details")
		}
	case FlavorAzure, FlavorOpenAI, "":
		if w := u.CacheWrite + u.CacheWr1h; w > 0 {
			prompt["cache_write_tokens"] = w
		}
	}
	return m
}

// responsesUsage renders Responses API usage.
func responsesUsage(u Usage, flavor string) map[string]any {
	in := map[string]any{"cached_tokens": u.CachedRead}
	m := map[string]any{
		"input_tokens":          u.Input,
		"output_tokens":         u.Output,
		"total_tokens":          u.Total(),
		"input_tokens_details":  in,
		"output_tokens_details": map[string]any{"reasoning_tokens": u.Reasoning},
	}
	w := u.CacheWrite + u.CacheWr1h
	switch {
	case flavor == FlavorQwen:
		detail := map[string]any{"cached_tokens": u.CachedRead}
		if w > 0 {
			detail["cache_creation_input_tokens"] = w
		}
		m["x_details"] = []any{map[string]any{"x_billing_type": "response_api", "input_tokens": u.Input,
			"output_tokens": u.Output, "total_tokens": u.Total(), "input_tokens_details": detail,
			"output_tokens_details": map[string]any{"reasoning_tokens": u.Reasoning}}}
	case w > 0:
		in["cache_write_tokens"] = w
	}
	return m
}

// anthropicUsage renders Messages usage; input_tokens excludes cache traffic.
// thinking_tokens and zero web tool counters follow anthropic-sdk-go v1.80.0
// message.go Usage/OutputTokensDetails/ServerToolUsage, checked 2026-10-14.
// This is wire-format evidence, not acceptance against a real Claude API.
func anthropicUsage(u Usage) map[string]any {
	return map[string]any{
		"input_tokens":                u.uncachedInput(),
		"output_tokens":               u.Output,
		"cache_read_input_tokens":     u.CachedRead,
		"cache_creation_input_tokens": u.CacheWrite + u.CacheWr1h,
		"cache_creation": map[string]any{
			"ephemeral_5m_input_tokens": u.CacheWrite,
			"ephemeral_1h_input_tokens": u.CacheWr1h,
		},
		"service_tier":          "standard",
		"output_tokens_details": map[string]any{"thinking_tokens": u.Reasoning},
		"server_tool_use":       map[string]any{"web_fetch_requests": 0, "web_search_requests": 0},
	}
}

// geminiUsage renders usageMetadata; candidates exclude thoughts.
func geminiUsage(u Usage) map[string]any {
	return map[string]any{
		"promptTokenCount":        u.Input,
		"candidatesTokenCount":    u.Output - u.Reasoning,
		"thoughtsTokenCount":      u.Reasoning,
		"cachedContentTokenCount": u.CachedRead,
		"totalTokenCount":         u.Total(),
		"promptTokensDetails":     []any{map[string]any{"modality": "TEXT", "tokenCount": u.Input}},
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
