// Package quota implements account-level USD quota limiting (daily/weekly/monthly).
//
// Architecture: memory-first, event-driven, hourly DB calibration.
//
//   - Increment: on request completion, add cost to in-memory counters (zero DB hit).
//   - Check: read in-memory exceeded map for admission control (zero DB hit).
//   - Evaluate: authoritative DB aggregation for a single client — used at startup
//     preload, admin config changes, and hourly calibration.
//   - Hourly calibration: aligns to natural hours (00:00, 01:00, ...).
//     At each hour boundary, Evaluate clients with limits or quota groups from DB.
//     This naturally handles period resets (daily/weekly/monthly boundaries
//     all fall on hour boundaries) and corrects any accumulated drift.
//   - Lazy period flip: increment/Check detect ResetsAt crossing and clear
//     the counter, eliminating the sub-second gap between boundary and
//     calibration tick.
//
// No 5-second ticker. No full-recompute polling.
package quota

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/catalog"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/costutil"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// Options configures Manager dependencies.
type Options struct {
	Catalog          *catalog.Catalog
	CostStore        *nosql.ModelCostStore
	CostCache        *pricing.PricingCache
	UsageStore       *nosql.UsageStore
	UsageJournal     *nosql.UsageJournal // durable fallback must drain before DB calibration
	ClientStore      *nosql.ClientStore
	ModelGroupStore  *nosql.ModelGroupStore
	ClientQuotaStore *nosql.ClientQuotaStore
	// JobLevelStore（额度体系 v2）：职级系数是个人维度额度的兜底来源；
	// 客户端未绑定职级（或绑定值不存在）= 未配置额度 = 请求拒绝。
	JobLevelStore *nosql.JobLevelStore
	Logger        *slog.Logger
}

// dimCounter holds an in-memory incremental counter for one quota dimension.
type dimCounter struct {
	limit    float64   // effective limit (0 = unlimited)
	used     float64   // incremental USD spend (folded, cost × group coefficient)
	resetsAt time.Time // next period boundary (UTC)
	period   string    // daily | weekly | monthly
}

// clientCounters holds per-group counters for one client.
// 额度体系 v2：个人折算额度与池分层限额已退役；个人维度限额由职级系数
// 融入每个模型组的有效额度（limit = 组基数 × 职级系数 × 个人系数）。
type clientCounters struct {
	// jobLevel 是客户端绑定的职级名（原样存储）；jobLevelMissing=true 表示
	// 未绑定或绑定值不存在 → 客户端级拒绝（Scope=job_level）。
	jobLevel        string
	jobLevelMissing bool
	groups          map[string]*groupCounters
	// calibrateScheduled is true once a short-delay Evaluate has been scheduled
	// to reconcile this cold-started client against the DB. Cleared on success.
	// Guards against the cold-start used=0 window where a new client can exceed
	// quota for up to an hour before the next natural-hour calibration.
	calibrateScheduled bool
}

type groupCounters struct {
	coefficient float64 // 组系数（cost_coefficient；company_general = 1）
	// 透明字段（额度公式推导，供 Status/UI 展示）：
	planCoefficient  float64            // 个人系数（plan；默认 1，0=不限）
	rankCoefficients map[string]float64 // 职级系数（daily/weekly/monthly；默认 1，0=不限）
	baseLimits       map[string]float64 // 组基数（额度策略日/周/月额度）
	inPlan           bool               // plan 显式添加（company_general 兜底为 false）
	bindings         []config.QuotaGroupBinding
	dims             map[string]*dimCounter
}

// Manager manages quota evaluation.
// Decision source: in-memory counters + exceeded map.
type Manager struct {
	accounting       [64]sync.RWMutex // 同一分片的记录可并行，校准独占以覆盖落库到增量的完整区间
	catalog          *catalog.Catalog
	costStore        *nosql.ModelCostStore
	costCache        *pricing.PricingCache
	usageStore       *nosql.UsageStore
	usageJournal     *nosql.UsageJournal
	clientStore      *nosql.ClientStore
	modelGroupStore  *nosql.ModelGroupStore
	clientQuotaStore *nosql.ClientQuotaStore
	jobLevelStore    *nosql.JobLevelStore
	logger           *slog.Logger

	mu                sync.RWMutex
	exceeded          map[string]ExceededInfo            // clientName → job_level 拒绝（额度 v2 唯一客户端级门槛）
	groupExceeded     map[string]map[string]ExceededInfo // clientName → group → 第一超限维度
	counters          map[string]*clientCounters         // clientName → per-group counters
	calibrationErrors map[string]string                  // incomplete history/read failures block admission until calibration succeeds
	// calibrationRetryAt rate-limits recalibration from request paths so a
	// transient failure clears on a later request rather than the hourly run.
	calibrationRetryAt  map[string]time.Time
	calibrationInFlight map[string]chan struct{}

	// hourly calibration timer
	calTimer *time.Timer
	timerMu  sync.Mutex // protects calTimer (written by timer goroutine, read by Stop)
	stopCh   chan struct{}
	started  atomic.Bool
}

