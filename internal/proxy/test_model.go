// test_model.go — 模型连通性测试：用目标的上游凭证发起最小请求。
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// TestModelResult 描述一次模型测试的结果。
//
// 测试按 provider 的模式逐个进行（chat、responses、anthropic 等，开启几个就测几个），
// 每个模式单独报告成功或失败，不再“失败才回退”：Modes 是逐模式结果，Summary 是可直接
// 展示的逐行汇总，Attempts 记录每个请求（含多 key 轮换）。Success 要求所有已测模式都通过。
// 顶层的 StatusCode/UpstreamURL/LatencyMS/BodyPreview 取第一个失败的模式，全部通过时取第一个模式。
type TestModelResult struct {
	Success     bool               `json:"success"`
	StatusCode  int                `json:"status_code"`
	UpstreamURL string             `json:"upstream_url"`
	LatencyMS   int64              `json:"latency_ms"`
	Error       string             `json:"error,omitempty"`
	BodyPreview string             `json:"body_preview,omitempty"`
	Summary     string             `json:"summary,omitempty"`
	Modes       []TestModelMode    `json:"modes,omitempty"`
	Attempts    []TestModelAttempt `json:"attempts,omitempty"`
}

// TestModelMode 是一个模式（接口形态）的测试结果；Path 只含上游路径，不含主机、查询串与凭据。
type TestModelMode struct {
	Mode         string `json:"mode"`
	Label        string `json:"label"`
	Success      bool   `json:"success"`
	StatusCode   int    `json:"status_code,omitempty"`
	LatencyMS    int64  `json:"latency_ms,omitempty"`
	Path         string `json:"path,omitempty"`
	Error        string `json:"error,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	BodyPreview  string `json:"body_preview,omitempty"`
}

// TestModelAttempt 是一次探测请求的摘要：只含上游路径（不含查询串、主机与凭据）。
type TestModelAttempt struct {
	Mode         string `json:"mode,omitempty"`
	Path         string `json:"path"`
	StatusCode   int    `json:"status_code,omitempty"`
	Error        string `json:"error,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	BodyPreview  string `json:"body_preview,omitempty"`
}

// 模式标识与展示名。
const (
	testModeChat      = "chat"
	testModeResponses = "responses"
	testModeAnthropic = "anthropic"
	testModeGemini    = "gemini"
	testModeImage     = "image"
)

var testModeLabels = map[string]string{
	testModeChat:      "CHAT模式",
	testModeResponses: "响应模式",
	testModeAnthropic: "CLAUDE模式",
	testModeGemini:    "GEMINI模式",
	testModeImage:     "图像模式",
}

// testModelTimeout 单次模型测试的请求超时。
const testModelTimeout = 60 * time.Second

// testMode 是一个待测试的模式：发往客户端路径 path（由 buildURL 按目标前缀与协议改写）。
type testMode struct {
	id   string
	path string
	body []byte
}

// TestModel 用目标 targetName 的上游凭证对 model 发起最小请求，验证上游通路是否正常。
// 按 endpoint_type 自动适配请求协议与路径，并把该 provider 开启的每个模式都测一遍：
//   - openai / azure_openai：chat、responses；
//   - dual_protocol：chat、anthropic，开启 supports_responses 时再加 responses
//     （与运行时 SupportsPath 的判定一致，未开启就不探测 /v1/responses）；
//   - claude：anthropic；gemini：generateContent；openai_image：图像生成。
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

	var modes []testMode
	switch target.EndpointType {
	case config.EndpointTypeOpenAI, config.EndpointTypeAzureOpenAI:
		// Azure v1 API 同样支持 Responses，reasoning 模型（GPT-5 系列等）只接受 Responses。
		modes = []testMode{
			{testModeChat, chatCompletionsPath, chatCompletionsBody(model)},
			{testModeResponses, responsesPath, responsesBody(model)},
		}
	case config.EndpointTypeDualProtocol:
		modes = []testMode{{testModeChat, chatCompletionsPath, chatCompletionsBody(model)}}
		if target.SupportsResponses {
			modes = append(modes, testMode{testModeResponses, responsesPath, responsesBody(model)})
		}
		modes = append(modes, testMode{testModeAnthropic, messagesPath, claudeBody(model)})
	case config.EndpointTypeClaude:
		modes = []testMode{{testModeAnthropic, messagesPath, claudeBody(model)}}
	case config.EndpointTypeGemini:
		// Gemini generateContent（model 在 URL 中）
		modes = []testMode{{testModeGemini, "/v1beta/models/" + model + ":generateContent", geminiBody()}}
	case config.EndpointTypeOpenAIImage:
		// Image 生成（terminal URL 由 buildURL 整体覆盖路径）
		modes = []testMode{{testModeImage, "/v1/images/generations", imageBody(model)}}
	default:
		return nil, fmt.Errorf("unsupported endpoint_type for model test: %s", target.EndpointType)
	}
	return s.testModes(state, target, modes)
}

