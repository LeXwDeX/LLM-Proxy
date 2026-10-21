// Usage collection observes response bytes without changing the wire protocol.
package proxy

import (
	"bytes"
	"encoding/json"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"math"
	"net/http"
	"net/url"
	"strings"
)

type usageField = uint16

const (
	usageFieldInput             = usage.UsageFieldInput
	usageFieldOutput            = usage.UsageFieldOutput
	usageFieldCached            = usage.UsageFieldCached
	usageFieldCacheCreation     = usage.UsageFieldCacheCreation
	usageFieldExplicitCacheRead = usage.UsageFieldExplicitCacheRead
	usageFieldReasoning         = usage.UsageFieldReasoning
	usageFieldCacheCreation5m   = usage.UsageFieldCacheCreation5m
	usageFieldCacheCreation1h   = usage.UsageFieldCacheCreation1h
	usageFieldToolUsePrompt     = usage.UsageFieldToolUsePrompt
	usageFieldImageInput        = usage.UsageFieldImageInput
	usageFieldCachedImageInput  = usage.UsageFieldCachedImageInput
	usageFieldImageOutput       = usage.UsageFieldImageOutput
)
const usageAllFields = usageFieldInput | usageFieldOutput | usageFieldCached | usageFieldCacheCreation | usageFieldExplicitCacheRead
const (
	usageStatusComplete   = "complete"
	usageStatusIncomplete = "incomplete"
	usageStatusConflict   = "conflict"
)

type usageTokens struct {
	components                                                        []usage.Component
	billingModel, iterationType                                       string
	iterationIndex                                                    int
	nonBillable                                                       bool
	InputTokens, OutputTokens, CachedTokens, CacheCreationTokens      int64
	ExplicitCacheReadTokens, ReasoningTokens                          int64
	CacheCreation5mTokens, CacheCreation1hTokens, ToolUsePromptTokens int64
	ImageInputTokens, CachedImageInputTokens, ImageOutputTokens       int64
	fields                                                            usageField
	status, adapter, diagnostic, responseStatus, finishReason         string
	costIncomplete                                                    bool
	pricingContext                                                    *usage.PricingContext
}

type usageParser struct {
	endpointType, model    string
	protocol               config.ProtocolType
	endpoint               *url.URL
	usageAdapter           string
	requestBody            []byte
	requestHeaders         http.Header
	pricingRequestFields   map[string]any
	pricingRequestCaptured bool
}

func extractUsageTokens(contentType string, body []byte) (usageTokens, string, bool) {
	return (usageParser{}).extractTokens(contentType, body)
}
func extractUsageFromSSE(body []byte) (usageTokens, string, bool) {
	return (usageParser{}).extractSSE(body)
}
func (p usageParser) extractTokens(contentType string, body []byte) (usageTokens, string, bool) {
	body = bytes.TrimSpace(body)
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") || bytes.HasPrefix(body, []byte("data:")) || bytes.HasPrefix(body, []byte("event:")) {
		return p.extractSSE(body)
	}
	payload, err := readUsagePayload(bytes.NewReader(body))
	if err != nil {
		return usageTokens{status: usageStatusIncomplete, diagnostic: "invalid_response"}, "", false
	}
	return p.parsePayload(payload)
}
func (p usageParser) extractSSE(body []byte) (usageTokens, string, bool) {
	u := newUsageStream("text/event-stream", "", p)
	_, _ = u.Write(body)
	u.Close()
	return u.tokens, u.model, u.found
}

// Infer shape only when callers have not supplied the actual operation.
func (p usageParser) operation(m map[string]any) config.ProtocolType {
	if p.protocol != "" {
		return p.protocol
	}
	if p.endpointType == config.EndpointTypeClaude {
		return config.ProtocolAnthropicMessages
	}
	if p.endpointType == config.EndpointTypeGemini {
		return config.ProtocolGemini
	}
	if _, ok := m["promptTokenCount"]; ok {
		return config.ProtocolGemini
	}
	if _, ok := m["cache_read_input_tokens"]; ok {
		return config.ProtocolAnthropicMessages
	}
	if _, ok := m["cache_creation_input_tokens"]; ok {
		return config.ProtocolAnthropicMessages
	}
	if _, ok := m["prompt_tokens"]; ok {
		return config.ProtocolOpenAIChat
	}
	return config.ProtocolOpenAIResponses
}

