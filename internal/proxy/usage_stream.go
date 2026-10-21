package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"io"
	"strings"
)

// The observation pipe applies backpressure but never changes forwarded bytes.
// Content is discarded by projection; only usage and lifecycle metadata survive.
type usageStream struct {
	writer               *io.PipeWriter
	done                 chan struct{}
	tokens               usageTokens
	model                string
	found                bool
	err                  error
	parser               usageParser
	terminal             bool
	streaming            bool
	jsonResponse         bool
	finalUsage           bool
	responseStatus       string
	finishReason         string
	standardError        bool
	snapshot             map[string]any
	pricingEnvelope      map[string]any
	claudeFinalEnvelope  map[string]any
	claudeLastDelta      map[string]any
	claudeIterationsSeen bool
	// finalSeen and finalTokens hold the protocol's official final usage as
	// observed so far (see officialFinalUsage). Written by the observer
	// goroutine; read only after Close.
	finalSeen   bool
	finalTokens usageTokens
	finalModel  string
	// finalConflict: official final usage arrived but its values conflict.
	finalConflict bool
	// choiceFinished tracks every Chat choice / Gemini candidate index seen
	// in the stream and whether it has a finish reason yet.
	choiceFinished map[int64]bool
	// choicesFinalUsage: a chunk carrying usage arrived when every choice
	// seen so far had finished (sticky).
	choicesFinalUsage bool
}

func newUsageStream(contentType, encoding string, parser usageParser) *usageStream {
	parser = parser.capturePricingRequest()
	reader, writer := io.Pipe()
	// A declared SSE response requires a terminal even if decompression fails
	// before any decoded bytes can be inspected.
	u := &usageStream{writer: writer, done: make(chan struct{}), parser: parser, tokens: usageTokens{status: usageStatusIncomplete}, streaming: strings.Contains(strings.ToLower(contentType), "text/event-stream")}
	u.jsonResponse = strings.EqualFold(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]), "application/json")
	go func() {
		defer close(u.done)
		defer func() { u.tokens.responseStatus = u.responseStatus; u.tokens.finishReason = u.finishReason }()
		defer reader.Close()
		defer io.Copy(io.Discard, reader)
		source, err := observationReader(reader, encoding)
		if err != nil {
			u.err = err
			return
		}
		// Registered last: close decoder before draining and closing the pipe.
		defer source.Close()
		observed := &observationErrors{Reader: source}
		defer func() {
			_, _ = io.Copy(io.Discard, observed)
			if observed.err != nil {
				u.err = observed.err
			}
		}()
		buffer := bufio.NewReader(observed)
		prefix, _ := buffer.Peek(5)
		if u.streaming || bytes.Equal(prefix, []byte("data:")) || bytes.Equal(prefix, []byte("event")) {
			u.streaming = true
			u.readSSE(buffer, observed)
		} else {
			payload, err := readUsagePayload(buffer)
			u.err = err
			if err == nil {
				u.tokens, u.model, u.found = u.parser.parsePayload(payload)
				u.observeLifecycle(payload, "")
				u.captureFinalUsage()
			}
		}
	}()
	return u
}
func (u *usageStream) Write(p []byte) (int, error) { _, _ = u.writer.Write(p); return len(p), nil }
func (u *usageStream) Close()                      { _ = u.writer.Close(); <-u.done }

// Vendor aborts follow the explicit choice. Keep all reported usage and the
// original reason, even when that reason does not mark this target as failed.
// DeepSeek documents insufficient_system_resource and aborted ("生成过程被中断")
// as Chat finish reasons: https://api-docs.deepseek.com/zh-cn/api/create-chat-completion
// (checked 2026-10-20; documentation contract, not live acceptance).
func (p usageParser) failureFinishReason(reason string) bool {
	switch reason {
	case "error":
		return true
	case "network_error", "sensitive":
		return config.NormalizeUsageAdapter(p.usageAdapter) == usageAdapterGLM
	case "insufficient_system_resource", "aborted":
		return config.NormalizeUsageAdapter(p.usageAdapter) == usageAdapterDeepSeek
	}
	return false
}

