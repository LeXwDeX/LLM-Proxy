package fakellm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures a deterministic Server. LegacyMock opts into the
// legacy mock-provider acceptance fixtures without changing in-process test defaults.
type Options struct {
	Name          string        // reported in X-Mock-Instance and reply text
	Model         string        // model listed by GET /models (default mock-model)
	DefaultDelay  time.Duration // global delay before every reply (POST only in LegacyMock)
	WSIdleTimeout time.Duration // idle read timeout of a WebSocket (default 15s)
	JournalSize   int           // recorded requests kept for /_requests (default 5000)
	LegacyMock    bool          // opt-in legacy mock-provider acceptance fixtures; standalone tests only
}

// Server is the fake provider. It is safe for concurrent use.
type Server struct {
	opts Options
	seq  atomic.Int64

	mu     sync.Mutex
	mode   string
	global *Scenario
	// legacyPostOnly mirrors the legacy mock provider: its /_control modes only
	// affect POST, so GET /models (the key wake-up probe) keeps answering.
	legacyPostOnly bool
	keyRules       map[string]*Scenario
	stats          stats
	journal        []Record
	store          map[string][]byte // stored Responses objects, by id
	order          []string
}

type stats struct {
	Total         int            `json:"total"`
	Success       int            `json:"success"`
	Failed        int            `json:"failed"`
	Paths         map[string]int `json:"paths"`
	WSConnections int            `json:"ws_connections"`
	WSRequests    int            `json:"ws_requests"`
	WSActive      int            `json:"ws_active"`
	WSTiers       map[string]int `json:"ws_tiers"`
	WSModels      map[string]int `json:"ws_models"`
	WSHeaderOK    int            `json:"ws_header_ok"`
	HTTPHeaderOK  int            `json:"http_header_ok"`
	HTTPTiers     map[string]int `json:"http_tiers"`
	HTTPModels    map[string]int `json:"http_models"`
	Keys          map[string]int `json:"keys"`
	Flavors       map[string]int `json:"flavors"`
}

func newStats() stats {
	zero := func(keys ...string) map[string]int {
		m := map[string]int{}
		for _, k := range keys {
			m[k] = 0
		}
		return m
	}
	// Pre-seeded keys keep legacy mock-provider consumers' lookups valid.
	return stats{
		Paths:      map[string]int{},
		WSTiers:    zero("default", "auto", "flex", "priority", "ultrafast"),
		WSModels:   zero("mock-model", "ws-smoke"),
		HTTPTiers:  zero("default", "priority", "ultrafast"),
		HTTPModels: zero("mock-model", "ws-smoke"),
		Keys:       map[string]int{},
		Flavors:    map[string]int{},
	}
}

// Record is one request as the fake saw it. Keys are stored as fingerprints.
// TopMember is one top-level request member as received.
type TopMember struct {
	Key   string `json:"key"`
	Value string `json:"value"` // raw JSON value; for forms, the decoded field text
}

// recordedTopMembers are matched case-insensitively (Unicode folding).
var recordedTopMembers = []string{"model", "service_tier", "speed", "stream", "type", "previous_response_id", "input", "max_output_tokens", "n", "size", "stream_options", "prompt_cache_options", "system"}

// requestTopMembers records multipart form fields (every part, in order, text
// as received), urlencoded pairs or JSON top-level members, by the request's
// content type.
func requestTopMembers(r *http.Request, raw []byte) []TopMember {
	ct, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/x-www-form-urlencoded" {
		// A JSON object sent as urlencoded (curl -d) is JSON, as the proxy reads it.
		if trimmed := bytes.TrimLeft(raw, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(raw) {
			return topMembers(raw)
		}
		return formTopMembers(raw)
	}
	if !strings.HasPrefix(ct, "multipart/") || params["boundary"] == "" {
		return topMembers(raw)
	}
	var found []TopMember
	reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	for {
		part, err := reader.NextPart()
		if err != nil {
			return found
		}
		value, _ := io.ReadAll(io.LimitReader(part, 1<<20))
		for _, name := range recordedTopMembers {
			if strings.EqualFold(part.FormName(), name) {
				found = append(found, TopMember{Key: part.FormName(), Value: string(value)})
				break
			}
		}
	}
}