func usageEnvelope(payload map[string]any) (map[string]any, string) {
	model := strings.ToLower(readString(payload["model"]))
	if model == "" {
		model = strings.ToLower(readString(payload["modelVersion"]))
	}
	for _, key := range []string{"response", "message"} {
		if nested, ok := payload[key].(map[string]any); ok {
			m, nm := usageEnvelope(nested)
			if model == "" {
				model = nm
			}
			if m != nil {
				return m, model
			}
		}
	}
	for _, key := range []string{"usage", "usageMetadata"} {
		if m, ok := payload[key].(map[string]any); ok {
			return m, model
		}
	}
	if _, ok := payload["prompt_tokens"]; ok {
		return payload, model
	}
	return nil, model
}
func (p usageParser) parsePayload(payload map[string]any) (usageTokens, string, bool) {
	p = p.capturePricingRequest()
	m, model := usageEnvelope(payload)
	if p.protocol == "" {
		if _, ok := payload["usageMetadata"]; ok {
			p.protocol = config.ProtocolGemini
		}
		if _, ok := payload["message"]; ok {
			p.protocol = config.ProtocolAnthropicMessages
		}
		if strings.HasPrefix(readString(payload["type"]), "message_") {
			p.protocol = config.ProtocolAnthropicMessages
		}
	}
	t, ok := p.parseMap(m, model)
	if t.pricingContext == nil {
		t.pricingContext = p.pricingContext(payload, m)
	}
	if t.billingModel != "" {
		model = t.billingModel
		p.finalizeClaudeFallbackBilling(&t, payload)
	}
	observer := usageStream{parser: p}
	observer.observeLifecycle(payload, "")
	t.responseStatus = observer.responseStatus
	t.finishReason = observer.finishReason
	return t, model, ok
}
func (p usageParser) parseMap(m map[string]any, responseModel string) (usageTokens, bool) {
	op := p.operation(m)
	model := p.model
	if model == "" {
		model = responseModel
	}
	adapter, _ := resolveUsageAdapter(p.usageAdapter, op)
	if len(m) == 0 {
		return usageTokens{status: usageStatusIncomplete, adapter: adapter, costIncomplete: true, diagnostic: "missing_usage"}, false
	}
	var t usageTokens
	switch op {
	case config.ProtocolAnthropicMessages:
		t = p.parseClaudeIterationUsage(m, responseModel)
	case config.ProtocolGemini:
		t = parseGeminiUsage(m)
	default:
		t = parseOpenAIUsage(m, op)
	}
	t.adapter = adapter
	if adapter == usageAdapterDeepSeek && op == config.ProtocolAnthropicMessages && strings.HasPrefix(strings.ToLower(model), "claude-") {
		t.costIncomplete = true
		t.diagnostic = "upstream_model_alias_requires_price_mapping"
	}

	switch adapter {
	case usageAdapterQwen:
		p.patchQwenUsage(&t, m, op)
	case usageAdapterDeepSeek:
		patchDeepSeekUsage(&t, m, op)
	case usageAdapterGLM:
		patchGLMUsage(&t, m, op)
	case usageAdapterKimi:
		p.patchKimiUsage(&t, m, op)
	}
	if op != config.ProtocolAnthropicMessages && op != config.ProtocolGemini {
		t.finalizeOpenAIImagePartitions(m, op)
	}
	t.normalizeImagePartitions()
	if t.status != "conflict" {
		if t.fields&(usageFieldInput|usageFieldOutput) == usageFieldInput|usageFieldOutput {
			t.status = usageStatusComplete
		} else {
			t.status = usageStatusIncomplete
		}
	}
	if t.status != usageStatusComplete {
		t.costIncomplete = true
	}
	return t, t.fields != 0
}

// Legacy helper is a standard protocol fallback, never a vendor detector.
func parseUsageMap(m map[string]any) (usageTokens, bool) { return (usageParser{}).parseMap(m, "") }
func (t *usageTokens) conflict(reason string) {
	t.status = usageStatusConflict
	t.costIncomplete = true
	if t.diagnostic == "" {
		t.diagnostic = reason
	}
}

func (t *usageTokens) normalizeImagePartitions() {
	evt := usage.Event{InputTokens: t.InputTokens, OutputTokens: t.OutputTokens, CachedTokens: t.CachedTokens,
		ImageInputTokens: t.ImageInputTokens, CachedImageInputTokens: t.CachedImageInputTokens, ImageOutputTokens: t.ImageOutputTokens,
		ExplicitCacheReadTokens: t.ExplicitCacheReadTokens, UsageStatus: t.status, CostIncomplete: t.costIncomplete, UsageDiagnostic: t.diagnostic}
	usage.NormalizeImagePartitions(&evt)
	t.ImageInputTokens, t.CachedImageInputTokens, t.ImageOutputTokens = evt.ImageInputTokens, evt.CachedImageInputTokens, evt.ImageOutputTokens
	t.ExplicitCacheReadTokens = evt.ExplicitCacheReadTokens
	t.status, t.costIncomplete, t.diagnostic = evt.UsageStatus, evt.CostIncomplete, evt.UsageDiagnostic
}

