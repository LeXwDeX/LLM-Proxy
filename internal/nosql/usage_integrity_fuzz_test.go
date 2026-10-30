package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// Issue #125: differential tests between the exhaustive integrity checks
// (Full) and the windowed fast path (Fast). Full(Q) must equal Fast(Q) for
// every query on every database, however damaged.

var integrityFuzzBase = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

var integrityFuzzClients = []string{"alice", "bob", "carol", "", "unknown"}
var integrityFuzzModels = []string{"executor", "advisor", "other"}
var integrityFuzzTargets = []string{"anthropic", "openai"}

func integrityFuzzIterations(def int) int {
	if n, err := strconv.Atoi(os.Getenv("LLMPROXY_INTEGRITY_FUZZ_ITERATIONS")); err == nil && n > 0 {
		return n
	}
	return def
}

func fuzzRecordID(rng *rand.Rand) string {
	b := make([]byte, 16)
	rng.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func fuzzEvent(rng *rand.Rand) usage.Event {
	hour := rng.Intn(40)
	at := integrityFuzzBase.Add(time.Duration(hour)*time.Hour + time.Duration(rng.Intn(3600))*time.Second + time.Duration(rng.Intn(1000))*time.Millisecond)
	e := usage.Event{
		RecordID:     fuzzRecordID(rng),
		Timestamp:    at,
		ClientName:   integrityFuzzClients[rng.Intn(len(integrityFuzzClients))],
		Target:       integrityFuzzTargets[rng.Intn(len(integrityFuzzTargets))],
		Model:        integrityFuzzModels[rng.Intn(len(integrityFuzzModels))],
		EndpointType: "claude",
		RequestID:    fmt.Sprintf("req-%d", rng.Int63()),
		Path:         "/v1/messages",
		StatusCode:   200,
		InputTokens:  int64(1 + rng.Intn(1000)),
		OutputTokens: int64(rng.Intn(500)),
	}
	if rng.Intn(5) == 0 {
		e.Components = []usage.Component{{Model: "advisor", IterationType: "advisor", InputTokens: int64(1 + rng.Intn(100))}}
	}
	return e
}

type integrityFuzzDB struct {
	db     *bolt.DB
	store  *UsageStore
	log    []string
	legacy int // Issue #125 P1.1 rows without record_id
	// legacyExempt holds the case 24 keys whose shape the P1.1 relaxation must
	// exempt unless later damage overwrote them.
	legacyExempt [][]byte
}

func (f *integrityFuzzDB) note(format string, args ...any) {
	f.log = append(f.log, fmt.Sprintf(format, args...))
}

func fuzzKeys(tx *bolt.Tx, bucket string) [][]byte {
	var keys [][]byte
	if b := tx.Bucket([]byte(bucket)); b != nil {
		_ = b.ForEach(func(k, _ []byte) error {
			keys = append(keys, bytes.Clone(k))
			return nil
		})
	}
	return keys
}

func pickKey(rng *rand.Rand, keys [][]byte) []byte {
	if len(keys) == 0 {
		return nil
	}
	return keys[rng.Intn(len(keys))]
}

// buildIntegrityFuzzDB records realistic history (several clients and hours,
// bundles, retention receipts, an index migration) and then applies random
// damage of every class the windowed path must treat as anomalous.
func buildIntegrityFuzzDB(t testing.TB, rng *rand.Rand) *integrityFuzzDB {
	t.Helper()
	db := testDB(t)
	store := NewUsageStore(db)
	store.now = func() time.Time { return integrityFuzzBase.Add(48 * time.Hour) }
	f := &integrityFuzzDB{db: db, store: store}
	events := 6 + rng.Intn(18)
	migrateAt := -1
	if rng.Intn(3) != 0 {
		migrateAt = rng.Intn(events + 1)
	}
	for i := 0; i < events; i++ {
		if i == migrateAt {
			f.note("migrate index: %v", MigrateUsageClientIndex(context.Background(), db, 2))
		}
		e := fuzzEvent(rng)
		if err := store.Record(e); err != nil {
			f.note("record err %v", err)
		}
	}
	if migrateAt == events {
		f.note("migrate index: %v", MigrateUsageClientIndex(context.Background(), db, 2))
	}
	if rng.Intn(2) == 0 {
		cut := integrityFuzzBase.Add(time.Duration(rng.Intn(30)) * time.Hour)
		n, err := store.PruneEventsBefore(cut, 1+rng.Intn(20))
		f.note("prune before %v n=%d err=%v", cut, n, err)
	}
	for i := 0; i < rng.Intn(3); i++ {
		e := fuzzEvent(rng)
		_ = store.Record(e)
	}
	damage := rng.Intn(5)
	for i := 0; i < damage; i++ {
		applyIntegrityDamage(t, rng, f)
	}
	return f
}

func applyIntegrityDamage(t testing.TB, rng *rand.Rand, f *integrityFuzzDB) {
	kind := rng.Intn(26)
	err := f.db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		rawKeys := fuzzKeys(tx, BucketUsageEvents)
		otherHour := integrityFuzzBase.Add(time.Duration(rng.Intn(40)) * time.Hour)
		switch kind {
		case 0: // delete a raw row, leaving owner, index and cells behind
			if k := pickKey(rng, rawKeys); k != nil {
				f.note("delete raw %s", k)
				return raw.Delete(k)
			}
		case 1: // garbage raw bytes
			if k := pickKey(rng, rawKeys); k != nil {
				f.note("garbage raw %s", k)
				return raw.Put(k, []byte(`{"not":"an event"`))
			}
		case 2: // invalid UTF-8
			if k := pickKey(rng, rawKeys); k != nil {
				f.note("invalid utf8 raw %s", k)
				v := bytes.Clone(raw.Get(k))
				return raw.Put(k, append(v[:len(v)-1], 0xff, '}'))
			}
		case 3: // move an unbound/bound row's time to another hour and lower that hour's quota cell
			if k := pickKey(rng, rawKeys); k != nil {
				var e usage.Event
				if json.Unmarshal(raw.Get(k), &e) != nil {
					return nil
				}
				e.Timestamp = otherHour.Add(time.Duration(rng.Intn(3600)) * time.Second)
				data, _ := json.Marshal(e)
				f.note("retime raw %s to %v", k, e.Timestamp)
				if err := raw.Put(k, data); err != nil {
					return err
				}
				quota := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
				qk := quotaAggKey(e.Timestamp, e.ClientName, e.Target, e.Model)
				var cell AggCell
				if v := quota.Get(qk); v != nil && json.Unmarshal(v, &cell) == nil {
					cell.InputTokens = 0
					cell.RecordCount = 0
					cell.Requests = 0
					v, _ := json.Marshal(cell)
					return quota.Put(qk, v)
				}
			}
		case 4: // cross-hour ReindexUsageEvent (a maintenance rewrite)
			if k := pickKey(rng, rawKeys); k != nil {
				before := bytes.Clone(raw.Get(k))
				var e usage.Event
				if json.Unmarshal(before, &e) != nil {
					return nil
				}
				e.Timestamp = otherHour.Add(time.Duration(rng.Intn(3600)) * time.Second)
				after, _ := json.Marshal(e)
				if err := raw.Put(k, after); err != nil {
					return err
				}
				f.note("reindex %s to %v", k, e.Timestamp)
				if err := ReindexUsageEvent(tx, k, before, after); err != nil {
					return raw.Put(k, before)
				}
			}
		case 5: // non-canonical stamp (offset), numeric id, zero timestamp
			e := fuzzEvent(rng)
			variant := rng.Intn(3)
			var key string
			switch variant {
			case 0:
				key = e.Timestamp.In(time.FixedZone("x", 8*3600)).Format(time.RFC3339Nano) + "_" + e.RecordID
			case 1:
				e.RecordID = strconv.Itoa(rng.Intn(1000))
				key = usageKey(e.Timestamp, e.RecordID)
			case 2:
				e.Timestamp = time.Time{}
				key = usageKey(e.Timestamp, e.RecordID)
			}
			data, _ := json.Marshal(e)
			f.note("legacy raw %s", key)
			return raw.Put([]byte(key), data)
		case 6: // invalid raw owner
			if b := tx.Bucket([]byte(rawOwnerBucket)); b != nil {
				if k := pickKey(rng, fuzzKeys(tx, rawOwnerBucket)); k != nil {
					f.note("garbage owner %s", k)
					return b.Put(k, []byte(`{"version":1}`))
				}
			}
		case 7: // owner for a different client than the row
			if b := tx.Bucket([]byte(rawOwnerBucket)); b != nil {
				if k := pickKey(rng, fuzzKeys(tx, rawOwnerBucket)); k != nil {
					var e usage.Event
					if json.Unmarshal(raw.Get(k), &e) == nil {
						e.ClientName = "mallory"
						data, _ := json.Marshal(e)
						f.note("owner disagrees %s", k)
						return raw.Put(k, data)
					}
				}
			}
		case 8: // malformed quota keys and null/lowered cells
			quota := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
			switch rng.Intn(4) {
			case 0:
				f.note("malformed quota key")
				return quota.Put([]byte("bad-hour|"+quotaKeyEncode("alice")+"|anthropic|executor"), []byte(`{"input_tokens":1}`))
			case 1:
				if k := pickKey(rng, fuzzKeys(tx, BucketQuotaUsageAggHourly)); k != nil {
					f.note("null quota %s", k)
					return quota.Put(k, []byte("null"))
				}
			case 2:
				if k := pickKey(rng, fuzzKeys(tx, BucketQuotaUsageAggHourly)); k != nil {
					f.note("lower quota %s", k)
					return quota.Put(k, []byte(`{"requests":0}`))
				}
			case 3:
				f.note("odd quota key in window")
				return quota.Put([]byte(otherHour.Format(hourLayout)+"|%zz|x|y"), []byte(`{}`))
			}
		case 9: // rollup proof damage
			proofs := tx.Bucket([]byte(rollupProofBucket))
			if proofs == nil {
				return nil
			}
			keys := fuzzKeys(tx, rollupProofBucket)
			if rng.Intn(2) == 0 {
				if k := pickKey(rng, keys); k != nil {
					f.note("garbage proof %q", k)
					return proofs.Put(k, []byte(`{}`))
				}
			}
			f.note("malformed proof key")
			return proofs.Put(rollupProofKey(BucketQuotaUsageAggHourly, []byte("zz|x|y|z")), []byte(`{}`))
		case 10: // client index: delete, wrong pointer, orphan, nested
			index := tx.Bucket([]byte(BucketUsageEventsByClient))
			if index == nil {
				return nil
			}
			keys := fuzzKeys(tx, BucketUsageEventsByClient)
			switch rng.Intn(4) {
			case 0:
				if k := pickKey(rng, keys); k != nil {
					f.note("delete index %q", k)
					return index.Delete(k)
				}
			case 1:
				if k := pickKey(rng, keys); k != nil {
					f.note("index pointer %q", k)
					return index.Put(k, pickKey(rng, rawKeys))
				}
			case 2:
				e := fuzzEvent(rng)
				key := []byte(usageKey(e.Timestamp, e.RecordID))
				f.note("orphan index %s", key)
				return index.Put(clientEventIndexKey(e, key), key)
			case 3:
				f.note("nested index")
				_, err := index.CreateBucketIfNotExists([]byte(clientIndexPrefix("alice") + "x"))
				return err
			}
		case 11: // index proof of a malformed row in another hour
			proofs := tx.Bucket([]byte(indexProofsV2))
			if proofs == nil {
				return nil
			}
			p, err := loadIndexProgressState(tx, false)
			if err != nil || p == nil {
				return nil
			}
			if k := pickKey(rng, rawKeys); k != nil {
				var e usage.Event
				if json.Unmarshal(raw.Get(k), &e) != nil {
					return nil
				}
				original := e
				original.Timestamp = otherHour
				proof := indexProof{Version: 2, Generation: p.Generation, Origin: "atomic", IndexKey: clientEventIndexKey(original, k), RawDigest: fmt.Sprintf("%x", rawDigest(k, raw.Get(k)))}
				proof.Binding = proof.binding(k)
				data, _ := json.Marshal(proof)
				f.note("cross-hour index proof %s", k)
				if err := proofs.Put(k, data); err != nil {
					return err
				}
				if rng.Intn(2) == 0 {
					return raw.Put(k, []byte(`{"client_name":"alice"`))
				}
			}
		case 12: // receipt without owner (pre-b8a3f86) or corrupt receipt
			receipts := tx.Bucket([]byte(BucketUsageReceipts))
			if k := pickKey(rng, fuzzKeys(tx, BucketUsageReceipts)); k != nil {
				if rng.Intn(2) == 0 {
					if owners := tx.Bucket([]byte(receiptOwnersBucket)); owners != nil {
						f.note("receipt owner removed %s", k)
						return owners.Delete(k)
					}
				}
				f.note("garbage receipt %s", k)
				return receipts.Put(k, []byte(`{}`))
			}
		case 13: // receipt in an old hour without any owner, odd stamp
			receipts := tx.Bucket([]byte(BucketUsageReceipts))
			e := fuzzEvent(rng)
			key := e.Timestamp.Format("2006-01-02T15:04:05-07:00") + "_" + e.RecordID
			f.note("odd receipt %s", key)
			return receipts.Put([]byte(key), []byte(`{}`))
		case 14: // fresh attestation whose root vanished
			if b := tx.Bucket([]byte(freshAttestationBucket)); b != nil {
				if k := pickKey(rng, fuzzKeys(tx, freshAttestationBucket)); k != nil {
					f.note("attested root deleted %s", k)
					if err := raw.Delete(k); err != nil {
						return err
					}
					if r := tx.Bucket([]byte(BucketUsageReceipts)); r != nil {
						return r.Delete(k)
					}
				}
			}
		case 15: // unresolved evidence
			e := fuzzEvent(rng)
			original, _ := json.Marshal(e)
			f.note("unresolved %s", e.RecordID)
			return putUnresolvedEvidence(tx, unresolvedUsage{Event: e, Original: original})
		case 16: // quarantine a row (as retention does for undecodable rows)
			if k := pickKey(rng, rawKeys); k != nil {
				f.note("quarantine %s", k)
				data := bytes.Clone(raw.Get(k))
				if err := quarantineUsageEvent(tx, k, data); err != nil {
					return nil
				}
				return raw.Delete(k)
			}
		case 17: // nested buckets in raw / quota
			f.note("nested raw/quota")
			if _, err := raw.CreateBucketIfNotExists([]byte(otherHour.Format(time.RFC3339Nano) + "_nested")); err != nil {
				return err
			}
			_, err := tx.Bucket([]byte(BucketQuotaUsageAggHourly)).CreateBucketIfNotExists([]byte(otherHour.Format(hourLayout) + "|nested"))
			return err
		case 18: // invalid or nested owner records
			if b := tx.Bucket([]byte(rawOwnerBucket)); b != nil {
				e := fuzzEvent(rng)
				f.note("nested owner")
				_, err := b.CreateBucketIfNotExists([]byte(usageKey(e.Timestamp, e.RecordID)))
				return err
			}
		case 19: // ambiguous raw scope
			if k := pickKey(rng, rawKeys); k != nil {
				f.note("ambiguous raw %s", k)
				return raw.Put(k, []byte(`{"client_name":"alice","client_name":"bob","timestamp":"2026-10-20T01:00:00Z"}`))
			}
		case 20: // raw row with a different client (owner digest mismatch) and same key
			if k := pickKey(rng, rawKeys); k != nil {
				var e usage.Event
				if json.Unmarshal(raw.Get(k), &e) == nil {
					e.InputTokens++
					data, _ := json.Marshal(e)
					f.note("tokens changed %s", k)
					return raw.Put(k, data)
				}
			}
		case 21: // raw for an empty/unknown client without owner, in an odd hour
			e := fuzzEvent(rng)
			e.ClientName = ""
			data, _ := json.Marshal(e)
			f.note("unowned raw")
			return raw.Put([]byte(usageKey(e.Timestamp, e.RecordID)), data)
		case 22: // owner record without raw row in an old hour
			if b := tx.Bucket([]byte(rawOwnerBucket)); b != nil {
				e := fuzzEvent(rng)
				e.ClientName = "alice"
				f.note("orphan owner")
				return writeRawOwner(tx, []byte(usageKey(e.Timestamp, e.RecordID)), e)
			}
		case 23: // quota cell inflated beyond raw (unverified records)
			quota := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
			if k := pickKey(rng, fuzzKeys(tx, BucketQuotaUsageAggHourly)); k != nil {
				var cell AggCell
				if json.Unmarshal(quota.Get(k), &cell) == nil {
					cell.RecordCount = cell.recordCount() + 3
					cell.InputTokens += 10
					v, _ := json.Marshal(cell)
					f.note("inflate quota %s", k)
					return quota.Put(k, v)
				}
			}
		case 24: // legacy row: no record_id, key stamp exactly the event time, no owner
			e := fuzzEvent(rng)
			e.RecordID = ""
			data, _ := json.Marshal(e)
			key := []byte(usageKey(e.Timestamp, fmt.Sprintf("legacy-%d", rng.Intn(1000))))
			f.note("legacy no-record-id raw %s", key)
			f.legacy++
			f.legacyExempt = append(f.legacyExempt, bytes.Clone(key))
			return raw.Put(key, data)
		case 25: // legacy-shaped row whose event hour differs from its key hour
			e := fuzzEvent(rng)
			e.RecordID = ""
			e.Timestamp = otherHour.Add(time.Duration(rng.Intn(3600)) * time.Second)
			data, _ := json.Marshal(e)
			keyAt := otherHour.Add(time.Hour + time.Duration(rng.Intn(3600))*time.Second)
			key := []byte(usageKey(keyAt, fmt.Sprintf("legacy-moved-%d", rng.Intn(1000))))
			f.note("legacy cross-hour raw %s event %v", key, e.Timestamp)
			f.legacy++
			return raw.Put(key, data)
		}
		return nil
	})
	if err != nil {
		f.note("damage %d err %v", kind, err)
	}
}