// New constructs a Manager.
func New(opts Options) (*Manager, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		catalog:             opts.Catalog,
		costStore:           opts.CostStore,
		costCache:           opts.CostCache,
		usageStore:          opts.UsageStore,
		usageJournal:        opts.UsageJournal,
		clientStore:         opts.ClientStore,
		modelGroupStore:     opts.ModelGroupStore,
		clientQuotaStore:    opts.ClientQuotaStore,
		jobLevelStore:       opts.JobLevelStore,
		logger:              logger,
		exceeded:            make(map[string]ExceededInfo),
		groupExceeded:       make(map[string]map[string]ExceededInfo),
		counters:            make(map[string]*clientCounters),
		calibrationErrors:   make(map[string]string),
		calibrationRetryAt:  make(map[string]time.Time),
		calibrationInFlight: make(map[string]chan struct{}),
	}, nil
}

// Start runs one-time preload + launches hourly calibration timer.
func (m *Manager) Start(ctx context.Context) error {
	if m.started.Swap(true) {
		return nil
	}

	// Preload clients with limits or quota groups so their status counters exist.
	preloadCtx, preloadCancel := context.WithTimeout(ctx, 10*time.Second)
	defer preloadCancel()

	names, err := m.quotaClientNames()
	if err != nil {
		m.logger.Warn("quota.Manager: preload failed", "error", err)
	}
	for _, name := range names {
		select {
		case <-preloadCtx.Done():
			m.logger.Warn("quota.Manager: preload timeout")
			goto startCalibration
		default:
			m.Evaluate(name)
		}
	}

startCalibration:
	// Launch hourly calibration aligned to natural hours.
	m.stopCh = make(chan struct{})
	m.scheduleCalibration()

	return nil
}

// scheduleCalibration schedules the next calibration at the next natural hour boundary.
func (m *Manager) scheduleCalibration() {
	// 已停止则不再排期（m3：避免 Stop 后回调重新装一个新 timer 导致永久复活）。
	select {
	case <-m.stopCh:
		return
	default:
	}
	now := time.Now().UTC()
	nextHour := time.Date(now.Year(), now.Month(), now.Day(), now.Hour()+1, 0, 0, 0, time.UTC)
	delay := nextHour.Sub(now)
	if delay <= 0 {
		delay = time.Second // edge case: exactly on hour boundary
	}
	t := time.AfterFunc(delay, func() {
		// 回调首先检查停止标志：Stop 已 close(stopCh) 时不再校准也不再排期。
		select {
		case <-m.stopCh:
			return
		default:
		}
		m.calibrateAll()
		m.scheduleCalibration() // reschedule for next hour
	})
	m.timerMu.Lock()
	m.calTimer = t
	m.timerMu.Unlock()
}

// calibrateAll runs authoritative DB aggregation for clients with limits or groups.
// Called at each natural hour boundary.
func (m *Manager) calibrateAll() {
	if m.clientStore == nil {
		return
	}
	names, err := m.quotaClientNames()
	if err != nil {
		m.logger.Warn("quota.Manager: calibration quotaClientNames failed", "error", err)
		return
	}
	m.logger.Debug("quota.Manager: hourly calibration", "clients", len(names))
	for _, name := range names {
		m.Evaluate(name)
	}
}

