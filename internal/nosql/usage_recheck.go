package nosql

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

const usageRecheckBucket = "usage_recheck_operations_v1"
const usageRecheckMarker = "usage_recheck_operations_initialized_v1"
const usageRecheckSafeMigration = "usage_recheck_identity_only_migration_v1"
const usageRecheckIdentityVerified = "identity_verified_accounting_unresolved"
const usageRecheckAccountingVerified = "accounting_verified"

var ErrUsageRecheckConflict = errors.New("usage recheck conflict")

type UsageRecheckEntry struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	Kind     string `json:"kind,omitempty"`
	Resolved bool   `json:"resolved"`
}

type UsageRecheckRequest struct {
	EvidenceID  string
	Version     string
	OperationID string
	Actor       string
}

type UsageRecheckResult struct {
	Status             string `json:"status"`
	AccountingVerified bool   `json:"accounting_verified"`
	AccountingRestored bool   `json:"accounting_restored"`
}

type usageRecheckReceipt struct {
	Request UsageRecheckRequest `json:"request"`
	Result  UsageRecheckResult  `json:"result"`
}

func recheckHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ListUsageRechecks exposes opaque identifiers and state, not original evidence.
func (s *UsageStore) ListUsageRechecks(offset, limit int) ([]UsageRecheckEntry, bool, error) {
	if s == nil || s.db == nil || offset < 0 || limit < 1 || limit > 500 {
		return nil, false, errors.New("invalid usage recheck query")
	}
	entries := make([]UsageRecheckEntry, 0)
	more := false
	err := s.db.View(func(tx *bolt.Tx) error {
		b, err := unresolvedState(tx)
		if err != nil || b == nil {
			return err
		}
		index := 0
		return b.ForEach(func(k, raw []byte) error {
			if raw == nil {
				return errors.New("invalid unresolved evidence")
			}
			var item unresolvedUsage
			if err := json.Unmarshal(raw, &item); err != nil {
				return err
			}
			if index >= offset && len(entries) < limit {
				kind := ""
				if item.Kind == "aggregate_backfill" {
					kind = item.Kind
				} else if item.Kind != "" {
					kind = "other"
				}
				entries = append(entries, UsageRecheckEntry{ID: recheckHash(k), Version: recheckHash(raw), Kind: kind, Resolved: item.Resolved})
			} else if index >= offset {
				more = true
			}
			index++
			return nil
		})
	})
	return entries, more, err
}

// RecheckUsage verifies a saved bundle and its accounting evidence, not upstream authenticity.
func (s *UsageStore) RecheckUsage(req UsageRecheckRequest) (UsageRecheckResult, error) {
	return s.recheckUsage(req, func(tx *bolt.Tx, event AuditEvent) error {
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketAdminAudit)).Put([]byte(auditKey(event.Timestamp, event.ID)), data)
	})
}