// formTopMembers records urlencoded pairs in order, keys and values decoded,
// skipping pairs that do not decode.
func formTopMembers(raw []byte) []TopMember {
	var found []TopMember
	for _, pair := range strings.Split(string(raw), "&") {
		rawKey, rawValue, _ := strings.Cut(pair, "=")
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			continue
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			continue
		}
		for _, name := range recordedTopMembers {
			if strings.EqualFold(key, name) {
				found = append(found, TopMember{Key: key, Value: value})
				break
			}
		}
	}
	return found
}

// topMembers walks the top-level object of raw and returns the members whose
// decoded key folds to one of recordedTopMembers, duplicates included.
func topMembers(raw []byte) []TopMember {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil
	}
	var found []TopMember
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return found
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return found
		}
		key, _ := token.(string)
		for _, name := range recordedTopMembers {
			if strings.EqualFold(key, name) {
				found = append(found, TopMember{Key: key, Value: string(value)})
				break
			}
		}
	}
	return found
}

type Record struct {
	Seq         int64             `json:"seq"`
	Time        time.Time         `json:"time"`
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	Query       string            `json:"query,omitempty"`
	Flavor      string            `json:"flavor"`
	Auth        string            `json:"auth"` // bearer, x-api-key, api-key, x-goog-api-key, query, none
	Key         string            `json:"key,omitempty"`
	Model       string            `json:"model,omitempty"`
	Stream      bool              `json:"stream,omitempty"`
	WebSocket   bool              `json:"websocket,omitempty"`
	ServiceTier string            `json:"service_tier,omitempty"`
	Previous    string            `json:"previous_response_id,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	BodyBytes   int               `json:"body_bytes"`
	// TopMembers lists, in wire order, the raw top-level request members
	// whose key folds to a routing name (model, service_tier, ...). Tests
	// use it to prove upstream saw exactly one value, whatever the parser.
	TopMembers []TopMember `json:"top_members,omitempty"`
	Status     int         `json:"status"`
	Outcome    string      `json:"outcome"` // ok, error, abort, cut, stream_error
	Usage      *Usage      `json:"usage,omitempty"`
}

// KeyFingerprint is how keys appear in records and stats.
func KeyFingerprint(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:6])
}

// New creates a Server.
func New(opts Options) *Server {
	if opts.Name == "" {
		opts.Name = "fake"
	}
	if opts.Model == "" {
		opts.Model = "mock-model"
	}
	if opts.WSIdleTimeout <= 0 {
		opts.WSIdleTimeout = 15 * time.Second
	}
	if opts.JournalSize <= 0 {
		opts.JournalSize = 5000
	}
	s := &Server{opts: opts, mode: "ok", keyRules: map[string]*Scenario{}, stats: newStats(), store: map[string][]byte{}}
	return s
}

// SetScenario installs the global scenario (nil clears it).
func (s *Server) SetScenario(sc *Scenario) {
	s.mu.Lock()
	s.global, s.legacyPostOnly = sc, false
	s.mu.Unlock()
}

// SetKeyScenario installs a rule for requests authenticated with key.
func (s *Server) SetKeyScenario(key string, sc *Scenario) {
	s.mu.Lock()
	if sc == nil {
		delete(s.keyRules, key)
	} else {
		s.keyRules[key] = sc
	}
	s.mu.Unlock()
}

// Reset clears scenarios, counters, the journal and stored responses.
func (s *Server) Reset() {
	s.mu.Lock()
	s.mode, s.global, s.legacyPostOnly = "ok", nil, false
	s.keyRules = map[string]*Scenario{}
	s.stats = newStats()
	s.journal = nil
	s.store = map[string][]byte{}
	s.order = nil
	s.mu.Unlock()
}

// Requests returns a copy of the journal, oldest first.
func (s *Server) Requests() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.journal...)
}

// LastSeq is a cursor for RequestsSince; it is stable when the journal ring
// drops old entries, unlike slice indices.
func (s *Server) LastSeq() int64 { return s.seq.Load() }

// RequestsSince returns journal entries with Seq greater than seq.
func (s *Server) RequestsSince(seq int64) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for _, r := range s.journal {
		if r.Seq > seq {
			out = append(out, r)
		}
	}
	return out
}

func (s *Server) record(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.journal = append(s.journal, r)
	if over := len(s.journal) - s.opts.JournalSize; over > 0 {
		s.journal = s.journal[over:]
	}
	if s.opts.LegacyMock && r.Method != http.MethodPost && !r.WebSocket {
		// Python counts POSTs and upgraded connections, not GET probes.
		return
	}
	switch r.Outcome {
	case "ok":
		s.stats.Success++
	case "upgraded", "canceled":
	default:
		s.stats.Failed++
	}
}

// takeScenario resolves the effective scenario: global, then key, then the
// per-request directive. Rules with Times are consumed here.
func (s *Server) takeScenario(method, key string, directive *Scenario) *Scenario {
	eff := &Scenario{}
	s.mu.Lock()
	if s.opts.DefaultDelay > 0 && (!s.opts.LegacyMock || method == http.MethodPost) {
		eff.DelayMS = int(s.opts.DefaultDelay / time.Millisecond)
	}
	consume := func(sc *Scenario, drop func()) {
		if sc == nil {
			return
		}
		eff.merge(sc)
		if sc.Times > 0 {
			sc.Times--
			if sc.Times == 0 {
				drop()
			}
		}
	}
	if !s.legacyPostOnly || method == http.MethodPost {
		consume(s.global, func() { s.global = nil })
	}
	if s.opts.LegacyMock && s.legacyPostOnly && method == http.MethodPost && s.mode == "timeout" {
		// The old timeout sleeps five seconds *after* the configured delay.
		eff.DelayMS += int(s.opts.DefaultDelay / time.Millisecond)
	}
	if key != "" {
		consume(s.keyRules[key], func() { delete(s.keyRules, key) })
	}
	s.mu.Unlock()
	eff.merge(directive)
	return eff
}

func (s *Server) remember(id string, obj []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.store[id]; !ok {
		s.order = append(s.order, id)
	}
	s.store[id] = obj
	if len(s.order) > 2000 {
		delete(s.store, s.order[0])
		s.order = s.order[1:]
	}
}

func (s *Server) recall(id string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.store[id]
	return b, ok
}

func (s *Server) forget(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.store[id]
	delete(s.store, id)
	return ok
}

func (s *Server) newID(prefix string) string {
	return fmt.Sprintf("%s-fake-%d-%d", prefix, time.Now().UnixNano(), s.seq.Add(1))
}

// --- request context --------------------------------------------------------

// call is one in-flight request after parsing.
type call struct {
	s      *Server
	w      http.ResponseWriter
	r      *http.Request
	raw    []byte
	req    map[string]any
	sc     *Scenario
	flavor string
	rec    Record
	model  string
	stream bool
}

func authOf(r *http.Request) (scheme, key string) {
	if v := r.Header.Get("Authorization"); v != "" {
		if after, ok := strings.CutPrefix(v, "Bearer "); ok {
			return "bearer", strings.TrimSpace(after)
		}
		return "authorization", v
	}
	for _, h := range []string{"x-api-key", "api-key", "x-goog-api-key"} {
		if v := r.Header.Get(h); v != "" {
			return h, v
		}
	}
	if v := r.URL.Query().Get("key"); v != "" {
		return "query", v
	}
	return "none", ""
}

// recordedHeaders are copied into the journal so tests can assert on what
// the proxy forwarded. Credentials are never recorded.
var recordedHeaders = []string{"anthropic-version", "anthropic-beta", "openai-beta", "x-request-id",
	"x-ws-smoke", "x-dashscope-session-cache", "http-referer", "x-title", "content-type", "accept-encoding", "user-agent"}

var geminiRe = regexp.MustCompile(`/models/([^/:]+):([A-Za-z]+)$`)

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch path {
	case "/_stats":
		s.serveStats(w)
		return
	case "/_control":
		s.serveControl(w, r)
		return
	case "/_requests":
		writeJSON(w, 200, s.Requests(), nil)
		return
	case "/_reset":
		s.Reset()
		writeJSON(w, 200, map[string]any{"instance": s.opts.Name, "reset": true}, nil)
		return
	case "/healthz":
		writeJSON(w, 200, map[string]string{"status": "ok"}, nil)
		return
	}
	w.Header().Set("X-Mock-Instance", s.opts.Name)
	w.Header().Set("X-Request-Id", s.newID("req"))

	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"message": "read body: " + err.Error()}}, nil)
		return
	}
	scheme, key := authOf(r)
	c := &call{s: s, w: w, r: r, raw: raw, req: parseBody(r, raw)}
	c.model, _ = c.req["model"].(string)
	if m := geminiRe.FindStringSubmatch(path); m != nil {
		c.model = m[1]
	}
	c.stream, _ = c.req["stream"].(bool)
	if strings.EqualFold(r.URL.Query().Get("alt"), "sse") || strings.HasSuffix(path, ":streamGenerateContent") {
		c.stream = true
	}

	directive, derr := FindDirective(raw)
	if derr == nil && directive == nil && r.Header.Get("X-Fake-Scenario") != "" {
		directive, derr = ParseDirective(r.Header.Get("X-Fake-Scenario"))
	}
	c.sc = s.takeScenario(r.Method, key, directive)
	finishPreset := applyFinishPreset(c.sc)
	c.flavor = c.sc.Flavor
	if c.flavor == "" {
		c.flavor = flavorForPath(path)
	}
	tier, _ := c.req["service_tier"].(string)
	prev, _ := c.req["previous_response_id"].(string)
	c.rec = Record{Seq: s.seq.Add(1), Time: time.Now(), Method: r.Method, Path: path, Query: redactQuery(r),
		Flavor: c.flavor, Auth: scheme, Key: KeyFingerprint(key), Model: c.model, Stream: c.stream,
		ServiceTier: tier, Previous: prev, BodyBytes: len(raw), TopMembers: requestTopMembers(r, raw), Headers: map[string]string{}}
	for _, h := range recordedHeaders {
		if v := r.Header.Get(h); v != "" {
			c.rec.Headers[h] = v
		}
	}

	s.mu.Lock()
	if !s.opts.LegacyMock || r.Method == http.MethodPost || isUpgrade(r) {
		s.stats.Total++
		s.stats.Paths[path]++
		s.stats.Keys[c.rec.Key]++
		s.stats.Flavors[c.flavor]++
		if r.Header.Get("X-Ws-Smoke") == "yes" {
			if isUpgrade(r) {
				s.stats.WSHeaderOK++
			} else {
				s.stats.HTTPHeaderOK++
			}
		}
		if !isUpgrade(r) && r.Method == http.MethodPost {
			if tier != "" {
				s.stats.HTTPTiers[tier]++
			}
			if c.model != "" {
				s.stats.HTTPModels[c.model]++
			}
		}
	}
	s.mu.Unlock()

	if derr != nil {
		c.fail(apiError{status: 400, errType: "invalid_request_error", code: "fake_directive", message: derr.Error()}, wireOf(path))
		return
	}
	// Reply presets model one documented Chat Completions contract only.
	if finishPreset != "" && (isUpgrade(r) || !strings.HasSuffix(strings.ToLower(path), "/chat/completions")) {
		c.fail(apiError{status: 400, errType: "invalid_request_error", code: "fake_directive", message: "fakellm: preset " + finishPreset + " only applies to Chat Completions"}, wireOf(path))
		return
	}

	if isUpgrade(r) {
		if strings.HasSuffix(path, "/responses") {
			s.serveResponsesWS(c)
			return
		}
		c.fail(apiError{status: 404, message: "websocket not supported on " + path}, wireOpenAI)
		return
	}

	if c.sc.DelayMS > 0 {
		select {
		case <-time.After(time.Duration(c.sc.DelayMS) * time.Millisecond):
		case <-r.Context().Done():
			c.rec.Outcome = "canceled"
			s.record(c.rec)
			return
		}
	}
	if c.sc.Abort {
		c.rec.Outcome = "abort"
		s.record(c.rec)
		panic(http.ErrAbortHandler)
	}
	if err := validateResponseEncoding(c.sc); err != nil {
		c.fail(apiError{status: 400, code: "fake_directive", errType: "invalid_request_error", message: err.Error()}, wireOf(path))
		return
	}
	encoded, err := newEncodedResponseWriter(w, c.sc)
	if err != nil {
		c.fail(apiError{status: 500, message: err.Error()}, wireOf(path))
		return
	}
	c.w = encoded
	// Cleanup on every exit, including panic; release detaches the sink so it
	// never supplies an encoded trailer to a failed transport.
	defer encoded.release()
	if c.sc.StreamError == 0 && (c.sc.Status != 0 || (c.sc.Preset != "" && !c.sc.SSEError)) {
		e, err := resolveError(c.sc, c.flavor)
		if err != nil {
			e = apiError{status: 400, errType: "invalid_request_error", code: "fake_directive", message: err.Error()}
		}
		if c.sc.SSEError {
			c.sseError(e)
			encoded.finish(r)
			return
		}
		c.fail(e, wireOf(path))
		encoded.finish(r)
		return
	}
	if c.sc.SSEError {
		e, _ := resolveError(&Scenario{Status: 429, Preset: firstNonEmpty(c.sc.Preset, "glm_1302")}, c.flavor)
		c.sseError(e)
		encoded.finish(r)
		return
	}
	s.dispatch(c, path)
	encoded.finish(r)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func isUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func wireOf(path string) string {
	p := strings.ToLower(path)
	switch {
	case geminiRe.MatchString(path):
		return wireGemini
	case strings.Contains(p, "/messages"):
		return wireAnthropic
	}
	return wireOpenAI
}

func (s *Server) dispatch(c *call, path string) {
	p := strings.ToLower(path)
	r := c.r
	if m := geminiRe.FindStringSubmatch(path); m != nil && r.Method == http.MethodPost {
		s.serveGemini(c, m[2])
		return
	}
	switch r.Method {
	case http.MethodGet:
		switch {
		case strings.HasSuffix(p, "/responses"):
			c.fail(apiError{status: 426, code: "upgrade_required", errType: "invalid_request_error", message: "WebSocket upgrade required"}, wireOpenAI)
		case strings.Contains(p, "/responses/"):
			s.serveResponseGet(c, path)
		case strings.HasSuffix(p, "/models") || strings.Contains(p, "/models/"):
			s.serveModels(c, path)
		case p == "/" || p == "" || strings.HasSuffix(p, "/openai"):
			c.ok(200, map[string]any{"object": "fake", "status": "ok"})
		default:
			c.fail(apiError{status: 404, code: "not_found", message: "fake: no route for GET " + path}, wireOf(path))
		}
	case http.MethodDelete:
		if strings.Contains(p, "/responses/") {
			id := path[strings.LastIndex(path, "/")+1:]
			if s.forget(id) {
				c.ok(200, map[string]any{"id": id, "object": "response.deleted", "deleted": true})
			} else {
				c.fail(apiError{status: 404, code: "not_found", message: "response not found"}, wireOpenAI)
			}
			return
		}
		c.ok(200, map[string]any{"deleted": true})
	case http.MethodPost:
		switch {
		case strings.HasSuffix(p, "/messages/count_tokens"):
			c.ok(200, map[string]any{"input_tokens": EstimateTokens(promptText(c.req))})
		case strings.HasSuffix(p, "/messages"):
			s.serveMessages(c)
		case strings.HasSuffix(p, "/chat/completions"):
			s.serveChat(c)
		case strings.HasSuffix(p, "/responses/input_tokens"):
			c.ok(200, map[string]any{"object": "response.input_tokens", "input_tokens": EstimateTokens(promptText(c.req))})
		case strings.HasSuffix(p, "/cancel") && strings.Contains(p, "/responses/"):
			s.serveResponseGet(c, strings.TrimSuffix(path, "/cancel"))
		case strings.HasSuffix(p, "/responses"):
			s.serveResponses(c)
		case strings.HasSuffix(p, "/completions"):
			s.serveCompletions(c)
		case strings.HasSuffix(p, "/embeddings"):
			s.serveEmbeddings(c)
		case strings.HasSuffix(p, "/images/generations"), strings.HasSuffix(p, "/images/edits"), strings.HasSuffix(p, "/images/variations"):
			s.serveImages(c)
		case strings.HasSuffix(p, "/audio/speech"):
			c.raw200("audio/mpeg", []byte("ID3fake-audio"))
		case strings.HasSuffix(p, "/audio/transcriptions"), strings.HasSuffix(p, "/audio/translations"):
			u := computeUsage("", 0, c.sc.Usage)
			c.ok(200, map[string]any{"text": replyText(s.opts.Name, c.raw, c.sc.Text),
				"usage": map[string]any{"type": "tokens", "input_tokens": u.Input, "output_tokens": u.Output, "total_tokens": u.Total(),
					"input_token_details": map[string]any{"text_tokens": 0, "audio_tokens": u.Input}}})
		case strings.HasSuffix(p, "/moderations"):
			c.ok(200, map[string]any{"id": s.newID("modr"), "model": c.model, "results": []any{map[string]any{"flagged": false, "categories": map[string]any{}, "category_scores": map[string]any{}}}})
		default:
			// Any other OpenAI-compatible endpoint (files, batches, ...) gets a
			// generic object so passthrough routes can be exercised.
			c.ok(200, map[string]any{"id": s.newID("obj"), "object": "fake.generic", "path": path})
		}
	default:
		c.fail(apiError{status: 405, message: "method not allowed"}, wireOf(path))
	}
}

func (s *Server) serveModels(c *call, path string) {
	if geminiish := strings.Contains(path, "/v1beta/") || c.flavor == FlavorGemini; geminiish {
		c.ok(200, map[string]any{"models": []any{map[string]any{"name": "models/" + s.opts.Model, "displayName": s.opts.Model,
			"supportedGenerationMethods": []string{"generateContent", "streamGenerateContent", "countTokens", "embedContent"}}}})
		return
	}
	model := map[string]any{"id": s.opts.Model, "object": "model", "created": 1700000000, "owned_by": "fake",
		"type": "model", "display_name": s.opts.Model, "created_at": "2025-01-01T00:00:00Z"}
	if i := strings.Index(path, "/models/"); i >= 0 {
		model["id"] = path[i+len("/models/"):]
		c.ok(200, model)
		return
	}
	c.ok(200, map[string]any{"object": "list", "data": []any{model}, "has_more": false,
		"first_id": s.opts.Model, "last_id": s.opts.Model})
}

func (s *Server) serveControl(w http.ResponseWriter, r *http.Request) {
	var cmd struct {
		Mode     *string              `json:"mode"`
		DelayMS  *int                 `json:"delay_ms"`
		Scenario *Scenario            `json:"scenario"`
		Clear    bool                 `json:"clear"`
		Keys     map[string]*Scenario `json:"keys"`
	}
	if r.Method == http.MethodPost {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if len(bytes.TrimSpace(body)) > 0 {
			if err := json.Unmarshal(body, &cmd); err != nil {
				writeJSON(w, 400, map[string]any{"error": err.Error()}, nil)
				return
			}
		}
	}
	if err := errors.Join(validateResponseEncoding(cmd.Scenario), validateIterations(cmd.Scenario)); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()}, nil)
		return
	}
	for _, sc := range cmd.Keys {
		if err := errors.Join(validateResponseEncoding(sc), validateIterations(sc)); err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()}, nil)
			return
		}
	}
	s.mu.Lock()
	if cmd.Clear {
		s.mode, s.global, s.legacyPostOnly = "ok", nil, false
		s.keyRules = map[string]*Scenario{}
		s.opts.DefaultDelay = 0
	}
	if cmd.Mode != nil {
		sc, ok := legacyMode(*cmd.Mode)
		if !ok {
			s.mu.Unlock()
			writeJSON(w, 400, map[string]any{"error": "unknown mode " + *cmd.Mode}, nil)
			return
		}
		s.mode, s.global, s.legacyPostOnly = *cmd.Mode, sc, true
	}
	if cmd.DelayMS != nil {
		s.opts.DefaultDelay = time.Duration(*cmd.DelayMS) * time.Millisecond
	}
	if cmd.Scenario != nil {
		s.global, s.legacyPostOnly = cmd.Scenario, false
	}
	for k, sc := range cmd.Keys {
		if sc == nil {
			delete(s.keyRules, k)
		} else {
			s.keyRules[k] = sc
		}
	}
	s.mu.Unlock()
	s.serveStats(w)
}

func (s *Server) serveStats(w http.ResponseWriter) {
	s.mu.Lock()
	out := map[string]any{}
	b, _ := json.Marshal(s.stats)
	_ = json.Unmarshal(b, &out)
	out["instance"] = s.opts.Name
	out["mode"] = s.mode
	out["delay_ms"] = int(s.opts.DefaultDelay / time.Millisecond)
	keyRules := map[string]*Scenario{}
	for k, v := range s.keyRules {
		keyRules[KeyFingerprint(k)] = v
	}
	out["key_rules"] = keyRules
	out["scenario"] = s.global
	// Marshal while holding the lock: takeScenario mutates Times.
	body, _ := json.Marshal(out)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(body)
}

// --- response helpers --------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any, hdr map[string]string) {
	body, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	for k, val := range hdr {
		w.Header().Set(k, val)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (c *call) ok(status int, v any) {
	writeJSON(c.w, status, v, nil)
	c.rec.Status, c.rec.Outcome = status, "ok"
	c.s.record(c.rec)
}

func (c *call) raw200(contentType string, body []byte) {
	c.w.Header().Set("Content-Type", contentType)
	c.w.WriteHeader(200)
	_, _ = c.w.Write(body)
	c.rec.Status, c.rec.Outcome = 200, "ok"
	c.s.record(c.rec)
}

func (c *call) fail(e apiError, wire string) {
	if c.s.opts.LegacyMock {
		// Preserve the legacy control modes' envelopes, including Python's
		// OpenAI-shaped rate429 on otherwise native Anthropic/Gemini paths.
		// Explicit provider presets keep their normal envelopes.
		if e.status == 429 && e.message == "mock injected rate limit" {
			wire = wireOpenAI
		}
		if e.status == 500 && e.message == "mock injected failure" && wire == wireAnthropic {
			e.errType = "api_error"
		}
	}
	hdr := map[string]string{}
	if c.sc != nil && c.sc.RetryAfter != "" {
		hdr["Retry-After"] = c.sc.RetryAfter
	}
	c.w.Header().Set("Content-Type", firstNonEmpty(e.contentType, "application/json"))
	for k, v := range hdr {
		c.w.Header().Set(k, v)
	}
	c.w.WriteHeader(e.status)
	_, _ = c.w.Write(e.envelope(wire, c.w.Header().Get("X-Request-Id")))
	c.rec.Status, c.rec.Outcome = e.status, "error"
	c.s.record(c.rec)
}

func (c *call) usage() Usage {
	return computeUsage(promptText(c.req), intField(c.req, "max_tokens", "max_completion_tokens", "max_output_tokens"), c.sc.Usage)
}

// parseBody decodes JSON or multipart form fields into a generic map.
func parseBody(r *http.Request, raw []byte) map[string]any {
	out := map[string]any{}
	ct, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "multipart/") && params["boundary"] != "" {
		mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			if part.FileName() != "" {
				_, _ = io.Copy(io.Discard, part)
				out["_files"] = toInt(out["_files"]) + 1
				continue
			}
			v, _ := io.ReadAll(io.LimitReader(part, 1<<20))
			switch string(v) {
			case "true":
				out[part.FormName()] = true
			case "false":
				out[part.FormName()] = false
			default:
				out[part.FormName()] = string(v)
			}
		}
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case float64:
		return int(x)
	}
	return 0
}

// redactQuery drops credential query parameters (Gemini ?key=, Azure
// api-key) before the query is journaled.
func redactQuery(r *http.Request) string {
	q := r.URL.Query()
	for _, k := range []string{"key", "api-key", "api_key"} {
		if q.Has(k) {
			q.Set(k, "REDACTED")
		}
	}
	return q.Encode()
}
