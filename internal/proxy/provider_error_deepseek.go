package proxy

// DeepSeek documents insufficient balance as HTTP 402 (handled by the common
// status layer). HTTP 429 denotes a rate limit, not a depleted API key.
func deepseekKeyExhausted(status int, _ upstreamErrorFields) (bool, string) {
	if status == 402 {
		return true, "billing_error"
	}
	return false, ""
}