// Standard errors are protocol-scoped, not vendor-scoped. Inspect only the
// top-level object and standard identifying members; metadata and generated
// content must not affect response completion or key policy.
func (p usageParser) standardErrorEnvelope(payload map[string]any) bool {
	switch p.protocol {
	case config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses, config.ProtocolOpenAIImage:
	default:
		return false
	}
	err, ok := payload["error"].(map[string]any)
	if !ok {
		return false
	}
	for _, key := range []string{"message", "type", "code"} {
		if value, ok := err[key].(string); ok && strings.TrimSpace(value) != "" {
			return true
		}
	}
	_, ok = err["code"].(json.Number)
	return ok
}

func (u *usageStream) observeLifecycle(payload map[string]any, event string) {
	if u.parser.standardErrorEnvelope(payload) {
		u.standardError = true
		u.terminal = true
	}
	if !u.streaming && u.jsonResponse && u.parser.protocol == config.ProtocolOpenAIChat && config.NormalizeUsageAdapter(u.parser.usageAdapter) == usageAdapterMinimax {
		if base, ok := payload["base_resp"].(map[string]any); ok {
			if number, ok := base["status_code"].(json.Number); ok {
				if code, err := number.Int64(); err == nil && code != 0 {
					u.standardError = true
					u.terminal = true
				}
			}
		}
	}
	// An error remains failed even if later frames supply usage or a success
	// terminal. Keep parsing those frames so reported tokens are not lost.
	defer func() {
		if u.standardError {
			u.responseStatus = "failed"
		}
	}()
	if status := readString(payload["status"]); status != "" {
		u.responseStatus = status
	}
	if feedback := usageDetails(payload, "promptFeedback"); feedback != nil {
		if reason := readString(feedback["blockReason"]); reason != "" && reason != "BLOCK_REASON_UNSPECIFIED" {
			u.terminal = true
			u.responseStatus = "blocked"
			u.finishReason = reason
		}
	}
	typ := readString(payload["type"])
	if typ == "" {
		typ = event
	}
	switch typ {
	case "message_stop", "response.completed", "response.incomplete", "response.failed":
		u.terminal = true
		u.responseStatus = strings.TrimPrefix(typ, "response.")
	case "error":
		u.terminal = true
		u.responseStatus = "failed"
		if u.parser.operation(nil) == config.ProtocolAnthropicMessages {
			u.standardError = true // A later message_stop cannot erase the failure.
		}
	case "image_generation.completed", "image_edit.completed":
		// Image streams carry the usage on this event and send no [DONE].
		u.terminal = true
	}
	if response, ok := payload["response"].(map[string]any); ok {
		if status := readString(response["status"]); status != "" {
			u.responseStatus = status
		}
	}
	if stop := readString(payload["stop_reason"]); stop != "" {
		u.finishReason = stop
	}
	for _, key := range []string{"choices", "candidates"} {
		list, _ := payload[key].([]any)
		for _, item := range list {
			entry, _ := item.(map[string]any)
			reason := readString(entry["finish_reason"])
			if reason == "" {
				reason = readString(entry["finishReason"])
			}
			if entry != nil {
				if u.choiceFinished == nil {
					u.choiceFinished = map[int64]bool{}
				}
				index := choiceIndex(entry)
				u.choiceFinished[index] = u.choiceFinished[index] || reason != ""
			}
			if reason != "" {
				u.finishReason = reason
				if key == "candidates" {
					u.terminal = true
				}
				if u.parser.failureFinishReason(reason) {
					u.responseStatus = "failed"
				}
			}
		}
	}
	if delta, ok := payload["delta"].(map[string]any); ok {
		if stop := readString(delta["stop_reason"]); stop != "" {
			u.finishReason = stop
		}
	}
}
func mergeUsageSnapshot(dst, src map[string]any) {
	for key, value := range src {
		if nested, ok := value.(map[string]any); ok {
			prior, _ := dst[key].(map[string]any)
			if prior == nil {
				prior = map[string]any{}
				dst[key] = prior
			}
			mergeUsageSnapshot(prior, nested)
		} else {
			dst[key] = value
		}
	}
}

