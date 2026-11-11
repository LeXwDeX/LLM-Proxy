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
	// temporarySessionAffinityTTL bounds a session binding created on a
	// non-preferred (more expensive) cost tier. It never slides, so the
	// session re-selects, and returns to the cheap tier once it recovers.
	temporarySessionAffinityTTL = 10 * time.Minute
	maxAffinityEntries          = 50000 // 容量上限，防止 map 无限增长
)

type affinityEntry struct {
	targetName string
	lastUsed   time.Time
	ttl        time.Duration // 零值沿用 IP 粘连的默认 TTL
	// expiresAt 非零表示临时绑定：绝对到期，不随使用滑动。
	expiresAt time.Time
	// prov 非空表示该条目由尚未结算的预写产生（见 affinityProv）；nil 为已确认。
	prov *affinityProv
}

func (e affinityEntry) temporary() bool { return !e.expiresAt.IsZero() }

func (e affinityEntry) expired(now time.Time) bool {
	if e.temporary() {
		return now.After(e.expiresAt)
	}
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
	a.setWithTTLLocked(key, targetName, now, ttl)
	a.mu.Unlock()
}

func (a *affinityMap) setWithTTLLocked(key, targetName string, now time.Time, ttl time.Duration) {
	// 新增条目前检查容量（更新已有条目不触发淘汰）
	if _, exists := a.entries[key]; !exists && len(a.entries) >= a.maxEntries {
		a.evictLocked(now)
	}
	a.entries[key] = affinityEntry{targetName: targetName, lastUsed: now, ttl: ttl}
}

// SetTemporary writes a binding that expires at an absolute time and never
// slides. It does not demote an existing sliding binding to the same target
// (that binding is the client's established stickiness and is refreshed
// instead), and it does not extend a live temporary binding to the same
// target, so a temporary binding always ends and the next request re-selects.
func (a *affinityMap) SetTemporary(key, targetName string, now time.Time, slidingTTL, ttl time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setTemporaryLocked(key, targetName, now, slidingTTL, ttl)
}

func (a *affinityMap) setTemporaryLocked(key, targetName string, now time.Time, slidingTTL, ttl time.Duration) {
	existing, exists := a.entries[key]
	if exists && !existing.expired(now) && existing.targetName == targetName {
		if !existing.temporary() {
			existing.lastUsed = now
			existing.ttl = slidingTTL
			a.entries[key] = existing
		}
		return
	}
	if !exists && len(a.entries) >= a.maxEntries {
		a.evictLocked(now)
	}
	a.entries[key] = affinityEntry{targetName: targetName, lastUsed: now, expiresAt: now.Add(ttl)}
}

// Touch refreshes the client's own live binding to targetName and reports
// whether one existed. A sliding binding slides; a temporary one is left
// untouched (it must end at its absolute expiry). A missing, expired or
// different binding is not created or overwritten.
func (a *affinityMap) Touch(key, targetName string, now time.Time, slidingTTL time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.touchLocked(key, targetName, now, slidingTTL)
}

func (a *affinityMap) touchLocked(key, targetName string, now time.Time, slidingTTL time.Duration) bool {
	e, ok := a.entries[key]
	if !ok || e.expired(now) || e.targetName != targetName {
		return false
	}
	if !e.temporary() {
		e.lastUsed = now
		e.ttl = slidingTTL
		a.entries[key] = e
	}
	return true
}

// provisional status of a prewrite.
const (
	provPending = iota
	provConfirmed
	provFailed
)

// affinityProv is one prewrite of a key. Prewrites that overlap form a chain
// (prev is the state this one was written over), so a rollback never falls
// back to a state that was itself never delivered: it walks to the newest
// confirmed state, or to the plain binding the chain started from, or to
// nothing. All fields are guarded by affinityMap.mu.
type affinityProv struct {
	status int
	// value is the entry this prewrite produced (without prov).
	value affinityEntry
	// prev is the earlier prewrite this one was written over; nil when it
	// was written over a plain entry (prevEntry, if hadPrev) or over nothing.
	prev      *affinityProv
	prevEntry affinityEntry
	hadPrev   bool
}

