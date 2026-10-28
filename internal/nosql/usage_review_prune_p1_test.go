package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72ExactJournalRetryAfterNormalPruning(t *testing.T) {
	for _, mode := range []string{"receipt_only", "partial_same_hour", "receipt_missing_quota", "partial_missing_quota"} {
		t.Run(mode, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			first := fixedPlainEvent()
			first.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa601"
			second := first
			second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa602"
			second.Timestamp = first.Timestamp.Add(10 * time.Minute)
			if err := store.Record(first); err != nil {
				t.Fatal(err)
			}
			partial := mode == "partial_same_hour" || mode == "partial_missing_quota"
			if partial {
				if err := store.Record(second); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.PruneEventsBefore(first.Timestamp.Add(2*time.Second), 100); err != nil {
				t.Fatal(err)
			}
			event := first
			if partial {
				event = second
			}
			missing := mode == "receipt_missing_quota" || mode == "partial_missing_quota"
			if missing {
				if err := db.Update(func(tx *bolt.Tx) error {
					return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
				}); err != nil {
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
			journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = journal.Close() })
			added, err := journal.RecordOnce(event)
			if added {
				t.Fatal("exact retry was reinserted")
			}
			if missing && !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("missing quota not blocked: %v", err)
			}
			if !missing && err != nil {
				t.Fatalf("normal prune exact retry blocked: %v", err)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
			units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false)
			if missing && (len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete)) {
				t.Fatalf("missing cell reported complete: %v %v", units, err)
			}
			want := 2.0
			if partial {
				want = 4
			}
			if !missing && (err != nil || costUnitTotal(units) != want) {
				t.Fatalf("normal stable cost changed: %v %v", units, err)
			}
		})
	}
}

func TestIssue72OldReceiptWithoutAccountingProvenanceRemainsBlocked(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(event.Timestamp.Add(2*time.Second), 100); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(rollupProofBucket))
		if b == nil {
			return nil
		}
		return tx.DeleteBucket([]byte(rollupProofBucket))
	}); err != nil {
		t.Fatal(err)
	}
	journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := journal.RecordOnce(event); added || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("unproven receipt-only retry allowed: %v %v", added, err)
	}
}

func TestIssue72PartiallyPrunedBundleRetryKeepsStableCost(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	bundle := fixedIdentityBundle()
	if err := store.Record(bundle); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		key := []byte(usageKey(bundle.Timestamp, bundle.RecordID))
		bucket := tx.Bucket([]byte(BucketUsageEvents))
		data := append([]byte(nil), bucket.Get(key)...)
		var event usage.Event
		if err := json.Unmarshal(data, &event); err != nil {
			return err
		}
		if err := capturePruneRollupProof(tx, event, key); err != nil {
			return err
		}
		if err := removeClientEventIndex(tx, key, data); err != nil {
			return err
		}
		digest, err := usage.RetryEquivalentDigest(event)
		if err != nil {
			return err
		}
		if err := writeUsageReceipt(tx, key, digest); err != nil {
			return err
		}
		if err := writeReceiptOwner(tx, key, digest, event, data); err != nil {
			return err
		}
		if err := bucket.Delete(key); err != nil {
			return err
		}
		return bumpRawRevision(tx)
	}); err != nil {
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
	journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := journal.RecordOnce(bundle); added || err != nil {
		t.Fatalf("partial bundle exact retry: %v %v", added, err)
	}
	from, to := bundle.Timestamp.Truncate(time.Hour), bundle.Timestamp.Add(time.Hour)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: bundle.ClientName, From: &from, To: &to}, replayCosts, false)
	if err != nil || costUnitTotal(units) != 12 {
		t.Fatalf("partial bundle stable cost: %v %v", units, err)
	}
}

func TestIssue72PruneBatchKeepsAllIdentityProofs(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	first := fixedPlainEvent()
	var events []usage.Event
	for i := 0; i < 3; i++ {
		event := first
		event.RecordID = fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012x", 0xa620+i)
		event.Timestamp = first.Timestamp.Add(time.Duration(i) * time.Minute)
		if err := store.Record(event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if n, err := store.PruneEventsBefore(events[2].Timestamp.Add(2*time.Second), 100); err != nil || n != 3 {
		t.Fatalf("prune batch: %d %v", n, err)
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
	journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if added, err := journal.RecordOnce(event); added || err != nil {
			t.Fatalf("pruned identity retry: %v %v", added, err)
		}
	}
	from, to := first.Timestamp.Truncate(time.Hour), first.Timestamp.Add(time.Hour)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: first.ClientName, From: &from, To: &to}, replayCosts, false)
	if err != nil || costUnitTotal(units) != 6 {
		t.Fatalf("pruned batch cost changed: %v %v", units, err)
	}
}