// quotaClientNames returns every active client plus clients that only exist as
// per-client plan entries. 额度体系 v2：职级门槛适用于所有客户端（未绑职级 =
// 拒绝），因此预加载/校准必须覆盖全量客户端，而不只是配置了限额的客户端。
// Archived entries (clients deleted via the admin UI) are excluded: their quota
// state is already cleared on delete and no requests can arrive for them.
func (m *Manager) quotaClientNames() ([]string, error) {
	set := map[string]struct{}{}
	if m.clientStore != nil {
		clients, err := m.clientStore.List()
		if err != nil {
			return nil, err
		}
		for _, c := range clients {
			set[c.Name] = struct{}{}
		}
	}
	if m.clientQuotaStore != nil {
		byClient, err := m.clientQuotaStore.List()
		if err != nil {
			return nil, err
		}
		for name := range byClient {
			set[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// RefreshAll re-evaluates every quota client from the DB. Called by admin
// handlers after global quota-affecting changes (model-group CRUD, target
// deletion) that invalidate the resolved group bindings in in-memory counters.
func (m *Manager) RefreshAll() {
	if m.clientStore == nil {
		return
	}
	names, err := m.quotaClientNames()
	if err != nil {
		m.logger.Warn("quota.Manager: RefreshAll quotaClientNames failed", "error", err)
		return
	}
	active := make(map[string]struct{}, len(names))
	for _, name := range names {
		active[name] = struct{}{}
		m.Evaluate(name)
	}
	// Clear stale in-memory state for clients that no longer have any quota
	// configuration (e.g. a client whose last group was removed and whose
	// personal quota is zero disappears from quotaClientNames).
	m.mu.Lock()
	for name := range m.counters {
		if _, ok := active[name]; !ok {
			delete(m.counters, name)
		}
	}
	for name := range m.exceeded {
		if _, ok := active[name]; !ok {
			delete(m.exceeded, name)
		}
	}
	for name := range m.groupExceeded {
		if _, ok := active[name]; !ok {
			delete(m.groupExceeded, name)
		}
	}
	for name := range m.calibrationErrors {
		if _, ok := active[name]; !ok {
			delete(m.calibrationErrors, name)
			delete(m.calibrationRetryAt, name)
		}
	}
	m.mu.Unlock()
}

// Stop halts calibration timer.
func (m *Manager) Stop() {
	if !m.started.Swap(false) {
		return
	}
	// 先 close stopCh 再 Stop timer：正在执行的回调会在重新排期前看到停止信号。
	if m.stopCh != nil {
		close(m.stopCh)
	}
	m.timerMu.Lock()
	if m.calTimer != nil {
		m.calTimer.Stop()
	}
	m.timerMu.Unlock()
}

// Increment adds cost to a client's in-memory counters and updates exceeded state.
// Called after request completion. Accepts raw token counts; computes USD cost internally.
// If a period boundary (ResetsAt) has been crossed, the counter resets first (lazy flip).
func (m *Manager) Increment(clientName string, epType string, model string, inputTokens, outputTokens, cachedTokens, cacheCreationTokens int64) {
	m.IncrementForTarget(clientName, "", epType, model, time.Now().UTC(), inputTokens, outputTokens, cachedTokens, cacheCreationTokens)
}

// IncrementForTarget accounts a successful request using its actual selected
// target. 额度体系 v2：消费按「计费原价 × 组系数」折算累计到归属模型组的
// 有效额度（组基数 × 职级系数 × 个人系数）；个人折算额度与池分层
// 限额已退役，客户端级门槛只剩「未绑职级 = 拒绝」。
func (m *Manager) IncrementForTarget(clientName, target, epType, model string, at time.Time, inputTokens, outputTokens, cachedTokens, cacheCreationTokens int64, partitions ...int64) {
	event := usage.Event{ClientName: clientName, Target: target, EndpointType: epType, Model: model, Timestamp: at,
		InputTokens: inputTokens, OutputTokens: outputTokens, CachedTokens: cachedTokens, CacheCreationTokens: cacheCreationTokens}
	fields := []*int64{&event.ExplicitCacheReadTokens, &event.CacheCreation5mTokens, &event.CacheCreation1hTokens, &event.ImageInputTokens, &event.CachedImageInputTokens, &event.ImageOutputTokens}
	for i, value := range partitions {
		if i < len(fields) {
			*fields[i] = value
		}
	}
	m.IncrementEvent(event)
}

// IncrementEvent preserves the complete request evidence used to select a rate.
func (m *Manager) IncrementEvent(event usage.Event) {
	clientName, target, model := event.ClientName, event.Target, event.Model
	resolver, err := m.getResolver()
	costUSD := usage.EstimateEventCost(event, resolver)
	if err != nil {
		m.calibrationFailed(clientName, err)
		return
	}
	if costUSD <= 0 {
		return
	}
	now := time.Now().UTC()

	m.mu.Lock()
	cc := m.counters[clientName]
	if cc == nil {
		// Cold start：配置读取含 DB I/O，不能在写锁内做——
		// 会阻塞请求准入路径上所有 Check(RLock)。释放锁 → 锁外取配置 →
		// 重新加锁 double-check（并发 Increment/Evaluate 可能已创建）。
		// 增量不丢失：本次 costUSD 在重新加锁后才写入计数器。
		m.mu.Unlock()
		qc, err := m.findQuotaConfig(clientName)
		if err != nil {
			m.calibrationFailed(clientName, err)
			return
		}
		m.mu.Lock()
		cc = m.counters[clientName]
		if cc == nil {
			cc = m.buildCountersFromClientLocked(clientName, qc)
			if cc == nil {
				m.mu.Unlock()
				return // client not found
			}
			// Schedule a one-shot short-delay Evaluate to close the cold-start
			// window: without this, used=0 persists until the next natural-hour
			// boundary, allowing up to ~1h of over-quota spend. The 10s delay
			// batches a burst of cold-start requests into one calibration.
			if !cc.calibrateScheduled {
				cc.calibrateScheduled = true
				client := clientName
				time.AfterFunc(10*time.Second, func() {
					// Stop 后跳过：这个一次性定时器无法被 Stop 取消，不带守卫会在
					// 停机期间访问即将关闭的 bbolt。（未 Start 的测试场景同样跳过。）
					if m.started.Load() {
						m.Evaluate(client)
					}
				})
			}
		}
	}
	groupName, coefficient := quotaGroupForCounters(cc, target, model)
	if cc.groups == nil {
		cc.groups = make(map[string]*groupCounters)
	}
	gc := cc.groups[groupName]
	if gc == nil {
		// 兜底组（company_general 或未入 plan 的归属）：无基数 → 各维不限，
		// 折算累计仅用于计量展示。
		gc = newFallbackGroupCounters(coefficient, now)
		cc.groups[groupName] = gc
	}

	if groups := m.groupExceeded[clientName]; groups != nil {
		delete(groups, groupName)
		if len(groups) == 0 {
			delete(m.groupExceeded, clientName)
		}
	}
	var firstGroupExceeded *ExceededInfo
	for _, dim := range []string{DimensionDaily, DimensionWeekly, DimensionMonthly} {
		dc := gc.dims[dim]
		if dc == nil {
			continue
		}
		if !dc.resetsAt.After(now) {
			resetsAt, err := NextResetAt(dim, now)
			if err != nil {
				continue
			}
			dc.used = 0
			dc.resetsAt = resetsAt
			// limit 沿用旧值：限额变更由整点 calibrateAll / 管理端 Evaluate
			// 从 DB 权威刷新，翻转时刻不做 I/O。
		}
		// 折算扣费：计费原价 × 组系数（与有效额度同币种，公式见议题 #120）。
		dc.used += costUSD * coefficient
		if firstGroupExceeded == nil && dc.limit > 0 && dc.used >= dc.limit {
			info := ExceededInfo{Scope: ScopeModelGroup, GroupName: groupName, Dimension: dim, Limit: dc.limit, Used: dc.used, ResetsAt: dc.resetsAt}
			firstGroupExceeded = &info
		}
	}
	if firstGroupExceeded != nil {
		if m.groupExceeded[clientName] == nil {
			m.groupExceeded[clientName] = make(map[string]ExceededInfo)
		}
		m.groupExceeded[clientName][groupName] = *firstGroupExceeded
	}

	m.mu.Unlock()
}

// quotaConfig bundles everything needed to build one client's counters:
// the client record, its plan (per-client group selections with personal
// coefficients), the global model-group catalog and the job-level catalog.
type quotaConfig struct {
	client  *config.Client
	plan    []config.QuotaGroup
	catalog map[string]config.ModelGroup
	levels  map[string]config.JobLevel // key: lowercase job-level name
}

// buildCountersFromClientLocked creates clientCounters from prefetched quota
// config — pure in-memory, no DB I/O (the caller fetches the client OUTSIDE the
// lock). Must be called with m.mu held. Returns nil when the client is missing.
func (m *Manager) buildCountersFromClientLocked(clientName string, qc quotaConfig) *clientCounters {
	if qc.client == nil {
		return nil
	}
	now := time.Now().UTC()
	cc := newClientCounters(qc, now)
	m.counters[clientName] = cc
	return cc
}

// newClientCounters builds empty (used=0) counters with effective limits
// derived from the quota v2 formula:
//
//	limit(dim) = 组基数(dim) × 职级系数(dim) × 个人系数
//
// 个人或职级系数为 0，或组系数为 0 → 该维度不限（limit=0）。未绑职级由
// jobLevelMissing 表达（客户端级拒绝），组限额按中性职级系数 1 展示。
func newClientCounters(qc quotaConfig, now time.Time) *clientCounters {
	cc := &clientCounters{groups: make(map[string]*groupCounters)}
	cc.jobLevel = strings.TrimSpace(qc.client.JobLevel)
	level, levelOK := qc.levels[strings.ToLower(cc.jobLevel)]
	cc.jobLevelMissing = cc.jobLevel == "" || !levelOK
	for _, plan := range qc.plan {
		name := strings.TrimSpace(plan.Name)
		if name == "" {
			continue
		}
		cc.groups[name] = buildGroupCounters(plan, groupSpec(qc.catalog, name), level, now)
	}
	return cc
}

// Evaluate performs authoritative DB aggregation for a single client.
// Used at: startup preload, hourly calibration, admin config changes.
// Replaces in-memory counters with DB-derived values (authoritative).
//
// 与 RecordUsage 共用分片读写锁，校准不能插入持久化与内存增量之间。
// 额度体系 v2：组维度折算用量 = 计费原价 × 组系数；客户端级门槛只剩
// 「未绑职级 = 未配置额度 = 拒绝」。
func (m *Manager) Evaluate(clientName string) {
	m.evaluate(clientName, false)
}

func (m *Manager) evaluate(clientName string, retry bool) {
	if m.clientStore == nil || m.usageStore == nil {
		return
	}
	gate := m.accountingGate(clientName)
	gate.Lock()
	defer gate.Unlock()
	if retry {
		// An explicit/preload calibration may have completed while this retry
		// waited for the accounting gate. Do not drain/evaluate it again.
		m.mu.RLock()
		failed := m.calibrationErrors[clientName] != ""
		ready := m.counters[clientName] != nil && !failed
		backedOff := failed && time.Now().Before(m.calibrationRetryAt[clientName])
		m.mu.RUnlock()
		if ready || backedOff {
			return
		}
	}
	// Journal writes already count in memory even when the primary DB write
	// failed. Never replace those counters until their durable queue is drained.
	if m.usageJournal != nil {
		if err := m.usageJournal.DrainClient(clientName); err != nil {
			m.calibrationFailed(clientName, err)
			return
		}
	}

	// Fetch client config.
	qc, err := m.findQuotaConfig(clientName)
	if err != nil {
		m.calibrationFailed(clientName, err)
		return
	}
	if qc.client == nil {
		// Client deleted: clear all state.
		m.mu.Lock()
		delete(m.exceeded, clientName)
		delete(m.groupExceeded, clientName)
		delete(m.counters, clientName)
		delete(m.calibrationErrors, clientName)
		delete(m.calibrationRetryAt, clientName)
		m.mu.Unlock()
		return
	}

	now := time.Now().UTC()

	// Build rate resolver (cached when available).
	resolver, err := m.getResolver()
	if err != nil {
		m.calibrationFailed(clientName, err)
		return
	}

	cc, err := calibratedCounters(clientName, qc, now, func(filter usage.Filter) ([]nosql.CostUnit, error) {
		return m.usageStore.ReadCostUnits(context.Background(), filter, resolver, true)
	})
	if err != nil {
		m.calibrationFailed(clientName, err)
		return
	}

	// Update exceeded map and counters.
	m.mu.Lock()
	// 职级拒绝：未绑职级（或绑定值不存在）= 未配置额度 = 客户端级拒绝。
	delete(m.exceeded, clientName)
	if cc.jobLevelMissing {
		m.exceeded[clientName] = jobLevelExceededInfo()
	}

	// Replace in-memory counters with DB-derived values.
	m.counters[clientName] = cc
	delete(m.calibrationErrors, clientName)
	delete(m.calibrationRetryAt, clientName)
	delete(m.groupExceeded, clientName)
	for name, gc := range cc.groups {
		for _, dim := range []string{DimensionDaily, DimensionWeekly, DimensionMonthly} {
			dc := gc.dims[dim]
			if dc != nil && dc.limit > 0 && dc.used >= dc.limit {
				if m.groupExceeded[clientName] == nil {
					m.groupExceeded[clientName] = make(map[string]ExceededInfo)
				}
				m.groupExceeded[clientName][name] = ExceededInfo{Scope: ScopeModelGroup, GroupName: name, Dimension: dim, Limit: dc.limit, Used: dc.used, ResetsAt: dc.resetsAt}
				break
			}
		}
	}

	m.mu.Unlock()
}

// calibratedCounters reads the union once, then applies each cost exactly once
// per eligible dimension. The ISO week may begin before the month's first day.
func calibratedCounters(client string, qc quotaConfig, now time.Time, read func(usage.Filter) ([]nosql.CostUnit, error)) (*clientCounters, error) {
	cc := newClientCounters(qc, now)
	starts := make(map[string]time.Time, 3)
	var earliest time.Time
	for _, dim := range []string{DimensionDaily, DimensionWeekly, DimensionMonthly} {
		from, _, err := PeriodRange(dim, now)
		if err != nil {
			return nil, err
		}
		starts[dim] = from
		if earliest.IsZero() || from.Before(earliest) {
			earliest = from
		}
	}
	cells, err := read(usage.Filter{From: &earliest, To: &now, ClientName: client})
	if err != nil {
		return nil, err
	}
	for _, cell := range cells {
		groupName, coefficient := quotaGroupForCounters(cc, cell.Target, cell.Model)
		gc := cc.groups[groupName]
		if gc == nil {
			gc = newFallbackGroupCounters(coefficient, now)
			cc.groups[groupName] = gc
		}
		for dim, from := range starts {
			if !cell.At.Before(from) {
				if dc := gc.dims[dim]; dc != nil {
					dc.used += cell.CostUSD * coefficient
				}
			}
		}
	}
	return cc, nil
}

func (m *Manager) calibrationFailed(clientName string, err error) {
	m.logger.Warn("quota.Manager: calibration unavailable; retaining counters and blocking admission", "client", clientName, "error", err)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calibrationErrors == nil {
		m.calibrationErrors = make(map[string]string)
	}
	m.calibrationErrors[clientName] = err.Error()
	if m.calibrationRetryAt == nil {
		m.calibrationRetryAt = make(map[string]time.Time)
	}
	m.calibrationRetryAt[clientName] = time.Now().Add(calibrationRetryBackoff)
}

// calibrationRetryBackoff spaces request-path recalibration attempts per client.
const calibrationRetryBackoff = 10 * time.Second

// retryCalibrationIfDue recalibrates a blocked client on admission or completion
// at most once per back-off, so one request pays for the retry, not a burst.
func (m *Manager) retryCalibrationIfDue(clientName string) {
	m.mu.Lock()
	if inFlight := m.calibrationInFlight[clientName]; inFlight != nil {
		m.mu.Unlock()
		<-inFlight
		return
	}
	cold := m.counters[clientName] == nil
	failed := m.calibrationErrors[clientName] != ""
	if (!cold && !failed) || (failed && time.Now().Before(m.calibrationRetryAt[clientName])) {
		m.mu.Unlock()
		return
	}
	if m.calibrationInFlight == nil {
		m.calibrationInFlight = make(map[string]chan struct{})
	}
	done := make(chan struct{})
	m.calibrationInFlight[clientName] = done
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.calibrationInFlight, clientName); close(done); m.mu.Unlock() }()
	m.evaluate(clientName, true)
}

func accountingUnavailableInfo() ExceededInfo {
	return ExceededInfo{Scope: ScopeAccounting, Dimension: "unavailable"}
}

// jobLevelExceededInfo is the client-level rejection marker for an unassigned
// (or unknown) job level. ResetsAt is zero: the marker is not period-bound and
// only clears when a valid job level is bound (Evaluate/RefreshAll).
func jobLevelExceededInfo() ExceededInfo {
	return ExceededInfo{Scope: ScopeJobLevel, Dimension: JobLevelUnassignedDimension}
}

// CheckTarget evaluates, for the actually selected target and upstream model,
// the model-group dimension first, then the client-level job-level gate
// (most specific first; any exceeded layer blocks).
func (m *Manager) CheckTarget(clientName, target, model string) (ExceededInfo, bool) {
	// Start preloading has a deadline, so a valid client can reach admission
	// before its counters exist. Load its persisted usage and job-level gate on
	// that first request instead of treating absent state as unlimited.
	m.retryCalibrationIfDue(clientName)
	m.mu.RLock()
	cc := m.counters[clientName]
	if m.calibrationErrors[clientName] != "" {
		m.mu.RUnlock()
		return accountingUnavailableInfo(), true
	}
	if cc != nil {
		groupName, _ := quotaGroupForCounters(cc, target, model)
		if gc := cc.groups[groupName]; gc != nil {
			now := time.Now().UTC()
			for _, dim := range []string{DimensionDaily, DimensionWeekly, DimensionMonthly} {
				dc := gc.dims[dim]
				if dc != nil && dc.limit > 0 && dc.used >= dc.limit && dc.resetsAt.After(now) {
					m.mu.RUnlock()
					return ExceededInfo{Scope: ScopeModelGroup, GroupName: groupName, Dimension: dim, Limit: dc.limit, Used: dc.used, ResetsAt: dc.resetsAt}, true
				}
			}
		}
	}
	m.mu.RUnlock()
	if info, exceeded := m.Check(clientName); exceeded {
		return info, true
	}
	return ExceededInfo{}, false
}

// Check returns whether the client is rejected at the client level (额度 v2：
// 唯一客户端级门槛是「未绑职级」，其标记不随周期翻转清除）。组维度超限由
// CheckTarget 判定。过期的组超限标记仍在这里惰性翻转。
func (m *Manager) Check(clientName string) (ExceededInfo, bool) {
	now := time.Now().UTC()
	m.mu.RLock()
	if m.calibrationErrors[clientName] != "" {
		m.mu.RUnlock()
		return accountingUnavailableInfo(), true
	}
	info, exceeded := m.exceeded[clientName]
	needsFlip := exceeded && info.Scope != ScopeJobLevel && !info.ResetsAt.After(now)
	if !needsFlip {
		needsFlip = countersExpired(m.counters[clientName], now)
	}
	if !needsFlip {
		for _, groupInfo := range m.groupExceeded[clientName] {
			if !groupInfo.ResetsAt.After(now) {
				needsFlip = true
				break
			}
		}
	}
	m.mu.RUnlock()
	if !needsFlip {
		return info, exceeded
	}

	// Lazy period flip: reset every expired group dimension and rebuild
	// exceeded state from the latest counters under the same write lock.
	m.mu.Lock()
	defer m.mu.Unlock()
	now = time.Now().UTC()
	m.flipExpiredLocked(clientName, now)
	info, exceeded = m.exceeded[clientName]
	return info, exceeded
}

func countersExpired(cc *clientCounters, now time.Time) bool {
	if cc == nil {
		return false
	}
	for _, gc := range cc.groups {
		for _, dc := range gc.dims {
			if dc != nil && !dc.resetsAt.After(now) {
				return true
			}
		}
	}
	return false
}

// flipExpiredLocked resets only dimensions whose period ended, then derives
// the client-level (job-level) and group exceeded state from the current
// counters. The caller must hold m.mu for writing so a concurrent new-period
// Increment cannot be erased.
func (m *Manager) flipExpiredLocked(clientName string, now time.Time) {
	cc := m.counters[clientName]
	if cc == nil {
		if info, ok := m.exceeded[clientName]; ok && info.Scope != ScopeJobLevel && !info.ResetsAt.After(now) {
			delete(m.exceeded, clientName)
		}
		for name, info := range m.groupExceeded[clientName] {
			if !info.ResetsAt.After(now) {
				delete(m.groupExceeded[clientName], name)
			}
		}
		if len(m.groupExceeded[clientName]) == 0 {
			delete(m.groupExceeded, clientName)
		}
		return
	}

	resetExpired := func(dims map[string]*dimCounter) {
		for _, dc := range dims {
			if dc == nil || dc.resetsAt.After(now) {
				continue
			}
			resetsAt, err := NextResetAt(dc.period, now)
			if err != nil {
				continue
			}
			dc.used = 0
			dc.resetsAt = resetsAt
		}
	}
	for _, gc := range cc.groups {
		resetExpired(gc.dims)
	}

	// 职级拒绝与周期无关：只要仍未绑定有效职级就持续生效。
	if cc.jobLevelMissing {
		m.exceeded[clientName] = jobLevelExceededInfo()
	} else {
		delete(m.exceeded, clientName)
	}

	delete(m.groupExceeded, clientName)
	for name, gc := range cc.groups {
		for _, dim := range []string{DimensionDaily, DimensionWeekly, DimensionMonthly} {
			dc := gc.dims[dim]
			if dc != nil && dc.limit > 0 && dc.used >= dc.limit {
				if m.groupExceeded[clientName] == nil {
					m.groupExceeded[clientName] = make(map[string]ExceededInfo)
				}
				m.groupExceeded[clientName][name] = ExceededInfo{Scope: ScopeModelGroup, GroupName: name, Dimension: dim, Limit: dc.limit, Used: dc.used, ResetsAt: dc.resetsAt}
				break
			}
		}
	}
}

// Status returns the quota usage report for a client.
// Uses in-memory counters for fast response; if no counters exist,
// falls back to DB aggregation (first request after restart before preload).
// 额度体系 v2：Quotas 为 plan 内各模型组的聚合视图（Σ有效上限 vs Σ折算
// 用量；任一组该维度不限 → Limit=0 表示不限），逐组明细在 Groups。
func (m *Manager) Status(clientName string) QuotaStatus {
	status := QuotaStatus{
		Client: clientName,
		Quotas: make(map[string]QuotaUsage),
		Groups: make(map[string]GroupStatus),
	}
	// Check also performs the lazy period flip for group counters.
	if info, exceeded := m.Check(clientName); exceeded {
		status.Exceeded = &info
	}

	m.mu.RLock()
	status.CalibrationError = m.calibrationErrors[clientName]
	cc := m.counters[clientName]
	if cc != nil {
		// Fast path: read from in-memory counters.
		// 字段拷贝必须在 RLock 内完成 —— Increment/Evaluate 在写锁下原地修改
		// 同一 *dimCounter（used/resetsAt），锁外裸读构成 data race（旧实现
		// RUnlock 后才遍历 dims）。dims map 创建后无结构性变更，锁内遍历安全。
		status.JobLevel = cc.jobLevel
		status.JobLevelMissing = cc.jobLevelMissing
		type agg struct {
			limit     float64
			used      float64
			resetsAt  time.Time
			unlimited bool
		}
		aggregates := make(map[string]*agg)
		for name, gc := range cc.groups {
			gs := GroupStatus{
				Coefficient:      gc.coefficient,
				PlanCoefficient:  gc.planCoefficient,
				RankCoefficients: gc.rankCoefficients,
				BaseLimits:       gc.baseLimits,
				InPlan:           gc.inPlan,
				Quotas:           make(map[string]QuotaUsage),
			}
			for dim, dc := range gc.dims {
				gs.Quotas[dim] = QuotaUsage{Limit: dc.limit, Used: dc.used, ResetsAt: dc.resetsAt}
				if gc.inPlan {
					a := aggregates[dim]
					if a == nil {
						a = &agg{resetsAt: dc.resetsAt}
						aggregates[dim] = a
					}
					a.used += dc.used
					if dc.limit <= 0 {
						a.unlimited = true
					} else {
						a.limit += dc.limit
					}
				}
			}
			if info, exceeded := m.groupExceeded[clientName][name]; exceeded && info.ResetsAt.After(time.Now().UTC()) {
				copy := info
				gs.Exceeded = &copy
			}
			status.Groups[name] = gs
		}
		for dim, a := range aggregates {
			limit := a.limit
			if a.unlimited {
				limit = 0
			}
			status.Quotas[dim] = QuotaUsage{Limit: limit, Used: a.used, ResetsAt: a.resetsAt}
		}
		m.mu.RUnlock()
	} else {
		m.mu.RUnlock()
		// Slow path: build the authoritative in-memory snapshot before rendering.
		if m.clientStore == nil || m.usageStore == nil {
			return status
		}
		m.retryCalibrationIfDue(clientName)
		m.mu.RLock()
		ready := m.counters[clientName] != nil
		status.CalibrationError = m.calibrationErrors[clientName]
		m.mu.RUnlock()
		if !ready {
			if info, exceeded := m.Check(clientName); exceeded {
				status.Exceeded = &info
			}
			return status
		}
		return m.Status(clientName)
	}

	// Read exceeded status.
	if info, exceeded := m.Check(clientName); exceeded {
		status.Exceeded = &info
	}

	return status
}

// getResolver returns the rate resolver from the cache when configured,
// otherwise falls back to a direct List+ToCostTable build (backward
// compatibility for tests that construct Manager without a CostCache).
// *pricing.Resolver and usage.CostTable both implement usage.RateResolver.
func (m *Manager) getResolver() (usage.RateResolver, error) {
	if m.costCache != nil {
		return m.costCache.GetChecked() // accounting cannot use degraded report estimates
	}
	var costs []nosql.ModelCost
	if m.costStore != nil {
		var err error
		costs, err = m.costStore.List()
		if err != nil {
			return nil, fmt.Errorf("load model costs: %w", err)
		}
	}
	return costutil.ToCostTable(costs, m.catalog), nil
}

// computeCost calculates USD cost from token counts using the rate resolver.
func (m *Manager) computeCost(target, model string, at time.Time, inputTokens, outputTokens, cachedTokens, cacheCreationTokens int64, partitions ...int64) (float64, error) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return 0, nil
	}
	resolver, err := m.getResolver()
	if err != nil {
		return 0, err
	}
	rates, ok := resolver.RateFor(target, model, at)
	if !ok {
		// Unknown model: no charge. This is intentional and consistent with
		// Evaluate's RateFor-failure branch (which skips the cell): a model
		// we cannot price is never counted, so in-memory counters and DB
		// aggregation agree. If pricing is added later, the next hourly
		// calibration reconciles the true spend from the DB.
		return 0, nil
	}
	return rates.CostFor(inputTokens, outputTokens, cachedTokens, cacheCreationTokens, partitions...), nil
}

