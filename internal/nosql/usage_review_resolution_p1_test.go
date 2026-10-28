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
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func resolutionCost(t *testing.T, store *UsageStore, event usage.Event) error {
	t.Helper()
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
	_, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false)
	return err
}

func TestIssue72ReregisterResolvedEvidenceRechecksAccounting(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(event)
	if err := store.RegisterUnresolvedUsage(event, original); err != nil {
		t.Fatal(err)
	}
	entries, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(entries) != 1 || !entries[0].Resolved {
		t.Fatalf("fixture: %+v %v", entries, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterUnresolvedUsage(event, original); err != nil {
		t.Fatal(err)
	}
	if err := resolutionCost(t, store, event); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("reregistration retained stale resolved flag: %v", err)
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
	if err := resolutionCost(t, clockedStore(db), event); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("restart lost reopened evidence: %v", err)
	}
}

func TestIssue72ReregisterEmptyOriginalRechecksAccounting(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterUnresolvedUsage(event, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterUnresolvedUsage(event, nil); err != nil {
		t.Fatal(err)
	}
	if err := resolutionCost(t, store, event); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("empty original hid stale proof: %v", err)
	}
}

func TestIssue72InternalReregistrationReopensStaleEvidence(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	original, _ := json.Marshal(event)
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterUnresolvedUsage(event, original); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)); err != nil {
			return err
		}
		return putUnresolvedUsage(tx, event, original)
	}); err != nil {
		t.Fatal(err)
	}
	if err := resolutionCost(t, store, event); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("internal registration kept stale proof: %v", err)
	}
}

func TestIssue72IdentityRetryRechecksPreviouslyResolvedEvidence(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	original, _ := json.Marshal(event)
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterUnresolvedUsage(event, original); err != nil {
		t.Fatal(err)
	}
	if added, err := store.RecordOnce(event); added || err != nil {
		t.Fatalf("healthy retry: added=%v err=%v", added, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
	}); err != nil {
		t.Fatal(err)
	}
	if added, err := store.RecordOnce(event); added || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("identity retry: added=%v err=%v", added, err)
	}
	if err := resolutionCost(t, store, event); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("retry retained stale proof: %v", err)
	}
}

func TestIssue72SidecarIdentityCannotHideMissingAccounting(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
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
			pathDB := db.Path()
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := OpenDB(pathDB)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			store = clockedStore(db)
			candidate := event
			if conflict {
				candidate.InputTokens++
			}
			line := journalLine(t, candidate)
			entry, _ := json.Marshal(rejectedUsageLine{Reason: usageRejectDigestConflict, Client: candidate.ClientName, RecordID: candidate.RecordID, Line: []byte(line)})
			path := filepath.Join(t.TempDir(), "usage.journal")
			mustWriteFile(t, path+usageJournalRejectedSuffix, string(entry)+"\n")
			journal, err := NewUsageJournal(store, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = journal.Close() })
			if err := resolutionCost(t, store, event); !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("sidecar identity hid missing quota: %v", err)
			}
		})
	}
}

func TestIssue72LegacyFreshCommitResolvesOnlyItsOwnEvidence(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	original, _ := json.Marshal(event)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey))
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterUnresolvedUsage(event, original); err != nil {
		t.Fatal(err)
	}
	if added, err := store.RecordOnce(event); !added || err != nil {
		t.Fatalf("fresh record: %v %v", added, err)
	}
	if err := resolutionCost(t, store, event); err != nil {
		t.Fatalf("fresh atomic record stayed unresolved on old db: %v", err)
	}
}