// testModes 逐个模式测试，并处理多 key 遍历：
//   - 多 key target（state.keyPool != nil）：遍历 snapshotActiveKeys() 返回的 active key 快照，
//     每个模式从上一个成功的 key 开始依次尝试，首个 2xx 即该模式成功；所有 key 都失败则该模式失败。
//     纯只读：不推进 rrCounter、不调 markExhausted，避免污染生产 keypool 状态。
//   - 单 key target（state.keyPool == nil）：直接用 APIKeys[0]。
func (s *Service) testModes(state *targetState, target *Target, modes []testMode) (*TestModelResult, error) {
	var keys []string
	if state.keyPool != nil {
		for _, ks := range state.keyPool.snapshotActiveKeys() {
			keys = append(keys, ks.Key)
		}
		if len(keys) == 0 {
			return nil, fmt.Errorf("target %q has no available API keys; keys are blocked or in cooldown", target.Name)
		}
	} else {
		keys = []string{target.APIKeys[0]}
	}

	var attempts []TestModelAttempt
	var results []*TestModelResult
	var triedCount int
	preferred := 0
	for _, mode := range modes {
		var modeResult *TestModelResult
		for i := range keys {
			idx := (preferred + i) % len(keys)
			result := s.sendTestRequest(target, mode.path, mode.body, keys[idx])
			triedCount++
			attempt := testModelAttemptOf(result)
			attempt.Mode = mode.id
			attempts = append(attempts, attempt)
			modeResult = result
			if result.Success {
				preferred = idx
				break
			}
		}
		results = append(results, modeResult)
	}

	out := &TestModelResult{Success: true, Attempts: attempts}
	var headline *TestModelResult
	for i, mode := range modes {
		r := results[i]
		entry := testModelModeOf(mode.id, r)
		out.Modes = append(out.Modes, entry)
		if !r.Success {
			out.Success = false
			if headline == nil {
				headline = r
			}
		}
	}
	if headline == nil {
		headline = results[0]
	}
	out.StatusCode = headline.StatusCode
	out.UpstreamURL = headline.UpstreamURL
	out.LatencyMS = headline.LatencyMS
	out.BodyPreview = headline.BodyPreview
	out.Summary = summarizeTestModes(out.Modes)
	if !out.Success {
		out.Error = headline.Error
		if triedCount > 1 {
			out.Error = fmt.Sprintf("%s (tried %d attempts across %d keys)", out.Error, triedCount, len(keys))
		}
		out.Error += describeTestModelAttempts(attempts)
	}
	return out, nil
}

// testModelModeOf 把一个模式最终的请求结果压成模式摘要。
func testModelModeOf(mode string, r *TestModelResult) TestModelMode {
	entry := TestModelMode{
		Mode:        mode,
		Label:       testModeLabels[mode],
		Success:     r.Success,
		StatusCode:  r.StatusCode,
		LatencyMS:   r.LatencyMS,
		BodyPreview: r.BodyPreview,
	}
	if u, err := url.Parse(r.UpstreamURL); err == nil {
		entry.Path = u.Path
	}
	if !r.Success {
		entry.Error = r.Error
		entry.ErrorCode, entry.ErrorMessage = parseUpstreamErrorBody(r.BodyPreview)
	}
	return entry
}

// summarizeTestModes 生成可直接展示的逐行汇总，每个模式一行：
//
//	1、CHAT模式：成功（238ms）
//	2、响应模式：失败（HTTP 404 Not Found · /api/coding/paas/v4/responses）
func summarizeTestModes(modes []TestModelMode) string {
	lines := make([]string, 0, len(modes))
	for i, m := range modes {
		if m.Success {
			lines = append(lines, fmt.Sprintf("%d、%s：成功（%dms）", i+1, m.Label, m.LatencyMS))
			continue
		}
		var detail []string
		if m.StatusCode != 0 {
			detail = append(detail, fmt.Sprintf("HTTP %d", m.StatusCode))
		}
		switch {
		case m.ErrorCode != "" && m.ErrorMessage != "":
			detail = append(detail, fmt.Sprintf("[%s] %s", m.ErrorCode, m.ErrorMessage))
		case m.ErrorMessage != "":
			detail = append(detail, m.ErrorMessage)
		case m.ErrorCode != "":
			detail = append(detail, "["+m.ErrorCode+"]")
		default:
			if preview := compactTestModelPreview(m.BodyPreview); preview != "" {
				detail = append(detail, preview)
			} else if m.StatusCode == 0 && m.Error != "" {
				detail = append(detail, m.Error) // 未收到响应：传输层错误
			}
		}
		text := strings.Join(detail, " ")
		if m.Path != "" {
			if text != "" {
				text += " · "
			}
			text += m.Path
		}
		lines = append(lines, fmt.Sprintf("%d、%s：失败（%s）", i+1, m.Label, text))
	}
	return strings.Join(lines, "\n")
}

