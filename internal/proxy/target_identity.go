// target_identity.go — 目标“上游连接身份”判定与熔断状态的代际分叉（#184）。
package proxy

import (
	"reflect"
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

func sameAnyMap(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
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
