package quota

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestMinuteScheduleAccountingSurvivesCalibration(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "domestic"}}, "employee")
	putModelGroup(t, db, config.ModelGroup{Name: "domestic", CostCoefficient: 1, DailyUSD: 5,
		Bindings: []config.QuotaGroupBinding{{Target: "upstream", Model: "qwen-max"}}})
	at := time.Now().UTC()
	// A minute window ending immediately before the event gets the cheap
	// rate at the UTC hour boundary, while this event gets the base price 10.
	minute := at.Hour()*60 + at.Minute()
	if minute%60 == 0 {
		// A sub-hour coverage start also needs the exact timestamp path.
		minute = at.Hour()*60 + 2
	}
	template := pricing.PricingTemplate{Model: "qwen-max", Start: at.Add(-time.Hour), Mode: pricing.ModeSchedule,
		Timezone: "+00:00", BaseRates: usage.CostRates{InputPer1MTokens: 10}, Periods: []pricing.PricingPeriod{{
			Weekdays: []int{1, 2, 3, 4, 5, 6, 7}, Start: fmt.Sprintf("%02d:00", at.Hour()),
			End: fmt.Sprintf("%02d:%02d", minute/60, minute%60), Rates: usage.CostRates{InputPer1MTokens: 1},
		}}}
	if at.Minute() == 0 {
		template.Start = at.Add(-time.Nanosecond)
		template.Periods[0].Start = fmt.Sprintf("%02d:01", at.Hour())
	}
	if err := pricing.ValidateTemplate(&template); err != nil {
		t.Fatal(err)
	}
	m.costCache = pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) { return []pricing.PricingTemplate{template}, nil }, nil)
	m.Evaluate("alice")
	event := usage.Event{Timestamp: at, ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000, StatusCode: 200}
	if err := m.RecordUsage(m.usageStore, event); err != nil {
		t.Fatal(err)
	}
	before := m.Status("alice").Groups["domestic"].Quotas[DimensionDaily].Used
	m.Evaluate("alice")
	after := m.Status("alice").Groups["domestic"].Quotas[DimensionDaily].Used
	if before != 10 || after != 10 {
		t.Fatalf("calibration changed charge: before=%v after=%v", before, after)
	}
}

func TestCalibrationUnionCrossMonthWeekAndSingleCoefficient(t *testing.T) {
	for _, coefficient := range []float64{0, 0.25, 2} {
		t.Run(fmt.Sprint(coefficient), func(t *testing.T) {
			db := testOpenDB(t)
			m := newTestManager(t, db)
			putQuotaClient(t, db, []config.QuotaGroup{{Name: "domestic"}}, "employee")
			putModelGroup(t, db, config.ModelGroup{Name: "domestic", CostCoefficient: coefficient, DailyUSD: 100, WeeklyUSD: 300, MonthlyUSD: 500, Bindings: []config.QuotaGroupBinding{{Target: "upstream", Model: "qwen-max"}}})
			now := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
			day := now.Truncate(24 * time.Hour)
			week := day.AddDate(0, 0, -3) // Monday 28 Sep, before monthly start
			m.costCache = pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) {
				return []pricing.PricingTemplate{
					{Target: "upstream", Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}},
					{Target: "upstream", Model: "qwen-max", Start: day.Add(10*time.Hour + 30*time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 10}},
					{Target: "upstream", Model: "qwen-max", Start: day.Add(10*time.Hour + 45*time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{}},
				}, nil
			}, nil)
			var todayCost float64
			for _, at := range []time.Time{week.Add(-time.Nanosecond), week, day.Add(-time.Nanosecond), day, day.Add(10*time.Hour + 29*time.Minute + 59*time.Second), day.Add(10*time.Hour + 30*time.Minute), day.Add(10*time.Hour + 45*time.Minute)} {
				if err := m.usageStore.Record(usage.Event{Timestamp: at, ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}); err != nil {
					t.Fatal(err)
				}
				if !at.Before(day) {
					cost, err := m.computeCost("upstream", "qwen-max", at, 1_000_000, 0, 0, 0)
					if err != nil {
						t.Fatal(err)
					}
					todayCost += cost
				}
			}
			if err := nosql.MigrateUsageClientIndex(context.Background(), db, 2); err != nil {
				t.Fatal(err)
			}
			qc, err := m.findQuotaConfig("alice")
			if err != nil {
				t.Fatal(err)
			}
			r, err := m.getResolver()
			if err != nil {
				t.Fatal(err)
			}
			reads := 0
			cc, err := calibratedCounters("alice", qc, now, func(f usage.Filter) ([]nosql.CostUnit, error) {
				reads++
				if f.ClientName != "alice" || !f.From.Equal(week) || !f.To.Equal(now) {
					t.Fatalf("wrong union: %+v", f)
				}
				return m.usageStore.ReadCostUnits(context.Background(), f, r, true)
			})
			if err != nil || reads != 1 {
				t.Fatalf("union reads=%d err=%v", reads, err)
			}
			if todayCost != 12 {
				t.Fatalf("real-time prices=%v", todayCost)
			}
			for dim, want := range map[string]float64{DimensionDaily: 12, DimensionMonthly: 12, DimensionWeekly: 14} {
				if got := cc.groups["domestic"].dims[dim].used; got != want*coefficient {
					t.Fatalf("%s used=%v want %v", dim, got, want*coefficient)
				}
			}
		})
	}
}

