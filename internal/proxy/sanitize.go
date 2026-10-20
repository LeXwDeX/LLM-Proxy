// sanitize.go — 请求净化（Azure 字段白名单）与模型名提取/验证。
package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

var (
	azureChatCompletionsFieldWhitelist = toFieldSet(
		"audio",
		"data_sources",
		"frequency_penalty",
		"function_call",
		"functions",
		"logit_bias",
		"logprobs",
		"max_completion_tokens",
		"max_tokens",
		"messages",
		"metadata",
		"modalities",
		"model",
		"n",
		"parallel_tool_calls",
		"prediction",
		"presence_penalty",
		"prompt_cache_key",
		"reasoning_effort",
		"response_format",
		"seed",
		"stop",
		"store",
		"stream",
		"stream_options",
		"temperature",
		"tool_choice",
		"tools",
		"top_logprobs",
		"top_p",
		"user",
		"user_security_context",
	)
	azureResponsesFieldWhitelist = toFieldSet(
		"background",
		"include",
		"input",
		"instructions",
		"max_output_tokens",
		"max_tool_calls",
		"metadata",
		"model",
		"parallel_tool_calls",
		"previous_response_id",
		"prompt",
		"prompt_cache_key",
		"reasoning",
		"store",
		"stream",
		"temperature",
		"text",
		"tool_choice",
		"tools",
		"top_logprobs",
		"top_p",
		"truncation",
		"user",
	)
	azureEmbeddingsFieldWhitelist = toFieldSet(
		"dimensions",
		"encoding_format",
		"input",
		"model",
		"user",
	)
)

func toFieldSet(fields ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		field = strings.ToLower(strings.TrimSpace(field))
		if field == "" {
			continue
		}
		set[field] = struct{}{}
	}
	return set
}

func whitelistForPath(path string) (map[string]struct{}, bool) {
	path = strings.ToLower(strings.TrimSpace(path))
	if path == "" {
		return nil, false
	}

	if strings.HasSuffix(path, "/chat/completions") {
		return azureChatCompletionsFieldWhitelist, true
	}
	if strings.HasSuffix(path, "/responses") {
		return azureResponsesFieldWhitelist, true
	}
	if strings.HasSuffix(path, "/embeddings") {
		return azureEmbeddingsFieldWhitelist, true
	}

	return nil, false
}

func sanitizeRequestBodyForAzure(r *http.Request, body []byte) ([]byte, []string) {
	if r == nil || len(body) == 0 {
		return body, nil
	}

	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return body, nil
	}

	fieldWhitelist, ok := whitelistForPath(r.URL.Path)
	if !ok {
		return body, nil
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}

	stripped := make([]string, 0)
	for key := range payload {
		keyLower := strings.ToLower(strings.TrimSpace(key))
		if _, allowed := fieldWhitelist[keyLower]; allowed {
			continue
		}
		// GPT caching controls affect upstream charges. Keep the client's policy
		// on chat/responses requests while retaining the whitelist for other models.
		if (keyLower == "prompt_cache_options" || keyLower == "prompt_cache_retention") &&
			(strings.HasSuffix(r.URL.Path, "/chat/completions") || strings.HasSuffix(r.URL.Path, "/responses")) &&
			strings.HasPrefix(strings.ToLower(extractModel(r, body)), "gpt") {
			continue
		}
		delete(payload, key)
		stripped = append(stripped, key)
	}

	if len(stripped) == 0 {
		return body, nil
	}

	filtered, err := json.Marshal(payload)
	if err != nil {
		return body, nil
	}

	sort.Strings(stripped)
	return filtered, stripped
}

// extractModel returns the request's original model spelling. Callers that need
// a lookup key must normalize it separately; forwarding uses the raw spelling
// to decide whether the provider's configured upstream string needs rewriting.
func extractModel(r *http.Request, body []byte) string {
	if r == nil {
		return ""
	}

	path := r.URL.Path
	pathLower := strings.ToLower(path)
	if isListEndpoint(pathLower) {
		return ""
	}

	const deploymentsSegment = "/deployments/"
	if idx := indexCaseInsensitive(path, deploymentsSegment); idx >= 0 {
		after := path[idx+len(deploymentsSegment):]
		if slash := strings.Index(after, "/"); slash >= 0 {
			after = after[:slash]
		}
		return strings.TrimSpace(after)
	}

	// Gemini native REST API: /v1beta/models/{model}:{action}
	// Also match /v1alpha/models/... and /v1/models/... variants.
	const modelsSegment = "/models/"
	if idx := indexCaseInsensitive(path, modelsSegment); idx >= 0 {
		after := path[idx+len(modelsSegment):]
		// Strip :{action} suffix (e.g. ":generatecontent", ":streamgeneratecontent")
		if colon := strings.Index(after, ":"); colon >= 0 {
			after = after[:colon]
		}
		// Strip any trailing path segments
		if slash := strings.Index(after, "/"); slash >= 0 {
			after = after[:slash]
		}
		if m := strings.TrimSpace(after); m != "" {
			return m
		}
	}

	if len(body) > 0 {
		// One classification for extraction, normalization and conversion:
		// a body is read as a form or as JSON, never both.
		kind, boundary, _ := classifyRequestBody(r.Header.Get("Content-Type"), body)
		switch kind {
		case bodyURLEncoded:
			// The last exact "model" pair, the one normalizeURLEncodedMembers
			// keeps; url.Values preserves pair order.
			if vals, err := url.ParseQuery(string(body)); err == nil {
				if models := vals["model"]; len(models) > 0 {
					return strings.TrimSpace(models[len(models)-1])
				}
			}
			return ""
		case bodyMultipart:
			if boundary == "" {
				return ""
			}
			return extractMultipartModel(body, boundary)
		}
		return extractModelFromJSON(body)
	}
	return ""
}

