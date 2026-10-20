package quota

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// --- test helpers ---

func testOpenDB(t testing.TB) *bolt.DB {
	t.Helper()
	db, err := nosql.OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// putAggCell writes a test AggCell directly into the target-aware quota hourly
// agg bucket (the source Evaluate reads via SumTargetModelRange).
// key = "<2006-01-02T15>|<client>|<target>|<model>"
func putAggCell(t testing.TB, db *bolt.DB, key string, cell nosql.AggCell) {
	t.Helper()
	err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(nosql.BucketQuotaUsageAggHourly))
		if err != nil {
			return err
		}
		data, err := json.Marshal(cell)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), data)
	})
	if err != nil {
		t.Fatalf("putAggCell %q: %v", key, err)
	}
}

// putClient writes a client directly to the clients bucket.
func putClient(t testing.TB, db *bolt.DB, c config.Client) {
	t.Helper()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal client %q: %v", c.Name, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(nosql.BucketClients)).Put([]byte(strings.ToLower(c.Name)), data)
	})
	if err != nil {
		t.Fatalf("putClient %q: %v", c.Name, err)
	}
}

// putClientQuotaGroups writes per-client plan entries directly into the
// client quota groups bucket (bypassing store validation, mirroring putClient).
func putClientQuotaGroups(t testing.TB, db *bolt.DB, clientName string, groups []config.QuotaGroup) {
	t.Helper()
	data, err := json.Marshal(struct {
		Name   string              `json:"name"`
		Groups []config.QuotaGroup `json:"groups"`
	}{Name: clientName, Groups: groups})
	if err != nil {
		t.Fatalf("marshal client quota groups %q: %v", clientName, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(nosql.BucketClientQuotaGroups)).Put([]byte(strings.ToLower(clientName)), data)
	})
	if err != nil {
		t.Fatalf("putClientQuotaGroups %q: %v", clientName, err)
	}
}

// putJobLevel writes a structured job level directly into the job_levels bucket.
// 系数指针语义：nil → 1（中性）；显式 0 → 该维度不限。
func putJobLevel(t testing.TB, db *bolt.DB, level config.JobLevel) {
	t.Helper()
	data, err := json.Marshal(level)
	if err != nil {
		t.Fatalf("marshal job level %q: %v", level.Name, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(nosql.BucketJobLevels)).Put([]byte(strings.ToLower(level.Name)), data)
	})
	if err != nil {
		t.Fatalf("putJobLevel %q: %v", level.Name, err)
	}
}

func f64(v float64) *float64 { return &v }

// putQuotaClient seeds client "alice" bound to jobLevel with the given plan.
func putQuotaClient(t testing.TB, db *bolt.DB, groups []config.QuotaGroup, jobLevel string) {
	t.Helper()
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", JobLevel: jobLevel})
	if jobLevel != "" {
		putJobLevel(t, db, config.JobLevel{Name: jobLevel})
	}
	putClientQuotaGroups(t, db, "alice", groups)
}

// putModelGroup writes a global model group directly into the model_groups
// bucket, bypassing store validation (mirrors putClient). Manager tests only
// need the catalog populated; ModelGroupStore validation is covered in the
// nosql package tests.
func putModelGroup(t testing.TB, db *bolt.DB, g config.ModelGroup) {
	t.Helper()
	data, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("marshal model group %q: %v", g.Name, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(nosql.BucketModelGroups)).Put([]byte(strings.ToLower(g.Name)), data)
	})
	if err != nil {
		t.Fatalf("putModelGroup %q: %v", g.Name, err)
	}
}

// putModelCost writes a model cost to the DB.
func putModelCost(t testing.TB, db *bolt.DB, mc nosql.ModelCost) {
	t.Helper()
	store := nosql.NewModelCostStore(db)
	if err := store.Upsert(mc); err != nil {
		t.Fatalf("putModelCost %v: %v", mc, err)
	}
}

// nowHourKey returns a key "YYYY-MM-DDTHH|client|target|model" for the current hour
// (target-aware quota hourly agg bucket layout).
func nowHourKey(target, client, model string) string {
	hour := time.Now().UTC().Truncate(time.Hour).Format("2006-01-02T15")
	return hour + "|" + client + "|" + target + "|" + model
}