func TestColdCalibrationFailureIsCoalescedAndBackedOff(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	var attempts atomic.Int64
	journal, err := nosql.NewUsageJournal(auditRecorderFunc(func(usage.Event) error {
		attempts.Add(1)
		return errors.New("primary unavailable")
	}), filepath.Join(t.TempDir(), "pending.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Record(usage.Event{ClientName: "alice", Model: "qwen-max"}); err != nil {
		t.Fatal(err)
	}
	m.usageJournal = journal
	attempts.Store(0)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if info, blocked := m.CheckTarget("alice", "upstream", "qwen-max"); !blocked || info.Scope != ScopeAccounting {
				t.Errorf("cold failure admitted: %+v %v", info, blocked)
			}
		}()
	}
	wg.Wait()
	m.Status("alice")
	if got := attempts.Load(); got != 1 {
		t.Fatalf("cold burst/status retried Drain %d times; want 1", got)
	}
}

func TestExactCalibrationMissingHistoryRetainsCountersAndBlocksUntilRecovery(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "domestic"}}, "employee")
	putModelGroup(t, db, config.ModelGroup{Name: "domestic", CostCoefficient: 1, DailyUSD: 5,
		Bindings: []config.QuotaGroupBinding{{Target: "upstream", Model: "qwen-max"}}})
	at := time.Now().UTC()
	cache := pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) {
		return []pricing.PricingTemplate{{Model: "qwen-max", Start: at.Add(-time.Nanosecond), Mode: pricing.ModeFlat,
			BaseRates: usage.CostRates{InputPer1MTokens: 10}}}, nil
	}, nil)
	m.costCache = cache
	m.Evaluate("alice")
	if err := m.RecordUsage(m.usageStore, usage.Event{Timestamp: at, ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	var rawKey, rawData []byte
	if err := db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(nosql.BucketUsageEvents))
		key, data := bucket.Cursor().First()
		rawKey, rawData = bytes.Clone(key), bytes.Clone(data)
		return bucket.Delete(key)
	}); err != nil {
		t.Fatal(err)
	}
	m.Evaluate("alice")
	status := m.Status("alice")
	if status.CalibrationError == "" || status.Exceeded == nil || status.Exceeded.Scope != ScopeAccounting || status.Groups["domestic"].Quotas[DimensionDaily].Used != 10 {
		t.Fatalf("failed calibration must keep the existing charge and expose error: %+v", status)
	}
	if info, blocked := m.CheckTarget("alice", "unrelated-target", "qwen-max"); !blocked || info.Scope != ScopeAccounting || info.Dimension != "unavailable" {
		t.Fatalf("unavailable accounting must block every target: %+v blocked=%v", info, blocked)
	}
	// The same missing-history condition must block a fresh manager before its
	// first request; Status must return diagnostics without recursive retries.
	cold := newTestManager(t, db)
	cold.costCache = cache
	if info, blocked := cold.CheckTarget("alice", "upstream", "qwen-max"); !blocked || info.Scope != ScopeAccounting {
		t.Fatalf("cold accounting failure was admitted: %+v blocked=%v", info, blocked)
	}
	if status := cold.Status("alice"); status.CalibrationError == "" || status.Exceeded == nil || status.Exceeded.Scope != ScopeAccounting {
		t.Fatalf("cold status must expose accounting failure: %+v", status)
	}
	// Restore only the lost raw bytes: Record would increment the existing
	// rollup a second time, so recovery does not re-submit an already charged event.
	if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(nosql.BucketUsageEvents)).Put(rawKey, rawData) }); err != nil {
		t.Fatal(err)
	}
	for _, manager := range []*Manager{m, cold} {
		manager.Evaluate("alice")
		status := manager.Status("alice")
		if status.CalibrationError != "" || status.Groups["domestic"].Quotas[DimensionDaily].Used != 10 {
			t.Fatalf("recovered calibration did not restore authoritative status: %+v", status)
		}
		if info, blocked := manager.CheckTarget("alice", "upstream", "qwen-max"); !blocked || info.Scope != ScopeModelGroup {
			t.Fatalf("recovery must restore the true quota result: %+v blocked=%v", info, blocked)
		}
	}
}

