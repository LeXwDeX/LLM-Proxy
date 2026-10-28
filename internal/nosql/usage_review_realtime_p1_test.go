package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

type noEvidenceRecorder struct{ store *UsageStore }

func (r noEvidenceRecorder) Record(event usage.Event) error { return r.store.Record(event) }
func (r noEvidenceRecorder) RecordExists(event usage.Event) (bool, error) {
	return r.store.RecordExists(event)
}

type failingEvidenceRecorder struct{ noEvidenceRecorder }

func (r failingEvidenceRecorder) RegisterUnresolvedUsage(usage.Event, []byte) error {
	return errors.New("injected registration failure")
}

func TestIssue72RealtimeCommittedIdentityCannotHideLostQuota(t *testing.T) {
	for _, entry := range []string{"journal_exact", "journal_conflict", "replay_conflict", "store_exact", "store_conflict", "batch_exact", "batch_conflict"} {
		t.Run(entry, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
			}); err != nil {
				t.Fatal(err)
			}
			candidate := event
			if entry != "journal_exact" && entry != "store_exact" && entry != "batch_exact" {
				candidate.InputTokens++
			}
			path := filepath.Join(t.TempDir(), "usage.journal")
			journal, err := NewUsageJournal(store, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = journal.Close() })
			switch entry {
			case "journal_exact", "journal_conflict":
				added, err := journal.RecordOnce(candidate)
				if added || (entry == "journal_exact" && !errors.Is(err, ErrUsageHistoryIncomplete)) || (entry == "journal_conflict" && !errors.Is(err, ErrUsageRecordConflict)) {
					t.Fatalf("journal retry: added=%v err=%v", added, err)
				}
			case "replay_conflict":
				mustWriteFile(t, path, journalLine(t, candidate)+"\n")
				if _, err := journal.Replay(); err != nil {
					t.Fatal(err)
				}
			case "store_conflict":
				added, err := store.RecordOnce(candidate)
				if added || !errors.Is(err, ErrUsageRecordConflict) {
					t.Fatalf("store conflict: %v %v", added, err)
				}
			case "store_exact":
				added, err := store.RecordOnce(candidate)
				if added || !errors.Is(err, ErrUsageHistoryIncomplete) {
					t.Fatalf("store identity retry: %v %v", added, err)
				}
			case "batch_conflict":
				if err := store.RecordBatch([]usage.Event{candidate}); !errors.Is(err, ErrUsageRecordConflict) {
					t.Fatalf("batch conflict: %v", err)
				}
			case "batch_exact":
				if err := store.RecordBatch([]usage.Event{candidate}); !errors.Is(err, ErrUsageHistoryIncomplete) {
					t.Fatalf("batch identity retry: %v", err)
				}
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			if units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("identity hid lost quota: %v %v", units, err)
			}
			pathDB := db.Path()
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = OpenDB(pathDB)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if _, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("restart lost accounting block: %v", err)
			}
		})
	}
}

func TestIssue72RealtimeEvidenceFailurePreservesJournal(t *testing.T) {
	for _, entry := range []string{"missing_registrar_retry", "failed_registrar_retry", "missing_registrar_replay", "failed_registrar_replay"} {
		t.Run(entry, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			conflict := event
			conflict.InputTokens++
			var primary usage.Recorder = noEvidenceRecorder{store: store}
			if strings.HasPrefix(entry, "failed") {
				primary = failingEvidenceRecorder{noEvidenceRecorder{store: store}}
			}
			path := filepath.Join(t.TempDir(), "usage.journal")
			journal, err := NewUsageJournal(primary, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			line := journalLine(t, conflict)
			if strings.HasSuffix(entry, "replay") {
				mustWriteFile(t, path, line+"\n")
				if _, err := journal.Replay(); err == nil {
					t.Fatal("missing proof did not stop replay")
				}
			} else {
				if added, err := journal.RecordOnce(conflict); added || err == nil {
					t.Fatalf("retry: %v %v", added, err)
				}
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, []byte(line+"\n")) {
				t.Fatalf("conflict not retained: %q %v", data, err)
			}
		})
	}
}

func TestIssue72HealthyRealtimeIdentityKeepsCompleteAccounting(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := journal.RecordOnce(event); added || err != nil {
		t.Fatalf("exact retry: %v %v", added, err)
	}
	conflict := event
	conflict.InputTokens++
	if added, err := journal.RecordOnce(conflict); added || !errors.Is(err, ErrUsageRecordConflict) {
		t.Fatalf("same-client conflict: %v %v", added, err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	if units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); err != nil || costUnitTotal(units) != 2 {
		t.Fatalf("verified identity blocked or charged twice: %v %v", units, err)
	}
	bob := event
	bob.ClientName = "bob"
	if added, err := journal.RecordOnce(bob); added || !errors.Is(err, ErrUsageIdentityUnverified) {
		t.Fatalf("different client conflict guessed: %v %v", added, err)
	}
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "bob", From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("different client evidence was lost: %v", err)
	}
}

