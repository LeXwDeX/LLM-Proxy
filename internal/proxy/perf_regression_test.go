// perf_regression_test.go — 性能优化前置回归测试。
// 覆盖错误分类的响应边界与 SSE 用量提取。
package proxy

import (
	"fmt"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"net/url"
	"strings"
	"testing"
)

// Error-classification boundaries exercise the same provider-aware path used
// in request forwarding; arbitrary message text is not a billing signal.
func TestTargetKeyExhaustedBodyBoundaries(t *testing.T) {
	endpoint, _ := url.Parse("https://api.openai.com")
	target := &Target{Endpoint: endpoint}
	for _, body := range [][]byte{nil, {}, []byte(`{"error":"EXCEEDED YOUR QUOTA"}`), []byte(`{"error":{"code":"rate_limit_exceeded","message":"PrepaidBillOverdue Arrearage RESOURCE_EXHAUSTED"}}`)} {
		if exhausted, reason := targetKeyExhausted(target, config.ProtocolOpenAIChat, 429, body); exhausted {
			t.Fatalf("unproven billing failure: %s (%s)", body, reason)
		}
	}
	padding := strings.Repeat("x", 3900)
	body := fmt.Sprintf(`{"error":{"code":"credit_balance_exhausted","message":"%s"}}`, padding)
	if exhausted, reason := targetKeyExhausted(target, config.ProtocolOpenAIChat, 429, []byte(body)); !exhausted || reason != "quota_exceeded" {
		t.Fatalf("large structured financial error lost: %v %s", exhausted, reason)
	}
}

func TestTargetKeyExhaustedAlibabaCodesNotMessages(t *testing.T) {
	endpoint, _ := url.Parse("https://dashscope.aliyuncs.com")
	target := &Target{Endpoint: endpoint, UsageAdapter: "qwen"}
	cases := []struct {
		code   string
		status int
		want   bool
	}{
		{"AccessDenied.Unpurchased", 403, true}, {"AllocationQuota.FreeTierOnly", 403, true},
		{"PrepaidBillOverdue", 429, true}, {"PostpaidBillOverdue", 429, true}, {"CommodityNotPurchased", 429, true},
		{"Throttling.AllocationQuota", 429, false}, {"Throttling", 429, false}, {"insufficient_quota", 429, false},
	}
	for _, tt := range cases {
		body := fmt.Sprintf(`{"code":%q}`, tt.code)
		exhausted, _ := targetKeyExhausted(target, config.ProtocolOpenAIChat, tt.status, []byte(body))
		if exhausted != tt.want {
			t.Fatalf("code=%s exhausted=%v want=%v", tt.code, exhausted, tt.want)
		}
		messageOnly := fmt.Sprintf(`{"code":"Throttling","message":%q}`, tt.code)
		if exhausted, _ := targetKeyExhausted(target, config.ProtocolOpenAIChat, tt.status, []byte(messageOnly)); exhausted {
			t.Fatalf("message triggered billing rule: %s", messageOnly)
		}
	}
}

// ---------- #9: extractUsageFromSSE 边界 case ----------

func TestExtractUsageFromSSE_OpenAIFormat(t *testing.T) {
	// OpenAI streaming: usage in the last data chunk
	sseBody := []byte(
		`data: {"id":"chatcmpl-abc","choices":[{"delta":{"content":"Hello"}}]}` + "\n\n" +
			`data: {"id":"chatcmpl-abc","choices":[{"delta":{"content":" world"}}]}` + "\n\n" +
			`data: {"id":"chatcmpl-abc","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60}}` + "\n\n" +
			`data: [DONE]` + "\n\n",
	)

	tokens, _, ok := extractUsageFromSSE(sseBody)
	if !ok {
		t.Fatal("expected usage found in OpenAI SSE")
	}
	if tokens.InputTokens != 50 {
		t.Errorf("expected input_tokens=50, got %d", tokens.InputTokens)
	}
	if tokens.OutputTokens != 10 {
		t.Errorf("expected output_tokens=10, got %d", tokens.OutputTokens)
	}
}

