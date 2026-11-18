package quota

// Issue #211 先实测：客户端写接口各阶段耗时分布。
//
// 本文件只测量、不改变生产行为。测试直接构造 Manager 与 bbolt 数据，用
// Manager 现有的 Debug 日志字段（drain/config/read_usage）采集 Evaluate 分段，
// 用 time.Since 包住 Evaluate 本身，并用 nosql.UsageIntegrityStats 记录快路径
// 命中/回退次数。结论输出为 markdown（stdout + /tmp/issue-211-measurements.md）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

const (
	timingTargets      = 20
	timingModel        = "model-a"
	timingMeasured     = "client-000"
	timingSamples      = 12
	timingWarmups      = 2
	timingEventSpanHrs = 24
)

type timingPhaseCapture struct {
	drain, cfg, read []time.Duration
}

func (h *timingPhaseCapture) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelDebug
}

func (h *timingPhaseCapture) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "quota.Manager: evaluate phases" {
		return nil
	}
	var drain, cfg, read time.Duration
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "drain":
			drain = a.Value.Duration()
		case "config":
			cfg = a.Value.Duration()
		case "read_usage":
			read = a.Value.Duration()
		}
		return true
	})
	h.drain = append(h.drain, drain)
	h.cfg = append(h.cfg, cfg)
	h.read = append(h.read, read)
	return nil
}

func (h *timingPhaseCapture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *timingPhaseCapture) WithGroup(string) slog.Handler     { return h }

func (h *timingPhaseCapture) reset() {
	h.drain, h.cfg, h.read = nil, nil, nil
}

type timingPricing struct {
	name      string
	templates func(now time.Time) []pricing.PricingTemplate
}

func timingPricingForms(now time.Time) []timingPricing {
	return []timingPricing{
		{
			name: "P0-flat-hourly",
			templates: func(time.Time) []pricing.PricingTemplate {
				return []pricing.PricingTemplate{{
					Model: timingModel, Mode: pricing.ModeFlat,
					BaseRates: usage.CostRates{InputPer1MTokens: 1, OutputPer1MTokens: 1},
				}}
			},
		},
		{
			name: "P1-exact-timestamps",
			templates: func(now time.Time) []pricing.PricingTemplate {
				// Start 落在半小时边界，RequiresEventTimestamps=true，走事件级精确读。
				start := now.Truncate(time.Hour).Add(-3*time.Hour + 30*time.Minute)
				return []pricing.PricingTemplate{{
					Model: timingModel, Mode: pricing.ModeFlat, Start: start,
					BaseRates: usage.CostRates{InputPer1MTokens: 1, OutputPer1MTokens: 1},
				}}
			},
		},
	}
}

func newTimingManager(t *testing.T, db *bolt.DB, capture *timingPhaseCapture, form timingPricing, now time.Time) *Manager {
	t.Helper()
	templates := form.templates(now)
	cache := pricing.NewTargetPricingCache(func() ([]pricing.PricingTemplate, error) {
		return templates, nil
	}, nil, nil)
	m, err := New(Options{
		CostStore:        nosql.NewModelCostStore(db),
		CostCache:        cache,
		UsageStore:       nosql.NewUsageStore(db),
		ClientStore:      nosql.NewClientStore(db),
		ModelGroupStore:  nosql.NewModelGroupStore(db),
		ClientQuotaStore: nosql.NewClientQuotaStore(db),
		JobLevelStore:    nosql.NewJobLevelStore(db),
		Logger:           slog.New(capture),
	})
	if err != nil {
		t.Fatalf("quota.New: %v", err)
	}
	return m
}

// seedTimingClients writes N clients, one model group bound to T targets, a job
// level and each client's plan in two bbolt transactions (client CRUD's own
// per-client write path is measured in the admin package, not here).
func seedTimingClients(t *testing.T, db *bolt.DB, clients int) {
	t.Helper()
	bindings := make([]config.QuotaGroupBinding, 0, timingTargets)
	for i := 0; i < timingTargets; i++ {
		bindings = append(bindings, config.QuotaGroupBinding{Target: fmt.Sprintf("target-%02d", i)})
	}
	plan := []config.QuotaGroup{{Name: "plan"}}
	if err := db.Update(func(tx *bolt.Tx) error {
		clientsBucket := tx.Bucket([]byte(nosql.BucketClients))
		planBucket := tx.Bucket([]byte(nosql.BucketClientQuotaGroups))
		for i := 0; i < clients; i++ {
			name := fmt.Sprintf("client-%03d", i)
			data, err := json.Marshal(config.Client{Name: name, AccessKey: fmt.Sprintf("sk-timing-%04d", i), JobLevel: "L1"})
			if err != nil {
				return err
			}
			if err := clientsBucket.Put([]byte(strings.ToLower(name)), data); err != nil {
				return err
			}
			planData, err := json.Marshal(struct {
				Name   string              `json:"name"`
				Groups []config.QuotaGroup `json:"groups"`
			}{Name: name, Groups: plan})
			if err != nil {
				return err
			}
			if err := planBucket.Put([]byte(strings.ToLower(name)), planData); err != nil {
				return err
			}
		}
		mgroup, err := json.Marshal(config.ModelGroup{Name: "plan", CostCoefficient: 1, Bindings: bindings})
		if err != nil {
			return err
		}
		if err := tx.Bucket([]byte(nosql.BucketModelGroups)).Put([]byte("plan"), mgroup); err != nil {
			return err
		}
		level, err := json.Marshal(config.JobLevel{Name: "L1"})
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(nosql.BucketJobLevels)).Put([]byte("l1"), level)
	}); err != nil {
		t.Fatalf("seed clients: %v", err)
	}
}