func TestIssue72LegacyFreshLateRegistrationChecksAttestationAfterRestart(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	original, _ := json.Marshal(event)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey))
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(event); err != nil {
		t.Fatal(err)
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
	if err := store.RegisterUnresolvedUsage(event, original); err != nil {
		t.Fatal(err)
	}
	if err := resolutionCost(t, store, event); err != nil {
		t.Fatalf("fresh committed event not proven after restart: %v", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterUnresolvedUsage(event, original); err != nil {
		t.Fatal(err)
	}
	if err := resolutionCost(t, store, event); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("stale fresh attestation hid missing quota: %v", err)
	}
}

func TestIssue72LegacyFreshCommitIgnoresOtherOwnersVerifiedReceipt(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	bob := fixedPlainEvent()
	bob.ClientName = "bob"
	bob.Timestamp = bob.Timestamp.Add(-10 * time.Minute)
	if err := store.Record(bob); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(bob.Timestamp.Add(2*time.Second), 100); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey))
	}); err != nil {
		t.Fatal(err)
	}
	alice := fixedPlainEvent()
	alice.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa312"
	raw, _ := json.Marshal(alice)
	if err := store.RegisterUnresolvedUsage(alice, raw); err != nil {
		t.Fatal(err)
	}
	if added, err := store.RecordOnce(alice); !added || err != nil {
		t.Fatalf("fresh record: added=%v err=%v", added, err)
	}
	if err := resolutionCost(t, store, alice); err != nil {
		t.Fatalf("other owner receipt blocked fresh legacy event: %v", err)
	}
	second := alice
	second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa313"
	second.Timestamp = alice.Timestamp.Add(time.Minute)
	secondRaw, _ := json.Marshal(second)
	if err := store.RegisterUnresolvedUsage(second, secondRaw); err != nil {
		t.Fatal(err)
	}
	if added, err := store.RecordOnce(second); !added || err != nil {
		t.Fatalf("second fresh event: added=%v err=%v", added, err)
	}
	from, to := alice.Timestamp.Truncate(time.Hour), alice.Timestamp.Add(time.Hour)
	if units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, replayCosts, false); err != nil || costUnitTotal(units) != 4 {
		t.Fatalf("other owner's receipt blocked continued scope: %v %v", units, err)
	}
}

func TestIssue72OtherClientPrunedReceiptDoesNotBlockProvenAccounting(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	alice := fixedPlainEvent()
	bob := alice
	bob.ClientName = "bob"
	bob.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa301"
	bob.Timestamp = alice.Timestamp.Add(-10 * time.Minute)
	if err := store.Record(bob); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(alice); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(bob.Timestamp.Add(2*time.Second), 100); err != nil {
		t.Fatal(err)
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
	original, _ := json.Marshal(alice)
	if err := store.RegisterUnresolvedUsage(alice, original); err != nil {
		t.Fatal(err)
	}
	if err := resolutionCost(t, store, alice); err != nil {
		t.Fatalf("other owner's receipt blocked proof: %v", err)
	}
}

func TestIssue72UnknownLegacyReceiptDoesNotGuessOwner(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	alice := fixedPlainEvent()
	bob := alice
	bob.ClientName = "bob"
	bob.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa305"
	bob.Timestamp = alice.Timestamp.Add(-10 * time.Minute)
	if err := store.Record(bob); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(alice); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(bob.Timestamp.Add(2*time.Second), 100); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(receiptOwnersBucket)).Delete([]byte(usageKey(bob.Timestamp, bob.RecordID)))
	}); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(alice)
	if err := store.RegisterUnresolvedUsage(alice, original); err != nil {
		t.Fatal(err)
	}
	if err := resolutionCost(t, store, alice); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("unattributed receipt was guessed to belong to another client: %v", err)
	}
}

func TestIssue72CorruptReceiptOwnerDoesNotGuessOtherClient(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	alice := fixedPlainEvent()
	bob := alice
	bob.ClientName = "bob"
	bob.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa307"
	bob.Timestamp = alice.Timestamp.Add(-10 * time.Minute)
	if err := store.Record(bob); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(alice); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(bob.Timestamp.Add(2*time.Second), 100); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(receiptOwnersBucket))
		key := []byte(usageKey(bob.Timestamp, bob.RecordID))
		var owner receiptOwner
		if err := json.Unmarshal(b.Get(key), &owner); err != nil {
			return err
		}
		owner.ClientHash = receiptClientHash("carol")
		data, err := json.Marshal(owner)
		if err != nil {
			return err
		}
		return b.Put(key, data)
	}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(alice)
	if err := store.RegisterUnresolvedUsage(alice, raw); err != nil {
		t.Fatal(err)
	}
	if err := resolutionCost(t, store, alice); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("corrupt owner metadata trusted: %v", err)
	}
}

func TestIssue72LegacyFreshCommitDoesNotHideSameClientHistory(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	old := fixedPlainEvent()
	old.Timestamp = old.Timestamp.Add(-10 * time.Minute)
	if err := store.Record(old); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(old.Timestamp.Add(2*time.Second), 100); err != nil {
		t.Fatal(err)
	}
	// Model an older receipt with known owner but no cell provenance.
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(receiptOwnersBucket))
		key := []byte(usageKey(old.Timestamp, old.RecordID))
		var owner receiptOwner
		if err := json.Unmarshal(b.Get(key), &owner); err != nil {
			return err
		}
		owner.UsageKey = nil
		owner.QuotaKey = nil
		owner.CellBinding = ""
		data, err := json.Marshal(owner)
		if err != nil {
			return err
		}
		return b.Put(key, data)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey))
	}); err != nil {
		t.Fatal(err)
	}
	fresh := fixedPlainEvent()
	fresh.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa306"
	raw, _ := json.Marshal(fresh)
	if err := store.RegisterUnresolvedUsage(fresh, raw); err != nil {
		t.Fatal(err)
	}
	if added, err := store.RecordOnce(fresh); !added || err != nil {
		t.Fatalf("fresh record: added=%v err=%v", added, err)
	}
	if err := resolutionCost(t, store, fresh); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("same-client pruned history was guessed complete: %v", err)
	}
}