func newTestManager(t testing.TB, db *bolt.DB) *Manager {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := New(Options{
		CostStore:        nosql.NewModelCostStore(db),
		UsageStore:       nosql.NewUsageStore(db),
		ClientStore:      nosql.NewClientStore(db),
		ModelGroupStore:  nosql.NewModelGroupStore(db),
		ClientQuotaStore: nosql.NewClientQuotaStore(db),
		JobLevelStore:    nosql.NewJobLevelStore(db),
		Logger:           logger,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func quotaTestCost(t testing.TB, db *bolt.DB, models ...string) {
	t.Helper()
	models = append([]string{"gpt-4o"}, models...)
	for _, model := range models {
		putModelCost(t, db, nosql.ModelCost{
			Model: model, InputPer1MTokens: 10, OutputPer1MTokens: 10,
		})
	}
}

func quotaTestInput(t testing.TB, m *Manager, client, target, model string, tokens int64) {
	t.Helper()
	m.IncrementForTarget(client, target, "openai", model, time.Now().UTC(), tokens, 0, 0, 0)
}

// --- quotaClientNames / RefreshAll ---

func TestManagerQuotaClientNamesSkipsArchivedAndRefreshAll(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	// 额度 v2：所有活跃客户端都进入名单（职级门槛适用于每个人）。
	putClient(t, db, config.Client{Name: "alive", AccessKey: "k1"})
	putClient(t, db, config.Client{Name: "grouped", AccessKey: "k2"})
	putModelGroup(t, db, config.ModelGroup{Name: "premium", CostCoefficient: 1})
	putClientQuotaGroups(t, db, "grouped", []config.QuotaGroup{{Name: "premium"}})
	// Archived entry (deleted client): present in the bucket but must be skipped.
	putClientQuotaGroups(t, db, "gone", []config.QuotaGroup{{Name: "premium"}})
	quotaStore := nosql.NewClientQuotaStore(db)
	if err := quotaStore.Archive(config.Client{Name: "gone", IPAddress: "192.0.2.7"}); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	names, err := m.quotaClientNames()
	if err != nil {
		t.Fatalf("quotaClientNames: %v", err)
	}
	if len(names) != 2 || names[0] != "alive" || names[1] != "grouped" {
		t.Fatalf("quotaClientNames = %v, want [alive grouped]", names)
	}

	// RefreshAll must evaluate active clients without panicking on archived entries.
	m.RefreshAll()

	m.mu.RLock()
	_, okAlive := m.counters["alive"]
	_, okGrouped := m.counters["grouped"]
	_, okGone := m.counters["gone"]
	m.mu.RUnlock()
	if !okAlive || !okGrouped {
		t.Fatalf("RefreshAll missed counters: alive=%v grouped=%v", okAlive, okGrouped)
	}
	if okGone {
		t.Fatalf("archived client got counters after RefreshAll")
	}
}

func TestManagerRefreshAllClearsRemovedConfig(t *testing.T) {
	db := testOpenDB(t)
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", JobLevel: "L5"})
	putJobLevel(t, db, config.JobLevel{Name: "L5"})
	putModelGroup(t, db, config.ModelGroup{Name: "plan", CostCoefficient: 1, DailyUSD: 5, Bindings: []config.QuotaGroupBinding{{Target: "x"}}})
	putClientQuotaGroups(t, db, "alice", []config.QuotaGroup{{Name: "plan"}})
	m := newTestManager(t, db)
	m.Evaluate("alice")

	m.mu.RLock()
	_, ok := m.counters["alice"]
	m.mu.RUnlock()
	if !ok {
		t.Fatal("expected alice counters after Evaluate")
	}

	// 客户端整体删除（记录删除 + plan 归档）后 RefreshAll 必须清掉内存状态。
	quotaStore := nosql.NewClientQuotaStore(db)
	if err := quotaStore.DeleteClientAndArchive(config.Client{Name: "alice", AccessKey: "k1"}); err != nil {
		t.Fatalf("delete+archive alice: %v", err)
	}

	m.RefreshAll()

	m.mu.RLock()
	_, stillThere := m.counters["alice"]
	_, exceeded := m.exceeded["alice"]
	m.mu.RUnlock()
	if stillThere {
		t.Fatal("expected alice counters cleared after client removed")
	}
	if exceeded {
		t.Fatal("expected alice exceeded cleared after client removed")
	}
}

// --- Evaluate：职级门槛 ---

func TestManagerEvaluate_ClientNotFound(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	// 模拟 client 之前有残留标记
	m.mu.Lock()
	m.exceeded["alice"] = ExceededInfo{
		Scope:     ScopeModelGroup,
		Dimension: DimensionDaily,
		Limit:     10.0,
		Used:      11.0,
		ResetsAt:  time.Now().UTC().Add(24 * time.Hour),
	}
	m.mu.Unlock()

	// "alice" 已被删除，Evaluate 应清除残留状态
	m.Evaluate("alice")

	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.exceeded["alice"]; ok {
		t.Error("expected exceeded entry for deleted client to be cleared")
	}
}

// TestManagerEvaluate_MissingJobLevelRejects：未绑职级 = 未配置额度 = 拒绝。
func TestManagerEvaluate_MissingJobLevelRejects(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1"})
	m.Evaluate("alice")

	info, exceeded := m.Check("alice")
	if !exceeded || info.Scope != ScopeJobLevel || info.Dimension != JobLevelUnassignedDimension {
		t.Fatalf("missing job level must reject: %+v, %v", info, exceeded)
	}
	if _, blocked := m.CheckTarget("alice", "any", "gpt-4o"); !blocked {
		t.Fatal("CheckTarget must block when job level is missing")
	}
}

func TestManagerCheckTarget_ColdClientRejectsMissingJobLevel(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1"})

	// No Start or Evaluate: this is a client omitted by bounded preload.
	info, blocked := m.CheckTarget("alice", "any", "gpt-4o")
	if !blocked || info.Scope != ScopeJobLevel || info.Dimension != JobLevelUnassignedDimension {
		t.Fatalf("cold client must reject without job level: %+v, %v", info, blocked)
	}
}

func TestManagerCheckTarget_ColdClientLoadsPersistedGroupUsage(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{Name: "plan", CostCoefficient: 1, DailyUSD: 10,
		Bindings: []config.QuotaGroupBinding{{Target: "openai"}}})
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "plan"}}, "L5")
	quotaTestCost(t, db)
	putAggCell(t, db, nowHourKey("openai", "alice", "gpt-4o"), nosql.AggCell{InputTokens: 1_000_000})

	m := newTestManager(t, db)
	info, blocked := m.CheckTarget("alice", "openai", "gpt-4o")
	if !blocked || info.Scope != ScopeModelGroup || info.GroupName != "plan" || info.Dimension != DimensionDaily {
		t.Fatalf("cold client must load persisted over-limit usage: %+v, %v", info, blocked)
	}
}

// TestManagerEvaluate_UnknownJobLevelRejects：绑定了不存在的职级同样拒绝。
func TestManagerEvaluate_UnknownJobLevelRejects(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", JobLevel: "ghost"})
	m.Evaluate("alice")

	info, exceeded := m.Check("alice")
	if !exceeded || info.Scope != ScopeJobLevel {
		t.Fatalf("unknown job level must reject: %+v, %v", info, exceeded)
	}
}

// TestManagerCheck_JobLevelRejectionPersistsAcrossFlip：职级拒绝与周期无关，
// 惰性翻转不得清除（ResetsAt 为零值）。
func TestManagerCheck_JobLevelRejectionPersistsAcrossFlip(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1"})
	m.Evaluate("alice")

	// 连续两次 Check（第二次可能触发 flip 路径）都必须保持拒绝。
	for i := 0; i < 2; i++ {
		info, exceeded := m.Check("alice")
		if !exceeded || info.Scope != ScopeJobLevel {
			t.Fatalf("check #%d lost job-level rejection: %+v, %v", i, info, exceeded)
		}
	}

	// 绑定职级后恢复放行。
	putJobLevel(t, db, config.JobLevel{Name: "L5"})
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", JobLevel: "L5"})
	m.Evaluate("alice")
	if _, exceeded := m.Check("alice"); exceeded {
		t.Fatal("bound job level must clear the rejection")
	}
}

// --- Evaluate：额度公式 ---

