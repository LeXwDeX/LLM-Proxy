// glm_ratelimit_test.go — 智谱 GLM 限流检测与冷却恢复的单元测试。
// 智谱限流统一为 HTTP 200 + SSE event:error + rate_limit_error，三种 code：
// 1308（5h 额度，有重置时间）、1310（周/月额度，有重置时间）、1302（速率/QPS，无重置时间）。
package proxy

import (
	"strings"
	"testing"
	"time"
)

// 真实抓取的 5h 限流 SSE（code 1308，message 带重置时间）。
var realGLMRateLimitBody = []byte("event: error\n" +
	`data: {"type":"error","error":{"type":"rate_limit_error","code":"1308","message":"[1308][已达到 5 小时的使用上限。您的限额将在 2026-06-16 16:34:31 重置。][202606161545190dde65524a744f1e]"},"request_id":"202606161545190dde65524a744f1e"}` + "\n\n" +
	"data: [DONE]\n")

// 真实抓取的周/月限流 SSE（code 1310，message 带重置时间）。
var realGLMQuotaRateLimitBody = []byte("event: error\n" +
	`data: {"type":"error","error":{"type":"rate_limit_error","code":"1310","message":"[1310][您已达到每周/每月使用上限，您的限额将在 2026-06-22 13:30:50 重置。][202606181014110938f431b2564f39]"},"request_id":"202606181014110938f431b2564f39"}` + "\n\n" +
	"data: [DONE]\n")

// 真实抓取的速率/QPS 限流 SSE（code 1302，message 无重置时间）。
var realGLMQPSRateLimitBody = []byte("event: error\n" +
	`data: {"type":"error","error":{"type":"rate_limit_error","code":"1302","message":"[1302][您的账户已达到速率限制，请您控制请求频率][20260618090314b07d8f0d4095429b]"},"request_id":"20260618090314b07d8f0d4095429b"}` + "\n\n" +
	"data: [DONE]\n")

