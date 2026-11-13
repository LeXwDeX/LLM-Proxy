package quota

import (
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// 额度只由客户端 plan 中模型组的绑定显式授予：(target, model) 不属于任何
// plan 组的请求在准入时拒绝（scope=unconfigured），不再有不限额的兜底组。
func TestManagerCheckTarget_RejectsRouteOutsidePlan(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{Name: "exact", CostCoefficient: 1, DailyUSD: 10, Bindings: []config.QuotaGroupBinding{{Target: "openai", Model: "gpt-4o"}}})
	putModelGroup(t, db, config.ModelGroup{Name: "whole", CostCoefficient: 1, DailyUSD: 10, Bindings: []config.QuotaGroupBinding{{Target: "claude"}}})
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "exact"}, {Name: "whole"}}, "L5")
	quotaTestCost(t, db)
	m := newTestManager(t, db)
	m.Evaluate("alice")

	for _, tc := range []struct{ target, model string }{
		{"openai", "gpt-4o"},
		{"OpenAI", "GPT-4o"},
		{"claude", "anything"},
	} {
		if info, rejected := m.CheckTarget("alice", tc.target, tc.model); rejected {
			t.Fatalf("CheckTarget(%s,%s) rejected %+v; bound routes must pass", tc.target, tc.model, info)
		}
	}
	for _, tc := range []struct{ target, model string }{
		{"openai", "gpt-4o-mini"}, // target bound only for another model
		{"unbound", "gpt-4o"},
	} {
		info, rejected := m.CheckTarget("alice", tc.target, tc.model)
		if !rejected {
			t.Fatalf("CheckTarget(%s,%s) passed; routes outside the plan must be rejected", tc.target, tc.model)
		}
		if info.Scope != ScopeUnconfigured || info.Dimension != UnconfiguredDimension {
			t.Fatalf("CheckTarget(%s,%s) = %+v, want scope %q dimension %q", tc.target, tc.model, info, ScopeUnconfigured, UnconfiguredDimension)
		}
	}
}

func TestManagerCheckTarget_ClientWithoutPlanRejected(t *testing.T) {
	db := testOpenDB(t)
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", JobLevel: "L5"})
	putJobLevel(t, db, config.JobLevel{Name: "L5"})
	quotaTestCost(t, db)
	m := newTestManager(t, db)
	m.Evaluate("alice")

	info, rejected := m.CheckTarget("alice", "openai", "gpt-4o")
	if !rejected || info.Scope != ScopeUnconfigured {
		t.Fatalf("CheckTarget = %+v rejected=%v; a client without any plan group must be rejected as unconfigured", info, rejected)
	}
}

func TestManagerIncrement_UsageOutsidePlanNotCounted(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{Name: "plan", CostCoefficient: 1, DailyUSD: 100, Bindings: []config.QuotaGroupBinding{{Target: "openai"}}})
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "plan"}}, "L5")
	quotaTestCost(t, db)
	m := newTestManager(t, db)

	quotaTestInput(t, m, "alice", "openai", "gpt-4o", 1_000_000)  // $10 into plan
	quotaTestInput(t, m, "alice", "unbound", "gpt-4o", 1_000_000) // admitted before a binding change: no group

	status := m.Status("alice")
	if len(status.Groups) != 1 {
		t.Fatalf("groups = %v, want only the plan group (no fallback group)", status.Groups)
	}
	if _, ok := status.Groups["company_general"]; ok {
		t.Fatal("company_general fallback group must no longer be created")
	}
	if got := status.Groups["plan"].Quotas[DimensionDaily].Used; got != 10 {
		t.Fatalf("plan used = %v, want 10", got)
	}
}

func TestManagerEvaluate_HistoricalUsageOutsidePlanNotCounted(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{Name: "plan", CostCoefficient: 1, DailyUSD: 100, Bindings: []config.QuotaGroupBinding{{Target: "openai"}}})
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "plan"}}, "L5")
	quotaTestCost(t, db)
	usageStore := nosql.NewUsageStore(db)
	for _, target := range []string{"openai", "unbound"} {
		if err := usageStore.Record(usage.Event{
			Timestamp: time.Now().UTC(), ClientName: "alice", Target: target,
			Model: "gpt-4o", EndpointType: "openai", InputTokens: 2_000_000, StatusCode: 200,
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	m := newTestManager(t, db)
	m.Evaluate("alice")

	status := m.Status("alice")
	if len(status.Groups) != 1 {
		t.Fatalf("groups = %v, want only the plan group", status.Groups)
	}
	if got := status.Groups["plan"].Quotas[DimensionDaily].Used; got != 20 {
		t.Fatalf("plan used = %v, want 20 (unbound usage ignored)", got)
	}
}