// TestManagerEvaluate_EffectiveLimitFormula 验证核心公式：
// 有效额度(dim) = 组基数(dim) × 职级系数(dim) × 个人系数。
func TestManagerEvaluate_EffectiveLimitFormula(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{
		Name: "sub", CostCoefficient: 0.5,
		DailyUSD: 30, WeeklyUSD: 150, MonthlyUSD: 500,
		Bindings: []config.QuotaGroupBinding{{Target: "ali"}},
	})
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", JobLevel: "L5"})
	putJobLevel(t, db, config.JobLevel{Name: "L5", CoefficientDaily: f64(2)}) // weekly/monthly nil → 1
	putClientQuotaGroups(t, db, "alice", []config.QuotaGroup{{Name: "sub", Coefficient: f64(0.5)}})
	m := newTestManager(t, db)
	m.Evaluate("alice")

	group := m.Status("alice").Groups["sub"]
	want := map[string]float64{
		DimensionDaily:   30 * 2 * 0.5,  // 30
		DimensionWeekly:  150 * 1 * 0.5, // 75
		DimensionMonthly: 500 * 1 * 0.5, // 250
	}
	for dim, expect := range want {
		if got := group.Quotas[dim].Limit; got != expect {
			t.Errorf("%s limit = %v, want %v", dim, got, expect)
		}
	}
	// 透明字段（UI 公式展示）。
	if group.Coefficient != 0.5 || group.PlanCoefficient != 0.5 || !group.InPlan {
		t.Errorf("transparency fields wrong: %+v", group)
	}
	if group.RankCoefficients[DimensionDaily] != 2 || group.RankCoefficients[DimensionWeekly] != 1 {
		t.Errorf("rank coefficients wrong: %+v", group.RankCoefficients)
	}
	if group.BaseLimits[DimensionDaily] != 30 {
		t.Errorf("base limits wrong: %+v", group.BaseLimits)
	}
}

func TestManagerRefreshAll_JobLevelRaisesLimitWithoutCancellingGroupDiscount(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{
		Name: "subscription", CostCoefficient: 0.5, WeeklyUSD: 50,
		Bindings: []config.QuotaGroupBinding{{Target: "openai"}},
	})
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "subscription"}}, "supervisor")
	quotaTestCost(t, db)
	putAggCell(t, db, nowHourKey("openai", "alice", "gpt-4o"), nosql.AggCell{InputTokens: 27_000_000})
	m := newTestManager(t, db)

	info, blocked := m.CheckTarget("alice", "openai", "gpt-4o")
	if !blocked || info.Dimension != DimensionWeekly || info.Limit != 50 || info.Used != 135 {
		t.Fatalf("initial weekly admission = %+v, %v; want used 135 against limit 50", info, blocked)
	}
	_, err := nosql.NewJobLevelStore(db).Update("supervisor", config.JobLevel{
		Name: "supervisor", CoefficientDaily: f64(5), CoefficientWeekly: f64(5), CoefficientMonthly: f64(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	m.RefreshAll()
	weekly := m.Status("alice").Groups["subscription"].Quotas[DimensionWeekly]
	if weekly.Limit != 250 || weekly.Used != 135 {
		t.Fatalf("after supervisor update weekly = %+v; want used 135 against limit 250", weekly)
	}
	if info, blocked := m.CheckTarget("alice", "openai", "gpt-4o"); blocked {
		t.Fatalf("supervisor coefficient should unblock discounted usage: %+v", info)
	}
}

// TestManagerEvaluate_ZeroCoefficientMeansUnlimited：任一系数为 0 → 该维度不限。
func TestManagerEvaluate_ZeroCoefficientMeansUnlimited(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{
		Name: "sub", CostCoefficient: 1,
		DailyUSD: 30, WeeklyUSD: 150, MonthlyUSD: 500,
		Bindings: []config.QuotaGroupBinding{{Target: "ali"}},
	})
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", JobLevel: "L5"})
	putJobLevel(t, db, config.JobLevel{Name: "L5", CoefficientDaily: f64(0)}) // 日维度不限
	putClientQuotaGroups(t, db, "alice", []config.QuotaGroup{{Name: "sub"}})
	quotaTestCost(t, db)
	m := newTestManager(t, db)
	m.Evaluate("alice")

	group := m.Status("alice").Groups["sub"]
	if group.Quotas[DimensionDaily].Limit != 0 {
		t.Fatalf("rank coefficient 0 must make daily unlimited, got %v", group.Quotas[DimensionDaily].Limit)
	}
	if group.Quotas[DimensionWeekly].Limit != 150 {
		t.Fatalf("weekly limit = %v, want 150", group.Quotas[DimensionWeekly].Limit)
	}

	// 大量消费不会触发日维度超限。
	quotaTestInput(t, m, "alice", "ali", "gpt-4o", 100_000_000) // $1000 raw
	if info, exceeded := m.CheckTarget("alice", "ali", "gpt-4o"); exceeded && info.Dimension == DimensionDaily {
		t.Fatalf("daily must stay unlimited: %+v", info)
	}
}

func TestManagerEvaluate_ZeroGroupCoefficientMeansUnlimitedAndNoFoldedUsage(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{Name: "free", CostCoefficient: 0, DailyUSD: 10,
		Bindings: []config.QuotaGroupBinding{{Target: "openai"}}})
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "free"}}, "L5")
	quotaTestCost(t, db)
	putAggCell(t, db, nowHourKey("openai", "alice", "gpt-4o"), nosql.AggCell{InputTokens: 1_000_000})

	m := newTestManager(t, db)
	m.Evaluate("alice")
	group := m.Status("alice").Groups["free"]
	if got := group.Quotas[DimensionDaily]; got.Limit != 0 || got.Used != 0 {
		t.Fatalf("zero group coefficient must yield unlimited and zero folded cost: %+v", got)
	}
	if info, blocked := m.CheckTarget("alice", "openai", "gpt-4o"); blocked {
		t.Fatalf("zero group coefficient must not reject: %+v", info)
	}
}

func TestFallbackGroupPreservesFreeCoefficient(t *testing.T) {
	group := newFallbackGroupCounters(0, time.Now())
	if group.coefficient != 0 {
		t.Fatalf("free group coefficient = %v, want 0", group.coefficient)
	}
	if group.dims[DimensionDaily].limit != 0 {
		t.Fatalf("free fallback group must remain unlimited")
	}
}