func (t *usageTokens) missingDimension(reason string) {
	t.costIncomplete = true
	if t.diagnostic == "" {
		t.diagnostic = reason
	}
}
func (t *usageTokens) count(m map[string]any, key string, field usageField) int64 {
	v, present := m[key]
	if !present {
		return 0
	}
	t.fields |= field
	var n int64
	valid := true
	switch x := v.(type) {
	case json.Number:
		var err error
		n, err = x.Int64()
		valid = err == nil
	case float64:
		valid = !math.IsNaN(x) && !math.IsInf(x, 0) && x >= 0 && x < math.Exp2(63) && math.Trunc(x) == x
		if valid {
			n = int64(x)
		}
	case int:
		n = int64(x)
	case int64:
		n = x
	default:
		valid = false
	}
	if !valid || n < 0 {
		t.conflict("invalid_count:" + key)
		return 0
	}
	return n
}
func usageDetails(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}
func (t *usageTokens) partition(total int64) {
	if t.CachedTokens > total || t.CacheCreationTokens > total-t.CachedTokens {
		t.conflict("input_partition_exceeds_total")
		t.InputTokens = 0
		return
	}
	t.InputTokens = total - t.CachedTokens - t.CacheCreationTokens
}

func readInt64(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case float32:
		return int64(v)
	case int64:
		return v
	case int32:
		return int64(v)
	case int:
		return int64(v)
	case json.Number:
		n, err := v.Int64()
		if err == nil {
			return n
		}
		f, err := v.Float64()
		if err == nil {
			return int64(f)
		}
	case string:
		var n json.Number = json.Number(strings.TrimSpace(v))
		if iv, err := n.Int64(); err == nil {
			return iv
		}
	}
	return 0
}

func readString(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// limitedCaptureWriter 捕获流的头部 + 尾部各一段，总预算 limit（头 1/4、尾 3/4）。
// 头部保留首事件（Claude message_start 的 input_tokens、错误日志摘要都取头部），
// 尾部保留最终 usage 累计块；中间超限部分丢弃。尾部用环形缓冲，Write 均摊
// O(1)，避免旧实现“超限后每次 Write 全量 memmove limit 字节”的 O(N×limit) 退化。
type limitedCaptureWriter struct {
	limit int

	head      []byte
	tail      []byte // 环形缓冲，装满后 len == tailLimit
	tailStart int    // 环形起点（最旧字节位置）
	tailFull  bool
}

func (w *limitedCaptureWriter) headLimit() int { return w.limit / 4 }
func (w *limitedCaptureWriter) tailLimit() int { return w.limit - w.limit/4 }

func (w *limitedCaptureWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.limit <= 0 {
		return n, nil
	}

	// 先填头部固定段。
	if hl := w.headLimit(); len(w.head) < hl {
		take := hl - len(w.head)
		if take > len(p) {
			take = len(p)
		}
		w.head = append(w.head, p[:take]...)
		p = p[take:]
	}
	if len(p) == 0 {
		return n, nil
	}

	tl := w.tailLimit()
	if tl <= 0 {
		return n, nil
	}

	// 单次写入即可覆盖整个环：直接取末尾 tl 字节重置。
	if len(p) >= tl {
		w.tail = append(w.tail[:0], p[len(p)-tl:]...)
		w.tailStart = 0
		w.tailFull = true
		return n, nil
	}

	if !w.tailFull {
		if len(w.tail)+len(p) <= tl {
			w.tail = append(w.tail, p...)
			return n, nil
		}
		// 补满后进入环形模式。
		fill := tl - len(w.tail)
		w.tail = append(w.tail, p[:fill]...)
		p = p[fill:]
		w.tailFull = true
		w.tailStart = 0
	}

	// 环形覆盖写（len(p) < tl 已保证，至多绕环一次）。
	for len(p) > 0 {
		copied := copy(w.tail[w.tailStart:], p)
		w.tailStart = (w.tailStart + copied) % tl
		p = p[copied:]
	}
	return n, nil
}

func (w *limitedCaptureWriter) Bytes() []byte {
	if len(w.head) == 0 && len(w.tail) == 0 {
		return nil
	}
	// 常见小响应：全部落在头部段，零拷贝直接返回。
	if len(w.tail) == 0 {
		return w.head
	}
	out := make([]byte, 0, len(w.head)+len(w.tail))
	out = append(out, w.head...)
	if w.tailFull && w.tailStart > 0 {
		out = append(out, w.tail[w.tailStart:]...)
		out = append(out, w.tail[:w.tailStart]...)
	} else {
		out = append(out, w.tail...)
	}
	return out
}
