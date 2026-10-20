// target.go — 目标选择（round-robin + muting）与健康状态管理。
package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

type targetState struct {
	target *Target

	*targetHealthState

	// inFlight 节点级在途请求数（S2 容量感知调度）：自管理原子计数，
	// 不受 mu 保护；acquireSlot/releaseSlot 成对推进，maxConcurrency 为 0 时仅计数不限流。
	runtimeWindow *targetRuntimeWindow
	inFlight      *atomic.Int64 // 同名目标的配置代际共享计数，旧响应关闭时仍释放当前容量

	keyPool *keyPool
}

// targetHealthState is shared across same-name configuration generations unless
// the upstream connection identity changes or the effective target resumes. Those
// transitions copy cumulative observations into fresh health, clearing cooldown,
// failure streak and probe lease. In-flight results settle on their original health
// object; detached generations cannot mute or release a probe on the new identity.
type targetHealthState struct {
	mu                 sync.RWMutex
	mutedUntil         time.Time
	lastSuccess        time.Time
	lastFailure        time.Time
	consecutiveFailure int
	totalSuccess       int64
	totalFailure       int64

	// S4 熔断半开状态机（派生态：由 mutedUntil + consecutiveFailure 推导，不引入第二权威）：
	//   ready      —— 连续失败 < breakerThreshold
	//   cooling    —— mutedUntil 未到期（沿用既有静默权威）
	//   half-open  —— 冷却到期且连续失败 ≥ 阈值：仅放行独占探测位上的探测请求，
	//                 成功回 ready 清零计数；失败回 cooling 且指数延长冷却。
	// breakerThreshold 仅在构造/测试期写入。探针租约对象的身份属于选中它的请求；
	// 到期后可被替换，但旧请求只能释放自己的对象。
	breakerThreshold int
	probeLease       atomic.Pointer[probeLease]

	// S5 调度可观测（议题 #18）：节点级亲和命中数与熔断次数计数器（自管理原子计数，
	// 只增不清零，供 /admin/metrics 与 admin UI 展示调度行为）。
	affinityHits atomic.Int64
	breakerTrips atomic.Int64
}

type probeLease struct{ until int64 }

func (s *targetState) releaseProbe(lease *probeLease) {
	if lease != nil {
		s.probeLease.CompareAndSwap(lease, nil)
	}
}

// TargetStats is a snapshot of target runtime metrics.
type TargetStats struct {
	MutedUntil         time.Time
	LastSuccess        time.Time
	LastFailure        time.Time
	ConsecutiveFailure int
	TotalSuccess       int64
	TotalFailure       int64
}

func newTargetState(t *Target, logger *slog.Logger) *targetState {
	s := &targetState{target: t, targetHealthState: &targetHealthState{breakerThreshold: breakerDefaultConsecutiveFailures}, inFlight: &atomic.Int64{}, runtimeWindow: &targetRuntimeWindow{}}
	if len(t.APIKeys) > 1 {
		s.keyPool = newKeyPool(t.Name, t.APIKeys, t.KeyResetTime, logger)
	}
	return s
}

func (s *targetState) Target() *Target {
	return s.target
}

func (s *targetState) IsMuted(now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mutedUntil.After(now)
}

func (s *targetState) NextRetry() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mutedUntil
}

func (s *targetState) MarkFailure(now time.Time, quietPeriod time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastFailure = now
	s.consecutiveFailure++
	s.totalFailure++
	if quietPeriod <= 0 {
		quietPeriod = 60 * time.Second
	}
	// S4：连续失败达到熔断阈值才进入冷却（muted）；未达阈值只累计计数，
	// 节点保持 ready 可调度。默认阈值为 1，与既有「单次失败即静默」行为完全一致。
	threshold := s.breakerThreshold
	if threshold <= 0 {
		threshold = breakerDefaultConsecutiveFailures
	}
	if s.consecutiveFailure >= threshold {
		s.mutedUntil = now.Add(s.breakerCooldown(quietPeriod))
		// S5 可观测：每次跨入/延长冷却计一次熔断事件（含 half-open 探测失败的指数延长）。
		s.breakerTrips.Add(1)
		s.runtimeWindow.record(now, 0, 0, 1)
	}
}