func TestExtractUsageFromSSE_GeminiFormat(t *testing.T) {
	// Gemini streaming: usageMetadata with promptTokenCount / candidatesTokenCount
	sseBody := []byte(
		`data: {"candidates":[{"content":{"parts":[{"text":"Hi"}]}}]}` + "\n\n" +
			`data: {"candidates":[{"content":{"parts":[{"text":" there"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":25,"candidatesTokenCount":5,"totalTokenCount":30}}` + "\n\n",
	)

	tokens, _, ok := extractUsageFromSSE(sseBody)
	if !ok {
		t.Fatal("expected usage found in Gemini SSE")
	}
	if tokens.InputTokens != 25 {
		t.Errorf("expected input_tokens=25, got %d", tokens.InputTokens)
	}
	if tokens.OutputTokens != 5 {
		t.Errorf("expected output_tokens=5, got %d", tokens.OutputTokens)
	}
}

func TestExtractUsageFromSSE_LargeBody_UsageAtEnd(t *testing.T) {
	// Simulate 500 chunks of content, usage only in the last chunk
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString(fmt.Sprintf(`data: {"id":"c","choices":[{"delta":{"content":"word%d"}}]}`, i))
		sb.WriteString("\n\n")
	}
	sb.WriteString(`data: {"id":"c","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`)
	sb.WriteString("\n\ndata: [DONE]\n\n")

	tokens, _, ok := extractUsageFromSSE([]byte(sb.String()))
	if !ok {
		t.Fatal("expected usage found in large SSE body")
	}
	if tokens.InputTokens != 1000 {
		t.Errorf("expected input_tokens=1000, got %d", tokens.InputTokens)
	}
	if tokens.OutputTokens != 500 {
		t.Errorf("expected output_tokens=500, got %d", tokens.OutputTokens)
	}
}

