package fakellm

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Scenario describes how one request is answered. The zero value is a normal
// successful reply. Fields are merged from the global, per-key and per-request
// layers; a later layer only overrides the fields it sets.
type Scenario struct {
	// Explicit HTTP response encoding; the default never negotiates Accept-Encoding.
	ResponseEncoding string `json:"response_encoding,omitempty"`
	// nil uses the normal encoding header; [] deliberately omits it. Values are
	// independent wire declarations for unsupported/multiple-header tests.
	ResponseEncodingHeaders []string `json:"response_encoding_headers,omitempty"`
	// truncated omits the trailer; corrupt prepends an invalid format prefix.
	ResponseEncodingFault string `json:"response_encoding_fault,omitempty"`
	// Flavor selects the provider dialect used for usage fields and error
	// envelopes: openai, azure, anthropic, gemini, glm, deepseek, qwen,
	// openrouter, kimi. Empty derives it from the path (see flavorForPath).
	Flavor string `json:"flavor,omitempty"`
	// PricingPreset supplies response billing evidence independently of the request.
	PricingPreset string `json:"pricing_preset,omitempty"`
	// Iterations adds an Anthropic usage.iterations array: "message" (one
	// executor item equal to top-level) or "advisor" (plus an advisor item).
	Iterations string `json:"iterations,omitempty"`

	// Status answers with this HTTP status and an error envelope instead of a
	// reply. Preset picks a provider-specific error body (see presets.go).
	Status     int    `json:"status,omitempty"`
	Preset     string `json:"preset,omitempty"`
	ErrCode    string `json:"code,omitempty"`
	ErrType    string `json:"type,omitempty"`
	ErrMessage string `json:"message,omitempty"`
	// Body replaces the whole error body verbatim.
	Body       string `json:"body,omitempty"`
	RetryAfter string `json:"retry_after,omitempty"`

	// Timing.
	DelayMS      int `json:"delay_ms,omitempty"`       // before the response header
	ChunkDelayMS int `json:"chunk_delay_ms,omitempty"` // between stream events
	// Abort drops the TCP connection without any response (a transport error).
	Abort bool `json:"abort,omitempty"`

	// Streams.
	Chunks int `json:"chunks,omitempty"` // text deltas per stream (default 3)
	// CutAfter drops the connection after this many stream events, without
	// a terminator, simulating a mid-generation failure.
	CutAfter int `json:"cut_after,omitempty"`
	// StreamError emits the dialect's in-stream error event after this many
	// deltas instead of finishing normally (OpenAI error chunk, Anthropic
	// event:error, Responses response.failed, OpenRouter finish_reason=error).
	StreamError int `json:"stream_error,omitempty"`
	// SSEError answers HTTP 200 with an SSE stream whose first event is an
	// error (GLM's historical rate-limit shape). Status/Preset/Code give the
	// embedded error.
	SSEError bool `json:"sse_error,omitempty"`
	// KeepAlive inserts provider keep-alive comment lines between events.
	KeepAlive bool `json:"keepalive,omitempty"`
	// Reasoning emits reasoning deltas (reasoning_content / thinking) first.
	Reasoning bool `json:"reasoning,omitempty"`

	// Content.
	Text string `json:"text,omitempty"`
	// Finish overrides the terminal finish reason (Chat finish_reason,
	// Anthropic stop_reason, Gemini finishReason), e.g. "sensitive",
	// "network_error", "insufficient_system_resource", "length".
	Finish string `json:"finish,omitempty"`

	// Usage overrides. Negative means "unset" when merging; see Usage.
	Usage *UsageSpec `json:"usage,omitempty"`
	// NoUsage omits the usage object entirely.
	NoUsage bool `json:"no_usage,omitempty"`

	// WebSocket (Responses).
	WSStatus     int  `json:"ws_status,omitempty"`      // reject the upgrade with this status
	WSErrorFrame bool `json:"ws_error_frame,omitempty"` // answer response.create with an error event
	WSCloseAfter int  `json:"ws_close_after,omitempty"` // close after N events of a turn
	WSCloseCode  int  `json:"ws_close_code,omitempty"`

	// WSTerminalResponse replaces only the WS terminal response with a JSON
	// value for protocol fault tests; not a vendor preset or HTTP behavior.
	WSTerminalResponse string `json:"ws_terminal_response,omitempty"`

	// Times limits a global or per-key scenario to the next N matching
	// requests; 0 means until replaced.
	Times int `json:"times,omitempty"`
}

