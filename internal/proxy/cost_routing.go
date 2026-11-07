package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// 成本感知分流（部署池 routing_strategy=cost）。
//
// 负载均衡与可用性始终优先，价格分流只是可尝试的偏好：
//   - 暂停、权限、模型、路径、熔断冷却、硬容量与探针在价格之前完成过滤，
//     价格只在剩余的可用候选中收窄范围，从不让请求因价格失败；
//   - 价表读取失败、未接入价表或最便宜层全部达到软上限时，退回现行负载均衡；
//   - 层内仍按现有归一化负载 + 存活 key 加权轮询分摊。
//
// 价格来自各 target 自己的计价模板（分时段、日期例外均在模板中声明），
// 代码不感知厂商、时段或节假日；新增峰谷价只需新增配置。
//
// 成本比较单位是客户端别名（请求中的 model）：
//   - 凡是能经自身 model_mappings 解析该请求名的池成员都参与比较（upstream
//     或 fallback 别名命中；paused 映射除外），无论各自的上游模型名如何；
//     成员经自己的映射把请求名解析成其上游名，再按
//     (target, 上游名, 请求时刻) 取价——与计费使用同一取价入口和键。
//     计费还按事件时间与模板 conditions 选价，因此选路价是请求时刻的估算。
//   - 以某个上游名直接请求时不做反向别名展开：一个上游名可能对应多个别名，
//     或在成员间含义不同；扩大候选会改变客户端指定的模型语义。该请求只比较
//     接受该上游名的成员。
//   - 计价模板只按上游名声明价格；别名价格不参与选路，否则选路价可能与被计费
//     按上游名落账的价格分叉。
const (
	// costTierTolerance：估算成本相差在该比例内视为同层，避免抖动并保留层内均衡。
	costTierTolerance = 0.05
	// 固定参考 token 构成（未命中输入 / 缓存命中输入 / 输出），使缓存价差异参与比较。
	costRefInputTokens  = 1_000_000
	costRefCachedTokens = 1_000_000
	costRefOutputTokens = 200_000
)

const (
	costGroupPriced   = iota // cost 池成员，且对该模型有价
	costGroupUnpriced        // cost 池成员，但查不到价格：排在有价成员之后
	costGroupOutside         // 不属于 cost 池：最后一层，仅作溢出兜底
)

type costRank struct {
	group int
	cost  float64
}

// within reports whether r falls in the tier anchored at best.
func (best costRank) within(r costRank) bool {
	if r.group != best.group {
		return false
	}
	return r.group != costGroupPriced || r.cost <= best.cost*(1+costTierTolerance)
}

func (r costRank) less(o costRank) bool {
	return r.group < o.group || (r.group == o.group && r.cost < o.cost)
}

type costRanker struct {
	resolver usage.RateResolver
	pools    map[string]struct{}
	// alias 是客户端请求的模型名，也是成本比较单位；成员的上游名只用于
	// 取价与出站，不参与「是否属于同一模型」的判断。
	alias string
	now   time.Time
}

// SetRateResolverSource 接入计价解析器来源（通常是价格缓存的 GetChecked）。
// 未接入或返回错误时，cost 池按现行负载均衡处理。
func (s *Service) SetRateResolverSource(source func() (usage.RateResolver, error)) {
	if source == nil {
		s.rateSource.Store(nil)
		return
	}
	s.rateSource.Store(&source)
}

func costPoolSet(pools []config.DeploymentPool) map[string]struct{} {
	set := make(map[string]struct{})
	for _, p := range pools {
		if config.NormalizeDeploymentPool(p).EffectiveRoutingStrategy() == config.PoolRoutingCost {
			set[strings.ToLower(strings.TrimSpace(p.Name))] = struct{}{}
		}
	}
	return set
}

// newCostRanker returns nil whenever cost routing must not influence selection.
func (s *Service) newCostRanker(alias string, now time.Time) *costRanker {
	pools := s.costPools.Load()
	alias = strings.TrimSpace(alias)
	if pools == nil || len(*pools) == 0 || alias == "" {
		return nil
	}
	source := s.rateSource.Load()
	if source == nil {
		return nil
	}
	resolver, err := (*source)()
	if err != nil || resolver == nil {
		return nil
	}
	return &costRanker{resolver: resolver, pools: *pools, alias: alias, now: now}
}

