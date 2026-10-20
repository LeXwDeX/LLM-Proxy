package nosql

import (
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestModelGroupAllowed(t *testing.T) {
	tests := []struct {
		name     string
		group    string
		selected []string
		want     bool
	}{
		{name: "empty means all", group: "group-a", want: true},
		{name: "case and whitespace", group: " Group-A ", selected: []string{"group-a"}, want: true},
		{name: "multiple values", group: "group-b", selected: []string{"group-a", "group-b"}, want: true},
		{name: "not selected", group: "group-c", selected: []string{"group-a", "group-b"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := modelGroupAllowed(tt.group, tt.selected); got != tt.want {
				t.Fatalf("modelGroupAllowed(%q, %#v) = %v, want %v", tt.group, tt.selected, got, tt.want)
			}
		})
	}
}

func TestResolveUsageOwnershipStates(t *testing.T) {
	existing := map[string]struct{}{"target-a": {}}

	if got := resolveUsageOwnership("", "", nil, existing); got.Resolution != UsageTargetUnknown || got.ModelGroup != UnknownTargetUsageModelGroup {
		t.Fatalf("unknown ownership = %+v", got)
	}
	if got := resolveUsageOwnership("target-a", "gpt-4o", nil, existing); got.Resolution != UsageTargetUnconfigured || got.ModelGroup != CompanyGeneralUsageModelGroup || got.TargetDeleted {
		t.Fatalf("unconfigured ownership = %+v", got)
	}
	if got := resolveUsageOwnership("deleted-target", "gpt-4o", nil, existing); got.Resolution != UsageTargetDeleted || got.ModelGroup != CompanyGeneralUsageModelGroup || !got.TargetDeleted {
		t.Fatalf("deleted ownership = %+v", got)
	}
	index := &GroupOwnershipIndex{
		whole: map[string]string{"target-a": "group-a"},
		exact: map[string]string{"target-a\x00gpt-4o": "group-exact"},
	}
	if got := resolveUsageOwnership("TARGET-A", "gpt-4o-mini", index, existing); got.Resolution != UsageTargetConfigured || got.ModelGroup != "group-a" || got.TargetDeleted {
		t.Fatalf("configured ownership = %+v", got)
	}
	// Exact binding wins over the whole-target binding on the same target.
	if got := resolveUsageOwnership("target-a", "GPT-4o", index, existing); got.ModelGroup != "group-exact" {
		t.Fatalf("exact-binding ownership = %+v", got)
	}
	if got := resolveUsageOwnership("target-a", "gpt-4o", index, map[string]struct{}{}); got.Resolution != UsageTargetDeleted || got.ModelGroup != "group-exact" || !got.TargetDeleted {
		t.Fatalf("deleted-with-binding ownership = %+v", got)
	}
}

func TestAggregateModelGroupsWholeTargetBinding(t *testing.T) {
	db := testDB(t)
	usageStore := NewUsageStore(db)
	targetStore := NewTargetStore(db)
	globalStore := NewModelGroupStore(db)
	if err := targetStore.Create(config.Target{Name: "target-a", Endpoint: "https://example.com", APIKey: "key", UsageAdapter: config.UsageAdapterStandard}); err != nil {
		t.Fatalf("target-a: %v", err)
	}
	if err := targetStore.Create(config.Target{Name: "target-b", Endpoint: "https://example.com", APIKey: "key", UsageAdapter: config.UsageAdapterStandard}); err != nil {
		t.Fatalf("target-b: %v", err)
	}
	if err := globalStore.Create(config.ModelGroup{Name: "group-a", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "target-a"}}}); err != nil {
		t.Fatalf("group-a: %v", err)
	}
	if err := globalStore.Create(config.ModelGroup{Name: "group-b", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "target-b"}}}); err != nil {
		t.Fatalf("group-b: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, event := range []usage.Event{
			{Timestamp: now, ClientName: "client", Target: "target-a", Model: "gpt-4o", InputTokens: 10, StatusCode: 200},
			{Timestamp: now, ClientName: "client", Target: "target-b", Model: "gpt-4o", InputTokens: 20, StatusCode: 200},
		} {
			if err := bumpQuotaAgg(tx, event); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("aggregate setup: %v", err)
	}
	from, to := now.Add(-time.Hour), now.Add(time.Hour)
	result, err := usageStore.AggregateModelGroups(usage.Filter{From: &from, To: &to}, nil, targetStore, globalStore)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	got := make(map[string]int64)
	for _, row := range result.ByModelGroupModel {
		got[row.ModelGroup+"/"+row.Model] = row.InputTokens
	}
	if got["group-a/gpt-4o"] != 10 {
		t.Errorf("group-a/gpt-4o input = %d, want 10", got["group-a/gpt-4o"])
	}
	if got["group-b/gpt-4o"] != 20 {
		t.Errorf("group-b/gpt-4o input = %d, want 20", got["group-b/gpt-4o"])
	}
}

