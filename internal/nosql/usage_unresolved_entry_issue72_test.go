package nosql

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// A rejection before a journal append is not proof that no consumption took
// place. Every caller must leave enough durable evidence to report uncertainty.
func TestIssue72EarlyRejectedConsumptionRemainsIncompleteAfterRestart(t *testing.T) {
	for _, entry := range []string{"record_once", "record_batch", "journal_record_once"} {
		t.Run(entry, func(t *testing.T) {
			db := testDB(t)
			future := boundScanTime.Add(48 * time.Hour).Truncate(time.Hour)
			putAggRaw(t, db, future.Format(hourLayout)+"|claude|alice|executor|anthropic", []byte(corruptCell))
			store := clockedStore(db)
			event := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaac01", boundScanTime)
			event.Model = "advisor"
			event.RequestID = "early-rejection"

			switch entry {
			case "record_once":
				if added, err := store.RecordOnce(event); added || !errors.Is(err, ErrUsageIdentityUnverified) {
					t.Fatalf("unverified direct write: added=%v err=%v", added, err)
				}
			case "record_batch":
				if err := store.RecordBatch([]usage.Event{event}); !errors.Is(err, ErrUsageIdentityUnverified) {
					t.Fatalf("unverified batch: %v", err)
				}
			case "journal_record_once":
				journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
				if err != nil {
					t.Fatal(err)
				}
				if added, err := journal.RecordOnce(event); added || !errors.Is(err, ErrUsageIdentityUnverified) {
					t.Fatalf("unverified pre-journal write: added=%v err=%v", added, err)
				}
				if err := journal.Close(); err != nil {
					t.Fatal(err)
				}
			}

			path := db.Path()
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := OpenDB(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			store = clockedStore(db)
			from := event.Timestamp.Truncate(time.Hour)
			to := from.Add(time.Hour - time.Nanosecond)
			filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
			if raw, err := store.List(filter); err != nil || len(raw) != 0 {
				t.Fatalf("unverified event entered raw history: raw=%+v err=%v", raw, err)
			}
			if got, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) || len(got) != 0 {
				t.Errorf("unverified consumption reported as complete stable cost: units=%+v err=%v", got, err)
			}
			if got, err := store.SumQuotaByClientRange(event.ClientName, from, to); err == nil || len(got) != 0 {
				t.Errorf("unverified consumption reported as complete quota: quota=%+v err=%v", got, err)
			}
			if got, err := store.SumByClientRange(event.ClientName, from, to); err == nil || len(got) != 0 {
				t.Errorf("unverified consumption reported as complete legacy quota: quota=%+v err=%v", got, err)
			}
			if got, err := store.SumByClientTargetRange(event.ClientName, from, to); err == nil || len(got) != 0 {
				t.Errorf("unverified consumption reported as complete target quota: quota=%+v err=%v", got, err)
			}
			if got, err := store.SumTargetModelRange(filter); err == nil || len(got) != 0 {
				t.Errorf("unverified consumption reported as complete target/model report: cells=%+v err=%v", got, err)
			}
			if got, err := store.Aggregate(filter, "hour", replayCosts); err == nil {
				t.Errorf("unverified consumption reported as complete aggregate: %+v", got)
			}
			if got, err := store.Summary(event.Timestamp.Add(10*time.Minute), replayCosts); err == nil {
				t.Errorf("unverified consumption reported as complete summary: %+v", got)
			}

			filter.ClientName = "bob"
			if got, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil || len(got) != 0 {
				t.Errorf("unrelated client blocked: units=%+v err=%v", got, err)
			}
			laterFrom, laterTo := to.Add(2*time.Hour), to.Add(3*time.Hour)
			filter.ClientName, filter.From, filter.To = event.ClientName, &laterFrom, &laterTo
			if got, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil || len(got) != 0 {
				t.Errorf("unrelated time range blocked: units=%+v err=%v", got, err)
			}
		})
	}
}

// A valid permanent rollup remains usable after normal pruning. A lost source
// bucket on the next startup must not inherit the old completion marker and
// silently turn that complete historical price into zero.
func TestIssue72MissingQuotaSourceOnOpenIsNotACompleteZero(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(event.Timestamp.Add(2*time.Second), 100); err != nil {
		t.Fatal(err)
	}
	from := event.Timestamp.Truncate(time.Hour)
	to := from.Add(time.Hour - time.Nanosecond)
	filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
	before, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false)
	if err != nil || costUnitTotal(before) != 2 {
		t.Fatalf("healthy pruned rollup is not complete: units=%+v err=%v", before, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(BucketQuotaUsageAggHourly)) }); err != nil {
		t.Fatal(err)
	}
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store = clockedStore(db)
	if added, err := store.RecordOnce(event); added || err != nil {
		t.Errorf("retained receipt must still deduplicate: added=%v err=%v", added, err)
	}
	after, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false)
	if err == nil && costUnitTotal(after) != costUnitTotal(before) {
		t.Errorf("lost source reported as a complete price: before=%+v after=%+v", before, after)
	} else if err != nil && !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Errorf("unexpected incomplete-history error: %v", err)
	}
}

// Recreating a source bucket must not erase the fact that an initialized
// bucket disappeared. The fact has no inferred client or time range.
func TestIssue72MissingAggregateSourceIsRecordedAcrossReopens(t *testing.T) {
	for _, source := range []string{BucketUsageAggHourly, BucketQuotaUsageAggHourly} {
		t.Run(source, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			if err := BackfillUsageAgg(db); err != nil {
				t.Fatal(err)
			}
			if err := BackfillQuotaUsageAgg(db); err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(source)) }); err != nil {
				t.Fatal(err)
			}
			path := db.Path()
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			for reopen := 0; reopen < 2; reopen++ {
				var err error
				db, err = OpenDB(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.View(func(tx *bolt.Tx) error {
					meta := tx.Bucket([]byte(BucketMeta))
					if got := meta.Get([]byte("usage_aggregate_source_missing_v1:" + source)); string(got) != "1" {
						t.Errorf("reopen %d hid missing source %s: marker=%q", reopen, source, got)
					}
					other := BucketUsageAggHourly
					if source == other {
						other = BucketQuotaUsageAggHourly
					}
					if got := meta.Get([]byte("usage_aggregate_source_missing_v1:" + other)); got != nil {
						t.Errorf("reopen %d falsely reported missing source %s: marker=%q", reopen, other, got)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				missing, err := clockedStore(db).MissingUsageAggregateSources()
				if err != nil || len(missing) != 1 || missing[0] != source {
					t.Errorf("reopen %d source status: missing=%v err=%v", reopen, missing, err)
				}
				if added, err := clockedStore(db).RecordOnce(event); added || err != nil {
					t.Errorf("reopen %d lost verified raw identity: added=%v err=%v", reopen, added, err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestIssue72MissingAggregateSourceStatusHealthyAndCorrupt(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	if missing, err := store.MissingUsageAggregateSources(); err != nil || len(missing) != 0 {
		t.Fatalf("fresh database reported missing aggregate sources: missing=%v err=%v", missing, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Put([]byte("usage_aggregate_source_missing_v1:"+BucketUsageAggHourly), []byte("invalid"))
	}); err != nil {
		t.Fatal(err)
	}
	if missing, err := store.MissingUsageAggregateSources(); err == nil || len(missing) != 0 {
		t.Errorf("invalid source status reported as complete: missing=%v err=%v", missing, err)
	}
}
