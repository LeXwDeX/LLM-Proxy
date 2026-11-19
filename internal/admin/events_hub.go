package admin

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// SSE hub defaults (Issue #225). Tests override the fields on Handler.events
// before the first subscription so they never wait for real 5s/15s/10m ticks.
const (
	defaultEventsInterval       = 5 * time.Second
	defaultEventsHeartbeat      = 15 * time.Second
	defaultEventsMaxLifetime    = 10 * time.Minute
	defaultEventsWriteTimeout   = 10 * time.Second
	defaultEventsMaxSubscribers = 32
)

// errEventsTooManySubscribers is returned before any response bytes are
// written, letting the handler answer 503 + Retry-After.
var errEventsTooManySubscribers = errors.New("admin events subscriber limit reached")

// eventsHub fans one periodically built snapshot out to every /admin/events
// subscriber. It is lazy: the ticker goroutine starts with the first subscriber
// and exits when the last one leaves, so an unused hub costs nothing.
//
// Locking: mu guards only the subscriber set and the latest serialized
// snapshot. The snapshot builder and all network writes run outside it, so a
// slow producer or client never blocks subscription bookkeeping. The builder
// must not run under Handler.configMu either (see buildEventsSnapshot).
type eventsHub struct {
	build func() []byte
	// logger may be nil (some tests construct Handlers without one).
	logger *slog.Logger

	// interval drives the single broadcast ticker. heartbeat, maxLifetime and
	// writeTimeout are read by each connection at start and never mutated
	// after the hub has subscribers.
	interval       time.Duration
	heartbeat      time.Duration
	maxLifetime    time.Duration
	writeTimeout   time.Duration
	maxSubscribers int

	mu     sync.Mutex
	subs   map[*eventsSubscriber]struct{}
	latest []byte
	cancel context.CancelFunc
	done   chan struct{}
}

// eventsSubscriber receives latest-wins snapshots. ch has capacity 1: a slow
// client keeps at most one pending frame and never blocks the broadcaster.
type eventsSubscriber struct {
	ch chan []byte
}

func newEventsHub(build func() []byte, logger *slog.Logger) *eventsHub {
	return &eventsHub{
		build:          build,
		logger:         logger,
		interval:       defaultEventsInterval,
		heartbeat:      defaultEventsHeartbeat,
		maxLifetime:    defaultEventsMaxLifetime,
		writeTimeout:   defaultEventsWriteTimeout,
		maxSubscribers: defaultEventsMaxSubscribers,
		subs:           make(map[*eventsSubscriber]struct{}),
	}
}

// subscribe registers one connection, starting the ticker goroutine for the
// first one. It returns errEventsTooManySubscribers without touching the
// response when the global limit is reached. The caller must unsubscribe.
func (h *eventsHub) subscribe() (*eventsSubscriber, error) {
	h.mu.Lock()
	if len(h.subs) >= h.maxSubscribers {
		limit := h.maxSubscribers
		h.mu.Unlock()
		if h.logger != nil {
			h.logger.Warn("admin events subscriber limit reached", "limit", limit)
		}
		return nil, errEventsTooManySubscribers
	}
	sub := &eventsSubscriber{ch: make(chan []byte, 1)}
	h.subs[sub] = struct{}{}
	started := false
	if h.cancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		h.cancel = cancel
		h.done = done
		started = true
		go h.run(ctx, done)
	}
	count := len(h.subs)
	h.mu.Unlock()

	if started && h.logger != nil {
		h.logger.Info("admin events hub started", "subscribers", count)
	}
	return sub, nil
}

// unsubscribe removes one subscriber and closes its channel. The last
// subscriber cancels the ticker goroutine; its exit is not awaited here
// because a tick may still be building a snapshot outside the lock. StopEvents
// is the synchronous variant used by server shutdown.
func (h *eventsHub) unsubscribe(sub *eventsSubscriber) {
	h.mu.Lock()
	if _, ok := h.subs[sub]; !ok {
		h.mu.Unlock()
		return
	}
	delete(h.subs, sub)
	close(sub.ch)
	var cancel context.CancelFunc
	if len(h.subs) == 0 && h.cancel != nil {
		cancel = h.cancel
		h.cancel = nil
		// Keep h.done: the run goroutine closes it on exit, so a later stop
		// still joins instead of returning while a tick unwinds.
	}
	remaining := len(h.subs)
	h.mu.Unlock()

	if cancel != nil {
		cancel()
		if h.logger != nil {
			h.logger.Info("admin events hub stopped", "subscribers", remaining)
		}
	}
}

// snapshot returns the latest serialized snapshot, building one immediately
// when the hub has none yet (first subscriber after startup). The build runs
// outside the lock. The result is cached for later subscribers but not fanned
// out: the caller writes this frame itself, and broadcasting it too would send
// the first snapshot twice to a fresh subscription.
func (h *eventsHub) snapshot() []byte {
	h.mu.Lock()
	latest := h.latest
	h.mu.Unlock()
	if latest != nil || h.build == nil {
		return latest
	}
	data := h.build()
	if data == nil {
		return nil
	}
	h.mu.Lock()
	if h.latest == nil {
		h.latest = data
	}
	h.mu.Unlock()
	return data
}

// publish stores the snapshot and fans it out latest-wins: each subscriber's
// pending frame is drained and replaced without blocking. Called with mu held
// only for the bookkeeping, never for the build.
func (h *eventsHub) publish(data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.latest = data
	for sub := range h.subs {
		select {
		case <-sub.ch: // drop the stale pending frame
		default:
		}
		select {
		case sub.ch <- data:
		default: // a concurrent publish already filled it; latest wins
		}
	}
}

// run is the single hub goroutine: one snapshot build and serialization per
// tick, shared by every subscriber.
func (h *eventsHub) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if h.build == nil {
				continue
			}
			if data := h.build(); data != nil {
				h.publish(data)
			}
		}
	}
}

// stop closes every subscriber channel and stops the ticker goroutine. It is
// safe for concurrent and repeated calls; only the first call owns the cancel.
func (h *eventsHub) stop() {
	h.mu.Lock()
	subs := make([]*eventsSubscriber, 0, len(h.subs))
	for sub := range h.subs {
		subs = append(subs, sub)
	}
	h.subs = make(map[*eventsSubscriber]struct{})
	for _, sub := range subs {
		close(sub.ch)
	}
	cancel, done := h.cancel, h.done
	h.cancel, h.done = nil, nil
	h.mu.Unlock()

	if cancel != nil {
		cancel()
		if h.logger != nil {
			h.logger.Info("admin events hub stopped", "subscribers", len(subs))
		}
	}
	// Wait outside the lock: the run goroutine may still be publishing.
	if done != nil {
		<-done
	}
}

// StopEvents closes every live /admin/events stream and stops the hub
// goroutine. cmd/proxy registers it with http.Server.RegisterOnShutdown so a
// 10-minute SSE connection cannot hold the graceful drain. Concurrent and
// repeated calls are safe; nil receivers are no-ops.
func (h *Handler) StopEvents() {
	if h == nil || h.events == nil {
		return
	}
	h.events.stop()
}
