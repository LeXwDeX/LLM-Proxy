package proxy

import (
	"fmt"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestKeyPool(keys []string) *keyPool {
	return newKeyPool("test-target", keys, "", slog.Default())
}

func TestSelectKeyForClient_Affinity(t *testing.T) {
	pool := newTestKeyPool([]string{"key-a", "key-b", "key-c", "key-d"})

	// 同一 client 首次分配后应该始终返回同一个 key（亲和保持）
	key1, idx1 := pool.selectKeyForClient("alice")
	key2, idx2 := pool.selectKeyForClient("alice")
	if key1 != key2 || idx1 != idx2 {
		t.Errorf("same client should get same key after affinity: got (%s,%d) and (%s,%d)", key1, idx1, key2, idx2)
	}

	// 多次调用仍然保持亲和
	for i := 0; i < 5; i++ {
		k, idx := pool.selectKeyForClient("alice")
		if k != key1 || idx != idx1 {
			t.Errorf("iteration %d: affinity broken, got (%s,%d) want (%s,%d)", i, k, idx, key1, idx1)
		}
	}
}

func TestSelectKeyForClient_EmptyClient(t *testing.T) {
	pool := newTestKeyPool([]string{"key-a", "key-b"})

	// 空 clientName 退化为 selectKey（顺序消费，返回第一个）
	key, idx := pool.selectKeyForClient("")
	if key != "key-a" || idx != 0 {
		t.Errorf("empty client should get first key, got (%s, %d)", key, idx)
	}
}

func TestSelectKeyForClient_Overflow(t *testing.T) {
	pool := newTestKeyPool([]string{"key-a", "key-b", "key-c"})

	// 找到 alice 的亲和 key
	_, preferredIdx := pool.selectKeyForClient("alice")

	// 标记亲和 key 为 exhausted
	pool.markExhausted(preferredIdx, "quota_exceeded")

	// 应该溢出到下一个 active key
	key, idx := pool.selectKeyForClient("alice")
	if key == "" || idx == -1 {
		t.Fatal("should overflow to next active key")
	}
	if idx == preferredIdx {
		t.Errorf("should not return exhausted preferred key, got idx=%d", idx)
	}
}

func TestSelectKeyForClient_AllExhausted(t *testing.T) {
	pool := newTestKeyPool([]string{"key-a", "key-b"})

	pool.markExhausted(0, "quota_exceeded")
	pool.markExhausted(1, "quota_exceeded")

	key, idx := pool.selectKeyForClient("alice")
	if key != "" || idx != -1 {
		t.Errorf("all exhausted should return empty, got (%s, %d)", key, idx)
	}
}

func TestSelectKeyForClient_RoundRobin(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2", "key-3", "key-4"})

	// 5 个不同客户端应该按轮询分配到 5 个不同的 key
	assigned := make(map[string]int)
	for i := 0; i < 5; i++ {
		client := fmt.Sprintf("client-%d", i)
		_, idx := pool.selectKeyForClient(client)
		assigned[client] = idx
	}

	// 验证每个 key 恰好被分配一次
	keyCount := make(map[int]int)
	for _, idx := range assigned {
		keyCount[idx]++
	}
	for i := 0; i < 5; i++ {
		if keyCount[i] != 1 {
			t.Errorf("key %d assigned %d times, want 1 (assignments: %v)", i, keyCount[i], assigned)
		}
	}
}

func TestSelectKeyForClient_AffinityAfterRoundRobin(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})

	// 3 个客户端轮询分配
	_, idx0 := pool.selectKeyForClient("c0")
	_, idx1 := pool.selectKeyForClient("c1")
	_, idx2 := pool.selectKeyForClient("c2")

	// 再次请求应该返回之前分配的 key（亲和保持）
	_, got0 := pool.selectKeyForClient("c0")
	_, got1 := pool.selectKeyForClient("c1")
	_, got2 := pool.selectKeyForClient("c2")

	if got0 != idx0 || got1 != idx1 || got2 != idx2 {
		t.Errorf("affinity broken after round-robin: want (%d,%d,%d) got (%d,%d,%d)",
			idx0, idx1, idx2, got0, got1, got2)
	}
}

func TestSelectKeyForClient_Distribution(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2", "key-3", "key-4"})

	clients := []string{
		"client-a", "client-b", "client-c", "client-d", "client-e",
		"client-f", "client-g", "client-h", "client-i", "client-j",
	}

	// 10 个客户端分配到 5 个 key，每个 key 应该被分配 2 次
	assigned := make(map[string]int)
	for _, c := range clients {
		_, idx := pool.selectKeyForClient(c)
		assigned[c] = idx
	}

	keyCount := make(map[int]int)
	for _, idx := range assigned {
		keyCount[idx]++
	}
	for i := 0; i < 5; i++ {
		if keyCount[i] != 2 {
			t.Errorf("key %d assigned %d times, want 2 (assignments: %v)", i, keyCount[i], assigned)
		}
	}
}

// TestSelectKeyForClient_TokenAware 验证 token 感知调度：新客户端优先分配到累计 token 最少的 key。
func TestSelectKeyForClient_TokenAware(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})

	// 模拟 key-0 已经消耗了大量 token
	pool.recordTokens(0, 10000)
	pool.recordTokens(0, 5000)

	// 模拟 key-1 消耗了少量 token
	pool.recordTokens(1, 1000)

	// key-2 没有消耗（0 token）

	// 新客户端应该分配到 key-2（累计最少）
	_, idx := pool.selectKeyForClient("new-client")
	if idx != 2 {
		t.Errorf("expected new client assigned to key-2 (least tokens), got key-%d", idx)
	}

	// 再分配一个客户端，应该还是 key-2（因为 key-2 现在也只有刚分配的请求的 token，但还没 record）
	_, idx2 := pool.selectKeyForClient("new-client-2")
	if idx2 != 2 {
		t.Errorf("expected second new client assigned to key-2, got key-%d", idx2)
	}

	// 给 key-2 记录大量 token
	pool.recordTokens(2, 20000)

	// 下一个新客户端应该分配到 key-1（现在最少）
	_, idx3 := pool.selectKeyForClient("new-client-3")
	if idx3 != 1 {
		t.Errorf("expected third new client assigned to key-1 (now least tokens), got key-%d", idx3)
	}
}

// TestSelectKeyForClient_TokenAwareFallbackToRoundRobin 验证无 token 数据时退化为轮询。
func TestSelectKeyForClient_TokenAwareFallbackToRoundRobin(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})

	// 没有任何 token 数据，应该退化为轮询
	assigned := make(map[int]int)
	for i := 0; i < 6; i++ {
		client := fmt.Sprintf("client-%d", i)
		_, idx := pool.selectKeyForClient(client)
		assigned[idx]++
	}

	// 轮询应该均匀分配
	for i := 0; i < 3; i++ {
		if assigned[i] != 2 {
			t.Errorf("key-%d assigned %d times, want 2 (round-robin fallback)", i, assigned[i])
		}
	}
}

// TestRecordTokens_SlidingWindow 验证滑动窗口过期清理。
func TestRecordTokens_SlidingWindow(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1"})
	pool.tokenWindow = 100 * time.Millisecond // 缩短窗口便于测试

	// 记录 token
	pool.recordTokens(0, 1000)
	pool.recordTokens(0, 2000)

	// 验证累计
	sum := pool.tokenSumInWindow(0, time.Now())
	if sum != 3000 {
		t.Errorf("expected sum 3000, got %d", sum)
	}

	// 等待窗口过期
	time.Sleep(150 * time.Millisecond)

	// 过期后应该为 0
	sum = pool.tokenSumInWindow(0, time.Now())
	if sum != 0 {
		t.Errorf("expected sum 0 after window expired, got %d", sum)
	}

	// 记录新的 token
	pool.recordTokens(0, 500)
	sum = pool.tokenSumInWindow(0, time.Now())
	if sum != 500 {
		t.Errorf("expected sum 500 after new record, got %d", sum)
	}
}

// TestLeastTokenActiveKeyIndex 验证 least-token 选择逻辑。
func TestLeastTokenActiveKeyIndex(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})

	// 无数据时返回 -2（退化为轮询）
	idx := pool.leastTokenActiveKeyIndex()
	if idx != -2 {
		t.Errorf("expected -2 (no data, fallback to round-robin), got %d", idx)
	}

	// 记录不同 token 量
	pool.recordTokens(0, 5000)
	pool.recordTokens(1, 1000)
	pool.recordTokens(2, 3000)

	// 应该返回 key-1（最少）
	idx = pool.leastTokenActiveKeyIndex()
	if idx != 1 {
		t.Errorf("expected key-1 (least tokens), got key-%d", idx)
	}

	// 标记 key-1 为 exhausted
	pool.markExhausted(1, "quota_exceeded")

	// 应该返回 key-2（现在最少）
	idx = pool.leastTokenActiveKeyIndex()
	if idx != 2 {
		t.Errorf("expected key-2 (least among active), got key-%d", idx)
	}

	// 全部 exhausted
	pool.markExhausted(0, "quota_exceeded")
	pool.markExhausted(2, "quota_exceeded")

	// 应该返回 -1（无 active key）
	idx = pool.leastTokenActiveKeyIndex()
	if idx != -1 {
		t.Errorf("expected -1 (no active key), got %d", idx)
	}
}

