package tracestore

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAuditRegressionRecordRacesClose(t *testing.T) {
	s := New(Config{Enabled: true, RingBufferSize: 2, ChannelBuffer: 1}, nil)
	s.mu.Lock()
	done := make(chan any, 1)
	go func() { defer func() { done <- recover() }(); s.Record(&TraceRecord{TraceID: "blocked"}) }()
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := s.ring.Load("blocked"); ok {
			break
		}
		if time.Now().After(deadline) {
			s.mu.Unlock()
			t.Fatal("record did not reach ring")
		}
		runtime.Gosched()
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		s.mu.Unlock()
		<-done
		t.Fatalf("Close returned before the in-progress record: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	s.mu.Unlock()
	if p := <-done; p != nil {
		t.Fatalf("Record panicked after concurrent Close: %v", p)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	s.Record(&TraceRecord{TraceID: "after-close"})
	if s.Get("after-close") != nil {
		t.Fatal("recorded after Close")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditRegressionTruncatedBodiesReleaseLargeBackingStorage(t *testing.T) {
	s := New(Config{Enabled: true, RingBufferSize: 16, ChannelBuffer: 1, MaxBodySize: 1024}, nil)
	defer s.Close()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < 16; i++ {
		s.Record(&TraceRecord{TraceID: fmt.Sprint(i), RequestBody: strings.Repeat("x", 4*1024*1024)})
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(s)
	if retained := int64(after.HeapAlloc) - int64(before.HeapAlloc); retained > 32*1024*1024 {
		t.Fatalf("1KiB trace slices retained large backing strings: %d bytes", retained)
	}
}