func TestIssue72PrunedRetryPreservesEmptyTargetAndModel(t *testing.T) {
	for _, field := range []string{"target", "model"} {
		t.Run(field, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			if field == "target" {
				event.Target = ""
			} else {
				event.Model = ""
			}
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			if _, err := store.PruneEventsBefore(event.Timestamp.Add(2*time.Second), 100); err != nil {
				t.Fatal(err)
			}
			journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
			if err != nil {
				t.Fatal(err)
			}
			if added, err := journal.RecordOnce(event); added || err != nil {
				t.Fatalf("pruned retry: added=%v err=%v", added, err)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false)
			if err != nil || len(units) != 1 || units[0].InputTokens != event.InputTokens {
				t.Fatalf("pruned stable usage: %v %v", units, err)
			}
		})
	}
}

func TestIssue72PrunedRollupProofAdvancesOnlyFromValidCells(t *testing.T) {
	for _, damage := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "deleted_before_write"}[damage], func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			first := fixedPlainEvent()
			first.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa603"
			if err := store.Record(first); err != nil {
				t.Fatal(err)
			}
			if _, err := store.PruneEventsBefore(first.Timestamp.Add(2*time.Second), 100); err != nil {
				t.Fatal(err)
			}
			if damage {
				if err := db.Update(func(tx *bolt.Tx) error {
					return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(first.Timestamp, first.ClientName, first.Target, first.Model))
				}); err != nil {
					t.Fatal(err)
				}
			}
			second := first
			second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa604"
			second.Timestamp = first.Timestamp.Add(time.Minute)
			if err := store.Record(second); err != nil {
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
			journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
			if err != nil {
				t.Fatal(err)
			}
			added, err := journal.RecordOnce(first)
			if added {
				t.Fatal("receipt retry reinserted")
			}
			if damage && !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("missing cell was re-attested: %v", err)
			}
			if !damage && err != nil {
				t.Fatalf("valid updated proof did not survive restart: %v", err)
			}
			from, to := first.Timestamp.Truncate(time.Hour), first.Timestamp.Add(time.Hour)
			units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: first.ClientName, From: &from, To: &to}, replayCosts, false)
			if damage && (len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete)) {
				t.Fatalf("lost old charge hidden: %v %v", units, err)
			}
			if !damage && (err != nil || costUnitTotal(units) != 4) {
				t.Fatalf("valid historical and new spend changed: %v %v", units, err)
			}
		})
	}
}

func TestIssue72PruneCannotAttestCorruptRollup(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Put(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model), []byte(`{"requests":1,"input_tokens":1}`))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(event.Timestamp.Add(2*time.Second), 100); err != nil {
		t.Fatal(err)
	}
	journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := journal.RecordOnce(event); added || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("corrupt historical rollup was trusted: %v %v", added, err)
	}
}

func TestIssue72UnknownSameHourReceiptCannotUseUnrelatedProof(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(event.Timestamp.Add(2*time.Second), 100); err != nil {
		t.Fatal(err)
	}
	other := event
	other.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa605"
	other.Timestamp = event.Timestamp.Add(time.Minute)
	putUsageReceipt(t, db, usageKey(other.Timestamp, other.RecordID), usageReceipt{Version: usageReceiptVersion, Digest: "unknown-digest"})
	journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := journal.RecordOnce(event); added || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("unknown historical receipt was ignored: %v %v", added, err)
	}
}

func TestIssue72CorruptRetentionProvenanceDoesNotResolve(t *testing.T) {
	for _, damage := range []string{"receipt_scope", "rollup_proof", "proof_marker"} {
		t.Run(damage, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			if _, err := store.PruneEventsBefore(event.Timestamp.Add(2*time.Second), 100); err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				switch damage {
				case "receipt_scope":
					b := tx.Bucket([]byte(receiptOwnersBucket))
					key := []byte(usageKey(event.Timestamp, event.RecordID))
					var owner receiptOwner
					if err := json.Unmarshal(b.Get(key), &owner); err != nil {
						return err
					}
					owner.QuotaKey = []byte("tampered")
					data, err := json.Marshal(owner)
					if err != nil {
						return err
					}
					return b.Put(key, data)
				case "rollup_proof":
					return tx.Bucket([]byte(rollupProofBucket)).Put(rollupProofKey(BucketQuotaUsageAggHourly, quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)), []byte("damaged"))
				default:
					return tx.Bucket([]byte(BucketMeta)).Put([]byte(rollupProofMarker), []byte("invalid"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
			if err != nil {
				t.Fatal(err)
			}
			if added, err := journal.RecordOnce(event); added || !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("damaged %s trusted: %v %v", damage, added, err)
			}
		})
	}
}

func TestIssue72UnownedProofSourceCannotDiscardRawOnPrune(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Put(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model), []byte("broken")); err != nil {
			return err
		}
		b, err := tx.CreateBucket([]byte(rollupProofBucket))
		if err != nil {
			return err
		}
		return b.Put([]byte("unowned"), []byte("original"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(event.Timestamp.Add(2*time.Second), 100); err == nil {
		t.Fatal("unowned proof source allowed raw retention deletion")
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketUsageEvents)).Get([]byte(usageKey(event.Timestamp, event.RecordID))) == nil {
			t.Error("failed prune removed raw event")
		}
		if string(tx.Bucket([]byte(rollupProofBucket)).Get([]byte("unowned"))) != "original" {
			t.Error("failed prune changed unowned proof")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIssue72DifferentModelPrunedReceiptDoesNotBlockProvenCell(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	first := fixedPlainEvent()
	first.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa606"
	other := first
	other.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa607"
	other.Model = "advisor"
	other.Timestamp = first.Timestamp.Add(time.Minute)
	if err := store.Record(first); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(other); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(other.Timestamp.Add(2*time.Second), 100); err != nil {
		t.Fatal(err)
	}
	journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := journal.RecordOnce(first); added || err != nil {
		t.Fatalf("different proven receipt blocked: %v %v", added, err)
	}
	from, to := first.Timestamp.Truncate(time.Hour), first.Timestamp.Add(time.Hour)
	if units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: first.ClientName, Model: first.Model, From: &from, To: &to}, replayCosts, false); err != nil || costUnitTotal(units) != 2 {
		t.Fatalf("unrelated model changed price: %v %v", units, err)
	}
}

