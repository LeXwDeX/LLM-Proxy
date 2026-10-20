package proxy

import "net/http"

// Kimi uses error.type, not message text or OpenAI error.code. The status is
// part of the contract: rate/concurrency/engine overload are not insolvency.
func kimiKeyExhausted(status int, fields upstreamErrorFields) (bool, string) {
	if status == http.StatusTooManyRequests && fields.Type == "exceeded_current_quota_error" {
		return true, "quota_exceeded"
	}
	if status == http.StatusUnauthorized && (fields.Type == "invalid_authentication_error" || fields.Type == "incorrect_api_key_error") {
		return true, "invalid_token"
	}
	return false, ""
}