func TestIssue72LegacyFreshAttestationContinuesSameCell(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey))
	}); err != nil {
		t.Fatal(err)
	}
	first := fixedPlainEvent()
	first.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa501"
	second := first
	second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa502"
	second.Timestamp = first.Timestamp.Add(time.Minute)
	if err := store.Record(first); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(second)
	if err := store.RegisterUnresolvedUsage(second, original); err != nil {
		t.Fatal(err)
	}
	if added, err := store.RecordOnce(second); !added || err != nil {
		t.Fatalf("second commit: %v %v", added, err)
	}
	from, to := first.Timestamp.Truncate(time.Hour), first.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: first.ClientName, From: &from, To: &to}
	if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil || costUnitTotal(units) != 4 {
		t.Fatalf("second new event remained blocked: %v %v", units, err)
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
	if err := store.RegisterUnresolvedUsage(second, original); err != nil {
		t.Fatal(err)
	}
	if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil || costUnitTotal(units) != 4 {
		t.Fatalf("restart lost continuation proof: %v %v", units, err)
	}
	third := first
	third.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa511"
	third.Timestamp = first.Timestamp.Add(2 * time.Minute)
	raw, _ := json.Marshal(third)
	if err := store.RegisterUnresolvedUsage(third, raw); err != nil {
		t.Fatal(err)
	}
	if added, err := store.RecordOnce(third); !added || err != nil {
		t.Fatalf("post-restart continuation: %v %v", added, err)
	}
	if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil || costUnitTotal(units) != 6 {
		t.Fatalf("post-restart scope chain: %v %v", units, err)
	}
}

func TestIssue72LegacyContinuationRejectsUnprovenScope(t *testing.T) {
	for _, scenario := range []string{"other_client_history", "corrupt_cell", "missing_anchor", "occupied_different_cell", "unattested_row"} {
		t.Run(scenario, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			first := fixedPlainEvent()
			first.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa503"
			if scenario == "other_client_history" {
				bob := first
				bob.ClientName = "bob"
				bob.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa504"
				if err := store.Record(bob); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey))
			}); err != nil {
				t.Fatal(err)
			}
			if err := store.Record(first); err != nil {
				t.Fatal(err)
			}
			if scenario == "corrupt_cell" {
				if err := db.Update(func(tx *bolt.Tx) error {
					b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
					key := quotaAggKey(first.Timestamp, first.ClientName, first.Target, first.Model)
					var cell AggCell
					if err := json.Unmarshal(b.Get(key), &cell); err != nil {
						return err
					}
					cell.InputTokens++
					data, err := json.Marshal(cell)
					if err != nil {
						return err
					}
					return b.Put(key, data)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "missing_anchor" {
				if err := db.Update(func(tx *bolt.Tx) error {
					return tx.Bucket([]byte(freshAttestationBucket)).Delete([]byte(usageKey(first.Timestamp, first.RecordID)))
				}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "unattested_row" {
				rogue := first
				rogue.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa512"
				rogue.Timestamp = first.Timestamp.Add(30 * time.Second)
				rows, keys, err := normalizeUsageBundle(rogue)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Update(func(tx *bolt.Tx) error { return writeUsageRowTx(tx, rows[0], keys[0]) }); err != nil {
					t.Fatal(err)
				}
			}
			second := first
			second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa505"
			second.Timestamp = first.Timestamp.Add(time.Minute)
			if scenario == "other_client_history" {
				second.ClientName = "bob"
			}
			if scenario == "occupied_different_cell" {
				second.Model = "advisor"
				putQuotaAggRaw(t, db, string(quotaAggKey(second.Timestamp, second.ClientName, second.Target, second.Model)), []byte(`{"requests":1,"input_tokens":7}`))
			}
			raw, _ := json.Marshal(second)
			if err := store.RegisterUnresolvedUsage(second, raw); err != nil {
				t.Fatal(err)
			}
			if added, err := store.RecordOnce(second); !added || err != nil {
				t.Fatalf("second commit: %v %v", added, err)
			}
			from, to := second.Timestamp.Truncate(time.Hour), second.Timestamp.Add(time.Hour)
			if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: second.ClientName, From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("unproven scope accepted: %v", err)
			}
		})
	}
}

func TestIssue72LegacyContinuationCanUseProvenEmptyNewCells(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey))
	}); err != nil {
		t.Fatal(err)
	}
	first := fixedPlainEvent()
	first.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa513"
	if err := store.Record(first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa514"
	second.Timestamp = first.Timestamp.Add(time.Minute)
	second.Model = "advisor"
	third := second
	third.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa515"
	third.Timestamp = first.Timestamp.Add(2 * time.Minute)
	third.Target = "different-target"
	for _, event := range []usage.Event{second, third} {
		raw, _ := json.Marshal(event)
		if err := store.RegisterUnresolvedUsage(event, raw); err != nil {
			t.Fatal(err)
		}
		if added, err := store.RecordOnce(event); !added || err != nil {
			t.Fatalf("new cell: added=%v err=%v", added, err)
		}
	}
	from, to := first.Timestamp.Truncate(time.Hour), first.Timestamp.Add(time.Hour)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: first.ClientName, From: &from, To: &to}, replayCosts, false)
	if err != nil || costUnitTotal(units) != 12 {
		t.Fatalf("new proven cells did not retain stable cost: %v %v", units, err)
	}
}

