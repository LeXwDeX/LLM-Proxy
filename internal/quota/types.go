package quota

import (
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Scope 常量：超限维度归属层（额度体系 v2）。
// 个人维度限额由职级系数兜底；池分层限额已退役。
const (
	// ScopeJobLevel 表示客户端未绑定职级（或绑定的职级不存在）——
	// 未配置额度，请求一律拒绝，直到管理员绑定有效职级。
	ScopeJobLevel   = "job_level"
	ScopeModelGroup = "model_group"
	ScopeAccounting = "accounting"
)

// AllScopeOptions provides display names for the runtime quota scopes.
func AllScopeOptions() []config.EnumOption {
	return []config.EnumOption{
		{Value: ScopeJobLevel, Label: "职级"},
		{Value: ScopeModelGroup, Label: "模型组"},
		{Value: ScopeAccounting, Label: "计费校准"},
	}
}

// ExceededInfo 内存超限标记结构。超限标记由 internal/quota 在准入检查时写入。
type ExceededInfo struct {
	Scope     string    `json:"scope,omitempty"` // job_level | model_group
	GroupName string    `json:"group_name,omitempty"`
	Dimension string    `json:"dimension"` // "daily" | "weekly" | "monthly" | "unassigned"(job_level)
	Limit     float64   `json:"limit"`     // 触发的限额值 (USD)
	Used      float64   `json:"used"`      // 评估时的累计用量 (USD)
	ResetsAt  time.Time `json:"resets_at"` // 下个自然周期起始 (UTC)
}

// JobLevelUnassignedDimension 是 job_level scope 的固定维度名。
const JobLevelUnassignedDimension = "unassigned"

// QuotaStatus admin 查询返回结构。由 internal/admin 的配额查询接口填充。
// 额度体系 v2：Quotas 为客户端 plan 内各模型组有效额度的聚合视图
// （Σ有效上限 vs Σ折算用量；任一组该维度不限 → Limit=0 表示不限）。
type QuotaStatus struct {
	CalibrationError string                 `json:"calibration_error,omitempty"`
	Client           string                 `json:"client"`
	JobLevel         string                 `json:"job_level,omitempty"`
	JobLevelMissing  bool                   `json:"job_level_missing,omitempty"`
	Quotas           map[string]QuotaUsage  `json:"quotas"`   // daily/weekly/monthly（plan 组聚合）
	Exceeded         *ExceededInfo          `json:"exceeded"` // nil if not exceeded
	Groups           map[string]GroupStatus `json:"groups,omitempty"`
}

// GroupStatus 单个模型组在客户端上的额度状态。Limit 为有效额度
// （组基数 × 职级系数 × 个人系数），Used 为折算用量
// （计费原价 × 组系数）。透明字段供 UI 展示公式推导。
type GroupStatus struct {
	Coefficient      float64               `json:"cost_coefficient"`            // 组系数
	PlanCoefficient  float64               `json:"plan_coefficient"`            // 个人系数（plan）
	RankCoefficients map[string]float64    `json:"rank_coefficients,omitempty"` // 职级系数（daily/weekly/monthly）
	BaseLimits       map[string]float64    `json:"base_limits,omitempty"`       // 组基数（额度策略里的日/周/月额度）
	InPlan           bool                  `json:"in_plan,omitempty"`           // 是否客户端 plan 显式添加（company_general 兜底为 false）
	Quotas           map[string]QuotaUsage `json:"quotas"`
	Exceeded         *ExceededInfo         `json:"exceeded,omitempty"`
}

// QuotaUsage 单维度用量进度。
type QuotaUsage struct {
	Limit    float64   `json:"limit"`
	Used     float64   `json:"used"`
	ResetsAt time.Time `json:"resets_at"`
}