func TestIssue72LegacyFreshCommitDoesNotIgnorePriorSnapshot(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	old := fixedPlainEvent()
	old.Timestamp = old.Timestamp.Add(-10 * time.Minute)
	if err := store.Record(old); err != nil {
		t.Fatal(err)
	}
	if err := BackfillUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	deleteUsageRawAndIndex(t, db, old)
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(BucketUsageAggHourly)).Delete(aggKey(old.Timestamp, old.EndpointType, old.ClientName, old.Model, old.Target)); err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(old.Timestamp, old.ClientName, old.Target, old.Model))
	}); err != nil {
		t.Fatal(err)
	}
	fresh := fixedPlainEvent()
	fresh.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa309"
	raw, _ := json.Marshal(fresh)
	if err := store.RegisterUnresolvedUsage(fresh, raw); err != nil {
		t.Fatal(err)
	}
	if added, err := store.RecordOnce(fresh); !added || err != nil {
		t.Fatalf("fresh record: added=%v err=%v", added, err)
	}
	if err := resolutionCost(t, store, fresh); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("historical snapshot was ignored: %v", err)
	}
}

func TestIssue72BackfillCaseFoldedScopeIsUnknown(t *testing.T) {
	db := testDB(t)
	hour := boundScanTime.Truncate(time.Hour)
	raw := []byte(fmt.Sprintf(`{"timestamp":%q,"client_name":"alice","CLIENT_NAME":"bob","record_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa302","model":"model","target":"target"}`, hour.Format(time.RFC3339)))
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte("legacy-conflict"), raw)
	}); err != nil {
		t.Fatal(err)
	}
	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
		if key, _ := bucket.Cursor().First(); key != nil {
			t.Errorf("ambiguous raw record was converted to an attributed quota cell: %q", key)
		}
		if got := tx.Bucket([]byte(BucketUsageEvents)).Get([]byte("legacy-conflict")); string(got) != string(raw) {
			t.Error("original ambiguous raw was changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	from, to := hour, hour.Add(time.Hour-time.Nanosecond)
	for _, client := range []string{"alice", "bob", "carol"} {
		if _, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
			t.Errorf("conflicting owner guessed for %s: %v", client, err)
		}
	}
	if _, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: "carol", Model: "other-model", From: &from, To: &to}, replayCosts, false); err != nil {
		t.Fatalf("known unrelated model blocked: %v", err)
	}
	if _, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: "carol", Model: "model", From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("known relevant model not blocked: %v", err)
	}
	from, to = from.Add(48*time.Hour), to.Add(48*time.Hour)
	if _, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: "carol", From: &from, To: &to}, replayCosts, false); err != nil {
		t.Fatalf("known disjoint time blocked: %v", err)
	}
}

func TestIssue72BackfillCaseFoldedTimestampKeepsKnownClient(t *testing.T) {
	db := testDB(t)
	hour := boundScanTime.Truncate(time.Hour)
	raw := []byte(fmt.Sprintf(`{"timestamp":%q,"TIMESTAMP":%q,"client_name":"alice","record_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa308"}`, hour.Format(time.RFC3339), hour.Add(time.Hour).Format(time.RFC3339)))
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte("legacy-conflict-time"), raw)
	}); err != nil {
		t.Fatal(err)
	}
	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	from, to := hour.Add(48*time.Hour), hour.Add(49*time.Hour)
	if _, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: "bob", From: &from, To: &to}, replayCosts, false); err != nil {
		t.Fatalf("unrelated owner blocked: %v", err)
	}
	if _, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("unknown time was guessed: %v", err)
	}
}

