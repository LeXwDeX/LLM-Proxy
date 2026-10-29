package nosql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// oracleContinuation is the ec8cb7c continuation algorithm, kept independent
// of the optimized scope proof so semantic regressions can be detected.
func oracleContinuation(ctx context.Context, tx *bolt.Tx, rows []usage.Event) bool {
	meta := tx.Bucket([]byte(BucketMeta))
	raw := tx.Bucket([]byte(BucketUsageEvents))
	if meta == nil || raw == nil || len(rows) == 0 || meta.Get([]byte(usageAccountingProofTrackedKey)) != nil || checkUsageIdentityAdmission(tx, rows) != nil {
		return false
	}
	type cellPair struct{ usage, quota string }
	scopes := make(map[string]bool)
	owners := make(map[string]bool)
	hours := make(map[time.Time]bool)
	for _, row := range rows {
		client := strings.TrimSpace(row.ClientName)
		if client == "" || strings.EqualFold(client, "unknown") || row.Timestamp.IsZero() {
			return false
		}
		hour := row.Timestamp.UTC().Truncate(time.Hour)
		scopes[client+"\x00"+hour.Format(hourLayout)] = true
		owners[receiptClientHash(client)] = true
		hours[hour] = true
	}
	if !quarantineOutsideProof(tx, owners, hours) || !freshSnapshotsDisjoint(tx, owners, hours) {
		return false
	}
	covered := make(map[cellPair]bool)
	seenRoots := make(map[string]bool)
	if err := raw.ForEach(func(k, value []byte) error {
		if err := proofScanStep(ctx, false); err != nil {
			return err
		}
		var event usage.Event
		if !decodeProofEvent(value, &event) || usageKey(event.Timestamp, event.RecordID) != string(k) {
			if !proofRawMayOverlap(tx, k, value, owners, hours) {
				return nil
			}
			return ErrUsageHistoryIncomplete
		}
		hour := event.Timestamp.UTC().Truncate(time.Hour)
		if !scopes[strings.TrimSpace(event.ClientName)+"\x00"+hour.Format(hourLayout)] {
			return nil
		}
		rootKey := string(k)
		if event.Supplementary {
			if strings.TrimSpace(event.ParentRecordID) == "" {
				return ErrUsageHistoryIncomplete
			}
			rootKey = usageKey(event.Timestamp, event.ParentRecordID)
		} else if event.ParentRecordID != "" {
			return ErrUsageHistoryIncomplete
		}
		var root usage.Event
		if !decodeProofEvent(raw.Get([]byte(rootKey)), &root) || root.Supplementary || root.ParentRecordID != "" {
			return ErrUsageHistoryIncomplete
		}
		anchorRows, anchorKeys, err := normalizeUsageBundle(root)
		if err != nil || len(anchorKeys) == 0 || anchorKeys[0] != rootKey || !validFreshAttestation(tx, anchorRows, anchorKeys) {
			return ErrUsageHistoryIncomplete
		}
		found := false
		for i, anchorKey := range anchorKeys {
			if anchorKey != string(k) {
				continue
			}
			want, err := usage.RetryEquivalentDigest(anchorRows[i])
			if err != nil {
				return err
			}
			got, err := usage.RetryEquivalentDigest(event)
			if err != nil || got != want {
				return ErrUsageHistoryIncomplete
			}
			found = true
			break
		}
		if !found {
			return ErrUsageHistoryIncomplete
		}
		if seenRoots[rootKey] {
			return nil
		}
		committed, err := inspectUsageBundleTx(tx, anchorRows, anchorKeys, nil)
		if err != nil || !committed {
			return ErrUsageHistoryIncomplete
		}
		needsProof := false
		for _, row := range anchorRows {
			pair := cellPair{string(aggKey(row.Timestamp, row.EndpointType, row.ClientName, row.Model, row.Target)), string(quotaAggKey(row.Timestamp, row.ClientName, row.Target, row.Model))}
			if !covered[pair] {
				needsProof = true
				break
			}
		}
		if needsProof {
			proven, err := usageAccountingProvenContext(ctx, tx, anchorRows, anchorKeys)
			if err != nil || !proven {
				return ErrUsageHistoryIncomplete
			}
		}
		seenRoots[rootKey] = true
		for _, row := range anchorRows {
			covered[cellPair{string(aggKey(row.Timestamp, row.EndpointType, row.ClientName, row.Model, row.Target)), string(quotaAggKey(row.Timestamp, row.ClientName, row.Target, row.Model))}] = true
		}
		return nil
	}); err != nil {
		return false
	}
	if len(seenRoots) == 0 {
		return false
	}
	for _, row := range rows {
		if !covered[cellPair{string(aggKey(row.Timestamp, row.EndpointType, row.ClientName, row.Model, row.Target)), string(quotaAggKey(row.Timestamp, row.ClientName, row.Target, row.Model))}] && !unoccupiedContinuationCellContext(ctx, tx, row) {
			return false
		}
	}
	return true
}