type integrityQuery struct {
	client, model string
	allowed       map[string]bool // nil: no pair filter
	from, to      time.Time
	windowKeys    bool
	exact         bool
}

func (q integrityQuery) String() string {
	keys := make([]string, 0, len(q.allowed))
	for k := range q.allowed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Sprintf("client=%q model=%q allow=%v from=%s to=%s windowKeys=%v exact=%v", q.client, q.model, keys, q.from.Format(time.RFC3339), q.to.Format(time.RFC3339), q.windowKeys, q.exact)
}

func (q integrityQuery) allow() costPairFilter {
	if q.allowed == nil {
		return nil
	}
	return func(target, model string) bool { return q.allowed[strings.ToLower(target)+"/"+strings.ToLower(model)] }
}

func randomIntegrityQuery(rng *rand.Rand) integrityQuery {
	clients := append([]string{"", "zed", "mallory"}, integrityFuzzClients...)
	q := integrityQuery{client: clients[rng.Intn(len(clients))]}
	if rng.Intn(3) == 0 {
		q.model = integrityFuzzModels[rng.Intn(len(integrityFuzzModels))]
	}
	if rng.Intn(3) == 0 {
		q.allowed = map[string]bool{}
		for _, target := range integrityFuzzTargets {
			for _, model := range integrityFuzzModels {
				if rng.Intn(2) == 0 {
					q.allowed[target+"/"+model] = true
				}
			}
		}
	}
	start := rng.Intn(44) - 2
	span := rng.Intn(12)
	switch rng.Intn(6) {
	case 0:
		span = 48 + rng.Intn(48)
	case 1:
		span = -1 - rng.Intn(3)
	}
	q.from = integrityFuzzBase.Add(time.Duration(start)*time.Hour + time.Duration(rng.Intn(3600))*time.Second)
	q.to = q.from.Add(time.Duration(span)*time.Hour + time.Duration(rng.Intn(3600))*time.Second)
	if rng.Intn(10) == 0 {
		q.from = time.Time{}
	}
	q.windowKeys = rng.Intn(2) == 0
	q.exact = rng.Intn(2) == 0
	return q
}

