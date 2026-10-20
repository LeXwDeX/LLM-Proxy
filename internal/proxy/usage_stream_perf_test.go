package proxy

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// The SSE observer must not allocate a fresh 4 KiB bufio.Reader per event;
// one reader is reset for every event of the stream.
func TestUsageStreamSSEReusesEventBuffer(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation budget is not meaningful under the race detector")
	}
	const events = 2000
	wire := perfSSEWire(events)
	run := func() {
		stream := newUsageStream("text/event-stream", "", usageParser{protocol: config.ProtocolOpenAIChat})
		for offset := 0; offset < len(wire); offset += 512 {
			_, _ = stream.Write(wire[offset:min(offset+512, len(wire))])
		}
		stream.Close()
		if !stream.found || stream.tokens.OutputTokens != 2000 || !stream.terminal {
			t.Fatalf("usage lost: %+v", stream.tokens)
		}
	}
	run() // warm up
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	run()
	runtime.ReadMemStats(&after)
	perEvent := (after.TotalAlloc - before.TotalAlloc) / events
	t.Logf("SSE observation allocated %d bytes per event", perEvent)
	if perEvent > 2560 {
		t.Fatalf("SSE observation allocated %d bytes per event, want <= 2560 (no per-event 4 KiB buffer)", perEvent)
	}
}

type recordingWriter struct {
	log    *[]string
	name   string
	buf    bytes.Buffer
	failAt int // fail the n-th write (1-based); 0 never fails
	writes int
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.writes++
	*w.log = append(*w.log, w.name)
	if w.failAt > 0 && w.writes >= w.failAt {
		return 0, errors.New("client gone")
	}
	return w.buf.Write(p)
}

type chunkReader struct {
	chunks [][]byte
	err    error
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	if len(r.chunks[0]) == 0 {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

func TestCopyClientFirstWritesClientBeforeObserver(t *testing.T) {
	var log []string
	client := &recordingWriter{log: &log, name: "client"}
	observer := &recordingWriter{log: &log, name: "observer"}
	src := &chunkReader{chunks: [][]byte{[]byte("data: a\n\n"), []byte("data: b\n\n")}}
	n, err := copyClientFirst(client, src, observer)
	if err != nil || n != 18 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if strings.Join(log, ",") != "client,observer,client,observer" {
		t.Fatalf("write order = %v", log)
	}
	if client.buf.String() != observer.buf.String() || client.buf.String() != "data: a\n\ndata: b\n\n" {
		t.Fatalf("client=%q observer=%q", client.buf.String(), observer.buf.String())
	}
}

// A failed client write still hands the chunk to the observer so usage it
// carries stays known; copying stops at the client error.
func TestCopyClientFirstObserverKeepsChunkWhenClientFails(t *testing.T) {
	var log []string
	client := &recordingWriter{log: &log, name: "client", failAt: 2}
	observer := &recordingWriter{log: &log, name: "observer"}
	usage := []byte(`data: {"usage":{"prompt_tokens":3,"completion_tokens":1}}` + "\n\n")
	src := &chunkReader{chunks: [][]byte{[]byte("data: a\n\n"), usage, []byte("data: [DONE]\n\n")}}
	n, err := copyClientFirst(client, src, observer)
	if err == nil || err.Error() != "client gone" || n != 9 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if observer.buf.String() != "data: a\n\n"+string(usage) {
		t.Fatalf("observer lost the failed chunk: %q", observer.buf.String())
	}
}

func TestCopyClientFirstUpstreamErrorAfterData(t *testing.T) {
	var log []string
	client := &recordingWriter{log: &log, name: "client"}
	observer := &recordingWriter{log: &log, name: "observer"}
	src := &chunkReader{chunks: [][]byte{[]byte("partial")}, err: io.ErrUnexpectedEOF}
	n, err := copyClientFirst(client, src, observer)
	if !errors.Is(err, io.ErrUnexpectedEOF) || n != 7 || observer.buf.String() != "partial" {
		t.Fatalf("n=%d err=%v observer=%q", n, err, observer.buf.String())
	}
	// Nil observer: plain copy.
	src = &chunkReader{chunks: [][]byte{[]byte("x")}}
	if n, err := copyClientFirst(&recordingWriter{log: &log, name: "c"}, src, nil); n != 1 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
