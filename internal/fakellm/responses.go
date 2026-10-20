package fakellm

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// responsesTurn builds the event sequence of one Responses generation. emit
// returns false to stop (cut or client gone).
type responsesTurn struct {
	s        *Server
	c        *call
	req      map[string]any
	raw      []byte
	sc       *Scenario
	flavor   string
	id       string
	model    string
	seq      int
	usage    Usage
	generate bool
}

func (s *Server) newTurn(c *call, req map[string]any, raw []byte, sc *Scenario) *responsesTurn {
	t := &responsesTurn{s: s, c: c, req: req, raw: raw, sc: sc, flavor: c.flavor, id: s.newID("resp"),
		model: firstNonEmpty(toString(req["model"]), s.opts.Model), generate: req["generate"] != false}
	t.usage = computeUsage(promptText(req), toInt(req["max_output_tokens"]), sc.Usage)
	if s.opts.LegacyMock && sc.Usage == nil {
		// Legacy-mock consumers rely on this exact synthetic fixture.
		t.usage = Usage{Input: 10, Output: 5}
	}
	if sc.Reasoning && (sc.Usage == nil || sc.Usage.Reasoning == nil) {
		t.usage.Reasoning = t.usage.Output / 2
	}
	if !t.generate {
		// generate:false warms the connection without producing output.
		t.usage = Usage{}
	}
	return t
}

func (t *responsesTurn) object(status string, output []any, withUsage bool) map[string]any {
	obj := map[string]any{"id": t.id, "object": "response", "created_at": time.Now().Unix(), "status": status,
		"model": t.model, "output": output, "error": nil, "incomplete_details": nil,
		"parallel_tool_calls": true, "tool_choice": "auto", "tools": []any{}, "text": map[string]any{"format": map[string]any{"type": "text"}}}
	for _, k := range []string{"service_tier", "previous_response_id", "instructions", "metadata", "store", "temperature", "max_output_tokens", "reasoning", "truncation", "user"} {
		if v, ok := t.req[k]; ok {
			obj[k] = v
		}
	}
	pricingPreset(t.sc.PricingPreset, obj, false)
	if withUsage && !t.sc.NoUsage {
		obj["usage"] = responsesUsage(t.usage, t.flavor)
	}
	return obj
}

// events returns the full event list of a successful turn plus the final
// response object.
func (t *responsesTurn) events() ([]map[string]any, map[string]any) {
	var evs []map[string]any
	add := func(m map[string]any) {
		m["sequence_number"] = t.seq
		t.seq++
		evs = append(evs, m)
	}
	add(map[string]any{"type": "response.created", "response": t.object("in_progress", []any{}, false)})
	add(map[string]any{"type": "response.in_progress", "response": t.object("in_progress", []any{}, false)})
	output := []any{}
	if !t.generate {
		final := t.object("completed", output, true)
		add(map[string]any{"type": "response.completed", "response": final})
		return evs, final
	}
	idx := 0
	if t.usage.Reasoning > 0 {
		rs := map[string]any{"id": "rs_" + t.id, "type": "reasoning", "summary": []any{}}
		add(map[string]any{"type": "response.output_item.added", "output_index": idx, "item": rs})
		add(map[string]any{"type": "response.output_item.done", "output_index": idx, "item": rs})
		output = append(output, rs)
		idx++
	}
	text := replyText(t.s.opts.Name, t.raw, t.sc.Text)
	itemID := "msg_" + t.id
	part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}
	add(map[string]any{"type": "response.output_item.added", "output_index": idx,
		"item": map[string]any{"id": itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}})
	add(map[string]any{"type": "response.content_part.added", "item_id": itemID, "output_index": idx, "content_index": 0, "part": part})
	for _, piece := range splitText(text, t.sc.Chunks) {
		add(map[string]any{"type": "response.output_text.delta", "item_id": itemID, "output_index": idx, "content_index": 0, "delta": piece, "logprobs": []any{}})
	}
	donePart := map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}
	add(map[string]any{"type": "response.output_text.done", "item_id": itemID, "output_index": idx, "content_index": 0, "text": text, "logprobs": []any{}})
	add(map[string]any{"type": "response.content_part.done", "item_id": itemID, "output_index": idx, "content_index": 0, "part": donePart})
	msg := map[string]any{"id": itemID, "type": "message", "status": "completed", "role": "assistant", "content": []any{donePart}}
	add(map[string]any{"type": "response.output_item.done", "output_index": idx, "item": msg})
	output = append(output, msg)
	final := t.object("completed", output, true)
	add(map[string]any{"type": "response.completed", "response": final})
	return evs, final
}

