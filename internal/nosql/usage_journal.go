package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

const usageJournalRetryInterval = 5 * time.Second

// defaultUsageJournalDrainWait bounds DrainClient's wait for the
// asynchronous writer (override with UsageJournalAsyncOptions.DrainWaitLimit).
const defaultUsageJournalDrainWait = 10 * time.Second

// ErrUsageJournalDrainTimeout reports that the asynchronous writer did not
// commit a client's queued events within the drain bound.
var ErrUsageJournalDrainTimeout = errors.New("usage journal: timed out waiting for the writer to commit queued events")

// usageJournalMaxTailRepair bounds how many tail bytes are considered a
// possibly-complete record when a crash left the journal without a trailing
// newline; real usage events are far smaller, so anything bigger is treated
// as an unparseable fragment.
const usageJournalMaxTailRepair = 1 << 20

// UsageJournal retries failed usage writes from a durable, append-only sibling
// file. The journal is kept beside the primary database so existing data
// directory backups include pending records.
//
// Two write modes share the same file and replay semantics:
//   - synchronous (default): Record writes the primary store first and only
//     falls back to a synced journal append when that write fails;
//   - asynchronous write-ahead (EnableAsyncCommit): Record returns after the
//     event is synced to the journal; a background writer batches the primary
//     commits and compacts committed lines away. A crash before that commit is
//     recovered by startup Replay with the same Timestamp and RecordID.
type UsageJournal struct {
	primary usage.Recorder
	path    string
	logger  *slog.Logger
	mu      sync.Mutex
	started sync.Once
	// Optional per-instance I/O hooks allow deterministic durability tests.
	syncFile func(*os.File) error
	syncDir  func(string) error
	// Explicit identity retries are uncommon and serialized separately. Ordinary
	// request-generated identities keep the existing concurrent append path.
	identityMu sync.Mutex

	// Guarded by mu. owned holds events appended by the asynchronous path
	// that the background writer still has to commit; committed holds events
	// the writer committed whose lines may still be in the file; foreign means
	// the file may hold events nobody owns (left by a previous process, a
	// failed commit or the synchronous failure path) that only Replay drains.
	owned     map[string]ownedLine // original line -> metadata
	committed map[string]struct{}  // original lines committed by the writer
	foreign   bool
	dirSynced bool
	// ownedByClient counts owned events per client; ownedCond (on mu) is
	// broadcast whenever the writer releases owned events.
	ownedByClient map[string]int
	ownedCond     *sync.Cond
	// drainWait bounds how long DrainClient waits for the writer.
	drainWait time.Duration
	// asyncOn (guarded by mu) is true while the asynchronous writer runs.
	// Then foreign is exact: with it false every line in the file is owned
	// or committed, so replays and calibration need not read the file.
	asyncOn bool
	// fileReads counts full journal reads (observability and tests).
	fileReads atomic.Int64

	// asyncMu: Record holds it shared across append and enqueue; Close holds
	// it exclusively so no producer races the queue shutdown.
	asyncMu  sync.RWMutex
	async    *usageJournalAsync
	deferred atomic.Int64
	// asyncPending mirrors the writer's pending count; it stays readable
	// while Close drains.
	asyncPending atomic.Int64
}

// NewUsageJournal creates a recorder that queues a failed primary write only
// after the recovery record has been synced to disk.
func NewUsageJournal(primary usage.Recorder, path string, logger *slog.Logger) (*UsageJournal, error) {
	if primary == nil {
		return nil, errors.New("usage journal: primary recorder is required")
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("usage journal: path is required")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("usage journal: resolve path: %w", err)
	}
	if info, err := os.Lstat(absolutePath); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("usage journal: path is not a regular file: %s", absolutePath)
		}
		if info.Mode().Perm() != 0o600 {
			if err := os.Chmod(absolutePath, 0o600); err != nil {
				return nil, fmt.Errorf("usage journal: restrict file permissions: %w", err)
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("usage journal: inspect path: %w", err)
	}
	j := &UsageJournal{
		primary:       primary,
		path:          absolutePath,
		logger:        logger,
		owned:         make(map[string]ownedLine),
		committed:     make(map[string]struct{}),
		ownedByClient: make(map[string]int),
		drainWait:     defaultUsageJournalDrainWait,
	}
	j.ownedCond = sync.NewCond(&j.mu)
	if err := j.registerExistingSidecars(); err != nil {
		return nil, err
	}
	return j, nil
}

