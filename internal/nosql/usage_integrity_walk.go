package nosql

import (
	"bytes"
	"context"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// integrityFast is one read's window over a classification. Every walk visits
// a superset of (keys whose hour is in [from, to]) ∪ A, in key order, so a
// callback sees the same objects in the same order as the exhaustive ForEach
// minus objects whose windowExempt predicate holds and whose hour is outside
// the window. Visiting more keys than necessary is always safe.
type integrityFast struct {
	class    *integrityClassification
	from, to time.Time // UTC hours, inclusive
	lo, hi   []byte    // hour-prefix bounds; nil means unbounded
	empty    bool
	// scope replaces the window hour ranges with the filter client's own
	// objects (Issue #125 P2). It is set only after a client-scoped read has
	// enumerated the client index for the window, so every skipped object is
	// one whose callbacks are neutral for this client filter (see each walk).
	scope *integrityClientScope
}

// integrityClientScope is the union of one client's current-window objects
// with what the client index cannot enumerate for that client:
//
//   - rawKeys: the raw-identity keys the index returned for the filter client
//     and for the empty client in the window hours. Empty-client rows have an
//     unknown quota owner, which fails every client-filtered read, so they
//     always take part.
//   - quotaRanges: per window hour, the quota aggregation range of the filter
//     client and the range of empty-owner keys. Empty-owner keys are neutral
//     outside the window but error inside it for every client filter.
//
// The classification's anomalous keys are added by each walk; they are the
// objects whose key hour cannot prove them outside any window.
type integrityClientScope struct {
	client      string
	rawKeys     [][]byte
	quotaRanges []keyRange
}

// withIntegrityClientScope attaches scope to the classification window of tx.
func withIntegrityClientScope(ctx context.Context, tx *bolt.Tx, scope *integrityClientScope) context.Context {
	value, ok := ctx.Value(integrityFastKey{}).(integrityFastValue)
	if !ok || value.tx != tx || value.fast == nil {
		return ctx
	}
	next := *value.fast
	next.scope = scope
	return context.WithValue(ctx, integrityFastKey{}, integrityFastValue{tx: tx, fast: &next})
}

// mergeSorted returns the union of two sorted key slices, in key order.
func mergeSorted(a, b [][]byte) [][]byte {
	out := make([][]byte, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i == len(a):
			out = append(out, b[j])
			j++
		case j == len(b):
			out = append(out, a[i])
			i++
		case bytes.Compare(a[i], b[j]) < 0:
			out = append(out, a[i])
			i++
		case bytes.Compare(a[i], b[j]) > 0:
			out = append(out, b[j])
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}

type integrityFastKey struct{}

type integrityFastValue struct {
	tx   *bolt.Tx
	fast *integrityFast
}

func newIntegrityFast(class *integrityClassification, from, to time.Time) *integrityFast {
	f := &integrityFast{class: class, from: from.UTC().Truncate(time.Hour), to: to.UTC().Truncate(time.Hour)}
	if f.to.Before(f.from) {
		f.empty = true
		return f
	}
	// Four-digit years keep hourLayout text in time order; anything else is
	// unbounded on that side, which only widens the walk.
	if f.from.Year() >= 1 && f.from.Year() <= 9999 {
		f.lo = []byte(f.from.Format(hourLayout))
	}
	if next := f.to.Add(time.Hour); next.Year() >= 1 && next.Year() <= 9999 && f.lo != nil {
		f.hi = []byte(next.Format(hourLayout))
	}
	return f
}

// integrityFastFrom returns the window attached to ctx for tx, if any.
func integrityFastFrom(ctx context.Context, tx *bolt.Tx) *integrityFast {
	value, _ := ctx.Value(integrityFastKey{}).(integrityFastValue)
	if value.tx != tx {
		return nil
	}
	return value.fast
}

// integrityReadContext decides once per read transaction whether the fast
// path applies to [from, to], and attaches the decision to ctx. It must be
// called inside the read transaction, after it began (see integrityState).
func integrityReadContext(ctx context.Context, tx *bolt.Tx, from, to time.Time) context.Context {
	if value, ok := ctx.Value(integrityFastKey{}).(integrityFastValue); ok && value.tx == tx {
		if value.fast == nil {
			return ctx
		}
		next := newIntegrityFast(value.fast.class, from, to)
		next.scope = value.fast.scope
		return context.WithValue(ctx, integrityFastKey{}, integrityFastValue{tx: tx, fast: next})
	}
	class := integrityClassFor(tx)
	var fast *integrityFast
	if class != nil {
		fast = newIntegrityFast(class, from, to)
	}
	return context.WithValue(ctx, integrityFastKey{}, integrityFastValue{tx: tx, fast: fast})
}

type keyRange struct{ lo, hi []byte } // hi nil: unbounded

func (r keyRange) contains(key []byte) bool {
	return bytes.Compare(key, r.lo) >= 0 && (r.hi == nil || bytes.Compare(key, r.hi) < 0)
}

// walkRanges visits, in key order, every key of the sorted disjoint ranges plus
// the sorted points outside all of them. A nested bucket is visited with a nil
// value, as ForEach does.
func walkRanges(bucket *bolt.Bucket, ranges []keyRange, points [][]byte, fn func(k, v []byte) error) error {
	var outside [][]byte
	for _, point := range points {
		covered := false
		for _, r := range ranges {
			if r.contains(point) {
				covered = true
				break
			}
		}
		if !covered {
			outside = append(outside, point)
		}
	}
	cursor := bucket.Cursor()
	next := 0
	visitPoints := func(before []byte) error {
		for next < len(outside) && (before == nil || bytes.Compare(outside[next], before) < 0) {
			if k, v := cursor.Seek(outside[next]); k != nil && bytes.Equal(k, outside[next]) {
				if err := fn(k, v); err != nil {
					return err
				}
			}
			next++
		}
		return nil
	}
	for _, r := range ranges {
		if err := visitPoints(r.lo); err != nil {
			return err
		}
		for k, v := cursor.Seek(r.lo); k != nil && (r.hi == nil || bytes.Compare(k, r.hi) < 0); k, v = cursor.Next() {
			if err := fn(k, v); err != nil {
				return err
			}
		}
	}
	return visitPoints(nil)
}

func (f *integrityFast) hourRange(prefix []byte, end []byte) []keyRange {
	if f.empty {
		return nil
	}
	lo := append(bytes.Clone(prefix), f.lo...)
	hi := end
	if f.hi != nil {
		hi = append(bytes.Clone(prefix), f.hi...)
	}
	return []keyRange{{lo: lo, hi: hi}}
}

// sortKeyRanges orders ranges by lower bound for walkRanges' single cursor.
func sortKeyRanges(ranges []keyRange) []keyRange {
	sort.Slice(ranges, func(i, j int) bool { return bytes.Compare(ranges[i].lo, ranges[j].lo) < 0 })
	return ranges
}

// eachEvent walks a bucket keyed by raw identity keys.
func (f *integrityFast) eachEvent(b *bolt.Bucket, fn func(k, v []byte) error) error {
	if f.scope != nil {
		return walkRanges(b, nil, mergeSorted(f.scope.rawKeys, f.class.events), fn)
	}
	return walkRanges(b, f.hourRange(nil, nil), f.class.events, fn)
}

// eachQuota walks the quota aggregation bucket.
func (f *integrityFast) eachQuota(b *bolt.Bucket, fn func(k, v []byte) error) error {
	if f.scope != nil {
		return walkRanges(b, f.scope.quotaRanges, f.class.quota, fn)
	}
	return walkRanges(b, f.hourRange(nil, nil), f.class.quota, fn)
}

// eachQuotaWindowKeys walks the quota bucket the way the calibration read's
// final quota visit does: every key whose text lies in [lo, end], not every
// key of the window's hours. With a scope, keys of other clients are skipped
// (quotaKeyInScope answers (false, nil) for a known different owner) and the
// scope's own ranges are visited instead; an anomalous key inside the textual
// range is still visited, because the exhaustive visit runs its callbacks on
// it. Keys outside the range stay untouched even if anomalous: the
// calibration path never visits them.
func (f *integrityFast) eachQuotaWindowKeys(b *bolt.Bucket, lo, end string, fn func(k, v []byte) error) error {
	if f.scope == nil {
		c := b.Cursor()
		for k, v := c.Seek([]byte(lo)); k != nil && string(k) <= end; k, v = c.Next() {
			if err := fn(k, v); err != nil {
				return err
			}
		}
		return nil
	}
	var points [][]byte
	for _, key := range f.class.quota {
		if string(key) >= lo && string(key) <= end {
			points = append(points, key)
		}
	}
	return walkRanges(b, f.scope.quotaRanges, points, fn)
}

// eachQuotaProof walks the quota-source keys of the rollup proof bucket; prefix
// is the source name followed by NUL.
func (f *integrityFast) eachQuotaProof(b *bolt.Bucket, prefix []byte, fn func(k, v []byte) error) error {
	if f.scope != nil {
		ranges := make([]keyRange, 0, len(f.scope.quotaRanges))
		for _, r := range f.scope.quotaRanges {
			ranges = append(ranges, keyRange{lo: append(bytes.Clone(prefix), r.lo...), hi: append(bytes.Clone(prefix), r.hi...)})
		}
		return walkRanges(b, sortKeyRanges(ranges), f.class.proofs, func(k, v []byte) error {
			if !bytes.HasPrefix(k, prefix) {
				return nil
			}
			return fn(k, v)
		})
	}
	end := append(bytes.Clone(prefix[:len(prefix)-1]), 1)
	return walkRanges(b, f.hourRange(prefix, end), f.class.proofs, func(k, v []byte) error {
		if !bytes.HasPrefix(k, prefix) {
			return nil
		}
		return fn(k, v)
	})
}

// eachIndex walks the client index: per client prefix, the entries of the
// window's hours, plus anomalous entries.
func (f *integrityFast) eachIndex(index *bolt.Bucket, fn func(k, v []byte) error) error {
	if f.scope != nil {
		var ranges []keyRange
		for _, client := range []string{f.scope.client, ""} {
			hexClient := hex.EncodeToString([]byte(strings.TrimSpace(client)))
			end := []byte(hexClient + "}")
			prefix := []byte(hexClient + "|")
			ranges = append(ranges, f.hourRange(prefix, end)...)
		}
		return walkRanges(index, sortKeyRanges(ranges), f.class.index, fn)
	}
	var ranges []keyRange
	if !f.empty {
		c := index.Cursor()
		for k, _ := c.First(); k != nil; {
			i := bytes.IndexByte(k, '|')
			if i < 0 {
				k, _ = c.Next()
				continue
			}
			client := bytes.Clone(k[:i])
			end := append(bytes.Clone(client), '}')
			prefix := append(bytes.Clone(client), '|')
			ranges = append(ranges, f.hourRange(prefix, end)...)
			k, _ = c.Seek(end)
		}
	}
	return walkRanges(index, ranges, f.class.index, fn)
}
