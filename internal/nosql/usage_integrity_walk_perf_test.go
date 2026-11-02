package nosql

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// The client-scoped walks pass up to integrityAnomalyLimit points against two
// ranges per window hour. The lookup must therefore not scan all ranges for
// every point. These tests pin the equivalence with the previous range-major
// implementation and the resulting complexity.

var errWalkRangesStop = errors.New("walk stopped by test callback")

// walkRangesLinearReference is the pre-optimization walkRanges: every point is
// checked against every range until one contains it. It is the semantic oracle
// of the property test and the baseline of the comparison-count assertion and
// benchmark.
func walkRangesLinearReference(bucket *bolt.Bucket, ranges []keyRange, points [][]byte, fn func(k, v []byte) error, compares *int64) error {
	compare := func(a, b []byte) int {
		if compares != nil {
			*compares++
		}
		return bytes.Compare(a, b)
	}
	contains := func(r keyRange, key []byte) bool {
		return compare(key, r.lo) >= 0 && (r.hi == nil || compare(key, r.hi) < 0)
	}
	var outside [][]byte
	for _, point := range points {
		covered := false
		for _, r := range ranges {
			if contains(r, point) {
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
		for next < len(outside) && (before == nil || compare(outside[next], before) < 0) {
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
		for k, v := cursor.Seek(r.lo); k != nil && (r.hi == nil || compare(k, r.hi) < 0); k, v = cursor.Next() {
			if err := fn(k, v); err != nil {
				return err
			}
		}
	}
	return visitPoints(nil)
}

type walkRangesVisit struct {
	key, value string
}

// collectWalkRanges runs walk with a callback that records every visit and
// stops with errWalkRangesStop before visit stopAfter (when >= 0).
func collectWalkRanges(walk func(fn func(k, v []byte) error) error, stopAfter int) ([]walkRangesVisit, error) {
	var visits []walkRangesVisit
	err := walk(func(k, v []byte) error {
		if stopAfter >= 0 && len(visits) == stopAfter {
			return errWalkRangesStop
		}
		visits = append(visits, walkRangesVisit{key: hex.EncodeToString(k), value: hex.EncodeToString(v)})
		return nil
	})
	return visits, err
}

// compareWalkRanges asserts that walkRanges visits exactly the keys, values and
// errors of the linear reference implementation for the same input.
func compareWalkRanges(t *testing.T, label string, bucket *bolt.Bucket, ranges []keyRange, points [][]byte, stopAfter int) {
	t.Helper()
	want, wantErr := collectWalkRanges(func(fn func(k, v []byte) error) error {
		return walkRangesLinearReference(bucket, ranges, points, fn, nil)
	}, stopAfter)
	got, gotErr := collectWalkRanges(func(fn func(k, v []byte) error) error {
		return walkRanges(bucket, ranges, points, fn)
	}, stopAfter)
	if !slices.Equal(want, got) {
		t.Fatalf("%s: visits differ from linear reference\n want=%v\n  got=%v", label, want, got)
	}
	if (wantErr == nil) != (gotErr == nil) || (wantErr != nil && !errors.Is(gotErr, errWalkRangesStop)) {
		t.Fatalf("%s: errors differ: reference=%v walkRanges=%v", label, wantErr, gotErr)
	}
}

var walkRangesAlphabet = []byte("abcd")

func walkRangesRandKey(rng *rand.Rand, maxLen int) []byte {
	key := make([]byte, rng.Intn(maxLen+1))
	for i := range key {
		key[i] = walkRangesAlphabet[rng.Intn(len(walkRangesAlphabet))]
	}
	return key
}

// walkRangesRandRanges builds sorted ranges, including empty, overlapping,
// nested and unbounded (nil lo or hi) shapes.
func walkRangesRandRanges(rng *rand.Rand) []keyRange {
	n := rng.Intn(9)
	ranges := make([]keyRange, 0, n)
	for i := 0; i < n; i++ {
		lo := walkRangesRandKey(rng, 3)
		if rng.Intn(8) == 0 {
			lo = nil
		}
		var hi []byte
		switch rng.Intn(5) {
		case 0:
			hi = nil // unbounded above
		case 1:
			hi = bytes.Clone(lo) // empty range (or unbounded below when lo is nil)
		default:
			hi = walkRangesRandKey(rng, 3)
			if bytes.Compare(hi, lo) < 0 {
				lo, hi = hi, lo
			}
		}
		ranges = append(ranges, keyRange{lo: lo, hi: hi})
	}
	sort.SliceStable(ranges, func(i, j int) bool { return bytes.Compare(ranges[i].lo, ranges[j].lo) < 0 })
	return ranges
}

func walkRangesRandPoints(rng *rand.Rand) [][]byte {
	n := rng.Intn(31)
	points := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		// Length 4 keys mostly fall outside the fixture bucket, exercising the
		// "point is not a bucket key" path.
		points = append(points, walkRangesRandKey(rng, 4))
	}
	sort.Slice(points, func(i, j int) bool { return bytes.Compare(points[i], points[j]) < 0 })
	return points
}

// walkRangesUniverse is every key of length 1..3 over the alphabet plus a few
// keys outside it, so ranges and points can hit present and absent keys.
func walkRangesUniverse() [][]byte {
	var keys [][]byte
	var build func(prefix []byte, depth int)
	build = func(prefix []byte, depth int) {
		if depth > 0 {
			keys = append(keys, bytes.Clone(prefix))
		}
		if depth == 3 {
			return
		}
		for _, c := range walkRangesAlphabet {
			build(append(prefix, c), depth+1)
		}
	}
	build(nil, 0)
	return append(keys, []byte("e"), []byte("ee"), []byte("z"), []byte("zz"))
}

type walkRangesFixture struct {
	bucket *bolt.Bucket
}

// newWalkRangesFixture creates a read transaction whose walk bucket holds the
// fixed universe and extraKeys, plus one nested bucket.
func newWalkRangesFixture(tb testing.TB, extraKeys [][]byte) *walkRangesFixture {
	tb.Helper()
	db := testDB(tb)
	fixture := &walkRangesFixture{}
	err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("walk"))
		if err != nil {
			return err
		}
		seen := make(map[string]bool)
		put := func(k []byte) error {
			if seen[string(k)] {
				return nil
			}
			seen[string(k)] = true
			return b.Put(k, append([]byte("v:"), k...))
		}
		for _, k := range walkRangesUniverse() {
			if err := put(k); err != nil {
				return err
			}
		}
		for _, k := range extraKeys {
			if err := put(k); err != nil {
				return err
			}
		}
		_, err = b.CreateBucketIfNotExists([]byte("nested"))
		return err
	})
	if err != nil {
		tb.Fatalf("build walk fixture: %v", err)
	}
	tx, err := db.Begin(false)
	if err != nil {
		tb.Fatalf("begin walk fixture read transaction: %v", err)
	}
	tb.Cleanup(func() { tx.Rollback() })
	fixture.bucket = tx.Bucket([]byte("walk"))
	return fixture
}

