package quota

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// 后台刷新（admin 写操作之后的额度重算）。
//
// 最终一致窗口：写操作提交后，受影响客户端的内存计数器在
// 「去抖（refreshDebounce）+ 排队 + 该客户端 Evaluate 耗时」之后才换成新配置
// 派生的值。窗口内准入仍使用已校准的旧计数器（旧模型组绑定/系数/限额），
// 不会放行未校准客户端；窗口只影响「配置变更何时对额度判定生效」，不影响
// 用量记账（Increment 与校准仍按各自的分片锁互斥）。
// 配置代次（NoteConfigChange）保证正在进行的校准不会用提交前读到的旧配置落地计数器：
// 代次变化则丢弃重算。小时整点校准会兜底收敛任何遗漏。

const (
	// refreshDebounce 合并连续写操作：窗口内的多次调度只触发一轮刷新。
	defaultRefreshDebounce = 300 * time.Millisecond
	// refreshConcurrency 限制后台刷新的并发 Evaluate 数，避免与请求争抢 bbolt 读。
	refreshConcurrency = 2
	// preloadConcurrency 限制启动预加载的并发 Evaluate 数。
	preloadConcurrency = 4
)

// calibrationRequestTimeout 是请求路径等待校准完成的上限。超时按校准失败处理
// （拒绝准入并退避重试），后台校准继续并在完成后自动恢复。
// 一次 evaluate 本身需要 2.5-3s，上限太小会把「慢」变成「拒绝」。
var calibrationRequestTimeout = 15 * time.Second

var errCalibrationTimeout = errors.New("calibration timed out on request path; retrying in background")

// RefreshScope 描述一次写操作可能影响的客户端范围，各字段取并集。
type RefreshScope struct {
	// All 刷新全部额度客户端并清理已不存在的内存状态（计价变更等全局影响）。
	All bool
	// Targets：模型组绑定中引用了这些目标的客户端（目标删除/改名）。
	Targets []string
	// Groups：plan 包含这些模型组的客户端（模型组增改删，含改名前后的名字）。
	Groups []string
	// JobLevels：绑定了这些职级的客户端（职级系数变更，含改名前后的名字）。
	JobLevels []string
	// Clients：显式指定的客户端。
	Clients []string
}

func (s RefreshScope) empty() bool {
	return !s.All && len(s.Targets) == 0 && len(s.Groups) == 0 && len(s.JobLevels) == 0 && len(s.Clients) == 0
}

type refreshSet struct {
	all       bool
	targets   map[string]struct{}
	groups    map[string]struct{}
	jobLevels map[string]struct{}
	clients   map[string]struct{}
}

func normKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func addAll(dst *map[string]struct{}, names []string, norm bool) {
	for _, n := range names {
		if norm {
			n = normKey(n)
		} else {
			n = strings.TrimSpace(n)
		}
		if n == "" {
			continue
		}
		if *dst == nil {
			*dst = make(map[string]struct{})
		}
		(*dst)[n] = struct{}{}
	}
}

func (r *refreshSet) merge(s RefreshScope) {
	r.all = r.all || s.All
	addAll(&r.targets, s.Targets, true)
	addAll(&r.groups, s.Groups, true)
	addAll(&r.jobLevels, s.JobLevels, true)
	addAll(&r.clients, s.Clients, false)
}

func (r refreshSet) empty() bool {
	return !r.all && len(r.targets) == 0 && len(r.groups) == 0 && len(r.jobLevels) == 0 && len(r.clients) == 0
}

// ScheduleRefresh 登记一次后台、去抖的刷新并立即返回；永不阻塞调用方，也不要求
// 调用方不持锁以外的任何条件。连续调用在去抖窗口内合并为一轮。
func (m *Manager) ScheduleRefresh(scope RefreshScope) {
	if scope.empty() {
		return
	}
	// 先推进配置代次（调用方已提交写入）：此刻起，所有读取了旧配置的 evaluate 都会在
	// 写计数器前发现代次变化而重算，包括「正在校准、尚无计数器」的客户端。
	m.NoteConfigChange()
	m.enqueueRefresh(scope)
}

// enqueueRefresh 登记去抖刷新但不推进配置代次（churn 延后重算使用：推进代次只会制造更多 churn）。
func (m *Manager) enqueueRefresh(scope RefreshScope) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if m.refreshStopped {
		return
	}
	m.refreshPending.merge(scope)
	if m.refreshActive {
		return // 已有定时器或运行中的循环，会取走新增内容
	}
	m.refreshActive = true
	debounce := m.refreshDebounce
	if debounce <= 0 {
		debounce = defaultRefreshDebounce
	}
	m.refreshTimer = time.AfterFunc(debounce, m.refreshLoop)
}

// NoteConfigChange 推进配置代次，不排期刷新。会影响额度判定的管理写入在提交成功
// 之后调用（ScheduleRefresh 已包含）；在途校准据此丢弃按旧配置算出的结果。
func (m *Manager) NoteConfigChange() uint64 { return m.configGen.Add(1) }

