package proxy

import (
	"bufio"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

const (
	// observationZstdWindowLimit is the RFC 9659 HTTP zstd window bound.
	// It applies to windowed frames and is not a bound on decoded output.
	observationZstdWindowLimit = 8 << 20
	// observationZstdSingleSegmentLimit bounds a single-segment frame, whose
	// window is its whole declared content size and is allocated up front.
	observationZstdSingleSegmentLimit = 64 << 20
)

// responseContentEncoding preserves every field value, including empty values.
// A list is deliberately not reduced to its first coding: the single-coding
// observer must reject stacked or ambiguous declarations, without changing the
// headers or bytes forwarded to the client.
func responseContentEncoding(header http.Header) string {
	return strings.TrimSpace(strings.Join(header.Values("Content-Encoding"), ","))
}

// responseIsIdentityEncoding permits raw-body classification only after all
// coding fields establish identity; an empty first list value is not identity.
func responseIsIdentityEncoding(header http.Header) bool {
	encoding := responseContentEncoding(header)
	return encoding == "" || strings.EqualFold(encoding, "identity")
}

// observationReader owns decoding resources, never the upstream response body.
// The HTTP zstd window limit does not bound the total decoded output.
// Decoder memory is bounded per stream and, for the large cases, across all
// concurrent observations: zstd single-segment frames above 8 MiB and every
// brotli stream reserve from the shared observationBudget without blocking.
// A failed reservation is an observation failure (incomplete, zero usage);
// the forwarded bytes are never affected.
func observationReader(source io.Reader, encoding string) (io.ReadCloser, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return io.NopCloser(source), nil
	case "gzip":
		return gzip.NewReader(source)
	case "deflate":
		return zlib.NewReader(source)
	case "br":
		// WBITS sits in the stream's first byte and caps the decoder's ring
		// buffer at 1<<WBITS (16 MiB at most without the large-window
		// extension). The decoder may allocate the whole window at the first
		// metablock header, before any output, so the window is reserved up
		// front, like a zstd single-segment frame.
		buffered := bufio.NewReaderSize(source, 16)
		first, err := buffered.Peek(1)
		if err != nil {
			return nil, fmt.Errorf("brotli observation: read stream header: %w", err)
		}
		wbits, err := brotliWindowBits(first[0])
		if err != nil {
			return nil, err
		}
		reserved := observationBrotliReservation(int64(1) << wbits)
		budget := observationBudget.Load()
		if !budget.tryAcquire(reserved) {
			budget.reject("br", reserved)
			return nil, errObservationBudgetExhausted
		}
		return &budgetedObservation{ReadCloser: io.NopCloser(brotli.NewReader(buffered)), budget: budget, reserved: reserved}, nil
	case "zstd":
		// A small buffer suffices for the header peek; larger decoder reads
		// bypass it once it is drained.
		buffered := bufio.NewReaderSize(source, 64)
		limit := observationZstdLimit(buffered)
		// A larger single-segment window is admitted only against the shared
		// budget. Without it the 8 MiB bound rejects the frame: an observation
		// failure (incomplete usage), never an unbounded allocation.
		var reserved int64
		budget := observationBudget.Load()
		if limit > observationZstdWindowLimit {
			if budget.tryAcquire(int64(limit)) {
				reserved = int64(limit)
			} else {
				budget.reject("zstd", int64(limit))
				limit = observationZstdWindowLimit
			}
		}
		decoder, err := zstd.NewReader(buffered, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxWindow(limit), zstd.WithDecoderMaxMemory(limit))
		if err != nil {
			budget.release(reserved)
			return nil, err
		}
		if reserved == 0 {
			return decoder.IOReadCloser(), nil
		}
		return &budgetedObservation{ReadCloser: decoder.IOReadCloser(), budget: budget, reserved: reserved}, nil
	default:
		return nil, fmt.Errorf("unsupported observation content encoding %q", encoding)
	}
}

