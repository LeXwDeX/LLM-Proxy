package fakellm

import (
	"strconv"
	"strings"
	"time"
)

// streamUsageWanted reports whether a Chat stream carries usage. OpenAI,
// Azure, Qwen, Kimi and MiniMax only send it with stream_options.include_usage; DeepSeek,
// GLM and OpenRouter always do. The legacy default (no flavor) always
// sends it, like the legacy mock provider.
func (c *call) streamUsageWanted() bool {
	if c.sc.NoUsage {
		return false
	}
	switch c.flavor {
	case FlavorOpenAI, FlavorAzure, FlavorQwen, FlavorKimi, FlavorMinimax:
		opts, _ := c.req["stream_options"].(map[string]any)
		include, _ := opts["include_usage"].(bool)
		return include
	}
	return true
}

// reasoningField is the delta key carrying visible reasoning text.
func reasoningField(flavor string) string {
	switch flavor {
	case FlavorOpenRouter:
		return "reasoning"
	case FlavorOpenAI, FlavorAzure:
		return "" // OpenAI chat does not expose reasoning text
	}
	return "reasoning_content"
}

func (s *Server) serveChat(c *call) {
	u := c.usage()
	if c.sc.Reasoning && (c.sc.Usage == nil || c.sc.Usage.Reasoning == nil) {
		u.Reasoning = u.Output / 2
	}
	text := replyText(s.opts.Name, c.raw, c.sc.Text)
	id := s.newID("chatcmpl")
	model := firstNonEmpty(c.model, s.opts.Model)
	if s.opts.LegacyMock {
		// Python Chat always echoes MOCK_MODEL; stats retain the requested model.
		model = s.opts.Model
	}
	created := time.Now().Unix()
	if !c.stream {
		msg := map[string]any{"role": "assistant", "content": text, "refusal": nil}
		if f := reasoningField(c.flavor); c.sc.Reasoning && f != "" {
			msg[f] = "fake reasoning"
		}
		resp := map[string]any{"id": id, "object": "chat.completion", "created": created, "model": model,
			"choices":            []any{map[string]any{"index": 0, "message": msg, "finish_reason": firstNonEmpty(c.sc.Finish, "stop"), "logprobs": nil}},
			"system_fingerprint": "fp_fake"}
		if tier, _ := c.req["service_tier"].(string); tier != "" && c.flavor != FlavorMinimax {
			resp["service_tier"] = tier
		}
		pricingPreset(c.sc.PricingPreset, resp, false)
		if c.flavor == FlavorOpenRouter {
			resp["provider"] = "Fake"
		}
		if c.flavor == FlavorMinimax {
			// ChatCompletionResp, checked 2026-10-13 against text-chat-openai.
			resp["base_resp"] = map[string]any{"status_code": 0, "status_msg": ""}
			resp["input_sensitive"], resp["output_sensitive"] = false, false
			delete(resp, "system_fingerprint")
			delete(msg, "refusal")
			delete(resp["choices"].([]any)[0].(map[string]any), "logprobs")
		}
		if !c.sc.NoUsage {
			resp["usage"] = openAIChatUsage(u, c.flavor, c.stream)
		}
		c.rec.Usage = &u
		c.ok(200, resp)
		return
	}

	st := c.startSSE()
	chunk := func(delta map[string]any, finish any) map[string]any {
		m := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"system_fingerprint": "fp_fake",
			"choices":            []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish, "logprobs": nil}}}
		if c.flavor == FlavorOpenRouter {
			m["provider"] = "Fake"
		}
		if c.flavor == FlavorMinimax {
			// ChatCompletionChunk has no base_resp; usage is null until final.
			m["usage"] = nil
			delete(m, "system_fingerprint")
			delete(m["choices"].([]any)[0].(map[string]any), "logprobs")
		}
		pricingPreset(c.sc.PricingPreset, m, false)
		return m
	}
	st.event("", chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	if f := reasoningField(c.flavor); c.sc.Reasoning && f != "" {
		for _, piece := range splitText("fake reasoning", 2) {
			st.event("", chunk(map[string]any{f: piece}, nil))
		}
	}
	for i, piece := range splitText(text, c.sc.Chunks) {
		if c.sc.StreamError > 0 && i >= c.sc.StreamError {
			e := streamFailure(c.sc, c.flavor, 500)
			if c.flavor == FlavorOpenRouter {
				m := chunk(map[string]any{"content": ""}, "error")
				m["error"] = map[string]any{"code": e.status, "message": e.message}
				st.event("", m)
			} else {
				st.event("", map[string]any{"error": map[string]any{"message": e.message, "type": e.errType, "code": e.code}})
			}
			st.done("stream_error", nil)
			return
		}
		st.event("", chunk(map[string]any{"content": piece}, nil))
	}
	wantUsage := c.streamUsageWanted()
	final := chunk(map[string]any{}, firstNonEmpty(c.sc.Finish, "stop"))
	if c.flavor == FlavorKimi && !c.sc.NoUsage {
		// Moonshot has also been observed to attach usage to the finishing
		// choice regardless of include_usage; emit both shapes (superset).
		final["choices"].([]any)[0].(map[string]any)["usage"] = openAIChatUsage(u, c.flavor, true)
	}
	switch {
	case !wantUsage:
		st.event("", final)
	case c.flavor == FlavorOpenAI || c.flavor == FlavorAzure || c.flavor == FlavorQwen || c.flavor == FlavorKimi || c.flavor == FlavorMinimax:
		// Official include_usage shape: a trailing chunk with no choices.
		// MiniMax documents the opt-in but not its position (2026-10-13).
		// For MiniMax, placement is inferred from the original OpenAI shape.
		st.event("", final)
		m := chunk(nil, nil)
		m["choices"] = []any{}
		m["usage"] = openAIChatUsage(u, c.flavor, true)
		st.event("", m)
	case c.flavor == FlavorOpenRouter:
		// OpenRouter repeats finish_reason on an extra usage chunk.
		st.event("", final)
		m := chunk(map[string]any{}, firstNonEmpty(c.sc.Finish, "stop"))
		m["usage"] = openAIChatUsage(u, c.flavor, true)
		st.event("", m)
	default:
		// DeepSeek / GLM / legacy: usage rides on the finish chunk.
		final["usage"] = openAIChatUsage(u, c.flavor, true)
		st.event("", final)
	}
	st.rawEvent("", "[DONE]")
	if wantUsage {
		st.done("ok", &u)
	} else {
		st.done("ok", nil)
	}
}

