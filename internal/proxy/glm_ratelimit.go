// glm_ratelimit.go — 智谱 GLM（Claude 兼容端点 /api/anthropic）的 5 小时限流检测。
//
// 背景：智谱 TokenPlan 的 5 小时使用上限（error.code = "1308"）返回的是 HTTP 200 +
// SSE 流，错误藏在 SSE event 里，而不是标准的 HTTP 429。现有 isKeyExhausted 只检查
// 非二响应，完全识别不到这种限流，导致 key 不被标记耗尽、不切换、不重新负载。
//
// 真实限流响应样例（~344 bytes）：
//
//	event: error
//	data: {"type":"error","error":{"type":"rate_limit_error","code":"1308","message":"[1308][已达到 5 小时的使用上限。您的限额将在 2026-06-16 16:34:31 重置。][...]"},"request_id":"..."}
//
//	data: [DONE]
//
// 本文件提供：SSE 限流识别 + message 文本中的重置时间解析（CST 北京时间）。
package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"time"
)

// 历史接入曾观察到智谱 GLM（Claude 兼容端点）的限流以 HTTP 200 + SSE
// event:error + rate_limit_error 形式返回。当前官方错误码文档使用 HTTP 429；
// 此历史兼容仅在已匹配的智谱平台/模型或显式适配配置下启用，不能泛化到其他上游。
// 重置时间藏在 SSE message 文本里。实测三种 code（测试环境 trace 验证）：
//   1308 = 5 小时使用上限（额度类，message 含精确重置时刻）
//   1310 = 每周/每月使用上限（额度类，message 含精确重置时刻）
//   1302 = 速率/QPS 限制（瞬时类，message 无重置时间）
// 其他未知 code（如 1311 套餐模型权限误报）走 6s 短冷却兜底（2026-08-14 起从 5h 收紧）。

// glmRateLimitType 是 SSE error 事件的 type 标识。
const glmRateLimitType = `"type":"rate_limit_error"`

// glmCodeRe 从 SSE error body 提取业务错误码（任意 1~4 位数字）。
var glmCodeRe = regexp.MustCompile(`"code":"(\d{1,4})"`)

// GLM 限流 keypool exhaustReason 取值，按 code 分类（用于 isHardFailureReason 与冷却策略）。
// 冷却到期后 recoverFromCooldownLocked 自动恢复 key，无需人工干预。
const (
	glmExhaustReason     = "glm_rate_limit_5h"       // 1308：5 小时额度
	glmRateLimitQuota    = "glm_rate_limit_quota"    // 1310：周/月额度
	glmRateLimitQPS      = "glm_rate_limit_qps"      // 1302：速率/QPS
	glmRateLimitOverload = "glm_rate_limit_overload" // 1305：平台服务过载（瞬时类，60s 冷却）
	glmRateLimitUnknown  = "glm_rate_limit_unknown"  // 未知 code 兜底（6s 短冷却，不按额度类 5h 处理）
)

// 智谱账户级失效码（#159 F5）：key 本身不可用，冷却等待无意义，永久屏蔽；属于账户级
// 硬失败，不归入 GLM 限流类。不用 glm_ 前缀：管理端按该前缀归入"GLM 限流"芯片，
// 这两类应计入"失效"。智谱 /models 探测不可靠，管理端唤醒直接恢复（见 WakeUpKeys）。
const (
	glmAccountArrears = "zhipu_arrears"      // 1113：账户欠费
	glmPlanExpired    = "zhipu_plan_expired" // 1309：GLM Coding Plan 套餐到期
)

// glmRateLimitFallbackCooldown 是重置时间解析失败/未知 code 时的兜底冷却时长。
// 2026-08-14 事故：智谱瞬时误报未知 code（1311 "套餐未开放模型权限"，实际套餐正常，
// 同批 key 2 分钟后即返回真实 200），旧 5h 兜底把全部 key 静默冷却 5 小时。
// 改为 6s 短冷却静默探测：误报场景快速自愈；真实额度耗尽（1308/1310）通常能解析出
// 精确重置时间，不依赖本兜底。
const glmRateLimitFallbackCooldown = 6 * time.Second

// glmQPSCooldown 是速率/QPS 限流（1302）的短冷却时长。
// QPS 限流是瞬时的，message 无重置时间；5 小时太长、5 秒又会打转，取 60 秒。
const glmQPSCooldown = 60 * time.Second

// glmResetTimeRe 匹配 message 文本里的重置时间，格式 "2026-06-16 16:34:31"（CST）。
// 秒部分可选：智谱 1310 周限额 message 实测用 "HH:MM"（无秒），1308 5h 额度用 "HH:MM:SS"（有秒）。
var glmResetTimeRe = regexp.MustCompile(`(\d{4}-\d{2}-\d{2} \d{2}:\d{2}(?::\d{2})?)`)