// seedTimingEvents records perClient events for every client through the
// canonical write path (RecordBatch), so the integrity classifier can reach the
// steady-state fast path.
func seedTimingEvents(t *testing.T, db *bolt.DB, clients, perClient int) {
	t.Helper()
	store := nosql.NewUsageStore(db)
	now := time.Now().UTC()
	base := now.Truncate(time.Hour)
	const chunk = 4000
	batch := make([]usage.Event, 0, chunk)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := store.RecordBatch(batch); err != nil {
			t.Fatalf("RecordBatch: %v", err)
		}
		batch = batch[:0]
	}
	for c := 0; c < clients; c++ {
		client := fmt.Sprintf("client-%03d", c)
		for i := 0; i < perClient; i++ {
			// 事件集中在最近 24 小时，控制聚合单元数量（总事件数仍是 N*M）。
			hoursAgo := 1 + i%timingEventSpanHrs
			minute := (i / timingEventSpanHrs) % 60
			batch = append(batch, usage.Event{
				Timestamp:   base.Add(-time.Duration(hoursAgo)*time.Hour + time.Duration(minute)*time.Minute),
				RecordID:    uuid.NewString(),
				ClientName:  client,
				Target:      fmt.Sprintf("target-%02d", i%timingTargets),
				Model:       timingModel,
				InputTokens: 100,
				StatusCode:  200,
			})
			if len(batch) == chunk {
				flush()
			}
		}
	}
	flush()
	if err := nosql.MigrateUsageClientIndex(context.Background(), db, 8192); err != nil {
		t.Fatalf("MigrateUsageClientIndex: %v", err)
	}
}

func prepareTimingDB(t *testing.T, clients, perClient int) *bolt.DB {
	t.Helper()
	db := testOpenDB(t)
	seedTimingClients(t, db, clients)
	seedTimingEvents(t, db, clients, perClient)
	t.Cleanup(func() { nosql.ForgetUsageIntegrityClassification(db) })
	return db
}

type timingSample struct {
	eval  time.Duration
	drain time.Duration
	cfg   time.Duration
	read  time.Duration
}

type timingResult struct {
	N, M        int
	pricing     string
	samples     []timingSample
	cold        time.Duration
	fast        uint64
	fallbacks   map[string]uint64
	coldFast    uint64
	coldFallback map[string]uint64
	complete    bool
	classifyMS  float64
}

func (r timingResult) phase(sel func(timingSample) time.Duration) (time.Duration, time.Duration, time.Duration) {
	values := make([]time.Duration, 0, len(r.samples))
	for _, s := range r.samples {
		values = append(values, sel(s))
	}
	return durationPercentiles(values)
}

