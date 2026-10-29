package admin

import (
	"bytes"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// defaultReportCacheTTL is how long a polled report response is reused. The
// overview and usage aggregate pages poll every few seconds from several
// browsers, and each call scans usage data inside a bbolt read transaction; a
// short TTL plus singleflight collapses that into one scan per key per window.
const defaultReportCacheTTL = 5 * time.Second

// reportCacheMaxEntries bounds the cache; query parameters (time ranges) are
// client-controlled, so unbounded keys must not grow memory.
const reportCacheMaxEntries = 256

// reportCacheMaxBytes bounds the total cached body bytes; reportCacheMaxEntryBytes
// is the largest single response worth caching (an eighth of the budget), so one
// huge report cannot evict everything else or pin memory.
const (
	reportCacheMaxBytes      = 32 << 20
	reportCacheMaxEntryBytes = 4 << 20
)

type reportEntry struct {
	status  int
	header  http.Header
	body    []byte
	expires time.Time
	// aborted marks a computation cut short by its own caller's context; such a
	// result is meaningless to the other callers sharing the flight.
	aborted bool
}

// reportCache is a display-only, short-TTL response cache with singleflight.
// It never backs billing, quota admission, export or audit paths: it is wired
// only to read-only report routes, and writes do not invalidate it (entries
// expire by TTL).
type reportCache struct {
	ttl time.Duration
	now func() time.Time // replaceable in tests

	// Limits default to the package constants; tests shrink them.
	maxEntries    int
	maxBytes      int
	maxEntryBytes int

	mu      sync.Mutex
	entries map[string]reportEntry
	bytes   int
	flight  singleflight.Group
}

func newReportCache(ttl time.Duration) *reportCache {
	return &reportCache{
		ttl: ttl, now: time.Now, entries: make(map[string]reportEntry),
		maxEntries: reportCacheMaxEntries, maxBytes: reportCacheMaxBytes, maxEntryBytes: reportCacheMaxEntryBytes,
	}
}

// do returns the cached entry for key, or runs build once for all concurrent
// callers and caches a 200 result for the TTL.
func (c *reportCache) do(key string, build func() reportEntry) reportEntry {
	if e, ok := c.lookup(key); ok {
		return e
	}
	v, _, _ := c.flight.Do(key, func() (any, error) {
		// A caller that just missed may arrive after the previous flight stored.
		if e, ok := c.lookup(key); ok {
			return e, nil
		}
		e := build()
		if e.status == http.StatusOK {
			c.store(key, e)
		}
		return e, nil
	})
	return v.(reportEntry)
}

func (c *reportCache) lookup(key string) (reportEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || !c.now().Before(e.expires) {
		return reportEntry{}, false
	}
	return e, true
}

func (c *reportCache) store(key string, e reportEntry) {
	size := len(e.body)
	if size > c.maxEntryBytes {
		return // shared with the in-flight callers, but not retained
	}
	now := c.now()
	e.expires = now.Add(c.ttl)
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[key]; ok {
		c.bytes -= len(old.body)
		delete(c.entries, key)
	}
	// Every insert sweeps expired entries, so stale bodies never wait for the
	// cache to fill before being released.
	for k, old := range c.entries {
		if !now.Before(old.expires) {
			c.bytes -= len(old.body)
			delete(c.entries, k)
		}
	}
	// Still over a limit with only live entries: drop arbitrary ones until the
	// new entry fits (display cache; a miss only costs one recompute).
	for k, old := range c.entries {
		if len(c.entries) < c.maxEntries && c.bytes+size <= c.maxBytes {
			break
		}
		c.bytes -= len(old.body)
		delete(c.entries, k)
	}
	c.entries[key] = e
	c.bytes += size
}

// bufferedResponse captures a handler's response so it can be shared.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }
func (b *bufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}
func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

// reportCacheKey separates entries by path, the full canonical query (url.Values
// Encode sorts keys; repeated values keep their order) and the session role:
// read-only sessions get masked fields (e.g. target endpoints) that admins do not.
func reportCacheKey(r *http.Request) string {
	role := ""
	if session, ok := SessionFromContext(r.Context()); ok && session != nil {
		role = session.Role
	}
	return r.URL.Path + "\x00" + r.URL.Query().Encode() + "\x00" + role
}

// reportFlightAttempts bounds how often a live caller re-joins a flight whose
// leader's client went away. Each round is one singleflight, so at most this
// many scans run per burst regardless of how many callers are waiting.
const reportFlightAttempts = 3

// cachedReport wraps a read-only report handler with the short-TTL cache. The
// computation runs under the first caller's own request context, so a client
// abort still stops the scan (Issue #97). If that leader aborts, the callers
// that were waiting on it retry as leader of a fresh flight instead of seeing
// the abort.
func (h *Handler) cachedReport(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cache := h.reportCache
		if cache == nil || cache.ttl <= 0 || r.Method != http.MethodGet {
			next(w, r)
			return
		}
		if r.Context().Err() != nil {
			next(w, r) // nobody to answer; the handler stops on the dead context
			return
		}
		key := reportCacheKey(r)
		for attempt := 0; attempt < reportFlightAttempts; attempt++ {
			entry := cache.do(key, func() reportEntry {
				rec := &bufferedResponse{header: make(http.Header)}
				next(rec, r)
				if rec.status == 0 && rec.body.Len() == 0 && r.Context().Err() != nil {
					return reportEntry{aborted: true}
				}
				status := rec.status
				if status == 0 {
					status = http.StatusOK
				}
				return reportEntry{status: status, header: rec.header, body: rec.body.Bytes()}
			})
			if entry.aborted {
				if r.Context().Err() != nil {
					return
				}
				continue
			}
			for k, v := range entry.header {
				w.Header()[k] = append([]string(nil), v...)
			}
			w.WriteHeader(entry.status)
			_, _ = w.Write(entry.body)
			return
		}
		// Leaders kept aborting. Running next here would multiply into one
		// uncached full scan per waiter, so shed load instead; the UI retries.
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, errorResponse("report temporarily unavailable, retry"))
	}
}
