package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/andybalholm/brotli"
)

// Single-segment zstd frames above 8 MiB reserve their declared content size
// from a process-wide budget. Without budget the 8 MiB bound applies, so the
// frame is an observation failure (incomplete usage), never an unbounded
// allocation. Close returns the reservation.

func withObservationBudget(t *testing.T, capacity int64) *observationByteBudget {
	t.Helper()
	budget := newObservationByteBudget(capacity)
	prev := observationBudget.Load()
	observationBudget.Store(budget)
	t.Cleanup(func() { observationBudget.Store(prev) })
	return budget
}

func decodeObservation(wire []byte) (io.ReadCloser, error) {
	reader, err := observationReader(bytes.NewReader(wire), "zstd")
	if err != nil {
		return nil, err
	}
	return reader, nil
}

func TestObservationZstdBudgetReservesAndReleases(t *testing.T) {
	if DefaultObservationDecodeBudgetBytes != 512<<20 {
		t.Fatalf("default budget=%d want 512 MiB", DefaultObservationDecodeBudgetBytes)
	}
	body := paddedUsageJSON(9 << 20)
	wire := singleSegmentZstd(t, body)
	size := int64(len(body))
	budget := withObservationBudget(t, 2*size)

	first, err := decodeObservation(wire)
	if err != nil {
		t.Fatal(err)
	}
	second, err := decodeObservation(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got := budget.inUse(); got != 2*size {
		t.Fatalf("reserved=%d want %d", got, 2*size)
	}
	// No budget left: the 8 MiB bound rejects the frame.
	third, err := decodeObservation(wire)
	if err == nil {
		_, err = io.ReadAll(third)
		third.Close()
	}
	if err == nil {
		t.Fatal("frame decoded without a budget reservation")
	}
	if got := budget.inUse(); got != 2*size {
		t.Fatalf("failed reservation changed the budget: %d", got)
	}
	decoded, err := io.ReadAll(first)
	if err != nil || !bytes.Equal(decoded, body) {
		t.Fatalf("reserved frame decode failed: %v", err)
	}
	first.Close()
	first.Close() // Close is idempotent for the reservation
	if got := budget.inUse(); got != size {
		t.Fatalf("after close reserved=%d want %d", got, size)
	}
	fourth, err := decodeObservation(wire)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := io.ReadAll(fourth); err != nil || !bytes.Equal(decoded, body) {
		t.Fatalf("released budget not reusable: %v", err)
	}
	fourth.Close()
	second.Close()
	if got := budget.inUse(); got != 0 {
		t.Fatalf("budget leaked: %d", got)
	}
}

func TestObservationZstdBudgetConcurrent(t *testing.T) {
	body := paddedUsageJSON(9 << 20)
	wire := singleSegmentZstd(t, body)
	size := int64(len(body))
	budget := withObservationBudget(t, 3*size)
	const workers = 8
	var ok, failed atomic.Int32
	var peak atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			reader, err := decodeObservation(wire)
			if err != nil {
				failed.Add(1)
				return
			}
			defer reader.Close()
			if used := budget.inUse(); used > peak.Load() {
				peak.Store(used)
			}
			decoded, err := io.ReadAll(reader)
			if err != nil {
				failed.Add(1)
				return
			}
			if !bytes.Equal(decoded, body) {
				t.Error("decoded bytes changed")
			}
			ok.Add(1)
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load()+failed.Load() != workers || ok.Load() == 0 {
		t.Fatalf("ok=%d failed=%d", ok.Load(), failed.Load())
	}
	if peak.Load() > 3*size {
		t.Fatalf("budget exceeded: peak=%d cap=%d", peak.Load(), 3*size)
	}
	if got := budget.inUse(); got != 0 {
		t.Fatalf("budget leaked after concurrent decoders: %d", got)
	}
}

// An exhausted budget keeps the forwarded bytes and marks usage incomplete.
func TestObservationZstdBudgetExhaustedService(t *testing.T) {
	withObservationBudget(t, 0)
	wire := singleSegmentZstd(t, paddedUsageJSON(9<<20))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "zstd")
		_, _ = w.Write(wire)
	}))
	defer up.Close()
	svc, err := NewService(&config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 30}, Targets: []config.Target{{Name: "probe", EndpointType: config.EndpointTypeOpenAI, UsageAdapter: "standard", Endpoint: up.URL, APIKey: "fixture"}}}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	rec := &globalAuditRecorder{}
	svc.SetUsageRecorder(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-chat"}`))
	req.Header.Set("Accept-Encoding", "zstd")
	req = req.WithContext(auth.WithPrincipal(req.Context(), newAllowAllPrincipal(t, "fixture")))
	rr := httptest.NewRecorder()
	svc.ServeHTTP(rr, req)
	if rr.Code != 200 || !bytes.Equal(rr.Body.Bytes(), wire) {
		t.Fatal("forwarded bytes changed when the observation budget was exhausted")
	}
	if len(rec.events) != 1 || rec.events[0].UsageStatus != usageStatusIncomplete || !rec.events[0].CostIncomplete {
		t.Fatalf("exhausted budget must be an observation failure: %+v", rec.events)
	}
}

