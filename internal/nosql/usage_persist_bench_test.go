package nosql

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func boltTxID(tb testing.TB, db *bolt.DB) int {
	tb.Helper()
	var id int
	if err := db.View(func(tx *bolt.Tx) error { id = tx.ID(); return nil }); err != nil {
		tb.Fatal(err)
	}
	return id
}

// BenchmarkUsageRecordConcurrent200 compares 200 concurrent request handlers
// persisting usage: synchronous db.Batch with the old and new MaxBatchDelay,
// and the asynchronous write-ahead journal. Every write commit bumps the bbolt
// meta txid, so its delta counts commits. Throughput includes draining the
// asynchronous writer.
//
//	go test ./internal/nosql -run '^$' -bench UsageRecordConcurrent200 -benchtime 20000x
func BenchmarkUsageRecordConcurrent200(b *testing.B) {
	const workers = 200
	for _, mode := range []struct {
		name  string
		delay time.Duration
		async bool
	}{
		{"sync_batch_10ms", 10 * time.Millisecond, false},
		{"sync_batch_2ms", 2 * time.Millisecond, false},
		{"async_journal_2ms", 2 * time.Millisecond, true},
	} {
		b.Run(mode.name, func(b *testing.B) {
			db := testDB(b)
			db.MaxBatchDelay = mode.delay
			store := NewUsageStore(db)
			var recorder usage.Recorder = store
			var journal *UsageJournal
			if mode.async {
				var err error
				journal, err = NewUsageJournal(store, filepath.Join(b.TempDir(), "journal.jsonl"), nil)
				if err != nil {
					b.Fatal(err)
				}
				if err := journal.EnableAsyncCommit(UsageJournalAsyncOptions{}); err != nil {
					b.Fatal(err)
				}
				recorder = journal
			}
			startTx := boltTxID(b, db)
			var next atomic.Int64
			var latencyTotal atomic.Int64
			b.ResetTimer()
			started := time.Now()
			var wg sync.WaitGroup
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						i := next.Add(1)
						if i > int64(b.N) {
							return
						}
						begin := time.Now()
						if err := recorder.Record(asyncTestEvent(int(i), "bench")); err != nil {
							b.Error(err)
							return
						}
						latencyTotal.Add(int64(time.Since(begin)))
					}
				}()
			}
			wg.Wait()
			if journal != nil {
				journal.Flush()
			}
			elapsed := time.Since(started)
			b.StopTimer()
			if journal != nil {
				if err := journal.Close(); err != nil {
					b.Fatal(err)
				}
			}
			commits := boltTxID(b, db) - startTx
			b.ReportMetric(float64(b.N)/elapsed.Seconds(), "events/s")
			b.ReportMetric(float64(commits), "commits")
			b.ReportMetric(float64(latencyTotal.Load())/float64(b.N)/1e6, "record_ms")
		})
	}
}