func TestContinuationMulticellScanBudget(t *testing.T) {
	for _, cells := range []int{4, 8} {
		t.Run(fmt.Sprint(cells), func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			if err := store.EnsureUsageReceipts(); err != nil {
				t.Fatal(err)
			}
			const history = 24
			contentionHistory(t, db, history, true)
			event := func(i, generation int) usage.Event {
				e := fixedPlainEvent()
				e.ClientName = fmt.Sprintf("active-%d", i%2)
				e.Model = fmt.Sprintf("model-%d", i)
				e.RecordID = fmt.Sprintf("40000000-0000-4000-8000-%012x", generation*100+i+1)
				e.Timestamp = e.Timestamp.Add(time.Duration(generation) * time.Second)
				return e
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				for i := 0; i < 3; i++ {
					e := event(i, 0)
					e.ClientName = "retained"
					e.Timestamp = e.Timestamp.Add(-72 * time.Hour)
					key := []byte(usageKey(e.Timestamp, e.RecordID))
					digest, err := usage.RetryEquivalentDigest(e)
					if err != nil {
						return err
					}
					data, err := json.Marshal(e)
					if err != nil {
						return err
					}
					if err = writeUsageReceipt(tx, key, digest); err != nil {
						return err
					}
					if err = writeReceiptOwner(tx, key, digest, e, data); err != nil {
						return err
					}
				}
				for i := 0; i < cells; i++ {
					rows, keys, err := normalizeUsageBundle(event(i, 0))
					if err != nil {
						return err
					}
					if err = writeUsageRowTx(tx, rows[0], keys[0]); err != nil {
						return err
					}
					if err = writeFreshAttestation(tx, rows[0], keys[0]); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			batch := make([]usage.Event, cells)
			for i := range batch {
				batch[i] = event(i, 1)
			}
			var raw, receipts int
			ctx := context.WithValue(context.Background(), proofScanHooksKey{}, &directIntegrityHooks{raw: func() { raw++ }, receipt: func() { receipts++ }})
			if err := store.recordBatchContext(ctx, batch); err != nil {
				t.Fatal(err)
			}
			if err := store.RecordBatch(batch); err != nil {
				t.Fatal(err)
			}
			if err := db.View(func(tx *bolt.Tx) error {
				for _, e := range batch {
					rows, keys, err := normalizeUsageBundle(e)
					if err != nil {
						return err
					}
					if !validFreshAttestation(tx, rows, keys) {
						return fmt.Errorf("missing attestation for %s", e.RecordID)
					}
					var stored usage.Event
					if err := json.Unmarshal(tx.Bucket([]byte(BucketUsageEvents)).Get([]byte(keys[0])), &stored); err != nil || stored.InputTokens != e.InputTokens || stored.PricingRuleID != e.PricingRuleID {
						return fmt.Errorf("raw row mismatch for %s: %v", e.RecordID, err)
					}
					for _, bucketKey := range []struct {
						bucket string
						key    []byte
					}{
						{BucketUsageAggHourly, aggKey(e.Timestamp, e.EndpointType, e.ClientName, e.Model, e.Target)},
						{BucketQuotaUsageAggHourly, quotaAggKey(e.Timestamp, e.ClientName, e.Target, e.Model)},
					} {
						var cell AggCell
						if err := json.Unmarshal(tx.Bucket([]byte(bucketKey.bucket)).Get(bucketKey.key), &cell); err != nil || cell.InputTokens != 2*e.InputTokens || cell.recordCount() != 2 {
							return fmt.Errorf("%s rollup mismatch: %+v err=%v", bucketKey.bucket, cell, err)
						}
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, e := range batch {
				from := e.Timestamp.Truncate(time.Hour)
				to := from.Add(time.Hour - time.Nanosecond)
				filter := usage.Filter{ClientName: e.ClientName, Model: e.Model, From: &from, To: &to}
				result, err := store.readCostUnits(context.Background(), filter, exactCostTable{usage.CostTable{e.Model: {InputPer1MTokens: 2}}}, true, nil, costReadHooks{})
				if err != nil || costUnitTotal(result.units) != 4 {
					t.Fatalf("cost %s: units=%+v err=%v", e.RecordID, result.units, err)
				}
			}
			// Four proof passes, each reaching only the rows of the hours being
			// proven: retained history and the receipts outside those hours are
			// not visited, and the pass count does not grow with the cell count.
			if raw != 4*cells || receipts != 0 {
				t.Fatalf("proof scan raw=%d receipts=%d, want raw=%d receipts=0 (history of %d rows is out of scope)", raw, receipts, 4*cells, history)
			}
		})
	}
}

func TestContinuationScopeOracleAndCancellation(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	if err := store.EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	contentionHistory(t, db, 2, true)
	base := fixedPlainEvent()
	base.ClientName = "scope-client"
	if err := store.RecordBatch([]usage.Event{base}); err != nil {
		t.Fatal(err)
	}
	clean := base
	clean.RecordID = "40000000-0000-4000-8000-000000000099"
	conflict := clean
	conflict.EndpointType = "other-endpoint"
	if err := db.View(func(tx *bolt.Tx) error {
		memo := freshScopeMemo{cells: make(map[string]bool), scopes: make(map[string]continuationProof)}
		for _, tc := range []struct {
			event usage.Event
			want  bool
		}{{conflict, false}, {clean, true}, {conflict, false}} {
			rows, _, err := normalizeUsageBundle(tc.event)
			if err != nil {
				return err
			}
			oracle := oracleContinuation(context.Background(), tx, rows)
			got := memo.proven(context.Background(), tx, rows)
			if got != tc.want || got != oracle {
				return fmt.Errorf("%s: got %v oracle %v want %v", tc.event.EndpointType, got, oracle, tc.want)
			}
		}
		rows, _, err := normalizeUsageBundle(clean)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if memo.proven(ctx, tx, rows) {
			return fmt.Errorf("canceled cache hit accepted")
		}
		freshMemo := freshScopeMemo{cells: make(map[string]bool), scopes: make(map[string]continuationProof)}
		ctx, cancel = context.WithCancel(context.Background())
		ctx = context.WithValue(ctx, proofScanHooksKey{}, &directIntegrityHooks{raw: func() { cancel() }})
		if freshMemo.proven(ctx, tx, rows) || len(freshMemo.cells) != 0 || len(freshMemo.scopes) != 0 {
			return fmt.Errorf("canceled partial proof cached")
		}
		if !freshMemo.proven(context.Background(), tx, rows) {
			return fmt.Errorf("clean proof after cancellation failed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestContinuationSecondRootDamageMatchesOracle(t *testing.T) {
	for _, damage := range []string{"attestation", "index", "zero", "digest", "quarantine", "snapshot", "source-unknown"} {
		t.Run(damage, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			if err := store.EnsureUsageReceipts(); err != nil {
				t.Fatal(err)
			}
			contentionHistory(t, db, 1, true)
			first := fixedPlainEvent()
			first.ClientName = "scope-client"
			second := first
			second.RecordID = "40000000-0000-4000-8000-000000000002"
			second.Model = "second"
			if err := store.RecordBatch([]usage.Event{first, second}); err != nil {
				t.Fatal(err)
			}
			if err := db.View(func(tx *bolt.Tx) error {
				rows, _, err := normalizeUsageBundle(first)
				if err != nil {
					return err
				}
				memo := freshScopeMemo{cells: make(map[string]bool), scopes: make(map[string]continuationProof)}
				if !memo.proven(context.Background(), tx, rows) {
					return fmt.Errorf("initial scope not proven")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				key := []byte(usageKey(second.Timestamp, second.RecordID))
				switch damage {
				case "attestation":
					return tx.Bucket([]byte(freshAttestationBucket)).Delete(key)
				case "index":
					return tx.Bucket([]byte(BucketUsageEventsByClient)).Delete(clientEventIndexKey(second, key))
				case "zero":
					return tx.Bucket([]byte(BucketUsageAggHourly)).Put(aggKey(second.Timestamp, second.EndpointType, second.ClientName, second.Model, second.Target), []byte("{}"))
				case "digest":
					return tx.Bucket([]byte(BucketUsageClientHours)).Put(clientHourKey(second.ClientName, second.Timestamp), []byte("broken"))
				case "quarantine":
					return tx.Bucket([]byte(BucketUsageEventsQuarantine)).Put(key, []byte("broken"))
				case "snapshot":
					root, err := tx.CreateBucketIfNotExists([]byte(usageBackfillSnapshotBucket))
					if err != nil {
						return err
					}
					source, err := root.CreateBucketIfNotExists([]byte(BucketUsageAggHourly))
					if err != nil {
						return err
					}
					generation, err := source.CreateBucketIfNotExists([]byte("fixture"))
					if err != nil {
						return err
					}
					return generation.Put([]byte("unknown"), []byte("broken"))
				case "source-unknown":
					return tx.Bucket([]byte(BucketMeta)).Put([]byte(usageAggregateSourceMissingPrefix+BucketUsageAggHourly), []byte("1"))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.View(func(tx *bolt.Tx) error {
				rows, _, err := normalizeUsageBundle(first)
				if err != nil {
					return err
				}
				oracle := oracleContinuation(context.Background(), tx, rows)
				memo := freshScopeMemo{cells: make(map[string]bool), scopes: make(map[string]continuationProof)}
				got := memo.proven(context.Background(), tx, rows)
				if got || oracle || got != oracle {
					return fmt.Errorf("got=%v oracle=%v", got, oracle)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestContinuationScopeMemoExactPairs(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	if err := store.EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	contentionHistory(t, db, 1, true)
	base := fixedPlainEvent()
	makeRow := func(client string, offset time.Duration) usage.Event {
		e := base
		e.ClientName = client
		e.Timestamp = e.Timestamp.Add(offset)
		return e
	}
	first := []usage.Event{makeRow("a", 0), makeRow("b", time.Hour)}
	cross := []usage.Event{makeRow("a", time.Hour), makeRow("b", 0)}
	if err := db.View(func(tx *bolt.Tx) error {
		memo := make(map[string]continuationProof)
		for _, rows := range [][]usage.Event{first, {first[1], first[0], first[0]}, cross, {makeRow("A", 0), first[1]}, {makeRow("x\xff", 0)}, {makeRow("x\xfe", 0)}} {
			if continuationScopeProvenMemoContext(context.Background(), tx, rows, memo) != oracleContinuation(context.Background(), tx, rows) {
				return fmt.Errorf("oracle mismatch")
			}
		}
		if len(memo) != 5 {
			return fmt.Errorf("scope pairs aliased: %d entries, want 5", len(memo))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestContinuationScopeMemoCrossPairsDoNotAlias(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	if err := store.EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	contentionHistory(t, db, 1, true)
	base := fixedPlainEvent()
	row := func(client string, hour int, id int) usage.Event {
		e := base
		e.ClientName = client
		e.Timestamp = e.Timestamp.Add(time.Duration(hour) * time.Hour)
		e.RecordID = fmt.Sprintf("40000000-0000-4000-8000-%012x", id)
		return e
	}
	a0, b1, a1, b0 := row("a", 0, 1), row("b", 1, 2), row("a", 1, 3), row("b", 0, 4)
	if err := db.Update(func(tx *bolt.Tx) error {
		for i, e := range []usage.Event{a0, b1, a1, b0} {
			rows, keys, err := normalizeUsageBundle(e)
			if err != nil {
				return err
			}
			if err = writeUsageRowTx(tx, rows[0], keys[0]); err != nil {
				return err
			}
			if i != 2 {
				if err = writeFreshAttestation(tx, rows[0], keys[0]); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		memo := make(map[string]continuationProof)
		for _, tc := range []struct {
			rows []usage.Event
			want bool
		}{{[]usage.Event{a0, b1}, true}, {[]usage.Event{a1, b0}, false}, {[]usage.Event{a0, b1}, true}} {
			got := continuationScopeProvenMemoContext(context.Background(), tx, tc.rows, memo)
			oracle := oracleContinuation(context.Background(), tx, tc.rows)
			if got != tc.want || got != oracle {
				return fmt.Errorf("got=%v oracle=%v want=%v", got, oracle, tc.want)
			}
		}
		if len(memo) != 2 {
			return fmt.Errorf("expected two separate scopes, got %d", len(memo))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestContinuationMixedAccountingMatchesOracle(t *testing.T) {
	for _, singleBundle := range []bool{false, true} {
		t.Run(fmt.Sprintf("single-bundle=%v", singleBundle), func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			if err := store.EnsureUsageReceipts(); err != nil {
				t.Fatal(err)
			}
			contentionHistory(t, db, 1, true)
			first := fixedPlainEvent()
			first.ClientName = "mixed"
			first.Model = "executor"
			old := first
			old.RecordID = "30000000-0000-4000-8000-000000000001"
			old.Timestamp = first.Timestamp.Add(-10 * time.Second)
			if err := store.RecordBatch([]usage.Event{old}); err != nil {
				t.Fatal(err)
			}
			second := first
			second.RecordID = "40000000-0000-4000-8000-000000000002"
			second.Model = "advisor"
			if singleBundle {
				first.Components = fixedIdentityBundle().Components
				if err := store.RecordBatch([]usage.Event{first}); err != nil {
					t.Fatal(err)
				}
			} else if err := store.RecordBatch([]usage.Event{first, second}); err != nil {
				t.Fatal(err)
			}
			if n, err := store.PruneEventsBefore(first.Timestamp.Add(-5*time.Second), 1); err != nil || n != 2 {
				t.Fatalf("prune=%d err=%v", n, err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				return invalidateRollupProof(tx, BucketQuotaUsageAggHourly, quotaAggKey(second.Timestamp, second.ClientName, second.Target, second.Model))
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.View(func(tx *bolt.Tx) error {
				rows, keys, err := normalizeUsageBundle(first)
				if err != nil {
					return err
				}
				if !singleBundle {
					retained, err := usageRetainedRollupsProvenContext(context.Background(), tx, rows, keys)
					if err != nil || !retained {
						return fmt.Errorf("first not retained proven: %v %v", retained, err)
					}
					secondRows, secondKeys, err := normalizeUsageBundle(second)
					if err != nil {
						return err
					}
					retained, err = usageRetainedRollupsProvenContext(context.Background(), tx, secondRows, secondKeys)
					if err != nil || retained {
						return fmt.Errorf("second retained proof should fail: %v %v", retained, err)
					}
					raw, err := usageAccountingRawProvenContext(context.Background(), tx, secondRows, secondKeys)
					if err != nil || !raw {
						return fmt.Errorf("second raw proof failed: %v %v", raw, err)
					}
				}
				oracle := oracleContinuation(context.Background(), tx, rows)
				got := continuationScopeProvenContext(context.Background(), tx, rows)
				if got != oracle || got == singleBundle {
					return fmt.Errorf("got=%v oracle=%v single=%v", got, oracle, singleBundle)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestContinuationSecondBundleMembershipMatchesOracle(t *testing.T) {
	for _, damage := range []string{"missing-child", "undeclared-child"} {
		t.Run(damage, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			if err := store.EnsureUsageReceipts(); err != nil {
				t.Fatal(err)
			}
			contentionHistory(t, db, 1, true)
			first := fixedPlainEvent()
			first.ClientName = "scope-client"
			second := first
			second.RecordID = "40000000-0000-4000-8000-000000000002"
			second.Components = fixedIdentityBundle().Components
			if err := store.RecordBatch([]usage.Event{first, second}); err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				rows, keys, err := normalizeUsageBundle(second)
				if err != nil {
					return err
				}
				if damage == "missing-child" {
					return tx.Bucket([]byte(BucketUsageEvents)).Delete([]byte(keys[1]))
				}
				stray := rows[1]
				stray.RecordID = "50000000-0000-4000-8000-000000000001"
				return writeUsageRowTx(tx, stray, usageKey(stray.Timestamp, stray.RecordID))
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.View(func(tx *bolt.Tx) error {
				rows, _, err := normalizeUsageBundle(first)
				if err != nil {
					return err
				}
				oracle := oracleContinuation(context.Background(), tx, rows)
				got := continuationScopeProvenContext(context.Background(), tx, rows)
				if got || oracle || got != oracle {
					return fmt.Errorf("got=%v oracle=%v", got, oracle)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestContinuationBatchRejectedKeepsEvidenceAndAccounting(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	if err := store.EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	contentionHistory(t, db, 1, true)
	first := fixedPlainEvent()
	first.ClientName = "reject-scope"
	if err := store.RecordBatch([]usage.Event{first}); err != nil {
		t.Fatal(err)
	}
	fresh := first
	fresh.RecordID = "40000000-0000-4000-8000-000000000099"
	conflict := first
	conflict.InputTokens++
	if err := store.RecordBatch([]usage.Event{fresh, conflict}); err == nil {
		t.Fatal("conflicting batch accepted")
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketUsageEvents)).Get([]byte(usageKey(fresh.Timestamp, fresh.RecordID))) != nil {
			return fmt.Errorf("fresh event partially committed")
		}
		for _, pair := range []struct {
			bucket string
			key    []byte
		}{{BucketUsageAggHourly, aggKey(first.Timestamp, first.EndpointType, first.ClientName, first.Model, first.Target)}, {BucketQuotaUsageAggHourly, quotaAggKey(first.Timestamp, first.ClientName, first.Target, first.Model)}} {
			var cell AggCell
			if err := json.Unmarshal(tx.Bucket([]byte(pair.bucket)).Get(pair.key), &cell); err != nil || cell.recordCount() != 1 || cell.InputTokens != first.InputTokens {
				return fmt.Errorf("partial accounting in %s: %+v %v", pair.bucket, cell, err)
			}
		}
		b := tx.Bucket([]byte(unresolvedBucket))
		if b == nil {
			return fmt.Errorf("unresolved evidence missing")
		}
		found := map[string]bool{}
		if err := b.ForEach(func(_, data []byte) error {
			var item unresolvedUsage
			if err := json.Unmarshal(data, &item); err != nil {
				return err
			}
			var original usage.Event
			if err := json.Unmarshal(item.Original, &original); err != nil {
				return err
			}
			if original.RecordID == fresh.RecordID || (original.RecordID == conflict.RecordID && original.InputTokens == conflict.InputTokens) {
				found[original.RecordID] = true
			}
			return nil
		}); err != nil {
			return err
		}
		if !found[fresh.RecordID] || !found[conflict.RecordID] {
			return fmt.Errorf("original evidence missing: %v", found)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
