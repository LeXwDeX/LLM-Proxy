package fakellm

import (
	"fmt"
	"net/http"
	"time"
)

// preset is a canned provider failure. Fields left empty fall back to the
// scenario's own values or to the status defaults.
type preset struct {
	status      int
	code        string
	errType     string
	message     string
	flavor      string // forces the envelope dialect, e.g. GLM business codes
	body        string // verbatim body (HTTP-200 error shapes)
	retry       string // Retry-After header
	contentType string // optional wire media type; defaults to application/json
	// finish makes this a reply preset: a normal HTTP 200 Chat Completions
	// answer (JSON or SSE, per the request) whose terminal finish_reason is
	// finish. It never renders an error envelope; see applyFinishPreset.
	finish string
}

// glmResetMessage mimics GLM's quota message, which carries a CST reset time.
func glmResetMessage(code, what string) string {
	reset := time.Now().In(time.FixedZone("CST", 8*3600)).Add(2 * time.Hour).Format("2006-01-02 15:04:05")
	return fmt.Sprintf("[%s][%s。您的限额将在 %s 重置。]", code, what, reset)
}

func lookupPreset(name string) (preset, bool) {
	switch name {
	// Cross-provider.
	case "invalid_key":
		return preset{status: 401, code: "invalid_api_key", errType: "authentication_error", message: "Incorrect API key provided"}, true
	case "rate_limit":
		return preset{status: 429, code: "rate_limit_exceeded", errType: "rate_limit_error", message: "Rate limit reached"}, true
	case "insufficient_quota": // OpenAI: throttling-class, does not disable the key
		return preset{status: 429, code: "insufficient_quota", errType: "insufficient_quota", message: "You exceeded your current quota"}, true
	case "credit_exhausted":
		return preset{status: 429, code: "credit_balance_exhausted", errType: "insufficient_quota", message: "Credit balance exhausted"}, true
	case "spend_limit":
		return preset{status: 429, code: "organization_spend_limit_exceeded", errType: "insufficient_quota", message: "Organization spend limit exceeded"}, true
	case "billing", "insufficient_balance":
		return preset{status: 402, code: "insufficient_balance", errType: "billing_error", message: "Insufficient Balance"}, true
	case "overloaded":
		return preset{status: 529, code: "overloaded", errType: "overloaded_error", message: "Overloaded"}, true
	case "server_error":
		return preset{status: 500, code: "server_error", errType: "api_error", message: "fake internal error"}, true
	case "unavailable":
		return preset{status: 503, code: "service_unavailable", errType: "api_error", message: "Service Unavailable"}, true
	case "bad_request":
		return preset{status: 400, code: "invalid_request_error", errType: "invalid_request_error", message: "fake invalid request"}, true
	case "not_found":
		return preset{status: 404, code: "model_not_found", errType: "not_found_error", message: "The model does not exist"}, true
	case "context_length":
		return preset{status: 400, code: "context_length_exceeded", errType: "invalid_request_error", message: "maximum context length exceeded"}, true

	// Gemini.
	case "gemini_invalid_key":
		return preset{status: 400, code: "API_KEY_INVALID", message: "API key not valid. Please pass a valid API key.", flavor: FlavorGemini}, true
	case "gemini_exhausted":
		return preset{status: 429, code: "RESOURCE_EXHAUSTED", message: "Resource has been exhausted (e.g. check quota).", flavor: FlavorGemini}, true

	// Zhipu GLM business codes.
	case "glm_1302":
		return preset{status: 429, code: "1302", errType: "rate_limit_error", message: "[1302][您当前使用该API的并发数过高，请降低并发]", flavor: FlavorGLM}, true
	case "glm_1303":
		return preset{status: 429, code: "1303", errType: "rate_limit_error", message: "[1303][您当前使用该API的频率过高，请降低频率]", flavor: FlavorGLM}, true
	case "glm_1305":
		return preset{status: 429, code: "1305", errType: "rate_limit_error", message: "[1305][当前API请求过多，请稍后重试]", flavor: FlavorGLM}, true
	case "glm_1308":
		return preset{status: 429, code: "1308", errType: "rate_limit_error", message: glmResetMessage("1308", "已达到 5 小时的使用上限"), flavor: FlavorGLM}, true
	case "glm_1310":
		return preset{status: 429, code: "1310", errType: "rate_limit_error", message: glmResetMessage("1310", "已达到每周/每月使用上限"), flavor: FlavorGLM}, true
	case "glm_1113":
		return preset{status: 429, code: "1113", errType: "rate_limit_error", message: "[1113][您的账户已欠费，请充值后重试]", flavor: FlavorGLM}, true
	case "glm_1309":
		return preset{status: 429, code: "1309", errType: "rate_limit_error", message: "[1309][您的 GLM Coding Plan 套餐已到期]", flavor: FlavorGLM}, true
	case "glm_1311":
		return preset{status: 429, code: "1311", errType: "rate_limit_error", message: "[1311][当前订阅套餐暂未开放该模型权限]", flavor: FlavorGLM}, true
	case "glm_1313":
		return preset{status: 429, code: "1313", errType: "rate_limit_error", message: "[1313][触发公平使用策略，请稍后再试]", flavor: FlavorGLM}, true
	case "glm_1316":
		return preset{status: 429, code: "1316", errType: "rate_limit_error", message: glmResetMessage("1316", "已达到 5 小时使用上限且账户余额不足"), flavor: FlavorGLM}, true
	case "glm_1000":
		return preset{status: 401, code: "1000", errType: "authentication_error", message: "身份验证失败", flavor: FlavorGLM}, true

	// Alibaba Qwen / Bailian.
	case "qwen_arrearage":
		return preset{status: 400, code: "Arrearage", errType: "Arrearage", message: "Access denied, please make sure your account is in good standing.", flavor: FlavorQwen}, true
	case "qwen_free_tier":
		return preset{status: 403, code: "AllocationQuota.FreeTierOnly", errType: "AllocationQuota.FreeTierOnly", message: "The free tier of the model has been exhausted.", flavor: FlavorQwen}, true
	case "qwen_unpurchased":
		return preset{status: 403, code: "AccessDenied.Unpurchased", errType: "AccessDenied.Unpurchased", message: "Access to model denied.", flavor: FlavorQwen}, true
	case "qwen_throttling":
		return preset{status: 429, code: "Throttling.RateQuota", errType: "Throttling.RateQuota", message: "Requests rate limit exceeded, please try again later.", flavor: FlavorQwen}, true
	case "qwen_allocation":
		return preset{status: 429, code: "Throttling.AllocationQuota", errType: "Throttling.AllocationQuota", message: "Allocated quota exceeded, please increase your quota limit.", flavor: FlavorQwen}, true
	case "qwen_data_inspection":
		return preset{status: 400, code: "data_inspection_failed", errType: "data_inspection_failed", message: "Input data may contain inappropriate content.", flavor: FlavorQwen}, true
	case "qwen_invalid_key":
		return preset{status: 401, code: "invalid_api_key", errType: "invalid_request_error", message: "Incorrect API key provided.", flavor: FlavorQwen}, true

	// DeepSeek.
	case "deepseek_402":
		return preset{status: 402, code: "invalid_request_error", errType: "unknown_error", message: "Insufficient Balance", flavor: FlavorDeepSeek}, true
	case "deepseek_429":
		return preset{status: 429, code: "rate_limit_reached", errType: "rate_limit_error", message: "Rate Limit Reached", flavor: FlavorDeepSeek}, true
	case "deepseek_503":
		return preset{status: 503, code: "server_overloaded", errType: "server_error", message: "Server Overloaded", flavor: FlavorDeepSeek}, true
	// Chat Completions finish_reason "aborted" ("生成过程被中断"). Source:
	// https://api-docs.deepseek.com/zh-cn/api/create-chat-completion, checked
	// 2026-10-20 (documentation contract, not live acceptance). JSON carries it
	// on choices[0]; SSE on the last data chunk, which also carries usage,
	// followed by [DONE].
	case "deepseek_aborted":
		return preset{status: 200, flavor: FlavorDeepSeek, finish: "aborted"}, true

	// Moonshot Kimi: keyed on error.type.
	case "kimi_quota":
		return preset{status: 429, errType: "exceeded_current_quota_error", message: "Your account is suspended, please check your plan and billing details", flavor: FlavorKimi}, true
	case "kimi_rate":
		return preset{status: 429, errType: "rate_limit_reached_error", message: "Your account reached max request rate", flavor: FlavorKimi}, true
	case "kimi_overloaded":
		return preset{status: 429, errType: "engine_overloaded_error", message: "The engine is currently overloaded", flavor: FlavorKimi}, true
	case "kimi_invalid_key":
		return preset{status: 401, errType: "invalid_authentication_error", message: "Invalid Authentication", flavor: FlavorKimi}, true

	// MiniMax HTTP 200 JSON ChatCompletionResp only; id/choices are optional.
	// Sources: /docs/api-reference/text-chat-openai and /docs/api-reference/errorcode
	// at https://platform.minimaxi.com, checked 2026-10-13 (not live acceptance).
	case "minimax_1008":
		return preset{status: 200, flavor: FlavorMinimax, body: `{"base_resp":{"status_code":1008,"status_msg":"insufficient balance"}}`}, true
	case "minimax_1002":
		return preset{status: 200, flavor: FlavorMinimax, body: `{"base_resp":{"status_code":1002,"status_msg":"rate limit exceeded"}}`}, true
	case "minimax_1004":
		return preset{status: 200, flavor: FlavorMinimax, body: `{"base_resp":{"status_code":1004,"status_msg":"authentication failed"}}`}, true
	case "minimax_2056":
		return preset{status: 200, flavor: FlavorMinimax, body: `{"base_resp":{"status_code":2056,"status_msg":"M Plan resource limit exceeded; wait for the next resource window"}}`}, true
	case "minimax_2013":
		return preset{status: 200, flavor: FlavorMinimax, body: `{"base_resp":{"status_code":2013,"status_msg":"invalid parameters"}}`}, true
	case "minimax_2049":
		return preset{status: 200, flavor: FlavorMinimax, body: `{"base_resp":{"status_code":2049,"status_msg":"invalid API key"}}`}, true
	case "minimax_1041":
		return preset{status: 200, flavor: FlavorMinimax, body: `{"base_resp":{"status_code":1041,"status_msg":"connection limit exceeded"}}`}, true
	case "minimax_2045":
		return preset{status: 200, flavor: FlavorMinimax, body: `{"base_resp":{"status_code":2045,"status_msg":"request rate growth limit exceeded"}}`}, true

	// OpenRouter.
	case "openrouter_402_inflight":
		return preset{status: 402, message: "Insufficient credits for in-flight requests", flavor: FlavorOpenRouter, retry: "2",
			body: `{"error":{"code":402,"message":"Insufficient credits for in-flight requests","metadata":{"limit_source":"openrouter_in_flight_budget","reason":"in_flight_budget_exhausted"}}}`}, true
	case "openrouter_402_weight":
		return preset{status: 402, flavor: FlavorOpenRouter, body: `{"error":{"metadata":{"limit_source":"openrouter_credits","reason":"weight_exceeds_budget"}}}`}, true
	case "openrouter_403_budget":
		return preset{status: 403, flavor: FlavorOpenRouter, body: `{"error":{"message":"Key budget exhausted","metadata":{"limit_source":"openrouter_key_limit","resets_at":"2099-01-01T00:00:00Z"}}}`}, true
	case "openrouter_403_moderation":
		return preset{status: 403, flavor: FlavorOpenRouter, body: `{"error":{"message":"Moderation rejected","metadata":{"reasons":["moderation"],"flagged_input":"fixture"}}}`}, true
	case "openrouter_401":
		return preset{status: 401, flavor: FlavorOpenRouter, body: `{"error":{"message":"Unauthorized"}}`}, true
	case "openrouter_403_key_limit":
		return preset{status: 403, message: "Key limit exceeded", flavor: FlavorOpenRouter}, true
	case "openrouter_200_error":
		return preset{status: 200, flavor: FlavorOpenRouter, body: `{"error":{"code":502,"message":"Provider returned error","metadata":{"provider_name":"fake","raw":"upstream failed"}}}`}, true
	case "openrouter_402":
		return preset{status: 402, message: "Insufficient credits. Add more using https://openrouter.ai/settings/credits", flavor: FlavorOpenRouter}, true
	case "openrouter_429":
		return preset{status: 429, message: "Rate limit exceeded: free-models-per-min.", flavor: FlavorOpenRouter}, true
	case "openrouter_provider_error":
		return preset{status: 502, message: "Provider returned error", flavor: FlavorOpenRouter}, true
	}
	return preset{}, false
}

