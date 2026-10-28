package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

const (
	fixedRootRecordID  = "11111111-1111-4111-8111-111111111111"
	fixedLaterRecordID = "22222222-2222-4222-8222-222222222222"
)

func fixedIdentityBundle() usage.Event {
	return usage.Event{
		RecordID:           fixedRootRecordID,
		Timestamp:          time.Date(2026, 10, 21, 9, 45, 0, 0, time.UTC),
		ClientName:         "alice",
		Target:             "anthropic",
		Model:              "executor",
		EndpointType:       "claude",
		RequestID:          "req-fixed",
		Path:               "/v1/messages",
		StatusCode:         200,
		InputTokens:        1_000_000,
		PricingRuleID:      "executor-rate",
		PricingRuleVersion: 1,
		Components: []usage.Component{{
			Model:              "advisor",
			IterationType:      "advisor",
			IterationIndex:     0,
			InputTokens:        2_000_000,
			PricingRuleID:      "advisor-rate",
			PricingRuleVersion: 1,
		}},
	}
}

func usageRecordCounts(t *testing.T, db *bolt.DB) (int, int64) {
	t.Helper()
	raw := 0
	var agg int64
	if err := db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
		for k, _ := cursor.First(); k != nil; k, _ = cursor.Next() {
			raw++
		}
		cursor = tx.Bucket([]byte(BucketUsageAggHourly)).Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			var cell AggCell
			if err := json.Unmarshal(v, &cell); err != nil {
				return err
			}
			agg += cell.recordCount()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return raw, agg
}

func TestRecordOnceRejectsExplicitIdentityWithoutTimestamp(t *testing.T) {
	store := NewUsageStore(testDB(t))
	event := fixedIdentityBundle()
	event.Timestamp = time.Time{}

	added, err := store.RecordOnce(event)
	if err == nil {
		t.Fatal("explicit record ID without a timestamp was accepted")
	}
	if added {
		t.Fatal("rejected identity reported a new insertion")
	}
	raw, agg := usageRecordCounts(t, store.db)
	if raw != 0 || agg != 0 {
		t.Fatalf("rejected identity wrote raw=%d agg=%d", raw, agg)
	}
}

func TestRecordExistsRejectsExplicitIdentityWithoutTimestamp(t *testing.T) {
	store := NewUsageStore(testDB(t))
	event := fixedIdentityBundle()
	event.Timestamp = time.Time{}

	exists, err := store.RecordExists(event)
	if err == nil || exists {
		t.Fatalf("RecordExists accepted explicit identity without time: exists=%v err=%v", exists, err)
	}
	raw, agg := usageRecordCounts(t, store.db)
	if raw != 0 || agg != 0 {
		t.Fatalf("rejected identity wrote raw=%d agg=%d", raw, agg)
	}
}

func TestRecordBatchRejectsExplicitIdentityWithoutTimestamp(t *testing.T) {
	store := NewUsageStore(testDB(t))
	ordinary := usage.Event{Timestamp: time.Time{}, ClientName: "alice", Model: "ordinary"}
	invalid := fixedIdentityBundle()
	invalid.Timestamp = time.Time{}

	if err := store.RecordBatch([]usage.Event{ordinary, invalid}); err == nil {
		t.Fatal("batch with explicit identity without time was accepted")
	}
	raw, agg := usageRecordCounts(t, store.db)
	if raw != 0 || agg != 0 {
		t.Fatalf("rejected batch wrote raw=%d agg=%d", raw, agg)
	}
}

func TestRecordOnceAllowsGeneratedIdentityWithoutTimestamp(t *testing.T) {
	store := NewUsageStore(testDB(t))
	event := usage.Event{Timestamp: time.Time{}, ClientName: "alice", Model: "ordinary"}

	for i := 0; i < 2; i++ {
		added, err := store.RecordOnce(event)
		if err != nil || !added {
			t.Fatalf("ordinary generated identity %d: added=%v err=%v", i, added, err)
		}
	}
	raw, agg := usageRecordCounts(t, store.db)
	if raw != 2 || agg != 2 {
		t.Fatalf("ordinary events did not remain independent: raw=%d agg=%d", raw, agg)
	}
}

func TestRecordOnceRetryIgnoresOnlyDerivedPricingRuleMetadata(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	event := fixedIdentityBundle()
	added, err := store.RecordOnce(event)
	if err != nil || !added {
		t.Fatalf("initial record: added=%v err=%v", added, err)
	}
	rawBefore := indexBucketSnapshot(t, db, BucketUsageEvents)

	retry := usage.CloneEvent(event)
	retry.PricingRuleID = "executor-rate-v2"
	retry.PricingRuleVersion = 2
	retry.Components[0].PricingRuleID = "advisor-rate-v2"
	retry.Components[0].PricingRuleVersion = 2
	added, err = store.RecordOnce(retry)
	if err != nil || added {
		t.Fatalf("price metadata-only retry: added=%v err=%v", added, err)
	}
	if rawAfter := indexBucketSnapshot(t, db, BucketUsageEvents); !reflect.DeepEqual(rawBefore, rawAfter) {
		t.Fatal("price metadata-only retry overwrote first accepted evidence")
	}
	raw, agg := usageRecordCounts(t, store.db)
	if raw != 2 || agg != 2 {
		t.Fatalf("price metadata-only retry changed rows: raw=%d agg=%d", raw, agg)
	}

	mutations := []struct {
		name   string
		mutate func(*usage.Event)
	}{
		{"model", func(e *usage.Event) { e.Model = "executor-v2" }},
		{"input tokens", func(e *usage.Event) { e.InputTokens++ }},
		{"component tokens", func(e *usage.Event) { e.Components[0].InputTokens++ }},
		{"non billable", func(e *usage.Event) { e.Components[0].NonBillable = true }},
		{"pricing context", func(e *usage.Event) {
			e.PricingContext = &usage.PricingContext{Version: 1, TotalInputTokens: 7, TotalInputKnown: true, TotalInputValid: true}
		}},
		{"diagnostic", func(e *usage.Event) { e.UsageDiagnostic = "changed" }},
		{"cost incomplete", func(e *usage.Event) { e.CostIncomplete = true }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := usage.CloneEvent(event)
			mutation.mutate(&changed)
			if _, err := store.RecordOnce(changed); err == nil {
				t.Fatalf("%s change was accepted as the same identity", mutation.name)
			}
			raw, agg := usageRecordCounts(t, store.db)
			if raw != 2 || agg != 2 {
				t.Fatalf("%s conflict changed rows: raw=%d agg=%d", mutation.name, raw, agg)
			}
		})
	}
}

