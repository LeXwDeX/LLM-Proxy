package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

// Issue #125 review follow-ups: unresolved/recheck writes keep the
// classification, cancellable classification, ReindexUsageEvent without a
// leaked mark, realtime writes against an older classification.

// installIntegrityClass makes class the production classification of db.
func installIntegrityClass(db *bolt.DB, class *integrityClassification) {
	integrityState.Lock()
	if class == nil {
		delete(integrityState.classes, db)
	} else {
		integrityState.classes[db] = class
	}
	integrityState.Unlock()
}

// compareInstalledWithFull runs q with the installed classification (Fast) and
// without any (Full) in production mode; it reports whether Fast was used.
func compareInstalledWithFull(t *testing.T, db *bolt.DB, q integrityQuery, class *integrityClassification, log []string) bool {
	t.Helper()
	installIntegrityClass(db, nil)
	full := runIntegrityQuery(db, q, false)
	installIntegrityClass(db, class)
	before := UsageIntegrityStats(db).FastReads
	fast := runIntegrityQuery(db, q, false)
	used := UsageIntegrityStats(db).FastReads > before
	for i := range full {
		if full[i] == fast[i] {
			continue
		}
		matched := false
		installIntegrityClass(db, nil)
		for retry := 0; retry < 20 && !matched; retry++ {
			matched = runIntegrityQuery(db, q, false)[i] == fast[i]
		}
		installIntegrityClass(db, class)
		if !matched {
			t.Fatalf("installed Fast != Full for %s\n full: %s\n fast: %s\n log: %v", q, full[i], fast[i], log)
		}
	}
	return used
}

// classifiedSnapshot copies every bucket the classification reads.
func classifiedSnapshot(t *testing.T, db *bolt.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = db.View(func(tx *bolt.Tx) error {
		for _, name := range []string{BucketUsageEvents, rawOwnerBucket, BucketUsageReceipts, receiptOwnersBucket, BucketUsageEventsQuarantine, BucketUsageAggHourly, BucketQuotaUsageAggHourly, rollupProofBucket, BucketUsageEventsByClient, BucketUsageClientHours, indexProofsV2, freshAttestationBucket} {
			if b := tx.Bucket([]byte(name)); b != nil {
				_ = b.ForEach(func(k, v []byte) error {
					out[name+"\x00"+string(k)] = string(v)
					return nil
				})
			}
		}
		return nil
	})
	return out
}

func TestIntegrityUnresolvedAndRecheckKeepClassification(t *testing.T) {
	productionIntegrityMode(t)
	db, store, base := integrityFixture(t)
	classifyNow(t, db)
	before := classifiedSnapshot(t, db)
	// A realtime rejection registers unresolved evidence (journal path).
	rejected := base
	rejected.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaae1"
	original, _ := json.Marshal(rejected)
	if err := store.RegisterUnresolvedUsage(rejected, original); err != nil {
		t.Fatal(err)
	}
	// Evidence for an already committed identity is resolved in place.
	committed := base
	committed.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaab2"
	original, _ = json.Marshal(committed)
	if err := store.RegisterUnresolvedUsage(committed, original); err != nil {
		t.Fatal(err)
	}
	// Recheck every stored evidence entry, plus a version conflict.
	var evidence [][2][]byte
	_ = db.View(func(tx *bolt.Tx) error {
		b, _ := unresolvedState(tx)
		if b != nil {
			_ = b.ForEach(func(k, v []byte) error {
				evidence = append(evidence, [2][]byte{bytes.Clone(k), bytes.Clone(v)})
				return nil
			})
		}
		return nil
	})
	if len(evidence) == 0 {
		t.Fatal("fixture registered no evidence")
	}
	for i, item := range evidence {
		version := recheckHash(item[1])
		if i == 0 {
			version = recheckHash([]byte("stale"))
		}
		if _, err := store.RecheckUsage(UsageRecheckRequest{EvidenceID: recheckHash(item[0]), Version: version, OperationID: uuid.NewString(), Actor: "test"}); err != nil && !errors.Is(err, ErrUsageRecheckConflict) {
			t.Fatalf("recheck: %v", err)
		}
	}
	after := classifiedSnapshot(t, db)
	if len(before) != len(after) {
		t.Fatalf("classified objects changed: %d -> %d", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("classified object %q changed", k)
		}
	}
	if !UsageIntegrityStats(db).Complete {
		t.Fatal("unresolved registration or recheck invalidated the classification")
	}
	integrityState.Lock()
	class := integrityState.classes[db]
	integrityState.Unlock()
	from := base.Timestamp.Add(-4 * time.Hour)
	for _, q := range []integrityQuery{
		{client: base.ClientName, from: base.Timestamp.Truncate(time.Hour), to: base.Timestamp, windowKeys: true, exact: true},
		{from: from, to: base.Timestamp.Add(time.Hour), exact: true},
		{client: "bob", from: base.Timestamp.Add(-3 * time.Hour), to: base.Timestamp.Add(-3 * time.Hour)},
	} {
		if !compareInstalledWithFull(t, db, q, class, nil) {
			t.Fatalf("fast path not used for %s", q)
		}
	}
}

