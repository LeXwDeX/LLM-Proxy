package quota

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// Evaluate 的窗口化读取与原全量校验读取在多模型组、跨周期数据上逐项一致。
func TestCalibratedCountersWindowedMatchesFullValidation(t *testing.T) {
	db := testOpenDB(t)
	putJobLevel(t, db, config.JobLevel{Name: "L5"})
	putModelGroup(t, db, config.ModelGroup{Name: "g1", CostCoefficient: 0.5, DailyUSD: 50, WeeklyUSD: 200, MonthlyUSD: 500, Bindings: []config.QuotaGroupBinding{{Target: "tA"}}})
	putModelGroup(t, db, config.ModelGroup{Name: "g2", CostCoefficient: 2, DailyUSD: 50, Bindings: []config.QuotaGroupBinding{{Target: "tB", Model: "glm-max"}}})
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", JobLevel: "L5"})
	putClientQuotaGroups(t, db, "alice", []config.QuotaGroup{{Name: "g1"}, {Name: "g2"}})
	quotaTestCost(t, db, "qwen-max", "glm-max")
	store := nosql.NewUsageStore(db)
	now := time.Now().UTC()
	for i := 0; i < 400; i++ {
		e := usage.Event{
			Timestamp: now.Add(-time.Duration(i*263) * time.Minute), ClientName: "alice",
			Target: []string{"tA", "tB", "tC"}[i%3], Model: []string{"qwen-max", "glm-max"}[i%2],
			InputTokens: int64(2000 + i), OutputTokens: int64(50 + i%9), StatusCode: 200,
		}
		if err := store.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	m := newTestManager(t, db)
	qc, err := m.findQuotaConfig("alice")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := m.getResolver()
	if err != nil {
		t.Fatal(err)
	}
	read := func(f func(context.Context, usage.Filter, usage.RateResolver, bool) ([]nosql.CostUnit, error)) *clientCounters {
		cc, err := calibratedCounters("alice", qc, now, func(filter usage.Filter) ([]nosql.CostUnit, error) {
			return f(context.Background(), filter, resolver, true)
		})
		if err != nil {
			t.Fatal(err)
		}
		return cc
	}
	oldCC, newCC := read(store.ReadCostUnits), read(store.ReadQuotaCostUnits)
	if !reflect.DeepEqual(oldCC, newCC) {
		t.Fatalf("counters differ:\nold=%s\nnew=%s", dump(oldCC), dump(newCC))
	}
	var any bool
	for _, gc := range newCC.groups {
		for _, dc := range gc.dims {
			any = any || dc.used > 0
		}
	}
	if !any {
		t.Fatal("fixture produced no usage")
	}
}

func dump(cc *clientCounters) string {
	out := ""
	for n, gc := range cc.groups {
		for d, dc := range gc.dims {
			out += fmt.Sprintf("%s/%s used=%v limit=%v; ", n, d, dc.used, dc.limit)
		}
	}
	return out
}
