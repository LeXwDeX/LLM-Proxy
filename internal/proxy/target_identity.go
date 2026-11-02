// target_identity.go — 目标“上游连接身份”判定与熔断状态的代际分叉（#184）。
package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"math/big"
	"reflect"
	"sort"
	"strings"
)

// forkForNewIdentity 返回供新配置代际使用的熔断状态：累计计数与观测时间点复制，
// 冷却（mutedUntil）、连续失败与探针租约清零。旧状态对象保持原样，仍由旧代际
// 的在途请求结算，迟到结果不会回写新状态。
// 调用方持有 Service.mu 写锁；这里只读取旧状态（锁顺序 Service.mu → health.mu），
// 且不按值复制含 mutex/atomic 的结构。
func (h *targetHealthState) forkForNewIdentity() *targetHealthState {
	fresh := &targetHealthState{}
	h.mu.RLock()
	fresh.lastSuccess = h.lastSuccess
	fresh.lastFailure = h.lastFailure
	fresh.totalSuccess = h.totalSuccess
	fresh.totalFailure = h.totalFailure
	fresh.breakerThreshold = h.breakerThreshold
	// MarkFailure updates totalFailure and breakerTrips together under mu;
	// copy both before releasing it so the new identity sees one snapshot.
	fresh.affinityHits.Store(h.affinityHits.Load())
	fresh.breakerTrips.Store(h.breakerTrips.Load())
	h.mu.RUnlock()
	return fresh
}

// targetIdentityChanges 返回两个已归一化目标之间发生变化的“上游连接身份”字段名
// （使用配置/管理端的 JSON 字段名）。身份字段决定请求发往哪个上游、用哪套凭据与
// 认证方式，或改写上游模型名；这些变化使旧的熔断结论不再适用于新配置。
// usage_adapter、model_mappings、max_concurrency、pool、key_reset_time、
// supports_responses、image_operation 等不改变连接身份，不在此列。
// 使用显式字段比较；新增 Target 字段须在 TestHealthIdentityFieldClassificationExhaustive
// 的分类表中归类，属于身份的须同步加入此处。
func targetIdentityChanges(old, next *Target) []string {
	if old == nil || next == nil {
		return nil
	}
	var changed []string
	if old.EndpointType != next.EndpointType {
		changed = append(changed, "endpoint_type")
	}
	if endpointString(old) != endpointString(next) {
		changed = append(changed, "endpoint")
	}
	if old.AuthMode != next.AuthMode {
		changed = append(changed, "auth_mode")
	}
	if old.AllowBearer != next.AllowBearer {
		changed = append(changed, "allow_bearer_passthrough")
	}
	if !sameCredentialSet(old, next) {
		changed = append(changed, "credentials")
	}
	if !sameStringMap(old.CustomHeaders, next.CustomHeaders) {
		changed = append(changed, "custom_headers")
	}
	if !sameAnyMap(old.CustomBody, next.CustomBody) {
		changed = append(changed, "custom_body")
	}
	if old.ModelPrefix != next.ModelPrefix {
		changed = append(changed, "model_prefix")
	}
	if normalizePrefix(old.OpenAIPrefix) != normalizePrefix(next.OpenAIPrefix) {
		changed = append(changed, "openai_prefix")
	}
	if normalizePrefix(old.AnthropicPrefix) != normalizePrefix(next.AnthropicPrefix) {
		changed = append(changed, "anthropic_prefix")
	}
	if old.PreferWebSocket != next.PreferWebSocket {
		changed = append(changed, "prefer_websocket")
	}
	return changed
}

func endpointString(t *Target) string {
	if t.Endpoint == nil {
		return ""
	}
	return t.Endpoint.String()
}

// sameCredentialSet 比较合并去重后的凭据集合。仅重排不算变化；替换、新增、删除都算。
func sameCredentialSet(a, b *Target) bool {
	if len(a.APIKeys) != len(b.APIKeys) {
		return false
	}
	set := make(map[string]struct{}, len(a.APIKeys))
	for _, k := range a.APIKeys {
		set[k] = struct{}{}
	}
	for _, k := range b.APIKeys {
		if _, ok := set[k]; !ok {
			return false
		}
	}
	return true
}

func sameStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// sameAnyMap 按 JSON 语义比较自定义请求体，而不是比较 json.RawMessage 的字节。
// 等价边界：对象键顺序无关；数组顺序有关；字符串按解码后的值比较（转义拼写
// 无关，包括 HTML 转义；非法 UTF-8 与未配对代理项按 encoding/json 的行为解码
// 为 U+FFFD 后比较）；数字按精确十进制值比较（不经过 float64，0.7 与 0.70、
// 1e2 与 100 等价，任意精度整数不丢位）；重复键按 encoding/json 的后者覆盖前者
// 处理。无法编码或不是合法 JSON 的值退回 reflect.DeepEqual，保持原来的严格性。
func sameAnyMap(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !sameJSONValue(av, bv) {
			return false
		}
	}
	return true
}