// Brotli observation reserves its decoder memory from the same budget as
// zstd: the window parsed from the stream header (WBITS, RFC 7932 9.1) plus
// the decoder's fixed overhead. Without budget the observation fails
// (incomplete, zero usage) and the forwarded bytes are unchanged.

func brotliWire(t *testing.T, body []byte, lgwin int) []byte {
	t.Helper()
	var b bytes.Buffer
	w := brotli.NewWriterOptions(&b, brotli.WriterOptions{Quality: 5, LGWin: lgwin})
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func decodeBrotliObservation(wire []byte) (io.ReadCloser, error) {
	return observationReader(bytes.NewReader(wire), "br")
}

func TestObservationBrotliWindowBits(t *testing.T) {
	for first, want := range map[byte]int{
		0x00: 16, 0x01: 17, 0x03: 18, 0x05: 19, 0x07: 20, 0x09: 21, 0x0b: 22, 0x0d: 23, 0x0f: 24,
		0x21: 10, 0x31: 11, 0x41: 12, 0x51: 13, 0x61: 14, 0x71: 15,
		0x80: 16, 0x8b: 22, 0xf1: 15, // bits above WBITS belong to the first meta-block
	} {
		if got, err := brotliWindowBits(first); err != nil || got != want {
			t.Errorf("first byte %#02x: wbits=%d err=%v want %d", first, got, err, want)
		}
	}
	// 0x11 is the large-window marker; RFC 7932 streams never use it.
	if _, err := brotliWindowBits(0x11); err == nil {
		t.Error("large-window marker must be rejected")
	}
	for _, lgwin := range []int{18, 22, 24} {
		wire := brotliWire(t, paddedUsageJSON(1<<lgwin), lgwin)
		if got, err := brotliWindowBits(wire[0]); err != nil || got != lgwin {
			t.Errorf("encoder lgwin=%d parsed %d err=%v", lgwin, got, err)
		}
	}
}

// A brotli observation reserves its whole window up front, like zstd: the
// decoder sizes its ring buffer at a metablock header as the next power of
// two of (output so far + the metablock length), so a single large metablock
// allocates the full window before any byte is output. Everything else
// (read buffer, Huffman tables) measured at most ~150 KiB.
func TestObservationBrotliReservationFormula(t *testing.T) {
	const kib, mib = int64(1 << 10), int64(1 << 20)
	for _, tc := range []struct {
		wbits int
		want  int64
	}{
		{22, (4*mib + 256*kib) * 6 / 5},
		{24, (16*mib + 256*kib) * 6 / 5},
		{10, (1*kib + 256*kib) * 6 / 5},
	} {
		if got := observationBrotliReservation(int64(1) << tc.wbits); got != tc.want {
			t.Errorf("wbits=%d reservation=%d want %d", tc.wbits, got, tc.want)
		}
	}
	// Resident bytes per stream at the full window, measured with
	// BenchmarkObservationBrotliResident: the reservation keeps a 20% margin.
	for _, m := range []struct {
		wbits    int
		resident int64
	}{{22, 4246 * kib}, {24, 16496 * kib}} {
		if got := observationBrotliReservation(int64(1) << m.wbits); got*10 < m.resident*12 {
			t.Errorf("wbits=%d reservation=%d below 1.2x measured %d", m.wbits, got, m.resident)
		}
	}
}

// Regression (round-2 review): a stream compressed in one write carries a
// large first metablock; the decoder allocates the whole window on the first
// read. The reservation must already cover what the decoder holds.
func TestObservationBrotliReservationCoversLargeMetablockFirstRead(t *testing.T) {
	wire := brotliSSEWire(12<<20, 5, 24, false)
	budget := withObservationBudget(t, 1<<30)
	const streams = 8
	before := brotliProbeHeapInUse()
	readers := make([]io.ReadCloser, streams)
	for i := range readers {
		reader, err := decodeBrotliObservation(wire)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(reader, make([]byte, 32<<10)); err != nil {
			t.Fatal(err)
		}
		readers[i] = reader
	}
	resident := int64(brotliProbeHeapInUse()-before) / streams
	reserved := budget.inUse() / streams
	for _, reader := range readers {
		reader.Close()
	}
	if reserved < resident {
		t.Fatalf("reserved %d bytes per stream, decoder holds %d after the first read", reserved, resident)
	}
	if got := budget.inUse(); got != 0 {
		t.Fatalf("budget leaked: %d", got)
	}
}

func TestObservationBrotliReservesWindowUpFront(t *testing.T) {
	window := int64(1) << 22
	full := observationBrotliReservation(window)
	budget := withObservationBudget(t, 2*full)
	body := paddedUsageJSON(1 << 20)
	wire := brotliWire(t, body, 22)
	first, err := decodeBrotliObservation(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got := budget.inUse(); got != full {
		t.Fatalf("reserved=%d want the full window %d before any read", got, full)
	}
	second, err := decodeBrotliObservation(wire)
	if err != nil {
		t.Fatal(err)
	}
	if third, err := decodeBrotliObservation(wire); err == nil {
		third.Close()
		t.Fatal("br observation admitted without a window reservation")
	}
	if decoded, err := io.ReadAll(first); err != nil || !bytes.Equal(decoded, body) {
		t.Fatalf("reserved br decode failed: %v", err)
	}
	if got := budget.inUse(); got != 2*full {
		t.Fatalf("reservation changed while decoding: %d", got)
	}
	first.Close()
	first.Close() // the reservation is returned once
	if got := budget.inUse(); got != full {
		t.Fatalf("after close reserved=%d want %d", got, full)
	}
	second.Close()
	if got := budget.inUse(); got != 0 {
		t.Fatalf("budget leaked: %d", got)
	}
	if got := observationBudgetSnapshot().Exhausted.Br; got != 1 {
		t.Fatalf("br exhaustion counter=%d want 1", got)
	}
}

func TestObservationBrotliSharesBudgetWithZstd(t *testing.T) {
	zstdBody := paddedUsageJSON(9 << 20)
	zstdWire := singleSegmentZstd(t, zstdBody)
	brWire := brotliWire(t, paddedUsageJSON(1<<16), 22)
	reserve := observationBrotliReservation(1 << 22)
	budget := withObservationBudget(t, int64(len(zstdBody))+reserve-1)
	zr, err := decodeObservation(zstdWire)
	if err != nil {
		t.Fatal(err)
	}
	if br, err := decodeBrotliObservation(brWire); err == nil {
		br.Close()
		t.Fatal("br ignored the zstd reservation in the shared budget")
	}
	zr.Close()
	br, err := decodeBrotliObservation(brWire)
	if err != nil {
		t.Fatalf("released zstd budget not available to br: %v", err)
	}
	br.Close()
	if got := budget.inUse(); got != 0 {
		t.Fatalf("budget leaked: %d", got)
	}
}

func TestObservationBrotliBudgetConcurrent(t *testing.T) {
	body := paddedUsageJSON(1 << 20)
	wire := brotliWire(t, body, 22)
	capacity := 3 * observationBrotliReservation(1<<22)
	budget := withObservationBudget(t, capacity)
	const workers = 12
	var ok, failed atomic.Int32
	var peak atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			reader, err := decodeBrotliObservation(wire)
			if err != nil {
				failed.Add(1)
				return
			}
			defer reader.Close()
			chunk := make([]byte, 32<<10)
			var decoded bytes.Buffer
			for {
				n, err := reader.Read(chunk)
				decoded.Write(chunk[:n])
				for used := budget.inUse(); used > peak.Load(); used = budget.inUse() {
					peak.Store(used)
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					failed.Add(1)
					return
				}
			}
			if !bytes.Equal(decoded.Bytes(), body) {
				t.Error("decoded bytes changed")
			}
			ok.Add(1)
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load()+failed.Load() != workers || ok.Load() == 0 {
		t.Fatalf("ok=%d failed=%d", ok.Load(), failed.Load())
	}
	if peak.Load() > capacity {
		t.Fatalf("budget exceeded: peak=%d cap=%d", peak.Load(), capacity)
	}
	if got := budget.inUse(); got != 0 {
		t.Fatalf("budget leaked after concurrent decoders: %d", got)
	}
	if got := observationBudgetSnapshot().Exhausted.Br; got != int64(failed.Load()) {
		t.Fatalf("br exhaustion counter=%d want %d", got, failed.Load())
	}
}

// Exhaustion is observable: per-encoding counters and bytes in use in the
// snapshot, and a Warn limited to one per 10 seconds carrying the encoding
// and the number of rejections since the previous warning.
func TestObservationBudgetExhaustionMetricsAndWarn(t *testing.T) {
	withObservationBudget(t, 0)
	logs := &rejectionLog{}
	prevLogger := observationBudgetLogger.Load()
	observationBudgetLogger.Store(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { observationBudgetLogger.Store(prevLogger) })
	now := time.Unix(1_000_000, 0)
	prevNow := observationBudgetNow
	observationBudgetNow = func() time.Time { return now }
	t.Cleanup(func() { observationBudgetNow = prevNow })

	brWire := brotliWire(t, paddedUsageJSON(1<<16), 22)
	zstdWire := singleSegmentZstd(t, paddedUsageJSON(9<<20))
	for i := 0; i < 3; i++ {
		if r, err := decodeBrotliObservation(brWire); err == nil {
			r.Close()
			t.Fatal("br admitted without budget")
		}
	}
	if r, err := decodeObservation(zstdWire); err == nil {
		r.Close()
	}
	const msg = "响应用量观察解码预算不足，本次用量按观察失败记录 / response observation decode budget exhausted; usage observation failed"
	if got := strings.Count(logs.String(), msg); got != 1 {
		t.Fatalf("warnings within 10s=%d want 1: %s", got, logs.String())
	}
	if out := logs.String(); !strings.Contains(out, `"encoding":"br"`) || !strings.Contains(out, `"rejected":1`) {
		t.Fatalf("first warning fields: %s", out)
	}
	now = now.Add(10 * time.Second)
	if r, err := decodeObservation(zstdWire); err == nil {
		r.Close()
	}
	if got := strings.Count(logs.String(), msg); got != 2 {
		t.Fatalf("warnings after 10s=%d want 2: %s", got, logs.String())
	}
	if out := logs.String(); !strings.Contains(out, `"encoding":"zstd"`) || !strings.Contains(out, `"rejected":4`) {
		t.Fatalf("second warning must carry the rejections since the first: %s", out)
	}
	snap := observationBudgetSnapshot()
	if snap.Exhausted.Br != 3 || snap.Exhausted.Zstd != 2 || snap.InUseBytes != 0 || snap.CapacityBytes != 0 {
		t.Fatalf("snapshot: %+v", snap)
	}
	if got := (&Service{}).ObservationBudgetMetrics(); got != snap {
		t.Fatalf("service metrics %+v != snapshot %+v", got, snap)
	}
}

func TestConfigureObservationDecodeBudget(t *testing.T) {
	prev := observationBudget.Load()
	t.Cleanup(func() { observationBudget.Store(prev) })
	for _, tc := range []struct{ in, want int64 }{{0, DefaultObservationDecodeBudgetBytes}, {-1, DefaultObservationDecodeBudgetBytes}, {64 << 20, 64 << 20}} {
		if got := ConfigureObservationDecodeBudget(tc.in, nil); got != tc.want || observationBudgetSnapshot().CapacityBytes != tc.want {
			t.Errorf("configure(%d)=%d capacity=%d want %d", tc.in, got, observationBudgetSnapshot().CapacityBytes, tc.want)
		}
	}
}

// An exhausted budget forwards the br bytes unchanged and settles the usage
// as an observation failure: zero tokens, incomplete, cause observation_failed.
func TestObservationBrotliBudgetExhaustedService(t *testing.T) {
	withObservationBudget(t, 0)
	sse := "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\ndata: [DONE]\n\n"
	wire := brotliWire(t, []byte(sse), 22)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "br")
		_, _ = w.Write(wire)
	}))
	defer upstream.Close()
	s, events, _, logs := goneService(t, upstream.URL, config.EndpointTypeOpenAI, config.UsageAdapterStandard)
	req := goneRequest(t, "/v1/chat/completions", `{"model":"test-model","stream":true,"messages":[]}`)
	req.Header.Set("Accept-Encoding", "br")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != 200 || !bytes.Equal(rr.Body.Bytes(), wire) || rr.Header().Get("Content-Encoding") != "br" {
		t.Fatalf("forwarded br bytes changed: status=%d headers=%v", rr.Code, rr.Header())
	}
	assertInterruptedEvent(t, s, events.snapshot(), logs, noUsageCauseObservationFailed, "streaming", "sse")
}