func integrityErrClass(err error) string {
	if err == nil {
		return "ok"
	}
	var parts []string
	for _, sentinel := range []struct {
		name string
		err  error
	}{{"history", ErrUsageHistoryIncomplete}, {"source", ErrUsageAggregateSourceMissing}, {"index", ErrUsageClientIndexIncomplete}, {"canceled", context.Canceled}} {
		if errors.Is(err, sentinel.err) {
			parts = append(parts, sentinel.name)
		}
	}
	return strings.Join(parts, "+") + ": " + err.Error()
}

// runIntegrityQuery runs every integrity-checked read for q in one mode.
func runIntegrityQuery(db *bolt.DB, q integrityQuery, fast bool) []string {
	saved := integrityForceFast.Load()
	integrityForceFast.Store(fast)
	defer integrityForceFast.Store(saved)
	store := NewUsageStore(db)
	store.now = func() time.Time { return integrityFuzzBase.Add(48 * time.Hour) }
	from, to := q.from, q.to
	filter := usage.Filter{ClientName: q.client, Model: q.model, From: &from, To: &to}
	var out []string
	encode := func(name string, value any, err error) {
		data, _ := json.Marshal(value)
		if err != nil {
			data = nil
		}
		out = append(out, name+" "+integrityErrClass(err)+" "+string(data))
	}
	var resolver usage.RateResolver = replayCosts
	if q.exact {
		resolver = exactCostTable{replayCosts}
	}
	result, err := store.readCostUnitsInScope(context.Background(), filter, resolver, true, nil, costReadHooks{windowKeys: q.windowKeys}, q.allow())
	sort.Slice(result.units, func(i, j int) bool {
		a, _ := json.Marshal(result.units[i])
		b, _ := json.Marshal(result.units[j])
		return string(a) < string(b)
	})
	encode("cost", result.units, err)
	_ = db.View(func(tx *bolt.Tx) error {
		err := checkDirectQuotaEvidence(tx, filter, from, to, q.allow(), &directIntegrityHooks{windowKeys: q.windowKeys})
		encode("direct", nil, err)
		cells, err := quotaProofBatchInScopeContext(context.Background(), tx, filter, from, to, q.allow(), nil)
		valid := map[string]bool{}
		for k, cell := range cells {
			valid[k] = cell.valid()
		}
		encode("batch", valid, err)
		encode("fresh", nil, checkMissingFreshRoots(tx, filter, from, to))
		encode("quotakeys", nil, validateQuotaKeys(integrityReadContext(context.Background(), tx, from, to), tx, filter, from, to, q.allow()))
		return nil
	})
	sums, err := store.SumQuotaByClientRange(q.client, from, to)
	encode("sumquota", sums, err)
	cells, err := store.SumTargetModelRange(filter)
	encode("targetmodel", cells, err)
	aggregate, err := store.Aggregate(filter, "hour", replayCosts)
	encode("aggregate", aggregate, err)
	rawFrom := from
	detail, err := store.readCostUnitsInScope(context.Background(), filter, resolver, false, &rawFrom, costReadHooks{}, q.allow())
	sort.Slice(detail.raw, func(i, j int) bool { return detail.raw[i].RecordID < detail.raw[j].RecordID })
	sort.Slice(detail.units, func(i, j int) bool {
		a, _ := json.Marshal(detail.units[i])
		b, _ := json.Marshal(detail.units[j])
		return string(a) < string(b)
	})
	encode("detail", detail, err)
	summary, err := store.Summary(to, replayCosts)
	summary.GeneratedAt = time.Time{}
	encode("summary", summary, err)
	return out
}

