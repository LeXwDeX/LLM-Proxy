package nosql

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// usageJournalRejectedSuffix names the sidecar beside the recovery journal
// that preserves rows the ledger permanently refuses. Like the ".corrupt"
// quarantine file it is private (0600), append-only and never replayed
// automatically; an operator reviews it by hand.
const usageJournalRejectedSuffix = ".rejected"

// Reasons recorded for permanently rejected journal rows.
const (
	usageRejectIdentityUnverified = "identity_unverified"
	usageRejectDigestConflict     = "digest_conflict"
	usageRejectInvalidBundle      = "invalid_bundle"
)

// rejectedUsageLine is one JSON line of the rejected sidecar. Line holds the
// original journal line byte for byte (base64 in JSON), so lines that are not
// valid UTF-8 survive unchanged and deduplicate by the SHA-256 of those bytes.
type rejectedUsageLine struct {
	RejectedAt time.Time `json:"rejected_at"`
	Reason     string    `json:"reason"`
	Error      string    `json:"error"`
	RecordID   string    `json:"record_id"`
	Client     string    `json:"client"`
	Line       []byte    `json:"line"`
}

func rejectedLineDigest(line []byte) [sha256.Size]byte {
	return sha256.Sum256(line)
}

// permanentUsageRejection classifies primary-store errors that no retry can
// resolve. Everything else (I/O, closed or failed bbolt transactions, missing
// buckets, quarantined identities) stays pending and fail-closed.
func permanentUsageRejection(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrUsageIdentityUnverified):
		return usageRejectIdentityUnverified, true
	case errors.Is(err, ErrUsageRecordConflict):
		return usageRejectDigestConflict, true
	case errors.Is(err, ErrUsageBundleInvalid):
		return usageRejectInvalidBundle, true
	default:
		return "", false
	}
}

// rejectLocked durably appends rejected rows to the sidecar before the caller
// may drop them from the journal. The sidecar stays a set keyed by the SHA-256
// of each original line, so a failed rewrite that re-rejects the same line on
// every tick, or a restored journal, never duplicates entries.
func (j *UsageJournal) rejectLocked(entries []rejectedUsageLine) error {
	path := j.path + usageJournalRejectedSuffix
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	seen := make(map[[sha256.Size]byte]struct{})
	for _, raw := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var prior rejectedUsageLine
		if json.Unmarshal([]byte(raw), &prior) == nil {
			seen[rejectedLineDigest(prior.Line)] = struct{}{}
		}
	}
	var out []byte
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		// Never glue a new entry onto a torn tail from a failed append.
		out = append(out, '\n')
	}
	appended := 0
	for _, entry := range entries {
		digest := rejectedLineDigest(entry.Line)
		if _, dup := seen[digest]; dup {
			continue
		}
		data, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("encode rejected usage line: %w", err)
		}
		out = append(append(out, data...), '\n')
		seen[digest] = struct{}{}
		appended++
	}
	if appended == 0 {
		return nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(out); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(j.path))
}

// errorLogger never drops a rejection report when no logger was configured.
func (j *UsageJournal) errorLogger() *slog.Logger {
	if j.logger != nil {
		return j.logger
	}
	return slog.Default()
}
