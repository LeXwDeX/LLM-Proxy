package quota

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

// Issue #195: target rename/merge must keep historical pricing and quota
// aggregation consistent without offline bbolt edits.
//
// Fixture: target "old" priced at $2/1M input by a target template, a global
// template at $50/1M for the same model, and model group "models" (daily $100)
// bound to (old, executor). One historical event spends 1M input on "old".

const (
	renameTargetPrice = 2.0
	renameGlobalPrice = 50.0
)

type renameFixture struct {
	db        *bolt.DB
	m         *Manager
	targets   *nosql.TargetStore
	templates *nosql.PricingTemplateStore
	history   usage.Event
}

func renameTarget(name string) config.Target {
	return config.Target{
		Name: name, EndpointType: config.EndpointTypeClaude, Endpoint: "https://upstream.invalid",
		APIKey: "test-key", UsageAdapter: config.UsageAdapterStandard,
		ModelMappings: []config.ModelMapping{{Upstream: "executor"}},
	}
}

func newRenameFixture(t *testing.T) *renameFixture {
	t.Helper()
	db := testOpenDB(t)
	f := &renameFixture{db: db, m: newTestManager(t, db), targets: nosql.NewTargetStore(db), templates: nosql.NewPricingTemplateStore(db)}
	if err := f.targets.Create(renameTarget("old")); err != nil {
		t.Fatal(err)
	}
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "models"}}, "employee")
	if err := nosql.NewModelGroupStore(db).Create(config.ModelGroup{Name: "models", CostCoefficient: 1, DailyUSD: 100,
		Bindings: []config.QuotaGroupBinding{{Target: "old", Model: "executor"}}}); err != nil {
		t.Fatal(err)
	}
	for _, tmpl := range []pricing.PricingTemplate{
		{Targets: []string{"old"}, Models: []string{"executor"}, Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: renameTargetPrice}},
		{Targets: []string{""}, Models: []string{"executor"}, Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: renameGlobalPrice}},
	} {
		if _, err := f.templates.Upsert(tmpl); err != nil {
			t.Fatal(err)
		}
	}
	f.m.costCache = pricing.NewPricingCache(f.templates.List, nil)
	f.m.Evaluate("alice")
	f.history = usage.Event{RecordID: uuid.NewString(), Timestamp: time.Now().UTC(), ClientName: "alice", Target: "old",
		Model: "executor", InputTokens: 1_000_000, StatusCode: 200}
	if err := f.m.RecordUsage(f.m.usageStore, f.history); err != nil {
		t.Fatal(err)
	}
	if used := bundleUsed(f.m); used != renameTargetPrice {
		t.Fatalf("history spend before rename = %v, want %v", used, renameTargetPrice)
	}
	return f
}

func (f *renameFixture) recalibrate(t *testing.T) {
	t.Helper()
	f.m.costCache.Invalidate()
	f.m.Evaluate("alice")
	if status := f.m.Status("alice"); status.CalibrationError != "" {
		t.Fatalf("calibration failed after rename: %s", status.CalibrationError)
	}
}

func (f *renameFixture) recordOn(t *testing.T, target string) {
	t.Helper()
	event := usage.Event{RecordID: uuid.NewString(), Timestamp: time.Now().UTC(), ClientName: "alice", Target: target,
		Model: "executor", InputTokens: 1_000_000, StatusCode: 200}
	if err := f.m.RecordUsage(f.m.usageStore, event); err != nil {
		t.Fatal(err)
	}
}