// breakerCooldown 计算本次失败后的冷却时长（S4）。连续失败达到阈值前按基线静默；
// 达到阈值后每多一次失败冷却翻倍（half-open 探测失败的延长冷却），
// 封顶 breakerMaxBackoffFactor 倍基线。调用方需持有 s.mu。
func (s *targetState) breakerCooldown(base time.Duration) time.Duration {
	threshold := s.breakerThreshold
	if threshold <= 0 {
		threshold = breakerDefaultConsecutiveFailures
	}
	cooldown := base
	cap := time.Duration(breakerMaxBackoffFactor) * base
	for i := threshold; i < s.consecutiveFailure && cooldown < cap; i++ {
		cooldown *= 2
	}
	return cooldown
}

func (s *targetState) MarkSuccess(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSuccess = now
	s.consecutiveFailure = 0
	s.totalSuccess++
	s.mutedUntil = time.Time{}
}

// Stats returns a snapshot of the target state metrics.
func (s *targetState) Stats() TargetStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return TargetStats{
		MutedUntil:         s.mutedUntil,
		LastSuccess:        s.lastSuccess,
		LastFailure:        s.lastFailure,
		ConsecutiveFailure: s.consecutiveFailure,
		TotalSuccess:       s.totalSuccess,
		TotalFailure:       s.totalFailure,
	}
}

// acquireSlot 尝试占用一个节点并发槽位；硬上限已满返回 false（S2 硬容量保护）。
// MaxConcurrency<=0 视为不限容量，恒成功仅计数。
func (s *targetState) acquireSlot() bool {
	s.runtimeWindow.mu.Lock()
	defer s.runtimeWindow.mu.Unlock()
	cap := s.target.MaxConcurrency
	if cap <= 0 {
		s.inFlight.Add(1)
		s.runtimeWindow.recordLocked(time.Now(), 1, 0, 0)
		return true
	}
	for {
		cur := s.inFlight.Load()
		if cur >= int64(cap) {
			return false
		}
		if s.inFlight.CompareAndSwap(cur, cur+1) {
			s.runtimeWindow.recordLocked(time.Now(), 1, 0, 0)
			return true
		}
	}
}

func (s *targetState) releaseSlot() {
	s.runtimeWindow.mu.Lock()
	defer s.runtimeWindow.mu.Unlock()
	s.runtimeWindow.recordLocked(time.Now(), -1, 0, 0)
	if s.inFlight.Add(-1) < 0 {
		s.inFlight.Store(0)
	}
}

func (s *targetState) inFlightCount() int64 {
	return s.inFlight.Load()
}

// normalizedLoad 归一化负载 = 在途数 / max(容量,1)。无容量上限的节点分母取 1，
// 仅用于调度优先级比较；其对外利用率恒为 0。
func (s *targetState) normalizedLoad() float64 {
	denom := int64(s.target.MaxConcurrency)
	if denom <= 0 {
		denom = 1
	}
	return float64(s.inFlight.Load()) / float64(denom)
}

// affinitySoftCapRatio 亲和回粘的归一化负载软上限（S3）：命中粘连且节点健康的
// 前提下，仅当归一化负载严格低于该值才回粘原节点；达到或超过时保留粘连记录、
// 流量按 S2 负载调度分流到其他健康节点，负载回落(<)后自动回粘。
const affinitySoftCapRatio = 0.8

// belowAffinitySoftCap 报告节点归一化负载是否低于亲和回粘软上限。
// 未配置容量的节点仅计数不限流，恒视为可回粘（不改变既有粘连行为）。
func (s *targetState) belowAffinitySoftCap() bool {
	if s.target.MaxConcurrency <= 0 {
		return true
	}
	return s.normalizedLoad() < affinitySoftCapRatio
}

const (
	// breakerDefaultConsecutiveFailures：连续失败进入冷却（muted）的默认阈值。
	// 取 1 保持与既有「单次失败即静默」完全一致的默认行为，S4 状态机为纯增量；
	// 状态机语义（含 N>1 的连续故障聚合）由单测以显式阈值覆盖。
	breakerDefaultConsecutiveFailures = 1

	// breakerMaxBackoffFactor：half-open 探测失败后的冷却延长封顶倍数（8x 基线），
	// 防止反复失败把恢复等待推到不可接受的长尾。
	breakerMaxBackoffFactor = 8

	// breakerProbeLease：half-open 探测位的租约时长，防止请求悬挂时永久阻塞恢复窗口。
	breakerProbeLease = 2 * time.Minute
)

