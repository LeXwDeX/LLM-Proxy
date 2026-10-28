package quota

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72OldSidecarMissingQuotaBlocksCalibrationAfterRestart(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "models"}}, "employee")
	putModelGroup(t, db, config.ModelGroup{Name: "models", CostCoefficient: 1, DailyUSD: 100, Bindings: []config.QuotaGroupBinding{{Target: "anthropic"}}})
	cache := pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) {
		return []pricing.PricingTemplate{{Target: "anthropic", Model: "executor", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}}}, nil
	}, nil)
	m.costCache = cache
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
		return tx.Bucket([]byte(nosql.BucketQuotaUsageAggHourly)).Delete([]byte(fmt.Sprintf("%s|alice|anthropic|executor", event.Timestamp.UTC().Format("2006-01-02T15"))))
	}); err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := json.Marshal(map[string]any{"reason": "identity_unverified", "record_id": event.RecordID, "client": "alice", "line": line})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "usage.journal")
	if err := os.WriteFile(path+".rejected", append(entry, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := nosql.NewUsageJournal(m.usageStore, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.usageJournal = journal
	m.Evaluate("alice")
	if status := m.Status("alice"); status.CalibrationError == "" || bundleUsed(m) != 2 {
		t.Fatalf("missing quota replaced counters: %+v", status)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	pathDB := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = nosql.OpenDB(pathDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	m = newTestManager(t, db)
	m.costCache = cache
	journal, err = nosql.NewUsageJournal(m.usageStore, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	m.usageJournal = journal
	m.Evaluate("alice")
	if status := m.Status("alice"); status.CalibrationError == "" {
		t.Fatalf("restart reported complete accounting: %+v", status)
	}
	if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); !blocked || info.Scope != ScopeAccounting {
		t.Fatalf("missing quota admitted: %+v blocked=%v", info, blocked)
	}
}
