// test_model.go — 模型连通性测试：用目标的上游凭证发起最小请求。
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// TestModelResult 描述一次模型测试的结果。
type TestModelResult struct {
	Success     bool   `json:"success"`
	StatusCode  int    `json:"status_code"`
	UpstreamURL string `json:"upstream_url"`
	LatencyMS   int64  `json:"latency_ms"`
	Error       string `json:"error,omitempty"`
	BodyPreview string `json:"body_preview,omitempty"`
}

// testModelTimeout 单次模型测试的请求超时。
const testModelTimeout = 60 * time.Second

// TestModel 用目标 targetName 的上游凭证对 model 发起最小请求，
// 验证上游通路是否正常。按 endpoint_type 自动适配请求协议与路径。
//
// OpenAI 类型采取双探：先试 chat/completions，400 则回退 responses API。
// Claude/Gemini/Image 各自独立适配。
func (s *Service) TestModel(targetName, model string) (*TestModelResult, error) {
	state, ok := s.targetByName(targetName)
	if !ok {
		return nil, fmt.Errorf("target %q not found", targetName)
	}
	target := state.Target()

	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("model must not be empty")
	}
	if len(target.APIKeys) == 0 {
		return nil, fmt.Errorf("target %q has no api keys", targetName)
	}

	switch target.EndpointType {
	case config.EndpointTypeOpenAI, config.EndpointTypeDualProtocol, config.EndpointTypeAzureOpenAI:
		// OpenAI/Azure 双探：chat → responses 回退，多 key 时遍历 active key 快照。
		// Azure v1 API 同样支持 Responses，reasoning 模型（GPT-5 系列等）只接受 Responses。
		return s.testWithKeyRotation(state, target, model, []string{chatCompletionsPath, responsesPath},
			func(path string) []byte {
				if path == chatCompletionsPath {
					return chatCompletionsBody(model)
				}
				return responsesBody(model)
			})

	case config.EndpointTypeClaude:
		// Anthropic Messages API（独立）
		return s.testWithKeyRotation(state, target, model, []string{"/v1/messages"},
			func(string) []byte { return claudeBody(model) })

	case config.EndpointTypeGemini:
		// Gemini generateContent（独立，model 在 URL 中）
		return s.testWithKeyRotation(state, target, model, []string{"/v1beta/models/" + model + ":generateContent"},
			func(string) []byte { return geminiBody() })

	case config.EndpointTypeOpenAIImage:
		// Image 生成（独立，terminal URL 由 buildURL 整体覆盖路径）
		return s.testWithKeyRotation(state, target, model, []string{"/v1/images/generations"},
			func(string) []byte { return imageBody(model) })

	default:
		return nil, fmt.Errorf("unsupported endpoint_type for model test: %s", target.EndpointType)
	}
}

// testWithKeyRotation 处理多 key 遍历逻辑：
//   - 多 key target（state.keyPool != nil）：遍历 snapshotActiveKeys() 返回的 active key 快照，
//     逐个尝试 paths，首个 2xx 即成功返回。所有 key 都失败则返回最后一次结果 + 合并 error。
//     纯只读：不推进 rrCounter、不调 markExhausted，避免污染生产 keypool 状态。
//   - 单 key target（state.keyPool == nil）：直接用 APIKeys[0]，行为与改造前一致。
//
// pathBody 根据 path 返回对应的请求体；对于 OpenAI 双探，chat 失败后自动试 responses。
func (s *Service) testWithKeyRotation(state *targetState, target *Target, model string, paths []string, pathBody func(path string) []byte) (*TestModelResult, error) {
	// 决定要尝试的 key 列表
	type keyChoice struct{ key string }
	var keys []keyChoice
	if state.keyPool != nil {
		for _, ks := range state.keyPool.snapshotActiveKeys() {
			keys = append(keys, keyChoice{ks.Key})
		}

		if len(keys) == 0 {
			return nil, fmt.Errorf("target %q has no available API keys; keys are blocked or in cooldown", target.Name)
		}
	} else {
		keys = []keyChoice{{target.APIKeys[0]}}
	}

	var lastResult *TestModelResult
	var triedCount int
	for _, kc := range keys {
		for _, path := range paths {
			result := s.sendTestRequest(target, path, pathBody(path), kc.key)
			triedCount++
			if result.Success {
				return result, nil
			}
			lastResult = result
		}
	}
	// 所有尝试都失败
	if lastResult == nil {
		lastResult = &TestModelResult{Error: "no key or path attempted"}
	}
	if triedCount > 1 {
		lastResult.Error = fmt.Sprintf("%s (tried %d attempts across %d keys)", lastResult.Error, triedCount, len(keys))
	}
	return lastResult, nil
}

