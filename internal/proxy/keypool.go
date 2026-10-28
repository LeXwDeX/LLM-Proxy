// keypool.go — 多 API Key 池状态机：客户端亲和、耗尽检测、冷却恢复。
package proxy

import (
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// tokenSample 记录一次请求的 token 用量和时间戳，用于滑动窗口统计。
type tokenSample struct {
	at     time.Time
	tokens int64 // input + output
}

// keyRuntime is shared only while this exact key continuously survives within
// one target's pool lifecycle. Every reader/writer uses the same keyPoolDomain.
type keyRuntime struct {
	exhausted            bool
	exhaustedAt          time.Time
	exhaustReason        string        // 耗尽原因
	cooldownEnd          time.Time     // 冷却结束时间
	blocked              bool          // 手动屏蔽（永久，直到手动解除）
	tokenSamples         []tokenSample // 滑动窗口内的 token 用量样本
	lastError            string        // 最近一次上游错误的受控提示，不含响应体
	lastErrorAt          time.Time     // 最近一次错误时间
	consecutiveRateLimit int           // 连续 429 限流次数（指数退避冷却依据，成功时清零）
	consecutiveFails     int           // 连续失败次数（HTTP 429 或 GLM 1302，WP-3：RETRY≥2 降级依据，成功时清零）
}

// keyEntry keeps a generation's immutable credential and index assignment.
type keyEntry struct {
	key string
	*keyRuntime
}

// keyPoolDomain serializes all generations that share any keyRuntime. The
// pointer is assigned before publication; an already-used mutex is never copied.
type keyPoolDomain struct {
	mu sync.Mutex
}

// keyPool 管理一个 target 的多个 API key。
// 支持轮询分配 + 客户端亲和（首次分配后记忆绑定）、耗尽检测、冷却恢复。
// 支持 token 感知调度：新客户端优先分配到滑动窗口内累计 token 最少的 key。
// 支持唤醒模型：所有 key 不可用时，单例化探测被屏蔽的 key 是否恢复。
// 适用于所有 provider 类型。
type keyPool struct {
	*keyPoolDomain
	entries        []keyEntry
	resetTime      time.Time // 额度重置时间点（零值表示未配置）
	resetTimeSpec  string    // 原始配置；monthly/日号格式需要按当前时间重新计算下一次重置
	logger         *slog.Logger
	targetName     string
	rrCounter      uint32         // 轮询计数器，新客户端按此值分配 key
	clientAffinity map[string]int // 客户端 → 已分配的 key index（记忆绑定）
	tokenWindow    time.Duration  // token 统计滑动窗口时长（默认 3 分钟）

	// #3 并发削峰：每个 key 当前在途请求数（least-connections 调度信号，实时性优于滞后的 token 窗口）
	inFlight []int64

	// #2 绑定再均衡 / #5 绑定淘汰
	clientAffinityAt   map[string]time.Time // 客户端 → 绑定时间戳
	rebalanceInterval  time.Duration        // 已绑定客户端的再平衡间隔（默认 30s）
	affinityTTL        time.Duration        // 绑定空闲淘汰 TTL（默认 10min）
	maxAffinityEntries int                  // clientAffinity 容量上限（默认 10000）

	// 唤醒模型：所有 key 不可用时，单例化探测被屏蔽的 key 是否恢复
	wakeUpCooldown   time.Time   // 唤醒冷却时间（1分钟内不重复触发）
	wakeUpInProgress atomic.Bool // 单例标记（防止并发惊群）

	// onKeyChange 回调：当 activeKeyCount 变化时通知外部系统（如 admission manager）。
	// 在 mu 之外以 goroutine 异步调用，避免 keypool 持锁时阻塞 admission 操作。
	onKeyChange func(active, total int)
	// onKeysUnavailable receives the credentials that are exhausted or blocked
	// after a change, so connection reuse can drop sockets opened with them.
	// Called like onKeyChange: outside mu, from the change goroutine.
	onKeysUnavailable func(keys []string)
	// notifyMu serializes snapshot+callbacks in asyncNotifyKeyChange so they
	// apply in read order and the last one carries the latest state.
	// Lock order: notifyMu before mu; never taken while holding mu.
	notifyMu sync.Mutex
}

func newKeyPool(targetName string, keys []string, resetTimeStr string, logger *slog.Logger) *keyPool {
	if len(keys) == 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	resetTimeStr = strings.TrimSpace(resetTimeStr)
	resetTime := parseResetTimeSpec(targetName, resetTimeStr, logger, time.Now())

	entries := make([]keyEntry, len(keys))
	for i, k := range keys {
		entries[i] = keyEntry{key: k, keyRuntime: &keyRuntime{}}
	}
	return &keyPool{
		keyPoolDomain:  &keyPoolDomain{},
		entries:        entries,
		resetTime:      resetTime,
		resetTimeSpec:  resetTimeStr,
		logger:         logger,
		targetName:     targetName,
		clientAffinity: make(map[string]int),
		tokenWindow:    3 * time.Minute, // 默认 3 分钟滑动窗口

		inFlight:           make([]int64, len(keys)),
		clientAffinityAt:   make(map[string]time.Time),
		rebalanceInterval:  30 * time.Second,
		affinityTTL:        10 * time.Minute,
		maxAffinityEntries: 10000,
	}
}

func parseResetTimeSpec(targetName, resetTimeStr string, logger *slog.Logger, now time.Time) time.Time {
	resetTimeStr = strings.TrimSpace(resetTimeStr)
	if resetTimeStr == "" {
		return time.Time{}
	}
	cst := time.FixedZone("CST", 8*3600)
	now = now.In(cst)

	dayStr := resetTimeStr
	if strings.HasPrefix(strings.ToLower(resetTimeStr), "monthly:") {
		dayStr = strings.TrimSpace(resetTimeStr[len("monthly:"):])
	}
	if day, ok := parseMonthlyResetDay(dayStr); ok {
		return nextMonthlyReset(now, day, cst)
	}
	if strings.HasPrefix(strings.ToLower(resetTimeStr), "monthly:") {
		logger.Warn("[keypool] invalid monthly reset_time format, ignoring",
			"target", targetName,
			"reset_time", resetTimeStr,
		)
		return time.Time{}
	}
	if t, err := time.ParseInLocation("2006-01-02", resetTimeStr, cst); err == nil {
		return t
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04", resetTimeStr, cst); err == nil {
		return t
	}
	logger.Warn("[keypool] invalid reset_time format, ignoring",
		"target", targetName,
		"reset_time", resetTimeStr,
	)
	return time.Time{}
}

func parseMonthlyResetDay(dayStr string) (int, bool) {
	dayStr = strings.TrimSpace(dayStr)
	if dayStr == "" {
		return 0, false
	}
	day, err := strconv.Atoi(dayStr)
	return day, err == nil && day >= 1 && day <= 31
}

func nextMonthlyReset(now time.Time, day int, loc *time.Location) time.Time {
	next := monthlyResetInMonth(now.Year(), now.Month(), day, loc)
	if !next.After(now) {
		year, month, _ := now.Date()
		nextMonth := time.Date(year, month, 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
		next = monthlyResetInMonth(nextMonth.Year(), nextMonth.Month(), day, loc)
	}
	return next
}

func monthlyResetInMonth(year int, month time.Month, day int, loc *time.Location) time.Time {
	lastDay := time.Date(year, month+1, 0, 0, 0, 0, 0, loc).Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(year, month, day, 0, 0, 0, 0, loc)
}

// selectKey 按顺序返回第一个可用的 key。
// 如果全部 exhausted，检查冷却期是否已过，过了则恢复。
// 绝境探测：如果所有 key 都在冷却中，选冷却剩余最短的 key 做探测（返回该 key，但不自动恢复）。
// 返回 (key, index)。如果无可用 key 返回 ("", -1)。
func (p *keyPool) selectKey() (string, int) {
	if p == nil || len(p.entries) == 0 {
		return "", -1
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	n := len(p.entries)

	// 第一轮：从轮询位置开始找第一个 active 的（#4 均衡：避免重试/探测总是回到 key-0）
	start := int(p.rrCounter) % n
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if !p.entries[idx].exhausted && !p.entries[idx].blocked {
			p.rrCounter++
			return p.entries[idx].key, idx
		}
	}

	// 第二轮：全部 exhausted/blocked，找冷却期已过的（跳过 blocked）
	if idx, ok := p.recoverFromCooldownLocked(now, ""); ok {
		return p.entries[idx].key, idx
	}

	// 第三轮：绝境探测 — 所有 key 都在冷却中，选冷却剩余最短的做探测
	// 探测成功由调用方负责恢复 key（markSuccess），失败则延长冷却
	bestIdx := -1
	shortestRemaining := time.Duration(0)
	for i := range p.entries {
		if p.entries[i].blocked {
			continue
		}
		// 跳过同账号共享配额类限流（探测也会被限或假阳性返回 200）：
		// rate_limited + GLM 各类限流（5h/周月额度/QPS/服务过载）。注意 quota_exceeded 等额度耗尽
		// 保留参与绝境探测（选冷却最短的探测，额度恢复时可复活）。
		switch p.entries[i].exhaustReason {
		case "rate_limited", glmExhaustReason, glmRateLimitQuota, glmRateLimitQPS, glmRateLimitOverload, glmRateLimitUnknown:
			continue
		}
		remaining := p.entries[i].cooldownEnd.Sub(now)
		if bestIdx == -1 || remaining < shortestRemaining {
			bestIdx = i
			shortestRemaining = remaining
		}
	}
	if bestIdx >= 0 {
		maskKey := maskAPIKey(p.entries[bestIdx].key)
		p.logger.Warn("[keypool] desperation probe: all keys exhausted, probing key with shortest cooldown",
			"target", p.targetName,
			"key_index", bestIdx,
			"key", maskKey,
			"cooldown_remaining_ms", shortestRemaining.Milliseconds(),
		)
		return p.entries[bestIdx].key, bestIdx
	}

	// 全部在冷却中（包括 rate_limited）
	return "", -1
}

// selectKeyForClient 按轮询分配 + 客户端亲和选择 key。
//
// 分配策略：
//  1. 已有亲和绑定 → 复用绑定的 key（如果 active）
//  2. 新客户端 → 优先选滑动窗口内累计 token 最少的 active key（token 感知调度）；
//     如果无 token 数据（刚启动），退化为轮询分配
//  3. 绑定的 key exhausted/blocked → 溢出到下一个 active key，更新亲和绑定
//  4. 全部 exhausted → 冷却恢复（与 selectKey 一致）
//
// clientName 为空时退化为 selectKey（顺序消费）。
func (p *keyPool) selectKeyForClient(clientName string) (string, int) {
	if p == nil || len(p.entries) == 0 {
		return "", -1
	}
	if clientName == "" {
		return p.selectKey()
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	n := len(p.entries)

	// 1. 检查已有亲和绑定
	if preferred, ok := p.clientAffinity[clientName]; ok && preferred >= 0 && preferred < n {
		if !p.entries[preferred].exhausted && !p.entries[preferred].blocked {
			// #2 再平衡：绑定存活，但若超过 rebalanceInterval 未重新评估，
			// 且存在负载更低的 active key，则迁移绑定（least-connections + least-token）。
			// 仅在确有更优 key 时迁移，避免无意义抖动。
			boundAt := p.clientAffinityAt[clientName]
			if now.Sub(boundAt) >= p.rebalanceInterval {
				best := p.bestActiveKeyLocked(now)
				if best >= 0 && best != preferred && p.scoreLessLocked(best, preferred, now) {
					p.logger.Info("[keypool] affinity rebalanced",
						"target", p.targetName,
						"client", clientName,
						"prev_key_index", preferred,
						"new_key_index", best,
					)
					preferred = best
				}
				p.bindLocked(clientName, preferred, now)
			}
			return p.entries[preferred].key, preferred
		}
		// 绑定的 key 不可用，走溢出
	}

	// 2. 新客户端：优先 least-connections + least-token 调度，无任何负载信号时退化为轮询
	if _, ok := p.clientAffinity[clientName]; !ok {
		bestIdx := -1
		hasSignal := false // 有在途请求 或 有 token 数据
		for i := 0; i < n; i++ {
			if p.entries[i].exhausted || p.entries[i].blocked {
				continue
			}
			if p.inFlight[i] > 0 || p.tokenSumInWindow(i, now) > 0 {
				hasSignal = true
			}
			if bestIdx == -1 || p.scoreLessLocked(i, bestIdx, now) {
				bestIdx = i
			}
		}
		if bestIdx >= 0 {
			if hasSignal {
				// 负载感知调度：选在途最少、其次累计 token 最少的
				p.bindLocked(clientName, bestIdx, now)
				return p.entries[bestIdx].key, bestIdx
			}
			// 无负载信号（刚启动 / 全空闲），退化为轮询
			for i := 0; i < n; i++ {
				idx := int(p.rrCounter) % n
				p.rrCounter++
				if !p.entries[idx].exhausted && !p.entries[idx].blocked {
					p.bindLocked(clientName, idx, now)
					return p.entries[idx].key, idx
				}
			}
		}
		// 所有 key 都 exhausted/blocked，跳过轮询分配，走冷却恢复
	}

	// 3. 溢出：从当前位置开始找下一个 active key
	start := int(p.rrCounter) % n
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if !p.entries[idx].exhausted && !p.entries[idx].blocked {
			p.bindLocked(clientName, idx, now)
			maskKey := maskAPIKey(p.entries[idx].key)
			p.logger.Info("[keypool] affinity overflow",
				"target", p.targetName,
				"client", clientName,
				"actual_key_index", idx,
				"actual_key", maskKey,
			)
			return p.entries[idx].key, idx
		}
	}

	// 4. 全部 exhausted/blocked，找冷却期已过的（跳过 blocked）
	if idx, ok := p.recoverFromCooldownLocked(now, clientName); ok {
		return p.entries[idx].key, idx
	}

	// 5. 全部在冷却中
	return "", -1
}

// hashClientKey 对 clientName + targetName 做 FNV-1a hash，保证同一客户端在同一 target 下映射到固定 key。
// 内联实现以避免 fnv.New32a() 的堆分配（hash.Hash 接口逃逸）。
func hashClientKey(clientName, targetName string) uint32 {
	const (
		offset32 = uint32(2166136261)
		prime32  = uint32(16777619)
	)
	h := offset32
	for i := 0; i < len(clientName); i++ {
		h ^= uint32(clientName[i])
		h *= prime32
	}
	h *= prime32 // separator byte 0x00
	for i := 0; i < len(targetName); i++ {
		h ^= uint32(targetName[i])
		h *= prime32
	}
	return h
}

// recoverFromCooldownLocked 查找冷却期已过且未被手动屏蔽的 key，将其恢复为 active。
// 如果提供了 clientName，则同时建立亲和绑定。
// 返回恢复的 key index 和 true；无可用 key 返回 (-1, false)。
// 调用方必须持有 p.mu。
func (p *keyPool) recoverFromCooldownLocked(now time.Time, clientName string) (int, bool) {
	for i := range p.entries {
		if p.entries[i].blocked {
			continue
		}
		if p.entries[i].exhausted && !now.Before(p.entries[i].cooldownEnd) {
			p.entries[i].exhausted = false
			p.entries[i].exhaustedAt = time.Time{}
			p.entries[i].exhaustReason = ""
			p.entries[i].cooldownEnd = time.Time{}
			maskKey := maskAPIKey(p.entries[i].key)
			logFields := []any{
				"target", p.targetName,
				"key_index", i,
				"key", maskKey,
				"reason", "cooldown_expired",
			}
			if clientName != "" {
				p.bindLocked(clientName, i, now)
				logFields = append(logFields, "client", clientName)
			}
			p.logger.Info("[keypool] key recovered", logFields...)
			go p.asyncNotifyKeyChange()
			return i, true
		}
	}
	return -1, false
}

// 429 限流（rate_limited）采用指数退避冷却：首次短，连续限流翻倍，上限 60s。
//
//	序列：5s → 10s → 20s → 40s → 60s(cap)
//
// 瞬时 RPM 限流（秒级恢复）首次 5s 快速恢复服务；持续限流逐渐加长避免多 key 打转。
// consecutiveRateLimit 记录连续限流次数，仅在真实成功（markKeySuccess）或探测成功
// （markRecovered）时清零——CD 过期恢复不清零（恢复后可能立即又限流，需累加）。
const (
	rateLimitedBaseCooldown = 5 * time.Second
	rateLimitedMaxCooldown  = 60 * time.Second
)

// permanentCooldownEnd 是"永久屏蔽"类 reason 的冷却终点（公元 9999 年）。
// 用于 quota_exceeded_api（无 resetTime）/ 默认未知 reason / 账户级硬失败跨重启保留时
// 重写 snapshot 的 CooldownEnd，确保 restoreExhaustedKeys 的 `if s.CooldownEnd.After(now)`
// 守卫为 true（永久在 now 之后）。
//
// 注意：time.Time 不是常量表达式，故用包级 var（init 后只读，不会被改写）。
var permanentCooldownEnd = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// rateLimitedCooldownFor 按 consecutiveRateLimit 计算指数退避冷却时长。
// count=1→5s, =2→10s, =3→20s, =4→40s, ≥5→60s(cap)。
func rateLimitedCooldownFor(count int) time.Duration {
	if count < 1 {
		count = 1
	}
	cd := rateLimitedBaseCooldown << (count - 1)
	if cd > rateLimitedMaxCooldown || cd <= 0 { // <=0 防高位溢出
		return rateLimitedMaxCooldown
	}
	return cd
}

// markExhausted 标记指定 index 的 key 为耗尽。
func (p *keyPool) markExhausted(index int, errorCode string) {
	p.markExhaustedWithError(index, errorCode, "")
}

// markExhaustedWithCooldown 标记 key 耗尽并使用外部指定的 cooldownEnd（而非按 errorCode 推导）。
// 用于智谱 GLM 5h 限流：cooldownEnd 来自 SSE message 里的精确重置时间（CST）。
// 其余行为（lastError 记录、日志、幂等去重）与 markExhaustedWithError 一致。
func (p *keyPool) markExhaustedWithCooldown(index int, errorCode string, cooldownEnd time.Time, errBody string) {
	if p == nil || index < 0 || index >= len(p.entries) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.entries[index].exhausted {
		return // 已标记，不重复
	}

	now := time.Now()
	p.entries[index].exhausted = true
	p.entries[index].exhaustedAt = now
	p.entries[index].exhaustReason = errorCode
	p.entries[index].cooldownEnd = cooldownEnd
	if errBody != "" {
		p.entries[index].lastError = truncateErrBody(errBody)
		p.entries[index].lastErrorAt = now
	}

	maskKey := maskAPIKey(p.entries[index].key)
	p.logger.Warn("[keypool] key exhausted",
		"target", p.targetName,
		"key_index", index,
		"key", maskKey,
		"error_code", errorCode,
		"cooldown_end", cooldownEnd,
		"upstream_error", p.entries[index].lastError,
	)
	// Notify admission manager of active key change
	go p.asyncNotifyKeyChange()
}

// markExhaustedWithError 标记 key 耗尽，并记录不含响应体的诊断提示。
func (p *keyPool) markExhaustedWithError(index int, errorCode, errBody string) {
	if p == nil || index < 0 || index >= len(p.entries) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.entries[index].exhausted {
		return // 已标记，不重复
	}

	now := time.Now()
	p.entries[index].exhausted = true
	p.entries[index].exhaustedAt = now
	if errBody != "" {
		p.entries[index].lastError = truncateErrBody(errBody)
		p.entries[index].lastErrorAt = now
	}

	// 按 errorCode 计算冷却结束时间
	// quota_exceeded → 根据 resetTime 拆分为两种子类型：
	//   - 有 resetTime → quota_exceeded_subscription（自动恢复）
	//   - 无 resetTime → quota_exceeded_api（人工恢复，永久屏蔽）
	// rate_limited → 指数退避冷却（5→10→20→40→60s，见 rateLimitedCooldownFor）
	// invalid_token / billing_error / account_disabled → 5 分钟冷却（防止误判导致永久屏蔽，支持唤醒恢复）
	// 其他错误 → 永久屏蔽（需手动解除）
	switch errorCode {
	case "quota_exceeded":
		resetTime := p.resetTime
		if p.resetTimeSpec != "" {
			resetTime = parseResetTimeSpec(p.targetName, p.resetTimeSpec, p.logger, now)
			p.resetTime = resetTime
		}
		if !resetTime.IsZero() && resetTime.After(now) {
			p.entries[index].exhaustReason = "quota_exceeded_subscription"
			p.entries[index].cooldownEnd = resetTime
		} else {
			p.entries[index].exhaustReason = "quota_exceeded_api"
			p.entries[index].cooldownEnd = permanentCooldownEnd
		}
	case "rate_limited":
		p.entries[index].exhaustReason = errorCode
		p.entries[index].consecutiveRateLimit++
		p.entries[index].cooldownEnd = now.Add(rateLimitedCooldownFor(p.entries[index].consecutiveRateLimit))
	case "invalid_token", "billing_error", "account_disabled":
		p.entries[index].exhaustReason = errorCode
		p.entries[index].cooldownEnd = now.Add(5 * time.Minute)
	default:
		p.entries[index].exhaustReason = errorCode
		p.entries[index].cooldownEnd = permanentCooldownEnd
	}

	maskKey := maskAPIKey(p.entries[index].key)
	p.logger.Warn("[keypool] key exhausted",
		"target", p.targetName,
		"key_index", index,
		"key", maskKey,
		"error_code", errorCode,
		"cooldown_end", p.entries[index].cooldownEnd,
		"upstream_error", p.entries[index].lastError,
	)
	// Notify admission manager of active key change
	go p.asyncNotifyKeyChange()
}

// truncateErrBody never exposes an upstream body in logs or status snapshots.
func truncateErrBody(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return "upstream error (details withheld)"
}

func diagnosticGLMCode(code string) string {
	switch code {
	case "", "1302", "1303", "1305", "1308", "1310", "1113", "1309":
		return code
	default:
		return "unknown"
	}
}

// markRateLimited 标记指定 key 为限流状态（指数退避冷却）。
// 与 markExhausted 不同，限流是临时的，冷却后自动恢复。
func (p *keyPool) markRateLimited(index int) {
	p.markExhausted(index, "rate_limited")
}

// markRateLimitedWithBody 标记指定 key 为限流状态，根据 429 body 里的业务错误码决定冷却时长。
// 智谱 API-CODE 文档（docs.bigmodel.cn/cn/api/api-code）：
//   - 1308/1310（额度限流）：从 message 提取重置时间，静默到该时刻（fallback 5h）
//   - 1302/1303（并发/频率限流）：60s 固定冷却
//   - 其他 code 或解析失败：指数退避（5→60s）
func (p *keyPool) markRateLimitedWithBody(index int, body []byte) {
	if p == nil || index < 0 || index >= len(p.entries) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries[index].exhausted {
		return // 幂等
	}

	now := time.Now()
	code, resetAt := parse429Body(body)

	p.entries[index].exhausted = true
	p.entries[index].exhaustedAt = now

	// reason 分类：解析到智谱业务码（1308/1310/1302/1303）时保留 GLM 语义，
	// 否则（非智谱上游或解析失败）退化为通用 rate_limited。
	// 这让 status() 能区分 GLM 时限额/周限额/QPS，而非全部显示"限流中"。
	reason := "rate_limited"
	if code != "" {
		reason = glmReasonForCode(code)
	}
	p.entries[index].exhaustReason = reason
	if len(body) != 0 {
		p.entries[index].lastError = truncateErrBody(string(body))
		p.entries[index].lastErrorAt = now
	}

	// 按 code 决定冷却时长
	switch code {
	case "1308", "1310":
		// 额度限流：用 message 里的重置时间，fallback 5h
		p.entries[index].cooldownEnd = glmRateLimitCooldownEnd(resetAt, now)
	case "1302", "1303", "1305":
		// 瞬时类限流：并发/频率限流(QPS)+服务过载 60s 固定
		p.entries[index].cooldownEnd = now.Add(60 * time.Second)
	case "1113", "1309":
		// 账户级失效（欠费/套餐到期，#159 F5）：与 SSE 路径 glmCooldownForCode 一致，永久屏蔽
		p.entries[index].cooldownEnd = permanentCooldownEnd
	default:
		// 区分"智谱未知 code（非空但不在 case 列表，如 1311 套餐模型权限误报）"与"非智谱上游（code 为空）"：
		// 前者与 SSE 路径（glmCooldownForCode else 分支）对齐，用 glmRateLimitFallbackCooldown（6s）
		// 短冷却兜底（resetAt 此处必为零，因 parse429Body 仅对 1308/1310 填 resetAt）；
		// 后者保持指数退避。两路径必须同值，避免双路径冷却时长不一致。
		if code != "" {
			// 智谱未知 code：与 SSE 路径对齐，6s 兜底（不递增 consecutiveRateLimit，与 SSE 路径行为一致）
			p.entries[index].cooldownEnd = glmRateLimitCooldownEnd(resetAt, now)
		} else {
			// 非智谱上游或解析失败：指数退避（5→60s）
			p.entries[index].consecutiveRateLimit++
			p.entries[index].cooldownEnd = now.Add(rateLimitedCooldownFor(p.entries[index].consecutiveRateLimit))
		}
	}

	maskKey := maskAPIKey(p.entries[index].key)
	p.logger.Warn("[keypool] key rate limited",
		"target", p.targetName,
		"key_index", index,
		"key", maskKey,
		"code", diagnosticGLMCode(code),
		"cooldown_end", p.entries[index].cooldownEnd,
	)
	// Notify admission manager of active key change
	go p.asyncNotifyKeyChange()
}

// selectNextActiveKey 从指定 key 之后找下一个 active key。
// 返回 (key, index)。如果无可用 key 返回 ("", -1)。
func (p *keyPool) selectNextActiveKey(afterIndex int) (string, int) {
	if p == nil || len(p.entries) == 0 {
		return "", -1
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	n := len(p.entries)

	// 从 afterIndex+1 开始找
	for i := 1; i <= n; i++ {
		idx := (afterIndex + i) % n
		if !p.entries[idx].exhausted && !p.entries[idx].blocked {
			return p.entries[idx].key, idx
		}
	}

	// 全部 exhausted/blocked，找冷却期已过的（跳过 blocked）
	if idx, ok := p.recoverFromCooldownLocked(now, ""); ok {
		return p.entries[idx].key, idx
	}

	return "", -1
}

// blockKey 手动屏蔽指定 key（永久，直到手动解除）。
func (p *keyPool) blockKey(index int) {
	if p == nil || index < 0 || index >= len(p.entries) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[index].blocked = true
	maskKey := maskAPIKey(p.entries[index].key)
	p.logger.Info("[keypool] key blocked manually",
		"target", p.targetName,
		"key_index", index,
		"key", maskKey,
	)
	// Notify admission manager of active key change
	go p.asyncNotifyKeyChange()
}

// unblockKey 解除手动屏蔽。
func (p *keyPool) unblockKey(index int) {
	if p == nil || index < 0 || index >= len(p.entries) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[index].blocked = false
	p.entries[index].exhausted = false
	p.entries[index].exhaustedAt = time.Time{}
	p.entries[index].exhaustReason = ""
	p.entries[index].cooldownEnd = time.Time{}
	maskKey := maskAPIKey(p.entries[index].key)
	p.logger.Info("[keypool] key unblocked manually",
		"target", p.targetName,
		"key_index", index,
		"key", maskKey,
	)
	// Notify admission manager of active key change
	go p.asyncNotifyKeyChange()
}

// blockedKeys 返回当前被手动屏蔽的 key 值集合。
// 独立快照 helper；配置热更新直接共享运行态，不使用此快照。
// 按 key 值匹配（而非 index），避免 config 变更导致 key 顺序错位时
// 错误地屏蔽了不同的 key。
func (p *keyPool) blockedKeys() map[string]bool {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make(map[string]bool)
	for _, e := range p.entries {
		if e.blocked {
			result[e.key] = true
		}
	}
	return result
}

// restoreBlockedKeys 将之前快照的屏蔽状态恢复到新构建的 key pool。
// 按 key 值匹配新 entries，即使 config 变更导致 key 顺序变化也能正确恢复。
func (p *keyPool) restoreBlockedKeys(blocked map[string]bool) {
	if p == nil || len(blocked) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.entries {
		if blocked[p.entries[i].key] {
			p.entries[i].blocked = true
		}
	}
}

// keyExhaustedState 是独立快照 helper 的耗尽状态；热更新直接共享运行态。
type keyExhaustedState struct {
	Exhausted            bool
	ExhaustReason        string
	CooldownEnd          time.Time
	ConsecutiveRateLimit int
}

// exhaustedKeys 返回当前处于 exhausted 状态且冷却期尚未过期的 key 快照。
// 按 key 值索引（而非 index），即使 config 变更导致 key 顺序变化也能正确恢复。
// 冷却期已过期的 key 不纳入快照——它们在下一次 selectKey 时会自然恢复。
//
// 跨重启语义由 markExhausted 赋予的 cd 数值本身承载，无需此处二次分类：
//   - 永久类（quota_exceeded_api / Arrearage / 未知等）：cd=permanentCooldownEnd(9999)，永不过期，天然跨重启保留。
//   - 凭证类（invalid_token / billing_error / account_disabled）：cd=now+5min，重启耗时 > 5min 时洗白为 active；
//     首个请求验证失败会重新 markExhausted(5min)，自愈闭环成立。刻意保留反而会破坏 5min 防误判语义。
//   - 瞬时/额度类（rate_limited / glm_* / quota_exceeded_subscription）：cd 过期即洗白，语义正确。
func (p *keyPool) exhaustedKeys() map[string]keyExhaustedState {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	result := make(map[string]keyExhaustedState)
	for _, e := range p.entries {
		if e.exhausted && !e.blocked && e.cooldownEnd.After(now) {
			result[e.key] = keyExhaustedState{
				Exhausted:            e.exhausted,
				ExhaustReason:        e.exhaustReason,
				CooldownEnd:          e.cooldownEnd,
				ConsecutiveRateLimit: e.consecutiveRateLimit,
			}
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// restoreExhaustedKeys 将之前快照的 exhausted 状态恢复到新构建的 key pool。
// 按 key 值匹配新 entries；cooldownEnd 已过期的 key 不恢复（让它自然变为 active）。
// 与 restoreBlockedKeys 独立：blocked key 的 exhausted 状态不会被恢复。
func (p *keyPool) restoreExhaustedKeys(state map[string]keyExhaustedState) {
	if p == nil || len(state) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	for i := range p.entries {
		s, ok := state[p.entries[i].key]
		if !ok {
			continue
		}
		if p.entries[i].blocked {
			continue
		}
		if s.CooldownEnd.After(now) {
			p.entries[i].exhausted = s.Exhausted
			p.entries[i].exhaustReason = s.ExhaustReason
			p.entries[i].cooldownEnd = s.CooldownEnd
			p.entries[i].consecutiveRateLimit = s.ConsecutiveRateLimit
		}
	}
}

// markRecovered 标记指定 key 为已恢复（绝境探测成功后调用）。
// 返回 true 表示状态发生了变化（key 由 exhausted 转为 active），
// 返回 false 表示该 key 本就不是 exhausted（含纯手动 blocked），未做任何改动。
func (p *keyPool) markRecovered(index int) bool {
	if p == nil || index < 0 || index >= len(p.entries) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.entries[index].exhausted {
		return false // 已经是 active 状态（或仅手动 blocked），无需恢复
	}
	p.entries[index].exhausted = false
	p.entries[index].exhaustedAt = time.Time{}
	p.entries[index].exhaustReason = ""
	p.entries[index].cooldownEnd = time.Time{}
	p.entries[index].consecutiveRateLimit = 0 // 探测成功，重置指数退避计数
	p.entries[index].consecutiveFails = 0     // WP-3：重置连续失败计数
	maskKey := maskAPIKey(p.entries[index].key)
	p.logger.Info("[keypool] key recovered after desperation probe",
		"target", p.targetName,
		"key_index", index,
		"key", maskKey,
	)
	// Notify admission manager of active key change
	go p.asyncNotifyKeyChange()
	return true
}

// markKeySuccess 标记指定 key 真实成功服务了一次请求，重置其连续 429 限流计数。
// 与 markRecovered（探测成功）区分：本方法用于真实请求成功（key 本就 active），
// 仅清零 consecutiveRateLimit，不动 exhausted 状态（active 无需恢复）。
// keyPool 为 nil / index 越界时为 no-op。
func (p *keyPool) markKeySuccess(index int) {
	if p == nil || index < 0 || index >= len(p.entries) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[index].consecutiveRateLimit = 0
}

// unmarkExhaustedForRetry 临时解除 key 的 exhausted 状态，允许下一次
// selectKeyForClient 重新选中该 key。保留 consecutiveFails 和 consecutiveRateLimit
// 不变（WP-3 RETRY<2 坚持同一个 key 时使用）。
// 注意：仅清除 exhausted 标记和 cooldownEnd，不动 exhaustReason（用于日志诊断）。
func (p *keyPool) unmarkExhaustedForRetry(index int) {
	if p == nil || index < 0 || index >= len(p.entries) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	availabilityChanged := p.entries[index].exhausted && !p.entries[index].blocked
	p.entries[index].exhausted = false
	p.entries[index].cooldownEnd = time.Time{}
	if availabilityChanged {
		go p.asyncNotifyKeyChange()
	}
}

// incrementFails 增加连续失败计数并返回当前计数（WP-3：HTTP 429 或 GLM 1302）。
// 返回值用于调用方判断 RETRY<2（坚持）还是 RETRY≥2（换 key）。
// keyPool 为 nil / keyIndex 越界时返回 0。
func (p *keyPool) incrementFails(keyIndex int) int {
	if p == nil || keyIndex < 0 || keyIndex >= len(p.entries) {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[keyIndex].consecutiveFails++
	return p.entries[keyIndex].consecutiveFails
}

// resetFails 重置连续失败计数（成功时调用，WP-3）。
// keyPool 为 nil / keyIndex 越界时为 no-op。
func (p *keyPool) resetFails(keyIndex int) {
	if p == nil || keyIndex < 0 || keyIndex >= len(p.entries) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[keyIndex].consecutiveFails = 0
}

// KeyStatus 表示单个 key 的状态快照（用于 admin API）。
type KeyStatus struct {
	Index         int       `json:"index"`
	KeyMask       string    `json:"key_mask"`
	Exhausted     bool      `json:"exhausted"`
	ExhaustReason string    `json:"exhaust_reason,omitempty"`
	ExhaustedAt   time.Time `json:"exhausted_at,omitempty"`
	CooldownEnd   time.Time `json:"cooldown_end,omitempty"`
	Blocked       bool      `json:"blocked"`
	LastError     string    `json:"last_error,omitempty"`    // 最近一次上游错误的受控提示
	LastErrorAt   time.Time `json:"last_error_at,omitempty"` // 最近一次错误时间
	Status        string    `json:"status"`                  // 已屏蔽 / 额度超限 / 限流中 / 使用中 / 等待中 / 可用(待激活) / 失效(待请求验证) 等
}

// isBlockedOnly 判断指定 key 是否为纯手动屏蔽（blocked 且非 exhausted）。
// 唤醒时对这类 key 直接解除屏蔽，不依赖探测端点（并非所有 provider 都支持 /v1/models）。
func (p *keyPool) isBlockedOnly(index int) bool {
	if p == nil || index < 0 || index >= len(p.entries) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.entries[index].blocked && !p.entries[index].exhausted
}

func (p *keyPool) status() []KeyStatus {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	result := make([]KeyStatus, len(p.entries))

	// 找第一个 active key 作为"使用中"
	currentIdx := -1
	for i, e := range p.entries {
		if !e.exhausted && !e.blocked {
			currentIdx = i
			break
		}
	}

	for i, e := range p.entries {
		ks := KeyStatus{
			Index:     i,
			KeyMask:   maskAPIKey(e.key),
			Exhausted: e.exhausted,
			Blocked:   e.blocked,
		}
		if e.exhausted {
			ks.ExhaustReason = e.exhaustReason
			ks.ExhaustedAt = e.exhaustedAt
			ks.CooldownEnd = e.cooldownEnd
		}
		if e.lastError != "" {
			ks.LastError = truncateErrBody(e.lastError)
			ks.LastErrorAt = e.lastErrorAt
		}

		// 计算显示状态（status() 只读：禁止改写 exhausted/exhaustReason/cooldownEnd 字段，
		// 也禁止调用 recoverFromCooldownLocked。冷却过期判定与 recoverFromCooldownLocked
		// 对齐：now >= cd 等价于 !now.Before(cd)，但必须先用 IsZero() 守护零值）。
		switch {
		case e.blocked:
			ks.Status = "已屏蔽"
		case e.exhausted:
			// 二维分类：reason × 冷却时间
			if e.cooldownEnd.IsZero() {
				// 无冷却终点（含零值）：走 reason 原文案，不判过期
				ks.Status = keyStatusLabelForReason(e.exhaustReason)
			} else if !now.Before(e.cooldownEnd) {
				// 冷却已过期（now >= cd）：按 reason 区分"真恢复类"与"待验证类"
				if isRecoverableAfterCooldown(e.exhaustReason) {
					ks.Status = "可用(待激活)"
				} else {
					ks.Status = "失效(待请求验证)"
				}
			} else {
				// 冷却未过期（now < cd）：走 reason 原文案
				ks.Status = keyStatusLabelForReason(e.exhaustReason)
			}
		case i == currentIdx:
			ks.Status = "使用中"
		default:
			ks.Status = "等待中"
		}

		result[i] = ks
	}
	return result
}

// maskAPIKey 遮蔽 key 中间部分，只保留前 6 后 4。
func maskAPIKey(key string) string {
	if len(key) <= 10 {
		return "***"
	}
	return key[:6] + "***" + key[len(key)-4:]
}

// recordTokens 记录指定 key 的一次请求 token 用量，用于滑动窗口负载均衡。
func (p *keyPool) recordTokens(keyIndex int, tokens int64) {
	if p == nil || keyIndex < 0 || keyIndex >= len(p.entries) || tokens <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	p.entries[keyIndex].tokenSamples = append(p.entries[keyIndex].tokenSamples, tokenSample{at: now, tokens: tokens})
	p.pruneExpiredSamplesLocked(keyIndex, now)
}

// pruneExpiredSamplesLocked 清理指定 key 的过期样本（超出滑动窗口）。
// 调用方必须持有 p.mu。
func (p *keyPool) pruneExpiredSamplesLocked(keyIndex int, now time.Time) {
	samples := p.entries[keyIndex].tokenSamples
	cutoff := now.Add(-p.tokenWindow)
	// 找到第一个未过期的索引
	i := 0
	for i < len(samples) && samples[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		remaining := samples[i:]
		// 当过期样本占比过半时拷贝到新切片，回收底层数组容量，
		// 避免高 RPS 下 tokenSamples 永久保留峰值 cap。
		if i > len(remaining) {
			p.entries[keyIndex].tokenSamples = append([]tokenSample(nil), remaining...)
		} else {
			p.entries[keyIndex].tokenSamples = remaining
		}
	}
}

// tokenSumInWindow 返回指定 key 在滑动窗口内的累计 token 数。
// 调用方必须持有 p.mu。
func (p *keyPool) tokenSumInWindow(keyIndex int, now time.Time) int64 {
	p.pruneExpiredSamplesLocked(keyIndex, now)
	var sum int64
	for _, s := range p.entries[keyIndex].tokenSamples {
		sum += s.tokens
	}
	return sum
}

// acquireInFlight 在为一次真实转发选中 key 后调用，增加该 key 的在途计数。
func (p *keyPool) acquireInFlight(index int) {
	if p == nil || index < 0 {
		return
	}
	p.mu.Lock()
	if index < len(p.inFlight) {
		p.inFlight[index]++
	}
	p.mu.Unlock()
}

// releaseInFlight 在一次转发结束（响应体关闭或转发出错）时调用，减少在途计数。
// 带下限保护，避免重复释放导致计数变负而永久扭曲调度。
func (p *keyPool) releaseInFlight(index int) {
	if p == nil || index < 0 {
		return
	}
	p.mu.Lock()
	if index < len(p.inFlight) && p.inFlight[index] > 0 {
		p.inFlight[index]--
	}
	p.mu.Unlock()
}

// activeKeyCount 返回当前处于可用状态（未冷却、未耗尽、未阻塞）的 key 数量，
// 供目标层（target.go #1）按真实存活容量加权分配请求。
func (p *keyPool) activeKeyCount() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.activeKeyCountLocked()
}

// activeKeyCountLocked reads this generation's entries under the shared domain.
func (p *keyPool) activeKeyCountLocked() int {
	count := 0
	for i := range p.entries {
		if !p.entries[i].exhausted && !p.entries[i].blocked {
			count++
		}
	}
	return count
}

// quotaExhaustionStatus 判断全池耗尽是否由限额类（1308/1310/quota_exceeded_subscription）
// 导致，用于 WP-1 的 writeUpstreamLimitedResponse 区分返回体语义。
// 返回 (isQuota, resetAt)：isQuota=true 时 resetAt 是最远的限额恢复时间。
// 如果有任一 key 不是限额类（如限流类/硬失败），返回 (false, 零值)。
func (p *keyPool) quotaExhaustionStatus() (bool, time.Time) {
	if p == nil {
		return false, time.Time{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var furthestReset time.Time
	anyExhausted := false
	for i := range p.entries {
		e := &p.entries[i]
		if !e.exhausted || e.blocked {
			// 有 active key → 不是全池耗尽
			return false, time.Time{}
		}
		anyExhausted = true
		// 限额类 reason 集合：1308(5h) / 1310(quota) / quota_exceeded_subscription
		switch e.exhaustReason {
		case glmExhaustReason, glmRateLimitQuota, glmRateLimitUnknown, "quota_exceeded_subscription":
			if e.cooldownEnd.After(furthestReset) {
				furthestReset = e.cooldownEnd
			}
		default:
			// 存在非限额类 reason（限流/硬失败）→ 返回限流语义
			return false, time.Time{}
		}
	}
	if !anyExhausted {
		return false, time.Time{}
	}
	return true, furthestReset
}

// keySnapshot 是一个 key 的只读快照，用于模型连通性测试等外部探针。
type keySnapshot struct {
	Key   string
	Index int
}

// snapshotActiveKeys 返回所有当前可用（非 exhausted、非 blocked、冷却期已过）的 key 快照，
// 按 rrCounter 轮询顺序排列。纯只读：不推进 rrCounter、不修改任何状态、不触发恢复回调。
// 供 TestModel 等探针使用，避免污染生产 keypool 状态。
// 如果所有 key 都在冷却中（rate_limited 等），返回空切片——测试不应绕过 keypool 的冷却决策。
func (p *keyPool) snapshotActiveKeys() []keySnapshot {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	n := len(p.entries)
	start := int(p.rrCounter) % n
	var out []keySnapshot
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		e := p.entries[idx]
		if e.blocked {
			continue
		}
		if e.exhausted {
			// 冷却期已过也算 active（recoverFromCooldownLocked 的语义一致）
			if now.Before(e.cooldownEnd) {
				continue
			}
		}
		out = append(out, keySnapshot{Key: e.key, Index: idx})
	}
	return out
}

// scoreLessLocked 比较两个 key 的负载得分：先比在途请求数（least-connections），
// 再比滑动窗口累计 token（least-token）。a 更空闲返回 true。调用方必须持有 p.mu。
func (p *keyPool) scoreLessLocked(a, b int, now time.Time) bool {
	if a < 0 || a >= len(p.entries) {
		return false
	}
	if b < 0 || b >= len(p.entries) {
		return true
	}
	infA, infB := p.inFlight[a], p.inFlight[b]
	if infA != infB {
		return infA < infB
	}
	return p.tokenSumInWindow(a, now) < p.tokenSumInWindow(b, now)
}

// bestActiveKeyLocked 返回当前负载最低（least-connections + least-token）的 active key。
// 无 active key 返回 -1。调用方必须持有 p.mu。
func (p *keyPool) bestActiveKeyLocked(now time.Time) int {
	best := -1
	for i := range p.entries {
		if p.entries[i].exhausted || p.entries[i].blocked {
			continue
		}
		if best == -1 || p.scoreLessLocked(i, best, now) {
			best = i
		}
	}
	return best
}

// bindLocked 建立/更新客户端到 key 的亲和绑定，并记录时间戳。
// 新增绑定前按需淘汰陈旧条目，防止 map 无限增长。调用方必须持有 p.mu。
func (p *keyPool) bindLocked(clientName string, idx int, now time.Time) {
	if _, exists := p.clientAffinity[clientName]; !exists {
		p.evictStaleAffinityLocked(now)
	}
	p.clientAffinity[clientName] = idx
	p.clientAffinityAt[clientName] = now
}

// evictStaleAffinityLocked 在绑定表达到容量上限时淘汰条目：
// 优先淘汰空闲超过 affinityTTL 的绑定；若仍超限，淘汰最旧的一条。
// 调用方必须持有 p.mu。
func (p *keyPool) evictStaleAffinityLocked(now time.Time) {
	if len(p.clientAffinity) < p.maxAffinityEntries {
		return
	}
	// 第一轮：淘汰所有空闲超过 TTL 的绑定
	for name, ts := range p.clientAffinityAt {
		if now.Sub(ts) >= p.affinityTTL {
			delete(p.clientAffinity, name)
			delete(p.clientAffinityAt, name)
		}
	}
	// 第二轮：若仍达上限，淘汰最旧的一条，保证能腾出空间
	for len(p.clientAffinity) >= p.maxAffinityEntries {
		oldestName := ""
		var oldestTS time.Time
		for name, ts := range p.clientAffinityAt {
			if oldestName == "" || ts.Before(oldestTS) {
				oldestName = name
				oldestTS = ts
			}
		}
		if oldestName == "" {
			break
		}
		delete(p.clientAffinity, oldestName)
		delete(p.clientAffinityAt, oldestName)
	}
}

// leastTokenActiveKeyIndex 返回滑动窗口内累计 token 最少的 active key 的索引。
// 如果没有 active key，返回 -1。
// 如果所有 active key 都没有 token 数据（刚启动），返回 -2 表示应退化为轮询。
func (p *keyPool) leastTokenActiveKeyIndex() int {
	if p == nil || len(p.entries) == 0 {
		return -1
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	bestIdx := -1
	bestTokens := int64(-1)
	hasAnyData := false

	for i := range p.entries {
		if p.entries[i].exhausted || p.entries[i].blocked {
			continue
		}
		sum := p.tokenSumInWindow(i, now)
		if sum > 0 {
			hasAnyData = true
		}
		if bestIdx == -1 || sum < bestTokens {
			bestIdx = i
			bestTokens = sum
		}
	}

	if bestIdx == -1 {
		return -1 // 无 active key
	}
	if !hasAnyData {
		return -2 // 有 active key 但无数据，退化为轮询
	}
	return bestIdx
}

// tryWakeUp 尝试触发唤醒模型。
// 返回 true 表示成功获取唤醒锁（调用方应执行探测逻辑），返回 false 表示唤醒冷却中或已有其他协程在执行。
// 调用方必须在探测完成后调用 wakeUpComplete()。
func (p *keyPool) tryWakeUp(now time.Time) bool {
	if p == nil {
		return false
	}

	// 检查冷却
	p.mu.Lock()
	if now.Before(p.wakeUpCooldown) {
		p.mu.Unlock()
		return false
	}
	p.mu.Unlock()

	// 单例检查（原子操作）
	if !p.wakeUpInProgress.CompareAndSwap(false, true) {
		return false // 已有其他协程在执行唤醒
	}

	p.logger.Info("[keypool] wake-up model triggered",
		"target", p.targetName,
	)
	return true
}

// wakeUpComplete 唤醒探测完成后调用，设置冷却并重置单例标记。
func (p *keyPool) wakeUpComplete() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.wakeUpCooldown = time.Now().Add(1 * time.Minute)
	p.mu.Unlock()
	p.wakeUpInProgress.Store(false)
}

// getExhaustedKeys 返回所有被屏蔽或超额的 key 索引（用于唤醒探测）。
// 按冷却剩余时间从短到长排序，优先探测即将恢复的 key。
func (p *keyPool) getExhaustedKeys() []int {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	type keyWithRemaining struct {
		index     int
		remaining time.Duration
	}
	var exhausted []keyWithRemaining

	for i := range p.entries {
		if p.entries[i].exhausted || p.entries[i].blocked {
			remaining := p.entries[i].cooldownEnd.Sub(now)
			if remaining < 0 {
				remaining = 0
			}
			exhausted = append(exhausted, keyWithRemaining{index: i, remaining: remaining})
		}
	}

	// 按冷却剩余时间排序（短的优先）
	sort.Slice(exhausted, func(i, j int) bool {
		return exhausted[i].remaining < exhausted[j].remaining
	})

	result := make([]int, len(exhausted))
	for i, e := range exhausted {
		result[i] = e.index
	}
	return result
}

// isAccountLevelExhaustion 判定耗尽原因是否为"账户级"失败——即同一账户下所有 key 大概率
// 共享的失效（无效凭证、欠费、封禁、总额度永久耗尽）。这类失败换 key 重试无望，应停止
// 逐 key 重试并把真实上游响应透传给客户端。
//
// 注意：与 isHardFailureReason 不同，这里**不包含** rate_limited（瞬时限流，换 key 或短
// 冷却后重试通常可恢复）与 quota_exceeded_subscription（到 resetTime 自动恢复，按订阅周期
// 计而非账户失效）。
func isAccountLevelExhaustion(reason string) bool {
	switch reason {
	case "invalid_token",
		"account_disabled",
		"billing_error",
		"quota_exceeded_api",
		"Arrearage",
		"free_tier_exhausted",
		"AccessDenied.Unpurchased",
		glmAccountArrears, // GLM 1113 账户欠费
		glmPlanExpired:    // GLM 1309 套餐到期
		return true
	default:
		return false
	}
}

// isHardFailureReason 判定某个耗尽原因是否为"硬失败"——即 key/账户本身的问题
// （无效、欠费、封禁、总额度耗尽），只能通过真实请求成功或冷却定时器到期来确认恢复，
// 廉价的 GET /models 探测无法可靠验证（部分上游对任意 key 都放行 /models，会造成假阳性）。
func isHardFailureReason(reason string) bool {
	switch reason {
	case "invalid_token",
		"account_disabled",
		"billing_error",
		"rate_limited",
		"quota_exceeded_api",
		"quota_exceeded_subscription",
		"Arrearage",
		"free_tier_exhausted",
		"AccessDenied.Unpurchased",
		glmAccountArrears,    // GLM 1113 账户欠费
		glmPlanExpired,       // GLM 1309 套餐到期
		glmExhaustReason,     // GLM 5h 额度限流（1308）
		glmRateLimitQuota,    // GLM 周/月额度限流（1310）
		glmRateLimitQPS,      // GLM 速率/QPS 限流（1302）
		glmRateLimitOverload, // GLM 服务过载（1305），/models 对过载同样不可靠（推理路径过载但 /models 可能假阳性 200）
		glmRateLimitUnknown:  // GLM 未知限流 code 兜底
		// GLM 所有限流均为 HTTP 200 + SSE 或 HTTP 429：GET /models 不消耗配额会假阳性返回 200，廉价探测不可靠。
		return true
	default:
		return false
	}
}

// isRecoverableAfterCooldown 判定某个耗尽原因在冷却到期后是否"真恢复"——即只要冷却窗口
// 过去，key 即可视为真正可用（额度已重置 / 限流窗口已过），无需进一步请求验证。
// 与 isHardFailureReason / isAccountLevelExhaustion 互补且独立：
//   - true：瞬时限流类（rate_limited / GLM 各类限流 / quota_exceeded_subscription），冷却过期=真可用
//   - false：账户/凭证类问题（invalid_token / billing_error / account_disabled / quota_exceeded_api /
//     Arrearage / free_tier_exhausted / AccessDenied.Unpurchased）或空/未知 reason，
//     冷却过期仅意味着允许重试，未必真正恢复，需要真实请求验证。
//
// 注意：永久类原因（permanentEnd(9999)）的 cooldownEnd 永远在未来，不会走到"已过期"分支，
// 因此不会在此函数被判 true；这里只覆盖"如果它们恰好过期了，是否算真恢复"。
func isRecoverableAfterCooldown(reason string) bool {
	switch reason {
	case "rate_limited",
		glmRateLimitQPS,               // 1302：速率/QPS 限流（瞬时）
		glmRateLimitOverload,          // 1305：服务过载（瞬时，60s 冷却过期=真恢复）
		glmExhaustReason,              // 1308：5h 额度限流
		glmRateLimitQuota,             // 1310：周/月额度限流
		glmRateLimitUnknown,           // GLM 未知额度类 code
		"quota_exceeded_subscription": // 订阅额度到 resetTime 自动恢复
		return true
	default:
		// invalid_token / billing_error / account_disabled / quota_exceeded_api /
		// Arrearage / free_tier_exhausted / AccessDenied.Unpurchased / 空 / 未知 → 保守返回 false
		return false
	}
}

// isCoolingExpiredAndRecoverable 判断 key 是否处于"冷却已过期且可恢复"状态：
// exhausted=true 但 cooldownEnd 已过（且非零值），且 reason 是可恢复类。
// 用于 healthz metrics（service.go TargetStatuses）与 status() 的"可用(待激活)"显示口径对齐——
// 避免"冷却过期但 Exhausted=true"的 key 在 metrics 被计入 RateLimitedKeys/OtherExhaustedKeys，
// 导致 ActiveKeys=0 与 UI 全绿显示割裂，触发误告警。
//
// 注意：status() 只读契约不受影响——本函数只读取 KeyStatus，不改写字段、不调用 recover。
func isCoolingExpiredAndRecoverable(ks KeyStatus) bool {
	if !ks.Exhausted {
		return false
	}
	if ks.CooldownEnd.IsZero() {
		return false // 零值视为无冷却终点（未设置），不走恢复分支
	}
	if !time.Now().Before(ks.CooldownEnd) && isRecoverableAfterCooldown(ks.ExhaustReason) {
		return true
	}
	return false
}

// keyStatusLabelForReason 返回 reason 在"冷却未过期（或无冷却终点 / 零值）"时的专属显示文案。
// 仅由 status() 在 key 已耗尽但尚未到冷却终点的分支调用。文案与前端 keyStatusColor 一一对应。
func keyStatusLabelForReason(reason string) string {
	switch reason {
	case "quota_exceeded_subscription":
		return "额度超限(自动恢复)"
	case "quota_exceeded_api":
		return "额度超限(人工恢复)"
	case "quota_exceeded":
		// 向后兼容：旧的未细分 quota_exceeded
		return "额度超限"
	case "rate_limited":
		return "限流中"
	case glmRateLimitQPS:
		// 1302：速率/QPS 限流
		return "GLM速率限流"
	case glmRateLimitOverload:
		// 1305：平台服务过载（瞬时类，60s 冷却）
		return "GLM服务过载"
	case glmExhaustReason, glmRateLimitQuota, glmRateLimitUnknown:
		// 1308/1310/未知额度类：统一显示为额度类限流
		return "GLM额度限流"
	case "invalid_token", "billing_error", "account_disabled":
		// 凭证/账户类硬失败：冷却中
		return "失效(冷却中)"
	case glmAccountArrears:
		return "失效(GLM欠费)"
	case glmPlanExpired:
		return "失效(GLM套餐到期)"
	default:
		// Arrearage / free_tier_exhausted / AccessDenied.Unpurchased / 空 / 未知 → 通用失效
		return "失效"
	}
}

// exhaustReasonAt 返回指定 key 当前的耗尽原因（未耗尽返回 ""）。供重试收敛逻辑读取。
func (p *keyPool) exhaustReasonAt(index int) string {
	if p == nil || index < 0 {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if index >= len(p.entries) || !p.entries[index].exhausted {
		return ""
	}
	return p.entries[index].exhaustReason
}

// getProbeableExhaustedKeys 返回适合廉价 GET 探测的耗尽 key（剔除硬失败）。
// 硬失败 key 不参与廉价探测，只靠冷却定时器被动恢复，避免假阳性死循环。
func (p *keyPool) getProbeableExhaustedKeys() []int {
	all := p.getExhaustedKeys()
	if len(all) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []int
	for _, idx := range all {
		if idx >= 0 && idx < len(p.entries) && !isHardFailureReason(p.entries[idx].exhaustReason) {
			out = append(out, idx)
		}
	}
	return out
}

// setOnKeyChange registers a callback that is invoked asynchronously whenever
// the active key count changes (key exhausted, recovered, blocked, unblocked).
// The callback receives (active, total) counts. It runs in a separate goroutine
// to avoid blocking the keyPool mutex during admission manager operations.
func (p *keyPool) setOnKeyChange(fn func(active, total int)) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.onKeyChange = fn
	p.mu.Unlock()
}

// asyncNotifyKeyChange fires the onKeyChange callback with current active/total
// counts. Designed to be called as `go p.asyncNotifyKeyChange()` from within
// methods holding mu — the goroutine will acquire mu after the caller releases it.
func (p *keyPool) asyncNotifyKeyChange() {
	if p == nil {
		return
	}
	p.notifyMu.Lock()
	defer p.notifyMu.Unlock()
	p.mu.Lock()
	fn, unavailable := p.onKeyChange, p.onKeysUnavailable
	var a, t int
	if fn != nil {
		a, t = p.activeKeyCountLocked(), len(p.entries)
	}
	var keys []string
	if unavailable != nil {
		for _, entry := range p.entries {
			if entry.exhausted || entry.blocked {
				keys = append(keys, entry.key)
			}
		}
	}
	p.mu.Unlock()
	if fn != nil {
		fn(a, t)
	}
	if len(keys) > 0 {
		// The notification runs asynchronously and may arrive after a key
		// recovered: re-read each key's current state right before retiring.
		if keys = p.stillUnavailable(keys); len(keys) > 0 {
			unavailable(keys)
		}
	}
}

// stillUnavailable keeps the keys that are exhausted or blocked now.
func (p *keyPool) stillUnavailable(keys []string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	current := make(map[string]bool, len(p.entries))
	for _, entry := range p.entries {
		if entry.exhausted || entry.blocked {
			current[entry.key] = true
		}
	}
	kept := keys[:0]
	for _, key := range keys {
		if current[key] {
			kept = append(kept, key)
		}
	}
	return kept
}

// setOnKeysUnavailable registers the callback for credentials that left
// rotation; see onKeysUnavailable.
func (p *keyPool) setOnKeysUnavailable(fn func(keys []string)) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.onKeysUnavailable = fn
	p.mu.Unlock()
}

// shareRuntimeFrom joins the immediately preceding published generation. The
// caller holds Service.mu for writing and p has not been published or used yet.
// Keep old entries intact: old requests retain their indexes and resource owner.
// Keys absent from old never reconnect to an earlier, detached runtime.
func (p *keyPool) shareRuntimeFrom(old *keyPool) {
	p.keyPoolDomain = old.keyPoolDomain
	p.mu.Lock()
	defer p.mu.Unlock()
	byKey := make(map[string]*keyRuntime, len(old.entries))
	for _, entry := range old.entries {
		byKey[entry.key] = entry.keyRuntime
	}
	for i := range p.entries {
		if runtime := byKey[p.entries[i].key]; runtime != nil {
			p.entries[i].keyRuntime = runtime
		}
	}
}

// sameKeyPoolConfig compares immutable pool configuration, never runtime state.
// A changed key order must keep a new pool: in-flight requests carry old indexes.
func sameKeyPoolConfig(a, b *keyPool) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.resetTimeSpec != b.resetTimeSpec || len(a.entries) != len(b.entries) {
		return false
	}
	for i := range a.entries {
		if a.entries[i].key != b.entries[i].key {
			return false
		}
	}
	return true
}
