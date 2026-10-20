package proxy

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// upstreamErrorFields reads only the error envelope. Text in generated content
// or an error message must never activate another provider's billing policy.
type upstreamErrorFields struct {
	Code    string `json:"code"`
	Type    string `json:"type"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

func decodeUpstreamError(body []byte) upstreamErrorFields {
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Code    string          `json:"code"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return upstreamErrorFields{}
	}
	if len(envelope.Error) > 0 {
		var raw struct {
			Code    json.RawMessage `json:"code"`
			Type    string          `json:"type"`
			Message string          `json:"message"`
			Status  string          `json:"status"`
		}
		if json.Unmarshal(envelope.Error, &raw) != nil {
			return upstreamErrorFields{}
		}
		fields := upstreamErrorFields{Type: raw.Type, Message: raw.Message, Status: raw.Status}
		// Google's code is numeric; it must not suppress the other fields.
		_ = json.Unmarshal(raw.Code, &fields.Code)
		return fields
	}
	return upstreamErrorFields{Code: envelope.Code, Message: envelope.Message}
}

// targetKeyExhausted distinguishes credential/billing failures from transient
// throttling. The caller retains ownership of retries and cooldown scheduling.
func targetKeyExhausted(target *Target, protocol config.ProtocolType, status int, body []byte) (bool, string) {
	if status < 400 {
		return false, ""
	}
	fields := decodeUpstreamError(body)
	if targetVendor(target) == usageAdapterOpenRouter {
		return openrouterKeyExhausted(status, body)
	}
	if status == http.StatusPaymentRequired {
		return true, "billing_error"
	}
	if targetVendor(target) == usageAdapterKimi {
		return kimiKeyExhausted(status, fields)
	}
	if status == http.StatusUnauthorized {
		switch strings.ToLower(fields.Code) {
		case "invalid_api_key", "invalidapikey", "invalid_token":
			return true, "invalid_token"
		}
		if fields.Type == "authentication_error" {
			return true, "invalid_token"
		}
		message := strings.ToLower(fields.Message)
		for _, phrase := range []string{"invalid api key", "invalid api-key", "invalid x-api-key", "incorrect api key", "token expired", "invalid access token", "access denied due to invalid subscription key"} {
			if strings.Contains(message, phrase) {
				return true, "invalid_token"
			}
		}
	}
	if target != nil {
		if usesQwenErrorPatch(target) {
			return qwenKeyExhausted(status, fields)
		}
		vendor := targetVendor(target)
		if vendor == usageAdapterDeepSeek {
			return deepseekKeyExhausted(status, fields)
		}
		if vendor == usageAdapterGLM {
			return false, ""
		} // GLM throttling is handled separately.
	}
	if protocol == config.ProtocolGemini {
		// RESOURCE_EXHAUSTED also reports RPM/TPM exhaustion, not key insolvency.
		if status == http.StatusBadRequest && (fields.Status == "API_KEY_INVALID" || fields.Message == "API key not valid. Please pass a valid API key.") {
			return true, "invalid_token"
		}
		return false, ""
	}
	if protocol == config.ProtocolAnthropicMessages {
		if fields.Type == "billing_error" {
			return true, "billing_error"
		}
		return false, ""
	}
	switch fields.Code {
	case "credit_balance_exhausted", "organization_spend_limit_exceeded", "project_spend_limit_exceeded", "organization_usage_limit_exceeded":
		return true, "quota_exceeded"
	}
	return false, ""
}