// admitDecision 描述 S4 准入判定结果。
type admitDecision int

const (
	admitOpen      admitDecision = iota // ready，或 half-open 窗口内抢到探测位：放行
	admitCooling                        // 冷却期内（mutedUntil 未到期）：拒绝
	admitProbeBusy                      // half-open 且探测位被占用：拒绝，避免流量打到未恢复节点
)

// admitWithProbeState 是目标选择前的统一准入闸门（ready→cooling→half-open 三态全由此推导）：
//   - mutedUntil 未到期 ⇒ cooling；
//   - 连续失败低于阈值 ⇒ ready 直接放行；
//   - 否则处于冷却已到期的 half-open 恢复窗口：仅独占探测位的一路请求获准充当探针
//     （原子 CAS 抢占租约位），其余请求被拒以保证未确认恢复的节点不再吸入流量。
//
// 非空租约必须传递给选中的请求，并在健康结算或无上游结果的退出时释放；
// 抢到却改选他节点会把租约孤立 breakerProbeLease 时长，期间该节点三路选路全拒。
func (s *targetState) admitWithProbeState(now time.Time) (admitDecision, *probeLease) {
	s.mu.RLock()
	muted := s.mutedUntil.After(now)
	streak := s.consecutiveFailure
	threshold := s.breakerThreshold
	s.mu.RUnlock()
	if muted {
		return admitCooling, nil
	}
	if threshold <= 0 {
		threshold = breakerDefaultConsecutiveFailures
	}
	if streak < threshold {
		return admitOpen, nil
	}
	lease := &probeLease{until: now.Add(breakerProbeLease).UnixNano()}
	for {
		cur := s.probeLease.Load()
		if cur != nil && cur.until > now.UnixNano() {
			return admitProbeBusy, nil
		}
		if s.probeLease.CompareAndSwap(cur, lease) {
			return admitOpen, lease
		}
	}
}

// probeSlotBusy 只读探测位占用检查（不参与竞争、不改状态）。
// 供选择器对兜底候选做错误分类，避免二次 admit 误占租约。
func (s *targetState) probeSlotBusy(now time.Time) bool {
	lease := s.probeLease.Load()
	return lease != nil && lease.until > now.UnixNano()
}

// S5 调度相位令牌（对外观测口径，admin UI 按此渲染徽章）。
const (
	phaseReady    = "ready"     // 无未恢复故障，全量接客
	phaseMuted    = "muted"     // 冷却期内（连续失败达熔断阈值）
	phaseHalfOpen = "half-open" // 冷却到期、仅独占探测位放行
	phaseOffline  = "offline"   // paused / 已移除：任意熔断状态下零流量
)

// AllTargetPhaseOptions exposes the same phase values used by the breaker.
func AllTargetPhaseOptions() []config.EnumOption {
	return []config.EnumOption{
		{Value: phaseReady, Label: "正常"}, {Value: phaseMuted, Label: "冷却"},
		{Value: phaseHalfOpen, Label: "恢复"}, {Value: phaseOffline, Label: "暂停"},
	}
}

// Phase 由既有 S4 权威态派生单节点调度相位（只读，不改任何状态）：
// paused 优先于一切（offline）→ mutedUntil 未到期（muted）→ 连续失败 ≥ 阈值即处于
// half-open 恢复窗口（探测位空闲或在途都算窗口开启）→ 否则 ready。
func (s *targetState) Phase(now time.Time) string {
	s.mu.RLock()
	muted := s.mutedUntil.After(now)
	streak := s.consecutiveFailure
	threshold := s.breakerThreshold
	paused := s.target != nil && s.target.Paused
	s.mu.RUnlock()
	if paused {
		return phaseOffline
	}
	if muted {
		return phaseMuted
	}
	if threshold <= 0 {
		threshold = breakerDefaultConsecutiveFailures
	}
	if streak >= threshold {
		return phaseHalfOpen
	}
	return phaseReady
}

func (s *targetState) affinityHitCount() int64 { return s.affinityHits.Load() }
func (s *targetState) breakerTripCount() int64 { return s.breakerTrips.Load() }