// TestManagerEvaluate_FoldedDeduction：扣费 = 计费原价 × 组系数（折算口径），
// 与有效额度同币种。
func TestManagerEvaluate_FoldedDeduction(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{
		Name: "sub", CostCoefficient: 0.3, DailyUSD: 100,
		Bindings: []config.QuotaGroupBinding{{Target: "ali"}},
	})
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", JobLevel: "L5"})
	putJobLevel(t, db, config.JobLevel{Name: "L5"})
	putClientQuotaGroups(t, db, "alice", []config.QuotaGroup{{Name: "sub"}})
	quotaTestCost(t, db)
	usageStore := nosql.NewUsageStore(db)
	if err := usageStore.Record(usage.Event{
		Timestamp: time.Now().UTC(), ClientName: "alice", Target: "ali",
		Model: "gpt-4o", EndpointType: "openai", InputTokens: 1_000_000, StatusCode: 200,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	m := newTestManager(t, db)
	m.Evaluate("alice")

	// $10 raw × 0.3 = $3 folded；limit = 100 × 1 × 1 = 100。
	group := m.Status("alice").Groups["sub"]
	if got := group.Quotas[DimensionDaily].Used; got != 3 {
		t.Fatalf("folded used = %v, want 3", got)
	}
	if got := group.Quotas[DimensionDaily].Limit; got != 100 {
		t.Fatalf("effective limit = %v, want 100", got)
	}
}

func TestManagerEvaluate_UnlimitedExplicitGroupShowsFoldedUsage(t *testing.T) {
	db := testOpenDB(t)
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", JobLevel: "L5"})
	putJobLevel(t, db, config.JobLevel{Name: "L5"})
	putModelGroup(t, db, config.ModelGroup{Name: "unlimited", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "openai"}}})
	putClientQuotaGroups(t, db, "alice", []config.QuotaGroup{{Name: "unlimited"}})
	quotaTestCost(t, db)
	usageStore := nosql.NewUsageStore(db)
	if err := usageStore.Record(usage.Event{
		Timestamp: time.Now().UTC(), ClientName: "alice", Target: "openai",
		Model: "gpt-4o", EndpointType: "openai", InputTokens: 1_000_000, StatusCode: 200,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	m := newTestManager(t, db)
	m.Evaluate("alice")
	status := m.Status("alice")
	group, ok := status.Groups["unlimited"]
	if !ok {
		t.Fatal("unlimited explicit group missing from status")
	}
	for _, dim := range []string{DimensionDaily, DimensionWeekly, DimensionMonthly} {
		q, ok := group.Quotas[dim]
		if !ok {
			t.Fatalf("unlimited group missing %s counter", dim)
		}
		if q.Limit != 0 || q.Used != 10 {
			t.Fatalf("unlimited group %s = %+v, want limit=0 used=10", dim, q)
		}
	}
	if group.Exceeded != nil {
		t.Fatalf("unlimited group must not be exceeded: %+v", *group.Exceeded)
	}
}

func TestManagerEvaluate_CompanyGeneralShowsFoldedUsage(t *testing.T) {
	db := testOpenDB(t)
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", JobLevel: "L5"})
	putJobLevel(t, db, config.JobLevel{Name: "L5"})
	quotaTestCost(t, db)
	usageStore := nosql.NewUsageStore(db)
	if err := usageStore.Record(usage.Event{
		Timestamp: time.Now().UTC(), ClientName: "alice", Target: "unbound",
		Model: "gpt-4o", EndpointType: "openai", InputTokens: 2_000_000, StatusCode: 200,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	m := newTestManager(t, db)
	m.Evaluate("alice")
	status := m.Status("alice")
	group, ok := status.Groups["company_general"]
	if !ok {
		t.Fatal("company_general missing from status after ungrouped usage")
	}
	if group.InPlan {
		t.Fatal("company_general fallback must not be marked in-plan")
	}
	for _, dim := range []string{DimensionDaily, DimensionWeekly, DimensionMonthly} {
		q, ok := group.Quotas[dim]
		if !ok {
			t.Fatalf("company_general missing %s counter", dim)
		}
		if q.Limit != 0 || q.Used != 20 {
			t.Fatalf("company_general %s = %+v, want limit=0 used=20", dim, q)
		}
	}
}

func TestManagerIncrement_CompanyGeneralWithoutConfiguredQuota(t *testing.T) {
	db := testOpenDB(t)
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", JobLevel: "L5"})
	putJobLevel(t, db, config.JobLevel{Name: "L5"})
	quotaTestCost(t, db)
	m := newTestManager(t, db)

	quotaTestInput(t, m, "alice", "unbound", "gpt-4o", 1_000_000)
	group, ok := m.Status("alice").Groups["company_general"]
	if !ok {
		t.Fatal("company_general missing after first ungrouped request")
	}
	for _, dim := range []string{DimensionDaily, DimensionWeekly, DimensionMonthly} {
		q, ok := group.Quotas[dim]
		if !ok || q.Limit != 0 || q.Used != 10 {
			t.Fatalf("company_general %s = %+v, present=%v; want limit=0 used=10", dim, q, ok)
		}
	}
}

// TestManagerEvaluate_GroupExceedsLimit：组维度超限进入 groupExceeded，
// CheckTarget 拦截该组、不影响其他组。
func TestManagerEvaluate_GroupExceedsLimit(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{
		Name: "plan", CostCoefficient: 1, DailyUSD: 10,
		Bindings: []config.QuotaGroupBinding{{Target: "openai"}},
	})
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "plan"}}, "L5")
	putModelCost(t, db, nosql.ModelCost{
		EndpointType: "openai", Model: "gpt-4o",
		InputPer1MTokens: 10, OutputPer1MTokens: 10,
	})
	// 500K input + 500K output = $5 + $5 = $10 → used >= limit → exceeded
	putAggCell(t, db, nowHourKey("openai", "alice", "gpt-4o"), nosql.AggCell{
		InputTokens:  500_000,
		OutputTokens: 500_000,
	})

	m := newTestManager(t, db)
	m.Evaluate("alice")

	info, exceeded := m.CheckTarget("alice", "openai", "gpt-4o")
	if !exceeded {
		t.Fatal("expected group exceeded for usage=$10, limit=$10")
	}
	if info.Scope != ScopeModelGroup || info.GroupName != "plan" || info.Dimension != DimensionDaily {
		t.Fatalf("exceeded info = %+v, want model_group/plan/daily", info)
	}
	if info.Limit != 10 {
		t.Errorf("expected limit=10, got %f", info.Limit)
	}
}

func TestManagerEvaluate_UnderLimit(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{
		Name: "plan", CostCoefficient: 1, DailyUSD: 100,
		Bindings: []config.QuotaGroupBinding{{Target: "openai"}},
	})
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "plan"}}, "L5")
	putModelCost(t, db, nosql.ModelCost{
		EndpointType: "openai", Model: "gpt-4o",
		InputPer1MTokens: 10, OutputPer1MTokens: 10,
	})
	putAggCell(t, db, nowHourKey("openai", "alice", "gpt-4o"), nosql.AggCell{
		InputTokens: 500_000, // $5
	})

	m := newTestManager(t, db)
	m.Evaluate("alice")

	if _, exceeded := m.CheckTarget("alice", "openai", "gpt-4o"); exceeded {
		t.Error("expected no exceeded for usage=$5, limit=$100")
	}
}

