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
}

// NormalizeDeploymentPool 规范化池标识并保留显示大小写；旧配置使用 Name 显示。
func NormalizeDeploymentPool(pool DeploymentPool) DeploymentPool {
	pool.DisplayName = strings.TrimSpace(pool.DisplayName)
	if pool.DisplayName == "" {
		pool.DisplayName = strings.TrimSpace(pool.Name)
	}
	pool.Name = strings.ToLower(strings.TrimSpace(pool.Name))
	pool.Description = strings.TrimSpace(pool.Description)
	return pool
}