func sameJSONValue(a, b any) bool {
	ac, aok := canonicalJSONValue(a)
	bc, bok := canonicalJSONValue(b)
	if aok && bok {
		return ac == bc
	}
	return reflect.DeepEqual(a, b)
}

func canonicalJSONValue(value any) (string, bool) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", false
	}
	return canonicalJSONBytes(data)
}

// canonicalJSONBytes 用 UseNumber 解析后生成规范形式，保证数字 token 不经过
// float64。json.Marshal 已会压缩空白并做 HTML 转义，这里再统一字符串转义与
// 对象键顺序，避免持久化、配置来源或编码器差异被误判为身份变化。解码时
// encoding/json 会把非法 UTF-8 与未配对代理项替换为 U+FFFD，对象重复键由 map
// 保留最后一个；这些也属于本比较的等价边界。
func canonicalJSONBytes(data []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return "", false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return "", false
	}
	var b strings.Builder
	writeCanonicalJSON(&b, value)
	return b.String(), true
}

func writeCanonicalJSON(b *strings.Builder, value any) {
	switch v := value.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if v {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writeCanonicalJSONString(b, v)
	case json.Number:
		// 与字符串形式区分，避免数字 1 与字符串 "1" 的规范形式相同。
		b.WriteString("n:")
		b.WriteString(canonicalJSONNumber(v.String()))
	case []any:
		b.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonicalJSON(b, item)
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonicalJSONString(b, k)
			b.WriteByte(':')
			writeCanonicalJSON(b, v[k])
		}
		b.WriteByte('}')
	default:
		// UseNumber 解析只会产生上面的类型；保留显式标记，避免未来新增类型
		// 与真实 JSON 值碰撞。
		b.WriteString("unexpected:")
		if t := reflect.TypeOf(value); t != nil {
			b.WriteString(t.String())
		}
	}
}

func writeCanonicalJSONString(b *strings.Builder, value string) {
	encoded, err := json.Marshal(value)
	if err != nil {
		b.WriteString(`"�"`)
		return
	}
	b.Write(encoded)
}

// canonicalJSONNumber 返回数字 token 的精确十进制规范形式：符号 + 有效数字 +
// 十进制指数。只做十进制移位与去零，不解析为浮点数，因此任意精度整数和超大
// 指数都不会丢位。
func canonicalJSONNumber(token string) string {
	original := token
	sign := ""
	if strings.HasPrefix(token, "-") {
		sign = "-"
		token = token[1:]
	}
	exp := new(big.Int)
	if i := strings.IndexAny(token, "eE"); i >= 0 {
		if _, ok := exp.SetString(token[i+1:], 10); !ok {
			return "invalid-number:" + original
		}
		token = token[:i]
	}
	intPart, fracPart := token, ""
	if i := strings.IndexByte(token, '.'); i >= 0 {
		intPart, fracPart = token[:i], token[i+1:]
	}
	digits := strings.TrimLeft(intPart+fracPart, "0")
	if digits == "" {
		return "0"
	}
	if trimmed := strings.TrimRight(digits, "0"); len(trimmed) != len(digits) {
		exp.Add(exp, big.NewInt(int64(len(digits)-len(trimmed))))
		digits = trimmed
	}
	exp.Sub(exp, big.NewInt(int64(len(fracPart))))
	return sign + digits + "e" + exp.String()
}

// shouldForkTargetHealth 判断同名目标新旧代际之间熔断状态是否须分叉，并给出原因。
// 触发：身份字段变化，或生效后的 Paused 由 true 变 false（目标 resume，或池 resume
// 经 applyPoolPause 物化）。pause 本身与其余编辑保持共享（#86）。
func shouldForkTargetHealth(old, next *Target) (reason string, fields []string, resumed bool) {
	fields = targetIdentityChanges(old, next)
	resumed = old != nil && next != nil && old.Paused && !next.Paused
	switch {
	case len(fields) > 0:
		return "identity_change", fields, resumed
	case resumed:
		return "resume", nil, true
	}
	return "", nil, false
}

// logTargetBreakerReset 记录一次熔断分叉。只含目标名、原因、字段名与此前相位，
// 不记录 key、header 或 body 的值。
func (s *Service) logTargetBreakerReset(name, reason string, fields []string, resumed bool, phase string, streak int) {
	if s.logger == nil {
		return
	}
	s.logger.Info("target breaker state reset for new configuration identity",
		"target", name,
		"reason", reason,
		"changed_fields", strings.Join(fields, ","),
		"also_resumed", resumed && reason != "resume",
		"previous_phase", phase,
		"previous_consecutive_failure", streak,
	)
}