// RefreshStatus 是后台刷新的可回读状态。
type RefreshStatus struct {
	// ConfigGeneration 是已提交的额度相关配置变更次数（单调递增）。
	ConfigGeneration uint64 `json:"config_generation"`
	// Completed 是已完成的后台刷新批次数。
	Completed uint64 `json:"completed_batches"`
	// Pending 为 true 表示有刷新已排期或正在运行，窗口内准入仍用旧计数器。
	Pending bool `json:"pending"`
}

// RefreshStatus 回读后台刷新状态。
func (m *Manager) RefreshStatus() RefreshStatus {
	m.refreshMu.Lock()
	pending := m.refreshActive
	m.refreshMu.Unlock()
	return RefreshStatus{ConfigGeneration: m.configGen.Load(), Completed: m.refreshDone.Load(), Pending: pending}
}

// FlushRefresh 阻塞直到没有待处理或运行中的刷新（测试与停机使用）。
func (m *Manager) FlushRefresh() {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	for m.refreshActive {
		m.refreshCond.Wait()
	}
}

func (m *Manager) refreshLoop() {
	for {
		m.refreshMu.Lock()
		if m.refreshStopped || m.refreshPending.empty() {
			m.refreshPending = refreshSet{}
			m.refreshActive = false
			m.refreshCond.Broadcast()
			m.refreshMu.Unlock()
			return
		}
		batch := m.refreshPending
		m.refreshPending = refreshSet{}
		m.refreshMu.Unlock()
		m.runRefresh(batch)
		m.refreshDone.Add(1)
	}
}

func (m *Manager) runRefresh(batch refreshSet) {
	if m.clientStore == nil {
		return
	}
	var names []string
	if batch.all {
		all, err := m.quotaClientNames()
		if err != nil {
			m.logger.Warn("quota.Manager: refresh quotaClientNames failed", "error", err)
			return
		}
		names = all
		defer m.dropInactive(names)
	} else {
		names = m.affectedClients(batch)
	}
	m.evaluateMany(names, refreshConcurrency, m.stopChan(), false)
}

// affectedClients 依据内存计数器（上一次校准时的绑定/职级/plan）找出受影响客户端。
// 另外一律纳入：正在校准（calibrationInFlight）与处于校准失败（calibrationErrors）
// 的客户端，它们要么还没有计数器、要么计数器不可信，不能靠计数器内容判断是否受影响。
func (m *Manager) affectedClients(b refreshSet) []string {
	set := make(map[string]struct{}, len(b.clients))
	for n := range b.clients {
		set[n] = struct{}{}
	}
	m.mu.RLock()
	for name := range m.calibrationInFlight {
		set[name] = struct{}{}
	}
	for name := range m.calibrationErrors {
		set[name] = struct{}{}
	}
	for name, cc := range m.counters {
		if cc == nil {
			continue
		}
		if _, ok := b.jobLevels[normKey(cc.jobLevel)]; ok {
			set[name] = struct{}{}
			continue
		}
	groups:
		for gname, gc := range cc.groups {
			if _, ok := b.groups[normKey(gname)]; ok {
				set[name] = struct{}{}
				break
			}
			for _, binding := range gc.bindings {
				if _, ok := b.targets[normKey(binding.Target)]; ok {
					set[name] = struct{}{}
					break groups
				}
			}
		}
	}
	m.mu.RUnlock()
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	return names
}

// evaluateMany 以有界并发评估 names；done 关闭后不再启动新的评估。
func (m *Manager) evaluateMany(names []string, workers int, done <-chan struct{}, retry bool) {
	if len(names) == 0 {
		return
	}
	if workers > len(names) {
		workers = len(names)
	}
	jobs := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for name := range jobs {
				m.runEvaluate(name, retry)
			}
		}()
	}
feed:
	for _, name := range names {
		select {
		case <-done:
			break feed
		default:
		}
		select {
		case jobs <- name:
		case <-done:
			break feed
		}
	}
	close(jobs)
	wg.Wait()
}

// runEvaluate 是后台刷新、预加载与请求路径校准的统一入口（evaluateHook 供测试注入慢评估）。
func (m *Manager) runEvaluate(name string, retry bool) {
	// 校准 panic 不得带崩进程或留下假「已就绪」状态：记录后按校准失败处理（fail-closed）。
	defer func() {
		if r := recover(); r != nil {
			m.logger.Error("quota.Manager: calibration panicked", "client", name, "panic", fmt.Sprint(r))
			m.calibrationFailed(name, fmt.Errorf("calibration panicked: %v", r))
		}
	}()
	if m.evaluateHook != nil {
		m.evaluateHook(name, retry)
		return
	}
	m.evaluate(name, retry)
}

// dropInactive 清理已不再有任何额度配置的客户端的内存状态。
func (m *Manager) dropInactive(activeNames []string) {
	active := make(map[string]struct{}, len(activeNames))
	for _, n := range activeNames {
		active[n] = struct{}{}
	}
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

// stopRefresh 在 Stop 时调用：不再接收新刷新，等待运行中的循环退出。
func (m *Manager) stopRefresh() {
	m.refreshMu.Lock()
	m.refreshStopped = true
	if m.refreshTimer != nil && m.refreshTimer.Stop() {
		m.refreshActive = false
		m.refreshCond.Broadcast()
	}
	for m.refreshActive {
		m.refreshCond.Wait()
	}
	m.refreshMu.Unlock()
}