func (c *costRanker) managed(t *Target) bool {
	if t == nil || t.Pool == "" {
		return false
	}
	_, ok := c.pools[strings.ToLower(t.Pool)]
	return ok
}

// comparison resolves the client alias through one member's own mapping and
// prices the pair (target, resolved upstream, now). The returned upstream keeps
// the configured spelling; it is the same key billing records for this member.
func (c *costRanker) comparison(t *Target) (string, costRank) {
	if !c.managed(t) {
		return "", costRank{group: costGroupOutside}
	}
	upstream, ok := resolveUpstreamModel(t, c.alias)
	if !ok || upstream == "" {
		return "", costRank{group: costGroupUnpriced}
	}
	rates, ok := c.resolver.RateFor(t.Name, upstream, c.now)
	if !ok {
		return upstream, costRank{group: costGroupUnpriced}
	}
	return upstream, costRank{group: costGroupPriced, cost: rates.CostFor(costRefInputTokens, costRefOutputTokens, costRefCachedTokens, 0)}
}

func (c *costRanker) rank(t *Target) costRank {
	_, rank := c.comparison(t)
	return rank
}

// describe renders an alias-first diagnostic for debug logs:
// target→upstream[$cost], target→upstream[unpriced] or target[outside].
func (c *costRanker) describe(targets []*Target) string {
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		upstream, rank := c.comparison(t)
		switch rank.group {
		case costGroupPriced:
			parts = append(parts, fmt.Sprintf("%s→%s[$%.6f]", targetName(t), upstream, rank.cost))
		case costGroupUnpriced:
			parts = append(parts, fmt.Sprintf("%s→%s[unpriced]", targetName(t), upstream))
		default:
			parts = append(parts, fmt.Sprintf("%s[outside]", targetName(t)))
		}
	}
	return strings.Join(parts, ",")
}

// cheapestTier narrows already-admitted candidates to the cheapest tier. It
// returns items unchanged when no candidate is managed, or when every member
// of that tier is at the soft load cap, so load balancing keeps the final say.
func cheapestTier[T any](c *costRanker, items []T, stateOf func(T) *targetState) []T {
	if c == nil || len(items) < 2 {
		return items
	}
	ranks := make([]costRank, len(items))
	best := 0
	for i, item := range items {
		ranks[i] = c.rank(stateOf(item).Target())
		if ranks[i].less(ranks[best]) {
			best = i
		}
	}
	if ranks[best].group == costGroupOutside {
		return items
	}
	tier := make([]T, 0, len(items))
	headroom := false
	for i, item := range items {
		if ranks[best].within(ranks[i]) {
			tier = append(tier, item)
			headroom = headroom || stateOf(item).belowAffinitySoftCap()
		}
	}
	if !headroom {
		return items
	}
	return tier
}

// cheaperTierAvailable reports whether a sticky target should yield: another
// eligible, healthy target with headroom sits in a strictly cheaper tier. It
// only reads state (no probe leases); findAvailableTargetWithModel still makes
// the binding admission decision, so a wrong answer never loses availability.
func (s *Service) cheaperTierAvailable(c *costRanker, bound *targetState, allowed map[string]struct{}, model, path string, now time.Time) bool {
	if c == nil || bound == nil || !c.managed(bound.Target()) {
		return false
	}
	boundUpstream, boundRank := c.comparison(bound.Target())
	for _, state := range s.targetSnapshot() {
		if state == nil || state == bound || state.Target() == nil {
			continue
		}
		t := state.Target()
		if t.Paused || !modelAllowed(t, model) || !t.SupportsPath(path) {
			continue
		}
		if allowed != nil {
			if _, ok := allowed[strings.ToLower(t.Name)]; !ok {
				continue
			}
		}
		if state.IsMuted(now) || !state.belowAffinitySoftCap() {
			continue
		}
		if t.MaxConcurrency > 0 && state.inFlightCount() >= int64(t.MaxConcurrency) {
			continue
		}
		upstream, r := c.comparison(t)
		if r.less(boundRank) && !r.within(boundRank) {
			if s.logger != nil && s.logger.Enabled(context.Background(), slog.LevelDebug) {
				s.logger.Debug("cost routing rebind candidate",
					"alias", c.alias,
					"bound_target", targetName(bound.Target()),
					"bound_upstream", boundUpstream,
					"candidate_target", targetName(t),
					"candidate_upstream", upstream,
				)
			}
			return true
		}
	}
	return false
}
