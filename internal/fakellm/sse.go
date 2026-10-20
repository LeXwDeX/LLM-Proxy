package fakellm

import (
	"encoding/json"
	"net/http"
	"time"
)

// sseStream writes one Server-Sent Events response and applies the stream
// faults of the scenario (chunk delay, cut, keep-alive comments).
type sseStream struct {
	c       *call
	flusher http.Flusher
	events  int
}

func (c *call) startSSE() *sseStream {
	h := c.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	c.w.WriteHeader(http.StatusOK)
	f, _ := c.w.(http.Flusher)
	st := &sseStream{c: c, flusher: f}
	st.flush()
	return st
}

func (st *sseStream) flush() {
	if st.flusher != nil {
		st.flusher.Flush()
	}
}

func keepAliveLine(flavor string) string {
	switch flavor {
	case FlavorOpenRouter:
		return ": OPENROUTER PROCESSING\n\n"
	case FlavorDeepSeek:
		return ": keep-alive\n\n"
	}
	return ": ping\n\n"
}

// event writes "event: name" (when non-empty) and a JSON data line.
func (st *sseStream) event(name string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	st.rawEvent(name, string(data))
}

func (st *sseStream) rawEvent(name, data string) {
	sc := st.c.sc
	if sc.CutAfter > 0 && st.events >= sc.CutAfter {
		st.c.rec.Status, st.c.rec.Outcome = 200, "cut"
		st.c.s.record(st.c.rec)
		// Drop the connection without the chunked terminator: the client
		// sees an unexpected EOF mid-stream.
		panic(http.ErrAbortHandler)
	}
	if st.events > 0 && sc.ChunkDelayMS > 0 {
		select {
		case <-time.After(time.Duration(sc.ChunkDelayMS) * time.Millisecond):
		case <-st.c.r.Context().Done():
			st.c.rec.Status, st.c.rec.Outcome = 200, "canceled"
			st.c.s.record(st.c.rec)
			panic(http.ErrAbortHandler)
		}
	}
	if sc.KeepAlive {
		_, _ = st.c.w.Write([]byte(keepAliveLine(st.c.flavor)))
	}
	buf := make([]byte, 0, len(data)+32)
	if name != "" {
		buf = append(buf, "event: "+name+"\n"...)
	}
	buf = append(buf, "data: "+data+"\n\n"...)
	_, _ = st.c.w.Write(buf)
	st.flush()
	st.events++
}

// done finishes the stream and records the outcome.
func (st *sseStream) done(outcome string, u *Usage) {
	st.flush()
	st.c.rec.Status, st.c.rec.Outcome, st.c.rec.Usage = 200, outcome, u
	st.c.s.record(st.c.rec)
}

// sseError answers 200 + SSE whose first event is an error: Zhipu's
// historical rate-limit shape (event: error + rate_limit_error + code).
func (c *call) sseError(e apiError) {
	st := c.startSSE()
	inner := map[string]any{"type": e.errType, "code": e.code, "message": e.message}
	st.event("error", map[string]any{"type": "error", "error": inner, "request_id": c.w.Header().Get("X-Request-Id")})
	st.rawEvent("", "[DONE]")
	c.rec.Status, c.rec.Outcome = 200, "sse_error"
	c.s.record(c.rec)
}