// isGLMRateLimitReason 判断某个 exhaustReason 是否为 GLM 限流类（5h 时限额 / 1310 周限额 /
// 1302 QPS / 1305 服务过载 / unknown 兜底）。用于 healthz metrics 聚合：GLM 各类限流本质都是
// "冷却后恢复的限流"，统一计入 rateLimitedKeys，与通用 rate_limited 同类，避免错落到 otherExhaustedKeys。
func isGLMRateLimitReason(reason string) bool {
	switch reason {
	case glmExhaustReason, glmRateLimitQuota, glmRateLimitQPS, glmRateLimitOverload, glmRateLimitUnknown:
		return true
	}
	return false
}

// isTransientGLMReason 判断某个 GLM 限流 reason 是否为瞬时类（冷却到期或手动唤醒后大概率能立刻恢复）。
// 瞬时类：1302/1303 (glmRateLimitQPS) 和 1305 (glmRateLimitOverload)。
// 额度类（1308/1310/unknown）是"非瞬时"——强制复活会立刻再撞限导致"唤醒按钮闪一下又灰"。
// 用于：重试策略（RETRY<2 同 key 坚持）和唤醒按钮（直接 markRecovered 跳过 /models 探测）。
func isTransientGLMReason(reason string) bool {
	return reason == glmRateLimitQPS || reason == glmRateLimitOverload
}

// sameKeyRetryable429Reason 判断 HTTP 429 后 key 的耗尽 reason 是否允许 WP-3 同 key 有限重试。
// 只有瞬时限流可坚持同 key：通用 rate_limited、GLM 瞬时类（1302/1303/1305），以及
// 并发洗白导致的空 reason（安全默认，保持原行为）。其余（GLM 额度类、余额/额度耗尽、
// 账户级失效）跳过同 key 重试直接换 key（#159）。
func sameKeyRetryable429Reason(reason string) bool {
	return reason == "" || reason == "rate_limited" || isTransientGLMReason(reason)
}

// detectGLMRateLimitInBody 扫描 SSE 响应体，判断是否为智谱 GLM 限流。
//
// 匹配条件（避免误判正常响应）：
//  1. 响应体包含 SSE error 事件标记 `event: error`（或 `event:error`）
//  2. error.type == "rate_limit_error"
//  3. 正则提取 error.code（任意 1~4 位数字）
//
// 返回值：
//   - limited: 是否判定为 GLM 限流
//   - code: 从 body 提取的业务错误码（如 "1308"/"1310"/"1302"）；未提取到返回空串，调用方按 unknown 处理
//   - resetAt: 从 message 文本解析到的重置时间（CST，仅额度类 1308/1310 有）。解析失败返回零值。
//
// 仅对 HTTP 200 + SSE 响应调用本函数（调用方负责前置过滤），避免对每个响应都做扫描。
func detectGLMRateLimitInBody(body []byte) (limited bool, code string, resetAt time.Time) {
	if len(body) == 0 {
		return false, "", time.Time{}
	}
	// 正常的 SSE 流式响应（message_start / content_block_delta 等）不会包含这三者的组合。
	if !bytes.Contains(body, []byte("event: error")) &&
		!bytes.Contains(body, []byte("event:error")) {
		return false, "", time.Time{}
	}
	if !bytes.Contains(body, []byte(glmRateLimitType)) {
		return false, "", time.Time{}
	}

	// 提取业务错误码（不限定具体值，1308/1310/1302/未知均识别）。
	if m := glmCodeRe.FindSubmatch(body); len(m) >= 2 {
		code = string(m[1])
	}

	// 命中限流，尝试从 message 文本里提取重置时间（额度类 1308/1310 有，速率类 1302 无）。
	resetAt = parseGLMResetTime(body)
	return true, code, resetAt
}

// glmReasonForCode 把业务错误码映射成 keypool exhaustReason。
// 1308 → 5h 额度；1310 → 周/月额度；1302/1303 → 速率/并发（瞬时频率类，60s 冷却）；
// 1305 → 服务过载（瞬时类，60s 冷却）；其余 → unknown（保守按额度类处理）。
func glmReasonForCode(code string) string {
	switch code {
	case "1308":
		return glmExhaustReason
	case "1310":
		return glmRateLimitQuota
	case "1302", "1303":
		return glmRateLimitQPS
	case "1305":
		return glmRateLimitOverload
	case "1113":
		return glmAccountArrears
	case "1309":
		return glmPlanExpired
	default:
		return glmRateLimitUnknown
	}
}

// isGLMAccountLevelCode 判断智谱业务码是否表示 key 所属账户不可用（欠费 1113、套餐到期 1309）。
// 这类 key 冷却等待无意义：永久屏蔽，待管理端唤醒或真实请求成功后恢复。
func isGLMAccountLevelCode(code string) bool {
	return code == "1113" || code == "1309"
}

