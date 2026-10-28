package nosql

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

const (
	indexProgressV2 = "usage_client_index_progress_v2"
	indexRequiredV2 = "usage_client_index_required_v2"
	indexProofsV2   = "usage_client_index_proofs_v2"
)

type indexProgress struct {
	Version    int    `json:"version"`
	Generation string `json:"generation"`
	Phase      string `json:"phase"`
	Through    []byte `json:"through"`
	Upper      []byte `json:"upper"`
	Binding    string `json:"binding"`
}

type indexProof struct {
	Version    int    `json:"version"`
	Generation string `json:"generation"`
	Origin     string `json:"origin"`
	IndexKey   []byte `json:"index_key"`
	RawDigest  string `json:"raw_digest"`
	Binding    string `json:"binding"`
}

func indexV2Hash(domain string, parts ...[]byte) string {
	h := sha256.New()
	h.Write([]byte(domain))
	var size [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(size[:], uint64(len(p)))
		h.Write(size[:])
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (p indexProgress) binding() string {
	return indexV2Hash("usage-client-index-progress-v2\x00", []byte(p.Generation), []byte(p.Phase), p.Through, p.Upper)
}

func (p indexProof) binding(key []byte) string {
	return indexV2Hash("usage-client-index-proof-v2\x00", key, []byte(p.Generation), []byte(p.Origin), p.IndexKey, []byte(p.RawDigest))
}

func indexV2Error(message string) error {
	return fmt.Errorf("%w: %s", ErrUsageClientIndexIncomplete, message)
}

// Missing all three anchors is indistinguishable from a never-initialized v2.
func loadIndexProgress(tx *bolt.Tx) (*indexProgress, error) {
	return loadIndexProgressState(tx, true)
}

func loadIndexProgressState(tx *bolt.Tx, checkV1 bool) (*indexProgress, error) {
	meta := tx.Bucket([]byte(BucketMeta))
	if meta == nil {
		return nil, indexV2Error("missing metadata")
	}
	if checkV1 && (meta.Get([]byte(usageClientIndexSourceMissing)) != nil || meta.Bucket([]byte(usageClientIndexSourceMissing)) != nil) {
		return nil, indexV2Error("v1 index source was lost")
	}
	data, required := meta.Get([]byte(indexProgressV2)), meta.Get([]byte(indexRequiredV2))
	proofs := tx.Bucket([]byte(indexProofsV2))
	if data == nil && required == nil && proofs == nil && meta.Bucket([]byte(indexProgressV2)) == nil && meta.Bucket([]byte(indexRequiredV2)) == nil {
		return nil, nil
	}
	if data == nil || string(required) != "2" || proofs == nil || meta.Bucket([]byte(indexProgressV2)) != nil || meta.Bucket([]byte(indexRequiredV2)) != nil {
		return nil, indexV2Error("v2 source missing")
	}
	var p indexProgress
	if json.Unmarshal(data, &p) != nil || p.Version != 2 || len(p.Generation) != 32 || (p.Phase != "scan" && p.Phase != "complete") || p.Binding != p.binding() || (len(p.Through) > 0 && (len(p.Upper) == 0 || bytes.Compare(p.Through, p.Upper) > 0)) {
		return nil, indexV2Error("invalid v2 progress")
	}
	generation, err := hex.DecodeString(p.Generation)
	canonical, marshalErr := json.Marshal(p)
	if err != nil || len(generation) != 16 || hex.EncodeToString(generation) != p.Generation || marshalErr != nil || !bytes.Equal(canonical, data) {
		return nil, indexV2Error("noncanonical v2 progress")
	}
	cursor, done := meta.Get([]byte(usageClientIndexCursor)), meta.Get([]byte(usageClientIndexDone))
	if checkV1 && ((p.Phase == "scan" && (done != nil || !bytes.Equal(cursor, p.Through))) || (p.Phase == "complete" && (done == nil || cursor != nil))) {
		return nil, indexV2Error("v1 and v2 progress disagree")
	}
	if len(p.Through) != 0 {
		if _, err := readIndexProof(proofs, &p, p.Through); err != nil {
			return nil, err
		}
	}
	return &p, nil
}

func saveIndexProgress(tx *bolt.Tx, p *indexProgress) error {
	p.Binding = p.binding()
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return tx.Bucket([]byte(BucketMeta)).Put([]byte(indexProgressV2), data)
}

// Existing v1 index rows are only safe at the starting point when Record's
// independent owner proves them; historical v1 partial progress is ambiguous.
func startIndexProgress(ctx context.Context, tx *bolt.Tx) (*indexProgress, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	meta := tx.Bucket([]byte(BucketMeta))
	if meta.Get([]byte(usageClientIndexCursor)) != nil || meta.Get([]byte(usageClientIndexDone)) != nil || meta.Get([]byte(usageClientIndexSourceMissing)) != nil {
		return nil, indexV2Error("unproven v1 starting state")
	}
	index := tx.Bucket([]byte(BucketUsageEventsByClient))
	raw := tx.Bucket([]byte(BucketUsageEvents))
	type hourState struct {
		count  uint64
		digest [sha256.Size]byte
	}
	hours := make(map[string]hourState)
	if err := index.ForEach(func(k, key []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		data := raw.Get(key)
		var event usage.Event
		owner, found, err := readRawOwner(tx, key)
		if err != nil || !found || data == nil || json.Unmarshal(data, &event) != nil || !bytes.Equal(k, clientEventIndexKey(event, key)) {
			return indexV2Error("unproven existing v1 index")
		}
		digest, err := usage.RetryEquivalentDigest(event)
		if err != nil || owner.Digest != digest || owner.Client != receiptClientHash(event.ClientName) {
			return indexV2Error("existing raw owner mismatch")
		}
		hour := string(clientHourKey(event.ClientName, event.Timestamp))
		state := hours[hour]
		state.count++
		xorDigest(&state.digest, rawDigest(key, data))
		hours[hour] = state
		return nil
	}); err != nil {
		return nil, err
	}
	if err := tx.Bucket([]byte(BucketUsageClientHours)).ForEach(func(k, v []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var state usageClientHour
		if v == nil || json.Unmarshal(v, &state) != nil || state.Count != hours[string(k)].count || state.Digest != hours[string(k)].digest {
			return indexV2Error("unproven existing v1 hour")
		}
		delete(hours, string(k))
		return nil
	}); err != nil {
		return nil, err
	}
	if len(hours) != 0 {
		return nil, indexV2Error("missing existing v1 hour")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var generation [16]byte
	if _, err := rand.Read(generation[:]); err != nil {
		return nil, err
	}
	upper, _ := raw.Cursor().Last()
	p := &indexProgress{Version: 2, Generation: hex.EncodeToString(generation[:]), Phase: "scan", Upper: bytes.Clone(upper)}
	if _, err := tx.CreateBucket([]byte(indexProofsV2)); err != nil {
		return nil, err
	}
	if err := meta.Put([]byte(indexRequiredV2), []byte("2")); err != nil {
		return nil, err
	}
	if err := saveIndexProgress(tx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func writeIndexProof(tx *bolt.Tx, p *indexProgress, key, data []byte, event usage.Event, origin string) error {
	b := tx.Bucket([]byte(indexProofsV2))
	digest := rawDigest(key, data)
	proof := indexProof{Version: 2, Generation: p.Generation, Origin: origin, IndexKey: clientEventIndexKey(event, key), RawDigest: hex.EncodeToString(digest[:])}
	proof.Binding = proof.binding(key)
	if old := b.Get(key); old != nil {
		existing, err := readIndexProof(b, p, key)
		if err != nil || existing.RawDigest != proof.RawDigest || !bytes.Equal(existing.IndexKey, proof.IndexKey) {
			return indexV2Error("invalid existing index proof")
		}
		return nil
	}
	encoded, err := json.Marshal(proof)
	if err != nil {
		return err
	}
	return b.Put(key, encoded)
}

func readIndexProof(b *bolt.Bucket, p *indexProgress, key []byte) (indexProof, error) {
	var proof indexProof
	data := b.Get(key)
	if data == nil || json.Unmarshal(data, &proof) != nil || proof.Version != 2 || proof.Generation != p.Generation || (proof.Origin != "scan" && proof.Origin != "atomic") || len(proof.IndexKey) == 0 || proof.Binding != proof.binding(key) {
		return proof, indexV2Error("invalid index proof")
	}
	digest, err := hex.DecodeString(proof.RawDigest)
	canonical, marshalErr := json.Marshal(proof)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != proof.RawDigest || marshalErr != nil || !bytes.Equal(data, canonical) {
		return proof, indexV2Error("noncanonical index proof")
	}
	return proof, nil
}

type indexRawReferences struct {
	tx   *bolt.Tx
	keys map[string]bool
}

func indexProofScope(indexKey, rawKey []byte) (unresolvedScope, bool) {
	prefix, rest, ok := strings.Cut(string(indexKey), "|")
	stamp, suffix, found := strings.Cut(rest, "|")
	owner, decodeErr := hex.DecodeString(prefix)
	at, timeErr := time.Parse(clientIndexTimeLayout, stamp)
	if !ok || !found || decodeErr != nil || !utf8.Valid(owner) || hex.EncodeToString(owner) != prefix || suffix != string(rawKey) || timeErr != nil || at.Format(clientIndexTimeLayout) != stamp || strings.TrimSpace(string(owner)) != string(owner) {
		return unresolvedScope{}, false
	}
	return unresolvedScope{ClientName: string(owner), Timestamp: at}, true
}

func (refs *indexRawReferences) checkOrphans(ctx context.Context, p *indexProgress, filter usage.Filter, from, to time.Time, allow costPairFilter) error {
	if p == nil {
		return nil
	}
	index, raw := refs.tx.Bucket([]byte(BucketUsageEventsByClient)), refs.tx.Bucket([]byte(BucketUsageEvents))
	if index == nil || raw == nil {
		return fmt.Errorf("%w: client index or raw source missing", ErrUsageHistoryIncomplete)
	}
	keys := make(map[string]bool)
	err := index.ForEach(func(indexKey, pointer []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		parts := strings.SplitN(string(indexKey), "|", 3)
		if len(parts) != 3 || parts[2] == "" {
			return fmt.Errorf("%w: invalid client index identity", ErrUsageHistoryIncomplete)
		}
		key := []byte(parts[2])
		keys[string(key)] = true
		if pointer != nil {
			keys[string(pointer)] = true
		}
		data := raw.Get(key)
		var event usage.Event
		matchesRaw := data != nil && json.Unmarshal(data, &event) == nil && bytes.Equal(indexKey, clientEventIndexKey(event, key))
		if matchesRaw {
			proof, attributable, invalid, owned, err := indexProofForRaw(ctx, refs.tx, refs, p, key, data, event)
			if err != nil {
				return err
			}
			if !invalid {
				return nil
			}
			current := unresolvedScope{ClientName: event.ClientName, Timestamp: event.Timestamp, Model: event.Model, Target: event.Target}
			inCurrent, _ := unresolvedScopeInRange(current, strings.TrimSpace(filter.ClientName), from, to, strings.ToLower(strings.TrimSpace(filter.Model)), allow)
			if attributable {
				original, valid := indexProofScope(proof.IndexKey, key)
				if !valid {
					return fmt.Errorf("%w: invalid client index proof scope", ErrUsageHistoryIncomplete)
				}
				inOriginal, _ := unresolvedScopeInRange(original, strings.TrimSpace(filter.ClientName), from, to, strings.ToLower(strings.TrimSpace(filter.Model)), allow)
				if inOriginal || inCurrent {
					return fmt.Errorf("%w: invalid client index proof", ErrUsageHistoryIncomplete)
				}
				return nil
			}
			if owned && !inCurrent {
				return nil
			}
			return fmt.Errorf("%w: invalid client index proof", ErrUsageHistoryIncomplete)
		}
		current, validCurrent := indexProofScope(indexKey, key)
		client := strings.TrimSpace(filter.ClientName)
		model := strings.ToLower(strings.TrimSpace(filter.Model))
		inCurrent := true
		if validCurrent {
			inCurrent, _ = unresolvedScopeInRange(current, client, from, to, model, allow)
		}
		proof, err := readIndexProof(refs.tx.Bucket([]byte(indexProofsV2)), p, key)
		if err == nil && (proof.Origin == "atomic" || (len(p.Through) != 0 && bytes.Compare(key, p.Through) <= 0)) {
			original, valid := indexProofScope(proof.IndexKey, key)
			if valid {
				inOriginal, _ := unresolvedScopeInRange(original, client, from, to, model, allow)
				if inOriginal || inCurrent {
					return fmt.Errorf("%w: orphaned client index evidence", ErrUsageHistoryIncomplete)
				}
				return nil
			}
		}
		if owner, known := readReceiptOwner(refs.tx, key); known && client != "" && owner != receiptClientHash(client) && !inCurrent {
			return nil
		}
		owner, known, err := readRawOwner(refs.tx, key)
		if err != nil {
			return err
		}
		if known && client != "" && owner.Client != receiptClientHash(client) && !inCurrent {
			return nil
		}
		return fmt.Errorf("%w: orphaned client index evidence with unknown scope", ErrUsageHistoryIncomplete)
	})
	if err == nil {
		refs.keys = keys
	}
	return err
}

func (refs *indexRawReferences) contains(ctx context.Context, key []byte) (bool, error) {
	if refs.keys == nil {
		keys := make(map[string]bool)
		if index := refs.tx.Bucket([]byte(BucketUsageEventsByClient)); index != nil {
			if err := index.ForEach(func(indexKey, rawKey []byte) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				parts := strings.SplitN(string(indexKey), "|", 3)
				if len(parts) == 3 && parts[2] != "" {
					keys[parts[2]] = true
				} else if rawKey == nil {
					return indexV2Error("invalid nested client index identity")
				}
				if rawKey != nil {
					keys[string(rawKey)] = true
				}
				return nil
			}); err != nil {
				return false, err
			}
		}
		refs.keys = keys
	}
	return refs.keys[string(key)], ctx.Err()
}

// A missing proof on an unowned indexed or scanned row has unknown attribution.
func indexProofForRaw(ctx context.Context, tx *bolt.Tx, refs *indexRawReferences, p *indexProgress, key, data []byte, event usage.Event) (indexProof, bool, bool, bool, error) {
	if p == nil {
		return indexProof{}, false, false, false, nil
	}
	b := tx.Bucket([]byte(indexProofsV2))
	encoded := b.Get(key)
	nested := b.Bucket(key) != nil
	if encoded == nil && !nested {
		index := tx.Bucket([]byte(BucketUsageEventsByClient))
		required := (len(p.Through) != 0 && bytes.Compare(key, p.Through) <= 0) || (index != nil && index.Get(clientEventIndexKey(event, key)) != nil)
		if !required {
			owner, known, err := readRawOwner(tx, key)
			if err != nil {
				return indexProof{}, false, false, false, err
			}
			identity, identityErr := usage.RetryEquivalentDigest(event)
			if known && identityErr == nil && owner.Client == receiptClientHash(event.ClientName) && owner.Digest == identity && usageKey(event.Timestamp, event.RecordID) == string(key) {
				return indexProof{}, false, false, true, nil
			}
			var errRef error
			required, errRef = refs.contains(ctx, key)
			if errRef != nil {
				return indexProof{}, false, false, false, errRef
			}
			if !required {
				return indexProof{}, false, false, false, nil
			}
		}
	}
	var proof indexProof
	var err error
	if encoded != nil {
		proof, err = readIndexProof(b, p, key)
		digest := rawDigest(key, data)
		if err == nil && proof.RawDigest == fmt.Sprintf("%x", digest) && bytes.Equal(proof.IndexKey, clientEventIndexKey(event, key)) && (proof.Origin == "atomic" || (len(p.Through) != 0 && bytes.Compare(key, p.Through) <= 0)) {
			return proof, true, false, false, nil
		}
	}
	owner, known, ownerErr := readRawOwner(tx, key)
	identity, identityErr := usage.RetryEquivalentDigest(event)
	owned := ownerErr == nil && known && identityErr == nil && owner.Client == receiptClientHash(event.ClientName) && owner.Digest == identity && usageKey(event.Timestamp, event.RecordID) == string(key)
	if encoded == nil && !nested && owned {
		return indexProof{}, false, false, true, nil
	}
	if encoded == nil {
		return indexProof{}, false, true, owned, nil
	}
	return proof, err == nil, true, owned, nil
}

// Malformed raw bytes cannot override the original proof's client and hour.
func checkMalformedIndexScope(ctx context.Context, tx *bolt.Tx, refs *indexRawReferences, key, data []byte, client string, from, to time.Time, model string, allow costPairFilter) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := loadIndexProgressState(tx, false)
	if err != nil {
		return fmt.Errorf("%w: invalid client index evidence", ErrUsageHistoryIncomplete)
	}
	if p == nil {
		return nil
	}
	proofs := tx.Bucket([]byte(indexProofsV2))
	value := proofs.Get(key)
	if value == nil && proofs.Bucket(key) == nil {
		required := len(p.Through) != 0 && bytes.Compare(key, p.Through) <= 0
		if !required {
			scope, ambiguous := backfillRawScope(data)
			index := tx.Bucket([]byte(BucketUsageEventsByClient))
			if !ambiguous && index != nil && scope.ClientName != "" && !scope.Timestamp.IsZero() {
				required = index.Get(clientEventIndexKey(scope, key)) != nil
			}
			if !required && index != nil {
				required, err = refs.contains(ctx, key)
				if err != nil {
					return err
				}
			}
		}
		if !required {
			return nil
		}
	} else if value != nil {
		proof, err := readIndexProof(proofs, p, key)
		if err == nil && (proof.Origin == "atomic" || (len(p.Through) != 0 && bytes.Compare(key, p.Through) <= 0)) {
			scope, valid := indexProofScope(proof.IndexKey, key)
			if valid {
				intersects, _ := unresolvedScopeInRange(scope, strings.TrimSpace(client), from, to, strings.ToLower(strings.TrimSpace(model)), allow)
				if intersects {
					return fmt.Errorf("%w: malformed raw has prior client index evidence", ErrUsageHistoryIncomplete)
				}
				return nil
			}
		}
	}
	owner, known, ownerErr := readRawOwner(tx, key)
	if ownerErr != nil {
		return ownerErr
	}
	if known && (boundRawOutsideHour(key, from, to) || (strings.TrimSpace(client) != "" && owner.Client != receiptClientHash(client))) {
		return nil
	}
	return fmt.Errorf("%w: malformed raw has untrusted client index evidence", ErrUsageHistoryIncomplete)
}
