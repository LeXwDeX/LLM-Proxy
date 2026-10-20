package nosql

import (
	"encoding/json"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// putAgg is a test helper that writes one AggCell directly to the hourly bucket.
func putAgg(t *testing.T, db *bolt.DB, key string, cell AggCell) {
	t.Helper()
	err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(BucketUsageAggHourly))
		if err != nil {
			return err
		}
		data, err := json.Marshal(cell)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), data)
	})
	if err != nil {
		t.Fatalf("putAgg %q: %v", key, err)
	}
}

func TestSumByClientRangeEmptyDB(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	result, err := store.SumByClientRange("alice",
		time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}
	if result == nil {
		t.Error("expected non-nil empty map")
	}
}

func TestSumByClientRangeFromAfterTo(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	putAgg(t, db, "2026-06-08T10|openai|alice|gpt-4", AggCell{InputTokens: 100, OutputTokens: 50, Requests: 1})

	result, err := store.SumByClientRange("alice",
		time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("from > to should return empty, got %d entries", len(result))
	}
}

func TestSumByClientRangeFromEqualsTo(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	putAgg(t, db, "2026-06-08T10|openai|alice|gpt-4", AggCell{InputTokens: 100, OutputTokens: 50, CachedTokens: 10, Requests: 1, Success: 1})

	// from == to (both truncate to same hour bucket) should scan that single hour.
	now := time.Date(2026, 6, 8, 10, 30, 0, 0, time.UTC)
	result, err := store.SumByClientRange("alice", now, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("from == to: expected 1 entry, got %d: %v", len(result), result)
	}
	totals, ok := result["openai:gpt-4"]
	if !ok {
		t.Fatalf("missing groupKey 'openai:gpt-4', got %v", result)
	}
	if totals.InputTokens != 100 {
		t.Errorf("InputTokens: got %d, want 100", totals.InputTokens)
	}
	if totals.OutputTokens != 50 {
		t.Errorf("OutputTokens: got %d, want 50", totals.OutputTokens)
	}
	if totals.CachedTokens != 10 {
		t.Errorf("CachedTokens: got %d, want 10", totals.CachedTokens)
	}
}

func TestSumByClientRangeSingleRecord(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	putAgg(t, db, "2026-06-08T10|openai|alice|gpt-4", AggCell{
		InputTokens: 100, OutputTokens: 50, CachedTokens: 10, Requests: 1, Success: 1,
	})

	from := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 8, 11, 0, 0, 0, time.UTC)
	result, err := store.SumByClientRange("alice", from, to)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	totals, ok := result["openai:gpt-4"]
	if !ok {
		t.Fatalf("missing groupKey 'openai:gpt-4', got %v", result)
	}
	if totals.InputTokens != 100 {
		t.Errorf("InputTokens: got %d, want 100", totals.InputTokens)
	}
	if totals.OutputTokens != 50 {
		t.Errorf("OutputTokens: got %d, want 50", totals.OutputTokens)
	}
	if totals.CachedTokens != 10 {
		t.Errorf("CachedTokens: got %d, want 10", totals.CachedTokens)
	}
}

// TestSumByClientRangeIncludesCacheCreationTokens guards the UsageTotals
// regression from commit 90780b1: the quota Manager consumes SumByClientRange
// to compute USD, and cache-creation vs cache-read tokens carry different unit
// prices. If only CachedTokens (read) is returned while CacheCreationTokens is
// dropped, the manager's cost calculation falls back to using CachedTokens at
// the CachedInput (write) price and silently over-charges clients.
func TestSumByClientRangeIncludesCacheCreationTokens(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	putAgg(t, db, "2026-06-08T10|claude|alice|claude-opus-4-1", AggCell{
		InputTokens:         1000,
		OutputTokens:        500,
		CachedTokens:        200,  // cache read hits
		CacheCreationTokens: 3000, // cache write
		Requests:            1,
		Success:             1,
	})
	putAgg(t, db, "2026-06-08T11|claude|alice|claude-opus-4-1", AggCell{
		InputTokens:         2000,
		OutputTokens:        800,
		CachedTokens:        1000,
		CacheCreationTokens: 4000,
		Requests:            2,
		Success:             2,
	})

	from := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	result, err := store.SumByClientRange("alice", from, to)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	totals, ok := result["claude:claude-opus-4-1"]
	if !ok {
		t.Fatalf("missing groupKey, got %v", result)
	}

	if totals.CachedTokens != 1200 {
		// 200 + 1000 = cache read totals
		t.Errorf("CachedTokens (cache read): got %d, want 1200", totals.CachedTokens)
	}
	if totals.CacheCreationTokens != 7000 {
		// 3000 + 4000 = cache creation totals; quota cost formula multiplies this
		// by CacheWritePer1MToken, distinct from CacheReadPer1MToken applied to
		// CachedTokens. Losing this field silently mis-bills the customer.
		t.Errorf("CacheCreationTokens (cache write): got %d, want 7000", totals.CacheCreationTokens)
	}
}