func TestCalibrationReadFailureBlocksColdAdmission(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if info, blocked := m.CheckTarget("alice", "upstream", "qwen-max"); !blocked || info.Scope != ScopeAccounting {
		t.Fatalf("unreadable configuration must not fail open: %+v blocked=%v", info, blocked)
	}
}

func TestPendingJournalCalibrationPreservesChargeUntilReplay(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "domestic"}}, "employee")
	putModelGroup(t, db, config.ModelGroup{Name: "domestic", CostCoefficient: 1, DailyUSD: 5,
		Bindings: []config.QuotaGroupBinding{{Target: "upstream", Model: "qwen-max"}}})
	quotaTestCost(t, db, "qwen-max")
	m.Evaluate("alice")
	var primaryUnavailable atomic.Bool
	primaryUnavailable.Store(true)
	journal, err := nosql.NewUsageJournal(accountingRetryRecorder{store: m.usageStore, auditRecorderFunc: func(event usage.Event) error {
		if primaryUnavailable.Load() {
			return errors.New("temporary primary write failure")
		}
		return m.usageStore.Record(event)
	}}, filepath.Join(t.TempDir(), "usage.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.usageJournal = journal
	if err := m.RecordUsage(journal, usage.Event{Timestamp: time.Now().UTC(), ClientName: "alice", Target: "upstream",
		EndpointType: "openai", Model: "qwen-max", InputTokens: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	m.Evaluate("alice")
	status := m.Status("alice")
	if status.CalibrationError == "" || status.Groups["domestic"].Quotas[DimensionDaily].Used != 10 {
		t.Fatalf("pending durable usage must remain charged and block calibration: %+v", status)
	}
	if info, blocked := m.CheckTarget("alice", "upstream", "qwen-max"); !blocked || info.Scope != ScopeAccounting {
		t.Fatalf("pending replay failure must block admission: %+v blocked=%v", info, blocked)
	}
	cold := newTestManager(t, db)
	cold.usageJournal = journal
	if info, blocked := cold.CheckTarget("alice", "upstream", "qwen-max"); !blocked || info.Scope != ScopeAccounting {
		t.Fatalf("pending replay failure must block cold admission: %+v blocked=%v", info, blocked)
	}
	primaryUnavailable.Store(false)
	for _, manager := range []*Manager{m, cold} {
		manager.Evaluate("alice")
		status := manager.Status("alice")
		if status.CalibrationError != "" || status.Groups["domestic"].Quotas[DimensionDaily].Used != 10 {
			t.Fatalf("drained journal must restore exact charge once: %+v", status)
		}
		if info, blocked := manager.CheckTarget("alice", "upstream", "qwen-max"); !blocked || info.Scope != ScopeModelGroup {
			t.Fatalf("replay recovery must restore true quota rejection: %+v blocked=%v", info, blocked)
		}
	}
}

func TestFinalReviewPricingLoadFailureMustRetainCharge(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "domestic"}}, "employee")
	putModelGroup(t, db, config.ModelGroup{Name: "domestic", CostCoefficient: 1, DailyUSD: 5, Bindings: []config.QuotaGroupBinding{{Target: "upstream", Model: "qwen-max"}}})
	store := nosql.NewPricingTemplateStore(db)
	_, err := store.Upsert(pricing.PricingTemplate{Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 10}})
	if err != nil {
		t.Fatal(err)
	}
	cache := pricing.NewPricingCache(store.List, nil)
	m.costCache = cache
	m.Evaluate("alice")
	if err := m.RecordUsage(m.usageStore, usage.Event{Timestamp: time.Now().UTC(), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	before := m.Status("alice").Groups["domestic"].Quotas[DimensionDaily].Used
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(nosql.BucketPricingTemplates)).Put([]byte("corrupt-template"), []byte("invalid JSON"))
	}); err != nil {
		t.Fatal(err)
	}
	cache.Invalidate()
	m.Evaluate("alice")
	status := m.Status("alice")
	info, blocked := m.CheckTarget("alice", "upstream", "qwen-max")
	if before != 10 || status.Groups["domestic"].Quotas[DimensionDaily].Used != 10 || status.CalibrationError == "" || !blocked || info.Scope != ScopeAccounting {
		t.Fatalf("before=%v after=%v error=%q blocked=%v scope=%q", before, status.Groups["domestic"].Quotas[DimensionDaily].Used, status.CalibrationError, blocked, info.Scope)
	}
}

