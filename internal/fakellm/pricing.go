package fakellm

// These named protocol fixtures model documented response evidence, not live
// billing guarantees. Sources checked 2026-10-14: OpenAI fast-mode and Claude
// fast-mode guides:
// https://developers.openai.com/api/docs/guides/fast-mode
// https://platform.claude.com/docs/en/build-with-claude/fast-mode
// Request values never determine these actual values.
func pricingPreset(preset string, dst map[string]any, anthropic bool) {
	switch preset {
	case "standard":
		if anthropic {
			dst["speed"] = "standard"
			dst["service_tier"] = "standard"
		} else {
			dst["service_tier"] = "default"
		}
	case "fast":
		if anthropic {
			dst["speed"] = "fast"
			dst["service_tier"] = "standard"
		} else {
			dst["service_tier"] = "fast"
		}
	case "ultrafast":
		if !anthropic {
			dst["service_tier"] = "ultrafast"
		}
	case "priority_standard":
		if anthropic {
			dst["speed"] = "standard"
			dst["service_tier"] = "priority"
		}
	case "null":
		dst["service_tier"] = nil
	case "empty":
		dst["service_tier"] = ""
	case "unknown":
		dst["service_tier"] = "future"
	case "missing":
		delete(dst, "service_tier")
		delete(dst, "speed")
	}
}