func compareIntegrityModes(t *testing.T, f *integrityFuzzDB, q integrityQuery) bool {
	t.Helper()
	full := runIntegrityQuery(f.db, q, false)
	fast := runIntegrityQuery(f.db, q, true)
	for i := range full {
		if full[i] == fast[i] {
			continue
		}
		// Some exhaustive checks report the first failing cell of a Go map,
		// so Full alone can pick different messages from run to run. Accept
		// Fast's verdict when Full itself produces it.
		matched := false
		for retry := 0; retry < 20 && !matched; retry++ {
			matched = runIntegrityQuery(f.db, q, false)[i] == fast[i]
		}
		if !matched {
			t.Errorf("Fast != Full for %s\n full: %s\n fast: %s\n damage: %v", q, full[i], fast[i], f.log)
			return false
		}
	}
	return true
}

func TestIntegrityFastEqualsFullFuzz(t *testing.T) {
	iterations := integrityFuzzIterations(40)
	usedFast, legacyRows := 0, 0
	for seed := 0; seed < iterations; seed++ {
		rng := rand.New(rand.NewSource(int64(seed)))
		f := buildIntegrityFuzzDB(t, rng)
		legacyRows += f.legacy
		legacyProblem := ""
		_ = f.db.View(func(tx *bolt.Tx) error {
			class, _ := classifyUsageIntegrity(context.Background(), tx)
			if class == nil {
				return nil // overflow or cancellation: the fast path is unused
			}
			usedFast++
			raw := tx.Bucket([]byte(BucketUsageEvents))
			if raw == nil {
				return nil
			}
			for _, key := range f.legacyExempt {
				value := raw.Get(key)
				if value == nil {
					continue // deleted or overwritten by later damage
				}
				var event usage.Event
				if json.Unmarshal(value, &event) != nil || event.RecordID != "" {
					continue // later damage changed the shape
				}
				at, ok := canonicalEventKeyStamp(key)
				if !ok || !at.Equal(event.Timestamp) {
					continue // later damage moved the event or the key
				}
				if class.eventSet[string(key)] {
					legacyProblem = fmt.Sprintf("seed %d: legacy row %q lost the P1.1 window exemption; damage: %v", seed, key, f.log)
					return nil
				}
			}
			return nil
		})
		if legacyProblem != "" {
			t.Fatal(legacyProblem)
		}
		for i := 0; i < 12; i++ {
			if !compareIntegrityModes(t, f, randomIntegrityQuery(rng)) {
				t.Fatalf("seed %d", seed)
			}
		}
	}
	if usedFast < iterations*9/10 {
		t.Fatalf("classification available on %d/%d databases; the fuzz would not exercise the fast path", usedFast, iterations)
	}
	if legacyRows == 0 {
		if iterations >= 10 {
			t.Fatalf("no legacy rows without record_id were generated; the P1.1 shape is not exercised")
		}
		t.Logf("coverage note: no legacy rows without record_id generated at %d iterations; the P1.1 shape was not exercised", iterations)
	}
	t.Logf("legacy rows without record_id generated: %d", legacyRows)
}