func durationPercentiles(values []time.Duration) (time.Duration, time.Duration, time.Duration) {
	if len(values) == 0 {
		return 0, 0, 0
	}
	sorted := append([]time.Duration(nil), values...)
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

func integrityDelta(before, after nosql.UsageIntegrityMetrics) (uint64, map[string]uint64) {
	fallbacks := make(map[string]uint64)
	for reason, n := range after.FallbackReads {
		if prev := before.FallbackReads[reason]; n > prev {
			fallbacks[reason] = n - prev
		}
	}
	return after.FastReads - before.FastReads, fallbacks
}

func runTimingCase(t *testing.T, clients, perClient int, form timingPricing) timingResult {
	t.Helper()
	db := prepareTimingDB(t, clients, perClient)
	now := time.Now().UTC()
	capture := &timingPhaseCapture{}
	m := newTimingManager(t, db, capture, form, now)

	// 冷路径：分类尚未安装时的一次 Evaluate（P1 应计入 incomplete 回退）。
	beforeCold := nosql.UsageIntegrityStats(db)
	coldStart := time.Now()
	m.NoteConfigChange()
	m.Evaluate(timingMeasured)
	cold := time.Since(coldStart)
	coldFast, coldFallback := integrityDelta(beforeCold, nosql.UsageIntegrityStats(db))

	classStart := time.Now()
	if err := nosql.RunClassificationOnce(context.Background(), db); err != nil {
		t.Fatalf("RunClassificationOnce: %v", err)
	}
	classifyDur := time.Since(classStart)
	stats := nosql.UsageIntegrityStats(db)

	for i := 0; i < timingWarmups; i++ {
		m.NoteConfigChange()
		m.Evaluate(timingMeasured)
	}
	capture.reset()
	before := nosql.UsageIntegrityStats(db)
	samples := make([]timingSample, 0, timingSamples)
	for i := 0; i < timingSamples; i++ {
		start := time.Now()
		m.NoteConfigChange()
		m.Evaluate(timingMeasured)
		samples = append(samples, timingSample{eval: time.Since(start)})
	}
	after := nosql.UsageIntegrityStats(db)
	fast, fallbacks := integrityDelta(before, after)
	// 分段来自同批 Evaluate 的 Debug 日志（与 samples 一一对应）。
	for i := range samples {
		if i < len(capture.drain) {
			samples[i].drain = capture.drain[i]
			samples[i].cfg = capture.cfg[i]
			samples[i].read = capture.read[i]
		}
	}
	return timingResult{
		N: clients, M: perClient, pricing: form.name, samples: samples, cold: cold,
		fast: fast, fallbacks: fallbacks, coldFast: coldFast, coldFallback: coldFallback,
		complete: stats.Complete,
		classifyMS: float64(classifyDur.Microseconds()) / 1000,
	}
}

func TestClientWriteTiming(t *testing.T) {
	if os.Getenv("ISSUE211_TIMING") != "1" {
		t.Skip("Issue #211 measurement: set ISSUE211_TIMING=1 to run (full matrix takes ~7 minutes)")
	}
	if testing.Short() {
		t.Skip("timing measurement")
	}
	forms := timingPricingForms(time.Now().UTC())
	// 单因子矩阵，总事件数 N*M <= 20 万（canonical 写入路径的实测吞吐约
// 1-2 千事件/秒，1M 事件的 case 需要十分钟以上的纯写入，超出测量预算）：
//   N 维：M 固定 200，N=10/100/1000
//   M 维：N 固定 20，M=100/1000/5000
// 未测：N=1000 x M=1000/5000（事件数与写入耗时都超出预算；M=5000 与 N=1000
// 的交叉组合在 200 万事件上限下也只剩 M<=2000）。
cases := []struct {
	n, m int
}{
	{10, 200}, {100, 200}, {1000, 200},
	{20, 100}, {20, 1000}, {20, 5000},
}
	var results []timingResult
	for _, form := range forms {
		for _, tc := range cases {
			name := fmt.Sprintf("%s/n%d-m%d", form.name, tc.n, tc.m)
			t.Run(name, func(t *testing.T) {
				results = append(results, runTimingCase(t, tc.n, tc.m, form))
			})
		}
	}
	reportQuotaTiming(t, results)
}

func reportQuotaTiming(t *testing.T, results []timingResult) {
	t.Helper()
	var b strings.Builder
	b.WriteString("\n### quota.Manager.Evaluate 分段（单位 ms；样本数 ")
	fmt.Fprintf(&b, "%d，单 case N*M<=20 万）\n\n", timingSamples)
	b.WriteString("| case | N | M | t_eval p50/p95/p99 | drain p50/p95/p99 | config p50/p95/p99 | read_usage p50/p95/p99 | cold t_eval | steady fast / fallback | cold fast / fallback | classification |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range results {
		ep50, ep95, ep99 := r.phase(func(s timingSample) time.Duration { return s.eval })
		dp50, dp95, dp99 := r.phase(func(s timingSample) time.Duration { return s.drain })
		cp50, cp95, cp99 := r.phase(func(s timingSample) time.Duration { return s.cfg })
		rp50, rp95, rp99 := r.phase(func(s timingSample) time.Duration { return s.read })
		complete := "no"
		if r.complete {
			complete = "yes"
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %s | %s | %s | %s | %s | %d / %s | %d / %s | %s (%.0fms) |\n",
			r.pricing, r.N, r.M,
			msTriple(ep50, ep95, ep99), msTriple(dp50, dp95, dp99), msTriple(cp50, cp95, cp99), msTriple(rp50, rp95, rp99),
			ms(r.cold), r.fast, formatFallbacks(r.fallbacks), r.coldFast, formatFallbacks(r.coldFallback), complete, r.classifyMS)
	}
	fmt.Print(b.String())
	appendMeasurementsFile(t, b.String())
}

func formatFallbacks(fallbacks map[string]uint64) string {
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

func ms(d time.Duration) string { return fmt.Sprintf("%.2f", float64(d.Microseconds())/1000) }

func msTriple(p50, p95, p99 time.Duration) string {
	return ms(p50) + " / " + ms(p95) + " / " + ms(p99)
}

func appendMeasurementsFile(t *testing.T, section string) {
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