// Record returns nil only once the event is durable in the primary store or
// in the synced journal. In synchronous mode it first attempts the primary
// recorder and journals only a failed write; in asynchronous mode it journals
// first and leaves the primary commit to the background writer.
func (j *UsageJournal) Record(event usage.Event) error {
	if j == nil || j.primary == nil {
		return errors.New("usage journal: recorder is not initialized")
	}
	event = usage.CloneEvent(event)
	if err := usage.ValidateExplicitRecordIdentity(event); err != nil {
		return j.preserveInvalidUsage(event, err)
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	} else {
		event.Timestamp = event.Timestamp.UTC()
	}
	if event.RecordID = strings.TrimSpace(event.RecordID); event.RecordID == "" {
		event.RecordID = uuid.NewString()
	} else {
		recordID, err := uuid.Parse(event.RecordID)
		if err != nil {
			return j.preserveInvalidUsage(event, fmt.Errorf("usage journal: invalid record ID: %w", err))
		}
		event.RecordID = recordID.String()
	}
	// Reject permanently invalid bundles before journaling, without replacing the payload.
	if _, _, err := normalizeUsageBundle(event); err != nil {
		return j.preserveInvalidUsage(event, err)
	}
	j.asyncMu.RLock()
	defer j.asyncMu.RUnlock()
	if j.async != nil {
		return j.recordAsync(j.async, event)
	}
	return j.recordSync(event)
}

