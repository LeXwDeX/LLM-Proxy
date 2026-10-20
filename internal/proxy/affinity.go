package proxy

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	affinityTTL        = 5 * time.Minute
	sessionAffinityTTL = 24 * time.Hour
	maxAffinityEntries = 50000 // 容量上限，防止 map 无限增长
)

type affinityEntry struct {
	targetName string
	lastUsed   time.Time
	ttl        time.Duration // 零值沿用 IP 粘连的默认 TTL
}

func (e affinityEntry) expired(now time.Time) bool {
	ttl := e.ttl
	if ttl == 0 {
		ttl = affinityTTL
	}
	return now.Sub(e.lastUsed) > ttl
}

// affinityMap 存储 IP/客户端+模型与客户端/会话+模型到目标的粘连映射。
// 带容量上限（maxEntries）和惰性淘汰机制，防止 map 在高基数场景下无限增长。
type affinityMap struct {
	mu         sync.RWMutex
	entries    map[string]affinityEntry
	maxEntries int // 容量上限，测试可覆盖；生产默认 maxAffinityEntries
}

func newAffinityMap() *affinityMap {
	return &affinityMap{
		entries:    make(map[string]affinityEntry),
		maxEntries: maxAffinityEntries,
	}
}

// affinityKey 生成粘连键。
// 使用 IP + 客户端名称 + 模型，确保同一客户端从不同 IP 访问时独立分配。
func affinityKey(clientIP, clientName, model string) string {
	return clientIP + "|" + clientName + ":" + model
}

// sessionAffinityID accepts the first non-empty session header. An invalid
// higher-priority value disables session affinity rather than falling through.
func sessionAffinityID(r *http.Request) string {
	for _, header := range [...]string{"X-Session-Affinity", "X-Session-Id", "session_id", "X-Claude-Code-Session-Id"} {
		id := strings.TrimSpace(r.Header.Get(header))
		if id == "" {
			continue
		}
		if len(id) > 256 {
			return ""
		}
		for _, ch := range id {
			if unicode.IsControl(ch) {
				return ""
			}
		}
		return id
	}
	return ""
}

// sessionAffinityKey scopes a session to its authenticated principal and the
// lowercase client model, not IP or a target-specific upstream model.
// Byte-length prefixes prevent delimiter collisions in names and session IDs.
func sessionAffinityKey(clientName, sessionID, model string) string {
	return "sess|" + strconv.Itoa(len(clientName)) + ":" + clientName + "|" + strconv.Itoa(len(sessionID)) + ":" + sessionID + "|" + strings.ToLower(model)
}

// Get 查询粘连目标，过期则惰性删除并返回空。
func (a *affinityMap) Get(key string, now time.Time) (string, bool) {
	a.mu.RLock()
	entry, ok := a.entries[key]
	a.mu.RUnlock()
	if !ok {
		return "", false
	}
	if entry.expired(now) {
		// 惰性删除过期条目
		a.mu.Lock()
		if e, exists := a.entries[key]; exists && e.expired(now) {
			delete(a.entries, key)
		}
		a.mu.Unlock()
		return "", false
	}
	return entry.targetName, true
}

// Set 设置/更新粘连记录。
// 当达到容量上限时触发淘汰：先清理所有过期条目，若仍超限则淘汰最旧条目。
func (a *affinityMap) Set(key, targetName string, now time.Time) {
	a.SetWithTTL(key, targetName, now, 0)
}

// SetWithTTL shares the same capacity bound with default-TTL entries.
// Each successful write refreshes lastUsed, making the TTL sliding.
func (a *affinityMap) SetWithTTL(key, targetName string, now time.Time, ttl time.Duration) {
	a.mu.Lock()
	// 新增条目前检查容量（更新已有条目不触发淘汰）
	if _, exists := a.entries[key]; !exists && len(a.entries) >= a.maxEntries {
		a.evictLocked(now)
	}
	a.entries[key] = affinityEntry{targetName: targetName, lastUsed: now, ttl: ttl}
	a.mu.Unlock()
}

// evictLocked 淘汰陈旧条目以腾出空间。调用方必须持有 a.mu 写锁。
//
// 第一轮：删除所有超过 TTL 的条目（惰性删除之外的主动清理）。
// 第二轮：若仍达上限（说明大量条目都是新鲜的），批量驱逐最旧的一批，
// 降到低水位（90% 容量），分摊掉每条新增都要全表扫描的 O(n) 成本，
// 避免稳态下每次 Set 都触发驱逐导致的 O(n²) 复杂度。
func (a *affinityMap) evictLocked(now time.Time) {
	// 第一轮：淘汰所有过期条目
	for k, e := range a.entries {
		if e.expired(now) {
			delete(a.entries, k)
		}
	}
	// 第二轮：若仍达上限，一次性按时间排序，批量驱逐到低水位
	if len(a.entries) < a.maxEntries {
		return
	}
	lowWatermark := a.maxEntries * 9 / 10
	if lowWatermark < 1 {
		lowWatermark = 1
	}
	// 收集所有剩余条目并按 lastUsed 排序，淘汰最旧的 (len - lowWatermark) 条
	type kv struct {
		key string
		ts  time.Time
	}
	remaining := make([]kv, 0, len(a.entries))
	for k, e := range a.entries {
		remaining = append(remaining, kv{key: k, ts: e.lastUsed})
	}
	sort.Slice(remaining, func(i, j int) bool {
		return remaining[i].ts.Before(remaining[j].ts)
	})
	evictCount := len(remaining) - lowWatermark
	for i := 0; i < evictCount; i++ {
		delete(a.entries, remaining[i].key)
	}
}
