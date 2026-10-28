package quota

import (
	"fmt"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72MalformedStableQuotaKeyBlocksAffectedCalibration(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	plan := []config.QuotaGroup{{Name: "models"}}
	putQuotaClient(t, db, plan, "employee")
	putClient(t, db, config.Client{Name: "bob", AccessKey: "test-bob", JobLevel: "employee"})
	putClientQuotaGroups(t, db, "bob", plan)
	putModelGroup(t, db, config.ModelGroup{Name: "models", CostCoefficient: 1, DailyUSD: 100, Bindings: []config.QuotaGroupBinding{{Target: "anthropic"}}})
	m.costCache = pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) {
		return []pricing.PricingTemplate{{Target: "anthropic", Model: "executor", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}}}, nil
	}, nil)
	m.Evaluate("alice")
	event := quotaBundle()
	event.Components = nil
	if err := m.RecordUsage(m.usageStore, event); err != nil {
		t.Fatal(err)
	}
	if got := bundleUsed(m); got != 2 {
		t.Fatalf("initial spend=%v", got)
	}
	hour := event.Timestamp.UTC().Format("2006-01-02T15")
	if err := db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(nosql.BucketQuotaUsageAggHourly))
		if err := bucket.Delete([]byte(fmt.Sprintf("%s|alice|anthropic|executor", hour))); err != nil {
			return err
		}
		return bucket.Put([]byte(hour+"|alice|anthropic"), []byte(`{"requests":1,"input_tokens":1000000}`))
	}); err != nil {
		t.Fatal(err)
	}
	m.Evaluate("alice")
	if status := m.Status("alice"); status.CalibrationError == "" || bundleUsed(m) != 2 {
		t.Fatalf("malformed rollup replaced spend: %+v", status)
	}
	if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); !blocked || info.Scope != ScopeAccounting {
		t.Fatalf("malformed rollup admitted affected client: %+v blocked=%v", info, blocked)
	}
	m.Evaluate("bob")
	if status := m.Status("bob"); status.CalibrationError != "" {
		t.Fatalf("unrelated owner blocked: %+v", status)
	}
}
