package nosql

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

const rawOwnerBucket = "usage_raw_owners_v1"
const rawOwnerMarker = "usage_raw_owners_initialized_v1"
const rawOwnerRequired = "usage_raw_owners_required_v1"

var errRawOwnerOrphan = errors.New("retained raw owner without identity")

type rawOwner struct {
	Version int    `json:"version"`
	Digest  string `json:"digest"`
	Client  string `json:"client_hash"`
	Binding string `json:"binding"`
}

func rawOwnerBinding(key []byte, digest, client string) string {
	h := sha256.New()
	h.Write([]byte("usage-raw-owner-v1\x00"))
	var size [8]byte
	for _, part := range [][]byte{key, []byte(digest), []byte(client)} {
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		h.Write(size[:])
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func rawOwnerSource(tx *bolt.Tx) (*bolt.Bucket, error) {
	meta := tx.Bucket([]byte(BucketMeta))
	b := tx.Bucket([]byte(rawOwnerBucket))
	if meta == nil {
		return nil, fmt.Errorf("%w: raw owner metadata missing", ErrUsageHistoryIncomplete)
	}
	marker := meta.Get([]byte(rawOwnerMarker))
	required := meta.Get([]byte(rawOwnerRequired))
	if (marker == nil) != (b == nil) || (marker != nil && string(marker) != "1") || (required != nil && string(required) != "1") || ((required == nil) != (marker == nil)) {
		return nil, fmt.Errorf("%w: raw owner source missing", ErrUsageHistoryIncomplete)
	}
	return b, nil
}

func readRawOwner(tx *bolt.Tx, key []byte) (rawOwner, bool, error) {
	b, err := rawOwnerSource(tx)
	if err != nil || b == nil {
		return rawOwner{}, false, err
	}
	data := b.Get(key)
	if data == nil {
		return rawOwner{}, false, nil
	}
	var owner rawOwner
	if json.Unmarshal(data, &owner) != nil || owner.Version != 1 || owner.Binding != rawOwnerBinding(key, owner.Digest, owner.Client) {
		return rawOwner{}, false, fmt.Errorf("%w: invalid raw owner", ErrUsageHistoryIncomplete)
	}
	for _, s := range []string{owner.Client, owner.Digest} {
		decoded, err := hex.DecodeString(s)
		if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != s {
			return rawOwner{}, false, fmt.Errorf("%w: invalid raw owner", ErrUsageHistoryIncomplete)
		}
	}
	canonical, err := json.Marshal(owner)
	if err != nil || !bytes.Equal(canonical, data) {
		return rawOwner{}, false, fmt.Errorf("%w: noncanonical raw owner", ErrUsageHistoryIncomplete)
	}
	return owner, true, nil
}

func writeRawOwner(tx *bolt.Tx, key []byte, event usage.Event) error {
	b, err := rawOwnerSource(tx)
	if err != nil {
		return err
	}
	client := strings.TrimSpace(event.ClientName)
	if client == "" || strings.EqualFold(client, "unknown") {
		if b != nil && b.Get(key) != nil {
			return fmt.Errorf("%w: raw owner already exists", ErrUsageHistoryIncomplete)
		}
		return nil
	}
	if b == nil {
		b, err = tx.CreateBucket([]byte(rawOwnerBucket))
		if err != nil {
			return err
		}
		if err := tx.Bucket([]byte(BucketMeta)).Put([]byte(rawOwnerMarker), []byte("1")); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(BucketMeta)).Put([]byte(rawOwnerRequired), []byte("1")); err != nil {
			return err
		}
	}
	if b.Get(key) != nil {
		return fmt.Errorf("%w: raw owner already exists", ErrUsageHistoryIncomplete)
	}
	digest, err := usage.RetryEquivalentDigest(event)
	if err != nil {
		return err
	}
	owner := rawOwner{Version: 1, Digest: digest, Client: receiptClientHash(client)}
	owner.Binding = rawOwnerBinding(key, owner.Digest, owner.Client)
	data, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	return b.Put(key, data)
}
