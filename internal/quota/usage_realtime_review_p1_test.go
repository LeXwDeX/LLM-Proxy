package quota

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72RealtimeIdenticalRetryBlocksMissingAccounting(t *testing.T) {
	for _, useJournal := range []bool{false, true} {
		for _, damage := range []string{"missing", "wrong_totals"} {
			t.Run(fmt.Sprintf("journal_%t_%s", useJournal, damage), func(t *testing.T) {
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
				if got := bundleUsed(m); got != 2 {
					t.Fatalf("fixture spend=%v", got)
				}
				if err := db.Update(func(tx *bolt.Tx) error {
					bucket := tx.Bucket([]byte(nosql.BucketQuotaUsageAggHourly))
					key := []byte(fmt.Sprintf("%s|alice|anthropic|executor", event.Timestamp.UTC().Format("2006-01-02T15")))
					if damage == "missing" {
						return bucket.Delete(key)
					}
					var cell map[string]any
					if err := json.Unmarshal(bucket.Get(key), &cell); err != nil {
						return err
					}
					cell["input_tokens"] = float64(1)
					data, err := json.Marshal(cell)
					if err != nil {
						return err
					}
					return bucket.Put(key, data)
				}); err != nil {
					t.Fatal(err)
				}
				journal, err := nosql.NewUsageJournal(m.usageStore, filepath.Join(t.TempDir(), "usage.journal"), nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = journal.Close() })
				m.usageJournal = journal
				var recorder usage.Recorder = m.usageStore
				if useJournal {
					recorder = journal
				}
				if err := m.RecordUsage(recorder, event); err == nil {
					t.Fatal("identity-only retry did not report incomplete accounting")
				}
				if got := bundleUsed(m); got != 2 {
					t.Fatalf("retry changed live spend=%v", got)
				}
				if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); !blocked || info.Scope != ScopeAccounting {
					t.Fatalf("unconfirmed accounting admitted: %+v %v", info, blocked)
				}
			})
		}
	}
}
