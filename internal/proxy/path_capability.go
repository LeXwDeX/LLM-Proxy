package proxy

import (
	"strings"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// PathSupportedByEndpointType applies the config declaration shared with the
// admin schema and provider profiles. Request entry validation remains strict;
// this public compatibility helper keeps its historical unknown-path fallback.
func PathSupportedByEndpointType(epType, path string) bool {
	return config.EndpointTypeSupportsSchema(epType, requestSchemaForPath(path))
}

func requestSchemaForPath(path string) config.RequestSchema {
	pathLower := strings.ToLower(strings.TrimSpace(path))
	switch {
	case isGeminiStylePath(pathLower):
		return config.RequestSchemaGemini
	case isAnthropicStylePath(pathLower):
		return config.RequestSchemaAnthropic
	case isOpenAIResponsesStylePath(pathLower), isAzureResponsesStylePath(pathLower):
		return config.RequestSchemaOpenAIResponses
	case isOpenAIImageStylePath(pathLower):
		return config.RequestSchemaOpenAIImage
	case strings.HasSuffix(pathLower, "/chat/completions"):
		return config.RequestSchemaOpenAIChat
	default:
		return config.RequestSchemaOpenAICompat
	}
}

func openAIImageOperationSupportsPath(operation, path string) bool {
	pathLower := strings.ToLower(path)
	switch operation {
	case config.ImageOperationEdits:
		return strings.HasSuffix(pathLower, "/images/edits")
	case config.ImageOperationVariations:
		return strings.HasSuffix(pathLower, "/images/variations")
	case config.ImageOperationGenerations, "":
		return strings.HasSuffix(pathLower, "/images/generations")
	default:
		return false
	}
}
