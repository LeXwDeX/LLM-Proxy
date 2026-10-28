package nosql

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// The only surviving quota cell must not disappear from accounting when its
// backfill marker is missing and the old raw identity has no receipt.
func TestIssue72BackfillMustNotReportLostEvidenceAsZero(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	deleteUsageRawAndIndex(t, db, event) // Simulate pre-receipt retention.
	if err := db.Update(func(tx *bolt.Tx) error {
		// This historical row predates raw owner evidence as well as receipts.
		if err := tx.Bucket([]byte(rawOwnerBucket)).Delete([]byte(usageKey(event.Timestamp, event.RecordID))); err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketUsageAggHourly)).Delete(aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target))
	}); err != nil {
		t.Fatal(err)
	}
	oldWatermarkDone(t, db, time.Time{})
	from := event.Timestamp.Truncate(time.Hour)
	to := from.Add(time.Hour).Add(-time.Nanosecond)
	filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
	before, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false)
	if err != nil || costUnitTotal(before) != 2 {
		t.Fatalf("fixture must contain a $2 quota rollup: units=%+v err=%v", before, err)
	}
	beforeQuota, err := store.SumQuotaByClientRange(event.ClientName, from, to)
	if err != nil || beforeQuota["anthropic:executor"].InputTokens != 1_000_000 {
		t.Fatalf("fixture must contain quota evidence: quota=%+v err=%v", beforeQuota, err)
	}
	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	store = clockedStore(db)
	if err := store.EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	if items, err := store.List(filter); err != nil || len(items) != 0 {
		t.Fatalf("legacy pruned row unexpectedly returned as raw: items=%+v err=%v", items, err)
	}
	if got, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) || len(got) != 0 {
		t.Errorf("lost quota evidence reported as a complete price: units=%+v err=%v", got, err)
	}
	if got, err := store.SumQuotaByClientRange(event.ClientName, from, to); err == nil || len(got) != 0 {
		t.Errorf("lost quota evidence reported as complete quota: quota=%+v err=%v", got, err)
	}
	if added, err := store.RecordOnce(event); added || !errors.Is(err, ErrUsageIdentityUnverified) {
		t.Errorf("previously pruned identity was treated as new: added=%t err=%v", added, err)
	}
	if items, err := store.List(filter); err != nil || len(items) != 0 {
		t.Errorf("retry changed raw history: items=%+v err=%v", items, err)
	}
}

// A sidecar is durable evidence, not proof that the price is zero. This
// exercises old pruning, a late older row, a v1 marker, backfill and restart.
func TestIssue72RejectedEvidenceStillBlocksAffectedPriceAfterRestart(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	moveAggCellToLegacy(t, db, event)
	deleteUsageRawAndIndex(t, db, event)
	if usageReceiptCount(t, db) != 0 {
		t.Fatal("fixture unexpectedly has a receipt")
	}
	late := event
	late.RecordID = fixedLaterRecordID
	late.Timestamp = event.Timestamp.Add(-48 * time.Hour)
	if err := store.Record(late); err != nil {
		t.Fatal(err)
	}
	putAggRaw(t, db, string(legacyAggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)), []byte(corruptCell))
	oldWatermarkDone(t, db, time.Time{})
	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	store = clockedStore(db)
	path := filepath.Join(t.TempDir(), "usage.journal")
	first := event
	first.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaab1"
	first.Timestamp = first.Timestamp.Add(time.Minute)
	mustWriteFile(t, path, journalLine(t, event)+"\n"+journalLine(t, first)+"\n")
	journal, err := NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Replay(); err != nil {
		t.Fatal(err)
	}
	if got := readRejectedSidecar(t, path); len(got) != 2 {
		t.Fatalf("expected both unknown identities in rejected sidecar, got %d", len(got))
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	pathDB := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(pathDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store = clockedStore(db)
	journal, err = NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	if _, err := journal.Replay(); err != nil {
		t.Fatal(err)
	}
	from := event.Timestamp.Truncate(time.Hour)
	to := from.Add(time.Hour).Add(-time.Nanosecond)
	filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
	if items, err := store.List(filter); err != nil || len(items) != 0 {
		t.Fatalf("unknown identities were added to raw history: items=%+v err=%v", items, err)
	}
	if got, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) || len(got) != 0 {
		t.Errorf("rejected identity reported as complete stable-price history: units=%+v err=%v", got, err)
	}
	if got, err := store.SumQuotaByClientRange(event.ClientName, from, to); err == nil || len(got) != 0 {
		t.Errorf("rejected identity reported as complete quota history: quota=%+v err=%v", got, err)
	}
	if got, err := store.Aggregate(filter, "hour", replayCosts); err == nil {
		t.Errorf("rejected identity reported as complete aggregate: %+v", got)
	}
	if added, err := store.RecordOnce(late); added || err != nil {
		t.Errorf("retained raw identity must remain idempotent: added=%t err=%v", added, err)
	}
}

