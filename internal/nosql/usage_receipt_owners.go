package nosql

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

const receiptOwnersBucket = "usage_receipt_owner_hashes_v1"
const receiptOwnersMarker = "usage_receipt_owner_hashes_initialized_v1"

type receiptOwner struct {
	ClientHash  string `json:"client_hash"`
	Binding     string `json:"binding"`
	UsageKey    []byte `json:"usage_key,omitempty"`
	QuotaKey    []byte `json:"quota_key,omitempty"`
	CellBinding string `json:"cell_binding,omitempty"`
}

func receiptClientHash(client string) string {
	sum := sha256.Sum256([]byte("usage-receipt-client-v1\x00" + strings.TrimSpace(client)))
	return hex.EncodeToString(sum[:])
}

func receiptOwnerBinding(key []byte, digest, owner string) string {
	h := sha256.New()
	h.Write([]byte("usage-receipt-owner-v1\x00"))
	h.Write(key)
	h.Write([]byte{0})
	h.Write([]byte(digest))
	h.Write([]byte{0})
	h.Write([]byte(owner))
	return hex.EncodeToString(h.Sum(nil))
}

// Owner information is optional for old receipts and never alters their payload.
func writeReceiptOwner(tx *bolt.Tx, key []byte, digest string, event usage.Event, original []byte) error {
	client := strings.TrimSpace(event.ClientName)
	var canonical usage.Event
	if client == "" || strings.EqualFold(client, "unknown") || !decodeProofEvent(original, &canonical) || usageKey(canonical.Timestamp, canonical.RecordID) != string(key) {
		return nil
	}
	meta := tx.Bucket([]byte(BucketMeta))
	if meta == nil {
		return errors.New("receipt owner: meta missing")
	}
	b := tx.Bucket([]byte(receiptOwnersBucket))
	marker := meta.Get([]byte(receiptOwnersMarker))
	if (marker != nil && (string(marker) != "1" || b == nil)) || (marker == nil && b != nil) {
		return errors.New("receipt owner: source unavailable")
	}
	if b == nil {
		var err error
		b, err = tx.CreateBucket([]byte(receiptOwnersBucket))
		if err != nil {
			return err
		}
		if err := meta.Put([]byte(receiptOwnersMarker), []byte("1")); err != nil {
			return err
		}
	}
	owner := receiptClientHash(client)
	entry := receiptOwner{ClientHash: owner, Binding: receiptOwnerBinding(key, digest, owner)}
	if existing := b.Get(key); existing != nil {
		prior, ok := decodeReceiptOwner(existing)
		if !ok || prior.ClientHash != entry.ClientHash || prior.Binding != entry.Binding {
			return errors.New("receipt owner: identity conflict")
		}
		entry = prior
	}
	usageKey := aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)
	quotaKey := quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)
	entry.UsageKey = usageKey
	entry.QuotaKey = quotaKey
	entry.CellBinding = receiptCellBinding(key, digest, owner, usageKey, quotaKey)
	value, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return b.Put(key, value)
}

func decodeReceiptOwner(data []byte) (receiptOwner, bool) {
	var owner receiptOwner
	if json.Unmarshal(data, &owner) != nil {
		return receiptOwner{}, false
	}
	canonical, err := json.Marshal(owner)
	return owner, err == nil && bytes.Equal(canonical, data)
}

func receiptCellBinding(key []byte, digest, owner string, usageKey, quotaKey []byte) string {
	h := sha256.New()
	h.Write([]byte("usage-receipt-cells-v1\x00"))
	var n [8]byte
	for _, part := range [][]byte{key, []byte(digest), []byte(owner), usageKey, quotaKey} {
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func readReceiptCells(tx *bolt.Tx, key []byte) ([]byte, []byte, bool) {
	owner, ok := readReceiptOwner(tx, key)
	if !ok {
		return nil, nil, false
	}
	receipt, ok, err := readUsageReceipt(tx, key)
	if err != nil || !ok {
		return nil, nil, false
	}
	entry, valid := decodeReceiptOwner(tx.Bucket([]byte(receiptOwnersBucket)).Get(key))
	if !valid || len(entry.UsageKey) == 0 || len(entry.QuotaKey) == 0 || entry.CellBinding != receiptCellBinding(key, receipt.Digest, owner, entry.UsageKey, entry.QuotaKey) {
		return nil, nil, false
	}
	stamp, _, found := strings.Cut(string(key), "_")
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if !found || err != nil {
		return nil, nil, false
	}
	usageScope, err := decodeAggKey(entry.UsageKey)
	if err != nil || !strings.HasPrefix(string(entry.UsageKey), "v2|") || !usageScope.dimsOK || usageScope.client == "" || usageScope.client == "unknown" || receiptClientHash(usageScope.client) != owner || !usageScope.hour.Equal(at.UTC().Truncate(time.Hour)) {
		return nil, nil, false
	}
	parts := strings.Split(string(entry.QuotaKey), "|")
	if len(parts) != 4 {
		return nil, nil, false
	}
	quotaClient, quotaTarget, quotaModel := quotaKeyDecode(parts[1]), quotaKeyDecode(parts[2]), quotaKeyDecode(parts[3])
	if string(quotaAggKey(at, quotaClient, quotaTarget, quotaModel)) != string(entry.QuotaKey) {
		return nil, nil, false
	}
	et, client, model, target := normalizeAggDimensions(usageScope.endpointType, quotaClient, quotaModel, quotaTarget)
	if et != usageScope.endpointType || client != usageScope.client || model != usageScope.model || target != usageScope.target {
		return nil, nil, false
	}
	return entry.UsageKey, entry.QuotaKey, true
}

func readReceiptOwner(tx *bolt.Tx, key []byte) (string, bool) {
	meta := tx.Bucket([]byte(BucketMeta))
	b := tx.Bucket([]byte(receiptOwnersBucket))
	if meta == nil || b == nil || string(meta.Get([]byte(receiptOwnersMarker))) != "1" {
		return "", false
	}
	owner, ok := decodeReceiptOwner(b.Get(key))
	if !ok {
		return "", false
	}
	decoded, err := hex.DecodeString(owner.ClientHash)
	if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != owner.ClientHash {
		return "", false
	}
	receipt, ok, err := readUsageReceipt(tx, key)
	if err != nil || !ok || owner.Binding != receiptOwnerBinding(key, receipt.Digest, owner.ClientHash) {
		return "", false
	}
	return owner.ClientHash, true
}

// Unknown owners and unknown timestamps remain relevant unless another known
// dimension proves that the receipt is outside the requested scope.
func receiptMayOverlap(tx *bolt.Tx, key []byte, owners map[string]bool, hours map[time.Time]bool) bool {
	if owner, ok := readReceiptOwner(tx, key); ok && !owners[owner] {
		return false
	}
	stamp, _, ok := strings.Cut(string(key), "_")
	at, err := time.Parse(time.RFC3339Nano, stamp)
	return !ok || err != nil || hours[at.UTC().Truncate(time.Hour)]
}