func TestAggregateModelGroupsExactBindingOwnership(t *testing.T) {
	db := testDB(t)
	usageStore := NewUsageStore(db)
	targetStore := NewTargetStore(db)
	globalStore := NewModelGroupStore(db)
	if err := targetStore.Create(config.Target{Name: "target-a", Endpoint: "https://example.com", APIKey: "key", UsageAdapter: config.UsageAdapterStandard, ModelMappings: []config.ModelMapping{{Upstream: "gpt-4o"}, {Upstream: "gpt-4o-mini"}}}); err != nil {
		t.Fatalf("target-a: %v", err)
	}
	if err := globalStore.Create(config.ModelGroup{Name: "group-general", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "target-a"}}}); err != nil {
		t.Fatalf("group-general: %v", err)
	}
	if err := globalStore.Create(config.ModelGroup{Name: "group-exact", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "target-a", Model: "gpt-4o"}}}); err != nil {
		t.Fatalf("group-exact: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, event := range []usage.Event{
			{Timestamp: now, ClientName: "client", Target: "target-a", Model: "gpt-4o", InputTokens: 10, StatusCode: 200},
			{Timestamp: now, ClientName: "client", Target: "target-a", Model: "gpt-4o-mini", InputTokens: 20, StatusCode: 200},
		} {
			if err := bumpQuotaAgg(tx, event); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("aggregate setup: %v", err)
	}
	from, to := now.Add(-time.Hour), now.Add(time.Hour)
	result, err := usageStore.AggregateModelGroups(usage.Filter{From: &from, To: &to}, nil, targetStore, globalStore)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	got := make(map[string]int64)
	for _, row := range result.ByModelGroupModel {
		got[row.ModelGroup+"/"+row.Model] = row.InputTokens
	}
	// Exact binding owns its model even though the target also has a
	// whole-target binding to another group (mirrors quota matching).
	if got["group-exact/gpt-4o"] != 10 {
		t.Errorf("group-exact/gpt-4o input = %d, want 10", got["group-exact/gpt-4o"])
	}
	if got["group-general/gpt-4o-mini"] != 20 {
		t.Errorf("group-general/gpt-4o-mini input = %d, want 20", got["group-general/gpt-4o-mini"])
	}
	if _, leaked := got["group-general/gpt-4o"]; leaked {
		t.Errorf("gpt-4o must belong to group-exact only, got %#v", got)
	}
}