// isGLMAccountLevelReason 判断 reason 是否为智谱账户级失效（1113/1309）。
func isGLMAccountLevelReason(reason string) bool {
	return reason == glmAccountArrears || reason == glmPlanExpired
}

// glmCooldownForCode 按 code 计算限流 key 的冷却结束时间。
//   - 额度类（1308/1310/未知）：优先 message 里的精确重置时间；解析失败 fallback now+6s。
//   - 速率/并发类（1302/1303）+ 服务过载（1305）：固定 now+60s（瞬时类，无/不依赖重置时间）。
//
// 注意：1303（并发限流）与 1302（QPS）同属瞬时频率类，此处与 markRateLimitedWithBody
// 的 429 路径 cooldown（case "1302","1303","1305" → 60s）保持一致，避免双路径分类冲突。
func glmCooldownForCode(code string, resetAt, now time.Time) time.Time {
	if code == "1302" || code == "1303" || code == "1305" {
		return now.Add(glmQPSCooldown)
	}
	if isGLMAccountLevelCode(code) {
		return permanentCooldownEnd
	}
	// 额度类（含未知 code 兜底）：复用现有 fallback 策略。
	return glmRateLimitCooldownEnd(resetAt, now)
}

// parseGLMResetTime 从限流 message 文本中提取重置时间，解析为 CST 时区的 time.Time。
// message 格式示例："[1308][已达到 5 小时的使用上限。您的限额将在 2026-06-16 16:34:31 重置。][...]"
// 双 layout 尝试：先 "2006-01-02 15:04:05"（有秒），失败后退 "2006-01-02 15:04"（无秒，智谱 1310 实测格式）。
// 解析失败返回零值（调用方应 fallback 到 now + 6s）。
func parseGLMResetTime(body []byte) time.Time {
	m := glmResetTimeRe.FindSubmatch(body)
	if len(m) < 2 {
		return time.Time{}
	}
	cst := time.FixedZone("CST", 8*3600)
	s := string(m[1])
	// 先尝试有秒格式
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, cst); err == nil {
		return t
	}
	// 退化为无秒格式（智谱 1310 周限额 message 实测 "HH:MM"）
	if t, err := time.ParseInLocation("2006-01-02 15:04", s, cst); err == nil {
		return t
	}
	return time.Time{}
}

// glmRateLimitCooldownEnd 计算限流 key 的冷却结束时间。
// 优先使用响应里解析到的精确重置时间；解析失败时 fallback 到 now + 6s（见 glmRateLimitFallbackCooldown）。
func glmRateLimitCooldownEnd(resetAt, now time.Time) time.Time {
	if !resetAt.IsZero() && resetAt.After(now) {
		return resetAt
	}
	return now.Add(glmRateLimitFallbackCooldown)
}

// glm429Response 表示智谱 HTTP 429 响应的 JSON 结构。
type glm429Response struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// parse429Body 解析智谱 HTTP 429 响应 body，提取业务错误码和重置时间。
// 智谱 429 body 格式：{"error":{"code":"1308","message":"...将在 2026-06-22 13:30 重置..."}}
// 返回 (code, resetAt)：
//   - code：业务错误码（1302/1303/1308/1310 等），解析失败返回空串
//   - resetAt：1308/1310 从 message 提取的重置时间（CST），其他返回零值
func parse429Body(body []byte) (code string, resetAt time.Time) {
	if len(body) == 0 {
		return "", time.Time{}
	}
	var resp glm429Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", time.Time{}
	}
	code = resp.Error.Code
	// 1308/1310 的 message 含重置时间
	if code == "1308" || code == "1310" {
		resetAt = parseGLMResetTime([]byte(resp.Error.Message))
	}
	return code, resetAt
}

// peekGLMFirstEvent waits only for the first data event, so a healthy stream is
// available immediately. Comments/heartbeats are skipped for error detection;
// every consumed and buffered byte is replayed unchanged to the caller.
func peekGLMFirstEvent(body io.Reader) ([]byte, io.Reader) {
	reader := bufio.NewReader(io.LimitReader(body, 4096))
	var prefix, line []byte
	hasData, afterCR := false, false
	for len(prefix) < 4096 {
		b, err := reader.ReadByte()
		if err != nil {
			break
		}
		prefix = append(prefix, b)
		if afterCR && b == '\n' {
			afterCR = false
			continue
		}
		afterCR = b == '\r'
		if b == '\n' || b == '\r' {
			if len(line) == 0 && hasData {
				break
			}
			if bytes.HasPrefix(line, []byte("data:")) || bytes.Equal(line, []byte("data")) {
				hasData = true
			}
			line = line[:0]
		} else {
			line = append(line, b)
		}
	}
	return prefix, io.MultiReader(bytes.NewReader(prefix), reader, body)
}