// 正常的 Claude SSE 流式响应（message_start），绝不能被误判为限流。
var normalClaudeSSEBody = []byte("event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","content":[],"model":"glm-5.2","stop_reason":null,"usage":{"input_tokens":25,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` + "\n\n")

func TestDetectGLMRateLimitInBody_RealSample1308(t *testing.T) {
	limited, code, resetAt := detectGLMRateLimitInBody(realGLMRateLimitBody)
	if !limited {
		t.Fatal("expected GLM rate limit to be detected on 1308 sample")
	}
	if code != "1308" {
		t.Errorf("code = %q, want 1308", code)
	}
	if resetAt.IsZero() {
		t.Fatal("expected non-zero reset time parsed from message")
	}
	cst := time.FixedZone("CST", 8*3600)
	want := time.Date(2026, 6, 16, 16, 34, 31, 0, cst)
	if !resetAt.Equal(want) {
		t.Errorf("resetAt = %v, want %v (CST)", resetAt, want)
	}
}

func TestDetectGLMRateLimitInBody_Quota1310(t *testing.T) {
	limited, code, resetAt := detectGLMRateLimitInBody(realGLMQuotaRateLimitBody)
	if !limited {
		t.Fatal("expected GLM rate limit to be detected on 1310 (weekly/monthly) sample")
	}
	if code != "1310" {
		t.Errorf("code = %q, want 1310", code)
	}
	if resetAt.IsZero() {
		t.Fatal("expected non-zero reset time parsed from 1310 message")
	}
	cst := time.FixedZone("CST", 8*3600)
	want := time.Date(2026, 6, 22, 13, 30, 50, 0, cst)
	if !resetAt.Equal(want) {
		t.Errorf("resetAt = %v, want %v (CST)", resetAt, want)
	}
}

func TestDetectGLMRateLimitInBody_QPS1302(t *testing.T) {
	limited, code, resetAt := detectGLMRateLimitInBody(realGLMQPSRateLimitBody)
	if !limited {
		t.Fatal("expected GLM rate limit to be detected on 1302 (QPS) sample")
	}
	if code != "1302" {
		t.Errorf("code = %q, want 1302", code)
	}
	// QPS 限流 message 无重置时间，resetAt 应为零值。
	if !resetAt.IsZero() {
		t.Errorf("QPS rate limit should have zero reset time, got %v", resetAt)
	}
}

func TestDetectGLMRateLimitInBody_NormalSSE_NotLimited(t *testing.T) {
	limited, _, _ := detectGLMRateLimitInBody(normalClaudeSSEBody)
	if limited {
		t.Fatal("normal Claude SSE must NOT be detected as rate limited")
	}
}

func TestDetectGLMRateLimitInBody_NonRateLimitError(t *testing.T) {
	// 非 rate_limit_error（如 overloaded_error 1305）不应判为限流
	body := []byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"code\":\"1305\"}}\n")
	if limited, _, _ := detectGLMRateLimitInBody(body); limited {
		t.Fatal("non-rate-limit error (overloaded_error) must not be detected")
	}
}

func TestDetectGLMRateLimitInBody_EmptyBody(t *testing.T) {
	if limited, _, _ := detectGLMRateLimitInBody(nil); limited {
		t.Fatal("empty body must not be detected")
	}
	if limited, _, _ := detectGLMRateLimitInBody([]byte{}); limited {
		t.Fatal("empty body must not be detected")
	}
}

func TestDetectGLMRateLimitInBody_NoSpaceVariant(t *testing.T) {
	// 兼容 "event:error"（无空格变体）
	body := []byte("event:error\n" +
		`data: {"type":"error","error":{"type":"rate_limit_error","code":"1308","message":"重置于 2026-06-16 16:34:31"}}` + "\n")
	limited, code, _ := detectGLMRateLimitInBody(body)
	if !limited {
		t.Fatal("event:error (no space) variant should be detected")
	}
	if code != "1308" {
		t.Errorf("code = %q, want 1308", code)
	}
}

func TestGLMReasonForCode(t *testing.T) {
	cases := map[string]string{
		"1308": glmExhaustReason,
		"1310": glmRateLimitQuota,
		"1302": glmRateLimitQPS,
		"1303": glmRateLimitQPS, // 1303 并发限流与 1302 QPS 同属瞬时频率类
		"9999": glmRateLimitUnknown,
		"":     glmRateLimitUnknown,
	}
	for code, want := range cases {
		if got := glmReasonForCode(code); got != want {
			t.Errorf("glmReasonForCode(%q) = %q, want %q", code, got, want)
		}
	}
}

func TestGLMCooldownForCode(t *testing.T) {
	now := time.Date(2026, 6, 16, 3, 0, 0, 0, time.UTC) // 11:00 CST
	cst := time.FixedZone("CST", 8*3600)

	t.Run("qps_1302_uses_short_60s_cooldown", func(t *testing.T) {
		got := glmCooldownForCode("1302", time.Time{}, now)
		want := now.Add(glmQPSCooldown)
		if !got.Equal(want) {
			t.Errorf("1302 cooldown = %v, want %v (now+60s)", got, want)
		}
	})

	t.Run("concurrency_1303_uses_short_60s_cooldown_aligned_with_429_path", func(t *testing.T) {
		// 1303 并发限流：SSE 路径必须与 429 路径（markRateLimitedWithBody case "1302","1303" → 60s）一致，
		// 避免双路径分类冲突（曾经 1303 在 SSE 走 5h、429 走 60s，差 300 倍）。
		got := glmCooldownForCode("1303", time.Time{}, now)
		want := now.Add(glmQPSCooldown)
		if !got.Equal(want) {
			t.Errorf("1303 cooldown = %v, want %v (now+60s, 与 429 路径对齐)", got, want)
		}
	})

	t.Run("quota_1310_uses_parsed_reset_time", func(t *testing.T) {
		resetAt := time.Date(2026, 6, 22, 13, 30, 50, 0, cst)
		got := glmCooldownForCode("1310", resetAt, now)
		if !got.Equal(resetAt) {
			t.Errorf("1310 cooldown = %v, want resetAt %v", got, resetAt)
		}
	})

	t.Run("1308_falls_back_to_now_plus_6s_when_no_reset", func(t *testing.T) {
		got := glmCooldownForCode("1308", time.Time{}, now)
		want := now.Add(glmRateLimitFallbackCooldown)
		if !got.Equal(want) {
			t.Errorf("1308 fallback cooldown = %v, want %v", got, want)
		}
	})

	t.Run("unknown_code_falls_back_to_now_plus_6s", func(t *testing.T) {
		// 2026-08-14 事故回归：智谱误报 1311（未知 code）曾走 5h 兜底导致全池静默 5 小时。
		got := glmCooldownForCode("1311", time.Time{}, now)
		want := now.Add(glmRateLimitFallbackCooldown)
		if !got.Equal(want) {
			t.Errorf("unknown code fallback = %v, want %v", got, want)
		}
		if !want.Equal(now.Add(6 * time.Second)) {
			t.Errorf("fallback cooldown should be 6s, got %v", glmRateLimitFallbackCooldown)
		}
	})
}

func TestParseGLMResetTime(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantOk  bool
		wantStr string // CST time string
	}{
		{
			name:    "standard format with CST reset time",
			body:    "您的限额将在 2026-06-16 16:34:31 重置",
			wantOk:  true,
			wantStr: "2026-06-16 16:34:31",
		},
		{
			name:   "no reset time in message",
			body:   "已达到 5 小时的使用上限",
			wantOk: false,
		},
		{
			name:   "empty body",
			body:   "",
			wantOk: false,
		},
	}
	cst := time.FixedZone("CST", 8*3600)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseGLMResetTime([]byte(tc.body))
			if tc.wantOk {
				if got.IsZero() {
					t.Fatalf("expected non-zero time, got zero")
				}
				if got.Format("2006-01-02 15:04:05") != tc.wantStr {
					t.Errorf("got %v, want %s", got.Format("2006-01-02 15:04:05"), tc.wantStr)
				}
				_, off := got.In(cst).Zone()
				if off != 8*3600 {
					t.Errorf("expected CST (+0800), got offset %d", off)
				}
			} else {
				if !got.IsZero() {
					t.Fatalf("expected zero time, got %v", got)
				}
			}
		})
	}
}

func TestGLMRateLimitCooldownEnd(t *testing.T) {
	now := time.Date(2026, 6, 16, 3, 0, 0, 0, time.UTC) // 11:00 CST
	cst := time.FixedZone("CST", 8*3600)

	t.Run("uses parsed reset time when valid and future", func(t *testing.T) {
		resetAt := time.Date(2026, 6, 16, 16, 34, 31, 0, cst)
		got := glmRateLimitCooldownEnd(resetAt, now)
		if !got.Equal(resetAt) {
			t.Errorf("expected resetAt used, got %v", got)
		}
	})

	t.Run("falls back to now+6s when reset time is zero", func(t *testing.T) {
		got := glmRateLimitCooldownEnd(time.Time{}, now)
		want := now.Add(6 * time.Second)
		if !got.Equal(want) {
			t.Errorf("expected now+6s fallback, got %v want %v", got, want)
		}
	})

	t.Run("falls back to now+6s when reset time is in the past", func(t *testing.T) {
		pastReset := time.Date(2026, 6, 15, 0, 0, 0, 0, cst)
		got := glmRateLimitCooldownEnd(pastReset, now)
		want := now.Add(6 * time.Second)
		if !got.Equal(want) {
			t.Errorf("expected now+6s fallback for past reset, got %v want %v", got, want)
		}
	})
}

// TestParse429Body 验证智谱 HTTP 429 body 解析（按 API-CODE 文档）。
func TestParse429Body(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode string
		wantTime bool
	}{
		{
			name:     "1308_5h_quota",
			body:     `{"error":{"code":"1308","message":"[1308][已达到 5 小时的使用上限。您的限额将在 2026-06-22 13:30:50 重置。]"}}`,
			wantCode: "1308",
			wantTime: true,
		},
		{
			name:     "1310_weekly_quota",
			body:     `{"error":{"code":"1310","message":"[1310][您已达到每周/每月使用上限，您的限额将在 2026-06-25 10:00:00 重置。]"}}`,
			wantCode: "1310",
			wantTime: true,
		},
		{
			name:     "1302_concurrency",
			body:     `{"error":{"code":"1302","message":"您当前使用该 API 的并发数过高，请降低并发，或联系客服增加限额"}}`,
			wantCode: "1302",
			wantTime: false,
		},
		{
			name:     "1303_rpm",
			body:     `{"error":{"code":"1303","message":"您当前使用该 API 的频率过高，请降低频率，或联系客服增加限额"}}`,
			wantCode: "1303",
			wantTime: false,
		},
		{
			name:     "unknown_code",
			body:     `{"error":{"code":"9999","message":"未知错误"}}`,
			wantCode: "9999",
			wantTime: false,
		},
		{
			name:     "empty_body",
			body:     "",
			wantCode: "",
			wantTime: false,
		},
		{
			name:     "invalid_json",
			body:     `{"error":invalid}`,
			wantCode: "",
			wantTime: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, resetAt := parse429Body([]byte(tc.body))
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
			if tc.wantTime && resetAt.IsZero() {
				t.Error("expected non-zero reset time")
			}
			if !tc.wantTime && !resetAt.IsZero() {
				t.Errorf("expected zero reset time, got %v", resetAt)
			}
		})
	}
}

// TestMarkRateLimitedWithBody 验证按智谱 429 body code 决定冷却时长 + 保留 GLM reason 语义。
func TestMarkRateLimitedWithBody(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0"})
	now := time.Now()

	// 1308（5h 额度限流）：用 message 里的重置时间
	body1308 := []byte(`{"error":{"code":"1308","message":"将在 2099-01-01 00:00:00 重置"}}`)
	pool.markRateLimitedWithBody(0, body1308)
	if !pool.entries[0].exhausted {
		t.Error("expected key-0 exhausted after 1308")
	}
	if pool.entries[0].exhaustReason != glmExhaustReason {
		t.Errorf("1308 reason = %q, want %q (GLM 时限额语义)", pool.entries[0].exhaustReason, glmExhaustReason)
	}
	// cooldown_end 应该 > now + 1h（远未来重置时间）
	if pool.entries[0].cooldownEnd.Before(now.Add(time.Hour)) {
		t.Errorf("1308 cooldown should be far future, got %v", pool.entries[0].cooldownEnd)
	}

	// 恢复
	pool.entries[0].exhausted = false
	pool.entries[0].exhaustReason = ""
	pool.entries[0].cooldownEnd = time.Time{}

	// 1310（周/月额度限流）
	body1310 := []byte(`{"error":{"code":"1310","message":"将在 2099-01-01 00:00:00 重置"}}`)
	pool.markRateLimitedWithBody(0, body1310)
	if pool.entries[0].exhaustReason != glmRateLimitQuota {
		t.Errorf("1310 reason = %q, want %q (GLM 周限额语义)", pool.entries[0].exhaustReason, glmRateLimitQuota)
	}

	// 恢复
	pool.entries[0].exhausted = false
	pool.entries[0].exhaustReason = ""
	pool.entries[0].cooldownEnd = time.Time{}

	// 1302（并发/频率限流）：60s 固定
	body1302 := []byte(`{"error":{"code":"1302","message":"并发过高"}}`)
	pool.markRateLimitedWithBody(0, body1302)
	if pool.entries[0].exhaustReason != glmRateLimitQPS {
		t.Errorf("1302 reason = %q, want %q (GLM QPS 语义)", pool.entries[0].exhaustReason, glmRateLimitQPS)
	}
	cd := pool.entries[0].cooldownEnd.Sub(now)
	if cd < 59*time.Second || cd > 61*time.Second {
		t.Errorf("1302 cooldown should be ~60s, got %v", cd)
	}

	// 恢复
	pool.entries[0].exhausted = false
	pool.entries[0].exhaustReason = ""
	pool.entries[0].cooldownEnd = time.Time{}

	// 1303（并发限流）：与 1302 同属瞬时频率类，reason + cooldown 必须与 1302 一致
	body1303 := []byte(`{"error":{"code":"1303","message":"您当前使用该 API 的频率过高"}}`)
	pool.markRateLimitedWithBody(0, body1303)
	if pool.entries[0].exhaustReason != glmRateLimitQPS {
		t.Errorf("1303 reason = %q, want %q (与 1302 同属 QPS 类)", pool.entries[0].exhaustReason, glmRateLimitQPS)
	}
	cd1303 := pool.entries[0].cooldownEnd.Sub(now)
	if cd1303 < 59*time.Second || cd1303 > 61*time.Second {
		t.Errorf("1303 cooldown should be ~60s (与 1302/429 路径对齐), got %v", cd1303)
	}

	// 恢复
	pool.entries[0].exhausted = false
	pool.entries[0].exhaustReason = ""
	pool.entries[0].cooldownEnd = time.Time{}
	pool.entries[0].consecutiveRateLimit = 0

	// 空 body（解析失败）：退化为通用 rate_limited + 指数退避首次 5s
	pool.markRateLimitedWithBody(0, []byte{})
	if pool.entries[0].exhaustReason != "rate_limited" {
		t.Errorf("empty body reason = %q, want %q (非 GLM 退化为通用)", pool.entries[0].exhaustReason, "rate_limited")
	}
	cd = pool.entries[0].cooldownEnd.Sub(now)
	if cd < 4*time.Second || cd > 6*time.Second {
		t.Errorf("empty body cooldown should be ~5s (exponential backoff), got %v", cd)
	}
}

// TestMarkRateLimitedWithBody_NonGLMReasonPreserved 验证非智谱格式的 429 body 不会被误判为 GLM。
func TestMarkRateLimitedWithBody_NonGLMReasonPreserved(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0"})
	// OpenAI 风格 429：{"error":{"message":"...","type":"rate_limit_exceeded"}} 无 code 字段
	bodyOpenAI := []byte(`{"error":{"message":"Rate limit exceeded","type":"rate_limit_exceeded"}}`)
	pool.markRateLimitedWithBody(0, bodyOpenAI)
	if pool.entries[0].exhaustReason != "rate_limited" {
		t.Errorf("non-GLM body reason = %q, want rate_limited", pool.entries[0].exhaustReason)
	}
}

// TestMarkRateLimitedWithBody_UnknownGLMCode_FallbackCooldown 守护 Notable #1：
// 智谱未知 code（非空但不在 1308/1310/1302/1303 列表，如 1311 套餐模型权限误报）在 429 路径
// 必须走 glmRateLimitFallbackCooldown（6s）短冷却兜底（与 SSE 路径 glmCooldownForCode else
// 分支对齐），而不是指数退避。2026-08-14 事故：旧 5h 兜底 + 智谱瞬时误报 1311 导致全池
// key 静默冷却 5 小时，人工解锁才恢复。
// 同时验证非智谱（code 为空）仍走指数退避。
func TestMarkRateLimitedWithBody_UnknownGLMCode_FallbackCooldown(t *testing.T) {
	now := time.Now()

	// 1. 智谱未知 code "9999"：reason = glmRateLimitUnknown，cooldown ~6s
	pool := newTestKeyPool([]string{"key-0"})
	bodyUnknown := []byte(`{"error":{"code":"9999","message":"未知错误"}}`)
	pool.markRateLimitedWithBody(0, bodyUnknown)
	if pool.entries[0].exhaustReason != glmRateLimitUnknown {
		t.Errorf("9999 reason = %q, want %q", pool.entries[0].exhaustReason, glmRateLimitUnknown)
	}
	cd := pool.entries[0].cooldownEnd.Sub(now)
	// 期望 ~6s（glmRateLimitFallbackCooldown）；放宽到 [5s, 7s] 容纳测试运行耗时
	if cd < 5*time.Second || cd > 7*time.Second {
		t.Errorf("9999 cooldown should be ~6s (aligned with SSE path), got %v", cd)
	}
	// 走兜底分支时不应递增 consecutiveRateLimit（与 SSE 路径行为一致）
	if pool.entries[0].consecutiveRateLimit != 0 {
		t.Errorf("9999 path should NOT increment consecutiveRateLimit, got %d",
			pool.entries[0].consecutiveRateLimit)
	}

	// 2. 对照：非智谱（code 为空，OpenAI 风格）仍走指数退避 ~5s
	pool2 := newTestKeyPool([]string{"key-0"})
	bodyNonGLM := []byte(`{"error":{"message":"Rate limit exceeded","type":"rate_limit_exceeded"}}`)
	pool2.markRateLimitedWithBody(0, bodyNonGLM)
	if pool2.entries[0].exhaustReason != "rate_limited" {
		t.Errorf("non-GLM reason = %q, want rate_limited", pool2.entries[0].exhaustReason)
	}
	cd2 := pool2.entries[0].cooldownEnd.Sub(now)
	if cd2 < 4*time.Second || cd2 > 6*time.Second {
		t.Errorf("non-GLM cooldown should be ~5s (exponential backoff), got %v", cd2)
	}
	// 指数退避分支应递增 consecutiveRateLimit
	if pool2.entries[0].consecutiveRateLimit != 1 {
		t.Errorf("non-GLM path should increment consecutiveRateLimit to 1, got %d",
			pool2.entries[0].consecutiveRateLimit)
	}
}

// TestGLMRateLimitBodyContainsChecks 验证三重匹配特征常量的正确性，防止有人误改常量值。
func TestGLMRateLimitBodyContainsChecks(t *testing.T) {
	for _, body := range [][]byte{realGLMRateLimitBody, realGLMQuotaRateLimitBody, realGLMQPSRateLimitBody} {
		bodyStr := string(body)
		if !strings.Contains(bodyStr, "event: error") {
			t.Fatal("test body must contain 'event: error'")
		}
		lower := strings.ToLower(bodyStr)
		if !strings.Contains(lower, strings.ToLower(glmRateLimitType)) {
			t.Fatalf("test body must contain %s", glmRateLimitType)
		}
		if !glmCodeRe.Match(body) {
			t.Fatal("test body must contain a code matched by glmCodeRe")
		}
	}
}

// TestGLMReasonForCode_1305 守护 1305 服务过载被正确归类为瞬时类（非额度类）。
// fix-glm-ratelimit G1：1305 落入 default 会让 key 误判为 5h 额度限流。
func TestGLMReasonForCode_1305(t *testing.T) {
	if got := glmReasonForCode("1305"); got != glmRateLimitOverload {
		t.Fatalf("glmReasonForCode(\"1305\") = %q, want %q", got, glmRateLimitOverload)
	}
	// 同时验证 isTransientGLMReason 对 1305 返回 true
	if !isTransientGLMReason(glmRateLimitOverload) {
		t.Fatal("isTransientGLMReason(glmRateLimitOverload) = false, want true")
	}
	// 同时验证非瞬时类（额度）返回 false
	if isTransientGLMReason(glmExhaustReason) {
		t.Fatal("isTransientGLMReason(glmExhaustReason) = true, want false")
	}
	if isTransientGLMReason(glmRateLimitQuota) {
		t.Fatal("isTransientGLMReason(glmRateLimitQuota) = true, want false")
	}
	if isTransientGLMReason(glmRateLimitUnknown) {
		t.Fatal("isTransientGLMReason(glmRateLimitUnknown) = true, want false")
	}
}

// TestGLMCooldownForCode_1305 守护 1305 使用 60s 短冷却（与 1302/1303 同级），而非 5h fallback。
func TestGLMCooldownForCode_1305(t *testing.T) {
	now := time.Now()
	want := now.Add(glmQPSCooldown)
	got := glmCooldownForCode("1305", time.Time{}, now)
	diff := got.Sub(want)
	if diff < 0 {
		diff = -diff
	}
	if diff > time.Second {
		t.Fatalf("glmCooldownForCode(\"1305\") = %v, want ~%v (diff=%v)", got, want, diff)
	}
	// 确认 1305 和 1302 产出相同冷却时长
	got1302 := glmCooldownForCode("1302", time.Time{}, now)
	if got.Sub(got1302) > time.Millisecond || got1302.Sub(got) > time.Millisecond {
		t.Fatalf("1305 and 1302 should have same cooldown, got %v vs %v", got, got1302)
	}
}

// TestGLMResetTimeRe_DualFormat 守护正则同时匹配 HH:MM（无秒）和 HH:MM:SS（有秒）两种格式。
// fix-glm-ratelimit G2：智谱 1310 周限额 message 实测用 "HH:MM"，正则放宽后不能丢失有秒格式。
func TestGLMResetTimeRe_DualFormat(t *testing.T) {
	cases := []struct {
		body    string
		want    string
		matched bool
	}{
		{`将在 2026-06-16 16:34:31 重置`, "2026-06-16 16:34:31", true},
		{`将在 2026-06-22 13:30 重置`, "2026-06-22 13:30", true},
		{`将在 2099-12-31 23:59:59 重置`, "2099-12-31 23:59:59", true},
		{`没有时间`, "", false},
		{`2026-06-22T13:30:00Z`, "", false}, // ISO 格式不匹配
	}
	for _, c := range cases {
		m := glmResetTimeRe.FindStringSubmatch(c.body)
		if c.matched {
			if len(m) < 2 {
				t.Errorf("body %q should match but did not", c.body)
				continue
			}
			if m[1] != c.want {
				t.Errorf("body %q extracted %q, want %q", c.body, m[1], c.want)
			}
		} else {
			if len(m) >= 1 {
				t.Errorf("body %q should NOT match but got %q", c.body, m[0])
			}
		}
	}
}

// TestParseGLMResetTime_DualLayout 守护 parseGLMResetTime 双 layout 尝试。
// fix-glm-ratelimit G2：只改正则不改为双 layout 会让无秒格式仍被 "15:04:05" 解析失败走 5h fallback。
func TestParseGLMResetTime_DualLayout(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want string // "2026-06-22T13:30:00+08:00" 等
		zero bool
	}{
		{
			name: "有秒（1308 5h 额度格式）",
			body: []byte(`[1308][已达到 5 小时的使用上限。您的限额将在 2026-06-16 16:34:31 重置。]`),
			want: "2026-06-16 16:34:31",
		},
		{
			name: "无秒（1310 周限额格式）",
			body: []byte(`[1310][您已达到每周/每月使用上限。您的限额将在 2026-06-22 13:30 重置。]`),
			want: "2026-06-22 13:30",
		},
		{
			name: "无重置时间",
			body: []byte(`[1302][速率超限]`),
			zero: true,
		},
		{
			name: "空 body",
			body: []byte{},
			zero: true,
		},
	}
	cst := time.FixedZone("CST", 8*3600)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseGLMResetTime(c.body)
			if c.zero {
				if !got.IsZero() {
					t.Fatalf("should be zero time, got %v", got)
				}
				return
			}
			if got.IsZero() {
				t.Fatal("parse failed, got zero time")
			}
			// 验证时区为 CST
			if got.Location().String() != "CST" && got.Location() != cst {
				t.Errorf("time zone = %v, want CST", got.Location())
			}
			// 验证时间字符串（CST 下）与预期一致
			gotStr := got.In(cst).Format("2006-01-02 15:04:05")
			if !strings.HasPrefix(gotStr, c.want[:16]) {
				t.Errorf("parsed time = %q, want prefix %q", gotStr, c.want[:16])
			}
		})
	}
}