// selectionKind 描述目标选择的路径，用于决定是否更新连接粘连。
type selectionKind int

const (
	selectionExplicit    selectionKind = iota // 用户显式指定目标（header/query）
	selectionAffinityHit                      // 粘连命中，目标健康可用
	selectionRoundRobin                       // 无粘连记录，轮询选出（首次或过期后）
	selectionFailover                         // 有粘连记录但目标不可用，failover 到其他目标
)

func (s *Service) selectTarget(
	principal *auth.Principal,
	clientIP string,
	sessionBoundTarget string, // 请求进入循环前读取的会话绑定；空值表示无绑定
	requestedLower string,
	allowed map[string]struct{},
	attempted map[string]struct{},
	model string,
	path string,
	now time.Time,
) (*targetState, *probeLease, selectionKind, error) {
	if requestedLower != "" {
		if !principal.CanAccess(requestedLower) {
			return nil, nil, selectionExplicit, newSelectionError(http.StatusForbidden, "target not allowed")
		}
		state, ok := s.targetByName(requestedLower)
		if !ok {
			return nil, nil, selectionExplicit, newSelectionError(http.StatusBadRequest, "unknown target")
		}
		if !modelAllowed(state.Target(), model) {
			return nil, nil, selectionExplicit, newSelectionError(http.StatusBadRequest, fmt.Sprintf("model %q not allowed for target %q", model, state.Target().Name))
		}
		if state.Target().Paused {
			return nil, nil, selectionExplicit, newSelectionError(http.StatusServiceUnavailable, "requested target paused")
		}
		if !state.Target().SupportsPath(path) {
			return nil, nil, selectionExplicit, newSelectionError(http.StatusBadRequest, fmt.Sprintf("target %q does not support path %q", state.Target().Name, path))
		}
		if _, tried := attempted[strings.ToLower(state.Target().Name)]; tried {
			return nil, nil, selectionExplicit, newSelectionError(http.StatusServiceUnavailable, "requested target already attempted")
		}
		decision, lease := state.admitWithProbeState(now)
		switch decision {
		case admitOpen:
			// 放行（含 half-open 抢到探测位的情形）。
		default: // admitCooling / admitProbeBusy：冷却或探测位占用均不可显式指定
			return nil, nil, selectionExplicit, newSelectionError(http.StatusServiceUnavailable, "requested target temporarily unavailable")
		}
		return state, lease, selectionExplicit, nil
	}

	// 会话粘连优先于 IP 粘连；记录存在但不可用时不尝试 IP 回粘，直接降级。
	// 会话一致性不受 S3 软上限影响，硬容量准入仍由转发路径执行。
	// 会话绑定已由调用方按客户端模型名读取，不解析目标特有的上游名。
	// IP 粘连：优先复用上次成功路由的目标。
	// IP 粘连 key 统一使用 upstream 模型名（而非 alias），让不同 alias 指向同一上游模型的客户端
	// 命中同一粘连记录。Get 兼容两代 key：优先查 alias（兼容旧记录 + 无 mapping 场景），未命中则
	// 尝试用上游名查找（修复后写入的 key）。
	// S3 容量门：命中的粘连节点还须归一化负载低于软上限(affinitySoftCapRatio)才回粘；
	// 超限时不改写记录，走下方 failover 式分流，负载回落 (<) 后自动回粘。
	hadAffinity := false
	if principal != nil && len(attempted) == 0 {
		affinityTarget, affOK := sessionBoundTarget, sessionBoundTarget != ""
		sessionHit := affOK
		if !affOK {
			// IP 粘连也优先用 alias，未命中再尝试上游名。
			affinityTarget, affOK = s.affinity.Get(affinityKey(clientIP, principal.Name, model), now)
			if !affOK {
				upstreamForAffinity := resolveAliasToUpstream(model, allowed, attempted, s.targetSnapshot())
				if upstreamForAffinity != model {
					affinityTarget, affOK = s.affinity.Get(affinityKey(clientIP, principal.Name, upstreamForAffinity), now)
				}
			}
		}
		if affOK {
			hadAffinity = true
			if state, exists := s.targetByName(affinityTarget); exists {
				t := state.Target()
				nameKey := strings.ToLower(t.Name)
				allowedOK := allowed == nil
				if !allowedOK {
					_, allowedOK = allowed[nameKey]
				}
				if t != nil && allowedOK && !t.Paused && modelAllowed(t, model) && t.SupportsPath(path) && (sessionHit || state.belowAffinitySoftCap()) {
					if decision, lease := state.admitWithProbeState(now); decision == admitOpen {
						state.affinityHits.Add(1) // S5 可观测：粘连回粘命中计数
						state.runtimeWindow.record(now, 0, 1, 0)
						return state, lease, selectionAffinityHit, nil
					}
				}
			}
		}
	}

	candidate, lease, unavailableFallback := s.findAvailableTargetWithModel(principal, allowed, attempted, model, path, now)
	if candidate != nil {
		// 如果之前有粘连记录但目标不可用，这是 failover；否则是首次轮询。
		kind := selectionRoundRobin
		if hadAffinity {
			kind = selectionFailover
		}
		return candidate, lease, kind, nil
	}

	if unavailableFallback != nil {
		// 兜底候选分类（只读判定，不参与探测位竞争）：
		//   冷却未到期   ⇒ "all targets muted until X"（既有语义）；
		//   half-open 探针在途 ⇒ "all targets recovering"（S4：恢复中，稍后重试）；
		//   其余         ⇒ 全部健康节点已达硬上限的容量受限错误（S2）。
		// 三者状态与全 muted 同为 503，均不强塞不可用节点。
		if unavailableFallback.IsMuted(now) {
			return nil, nil, selectionRoundRobin, newSelectionError(http.StatusServiceUnavailable, fmt.Sprintf("all targets muted until %s", unavailableFallback.NextRetry().Format(time.RFC3339)))
		}
		if unavailableFallback.probeSlotBusy(now) {
			return nil, nil, selectionRoundRobin, newSelectionError(http.StatusServiceUnavailable, "all targets recovering (half-open probe in flight)")
		}
		return nil, nil, selectionRoundRobin, newSelectionError(http.StatusServiceUnavailable, "all targets at capacity")
	}

	if len(attempted) > 0 {
		return nil, nil, selectionRoundRobin, newSelectionError(http.StatusServiceUnavailable, "no alternative target available")
	}

	if model != "" {
		if s.hasModelCandidateIgnoringPath(allowed, attempted, model) {
			return nil, nil, selectionRoundRobin, newSelectionError(http.StatusBadRequest, fmt.Sprintf("no target supports path %q for model %q", path, model))
		}
		return nil, nil, selectionRoundRobin, newSelectionError(http.StatusBadRequest, "model not supported by any target")
	}

	return nil, nil, selectionRoundRobin, newSelectionError(http.StatusServiceUnavailable, "no target available")
}

