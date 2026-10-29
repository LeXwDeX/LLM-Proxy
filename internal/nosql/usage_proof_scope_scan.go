package nosql

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// A write batch proves many client/hour scopes in one transaction, and each
// proof used to walk every raw row and receipt. The scope summary is built
// once per transaction (a single cheap pass) and lists the only rows that can
// matter outside the hours being proven: raw rows that are not canonical and
// receipts whose key stamp is not canonical UTC. Everything else is reachable
// by key prefix, because a canonical key starts with its own UTC hour. The
// scoped walk therefore visits a superset of what the exhaustive walk acted on
// (the per-row skips stay in the callbacks), in key order.
type txScopeSummaryKey struct{}

type txScopeSummary struct {
	tx          *bolt.Tx
	rawBuilt    bool
	rawOdd      [][]byte
	receiptsOdd [][]byte
	receiptsOK  bool
}

// withTxScopeSummary is valid only while tx performs no writes that could add
// non-canonical raw rows or receipts, as in the pre-write proof phase.
func withTxScopeSummary(ctx context.Context, tx *bolt.Tx) context.Context {
	return context.WithValue(ctx, txScopeSummaryKey{}, &txScopeSummary{tx: tx})
}

func txScopeSummaryFrom(ctx context.Context, tx *bolt.Tx) *txScopeSummary {
	summary, _ := ctx.Value(txScopeSummaryKey{}).(*txScopeSummary)
	if summary == nil || summary.tx != tx {
		return nil
	}
	return summary
}

func canonicalReceiptStamp(key []byte) bool {
	stamp, _, found := strings.Cut(string(key), "_")
	at, err := time.Parse(time.RFC3339Nano, stamp)
	return found && err == nil && at.UTC().Format(time.RFC3339Nano) == stamp
}

func (s *txScopeSummary) rawOddKeys(raw *bolt.Bucket) [][]byte {
	if !s.rawBuilt {
		_ = raw.ForEach(func(k, v []byte) error {
			if !canonicalRawRowCached(k, v) {
				s.rawOdd = append(s.rawOdd, append([]byte(nil), k...))
			}
			return nil
		})
		s.rawBuilt = true
	}
	return s.rawOdd
}

func (s *txScopeSummary) receiptOddKeys(receipts *bolt.Bucket) [][]byte {
	if !s.receiptsOK {
		_ = receipts.ForEach(func(k, _ []byte) error {
			if !canonicalReceiptStamp(k) {
				s.receiptsOdd = append(s.receiptsOdd, append([]byte(nil), k...))
			}
			return nil
		})
		s.receiptsOK = true
	}
	return s.receiptsOdd
}

func hourPrefixes(hours map[time.Time]bool) []string {
	prefixes := make([]string, 0, len(hours))
	for hour := range hours {
		prefixes = append(prefixes, hour.UTC().Format("2006-01-02T15:"))
	}
	sort.Strings(prefixes)
	return prefixes
}

// walkScoped visits, in key order, every key under one of prefixes plus the
// odd keys that are under none.
func walkScoped(bucket *bolt.Bucket, prefixes []string, odd [][]byte, fn func(k, v []byte) error) error {
	var points [][]byte
	for _, key := range odd {
		covered := false
		for _, prefix := range prefixes {
			if bytes.HasPrefix(key, []byte(prefix)) {
				covered = true
				break
			}
		}
		if !covered {
			points = append(points, key)
		}
	}
	next := 0
	cursor := bucket.Cursor()
	// A nested bucket is a key with a nil value, and ForEach hands it to the
	// callback as such; Get would hide it, so existence is decided by a seek.
	visitPoints := func(before string, bounded bool) error {
		for next < len(points) && (!bounded || string(points[next]) < before) {
			if k, v := cursor.Seek(points[next]); k != nil && bytes.Equal(k, points[next]) {
				if err := fn(k, v); err != nil {
					return err
				}
			}
			next++
		}
		return nil
	}
	for _, prefix := range prefixes {
		if err := visitPoints(prefix, true); err != nil {
			return err
		}
		for k, v := cursor.Seek([]byte(prefix)); k != nil && bytes.HasPrefix(k, []byte(prefix)); k, v = cursor.Next() {
			if err := fn(k, v); err != nil {
				return err
			}
		}
	}
	return visitPoints("", false)
}

// forEachRawInHours is raw.ForEach restricted, when ctx carries a transaction
// scope summary, to the rows that can matter for hours. The callback keeps its
// own per-row skips, so both forms reach the same verdict.
func forEachRawInHours(ctx context.Context, tx *bolt.Tx, raw *bolt.Bucket, hours map[time.Time]bool, fn func(k, v []byte) error) error {
	summary := txScopeSummaryFrom(ctx, tx)
	if summary == nil || !proofScopeSkipEnabled {
		return raw.ForEach(fn)
	}
	return walkScoped(raw, hourPrefixes(hours), summary.rawOddKeys(raw), fn)
}

func forEachReceiptInHours(ctx context.Context, tx *bolt.Tx, receipts *bolt.Bucket, hours map[time.Time]bool, fn func(k, v []byte) error) error {
	summary := txScopeSummaryFrom(ctx, tx)
	if summary == nil || !proofScopeSkipEnabled {
		return receipts.ForEach(fn)
	}
	return walkScoped(receipts, hourPrefixes(hours), summary.receiptOddKeys(receipts), fn)
}
