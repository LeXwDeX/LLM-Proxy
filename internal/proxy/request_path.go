package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// clientPathKind describes routes that the proxy knows how to classify before
// target selection. Unknown paths must not fall through to OpenAI Chat: that
// fallback can select a target and then misread a different protocol's usage.
type clientPathKind int

const (
	clientPathUnknown clientPathKind = iota
	clientPathOpenAICompatible
	clientPathModelRequest
	clientPathModelList
	clientPathResponses
	clientPathAnthropic
	clientPathGemini
)

func modelRequiredForRequest(r *http.Request, kind clientPathKind) bool {
	if r == nil || r.Method != http.MethodPost {
		return false
	}
	switch kind {
	case clientPathModelRequest, clientPathAnthropic, clientPathGemini:
		return true
	case clientPathResponses:
		// Response retrieval and cancellation use an ID in the URL, not a model
		// in the body. Generation requests are the collection POSTs.
		path := strings.ToLower(r.URL.Path)
		return path == "/v1/responses" || path == "/openai/v1/responses" || isAzureDeploymentRequestPath(path)
	default:
		return false
	}
}

func classifyClientPath(path string) clientPathKind {
	p := strings.ToLower(strings.TrimSpace(path))
	switch requestSchemaForPath(p) {
	case config.RequestSchemaOpenAIResponses:
		return clientPathResponses
	case config.RequestSchemaAnthropic:
		return clientPathAnthropic
	case config.RequestSchemaGemini:
		return clientPathGemini
	}
	if isListEndpoint(p) || strings.HasPrefix(p, "/v1/models/") {
		return clientPathModelList
	}
	if isAzureDeploymentRequestPath(p) {
		return clientPathModelRequest
	}
	switch p {
	case "/v1/chat/completions", "/chat/completions", "/openai/v1/chat/completions",
		"/v1/completions", "/openai/v1/completions", "/v1/embeddings", "/openai/v1/embeddings",
		"/v1/images/generations", "/v1/images/edits", "/v1/images/variations",
		"/v1/audio/transcriptions", "/v1/audio/translations", "/v1/audio/speech",
		"/v1/moderations":
		return clientPathModelRequest
	}
	for _, prefix := range []string{
		"/v1/files", "/v1/uploads", "/v1/batches", "/v1/fine_tuning", "/v1/assistants",
		"/v1/threads", "/v1/vector_stores", "/v1/realtime", "/v1/conversations",
		"/v1/containers", "/v1/videos",
	} {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return clientPathOpenAICompatible
		}
	}
	return clientPathUnknown
}

func isAzureResponsesStylePath(path string) bool {
	if path == "/openai/v1/responses" || strings.HasPrefix(path, "/openai/v1/responses/") {
		return true
	}
	return strings.HasSuffix(path, "/responses") && isAzureDeploymentRequestPath(path)
}

func isGeminiModelMethodPath(path string) bool {
	for _, prefix := range []string{"/v1/models/", "/v1alpha/models/", "/v1beta/models/"} {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		remainder := strings.TrimPrefix(path, prefix)
		model, method, ok := strings.Cut(remainder, ":")
		if !ok || model == "" || strings.Contains(model, "/") {
			return false
		}
		switch method {
		case "generatecontent", "streamgeneratecontent", "counttokens", "embedcontent", "batchembedcontents":
			return true
		}
	}
	return false
}

func isAzureDeploymentRequestPath(path string) bool {
	const prefix = "/openai/deployments/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	remainder := strings.TrimPrefix(path, prefix)
	deployment, operation, ok := strings.Cut(remainder, "/")
	if !ok || deployment == "" {
		return false
	}
	switch operation {
	case "chat/completions", "completions", "responses", "embeddings",
		"images/generations", "images/edits", "images/variations",
		"audio/transcriptions", "audio/translations", "audio/speech":
		return true
	}
	return false
}

type requestError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
		Path    string `json:"path"`
	} `json:"error"`
}

func writeRequestPathError(w http.ResponseWriter, path string) {
	status := http.StatusNotFound
	code := "unknown_request_path"
	message := "request path " + strconv.Quote(path) + " is not supported"
	if isNonCanonicalResponsesPath(path) {
		status = http.StatusBadRequest
		code = "invalid_responses_path"
		message = "request path " + strconv.Quote(path) + " is invalid for OpenAI Responses; use /v1/responses"
	}
	writeClientRequestError(w, status, code, message, path)
}

func writeClientRequestError(w http.ResponseWriter, status int, code, message, path string) {
	var response requestError
	response.Error.Message = message
	response.Error.Type = "invalid_request_error"
	response.Error.Code = code
	response.Error.Path = path
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func isNonCanonicalResponsesPath(path string) bool {
	p := strings.ToLower(strings.TrimSpace(path))
	if p == "/responses" || strings.HasPrefix(p, "/responses/") {
		return true
	}
	if !strings.HasPrefix(p, "/v") {
		return false
	}
	version, remainder, ok := strings.Cut(strings.TrimPrefix(p, "/"), "/")
	if !ok || len(version) < 2 || version[0] != 'v' {
		return false
	}
	for _, digit := range version[1:] {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return version != "v1" && (remainder == "responses" || strings.HasPrefix(remainder, "responses/"))
}