func firstInt(v ...int) int {
	for _, n := range v {
		if n != 0 {
			return n
		}
	}
	return 0
}

func (s *Server) serveCompletions(c *call) {
	u := c.usage()
	text := replyText(s.opts.Name, c.raw, c.sc.Text)
	id := s.newID("cmpl")
	model := firstNonEmpty(c.model, s.opts.Model)
	if !c.stream {
		resp := map[string]any{"id": id, "object": "text_completion", "created": time.Now().Unix(), "model": model,
			"choices": []any{map[string]any{"text": text, "index": 0, "logprobs": nil, "finish_reason": "stop"}}}
		if !c.sc.NoUsage {
			resp["usage"] = openAIChatUsage(u, c.flavor, c.stream)
		}
		c.rec.Usage = &u
		c.ok(200, resp)
		return
	}
	st := c.startSSE()
	for _, piece := range splitText(text, c.sc.Chunks) {
		st.event("", map[string]any{"id": id, "object": "text_completion", "model": model,
			"choices": []any{map[string]any{"text": piece, "index": 0, "finish_reason": nil}}})
	}
	final := map[string]any{"id": id, "object": "text_completion", "model": model,
		"choices": []any{map[string]any{"text": "", "index": 0, "finish_reason": "stop"}}}
	var billed *Usage
	if c.streamUsageWanted() {
		final["usage"] = openAIChatUsage(u, c.flavor, c.stream)
		billed = &u
	}
	st.event("", final)
	st.rawEvent("", "[DONE]")
	st.done("ok", billed)
}

func fakeVector(seed []byte, n int) []float64 {
	v := make([]float64, n)
	for i := range v {
		b := byte(i)
		if len(seed) > 0 {
			b = seed[i%len(seed)]
		}
		v[i] = float64(int(b)-128) / 128
	}
	return v
}

