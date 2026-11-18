package admin

// Issue #211 先实测：POST/PUT/DELETE /admin/data/clients 各阶段耗时。
//
// 只测量、不改变生产行为。锁内段（clientStore 写 + 唯一性检查 +
// reloadAuthFromClientStore）按包内可访问函数分段计时；整请求用 httptest +
// 真实 session/路由计时。Evaluate 的分段测量在 internal/quota 的同名用例中。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

const (
	adminTimingTargets  = 20
	adminTimingModel    = "model-a"
	adminTimingMeasured = "client-000"
	adminTimingComp     = 12
	adminTimingHTTP     = 8
	adminTimingWarmups  = 2
)

type clientTimingEnv struct {
	h      *Handler
	db     *bolt.DB
	cookie *http.Cookie
	n      int
}

func newClientTimingEnv(t *testing.T, clients, perClient int, wantsExact bool) *clientTimingEnv {
	t.Helper()
	base := newIssue94Env(t)
	seedAdminTimingClients(t, base.db, clients)
	seedAdminTimingEvents(t, base.db, perClient)

	now := time.Now().UTC()
	var templates []pricing.PricingTemplate
	if wantsExact {
		templates = []pricing.PricingTemplate{{
			Model: adminTimingModel, Mode: pricing.ModeFlat,
			Start:     now.Truncate(time.Hour).Add(-3*time.Hour + 30*time.Minute),
			BaseRates: usage.CostRates{InputPer1MTokens: 1, OutputPer1MTokens: 1},
		}}
	} else {
		templates = []pricing.PricingTemplate{{
			Model: adminTimingModel, Mode: pricing.ModeFlat,
			BaseRates: usage.CostRates{InputPer1MTokens: 1, OutputPer1MTokens: 1},
		}}
	}
	cache := pricing.NewTargetPricingCache(func() ([]pricing.PricingTemplate, error) { return templates, nil }, nil, nil)
	mgr, err := quota.New(quota.Options{
		CostStore:        nosql.NewModelCostStore(base.db),
		CostCache:        cache,
		UsageStore:       nosql.NewUsageStore(base.db),
		ClientStore:      nosql.NewClientStore(base.db),
		ModelGroupStore:  nosql.NewModelGroupStore(base.db),
		ClientQuotaStore: nosql.NewClientQuotaStore(base.db),
		JobLevelStore:    nosql.NewJobLevelStore(base.db),
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("quota.New: %v", err)
	}
	base.h.quotaManager = mgr
	_, cookie, err := base.sessions.CreateSession("operator", RoleAdmin)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return &clientTimingEnv{h: base.h, db: base.db, cookie: cookie, n: clients}
}

func seedAdminTimingClients(t *testing.T, db *bolt.DB, clients int) {
	t.Helper()
	bindings := make([]config.QuotaGroupBinding, 0, adminTimingTargets)
	for i := 0; i < adminTimingTargets; i++ {
		bindings = append(bindings, config.QuotaGroupBinding{Target: fmt.Sprintf("target-%02d", i)})
	}
	plan := []config.QuotaGroup{{Name: "plan"}}
	if err := db.Update(func(tx *bolt.Tx) error {
		clientsBucket := tx.Bucket([]byte(nosql.BucketClients))
		planBucket := tx.Bucket([]byte(nosql.BucketClientQuotaGroups))
		for i := 0; i < clients; i++ {
			name := fmt.Sprintf("client-%03d", i)
			body, err := json.Marshal(config.Client{Name: name, AccessKey: fmt.Sprintf("sk-admin-%04d", i), JobLevel: "L1"})
			if err != nil {
				return err
			}
			if err := clientsBucket.Put([]byte(strings.ToLower(name)), body); err != nil {
				return err
			}
			planBody, err := json.Marshal(struct {
				Name   string              `json:"name"`
				Groups []config.QuotaGroup `json:"groups"`
			}{Name: name, Groups: plan})
			if err != nil {
				return err
			}
			if err := planBucket.Put([]byte(strings.ToLower(name)), planBody); err != nil {
				return err
			}
		}
		groupBody, err := json.Marshal(config.ModelGroup{Name: "plan", CostCoefficient: 1, Bindings: bindings})
		if err != nil {
			return err
		}
		if err := tx.Bucket([]byte(nosql.BucketModelGroups)).Put([]byte("plan"), groupBody); err != nil {
			return err
		}
		levelBody, err := json.Marshal(config.JobLevel{Name: "L1"})
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(nosql.BucketJobLevels)).Put([]byte("l1"), levelBody)
	}); err != nil {
		t.Fatalf("seed clients: %v", err)
	}
}