// TestRateLimitedCooldownFor 验证 429 指数退避冷却序列：5→10→20→40→60(cap)。
func TestRateLimitedCooldownFor(t *testing.T) {
	cases := []struct {
		count int
		want  time.Duration
	}{
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{4, 40 * time.Second},
		{5, 60 * time.Second},
		{6, 60 * time.Second}, // cap
		{100, 60 * time.Second},
		{0, 5 * time.Second}, // 异常输入兜底
		{-1, 5 * time.Second},
	}
	for _, c := range cases {
		if got := rateLimitedCooldownFor(c.count); got != c.want {
			t.Errorf("rateLimitedCooldownFor(%d) = %v, want %v", c.count, got, c.want)
		}
	}
}

// TestRateLimitedExponentialBackoff 验证连续 429 限流冷却逐步翻倍，markKeySuccess 后重置。
func TestRateLimitedExponentialBackoff(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0"})
	now := time.Now()
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second}
	for i, w := range want {
		pool.markRateLimited(0) // active→exhausted，count++
		got := pool.entries[0].cooldownEnd.Sub(now)
		if got < w-time.Second || got > w+time.Second {
			t.Errorf("iteration %d: cooldown ~%v expected, got %v (count=%d)", i+1, w, got, pool.entries[0].consecutiveRateLimit)
		}
		// 模拟 CD 过期恢复：recoverFromCooldownLocked 只清 exhausted 不清 count
		pool.entries[0].exhausted = false
		pool.entries[0].cooldownEnd = time.Time{}
	}
	if pool.entries[0].consecutiveRateLimit != len(want) {
		t.Errorf("expected count=%d after %d consecutive limits, got %d", len(want), len(want), pool.entries[0].consecutiveRateLimit)
	}
	// markKeySuccess（真实成功）重置计数
	pool.markKeySuccess(0)
	if pool.entries[0].consecutiveRateLimit != 0 {
		t.Errorf("expected count reset to 0 after markKeySuccess, got %d", pool.entries[0].consecutiveRateLimit)
	}
	// 重置后再次限流应回到 5s
	pool.entries[0].exhausted = false
	pool.markRateLimited(0)
	got := pool.entries[0].cooldownEnd.Sub(time.Now())
	if got < 4*time.Second || got > 6*time.Second {
		t.Errorf("after reset, expected first cooldown ~5s again, got %v (count=%d)", got, pool.entries[0].consecutiveRateLimit)
	}
}

// TestMarkRateLimited 验证限流标记和冷却恢复。
func TestMarkRateLimited(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1"})

	// 标记 key-0 为限流
	pool.markRateLimited(0)

	// key-0 应该被标记为 exhausted
	if !pool.entries[0].exhausted {
		t.Error("expected key-0 to be marked exhausted")
	}
	if pool.entries[0].exhaustReason != "rate_limited" {
		t.Errorf("expected exhaustReason 'rate_limited', got %q", pool.entries[0].exhaustReason)
	}

	// 首次限流冷却应为 5s（指数退避序列起点：5→10→20→40→60）
	expectedCooldown := time.Now().Add(rateLimitedBaseCooldown)
	diff := pool.entries[0].cooldownEnd.Sub(expectedCooldown)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("expected first cooldown around %v, got %v", rateLimitedBaseCooldown, pool.entries[0].cooldownEnd.Sub(time.Now()))
	}

	// selectKeyForClient 应该返回 key-1（key-0 被限流）
	_, idx := pool.selectKeyForClient("alice")
	if idx != 1 {
		t.Errorf("expected key-1 (key-0 rate limited), got key-%d", idx)
	}

	// 等待冷却过期
	time.Sleep(50 * time.Millisecond)
	// 手动设置冷却时间为过去（加速测试）
	pool.mu.Lock()
	pool.entries[0].cooldownEnd = time.Now().Add(-time.Second)
	pool.mu.Unlock()

	// 冷却过期后应该能恢复
	key, idx := pool.selectKey()
	if key == "" || idx == -1 {
		t.Error("expected key to recover after cooldown")
	}
}

// TestSelectNextActiveKey 验证切换到下一个 active key。
func TestSelectNextActiveKey(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})

	// 从 key-0 之后找，应该返回 key-1
	_, idx := pool.selectNextActiveKey(0)
	if idx != 1 {
		t.Errorf("expected key-1 after key-0, got key-%d", idx)
	}

	// 标记 key-1 为 exhausted
	pool.markExhausted(1, "quota_exceeded")

	// 从 key-0 之后找，应该跳过 key-1 返回 key-2
	_, idx = pool.selectNextActiveKey(0)
	if idx != 2 {
		t.Errorf("expected key-2 (skip exhausted key-1), got key-%d", idx)
	}

	// 标记 key-2 也为 exhausted
	pool.markExhausted(2, "quota_exceeded")

	// 从 key-0 之后找，应该绕回 key-0
	_, idx = pool.selectNextActiveKey(0)
	if idx != 0 {
		t.Errorf("expected key-0 (wrap around), got key-%d", idx)
	}

	// 全部 exhausted
	pool.markExhausted(0, "quota_exceeded")

	// 应该返回 -1（无 active key）
	_, idx = pool.selectNextActiveKey(0)
	if idx != -1 {
		t.Errorf("expected -1 (no active key), got %d", idx)
	}
}

// TestPeriodicResetTime 验证周期性 resetTime（monthly:23 / 23）。
func TestPeriodicResetTime(t *testing.T) {
	// 测试 monthly:23 格式
	pool := newKeyPool("test", []string{"key-a"}, "monthly:23", slog.Default())
	if pool.resetTime.IsZero() {
		t.Error("expected resetTime to be set for monthly:23")
	}
	// 验证日期是 23 号
	if pool.resetTime.Day() != 23 {
		t.Errorf("expected day 23, got %d", pool.resetTime.Day())
	}
	poolDayOnly := newKeyPool("test", []string{"key-a"}, "23", slog.Default())
	if poolDayOnly.resetTime.IsZero() {
		t.Error("expected resetTime to be set for day-only 23")
	}
	if poolDayOnly.resetTime.Day() != 23 {
		t.Errorf("expected day 23 for day-only input, got %d", poolDayOnly.resetTime.Day())
	}

	// 测试无效格式
	pool2 := newKeyPool("test", []string{"key-a"}, "monthly:32", slog.Default())
	if !pool2.resetTime.IsZero() {
		t.Error("expected resetTime to be zero for invalid monthly:32")
	}

	pool3 := newKeyPool("test", []string{"key-a"}, "monthly:abc", slog.Default())
	if !pool3.resetTime.IsZero() {
		t.Error("expected resetTime to be zero for invalid monthly:abc")
	}
}

func TestMonthlyResetTimeRecomputesNextCycle(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, time.May, 30, 4, 0, 0, 0, cst)
	next := parseResetTimeSpec("test", "23", slog.Default(), now)
	want := time.Date(2026, time.June, 23, 0, 0, 0, 0, cst)
	if !next.Equal(want) {
		t.Fatalf("expected next monthly reset %v, got %v", want, next)
	}
}

// TestDesperationProbe 验证绝境探测机制。
func TestDesperationProbe(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})

	// 标记所有 key 为 exhausted，但冷却时间不同
	now := time.Now()
	pool.mu.Lock()
	pool.entries[0].exhausted = true
	pool.entries[0].exhaustReason = "quota_exceeded"
	pool.entries[0].cooldownEnd = now.Add(5 * time.Minute) // 5 分钟后

	pool.entries[1].exhausted = true
	pool.entries[1].exhaustReason = "quota_exceeded"
	pool.entries[1].cooldownEnd = now.Add(1 * time.Minute) // 1 分钟后（最短）

	pool.entries[2].exhausted = true
	pool.entries[2].exhaustReason = "quota_exceeded"
	pool.entries[2].cooldownEnd = now.Add(10 * time.Minute) // 10 分钟后
	pool.mu.Unlock()

	// selectKey 应该返回冷却最短的 key-1 做绝境探测
	_, idx := pool.selectKey()
	if idx != 1 {
		t.Errorf("expected desperation probe to select key-1 (shortest cooldown), got key-%d", idx)
	}

	// 模拟绝境探测成功，标记 key-1 为已恢复
	pool.markRecovered(1)

	// key-1 应该恢复为 active
	if pool.entries[1].exhausted {
		t.Error("expected key-1 to be recovered after markRecovered")
	}

	// 现在 selectKey 应该返回 key-1（唯一的 active key）
	_, idx = pool.selectKey()
	if idx != 1 {
		t.Errorf("expected key-1 after recovery, got key-%d", idx)
	}
}

