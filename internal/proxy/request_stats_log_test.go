package proxy

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
)

type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) lines(substr string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, line := range strings.Split(b.buf.String(), "\n") {
		if strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return out
}

// A full statistics queue under overload must not log one Error per request:
// at most one line per 10s, carrying how many completions were dropped. The
// record_failures counter still counts every one.
func TestRequestStatsFailureLogIsRateLimited(t *testing.T) {
	logs := &syncLogBuffer{}
	service, err := NewService(&config.Config{}, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	service.requestStatsLog.now = func() time.Time { return clock }
	recorder := failingStatsRecorder{err: errors.New("request stats queue full; completion dropped")}
	for i := 0; i < 100; i++ {
		service.recordRequestCompletion(recorder, requeststats.Success)
		clock = clock.Add(50 * time.Millisecond)
	}
	got := logs.lines("request stats record failed")
	if len(got) != 1 || !strings.Contains(got[0], "dropped=1") || !strings.Contains(got[0], "level=ERROR") {
		t.Fatalf("first failure must log once with its count: %q", got)
	}
	clock = clock.Add(10 * time.Second)
	service.recordRequestCompletion(recorder, requeststats.Success)
	got = logs.lines("request stats record failed")
	if len(got) != 2 || !strings.Contains(got[1], "dropped=100") {
		t.Fatalf("next window must report the 99 suppressed failures plus this one: %q", got)
	}
	if service.RequestStatsRecordFailures() != 101 {
		t.Fatalf("record_failures = %d, want every failure counted", service.RequestStatsRecordFailures())
	}
}
