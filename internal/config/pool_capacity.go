package config

import (
	"log/slog"
	"sort"
	"strings"
)

// CostPoolMembersWithoutCapacity 返回生效策略为成本优先的部署池中、启用中且未配置
// max_concurrency（<=0 视为不限）的成员名，键为池标识（小写）。
//
// 成本优先按软上限（负载达 0.8）才溢出到下一价格层；成员不设并发上限时软上限永不触发，
// 流量会集中到最便宜成员。该函数只读、无副作用，供启动日志、配置提交日志与管理端提示共用。
// 无命中的池不出现在结果中；成员名保持配置顺序，且不含凭据与地址。
func CostPoolMembersWithoutCapacity(cfg *Config) map[string][]string {
	result := map[string][]string{}
	if cfg == nil {
		return result
	}
	costPools := make(map[string]struct{}, len(cfg.Pools))
	for _, pool := range cfg.Pools {
		normalized := NormalizeDeploymentPool(pool)
		if normalized.Name != "" && normalized.EffectiveRoutingStrategy() == PoolRoutingCost {
			costPools[normalized.Name] = struct{}{}
		}
	}
	for _, target := range cfg.Targets {
		if target.Paused || target.MaxConcurrency > 0 {
			continue
		}
		poolName := strings.ToLower(strings.TrimSpace(target.Pool))
		if _, ok := costPools[poolName]; !ok {
			continue
		}
		result[poolName] = append(result[poolName], target.Name)
	}
	return result
}

// LogCostPoolCapacityWarnings 对每个存在未设并发上限成员的成本优先池写一条 Warn（中英双语），
// 字段为 pool、members、count；不含端点、凭据等其他信息。
//
// seen 非 nil 时按池去重：成员集合与上次相同的池不重复写，恢复正常的池从 seen 中清除；
// 为 nil 则每次全部写出（启动时使用）。调用方负责 seen 的并发保护。
func LogCostPoolCapacityWarnings(logger *slog.Logger, cfg *Config, seen map[string]string) {
	if logger == nil {
		return
	}
	pools := CostPoolMembersWithoutCapacity(cfg)
	if seen != nil {
		for pool := range seen {
			if _, ok := pools[pool]; !ok {
				delete(seen, pool)
			}
		}
	}
	names := make([]string, 0, len(pools))
	for pool := range pools {
		names = append(names, pool)
	}
	sort.Strings(names)
	for _, pool := range names {
		members := pools[pool]
		if seen != nil {
			key := strings.Join(members, "\x00")
			if prev, ok := seen[pool]; ok && prev == key {
				continue
			}
			seen[pool] = key
		}
		logger.Warn("成本优先部署池存在未配置 max_concurrency 的成员，流量会集中到最便宜成员，建议配置 / cost-first pool has members without max_concurrency; traffic will concentrate on the cheapest member, configure a limit",
			"pool", pool, "members", members, "count", len(members))
	}
}