// readSSE observes events in order. A clean EOF (no decoder or read error) may
// omit the blank line after the last event: complete data ([DONE] or a fully
// parsed JSON payload) is then treated as dispatched. Incomplete last data and
// any decoder/read error still leave the stream unterminated and failed.
func (u *usageStream) readSSE(buffer *bufio.Reader, source *observationErrors) {
	// The SSE UTF-8 decoder ignores one leading BOM. It is not part of the
	// first field name; forwarding still retains the original response bytes.
	if prefix, _ := buffer.Peek(3); bytes.Equal(prefix, []byte{0xef, 0xbb, 0xbf}) {
		_, _ = buffer.Discard(3)
	}
	// One event reader and one data buffer serve the whole stream; each event
	// resets them. Every event is drained before the next reset.
	event := &sseEventReader{}
	data := bufio.NewReader(event)
	for {
		*event = sseEventReader{reader: buffer, lineStart: true}
		data.Reset(event)
		// Peek recognizes DONE without asking the JSON decoder to interpret it.
		prefix, _ := data.Peek(6)
		complete := false
		if bytes.HasPrefix(prefix, []byte("[DONE]")) {
			_, _ = io.Copy(io.Discard, data)
			u.terminal = true
			complete = true
			u.captureFinalUsage()
		} else {
			payload, err := readUsagePayload(data)
			_, _ = io.Copy(io.Discard, data)
			if err != nil && len(prefix) > 0 {
				u.err = err
			}
			if err == nil && payload != nil {
				complete = true
				u.observeLifecycle(payload, event.event)
				usageEvent := event.event
				// A message_stop also confirms the preceding delta when a compatible
				// server omits stop_reason; an interrupted intermediate delta does not.
				if u.parser.protocol == config.ProtocolAnthropicMessages &&
					(readString(payload["type"]) == "message_stop" || (readString(payload["type"]) == "" && event.event == "message_stop")) &&
					!u.finalUsage && u.claudeLastDelta != nil {
					payload = u.claudeLastDelta
					usageEvent = "message_stop"
				}
				m, model := usageEnvelope(payload)
				if model != "" {
					u.model = model
				}
				if u.parser.protocol == "" {
					if _, ok := payload["usageMetadata"]; ok {
						u.parser.protocol = config.ProtocolGemini
					}
					if strings.HasPrefix(readString(payload["type"]), "message_") || strings.HasPrefix(event.event, "message_") {
						u.parser.protocol = config.ProtocolAnthropicMessages
					}
				}
				u.observePricingEnvelope(payload)
				u.observeFinalUsage(payload, usageEvent, m)
				if u.parser.operation(m) == config.ProtocolAnthropicMessages &&
					(readString(payload["type"]) == "message_delta" || (readString(payload["type"]) == "" && event.event == "message_delta")) {
					payload["type"] = "message_delta"
					u.claudeLastDelta = payload
				}
				claudeDiagnostic := ""
				if m != nil {
					if u.snapshot == nil {
						u.snapshot = map[string]any{}
					}
					if u.parser.operation(m) == config.ProtocolAnthropicMessages {
						typ := readString(payload["type"])
						if typ == "" {
							typ = event.event
						}
						// Remember early evidence, but never bill an intermediate array.
						iterations, array := m["iterations"].([]any)
						if len(iterations) > 0 {
							u.claudeIterationsSeen = true
						}
						if typ != "message_delta" || !u.finalUsage {
							// Keep the last delta intact until message_stop can confirm it.
							counters := make(map[string]any, len(m))
							for key, value := range m {
								if key != "iterations" {
									counters[key] = value
								}
							}
							m = counters
						} else {
							delete(u.snapshot, "iterations")
							if u.claudeIterationsSeen && !array {
								claudeDiagnostic = "missing_final_iterations"
							}
							for _, raw := range iterations {
								item, _ := raw.(map[string]any)
								if item["type"] == "fallback_message" {
									// Different returned attempt: none of the original
									// executor's optional usage evidence belongs to it.
									u.snapshot = map[string]any{}
									if _, present := m["input_tokens"]; !present {
										claudeDiagnostic = "missing_fallback_input_usage"
									}
									break
								}
							}
							u.claudeFinalEnvelope = payload
						}
					}
					mergeUsageSnapshot(u.snapshot, m)
					parsed := map[string]any{"usage": u.snapshot, "model": u.model}
					if u.claudeFinalEnvelope != nil {
						parsed["delta"] = u.claudeFinalEnvelope["delta"]
					}
					var parsedModel string
					u.tokens, parsedModel, u.found = u.parser.parsePayload(parsed)
					if claudeDiagnostic != "" {
						u.tokens.costIncomplete = true
						appendTokenDiagnostic(&u.tokens, claudeDiagnostic)
					}
					if u.tokens.billingModel != "" {
						u.model = parsedModel
					}
					u.observeChoicesFinalUsage()
				}
				if u.snapshot != nil && u.tokens.billingModel == "" {
					u.updatePricingContext()
				}
				u.captureFinalUsage()
			}
		}
		if event.eof {
			if len(prefix) > 0 && (!complete || (source != nil && source.err != nil)) {
				u.err = io.ErrUnexpectedEOF
				u.terminal = false
			}
			break
		}
	}
	op := u.parser.operation(u.snapshot)
	requiresFinalUsage := op == config.ProtocolOpenAIResponses || op == config.ProtocolAnthropicMessages
	if u.found && (!u.terminal || (requiresFinalUsage && !u.finalUsage)) && u.tokens.status != usageStatusConflict {
		u.tokens.status = usageStatusIncomplete
		u.tokens.costIncomplete = true
		u.tokens.diagnostic = "missing_stream_terminal"
		if u.terminal {
			u.tokens.diagnostic = "missing_final_usage"
		}
	}
	if !u.found {
		u.tokens.status = usageStatusIncomplete
		u.tokens.costIncomplete = true
		u.tokens.diagnostic = "missing_usage"
	}
}

