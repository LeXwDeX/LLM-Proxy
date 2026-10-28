package quota

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// A rejected identity remains unresolved after a process restart. Rebuilding
// counters from the now-empty quota rollup must not turn it into zero spend.
func TestIssue72RejectedIdentityBlocksCalibrationAfterRestart(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "models"}}, "employee")
	putModelGroup(t, db, config.ModelGroup{Name: "models", CostCoefficient: 1, DailyUSD: 100,
		Bindings: []config.QuotaGroupBinding{{Target: "anthropic"}}})
	resolver := pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) {
		return []pricing.PricingTemplate{{Target: "anthropic", Model: "executor", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}}}, nil
	}, nil)
	m.costCache = resolver
	event := quotaBundle()
	event.Components = nil
	if err := m.RecordUsage(m.usageStore, event); err != nil {
		t.Fatal(err)
	}
	if got := bundleUsed(m); got != 2 {
		t.Fatalf("fixture live usage=%v, want 2", got)
	}
	// Model an older receipt-less prune and a persisted v1 boundary. The
	// boundary also models an old scan clock ahead of this process's clock.
	if err := db.Update(func(tx *bolt.Tx) error {
		key := event.Timestamp.UTC().Format(time.RFC3339Nano) + "_" + event.RecordID
		if err := tx.Bucket([]byte(nosql.BucketUsageEvents)).Delete([]byte(key)); err != nil {
			return err
		}
		meta := tx.Bucket([]byte(nosql.BucketMeta))
		if err := meta.Put([]byte("usage_receipts_legacy_unknown_before_v1"), []byte(event.Timestamp.Add(time.Second).UTC().Format(time.RFC3339Nano))); err != nil {
			return err
		}
		if err := meta.Delete([]byte("usage_receipts_watermark_done_v2")); err != nil {
			return err
		}
		return meta.Put([]byte("usage_receipts_watermark_done_v1"), []byte("1"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := nosql.BackfillQuotaUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	store := nosql.NewUsageStore(db)
	path := filepath.Join(t.TempDir(), "usage.journal")
	writePendingJournalLine(t, path, event)
	journal, err := nosql.NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Replay(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".rejected"); err != nil {
		t.Fatalf("fixture did not produce a rejected sidecar: %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen the database and recreate the store, journal and manager.
	dbPath := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = nosql.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store = nosql.NewUsageStore(db)
	journal, err = nosql.NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	if _, err := journal.Replay(); err != nil {
		t.Fatal(err)
	}
	m = newTestManager(t, db)
	m.costCache = resolver
	m.usageStore = store
	m.usageJournal = journal
	from := event.Timestamp.UTC().Truncate(time.Hour)
	to := from.Add(time.Hour).Add(-time.Nanosecond)
	filter := usage.Filter{ClientName: "alice", From: &from, To: &to}
	if items, err := store.List(filter); err != nil || len(items) != 0 {
		t.Fatalf("unknown event became raw history: items=%+v err=%v", items, err)
	}
	if units, err := store.ReadCostUnits(context.Background(), filter, resolver.Get(), true); !errors.Is(err, nosql.ErrUsageHistoryIncomplete) || len(units) != 0 {
		t.Errorf("unresolved event reported complete price: units=%+v err=%v", units, err)
	}
	m.Evaluate("alice")
	if status := m.Status("alice"); status.CalibrationError == "" {
		t.Errorf("unresolved event calibrated as complete: %+v", status)
	}
	if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); !blocked || info.Scope != ScopeAccounting {
		t.Errorf("unresolved event admitted after restart: blocked=%t info=%+v", blocked, info)
	}
}

func TestIssue72CorruptQuotaRetainsCountersAndBlocksOnlyAffectedClient(t *testing.T) {
	for _, tc := range []struct {
		name        string
		coefficient float64
		rate        float64
	}{
		{"paid", 1, 2},
		{"zero_group_coefficient", 0, 2},
		{"zero_price", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testOpenDB(t)
			m := newTestManager(t, db)
			plan := []config.QuotaGroup{{Name: "models"}}
			putQuotaClient(t, db, plan, "employee")
			putClient(t, db, config.Client{Name: "bob", AccessKey: "test-bob", JobLevel: "employee"})
			putClientQuotaGroups(t, db, "bob", plan)
			putModelGroup(t, db, config.ModelGroup{Name: "models", CostCoefficient: tc.coefficient, DailyUSD: 100,
				Bindings: []config.QuotaGroupBinding{{Target: "anthropic"}}})
			m.costCache = pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) {
				return []pricing.PricingTemplate{{Target: "anthropic", Model: "executor", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: tc.rate}}}, nil
			}, nil)
			alice, bob := quotaBundle(), quotaBundle()
			alice.Components, bob.Components = nil, nil
			bob.ClientName = "bob"
			if err := m.RecordUsage(m.usageStore, alice); err != nil {
				t.Fatal(err)
			}
			if err := m.RecordUsage(m.usageStore, bob); err != nil {
				t.Fatal(err)
			}
			m.Evaluate("alice")
			before := bundleUsed(m)
			if before != tc.rate*tc.coefficient {
				t.Fatalf("fixture used=%v, want %v", before, tc.rate*tc.coefficient)
			}
			key := fmt.Sprintf("%s|alice|anthropic|executor", alice.Timestamp.UTC().Format("2006-01-02T15"))
			if err := db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(nosql.BucketQuotaUsageAggHourly)).Put([]byte(key), []byte("{broken"))
			}); err != nil {
				t.Fatal(err)
			}
			m.Evaluate("alice")
			status := m.Status("alice")
			if status.CalibrationError == "" || status.Groups["models"].Quotas[DimensionDaily].Used != before {
				t.Errorf("calibration did not retain counters and report incomplete history: %+v", status)
			}
			if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); !blocked || info.Scope != ScopeAccounting {
				t.Errorf("affected client admitted: blocked=%t info=%+v", blocked, info)
			}
			m.Evaluate("bob")
			if status := m.Status("bob"); status.CalibrationError != "" || status.Groups["models"].Quotas[DimensionDaily].Used != before {
				t.Errorf("unrelated client was affected: %+v", status)
			}
			if info, blocked := m.CheckTarget("bob", "anthropic", "executor"); blocked {
				t.Errorf("unrelated client blocked: %+v", info)
			}
		})
	}
}