// TestDesperationProbe_SkipRateLimited 验证绝境探测跳过 rate_limited key。
func TestDesperationProbe_SkipRateLimited(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1"})

	now := time.Now()
	pool.mu.Lock()
	// key-0 是 rate_limited（应该被跳过）
	pool.entries[0].exhausted = true
	pool.entries[0].exhaustReason = "rate_limited"
	pool.entries[0].cooldownEnd = now.Add(30 * time.Second)

	// key-1 是 quota_exceeded
	pool.entries[1].exhausted = true
	pool.entries[1].exhaustReason = "quota_exceeded"
	pool.entries[1].cooldownEnd = now.Add(5 * time.Minute)
	pool.mu.Unlock()

	// 绝境探测应该跳过 rate_limited 的 key-0，选 key-1
	_, idx := pool.selectKey()
	if idx != 1 {
		t.Errorf("expected desperation probe to skip rate_limited key-0 and select key-1, got key-%d", idx)
	}
}

// TestMarkRecovered 验证 markRecovered 方法。
func TestMarkRecovered(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0"})

	// 标记为 exhausted
	pool.markExhausted(0, "quota_exceeded")
	if !pool.entries[0].exhausted {
		t.Error("expected key-0 to be exhausted")
	}

	// 标记为已恢复
	if !pool.markRecovered(0) {
		t.Error("expected markRecovered to return true when recovering an exhausted key")
	}
	if pool.entries[0].exhausted {
		t.Error("expected key-0 to be recovered")
	}
	if pool.entries[0].exhaustReason != "" {
		t.Errorf("expected exhaustReason to be cleared, got %q", pool.entries[0].exhaustReason)
	}

	// 对已经 active 的 key 调用 markRecovered 应该是 no-op 且返回 false
	if pool.markRecovered(0) {
		t.Error("expected markRecovered to return false for an already-active key")
	}
	if pool.entries[0].exhausted {
		t.Error("expected key-0 to remain active")
	}

	// 对仅手动 blocked（未 exhausted）的 key 调用应返回 false，且不清除 blocked 标记
	pool.blockKey(0)
	if pool.markRecovered(0) {
		t.Error("expected markRecovered to return false for a manually-blocked key")
	}
	if !pool.entries[0].blocked {
		t.Error("expected manual block to be preserved after markRecovered")
	}
}

func TestBlockKey(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1"})

	// 屏蔽 key-0
	pool.blockKey(0)
	if !pool.entries[0].blocked {
		t.Error("expected key-0 to be blocked")
	}

	// selectKeyForClient 应该跳过 blocked key
	key, idx := pool.selectKeyForClient("client1")
	if idx == 0 {
		t.Error("expected to skip blocked key-0")
	}
	if key != "key-1" {
		t.Errorf("expected key-1, got %s", key)
	}

	// 边界条件：nil pool
	var nilPool *keyPool
	nilPool.blockKey(0) // should not panic

	// 边界条件：越界索引
	pool.blockKey(-1)
	pool.blockKey(100)
}

func TestUnblockKey(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0"})

	// 先屏蔽
	pool.blockKey(0)
	if !pool.entries[0].blocked {
		t.Error("expected key-0 to be blocked")
	}

	// 解除屏蔽
	pool.unblockKey(0)
	if pool.entries[0].blocked {
		t.Error("expected key-0 to be unblocked")
	}
	if pool.entries[0].exhausted {
		t.Error("expected exhausted to be cleared")
	}

	// 边界条件：nil pool
	var nilPool *keyPool
	nilPool.unblockKey(0) // should not panic

	// 边界条件：越界索引
	pool.unblockKey(-1)
	pool.unblockKey(100)
}

func TestKeyPoolStatus(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})

	// 初始状态
	statuses := pool.status()
	if len(statuses) != 3 {
		t.Fatalf("expected 3 statuses, got %d", len(statuses))
	}

	// 第一个 key 应该是"使用中"
	if statuses[0].Status != "使用中" {
		t.Errorf("expected first key status to be '使用中', got %q", statuses[0].Status)
	}

	// 其他 key 应该是"等待中"
	if statuses[1].Status != "等待中" {
		t.Errorf("expected second key status to be '等待中', got %q", statuses[1].Status)
	}

	// 屏蔽一个 key
	pool.blockKey(1)
	statuses = pool.status()
	if statuses[1].Status != "已屏蔽" {
		t.Errorf("expected blocked key status to be '已屏蔽', got %q", statuses[1].Status)
	}

	// 标记一个 key 为额度超限（无 resetTime → quota_exceeded_api → 人工恢复）
	pool.markExhausted(2, "quota_exceeded")
	statuses = pool.status()
	if statuses[2].Status != "额度超限(人工恢复)" {
		t.Errorf("expected exhausted key status to be '额度超限(人工恢复)', got %q", statuses[2].Status)
	}

	// 标记一个 key 为限流中
	pool.markRateLimited(0)
	statuses = pool.status()
	if statuses[0].Status != "限流中" {
		t.Errorf("expected rate limited key status to be '限流中', got %q", statuses[0].Status)
	}

	// 边界条件：nil pool
	var nilPool *keyPool
	if nilPool.status() != nil {
		t.Error("expected nil pool to return nil status")
	}
}

func TestMarkExhausted_QuotaExceeded_WithResetTime(t *testing.T) {
	// 设置一个未来的 resetTime
	cst := time.FixedZone("CST", 8*3600)
	futureReset := time.Now().In(cst).AddDate(0, 0, 7) // 7天后
	resetStr := futureReset.Format("2006-01-02 15:04")

	pool := newKeyPool("test", []string{"key-a"}, resetStr, slog.Default())
	pool.markExhausted(0, "quota_exceeded")

	// cooldownEnd 应该等于 resetTime
	if !pool.entries[0].cooldownEnd.Equal(pool.resetTime) {
		t.Errorf("quota_exceeded with future resetTime should use resetTime, got %v want %v", pool.entries[0].cooldownEnd, pool.resetTime)
	}
	// exhaustReason 应该是 quota_exceeded_subscription
	if pool.entries[0].exhaustReason != "quota_exceeded_subscription" {
		t.Errorf("expected exhaustReason 'quota_exceeded_subscription', got %q", pool.entries[0].exhaustReason)
	}
}

func TestMarkExhausted_QuotaExceeded_NoResetTime(t *testing.T) {
	pool := newTestKeyPool([]string{"key-a"})
	pool.markExhausted(0, "quota_exceeded")

	// 没有 resetTime，应该永久屏蔽（cooldownEnd 在远未来）
	if pool.entries[0].cooldownEnd.Year() < 9000 {
		t.Errorf("quota_exceeded without resetTime should be permanent, got %v", pool.entries[0].cooldownEnd)
	}
	// exhaustReason 应该是 quota_exceeded_api
	if pool.entries[0].exhaustReason != "quota_exceeded_api" {
		t.Errorf("expected exhaustReason 'quota_exceeded_api', got %q", pool.entries[0].exhaustReason)
	}
}

// TestQuotaExceeded_StatusDisplay 验证两种超额子类型的状态显示。
func TestQuotaExceeded_StatusDisplay(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	futureReset := time.Now().In(cst).AddDate(0, 0, 7)
	resetStr := futureReset.Format("2006-01-02 15:04")

	pool := newKeyPool("test", []string{"key-sub", "key-api"}, resetStr, slog.Default())

	// key-0: 有 resetTime → quota_exceeded_subscription → "额度超限(自动恢复)"
	pool.markExhausted(0, "quota_exceeded")
	// key-1: 手动设置为无 resetTime 的情况（模拟 API 额度耗尽）
	pool.entries[1].exhausted = true
	pool.entries[1].exhaustReason = "quota_exceeded_api"
	pool.entries[1].cooldownEnd = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

	statuses := pool.status()

	if statuses[0].Status != "额度超限(自动恢复)" {
		t.Errorf("expected key-0 status '额度超限(自动恢复)', got %q", statuses[0].Status)
	}
	if statuses[1].Status != "额度超限(人工恢复)" {
		t.Errorf("expected key-1 status '额度超限(人工恢复)', got %q", statuses[1].Status)
	}
}