// TestAggregateModelGroupsExactOnlyGroupVisible guards the reported bug: a
// group whose bindings are ALL exact (target+model) — e.g. a subscription
// group covering only specific models — must appear in consumption statistics
// instead of its traffic silently falling into company_general.
func TestAggregateModelGroupsExactOnlyGroupVisible(t *testing.T) {
	db := testDB(t)
	usageStore := NewUsageStore(db)
	targetStore := NewTargetStore(db)
	globalStore := NewModelGroupStore(db)
	if err := targetStore.Create(config.Target{Name: "ali", Endpoint: "https://example.com", APIKey: "key", UsageAdapter: config.UsageAdapterStandard, ModelMappings: []config.ModelMapping{{Upstream: "qwen3.8-max"}, {Upstream: "qwen-turbo"}}}); err != nil {
		t.Fatalf("ali: %v", err)
	}
	if err := globalStore.Create(config.ModelGroup{Name: "qwen-sub", CostCoefficient: 0.3, Bindings: []config.QuotaGroupBinding{{Target: "ali", Model: "qwen3.8-max"}}}); err != nil {
		t.Fatalf("qwen-sub: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, event := range []usage.Event{
			{Timestamp: now, ClientName: "client", Target: "ali", Model: "qwen3.8-max", InputTokens: 10, StatusCode: 200},
			{Timestamp: now, ClientName: "client", Target: "ali", Model: "qwen-turbo", InputTokens: 20, StatusCode: 200},
		} {
			if err := bumpQuotaAgg(tx, event); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("aggregate setup: %v", err)
	}
	from, to := now.Add(-time.Hour), now.Add(time.Hour)
	result, err := usageStore.AggregateModelGroups(usage.Filter{From: &from, To: &to}, nil, targetStore, globalStore)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	got := make(map[string]int64)
	for _, row := range result.ByTargetModel {
		got[row.ModelGroup+"/"+row.Model] = row.InputTokens
	}
	if got["qwen-sub/qwen3.8-max"] != 10 {
		t.Errorf("qwen-sub/qwen3.8-max input = %d, want 10 (exact-only group must be visible)", got["qwen-sub/qwen3.8-max"])
	}
	if got[CompanyGeneralUsageModelGroup+"/qwen-turbo"] != 20 {
		t.Errorf("company_general/qwen-turbo input = %d, want 20", got[CompanyGeneralUsageModelGroup+"/qwen-turbo"])
	}
	// The group must also appear in the group-level rollup.
	found := false
	for _, group := range result.Groups {
		if group.Name == "qwen-sub" && group.Totals.InputTokens == 10 && group.ModelCount == 1 && group.TargetCount == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("qwen-sub missing from group rollup: %+v", result.Groups)
	}
}

func TestAggregateModelGroupsFallbacksAndDeletedTarget(t *testing.T) {
	db := testDB(t)
	usageStore := NewUsageStore(db)
	targetStore := NewTargetStore(db)
	if err := targetStore.Create(config.Target{Name: "target-a", Endpoint: "https://example.com", APIKey: "key", UsageAdapter: config.UsageAdapterStandard}); err != nil {
		t.Fatalf("target-a: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, event := range []usage.Event{
			{Timestamp: now, ClientName: "client", Target: "target-a", Model: "gpt-4o", InputTokens: 10, StatusCode: 200},
			{Timestamp: now, ClientName: "client", Target: "target-b", Model: "gpt-4o", InputTokens: 20, StatusCode: 429},
			{Timestamp: now, ClientName: "client", Model: "gpt-4o", InputTokens: 30, StatusCode: 500},
		} {
			if err := bumpQuotaAgg(tx, event); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("aggregate setup: %v", err)
	}
	from, to := now.Add(-time.Hour), now.Add(time.Hour)
	result, err := usageStore.AggregateModelGroups(usage.Filter{From: &from, To: &to, ClientName: "client"}, nil, targetStore)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(result.ByTargetModel) != 3 {
		t.Fatalf("target/model rows = %d, want 3", len(result.ByTargetModel))
	}
	for _, row := range result.ByTargetModel {
		switch row.Target {
		case "":
			if row.ModelGroup != UnknownTargetUsageModelGroup || row.TargetDeleted {
				t.Errorf("unknown target row = %+v", row)
			}
			if row.Totals.Requests != 0 {
				t.Errorf("unknown target requests = %d, want 0 for status 500", row.Totals.Requests)
			}
		case "target-a":
			if row.ModelGroup != CompanyGeneralUsageModelGroup || row.TargetDeleted {
				t.Errorf("unconfigured target row = %+v", row)
			}
			if row.Totals.Requests != 1 {
				t.Errorf("target-a requests = %d, want 1", row.Totals.Requests)
			}
		case "target-b":
			if row.ModelGroup != CompanyGeneralUsageModelGroup || !row.TargetDeleted {
				t.Errorf("deleted target row = %+v", row)
			}
			if row.Totals.Requests != 1 {
				t.Errorf("429 target requests = %d, want 1", row.Totals.Requests)
			}
		}
	}
}
