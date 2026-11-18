// shadow_routing.go — 性能优先池的选路影子模式（#212 阶段 T2）。
//
// 影子只回答「如果按上游生成性能估计选路，会选谁、和实际差多少」，绝不改变
// 实际选路：比较发生在真实加权选择完成之后，对已准入候选做一次纯读取的估计
// 比较，只累加计数器。不消费 rrCounter，不修改候选/负载/粘连/熔断/租约，
// 不重新准入，也不改变任何返回值。
//
// 计数分母口径（agree + disagree）：
//   - 计数：selectTargetTraced 中未命中粘连时的普通加权选路（含没有粘连
//     记录时由目标重试触发的再次选路），且实际选中 target 所属池为性能优先池
//     （routing_strategy=load）、该池在本次已准入候选中至少 2 个成员、请求
//     带客户端模型名（否则没有可比较的按模型估计），且该池至少一个成员的
//     TTFT 与 TPS 样本均充足（全部不足时所有人都退回同一中性先验，比较无意义，
//     与 #199 的“全部样本不足则系数取 1”一致）。
//   - 不计数：显式 X-Proxy-Target、会话/IP 粘连命中、粘连绑定不可用后的
//     failover 轮、半开或模型探针等非加权选择路径，以及成本优先池与其他池。
package proxy

import (
	"sort"
	"strings"
	"time"
)

// ShadowPoolMember is one target's shadow selection record inside a pool.
// Share is computed by the backend: shadow selections over the pool's total
// comparisons, so all member shares of one pool sum to 1.
type ShadowPoolMember struct {
	Target           string  `json:"target"`
	ShadowSelections int64   `json:"shadow_selections"`
	Share            float64 `json:"share"`
}

// ShadowPoolStats is the read-only shadow comparison of one performance-priority
// pool: how often the performance-only pick matched the actual pick, plus each
// member's share of the shadow picks.
type ShadowPoolStats struct {
	Pool     string             `json:"pool"`
	Agree    int64              `json:"agree"`
	Disagree int64              `json:"disagree"`
	Members  []ShadowPoolMember `json:"members"`
}

// shadowPoolCounter is one pool's accumulated shadow outcome. It is guarded by
// Service.shadowMu and never exposed directly.
type shadowPoolCounter struct {
	agree    int64
	disagree int64
	members  map[string]int64
}

// performancePriorityPool reports whether pool (lowercase) is currently
// configured with the performance-priority routing strategy.
func (s *Service) performancePriorityPool(pool string) bool {
	pools := s.loadPools.Load()
	if pools == nil || len(*pools) == 0 {
		return false
	}
	_, ok := (*pools)[pool]
	return ok
}

// observeShadowSelection compares a completed weighted selection with the pick
// a performance-only comparison would have made. It only reads: estimates live
// under each target's own lock and the outcome counters under shadowMu, never
// the selection locks.
func (s *Service) observeShadowSelection(actual *targetState, admitted []admittedCandidate, model string, now time.Time) {
	if actual == nil || actual.Target() == nil || strings.TrimSpace(model) == "" {
		return
	}
	target := actual.Target()
	pool := strings.ToLower(strings.TrimSpace(target.Pool))
	if pool == "" || !s.performancePriorityPool(pool) {
		return
	}
	// Pool members among the candidates admitted for this request. Pool
	// membership is configuration, so filtering the snapshot cannot diverge
	// from the actual selection's view.
	members := make([]*targetState, 0, len(admitted))
	for _, c := range admitted {
		t := c.state.Target()
		if t == nil || strings.ToLower(strings.TrimSpace(t.Pool)) != pool {
			continue
		}
		members = append(members, c.state)
	}
	if len(members) < 2 {
		return
	}
	bestName := ""
	bestMS := 0.0
	sufficient := false
	for _, member := range members {
		t := member.Target()
		// resolveUpstreamModel is the same alias/mapping resolution the forward
		// path uses; it is pure and changes no admission or lease state.
		upstream, ok := resolveUpstreamModel(t, model)
		if !ok || upstream == "" {
			continue
		}
		est, observed := member.performanceEstimate(upstream, now)
		// At least one member must have enough samples in both TTFT and TPS.
		// Otherwise every candidate falls back to the same neutral prior, the
		// comparison carries no information, and it must not count — the same
		// rule as #199's "all insufficient → coefficient 1". Members with
		// insufficient samples still take part in the argmin with their prior.
		if observed && !est.Insufficient {
			sufficient = true
		}
		ms := predictedCompletionMS(est)
		if bestName == "" || ms < bestMS || (ms == bestMS && shadowNameLess(t.Name, bestName)) {
			bestName, bestMS = t.Name, ms
		}
	}
	if bestName == "" || !sufficient {
		return
	}
	s.recordShadowOutcome(pool, bestName, target.Name)
}