// testModelAttemptPreviewRunes 限制 error 文本中每次探测的报错体摘要长度。
const testModelAttemptPreviewRunes = 200

// testModelAttemptOf 把一次探测结果压成摘要。路径取上游 URL 的 path，
// 不含主机、查询串与凭据；本地构造失败（未发出请求）时回退到客户端路径。
func testModelAttemptOf(result *TestModelResult) TestModelAttempt {
	attempt := TestModelAttempt{
		StatusCode:  result.StatusCode,
		BodyPreview: result.BodyPreview,
	}
	if !result.Success {
		attempt.Error = result.Error
		attempt.ErrorCode, attempt.ErrorMessage = parseUpstreamErrorBody(result.BodyPreview)
	}
	if u, err := url.Parse(result.UpstreamURL); err == nil {
		attempt.Path = u.Path
	}
	return attempt
}

// describeTestModelAttempts 生成追加到 error 末尾的逐次摘要：每次探测一行，
// 形如“#1 /path → HTTP 403 [1220] 消息”，使每次的状态码与上游原因都能在管理端直接读到；
// 单次探测也附带，便于定位上游原因。上游错误体无法解析时回退到压缩后的原文预览。
func describeTestModelAttempts(attempts []TestModelAttempt) string {
	var b strings.Builder
	for i, a := range attempts {
		if a.Path == "" && a.StatusCode == 0 && a.BodyPreview == "" {
			continue
		}
		b.WriteString("\n")
		if len(attempts) > 1 {
			fmt.Fprintf(&b, "#%d ", i+1)
		}
		b.WriteString(a.Path)
		if a.StatusCode != 0 {
			fmt.Fprintf(&b, " → HTTP %d", a.StatusCode)
		}
		switch {
		case a.ErrorCode != "" && a.ErrorMessage != "":
			fmt.Fprintf(&b, " [%s] %s", a.ErrorCode, a.ErrorMessage)
		case a.ErrorMessage != "":
			b.WriteString(" " + a.ErrorMessage)
		case a.ErrorCode != "":
			fmt.Fprintf(&b, " [%s]", a.ErrorCode)
		default:
			if preview := compactTestModelPreview(a.BodyPreview); preview != "" {
				b.WriteString(" " + preview)
			}
		}
	}
	return b.String()
}

// compactTestModelPreview 折叠空白、剔除非法 UTF-8 并按字符数截断。
func compactTestModelPreview(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) > testModelAttemptPreviewRunes {
		return string(runes[:testModelAttemptPreviewRunes]) + "…"
	}
	return s
}

const chatCompletionsPath = "/v1/chat/completions"
const responsesPath = "/v1/responses"
const messagesPath = "/v1/messages"

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

// parseUpstreamErrorBody 从上游错误体里取错误码与消息，覆盖常见形态：
// {"error":{"code","message"}}、{"error":{"type","message"}}、
// Spring 风格 {"status":404,"error":"Not Found"}、{"code":1000,"msg":"..."} 与顶层 {"message"}。
// 只做展示用的最佳努力：不是 JSON、被截断或字段缺失时返回空，调用方回退到原文预览。
func parseUpstreamErrorBody(body string) (code, message string) {
	body = strings.TrimSpace(body)
	if body == "" || body[0] != '{' {
		return "", ""
	}
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &top) != nil {
		return "", ""
	}
	if raw, ok := top["error"]; ok {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) == nil {
			if code = jsonScalar(obj["code"]); code == "" {
				code = jsonScalar(obj["type"])
			}
			if message = jsonScalar(obj["message"]); message == "" {
				message = jsonScalar(obj["msg"])
			}
		} else {
			message = jsonScalar(raw)
			if code = jsonScalar(top["status"]); code == "" {
				code = jsonScalar(top["code"])
			}
		}
	}
	if message == "" {
		if message = jsonScalar(top["msg"]); message == "" {
			message = jsonScalar(top["message"])
		}
	}
	if code == "" {
		code = jsonScalar(top["code"])
	}
	return truncateRunes(code, 64), compactTestModelPreview(message)
}

// jsonScalar 把 JSON 字符串或数字转成文本；其他类型（对象、数组、null、布尔）视为空。
func jsonScalar(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	switch {
	case raw[0] == '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return ""
		}
		return strings.TrimSpace(s)
	case raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9'):
		return string(raw)
	}
	return ""
}

func truncateRunes(s string, limit int) string {
	if r := []rune(s); len(r) > limit {
		return string(r[:limit])
	}
	return s
}