// choiceIndex reads a choice/candidate index; an absent index is 0.
func choiceIndex(entry map[string]any) int64 {
	switch v := entry["index"].(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
	case float64:
		return int64(v)
	}
	return 0
}

// observeChoicesFinalUsage runs for a chunk that carries usage, after its
// lifecycle: the usage is final when every choice (candidate) seen so far
// has a finish reason. A chunk with no choices of its own (the trailing
// OpenAI usage chunk) qualifies once the earlier choices finished; a usage
// chunk before any choice, or with one choice of several unfinished, does not.
func (u *usageStream) observeChoicesFinalUsage() {
	if u.choicesFinalUsage || len(u.choiceFinished) == 0 {
		return
	}
	for _, finished := range u.choiceFinished {
		if !finished {
			return
		}
	}
	u.choicesFinalUsage = true
}

// officialFinalUsage reports whether the protocol's official final usage has
// been observed:
//   - Responses (terminal event usage) and Anthropic (terminal message_delta
//     output, or the last delta confirmed by message_stop) use finalUsage;
//   - Gemini: a usageMetadata chunk after every candidate finished
//     (choicesFinalUsage), or a terminal without candidates (blocked prompt,
//     error envelope);
//   - Chat and other streams: the DONE marker or another terminal, or a usage
//     chunk after every choice finished (choicesFinalUsage). One finished
//     choice of several, or a finish reason without the usage that follows,
//     is not final.
//
// A whole non-streaming JSON body with usage is final.
func (u *usageStream) officialFinalUsage() bool {
	if !u.found {
		return false
	}
	if !u.streaming {
		return true
	}
	switch u.parser.operation(u.snapshot) {
	case config.ProtocolOpenAIResponses, config.ProtocolAnthropicMessages:
		return u.finalUsage
	case config.ProtocolGemini:
		return u.choicesFinalUsage || (u.terminal && len(u.choiceFinished) == 0)
	}
	return u.terminal || u.choicesFinalUsage
}

