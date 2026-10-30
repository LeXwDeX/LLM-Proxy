package nosql

import (
	"bytes"
	"context"
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
		return context.WithValue(ctx, integrityFastKey{}, integrityFastValue{tx: tx, fast: newIntegrityFast(value.fast.class, from, to)})
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

// eachEvent walks a bucket keyed by raw identity keys.
func (f *integrityFast) eachEvent(b *bolt.Bucket, fn func(k, v []byte) error) error {
	return walkRanges(b, f.hourRange(nil, nil), f.class.events, fn)
}

// eachQuota walks the quota aggregation bucket.
func (f *integrityFast) eachQuota(b *bolt.Bucket, fn func(k, v []byte) error) error {
	return walkRanges(b, f.hourRange(nil, nil), f.class.quota, fn)
}

// eachQuotaProof walks the quota-source keys of the rollup proof bucket; prefix
// is the source name followed by NUL.
func (f *integrityFast) eachQuotaProof(b *bolt.Bucket, prefix []byte, fn func(k, v []byte) error) error {
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