func (s *UsageStore) recheckUsage(req UsageRecheckRequest, recordAudit func(*bolt.Tx, AuditEvent) error) (UsageRecheckResult, error) {
	// Issue #125: a maintenance write; the integrity classification is stale
	// from here until a rebuild that starts after this write ended.
	defer integrityMutationBegin()()
	var result UsageRecheckResult
	if s == nil || s.db == nil || len(req.EvidenceID) != 64 || len(req.Version) != 64 || strings.TrimSpace(req.Actor) == "" || req.Actor != strings.TrimSpace(req.Actor) {
		return result, errors.New("invalid usage recheck request")
	}
	for _, v := range []string{req.EvidenceID, req.Version} {
		decoded, err := hex.DecodeString(v)
		if err != nil || hex.EncodeToString(decoded) != v {
			return result, errors.New("invalid usage recheck identifier")
		}
	}
	id, err := uuid.Parse(req.OperationID)
	if err != nil || id.String() != req.OperationID {
		return result, errors.New("invalid usage recheck operation ID")
	}
	var rejection error
	err = s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(BucketMeta))
		audit := tx.Bucket([]byte(BucketAdminAudit))
		if meta == nil || audit == nil {
			return errors.New("usage recheck audit unavailable")
		}
		ops := tx.Bucket([]byte(usageRecheckBucket))
		marker := meta.Get([]byte(usageRecheckMarker))
		if (marker != nil && (string(marker) != "1" || ops == nil)) || (marker == nil && ops != nil) {
			return errors.New("usage recheck operations unavailable")
		}
		if ops == nil {
			var err error
			ops, err = tx.CreateBucket([]byte(usageRecheckBucket))
			if err != nil {
				return err
			}
			if err := meta.Put([]byte(usageRecheckMarker), []byte("1")); err != nil {
				return err
			}
		}
		writeAudit := func(status string) error {
			now := time.Now().UTC()
			event := AuditEvent{ID: uuid.NewString(), Timestamp: now, Actor: req.Actor, Action: "usage_recheck", Object: req.EvidenceID, Result: status, Detail: req.OperationID}
			return recordAudit(tx, event)
		}
		if raw := ops.Get([]byte(req.OperationID)); raw != nil {
			var prior usageRecheckReceipt
			if err := json.Unmarshal(raw, &prior); err != nil {
				return err
			}
			if prior.Request.OperationID != req.OperationID || (prior.Result.Status != "verified" && prior.Result.Status != usageRecheckIdentityVerified && prior.Result.Status != usageRecheckAccountingVerified && prior.Result.Status != "insufficient_evidence" && prior.Result.Status != "version_conflict") {
				return errors.New("invalid usage recheck receipt")
			}
			if prior.Request != req {
				rejection = ErrUsageRecheckConflict
				return writeAudit("operation_conflict")
			}
			result = prior.Result
			if result.Status == "verified" {
				result.Status = usageRecheckIdentityVerified
				result.AccountingVerified = false
				result.AccountingRestored = false
			}
			if result.Status == "version_conflict" {
				rejection = ErrUsageRecheckConflict
			}
			return writeAudit("retry_" + result.Status)
		}
		b, err := unresolvedState(tx)
		if err != nil {
			return err
		}
		var key, value []byte
		if b != nil {
			if err := b.ForEach(func(k, v []byte) error {
				if recheckHash(k) == req.EvidenceID {
					key, value = append([]byte(nil), k...), append([]byte(nil), v...)
				}
				return nil
			}); err != nil {
				return err
			}
		}
		if value == nil || recheckHash(value) != req.Version {
			rejection = ErrUsageRecheckConflict
			result.Status = "version_conflict"
			data, err := json.Marshal(usageRecheckReceipt{Request: req, Result: result})
			if err != nil {
				return err
			}
			if err := ops.Put([]byte(req.OperationID), data); err != nil {
				return err
			}
			return writeAudit(result.Status)
		}
		var item unresolvedUsage
		if err := json.Unmarshal(value, &item); err != nil {
			return err
		}
		rows, keys, valid := recheckEvidenceBundle(tx, item)
		identityVerified := valid && recheckCommittedBundle(tx, rows, keys)
		result.Status = "insufficient_evidence"
		if identityVerified {
			result.Status = usageRecheckIdentityVerified
			if usageAccountingProven(tx, rows, keys) {
				result.Status = usageRecheckAccountingVerified
				result.AccountingVerified = true
				result.AccountingRestored = !item.Resolved
				if !item.Resolved {
					item.Resolved = true
					updated, err := json.Marshal(item)
					if err != nil {
						return err
					}
					if err := b.Put(key, updated); err != nil {
						return err
					}
				}
			}
		}
		data, err := json.Marshal(usageRecheckReceipt{Request: req, Result: result})
		if err != nil {
			return err
		}
		if err := ops.Put([]byte(req.OperationID), data); err != nil {
			return err
		}
		auditResult := result.Status
		if result.AccountingRestored {
			auditResult = "accounting_restored"
		}
		return writeAudit(auditResult)
	})
	if err != nil {
		return UsageRecheckResult{}, err
	}
	return result, rejection
}