func TestQuotaGroupForPrefersExactModelBinding(t *testing.T) {
	now := time.Now().UTC()
	cc := &clientCounters{groups: make(map[string]*groupCounters)}
	cc.groups["target"] = buildGroupCounters(
		config.QuotaGroup{Name: "target"},
		config.ModelGroup{Name: "target", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "openai"}}},
		config.JobLevel{}, now)
	cc.groups["exact"] = buildGroupCounters(
		config.QuotaGroup{Name: "exact"},
		config.ModelGroup{Name: "exact", CostCoefficient: 2, Bindings: []config.QuotaGroupBinding{{Target: "openai", Model: "gpt-4o"}}},
		config.JobLevel{}, now)
	name, coefficient := quotaGroupForCounters(cc, "OPENAI", "GPT-4O")
	if name != "exact" || coefficient != 2 {
		t.Fatalf("exact binding = %q, %v; want exact, 2", name, coefficient)
	}
	name, coefficient = quotaGroupForCounters(cc, "openai", "gpt-4.1")
	if name != "target" || coefficient != 1 {
		t.Fatalf("target binding = %q, %v; want target, 1", name, coefficient)
	}
}

// TestManagerEvaluate_CacheTokensUseSeparatePrices guards the quota pricing
// regression from commit 90780b1: Evaluate() and Status() read DB aggregates
// and must multiply CachedTokens at CacheReadPer1MToken and
// CacheCreationTokens at CacheWritePer1MToken — not reuse CachedInput for
// both. If the formula collapses to old single-price behaviour, the per-
// request Increment path and the per-hour Evaluate path diverge, and clients
// are either over-charged or under-charged depending on which path ran last.
func TestManagerEvaluate_CacheTokensUseSeparatePrices(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{
		Name: "claude-plan", CostCoefficient: 1, DailyUSD: 19,
		Bindings: []config.QuotaGroupBinding{{Target: "claude"}},
	})
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "claude-plan"}}, "L5")
	putModelCost(t, db, nosql.ModelCost{
		EndpointType:         "claude",
		Model:                "claude-opus-4-1",
		InputPer1MTokens:     15,
		OutputPer1MTokens:    75,
		CacheWritePer1MToken: 18.75, // cache WRITE price
		CacheReadPer1MToken:  1.50,  // cache READ price — 12.5× cheaper than write
	})

	// Agg cell that exercises BOTH cache dimensions.
	//   1M input tokens  × $15    /1M = $15.00
	//   0  output tokens × $75    /1M =  $0.00
	//   1M cache-read    × $1.50  /1M =  $1.50   (correct price)
	//   1M cache-creation× $18.75 /1M = $18.75
	//   Total CORRECT = $35.25 ; total WRONG (read priced as write) = $52.50
	putAggCell(t, db, nowHourKey("claude", "alice", "claude-opus-4-1"), nosql.AggCell{
		InputTokens:         1_000_000,
		CachedTokens:        1_000_000,
		CacheCreationTokens: 1_000_000,
	})

	m := newTestManager(t, db)
	m.Evaluate("alice")

	m.mu.RLock()
	cc := m.counters["alice"]
	var used float64
	if cc != nil {
		if gc := cc.groups["claude-plan"]; gc != nil {
			if dc := gc.dims[DimensionDaily]; dc != nil {
				used = dc.used
			}
		}
	}
	m.mu.RUnlock()

	const wantUsed = 35.25
	if used < wantUsed-1e-9 || used > wantUsed+1e-9 {
		t.Fatalf("Evaluate used = %f, want exactly %f (wrong formula likely applies CachedInput to CachedTokens)", used, wantUsed)
	}
	info, exceeded := m.CheckTarget("alice", "claude", "claude-opus-4-1")
	if !exceeded {
		t.Fatalf("expected exceeded at limit=$19 with used=$%.2f", used)
	}
	if info.Dimension != DimensionDaily {
		t.Errorf("expected dimension=daily, got %q", info.Dimension)
	}
}

// TestManagerIncrementConsistentWithEvaluate asserts that the in-memory
// Increment path (post-request) and the authoritative Evaluate/Status path
// (DB back-read) produce the same USD total for identical token counts —
// otherwise a process restart or hourly calibration would silently rewrite
// the client's quota consumption.
func TestManagerIncrementConsistentWithEvaluate(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", JobLevel: "L5"})
	putJobLevel(t, db, config.JobLevel{Name: "L5"})
	putModelCost(t, db, nosql.ModelCost{
		EndpointType:         "claude",
		Model:                "claude-opus-4-1",
		InputPer1MTokens:     15,
		OutputPer1MTokens:    75,
		CacheWritePer1MToken: 18.75,
		CacheReadPer1MToken:  1.50,
	})

	// First, record via Increment (no group bindings → company_general, 系数 1).
	m.Increment("alice", "claude", "claude-opus-4-1",
		/*input*/ 1_000_000 /*output*/, 2_000_000 /*cacheRead*/, 500_000 /*cacheCreation*/, 3_000_000)

	m.mu.RLock()
	incrementUsed := m.counters["alice"].groups["company_general"].dims[DimensionDaily].used
	m.mu.RUnlock()

	// Now simulate the post-restart / calibration path: write the same usage
	// into the agg bucket and run Evaluate; it must match Increment exactly.
	putAggCell(t, db, nowHourKey("claude", "alice", "claude-opus-4-1"), nosql.AggCell{
		InputTokens:         1_000_000,
		OutputTokens:        2_000_000,
		CachedTokens:        500_000,
		CacheCreationTokens: 3_000_000,
	})

	m.Evaluate("alice")

	m.mu.RLock()
	evaluateUsed := m.counters["alice"].groups["company_general"].dims[DimensionDaily].used
	m.mu.RUnlock()

	const eps = 1e-9
	if incrementUsed < eps {
		t.Fatalf("Increment produced zero used — sanity check failed")
	}
	diff := incrementUsed - evaluateUsed
	if diff > eps || diff < -eps {
		t.Fatalf("Increment used=$%.9f vs Evaluate used=$%.9f diverge by $%.9f — cache token pricing is inconsistent between paths",
			incrementUsed, evaluateUsed, diff)
	}
}

func TestManagerEvaluate_DailyAndWeeklyBothExceeded(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{
		Name: "plan", CostCoefficient: 1, DailyUSD: 10, WeeklyUSD: 10,
		Bindings: []config.QuotaGroupBinding{{Target: "openai"}},
	})
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "plan"}}, "L5")
	putModelCost(t, db, nosql.ModelCost{
		EndpointType: "openai", Model: "gpt-4o",
		InputPer1MTokens: 10, OutputPer1MTokens: 10,
	})
	// $10 usage
	putAggCell(t, db, nowHourKey("openai", "alice", "gpt-4o"), nosql.AggCell{
		InputTokens:  500_000,
		OutputTokens: 500_000,
	})

	m := newTestManager(t, db)
	m.Evaluate("alice")

	m.mu.RLock()
	info := m.groupExceeded["alice"]["plan"]
	m.mu.RUnlock()
	if info.GroupName == "" {
		t.Fatal("expected group exceeded")
	}
	// Daily should be recorded first (deterministic daily→weekly→monthly order)
	if info.Dimension != DimensionDaily {
		t.Errorf("expected first-exceeded dimension=daily, got %q", info.Dimension)
	}
}

