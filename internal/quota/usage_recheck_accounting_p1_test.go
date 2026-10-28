package quota

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72IdentityRecheckCannotClearQuotaCalibration(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "models"}}, "employee")
	putModelGroup(t, db, config.ModelGroup{Name: "models", CostCoefficient: 1, DailyUSD: 100, Bindings: []config.QuotaGroupBinding{{Target: "anthropic"}}})
	m.costCache = pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) {
		return []pricing.PricingTemplate{{Target: "anthropic", Model: "executor", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}}, {Target: "anthropic", Model: "advisor", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 5}}}, nil
	}, nil)
	m.Evaluate("alice")
	current := quotaBundle()
	current.Components = nil
	if err := m.RecordUsage(m.usageStore, current); err != nil {
		t.Fatal(err)
	}
	if got := bundleUsed(m); got != 2 {
		t.Fatalf("fixture spend=%v", got)
	}
	missing := quotaBundle()
	missing.Components = nil
	missing.Model = "advisor"
	original, err := json.Marshal(missing)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.usageStore.RegisterUnresolvedUsage(missing, original); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		key := missing.Timestamp.UTC().Format(time.RFC3339Nano) + "_" + missing.RecordID
		return tx.Bucket([]byte(nosql.BucketUsageEvents)).Put([]byte(key), original)
	}); err != nil {
		t.Fatal(err)
	}
	entries, _, err := m.usageStore.ListUsageRechecks(0, 100)
	if err != nil || len(entries) != 1 {
		t.Fatalf("evidence: %v %v", entries, err)
	}
	result, err := m.usageStore.RecheckUsage(nosql.UsageRecheckRequest{EvidenceID: entries[0].ID, Version: entries[0].Version, OperationID: "dddddddd-dddd-4ddd-8ddd-dddddddddd13", Actor: "admin"})
	if err != nil || result.Status != "identity_verified_accounting_unresolved" || result.AccountingRestored {
		t.Fatalf("recheck: %+v %v", result, err)
	}
	m.Evaluate("alice")
	if status := m.Status("alice"); status.CalibrationError == "" || bundleUsed(m) != 2 {
		t.Fatalf("identity-only recheck replaced counters: %+v", status)
	}
	if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); !blocked || info.Scope != ScopeAccounting {
		t.Fatalf("incomplete accounting admitted: %+v %v", info, blocked)
	}
}