// TestKeyPoolStatus_GLMDisplay 验证 GLM 各类限流 reason 的状态显示文案，
// 以及冷却已过期时 recoverable/hard-failure 两类 reason 的区分。
// 关键约束（archgate）：status() 不得改写字段值、不得调用 recoverFromCooldownLocked。
func TestKeyPoolStatus_GLMDisplay(t *testing.T) {
	pool := newTestKeyPool([]string{"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9", "k10", "k11", "k12"})
	now := time.Now()

	cases := []struct {
		idx         int
		reason      string
		cooldownEnd time.Time
		want        string
		desc        string
	}{
		// GLM QPS 限流
		{0, glmRateLimitQPS, now.Add(60 * time.Second), "GLM速率限流", "GLM QPS 未过期"},
		{1, glmRateLimitQPS, now.Add(-60 * time.Second), "可用(待激活)", "GLM QPS 已过期→真恢复"},
		// GLM 额度限流
		{2, glmRateLimitQuota, now.Add(7 * 24 * time.Hour), "GLM额度限流", "GLM 周额度 未过期"},
		{3, glmRateLimitQuota, now.Add(-1 * time.Hour), "可用(待激活)", "GLM 周额度 已过期→真恢复"},
		{4, glmExhaustReason, now.Add(-1 * time.Hour), "可用(待激活)", "GLM 5h 已过期→真恢复"},
		// 凭证/账户类硬失败
		{5, "invalid_token", now.Add(5 * time.Minute), "失效(冷却中)", "invalid_token 未过期"},
		{6, "invalid_token", now.Add(-1 * time.Minute), "失效(待请求验证)", "invalid_token 已过期→不落可用(关键)"},
		{7, "billing_error", now.Add(-1 * time.Minute), "失效(待请求验证)", "billing_error 已过期→待验证"},
		{8, "account_disabled", now.Add(-1 * time.Minute), "失效(待请求验证)", "account_disabled 已过期→待验证"},
		// 额度类
		{9, "quota_exceeded_api", time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "额度超限(人工恢复)", "quota_exceeded_api permanent→不落已过期"},
		{10, "quota_exceeded_subscription", now.Add(7 * 24 * time.Hour), "额度超限(自动恢复)", "quota_sub 未过期"},
		// rate_limited 保留原行为
		{11, "rate_limited", now.Add(5 * time.Second), "限流中", "rate_limited 未过期"},
		// 零值 cooldownEnd：走 reason 文案，不落已过期分支
		{12, "rate_limited", time.Time{}, "限流中", "零值 cooldownEnd→走 reason 文案"},
	}

	for _, c := range cases {
		pool.markExhaustedWithCooldown(c.idx, c.reason, c.cooldownEnd, "test body")
		// markExhaustedWithCooldown 幂等：若已被 markExhausted 标记过会跳过；
		// 这里直接构造场景，确保字段正确
		pool.entries[c.idx].exhausted = true
		pool.entries[c.idx].exhaustReason = c.reason
		pool.entries[c.idx].cooldownEnd = c.cooldownEnd
	}

	statuses := pool.status()
	for _, c := range cases {
		got := statuses[c.idx].Status
		if got != c.want {
			t.Errorf("idx=%d (%s): reason=%q cd=%v\n  got  %q\n  want %q",
				c.idx, c.desc, c.reason, c.cooldownEnd, got, c.want)
		}
		// 同时验证字段未被 status() 改写（archgate 只读契约）
		if !pool.entries[c.idx].exhausted {
			t.Errorf("idx=%d: exhausted 字段被 status() 改写为 false", c.idx)
		}
		if pool.entries[c.idx].exhaustReason != c.reason {
			t.Errorf("idx=%d: exhaustReason 被改写为 %q", c.idx, pool.entries[c.idx].exhaustReason)
		}
		if !pool.entries[c.idx].cooldownEnd.Equal(c.cooldownEnd) {
			t.Errorf("idx=%d: cooldownEnd 被改写为 %v", c.idx, pool.entries[c.idx].cooldownEnd)
		}
	}
}

// TestIsRecoverableAfterCooldown 验证谓词独立性：与 isHardFailureReason/isAccountLevelExhaustion
// 分类语义不同（recoverable 子集 ≠ hard-failure 子集）。
func TestIsRecoverableAfterCooldown(t *testing.T) {
	recoverable := []string{
		"rate_limited",
		glmRateLimitQPS,
		glmExhaustReason,
		glmRateLimitQuota,
		glmRateLimitUnknown,
		"quota_exceeded_subscription",
	}
	notRecoverable := []string{
		"invalid_token",
		"billing_error",
		"account_disabled",
		"quota_exceeded_api",
		"Arrearage",
		"free_tier_exhausted",
		"AccessDenied.Unpurchased",
		"",            // 空
		"unknown_xyz", // 未知
	}
	for _, r := range recoverable {
		if !isRecoverableAfterCooldown(r) {
			t.Errorf("isRecoverableAfterCooldown(%q) = false, want true", r)
		}
	}
	for _, r := range notRecoverable {
		if isRecoverableAfterCooldown(r) {
			t.Errorf("isRecoverableAfterCooldown(%q) = true, want false", r)
		}
	}
}

// TestKeyStatusLabelForReason 验证未过期文案映射覆盖 spec 矩阵。
func TestKeyStatusLabelForReason(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{"quota_exceeded_subscription", "额度超限(自动恢复)"},
		{"quota_exceeded_api", "额度超限(人工恢复)"},
		{"quota_exceeded", "额度超限"},
		{"rate_limited", "限流中"},
		{glmRateLimitQPS, "GLM速率限流"},
		{glmRateLimitOverload, "GLM服务过载"},
		{glmExhaustReason, "GLM额度限流"},
		{glmRateLimitQuota, "GLM额度限流"},
		{glmRateLimitUnknown, "GLM额度限流"},
		{"invalid_token", "失效(冷却中)"},
		{"billing_error", "失效(冷却中)"},
		{"account_disabled", "失效(冷却中)"},
		{"Arrearage", "失效"},
		{"", "失效"},
		{"unknown_xyz", "失效"},
	}
	for _, c := range cases {
		got := keyStatusLabelForReason(c.reason)
		if got != c.want {
			t.Errorf("keyStatusLabelForReason(%q) = %q, want %q", c.reason, got, c.want)
		}
	}
}

func TestSelectKey_CooldownEnd(t *testing.T) {
	// 用 quota_exceeded + 近未来 resetTime 测试冷却恢复
	cst := time.FixedZone("CST", 8*3600)
	futureReset := time.Now().In(cst).Add(1 * time.Hour)
	resetStr := futureReset.Format("2006-01-02 15:04")

	pool := newKeyPool("test-target", []string{"key-a", "key-b"}, resetStr, slog.Default())
	pool.markExhausted(0, "quota_exceeded")
	pool.markExhausted(1, "quota_exceeded")

	// 全部在冷却中：绝境探测应该返回冷却最短的 key（而不是空）
	key, idx := pool.selectKey()
	if key == "" || idx == -1 {
		t.Errorf("desperation probe should return a key even when all in cooldown, got (%s, %d)", key, idx)
	}

	// 手动设置 cooldownEnd 为过去
	pool.entries[0].cooldownEnd = time.Now().Add(-1 * time.Second)

	key, idx = pool.selectKey()
	if key != "key-a" || idx != 0 {
		t.Errorf("should recover key-a after cooldown, got (%s, %d)", key, idx)
	}
}

func TestHashClientKey_Deterministic(t *testing.T) {
	h1 := hashClientKey("alice", "target-a")
	h2 := hashClientKey("alice", "target-a")
	if h1 != h2 {
		t.Errorf("hash should be deterministic: %d != %d", h1, h2)
	}

	// 不同 target 应该产生不同 hash（避免跨 target 碰撞）
	h3 := hashClientKey("alice", "target-b")
	if h1 == h3 {
		t.Errorf("different targets should produce different hashes")
	}
}