// sessionTargetInvalid checks only permanent configuration/permission changes.
// Paused, breaker, capacity, network and path failures keep the original binding.
// Called only after a successful request selected a different target.
func (s *Service) sessionTargetInvalid(name string, allowed map[string]struct{}, model string) bool {
	if allowed != nil {
		if _, ok := allowed[strings.ToLower(name)]; !ok {
			return true
		}
	}
	state, exists := s.targetByName(name)
	return !exists || state == nil || !modelAllowed(state.Target(), model)
}

func (s *Service) targetByName(name string) (*targetState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	target, ok := s.targetsByName[strings.ToLower(name)]
	return target, ok
}

func (s *Service) targetSnapshot() []*targetState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot := make([]*targetState, len(s.targetOrder))
	copy(snapshot, s.targetOrder)
	return snapshot
}

// resolveAliasToUpstream 把客户端传入的 model 名（可能是 alias、可能是 upstream）
// 通过扫描所有目标映射表解析成真正的上游模型名。
//
// 策略：遍历 snapshot 中可访问且未尝试过的 target，只要有一个 target 能解析出 upstream
// 就返回该 upstream。若没有任何 target 解析成功（例如 target 没配 ModelMappings，或
// model 本身就是上游名），原样返回 model。
//
// 用于 Get side 粘连 key 的生成——和 Set side 使用的 upstreamModel 保持一致，
// 让使用不同 alias 但指向同一上游的请求命中同一条粘连记录。
func resolveAliasToUpstream(model string, allowed map[string]struct{}, attempted map[string]struct{}, snapshot []*targetState) string {
	if model == "" {
		return model
	}
	for _, state := range snapshot {
		if state == nil || state.Target() == nil {
			continue
		}
		t := state.Target()
		if t.Paused {
			continue
		}
		name := strings.ToLower(t.Name)
		if attempted != nil {
			if _, seen := attempted[name]; seen {
				continue
			}
		}
		if allowed != nil {
			if _, ok := allowed[name]; !ok {
				continue
			}
		}
		if up, ok := resolveUpstreamModel(t, model); ok && up != "" {
			return up
		}
	}
	return model
}