func TestIssue72CurrentFutureUnverifiedIdentityIsNotNew(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{"current", boundScanTime.Add(-10 * time.Minute)},
		{"future", boundScanTime.Add(48 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaab2", tc.at)
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			moveAggCellToLegacy(t, db, event)
			deleteUsageRawAndIndex(t, db, event)
			late := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaab3", boundScanTime.Add(-48*time.Hour))
			if err := store.Record(late); err != nil {
				t.Fatal(err)
			}
			putAggRaw(t, db, string(legacyAggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)), []byte(corruptCell))
			oldWatermarkDone(t, db, time.Time{})
			if err := BackfillQuotaUsageAgg(db); err != nil {
				t.Fatal(err)
			}
			store = clockedStore(db)
			first := event
			first.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaab4"
			first.Model = "advisor"
			for _, candidate := range []usage.Event{event, first} {
				if exists, err := store.RecordExists(candidate); exists || !errors.Is(err, ErrUsageIdentityUnverified) {
					t.Errorf("unknown identity passed pre-journal lookup: exists=%t err=%v", exists, err)
				}
				if added, err := store.RecordOnce(candidate); added || !errors.Is(err, ErrUsageIdentityUnverified) {
					t.Errorf("unknown identity treated as new: added=%t err=%v", added, err)
				}
			}
			from := tc.at.Truncate(time.Hour)
			to := from.Add(time.Hour).Add(-time.Nanosecond)
			items, err := store.List(usage.Filter{ClientName: event.ClientName, From: &from, To: &to})
			if err != nil || len(items) != 0 {
				t.Errorf("unknown identities changed raw history: items=%+v err=%v", items, err)
			}
		})
	}
}

func TestIssue72CorruptQuotaCellDoesNotReturnPartialCost(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	alice := fixedPlainEvent()
	bad := alice
	bad.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaab5"
	bad.Model = "advisor"
	bob := alice
	bob.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaab6"
	bob.ClientName = "bob"
	for _, event := range []usage.Event{alice, bad, bob} {
		if err := store.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	putQuotaAggRaw(t, db, string(quotaAggKey(bad.Timestamp, bad.ClientName, bad.Target, bad.Model)), []byte(corruptCell))
	from := alice.Timestamp.Truncate(time.Hour)
	to := from.Add(time.Hour).Add(-time.Nanosecond)
	filter := usage.Filter{ClientName: alice.ClientName, From: &from, To: &to}
	if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
		t.Errorf("corrupt quota cell produced a partial stable-price total: units=%+v err=%v", units, err)
	}
	if totals, err := store.SumQuotaByClientRange(alice.ClientName, from, to); !errors.Is(err, ErrUsageHistoryIncomplete) || len(totals) != 0 {
		t.Errorf("corrupt quota cell produced partial quota: totals=%+v err=%v", totals, err)
	}
	filter.ClientName = bob.ClientName
	if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil || costUnitTotal(units) != 2 {
		t.Errorf("unrelated client stable price was blocked: units=%+v err=%v", units, err)
	}
	if totals, err := store.SumQuotaByClientRange(bob.ClientName, from, to); err != nil || totals["anthropic:executor"].InputTokens != 1_000_000 {
		t.Errorf("unrelated client quota was blocked: totals=%+v err=%v", totals, err)
	}
	outsideFrom, outsideTo := from.Add(24*time.Hour), to.Add(24*time.Hour)
	filter.ClientName, filter.From, filter.To = alice.ClientName, &outsideFrom, &outsideTo
	if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil || len(units) != 0 {
		t.Errorf("unrelated time range was blocked: units=%+v err=%v", units, err)
	}
}