func TestTargetKeyExhaustedOfficialErrors(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		adapter    string
		protocol   config.ProtocolType
		statusCode int
		body       string
		wantExh    bool
		wantCode   string
	}{
		// ── 2xx 不算 ──
		{"200 OK", "api.openai.com", "standard", config.ProtocolOpenAIChat, 200, `{"ok":true}`, false, ""},

		// ── 429 限流：不标记 key 耗尽（临时限速，key 没问题）──
		// 百炼: Throttling / Throttling.RateQuota / Throttling.BurstRate
		{"百炼 429 Throttling 纯限流", "dashscope.aliyuncs.com", "qwen", config.ProtocolOpenAIChat, 429, `{"code":"Throttling","message":"Requests throttling triggered."}`, false, ""},
		{"百炼 429 Throttling.RateQuota", "dashscope.aliyuncs.com", "qwen", config.ProtocolOpenAIChat, 429, `{"code":"Throttling.RateQuota","message":"You have exceeded your request limit."}`, false, ""},
		{"百炼 429 Throttling.BurstRate", "dashscope.aliyuncs.com", "qwen", config.ProtocolOpenAIChat, 429, `{"code":"Throttling.BurstRate","message":"Request rate increased too quickly."}`, false, ""},
		// OpenAI: "Rate limit reached for requests"
		{"OpenAI 429 rate limit", "api.openai.com", "standard", config.ProtocolOpenAIChat, 429, `{"error":{"message":"Rate limit reached for requests","type":"rate_limit_error"}}`, false, ""},
		// Claude: rate_limit_error
		{"Claude 429 rate_limit_error", "api.anthropic.com", "standard", config.ProtocolAnthropicMessages, 429, `{"type":"error","error":{"type":"rate_limit_error","message":"Rate limit exceeded."}}`, false, ""},
		// DeepSeek: "Rate Limit Reached"
		{"DeepSeek 429 rate limit", "api.deepseek.com", "deepseek", config.ProtocolOpenAIChat, 429, `{"error":{"message":"Rate Limit Reached","type":"rate_limit_reached"}}`, false, ""},

		// ── 百炼 (alibabacloud.com/help/en/model-studio/error-code) ──
		// 401 InvalidApiKey
		{"百炼 401 InvalidApiKey", "dashscope.aliyuncs.com", "qwen", config.ProtocolOpenAIChat, 401, `{"code":"InvalidApiKey","message":"Invalid API-key provided."}`, true, "invalid_token"},
		// 400 Arrearage
		{"百炼 400 Arrearage", "dashscope.aliyuncs.com", "qwen", config.ProtocolOpenAIChat, 400, `{"code":"Arrearage","message":"Access denied, please make sure your account is in good standing."}`, true, "Arrearage"},
		// 403 AccessDenied.Unpurchased
		{"百炼 403 AccessDenied.Unpurchased", "dashscope.aliyuncs.com", "qwen", config.ProtocolOpenAIChat, 403, `{"code":"AccessDenied.Unpurchased","message":"Access to model denied. Please make sure you are eligible for using the model."}`, true, "AccessDenied.Unpurchased"},
		// 403 AllocationQuota.FreeTierOnly
		{"百炼 403 FreeTierOnly", "dashscope.aliyuncs.com", "qwen", config.ProtocolOpenAIChat, 403, `{"code":"AllocationQuota.FreeTierOnly","message":"The free tier of the model has been exhausted."}`, true, "free_tier_exhausted"},
		// 429 Throttling.AllocationQuota — TPM 限流（不是真正配额耗尽，不标记 key）
		{"百炼 429 Throttling.AllocationQuota", "dashscope.aliyuncs.com", "qwen", config.ProtocolOpenAIChat, 429, `{"code":"Throttling.AllocationQuota","message":"Allocated quota exceeded, please increase your quota limit."}`, false, ""},
		// 429 PrepaidBillOverdue
		{"百炼 429 PrepaidBillOverdue", "dashscope.aliyuncs.com", "qwen", config.ProtocolOpenAIChat, 429, `{"code":"PrepaidBillOverdue","message":"The prepaid bill is overdue."}`, true, "quota_exceeded"},
		// 429 PostpaidBillOverdue
		{"百炼 429 PostpaidBillOverdue", "dashscope.aliyuncs.com", "qwen", config.ProtocolOpenAIChat, 429, `{"code":"PostpaidBillOverdue","message":"The postpaid bill is overdue."}`, true, "quota_exceeded"},
		// 429 CommodityNotPurchased
		{"百炼 429 CommodityNotPurchased", "dashscope.aliyuncs.com", "qwen", config.ProtocolOpenAIChat, 429, `{"code":"CommodityNotPurchased","message":"Commodity has not purchased yet."}`, true, "quota_exceeded"},
		// 429 TokenPlan TPM 限流（"upgrade your API plan" 是限流建议，不是真正额度耗尽）
		{"百炼 429 TokenPlan TPM rate limit", "dashscope.aliyuncs.com", "qwen", config.ProtocolOpenAIChat, 429, "event:error\ndata:{\"request_id\":\"abc\",\"code\":\"Throttling\",\"message\":\"Rate limit exceeded. Please wait and try again, or upgrade your API plan.\"}", false, ""},
		// 模糊 insufficient_quota 不凭 message 推断财务耗尽。
		{"百炼 429 TokenPlan quota has been exhausted", "dashscope.aliyuncs.com", "qwen", config.ProtocolOpenAIChat, 429, `{"error":{"message":"Your token-plan quota has been exhausted.","id":"abc","type":"insufficient_quota","code":"insufficient_quota"}}`, false, ""},

		// ── OpenAI (developers.openai.com/api/docs/guides/error-codes) ──
		// 401 "Incorrect API key provided"
		{"OpenAI 401 incorrect key", "api.openai.com", "standard", config.ProtocolOpenAIChat, 401, `{"error":{"message":"Incorrect API key provided: sk-***...***","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}`, true, "invalid_token"},
		// 旧兼容响应缺少明确财务 code，保守保持临时失败。
		{"OpenAI 429 quota exceeded", "api.openai.com", "standard", config.ProtocolOpenAIChat, 429, `{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}`, false, ""},

		// ── Azure OpenAI ──
		// 401 "Access denied due to invalid subscription key"
		{"Azure 401 invalid subscription key", "example.openai.azure.com", "standard", config.ProtocolOpenAIChat, 401, `{"error":{"code":"401","message":"Access denied due to invalid subscription key or wrong API endpoint."}}`, true, "invalid_token"},

		// ── Claude (platform.claude.com/docs/en/api/errors) ──
		// 401 authentication_error: "invalid x-api-key"
		{"Claude 401 authentication_error", "api.anthropic.com", "standard", config.ProtocolAnthropicMessages, 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, true, "invalid_token"},
		// 402 billing_error
		{"Claude 402 billing_error", "api.anthropic.com", "standard", config.ProtocolAnthropicMessages, 402, `{"type":"error","error":{"type":"billing_error","message":"Your credit balance is too low to access the Anthropic API."}}`, true, "billing_error"},

		// ── Gemini (ai.google.dev/gemini-api/docs/troubleshooting) ──
		// 400 INVALID_ARGUMENT: "API key not valid"
		{"Gemini 400 API_KEY_INVALID", "generativelanguage.googleapis.com", "standard", config.ProtocolGemini, 400, `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT"}}`, true, "invalid_token"},
		// 429 RESOURCE_EXHAUSTED
		{"Gemini 429 RESOURCE_EXHAUSTED", "generativelanguage.googleapis.com", "standard", config.ProtocolGemini, 429, `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`, false, ""},

		// ── DeepSeek (api-docs.deepseek.com/quick_start/error_codes) ──
		// 401 Authentication Fails
		{"DeepSeek 401 auth fails", "api.deepseek.com", "deepseek", config.ProtocolOpenAIChat, 401, `{"error":{"message":"Authentication Fails, Your api key: **** is invalid","type":"authentication_error","param":"","code":"invalid_api_key"}}`, true, "invalid_token"},
		// 402 Insufficient Balance
		{"DeepSeek 402 insufficient balance", "api.deepseek.com", "deepseek", config.ProtocolOpenAIChat, 402, `{"error":{"message":"Insufficient Balance","type":"insufficient_balance","param":"","code":"insufficient_balance"}}`, true, "billing_error"},

		// ── 不匹配的错误 ──
		{"500 internal error", "api.openai.com", "standard", config.ProtocolOpenAIChat, 500, `{"error":"internal server error"}`, false, ""},
		{"403 forbidden", "api.openai.com", "standard", config.ProtocolOpenAIChat, 403, `{"error":"forbidden"}`, false, ""},
		{"503 overloaded", "api.openai.com", "standard", config.ProtocolOpenAIChat, 503, `{"error":"The engine is currently overloaded, please try again later"}`, false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint, err := url.Parse("https://" + tt.host)
			if err != nil {
				t.Fatal(err)
			}
			gotExh, gotCode := targetKeyExhausted(&Target{Endpoint: endpoint, UsageAdapter: tt.adapter}, tt.protocol, tt.statusCode, []byte(tt.body))
			if gotExh != tt.wantExh {
				t.Errorf("targetKeyExhausted() exhausted = %v, want %v (body: %s)", gotExh, tt.wantExh, tt.body)
			}
			if gotCode != tt.wantCode {
				t.Errorf("targetKeyExhausted() code = %q, want %q", gotCode, tt.wantCode)
			}
		})
	}
}

// TestInvalidToken_5MinuteCooldown 验证 invalid_token 冷却时间为5分钟（非永久屏蔽）。
func TestInvalidToken_5MinuteCooldown(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1"})

	// 标记 key-0 为 invalid_token
	pool.markExhausted(0, "invalid_token")

	// key-0 应该被标记为 exhausted
	if !pool.entries[0].exhausted {
		t.Error("expected key-0 to be marked exhausted")
	}
	if pool.entries[0].exhaustReason != "invalid_token" {
		t.Errorf("expected exhaustReason 'invalid_token', got %q", pool.entries[0].exhaustReason)
	}

	// 冷却时间应该是 5 分钟后（非永久）
	expectedCooldown := time.Now().Add(5 * time.Minute)
	diff := pool.entries[0].cooldownEnd.Sub(expectedCooldown)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("expected cooldown around 5 minutes, got %v", pool.entries[0].cooldownEnd.Sub(time.Now()))
	}

	// 冷却时间不应该是永久（9999年）
	permanentEnd := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if pool.entries[0].cooldownEnd.Equal(permanentEnd) {
		t.Error("invalid_token should not use permanent cooldown")
	}
}

// TestBillingError_5MinuteCooldown 验证 billing_error 冷却时间为5分钟。
func TestBillingError_5MinuteCooldown(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0"})

	pool.markExhausted(0, "billing_error")

	expectedCooldown := time.Now().Add(5 * time.Minute)
	diff := pool.entries[0].cooldownEnd.Sub(expectedCooldown)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("expected cooldown around 5 minutes, got %v", pool.entries[0].cooldownEnd.Sub(time.Now()))
	}
}

// TestWakeUp_Singleton 验证唤醒模型单例化（并发调用只执行一次）。
func TestWakeUp_Singleton(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1"})

	// 标记所有 key 为 exhausted
	pool.markExhausted(0, "invalid_token")
	pool.markExhausted(1, "invalid_token")

	now := time.Now()

	// 第一次调用应该成功
	if !pool.tryWakeUp(now) {
		t.Error("first tryWakeUp should succeed")
	}

	// 第二次调用应该失败（已有协程在执行）
	if pool.tryWakeUp(now) {
		t.Error("second tryWakeUp should fail (singleton)")
	}

	// 完成唤醒
	pool.wakeUpComplete()

	// 冷却期内再次调用应该失败
	if pool.tryWakeUp(now.Add(30 * time.Second)) {
		t.Error("tryWakeUp within cooldown should fail")
	}

	// 冷却期后再次调用应该成功
	if !pool.tryWakeUp(now.Add(2 * time.Minute)) {
		t.Error("tryWakeUp after cooldown should succeed")
	}
	pool.wakeUpComplete()
}

