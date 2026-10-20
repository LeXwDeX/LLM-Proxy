package nosql

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// seedLegacyBilling 种入 main 侧 B1/B2 数据（billing_templates、time_segments、
// groups、targets.group、per-target model_costs），模拟生产 DB。
func seedLegacyBilling(t *testing.T, db *bolt.DB) {
	t.Helper()
	put := func(bucket string, key string, v any) {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s/%s: %v", bucket, key, err)
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			b, err := tx.CreateBucketIfNotExists([]byte(bucket))
			if err != nil {
				return err
			}
			return b.Put([]byte(key), data)
		}); err != nil {
			t.Fatalf("seed %s/%s: %v", bucket, key, err)
		}
	}
	put("time_segments", "peak", legacyTimeSegment{Name: "peak", StartHour: 9, EndHour: 18, Days: []int{1, 2, 3, 4, 5}})
	put("time_segments", "night", legacyTimeSegment{Name: "night", StartHour: 22, EndHour: 24})
	put("time_segments", "promo", legacyTimeSegment{Name: "promo", StartHour: 10, EndHour: 12, Days: []int{6}, DateFrom: "2026-03-01", DateTo: "2026-03-15"})
	put("billing_templates", "std", legacyBillingTemplate{
		Name: "std",
		Rules: []legacyBillingRule{
			{Target: "*", Model: "gpt-4o", InputPer1MTokens: 10, OutputPer1MTokens: 20, CacheWrite1M: 3, CacheRead1M: 0.5},
			{Target: "*", Model: "gpt-4o", Segment: "peak", InputPer1MTokens: 20, OutputPer1MTokens: 40, CacheWrite1M: 6, CacheRead1M: 1},
			{Target: "ws-a", Model: "gpt-4o", InputPer1MTokens: 5, OutputPer1MTokens: 10, CacheWrite1M: 1.5, CacheRead1M: 0.25},
		},
	})
	put("billing_templates", "wild", legacyBillingTemplate{
		Name: "wild",
		Rules: []legacyBillingRule{
			{Target: "*", Model: "*", InputPer1MTokens: 1}, // 不可表达 → 跳过
			{Target: "*", Model: "gpt-4o", Segment: "night", InputPer1MTokens: 15, OutputPer1MTokens: 30, CacheWrite1M: 4, CacheRead1M: 0.6},
		},
	})
	put("groups", "wangsu", legacyConsumerGroup{Name: "wangsu", CostCoefficient: 0.8})
	put("groups", "full", legacyConsumerGroup{Name: "full"}) // 系数缺省 0 → 1
	put(BucketTargets, "ws-a", map[string]any{"name": "ws-a", "group": "wangsu", "endpoint": "https://a", "endpoint_type": "openai"})
	put(BucketTargets, "ws-b", map[string]any{"name": "ws-b", "group": "wangsu", "endpoint": "https://b", "endpoint_type": "openai"})
	// per-target model_costs 复合键条目。
	put(BucketModelCosts, "ws-z\x00claude-3", ModelCost{Target: "ws-z", Model: "claude-3", InputPer1MTokens: 3, OutputPer1MTokens: 15})
}