func TestManagerEvaluate_AutoClearOnCycle(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	// Manually set a non-job-level exceeded entry with past ResetsAt (no counters)
	m.mu.Lock()
	m.exceeded["alice"] = ExceededInfo{
		Scope:     ScopeModelGroup,
		Dimension: DimensionDaily,
		Limit:     10,
		Used:      15,
		ResetsAt:  time.Now().UTC().Add(-time.Hour), // in the past
	}
	m.mu.Unlock()

	// Check should auto-clear (ResetsAt in past)
	if _, exceeded := m.Check("alice"); exceeded {
		t.Error("expected Check to auto-clear past ResetsAt")
	}
}

// --- Start/Stop tests ---

func TestManagerStart_Idempotent(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Second Start should be idempotent
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	m.Stop()
}

func TestManagerStop_Idempotent(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Stop()
	// Second Stop should be idempotent
	m.Stop()
}

func TestManagerStart_StopCalibration(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Calibration timer should be active.
	if m.calTimer == nil {
		t.Fatal("calTimer should be set after Start")
	}

	m.Stop()

	if m.started.Load() {
		t.Fatal("started should be false after Stop")
	}
}

func TestManagerStart_PreloadClients(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	// 已绑职级的客户端：预加载后无 exceeded。
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", JobLevel: "L5"})
	putJobLevel(t, db, config.JobLevel{Name: "L5"})
	putModelCost(t, db, nosql.ModelCost{
		EndpointType: "openai", Model: "gpt-4o",
		InputPer1MTokens: 10, OutputPer1MTokens: 10,
	})
	putAggCell(t, db, nowHourKey("openai", "alice", "gpt-4o"), nosql.AggCell{
		InputTokens: 100_000, // $1
	})

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Give preload a moment to complete
	time.Sleep(100 * time.Millisecond)
	m.Stop()

	m.mu.RLock()
	_, exceeded := m.exceeded["alice"]
	_, hasCounters := m.counters["alice"]
	m.mu.RUnlock()
	if exceeded {
		t.Error("expected alice NOT exceeded (job level bound, company_general unlimited)")
	}
	if !hasCounters {
		t.Error("expected alice counters preloaded at Start")
	}
}

// --- Check tests ---

func TestManagerCheck_NoExceeded(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	_, exceeded := m.Check("alice")
	if exceeded {
		t.Error("expected not exceeded")
	}
}

func TestManagerCheck_ExceededFuture(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	m.mu.Lock()
	m.exceeded["alice"] = ExceededInfo{
		Scope:     ScopeModelGroup,
		Dimension: DimensionDaily,
		Limit:     10,
		Used:      15,
		ResetsAt:  time.Now().UTC().Add(24 * time.Hour), // future
	}
	m.mu.Unlock()

	info, exceeded := m.Check("alice")
	if !exceeded {
		t.Fatal("expected exceeded")
	}
	if info.Dimension != DimensionDaily {
		t.Errorf("expected daily, got %q", info.Dimension)
	}
}

func TestManagerCheck_ExceededPast_AutoClear(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	m.mu.Lock()
	m.exceeded["alice"] = ExceededInfo{
		Scope:     ScopeModelGroup,
		Dimension: DimensionDaily,
		Limit:     10,
		Used:      15,
		ResetsAt:  time.Now().UTC().Add(-time.Hour), // past
	}
	m.mu.Unlock()

	_, exceeded := m.Check("alice")
	if exceeded {
		t.Error("expected auto-clear for past ResetsAt")
	}

	// Verify entry was actually deleted
	m.mu.RLock()
	_, exists := m.exceeded["alice"]
	m.mu.RUnlock()
	if exists {
		t.Error("expected exceeded entry to be deleted")
	}
}

func TestManagerCheckTarget_GroupPeriodFlipClearsUsedAndKeepsCurrentDimension(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{
		Name: "plan", CostCoefficient: 1, DailyUSD: 10, WeeklyUSD: 100,
		Bindings: []config.QuotaGroupBinding{{Target: "plan"}},
	})
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "plan"}}, "L5")
	quotaTestCost(t, db)
	m := newTestManager(t, db)
	quotaTestInput(t, m, "alice", "plan", "gpt-4o", 1_000_000)

	// $10 used 触顶 daily=10 → groupExceeded；把 daily 周期拨回过去触发翻转。
	m.mu.Lock()
	daily := m.counters["alice"].groups["plan"].dims[DimensionDaily]
	daily.resetsAt = time.Now().UTC().Add(-time.Hour)
	groupExceeded := m.groupExceeded["alice"]["plan"]
	groupExceeded.ResetsAt = daily.resetsAt
	m.groupExceeded["alice"]["plan"] = groupExceeded
	m.mu.Unlock()

	status := m.Status("alice")
	group := status.Groups["plan"]
	if group.Exceeded != nil {
		t.Fatalf("Status retained expired group exceed: %+v", *group.Exceeded)
	}
	if got := group.Quotas[DimensionDaily].Used; got != 0 {
		t.Fatalf("expired group daily used = %v, want 0", got)
	}
	if got := group.Quotas[DimensionWeekly].Used; got != 10 {
		t.Fatalf("current group weekly used = %v, want 10", got)
	}
	if _, exceeded := m.CheckTarget("alice", "plan", "gpt-4o"); exceeded {
		t.Fatal("expired group daily quota should no longer block CheckTarget")
	}
}

func TestManagerCheck_GroupFlipKeepsNewPeriodExceededState(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	now := time.Now().UTC()
	m.mu.Lock()
	m.counters["alice"] = &clientCounters{
		groups: map[string]*groupCounters{
			"plan": {
				coefficient:     1,
				planCoefficient: 1,
				inPlan:          true,
				bindings:        []config.QuotaGroupBinding{{Target: "plan"}},
				dims: map[string]*dimCounter{
					DimensionDaily: {limit: 10, used: 12, resetsAt: now.Add(time.Hour), period: DimensionDaily},
				},
			},
		},
	}
	m.groupExceeded["alice"] = map[string]ExceededInfo{
		"plan": {Scope: ScopeModelGroup, GroupName: "plan", Dimension: DimensionDaily, Limit: 10, Used: 11, ResetsAt: now.Add(-time.Hour)},
	}
	m.mu.Unlock()

	m.Check("alice")
	info, exceeded := m.CheckTarget("alice", "plan", "gpt-4o")
	if !exceeded || info.ResetsAt.Before(now) || info.Used != 12 {
		t.Fatalf("new-period group exceed was lost: %+v, %v", info, exceeded)
	}
}