func TestSumByClientRangeCrossHours(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	// Same client + model across 3 hours → should sum.
	putAgg(t, db, "2026-06-08T10|openai|alice|gpt-4", AggCell{InputTokens: 100, OutputTokens: 50, CachedTokens: 5, Requests: 1})
	putAgg(t, db, "2026-06-08T11|openai|alice|gpt-4", AggCell{InputTokens: 200, OutputTokens: 100, CachedTokens: 10, Requests: 2})
	putAgg(t, db, "2026-06-08T12|openai|alice|gpt-4", AggCell{InputTokens: 300, OutputTokens: 150, CachedTokens: 15, Requests: 3})

	from := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	result, err := store.SumByClientRange("alice", from, to)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	totals := result["openai:gpt-4"]
	if totals.InputTokens != 600 {
		t.Errorf("InputTokens: got %d, want 600", totals.InputTokens)
	}
	if totals.OutputTokens != 300 {
		t.Errorf("OutputTokens: got %d, want 300", totals.OutputTokens)
	}
	if totals.CachedTokens != 30 {
		t.Errorf("CachedTokens: got %d, want 30", totals.CachedTokens)
	}
}

func TestSumByClientRangeSameHourDifferentModels(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	putAgg(t, db, "2026-06-08T10|openai|alice|gpt-4", AggCell{InputTokens: 100, OutputTokens: 50, Requests: 1})
	putAgg(t, db, "2026-06-08T10|openai|alice|gpt-3.5", AggCell{InputTokens: 200, OutputTokens: 100, Requests: 1})

	from := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 8, 11, 0, 0, 0, time.UTC)
	result, err := store.SumByClientRange("alice", from, to)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 groupKeys, got %d: %v", len(result), result)
	}
	if gpt4 := result["openai:gpt-4"]; gpt4.InputTokens != 100 {
		t.Errorf("gpt-4 InputTokens: got %d, want 100", gpt4.InputTokens)
	}
	if gpt35 := result["openai:gpt-3.5"]; gpt35.InputTokens != 200 {
		t.Errorf("gpt-3.5 InputTokens: got %d, want 200", gpt35.InputTokens)
	}
}

func TestSumByClientRangeSameHourDifferentEndpointTypes(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	putAgg(t, db, "2026-06-08T10|openai|alice|gpt-4", AggCell{InputTokens: 100, OutputTokens: 50, Requests: 1})
	putAgg(t, db, "2026-06-08T10|claude|alice|claude-3", AggCell{InputTokens: 500, OutputTokens: 200, Requests: 1})

	from := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 8, 11, 0, 0, 0, time.UTC)
	result, err := store.SumByClientRange("alice", from, to)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 groupKeys, got %d: %v", len(result), result)
	}
	if o := result["openai:gpt-4"]; o.InputTokens != 100 {
		t.Errorf("openai:gpt-4 InputTokens: got %d, want 100", o.InputTokens)
	}
	if c := result["claude:claude-3"]; c.InputTokens != 500 {
		t.Errorf("claude:claude-3 InputTokens: got %d, want 500", c.InputTokens)
	}
}

func TestSumByClientRangeClientFilter(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	putAgg(t, db, "2026-06-08T10|openai|alice|gpt-4", AggCell{InputTokens: 100, OutputTokens: 50, Requests: 1})
	putAgg(t, db, "2026-06-08T10|openai|bob|gpt-4", AggCell{InputTokens: 200, OutputTokens: 100, Requests: 1})
	putAgg(t, db, "2026-06-08T10|openai|charlie|gpt-4", AggCell{InputTokens: 300, OutputTokens: 150, Requests: 1})

	from := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 8, 11, 0, 0, 0, time.UTC)

	// Query for alice only.
	result, err := store.SumByClientRange("alice", from, to)
	if err != nil {
		t.Fatalf("alice err: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("alice: expected 1 groupKey, got %d: %v", len(result), result)
	}
	if a := result["openai:gpt-4"]; a.InputTokens != 100 {
		t.Errorf("alice InputTokens: got %d, want 100", a.InputTokens)
	}

	// Query for bob only.
	result, err = store.SumByClientRange("bob", from, to)
	if err != nil {
		t.Fatalf("bob err: %v", err)
	}
	if b := result["openai:gpt-4"]; b.InputTokens != 200 {
		t.Errorf("bob InputTokens: got %d, want 200", b.InputTokens)
	}

	// Query for non-existent client.
	result, err = store.SumByClientRange("dave", from, to)
	if err != nil {
		t.Fatalf("dave err: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("dave: expected 0 groupKeys, got %d", len(result))
	}
}

func TestSumByClientRangeOutOfWindow(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	putAgg(t, db, "2026-06-08T10|openai|alice|gpt-4", AggCell{InputTokens: 100, OutputTokens: 50, Requests: 1})
	putAgg(t, db, "2026-06-08T13|openai|alice|gpt-4", AggCell{InputTokens: 200, OutputTokens: 100, Requests: 1})
	// In-window row.
	putAgg(t, db, "2026-06-08T11|openai|alice|gpt-4", AggCell{InputTokens: 300, OutputTokens: 150, Requests: 1})

	from := time.Date(2026, 6, 8, 11, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	result, err := store.SumByClientRange("alice", from, to)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	totals := result["openai:gpt-4"]
	if totals.InputTokens != 300 {
		t.Errorf("expected only in-window row: got %d, want 300", totals.InputTokens)
	}
}

func TestSumByClientRangeLegacyEmptyEndpointType(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	// Key with empty endpoint_type segment.
	putAgg(t, db, "2026-06-08T10||alice|gpt-4", AggCell{InputTokens: 100, OutputTokens: 50, CachedTokens: 5, Requests: 1})

	from := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 8, 11, 0, 0, 0, time.UTC)
	result, err := store.SumByClientRange("alice", from, to)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 groupKey, got %d: %v", len(result), result)
	}
	totals, ok := result[":gpt-4"]
	if !ok {
		t.Fatalf("missing groupKey ':gpt-4', got %v", result)
	}
	if totals.InputTokens != 100 {
		t.Errorf("InputTokens: got %d, want 100", totals.InputTokens)
	}
}