// integrityMutateOutside changes or deletes objects whose key hour is outside
// [from, to] and that are not anomalous; it reports how many were changed.
func integrityMutateOutside(t *testing.T, rng *rand.Rand, db *bolt.DB, class *integrityClassification, from, to time.Time) int {
	t.Helper()
	from, to = from.UTC().Truncate(time.Hour), to.UTC().Truncate(time.Hour)
	outside := func(hour time.Time) bool { return hour.Before(from) || hour.After(to) }
	inSet := func(set [][]byte, key []byte) bool {
		for _, k := range set {
			if bytes.Equal(k, key) {
				return true
			}
		}
		return false
	}
	type candidate struct {
		bucket string
		key    []byte
	}
	changed := 0
	_ = db.Update(func(tx *bolt.Tx) error {
		var candidates []candidate
		for _, name := range []string{BucketUsageEvents, rawOwnerBucket, BucketUsageReceipts, freshAttestationBucket, receiptOwnersBucket, indexProofsV2} {
			for _, k := range fuzzKeys(tx, name) {
				if hour, ok := canonicalEventKeyHour(k); ok && outside(hour) && !class.eventSet[string(k)] && tx.Bucket([]byte(name)).Get(k) != nil {
					candidates = append(candidates, candidate{name, k})
				}
			}
		}
		for _, k := range fuzzKeys(tx, BucketQuotaUsageAggHourly) {
			if hour, ok := quotaKeyHour(k); ok && outside(hour) && !inSet(class.quota, k) {
				candidates = append(candidates, candidate{BucketQuotaUsageAggHourly, k})
			}
		}
		for _, k := range fuzzKeys(tx, rollupProofBucket) {
			if _, key, found := bytes.Cut(k, []byte{0}); found {
				if hour, ok := quotaKeyHour(key); ok && outside(hour) && !inSet(class.proofs, k) {
					candidates = append(candidates, candidate{rollupProofBucket, k})
				}
			}
		}
		for _, k := range fuzzKeys(tx, BucketUsageEventsByClient) {
			parts := strings.SplitN(string(k), "|", 3)
			if len(parts) != 3 {
				continue
			}
			if scope, ok := indexProofScope(k, []byte(parts[2])); ok && outside(scope.Timestamp.UTC().Truncate(time.Hour)) && !inSet(class.index, k) {
				candidates = append(candidates, candidate{BucketUsageEventsByClient, k})
			}
		}
		if len(candidates) == 0 {
			return nil
		}
		for n := 1 + rng.Intn(2); n > 0; n-- {
			c := candidates[rng.Intn(len(candidates))]
			b := tx.Bucket([]byte(c.bucket))
			switch rng.Intn(3) {
			case 0:
				_ = b.Delete(c.key)
			case 1:
				junk := make([]byte, rng.Intn(40))
				rng.Read(junk)
				_ = b.Put(c.key, junk)
			case 2:
				_ = b.Put(c.key, []byte(`{"input_tokens":1,"requests":1,"record_count":1}`))
			}
			changed++
		}
		return nil
	})
	return changed
}

