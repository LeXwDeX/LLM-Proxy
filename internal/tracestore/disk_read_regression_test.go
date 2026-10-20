package tracestore

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiskReadEscapedBodyAndLaterRecords(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.RingBufferSize = 1
	cfg.DiskPath = filepath.Join(t.TempDir(), "trace.log")
	store := New(cfg, nil)
	body := strings.Repeat("<", cfg.MaxBodySize)
	store.Record(&TraceRecord{TraceID: "escaped", Timestamp: time.Now(), RequestBody: body, ResponseBody: body})
	store.Record(&TraceRecord{TraceID: "later", Timestamp: time.Now().Add(time.Second), RequestBody: "later"})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reader := New(cfg, nil)
	defer reader.Close()
	for _, id := range []string{"escaped", "later"} {
		if got := reader.Get(id); got == nil || got.TraceID != id {
			t.Errorf("disk Get(%q) did not recover persisted record", id)
		}
	}
	if got := reader.List(0, 2); len(got) != 2 || got[0].TraceID != "later" || got[1].TraceID != "escaped" {
		t.Errorf("disk List returned %d records, want both in timestamp order", len(got))
	}
}
