package quota

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
)

// quotaGroupForCounters must trim binding fields at compare time: bindings
// persisted before write-side normalization (or edited directly) may carry
// surrounding whitespace, and a whitespace-only Model used to be treated as an
// exact binding instead of a whole-target binding.
func TestQuotaGroupForCountersTrimsBindingWhitespace(t *testing.T) {
	now := time.Now().UTC()
	build := func(binding config.QuotaGroupBinding) *clientCounters {
		cc := &clientCounters{groups: make(map[string]*groupCounters)}
		cc.groups["plan"] = buildGroupCounters(
			config.QuotaGroup{Name: "plan"},
			config.ModelGroup{Name: "plan", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{binding}},
			config.JobLevel{}, now)
		return cc
	}
	cases := []struct {
		name    string
		binding config.QuotaGroupBinding
		target  string
		model   string
		matched bool
	}{
		{"binding target has whitespace", config.QuotaGroupBinding{Target: " openai "}, "openai", "gpt-4o", true},
		{"binding model has whitespace", config.QuotaGroupBinding{Target: "openai", Model: " gpt-4o "}, "openai", "gpt-4o", true},
		{"binding model is blank", config.QuotaGroupBinding{Target: " openai ", Model: "   "}, "openai", "gpt-4o", true},
		{"request has whitespace", config.QuotaGroupBinding{Target: "openai"}, " openai ", " gpt-4o ", true},
		{"unrelated target", config.QuotaGroupBinding{Target: "other"}, "openai", "gpt-4o", false},
		{"exact model differs", config.QuotaGroupBinding{Target: "openai", Model: "gpt-4o"}, "openai", "gpt-4.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, _, matched := quotaGroupForCounters(build(tc.binding), tc.target, tc.model)
			if matched != tc.matched {
				t.Fatalf("quotaGroupForCounters matched = %v, want %v", matched, tc.matched)
			}
			if matched && name != "plan" {
				t.Fatalf("matched group = %q, want plan", name)
			}
		})
	}
}

// A client plan whose catalog group carries whitespace bindings must still
// grant the route (no 403 unconfigured) once the counters are built, and an
// unrelated model must stay unmatched.
func TestManagerCheckTargetMatchesWhitespaceBindings(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{
		Name: "plan", CostCoefficient: 1, DailyUSD: 10,
		Bindings: []config.QuotaGroupBinding{{Target: " openai ", Model: " gpt-4o "}},
	})
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "plan"}}, "L5")
	putModelCost(t, db, nosql.ModelCost{
		EndpointType: "openai", Model: "gpt-4o",
		InputPer1MTokens: 10, OutputPer1MTokens: 10,
	})
	putAggCell(t, db, nowHourKey("openai", "alice", "gpt-4o"), nosql.AggCell{
		InputTokens: 500_000, OutputTokens: 500_000,
	})

	m := newTestManager(t, db)
	m.Evaluate("alice")

	info, exceeded := m.CheckTarget("alice", "openai", "gpt-4o")
	if !exceeded || info.Scope != ScopeModelGroup || info.GroupName != "plan" {
		t.Fatalf("CheckTarget = %+v exceeded=%v, want model_group/plan exceeded", info, exceeded)
	}
	info, exceeded = m.CheckTarget("alice", "openai", "gpt-4.1")
	if !exceeded || info.Scope != ScopeUnconfigured {
		t.Fatalf("unrelated model = %+v exceeded=%v, want unconfigured rejection", info, exceeded)
	}
}

// The API must expose whether a plan group has any binding so the quota page
// can flag "no route can match this group" instead of reporting it healthy.
func TestGroupStatusJSONReportsBound(t *testing.T) {
	raw, err := json.Marshal(GroupStatus{InPlan: true})
	if err != nil {
		t.Fatalf("marshal GroupStatus: %v", err)
	}
	if !strings.Contains(string(raw), `"bound":false`) {
		t.Fatalf("GroupStatus JSON must always expose bound: %s", raw)
	}
}