// observationZstdLimit sizes the decoder from the first frame header. The
// library treats a single-segment frame's content size as its window, so a
// whole-buffer frame over 8 MiB would otherwise be rejected. Such a frame may
// raise the stream's limit to its declared size, up to
// observationZstdSingleSegmentLimit; a stream that starts with a windowed
// frame keeps the 8 MiB bound. Later frames share the admitted limit, so the
// per-stream decoder memory never exceeds what the first header admitted.
// The peek waits only for header bytes the decoder must read first.
func observationZstdLimit(source *bufio.Reader) uint64 {
	prefix, err := source.Peek(5)
	if err != nil || string(prefix[:4]) != "\x28\xb5\x2f\xfd" || prefix[4]&(1<<5) == 0 {
		return observationZstdWindowLimit
	}
	descriptor := prefix[4]
	size := 5 + [4]int{0, 1, 2, 4}[descriptor&3] + [4]int{1, 2, 4, 8}[descriptor>>6]
	frameHeader, err := source.Peek(size)
	if err != nil {
		return observationZstdWindowLimit
	}
	var header zstd.Header
	if header.Decode(frameHeader) != nil || !header.SingleSegment || !header.HasFCS ||
		header.FrameContentSize <= observationZstdWindowLimit || header.FrameContentSize > observationZstdSingleSegmentLimit {
		return observationZstdWindowLimit
	}
	return header.FrameContentSize
}

// brotliWindowBits decodes WBITS from a brotli stream's first byte (RFC 7932
// section 9.1, 1-7 bits, least significant first). The large-window marker
// is rejected: HTTP brotli does not use it.
func brotliWindowBits(first byte) (int, error) {
	if first&1 == 0 {
		return 16, nil
	}
	if n := int(first>>1) & 7; n != 0 {
		return 17 + n, nil
	}
	switch n := int(first>>4) & 7; n {
	case 0:
		return 17, nil
	case 1:
		return 0, errors.New("brotli observation: invalid window bits (large-window marker)")
	default:
		return 8 + n, nil
	}
}

// Brotli decoder memory, measured with andybalholm/brotli v1.2.6
// (BenchmarkObservationBrotliResident): the ring buffer is allocated at each
// metablock header as the next power of two of (output so far + that
// metablock's length), capped at 1<<WBITS. A stream compressed in one write
// carries a large first metablock, so the full window can be allocated
// before the first output byte. Everything else (32 KiB read buffer, Huffman
// tables, context maps) measured at most ~150 KiB, quality 11 included;
// 256 KiB covers it.
const observationBrotliFixedOverhead = 256 << 10

// observationBrotliReservation is the budget a brotli observation reserves at
// open and holds until Close: the full window plus the fixed overhead, plus a
// 20% margin over the measured resident memory.
func observationBrotliReservation(window int64) int64 {
	return (window + observationBrotliFixedOverhead) * 6 / 5
}

// errObservationBudgetExhausted fails an observation (incomplete usage)
// instead of allocating decoder memory past the shared budget.
var errObservationBudgetExhausted = errors.New("response observation decoder budget exhausted")

// DefaultObservationDecodeBudgetBytes bounds the decoder memory that all
// concurrent observations may reserve unless configured otherwise: brotli
// streams (window + overhead, see observationBrotliReservation) and zstd
// single-segment frames above 8 MiB (declared Frame_Content_Size).
const DefaultObservationDecodeBudgetBytes = 512 << 20

// observationBudget is the process-wide budget; startup may replace it
// (ConfigureObservationDecodeBudget) and tests install small ones.
var observationBudget atomic.Pointer[observationByteBudget]

// observationBudgetLogger receives the rate-limited exhaustion warning.
var observationBudgetLogger atomic.Pointer[slog.Logger]

// observationBudgetNow is the warning rate limiter's clock (tests replace it).
var observationBudgetNow = time.Now

// observationBudgetWarnInterval: at most one exhaustion warning per interval.
const observationBudgetWarnInterval = 10 * time.Second

func init() {
	observationBudget.Store(newObservationByteBudget(DefaultObservationDecodeBudgetBytes))
}