// seedAdminTimingEvents seeds perClient events for the measured client only.
// N 维只影响 clientStore/auth 的整表规模，M 维影响 Evaluate 的读规模。
func seedAdminTimingEvents(t *testing.T, db *bolt.DB, perClient int) {
	t.Helper()
	store := nosql.NewUsageStore(db)
	base := time.Now().UTC().Truncate(time.Hour)
	batch := make([]usage.Event, 0, perClient)
	for i := 0; i < perClient; i++ {
		hoursAgo := 1 + i%24
		minute := (i / 24) % 60
		batch = append(batch, usage.Event{
			Timestamp:   base.Add(-time.Duration(hoursAgo)*time.Hour + time.Duration(minute)*time.Minute),
			RecordID:    uuid.NewString(),
			ClientName:  adminTimingMeasured,
			Target:      fmt.Sprintf("target-%02d", i%adminTimingTargets),
			Model:       adminTimingModel,
			InputTokens: 100,
			StatusCode:  200,
		})
	}
	if err := store.RecordBatch(batch); err != nil {
		t.Fatalf("RecordBatch: %v", err)
	}
	if err := nosql.MigrateUsageClientIndex(context.Background(), db, 8192); err != nil {
		t.Fatalf("MigrateUsageClientIndex: %v", err)
	}
	if err := nosql.RunClassificationOnce(context.Background(), db); err != nil {
		t.Fatalf("RunClassificationOnce: %v", err)
	}
	t.Cleanup(func() { nosql.ForgetUsageIntegrityClassification(db) })
}

type adminDurationSet struct {
	values []time.Duration
}

func (s *adminDurationSet) add(d time.Duration) { s.values = append(s.values, d) }

func (s adminDurationSet) percentiles() (time.Duration, time.Duration, time.Duration) {
	if len(s.values) == 0 {
		return 0, 0, 0
	}
	sorted := append([]time.Duration(nil), s.values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	at := func(p float64) time.Duration {
		idx := int(float64(len(sorted))*p+0.999999) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return sorted[idx]
	}
	return at(0.50), at(0.95), at(0.99)
}

type adminTimingResult struct {
	n, m    int
	pricing string

	lockCreate, lockUpdate, lockDelete adminDurationSet
	storeCreate, storeUpdate, storeDelete adminDurationSet
	auth                                adminDurationSet
	audit                               adminDurationSet
	eval                                adminDurationSet
	totalPost, totalPut, totalDelete    adminDurationSet
	fast                                 uint64
	fallbacks                            map[string]uint64
}

func (e *clientTimingEnv) doTimed(t *testing.T, method, path string, body []byte) (time.Duration, int) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Accept", "application/json")
	req.AddCookie(e.cookie)
	rec := httptest.NewRecorder()
	start := time.Now()
	e.h.ServeHTTP(rec, req)
	return time.Since(start), rec.Code
}

func (e *clientTimingEnv) timedLocked(t *testing.T, storeFn func() error) (lock, store, auth time.Duration) {
	t.Helper()
	start := time.Now()
	e.h.configMu.Lock()
	storeStart := time.Now()
	err := storeFn()
	store = time.Since(storeStart)
	authStart := time.Now()
	authErr := e.h.reloadAuthFromClientStore()
	auth = time.Since(authStart)
	e.h.configMu.Unlock()
	lock = time.Since(start)
	if err != nil {
		t.Fatalf("store write: %v", err)
	}
	if authErr != nil {
		t.Fatalf("auth reload: %v", authErr)
	}
	return lock, store, auth
}