// UsageSpec pins token counts. Nil pointers keep the deterministic default.
type UsageSpec struct {
	Input      *int `json:"in,omitempty"`        // total input, including cached and cache writes
	Output     *int `json:"out,omitempty"`       // total output, including reasoning
	CachedRead *int `json:"cached,omitempty"`    // cache hits (subset of Input)
	CacheWrite *int `json:"cw5m,omitempty"`      // 5-minute / default cache writes (subset of Input)
	CacheWr1h  *int `json:"cw1h,omitempty"`      // 1-hour cache writes (subset of Input)
	Reasoning  *int `json:"reasoning,omitempty"` // reasoning tokens (subset of Output)
	Images     *int `json:"images,omitempty"`    // generated images
}

func (s *Scenario) merge(o *Scenario) {
	if o == nil {
		return
	}
	str := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	num := func(dst *int, v int) {
		if v != 0 {
			*dst = v
		}
	}
	flag := func(dst *bool, v bool) {
		if v {
			*dst = true
		}
	}
	str(&s.Flavor, o.Flavor)
	str(&s.ResponseEncoding, o.ResponseEncoding)
	str(&s.ResponseEncodingFault, o.ResponseEncodingFault)
	if o.ResponseEncodingHeaders != nil {
		s.ResponseEncodingHeaders = append([]string{}, o.ResponseEncodingHeaders...)
	}
	str(&s.PricingPreset, o.PricingPreset)
	str(&s.Iterations, o.Iterations)
	num(&s.Status, o.Status)
	str(&s.Preset, o.Preset)
	str(&s.ErrCode, o.ErrCode)
	str(&s.ErrType, o.ErrType)
	str(&s.ErrMessage, o.ErrMessage)
	str(&s.Body, o.Body)
	str(&s.RetryAfter, o.RetryAfter)
	num(&s.DelayMS, o.DelayMS)
	num(&s.ChunkDelayMS, o.ChunkDelayMS)
	flag(&s.Abort, o.Abort)
	num(&s.Chunks, o.Chunks)
	num(&s.CutAfter, o.CutAfter)
	num(&s.StreamError, o.StreamError)
	flag(&s.SSEError, o.SSEError)
	flag(&s.KeepAlive, o.KeepAlive)
	flag(&s.Reasoning, o.Reasoning)
	str(&s.Text, o.Text)
	str(&s.Finish, o.Finish)
	flag(&s.NoUsage, o.NoUsage)
	num(&s.WSStatus, o.WSStatus)
	flag(&s.WSErrorFrame, o.WSErrorFrame)
	num(&s.WSCloseAfter, o.WSCloseAfter)
	num(&s.WSCloseCode, o.WSCloseCode)
	str(&s.WSTerminalResponse, o.WSTerminalResponse)
	if o.Usage != nil {
		if s.Usage == nil {
			s.Usage = &UsageSpec{}
		}
		u := s.Usage
		pick := func(dst **int, v *int) {
			if v != nil {
				x := *v
				*dst = &x
			}
		}
		pick(&u.Input, o.Usage.Input)
		pick(&u.Output, o.Usage.Output)
		pick(&u.CachedRead, o.Usage.CachedRead)
		pick(&u.CacheWrite, o.Usage.CacheWrite)
		pick(&u.CacheWr1h, o.Usage.CacheWr1h)
		pick(&u.Reasoning, o.Usage.Reasoning)
		pick(&u.Images, o.Usage.Images)
	}
}

// directiveRe finds "[[fake k=v k2=v2 flag]]". The syntax avoids quotes and
// backslashes so it survives JSON string escaping in any request schema.
var directiveRe = regexp.MustCompile(`\[\[fake(?::|\s)([^\]]*)\]\]`)

// FindDirective extracts the first directive from raw request bytes.
func FindDirective(raw []byte) (*Scenario, error) {
	m := directiveRe.FindSubmatch(raw)
	if m == nil {
		return nil, nil
	}
	return ParseDirective(string(m[1]))
}