func TestIntegrityClassificationHonoursCancellation(t *testing.T) {
	productionIntegrityMode(t)
	db, _, _ := integrityFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checks := &cancelOnCheck{Context: ctx, cancel: cancel, left: 3}
	if err := RunClassificationOnce(checks, db); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled classification err=%v", err)
	}
	if UsageIntegrityStats(db).Complete {
		t.Fatal("canceled classification was installed")
	}
	classifyNow(t, db)
}

func TestIntegrityClassificationLoopStopsPromptly(t *testing.T) {
	productionIntegrityMode(t)
	db, _, _ := integrityFixture(t)
	started := make(chan struct{})
	var once bool
	integrityClassifyStep = func() {
		if !once {
			once = true
			close(started)
		}
		time.Sleep(20 * time.Millisecond) // a slow classification
	}
	t.Cleanup(func() { integrityClassifyStep = nil })
	stop := StartUsageIntegrityClassification(context.Background(), db, time.Hour, nil)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		stop()
		t.Fatal("classification never reached a cancellation check")
	}
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not return while a classification was running")
	}
	if UsageIntegrityStats(db).Complete {
		t.Fatal("a classification interrupted by stop was installed")
	}
}

func TestIntegrityReindexRollbackDoesNotLeak(t *testing.T) {
	productionIntegrityMode(t)
	db, _, base := integrityFixture(t)
	classifyNow(t, db)
	errRollback := errors.New("rollback")
	err := db.Update(func(tx *bolt.Tx) error {
		key := []byte(usageKey(base.Timestamp, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaab2"))
		before := bytes.Clone(tx.Bucket([]byte(BucketUsageEvents)).Get(key))
		if err := ReindexUsageEvent(tx, key, before, before); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("update err=%v", err)
	}
	if n := integrityInflight.Load(); n != 0 {
		t.Fatalf("rolled back reindex left %d writes in flight", n)
	}
	if UsageIntegrityStats(db).Complete {
		t.Fatal("reindex did not invalidate the classification")
	}
	classifyNow(t, db)
	// A committed reindex invalidates too, and the next build reinstalls.
	if err := db.Update(func(tx *bolt.Tx) error {
		key := []byte(usageKey(base.Timestamp, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaab2"))
		before := bytes.Clone(tx.Bucket([]byte(BucketUsageEvents)).Get(key))
		return ReindexUsageEvent(tx, key, before, before)
	}); err != nil {
		t.Fatal(err)
	}
	if UsageIntegrityStats(db).Complete || integrityInflight.Load() != 0 {
		t.Fatalf("committed reindex: %+v", UsageIntegrityStats(db))
	}
	classifyNow(t, db)
}

// Realtime writes after a classification must keep Fast == Full under that
// older classification, or invalidate it (v3 item 4).
func TestIntegrityRealtimeWritesAgainstOlderClassification(t *testing.T) {
	productionIntegrityMode(t)
	seeds := integrityFuzzIterations(20)
	used, invalidated, writes := 0, 0, 0
	for seed := 0; seed < seeds; seed++ {
		rng := rand.New(rand.NewSource(int64(5_000_000 + seed)))
		f := buildIntegrityFuzzDB(t, rng)
		if err := RunClassificationOnce(context.Background(), f.db); err != nil {
			t.Fatal(err)
		}
		integrityState.Lock()
		class := integrityState.classes[f.db]
		integrityState.Unlock()
		if class == nil {
			continue
		}
		var recorded []usage.Event
		for i := 0; i < 2+rng.Intn(5); i++ {
			e := fuzzEvent(rng)
			switch rng.Intn(5) {
			case 0:
				e.ClientName = ""
			case 1:
				e.ClientName = "unknown"
			case 2:
				e.Timestamp = e.Timestamp.In(time.FixedZone("plus8", 8*3600))
			case 3:
				if len(recorded) > 0 {
					e = recorded[rng.Intn(len(recorded))] // retry of an existing row
				}
			case 4:
				e.Target = ""
			}
			err := f.store.Record(e)
			f.note("realtime %s client=%q target=%q zone=%s err=%v", e.RecordID, e.ClientName, e.Target, e.Timestamp.Location(), err)
			if err == nil {
				recorded = append(recorded, e)
			}
			writes++
		}
		if !UsageIntegrityStats(f.db).Complete {
			invalidated++
			continue
		}
		for i := 0; i < 8; i++ {
			if compareInstalledWithFull(t, f.db, randomIntegrityQuery(rng), class, f.log) {
				used++
			}
		}
		installIntegrityClass(f.db, nil)
	}
	t.Logf("realtime writes=%d, databases invalidated=%d, fast reads compared=%d", writes, invalidated, used)
	if used < seeds*4 {
		t.Fatalf("only %d fast reads compared against the older classification", used)
	}
}

var _ = fmt.Sprintf