func (e *clientTimingEnv) measureComponents(t *testing.T) *adminTimingResult {
	t.Helper()
	res := &adminTimingResult{}
	for i := 0; i < adminTimingComp; i++ {
		// 创建：唯一性检查 = 扫描 N 个客户端。
		name := fmt.Sprintf("probe-create-%d", i)
		client := config.Client{Name: name, AccessKey: fmt.Sprintf("sk-probe-create-%d", i), JobLevel: "L1"}
		lock, store, auth := e.timedLocked(t, func() error { return e.h.clientStore.Create(client) })
		res.lockCreate.add(lock)
		res.storeCreate.add(store)
		res.auth.add(auth)
		_ = e.h.clientStore.Delete(name)

		// 更新：客户端已存在，唯一性检查排除自身后仍扫描全表。
		updated := config.Client{Name: adminTimingMeasured, AccessKey: fmt.Sprintf("sk-probe-update-%d", i), JobLevel: "L1"}
		lock, store, auth = e.timedLocked(t, func() error { return e.h.clientStore.Update(adminTimingMeasured, updated) })
		res.lockUpdate.add(lock)
		res.storeUpdate.add(store)
		res.auth.add(auth)

		// 删除：生产路径走 DeleteClientAndArchive（客户端记录 + 额度记录归档）。
		delName := fmt.Sprintf("probe-delete-%d", i)
		delClient := config.Client{Name: delName, AccessKey: fmt.Sprintf("sk-probe-delete-%d", i), JobLevel: "L1"}
		if err := e.h.clientStore.Create(delClient); err != nil {
			t.Fatalf("seed delete probe: %v", err)
		}
		existing, err := e.h.clientStore.Get(delName)
		if err != nil || existing == nil {
			t.Fatalf("load delete probe: %v", err)
		}
		lock, store, auth = e.timedLocked(t, func() error {
			return e.h.clientQuotaStore.DeleteClientAndArchive(*existing)
		})
		res.lockDelete.add(lock)
		res.storeDelete.add(store)
		res.auth.add(auth)

		// 审计：一次 bbolt 写事务。
		auditStart := time.Now()
		if err := e.h.auditStore.Record(nosql.AuditEvent{
			Action: "client_update", Actor: "operator", Object: adminTimingMeasured, Result: "success",
		}); err != nil {
			t.Fatalf("audit record: %v", err)
		}
		res.audit.add(time.Since(auditStart))
	}
	return res
}

func (e *clientTimingEnv) measureHTTP(t *testing.T, res *adminTimingResult) {
	t.Helper()
	postBody := func(prefix string, i int) []byte {
		body, _ := json.Marshal(config.Client{Name: fmt.Sprintf("%s-%d", prefix, i), AccessKey: fmt.Sprintf("sk-%s-%d", prefix, i), JobLevel: "L1"})
		return body
	}
	putBody := func(i int) []byte {
		body, _ := json.Marshal(config.Client{Name: adminTimingMeasured, AccessKey: fmt.Sprintf("sk-put-%d", i), JobLevel: "L1"})
		return body
	}
	for i := 0; i < adminTimingWarmups; i++ {
		if _, code := e.doTimed(t, http.MethodPost, "/data/clients", postBody("warm-post", i)); code >= 300 {
			t.Fatalf("POST warmup: status %d", code)
		}
		_ = e.h.clientStore.Delete(fmt.Sprintf("warm-post-%d", i))
		if _, code := e.doTimed(t, http.MethodPut, "/data/clients/"+adminTimingMeasured, putBody(i)); code >= 300 {
			t.Fatalf("PUT warmup: status %d", code)
		}
	}
	for i := 0; i < adminTimingHTTP; i++ {
		dur, code := e.doTimed(t, http.MethodPost, "/data/clients", postBody("post-probe", i))
		if code >= 300 {
			t.Fatalf("POST: status %d", code)
		}
		res.totalPost.add(dur)
		_ = e.h.clientStore.Delete(fmt.Sprintf("post-probe-%d", i))
	}
	for i := 0; i < adminTimingHTTP; i++ {
		dur, code := e.doTimed(t, http.MethodPut, "/data/clients/"+adminTimingMeasured, putBody(i))
		if code >= 300 {
			t.Fatalf("PUT: status %d", code)
		}
		res.totalPut.add(dur)
	}
	// DELETE 的探针在计时前创建，warmup 与采样各用独立名称。
	for i := 0; i < adminTimingWarmups+adminTimingHTTP; i++ {
		prefix := "del-probe"
		if i < adminTimingWarmups {
			prefix = "del-warm"
		}
		idx := i
		if i < adminTimingWarmups {
			idx = i
		} else {
			idx = i - adminTimingWarmups
		}
		c := config.Client{Name: fmt.Sprintf("%s-%d", prefix, idx), AccessKey: fmt.Sprintf("sk-%s-%d", prefix, idx), JobLevel: "L1"}
		if err := e.h.clientStore.Create(c); err != nil {
			t.Fatalf("seed delete http probe: %v", err)
		}
	}
	for i := 0; i < adminTimingWarmups; i++ {
		if _, code := e.doTimed(t, http.MethodDelete, fmt.Sprintf("/data/clients/del-warm-%d", i), nil); code >= 300 {
			t.Fatalf("DELETE warmup: status %d", code)
		}
	}
	for i := 0; i < adminTimingHTTP; i++ {
		dur, code := e.doTimed(t, http.MethodDelete, fmt.Sprintf("/data/clients/del-probe-%d", i), nil)
		if code >= 300 {
			t.Fatalf("DELETE: status %d", code)
		}
		res.totalDelete.add(dur)
	}
}

