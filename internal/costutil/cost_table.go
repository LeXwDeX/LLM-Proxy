// Package costutil provides shared cost-table construction utilities used by
// the proxy quota subsystem and admin layer.
package costutil

import (
	"strings"

	"github.com/LeXwDeX/LLM-Proxy/internal/catalog"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// ToCostTable builds the global-layer CostTable keyed by model name only.
// Layer 1: catalog default prices as baseline.
// Layer 2: custom model_costs override (higher priority).
// per-target 记录（Target 非空）不属于全局层，跳过——它们经 ToTargetCostTable
// 进入解析阶梯的 per-target 层。
func ToCostTable(costs []nosql.ModelCost, cat *catalog.Catalog) usage.CostTable {
	table := make(usage.CostTable)

	// Layer 1: catalog default prices as baseline.
	if cat != nil {
		for _, entry := range cat.ListAll() {
			if entry.DefaultCost == nil || entry.Model == "" {
				continue
			}
			model := strings.ToLower(strings.TrimSpace(entry.Model))
			rates := usage.CostRates{
				InputPer1MTokens:            entry.DefaultCost.InputPer1MTokens,
				OutputPer1MTokens:           entry.DefaultCost.OutputPer1MTokens,
				CacheWritePer1MToken:        entry.DefaultCost.CacheWritePer1MToken,
				CacheReadPer1MToken:         entry.DefaultCost.CacheReadPer1MToken,
				ImageInputPer1MTokens:       usage.CloneRate(entry.DefaultCost.ImageInputPer1MTokens),
				CachedImageInputPer1MTokens: usage.CloneRate(entry.DefaultCost.CachedImageInputPer1MTokens),
				ImageOutputPer1MTokens:      usage.CloneRate(entry.DefaultCost.ImageOutputPer1MTokens),
			}
			table[model] = rates
		}
	}

	// Layer 2: custom model_costs override (higher priority).
	for _, cost := range costs {
		if strings.TrimSpace(cost.Target) != "" {
			continue // per-target layer, not part of the global table
		}
		model := strings.ToLower(strings.TrimSpace(cost.Model))
		if model == "" {
			continue
		}
		rates := usage.CostRates{
			InputPer1MTokens:            cost.InputPer1MTokens,
			OutputPer1MTokens:           cost.OutputPer1MTokens,
			CacheWritePer1MToken:        cost.CacheWritePer1MToken,
			CacheReadPer1MToken:         cost.CacheReadPer1MToken,
			ExplicitCacheReadPer1MToken: usage.CloneRate(cost.ExplicitCacheReadPer1MToken),
			CacheWrite5mPer1MToken:      usage.CloneRate(cost.CacheWrite5mPer1MToken),
			CacheWrite1hPer1MToken:      usage.CloneRate(cost.CacheWrite1hPer1MToken),
			ImageInputPer1MTokens:       usage.CloneRate(cost.ImageInputPer1MTokens),
			CachedImageInputPer1MTokens: usage.CloneRate(cost.CachedImageInputPer1MTokens),
			ImageOutputPer1MTokens:      usage.CloneRate(cost.ImageOutputPer1MTokens),
		}
		table[model] = rates
	}

	return table
}

// ToTargetCostTable extracts only the per-target model costs into a
// target → model → rates lookup. 空目标（全局层）记录被跳过。
func ToTargetCostTable(costs []nosql.ModelCost) map[string]map[string]usage.CostRates {
	out := make(map[string]map[string]usage.CostRates)
	for _, cost := range costs {
		target := strings.ToLower(strings.TrimSpace(cost.Target))
		model := strings.ToLower(strings.TrimSpace(cost.Model))
		if target == "" || model == "" {
			continue
		}
		if out[target] == nil {
			out[target] = make(map[string]usage.CostRates)
		}
		out[target][model] = usage.CostRates{
			InputPer1MTokens:            cost.InputPer1MTokens,
			OutputPer1MTokens:           cost.OutputPer1MTokens,
			CacheWritePer1MToken:        cost.CacheWritePer1MToken,
			CacheReadPer1MToken:         cost.CacheReadPer1MToken,
			ExplicitCacheReadPer1MToken: usage.CloneRate(cost.ExplicitCacheReadPer1MToken),
			CacheWrite5mPer1MToken:      usage.CloneRate(cost.CacheWrite5mPer1MToken),
			CacheWrite1hPer1MToken:      usage.CloneRate(cost.CacheWrite1hPer1MToken),
			ImageInputPer1MTokens:       usage.CloneRate(cost.ImageInputPer1MTokens),
			CachedImageInputPer1MTokens: usage.CloneRate(cost.CachedImageInputPer1MTokens),
			ImageOutputPer1MTokens:      usage.CloneRate(cost.ImageOutputPer1MTokens),
		}
	}
	return out
}
