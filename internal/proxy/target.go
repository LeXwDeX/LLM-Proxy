// target.go — 目标选择（round-robin + muting）与健康状态管理。
package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
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
	mu         sync.RWMutex
	mutedUntil time.Time
	// muteReason/muteCode 说明当前冷却的来源：空表示普通熔断；muteReasonUpstream403
	// 表示上游 403 拒绝本目标账户后的固定冷却。muteCode 只取上游错误 envelope 的
	// code/type（受控字段），不保存 message 或正文。冷却结束或成功后清空。
	muteReason string
	muteCode   string
	muteStatus int
	// lastUnverifiedRejection is the time of the last unconfirmed 401/403
	// cooldown; a repeat within the escalation window takes the fixed hour.
	// Cleared by a real success.
	lastUnverifiedRejection time.Time
	// consecutiveSentTimeouts counts attempt timeouts after the request may
	// have been sent (never retried). sentTimeoutBreakerThreshold of them in a
	// row, with no success between, cool the target. Cleared by a success.
	consecutiveSentTimeouts int
	lastSentTimeout         time.Time
	lastSuccess             time.Time
	lastFailure             time.Time
	consecutiveFailure      int
	// modelLimits holds per-upstream-model 429 cooldowns (key: the upstream
	// model in its configured spelling). A subscription or model rate limit
	// affects one model of the target, so it must not cool the whole target
	// (target-level cooldowns stay for account, network, 5xx and 408/504).
	// Bounded by maxModelLimits; ClearMute clears it, and an identity fork
	// starts empty. Guarded by mu.
	modelLimits  map[string]modelLimit
	totalSuccess int64
	totalFailure int64

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
	// lastEarlyProbe (unix nanos) spaces early half-open probes taken while
	// every candidate is in a transient cooldown (tryEarlyProbe).
	lastEarlyProbe atomic.Int64
}

type probeLease struct{ until int64 }

func (s *targetState) releaseProbe(lease *probeLease) {
	if lease != nil {
		s.probeLease.CompareAndSwap(lease, nil)
	}
}

// maxModelLimits bounds the per-target model cooldown map; a 429 for a new
// model beyond it falls back to the target-level cooldown.
const maxModelLimits = 32

// modelLimit is the 429 cooldown of one upstream model of a target.
type modelLimit struct {
	until  time.Time
	streak int
	status int
	reason string
	// probe is the half-open lease of this model: once the cooldown ended
	// (the entry stays until the model succeeds) a single request may go
	// through to test the model; the others see "probe in flight".
	probe *modelProbe
}

// modelProbe is the per-model half-open lease. Its owner is the request that
// acquired it (releaseModelProbe); a success clears the whole entry, a new 429
// re-cools the model, anything else releases the lease.
type modelProbe struct {
	model string
	until int64
}

type modelAdmission int

const (
	modelOpen      modelAdmission = iota // no cooldown, or this request holds the probe
	modelCooling                         // cooldown running
	modelProbeBusy                       // cooldown ended, another request is the probe
)

// ModelCooldown is the read-only view of an active model-level cooldown.
type ModelCooldown struct {
	Model  string    `json:"model"`
	Until  time.Time `json:"until"`
	Status int       `json:"status"`
	Reason string    `json:"reason"`
}

// markModelLimited records a 429 for one upstream model. exact (an upstream
// Retry-After) uses base as-is; otherwise base doubles per consecutive 429 of
// that model, capped at breakerMaxBackoffFactor times. A failure arriving
// while the model is already cooling is a late answer of the same incident:
// it is only counted. It reports false when the map is full, so the caller
// falls back to a target-level cooldown (nothing has been counted then).
func (s *targetState) markModelLimited(now time.Time, model string, base time.Duration, exact bool, status int, reason string) bool {
	return s.markModelLimitedCounted(now, model, base, exact, status, reason, true)
}

// setModelCooldown starts the model cooldown of a target whose keys are all
// rate limited for it: the cooldown ends when the earliest key recovers. The
// 429s were already settled per key, so no failure is counted here.
func (s *targetState) setModelCooldown(now time.Time, model string, d time.Duration, status int, reason string) bool {
	return s.markModelLimitedCounted(now, model, d, true, status, reason, false)
}

func (s *targetState) markModelLimitedCounted(now time.Time, model string, base time.Duration, exact bool, status int, reason string, count bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, known := s.modelLimits[model]
	if !known && len(s.modelLimits) >= maxModelLimits {
		for k, e := range s.modelLimits {
			if !e.until.After(now) {
				delete(s.modelLimits, k)
			}
		}
		if len(s.modelLimits) >= maxModelLimits {
			return false
		}
	}
	if count {
		s.lastFailure = now
		s.totalFailure++
		s.consecutiveSentTimeouts = 0 // the upstream answered
	}
	if known && entry.until.After(now) {
		return true
	}
	// A counted 429 is an upstream answer, so the target itself is reachable: a
	// half-open probe that drew it has recovered the target (only this model is
	// limited). A running target-level cooldown is left to its own probe. An
	// uncounted cooldown (every key limited) is not an answer: no recovery.
	if count && !s.mutedUntil.After(now) {
		s.consecutiveFailure = 0
		s.muteReason, s.muteCode, s.muteStatus = "", "", 0
	}
	if base <= 0 {
		base = 60 * time.Second
	}
	entry.streak++
	cooldown := base
	if !exact {
		cap := time.Duration(breakerMaxBackoffFactor) * base
		for i := 1; i < entry.streak && cooldown < cap; i++ {
			cooldown *= 2
		}
	}
	entry.until, entry.status, entry.reason = now.Add(cooldown), status, reason
	entry.probe = nil // a 429 (a probe's included) re-cools; the lease is over
	if s.modelLimits == nil {
		s.modelLimits = make(map[string]modelLimit)
	}
	s.modelLimits[model] = entry
	s.breakerTrips.Add(1)
	s.runtimeWindow.record(now, 0, 0, 1)
	return true
}