// applyFinishPreset resolves a reply preset into ordinary scenario fields and
// returns its name. Error presets and unknown names are left for resolveError.
// An explicit finish or flavor in the same scenario still wins.
func applyFinishPreset(sc *Scenario) string {
	p, ok := lookupPreset(sc.Preset)
	if !ok || p.finish == "" || sc.Status != 0 {
		return ""
	}
	name := sc.Preset
	sc.Finish = firstNonEmpty(sc.Finish, p.finish)
	sc.Flavor = firstNonEmpty(sc.Flavor, p.flavor)
	sc.Preset = ""
	return name
}

// apiError is a resolved failure ready to be rendered in any dialect.
type apiError struct {
	contentType string
	status      int
	code        string
	errType     string
	message     string
	flavor      string
	body        string
}

func resolveError(sc *Scenario, flavor string) (apiError, error) {
	e := apiError{status: sc.Status, flavor: flavor}
	if sc.Preset != "" {
		p, ok := lookupPreset(sc.Preset)
		if !ok {
			return e, fmt.Errorf("fakellm: unknown preset %q", sc.Preset)
		}
		if e.status == 0 {
			e.status = p.status
		}
		e.code, e.errType, e.message, e.body = p.code, p.errType, p.message, p.body
		e.contentType = p.contentType
		if p.retry != "" && sc.RetryAfter == "" {
			sc.RetryAfter = p.retry
		}
		if p.flavor != "" {
			e.flavor = p.flavor
		}
	}
	if sc.ErrCode != "" {
		e.code = sc.ErrCode
	}
	if sc.ErrType != "" {
		e.errType = sc.ErrType
	}
	if sc.ErrMessage != "" {
		e.message = sc.ErrMessage
	}
	if sc.Body != "" {
		e.body = sc.Body
	}
	if e.status == 0 {
		e.status = http.StatusInternalServerError
	}
	if e.message == "" {
		e.message = "fake " + http.StatusText(e.status)
	}
	if e.errType == "" {
		e.errType = defaultErrType(e.status)
	}
	return e, nil
}

