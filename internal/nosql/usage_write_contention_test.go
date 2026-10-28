package nosql

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// The fixture uses the normal row writer in one transaction, rather than
// repeatedly invoking RecordBatch and including its proof scans in setup.
func contentionHistory(t *testing.T, db *bolt.DB, n int, legacy bool) {
	t.Helper()
	err := db.Update(func(tx *bolt.Tx) error {
		if legacy {
			if err := tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey)); err != nil {
				return err
			}
		}
		for i := 0; i < n; i++ {
			e := fixedPlainEvent()
			e.RecordID = fmt.Sprintf("00000000-0000-4000-8000-%012x", i+1)
			e.ClientName = fmt.Sprintf("history-%03d", i%100)
			e.Timestamp = e.Timestamp.Add(-time.Duration(i%24+1) * time.Hour)
			if err := writeUsageRowTx(tx, e, usageKey(e.Timestamp, e.RecordID)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		for _, name := range []string{BucketUsageEvents, BucketUsageEventsByClient} {
			if got := tx.Bucket([]byte(name)).Stats().KeyN; got != n {
				return fmt.Errorf("%s rows=%d, want %d", name, got, n)
			}
		}
		for _, name := range []string{BucketUsageAggHourly, BucketQuotaUsageAggHourly, rawOwnerBucket} {
			if b := tx.Bucket([]byte(name)); b == nil || b.Stats().KeyN == 0 {
				return fmt.Errorf("%s missing fixture data", name)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// CONTENTION_ROWS and CONTENTION_BATCH permit larger diagnostic runs while
// keeping the regular test small. Times are observations, not assertions.
func TestUsageWriteContention(t *testing.T) {
	sizes := []int{1000, 4000}
	batchSize := 8
	if text := os.Getenv("CONTENTION_ROWS"); text != "" {
		n, err := strconv.Atoi(text)
		if err != nil || n < 1 || n > 250000 {
			t.Fatal("invalid CONTENTION_ROWS")
		}
		sizes = []int{n}
	}
	if text := os.Getenv("CONTENTION_BATCH"); text != "" {
		n, err := strconv.Atoi(text)
		if err != nil || n < 1 || n > 1000 {
			t.Fatal("invalid CONTENTION_BATCH")
		}
		batchSize = n
	}
	for _, size := range sizes {
		for _, legacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("rows=%d/legacy=%t", size, legacy), func(t *testing.T) {
				db := testDB(t)
				contentionHistory(t, db, size, legacy)
				store := clockedStore(db)
				if err := store.EnsureUsageReceipts(); err != nil {
					t.Fatal(err)
				}
				target := config.Target{Name: "test", Endpoint: "https://example.com", ResourcePathPrefix: "/", APIKey: "fixture", UsageAdapter: config.UsageAdapterStandard}
				targets := NewTargetStore(db)
				if err := targets.Create(target); err != nil {
					t.Fatal(err)
				}
				event := fixedPlainEvent()
				event.ClientName = "new-client"
				event.RecordID = "10000000-0000-4000-8000-000000000001"
				if err := db.View(func(tx *bolt.Tx) error {
					rows, _, err := normalizeUsageBundle(event)
					if err != nil {
						return err
					}
					if freshScopeProven(tx, rows) != legacy {
						return fmt.Errorf("unexpected fresh proof for legacy=%t", legacy)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				firstStart := time.Now()
				if err := store.RecordBatch([]usage.Event{event}); err != nil {
					t.Fatal(err)
				}
				t.Logf("first record=%s", time.Since(firstStart))
				if err := db.View(func(tx *bolt.Tx) error {
					rows, keys, err := normalizeUsageBundle(event)
					if err != nil {
						return err
					}
					if validFreshAttestation(tx, rows, keys) != legacy {
						return fmt.Errorf("unexpected fresh attestation for legacy=%t", legacy)
					}
					if continuationScopeProven(tx, rows) != legacy {
						return fmt.Errorf("unexpected continuation proof for legacy=%t", legacy)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				batch := make([]usage.Event, batchSize)
				for i := range batch {
					batch[i] = event
					batch[i].RecordID = fmt.Sprintf("20000000-0000-4000-8000-%012x", i+1)
					batch[i].Timestamp = event.Timestamp.Add(time.Duration(i+1) * time.Millisecond)
				}
				type result struct {
					elapsed time.Duration
					err     error
				}
				done := make(chan result, 1)
				started := make(chan struct{})
				go func() {
					start := time.Now()
					close(started)
					err := store.RecordBatch(batch)
					done <- result{time.Since(start), err}
				}()
				<-started
				time.Sleep(time.Millisecond)
				targetDone := make(chan result, 1)
				go func() {
					start := time.Now()
					err := targets.Update(target.Name, target)
					targetDone <- result{time.Since(start), err}
				}()
				timer := time.AfterFunc(5*time.Second, func() {
					buf := make([]byte, 1<<20)
					n := runtime.Stack(buf, true)
					fmt.Fprintf(os.Stderr, "contention stack after 5s:\n%s\n", buf[:n])
				})
				r := <-done
				w := <-targetDone
				timer.Stop()
				if r.err != nil || w.err != nil {
					t.Fatalf("batch error=%v target error=%v", r.err, w.err)
				}
				t.Logf("same-hour batch=%d duration=%s target update duration=%s (launched ~1ms after batch; overlap not guaranteed)", batchSize, r.elapsed, w.elapsed)
			})
		}
	}
}
