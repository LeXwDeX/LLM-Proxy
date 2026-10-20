package fakellm

// serveMessages mimics the Anthropic Messages API (also served by GLM,
// DeepSeek and Qwen under their Anthropic-compatible prefixes).
func (s *Server) serveMessages(c *call) {
	u := c.usage()
	if c.sc.Reasoning && (c.sc.Usage == nil || c.sc.Usage.Reasoning == nil) {
		u.Reasoning = u.Output / 2
	}
	text := replyText(s.opts.Name, c.raw, c.sc.Text)
	id := s.newID("msg")
	model := firstNonEmpty(c.model, s.opts.Model)
	_, thinkingOn := c.req["thinking"].(map[string]any)
	thinking := c.sc.Reasoning || thinkingOn

	if !c.stream {
		content := []any{}
		if thinking {
			content = append(content, map[string]any{"type": "thinking", "thinking": "fake reasoning", "signature": "fake-signature"})
		}
		content = append(content, map[string]any{"type": "text", "text": text})
		resp := map[string]any{"id": id, "type": "message", "role": "assistant", "model": model,
			"content": content, "stop_reason": firstNonEmpty(c.sc.Finish, "end_turn"), "stop_sequence": nil}
		if !c.sc.NoUsage {
			resp["usage"] = anthropicUsage(u)
			pricingPreset(c.sc.PricingPreset, resp["usage"].(map[string]any), true)
			anthropicIterations(c.sc.Iterations, resp["usage"].(map[string]any))
		}
		c.rec.Usage = &u
		c.ok(200, resp)
		return
	}

	st := c.startSSE()
	startUsage := anthropicUsage(u)
	pricingPreset(c.sc.PricingPreset, startUsage, true)
	startUsage["output_tokens"] = 1
	startUsage["output_tokens_details"] = map[string]any{"thinking_tokens": 0}
	msg := map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil}
	if !c.sc.NoUsage {
		msg["usage"] = startUsage
	}
	st.event("message_start", map[string]any{"type": "message_start", "message": msg})
	idx := 0
	if thinking {
		st.event("content_block_start", map[string]any{"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}})
		st.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "thinking_delta", "thinking": "fake reasoning"}})
		st.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "signature_delta", "signature": "fake-signature"}})
		st.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		idx++
	}
	st.event("content_block_start", map[string]any{"type": "content_block_start", "index": idx,
		"content_block": map[string]any{"type": "text", "text": ""}})
	st.event("ping", map[string]any{"type": "ping"})
	for i, piece := range splitText(text, c.sc.Chunks) {
		if c.sc.StreamError > 0 && i >= c.sc.StreamError {
			e := streamFailure(c.sc, c.flavor, 529)
			inner := map[string]any{"type": e.errType, "message": e.message}
			if e.code != "" && c.flavor == FlavorGLM {
				inner["code"] = e.code
			}
			st.event("error", map[string]any{"type": "error", "error": inner})
			st.done("stream_error", nil)
			return
		}
		st.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "text_delta", "text": piece}})
	}
	st.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
	delta := map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": firstNonEmpty(c.sc.Finish, "end_turn"), "stop_sequence": nil}}
	if !c.sc.NoUsage {
		// Current API repeats the cumulative input/cache counts here.
		delta["usage"] = anthropicUsage(u)
		pricingPreset(c.sc.PricingPreset, delta["usage"].(map[string]any), true)
		anthropicIterations(c.sc.Iterations, delta["usage"].(map[string]any))
	}
	st.event("message_delta", delta)
	st.event("message_stop", map[string]any{"type": "message_stop"})
	if c.sc.NoUsage {
		st.done("ok", nil)
		return
	}
	st.done("ok", &u)
}