// shadowNameLess is the deterministic tie-break: target name ascending,
// case-insensitively first and raw spelling second.
func shadowNameLess(a, b string) bool {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	if la != lb {
		return la < lb
	}
	return a < b
}

func (s *Service) recordShadowOutcome(pool, shadowTarget, actualTarget string) {
	s.shadowMu.Lock()
	defer s.shadowMu.Unlock()
	if s.shadowCounters == nil {
		s.shadowCounters = make(map[string]*shadowPoolCounter)
	}
	counter := s.shadowCounters[pool]
	if counter == nil {
		counter = &shadowPoolCounter{members: make(map[string]int64)}
		s.shadowCounters[pool] = counter
	}
	if strings.EqualFold(shadowTarget, actualTarget) {
		counter.agree++
	} else {
		counter.disagree++
	}
	counter.members[shadowTarget]++
}

// ShadowRoutingSnapshot returns a thread-safe read-only copy of the shadow
// comparison for every pool that is still configured as performance-priority
// and has at least one comparison. Members list the pool's current targets plus
// any target that was shadow-picked before leaving the pool; Share is computed
// here so the frontend never rescales.
func (s *Service) ShadowRoutingSnapshot() []ShadowPoolStats {
	if s == nil {
		return nil
	}
	pools := s.loadPools.Load()
	if pools == nil || len(*pools) == 0 {
		return nil
	}

	s.shadowMu.Lock()
	counters := make(map[string]shadowPoolCounter, len(s.shadowCounters))
	for pool, c := range s.shadowCounters {
		copied := shadowPoolCounter{agree: c.agree, disagree: c.disagree, members: make(map[string]int64, len(c.members))}
		for name, n := range c.members {
			copied.members[name] = n
		}
		counters[pool] = copied
	}
	s.shadowMu.Unlock()
	if len(counters) == 0 {
		return nil
	}

	current := make(map[string][]string)
	for _, state := range s.targetSnapshot() {
		if state == nil || state.Target() == nil {
			continue
		}
		t := state.Target()
		pool := strings.ToLower(strings.TrimSpace(t.Pool))
		if pool == "" {
			continue
		}
		current[pool] = append(current[pool], t.Name)
	}

	out := make([]ShadowPoolStats, 0, len(counters))
	for pool, counter := range counters {
		if _, ok := (*pools)[pool]; !ok {
			continue // strategy changed or pool removed since the counts
		}
		total := counter.agree + counter.disagree
		if total <= 0 {
			continue
		}
		memberSet := make(map[string]struct{}, len(current[pool])+len(counter.members))
		for _, name := range current[pool] {
			memberSet[name] = struct{}{}
		}
		for name := range counter.members {
			memberSet[name] = struct{}{}
		}
		names := make([]string, 0, len(memberSet))
		for name := range memberSet {
			names = append(names, name)
		}
		sort.Slice(names, func(i, j int) bool { return shadowNameLess(names[i], names[j]) })

		stats := ShadowPoolStats{Pool: pool, Agree: counter.agree, Disagree: counter.disagree}
		stats.Members = make([]ShadowPoolMember, 0, len(names))
		for _, name := range names {
			selections := counter.members[name]
			stats.Members = append(stats.Members, ShadowPoolMember{
				Target:           name,
				ShadowSelections: selections,
				Share:            float64(selections) / float64(total),
			})
		}
		out = append(out, stats)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pool < out[j].Pool })
	return out
}