// TestWakeUp_Cooldown 验证唤醒冷却（1分钟内不重复触发）。
func TestWakeUp_Cooldown(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0"})

	now := time.Now()

	// 第一次调用应该成功
	if !pool.tryWakeUp(now) {
		t.Error("first tryWakeUp should succeed")
	}
	pool.wakeUpComplete()

	// 冷却期内（30秒后）应该失败
	if pool.tryWakeUp(now.Add(30 * time.Second)) {
		t.Error("tryWakeUp within 1 minute cooldown should fail")
	}

	// 冷却期后（2分钟后）应该成功
	if !pool.tryWakeUp(now.Add(2 * time.Minute)) {
		t.Error("tryWakeUp after 1 minute cooldown should succeed")
	}
	pool.wakeUpComplete()
}

// TestGetExhaustedKeys 验证获取被屏蔽的 key 列表（按冷却时间排序）。
func TestGetExhaustedKeys(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})

	// 标记 key-0 为 invalid_token（5分钟冷却）
	pool.markExhausted(0, "invalid_token")
	// 标记 key-1 为 rate_limited（5秒冷却）
	pool.markRateLimited(1)
	// key-2 保持 active

	exhausted := pool.getExhaustedKeys()

	// 应该返回 2 个 key（key-0 和 key-1）
	if len(exhausted) != 2 {
		t.Errorf("expected 2 exhausted keys, got %d", len(exhausted))
	}

	// 应该按冷却时间排序：key-1（5秒）在前，key-0（5分钟）在后
	if exhausted[0] != 1 {
		t.Errorf("expected first exhausted key to be key-1 (shorter cooldown), got key-%d", exhausted[0])
	}
	if exhausted[1] != 0 {
		t.Errorf("expected second exhausted key to be key-0 (longer cooldown), got key-%d", exhausted[1])
	}
}

// ---- #1~#5 负载优化针对性单测 ----

// TestActiveKeyCount 验证 #1：activeKeyCount 只统计未耗尽/未屏蔽的 key。
func TestActiveKeyCount(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2", "key-3"})
	if got := pool.activeKeyCount(); got != 4 {
		t.Fatalf("fresh pool: expected 4 active, got %d", got)
	}
	pool.markExhausted(0, "quota_exceeded")
	pool.blockKey(1)
	if got := pool.activeKeyCount(); got != 2 {
		t.Errorf("after 1 exhausted + 1 blocked: expected 2 active, got %d", got)
	}
	// 冷却恢复后应回升
	pool.mu.Lock()
	pool.entries[0].exhausted = false
	pool.mu.Unlock()
	if got := pool.activeKeyCount(); got != 3 {
		t.Errorf("after recovery: expected 3 active, got %d", got)
	}
	var nilPool *keyPool
	if got := nilPool.activeKeyCount(); got != 0 {
		t.Errorf("nil pool: expected 0, got %d", got)
	}
}

// TestAffinityRebalance 验证 #2：绑定超过 rebalanceInterval 后，存在更优 key 时迁移绑定。
func TestAffinityRebalance(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1"})
	pool.rebalanceInterval = 0 // 立即允许再平衡

	// 让 key-0 先被绑定（给 key-0 较高 token 使新客户端走负载感知到 key-1? 这里手动绑定）
	now := time.Now()
	pool.mu.Lock()
	pool.bindLocked("alice", 0, now.Add(-time.Hour)) // 很久以前绑定到 key-0
	pool.mu.Unlock()

	// 制造 key-0 负载高于 key-1：给 key-0 记 token
	pool.recordTokens(0, 50000)

	// 再次选择：应再平衡到更空闲的 key-1
	_, idx := pool.selectKeyForClient("alice")
	if idx != 1 {
		t.Errorf("expected rebalance to key-1 (less loaded), got key-%d", idx)
	}

	// 反向：没有更优 key 时不应抖动
	pool2 := newTestKeyPool([]string{"key-0", "key-1"})
	pool2.rebalanceInterval = 0
	pool2.mu.Lock()
	pool2.bindLocked("bob", 0, time.Now().Add(-time.Hour))
	pool2.mu.Unlock()
	pool2.recordTokens(1, 50000) // key-1 更忙，key-0 仍最优
	_, idx2 := pool2.selectKeyForClient("bob")
	if idx2 != 0 {
		t.Errorf("expected to stay on key-0 (still best), got key-%d", idx2)
	}
}

// TestAffinityNoRebalanceBeforeInterval 验证 #2：未到再平衡间隔时不迁移。
func TestAffinityNoRebalanceBeforeInterval(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1"})
	pool.rebalanceInterval = time.Hour // 几乎不触发
	now := time.Now()
	pool.mu.Lock()
	pool.bindLocked("alice", 0, now)
	pool.mu.Unlock()
	pool.recordTokens(0, 50000) // key-0 更忙，但还没到再平衡时间
	_, idx := pool.selectKeyForClient("alice")
	if idx != 0 {
		t.Errorf("expected to stay on bound key-0 before interval, got key-%d", idx)
	}
}

// TestInFlightScheduling 验证 #3：在途计数纳入调度，least-connections 优先。
func TestInFlightScheduling(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})
	// key-0 有 2 个在途，key-1 有 1 个，key-2 空闲
	pool.acquireInFlight(0)
	pool.acquireInFlight(0)
	pool.acquireInFlight(1)
	// 新客户端应选 key-2（在途最少）
	_, idx := pool.selectKeyForClient("alice")
	if idx != 2 {
		t.Errorf("expected key-2 (least in-flight), got key-%d", idx)
	}
	// 释放保护：重复释放不应使计数变负
	pool.releaseInFlight(2)
	pool.releaseInFlight(2)
	pool.mu.Lock()
	v := pool.inFlight[2]
	pool.mu.Unlock()
	if v < 0 {
		t.Errorf("inFlight must not go negative, got %d", v)
	}
	// in-flight 优先于 token：key-2 给高 token 但 0 在途，key-0 释放到 0 在途但低 token
	pool.releaseInFlight(0)
	pool.releaseInFlight(0)
	pool.releaseInFlight(1)
	pool.recordTokens(2, 99999)
	pool.acquireInFlight(0) // key-0 现在 1 在途
	_, idx2 := pool.selectKeyForClient("bob")
	if idx2 == 0 {
		t.Errorf("key-0 has in-flight load, should not be picked first, got key-%d", idx2)
	}
}

// TestSelectKeyRoundRobin 验证 #4：selectKey 重试路径轮询，不总是回到 key-0。
func TestSelectKeyRoundRobin(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})
	seen := make(map[int]int)
	for i := 0; i < 6; i++ {
		_, idx := pool.selectKey()
		seen[idx]++
	}
	for i := 0; i < 3; i++ {
		if seen[i] != 2 {
			t.Errorf("selectKey round-robin: key-%d picked %d times, want 2 (dist=%v)", i, seen[i], seen)
		}
	}
}

// TestAffinityEviction 验证 #5：达到容量上限时淘汰陈旧绑定。
func TestAffinityEviction(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1"})
	pool.maxAffinityEntries = 3
	pool.affinityTTL = 10 * time.Minute
	now := time.Now()

	pool.mu.Lock()
	// 2 条陈旧（超 TTL），1 条新鲜
	pool.bindLocked("old-1", 0, now.Add(-time.Hour))
	pool.bindLocked("old-2", 1, now.Add(-time.Hour))
	pool.bindLocked("fresh", 0, now)
	// 此时 len==3 已达上限；再绑定触发淘汰，应清掉 2 条陈旧
	pool.bindLocked("new", 1, now)
	defer pool.mu.Unlock()

	if _, ok := pool.clientAffinity["old-1"]; ok {
		t.Error("old-1 should be evicted (stale > TTL)")
	}
	if _, ok := pool.clientAffinity["old-2"]; ok {
		t.Error("old-2 should be evicted (stale > TTL)")
	}
	if _, ok := pool.clientAffinity["fresh"]; !ok {
		t.Error("fresh binding should survive eviction")
	}
	if _, ok := pool.clientAffinity["new"]; !ok {
		t.Error("new binding should be present")
	}
}

// --- fix-B/C/D 针对性单测 ---

// TestIsHardFailureReason 验证硬失败分类（决定是否参与廉价探测）。
func TestIsHardFailureReason(t *testing.T) {
	hard := []string{"invalid_token", "account_disabled", "billing_error", "rate_limited",
		"quota_exceeded_api", "quota_exceeded_subscription", "Arrearage", "free_tier_exhausted",
		"AccessDenied.Unpurchased", glmExhaustReason, glmRateLimitQuota, glmRateLimitQPS, glmRateLimitUnknown}
	for _, r := range hard {
		if !isHardFailureReason(r) {
			t.Errorf("%q should be a hard failure", r)
		}
	}
	for _, r := range []string{"", "unknown", "some_default_code"} {
		if isHardFailureReason(r) {
			t.Errorf("%q should NOT be a hard failure", r)
		}
	}
}

