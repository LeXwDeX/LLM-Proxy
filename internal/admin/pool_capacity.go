package admin

import "github.com/LeXwDeX/LLM-Proxy/internal/config"

// poolListEntry 在部署池配置上附加只读的运行提示字段（不落库、不进写接口）：
//   - effective_routing_strategy：生效策略，空值（旧数据）等同成本优先；
//   - member_capacity_advised：该策略是否依赖成员并发上限（成本优先的软上限溢出）；
//   - members_without_capacity：启用中且未配置 max_concurrency 的成员名，仅成本优先池非空。
type poolListEntry struct {
	config.DeploymentPool
	EffectiveRoutingStrategy string   `json:"effective_routing_strategy"`
	MemberCapacityAdvised    bool     `json:"member_capacity_advised"`
	MembersWithoutCapacity   []string `json:"members_without_capacity"`
}

func buildPoolListEntries(cfg *config.Config) []poolListEntry {
	missing := config.CostPoolMembersWithoutCapacity(cfg)
	entries := make([]poolListEntry, 0, len(cfg.Pools))
	for _, pool := range cfg.Pools {
		normalized := config.NormalizeDeploymentPool(pool)
		members := missing[normalized.Name]
		if members == nil {
			members = []string{}
		}
		strategy := normalized.EffectiveRoutingStrategy()
		entries = append(entries, poolListEntry{
			DeploymentPool:           pool,
			EffectiveRoutingStrategy: strategy,
			MemberCapacityAdvised:    strategy == config.PoolRoutingCost,
			MembersWithoutCapacity:   members,
		})
	}
	return entries
}

// warnCostPoolCapacity 在配置提交或重载成功后，对存在未设并发上限成员的成本优先池写 Warn。
// 按池记录上次内容去重，避免暂停/恢复等无关提交反复刷屏。
func (h *Handler) warnCostPoolCapacity(cfg *config.Config) {
	if h.logger == nil || cfg == nil {
		return
	}
	h.capacityWarnedMu.Lock()
	defer h.capacityWarnedMu.Unlock()
	if h.capacityWarned == nil {
		h.capacityWarned = map[string]string{}
	}
	config.LogCostPoolCapacityWarnings(h.logger, cfg, h.capacityWarned)
}