// ParseDirective parses "k=v" tokens separated by spaces or commas. Boolean
// fields accept a bare name. Usage fields use the "usage." prefix, and
// "usage=none" omits usage. Unknown keys are an error so tests fail loudly.
func ParseDirective(text string) (*Scenario, error) {
	s := &Scenario{}
	for _, tok := range strings.FieldsFunc(text, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' }) {
		key, val, hasVal := strings.Cut(tok, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		// Spaces cannot appear in a value; "_" stands in for them in messages.
		atoi := func() (int, error) {
			n, err := strconv.Atoi(val)
			if err != nil {
				return 0, fmt.Errorf("fakellm directive %s=%q: %w", key, val, err)
			}
			return n, nil
		}
		boolean := func(dst *bool) error {
			if !hasVal {
				*dst = true
				return nil
			}
			b, err := strconv.ParseBool(val)
			*dst = b
			return err
		}
		var err error
		setInt := func(dst *int) { *dst, err = atoi() }
		setUsage := func(dst **int) {
			var n int
			if n, err = atoi(); err == nil {
				if s.Usage == nil {
					s.Usage = &UsageSpec{}
				}
				*dst = &n
			}
		}
		switch key {
		case "encoding":
			s.ResponseEncoding = val
		case "encoding_header":
			s.ResponseEncodingHeaders = []string{val}
		case "encoding_fault":
			s.ResponseEncodingFault = val
		case "flavor":
			s.Flavor = strings.ToLower(val)
		case "status":
			setInt(&s.Status)
		case "pricing":
			s.PricingPreset = val
		case "iterations":
			s.Iterations = val
			err = validateIterations(s)
		case "preset":
			s.Preset = strings.ToLower(val)
		case "code":
			s.ErrCode = val
		case "type":
			s.ErrType = val
		case "message", "msg":
			s.ErrMessage = strings.ReplaceAll(val, "_", " ")
		case "retry_after":
			s.RetryAfter = val
		case "delay_ms", "delay":
			setInt(&s.DelayMS)
		case "chunk_delay_ms", "chunk_delay":
			setInt(&s.ChunkDelayMS)
		case "abort":
			err = boolean(&s.Abort)
		case "chunks":
			setInt(&s.Chunks)
		case "cut_after", "cut":
			setInt(&s.CutAfter)
		case "stream_error":
			setInt(&s.StreamError)
		case "sse_error":
			err = boolean(&s.SSEError)
		case "keepalive":
			err = boolean(&s.KeepAlive)
		case "reasoning":
			err = boolean(&s.Reasoning)
		case "text":
			s.Text = strings.ReplaceAll(val, "_", " ")
		case "finish":
			s.Finish = val
		case "usage":
			if val != "none" {
				err = fmt.Errorf("fakellm directive usage=%q: only usage=none is supported", val)
			}
			s.NoUsage = true
		case "usage.in":
			setUsage(&s.ensureUsage().Input)
		case "usage.out":
			setUsage(&s.ensureUsage().Output)
		case "usage.cached":
			setUsage(&s.ensureUsage().CachedRead)
		case "usage.cw5m", "usage.cw":
			setUsage(&s.ensureUsage().CacheWrite)
		case "usage.cw1h":
			setUsage(&s.ensureUsage().CacheWr1h)
		case "usage.reasoning":
			setUsage(&s.ensureUsage().Reasoning)
		case "usage.images":
			setUsage(&s.ensureUsage().Images)
		case "ws_terminal_response":
			if !json.Valid([]byte(val)) {
				err = fmt.Errorf("fakellm directive ws_terminal_response=%q: invalid JSON", val)
			} else {
				s.WSTerminalResponse = val
			}
		case "ws_status":
			setInt(&s.WSStatus)
		case "ws_error_frame", "ws_error":
			err = boolean(&s.WSErrorFrame)
		case "ws_close_after":
			setInt(&s.WSCloseAfter)
		case "ws_close_code":
			setInt(&s.WSCloseCode)
		default:
			err = fmt.Errorf("fakellm directive: unknown key %q", key)
		}
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Scenario) ensureUsage() *UsageSpec {
	if s.Usage == nil {
		s.Usage = &UsageSpec{}
	}
	return s.Usage
}

// legacyMode maps the legacy mock provider's /_control modes onto scenarios.
func legacyMode(mode string) (*Scenario, bool) {
	switch mode {
	case "ok", "":
		return nil, true
	case "fail500":
		return &Scenario{Status: 500, ErrType: "mock_error", ErrMessage: "mock injected failure"}, true
	case "rate429":
		return &Scenario{Status: 429, ErrType: "rate_limit_error", ErrMessage: "mock injected rate limit", RetryAfter: "1"}, true
	case "timeout":
		return &Scenario{DelayMS: 5000}, true
	}
	return nil, false
}

// validateIterations accepts only the documented iterations shapes; directives
// and /_control JSON share it. Unset keeps the default without iterations.
func validateIterations(sc *Scenario) error {
	if sc == nil {
		return nil
	}
	switch sc.Iterations {
	case "", "message", "advisor":
		return nil
	}
	return fmt.Errorf("fakellm iterations=%q: want message or advisor", sc.Iterations)
}