func (s *Service) findAvailableTarget(allowed map[string]struct{}, attempted map[string]struct{}, now time.Time) (*targetState, *targetState) {
	state, _, fallback := s.findAvailableTargetWithModel(nil, allowed, attempted, "", "", now)
	return state, fallback
}

func (s *Service) hasModelCandidateIgnoringPath(allowed map[string]struct{}, attempted map[string]struct{}, model string) bool {
	for _, state := range s.targetSnapshot() {
		if state == nil || state.Target() == nil {
			continue
		}
		target := state.Target()
		if target.Paused {
			continue
		}
		name := strings.ToLower(target.Name)
		if attempted != nil {
			if _, seen := attempted[name]; seen {
				continue
			}
		}
		if allowed != nil {
			if _, ok := allowed[name]; !ok {
				continue
			}
		}
		if modelAllowed(target, model) {
			return true
		}
	}
	return false
}

func (s *Service) findAvailableTargetWithModel(principal *auth.Principal, allowed map[string]struct{}, attempted map[string]struct{}, model string, path string, now time.Time) (*targetState, *probeLease, *targetState) {
	var mutedCandidate *targetState
	var capacityFallback *targetState
	var recoveringCandidate *targetState
	snapshot := s.targetSnapshot()
	if len(snapshot) == 0 {
		return nil, nil, nil
	}

	// 第一步：过滤出支持该模型的 targets，并统计 key 权重
	// 权重语义沿用「当前存活 key 数」：冷却/耗尽的 key 不再吸引流量，
	// 避免请求被分配到实际无可用 key 的 target 后再触发重试。
	type targetWithKeys struct {
		state       *targetState
		keyCount    int     // 配置 key 数（兜底权重）
		activeCount int     // 当前存活 key 数（首选权重）
		load        float64 // 本次选择的负载快照，后续遍历不得重读并发计数
	}
	var candidates []targetWithKeys

	for _, state := range snapshot {
		if state == nil || state.Target() == nil {
			continue
		}
		target := state.Target()
		if target.Paused {
			continue
		}
		name := strings.ToLower(target.Name)
		if attempted != nil {
			if _, seen := attempted[name]; seen {
				continue
			}
		}
		if allowed != nil {
			if _, ok := allowed[name]; !ok {
				continue
			}
		}
		if !modelAllowed(target, model) {
			continue
		}
		if !target.SupportsPath(path) {
			continue
		}

		// 容量检查必须先于探针租约获取，避免满载节点占位后未发出探测。
		if cap := target.MaxConcurrency; cap > 0 && state.inFlightCount() >= int64(cap) {
			if capacityFallback == nil {
				capacityFallback = state
			}
			continue
		}

		// S4 熔断准入：冷却节点与探测位被占的 half-open 节点均不进入候选权重池。
		//   - cooling（mutedUntil 未到期）：记录为全静默兜底候选（携带最早恢复时间）；
		//   - probeBusy（冷却已到期、独占探针在途）：记录为恢复中兜底候选，
		//     供上层区分「全部冷却」/「全部满载」/「恢复中」三类 503 语义。
		decision, lease := state.admitWithProbeState(now)
		switch decision {
		case admitCooling:
			if mutedCandidate == nil || state.NextRetry().Before(mutedCandidate.NextRetry()) {
				mutedCandidate = state
			}
			continue
		case admitProbeBusy:
			if recoveringCandidate == nil {
				recoveringCandidate = state
			}
			continue
		}

		// S4 探针优先：本次调用刚抢到的探测租约必须当场消化——探针即本次选择结果，
		// 不得再交给加权轮询（相位偏向他节点时租约将孤立，节点恢复窗口全程拒流）。
		if lease != nil {
			return state, lease, nil
		}

		// 统计该 target 的配置 key 数量（兜底）
		keyCount := 1 // 至少有 api_key
		if len(target.APIKeys) > 0 {
			keyCount = len(target.APIKeys)
		}
		// 统计当前存活 key 数量（首选权重）。
		// 无 keyPool（单 key target）时退化为配置数，避免被多 key target 的存活权重挤占到 0。
		activeCount := keyCount
		if state.keyPool != nil {
			activeCount = state.keyPool.activeKeyCount()
		}

		candidates = append(candidates, targetWithKeys{state: state, keyCount: keyCount, activeCount: activeCount, load: state.normalizedLoad()})
	}

	if len(candidates) == 0 {
		// 兜底优先级：静默节点（带最早恢复时间）先于满载节点，再先于恢复中节点——
		// 前两者驱动 "all targets muted until X" / 容量受限错误，恢复中节点是 S4 新增的
		// 半开探针占位语义（探针成功即可复 ready，不宜作为硬阻塞信号）。
		if mutedCandidate != nil {
			return nil, nil, mutedCandidate
		}
		if capacityFallback != nil {
			return nil, nil, capacityFallback
		}
		return nil, nil, recoveringCandidate
	}

	// S2 容量感知调度：非亲和路径选归一化负载最低的健康节点；
	// 负载并列时按存活 key 权重轮询打散（全员并列即退化为旧加权轮询，冷启动零回归）。
	minLoad := -1.0
	for _, c := range candidates {
		if load := c.load; minLoad < 0 || load < minLoad {
			minLoad = load
		}
	}
	winWeight := 0
	for _, c := range candidates {
		if c.load != minLoad {
			continue
		}
		w := c.keyCount
		if c.activeCount > 0 {
			w = c.activeCount
		}
		winWeight += w
	}
	keyIndex := int((s.rrCounter.Add(1) - 1) % uint64(winWeight))
	cumulative := 0
	for _, c := range candidates {
		if c.load != minLoad {
			continue
		}
		w := c.keyCount
		if c.activeCount > 0 {
			w = c.activeCount
		}
		cumulative += w
		if keyIndex < cumulative {
			return c.state, nil, nil
		}
	}

	return nil, nil, capacityFallback
}