// Older rechecks changed accounting state after checking identity only.
func restoreIdentityOnlyRechecks(tx *bolt.Tx) error {
	meta := tx.Bucket([]byte(BucketMeta))
	if meta == nil {
		return errors.New("usage recheck migration: meta unavailable")
	}
	if marker := meta.Get([]byte(usageRecheckSafeMigration)); marker != nil {
		if string(marker) != "1" {
			return errors.New("usage recheck migration: invalid marker")
		}
		return nil
	}
	ops, marker := tx.Bucket([]byte(usageRecheckBucket)), meta.Get([]byte(usageRecheckMarker))
	if (marker != nil && (string(marker) != "1" || ops == nil)) || (marker == nil && ops != nil) {
		return errors.New("usage recheck migration: operations unavailable")
	}
	if ops == nil {
		return meta.Put([]byte(usageRecheckSafeMigration), []byte("1"))
	}
	type oldReceipt struct {
		raw   []byte
		found bool
	}
	legacy := make(map[string]*oldReceipt)
	if err := ops.ForEach(func(_, raw []byte) error {
		var receipt usageRecheckReceipt
		if raw == nil || json.Unmarshal(raw, &receipt) != nil {
			return errors.New("usage recheck migration: invalid receipt")
		}
		if receipt.Result.Status == "verified" {
			legacy[receipt.Request.EvidenceID] = &oldReceipt{raw: append([]byte(nil), raw...)}
		}
		return nil
	}); err != nil {
		return err
	}
	b, err := unresolvedState(tx)
	if err != nil {
		return err
	}
	type restoreEntry struct{ key, value []byte }
	var restore []restoreEntry
	if b != nil {
		if err := b.ForEach(func(k, raw []byte) error {
			prior := legacy[recheckHash(k)]
			if prior == nil {
				return nil
			}
			prior.found = true
			var item unresolvedUsage
			if json.Unmarshal(raw, &item) != nil {
				return errors.New("usage recheck migration: invalid evidence")
			}
			if item.Resolved {
				item.Resolved = false
				value, err := json.Marshal(item)
				if err != nil {
					return err
				}
				restore = append(restore, restoreEntry{append([]byte(nil), k...), value})
			}
			return nil
		}); err != nil {
			return err
		}
	}
	for _, entry := range restore {
		if err := b.Put(entry.key, entry.value); err != nil {
			return err
		}
	}
	for _, prior := range legacy {
		if !prior.found {
			if err := putUnresolvedEvidence(tx, unresolvedUsage{Kind: "recheck_unattributed", Original: prior.raw}); err != nil {
				return err
			}
		}
	}
	return meta.Put([]byte(usageRecheckSafeMigration), []byte("1"))
}

func recheckCommittedBundle(tx *bolt.Tx, rows []usage.Event, keys []string) bool {
	for i, row := range rows {
		digest, err := usage.RetryEquivalentDigest(row)
		if err != nil {
			return false
		}
		state, _, _, err := lookupUsageIdentity(tx, []byte(keys[i]), []byte(digest))
		if err != nil || state == identityNew {
			return false
		}
	}
	return true
}

func recheckEvidenceBundle(tx *bolt.Tx, item unresolvedUsage) ([]usage.Event, []string, bool) {
	if item.Kind != "" || item.Event.Supplementary || item.Event.ParentRecordID != "" || strings.TrimSpace(item.Event.ClientName) == "" || strings.EqualFold(strings.TrimSpace(item.Event.ClientName), "unknown") || strings.TrimSpace(item.Event.RecordID) == "" || item.Event.Timestamp.IsZero() {
		return nil, nil, false
	}
	var original usage.Event
	decoder := json.NewDecoder(bytes.NewReader(item.Original))
	decoder.DisallowUnknownFields()
	if !utf8.Valid(item.Original) || !json.Valid(item.Original) || foldedJSONCollision(item.Original) || !recheckExactJSONFields(item.Original, reflect.TypeOf(usage.Event{})) || decoder.Decode(&original) != nil || strings.TrimSpace(original.RecordID) == "" || original.Timestamp.IsZero() {
		return nil, nil, false
	}
	rows, keys, err := normalizeUsageBundle(item.Event)
	if err != nil || len(rows) == 0 {
		return nil, nil, false
	}
	originalRows, _, err := normalizeUsageBundle(original)
	if err != nil {
		return nil, nil, false
	}
	want, err := usage.RetryEquivalentDigest(rows[0])
	if err != nil {
		return nil, nil, false
	}
	got, err := usage.RetryEquivalentDigest(originalRows[0])
	if err != nil || got != want {
		return nil, nil, false
	}
	quarantine := tx.Bucket([]byte(BucketUsageEventsQuarantine))
	if quarantine == nil {
		return nil, nil, false
	}
	for _, key := range keys {
		if quarantine.Get([]byte(key)) != nil {
			return nil, nil, false
		}
	}
	return rows, keys, true
}

// encoding/json otherwise accepts case-insensitive struct field aliases.
func recheckExactJSONFields(raw json.RawMessage, typ reflect.Type) bool {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return true
	}
	if typ == reflect.TypeOf(time.Time{}) {
		return true
	}
	switch typ.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil || object == nil {
			return false
		}
		fields := make(map[string]reflect.Type, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if field.IsExported() && name != "-" && name != "" {
				fields[name] = field.Type
			}
		}
		for name, value := range object {
			field, ok := fields[name]
			if !ok || !recheckExactJSONFields(value, field) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return false
		}
		for _, value := range values {
			if !recheckExactJSONFields(value, typ.Elem()) {
				return false
			}
		}
	}
	return true
}
