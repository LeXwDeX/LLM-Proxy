package config

import "strings"

// DeploymentPool 是一组现有 Target 的逻辑分组（部署池）。
//
// 池不拥有上游配置，仅通过 Target.Pool 字段引用成员；授权一池即授权
// 全部成员节点，池暂停使成员节点等效暂停（internal/proxy/pool.go 物化）。
// Name 保持小写 + 去空白后的唯一标识，DisplayName 保留用户输入的大小写。
type DeploymentPool struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
	Paused      bool   `json:"paused,omitempty"`
	// RoutingStrategy 选择池成员间的灾备/分流逻辑：cost（成本优先）按各自计价模板于请求时刻的
	// 估算成本分层，同层负载均衡并互为灾备（见 internal/proxy/cost_routing.go）；
	// load（性能优先）按负载与存活 key 加权均衡，不看价格。空值（含旧数据）等同 cost，
	// 不做数据迁移，存量显式 load 的池保持性能优先。
	RoutingStrategy string `json:"routing_strategy,omitempty"`
}

const (
	PoolRoutingLoad = "load"
	PoolRoutingCost = "cost"
)

var poolRoutingStrategyOptions = []EnumOption{
	{PoolRoutingLoad, "性能优先"},
	{PoolRoutingCost, "成本优先"},
}

// AllPoolRoutingStrategyOptions 经 /admin/data/schema 下发，前端不重复声明。
func AllPoolRoutingStrategyOptions() []EnumOption {
	return cloneEnumOptions(poolRoutingStrategyOptions)
}

// IsValidPoolRoutingStrategy 接受空值（旧数据，等同 cost）与公开枚举。
func IsValidPoolRoutingStrategy(strategy string) bool {
	return strategy == "" || enumContains(poolRoutingStrategyOptions, strategy)
}

// EffectiveRoutingStrategy 返回生效策略，空值按 cost 处理；显式 load 保持性能优先。
func (p DeploymentPool) EffectiveRoutingStrategy() string {
	if p.RoutingStrategy == "" {
		return PoolRoutingCost
	}
	return p.RoutingStrategy
}

// NormalizeDeploymentPool 规范化池标识并保留显示大小写；旧配置使用 Name 显示。
func NormalizeDeploymentPool(pool DeploymentPool) DeploymentPool {
	pool.DisplayName = strings.TrimSpace(pool.DisplayName)
	if pool.DisplayName == "" {
		pool.DisplayName = strings.TrimSpace(pool.Name)
	}
	pool.Name = strings.ToLower(strings.TrimSpace(pool.Name))
	pool.Description = strings.TrimSpace(pool.Description)
	pool.RoutingStrategy = strings.ToLower(strings.TrimSpace(pool.RoutingStrategy))
	return pool
}