// The full save path: bindings with whitespace (including a whitespace-only
// model) go through ModelGroupStore.Create, then real (target, model) lookups
// must resolve to the saved groups instead of being rejected as unconfigured.
func TestManagerCheckTargetAfterStoreNormalizedBindings(t *testing.T) {
	db := testOpenDB(t)
	if err := nosql.NewTargetStore(db).Create(config.Target{
		Name: "openai", EndpointType: "openai", Endpoint: "https://example.com", APIKey: "key",
		UsageAdapter:  config.UsageAdapterStandard,
		ModelMappings: []config.ModelMapping{{Upstream: "gpt-4o"}},
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	groups := nosql.NewModelGroupStore(db)
	if err := groups.Create(config.ModelGroup{
		Name: "exact", CostCoefficient: 1, DailyUSD: 10,
		Bindings: []config.QuotaGroupBinding{{Target: " openai ", Model: " gpt-4o "}},
	}); err != nil {
		t.Fatalf("create exact group: %v", err)
	}
	if err := groups.Create(config.ModelGroup{
		Name: "whole", CostCoefficient: 1, DailyUSD: 10,
		Bindings: []config.QuotaGroupBinding{{Target: " openai ", Model: "   "}},
	}); err != nil {
		t.Fatalf("create whole-target group: %v", err)
	}
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "exact"}, {Name: "whole"}}, "L5")

	m := newTestManager(t, db)
	m.Evaluate("alice")
	m.mu.RLock()
	cc := m.counters["alice"]
	exactGroup, _, exactMatched := quotaGroupForCounters(cc, "openai", "gpt-4o")
	wholeGroup, _, wholeMatched := quotaGroupForCounters(cc, "openai", "gpt-4.1")
	m.mu.RUnlock()
	if !exactMatched || exactGroup != "exact" {
		t.Fatalf("gpt-4o matched group = %q (matched=%v), want exact", exactGroup, exactMatched)
	}
	if !wholeMatched || wholeGroup != "whole" {
		t.Fatalf("gpt-4.1 matched group = %q (matched=%v), want whole", wholeGroup, wholeMatched)
	}
	for _, model := range []string{"gpt-4o", "gpt-4.1"} {
		if info, exceeded := m.CheckTarget("alice", "openai", model); exceeded {
			t.Fatalf("CheckTarget(openai, %s) rejected: %+v", model, info)
		}
	}
	if info, exceeded := m.CheckTarget("alice", "other", "gpt-4o"); !exceeded || info.Scope != ScopeUnconfigured {
		t.Fatalf("unrelated target = %+v exceeded=%v, want unconfigured rejection", info, exceeded)
	}
}

// Status must tell the admin page which plan groups actually bind routes: a
// group left without bindings (e.g. its target was deleted) still has base
// limits and reports in_plan, but no request can match it.
func TestManagerStatusReportsGroupBindingPresence(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{
		Name: "bound", CostCoefficient: 1, DailyUSD: 10,
		Bindings: []config.QuotaGroupBinding{{Target: "openai"}},
	})
	putModelGroup(t, db, config.ModelGroup{Name: "unbound", CostCoefficient: 1, DailyUSD: 10})
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "bound"}, {Name: "unbound"}}, "L5")
	putModelCost(t, db, nosql.ModelCost{EndpointType: "openai", Model: "gpt-4o", InputPer1MTokens: 10, OutputPer1MTokens: 10})

	m := newTestManager(t, db)
	m.Evaluate("alice")

	status := m.Status("alice")
	if gs, ok := status.Groups["bound"]; !ok || !gs.Bound || !gs.InPlan {
		t.Fatalf("bound group status = %+v (present=%v), want in_plan with bound=true", gs, ok)
	}
	if gs, ok := status.Groups["unbound"]; !ok || gs.Bound || !gs.InPlan {
		t.Fatalf("unbound group status = %+v (present=%v), want in_plan with bound=false", gs, ok)
	}
	if _, exceeded := m.CheckTarget("alice", "openai", "gpt-4o"); exceeded {
		t.Fatal("bound route must not be rejected")
	}
}