func TestSumByClientRangeMultiClient(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	// 3 clients × 2 hours × 2 models → 12 rows.
	clients := []string{"alice", "bob", "charlie"}
	hours := []string{"2026-06-08T10", "2026-06-08T11"}
	models := []string{"gpt-4", "gpt-3.5"}
	for _, c := range clients {
		for _, h := range hours {
			for _, m := range models {
				key := h + "|openai|" + c + "|" + m
				putAgg(t, db, key, AggCell{InputTokens: 100, OutputTokens: 50, CachedTokens: 5, Requests: 1, Success: 1})
			}
		}
	}

	from := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 8, 11, 0, 0, 0, time.UTC)

	// Query each client: expects 2 groupKeys with 2× aggregation each.
	for _, c := range clients {
		result, err := store.SumByClientRange(c, from, to)
		if err != nil {
			t.Fatalf("%s err: %v", c, err)
		}
		if len(result) != 2 {
			t.Errorf("%s: expected 2 groupKeys, got %d: %v", c, len(result), result)
			continue
		}
		for _, gk := range []string{"openai:gpt-4", "openai:gpt-3.5"} {
			totals, ok := result[gk]
			if !ok {
				t.Errorf("%s: missing groupKey %s", c, gk)
				continue
			}
			// 2 hours × 100 input = 200.
			if totals.InputTokens != 200 {
				t.Errorf("%s %s InputTokens: got %d, want 200", c, gk, totals.InputTokens)
			}
			if totals.OutputTokens != 100 {
				t.Errorf("%s %s OutputTokens: got %d, want 100", c, gk, totals.OutputTokens)
			}
			if totals.CachedTokens != 10 {
				t.Errorf("%s %s CachedTokens: got %d, want 10", c, gk, totals.CachedTokens)
			}
		}
	}
}

func TestSumByClientRangeCorruptKey(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	// Write one valid record.
	putAgg(t, db, "2026-06-08T10|openai|alice|gpt-4", AggCell{InputTokens: 100, OutputTokens: 50, Requests: 1, Success: 1})

	// Write one record with a malformed key (not 4 pipe-separated segments).
	err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(BucketUsageAggHourly))
		if err != nil {
			return err
		}
		data, _ := json.Marshal(AggCell{InputTokens: 999, OutputTokens: 999, Requests: 1})
		return b.Put([]byte("notakey"), data)
	})
	if err != nil {
		t.Fatalf("putAgg corrupt key: %v", err)
	}

	from := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 8, 11, 0, 0, 0, time.UTC)
	result, err := store.SumByClientRange("alice", from, to)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Should still find the valid record.
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d: %v", len(result), result)
	}
	if totals := result["openai:gpt-4"]; totals.InputTokens != 100 {
		t.Errorf("InputTokens: got %d, want 100", totals.InputTokens)
	}
}

func TestSumByClientRangeCorruptValue(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	// Write one valid record.
	putAgg(t, db, "2026-06-08T10|openai|alice|gpt-4", AggCell{InputTokens: 100, OutputTokens: 50, Requests: 1, Success: 1})

	// Write one record with a valid key but corrupt (non-JSON) value.
	err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(BucketUsageAggHourly))
		if err != nil {
			return err
		}
		return b.Put([]byte("2026-06-08T10|openai|alice|gpt-3.5"), []byte("notjson"))
	})
	if err != nil {
		t.Fatalf("putAgg corrupt value: %v", err)
	}

	from := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 8, 11, 0, 0, 0, time.UTC)
	result, err := store.SumByClientRange("alice", from, to)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Should find only the valid record, skipping corrupt one.
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d: %v", len(result), result)
	}
	if totals := result["openai:gpt-4"]; totals.InputTokens != 100 {
		t.Errorf("InputTokens: got %d, want 100", totals.InputTokens)
	}
}
