// keypool_probe.go owns exhausted-key recovery requests and eligibility.
package proxy

import (
	"context"
	"net/http"
	"net/url"
	"time"

	appmiddleware "github.com/LeXwDeX/LLM-Proxy/internal/middleware"
)

// wakeUpProbe 执行唤醒探测，遍历所有被屏蔽的 key 并尝试恢复。
// 返回第一个成功恢复的 key 索引，如果全部失败返回 -1。
func (s *Service) wakeUpProbe(r *http.Request, state *targetState) int {
	if state.keyPool == nil {
		return -1
	}

	target := state.Target()
	if target == nil {
		return -1
	}

	exhaustedKeys := state.keyPool.getProbeableExhaustedKeys()
	if len(exhaustedKeys) == 0 {
		// 没有可廉价探测的 key（全部为硬失败：无效/欠费/封禁/总额度耗尽）。
		// 这类 key 只能由冷却定时器被动恢复或真实请求成功确认，避免假阳性复活。
		return -1
	}

	s.logger.Info("[keypool] wake-up probe started",
		"request_id", appmiddleware.RequestIDFromContext(r.Context()),
		"target", target.Name,
		"exhausted_keys", len(exhaustedKeys),
	)

	// 对每个被屏蔽的 key 发送探测请求
	for _, idx := range exhaustedKeys {
		if s.probeKey(r.Context(), state, idx) {
			state.keyPool.markRecovered(idx)
			s.logger.Info("[keypool] wake-up probe succeeded",
				"request_id", appmiddleware.RequestIDFromContext(r.Context()),
				"target", target.Name,
				"key_index", idx,
			)
			return idx
		}
	}

	s.logger.Warn("[keypool] wake-up probe failed, all keys still exhausted",
		"request_id", appmiddleware.RequestIDFromContext(r.Context()),
		"target", target.Name,
	)
	return -1
}

// probeKey 对指定 key 发送轻量探测请求，验证 key 是否有效。
// 返回 true 表示 key 有效（探测成功），false 表示 key 仍无效。
func (s *Service) probeKey(ctx context.Context, state *targetState, keyIndex int) bool {
	target := state.Target()
	if target == nil || keyIndex < 0 || keyIndex >= len(state.keyPool.entries) {
		return false
	}

	// Read the entry under the same domain used by old and current generations.
	// Release it before HTTP work; callbacks must never enter Service under it.
	state.keyPool.mu.Lock()
	apiKey := state.keyPool.entries[keyIndex].key
	state.keyPool.mu.Unlock()

	// 构建探测请求 URL（GET /models 或 provider 根 URL）
	probeURL := s.buildProbeURL(target)
	if probeURL == "" {
		return false
	}

	// 创建探测请求（短超时 5 秒）
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	probeReq, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, nil)
	if err != nil {
		return false
	}

	strategy := s.resolveAuthStrategy(target, nil)
	if strategy == nil {
		strategy = &BearerAuth{} // 历史 unknown provider 探测默认
	}
	if err := strategy.InjectAuth(probeReq, apiKey, ""); err != nil {
		return false
	}

	// 发送探测请求
	resp, err := s.doHTTP(probeReq)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	// 判断探测结果：只有 HTTP 200 才算 key 真实有效。
	// 为什么不接受 404/405/2xx 以外：某些上游（如百炼 compatible-mode 的 /v1/models）
	// 对任意 key 都返回 200，或路径不匹配返回 404——这些都无法证明 key 有效，
	// 接受它们会导致"探测假阳性 → 复活无效 key → 真实请求立刻又失败"的死循环。
	// 因此探测从严：非 200 一律视为未恢复，硬失败 key 交由冷却定时器被动恢复。
	return resp.StatusCode == http.StatusOK
}

// buildProbeURL 根据 endpoint_type 构建探测请求 URL。
func (s *Service) buildProbeURL(target *Target) string {
	if target == nil || target.Endpoint == nil {
		return ""
	}

	registry := s.providerRegistry
	if registry == nil {
		if fallbackProviderRegistryError != nil {
			return ""
		}
		registry = fallbackProviderRegistry
	}
	profile := registry.Lookup(target.EndpointType)
	if profile == nil {
		return target.Endpoint.String() + "/v1/models"
	}
	if profile.probeUsesMappedPath {
		u, err := s.buildURL(target, &url.URL{Path: profile.probePath})
		if err != nil {
			return ""
		}
		return u.String()
	}
	return target.Endpoint.String() + profile.probePath
}
