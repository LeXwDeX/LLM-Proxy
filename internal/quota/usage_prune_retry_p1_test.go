package quota

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72QuotaExactRetryAfterPrune(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
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
			event.Timestamp = time.Now().UTC().Add(-2 * time.Second)
			if err := m.RecordUsage(m.usageStore, event); err != nil {
				t.Fatal(err)
			}
			if got := bundleUsed(m); got != 2 {
				t.Fatalf("fixture spend=%v", got)
			}
			if _, err := m.usageStore.PruneEventsBefore(event.Timestamp.Add(2*time.Second), 100); err != nil {
				t.Fatal(err)
			}
			if missing {
				if err := db.Update(func(tx *bolt.Tx) error {
					return tx.Bucket([]byte(nosql.BucketQuotaUsageAggHourly)).Delete([]byte(fmt.Sprintf("%s|alice|anthropic|executor", event.Timestamp.UTC().Format("2006-01-02T15"))))
				}); err != nil {
					t.Fatal(err)
				}
			}
			journal, err := nosql.NewUsageJournal(m.usageStore, filepath.Join(t.TempDir(), "usage.journal"), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = journal.Close() })
			m.usageJournal = journal
			err = m.RecordUsage(journal, event)
			if missing && err == nil {
				t.Fatal("missing quota allowed retry")
			}
			if !missing && err != nil {
				t.Fatalf("healthy pruned retry blocked: %v", err)
			}
			m.Evaluate("alice")
			status := m.Status("alice")
			if missing && status.CalibrationError == "" {
				t.Fatalf("missing quota calibrated: %+v", status)
			}
			if !missing && (status.CalibrationError != "" || bundleUsed(m) != 2) {
				t.Fatalf("healthy prune changed spend: %+v", status)
			}
		})
	}
}

func TestIssue72QuotaModelConflictKeepsCommittedScopeBlocked(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	plan := []config.QuotaGroup{{Name: "models"}}
	putQuotaClient(t, db, plan, "employee")
	putClient(t, db, config.Client{Name: "bob", AccessKey: "test-bob", JobLevel: "employee"})
	putClientQuotaGroups(t, db, "bob", plan)
	putModelGroup(t, db, config.ModelGroup{Name: "models", CostCoefficient: 1, DailyUSD: 100, Bindings: []config.QuotaGroupBinding{{Target: "anthropic"}}})
	m.costCache = pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) {
		return []pricing.PricingTemplate{{Target: "anthropic", Model: "executor", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}}, {Target: "anthropic", Model: "advisor", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 5}}}, nil
	}, nil)
	m.Evaluate("alice")
	event := quotaBundle()
	event.Components = nil
	if err := m.RecordUsage(m.usageStore, event); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(nosql.BucketQuotaUsageAggHourly)).Delete([]byte(fmt.Sprintf("%s|alice|anthropic|executor", event.Timestamp.UTC().Format("2006-01-02T15"))))
	}); err != nil {
		t.Fatal(err)
	}
	journal, err := nosql.NewUsageJournal(m.usageStore, filepath.Join(t.TempDir(), "usage.journal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.usageJournal = journal
	changed := event
	changed.Model = "advisor"
	if err := m.RecordUsage(journal, changed); !errors.Is(err, nosql.ErrUsageRecordConflict) {
		t.Fatalf("conflict: %v", err)
	}
	m.Evaluate("alice")
	status := m.Status("alice")
	if status.CalibrationError == "" || bundleUsed(m) != 2 {
		t.Fatalf("lost original scope replaced counters: %+v", status)
	}
	m.Evaluate("bob")
	if status := m.Status("bob"); status.CalibrationError != "" {
		t.Fatalf("unrelated client blocked: %+v", status)
	}
}

func TestIssue72QuotaRemovedComponentCannotReduceCounters(t *testing.T) {
	db:=testOpenDB(t);m:=newTestManager(t,db)
	putQuotaClient(t,db,[]config.QuotaGroup{{Name:"models"}},"employee")
	putModelGroup(t,db,config.ModelGroup{Name:"models",CostCoefficient:1,DailyUSD:100,Bindings:[]config.QuotaGroupBinding{{Target:"anthropic"}}})
	m.costCache=pricing.NewPricingCache(func()([]pricing.PricingTemplate,error){return []pricing.PricingTemplate{{Target:"anthropic",Model:"executor",Mode:pricing.ModeFlat,BaseRates:usage.CostRates{InputPer1MTokens:2}},{Target:"anthropic",Model:"advisor",Mode:pricing.ModeFlat,BaseRates:usage.CostRates{InputPer1MTokens:5}}},nil},nil)
	m.Evaluate("alice");event:=quotaBundle()
	if err:=m.RecordUsage(m.usageStore,event);err!=nil{t.Fatal(err)}
	if got:=bundleUsed(m);got!=12{t.Fatalf("fixture spend=%v",got)}
	if err:=db.Update(func(tx *bolt.Tx)error{return tx.Bucket([]byte(nosql.BucketQuotaUsageAggHourly)).Delete([]byte(fmt.Sprintf("%s|alice|anthropic|advisor",event.Timestamp.UTC().Format("2006-01-02T15"))))});err!=nil{t.Fatal(err)}
	journal,err:=nosql.NewUsageJournal(m.usageStore,filepath.Join(t.TempDir(),"usage.journal"),nil);if err!=nil{t.Fatal(err)};m.usageJournal=journal
	candidate:=usage.CloneEvent(event);candidate.Components=nil
	if err:=m.RecordUsage(journal,candidate);!errors.Is(err,nosql.ErrUsageRecordConflict){t.Fatalf("removed component retry: %v",err)}
	m.Evaluate("alice");status:=m.Status("alice")
	if status.CalibrationError==""||bundleUsed(m)!=12{t.Fatalf("incomplete bundle changed quota: %+v",status)}
	if info,blocked:=m.CheckTarget("alice","anthropic","advisor");!blocked||info.Scope!=ScopeAccounting{t.Fatalf("incomplete bundle admitted: %+v %v",info,blocked)}
}