// buildGroupCounters computes one plan group's effective per-dimension limits
// (额度体系 v2 公式)：
//
//	limit(dim) = 组基数(dim) × 职级系数(dim) × 个人系数
//
// 个人、职级或组系数为 0 → limit=0（沿用计数器「limit<=0 = 不限」语义）。负系数按
// 中性 1 处理（写入侧已校验非负，这里只是防御）。职级缺失由客户端级
// jobLevelMissing 拒绝表达，此处按中性系数 1 展示。
func buildGroupCounters(plan config.QuotaGroup, spec config.ModelGroup, level config.JobLevel, now time.Time) *groupCounters {
	groupCoef := spec.CostCoefficient
	if groupCoef < 0 {
		groupCoef = 1
	}
	planCoef := plan.PlanCoefficient()
	if planCoef < 0 {
		planCoef = 1
	}
	gc := &groupCounters{
		coefficient:      groupCoef,
		planCoefficient:  planCoef,
		rankCoefficients: make(map[string]float64, 3),
		baseLimits:       make(map[string]float64, 3),
		inPlan:           true,
		dims:             make(map[string]*dimCounter),
	}
	gc.bindings = append([]config.QuotaGroupBinding(nil), spec.Bindings...)
	bases := map[string]float64{
		DimensionDaily:   spec.DailyUSD,
		DimensionWeekly:  spec.WeeklyUSD,
		DimensionMonthly: spec.MonthlyUSD,
	}
	for _, dim := range []string{DimensionDaily, DimensionWeekly, DimensionMonthly} {
		rankCoef := level.CoefficientFor(dim)
		if rankCoef < 0 {
			rankCoef = 1
		}
		base := bases[dim]
		gc.rankCoefficients[dim] = rankCoef
		gc.baseLimits[dim] = base
		reset, err := NextResetAt(dim, now)
		if err != nil {
			continue
		}
		limit := 0.0
		if base > 0 && rankCoef > 0 && planCoef > 0 && groupCoef > 0 {
			limit = base * rankCoef * planCoef
		}
		gc.dims[dim] = &dimCounter{limit: limit, resetsAt: reset, period: dim}
	}
	return gc
}

