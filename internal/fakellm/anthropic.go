package fakellm

import "strings"

// anthropicTailSuffixes are synthetic counterexamples appended after usage;
// they are not official Claude sequences.
var anthropicTailSuffixes = []string{"_tail_nonfinal", "_tail_empty_stop", "_tail_error_empty_stop", "_tail_invalid_final_retry", "_tail_invalid_iteration_retry", "_tail_omitted_iterations_retry", "_tail_message_mismatch_retry", "_tail_error_retry_final", "_tail_retry_final", "_tail_cost_incomplete_retry"}

func anthropicTail(mode string) (string, string) {
	for _, suffix := range anthropicTailSuffixes {
		if strings.HasSuffix(mode, suffix) {
			return strings.TrimSuffix(mode, suffix), strings.TrimPrefix(suffix, "_tail_")
		}
	}
	return mode, ""
}

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
	mode, tail := anthropicTail(c.sc.Iterations)
	replaceEmpty := mode == "replace_empty_iterations"
	if replaceEmpty {
		mode = "advisor"
	}
	lateError := strings.HasSuffix(mode, "_then_error")
	intermediateError, stopConfirmedError := mode == "advisor_intermediate_then_error", mode == "advisor_stop_then_error"
	mode = strings.TrimSuffix(mode, "_then_error")
	if intermediateError || stopConfirmedError {
		mode = "advisor"
	}
	zeroRefusal := strings.Contains(mode, "refusal_") && mode != "refusal_nonzero" && mode != "refusal_nonrefusal"
	fallback := strings.HasPrefix(mode, "fallback_")
	midOutput := mode == "fallback_output" || mode == "fallback_missing_input" || mode == "fallback_chain" || mode == "fallback_chain_unknown"
	requestModel, echoPrefix := model, c.sc.IterationModelPrefix
	boundary := map[string]any{"type": "fallback", "from": map[string]any{"model": echoedModel(echoPrefix, model)}, "to": map[string]any{"model": echoedModel(echoPrefix, "fake-fallback")}}
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
			anthropicIterations(mode, resp["usage"].(map[string]any))
			echoIterationModels(resp["usage"].(map[string]any), echoPrefix, requestModel)
		}
		anthropicIterationStop(mode, resp)
		if zeroRefusal {
			resp["content"] = []any{}
		}
		if fallback {
			resp["model"] = "fake-fallback"
			if !zeroRefusal && mode != "fallback_sticky" {
				resp["content"] = append([]any{boundary}, content...)
			}
		}
		resp["model"] = echoedModel(echoPrefix, resp["model"].(string))
		c.rec.Usage = &u
		c.ok(200, resp)
		return
	}

	st := c.startSSE()
	startUsage := anthropicUsage(u)
	pricingPreset(c.sc.PricingPreset, startUsage, true)
	if mode == "cache_missing" {
		delete(startUsage, "cache_read_input_tokens")
	}
	startUsage["output_tokens"] = 1
	startUsage["output_tokens_details"] = map[string]any{"thinking_tokens": 0}
	if zeroRefusal {
		startUsage["output_tokens"] = 0
	}
	if mode == "refusal_missing_output" {
		delete(startUsage, "output_tokens")
	}
	// Official streaming contract, checked 2026-10-21: before-output/sticky
	// fallback names the serving model at message_start; mid-output does not.
	// https://platform.claude.com/docs/en/build-with-claude/refusals-and-fallback
	if fallback && !midOutput {
		model = "fake-fallback"
	}
	msg := map[string]any{"id": id, "type": "message", "role": "assistant", "model": echoedModel(echoPrefix, model), "content": []any{},
		"stop_reason": nil, "stop_sequence": nil}
	if !c.sc.NoUsage {
		msg["usage"] = startUsage
	}
	st.event("message_start", map[string]any{"type": "message_start", "message": msg})
	idx := 0
	if fallback && !zeroRefusal && mode != "fallback_sticky" {
		if midOutput {
			st.event("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "text", "text": ""}})
			st.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "text_delta", "text": "partial"}})
			st.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
			idx++
		}
		st.event("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": boundary})
		st.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		idx++
	}
	if !zeroRefusal {
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
	}
	delta := map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": firstNonEmpty(c.sc.Finish, "end_turn"), "stop_sequence": nil}}
	if !c.sc.NoUsage {
		// Default fixture repeats input/cache; the gap mode intentionally omits them.
		delta["usage"] = anthropicUsage(u)
		if tail == "invalid_final_retry" {
			// Synthetic counterexample: a valid final with no cache evidence.
			delta["usage"] = map[string]any{"input_tokens": 7, "output_tokens": 2, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
		} else {
			pricingPreset(c.sc.PricingPreset, delta["usage"].(map[string]any), true)
			anthropicIterations(mode, delta["usage"].(map[string]any))
			echoIterationModels(delta["usage"].(map[string]any), echoPrefix, requestModel)
			finalUsage := delta["usage"].(map[string]any)
			if mode == "advisor_early_only" {
				st.event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{}, "usage": finalUsage})
				delete(finalUsage, "iterations")
			}
			if mode == "fallback_missing_input" {
				for _, key := range []string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "cache_creation", "output_tokens_details", "service_tier"} {
					delete(finalUsage, key)
				}
			}
		}
	}
	anthropicIterationStop(mode, delta["delta"].(map[string]any))
	if intermediateError || stopConfirmedError {
		delta["delta"] = map[string]any{}
	}
	st.event("message_delta", delta)
	// Synthetic settlement counterexamples: an error can follow intermediate
	// usage, a terminal delta, or a delta confirmed by message_stop.
	if lateError {
		if stopConfirmedError {
			st.event("message_stop", map[string]any{"type": "message_stop"})
		}
		st.event("error", map[string]any{"type": "error", "error": map[string]any{"type": "overloaded_error", "message": "synthetic error after usage"}})
		if intermediateError {
			st.event("message_stop", map[string]any{"type": "message_stop"})
		}
		st.done("stream_error", &u)
		return
	}
	finalUsage, _ := delta["usage"].(map[string]any)
	if replaceEmpty {
		second := map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": firstNonEmpty(c.sc.Finish, "end_turn")}}
		replacement := make(map[string]any, len(finalUsage))
		for key, value := range finalUsage {
			replacement[key] = value
		}
		replacement["iterations"] = []any{}
		second["usage"] = replacement
		st.event("message_delta", second)
	}
	switch tail {
	case "invalid_final_retry":
		// Synthetic counterexamples, not official Claude sequences: an invalid
		// final must not leave cache/model evidence for the next valid final.
		st.event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"},
			"usage": map[string]any{"input_tokens": 11, "output_tokens": nil, "cache_read_input_tokens": 1000}})
		st.event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"},
			"usage": map[string]any{"input_tokens": 7, "output_tokens": 4, "iterations": []any{}}})
		st.event("message_stop", map[string]any{"type": "message_stop"})
		billed := Usage{Input: 7, Output: 4}
		c.rec.Usage = &billed
		st.done("ok", &billed)
		return
	case "invalid_iteration_retry", "omitted_iterations_retry", "message_mismatch_retry":
		// Synthetic: unverifiable/omitted iterations or a message-only mismatch
		// after an earlier valid final. Not an official Claude sequence.
		retryUsage := map[string]any{"input_tokens": 70, "output_tokens": 20, "cache_read_input_tokens": 30, "cache_creation_input_tokens": 0}
		switch tail {
		case "invalid_iteration_retry":
			retryUsage["iterations"] = []any{
				map[string]any{"type": "message", "input_tokens": 70, "output_tokens": 20, "cache_read_input_tokens": 30, "cache_creation_input_tokens": 0},
				map[string]any{"type": "advisor_message", "model": "fake-advisor", "input_tokens": -1, "output_tokens": 3, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0},
			}
		case "message_mismatch_retry":
			retryUsage["iterations"] = []any{
				map[string]any{"type": "message", "input_tokens": 71, "output_tokens": 20, "cache_read_input_tokens": 30, "cache_creation_input_tokens": 0},
			}
		}
		retry := map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": firstNonEmpty(c.sc.Finish, "end_turn")}, "usage": retryUsage}
		st.event("message_delta", retry)
		st.event("message_stop", map[string]any{"type": "message_stop"})
		st.done("ok", &u)
		return
	case "error_retry_final", "retry_final", "cost_incomplete_retry":
		// Synthetic: an error or a 50/9 replacement final after the committed
		// snapshot; cost_incomplete_retry adds one server tool request.
		if tail == "error_retry_final" {
			st.event("error", map[string]any{"type": "error", "error": map[string]any{"type": "overloaded_error", "message": "synthetic error after usage"}})
		}
		replacement := map[string]any{"input_tokens": 50, "output_tokens": 9, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0, "iterations": []any{}}
		if tail == "cost_incomplete_retry" {
			replacement["server_tool_use"] = map[string]any{"web_search_requests": 1}
		}
		st.event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": firstNonEmpty(c.sc.Finish, "end_turn")}, "usage": replacement})
		st.event("message_stop", map[string]any{"type": "message_stop"})
		billed := Usage{Input: 50, Output: 9}
		c.rec.Usage = &billed
		status := "ok"
		if tail == "error_retry_final" {
			status = "stream_error"
		}
		st.done(status, &billed)
		return
	case "nonfinal":
		// No message_stop: the stream ends after a delta that only repeats output.
		out := 0
		if n, ok := finalUsage["output_tokens"].(int); ok {
			out = n
		}
		st.event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{}, "usage": map[string]any{"output_tokens": out}})
		st.done("ok", &u)
		return
	case "empty_stop":
		st.event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{}, "usage": map[string]any{}})
	case "error_empty_stop":
		st.event("error", map[string]any{"type": "error", "error": map[string]any{"type": "overloaded_error", "message": "synthetic error after usage"}})
		st.event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{}, "usage": map[string]any{}})
		st.event("message_stop", map[string]any{"type": "message_stop"})
		st.done("stream_error", &u)
		return
	}
	st.event("message_stop", map[string]any{"type": "message_stop"})
	if c.sc.NoUsage {
		st.done("ok", nil)
		return
	}
	st.done("ok", &u)
}