func TestExtractUsageFromSSE_NoUsage(t *testing.T) {
	sseBody := []byte(
		`data: {"id":"c","choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
			`data: {"id":"c","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			`data: [DONE]` + "\n\n",
	)

	_, _, ok := extractUsageFromSSE(sseBody)
	if ok {
		t.Error("expected no usage found")
	}
}

func TestExtractUsageFromSSE_EmptyBody(t *testing.T) {
	_, _, ok := extractUsageFromSSE(nil)
	if ok {
		t.Error("expected no usage for nil body")
	}
	_, _, ok = extractUsageFromSSE([]byte{})
	if ok {
		t.Error("expected no usage for empty body")
	}
}

func TestExtractUsageFromSSE_MalformedChunks_Skipped(t *testing.T) {
	sseBody := []byte(
		`data: not json at all` + "\n\n" +
			`data: {"broken json` + "\n\n" +
			`data: {"id":"c","choices":[{"delta":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n" +
			`data: [DONE]` + "\n\n",
	)

	tokens, _, ok := extractUsageFromSSE(sseBody)
	if !ok {
		t.Fatal("expected usage found despite malformed chunks")
	}
	if tokens.InputTokens != 10 {
		t.Errorf("expected input_tokens=10, got %d", tokens.InputTokens)
	}
}

func TestExtractUsageFromSSE_WithCachedTokens(t *testing.T) {
	// OpenAI with prompt_tokens_details.cached_tokens.
	// prompt_tokens INCLUDES cache_hit; InputTokens must be the miss portion
	// (prompt_tokens - cached_tokens) so billing charges cache hits at CacheRead price.
	sseBody := []byte(
		`data: {"id":"c","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"prompt_tokens_details":{"cached_tokens":80}}}` + "\n\n" +
			`data: [DONE]` + "\n\n",
	)

	tokens, _, ok := extractUsageFromSSE(sseBody)
	if !ok {
		t.Fatal("expected usage found")
	}
	if tokens.InputTokens != 20 {
		t.Errorf("expected input_tokens=20 (prompt_tokens - cached), got %d", tokens.InputTokens)
	}
	if tokens.CachedTokens != 80 {
		t.Errorf("expected cached_tokens=80, got %d", tokens.CachedTokens)
	}
}

func TestExtractUsageFromSSE_OnlyNonDataLines(t *testing.T) {
	// SSE with event: lines and comments, no data: lines with usage
	sseBody := []byte(
		`event: message_start` + "\n" +
			`: this is a comment` + "\n\n" +
			`event: content_block_delta` + "\n" +
			`data: {"type":"content_block_delta","delta":{"text":"hi"}}` + "\n\n" +
			`event: message_stop` + "\n\n",
	)

	_, _, ok := extractUsageFromSSE(sseBody)
	if ok {
		t.Error("expected no usage when no usage data present")
	}
}

// ---------- limitedCaptureWriter 行为锁定 ----------

func TestLimitedCaptureWriter_BasicWrite(t *testing.T) {
	w := &limitedCaptureWriter{limit: 1024}
	data := []byte("hello world")
	n, err := w.Write(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(data) {
		t.Errorf("expected n=%d, got %d", len(data), n)
	}
	if string(w.Bytes()) != "hello world" {
		t.Errorf("expected 'hello world', got %q", string(w.Bytes()))
	}
}

func TestLimitedCaptureWriter_MultipleWrites(t *testing.T) {
	w := &limitedCaptureWriter{limit: 1024}
	w.Write([]byte("hello "))
	w.Write([]byte("world"))
	if string(w.Bytes()) != "hello world" {
		t.Errorf("expected 'hello world', got %q", string(w.Bytes()))
	}
}

func TestLimitedCaptureWriter_OverflowTruncates(t *testing.T) {
	w := &limitedCaptureWriter{limit: 10}
	w.Write([]byte("12345678901234567890")) // 20 bytes, limit 10
	got := string(w.Bytes())
	if len(got) != 10 {
		t.Errorf("expected 10 bytes, got %d: %q", len(got), got)
	}
	// New head+tail semantics: head 1/4 (=2 bytes) + tail 3/4 (=8 bytes).
	// head = "12", tail = last 8 of input = "34567890" → "1234567890".
	if got != "1234567890" {
		t.Errorf("expected head+tail %q, got %q", "1234567890", got)
	}
}

func TestLimitedCaptureWriter_IncrementalOverflow(t *testing.T) {
	w := &limitedCaptureWriter{limit: 10}
	// Write 8 bytes, then 8 more = 16 total, limit 10
	w.Write([]byte("12345678"))
	w.Write([]byte("ABCDEFGH"))
	got := string(w.Bytes())
	if len(got) != 10 {
		t.Errorf("expected 10 bytes after overflow, got %d: %q", len(got), got)
	}
	// New head+tail semantics: headLimit=2, tailLimit=8.
	// 1st write "12345678": head="12", tail="345678" (6 bytes, not full).
	// 2nd write "ABCDEFGH" (8 bytes >= tailLimit 8): tail reset to last 8 = "ABCDEFGH".
	// Bytes() = head "12" + tail "ABCDEFGH" = "12ABCDEFGH".
	expected := "12ABCDEFGH"
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestLimitedCaptureWriter_ZeroLimit(t *testing.T) {
	w := &limitedCaptureWriter{limit: 0}
	n, err := w.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 5 {
		t.Errorf("expected n=5, got %d", n)
	}
	if w.Bytes() != nil {
		t.Errorf("expected nil bytes for zero limit, got %q", string(w.Bytes()))
	}
}

func TestLimitedCaptureWriter_EmptyBytes(t *testing.T) {
	w := &limitedCaptureWriter{limit: 1024}
	if w.Bytes() != nil {
		t.Error("expected nil for empty writer")
	}
}

func TestLimitedCaptureWriter_ReturnsNEqualToInputLen(t *testing.T) {
	// Write must always return len(p) even when truncating, to satisfy io.Writer contract
	w := &limitedCaptureWriter{limit: 5}
	data := []byte("1234567890")
	n, err := w.Write(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(data) {
		t.Errorf("Write must return n=%d (len(input)), got %d", len(data), n)
	}
}