// newFallbackGroupCounters builds counters for a group the client's plan does
// not explicitly contain (company_general 兜底或未入 plan 的归属流量)：无基数
// → 各维不限，折算累计仅用于计量展示。
func newFallbackGroupCounters(coefficient float64, now time.Time) *groupCounters {
	if coefficient < 0 {
		coefficient = 1
	}
	gc := &groupCounters{
		coefficient:      coefficient,
		planCoefficient:  1,
		rankCoefficients: map[string]float64{DimensionDaily: 1, DimensionWeekly: 1, DimensionMonthly: 1},
		baseLimits:       map[string]float64{DimensionDaily: 0, DimensionWeekly: 0, DimensionMonthly: 0},
		dims:             make(map[string]*dimCounter),
	}
	for _, dim := range []string{DimensionDaily, DimensionWeekly, DimensionMonthly} {
		reset, err := NextResetAt(dim, now)
		if err != nil {
			continue
		}
		gc.dims[dim] = &dimCounter{limit: 0, resetsAt: reset, period: dim}
	}
	return gc
}

func quotaGroupForCounters(cc *clientCounters, target, model string) (string, float64) {
	if cc == nil {
		return "company_general", 1
	}
	target, model = strings.ToLower(strings.TrimSpace(target)), strings.ToLower(strings.TrimSpace(model))
	for name, group := range cc.groups {
		for _, binding := range group.bindings {
			if strings.EqualFold(binding.Target, target) && binding.Model != "" && strings.EqualFold(binding.Model, model) {
				return name, group.coefficient
			}
		}
	}
	for name, group := range cc.groups {
		for _, binding := range group.bindings {
			if strings.EqualFold(binding.Target, target) && binding.Model == "" {
				return name, group.coefficient
			}
		}
	}
	return "company_general", 1
}