func sameIntegrityAnomalies(a, b *integrityClassification) bool {
	if a == nil || b == nil || len(a.events) != len(b.events) || len(a.quota) != len(b.quota) || len(a.proofs) != len(b.proofs) || len(a.index) != len(b.index) || a.indexClassified != b.indexClassified {
		return false
	}
	for _, pair := range [][2][][]byte{{a.events, b.events}, {a.quota, b.quota}, {a.proofs, b.proofs}, {a.index, b.index}} {
		for i := range pair[0] {
			if !bytes.Equal(pair[0][i], pair[1][i]) {
				return false
			}
		}
	}
	return true
}

// TestIntegrityFullIgnoresOutsideNonAnomalous checks the definition of A
// directly: changing or deleting objects outside the window that are not in A
// leaves the exhaustive verdict unchanged, whenever the change keeps A itself
// unchanged (a change that creates a new anomaly is a maintenance write, which
// bumps the epoch instead).
func TestIntegrityFullIgnoresOutsideNonAnomalous(t *testing.T) {
	iterations := integrityFuzzIterations(40)
	checked, reclassified, notSkippable := 0, 0, 0
	for seed := 0; seed < iterations; seed++ {
		rng := rand.New(rand.NewSource(int64(1_000_000 + seed)))
		for i := 0; i < 4; i++ {
			f := buildIntegrityFuzzDB(t, rand.New(rand.NewSource(int64(seed))))
			q := randomIntegrityQuery(rng)
			var before *integrityClassification
			_ = f.db.View(func(tx *bolt.Tx) error { before, _ = classifyUsageIntegrity(context.Background(), tx); return nil })
			if before == nil {
				continue
			}
			want := runIntegrityQuery(f.db, q, false)
			if integrityMutateOutside(t, rng, f.db, before, q.from, q.to) == 0 {
				continue
			}
			f.note("mutated objects outside %s", q)
			var after *integrityClassification
			_ = f.db.View(func(tx *bolt.Tx) error { after, _ = classifyUsageIntegrity(context.Background(), tx); return nil })
			if !sameIntegrityAnomalies(before, after) {
				// The mutation created or removed anomalies, as a maintenance
				// write would (which bumps the epoch). Verify the reclassified
				// database instead: Fast under the new A must equal Full.
				if after == nil {
					notSkippable++
					continue
				}
				reclassified++
				if !compareIntegrityModes(t, f, q) {
					t.Fatalf("seed %d: reclassified Fast != Full", seed)
				}
				continue
			}
			checked++
			got := runIntegrityQuery(f.db, q, false)
			for j := range want {
				// Summary reads its own trailing 30-day window, not q's.
				if strings.HasPrefix(want[j], "summary ") {
					continue
				}
				if want[j] != got[j] {
					t.Fatalf("seed %d: Full changed after mutating non-anomalous objects outside %s\n before: %s\n after:  %s\n damage: %v", seed, q, want[j], got[j], f.log)
				}
			}
		}
	}
	t.Logf("invariance cases with A unchanged: %d; A changed and reclassified: %d; reclassification unavailable: %d", checked, reclassified, notSkippable)
	if checked < iterations || reclassified < iterations {
		t.Fatalf("coverage too low: unchanged=%d reclassified=%d (want >= %d each)", checked, reclassified, iterations)
	}
}