// TestIsAccountLevelExhaustion 验证账户级失败分类（决定 fix-C 是否收敛透传）。
// 关键：rate_limited 与 quota_exceeded_subscription 是瞬时/可自动恢复的，不应收敛。
func TestIsAccountLevelExhaustion(t *testing.T) {
	account := []string{"invalid_token", "account_disabled", "billing_error",
		"quota_exceeded_api", "Arrearage", "free_tier_exhausted", "AccessDenied.Unpurchased"}
	for _, r := range account {
		if !isAccountLevelExhaustion(r) {
			t.Errorf("%q should be account-level", r)
		}
	}
	for _, r := range []string{"rate_limited", "quota_exceeded_subscription", "", "unknown"} {
		if isAccountLevelExhaustion(r) {
			t.Errorf("%q should NOT be account-level", r)
		}
	}
}

// TestGetProbeableExhaustedKeys 验证 fix-B：硬失败 key 不参与廉价 GET 探测。
func TestGetProbeableExhaustedKeys(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})
	pool.markExhausted(0, "invalid_token") // 硬失败 → 不可探测
	pool.markExhausted(1, "some_unknown")  // default 永久 → 可探测
	// key-2 保持 active

	probeable := pool.getProbeableExhaustedKeys()
	for _, idx := range probeable {
		if idx == 0 {
			t.Error("hard-failure key-0 must NOT be probeable")
		}
	}
	found := false
	for _, idx := range probeable {
		if idx == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("unknown-reason key-1 should be probeable, got %v", probeable)
	}

	// getExhaustedKeys 仍应包含两者（不过滤）
	all := pool.getExhaustedKeys()
	if len(all) != 2 {
		t.Errorf("getExhaustedKeys should return both exhausted keys, got %v", all)
	}
}

// TestExhaustReasonAt 验证 fix-C 读取耗尽原因的 helper。
func TestExhaustReasonAt(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1"})
	if r := pool.exhaustReasonAt(0); r != "" {
		t.Errorf("active key should return empty reason, got %q", r)
	}
	pool.markExhausted(0, "invalid_token")
	if r := pool.exhaustReasonAt(0); r != "invalid_token" {
		t.Errorf("exhausted key reason: got %q want invalid_token", r)
	}
	if r := pool.exhaustReasonAt(99); r != "" {
		t.Errorf("out-of-range index should return empty, got %q", r)
	}
}

// TestMarkExhaustedWithError verifies safe diagnostics without storing the body.
func TestMarkExhaustedWithError(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1"})
	body := `{"code":"invalid_api_key","message":"Incorrect API key provided"}`
	pool.markExhaustedWithError(0, "invalid_token", body)

	st := pool.status()
	if st[0].LastError != "upstream error (details withheld)" {
		t.Errorf("LastError not recorded: got %q", st[0].LastError)
	}
	if st[0].LastErrorAt.IsZero() {
		t.Error("LastErrorAt should be set")
	}
	if st[1].LastError != "" {
		t.Error("untouched key should have empty LastError")
	}

	// A longer body has the same safe marker, not a truncated prefix.
	pool2 := newTestKeyPool([]string{"key-0"})
	long := strings.Repeat("x", 1000)
	pool2.markExhaustedWithError(0, "billing_error", long)
	got := pool2.status()[0].LastError
	if got != "upstream error (details withheld)" {
		t.Errorf("long body should be withheld, got %q", got)
	}
}

// TestExhaustedKeys_SnapshotAndRestore 验证 ApplyConfig 重建 keyPool 时冷却状态保留。
// 场景：智谱 GLM 1310 周限流（cooldownEnd=下周四），重建 keyPool 后状态不丢失。
func TestExhaustedKeys_SnapshotAndRestore(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})
	now := time.Now()
	future := now.Add(5 * time.Hour)

	// 标记 key-0 为 GLM 周限流（cooldownEnd=未来）
	pool.markExhaustedWithCooldown(0, glmRateLimitQuota, future, "1310 body")
	// 标记 key-1 为普通 quota_exceeded
	pool.markExhausted(1, "quota_exceeded")
	// key-2 保持 active

	// Snapshot
	state := pool.exhaustedKeys()
	if len(state) != 2 {
		t.Fatalf("expected 2 exhausted keys, got %d", len(state))
	}
	s0, ok := state["key-0"]
	if !ok {
		t.Fatal("key-0 should be in snapshot")
	}
	if !s0.Exhausted || s0.ExhaustReason != glmRateLimitQuota {
		t.Errorf("key-0 state mismatch: exhausted=%v reason=%q", s0.Exhausted, s0.ExhaustReason)
	}
	if !s0.CooldownEnd.Equal(future) {
		t.Errorf("key-0 cooldownEnd = %v, want %v", s0.CooldownEnd, future)
	}

	s1, ok := state["key-1"]
	if !ok || s1.ExhaustReason != "quota_exceeded_api" {
		t.Errorf("key-1 state mismatch: %+v", s1)
	}

	// key-2 不应在 snapshot 中
	if _, ok := state["key-2"]; ok {
		t.Error("key-2 should not be in snapshot (active)")
	}

	// 模拟 ApplyConfig：重建新 keyPool，恢复状态
	pool2 := newTestKeyPool([]string{"key-0", "key-1", "key-2"})
	pool2.restoreExhaustedKeys(state)

	// 验证恢复后的状态
	st0 := pool2.entries[0]
	if !st0.exhausted || st0.exhaustReason != glmRateLimitQuota {
		t.Errorf("key-0 not restored: exhausted=%v reason=%q", st0.exhausted, st0.exhaustReason)
	}
	if !st0.cooldownEnd.Equal(future) {
		t.Errorf("key-0 cooldownEnd not restored: %v want %v", st0.cooldownEnd, future)
	}

	st1 := pool2.entries[1]
	if !st1.exhausted || st1.exhaustReason != "quota_exceeded_api" {
		t.Errorf("key-1 not restored: exhausted=%v reason=%q", st1.exhausted, st1.exhaustReason)
	}

	// key-2 仍是 active
	st2 := pool2.entries[2]
	if st2.exhausted {
		t.Error("key-2 should remain active after restore")
	}
}

// TestExhaustedKeys_RestoreSkipsExpired 验证冷却已过期的 key 不会被恢复。
func TestExhaustedKeys_RestoreSkipsExpired(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0"})
	past := time.Now().Add(-1 * time.Hour)

	// 标记 cooldownEnd 已过期（不应进 snapshot）
	pool.entries[0].exhausted = true
	pool.entries[0].exhaustReason = "rate_limited"
	pool.entries[0].cooldownEnd = past

	state := pool.exhaustedKeys()
	if len(state) != 0 {
		t.Errorf("expired cooldown should NOT be in snapshot, got %d entries", len(state))
	}
}

// TestExhaustedKeys_RestoreEmpty 验证边界：空 map、nil pool 不 panic。
func TestExhaustedKeys_RestoreEmpty(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0"})

	// 空 map 应安全
	pool.restoreExhaustedKeys(nil)
	pool.restoreExhaustedKeys(map[string]keyExhaustedState{})

	// key 不存在于新 pool 也不应 panic
	pool.restoreExhaustedKeys(map[string]keyExhaustedState{
		"nonexistent-key": {Exhausted: true, ExhaustReason: "test"},
	})

	if pool.entries[0].exhausted {
		t.Error("restoring nonexistent key should not mark real keys")
	}
}

// TestExhaustedKeys_KeyReorder 验证 key 顺序变化时按 key 值匹配恢复。
func TestExhaustedKeys_KeyReorder(t *testing.T) {
	pool := newTestKeyPool([]string{"key-a", "key-b"})
	future := time.Now().Add(time.Hour)
	pool.markExhaustedWithCooldown(0, "glm_rate_limit_quota", future, "")

	state := pool.exhaustedKeys()

	// 重建 keyPool，key 顺序反转
	pool2 := newTestKeyPool([]string{"key-b", "key-a"})
	pool2.restoreExhaustedKeys(state)

	// key-a 在 index 1（保持恢复）
	if !pool2.entries[1].exhausted || pool2.entries[1].exhaustReason != "glm_rate_limit_quota" {
		t.Errorf("key-a should be restored at new index 1, got exhausted=%v reason=%q",
			pool2.entries[1].exhausted, pool2.entries[1].exhaustReason)
	}
	// key-b 仍是 active
	if pool2.entries[0].exhausted {
		t.Error("key-b should remain active")
	}
}

// TestExhaustedKeys_ConsecutiveRateLimit 验证指数退避计数也保留。
func TestExhaustedKeys_ConsecutiveRateLimit(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0"})
	now := time.Now()
	// 模拟 key 因连续 429 限流已处于冷却中，consecutiveRateLimit=3
	pool.entries[0].exhausted = true
	pool.entries[0].exhaustReason = "rate_limited"
	pool.entries[0].cooldownEnd = now.Add(20 * time.Second)
	pool.entries[0].consecutiveRateLimit = 3

	state := pool.exhaustedKeys()
	pool2 := newTestKeyPool([]string{"key-0"})
	pool2.restoreExhaustedKeys(state)

	if pool2.entries[0].consecutiveRateLimit != 3 {
		t.Errorf("consecutiveRateLimit not preserved: got %d want 3",
			pool2.entries[0].consecutiveRateLimit)
	}
}