// ConfigureObservationDecodeBudget installs the process-wide decode budget at
// startup, before any request is served; bytes <= 0 selects the default. The
// logger receives the exhaustion warnings (nil keeps slog.Default). It
// returns the effective budget in bytes.
func ConfigureObservationDecodeBudget(bytes int64, logger *slog.Logger) int64 {
	if bytes <= 0 {
		bytes = DefaultObservationDecodeBudgetBytes
	}
	observationBudget.Store(newObservationByteBudget(bytes))
	if logger != nil {
		observationBudgetLogger.Store(logger)
	}
	return bytes
}

// ObservationBudgetMetrics reports the shared observation decode budget.
type ObservationBudgetMetrics struct {
	CapacityBytes int64 `json:"capacity_bytes"`
	InUseBytes    int64 `json:"in_use_bytes"`
	// Exhausted counts observations refused by the budget, per encoding
	// (cumulative since the budget was installed).
	Exhausted struct {
		Br   int64 `json:"br"`
		Zstd int64 `json:"zstd"`
	} `json:"observation_budget_exhausted"`
}

// ObservationBudgetMetrics reports the process-wide observation budget.
func (s *Service) ObservationBudgetMetrics() ObservationBudgetMetrics {
	return observationBudgetSnapshot()
}

func observationBudgetSnapshot() ObservationBudgetMetrics {
	budget := observationBudget.Load()
	var m ObservationBudgetMetrics
	budget.mu.Lock()
	m.CapacityBytes, m.InUseBytes = budget.capacity, budget.used
	budget.mu.Unlock()
	m.Exhausted.Br = budget.exhaustedBr.Load()
	m.Exhausted.Zstd = budget.exhaustedZstd.Load()
	return m
}

// observationByteBudget is a non-blocking byte semaphore.
type observationByteBudget struct {
	mu             sync.Mutex
	capacity, used int64

	exhaustedBr, exhaustedZstd atomic.Int64
	// Warning rate limit, under mu: rejections since the last warning.
	lastWarn         time.Time
	rejectedSinceLog int64
}

// reject counts a refused reservation and logs at most one warning per
// observationBudgetWarnInterval, carrying the encoding of the rejection that
// triggered it and the number of rejections since the previous warning.
func (b *observationByteBudget) reject(encoding string, requested int64) {
	var total int64
	if encoding == "zstd" {
		total = b.exhaustedZstd.Add(1)
	} else {
		total = b.exhaustedBr.Add(1)
	}
	now := observationBudgetNow()
	b.mu.Lock()
	b.rejectedSinceLog++
	if !b.lastWarn.IsZero() && now.Sub(b.lastWarn) < observationBudgetWarnInterval {
		b.mu.Unlock()
		return
	}
	rejected := b.rejectedSinceLog
	b.rejectedSinceLog = 0
	b.lastWarn = now
	used, capacity := b.used, b.capacity
	b.mu.Unlock()
	logger := observationBudgetLogger.Load()
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("响应用量观察解码预算不足，本次用量按观察失败记录 / response observation decode budget exhausted; usage observation failed",
		"encoding", encoding,
		"rejected", rejected,
		"rejected_total", total,
		"requested_bytes", requested,
		"in_use_bytes", used,
		"budget_bytes", capacity,
	)
}

func newObservationByteBudget(capacity int64) *observationByteBudget {
	return &observationByteBudget{capacity: capacity}
}

func (b *observationByteBudget) tryAcquire(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || b.used+n > b.capacity {
		return false
	}
	b.used += n
	return true
}

func (b *observationByteBudget) release(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
}

func (b *observationByteBudget) inUse() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// budgetedObservation returns its reservation once, on the first Close.
type budgetedObservation struct {
	io.ReadCloser
	budget   *observationByteBudget
	reserved int64
	once     sync.Once
}

func (b *budgetedObservation) Close() error {
	var err error
	b.once.Do(func() {
		err = b.ReadCloser.Close()
		b.budget.release(b.reserved)
	})
	return err
}

// observationErrors retains decoder failures even when an SSE event boundary
// projects the reader's termination as EOF.
type observationErrors struct {
	io.Reader
	err error
}

func (r *observationErrors) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}