// failedEvent is response.failed carrying the scenario's error.
func (t *responsesTurn) failedEvent() map[string]any {
	e := streamFailure(t.sc, t.flavor, 500)
	obj := t.object("failed", []any{}, true)
	obj["error"] = map[string]any{"code": firstNonEmpty(e.code, "server_error"), "message": e.message}
	return map[string]any{"type": "response.failed", "response": obj, "sequence_number": t.seq}
}

// previousKnown validates previous_response_id against stored responses.
func (s *Server) previousKnown(req map[string]any, local map[string]bool) (string, bool) {
	prev := toString(req["previous_response_id"])
	if prev == "" || local[prev] {
		return prev, true
	}
	_, ok := s.recall(prev)
	return prev, ok
}

func previousNotFound(prev string) apiError {
	return apiError{status: 400, code: "previous_response_not_found", errType: "invalid_request_error",
		message: "Previous response with id '" + prev + "' not found."}
}

func (s *Server) serveResponses(c *call) {
	if c.flavor == FlavorOpenRouter && (c.req["previous_response_id"] != nil || c.req["store"] == true) {
		c.fail(apiError{status: 400, message: "OpenRouter Responses API is stateless", errType: "invalid_request_error"}, wireOpenAI)
		return
	}
	if prev, ok := s.previousKnown(c.req, nil); !ok {
		c.fail(previousNotFound(prev), wireOpenAI)
		return
	}
	t := s.newTurn(c, c.req, c.raw, c.sc)
	evs, final := t.events()
	if c.req["store"] != false {
		s.remember(t.id, mustJSON(final))
	}
	if !c.stream {
		c.rec.Usage = &t.usage
		if c.req["background"] == true {
			queued := t.object("queued", []any{}, false)
			c.ok(200, queued)
			return
		}
		c.ok(200, final)
		return
	}
	st := c.startSSE()
	deltas := 0
	for _, ev := range evs {
		if ev["type"] == "response.output_text.delta" {
			if c.sc.StreamError > 0 && deltas >= c.sc.StreamError {
				st.event("response.failed", t.failedEvent())
				st.done("stream_error", nil)
				return
			}
			deltas++
		}
		st.event(toString(ev["type"]), ev)
	}
	if c.sc.NoUsage {
		st.done("ok", nil)
		return
	}
	st.done("ok", &t.usage)
}

func (s *Server) serveResponseGet(c *call, path string) {
	id := path[strings.LastIndex(path, "/")+1:]
	if strings.HasSuffix(strings.ToLower(path), "/input_items") {
		trimmed := strings.TrimSuffix(path, "/input_items")
		id = trimmed[strings.LastIndex(trimmed, "/")+1:]
		if _, ok := s.recall(id); !ok {
			c.fail(apiError{status: 404, code: "not_found", message: "response not found"}, wireOpenAI)
			return
		}
		c.ok(200, map[string]any{"object": "list", "data": []any{}, "has_more": false})
		return
	}
	b, ok := s.recall(id)
	if !ok {
		c.fail(apiError{status: 404, code: "not_found", message: "No response found with id '" + id + "'."}, wireOpenAI)
		return
	}
	var obj map[string]any
	_ = json.Unmarshal(b, &obj)
	if strings.HasSuffix(strings.ToLower(c.r.URL.Path), "/cancel") {
		obj["status"] = "cancelled"
	}
	c.ok(200, obj)
}