func (s *Server) serveEmbeddings(c *call) {
	u := computeUsage(promptText(c.req), 0, c.sc.Usage)
	n := 1
	if arr, ok := c.req["input"].([]any); ok && len(arr) > 0 {
		n = len(arr)
	}
	data := make([]any, n)
	for i := range data {
		data[i] = map[string]any{"object": "embedding", "index": i, "embedding": fakeVector(c.raw, 8)}
	}
	resp := map[string]any{"object": "list", "data": data, "model": firstNonEmpty(c.model, s.opts.Model)}
	if !c.sc.NoUsage {
		resp["usage"] = map[string]any{"prompt_tokens": u.Input, "total_tokens": u.Input}
	}
	u.Output = 0
	c.rec.Usage = &u
	c.ok(200, resp)
}

// onePixelPNG is a valid 1x1 PNG, base64 encoded.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

// serveImages mimics the official Images API. gpt-image-* models report
// token usage with input_tokens/output_tokens (Responses-style names) and an
// input text/image split; dall-e-* models report no usage at all.
func (s *Server) serveImages(c *call) {
	model := firstNonEmpty(c.model, "gpt-image-1")
	n := toInt(c.req["n"])
	if s, ok := c.req["n"].(string); ok && s != "" {
		n, _ = strconv.Atoi(s)
	}
	if n <= 0 {
		n = 1
	}
	if c.sc.Usage != nil && c.sc.Usage.Images != nil {
		n = *c.sc.Usage.Images
	}
	u := computeUsage(promptText(c.req), 0, c.sc.Usage)
	if c.sc.Usage == nil || c.sc.Usage.Output == nil {
		u.Output = 272 * n // gpt-image low-quality 1024x1024 output tokens
	}
	imageIn := 0
	if files := toInt(c.req["_files"]); files > 0 {
		imageIn = 65 * files
		u.Input += imageIn
	}
	u.Images = n
	isGPTImage := strings.HasPrefix(strings.ToLower(model), "gpt-image") || strings.Contains(strings.ToLower(model), "image-1")
	usage := map[string]any{"input_tokens": u.Input, "output_tokens": u.Output, "total_tokens": u.Total(),
		"input_tokens_details": map[string]any{"text_tokens": u.Input - imageIn, "image_tokens": imageIn}}
	// Default to the official Images shape without an output split. This
	// fake-only request flag also exercises compatible upstreams with a split.
	if details, _ := c.req["_output_tokens_details"].(bool); details {
		usage["output_tokens_details"] = map[string]any{"image_tokens": u.Output}
	}
	edit := strings.HasSuffix(strings.ToLower(c.r.URL.Path), "/edits")

	if c.stream && isGPTImage {
		prefix := "image_generation"
		if edit {
			prefix = "image_edit"
		}
		st := c.startSSE()
		st.event(prefix+".partial_image", map[string]any{"type": prefix + ".partial_image", "b64_json": onePixelPNG,
			"partial_image_index": 0, "created_at": time.Now().Unix(), "size": "1024x1024", "quality": "low", "background": "opaque", "output_format": "png"})
		done := map[string]any{"type": prefix + ".completed", "b64_json": onePixelPNG, "created_at": time.Now().Unix(),
			"size": "1024x1024", "quality": "low", "background": "opaque", "output_format": "png"}
		if !c.sc.NoUsage {
			done["usage"] = usage
		}
		st.event(prefix+".completed", done)
		st.done("ok", &u)
		return
	}

	data := make([]any, n)
	for i := range data {
		if format, _ := c.req["response_format"].(string); format == "url" {
			data[i] = map[string]any{"url": "https://fake.invalid/image.png", "revised_prompt": "fake"}
		} else {
			data[i] = map[string]any{"b64_json": onePixelPNG}
		}
	}
	resp := map[string]any{"created": time.Now().Unix(), "data": data}
	if isGPTImage {
		resp["background"], resp["output_format"], resp["quality"], resp["size"] = "opaque", "png", "low", "1024x1024"
		if !c.sc.NoUsage {
			resp["usage"] = usage
		}
		c.rec.Usage = &u
	}
	c.ok(200, resp)
}