func TestIssue72BackfillCannotRevivePriorPrunedCellProof(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	first := fixedPlainEvent()
	first.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa608"
	second := first
	second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa609"
	second.Timestamp = first.Timestamp.Add(time.Minute)
	if err := store.Record(first); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(first.Timestamp.Add(2*time.Second), 100); err != nil {
		t.Fatal(err)
	}
	third := first
	third.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa610"
	third.Timestamp = first.Timestamp.Add(2 * time.Minute)
	rows, keys, err := normalizeUsageBundle(third)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(keys[0]), data); err != nil {
			return err
		}
		if err := addClientEventIndex(tx, rows[0], []byte(keys[0]), data); err != nil {
			return err
		}
		return bumpRawRevision(tx)
	}); err != nil {
		t.Fatal(err)
	}
	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := journal.RecordOnce(first); added || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("backfill incorrectly reused old proof: %v %v", added, err)
	}
}

func TestIssue72RetainedProofCannotCoverUnrecordedRawEvent(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	first := fixedPlainEvent()
	first.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa611"
	if err := store.Record(first); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(first.Timestamp.Add(2*time.Second), 100); err != nil {
		t.Fatal(err)
	}
	second := first
	second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa612"
	second.Timestamp = first.Timestamp.Add(time.Minute)
	rows, keys, err := normalizeUsageBundle(second)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(keys[0]), data); err != nil {
			return err
		}
		if err := addClientEventIndex(tx, rows[0], []byte(keys[0]), data); err != nil {
			return err
		}
		return bumpRawRevision(tx)
	}); err != nil {
		t.Fatal(err)
	}
	journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := journal.RecordOnce(second); added || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("unrecorded raw event accepted by stale proof: %v %v", added, err)
	}
	from, to := first.Timestamp.Truncate(time.Hour), first.Timestamp.Add(time.Hour)
	if units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: first.ClientName, From: &from, To: &to}, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("missing event accounting reported complete: %v %v", units, err)
	}
}

func TestIssue72RetainedProofRejectsEqualCountReplacement(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	first := fixedPlainEvent()
	first.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa613"
	second := first
	second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa614"
	second.Timestamp = first.Timestamp.Add(time.Minute)
	if err := store.Record(first); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(first.Timestamp.Add(2*time.Second), 100); err != nil {
		t.Fatal(err)
	}
	replacement := second
	replacement.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa615"
	replacement.Timestamp = second.Timestamp.Add(time.Minute)
	rows, keys, err := normalizeUsageBundle(replacement)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageEvents))
		oldKey := []byte(usageKey(second.Timestamp, second.RecordID))
		old := append([]byte(nil), b.Get(oldKey)...)
		if err := removeClientEventIndex(tx, oldKey, old); err != nil {
			return err
		}
		if err := b.Delete(oldKey); err != nil {
			return err
		}
		if err := b.Put([]byte(keys[0]), data); err != nil {
			return err
		}
		if err := addClientEventIndex(tx, rows[0], []byte(keys[0]), data); err != nil {
			return err
		}
		return bumpRawRevision(tx)
	}); err != nil {
		t.Fatal(err)
	}
	journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := journal.RecordOnce(first); added || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("equal-count replacement was trusted: %v %v", added, err)
	}
}

func TestIssue72UnrelatedMalformedRawDoesNotBlockRetainedProof(t *testing.T) {
	for _, scenario := range []string{"other_owner", "other_hour"} {
		t.Run(scenario, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			if _, err := store.PruneEventsBefore(event.Timestamp.Add(2*time.Second), 100); err != nil {
				t.Fatal(err)
			}
			other := event.Timestamp
			if scenario == "other_hour" {
				other = other.Add(48 * time.Hour)
			}
			owner := "bob"
			if scenario == "other_hour" {
				owner = ""
			}
			raw := []byte(fmt.Sprintf(`{"timestamp":%q,"client_name":%q,"record_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa616","model":123}`, other.Format(time.RFC3339), owner))
			if err := db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte("malformed-fixture"), raw)
			}); err != nil {
				t.Fatal(err)
			}
			journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
			if err != nil {
				t.Fatal(err)
			}
			if added, err := journal.RecordOnce(event); added || err != nil {
				t.Fatalf("unrelated malformed raw blocked proof: %v %v", added, err)
			}
		})
	}
}