func TestPricingSourceFailureBlocksCalibrationAndInflightAccountingUntilRecovery(t *testing.T) {
	for _, source := range []string{"templates", "providers", "legacy-costs"} {
		t.Run(source, func(t *testing.T) {
			db := testOpenDB(t)
			m := newTestManager(t, db)
			putQuotaClient(t, db, []config.QuotaGroup{{Name: "domestic"}}, "employee")
			putModelGroup(t, db, config.ModelGroup{Name: "domestic", CostCoefficient: 1, DailyUSD: 5,
				Bindings: []config.QuotaGroupBinding{{Target: "upstream", Model: "qwen-max"}}})
			bucketName := nosql.BucketModelCosts
			if source == "legacy-costs" {
				quotaTestCost(t, db, "qwen-max")
			} else {
				store := nosql.NewPricingTemplateStore(db)
				if _, err := store.Upsert(pricing.PricingTemplate{Model: "qwen-max", Mode: pricing.ModeFlat,
					BaseRates: usage.CostRates{InputPer1MTokens: 10}}); err != nil {
					t.Fatal(err)
				}
				bucketName = nosql.BucketPricingTemplates
				if source == "providers" {
					bucketName = nosql.BucketTargets
					targetStore := nosql.NewTargetStore(db)
					m.costCache = pricing.NewTargetPricingCache(store.List, nil, func() (map[string]string, error) {
						targets, err := targetStore.List()
						providers := make(map[string]string, len(targets))
						for _, target := range targets {
							providers[target.Name] = target.EndpointType
						}
						return providers, err
					})
				} else {
					m.costCache = pricing.NewPricingCache(store.List, nil)
				}
			}
			m.Evaluate("alice")
			event := usage.Event{Timestamp: time.Now().UTC(), ClientName: "alice", Target: "upstream",
				Model: "qwen-max", InputTokens: 1_000_000}
			if err := m.RecordUsage(m.usageStore, event); err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(bucketName)).Put([]byte("broken-price-source"), []byte("invalid JSON"))
			}); err != nil {
				t.Fatal(err)
			}
			if m.costCache != nil {
				m.costCache.Invalidate()
			}
			// A request admitted before the price source failed must still persist
			// its event, retain existing counters and block subsequent requests.
			if err := m.RecordUsage(m.usageStore, event); err != nil {
				t.Fatal(err)
			}
			assertUnavailable := func() {
				t.Helper()
				status := m.Status("alice")
				if status.CalibrationError == "" || status.Groups["domestic"].Quotas[DimensionDaily].Used != 10 {
					t.Fatalf("failed pricing source must retain counters and expose error: %+v", status)
				}
				if info, blocked := m.CheckTarget("alice", "upstream", "qwen-max"); !blocked || info.Scope != ScopeAccounting {
					t.Fatalf("failed pricing must block admission: %+v blocked=%v", info, blocked)
				}
			}
			assertUnavailable()
			m.Evaluate("alice")
			assertUnavailable()
			cold := newTestManager(t, db)
			cold.costCache = m.costCache
			if info, blocked := cold.CheckTarget("alice", "upstream", "qwen-max"); !blocked || info.Scope != ScopeAccounting {
				t.Fatalf("failed pricing source must block cold admission: %+v blocked=%v", info, blocked)
			}
			if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(bucketName)).Delete([]byte("broken-price-source")) }); err != nil {
				t.Fatal(err)
			}
			if info, blocked := m.CheckTarget("alice", "upstream", "qwen-max"); !blocked || info.Scope != ScopeAccounting {
				t.Fatalf("source repair must not clear the failure before reliable calibration: %+v blocked=%v", info, blocked)
			}
			// Failed loads must not be cached: repair alone allows reliable
			// calibration, including the event already persisted during failure.
			for _, manager := range []*Manager{m, cold} {
				manager.Evaluate("alice")
				status := manager.Status("alice")
				if status.CalibrationError != "" || status.Groups["domestic"].Quotas[DimensionDaily].Used != 20 {
					t.Fatalf("repaired pricing must recover all persisted charges: %+v", status)
				}
				if info, blocked := manager.CheckTarget("alice", "upstream", "qwen-max"); !blocked || info.Scope != ScopeModelGroup {
					t.Fatalf("recovery must restore real quota rejection: %+v blocked=%v", info, blocked)
				}
			}
		})
	}
}