// TestExhaustedKeys_CooldownExpiryBehavior 守护 exhaustedKeys 的跨重启语义（回退修复#3 后的策略）：
//
// 跨重启语义完全由 markExhausted 赋予的 cd 数值承载，exhaustedKeys 只用单一守卫
// `cooldownEnd.After(now)` 判断，不按 reason 二次分类：
//
//   - 永久类（quota_exceeded_api 等，cd=permanentCooldownEnd(9999)）：cd 永不过期 → 天然收集保留。
//   - 凭证类（invalid_token 等，cd=now+5min）：重启耗时 > 5min 后 cd 过期 → 不收集 → 新 pool 洗白为 active。
//     这是有意的：首个请求验证失败会重新 markExhausted(5min)，自愈闭环成立；刻意保留反而破坏防误判语义。
//   - 瞬时类（rate_limited，cd=5-60s）：重启后通常 cd 过期 → 洗白，语义正确。
func TestExhaustedKeys_CooldownExpiryBehavior(t *testing.T) {
	pool := newTestKeyPool([]string{"perm-key", "cred-key", "rate-key"})
	now := time.Now()
	past := now.Add(-1 * time.Hour) // 冷却已过期 1 小时

	// key-0：quota_exceeded_api（永久类，cd 本就是 9999），未过期
	pool.entries[0].exhausted = true
	pool.entries[0].exhaustReason = "quota_exceeded_api"
	pool.entries[0].cooldownEnd = permanentCooldownEnd

	// key-1：invalid_token（凭证类，cd 已过期）
	pool.entries[1].exhausted = true
	pool.entries[1].exhaustReason = "invalid_token"
	pool.entries[1].cooldownEnd = past

	// key-2：rate_limited（瞬时类，cd 已过期）
	pool.entries[2].exhausted = true
	pool.entries[2].exhaustReason = "rate_limited"
	pool.entries[2].cooldownEnd = past

	state := pool.exhaustedKeys()

	// key-0（永久类，cd 未过期）应被收集，CooldownEnd 保留原值（未被重写）
	s0, ok0 := state["perm-key"]
	if !ok0 {
		t.Fatal("permanent key (quota_exceeded_api, cd=9999) MUST be collected (cd never expires)")
	}
	if !s0.Exhausted || s0.ExhaustReason != "quota_exceeded_api" {
		t.Errorf("perm-key snapshot: exhausted=%v reason=%q", s0.Exhausted, s0.ExhaustReason)
	}
	if !s0.CooldownEnd.Equal(permanentCooldownEnd) {
		t.Errorf("perm-key CooldownEnd = %v, want permanentCooldownEnd %v", s0.CooldownEnd, permanentCooldownEnd)
	}

	// key-1（凭证类，cd 已过期）应被洗白（不收集）
	if _, ok := state["cred-key"]; ok {
		t.Error("credential key (invalid_token, cd expired) should NOT be collected (cooldown 决定，非 reason 决定)")
	}

	// key-2（瞬时类，cd 已过期）应被洗白（不收集）
	if _, ok := state["rate-key"]; ok {
		t.Error("rate_limited key (cd expired) should NOT be collected")
	}

	// 验证 restoreExhaustedKeys：只恢复 key-0
	pool2 := newTestKeyPool([]string{"perm-key", "cred-key", "rate-key"})
	pool2.restoreExhaustedKeys(state)
	if !pool2.entries[0].exhausted || pool2.entries[0].exhaustReason != "quota_exceeded_api" {
		t.Errorf("perm-key not restored: exhausted=%v reason=%q",
			pool2.entries[0].exhausted, pool2.entries[0].exhaustReason)
	}
	// key-1/key-2 保持 active（洗白）
	if pool2.entries[1].exhausted {
		t.Error("cred-key should remain active (whitelisted after cooldown expiry)")
	}
	if pool2.entries[2].exhausted {
		t.Error("rate-key should remain active (whitelisted after cooldown expiry)")
	}
}

// TestExhaustedKeys_FutureCooldownCollected 验证 cd 未过期时所有 reason 都被收集（不按 reason 区分）。
func TestExhaustedKeys_FutureCooldownCollected(t *testing.T) {
	pool := newTestKeyPool([]string{"k0", "k1"})
	future := time.Now().Add(time.Hour)

	pool.entries[0].exhausted = true
	pool.entries[0].exhaustReason = "billing_error"
	pool.entries[0].cooldownEnd = future

	pool.entries[1].exhausted = true
	pool.entries[1].exhaustReason = "rate_limited"
	pool.entries[1].cooldownEnd = future

	state := pool.exhaustedKeys()
	// cd 未过期时，凭证类和瞬时类都应被收集，CooldownEnd 保留原值
	for _, key := range []string{"k0", "k1"} {
		s, ok := state[key]
		if !ok {
			t.Errorf("%s with future cooldown should be collected", key)
			continue
		}
		if !s.CooldownEnd.Equal(future) {
			t.Errorf("%s CooldownEnd = %v, want original future %v (must NOT be overwritten)",
				key, s.CooldownEnd, future)
		}
	}
}

// TestMarkRateLimitedWithBody_1305 守护 fix-glm-ratelimit G1：429 路径 code 1305 使用 60s 冷却。
// 防止 1305 落到 default 分支（code 非空 = 智谱未知 code → 5h fallback）或误判为额度类。
func TestMarkRateLimitedWithBody_1305(t *testing.T) {
	pool := newKeyPool("test-target", []string{"k0"}, "", slog.Default())
	// 智谱 429 body 格式：{"error":{"code":"1305","message":"服务暂不可用"}}
	body := []byte(`{"error":{"code":"1305","message":"服务暂不可用"}}`)

	before := time.Now()
	pool.markRateLimitedWithBody(0, body)
	after := time.Now()

	if !pool.entries[0].exhausted {
		t.Fatal("key-0 should be marked exhausted")
	}
	if pool.entries[0].exhaustReason != glmRateLimitOverload {
		t.Errorf("exhaustReason = %q, want %q (1305 = overload)",
			pool.entries[0].exhaustReason, glmRateLimitOverload)
	}
	// 冷却时长应在 58s~62s（60s ± 2s 容差）
	cooldown := pool.entries[0].cooldownEnd.Sub(before)
	cooldownHi := pool.entries[0].cooldownEnd.Sub(after)
	if cooldown < 55*time.Second || cooldown > 65*time.Second {
		t.Errorf("1305 cooldown should be ~60s, got %v (before=%v, cooldownEnd=%v)",
			cooldown, before, pool.entries[0].cooldownEnd)
	}
	if cooldownHi > 62*time.Second || cooldownHi < 58*time.Second {
		t.Errorf("1305 cooldown from end time = %v, want ~60s", cooldownHi)
	}
}

// TestGLMRateLimitOverload_ClassifierMatrix 守护 fix-glm-ratelimit G1：1305 在所有
// GLM reason 分类函数中正确归类（防止状态分裂）。
// - isGLMRateLimitReason = true（healthz/metrics 聚合）
// - isHardFailureReason = true（/models 探测对过载同样不可靠）
// - isRecoverableAfterCooldown = true（60s 冷却过期=真恢复）
func TestGLMRateLimitOverload_ClassifierMatrix(t *testing.T) {
	if !isGLMRateLimitReason(glmRateLimitOverload) {
		t.Error("isGLMRateLimitReason(glmRateLimitOverload) = false, want true (healthz aggregation)")
	}
	if !isHardFailureReason(glmRateLimitOverload) {
		t.Error("isHardFailureReason(glmRateLimitOverload) = false, want true (probe unreliable for overload)")
	}
	if !isRecoverableAfterCooldown(glmRateLimitOverload) {
		t.Error("isRecoverableAfterCooldown(glmRateLimitOverload) = false, want true (60s cooldown = real recovery)")
	}
}

// TestQuotaExhaustionStatus_ExcludesOverload 守护 fix-glm-ratelimit G1 task 1.9：
// quotaExhaustionStatus 的 reason 集合**不能包含** glmRateLimitOverload（1305 = 瞬时类）。
// 若误入，全池 1305 时响应体会错报"额度耗尽+恢复时间"，与瞬时语义矛盾。
func TestQuotaExhaustionStatus_ExcludesOverload(t *testing.T) {
	pool := newKeyPool("test-target", []string{"k0"}, "", slog.Default())
	future := time.Now().Add(60 * time.Second)
	pool.markExhaustedWithCooldown(0, glmRateLimitOverload, future, "1305 body")

	// 唯一 key 是 glmRateLimitOverload (瞬时类)，不应被判为额度耗尽
	isQuota, resetAt := pool.quotaExhaustionStatus()
	if isQuota {
		t.Errorf("quotaExhaustionStatus should return false for overload-only keys, got true (reset=%v)", resetAt)
	}
}