func TestIssue72JournalCaseFoldedClientIsNotAttributed(t *testing.T) {
	for _, sidecar := range []bool{false, true} {
		t.Run(fmt.Sprint(sidecar), func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			hour := boundScanTime.Truncate(time.Hour)
			raw := fmt.Sprintf(`{"timestamp":%q,"client_name":"alice","CLIENT_NAME":"bob","record_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa310","model":"model","target":"target"}`, hour.Format(time.RFC3339))
			path := filepath.Join(t.TempDir(), "usage.journal")
			if sidecar {
				entry, _ := json.Marshal(rejectedUsageLine{Reason: usageRejectInvalidBundle, Line: []byte(raw)})
				mustWriteFile(t, path+usageJournalRejectedSuffix, string(entry)+"\n")
			} else {
				mustWriteFile(t, path, raw+"\n")
			}
			journal, err := NewUsageJournal(store, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = journal.Close() })
			if _, err := journal.Replay(); err != nil {
				t.Fatal(err)
			}
			if rows, err := store.List(usage.Filter{}); err != nil || len(rows) != 0 {
				t.Fatalf("ambiguous journal event entered ledger: %v %v", rows, err)
			}
			from, to := hour, hour.Add(time.Hour-time.Nanosecond)
			for _, client := range []string{"alice", "bob", "carol"} {
				if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
					t.Errorf("unknown owner guessed for %s: %v", client, err)
				}
			}
			from, to = hour.Add(24*time.Hour), hour.Add(25*time.Hour)
			if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "carol", From: &from, To: &to}, replayCosts, false); err != nil {
				t.Fatalf("known disjoint hour blocked: %v", err)
			}
		})
	}
}

func TestIssue72InvalidUTF8JournalBytesRemainUnattributed(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSuffix(journalLine(t, event), "}") + ",\"future\":\"\xff\xfe\"}"
	path := filepath.Join(t.TempDir(), "usage.journal")
	original := []byte(line + "\n")
	mustWriteFile(t, path, string(original))
	journal, err := NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	fault := errors.New("quarantine sync failure")
	journal.syncFile = func(f *os.File) error {
		if f.Name() == path+".corrupt" {
			return fault
		}
		return f.Sync()
	}
	if _, err := journal.Replay(); !errors.Is(err, fault) {
		t.Fatalf("sync failure lost: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("failed quarantine changed original: %q %v", got, err)
	}
	journal.syncFile = nil
	if _, err := journal.Replay(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path + ".corrupt"); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("invalid UTF-8 not preserved byte-for-byte: %q %v", got, err)
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
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	if _, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: "bob", From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageAggregateSourceMissing) {
		t.Fatalf("unknown original scope was guessed: %v", err)
	}
}

func TestIssue72BackfillNestedCaseFoldedModelIsNotAttributed(t *testing.T) {
	db := testDB(t)
	hour := boundScanTime.Truncate(time.Hour)
	raw := []byte(fmt.Sprintf(`{"timestamp":%q,"client_name":"alice","record_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa311","model":"root","target":"target","components":[{"model":"advisor","MODEL":"other","iteration_index":1,"input_tokens":100}]}`, hour.Format(time.RFC3339)))
	if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte("legacy-nested"), raw) }); err != nil {
		t.Fatal(err)
	}
	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if key, _ := tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Cursor().First(); key != nil {
			t.Errorf("ambiguous component was attributed: %q", key)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	from, to := hour, hour.Add(time.Hour-time.Nanosecond)
	if _, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", Model: "advisor", From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("ambiguous component did not block affected owner: %v", err)
	}
	if _, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: "bob", From: &from, To: &to}, replayCosts, false); err != nil {
		t.Fatalf("unrelated owner blocked: %v", err)
	}
}

func TestIssue72RecheckKeepsProvenSameClientConflict(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	conflict := event
	conflict.InputTokens++
	raw, _ := json.Marshal(conflict)
	if err := store.RegisterUnresolvedUsage(conflict, raw); err != nil {
		t.Fatal(err)
	}
	entries, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(entries) != 1 || !entries[0].Resolved {
		t.Fatalf("fixture: %+v %v", entries, err)
	}
	req := UsageRecheckRequest{EvidenceID: entries[0].ID, Version: entries[0].Version, OperationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa303", Actor: "admin"}
	result, err := store.RecheckUsage(req)
	if err != nil || result.AccountingRestored {
		t.Fatalf("recheck: %+v %v", result, err)
	}
	after, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(after) != 1 || !after[0].Resolved {
		t.Fatalf("proven conflict reopened: %+v %v", after, err)
	}
	if err := resolutionCost(t, store, event); err != nil {
		t.Fatalf("proven conflict blocked: %v", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
	}); err != nil {
		t.Fatal(err)
	}
	req.OperationID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa304"
	if _, err := store.RecheckUsage(req); err != nil {
		t.Fatal(err)
	}
	if err := resolutionCost(t, store, event); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("missing aggregate did not reopen conflict: %v", err)
	}
}