func TestUnknownUnpricedModelRemainsUncharged(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "domestic"}}, "employee")
	putModelGroup(t, db, config.ModelGroup{Name: "domestic", CostCoefficient: 1, DailyUSD: 5,
		Bindings: []config.QuotaGroupBinding{{Target: "upstream", Model: "unknown-unpriced"}}})
	m.costCache = pricing.NewPricingCache(nil, nil)
	m.Evaluate("alice")
	if err := m.RecordUsage(m.usageStore, usage.Event{Timestamp: time.Now().UTC(), ClientName: "alice", Target: "upstream",
		Model: "unknown-unpriced", InputTokens: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	m.Evaluate("alice")
	status := m.Status("alice")
	if status.CalibrationError != "" || status.Groups["domestic"].Quotas[DimensionDaily].Used != 0 {
		t.Fatalf("a healthy source with an unpriced model must remain uncharged: %+v", status)
	}
	if info, blocked := m.CheckTarget("alice", "upstream", "unknown-unpriced"); blocked {
		t.Fatalf("unknown price must not create an accounting failure: %+v", info)
	}
}

func TestColdStatusReportsClientReadFailures(t *testing.T) {
	for _, failure := range []string{"closed-db", "corrupt-client"} {
		t.Run(failure, func(t *testing.T) {
			db := testOpenDB(t)
			m := newTestManager(t, db)
			if failure == "closed-db" {
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err := db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(nosql.BucketClients)).Put([]byte("alice"), []byte("invalid JSON"))
			}); err != nil {
				t.Fatal(err)
			}
			status := m.Status("alice")
			if status.CalibrationError == "" || status.Exceeded == nil || status.Exceeded.Scope != ScopeAccounting {
				t.Fatalf("cold Status must expose client read failure: %+v", status)
			}
		})
	}
}

func TestTransientCalibrationFailureRecoversOnNextAdmission(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "domestic"}}, "employee")
	putModelGroup(t, db, config.ModelGroup{Name: "domestic", CostCoefficient: 1, DailyUSD: 5,
		Bindings: []config.QuotaGroupBinding{{Target: "upstream", Model: "qwen-max"}}})
	m.Evaluate("alice") // warm counters: CheckTarget no longer takes the cold path
	m.calibrationFailed("alice", errors.New("transient read failure"))
	if info, blocked := m.CheckTarget("alice", "upstream", "qwen-max"); !blocked || info.Scope != ScopeAccounting {
		t.Fatalf("fresh failure must block within the retry back-off: %+v blocked=%v", info, blocked)
	}
	// Once the back-off has passed, the next admission recalibrates instead of
	// waiting for the hourly run.
	m.mu.Lock()
	m.calibrationRetryAt["alice"] = time.Now().Add(-time.Second)
	m.mu.Unlock()
	if info, blocked := m.CheckTarget("alice", "upstream", "qwen-max"); blocked {
		t.Fatalf("transient failure kept a warm client blocked: %+v", info)
	}
	if status := m.Status("alice"); status.CalibrationError != "" {
		t.Fatalf("successful recalibration left the error: %+v", status)
	}
}