// groupSpec resolves a plan group name against the global catalog, returning
// the full ModelGroup (base limits + cost coefficient + bindings) used by the
// quota v2 formula. Unknown names and the built-in company_general group fall
// back to a neutral spec (coefficient 1, zero bases → unlimited, no bindings).
func groupSpec(catalog map[string]config.ModelGroup, name string) config.ModelGroup {
	if strings.EqualFold(strings.TrimSpace(name), "company_general") {
		return config.ModelGroup{Name: "company_general", CostCoefficient: 1}
	}
	if catalog != nil {
		if mg, ok := catalog[strings.ToLower(strings.TrimSpace(name))]; ok {
			return mg
		}
	}
	return config.ModelGroup{Name: name, CostCoefficient: 1}
}

// jobLevelCatalog loads the structured job-level catalog into a lookup map
// keyed by lowercased name. Returns nil when the store is unset or load fails
// (every client then evaluates as job-level-missing → rejected).
func (m *Manager) jobLevelCatalog() (map[string]config.JobLevel, error) {
	if m.jobLevelStore == nil {
		return nil, nil
	}
	levels, err := m.jobLevelStore.List()
	if err != nil {
		m.logger.Warn("quota.Manager: job level catalog load failed", "error", err)
		return nil, err
	}
	cat := make(map[string]config.JobLevel, len(levels))
	for _, l := range levels {
		cat[strings.ToLower(strings.TrimSpace(l.Name))] = l
	}
	return cat, nil
}

