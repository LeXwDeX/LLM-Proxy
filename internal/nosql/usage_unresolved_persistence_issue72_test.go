package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72UnresolvedRegistrationFailureKeepsJournal(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	putAggRaw(t, db, boundScanTime.Add(48*time.Hour).Format(hourLayout)+"|claude|alice|executor|anthropic", []byte(corruptCell))
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Put([]byte("usage_unresolved_events_initialized_v1"), []byte("invalid"))
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "usage.journal")
	event := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaac21", boundScanTime)
	mustWriteFile(t, path, journalLine(t, event)+"\n")
	journal, err := NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Replay(); err == nil {
		t.Fatal("registration failure was hidden")
	}
	if got := journalContent(t, path); got != journalLine(t, event) {
		t.Fatalf("registration failure removed original journal line: %q", got)
	}
	if err := journal.DrainClient(event.ClientName); err == nil {
		t.Fatal("registration failure allowed drain")
	}
}

func TestIssue72BatchPreservesUncommittedEventsAndResolvesVerifiedRetry(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	putAggRaw(t, db, boundScanTime.Add(48*time.Hour).Format(hourLayout)+"|claude|alice|executor|anthropic", []byte(corruptCell))
	blocked := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaac22", boundScanTime)
	fresh := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaac23", boundScanTime.Add(72*time.Hour))
	fresh.ClientName = "bob"
	if err := store.RecordBatch([]usage.Event{fresh, blocked}); !errors.Is(err, ErrUsageIdentityUnverified) {
		t.Fatalf("batch rejection: %v", err)
	}
	if rawUsageRowCount(t, db) != 0 {
		t.Fatal("rejected batch committed a row")
	}
	query := func(event usage.Event) error {
		from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
		_, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false)
		return err
	}
	if err := query(fresh); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("rolled-back fresh event missing from evidence: %v", err)
	}
	if err := query(blocked); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("unverified event missing from evidence: %v", err)
	}
	if added, err := store.RecordOnce(fresh); !added || err != nil {
		t.Fatalf("verified retry: added=%v err=%v", added, err)
	}
	if err := query(fresh); err != nil {
		t.Fatalf("verified retry did not resolve evidence: %v", err)
	}
	if err := query(blocked); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("unrelated unresolved event was cleared: %v", err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(unresolvedBucket)).ForEach(func(_, data []byte) error {
			var item unresolvedUsage
			if err := json.Unmarshal(data, &item); err != nil {
				return err
			}
			if len(item.Original) == 0 {
				t.Error("original event was discarded")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIssue72CommittedDigestConflictDoesNotBlockAccounting(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	conflict := event
	conflict.InputTokens++
	if _, err := store.RecordOnce(conflict); !errors.Is(err, ErrUsageRecordConflict) {
		t.Fatalf("expected digest conflict, got %v", err)
	}
	if err := store.RecordBatch([]usage.Event{conflict}); !errors.Is(err, ErrUsageRecordConflict) {
		t.Fatalf("expected batch digest conflict, got %v", err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Truncate(time.Hour).Add(time.Hour)
	if units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); err != nil || costUnitTotal(units) != 2 {
		t.Fatalf("confirmed conflict blocked committed cost: units=%v err=%v", units, err)
	}
}

func TestIssue72JournalRegistersOriginalBytesIdempotently(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	putAggRaw(t, db, boundScanTime.Add(48*time.Hour).Format(hourLayout)+"|claude|alice|executor|anthropic", []byte(corruptCell))
	event := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaac24", boundScanTime)
	line := "  " + journalLine(t, event) + "  "
	path := filepath.Join(t.TempDir(), "usage.journal")
	journal, err := NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		mustWriteFile(t, path, line+"\n")
		if _, err := journal.Replay(); err != nil {
			t.Fatal(err)
		}
	}
	count, originals := 0, 0
	if err := db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(unresolvedBucket)).ForEach(func(_, raw []byte) error {
			count++
			var item unresolvedUsage
			if err := json.Unmarshal(raw, &item); err != nil {
				return err
			}
			if string(item.Original) == line {
				originals++
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if originals != 1 || count != 2 {
		t.Fatalf("replay duplicated evidence or lost exact bytes: count=%d exact=%d", count, originals)
	}
}

func TestIssue72PartialBundleConflictRemainsUnconfirmed(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedIdentityBundle()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	deleteUsageRawAndIndex(t, db, usage.ExpandEvent(event)[1])
	conflict := event
	conflict.InputTokens++
	if exists, err := store.RecordExists(conflict); exists || !errors.Is(err, ErrUsageBundlePartial) {
		t.Fatalf("partial conflict lookup: exists=%v err=%v", exists, err)
	}
	if added, err := store.RecordOnce(conflict); added || !errors.Is(err, ErrUsageBundlePartial) {
		t.Fatalf("partial conflict write: added=%v err=%v", added, err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Truncate(time.Hour).Add(time.Hour)
	if units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("partial conflict did not block accounting: units=%v err=%v", units, err)
	}
}

func TestIssue72InvalidUnknownTimeIsActivationBlocker(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	event.Timestamp = time.Time{}
	if _, err := store.RecordOnce(event); !errors.Is(err, usage.ErrExplicitRecordMissingTimestamp) {
		t.Fatalf("invalid event rejected incorrectly: %v", err)
	}
	from, to := boundScanTime, boundScanTime.Add(time.Hour)
	_, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false)
	if !errors.Is(err, ErrUsageAggregateSourceMissing) || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("unknown time must report activation blocker: %v", err)
	}
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "unrelated", From: &from, To: &to}, replayCosts, false); err != nil {
		t.Fatalf("known different client was blocked: %v", err)
	}
}

func TestIssue72CorruptJournalEvidenceBlocksActivation(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	path := filepath.Join(t.TempDir(), "usage.journal")
	line := "{broken"
	mustWriteFile(t, path, line+"\n")
	journal, err := NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Replay(); err != nil {
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
	from, to := boundScanTime, boundScanTime.Add(time.Hour)
	_, err = clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, replayCosts, false)
	if !errors.Is(err, ErrUsageAggregateSourceMissing) || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("unattributed journal evidence did not block activation: %v", err)
	}
}

func TestIssue72ExistingRejectedSidecarIsRegisteredOnOpen(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	path := filepath.Join(t.TempDir(), "usage.journal")
	line := journalLine(t, event)
	entry, err := json.Marshal(rejectedUsageLine{Reason: usageRejectIdentityUnverified, Client: event.ClientName, RecordID: event.RecordID, Line: []byte(line)})
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, path+usageJournalRejectedSuffix, string(entry)+"\n")
	journal, err := NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Truncate(time.Hour).Add(time.Hour)
	for _, client := range []string{event.ClientName, "bob"} {
		_, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, replayCosts, false)
		if client == event.ClientName && !errors.Is(err, ErrUsageHistoryIncomplete) {
			t.Errorf("historical rejection invisible: %v", err)
		}
		if client == "bob" && err != nil {
			t.Errorf("unrelated client blocked: %v", err)
		}
	}
}