func TestRecordOnceRawHitRepairsIndexFromStoredBytes(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	event := fixedIdentityBundle()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	migrateClientIndex(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		idx := tx.Bucket([]byte(BucketUsageEventsByClient))
		cursor := idx.Cursor()
		var keys [][]byte
		for k, _ := cursor.First(); k != nil; k, _ = cursor.Next() {
			keys = append(keys, append([]byte(nil), k...))
		}
		for _, key := range keys {
			if err := idx.Delete(key); err != nil {
				return err
			}
		}
		hours := tx.Bucket([]byte(BucketUsageClientHours))
		cursor = hours.Cursor()
		keys = nil
		for k, _ := cursor.First(); k != nil; k, _ = cursor.Next() {
			keys = append(keys, append([]byte(nil), k...))
		}
		for _, key := range keys {
			if err := hours.Delete(key); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	retry := usage.CloneEvent(event)
	retry.PricingRuleID = "executor-rate-v2"
	retry.PricingRuleVersion = 2
	retry.Components[0].PricingRuleID = "advisor-rate-v2"
	retry.Components[0].PricingRuleVersion = 2
	added, err := store.RecordOnce(retry)
	if err != nil || added {
		t.Fatalf("raw hit repair: added=%v err=%v", added, err)
	}
	got, err := store.ReadClientEvents(context.Background(), "alice", event.Timestamp.Truncate(time.Hour), event.Timestamp.Truncate(time.Hour).Add(time.Hour), 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("index was not repaired from the stored raw bytes: rows=%d err=%v", len(got), err)
	}
}

func TestUsageJournalRejectsExplicitIdentityWithoutTimestamp(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	path := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	journal, err := NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	event := fixedIdentityBundle()
	event.Timestamp = time.Time{}

	if err := journal.Record(event); err == nil {
		t.Fatal("journal accepted explicit identity without a timestamp")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("rejected event touched the journal: %v", err)
	}
	added, err := journal.RecordOnce(event)
	if err == nil || added {
		t.Fatalf("RecordOnce accepted explicit identity without time: added=%v err=%v", added, err)
	}
	raw, agg := usageRecordCounts(t, db)
	if raw != 0 || agg != 0 {
		t.Fatalf("rejected journal event wrote raw=%d agg=%d", raw, agg)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("rejected RecordOnce touched the journal: %v", err)
	}
}

func TestUsageJournalPendingRetryIgnoresDerivedPricingMetadata(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	path := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	primary := &failFirstWithExists{store: store}
	journal, err := NewUsageJournal(primary, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	event := fixedIdentityBundle()
	if err := journal.Record(event); err != nil {
		t.Fatal(err)
	}
	if lines := journalEventLines(t, journal); lines != 1 {
		t.Fatalf("pending lines=%d, want 1", lines)
	}

	retry := usage.CloneEvent(event)
	retry.PricingRuleID = "executor-rate-v2"
	retry.PricingRuleVersion = 2
	retry.Components[0].PricingRuleID = "advisor-rate-v2"
	retry.Components[0].PricingRuleVersion = 2
	added, err := journal.RecordOnce(retry)
	if err != nil || added {
		t.Fatalf("pending price metadata-only retry: added=%v err=%v", added, err)
	}
	if lines := journalEventLines(t, journal); lines != 1 {
		t.Fatalf("matched pending retry appended a line: %d", lines)
	}

	changed := usage.CloneEvent(retry)
	changed.InputTokens++
	if _, err := journal.RecordOnce(changed); err == nil {
		t.Fatal("changed pending payload was accepted")
	}
	if lines := journalEventLines(t, journal); lines != 1 {
		t.Fatalf("conflicting pending retry changed the journal: %d lines", lines)
	}
	if err := store.checkUnresolvedClient(event.ClientName); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("conflicting pending retry was not preserved as unresolved: %v", err)
	}
}

type failFirstWithExists struct {
	store  *UsageStore
	failed bool
}

func (r *failFirstWithExists) Record(event usage.Event) error {
	if !r.failed {
		r.failed = true
		return errors.New("primary unavailable")
	}
	return r.store.Record(event)
}

func (r *failFirstWithExists) RecordExists(event usage.Event) (bool, error) {
	return r.store.RecordExists(event)
}

func (r *failFirstWithExists) RegisterUnresolvedUsage(event usage.Event, raw []byte) error {
	return r.store.RegisterUnresolvedUsage(event, raw)
}

func journalEventLines(t *testing.T, journal *UsageJournal) int {
	t.Helper()
	journal.mu.Lock()
	defer journal.mu.Unlock()
	data, err := os.ReadFile(journal.path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	lines := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event usage.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("journal line is not an event: %v", err)
		}
		lines++
	}
	return lines
}
