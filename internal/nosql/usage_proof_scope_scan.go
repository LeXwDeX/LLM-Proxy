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
// once per transaction and lists the only rows that can matter outside the
// hours being proven: raw rows that are not canonical and receipts whose key
// stamp is not canonical UTC. Everything else is reachable by key prefix,
// because a canonical key starts with its own UTC hour. The scoped walk
// therefore visits a superset of what the exhaustive walk acted on (the
// per-row skips stay in the callbacks), in key order.
//
// Issue #114: the odd set comes from the persisted classification
// (usage_proof_odd_index.go), located by key-stamp hour, so loading it is
// O(odd rows of the proved hours), not O(history). If the index is absent,
// incomplete, or an entry no longer matches its object, the summary falls back
// to the previous full classification scan for the rest of the transaction.
type txScopeSummaryKey struct{}

type txScopeSummary struct {
	tx *bolt.Tx

	rawChecked  bool
	rawFallback bool
	rawGlobal   [][]byte
	rawHours    map[string][][]byte
	rawBuilt    bool
	rawOdd      [][]byte

	receiptsChecked  bool
	receiptsFallback bool
	receiptsGlobal   [][]byte
	receiptsHours    map[string][][]byte
	receiptsOK       bool
	receiptsOdd      [][]byte
}

// withTxScopeSummary is valid only while tx performs no writes that could add
// non-canonical raw rows or receipts. The write paths that attach it only add
// canonical rows, and retention (which deletes rows and writes receipts) does
// not use it.
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

func (s *txScopeSummary) fallbackRaw(raw *bolt.Bucket) [][]byte {
	if !s.rawBuilt {
		s.rawOdd = s.rawOdd[:0]
		_ = raw.ForEach(func(k, v []byte) error {
			proofOddFullClassifications.Add(1)
			if !canonicalRawRowCached(k, v) {
				s.rawOdd = append(s.rawOdd, append([]byte(nil), k...))
			}
			return nil
		})
		s.rawBuilt = true
	}
	return s.rawOdd
}

func (s *txScopeSummary) rawOddKeys(raw *bolt.Bucket, hours map[time.Time]bool) [][]byte {
	if !s.rawFallback {
		if !s.rawChecked {
			s.rawChecked = true
			if proofOddIndexReady(s.tx) {
				if keys, ok := proofOddLoadGlobal(s.tx, proofOddRawGlobal, raw); ok {
					s.rawGlobal = keys
					s.rawHours = make(map[string][][]byte)
				} else {
					s.rawFallback = true
				}
			} else {
				s.rawFallback = true
			}
		}
		if !s.rawFallback {
			keys := append([][]byte(nil), s.rawGlobal...)
			for hour := range hours {
				hourKey := hour.UTC().Format(hourLayout)
				list, ok := s.rawHours[hourKey]
				if !ok {
					list, ok = proofOddLoadHour(s.tx, proofOddRawHour, hourKey, raw)
					if !ok {
						s.rawFallback = true
						break
					}
					s.rawHours[hourKey] = list
				}
				keys = append(keys, list...)
			}
			if !s.rawFallback {
				return keys
			}
		}
	}
	return s.fallbackRaw(raw)
}

func (s *txScopeSummary) fallbackReceipts(receipts *bolt.Bucket) [][]byte {
	if !s.receiptsOK {
		s.receiptsOdd = s.receiptsOdd[:0]
		_ = receipts.ForEach(func(k, _ []byte) error {
			proofOddFullClassifications.Add(1)
			if !canonicalReceiptStamp(k) {
				s.receiptsOdd = append(s.receiptsOdd, append([]byte(nil), k...))
			}
			return nil
		})
		s.receiptsOK = true
	}
	return s.receiptsOdd
}

func (s *txScopeSummary) receiptOddKeys(receipts *bolt.Bucket, hours map[time.Time]bool) [][]byte {
	if !s.receiptsFallback {
		if !s.receiptsChecked {
			s.receiptsChecked = true
			if proofOddIndexReady(s.tx) {
				if keys, ok := proofOddLoadGlobal(s.tx, proofOddReceiptGlobal, receipts); ok {
					s.receiptsGlobal = keys
					s.receiptsHours = make(map[string][][]byte)
				} else {
					s.receiptsFallback = true
				}
			} else {
				s.receiptsFallback = true
			}
		}
		if !s.receiptsFallback {
			keys := append([][]byte(nil), s.receiptsGlobal...)
			for hour := range hours {
				hourKey := hour.UTC().Format(hourLayout)
				list, ok := s.receiptsHours[hourKey]
				if !ok {
					list, ok = proofOddLoadHour(s.tx, proofOddReceiptHour, hourKey, receipts)
					if !ok {
						s.receiptsFallback = true
						break
					}
					s.receiptsHours[hourKey] = list
				}
				keys = append(keys, list...)
			}
			if !s.receiptsFallback {
				return keys
			}
		}
	}
	return s.fallbackReceipts(receipts)
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
	return walkScoped(raw, hourPrefixes(hours), summary.rawOddKeys(raw, hours), fn)
}

func forEachReceiptInHours(ctx context.Context, tx *bolt.Tx, receipts *bolt.Bucket, hours map[time.Time]bool, fn func(k, v []byte) error) error {
	summary := txScopeSummaryFrom(ctx, tx)
	if summary == nil || !proofScopeSkipEnabled {
		return receipts.ForEach(fn)
	}
	return walkScoped(receipts, hourPrefixes(hours), summary.receiptOddKeys(receipts, hours), fn)
}