func TestIssue72LegacyContinuationConcurrentWriters(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey))
	}); err != nil {
		t.Fatal(err)
	}
	first := fixedPlainEvent()
	first.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa506"
	if err := store.Record(first); err != nil {
		t.Fatal(err)
	}
	events := make([]usage.Event, 2)
	for i := range events {
		events[i] = first
		events[i].RecordID = fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012x", 0xa507+i)
		events[i].Timestamp = first.Timestamp.Add(time.Duration(i+1) * time.Minute)
		raw, _ := json.Marshal(events[i])
		if err := store.RegisterUnresolvedUsage(events[i], raw); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, event := range events {
		wg.Add(1)
		go func(e usage.Event) {
			defer wg.Done()
			if added, err := store.RecordOnce(e); !added || err != nil {
				t.Errorf("concurrent commit: %v %v", added, err)
			}
		}(event)
	}
	wg.Wait()
	from, to := first.Timestamp.Truncate(time.Hour), first.Timestamp.Add(time.Hour)
	if units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: first.ClientName, From: &from, To: &to}, replayCosts, false); err != nil || costUnitTotal(units) != 6 {
		t.Fatalf("concurrent continuation: %v %v", units, err)
	}
}

func TestIssue72LegacyContinuationBundleAndBatch(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey))
	}); err != nil {
		t.Fatal(err)
	}
	first := fixedIdentityBundle()
	first.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa508"
	if err := store.Record(first); err != nil {
		t.Fatal(err)
	}
	second := usage.CloneEvent(first)
	second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa509"
	second.Timestamp = first.Timestamp.Add(time.Minute)
	third := usage.CloneEvent(first)
	third.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa510"
	third.Timestamp = first.Timestamp.Add(2 * time.Minute)
	for _, event := range []usage.Event{second, third} {
		raw, _ := json.Marshal(event)
		if err := store.RegisterUnresolvedUsage(event, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordBatch([]usage.Event{second, third}); err != nil {
		t.Fatal(err)
	}
	from, to := first.Timestamp.Truncate(time.Hour), first.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: first.ClientName, From: &from, To: &to}
	units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false)
	if err != nil || costUnitTotal(units) != 36 {
		t.Fatalf("batch bundle continuation: %v %v", units, err)
	}
	rows, err := store.List(usage.Filter{ClientName: first.ClientName})
	if err != nil || len(rows) != 6 {
		t.Fatalf("bundle rows: %d %v", len(rows), err)
	}
}

func TestIssue72UnknownSidecarOwnerCannotBeExcluded(t *testing.T) {
	for _, tc := range []struct {
		owner              string
		committed, blocked bool
	}{
		{"unknown", false, true}, {"Unknown", false, true}, {"UNKNOWN", false, true}, {" UNKNOWN ", false, true},
		{"unknown", true, true}, {"bob", false, false},
	} {
		t.Run(fmt.Sprintf("%s_committed_%t", tc.owner, tc.committed), func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			event.ClientName = tc.owner
			event.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa507"
			if tc.committed {
				if err := store.Record(event); err != nil {
					t.Fatal(err)
				}
			}
			line := journalLine(t, event)
			entry, _ := json.Marshal(rejectedUsageLine{Reason: usageRejectIdentityUnverified, Line: []byte(line)})
			path := filepath.Join(t.TempDir(), "usage.journal")
			mustWriteFile(t, path+usageJournalRejectedSuffix, string(entry)+"\n")
			journal, err := NewUsageJournal(store, path, nil)
			if err != nil {
				t.Fatal(err)
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
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			filter := usage.Filter{ClientName: "alice", From: &from, To: &to}
			if _, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); tc.blocked && (!errors.Is(err, ErrUsageHistoryIncomplete) || !errors.Is(err, ErrUsageAggregateSourceMissing)) {
				t.Fatalf("unknown owner excluded: %v", err)
			} else if !tc.blocked && err != nil {
				t.Fatalf("known other owner blocked: %v", err)
			}
			from, to = from.Add(48*time.Hour), to.Add(48*time.Hour)
			if _, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil {
				t.Fatalf("unrelated time blocked: %v", err)
			}
		})
	}
}

func TestIssue72PreviouslyResolvedUnknownOwnerStillBlocksRelevantScope(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	event.ClientName = "UnKnOwN"
	raw, _ := json.Marshal(event)
	if err := db.Update(func(tx *bolt.Tx) error {
		return putUnresolvedEvidence(tx, unresolvedUsage{Event: event, Original: raw, Resolved: true})
	}); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: "alice", From: &from, To: &to}
	if _, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("old resolved unknown owner excluded: %v", err)
	}
	from, to = from.Add(48*time.Hour), to.Add(48*time.Hour)
	if _, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil {
		t.Fatalf("known unrelated time blocked: %v", err)
	}
}