// extractModelFromJSON keeps the semantics of decoding the whole body into
// map[string]any without materializing every message: the body must be one
// valid JSON object, the "model" key matches exactly (not case-insensitively,
// so no struct), a duplicate key's last value wins, and a non-string model
// yields "". RawMessage members are not decoded, so the one remaining error
// of the map[string]any decode, a number outside float64 range anywhere in the
// body, is checked separately only when a string model was found.
func extractModelFromJSON(body []byte) string {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	raw := payload["model"]
	var model string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &model) != nil {
		return ""
	}
	if !jsonNumbersFitFloat64(body) {
		return ""
	}
	return strings.TrimSpace(model)
}

// jsonNumbersFitFloat64 reports whether every number in an already validated
// JSON document parses as float64, as encoding/json requires for interface{}
// values. Only numbers with an exponent or a very long mantissa can overflow.
func jsonNumbersFitFloat64(body []byte) bool {
	for i := 0; i < len(body); {
		switch c := body[i]; {
		case c == '"':
			i++
			for {
				quote := bytes.IndexByte(body[i:], '"')
				if quote < 0 {
					return true
				}
				end := i + quote
				backslashes := 0
				for j := end - 1; j >= i && body[j] == '\\'; j-- {
					backslashes++
				}
				i = end + 1
				if backslashes%2 == 0 {
					break
				}
			}
		case c == '-' || (c >= '0' && c <= '9'):
			start := i
			exponent := false
			for i < len(body) {
				d := body[i]
				if d == 'e' || d == 'E' {
					exponent = true
				} else if !(d >= '0' && d <= '9') && d != '-' && d != '+' && d != '.' {
					break
				}
				i++
			}
			if exponent || i-start > 300 {
				if _, err := strconv.ParseFloat(string(body[start:i]), 64); err != nil {
					return false
				}
			}
		default:
			i++
		}
	}
	return true
}

func indexCaseInsensitive(value, substring string) int {
	for index := 0; index+len(substring) <= len(value); index++ {
		if strings.EqualFold(value[index:index+len(substring)], substring) {
			return index
		}
	}
	return -1
}

// extractMultipartModel returns the last part named exactly "model", the same
// rule as JSON map decoding and convertMultipartToJSON (last field wins), and
// the part normalizeMultipartMembers keeps. Case variants are not models. A
// body that does not parse completely has no model.
func extractMultipartModel(body []byte, boundary string) string {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	model := ""
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return model
		}
		if err != nil {
			return ""
		}

		if part.FormName() == "model" {
			data, err := io.ReadAll(io.LimitReader(part, 1024))
			if err != nil {
				return ""
			}
			// Drain the rest, checking that the part ends cleanly.
			if _, err := io.Copy(io.Discard, part); err != nil {
				return ""
			}
			model = strings.TrimSpace(string(data))
			continue
		}

		if _, err := io.Copy(io.Discard, part); err != nil {
			return ""
		}
	}
}

// resolveUpstreamModel resolves a client-supplied model name (either an upstream
// name or a fallback alias) to the upstream name preserving its original case.
// Returns ok=true when the model is allowed (or when the target has no mapping
// restrictions, indicated by a nil index; an empty non-nil index means every
// configured mapping is paused and rejects all models).
func resolveUpstreamModel(t *Target, model string) (upstream string, ok bool) {
	if t == nil {
		return "", false
	}
	if t.allowedModelIdx == nil {
		return model, true
	}
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return "", false
	}
	up, ok := t.allowedModelIdx[key]
	return up, ok
}

// modelAllowed reports whether the given target accepts the model.
// An empty model is rejected when the target has model restrictions.
// A nil target always rejects.
func modelAllowed(t *Target, model string) bool {
	if t == nil {
		return false
	}
	_, ok := resolveUpstreamModel(t, model)
	return ok
}

func (s *Service) anyTargetRequiresModel() bool {
	snapshot := s.targetSnapshot()
	for _, state := range snapshot {
		if state == nil || state.Target() == nil {
			continue
		}
		if state.Target().allowedModelIdx != nil {
			return true
		}
	}
	return false
}

// ensureModelAllowed returns an error if target has model restrictions and model
// is either empty or not allowed. On success it returns the upstream model name
// (preserving original case) which callers must use when forwarding.
func (s *Service) ensureModelAllowed(target *Target, model string) (string, error) {
	if target == nil {
		return model, nil
	}
	if target.allowedModelIdx == nil {
		return model, nil
	}

	if strings.TrimSpace(model) == "" {
		return "", errors.New("model required when model_mappings is configured")
	}
	upstream, ok := resolveUpstreamModel(target, model)
	if !ok {
		return "", fmt.Errorf("model %q not allowed for target %q", model, target.Name)
	}
	return upstream, nil
}

// normalizeAllowed lowercases and deduplicates a list, skipping empties.
// Retained for compatibility with tests that exercise the helper directly.
func normalizeAllowed(list []string) map[string]struct{} {
	m := make(map[string]struct{}, len(list))
	for _, item := range list {
		item = strings.ToLower(strings.TrimSpace(item))
		if item == "" {
			continue
		}
		m[item] = struct{}{}
	}
	return m
}