type selectionError struct {
	Status  int
	Message string
}

func (e *selectionError) Error() string {
	return e.Message
}

func newSelectionError(status int, message string) error {
	return &selectionError{
		Status:  status,
		Message: message,
	}
}

func buildTargetStates(targets []config.Target, logger ...*slog.Logger) (map[string]*targetState, []*targetState, error) {
	var l *slog.Logger
	if len(logger) > 0 {
		l = logger[0]
	}
	if len(targets) == 0 {
		return make(map[string]*targetState), nil, nil
	}

	parsed := make(map[string]*targetState, len(targets))
	order := make([]*targetState, 0, len(targets))
	for idx, t := range targets {
		t = config.NormalizeTargetForCompatibility(t)
		// Persisted targets are guaranteed explicit by nosql migration/validation.
		if strings.TrimSpace(t.UsageAdapter) == "" {
			t.UsageAdapter = config.UsageAdapterStandard
		}
		// Stored targets may predate enum validation. One dirty record must not
		// stop the whole configuration (that would fail startup): skip targets
		// whose provider cannot be served, and keep the pre-validation behavior
		// for the rest.
		if !config.IsValidEndpointType(t.EndpointType) || !config.IsValidUsageAdapter(t.UsageAdapter) {
			if l != nil {
				l.Error("skipping target with unsupported provider settings", "target", t.Name, "endpoint_type", t.EndpointType, "usage_adapter", t.UsageAdapter)
			}
			continue
		}
		if !config.IsValidAuthMode(t.AuthMode) {
			if l != nil {
				l.Warn("unknown auth mode; using the provider default", "target", t.Name, "auth_mode", t.AuthMode)
			}
			t.AuthMode = ""
		}
		if !config.IsValidImageOperation(t.ImageOperation) && l != nil {
			// The target stays published but matches no image path.
			l.Warn("unknown image operation; target will not receive image requests", "target", t.Name, "image_operation", t.ImageOperation)
		}
		endpoint, err := url.Parse(strings.TrimSpace(t.Endpoint))
		if err != nil {
			return nil, nil, fmt.Errorf("proxy: invalid endpoint for target %q: %w", t.Name, err)
		}
		if endpoint.Scheme == "" || endpoint.Host == "" {
			return nil, nil, fmt.Errorf("proxy: invalid endpoint for target %q: missing scheme or host", t.Name)
		}

		modelIndex := buildModelIndex(t.ModelMappings)

		// 合并 api_key + api_keys 为有序池（去重，api_key 排第一）
		primaryKey := strings.TrimSpace(t.APIKey)
		var mergedKeys []string
		seen := make(map[string]struct{})
		if primaryKey != "" {
			mergedKeys = append(mergedKeys, primaryKey)
			seen[primaryKey] = struct{}{}
		}
		for _, k := range t.APIKeys {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			if _, dup := seen[k]; dup {
				continue
			}
			mergedKeys = append(mergedKeys, k)
			seen[k] = struct{}{}
		}

		info := &Target{
			Name:                        strings.TrimSpace(t.Name),
			EndpointType:                config.NormalizeEndpointType(t.EndpointType),
			UsageAdapter:                config.NormalizeUsageAdapter(t.UsageAdapter),
			Endpoint:                    endpoint,
			APIKey:                      primaryKey,
			APIKeys:                     mergedKeys,
			KeyResetTime:                t.KeyResetTime,
			Paused:                      t.Paused,
			Pool:                        strings.ToLower(strings.TrimSpace(t.Pool)),
			MaxConcurrency:              t.MaxConcurrency,
			AllowBearer:                 t.AllowBearer,
			AuthMode:                    t.AuthMode,
			ImageOperation:              config.NormalizeImageOperation(t.ImageOperation),
			ModelMappings:               t.ModelMappings,
			ModelPrefix:                 t.ModelPrefix,
			OpenAIPrefix:                t.OpenAIPrefix,
			AnthropicPrefix:             t.AnthropicPrefix,
			SupportsResponses:           t.SupportsResponses,
			RequestStreamUsageIfMissing: t.RequestStreamUsageIfMissing,
			PreferWebSocket:             t.PreferWebSocket,
			CustomHeaders:               t.CustomHeaders,
			CustomBody:                  t.CustomBody,
			allowedModelIdx:             modelIndex,
		}
		if info.Name == "" {
			return nil, nil, fmt.Errorf("proxy: target name at index %d must not be empty", idx)
		}
		if len(info.APIKeys) == 0 && !info.AllowBearer {
			return nil, nil, fmt.Errorf("proxy: target %q missing api_key and bearer passthrough not enabled", info.Name)
		}

		nameKey := strings.ToLower(info.Name)
		if _, exists := parsed[nameKey]; exists {
			return nil, nil, fmt.Errorf("proxy: duplicate target name %q", info.Name)
		}

		state := newTargetState(info, l)
		parsed[nameKey] = state
		order = append(order, state)
	}

	return parsed, order, nil
}

func normalizePrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return ""
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	return strings.TrimRight(prefix, "/")
}

// buildModelIndex creates a lowercase lookup map from upstream name or any of
// its comma-separated fallback aliases to the upstream name preserving its
// original case. Fallback keys that collide (case-insensitively) with an
// upstream name already in the index are skipped; the earlier entry wins the
// key slot. Empty upstream and fallback values are ignored. Paused mappings
// are excluded so the model cannot be requested on this target; when every
// configured mapping is paused the result is an EMPTY NON-NIL map (model
// restrictions exist and reject everything) — nil is reserved for "no
// model_mappings configured" (unrestricted).
func buildModelIndex(mappings []config.ModelMapping) map[string]string {
	if len(mappings) == 0 {
		return nil
	}
	idx := make(map[string]string, len(mappings)*2)
	for _, m := range mappings {
		if m.Paused {
			continue
		}
		upKey := strings.ToLower(strings.TrimSpace(m.Upstream))
		if upKey == "" {
			continue
		}
		idx[upKey] = m.Upstream
		for _, alias := range config.SplitFallbackAliases(m.Fallback) {
			aliasLow := strings.ToLower(alias)
			if _, exists := idx[aliasLow]; !exists {
				idx[aliasLow] = m.Upstream
			}
		}
	}
	return idx
}