// RecordOnce suppresses live recharging of an explicitly retried identity.
// Ordinary requests have no RecordID yet and keep one append/fsync and enqueue.
// Explicit retries validate both committed and pending payloads without an
// unbounded in-memory identity cache. Production's UsageStore supplies the
// narrow lookup; decorators accepting retry identities must forward it.
func (j *UsageJournal) RecordOnce(event usage.Event) (bool, error) {
	if j == nil || j.primary == nil {
		return false, errors.New("usage journal: recorder is not initialized")
	}
	if strings.TrimSpace(event.RecordID) == "" {
		err := j.Record(event)
		return err == nil, err
	}
	if err := usage.ValidateExplicitRecordIdentity(event); err != nil {
		return false, j.preserveInvalidUsage(event, err)
	}
	original := usage.CloneEvent(event)
	rows, _, err := normalizeUsageBundle(event)
	if err != nil {
		return false, j.preserveInvalidUsage(event, err)
	}
	event = rows[0]
	checker, ok := j.primary.(interface {
		RecordExists(usage.Event) (bool, error)
	})
	if !ok {
		return false, j.preservePrecheckFailure(event, original, errors.New("usage journal: primary cannot verify retry identity"))
	}
	j.identityMu.Lock()
	defer j.identityMu.Unlock()
	// Hold the file lock across the primary read and pending scan: compaction
	// cannot remove a line between these two checks and hide its identity.
	j.mu.Lock()
	exists, err := checker.RecordExists(event)
	if err != nil || exists {
		j.mu.Unlock()
		if saveErr := j.preservePrecheckFailure(event, original, err); saveErr != nil {
			return false, saveErr
		}
		if exists {
			checker, ok := j.primary.(interface {
				CheckUnresolvedClientRange(string, time.Time, time.Time) error
			})
			if !ok {
				return false, errors.New("usage journal: primary cannot check unresolved accounting")
			}
			hour := event.Timestamp.UTC().Truncate(time.Hour)
			if err := checker.CheckUnresolvedClientRange(event.ClientName, hour, hour.Add(time.Hour-time.Nanosecond)); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	pending, corrupt, err := j.readLocked()
	j.mu.Unlock()
	if err != nil {
		return false, j.preservePrecheckFailure(event, original, err)
	}
	if len(corrupt) > 0 {
		return false, j.preservePrecheckFailure(event, original, errors.New("usage journal: cannot verify retry identity in corrupt journal"))
	}
	digest, err := usage.RetryEquivalentDigest(event)
	if err != nil {
		return false, j.preservePrecheckFailure(event, original, err)
	}
	for _, prior := range pending {
		if prior.RecordID != event.RecordID {
			continue
		}
		priorRows, _, err := normalizeUsageBundle(prior)
		if err != nil {
			return false, j.preservePrecheckFailure(event, original, err)
		}
		priorDigest, err := usage.RetryEquivalentDigest(priorRows[0])
		if err != nil {
			return false, j.preservePrecheckFailure(event, original, err)
		}
		if digest != priorDigest {
			return false, j.preservePrecheckFailure(event, original, fmt.Errorf("usage record ID %q already exists with different event data", event.RecordID))
		}
		return false, nil
	}
	err = j.Record(event)
	return err == nil, err
}

func (j *UsageJournal) preservePrecheckFailure(event, original usage.Event, cause error) error {
	data, err := json.Marshal(original)
	registrar, ok := j.primary.(interface {
		RegisterUnresolvedUsage(usage.Event, []byte) error
	})
	if err == nil && ok {
		err = registrar.RegisterUnresolvedUsage(event, data)
	} else if err == nil {
		err = errors.New("usage journal: primary cannot register unresolved usage")
	}
	if err != nil {
		j.mu.Lock()
		appendErr := j.appendLocked(original)
		j.mu.Unlock()
		return errors.Join(cause, err, appendErr)
	}
	return cause
}

func (j *UsageJournal) recordSync(event usage.Event) error {
	if err := j.primary.Record(event); err == nil {
		return nil
	} else {
		j.mu.Lock()
		journalErr := j.appendLocked(event)
		j.mu.Unlock()
		if journalErr != nil {
			return errors.Join(fmt.Errorf("primary usage write: %w", err), fmt.Errorf("journal recovery write: %w", journalErr))
		}
		if j.logger != nil {
			j.logger.Warn("usage event durably queued for retry",
				"request_id", event.RequestID,
				"client", event.ClientName,
				"error", err,
			)
		}
		return nil
	}
}

// Replay writes every pending event to the primary recorder and removes only
// records whose write completed. Stable RecordID values let the primary store
// treat a replay after an uncertain write outcome as idempotent.
//
// Corrupt journal lines (torn appends after a crash, disk-full truncation,
// bit rot) never abort the replay: an unparseable tail fragment is quarantined,
// a complete record that only lost its trailing newline is repaired and
// replayed on the next append, and any other bad line is preserved in a
// sibling quarantine file and excluded from the rewrite. The rewrite is
// skipped when the quarantine write or the re-read fails, so corrupt lines
// and concurrently appended records can never be lost by a partial recovery.
// A record that fails transiently (I/O, a failed bbolt transaction) stays
// queued and is reported via the returned error without blocking the
// remaining events. A record the ledger permanently rejects (an identity
// before the receipt watermark, a digest conflict with committed data, or an
// invalid or partially committed bundle) is moved to the private ".rejected"
// sidecar with its original line and reason. Accounting evidence is registered
// before removing the line, including for a verified identity conflict.
func (j *UsageJournal) Replay() ([]string, error) {
	// Events the asynchronous writer still owns are committed by the writer;
	// replaying them here would only duplicate work and report recoveries
	// that never failed.
	return j.replay(nil, true)
}

// DrainClient makes all queued writes for a client visible to authoritative
// quota calibration. A failed replay or a concurrent append prevents the
// caller from replacing counters with an incomplete primary-store snapshot.
// Other clients' failed writes remain queued without blocking this client.
// Unresolved rejected rows remain visible to calibration after sidecar removal.
func (j *UsageJournal) DrainClient(clientName string) error {
	return j.drainClient(clientName, nil)
}

type usageDrainWindow struct{ from, to time.Time }

// DrainClientRange drains relevant journal rows and checks unresolved history.
// It skips only known-client events before the start hour, not future events.
// The caller must still read authoritative usage before replacing quota counters.
func (j *UsageJournal) DrainClientRange(clientName string, from, to time.Time) error {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return errors.New("usage journal: invalid calibration window")
	}
	return j.drainClient(clientName, &usageDrainWindow{from: from, to: to})
}

func (j *UsageJournal) drainClient(clientName string, window *usageDrainWindow) error {
	clientName = strings.TrimSpace(clientName)
	// Calibration needs every queued event of the client in the primary
	// store now. Events the asynchronous writer still owns are committed by
	// its batches; waiting for them avoids replaying a backlog one event at a
	// time while the caller holds the client's accounting gate. Events the
	// writer fails to commit are released as foreign and replayed below.
	// The wait is bounded: with the database stalled the caller abandons this
	// calibration (keeping its counters) instead of holding the client's
	// gate. The events stay owned and durable, so nothing is lost or counted
	// twice; a later calibration drains them.
	j.mu.Lock()
	if j.relevantOwnedLocked(clientName, window) > 0 {
		limit := j.drainWait
		deadline := time.Now().Add(limit)
		wake := time.AfterFunc(limit, func() {
			j.mu.Lock()
			j.ownedCond.Broadcast()
			j.mu.Unlock()
		})
		for j.relevantOwnedLocked(clientName, window) > 0 {
			if !time.Now().Before(deadline) {
				pending := j.relevantOwnedLocked(clientName, window)
				j.mu.Unlock()
				wake.Stop()
				return fmt.Errorf("%w: %d events for client still owned by the writer after %v", ErrUsageJournalDrainTimeout, pending, limit)
			}
			j.ownedCond.Wait()
		}
		wake.Stop()
	}
	j.mu.Unlock()
	if _, err := j.replayScoped(&clientName, true, window); err != nil {
		return err
	}
	if window != nil {
		checker, ok := j.primary.(interface {
			CheckUnresolvedClientRange(string, time.Time, time.Time) error
		})
		if !ok {
			return errors.New("usage journal: primary cannot check scoped unresolved usage")
		}
		if err := checker.CheckUnresolvedClientRange(clientName, window.from, window.to); err != nil {
			return err
		}
	} else {
		if checker, ok := j.primary.(interface{ checkUnresolvedClient(string) error }); ok {
			if err := checker.checkUnresolvedClient(clientName); err != nil {
				return err
			}
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	// With the writer running and no foreign lines, every line in the file is
	// committed or owned, so the in-memory view is the file's content and
	// the lock is not held across a file read and decode.
	if j.asyncOn && !j.foreign {
		if j.relevantOwnedLocked(clientName, window) > 0 {
			return errors.New("usage journal still has pending records for client")
		}
		return nil
	}
	entries, corrupt, err := j.readEntriesLocked()
	if err != nil {
		return fmt.Errorf("verify drained usage journal: %w", err)
	}
	if len(corrupt) > 0 {
		return errors.New("usage journal contains unrecovered corrupt records")
	}
	for _, entry := range entries {
		if _, done := j.committed[entry.line]; done {
			continue
		}
		if journalEventInDrain(entry.event, clientName, window) {
			return errors.New("usage journal still has pending records for client")
		}
	}
	return nil
}

func journalEventInDrain(event usage.Event, client string, window *usageDrainWindow) bool {
	owner := strings.TrimSpace(event.ClientName)
	if window == nil {
		return owner == client
	}
	if owner == "" || strings.EqualFold(owner, "unknown") || client == "" {
		return true
	}
	if owner != client {
		return false
	}
	return event.Timestamp.IsZero() || !event.Timestamp.UTC().Truncate(time.Hour).Before(window.from.UTC().Truncate(time.Hour))
}

// mu must be held while inspecting writer-owned lines.
func (j *UsageJournal) relevantOwnedLocked(client string, window *usageDrainWindow) int {
	if window == nil {
		return j.ownedByClient[client]
	}
	// The metadata was taken from the event when it was appended, so this
	// filter never decodes a journal line while the lock is held.
	count := 0
	for _, owned := range j.owned {
		if !owned.consistent || journalEventInDrain(owned.event, client, window) {
			count++
		}
	}
	return count
}

// ownedLine is what the drain filter needs to know about a line the writer
// owns. event carries only ClientName and Timestamp, exactly as a decode of
// the line would yield them; consistent is false when that client differs from
// the one the line was counted under, which the filter treats as relevant to
// every drain.
type ownedLine struct {
	client     string
	event      usage.Event
	consistent bool
}

func (j *UsageJournal) replay(clientName *string, skipOwned bool) ([]string, error) {
	return j.replayScoped(clientName, skipOwned, nil)
}

func (j *UsageJournal) replayScoped(clientName *string, skipOwned bool, window *usageDrainWindow) ([]string, error) {
	if j == nil || j.primary == nil {
		return nil, errors.New("usage journal: recorder is not initialized")
	}
	j.mu.Lock()
	if skipOwned && j.asyncOn && !j.foreign {
		// Every line is committed or owned by the writer: nothing to replay,
		// and reading the file here would stall the group-commit syncer.
		j.mu.Unlock()
		return nil, nil
	}
	entries, corrupt, err := j.readEntriesLocked()
	if err != nil {
		j.mu.Unlock()
		return nil, err
	}
	if len(entries) == 0 && len(corrupt) == 0 {
		j.mu.Unlock()
		return nil, nil
	}
	if window != nil && len(corrupt) > 0 {
		j.mu.Unlock()
		return nil, fmt.Errorf("%w: usage journal contains corrupt records", ErrUsageHistoryIncomplete)
	}
	var quarantineErr error
	if len(corrupt) > 0 {
		quarantineErr = j.quarantineLocked(corrupt)
	}
	snapshot := make([]journalEntry, 0, len(entries))
	for _, entry := range entries {
		event := entry.event
		if _, done := j.committed[entry.line]; done {
			continue
		}
		if window != nil {
			owner := strings.TrimSpace(event.ClientName)
			if owner == "" || strings.EqualFold(owner, "unknown") || (clientName != nil && owner == *clientName && event.Timestamp.IsZero()) {
				j.mu.Unlock()
				return nil, fmt.Errorf("%w: usage journal owner or time unknown", ErrUsageHistoryIncomplete)
			}
		}
		if _, owned := j.owned[entry.line]; owned && skipOwned {
			continue
		}
		if clientName == nil || journalEventInDrain(event, *clientName, window) {
			snapshot = append(snapshot, entry)
		}
	}
	if len(snapshot) == 0 && len(corrupt) == 0 {
		if clientName == nil {
			// Every remaining line is committed or owned by the writer.
			j.foreign = false
		}
		j.mu.Unlock()
		return nil, nil
	}
	j.mu.Unlock()

	resolved := make(map[string]int, len(snapshot))
	var recovered []string
	var rejected []rejectedUsageLine
	var replayErr error
	for _, entry := range snapshot {
		event := entry.event
		if err := j.primary.Record(event); err != nil {
			if reason, permanent := permanentUsageRejection(err); permanent {
				registrar, ok := j.primary.(interface {
					RegisterUnresolvedUsage(usage.Event, []byte) error
				})
				if !ok {
					replayErr = errors.Join(replayErr, errors.New("usage journal: primary cannot register unresolved usage"))
					continue
				}
				if saveErr := registrar.RegisterUnresolvedUsage(event, []byte(entry.line)); saveErr != nil {
					replayErr = errors.Join(replayErr, fmt.Errorf("register unresolved journal line: %w", saveErr))
					continue
				}
				// Retrying can never commit this row. It leaves the pending
				// set only after a durable sidecar copy exists (below).
				rejected = append(rejected, rejectedUsageLine{
					RejectedAt: time.Now().UTC(), Reason: reason, Error: err.Error(),
					RecordID: event.RecordID, Client: strings.TrimSpace(event.ClientName), Line: []byte(entry.line),
				})
				continue
			}
			if replayErr == nil {
				replayErr = fmt.Errorf("replay usage event %s: %w", event.RecordID, err)
			}
			continue
		}
		resolved[entry.line]++
		recovered = append(recovered, event.ClientName)
	}
	j.mu.Lock()
	var rewriteErr error
	rewriteErrSet := false
	var savedAside []rejectedUsageLine
	rewritten := false
	current, currentCorrupt, readErr := j.readEntriesLocked()
	switch {
	case readErr != nil:
		// Records appended while the snapshot was being drained already count
		// as persisted, so rewriting from the stale snapshot could drop them.
		// Leave the journal untouched; the next tick retries the full replay.
		rewriteErr = fmt.Errorf("re-read recovery journal: %w", readErr)
	case quarantineErr != nil:
		// The raw corrupt lines must stay in the journal until a durable copy
		// exists in the quarantine file; the quarantine failure (for example
		// a full disk) is also the most likely reason a rewrite would lose
		// them for good.
		rewriteErr = fmt.Errorf("quarantine corrupt journal lines: %w", quarantineErr)
	case window != nil && len(currentCorrupt) > 0:
		rewriteErr = fmt.Errorf("%w: usage journal contains corrupt records", ErrUsageHistoryIncomplete)
	default:
		if len(rejected) > 0 {
			if err := j.rejectLocked(rejected); err != nil {
				// Without a durable sidecar copy the rows stay pending, and
				// calibration for their clients stays fail-closed.
				replayErr = errors.Join(replayErr, fmt.Errorf("preserve permanently rejected journal lines: %w", err))
			} else {
				savedAside = rejected
				for _, entry := range rejected {
					resolved[string(entry.Line)]++
				}
			}
		}
		kept := make([]journalEntry, 0, len(current))
		for _, entry := range current {
			if resolved[entry.line] > 0 {
				resolved[entry.line]--
				continue
			}
			if _, committed := j.committed[entry.line]; !committed {
				kept = append(kept, entry)
			}
		}
		if len(currentCorrupt) > 0 {
			// Corrupt lines that appeared while the snapshot was being
			// drained need a durable quarantine copy before the rewrite
			// drops them from the journal; a failed quarantine skips the
			// rewrite so they survive for the next tick.
			if err := j.quarantineLocked(currentCorrupt); err != nil {
				rewriteErr = fmt.Errorf("quarantine corrupt journal lines: %w", err)
				rewriteErrSet = true
			}
		}
		if rewriteErrSet {
			break
		}
		if err := j.rewriteLocked(kept); err != nil {
			rewriteErr = fmt.Errorf("rewrite recovery journal: %w", err)
		} else {
			j.resetCompactionStateLocked(kept)
			rewritten = true
		}
	}
	j.mu.Unlock()
	for _, entry := range savedAside {
		if rewritten {
			j.errorLogger().Error("usage journal line permanently rejected by the ledger; moved to the rejected sidecar for manual review",
				"path", j.path+usageJournalRejectedSuffix, "record_id", entry.RecordID, "client", entry.Client,
				"reason", entry.Reason, "error", entry.Error)
			continue
		}
		j.errorLogger().Error("usage journal line permanently rejected by the ledger; saved to the rejected sidecar, but the journal rewrite failed and the line may still be pending",
			"path", j.path+usageJournalRejectedSuffix, "record_id", entry.RecordID, "client", entry.Client,
			"reason", entry.Reason, "error", entry.Error)
	}
	if rewriteErr != nil {
		replayErr = errors.Join(replayErr, rewriteErr)
	}
	// When the re-read also failed, the switch reported only that error; join
	// the quarantine failure so the operator sees every broken stage.
	if quarantineErr != nil && readErr != nil {
		replayErr = errors.Join(replayErr, fmt.Errorf("quarantine corrupt journal lines: %w", quarantineErr))
	}
	sort.Strings(recovered)
	recovered = uniqueStrings(recovered)
	return recovered, replayErr
}

// Start retries the durable queue in the background until ctx is canceled.
// The callback recalibrates quota counters for clients whose deferred events
// became visible in the primary usage store.
func (j *UsageJournal) Start(ctx context.Context, onRecovered func([]string)) {
	if j == nil {
		return
	}
	j.started.Do(func() {
		go func() {
			ticker := time.NewTicker(usageJournalRetryInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					recovered, err := j.replay(nil, true)
					if err != nil && j.logger != nil {
						j.logger.Warn("usage journal replay failed", "error", err)
					}
					if len(recovered) > 0 {
						if j.logger != nil {
							j.logger.Info("usage journal replay completed", "clients", len(recovered))
						}
						if onRecovered != nil {
							onRecovered(recovered)
						}
					}
				}
			}
		}()
	})
}

// trimTornTailLocked makes sure the journal ends on a record boundary before
// the next positioned append. A file whose last byte is not a newline holds a
// tail from an interrupted append. If that tail still parses as a complete
// usage event (the record bytes reached the disk but the newline did not),
// the boundary is repaired by appending the missing newline so the
// acknowledged record survives. An unparseable fragment is quarantined before
// truncation, so its original bytes remain available for manual review.
func (j *UsageJournal) trimTornTailLocked(file *os.File, size int64) (int64, error) {
	last := make([]byte, 1)
	if _, err := file.ReadAt(last, size-1); err != nil {
		return size, err
	}
	if last[0] == '\n' {
		return size, nil
	}
	const chunk = 4096
	buf := make([]byte, chunk)
	boundary := int64(0)
	var tail []byte
	for pos := size; pos > 0; {
		step := int64(chunk)
		if pos < step {
			step = pos
		}
		pos -= step
		if _, err := file.ReadAt(buf[:step], pos); err != nil {
			return size, err
		}
		// Chunks are read back to front, so each earlier-in-file chunk is
		// prepended to keep tail in file order. The chunk must be COPIED:
		// appending a zero-length tail onto the reused buf slice would alias
		// it, and the next ReadAt would overwrite the bytes already collected.
		chunkCopy := make([]byte, step)
		copy(chunkCopy, buf[:step])
		tail = append(chunkCopy, tail...)
		if idx := bytes.LastIndexByte(buf[:step], '\n'); idx >= 0 {
			boundary = pos + int64(idx) + 1
			// This chunk may contain preceding complete records. Validate only
			// the final line, not those records plus the tail concatenated.
			tail = tail[idx+1:]
			break
		}
	}
	// tail now holds the bytes after the last newline. A fragment larger
	// than any single record cannot be a complete event.
	if len(tail) <= usageJournalMaxTailRepair && isReplayableEventLine(tail) {
		if _, err := file.WriteAt([]byte{'\n'}, size); err != nil {
			return size, err
		}
		if err := file.Sync(); err != nil {
			return size, err
		}
		if j.logger != nil {
			j.logger.Info("repaired usage journal tail missing its trailing newline",
				"path", j.path, "size", size+1)
		}
		return size + 1, nil
	}
	if err := j.quarantineLocked([]string{string(tail)}); err != nil {
		return size, fmt.Errorf("preserve torn usage journal tail: %w", err)
	}
	if err := file.Truncate(boundary); err != nil {
		return size, err
	}
	if j.logger != nil {
		j.logger.Warn("trimmed unparseable usage journal tail before appending",
			"path", j.path, "size", boundary)
	}
	return boundary, nil
}

// isReplayableEventLine mirrors readLocked's validation: the line must decode
// as a usage event with a well-formed record ID before callers treat it as a
// recoverable record.
func isReplayableEventLine(line []byte) bool {
	var event usage.Event
	if err := json.Unmarshal(line, &event); err != nil {
		return false
	}
	if strings.TrimSpace(event.RecordID) == "" {
		return false
	}
	_, err := uuid.Parse(event.RecordID)
	return err == nil
}

func (j *UsageJournal) appendLocked(event usage.Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode usage event: %w", err)
	}
	// Synchronous-path appends are owned by nobody but Replay.
	j.foreign = true
	return j.appendLinesLocked(append(data, '\n'))
}

// appendLinesLocked durably appends one or more complete newline-terminated
// records with a single write and a single fsync.
func (j *UsageJournal) appendLinesLocked(line []byte) error {
	// A newly created journal needs its directory entry synced as well;
	// re-syncing the directory for every append of an existing file only
	// costs latency.
	_, statErr := os.Lstat(j.path)
	needDirSync := !j.dirSynced || errors.Is(statErr, fs.ErrNotExist)
	// O_RDWR (not O_APPEND): appendLocked positions writes explicitly and
	// reads the tail back to trim torn fragments.
	file, err := os.OpenFile(j.path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	offset := info.Size()
	if offset > 0 {
		trimmed, err := j.trimTornTailLocked(file, offset)
		if err != nil {
			return err
		}
		offset = trimmed
	}
	// Single-writer under j.mu: position the write explicitly so a failed or
	// short write (for example ENOSPC) can be rolled back to the previous
	// boundary instead of poisoning the following record.
	n, err := file.WriteAt(line, offset)
	if err != nil && n < len(line) {
		truncateErr := file.Truncate(offset)
		if syncErr := file.Sync(); truncateErr == nil {
			truncateErr = syncErr
		}
		if truncateErr != nil && j.logger != nil {
			j.logger.Error("usage journal truncation after failed append left a torn line",
				"path", j.path, "error", truncateErr)
		}
		return err
	}
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if !needDirSync {
		return nil
	}
	// The journal is often created on the first recovery event. Syncing the
	// file alone does not make that new directory entry durable across a crash.
	if err := syncDirectory(filepath.Dir(j.path)); err != nil {
		return err
	}
	j.dirSynced = true
	return nil
}

// readLocked returns the replayable events and the raw corrupt lines found in
// the journal. Corrupt lines, including a partially written final line, are
// returned for quarantine because they may hold recoverable usage data.
func (j *UsageJournal) readLocked() ([]usage.Event, []string, error) {
	entries, corrupt, err := j.readEntriesLocked()
	if err != nil {
		return nil, nil, err
	}
	events := make([]usage.Event, len(entries))
	for i, entry := range entries {
		events[i] = entry.event
	}
	return events, corrupt, nil
}

// journalEntry pairs a replayable event with its original journal line, so a
// permanently rejected row can be preserved byte for byte.
type journalEntry struct {
	event      usage.Event
	line       string
	terminated bool
}

// readEntriesLocked is readLocked keeping each event's original line.
func (j *UsageJournal) readEntriesLocked() ([]journalEntry, []string, error) {
	j.fileReads.Add(1)
	data, err := os.ReadFile(j.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	endsWithNewline := strings.HasSuffix(string(data), "\n")
	lines := strings.Split(string(data), "\n")
	events := make([]journalEntry, 0, len(lines))
	var corrupt []string
	for lineNumber, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event usage.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			tornTail := !endsWithNewline && lineNumber == len(lines)-1
			if tornTail && j.logger != nil {
				j.logger.Warn("preserving torn usage journal tail from an interrupted append",
					"path", j.path, "line", lineNumber+1)
			}
			corrupt = append(corrupt, line)
			continue
		}
		if _, ambiguous := backfillRawScope([]byte(line)); ambiguous {
			corrupt = append(corrupt, line)
			continue
		}
		if strings.TrimSpace(event.RecordID) == "" {
			corrupt = append(corrupt, line)
			continue
		}
		recordID, err := uuid.Parse(event.RecordID)
		if err != nil {
			corrupt = append(corrupt, line)
			continue
		}
		event.RecordID = recordID.String()
		events = append(events, journalEntry{event: event, line: line, terminated: lineNumber < len(lines)-1})
	}
	return events, corrupt, nil
}

// quarantineLocked preserves undecodable journal lines beside the journal so
// they can be inspected and replayed manually instead of being silently lost.
// Lines already present in the quarantine file are skipped: a tick that
// quarantines successfully but cannot rewrite re-quarantines the same lines
// on every retry, and the file must stay a set rather than grow without
// bound during a prolonged disk failure.
func (j *UsageJournal) quarantineLocked(lines []string) error {
	registrar, ok := j.primary.(interface {
		RegisterUnresolvedUsage(usage.Event, []byte) error
	})
	if !ok && len(lines) > 0 {
		return errors.New("usage journal: primary cannot register corrupt evidence")
	}
	for _, line := range lines {
		var event usage.Event
		scope, ambiguous := backfillRawScope([]byte(line))
		if ambiguous || json.Unmarshal([]byte(line), &event) != nil {
			event = scope
		}
		if err := registrar.RegisterUnresolvedUsage(event, []byte(line)); err != nil {
			return fmt.Errorf("register corrupt journal evidence: %w", err)
		}
	}
	quarantinePath := j.path + ".corrupt"
	existing, err := os.ReadFile(quarantinePath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	seen := make(map[string]struct{}, len(lines))
	for _, line := range strings.Split(string(existing), "\n") {
		seen[line] = struct{}{}
	}
	file, err := os.OpenFile(quarantinePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		if _, err := file.WriteString("\n"); err != nil {
			return err
		}
	}
	appended := 0
	for _, line := range lines {
		if _, dup := seen[line]; dup {
			continue
		}
		if _, err := file.WriteString(line + "\n"); err != nil {
			return err
		}
		seen[line] = struct{}{}
		appended++
	}
	if err := j.syncJournalFile(file); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if j.logger != nil && appended > 0 {
		j.logger.Warn("quarantined corrupt usage journal lines",
			"path", quarantinePath, "lines", appended)
	}
	return j.syncJournalDirectory(filepath.Dir(j.path))
}

func (j *UsageJournal) rewriteLocked(entries []journalEntry) error {
	dir := filepath.Dir(j.path)
	if len(entries) == 0 {
		if err := os.Remove(j.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		j.dirSynced = false
		return j.syncJournalDirectory(dir)
	}

	temp, err := os.CreateTemp(dir, ".usage-journal-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	for _, entry := range entries {
		if _, err := temp.WriteString(entry.line); err != nil {
			_ = temp.Close()
			return err
		}
		if entry.terminated {
			if _, err := temp.WriteString("\n"); err != nil {
				_ = temp.Close()
				return err
			}
		}
	}
	if err := j.syncJournalFile(temp); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, j.path); err != nil {
		return err
	}
	j.dirSynced = false
	if err := j.syncJournalDirectory(dir); err != nil {
		return err
	}
	j.dirSynced = true
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (j *UsageJournal) syncJournalFile(file *os.File) error {
	if j.syncFile != nil {
		return j.syncFile(file)
	}
	return file.Sync()
}

func (j *UsageJournal) syncJournalDirectory(path string) error {
	if j.syncDir != nil {
		return j.syncDir(path)
	}
	return syncDirectory(path)
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	unique := values[:1]
	for _, value := range values[1:] {
		if value != unique[len(unique)-1] {
			unique = append(unique, value)
		}
	}
	return unique
}
