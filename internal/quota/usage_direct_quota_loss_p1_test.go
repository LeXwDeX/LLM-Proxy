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

func TestIssue72LostQuotaCellBlocksCalibrationWithoutRetry(t *testing.T) {
	for _, damage := range []string{"missing", "null"} {
		t.Run(damage, func(t *testing.T) {
			db := testOpenDB(t)
			m := newTestManager(t, db)
			putQuotaClient(t, db, []config.QuotaGroup{{Name: "models"}}, "employee")
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
			if used := bundleUsed(m); used != 2 {
				t.Fatalf("initial spend: %v", used)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				key := []byte(fmt.Sprintf("%s|alice|anthropic|executor", event.Timestamp.UTC().Format("2006-01-02T15")))
				b := tx.Bucket([]byte(nosql.BucketQuotaUsageAggHourly))
				if damage == "missing" {
					return b.Delete(key)
				}
				return b.Put(key, []byte("null"))
			}); err != nil {
				t.Fatal(err)
			}
			m.Evaluate("alice")
			if status := m.Status("alice"); status.CalibrationError == "" || bundleUsed(m) != 2 {
				t.Errorf("lost spend: %+v", status)
			}
			if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); !blocked || info.Scope != ScopeAccounting {
				t.Errorf("admitted: %+v %t", info, blocked)
			}
		})
	}
}
