package proxy

import "encoding/json"

func decodeOpenRouterMetadata(body []byte) (source, reason string) {
	var envelope struct {
		Error struct {
			Metadata struct {
				LimitSource string `json:"limit_source"`
				Reason      string `json:"reason"`
			} `json:"metadata"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return "", ""
	}
	return envelope.Error.Metadata.LimitSource, envelope.Error.Metadata.Reason
}

func openrouterKeyExhausted(status int, body []byte) (bool, string) {
	source, reason := decodeOpenRouterMetadata(body)
	switch status {
	case 401:
		return true, "invalid_token"
	case 402:
		if source == "openrouter_in_flight_budget" || (source == "" && reason == "in_flight_budget_exhausted") {
			return true, "rate_limited"
		}
		if reason == "weight_exceeds_budget" {
			return false, ""
		}
		return true, "billing_error"
	case 403:
		switch source {
		case "openrouter_key_limit", "openrouter_guardrail_budget", "openrouter_workspace_budget":
			return true, "quota_exceeded"
		}
	}
	return false, ""
}