// findQuotaConfig loads a client's quota v2 config in one step: the client
// record, its plan (per-client group selections with personal coefficients),
// the global model-group catalog and the job-level catalog.
func (m *Manager) findQuotaConfig(clientName string) (quotaConfig, error) {
	var qc quotaConfig
	if m.clientStore == nil {
		return qc, nil
	}
	client, err := m.clientStore.Get(clientName)
	if err != nil {
		return qc, err
	}
	qc.client = client
	if qc.client == nil {
		return qc, nil
	}
	if m.clientQuotaStore != nil {
		qc.plan, err = m.clientQuotaStore.Get(clientName)
		if err != nil {
			return qc, err
		}
	}
	qc.catalog, err = m.modelGroupCatalog()
	if err != nil {
		return qc, err
	}
	qc.levels, err = m.jobLevelCatalog()
	return qc, err
}

// modelGroupCatalog loads the global model-group catalog into a lookup map
// keyed by lowercased group name. Returns nil when the store is unset or load
// fails.
func (m *Manager) modelGroupCatalog() (map[string]config.ModelGroup, error) {
	if m.modelGroupStore == nil {
		return nil, nil
	}
	groups, err := m.modelGroupStore.List()
	if err != nil {
		m.logger.Warn("quota.Manager: model group catalog load failed", "error", err)
		return nil, err
	}
	cat := make(map[string]config.ModelGroup, len(groups))
	for _, g := range groups {
		cat[strings.ToLower(strings.TrimSpace(g.Name))] = g
	}
	return cat, nil
}
