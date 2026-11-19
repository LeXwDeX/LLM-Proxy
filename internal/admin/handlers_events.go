package admin

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
)

// eventsSnapshot is the payload of one /admin/events frame (Issue #225). Its
// targets_runtime is assembled by the same helper as /admin/metrics, so the
// streamed and the polled view share field names and types; shadow_routing is
// omitted when there are no comparisons, exactly like /admin/metrics. The
// snapshot carries no endpoint URLs, keys or request bodies.
type eventsSnapshot struct {
	GeneratedAt    time.Time               `json:"generated_at"`
	TargetsRuntime []metricsTargetRuntime  `json:"targets_runtime"`
	ShadowRouting  []proxy.ShadowPoolStats `json:"shadow_routing,omitempty"`
}

// buildEventsSnapshot computes and serializes one SSE payload. It runs on the
// hub goroutine or on the first subscriber's request goroutine, never under
// the hub lock or Handler.configMu.
func (h *Handler) buildEventsSnapshot() []byte {
	now := time.Now()
	snapshot := eventsSnapshot{
		GeneratedAt:    now,
		TargetsRuntime: h.buildTargetsRuntime(now, h.proxyService.TargetStatuses(now)),
		ShadowRouting:  h.proxyService.ShadowRoutingSnapshot(),
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("admin events snapshot encode failed", "error", err)
		}
		return nil
	}
	return data
}

// handleEvents streams the runtime snapshot as server-sent events. Readonly
// sessions may subscribe; the route is GET, has no side effects and writes no
// audit event. Unauthenticated requests are rejected by SessionManager
// middleware before reaching here.
func (h *Handler) handleEvents(w http.ResponseWriter, r *http.Request) {
	hub := h.events
	if hub == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse("event stream unavailable"))
		return
	}
	// Enforce the global limit before any response headers so the rejection is
	// a plain 503 the EventSource fallback can honor.
	sub, err := hub.subscribe()
	if err != nil {
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, errorResponse("too many event stream subscribers"))
		return
	}
	defer hub.unsubscribe(sub)

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	// Disable proxy buffering (nginx and friends); Cache-Control: no-store is
	// already set by AdminSecurityHeaders and is stricter than no-cache.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Per-write deadline keeps a stalled client from holding the connection
	// forever; a real write failure ends the stream.
	rc := http.NewResponseController(w)
	write := func(payload []byte) bool {
		return h.writeEventsPayload(w, rc, hub.writeTimeout, payload)
	}

	// Reconnect delay hint for EventSource; reconnecting revalidates the
	// session through the middleware.
	if !write([]byte("retry: 3000\n\n")) {
		return
	}
	// First frame as soon as possible: the latest snapshot, or one computed
	// right now when this is the first subscriber after startup.
	if data := hub.snapshot(); data != nil {
		if !write(eventsFrame(data)) {
			return
		}
	}

	heartbeat := time.NewTicker(hub.heartbeat)
	defer heartbeat.Stop()
	lifetime := time.NewTimer(hub.maxLifetime)
	defer lifetime.Stop()

	for {
		select {
		case <-r.Context().Done():
			// Client went away (or server shutdown): closing is enough, no
			// JSON error may follow a started stream.
			return
		case <-lifetime.C:
			// Bounded connection lifetime: EventSource reconnects and
			// revalidates the session, so a sliding cookie cannot be renewed
			// indefinitely by a hidden tab.
			return
		case data, ok := <-sub.ch:
			if !ok { // StopEvents closed the hub
				return
			}
			if !write(eventsFrame(data)) {
				return
			}
		case <-heartbeat.C:
			if !write([]byte(": ping\n\n")) {
				return
			}
		}
	}
}

// eventsFrame wraps one serialized snapshot in an SSE event.
func eventsFrame(data []byte) []byte {
	frame := make([]byte, 0, len(data)+24)
	frame = append(frame, "event: snapshot\ndata: "...)
	frame = append(frame, data...)
	frame = append(frame, '\n', '\n')
	return frame
}

// writeEventsPayload writes one SSE chunk with a bounded write deadline and
// flushes it. A failed write (usually a disconnected client) is logged and
// ends the stream.
func (h *Handler) writeEventsPayload(w http.ResponseWriter, rc *http.ResponseController, timeout time.Duration, payload []byte) bool {
	_ = rc.SetWriteDeadline(time.Now().Add(timeout))
	if _, err := w.Write(payload); err != nil {
		if h.logger != nil {
			h.logger.Debug("admin events write failed", "error", err)
		}
		return false
	}
	if err := rc.Flush(); err != nil {
		if h.logger != nil {
			h.logger.Debug("admin events flush failed", "error", err)
		}
		return false
	}
	return true
}