func TestManagerStatus_CompanyGeneralCounterFlipsWithoutLimit(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	m.mu.Lock()
	m.counters["alice"] = &clientCounters{
		groups: map[string]*groupCounters{
			"company_general": {
				coefficient: 1,
				dims: map[string]*dimCounter{
					DimensionDaily: {used: 7, resetsAt: time.Now().UTC().Add(-time.Hour), period: DimensionDaily},
				},
			},
		},
	}
	m.mu.Unlock()

	usage := m.Status("alice").Groups["company_general"].Quotas[DimensionDaily]
	if usage.Limit != 0 || usage.Used != 0 {
		t.Fatalf("company_general flipped usage = %+v, want zero used with no fabricated limit", usage)
	}
}

// --- Status tests ---

// TestManagerStatus_QuotasAggregatesPlanGroups：Quotas 为 plan 组聚合视图
// （Σ有效上限 vs Σ折算用量；任一组不限 → Limit=0）。
func TestManagerStatus_QuotasAggregatesPlanGroups(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{
		Name: "a", CostCoefficient: 1, DailyUSD: 10, WeeklyUSD: 50, MonthlyUSD: 200,
		Bindings: []config.QuotaGroupBinding{{Target: "a"}},
	})
	putModelGroup(t, db, config.ModelGroup{
		Name: "b", CostCoefficient: 1, DailyUSD: 5, WeeklyUSD: 25, MonthlyUSD: 100,
		Bindings: []config.QuotaGroupBinding{{Target: "b"}},
	})
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "a"}, {Name: "b"}}, "L5")
	m := newTestManager(t, db)
	m.Evaluate("alice")

	status := m.Status("alice")
	if status.Client != "alice" {
		t.Errorf("expected Client=alice, got %q", status.Client)
	}
	if status.JobLevel != "L5" || status.JobLevelMissing {
		t.Errorf("job level fields wrong: %+v", status)
	}
	want := map[string]float64{DimensionDaily: 15, DimensionWeekly: 75, DimensionMonthly: 300}
	for dim, expect := range want {
		q, ok := status.Quotas[dim]
		if !ok {
			t.Fatalf("expected Quotas[%q] entry", dim)
		}
		if q.Limit != expect {
			t.Errorf("Quotas[%s].Limit = %v, want %v", dim, q.Limit, expect)
		}
	}
}

func TestManagerStatus_NilStore(t *testing.T) {
	m, _ := New(Options{})
	status := m.Status("alice")
	if status.Client != "alice" {
		t.Errorf("expected Client=alice, got %q", status.Client)
	}
	if status.Quotas == nil {
		t.Error("expected Quotas to be non-nil even with nil stores")
	}
}

// TestManagerIncrementConcurrentColdStart verifies BUG #1 fix:
// Concurrent Increment calls on a freshly created client (cold start,
// counters == nil) must not lose increments due to the old code path
// releasing the lock and calling Evaluate (which overwrites counters).
func TestManagerIncrementConcurrentColdStart(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	putClient(t, db, config.Client{Name: "concurrent", AccessKey: "k1", JobLevel: "L5"})
	putJobLevel(t, db, config.JobLevel{Name: "L5"})
	putModelCost(t, db, nosql.ModelCost{
		EndpointType:     "openai",
		Model:            "gpt-4o",
		InputPer1MTokens: 1, // $1 per 1M input tokens
	})

	// 50 concurrent Increment calls, each adding 1M input tokens = $1.
	// No prior Evaluate/Start, so all 50 hit the cold-start path.
	const goroutines = 50
	const tokensPerCall = int64(1_000_000)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			m.Increment("concurrent", "openai", "gpt-4o",
				tokensPerCall, 0, 0, 0)
		}()
	}
	wg.Wait()

	m.mu.RLock()
	cc := m.counters["concurrent"]
	if cc == nil {
		m.mu.RUnlock()
		t.Fatal("expected counters to be initialized after concurrent increments")
	}
	used := cc.groups["company_general"].dims[DimensionDaily].used
	m.mu.RUnlock()

	// Each call adds exactly $1; total should be $50.
	const eps = 1e-6
	expected := float64(goroutines)
	if used < expected-eps {
		t.Fatalf("concurrent Increment lost updates: expected used≈$%.2f, got $%.6f (lost $%.6f — BUG #1 race still present)",
			expected, used, expected-used)
	}
	if used > expected+eps {
		t.Fatalf("concurrent Increment over-counted: expected $%.2f, got $%.6f", expected, used)
	}
}

// --- IncrementForTarget：组归属与折算 ---

func TestManagerIncrementForTarget_FoldedByGroupCoefficient(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{Name: "plan", CostCoefficient: 0.1, Bindings: []config.QuotaGroupBinding{{Target: "codex"}}})
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "plan"}}, "L5")
	quotaTestCost(t, db, "other")
	m := newTestManager(t, db)

	quotaTestInput(t, m, "alice", "codex", "gpt-4o", 1_000_000) // $10 raw
	status := m.Status("alice")
	// 折算扣费：$10 × 0.1 = $1；plan 聚合视图同币种。
	if got := status.Groups["plan"].Quotas[DimensionDaily].Used; got != 1 {
		t.Fatalf("plan folded used = %v, want 1", got)
	}
	if got := status.Quotas[DimensionDaily].Used; got != 1 {
		t.Fatalf("aggregate used = %v, want 1", got)
	}
}

func TestManagerIncrementForTarget_ExactFallbackAndCompanyGeneral(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{Name: "target", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "openai"}}})
	putModelGroup(t, db, config.ModelGroup{Name: "exact", CostCoefficient: 0.5, Bindings: []config.QuotaGroupBinding{{Target: "openai", Model: "gpt-4o"}}})
	putQuotaClient(t, db, []config.QuotaGroup{
		{Name: "target"},
		{Name: "exact"},
	}, "L5")
	quotaTestCost(t, db, "other")
	m := newTestManager(t, db)

	quotaTestInput(t, m, "alice", "openai", "gpt-4o", 1_000_000)  // exact: $10 raw × 0.5 = $5
	quotaTestInput(t, m, "alice", "openai", "other", 1_000_000)   // target fallback: $10 × 1 = $10
	quotaTestInput(t, m, "alice", "unbound", "gpt-4o", 1_000_000) // company_general: $10 × 1 = $10

	status := m.Status("alice")
	// plan 聚合 = exact 5 + target 10 = 15（company_general 不入 plan 聚合）
	if got := status.Quotas[DimensionDaily].Used; got != 15 {
		t.Fatalf("aggregate used = %v, want 15", got)
	}
	if got := status.Groups["exact"].Quotas[DimensionDaily].Used; got != 5 {
		t.Fatalf("exact folded used = %v, want 5", got)
	}
	if got := status.Groups["target"].Quotas[DimensionDaily].Used; got != 10 {
		t.Fatalf("target folded used = %v, want 10", got)
	}
	if got := status.Groups["company_general"].Quotas[DimensionDaily]; got.Limit != 0 || got.Used != 10 {
		t.Fatalf("company_general daily = %+v, want unlimited folded used 10", got)
	}
}