// modelLimitedUntil reports the end of the model's active 429 cooldown.
func (s *targetState) modelLimitedUntil(model string, now time.Time) (time.Time, bool) {
	ml, ok := s.modelLimitFor(model, now)
	return ml.until, ok
}

func (s *targetState) modelLimitFor(model string, now time.Time) (modelLimit, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.modelLimits) == 0 {
		return modelLimit{}, false
	}
	ml, ok := s.modelLimits[model]
	if !ok || !ml.until.After(now) {
		return modelLimit{}, false
	}
	return ml, true
}

// requestModelLimit reports the active 429 cooldown of the upstream model
// this target would use for the client's request name.
func (s *targetState) requestModelLimit(alias string, now time.Time) (modelLimit, bool) {
	upstream, ok := resolveUpstreamModel(s.target, alias)
	if !ok || upstream == "" {
		return modelLimit{}, false
	}
	return s.modelLimitFor(upstream, now)
}

// admitModel is the model-level admission gate for a request alias: no entry
// opens; a running cooldown blocks (until is its end); an ended cooldown lets
// exactly one request through as the probe (mp is its lease, to be released by
// releaseModelProbe unless the model succeeds or is limited again) and blocks
// the others while that lease is live.
func (s *targetState) admitModel(alias string, now time.Time) (modelAdmission, time.Time, *modelProbe) {
	upstream, ok := resolveUpstreamModel(s.target, alias)
	if !ok || upstream == "" {
		return modelOpen, time.Time{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ml, known := s.modelLimits[upstream]
	if !known {
		return modelOpen, time.Time{}, nil
	}
	if ml.until.After(now) {
		return modelCooling, ml.until, nil
	}
	if ml.probe != nil && ml.probe.until > now.UnixNano() {
		return modelProbeBusy, time.Time{}, nil
	}
	mp := &modelProbe{model: upstream, until: now.Add(breakerProbeLease).UnixNano()}
	ml.probe = mp
	s.modelLimits[upstream] = ml
	return modelOpen, time.Time{}, mp
}

// modelView is the read-only form of admitModel: it never takes the lease.
func (s *targetState) modelView(alias string, now time.Time) (adm modelAdmission, until time.Time) {
	upstream, ok := resolveUpstreamModel(s.target, alias)
	if !ok || upstream == "" {
		return modelOpen, time.Time{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ml, known := s.modelLimits[upstream]
	switch {
	case !known:
		return modelOpen, time.Time{}
	case ml.until.After(now):
		return modelCooling, ml.until
	case ml.probe != nil && ml.probe.until > now.UnixNano():
		return modelProbeBusy, time.Time{}
	}
	return modelOpen, time.Time{}
}

// releaseModelProbe gives back a lease that did not settle the model (a 4xx,
// 5xx, cancel or an unused selection). Only the owner's lease is released.
func (s *targetState) releaseModelProbe(mp *modelProbe) {
	if mp == nil {
		return
	}
	s.mu.Lock()
	if ml, ok := s.modelLimits[mp.model]; ok && ml.probe == mp {
		ml.probe = nil
		s.modelLimits[mp.model] = ml
	}
	s.mu.Unlock()
}

// clearModelLimit ends one model's cooldown and its streak. Only a real
// success of that model may call it: another model's success proves nothing
// about this model's rate limit.
func (s *targetState) clearModelLimit(model string) {
	if model == "" {
		return
	}
	s.mu.Lock()
	delete(s.modelLimits, model)
	s.mu.Unlock()
}

// modelCooldowns lists the active model-level cooldowns sorted by model.
func (s *targetState) modelCooldowns(now time.Time) []ModelCooldown {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []ModelCooldown
	for model, ml := range s.modelLimits {
		if ml.until.After(now) {
			out = append(out, ModelCooldown{Model: model, Until: ml.until, Status: ml.status, Reason: ml.reason})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// TargetStats is a snapshot of target runtime metrics.
type TargetStats struct {
	MutedUntil         time.Time
	MuteReason         string
	MuteCode           string
	MuteStatus         int
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
	s.markFailureClassified(now, quietPeriod, false, 0, "")
}

// markFailureClassified 记一次失败并进入熔断冷却（S4）。exact=false 时连续失败达到
// 阈值后冷却 = base 按连续次数翻倍（封顶 breakerMaxBackoffFactor 倍）；exact=true
// （上游 Retry-After）时按 base 原值、不翻倍。status/reason 记录冷却来源供管理端
// 展示（空 reason 为未分类的普通熔断）。
//
// 冷却期内迟到的失败（并发请求在同一次故障中先后失败）只累计总数，不推高连续
// 次数、不延长冷却，避免一次故障直接把退避翻到上限；半开探针失败发生在冷却
// 到期之后，仍正常翻倍。也不缩短仍在生效的更长冷却（如 403 的固定冷却）。
func (s *targetState) markFailureClassified(now time.Time, base time.Duration, exact bool, status int, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastFailure = now
	s.totalFailure++
	if status != 0 && status != http.StatusGatewayTimeout {
		s.consecutiveSentTimeouts = 0 // the upstream answered
	}
	if s.mutedUntil.After(now) {
		return
	}
	s.consecutiveFailure++
	if base <= 0 {
		base = 60 * time.Second
	}
	// S4：连续失败达到熔断阈值才进入冷却（muted）；未达阈值只累计计数，
	// 节点保持 ready 可调度。默认阈值为 1，与既有「单次失败即静默」行为完全一致。
	threshold := s.breakerThreshold
	if threshold <= 0 {
		threshold = breakerDefaultConsecutiveFailures
	}
	if s.consecutiveFailure < threshold {
		return
	}
	// S5 可观测：每次跨入/延长冷却计一次熔断事件（含 half-open 探测失败的指数延长）。
	s.breakerTrips.Add(1)
	s.runtimeWindow.record(now, 0, 0, 1)
	cooldown := base
	if !exact {
		cooldown = s.breakerCooldown(base)
	}
	s.mutedUntil = now.Add(cooldown)
	s.muteReason, s.muteCode, s.muteStatus = reason, "", status
}

// sentTimeoutBreakerThreshold consecutive sent timeouts, each within
// sentTimeoutWindow of the previous, cool the target.
const (
	sentTimeoutBreakerThreshold = 3
	sentTimeoutWindow           = 10 * time.Minute
)

// noteSentTimeout counts one sent timeout and reports whether the streak
// reached the threshold (the streak then restarts).
func (s *targetState) noteSentTimeout(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The streak only counts timeouts close together: a slow model's rare
	// timeouts spread over hours on a quiet target never add up.
	if !s.lastSentTimeout.IsZero() && now.Sub(s.lastSentTimeout) > sentTimeoutWindow {
		s.consecutiveSentTimeouts = 0
	}
	s.lastSentTimeout = now
	s.consecutiveSentTimeouts++
	if s.consecutiveSentTimeouts >= sentTimeoutBreakerThreshold {
		s.consecutiveSentTimeouts = 0
		return true
	}
	return false
}

// markReachable settles a response that proves the target answered (a 4xx
// about the request, a reachable rejection) without proving it serves. It
// ends a half-open probe like MarkSuccess, but never clears a running
// account-rejection cooldown: only a real success may do that, so a late 4xx
// from a concurrent request cannot undo another request's 403/402 cooldown.
func (s *targetState) markReachable(now time.Time, holdsProbe bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mutedUntil.After(now) {
		// A cooling target is only ended by its own probe, and an account
		// rejection only by a real success. A late 4xx from a request that
		// started before the cooldown must not reopen a rate-limited or
		// failing upstream.
		if !holdsProbe || upstreamRejectionMutesTarget(s.muteStatus) {
			return
		}
	}
	s.markSuccessLocked(now)
}

// MarkFailureFor 记一次失败并按固定时长冷却，不经阈值与指数退避：用于上游已明确拒绝
// 本目标账户（如 403）的场景。冷却到期后与普通熔断一样由 half-open 探测复活，不是
// 永久屏蔽；已有更长冷却时保留较长者。reason/code 供管理端展示冷却来源。
func (s *targetState) MarkFailureFor(now time.Time, cooldown time.Duration, status int, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyFixedCooldownLocked(now, cooldown, status, code)
}

// applyFixedCooldownLocked sets a fixed cooldown. A late result that does not
// lengthen a running cooldown is only counted (no streak, trip or relabel), so
// N concurrent rejections of one incident are one breaker event.
func (s *targetState) applyFixedCooldownLocked(now time.Time, cooldown time.Duration, status int, code string) bool {
	s.lastFailure = now
	s.totalFailure++
	until := now.Add(cooldown)
	if s.mutedUntil.After(now) && !until.After(s.mutedUntil) {
		return false
	}
	s.consecutiveFailure++
	s.mutedUntil = until
	s.muteReason, s.muteCode, s.muteStatus = muteReasonForStatus(status), code, status
	s.breakerTrips.Add(1)
	s.runtimeWindow.record(now, 0, 0, 1)
	return true
}

// markAccountRejection applies an account-rejection cooldown atomically:
// a confirmed rejection takes fixed; an unconfirmed one takes short unless an
// earlier unconfirmed rejection of this target ended its cooldown within
// window with no success since (then fixed). While an account cooldown is
// already running the result is a late answer of the same incident: it is
// only counted and neither records a new unconfirmed time nor escalates.
// It reports whether a cooldown was applied or extended.
func (s *targetState) markAccountRejection(now time.Time, status int, code string, confirmed bool, short, fixed, window time.Duration) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mutedUntil.After(now) && upstreamRejectionMutesTarget(s.muteStatus) {
		s.lastFailure = now
		s.totalFailure++
		return s.mutedUntil.Sub(now), false
	}
	cooldown := fixed
	if !confirmed {
		recent := !s.lastUnverifiedRejection.IsZero() && now.Sub(s.lastUnverifiedRejection) <= window
		s.lastUnverifiedRejection = now
		if !recent {
			cooldown = short
		}
	}
	return cooldown, s.applyFixedCooldownLocked(now, cooldown, status, code)
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
	s.markSuccessLocked(now)
}

func (s *targetState) markSuccessLocked(now time.Time) {
	s.lastSuccess = now
	s.consecutiveFailure = 0
	s.totalSuccess++
	s.mutedUntil = time.Time{}
	s.muteReason, s.muteCode, s.muteStatus = "", "", 0
	s.lastUnverifiedRejection = time.Time{}
	s.consecutiveSentTimeouts = 0
}

// Stats returns a snapshot of the target state metrics.
func (s *targetState) Stats() TargetStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return TargetStats{
		MutedUntil:         s.mutedUntil,
		MuteReason:         s.muteReason,
		MuteCode:           s.muteCode,
		MuteStatus:         s.muteStatus,
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

// earlyProbeSpacing is the minimum gap between two early probes of one
// target, so a cooling target receives at most one request per interval.
const earlyProbeSpacing = 5 * time.Second

// tryEarlyProbe takes the exclusive probe slot of a target that is still
// cooling, for use only when no candidate is admissible and every blocked
// one is in a transient (non-account, non-rate-limit) cooldown. One request
// per earlyProbeSpacing gets through; success clears the cooldown as any
// probe does, and a failure arrives while still cooling, so it is only
// counted and neither doubles nor extends the cooldown.
func (s *targetState) tryEarlyProbe(now time.Time) *probeLease {
	last := s.lastEarlyProbe.Load()
	if last != 0 && now.UnixNano()-last < int64(earlyProbeSpacing) {
		return nil
	}
	lease := &probeLease{until: now.Add(breakerProbeLease).UnixNano()}
	cur := s.probeLease.Load()
	if cur != nil && cur.until > now.UnixNano() {
		return nil
	}
	if !s.probeLease.CompareAndSwap(cur, lease) {
		return nil
	}
	if !s.lastEarlyProbe.CompareAndSwap(last, now.UnixNano()) {
		s.probeLease.CompareAndSwap(lease, nil)
		return nil
	}
	return lease
}

// earlyProbeEligible reports whether a cooling target may be probed early:
// transient failures only. Account rejections keep their fixed cooldown and
// rate limits (429) keep the upstream's back-off.
func earlyProbeEligible(st TargetStats) bool {
	return !upstreamRejectionMutesTarget(st.MuteStatus) && st.MuteStatus != http.StatusTooManyRequests
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

// Cooldown sources shown by the admin UI. 401/402/403 are fixed cooldowns
// after an upstream rejected this target's account; the others are the
// classified breaker cooldowns of failure_cooldown.go.
const (
	muteReasonNetwork     = "network"
	muteReasonUpstream401 = "upstream_401"
	muteReasonUpstream402 = "upstream_402"
	muteReasonUpstream403 = "upstream_403"
)

// muteReasonForStatus maps an account-level upstream rejection to its
// cooldown reason. Other statuses use the ordinary breaker.
func muteReasonForStatus(status int) string {
	switch status {
	case 401:
		return muteReasonUpstream401
	case 402:
		return muteReasonUpstream402
	case 403:
		return muteReasonUpstream403
	}
	return ""
}

// AllTargetMuteReasonOptions exposes the cooldown sources shown by the admin UI.
// An empty reason is the ordinary breaker cooldown and is not listed.
func AllTargetMuteReasonOptions() []config.EnumOption {
	return []config.EnumOption{
		{Value: muteReasonUpstream401, Label: "401 冷却"},
		{Value: muteReasonUpstream402, Label: "402 冷却"},
		{Value: muteReasonUpstream403, Label: "403 冷却"},
		{Value: "upstream_408", Label: "408 冷却"},
		{Value: "upstream_429", Label: "429 限流冷却"},
		{Value: "upstream_500", Label: "500 冷却"},
		{Value: "upstream_502", Label: "502 冷却"},
		{Value: "upstream_503", Label: "503 冷却"},
		{Value: "upstream_504", Label: "504 冷却"},
		{Value: "upstream_529", Label: "529 过载冷却"},
		{Value: "upstream_5xx", Label: "5xx 冷却"},
		{Value: muteReasonNetwork, Label: "网络冷却"},
	}
}

// ClearMute 由管理员手动解除冷却（如充值后）：清空冷却、连续失败、原因与探针租约，
// 下一请求即可按正常权重调度。累计计数保留。
func (s *targetState) ClearMute() {
	s.mu.Lock()
	s.mutedUntil = time.Time{}
	s.consecutiveFailure = 0
	s.muteReason, s.muteCode, s.muteStatus = "", "", 0
	s.lastUnverifiedRejection = time.Time{}
	s.consecutiveSentTimeouts = 0
	s.modelLimits = nil
	s.mu.Unlock()
	s.probeLease.Store(nil)
	// An operator who clears the target (e.g. after topping up) also clears
	// its keys cooled for an account reason; otherwise the next request still
	// finds every key exhausted. Rate-limit and quota-window keys keep their
	// own recovery.
	if s.keyPool != nil {
		for _, idx := range s.keyPool.getExhaustedKeys() {
			switch s.keyPool.exhaustReasonAt(idx) {
			case keyReasonForbidden, keyReasonUnauthorized, keyReasonForbiddenUnverified, "invalid_token", "billing_error", "account_disabled":
				s.keyPool.markRecovered(idx)
			}
		}
	}
}

// ClearTargetMute clears the cooldown of the named target (case-insensitive).
// It reports false when no such target exists.
func (s *Service) ClearTargetMute(name string) bool {
	for _, state := range s.targetSnapshot() {
		if state == nil || state.Target() == nil {
			continue
		}
		if strings.EqualFold(state.Target().Name, strings.TrimSpace(name)) {
			state.ClearMute()
			return true
		}
	}
	return false
}

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
	var trace selectionTrace
	state, lease, kind, err := s.selectTargetTraced(principal, clientIP, sessionBoundTarget, requestedLower, allowed, attempted, model, path, now, &trace)
	if state != nil {
		// Callers of this form do not carry the lease through a request.
		state.releaseModelProbe(trace.modelProbe)
	}
	return state, lease, kind, err
}

// selectionTrace reports what the first selection round learned about the
// request's sticky bindings, so a later failure can still tell whether the
// request was bound (stickyVerdictPending, in-proxy wait) even though selection failed.
type selectionTrace struct {
	bound      []string // sticky targets in preference order (session first)
	sessionHit bool
	// retrySticky (input) makes this round try the sticky targets that were
	// not attempted yet before any other candidate: the round after a wait for
	// the sticky target.
	retrySticky bool
	// modelProbe (output) is the model half-open lease taken for the selected
	// target; the caller owns it and releases it with the selection.
	modelProbe *modelProbe
}

// collectStickyBindings gathers the sticky targets a request is bound to:
// the session binding alone when present, otherwise the IP binding by client
// alias plus the one written under each member's mapped upstream spelling
// (Set writes the serving member's original spelling, so every member's
// spelling must be tried).
func (s *Service) collectStickyBindings(principal *auth.Principal, clientIP, sessionBoundTarget string, allowed, attempted map[string]struct{}, model string, now time.Time) (bound []string, sessionHit bool) {
	if principal == nil {
		return nil, false
	}
	sessionHit = sessionBoundTarget != ""
	seen := make(map[string]struct{})
	addBound := func(target string) {
		if target == "" {
			return
		}
		if _, ok := seen[target]; ok {
			return
		}
		seen[target] = struct{}{}
		bound = append(bound, target)
	}
	if sessionHit {
		addBound(sessionBoundTarget)
		return bound, sessionHit
	}
	if target, ok := s.affinity.Get(affinityKey(clientIP, principal.Name, model), now); ok {
		addBound(target)
	}
	for _, upstream := range resolveAliasUpstreams(model, allowed, attempted, s.targetSnapshot()) {
		if upstream == model {
			continue // alias key already checked above
		}
		if target, ok := s.affinity.Get(affinityKey(clientIP, principal.Name, upstream), now); ok {
			addBound(target)
		}
	}
	return bound, sessionHit
}

// stickyCandidate reports whether a bound target is still a legal candidate
// for this request: it exists, is not paused, and passes the ACL, model and
// path filters. Health and load are not considered.
func (s *Service) stickyCandidate(name string, allowed map[string]struct{}, model, path string) (*targetState, bool) {
	state, exists := s.targetByName(name)
	if !exists || state == nil {
		return nil, false
	}
	t := state.Target()
	if t == nil {
		return nil, false
	}
	if allowed != nil {
		if _, ok := allowed[strings.ToLower(t.Name)]; !ok {
			return nil, false
		}
	}
	if t.Paused || !modelAllowed(t, model) || !t.SupportsPath(path) {
		return nil, false
	}
	return state, true
}

func (s *Service) selectTargetTraced(
	principal *auth.Principal,
	clientIP string,
	sessionBoundTarget string,
	requestedLower string,
	allowed map[string]struct{},
	attempted map[string]struct{},
	model string,
	path string,
	now time.Time,
	trace *selectionTrace,
) (*targetState, *probeLease, selectionKind, error) {
	if trace == nil {
		trace = &selectionTrace{}
	}
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
		// A model-level 429 cooldown blocks that model on the target (and after
		// it a single probe request goes through); the check precedes target
		// admission so no target lease is taken for a blocked model.
		gate, gateUntil, mp := state.admitModel(model, now)
		switch gate {
		case modelCooling:
			return nil, nil, selectionExplicit, &selectionError{
				Status:  http.StatusServiceUnavailable,
				Message: "requested target temporarily unavailable for this model (rate limited)",
				RetryAt: gateUntil,
			}
		case modelProbeBusy:
			return nil, nil, selectionExplicit, &selectionError{
				Status:  http.StatusServiceUnavailable,
				Message: "requested target temporarily unavailable for this model (recovering)",
				RetryAt: now.Add(retryAfterMin),
			}
		}
		decision, lease := state.admitWithProbeState(now)
		switch decision {
		case admitOpen:
			// 放行（含 half-open 抢到探测位的情形）。
		default: // admitCooling / admitProbeBusy：冷却或探测位占用均不可显式指定
			state.releaseModelProbe(mp)
			return nil, nil, selectionExplicit, newSelectionError(http.StatusServiceUnavailable, "requested target temporarily unavailable")
		}
		trace.modelProbe = mp
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
	ranker := s.newCostRanker(model, now)
	if principal != nil && (len(attempted) == 0 || trace.retrySticky) {
		sessionHit := sessionBoundTarget != ""
		boundTargets, _ := s.collectStickyBindings(principal, clientIP, sessionBoundTarget, allowed, attempted, model, now)
		if len(attempted) == 0 {
			trace.bound, trace.sessionHit = boundTargets, sessionHit
		}
		hadAffinity = len(boundTargets) > 0
		// 逐条验证；先命中的旧拼写若指向本轮不可用的目标，继续尝试后续
		// 绑定，不能让它遮挡仍然可用的粘连记录。
		for _, affinityTarget := range boundTargets {
			state, ok := s.stickyCandidate(affinityTarget, allowed, model, path)
			if !ok {
				continue
			}
			if _, tried := attempted[strings.ToLower(state.Target().Name)]; tried {
				continue // a target is sent a request at most once
			}
			if !sessionHit && !state.belowAffinitySoftCap() {
				continue
			}
			// The target's cooldown for this request's upstream model (a 429
			// of that model) makes it unavailable for this request only; the
			// check precedes admission so no probe lease is taken for it.
			gate, _, mp := state.admitModel(model, now)
			if gate != modelOpen {
				continue
			}
			if decision, lease := state.admitWithProbeState(now); decision == admitOpen {
				state.affinityHits.Add(1) // S5 可观测：粘连回粘命中计数
				state.runtimeWindow.record(now, 0, 1, 0)
				trace.modelProbe = mp
				return state, lease, selectionAffinityHit, nil
			}
			state.releaseModelProbe(mp)
		}
	}

	candidate, lease, unavailableFallback, why := s.findAvailableRankedDetail(ranker, allowed, attempted, model, path, now)
	if candidate != nil {
		// 如果之前有粘连记录但目标不可用，这是 failover；否则是首次轮询。
		kind := selectionRoundRobin
		if hadAffinity {
			kind = selectionFailover
		}
		trace.modelProbe = why.modelProbe
		return candidate, lease, kind, nil
	}

	if unavailableFallback != nil {
		// 兜底候选分类（只读判定，不参与探测位竞争）：
		//   冷却未到期   ⇒ "all targets muted until X"（既有语义；含请求模型的 429 冷却）；
		//   half-open 探针在途 ⇒ "all targets recovering"（S4：恢复中，稍后重试）；
		//   其余         ⇒ 全部健康节点已达硬上限的容量受限错误（S2）。
		// 三者状态与全 muted 同为 503，均不强塞不可用节点。
		// Transient/EarliestRecovery 供有界等待判定：阻塞全部为短暂原因（不含账户拒绝）
		// 且最早恢复时间已知；RetryAt 供 503 的 Retry-After。
		transient := !why.hasAccount && !why.earliestRecovery.IsZero()
		if why.cooling || unavailableFallback.IsMuted(now) {
			// Nothing can serve and nothing is merely full or recovering: let one
			// early probe through to the earliest transient cooldown instead of
			// failing everyone until it ends.
			if why.probe != nil {
				if probe := why.probe.tryEarlyProbe(now); probe != nil {
					kind := selectionRoundRobin
					if hadAffinity {
						kind = selectionFailover
					}
					return why.probe, probe, kind, nil
				}
			}
			until := why.mutedUntil
			if until.IsZero() {
				until = unavailableFallback.NextRetry()
			}
			selErr := &selectionError{
				Status:           http.StatusServiceUnavailable,
				Message:          fmt.Sprintf("all targets muted until %s", until.Format(time.RFC3339)),
				RetryAt:          until,
				Transient:        transient,
				EarliestRecovery: why.earliestRecovery,
				ProbeInFlight:    why.busyState != nil,
			}
			if stats := unavailableFallback.Stats(); why.accountOnly && upstreamRejectionMutesTarget(stats.MuteStatus) {
				selErr.MuteStatus, selErr.MuteCode, selErr.MutedUntil = stats.MuteStatus, stats.MuteCode, stats.MutedUntil
				if t := unavailableFallback.Target(); t != nil {
					selErr.MuteTarget = t.Name
				}
			}
			return nil, nil, selectionRoundRobin, selErr
		}
		if unavailableFallback.probeSlotBusy(now) || unavailableFallback == why.busyState {
			return nil, nil, selectionRoundRobin, &selectionError{
				Status: http.StatusServiceUnavailable, Message: "all targets recovering (half-open probe in flight)",
				RetryAt: now.Add(retryAfterMin), ProbeInFlight: true,
			}
		}
		return nil, nil, selectionRoundRobin, &selectionError{
			Status: http.StatusServiceUnavailable, Message: "all targets at capacity", RetryAt: now.Add(retryAfterMin),
		}
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

// resolveAliasUpstreams 把客户端传入的 model 名（可能是 alias、可能是 upstream）
// 通过扫描所有目标映射表解析成各成员真正的上游模型名，按 target 顺序去重返回。
//
// 用于 Get side 粘连 key 的生成：Set side 使用实际服务成员的 upstreamModel 原始
// 拼写，同一别名在不同成员可能对应不同上游拼写，必须逐个尝试才能命中。没有
// target 能解析时返回空；调用方已单独尝试过 alias key。去重按原始拼写，避免把
// 大小写不同的两个实际 key 合并。
func resolveAliasUpstreams(model string, allowed map[string]struct{}, attempted map[string]struct{}, snapshot []*targetState) []string {
	if model == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var upstreams []string
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
			if _, exists := seen[up]; exists {
				continue
			}
			seen[up] = struct{}{}
			upstreams = append(upstreams, up)
		}
	}
	return upstreams
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
	return s.findAvailableTargetWithModelRanked(s.newCostRanker(model, now), allowed, attempted, model, path, now)
}

// findAvailableTargetWithModelRanked is the availability-first selector; a
// non-nil ranker only narrows the admitted candidates (cost_routing.go).
// blockage describes why no candidate was admissible.
//
//	accountOnly:   every blocked candidate cools after an account rejection
//	               (no ordinary cooldown, full or recovering sibling); only then
//	               may the caller surface the rejection.
//	probe:         the earliest-recovering candidate cooling for an early-probe
//	               eligible transient reason, set only when nothing is full or
//	               recovering; the caller may take one early probe of it even
//	               if siblings cool for account rejections or rate limits.
type blockage struct {
	accountOnly bool
	probe       *targetState
	// cooling is set when at least one candidate is blocked by a cooldown
	// (target-level or the request model's 429 cooldown); mutedUntil is the
	// earliest end among all of them (account rejections included).
	cooling    bool
	mutedUntil time.Time
	// earliestRecovery is the earliest end among the transient cooldowns
	// (model-level 429 and target-level cooldowns that are not account
	// rejections). hasAccount reports an account-rejection cooldown among the
	// blockers. Full and recovering (probe in flight) candidates have no known
	// recovery time and are not part of it.
	earliestRecovery time.Time
	hasAccount       bool
	// busyState is a candidate blocked only by an in-flight probe (target or
	// model level); modelProbe is the model lease taken for the returned
	// candidate (the caller owns it).
	busyState  *targetState
	modelProbe *modelProbe
}

// findAvailableRankedDetail is findAvailableTargetWithModelRanked plus the
// blockage summary used when nothing is admissible.
func (s *Service) findAvailableRankedDetail(ranker *costRanker, allowed map[string]struct{}, attempted map[string]struct{}, model string, path string, now time.Time) (candidate *targetState, lease *probeLease, fallback *targetState, why blockage) {
	var mutedCandidate *targetState
	var capacityFallback *targetState
	var recoveringCandidate *targetState
	// onlyAccountMuted stays true while every cooling candidate is cooling
	// after an account rejection (401/402/403). Only then may the caller
	// answer with the upstream rejection instead of the ordinary 503.
	onlyAccountMuted := true
	var probeCandidate *targetState
	// Cooling candidates, target- or model-level, tracked by their own end.
	var mutedUntil, transientUntil time.Time
	hasAccountCooling := false
	noteCooling := func(state *targetState, until time.Time, account bool) {
		if mutedCandidate == nil || until.Before(mutedUntil) {
			mutedCandidate, mutedUntil = state, until
		}
		if account {
			hasAccountCooling = true
		} else if transientUntil.IsZero() || until.Before(transientUntil) {
			transientUntil = until
		}
	}
	snapshot := s.targetSnapshot()
	if len(snapshot) == 0 {
		return nil, nil, nil, blockage{}
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

		// 请求上游模型的 429 冷却：该模型对本请求不可用，但同目标其他模型照常可用。
		// 先于容量与探针租约检查，避免为不可用的模型占用探针位。它不是账户拒绝，
		// 也不作为提前探测候选（429 保留上游退避）。冷却结束后模型也有半开门：
		// 只放一个探针请求，其余视为探针在途。
		gate, gateUntil, modelLease := state.admitModel(model, now)
		switch gate {
		case modelCooling:
			onlyAccountMuted = false
			noteCooling(state, gateUntil, false)
			continue
		case modelProbeBusy:
			onlyAccountMuted = false
			if recoveringCandidate == nil {
				recoveringCandidate = state
			}
			continue
		}

		// 容量检查必须先于探针租约获取，避免满载节点占位后未发出探测。
		if cap := target.MaxConcurrency; cap > 0 && state.inFlightCount() >= int64(cap) {
			state.releaseModelProbe(modelLease)
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
			stats := state.Stats()
			account := upstreamRejectionMutesTarget(stats.MuteStatus)
			if !account {
				onlyAccountMuted = false
			}
			if earlyProbeEligible(stats) && (probeCandidate == nil || state.NextRetry().Before(probeCandidate.NextRetry())) {
				probeCandidate = state
			}
			state.releaseModelProbe(modelLease)
			noteCooling(state, stats.MutedUntil, account)
			continue
		case admitProbeBusy:
			state.releaseModelProbe(modelLease)
			if recoveringCandidate == nil {
				recoveringCandidate = state
			}
			continue
		}

		// S4 探针优先：本次调用刚抢到的探测租约必须当场消化——探针即本次选择结果，
		// 不得再交给加权轮询（相位偏向他节点时租约将孤立，节点恢复窗口全程拒流）。
		// 模型级半开租约同理：持有者即本次选择结果。
		if lease != nil || modelLease != nil {
			return state, lease, nil, blockage{modelProbe: modelLease}
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
		busy := capacityFallback != nil || recoveringCandidate != nil
		why = blockage{
			cooling: mutedCandidate != nil, mutedUntil: mutedUntil, earliestRecovery: transientUntil,
			hasAccount: hasAccountCooling, busyState: recoveringCandidate,
		}
		if mutedCandidate != nil {
			// A mix of causes (another candidate is only full, recovering or
			// in an ordinary cooldown) is a temporary 503, not an account
			// rejection: report a non-account fallback in that case.
			why.accountOnly = onlyAccountMuted && !busy
			if !busy {
				why.probe = probeCandidate
			}
			return nil, nil, mutedCandidate, why
		}
		if capacityFallback != nil {
			return nil, nil, capacityFallback, why
		}
		return nil, nil, recoveringCandidate, why
	}

	// 成本分层（仅 cost 池）：在已准入候选中收窄到最便宜层，不减少可用性。
	admitted := candidates
	candidates = cheapestTier(ranker, candidates, func(c targetWithKeys) *targetState { return c.state })
	// Debug 诊断：以客户端别名为主列出已准入候选及各自上游名与估算价。
	// 仅在启用 Debug 时计算结果，避免正常请求重复取价；全部候选都在
	// cost 池外时不输出，避免只在别处配置了 cost 池的普通请求被噪声刷屏。
	managed := false
	if ranker != nil {
		for _, c := range admitted {
			if ranker.managed(c.state.Target()) {
				managed = true
				break
			}
		}
	}
	if managed && s.logger != nil && s.logger.Enabled(context.Background(), slog.LevelDebug) {
		targets := make([]*Target, 0, len(admitted))
		for _, c := range admitted {
			targets = append(targets, c.state.Target())
		}
		s.logger.Debug("cost routing candidates",
			"alias", ranker.alias,
			"admitted", len(admitted),
			"tier", len(candidates),
			"members", ranker.describe(targets),
		)
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
			return c.state, nil, nil, blockage{}
		}
	}

	return nil, nil, capacityFallback, blockage{}
}

func (s *Service) findAvailableTargetWithModelRanked(ranker *costRanker, allowed map[string]struct{}, attempted map[string]struct{}, model string, path string, now time.Time) (*targetState, *probeLease, *targetState) {
	candidate, lease, fallback, why := s.findAvailableRankedDetail(ranker, allowed, attempted, model, path, now)
	if candidate != nil {
		candidate.releaseModelProbe(why.modelProbe) // this form does not carry the lease through a request
	}
	return candidate, lease, fallback
}

type selectionError struct {
	Status  int
	Message string
	// RetryAt is the earliest time a blocked candidate may serve again
	// (cooldown end; zero when unknown). It feeds Retry-After on the 503.
	RetryAt time.Time
	// Transient is set when every blocker is short-lived (model-level 429
	// cooldown, a target-level cooldown that is not an account rejection, a full
	// target or a probe in flight) and EarliestRecovery is known. Only then may
	// the request wait in the proxy for recovery (inProxyWait).
	Transient        bool
	EarliestRecovery time.Time
	// ProbeInFlight is set when a half-open probe (target or model level) is
	// the reason a candidate cannot be admitted right now.
	ProbeInFlight bool
	// Set when every candidate is cooling after an account-level upstream
	// rejection: the earliest-recovering target and why it is cooling.
	MuteStatus int
	MuteCode   string
	MutedUntil time.Time
	MuteTarget string
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
			return nil, nil, fmt.Errorf("proxy: invalid endpoint for target %q: invalid URL syntax", t.Name)
		}
		if endpoint.Scheme == "" || endpoint.Host == "" {
			return nil, nil, fmt.Errorf("proxy: invalid endpoint for target %q: missing scheme or host", t.Name)
		}

		modelIndex := buildModelIndex(t.ModelMappings)

		// 合并 api_key + api_keys 为有序池（去重，api_key 排第一）
		primaryKey := strings.TrimSpace(t.APIKey)
		mergedKeys := mergeTargetKeys(t)
		if primaryKey == "" && len(mergedKeys) == 1 {
			primaryKey = mergedKeys[0]
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

func mergeTargetKeys(t config.Target) []string {
	var keys []string
	seen := make(map[string]struct{})
	for _, raw := range append([]string{t.APIKey}, t.APIKeys...) {
		key := strings.TrimSpace(raw)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; !exists {
			keys = append(keys, key)
			seen[key] = struct{}{}
		}
	}
	return keys
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