func TestMigrateBillingData(t *testing.T) {
	db := testDB(t)
	seedLegacyBilling(t, db)

	if err := MigrateBillingData(db, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// ---- pricing templates ----
	templates, err := NewPricingTemplateStore(db).List()
	if err != nil {
		t.Fatalf("list templates: %v", err)
	}
	byKey := map[string][]pricing.PricingTemplate{}
	for _, tpl := range templates {
		k := tpl.Target + "\x00" + tpl.Model
		byKey[k] = append(byKey[k], tpl)
	}

	// ("", gpt-4o)：template0（全天 $10）+ schedule（peak/night 两时段，BaseRates=全天价）。
	t0 := byKey["\x00gpt-4o"]
	var foundT0, foundSched bool
	for _, tpl := range t0 {
		if pricing.IsTemplate0(&tpl) {
			foundT0 = true
			if tpl.BaseRates.InputPer1MTokens != 10 {
				t.Fatalf("template0 rates wrong: %+v", tpl.BaseRates)
			}
		} else if tpl.Mode == pricing.ModeSchedule {
			foundSched = true
			if tpl.Timezone != "+08:00" {
				t.Fatalf("schedule timezone must be +08:00, got %q", tpl.Timezone)
			}
			if len(tpl.Periods) != 2 {
				t.Fatalf("expected 2 periods (peak + night), got %d", len(tpl.Periods))
			}
			if tpl.BaseRates.InputPer1MTokens != 10 {
				t.Fatalf("schedule base rates must be the all-day rates, got %+v", tpl.BaseRates)
			}
			var peakOK, nightOK bool
			for _, p := range tpl.Periods {
				if p.Start == "09:00" && p.End == "18:00" && len(p.Weekdays) == 5 && p.Rates.InputPer1MTokens == 20 {
					peakOK = true
				}
				if p.Start == "22:00" && p.End == "24:00" && len(p.Weekdays) == 7 && p.Rates.InputPer1MTokens == 15 {
					nightOK = true
				}
			}
			if !peakOK || !nightOK {
				t.Fatalf("periods wrong: %+v", tpl.Periods)
			}
			if !tpl.Start.Before(time.Now().UTC().Add(time.Minute)) || !tpl.Start.After(time.Now().UTC().Add(-48*time.Hour)) {
				t.Fatalf("schedule must be active from the migration instant, got %v", tpl.Start)
			}
		}
	}
	if !foundT0 || !foundSched {
		t.Fatalf("expected template0 + schedule for default target gpt-4o, got %+v", t0)
	}

	// ("ws-a", gpt-4o)：template0（全天 $5，来自精确规则）。
	wsa := byKey["ws-a\x00gpt-4o"]
	if len(wsa) != 1 || !pricing.IsTemplate0(&wsa[0]) || wsa[0].BaseRates.InputPer1MTokens != 5 {
		t.Fatalf("ws-a template0 wrong: %+v", wsa)
	}

	// ("ws-z", claude-3)：per-target model_costs → template0。
	wsz := byKey["ws-z\x00claude-3"]
	if len(wsz) != 1 || !pricing.IsTemplate0(&wsz[0]) || wsz[0].BaseRates.InputPer1MTokens != 3 {
		t.Fatalf("ws-z per-target template0 wrong: %+v", wsz)
	}

	// 无 wildcard-model 模板。
	if _, ok := byKey["\x00*"]; ok {
		t.Fatal("wildcard-model rule must be skipped")
	}

	// ---- model groups ----
	groups, err := NewModelGroupStore(db).List()
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	var foundWangsu, foundFull bool
	for _, g := range groups {
		switch strings.ToLower(g.Name) {
		case "wangsu":
			foundWangsu = true
			if g.CostCoefficient != 0.8 || len(g.Bindings) != 2 {
				t.Fatalf("wangsu group wrong: %+v", g)
			}
		case "full":
			foundFull = true
			if g.CostCoefficient != 1 {
				t.Fatalf("full group coefficient must default to 1, got %v", g.CostCoefficient)
			}
		}
	}
	if !foundWangsu || !foundFull {
		t.Fatalf("expected wangsu + full groups, got %+v", groups)
	}

	// ---- legacy buckets untouched（只读迁移）----
	for _, bucket := range []string{"billing_templates", "time_segments", "groups"} {
		if err := db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(bucket))
			if b == nil || b.Stats().KeyN == 0 {
				return errLegacyGone
			}
			return nil
		}); err != nil {
			t.Fatalf("legacy bucket %s must stay untouched: %v", bucket, err)
		}
	}

	// ---- 幂等：二次执行零写入 ----
	if err := MigrateBillingData(db, nil); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	templates2, err := NewPricingTemplateStore(db).List()
	if err != nil {
		t.Fatalf("relist: %v", err)
	}
	if len(templates2) != len(templates) {
		t.Fatalf("second run must be a no-op: %d vs %d", len(templates2), len(templates))
	}
}

func usage4Rates(in, out, cw, cr float64) usage.CostRates {
	return usage.CostRates{InputPer1MTokens: in, OutputPer1MTokens: out, CacheWritePer1MToken: cw, CacheReadPer1MToken: cr}
}

func modelGroupForTest(name string) config.ModelGroup {
	return config.ModelGroup{Name: name, CostCoefficient: 1}
}

var errLegacyGone = &legacyCheckError{}

type legacyCheckError struct{}

func (*legacyCheckError) Error() string { return "legacy bucket emptied" }

func TestMigrateBillingDataConflictKeepsTargetSide(t *testing.T) {
	db := testDB(t)

	// 目标侧已有 ("", gpt-4o) template0 和同名模型组。
	store := NewPricingTemplateStore(db)
	if _, err := store.Upsert(pricing.PricingTemplate{
		Target: "", Model: "gpt-4o", Mode: pricing.ModeFlat, Timezone: pricing.DefaultTimezone,
		BaseRates: usage4Rates(99, 99, 99, 99),
	}); err != nil {
		t.Fatalf("seed target-side template0: %v", err)
	}
	if err := NewModelGroupStore(db).Create(modelGroupForTest("wangsu")); err != nil {
		t.Fatalf("seed target-side group: %v", err)
	}

	seedLegacyBilling(t, db)
	if err := MigrateBillingData(db, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	templates, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, tpl := range templates {
		if tpl.Target == "" && pricing.IsTemplate0(&tpl) && tpl.BaseRates.InputPer1MTokens != 99 {
			t.Fatalf("existing target-side template0 must win, got %+v", tpl.BaseRates)
		}
	}
	groups, err := NewModelGroupStore(db).List()
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	for _, g := range groups {
		if strings.EqualFold(g.Name, "wangsu") && g.CostCoefficient == 0.8 {
			t.Fatalf("existing target-side group must win, got migrated %+v", g)
		}
	}
}