func runAdminTimingCase(t *testing.T, n, m int, pricingName string, wantsExact bool) *adminTimingResult {
	t.Helper()
	env := newClientTimingEnv(t, n, m, wantsExact)
	res := env.measureComponents(t)
	// Evaluate 单列计时（与整请求同一 DB；整请求中也同步执行一次）。
	for i := 0; i < 4; i++ {
		start := time.Now()
		env.h.quotaManager.NoteConfigChange()
		env.h.quotaManager.Evaluate(adminTimingMeasured)
		res.eval.add(time.Since(start))
	}
	before := nosql.UsageIntegrityStats(env.db)
	env.measureHTTP(t, res)
	after := nosql.UsageIntegrityStats(env.db)
	res.n, res.m, res.pricing = n, m, pricingName
	res.fast = after.FastReads - before.FastReads
	res.fallbacks = make(map[string]uint64)
	for reason, count := range after.FallbackReads {
		if prev := before.FallbackReads[reason]; count > prev {
			res.fallbacks[reason] = count - prev
		}
	}
	return res
}

func TestClientWriteTiming(t *testing.T) {
	if os.Getenv("ISSUE211_TIMING") != "1" {
		t.Skip("Issue #211 measurement: set ISSUE211_TIMING=1 to run (~40 seconds)")
	}
	if testing.Short() {
		t.Skip("timing measurement")
	}
	cases := []struct{ n, m int }{
		{10, 1000}, {100, 1000}, {1000, 1000},
		{100, 100}, {100, 5000},
	}
	var results []*adminTimingResult
	for _, form := range []struct {
		name  string
		exact bool
	}{{"P0-flat-hourly", false}, {"P1-exact-timestamps", true}} {
		for _, tc := range cases {
			name := fmt.Sprintf("%s/n%d-m%d", form.name, tc.n, tc.m)
			t.Run(name, func(t *testing.T) {
				results = append(results, runAdminTimingCase(t, tc.n, tc.m, form.name, form.exact))
			})
		}
	}
	reportAdminTiming(t, results)
}

func reportAdminTiming(t *testing.T, results []*adminTimingResult) {
	t.Helper()
	var b strings.Builder
	b.WriteString("\n### admin 客户端写锁内段（p50，单位 ms；锁内组件样本 ")
	fmt.Fprintf(&b, "%d）\n\n", adminTimingComp)
	b.WriteString("| case | N | M | t_lock create | t_lock update | t_lock delete | t_store create | t_store update | t_store delete | t_auth | t_audit |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range results {
		lp := func(set adminDurationSet) string { p50, _, _ := set.percentiles(); return timingMS(p50) }
		fmt.Fprintf(&b, "| %s | %d | %d | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			r.pricing, r.n, r.m,
			lp(r.lockCreate), lp(r.lockUpdate), lp(r.lockDelete),
			lp(r.storeCreate), lp(r.storeUpdate), lp(r.storeDelete), lp(r.auth), lp(r.audit))
	}
	b.WriteString("\n### admin 客户端写整请求（httptest，单位 ms；样本 ")
	fmt.Fprintf(&b, "%d）\n\n", adminTimingHTTP)
	b.WriteString("说明：本表 DB 只有被测客户端 client-000 有 M 条历史事件（N 维只扩大 clientStore/auth 规模），" +
		"因此 P0 的整月 rollup 全表扫描成本小于 N*M 全量数据；该成本见 quota 表。\n\n")
	b.WriteString("| case | N | M | POST p50/p95/p99 | PUT p50/p95/p99 | DELETE p50/p95/p99 | t_eval p50 | fast/fallback |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, r := range results {
		triple := func(set adminDurationSet) string {
			p50, p95, p99 := set.percentiles()
			return timingMS(p50) + " / " + timingMS(p95) + " / " + timingMS(p99)
		}
		evalP50, _, _ := r.eval.percentiles()
		fmt.Fprintf(&b, "| %s | %d | %d | %s | %s | %s | %s | %d / %s |\n",
			r.pricing, r.n, r.m, triple(r.totalPost), triple(r.totalPut), triple(r.totalDelete), timingMS(evalP50), r.fast, timingFallbacks(r.fallbacks))
	}
	fmt.Print(b.String())
	appendAdminMeasurements(t, b.String())
}

func timingMS(d time.Duration) string { return fmt.Sprintf("%.2f", float64(d.Microseconds())/1000) }

func timingFallbacks(fallbacks map[string]uint64) string {
	if len(fallbacks) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(fallbacks))
	for reason, n := range fallbacks {
		parts = append(parts, fmt.Sprintf("%s=%d", reason, n))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func appendAdminMeasurements(t *testing.T, section string) {
	t.Helper()
	f, err := os.OpenFile("/tmp/issue-211-measurements.md", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Logf("write measurements file: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(section); err != nil {
		t.Logf("write measurements file: %v", err)
	}
}