// captureFinalUsage snapshots the tokens at the event that completes (or
// keeps) official final usage, before end-of-stream adjustments. Response
// failure is independent of confirmed usage and updates only its lifecycle.
func (u *usageStream) captureFinalUsage() {
	official := u.officialFinalUsage()
	u.finalConflict = official && u.tokens.status == usageStatusConflict
	u.finalSeen = official && !u.finalConflict
	if u.finalSeen {
		u.finalTokens = u.tokens
		u.finalTokens.responseStatus, u.finalTokens.finishReason = u.responseStatus, u.finishReason
		u.finalModel = u.model
	}
}

// Transport/generation termination is not evidence of a final usage snapshot.
// Responses publishes it on the terminal response; Claude publishes cumulative
// output on the terminal message_delta. An error cannot revoke confirmed usage.
func (u *usageStream) observeFinalUsage(payload map[string]any, event string, m map[string]any) {
	typ := readString(payload["type"])
	if typ == "" {
		typ = event
	}
	switch u.parser.operation(m) {
	case config.ProtocolOpenAIResponses:
		switch typ {
		case "response.completed", "response.incomplete", "response.failed":
			_, input := m["input_tokens"]
			_, output := m["output_tokens"]
			u.finalUsage = input && output
		}
	case config.ProtocolAnthropicMessages:
		switch typ {
		case "message_delta":
			_, output := m["output_tokens"]
			u.finalUsage = output && (readString(usageDetails(payload, "delta")["stop_reason"]) != "" || event == "message_stop")
		case "error":
			if u.finalSeen {
				appendTokenDiagnostic(&u.tokens, diagnosticInterruptedAfterFinalUsage)
			}
			// Unconfirmed usage stays unconfirmed; message_stop must not promote it.
			u.claudeLastDelta = nil
		}
	}
}

// sseEventReader exposes only joined data lines from one SSE event. It handles
// LF, CRLF and CR without buffering an entire content event or generated image.
type sseEventReader struct {
	reader                         *bufio.Reader
	lineStart, emitting, done, eof bool
	event                          string
	pending                        []byte
}

func (r *sseEventReader) newline(b byte) bool {
	if b == '\r' {
		if next, _ := r.reader.Peek(1); len(next) == 1 && next[0] == '\n' {
			_, _ = r.reader.ReadByte()
		}
		return true
	}
	return b == '\n'
}
func (r *sseEventReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(r.pending) > 0 {
			k := copy(p[n:], r.pending)
			r.pending = r.pending[k:]
			n += k
			continue
		}
		if r.done {
			if n > 0 {
				return n, nil
			}
			return 0, io.EOF
		}
		if r.lineStart {
			var field []byte
			endedLine := false
			for {
				b, err := r.reader.ReadByte()
				if err != nil {
					r.eof = true
					r.done = true
					break
				}
				if r.newline(b) {
					endedLine = true
					if len(field) == 0 {
						r.done = true
					}
					break
				}
				if b == ':' {
					break
				}
				if len(field) < 64 {
					field = append(field, b)
				}
			}
			if r.done {
				continue
			}
			r.emitting = string(field) == "data"
			if endedLine {
				if r.emitting {
					p[n] = '\n'
					n++
				}
				continue
			}
			r.lineStart = false
			if next, _ := r.reader.Peek(1); len(next) == 1 && next[0] == ' ' {
				_, _ = r.reader.ReadByte()
			}
			if string(field) == "event" {
				var value []byte
				for {
					b, err := r.reader.ReadByte()
					if err != nil {
						r.eof = true
						r.done = true
						break
					}
					if r.newline(b) {
						break
					}
					if len(value) < 128 {
						value = append(value, b)
					}
				}
				r.event = string(value)
				r.lineStart = true
			}
			continue
		}
		b, err := r.reader.ReadByte()
		if err != nil {
			r.eof = true
			r.done = true
			continue
		}
		if r.newline(b) {
			r.lineStart = true
			if r.emitting {
				p[n] = '\n'
				n++
			}
			continue
		}
		if r.emitting {
			p[n] = b
			n++
		}
	}
	return n, nil
}

