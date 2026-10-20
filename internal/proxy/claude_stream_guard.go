package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

var errClaudeStreamOverlap = errors.New("claude stream overlapping message_start")

const claudeStreamMaxEvent = 1 << 20
const claudeStreamOverlapErrorEvent = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"Upstream stream error (internal server error): upstream sent message_start for a new message before message_stop of the previous one; the stream was terminated by the proxy.\"}}\n\n"

func claudeStreamGuardEnabled(r *http.Request, resp *http.Response) bool {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || r.Context().Value(responsesWebsocketContextKey{}) != nil {
		return false
	}
	encoding := responseContentEncoding(resp.Header)
	if encoding != "" && !strings.EqualFold(encoding, "identity") {
		return false
	}
	// Avoid a lowercase copy of the header on the forwarding hot path.
	ct := resp.Header.Get("Content-Type")
	const sse = "text/event-stream"
	isSSE := false
	for i := 0; i+len(sse) <= len(ct); i++ {
		if strings.EqualFold(ct[i:i+len(sse)], sse) {
			isSSE = true
			break
		}
	}
	if !isSSE {
		return false
	}
	// ResolveProtocol lowercases paths. Reject non-candidates without allocating
	// even when an unrelated API path uses uppercase letters.
	path := strings.TrimSpace(r.URL.Path)
	const prefix = "/v1/messages"
	if len(path) < len(prefix) || !strings.EqualFold(path[:len(prefix)], prefix) ||
		(len(path) > len(prefix) && path[len(prefix)] != '/' && path[len(prefix)] != '?') {
		return false
	}
	return ResolveProtocol(r.URL.Path) == config.ProtocolAnthropicMessages
}

// claudeStreamGuard preserves raw SSE framing, but terminates overlapping
// message lifecycles. It only runs on identity-encoded HTTP streams. Oversized
// unfinished events permanently bypass validation to bound buffer growth.
type claudeStreamGuard struct {
	writer            io.Writer
	buf               []byte
	scan, lineStart   int
	openID, newID     string
	passthrough       bool
	terminalErr       error
	downstreamFailure bool
}

func (g *claudeStreamGuard) Write(p []byte) (int, error) {
	if g.terminalErr != nil {
		return 0, g.terminalErr
	}
	if g.passthrough {
		return g.writeRaw(p)
	}
	consumed := 0
	for len(p) > 0 {
		// Do not append a whole arbitrary-size Write before checking the cap.
		n := min(len(p), claudeStreamMaxEvent+1-len(g.buf))
		g.buf = append(g.buf, p[:n]...)
		p = p[n:]
		consumed += n
		if err := g.drain(false); err != nil {
			return consumed, err
		}
		if len(g.buf) > claudeStreamMaxEvent {
			g.passthrough = true
			if _, err := g.writeRaw(g.buf); err != nil {
				return consumed, err
			}
			g.buf = nil
			n, err := g.writeRaw(p)
			return consumed + n, err
		}
	}
	return consumed, nil
}

// drain scans each byte once across Writes. A trailing CR waits for the next
// byte (or EOF), so a split CRLF remains part of the same raw event.
func (g *claudeStreamGuard) drain(eof bool) error {
	eventStart := 0
	for g.scan < len(g.buf) {
		i := g.scan
		b := g.buf[i]
		if b != '\r' && b != '\n' {
			g.scan++
			continue
		}
		if b == '\r' && i+1 == len(g.buf) && !eof {
			break
		}
		g.scan++
		if b == '\r' && g.scan < len(g.buf) && g.buf[g.scan] == '\n' {
			g.scan++
		}
		blank := i == g.lineStart
		g.lineStart = g.scan
		if !blank {
			continue
		}
		event := g.buf[eventStart:g.scan]
		if g.overlaps(event) {
			// Never forward the offending event or any subsequent bytes.
			g.buf = nil
			if _, err := g.writeRaw([]byte(claudeStreamOverlapErrorEvent)); err != nil {
				return err
			}
			g.terminalErr = errClaudeStreamOverlap
			return g.terminalErr
		}
		if _, err := g.writeRaw(event); err != nil {
			return err
		}
		eventStart = g.scan
	}
	if eventStart > 0 {
		g.buf = g.buf[:copy(g.buf, g.buf[eventStart:])]
		g.scan -= eventStart
		g.lineStart -= eventStart
	}
	return nil
}

func (g *claudeStreamGuard) overlaps(event []byte) bool {
	// Deltas, pings and opaque extensions need no JSON parsing.
	if !bytes.Contains(event, []byte("message_start")) && !bytes.Contains(event, []byte("message_stop")) {
		return false
	}
	var data []byte
	event = bytes.TrimPrefix(event, []byte("\xef\xbb\xbf"))
	for len(event) > 0 {
		i := bytes.IndexAny(event, "\r\n")
		if i < 0 {
			i = len(event)
		}
		line := event[:i]
		event = event[i:]
		if len(event) > 0 {
			cr := event[0] == '\r'
			event = event[1:]
			if cr && len(event) > 0 && event[0] == '\n' {
				event = event[1:]
			}
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			value := bytes.TrimPrefix(line[5:], []byte(" "))
			data = append(data, value...)
			data = append(data, '\n')
		}
	}
	var lifecycle struct {
		Type    string `json:"type"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	if json.Unmarshal(data, &lifecycle) != nil {
		return false
	}
	// The JSON type is authoritative, not the event: label or text in a delta.
	switch lifecycle.Type {
	case "message_start":
		if g.openID != "" && lifecycle.Message.ID != g.openID {
			g.newID = lifecycle.Message.ID
			return true
		}
		g.openID = lifecycle.Message.ID
	case "message_stop":
		g.openID = ""
	}
	return false
}

func (g *claudeStreamGuard) writeRaw(p []byte) (int, error) {
	n, err := g.writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		g.downstreamFailure = true
		g.terminalErr = err
	}
	return n, err
}

// flush preserves a truncated final event only after a successful upstream EOF.
func (g *claudeStreamGuard) flush() error {
	if g.terminalErr != nil {
		return g.terminalErr
	}
	if err := g.drain(true); err != nil {
		return err
	}
	if len(g.buf) == 0 {
		return nil
	}
	_, err := g.writeRaw(g.buf)
	g.buf = nil
	return err
}