func TestWalkRangesMatchesLinearReference(t *testing.T) {
	fixture := newWalkRangesFixture(t, nil)
	rng := rand.New(rand.NewSource(20261030))
	for i := 0; i < 300; i++ {
		ranges := walkRangesRandRanges(rng)
		points := walkRangesRandPoints(rng)
		compareWalkRanges(t, fmt.Sprintf("random case %d", i), fixture.bucket, ranges, points, -1)
	}
	for i := 0; i < 30; i++ {
		ranges := walkRangesRandRanges(rng)
		points := walkRangesRandPoints(rng)
		compareWalkRanges(t, fmt.Sprintf("early-stop case %d", i), fixture.bucket, ranges, points, i%7)
	}
}

func TestWalkRangesBoundaries(t *testing.T) {
	points := [][]byte{[]byte("a"), []byte("ab"), []byte("b"), []byte("b"), []byte("c")}
	cases := []struct {
		name   string
		ranges []keyRange
		points [][]byte
	}{
		{"no ranges", nil, points},
		{"no points", []keyRange{{lo: []byte("a"), hi: []byte("c")}}, nil},
		{"both empty", nil, nil},
		{"point on bounds", []keyRange{{lo: []byte("ab"), hi: []byte("b")}}, points},
		{"empty range", []keyRange{{lo: []byte("b"), hi: []byte("b")}}, points},
		{"unbounded high", []keyRange{{lo: []byte("b"), hi: nil}}, points},
		{"nil lo", []keyRange{{lo: nil, hi: []byte("b")}}, points},
		{"fully unbounded", []keyRange{{lo: nil, hi: nil}}, points},
		{"duplicate points outside", nil, [][]byte{[]byte("a"), []byte("a"), []byte("z")}},
		{"overlapping ranges", []keyRange{{lo: []byte("a"), hi: []byte("c")}, {lo: []byte("ab"), hi: []byte("d")}}, points},
		{"nested ranges", []keyRange{{lo: []byte("a"), hi: []byte("d")}, {lo: []byte("ab"), hi: []byte("b")}}, points},
		{"adjacent ranges", []keyRange{{lo: []byte("a"), hi: []byte("ab")}, {lo: []byte("ab"), hi: []byte("b")}}, points},
	}
	fixture := newWalkRangesFixture(t, nil)
	for _, tc := range cases {
		ranges := slices.Clone(tc.ranges)
		sort.SliceStable(ranges, func(i, j int) bool { return bytes.Compare(ranges[i].lo, ranges[j].lo) < 0 })
		sortedPoints := slices.Clone(tc.points)
		sort.Slice(sortedPoints, func(i, j int) bool { return bytes.Compare(sortedPoints[i], sortedPoints[j]) < 0 })
		compareWalkRanges(t, tc.name, fixture.bucket, ranges, sortedPoints, -1)
		compareWalkRanges(t, tc.name+" (early stop)", fixture.bucket, ranges, sortedPoints, 2)
	}
}

