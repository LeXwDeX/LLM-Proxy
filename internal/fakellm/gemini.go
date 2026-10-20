package fakellm

import "strings"

// serveGemini mimics generateContent, streamGenerateContent (SSE with
// alt=sse, or a JSON array without it), countTokens, embedContent and
// batchEmbedContents, including image-output models (inlineData parts).
func (s *Server) serveGemini(c *call, method string) {
	switch strings.ToLower(method) {
	case "counttokens":
		c.ok(200, map[string]any{"totalTokens": EstimateTokens(promptText(c.req)),
			"promptTokensDetails": []any{map[string]any{"modality": "TEXT", "tokenCount": EstimateTokens(promptText(c.req))}}})
		return
	case "embedcontent":
		c.ok(200, map[string]any{"embedding": map[string]any{"values": fakeVector(c.raw, 8)}})
		return
	case "batchembedcontents":
		n := 1
		if reqs, ok := c.req["requests"].([]any); ok && len(reqs) > 0 {
			n = len(reqs)
		}
		embs := make([]any, n)
		for i := range embs {
			embs[i] = map[string]any{"values": fakeVector(c.raw, 8)}
		}
		c.ok(200, map[string]any{"embeddings": embs})
		return
	case "generatecontent", "streamgeneratecontent":
	default:
		c.fail(apiError{status: 404, message: "fake: unsupported gemini method " + method}, wireGemini)
		return
	}

	gen, _ := c.req["generationConfig"].(map[string]any)
	maxOut := toInt(gen["maxOutputTokens"])
	u := computeUsage(promptText(c.req), maxOut, c.sc.Usage)
	thinkingCfg, _ := gen["thinkingConfig"].(map[string]any)
	if (c.sc.Reasoning || thinkingCfg != nil) && (c.sc.Usage == nil || c.sc.Usage.Reasoning == nil) {
		u.Reasoning = u.Output / 2
	}
	text := replyText(s.opts.Name, c.raw, c.sc.Text)
	model := firstNonEmpty(c.model, s.opts.Model)
	imageOut := strings.Contains(strings.ToLower(model), "image")
	if mods, ok := gen["responseModalities"].([]any); ok {
		for _, m := range mods {
			if strings.EqualFold(toString(m), "IMAGE") {
				imageOut = true
			}
		}
	}

	usage := func() map[string]any {
		m := geminiUsage(u)
		if imageOut {
			m["candidatesTokensDetails"] = []any{map[string]any{"modality": "IMAGE", "tokenCount": u.Output - u.Reasoning}}
		}
		return m
	}
	candidate := func(parts []any, finish string) map[string]any {
		cand := map[string]any{"content": map[string]any{"role": "model", "parts": parts}, "index": 0}
		if finish != "" {
			cand["finishReason"] = finish
		}
		return cand
	}
	responseID := s.newID("gem")
	chunk := func(parts []any, finish string, withUsage bool) map[string]any {
		m := map[string]any{"candidates": []any{candidate(parts, finish)}, "modelVersion": model, "responseId": responseID}
		if withUsage && !c.sc.NoUsage {
			m["usageMetadata"] = usage()
		}
		return m
	}

	if !c.stream {
		parts := []any{}
		if u.Reasoning > 0 {
			parts = append(parts, map[string]any{"text": "fake reasoning", "thought": true})
		}
		parts = append(parts, map[string]any{"text": text})
		if imageOut {
			parts = append(parts, map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": onePixelPNG}})
		}
		c.rec.Usage = &u
		c.ok(200, chunk(parts, firstNonEmpty(c.sc.Finish, "STOP"), true))
		return
	}

	pieces := splitText(text, c.sc.Chunks)
	sse := strings.EqualFold(c.r.URL.Query().Get("alt"), "sse")
	if !sse {
		// Without alt=sse Gemini streams one JSON array.
		arr := []any{}
		for i, piece := range pieces {
			arr = append(arr, chunk([]any{map[string]any{"text": piece}}, map[bool]string{true: "STOP"}[i == len(pieces)-1], i == len(pieces)-1))
		}
		c.rec.Usage = &u
		c.ok(200, arr)
		return
	}
	st := c.startSSE()
	if u.Reasoning > 0 {
		st.event("", chunk([]any{map[string]any{"text": "fake reasoning", "thought": true}}, "", false))
	}
	for i, piece := range pieces {
		if c.sc.StreamError > 0 && i >= c.sc.StreamError {
			e := streamFailure(c.sc, c.flavor, 500)
			st.event("", map[string]any{"error": map[string]any{"code": e.status, "message": e.message, "status": geminiStatus(e.status)}})
			st.done("stream_error", nil)
			return
		}
		last := i == len(pieces)-1
		parts := []any{map[string]any{"text": piece}}
		if last && imageOut {
			parts = append(parts, map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": onePixelPNG}})
		}
		finish := ""
		if last {
			finish = firstNonEmpty(c.sc.Finish, "STOP")
		}
		// Gemini repeats cumulative usageMetadata on every chunk.
		st.event("", chunk(parts, finish, true))
	}
	if c.sc.NoUsage {
		st.done("ok", nil)
		return
	}
	st.done("ok", &u)
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}