func usageBucketsSnapshot(t *testing.T, db *bolt.DB) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	if err := db.View(func(tx *bolt.Tx) error {
		for _, name := range []string{nosql.BucketUsageEvents, nosql.BucketUsageAggHourly, nosql.BucketQuotaUsageAggHourly} {
			var buf bytes.Buffer
			if b := tx.Bucket([]byte(name)); b != nil {
				if err := b.ForEach(func(k, v []byte) error {
					fmt.Fprintf(&buf, "%s=%s\n", k, v)
					return nil
				}); err != nil {
					return err
				}
			}
			out[name] = buf.Bytes()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func equalSnapshots(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !bytes.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

// Reproduction of the 2026-11-10 incident step "pricing-clean": rebinding the
// template to the new name only makes the old-name history fall back to the
// global price. This documents the hazard the supported rename avoids.
func TestIssue195RebindingOnlyNewNameFallsBackToGlobalPrice(t *testing.T) {
	f := newRenameFixture(t)
	list, err := f.templates.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, tmpl := range list {
		if len(tmpl.Targets) == 1 && tmpl.Targets[0] == "old" {
			tmpl.Targets = []string{"new"}
			tmpl.Target = ""
			if _, err := f.templates.Upsert(tmpl); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.m.costCache.Invalidate()
	f.m.Evaluate("alice")
	if used := bundleUsed(f.m); used != renameGlobalPrice {
		t.Fatalf("history spend after new-name-only rebind = %v, want global fallback %v", used, renameGlobalPrice)
	}
}

// Reproduction of the incident step "quota-fix": deleting the old name's quota
// aggregation cell fails closed (accounting, served as 503). The supported
// rename must never need this.
func TestIssue195DeletingOldNameQuotaAggregationFailsClosed(t *testing.T) {
	f := newRenameFixture(t)
	key := []byte(fmt.Sprintf("%s|alice|old|executor", f.history.Timestamp.UTC().Format("2006-01-02T15")))
	if err := f.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(nosql.BucketQuotaUsageAggHourly)).Delete(key)
	}); err != nil {
		t.Fatal(err)
	}
	f.m.Evaluate("alice")
	if info, blocked := f.m.CheckTarget("alice", "old", "executor"); !blocked || info.Scope != ScopeAccounting {
		t.Fatalf("deleted aggregation admitted: %+v blocked=%t", info, blocked)
	}
}

func TestIssue195RenameKeepsHistoryPricingAndGroupWithoutTouchingUsage(t *testing.T) {
	f := newRenameFixture(t)
	before := usageBucketsSnapshot(t, f.db)
	if _, err := f.targets.Rename(nosql.TargetRenameRequest{From: "old", To: "new"}); err != nil {
		t.Fatal(err)
	}
	if after := usageBucketsSnapshot(t, f.db); !equalSnapshots(before, after) {
		t.Fatal("rename rewrote usage events or aggregation buckets")
	}
	f.recalibrate(t)
	if used := bundleUsed(f.m); used != renameTargetPrice {
		t.Fatalf("history spend after rename = %v, want target price %v (not global %v)", used, renameTargetPrice, renameGlobalPrice)
	}
	if info, blocked := f.m.CheckTarget("alice", "new", "executor"); blocked {
		t.Fatalf("renamed target blocked: %+v", info)
	}
	// New traffic on the new name keeps the same template and the same group.
	f.recordOn(t, "new")
	if used := bundleUsed(f.m); used != 2*renameTargetPrice {
		t.Fatalf("spend after new-name traffic = %v, want %v (new name must be priced and grouped like old)", used, 2*renameTargetPrice)
	}
	f.recalibrate(t)
	if used := bundleUsed(f.m); used != 2*renameTargetPrice {
		t.Fatalf("calibrated spend after new-name traffic = %v, want %v", used, 2*renameTargetPrice)
	}
}

func TestIssue195MergeKeepsSourceHistoryPricingWithoutTouchingUsage(t *testing.T) {
	f := newRenameFixture(t)
	if err := f.targets.Create(renameTarget("dest")); err != nil {
		t.Fatal(err)
	}
	before := usageBucketsSnapshot(t, f.db)
	res, err := f.targets.Rename(nosql.TargetRenameRequest{From: "old", To: "dest", Merge: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Merged {
		t.Fatalf("merge result not marked merged: %+v", res)
	}
	if after := usageBucketsSnapshot(t, f.db); !equalSnapshots(before, after) {
		t.Fatal("merge rewrote usage events or aggregation buckets")
	}
	list, err := f.targets.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range list {
		if target.Name == "old" {
			t.Fatal("merged source target still exists")
		}
	}
	f.recalibrate(t)
	if used := bundleUsed(f.m); used != renameTargetPrice {
		t.Fatalf("source history spend after merge = %v, want %v", used, renameTargetPrice)
	}
	if info, blocked := f.m.CheckTarget("alice", "dest", "executor"); blocked {
		t.Fatalf("merge destination blocked: %+v", info)
	}
}