const chatCompletionsPath = "/v1/chat/completions"
const responsesPath = "/v1/responses"

// sendTestRequest 构造并发送一次最小测试请求，返回结构化结果。
// apiKey 指定用哪个 key 注入认证头（由上层 testWithKeyRotation 决定轮询顺序）。
func (s *Service) sendTestRequest(target *Target, clientPath string, body []byte, apiKey string) *TestModelResult {
	result := &TestModelResult{}

	// Build a client-shaped request so provider body/path policies and model
	// mappings see exactly the same inputs as ordinary forwarding.
	clientReq, err := http.NewRequest(http.MethodPost, clientPath, nil)
	if err != nil {
		result.Error = fmt.Sprintf("build request: %v", err)
		return result
	}
	clientReq.Header.Set("Content-Type", "application/json")
	model := extractModel(clientReq, body)
	upstreamModel, mapped := resolveUpstreamModel(target, model)
	// Admin probes may test a model before adding it to the allowed mappings.
	// Keep that existing capability while applying the target prefix to its wire name.
	if !mapped {
		upstreamModel = model
	}
	body, _ = sanitizeTargetBody(s.providerRegistry.Lookup(target.EndpointType), clientReq, body)
	body = prepareTargetBody(target, clientReq, body, model, upstreamModel)
	upstreamURL, err := s.buildURL(target, clientReq.URL)
	if err != nil {
		result.Error = fmt.Sprintf("build upstream url: %v", err)
		return result
	}
	result.UpstreamURL = upstreamURL.String()

	ctx, cancel := context.WithTimeout(context.Background(), testModelTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL.String(), bytes.NewReader(body))
	if err != nil {
		result.Error = fmt.Sprintf("build request: %v", err)
		return result
	}
	req.Header.Set("Content-Type", "application/json")

	if err := injectTargetRequestHeaders(req, target, s.resolveAuthStrategy(target, clientReq), apiKey, clientReq.URL.Path); err != nil {
		result.Error = fmt.Sprintf("inject auth: %v", err)
		return result
	}
	req.Host = target.Endpoint.Host

	start := time.Now()
	resp, err := s.doHTTP(req)
	result.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer resp.Body.Close()

	result.StatusCode = resp.StatusCode
	preview := make([]byte, 1024)
	n, _ := io.ReadFull(resp.Body, preview)
	result.BodyPreview = strings.TrimSpace(string(preview[:n]))

	result.Success = resp.StatusCode >= 200 && resp.StatusCode < 300
	if !result.Success {
		result.Error = fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode)
	}
	return result
}

// === 各协议最小请求体构造 ===

func chatCompletionsBody(model string) []byte {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	return body
}

func responsesBody(model string) []byte {
	body, _ := json.Marshal(map[string]any{
		"model":             model,
		"input":             "hi",
		"max_output_tokens": 16,
	})
	return body
}

func claudeBody(model string) []byte {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	return body
}

func geminiBody() []byte {
	body, _ := json.Marshal(map[string]any{
		"contents": []map[string]any{
			{"parts": []map[string]string{{"text": "hi"}}},
		},
	})
	return body
}

func imageBody(model string) []byte {
	body, _ := json.Marshal(map[string]any{
		"model":  model,
		"prompt": "hi",
		"size":   "1024x1024",
	})
	return body
}