// --- WebSocket -----------------------------------------------------------------

var upgrader = websocket.Upgrader{
	ReadBufferSize: 64 << 10, WriteBufferSize: 64 << 10,
	CheckOrigin: func(*http.Request) bool { return true },
}

// serveResponsesWS implements the Responses WebSocket mode: each text frame
// {"type":"response.create",...} produces the same events as the SSE stream.
// A connection may only continue responses it produced or stored ones.
func (s *Server) serveResponsesWS(c *call) {
	if c.sc.WSStatus != 0 {
		e, _ := resolveError(&Scenario{Status: c.sc.WSStatus}, c.flavor)
		c.fail(e, wireOpenAI)
		return
	}
	if c.sc.Status != 0 || c.sc.Preset != "" {
		// A handshake-level failure (e.g. 401/429 before upgrading).
		e, err := resolveError(c.sc, c.flavor)
		if err != nil {
			e = apiError{status: 400, message: err.Error()}
		}
		c.fail(e, wireOpenAI)
		return
	}
	var responseHeader http.Header
	if s.opts.LegacyMock {
		responseHeader = http.Header{"X-Mock-Instance": {s.opts.Name}}
	}
	conn, err := upgrader.Upgrade(c.w, c.r, responseHeader)
	if err != nil {
		c.rec.Status, c.rec.Outcome = 400, "error"
		s.record(c.rec)
		return
	}
	defer conn.Close()
	conn.SetReadLimit(2 << 20)
	_, key := authOf(c.r)
	c.rec.WebSocket, c.rec.Status, c.rec.Outcome = true, 101, "upgraded"
	s.record(c.rec)
	s.mu.Lock()
	s.stats.WSConnections++
	s.stats.WSActive++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.stats.WSActive--
		s.mu.Unlock()
	}()

	local := map[string]bool{}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(s.opts.WSIdleTimeout))
		mt, payload, err := conn.ReadMessage()
		if err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				return
			}
			return
		}
		if mt != websocket.TextMessage {
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseUnsupportedData, "text frames only"), time.Now().Add(time.Second))
			return
		}
		if !s.serveWSTurn(c, conn, key, payload, local) {
			return
		}
	}
}

func wsError(conn *websocket.Conn, e apiError) error {
	return conn.WriteJSON(map[string]any{"type": "error", "status": e.status,
		"error": map[string]any{"type": e.errType, "code": e.code, "message": e.message, "param": nil}})
}