// affinityUndo is the settlement token of one binding written ahead of the
// response headers.
type affinityUndo struct {
	key  string
	node *affinityProv // nil when the write changed nothing
	// target and slidingTTL describe the sliding binding the request asserts
	// (slidingTTL 0 is the IP default), for refresh or rebuild on success.
	target     string
	slidingTTL time.Duration
	rebuild    rebuildPolicy
}

// rebuildPolicy says whether a success may recreate a lapsed binding.
type rebuildPolicy int

const (
	rebuildNever       rebuildPolicy = iota // temporary bindings keep their absolute expiry
	rebuildAlways                           // written as a sliding binding on a preferred tier
	rebuildIfPreferred                      // a hit: recreated only on a preferred tier
)

// Prewrite runs op (which must use only the *Locked writers) on key under the
// map's lock and returns the token that settles it. Reading the previous entry
// and writing are one critical section, so the token never describes a state
// another writer replaced in between.
func (a *affinityMap) Prewrite(key string, now time.Time, op func()) affinityUndo {
	a.mu.Lock()
	defer a.mu.Unlock()
	raw, existed := a.entries[key]
	op()
	u := affinityUndo{key: key}
	after, ok := a.entries[key]
	if !ok || (existed && after == raw) {
		return u
	}
	n := &affinityProv{value: after}
	n.value.prov = nil
	switch {
	case existed && raw.prov != nil && raw.prov.status != provFailed && !raw.expired(now):
		n.prev = raw.prov
	case existed && !raw.expired(now):
		n.prevEntry, n.hadPrev = raw, true
		n.prevEntry.prov = nil
	}
	after.prov = n
	a.entries[key] = after
	u.node = n
	return u
}

// Rollback settles a failed request's prewrite. Only if the key still holds
// that prewrite does it change anything: it then restores the newest state
// that was not itself a failed prewrite (a confirmed one, a pending one of an
// overlapping request, the plain binding before the chain), or removes the
// key. A binding another request rewrote since is theirs and stays.
func (a *affinityMap) Rollback(u affinityUndo) {
	n := u.node
	if n == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	n.status = provFailed
	if cur, ok := a.entries[u.key]; !ok || cur.prov != n {
		return
	}
	for p := n; ; {
		prev := p.prev
		if prev == nil {
			if p.hadPrev {
				a.entries[u.key] = p.prevEntry
			} else {
				delete(a.entries, u.key)
			}
			return
		}
		switch prev.status {
		case provConfirmed:
			a.entries[u.key] = prev.value
			return
		case provPending:
			e := prev.value
			e.prov = prev
			a.entries[u.key] = e
			return
		}
		p = prev
	}
}

// Confirm settles a delivered request's prewrite: the prewrite becomes a plain
// binding. A live sliding binding to the request's target is refreshed to now
// (a temporary one is never renewed). It reports whether the key no longer
// holds a live binding (the response outlived the sliding TTL); the caller
// decides, by the rebuild policy, whether to Revive it.
func (a *affinityMap) Confirm(u affinityUndo, now time.Time) (lapsed bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.entries[u.key]
	if n := u.node; n != nil {
		n.status = provConfirmed
		n.prev, n.prevEntry, n.hadPrev = nil, affinityEntry{}, false
		if ok && e.prov == n {
			e.prov = nil
			a.entries[u.key] = e
		}
	}
	if !ok || e.expired(now) {
		return true
	}
	if e.targetName == u.target && !e.temporary() {
		e.lastUsed, e.ttl = now, u.slidingTTL
		a.entries[u.key] = e
	}
	return false
}

// Revive recreates a lapsed sliding binding at now, only while the key holds
// no live binding: one another request put there meanwhile is not overwritten.
func (a *affinityMap) Revive(key, targetName string, now time.Time, slidingTTL time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, ok := a.entries[key]; ok && !e.expired(now) {
		return
	}
	a.setWithTTLLocked(key, targetName, now, slidingTTL)
}

// IsTemporary reports whether key holds a live temporary binding.
func (a *affinityMap) IsTemporary(key string, now time.Time) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	e, ok := a.entries[key]
	return ok && !e.expired(now) && e.temporary()
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
