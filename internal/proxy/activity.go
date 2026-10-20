package proxy

import (
	"strings"
	"sync"
	"time"
)

const (
	ActivityWindow = 5 * time.Minute
	ActivityStep   = 10 * time.Second
)

type ActivityInterval struct {
	From    time.Time
	To      time.Time
	Partial bool
}

type ClientActivity struct {
	Name           string
	ActiveRequests int
	Peaks          []*int
}

// ActivitySnapshot contains one lock-consistent runtime observation. Keys are
// normalized client names, never credential identities.
type ActivitySnapshot struct {
	Since          time.Time
	From           time.Time
	To             time.Time
	Complete       bool
	ActiveRequests int
	ActiveClients  int
	Intervals      []ActivityInterval
	Peaks          []*int
	Clients        map[string]ClientActivity
}

type activityClient struct {
	name   string
	active int
	seen   time.Time
}

type activityBucket struct {
	peak    int
	clients map[string]int
}

type activityTracker struct {
	mu      sync.Mutex
	clock   func() time.Time
	since   time.Time
	last    time.Time
	current time.Time
	active  int
	clients map[string]*activityClient
	buckets map[time.Time]*activityBucket
}

func newActivityTracker(now time.Time) *activityTracker {
	t := &activityTracker{clock: time.Now, since: now.UTC(), clients: make(map[string]*activityClient), buckets: make(map[time.Time]*activityBucket)}
	t.advance(now.UTC())
	return t
}

func activityName(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

func (t *activityTracker) baseline() *activityBucket {
	b := &activityBucket{peak: t.active, clients: make(map[string]int)}
	for key, c := range t.clients {
		if c.active > 0 {
			b.clients[key] = c.active
		}
	}
	return b
}

// advance fills silent periods from the sustained-request baseline. Work is
// bounded to the retained window even after a large clock jump.
func (t *activityTracker) advance(now time.Time) {
	if !t.last.IsZero() && now.Before(t.last) {
		t.since = now
		t.current = time.Time{}
		clear(t.buckets)
		for key, c := range t.clients {
			if c.active == 0 {
				delete(t.clients, key)
			} else {
				c.seen = now
			}
		}
	}
	current := now.Truncate(ActivityStep)
	if t.current.IsZero() || !current.Equal(t.current) {
		earliest := now.Add(-ActivityWindow).Truncate(ActivityStep)
		start := t.current.Add(ActivityStep)
		if t.current.IsZero() || start.Before(earliest) {
			start = earliest
		}
		// At startup only the current bucket is observed; older buckets stay unknown.
		if t.current.IsZero() {
			start = current
		}
		for at := start; !at.After(current); at = at.Add(ActivityStep) {
			t.buckets[at] = t.baseline()
		}
		for at := range t.buckets {
			if at.Before(earliest) {
				delete(t.buckets, at)
			}
		}
		for key, c := range t.clients {
			if c.active == 0 && c.seen.Before(now.Add(-ActivityWindow)) {
				delete(t.clients, key)
			}
		}
		t.current = current
	}
	t.last = now
}

func (t *activityTracker) begin(name string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.beginLocked(name, t.clock())
}

func (t *activityTracker) beginLocked(name string, now time.Time) string {
	now = now.UTC()
	t.advance(now)
	key := activityName(name)
	c := t.clients[key]
	if c == nil {
		c = &activityClient{name: strings.TrimSpace(name)}
		t.clients[key] = c
	}
	c.active++
	c.seen = now
	t.active++
	b := t.buckets[t.current]
	if t.active > b.peak {
		b.peak = t.active
	}
	if c.active > b.clients[key] {
		b.clients[key] = c.active
	}
	return key
}

func (t *activityTracker) end(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.endLocked(key, t.clock())
}

func (t *activityTracker) endLocked(key string, now time.Time) {
	now = now.UTC()
	t.advance(now)
	if c := t.clients[key]; c != nil && c.active > 0 {
		c.active--
		c.seen = now
		t.active--
	}
}

func (t *activityTracker) snapshot() ActivitySnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked(t.clock())
}

func (t *activityTracker) snapshotLocked(now time.Time) ActivitySnapshot {
	now = now.UTC()
	t.advance(now)
	from := now.Add(-ActivityWindow)
	result := ActivitySnapshot{Since: t.since, From: from, To: now, Complete: !from.Before(t.since), ActiveRequests: t.active, Clients: make(map[string]ClientActivity)}
	for at := from.Truncate(ActivityStep); at.Before(now); at = at.Add(ActivityStep) {
		start, end := at, at.Add(ActivityStep)
		if start.Before(from) {
			start = from
		}
		if end.After(now) {
			end = now
		}
		result.Intervals = append(result.Intervals, ActivityInterval{From: start, To: end, Partial: end.Sub(start) != ActivityStep})
		var peak *int
		if b := t.buckets[at]; b != nil && !at.Before(t.since) && start.Equal(at) {
			v := b.peak
			peak = &v
		}
		result.Peaks = append(result.Peaks, peak)
	}
	for key, c := range t.clients {
		if c.active > 0 {
			result.ActiveClients++
		}
		client := ClientActivity{Name: c.name, ActiveRequests: c.active, Peaks: make([]*int, len(result.Intervals))}
		for i, interval := range result.Intervals {
			if result.Peaks[i] != nil {
				at := interval.From.Truncate(ActivityStep)
				v := t.buckets[at].clients[key]
				client.Peaks[i] = &v
			}
		}
		result.Clients[key] = client
	}
	return result
}

func (s *Service) ActivitySnapshot() ActivitySnapshot { return s.activity.snapshot() }

type WebsocketActivity struct {
	Name     string
	Sessions int
}

// WebsocketActivitySnapshot merges sessions across credentials while exposing
// only display names. Its lock is separate from the request runtime snapshot.
func (s *Service) WebsocketActivitySnapshot() map[string]WebsocketActivity {
	s.websocketMu.Lock()
	defer s.websocketMu.Unlock()
	result := make(map[string]WebsocketActivity)
	for identity, count := range s.websocketClients {
		name, _, _ := strings.Cut(identity, "\x00")
		key := activityName(name)
		item := result[key]
		item.Name = name
		item.Sessions += count
		result[key] = item
	}
	return result
}