// Only retain the fields understood by parseUsageFromPayload. Other response
// content is consumed token by token, including large arrays of generated data.
func readUsagePayload(reader io.Reader) (map[string]any, error) {
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	payload, err := projectUsageObject(decoder, true)
	if err != nil {
		return nil, err
	}
	// Only JSON whitespace may follow the value. The trailing bytes are
	// checked directly instead of with a final decoder.Token(): with the
	// legacy encoding/json decoder (Go before jsonv2 became the default),
	// that extra Token call refills and grows the decoder's read buffer for
	// every SSE event even though nothing but EOF remains.
	if err = expectJSONWhitespace(decoder.Buffered()); err == nil {
		err = expectJSONWhitespace(reader)
	}
	if err != nil {
		return nil, err
	}
	return payload, nil
}

var errUsagePayloadTrailingData = fmt.Errorf("unexpected data after usage payload")

// expectJSONWhitespace consumes reader to EOF and fails on the first byte that
// is not JSON whitespace, or on a read error.
func expectJSONWhitespace(reader io.Reader) error {
	bytesReader, ok := reader.(io.ByteReader)
	if !ok {
		bytesReader = bufio.NewReaderSize(reader, 16)
	}
	for {
		b, err := bytesReader.ReadByte()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch b {
		case ' ', '\t', '\r', '\n':
		default:
			return errUsagePayloadTrailingData
		}
	}
}

func projectUsageObject(decoder *json.Decoder, wrappers bool) (map[string]any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		if token == json.Delim('[') {
			for decoder.More() {
				if err := skipUsageValue(decoder); err != nil {
					return nil, err
				}
			}
			_, err = decoder.Token()
			return nil, err
		}
		return nil, nil
	}
	payload := make(map[string]any)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, _ := token.(string)
		switch key {
		case "service_tier", "base_resp", "promptFeedback", "usage", "usageMetadata", "model", "modelVersion", "prompt_tokens", "completion_tokens", "prompt_tokens_details", "type", "status", "stop_reason", "finish_reason", "finishReason", "error", "index":
			var value any
			if err = decoder.Decode(&value); err == nil {
				payload[key] = value
			}
		case "stop_details":
			payload[key], err = projectClaudeStopDetails(decoder)
		case "choices", "candidates":
			payload[key], err = projectUsageArray(decoder)
		case "delta":
			payload[key], err = projectUsageObject(decoder, false)
		case "response", "message":
			if wrappers {
				payload[key], err = projectUsageObject(decoder, false)
			} else {
				err = skipUsageValue(decoder)
			}
		default:
			err = skipUsageValue(decoder)
		}
		if err != nil {
			return nil, err
		}
	}
	_, err = decoder.Token()
	return payload, err
}

func skipUsageValue(decoder *json.Decoder) error {
	depth := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

func projectUsageArray(decoder *json.Decoder) ([]any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if token == nil {
		return nil, nil
	}
	if token != json.Delim('[') {
		return nil, fmt.Errorf("expected usage envelope array")
	}
	var list []any
	for decoder.More() {
		item, err := projectUsageObject(decoder, false)
		if err != nil {
			return nil, err
		}
		list = append(list, item)
	}
	_, err = decoder.Token()
	return list, err
}

// Retain category and its presence/null distinction; discard explanation and
// every other member so generated text cannot become billing evidence.
func projectClaudeStopDetails(decoder *json.Decoder) (map[string]any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		if delim, ok := token.(json.Delim); ok && delim == '[' {
			for decoder.More() {
				if err := skipUsageValue(decoder); err != nil {
					return nil, err
				}
			}
			_, err = decoder.Token()
		}
		return nil, err
	}
	details := map[string]any{}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, err
		}
		if token == "category" {
			var value any
			err = decoder.Decode(&value)
			details["category"] = value
		} else {
			err = skipUsageValue(decoder)
		}
		if err != nil {
			return nil, err
		}
	}
	_, err = decoder.Token()
	return details, err
}
