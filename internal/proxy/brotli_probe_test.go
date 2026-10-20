package proxy

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"runtime"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
)

// BenchmarkObservationBrotliResident measures the resident decoder memory per
// brotli stream (heap in use after GC with 16 decoders alive at end of
// stream) for SSE-shaped bodies. It is the evidence behind
// observationBrotliReservation:
//
//	go test ./internal/proxy/ -run '^$' -bench ObservationBrotliResident -benchtime 1x
func BenchmarkObservationBrotliResident(b *testing.B) {
	for _, tc := range []struct {
		quality, lgwin, size int
		flushEachEvent       bool
	}{
		{5, 22, 64 << 10, true}, {5, 22, 512 << 10, true}, {5, 22, 2 << 20, true}, {5, 22, 8 << 20, true},
		{11, 22, 64 << 10, true}, {11, 22, 512 << 10, true},
		{5, 24, 64 << 10, true}, {5, 24, 2 << 20, true}, {5, 24, 32 << 20, true},
		{11, 22, 8 << 20, false}, {5, 24, 32 << 20, false},
	} {
		name := fmt.Sprintf("q%d/lgwin%d/%dKiB/flush=%v", tc.quality, tc.lgwin, tc.size>>10, tc.flushEachEvent)
		b.Run(name, func(b *testing.B) {
			wire := brotliSSEWire(tc.size, tc.quality, tc.lgwin, tc.flushEachEvent)
			const streams = 16
			var perStream float64
			for i := 0; i < b.N; i++ {
				before := brotliProbeHeapInUse()
				readers := make([]*brotli.Reader, streams)
				for j := range readers {
					readers[j] = brotli.NewReader(bytes.NewReader(wire))
					if _, err := io.Copy(io.Discard, readers[j]); err != nil {
						b.Fatal(err)
					}
				}
				perStream = float64(brotliProbeHeapInUse()-before) / streams
				runtime.KeepAlive(readers)
			}
			b.ReportMetric(perStream/1024, "resident-KiB/stream")
		})
	}
}

func brotliProbeHeapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

// brotliSSEWire encodes chat-completion chunk events; with flushEachEvent
// every event ends a metablock, as an upstream compressing a live stream does.
func brotliSSEWire(size, quality, lgwin int, flushEachEvent bool) []byte {
	words := strings.Fields("the quick brown fox jumps over lazy dog model token response stream answer reasoning function call json value data user assistant system content delta choice index finish stop length usage prompt completion")
	rng := rand.New(rand.NewSource(1))
	var plain bytes.Buffer
	for plain.Len() < size {
		var content strings.Builder
		for i := 0; i < 3+rng.Intn(6); i++ {
			content.WriteString(words[rng.Intn(len(words))])
			content.WriteByte(' ')
		}
		fmt.Fprintf(&plain, "data: {\"id\":\"chatcmpl-%d\",\"object\":\"chat.completion.chunk\",\"created\":1760000000,\"model\":\"deepseek-chat\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", rng.Intn(1000), content.String())
	}
	var out bytes.Buffer
	w := brotli.NewWriterOptions(&out, brotli.WriterOptions{Quality: quality, LGWin: lgwin})
	if flushEachEvent {
		for _, event := range bytes.SplitAfter(plain.Bytes(), []byte("\n\n")) {
			w.Write(event)
			w.Flush()
		}
	} else {
		w.Write(plain.Bytes())
	}
	w.Close()
	return out.Bytes()
}