// serveWSTurn handles one response.create; false closes the connection.
func (s *Server) serveWSTurn(parent *call, conn *websocket.Conn, key string, payload []byte, local map[string]bool) bool {
	var req map[string]any
	if json.Unmarshal(payload, &req) != nil || req == nil {
		return wsError(conn, apiError{status: 400, code: "invalid_json", errType: "invalid_request_error", message: "expected a JSON object"}) == nil
	}
	if toString(req["type"]) != "response.create" {
		return wsError(conn, apiError{status: 400, code: "unsupported_event", errType: "invalid_request_error", message: "only response.create events are supported"}) == nil
	}
	directive, derr := FindDirective(payload)
	if derr != nil {
		return wsError(conn, apiError{status: 400, code: "fake_directive", errType: "invalid_request_error", message: derr.Error()}) == nil
	}
	// Real upstream WebSocket mode (verified against the Codex CLI proxy on
	// 2026-10-12) rejects the string shorthand that HTTP Responses accepts.
	if _, isString := req["input"].(string); isString && !s.opts.LegacyMock {
		return wsError(conn, apiError{status: 400, errType: "invalid_request_error", message: "websocket request requires array field: input"}) == nil
	}
	var sc *Scenario
	if s.opts.LegacyMock {
		// Python /_control faults and delays affect HTTP POST, not WS turns.
		sc = &Scenario{}
		sc.merge(directive)
	} else {
		sc = s.takeScenario(http.MethodPost, key, directive)
	}
	model := firstNonEmpty(toString(req["model"]), s.opts.Model)
	tier := toString(req["service_tier"])
	rec := parent.rec
	rec.Seq, rec.Time, rec.WebSocket, rec.Model, rec.ServiceTier = s.seq.Add(1), time.Now(), true, model, tier
	rec.Previous, rec.BodyBytes, rec.Stream, rec.Usage = toString(req["previous_response_id"]), len(payload), true, nil
	rec.TopMembers = topMembers(payload)
	s.mu.Lock()
	s.stats.WSRequests++
	s.stats.WSTiers[tier]++
	s.stats.WSModels[model]++
	s.mu.Unlock()

	if sc.DelayMS > 0 {
		time.Sleep(time.Duration(sc.DelayMS) * time.Millisecond)
	}
	fail := func(e apiError) bool {
		rec.Status, rec.Outcome = e.status, "error"
		s.record(rec)
		return wsError(conn, e) == nil
	}
	if sc.Status != 0 || sc.Preset != "" || sc.WSErrorFrame {
		e, err := resolveError(&Scenario{Status: sc.Status, Preset: sc.Preset, ErrCode: sc.ErrCode, ErrType: sc.ErrType, ErrMessage: sc.ErrMessage}, parent.flavor)
		if err != nil {
			e = apiError{status: 400, message: err.Error()}
		}
		return fail(e)
	}
	if sc.WSTerminalResponse != "" && !json.Valid([]byte(sc.WSTerminalResponse)) {
		return fail(apiError{status: 400, code: "fake_directive", errType: "invalid_request_error", message: "ws_terminal_response must be valid JSON"})
	}
	prev, ok := s.previousKnown(req, local)
	if s.opts.LegacyMock && prev != "" {
		ok = local[prev]
	}
	if !ok {
		if s.opts.LegacyMock {
			return fail(apiError{status: 409, code: "invalid_previous_response_id", errType: "mock_error", message: "previous response is not from this connection"})
		}
		return fail(previousNotFound(prev))
	}
	c := &call{s: s, w: parent.w, r: parent.r, raw: payload, req: req, sc: sc, flavor: parent.flavor, rec: rec}
	t := s.newTurn(c, req, payload, sc)
	evs, final := t.events()
	deltas := 0
	for i, ev := range evs {
		if sc.WSCloseAfter > 0 && i >= sc.WSCloseAfter {
			code := firstInt(sc.WSCloseCode, websocket.CloseInternalServerErr)
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, "fake close"), time.Now().Add(time.Second))
			rec.Status, rec.Outcome = 200, "cut"
			s.record(rec)
			return false
		}
		if sc.CutAfter > 0 && i >= sc.CutAfter {
			rec.Status, rec.Outcome = 200, "cut"
			s.record(rec)
			return false // drop without a close frame
		}
		if ev["type"] == "response.output_text.delta" {
			if sc.StreamError > 0 && deltas >= sc.StreamError {
				_ = conn.WriteJSON(t.failedEvent())
				rec.Status, rec.Outcome = 200, "stream_error"
				s.record(rec)
				return true
			}
			deltas++
		}
		if i > 0 && sc.ChunkDelayMS > 0 {
			time.Sleep(time.Duration(sc.ChunkDelayMS) * time.Millisecond)
		}
		if i == len(evs)-1 {
			// Journal and store before the terminal frame so an observer
			// that saw the turn finish also sees its record.
			if s.opts.LegacyMock {
				clear(local) // the Python mock only accepts the most recent turn
			}
			local[t.id] = true
			if req["store"] != false {
				s.remember(t.id, mustJSON(final))
			}
			rec.Status, rec.Outcome, rec.Usage = 200, "ok", &t.usage
			s.record(rec)
		}
		if ev["type"] == "response.completed" && sc.WSTerminalResponse != "" {
			ev["response"] = json.RawMessage(sc.WSTerminalResponse)
		}
		if err := conn.WriteJSON(ev); err != nil {
			return false
		}
	}
	return true
}
