package nosql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func seedRawUsageRows(t *testing.T, db *bolt.DB, n int, client string) {
	t.Helper()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(BucketUsageEvents))
		if err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			at := base.Add(time.Duration(i) * time.Second)
			id := fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012d", i)
			raw, _ := json.Marshal(usage.Event{RecordID: id, Timestamp: at, ClientName: client, Model: "m"})
			if err := b.Put([]byte(usageKey(at, id)), raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Issue #151: a filter that barely matches (and no From) used to reverse-scan
// the whole bucket inside one View. The scan now has a budget and reports an
// explicit truncated flag, and honors context cancellation.
func TestListContextScanBudgetReportsTruncation(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	seedRawUsageRows(t, db, 300, "other")
	old := usageListScanBudget
	usageListScanBudget = 100
	defer func() { usageListScanBudget = old }()

	events, truncated, err := store.ListContext(context.Background(), usage.Filter{ClientName: "nobody", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(events) != 0 {
		t.Fatalf("truncated=%v events=%d, want truncated with no matches", truncated, len(events))
	}
	// A satisfied limit before the budget runs out is not truncation.
	events, truncated, err = store.ListContext(context.Background(), usage.Filter{ClientName: "other", Limit: 10})
	if err != nil || truncated || len(events) != 10 {
		t.Fatalf("truncated=%v events=%d err=%v", truncated, len(events), err)
	}
}

func TestListContextHonorsCancellation(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	seedRawUsageRows(t, db, 600, "other")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.ListContext(ctx, usage.Filter{ClientName: "nobody"}); err == nil {
		t.Fatal("cancelled scan returned no error")
	}
}

// A scan whose budget runs out exactly where the result is already complete
// (heap full with older rows next, or the next row below From) is not
// truncation: the completeness breaks must be checked before the budget.
func TestListContextScanBudgetBoundaryIsNotTruncation(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	seedRawUsageRows(t, db, 300, "other")
	old := usageListScanBudget
	usageListScanBudget = 10
	defer func() { usageListScanBudget = old }()

	events, truncated, err := store.ListContext(context.Background(), usage.Filter{ClientName: "other", Limit: 10})
	if err != nil || truncated || len(events) != 10 {
		t.Fatalf("heap-full boundary: truncated=%v events=%d err=%v, want complete 10", truncated, len(events), err)
	}
	from := time.Date(2026, 10, 1, 0, 0, 290, 0, time.UTC)
	events, truncated, err = store.ListContext(context.Background(), usage.Filter{ClientName: "other", Limit: 100, From: &from})
	if err != nil || truncated || len(events) != 10 {
		t.Fatalf("lower-bound boundary: truncated=%v events=%d err=%v, want complete 10", truncated, len(events), err)
	}
	// One more matching row beyond the budget is still reported as truncation.
	from = from.Add(-time.Second)
	events, truncated, err = store.ListContext(context.Background(), usage.Filter{ClientName: "other", Limit: 100, From: &from})
	if err != nil || !truncated || len(events) != 10 {
		t.Fatalf("over budget: truncated=%v events=%d err=%v, want truncated 10", truncated, len(events), err)
	}
}