func TestManagerIncrementForTarget_OneRequestOneGroupOnce(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{Name: "exact", CostCoefficient: 0.25, Bindings: []config.QuotaGroupBinding{{Target: "openai", Model: "gpt-4o"}}})
	putModelGroup(t, db, config.ModelGroup{Name: "target", CostCoefficient: 2, Bindings: []config.QuotaGroupBinding{{Target: "openai"}}})
	putQuotaClient(t, db, []config.QuotaGroup{
		{Name: "exact"},
		{Name: "target"},
	}, "L5")
	quotaTestCost(t, db)
	m := newTestManager(t, db)

	quotaTestInput(t, m, "alice", "openai", "gpt-4o", 1_000_000) // $10 raw → exact 折算 $2.5
	status := m.Status("alice")
	if got := status.Quotas[DimensionDaily].Used; got != 2.5 {
		t.Fatalf("aggregate used = %v, want 2.5 exactly once", got)
	}
	if got := status.Groups["exact"].Quotas[DimensionDaily].Used; got != 2.5 {
		t.Fatalf("exact folded used = %v, want 2.5", got)
	}
	if got := status.Groups["target"].Quotas[DimensionDaily]; got.Limit != 0 || got.Used != 0 {
		t.Fatalf("unused target fallback group daily = %+v, want visible unlimited zero usage", got)
	}
}

func TestManagerCheckTarget_GroupBlocksOnlyThatGroupAndOtherContinues(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{Name: "a", CostCoefficient: 1, DailyUSD: 10, Bindings: []config.QuotaGroupBinding{{Target: "a"}}})
	putModelGroup(t, db, config.ModelGroup{Name: "b", CostCoefficient: 1, DailyUSD: 10, Bindings: []config.QuotaGroupBinding{{Target: "b"}}})
	putQuotaClient(t, db, []config.QuotaGroup{
		{Name: "a"},
		{Name: "b"},
	}, "L5")
	quotaTestCost(t, db)
	m := newTestManager(t, db)
	quotaTestInput(t, m, "alice", "a", "gpt-4o", 1_000_000)

	info, exceeded := m.CheckTarget("alice", "a", "gpt-4o")
	if !exceeded || info.Scope != ScopeModelGroup || info.GroupName != "a" {
		t.Fatalf("group a check = %+v, %v; want model_group/a", info, exceeded)
	}
	if info.Dimension != DimensionDaily {
		t.Fatalf("group dimension = %q, want daily", info.Dimension)
	}
	if _, exceeded := m.CheckTarget("alice", "b", "gpt-4o"); exceeded {
		t.Fatal("group b should remain usable")
	}
	quotaTestInput(t, m, "alice", "b", "gpt-4o", 1_000_000)
	if _, exceeded := m.CheckTarget("alice", "b", "gpt-4o"); !exceeded {
		t.Fatal("group b should become exceeded only after its own usage")
	}
}

func TestManagerIncrementForTarget_ActualTargetAndMappedModelDetermineGroup(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{Name: "primary", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "primary", Model: "real-model"}}})
	putModelGroup(t, db, config.ModelGroup{Name: "failover", CostCoefficient: 0.1, Bindings: []config.QuotaGroupBinding{{Target: "failover", Model: "real-model"}}})
	putQuotaClient(t, db, []config.QuotaGroup{
		{Name: "primary"},
		{Name: "failover"},
	}, "L5")
	quotaTestCost(t, db, "real-model")
	m := newTestManager(t, db)

	// The client requested an alias, but accounting receives the selected target
	// and upstream model after routing/failover.
	quotaTestInput(t, m, "alice", "failover", "real-model", 1_000_000) // $10 raw × 0.1 = $1
	status := m.Status("alice")
	if got := status.Quotas[DimensionDaily].Used; got != 1 {
		t.Fatalf("aggregate failover usage = %v, want 1", got)
	}
	if got := status.Groups["failover"].Quotas[DimensionDaily].Used; got != 1 {
		t.Fatalf("failover folded usage = %v, want 1", got)
	}
	if got := status.Groups["primary"].Quotas[DimensionDaily].Used; got != 0 {
		t.Fatalf("primary usage = %v, want 0", got)
	}
}

func TestManagerEvaluate_RebuildsGroupsFromQuotaHourlyAgg(t *testing.T) {
	db := testOpenDB(t)
	putModelGroup(t, db, config.ModelGroup{Name: "api", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "api"}}})
	putModelGroup(t, db, config.ModelGroup{Name: "plan", CostCoefficient: 0.1, Bindings: []config.QuotaGroupBinding{{Target: "plan"}}})
	putQuotaClient(t, db, []config.QuotaGroup{
		{Name: "api"},
		{Name: "plan"},
	}, "L5")
	quotaTestCost(t, db)
	usageStore := nosql.NewUsageStore(db)
	now := time.Now().UTC()
	for _, event := range []usage.Event{
		{Timestamp: now, ClientName: "alice", Target: "api", Model: "gpt-4o", EndpointType: "openai", InputTokens: 1_000_000, StatusCode: 200},
		{Timestamp: now, ClientName: "alice", Target: "plan", Model: "gpt-4o", EndpointType: "openai", InputTokens: 2_000_000, StatusCode: 200},
	} {
		if err := usageStore.Record(event); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	m := newTestManager(t, db)
	m.Evaluate("alice")

	status := m.Status("alice")
	// api: $10 × 1 = 10；plan: $20 × 0.1 = 2；聚合 = 12。
	if got := status.Quotas[DimensionDaily].Used; got != 12 {
		t.Fatalf("rebuilt aggregate used = %v, want 12 (10 + 20*.1)", got)
	}
	if got := status.Groups["api"].Quotas[DimensionDaily].Used; got != 10 {
		t.Fatalf("rebuilt api folded used = %v, want 10", got)
	}
	if got := status.Groups["plan"].Quotas[DimensionDaily].Used; got != 2 {
		t.Fatalf("rebuilt plan folded used = %v, want 2", got)
	}
}