// walkRangesScopedFixture builds the shape a client-scoped month read walks:
// two quota ranges per window hour (client and empty owner) and up to
// integrityAnomalyLimit anomalous points. The points sort after every range, so
// the former nested filter had to scan all ranges for each point.
func walkRangesScopedFixture(tb testing.TB, hours, pointCount int) (*bolt.Bucket, []keyRange, [][]byte) {
	tb.Helper()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	owner := quotaKeyEncode("client-00")
	ranges := make([]keyRange, 0, 2*hours)
	for h := 0; h < hours; h++ {
		hour := start.Add(time.Duration(h) * time.Hour)
		ranges = append(ranges,
			quotaOwnerHourRange(hour, owner),
			quotaOwnerHourRange(hour, ""),
		)
	}
	ranges = sortKeyRanges(ranges)
	points := make([][]byte, 0, pointCount)
	for i := 0; i < pointCount; i++ {
		points = append(points, []byte(fmt.Sprintf("zz%010d", i)))
	}
	fixture := newWalkRangesFixture(tb, points)
	return fixture.bucket, ranges, points
}

func TestWalkRangesLookupComplexity(t *testing.T) {
	const (
		hours       = 720 // one month: two ranges per hour
		pointCount  = 10000
		maxCompares = 200000
	)
	bucket, ranges, points := walkRangesScopedFixture(t, hours, pointCount)
	if len(ranges) != 2*hours {
		t.Fatalf("ranges=%d, want %d", len(ranges), 2*hours)
	}

	savedTracing := walkRangeCompareTracing.Swap(true)
	savedCount := walkRangeCompares.Swap(0)
	t.Cleanup(func() {
		walkRangeCompares.Store(savedCount)
		walkRangeCompareTracing.Store(savedTracing)
	})
	visits := 0
	if err := walkRanges(bucket, ranges, points, func(k, v []byte) error {
		visits++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	newCompares := walkRangeCompares.Load()

	var referenceCompares int64
	referenceVisits := 0
	if err := walkRangesLinearReference(bucket, ranges, points, func(k, v []byte) error {
		referenceVisits++
		return nil
	}, &referenceCompares); err != nil {
		t.Fatal(err)
	}
	if visits != pointCount || referenceVisits != pointCount {
		t.Fatalf("visits walkRanges=%d reference=%d, want %d", visits, referenceVisits, pointCount)
	}
	if newCompares >= maxCompares {
		t.Fatalf("walkRanges made %d lookup comparisons, want < %d", newCompares, maxCompares)
	}
	if referenceCompares < 10_000_000 {
		t.Fatalf("linear reference made only %d comparisons; the fixture no longer exercises the old worst case", referenceCompares)
	}
	t.Logf("points=%d ranges=%d | linear reference comparisons=%d | walkRanges lookup comparisons=%d",
		pointCount, len(ranges), referenceCompares, newCompares)
}

func BenchmarkWalkRangesScopedLookup(b *testing.B) {
	bucket, ranges, points := walkRangesScopedFixture(b, 720, 10000)
	noop := func(k, v []byte) error { return nil }
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := walkRanges(bucket, ranges, points, noop); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWalkRangesScopedLinearReference(b *testing.B) {
	bucket, ranges, points := walkRangesScopedFixture(b, 720, 10000)
	noop := func(k, v []byte) error { return nil }
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := walkRangesLinearReference(bucket, ranges, points, noop, nil); err != nil {
			b.Fatal(err)
		}
	}
}