func defaultErrType(status int) string {
	switch {
	case status == 401:
		return "authentication_error"
	case status == 402:
		return "billing_error"
	case status == 403:
		return "permission_error"
	case status == 404:
		return "not_found_error"
	case status == 429:
		return "rate_limit_error"
	case status == 529:
		return "overloaded_error"
	case status >= 500:
		return "api_error"
	}
	return "invalid_request_error"
}

func geminiStatus(status int) string {
	switch status {
	case 400:
		return "INVALID_ARGUMENT"
	case 401:
		return "UNAUTHENTICATED"
	case 403:
		return "PERMISSION_DENIED"
	case 404:
		return "NOT_FOUND"
	case 429:
		return "RESOURCE_EXHAUSTED"
	case 503:
		return "UNAVAILABLE"
	}
	return "INTERNAL"
}

// Wire protocols decide the outer error shape; the flavor fills in vendor
// specifics (e.g. GLM's Anthropic-compatible endpoint keeps the Anthropic
// envelope but carries its numeric business code).
const (
	wireOpenAI    = "openai"
	wireAnthropic = "anthropic"
	wireGemini    = "gemini"
)

// envelope renders the error body for the wire protocol and dialect.
func (e apiError) envelope(wire, requestID string) []byte {
	if e.body != "" {
		return []byte(e.body)
	}
	switch wire {
	case wireAnthropic:
		inner := map[string]any{"type": e.errType, "message": e.message}
		if e.flavor == FlavorGLM && e.code != "" {
			inner["code"] = e.code
		}
		return mustJSON(map[string]any{"type": "error", "error": inner, "request_id": requestID})
	case wireGemini:
		status := geminiStatus(e.status)
		if e.code == "API_KEY_INVALID" || e.code == "RESOURCE_EXHAUSTED" {
			status = e.code
		}
		return mustJSON(map[string]any{"error": map[string]any{"code": e.status, "message": e.message, "status": status}})
	}
	switch e.flavor {
	case FlavorGLM:
		return mustJSON(map[string]any{"error": map[string]any{"code": e.code, "message": e.message}})
	case FlavorQwen:
		return mustJSON(map[string]any{"error": map[string]any{"message": e.message, "type": e.errType, "param": nil, "code": e.code}, "request_id": requestID})
	case FlavorOpenRouter:
		return mustJSON(map[string]any{"error": map[string]any{"code": e.status, "message": e.message, "metadata": map[string]any{"provider_name": "fake"}}, "user_id": "user_fake"})
	}
	code := any(e.code)
	if e.code == "" {
		code = nil
	}
	return mustJSON(map[string]any{"error": map[string]any{"message": e.message, "type": e.errType, "param": nil, "code": code}})
}

// streamFailure resolves the error emitted inside a stream (stream_error).
// def applies when the scenario names neither a status nor a preset.
func streamFailure(sc *Scenario, flavor string, def int) apiError {
	status := sc.Status
	if status == 0 && sc.Preset == "" {
		status = def
	}
	e, err := resolveError(&Scenario{Status: status, Preset: sc.Preset, ErrCode: sc.ErrCode, ErrType: sc.ErrType, ErrMessage: sc.ErrMessage}, flavor)
	if err != nil {
		return apiError{status: 400, errType: "invalid_request_error", message: err.Error()}
	}
	return e
}
