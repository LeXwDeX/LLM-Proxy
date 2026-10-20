package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

const (
	defaultUsageJournalQueueSize = 8192
	defaultUsageJournalMaxBatch  = 512
	// usageJournalMaxAppendBatch bounds how many records share one journal
	// write+fsync (group commit).
	usageJournalMaxAppendBatch = 1024
	// usageJournalCompactBytes triggers a rewrite that drops committed lines
	// while unowned (foreign) lines keep the file from being truncated.
	usageJournalCompactBytes = 4 << 20
	// usageJournalCompactEvery bounds how many batches may pass without a
	// compaction check when sustained load never lets the queue run empty.
	usageJournalCompactEvery = 64
)

// UsageJournalAsyncOptions tunes the asynchronous write-ahead mode.
type UsageJournalAsyncOptions struct {
	// QueueSize bounds the in-memory commit queue. A full queue never blocks
	// the request: the event is already durable in the journal and is left
	// to the periodic Replay. Default 8192.
	QueueSize int
	// MaxBatch bounds how many events share one primary write transaction.
	// Default 512.
	MaxBatch int
	// DrainWaitLimit bounds how long DrainClient (quota calibration, under
	// the client's accounting gate) waits for the writer. Default 10s.
	DrainWaitLimit time.Duration
}

// usageBatchRecorder lets the background writer commit many events in one
// write transaction. UsageStore implements it; other recorders fall back to
// per-event Record calls.
type usageBatchRecorder interface {
	RecordBatch([]usage.Event) error
}

type journalAppend struct {
	line   []byte
	id     string
	client string
	done   chan error
}

type usageJournalAsync struct {
	appendCh   chan *journalAppend
	queue      chan usage.Event
	maxBatch   int
	syncerDone chan struct{}
	writerDone chan struct{}

	pendingMu   sync.Mutex
	pendingCond *sync.Cond
	pending     int
	mirror      *atomic.Int64
}

// EnableAsyncCommit switches Record to write-ahead mode: Record returns once
// the event is synced to the journal and a background writer commits it to
// the primary store in batches. Call Close during shutdown, after request
// handlers have finished and before the database is closed.
func (j *UsageJournal) EnableAsyncCommit(opts UsageJournalAsyncOptions) error {
	if j == nil || j.primary == nil {
		return errors.New("usage journal: recorder is not initialized")
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = defaultUsageJournalQueueSize
	}
	if opts.MaxBatch <= 0 {
		opts.MaxBatch = defaultUsageJournalMaxBatch
	}
	j.asyncMu.Lock()
	defer j.asyncMu.Unlock()
	if j.async != nil {
		return errors.New("usage journal: asynchronous commit already enabled")
	}
	j.mu.Lock()
	info, err := os.Stat(j.path)
	switch {
	case err == nil && info.Size() > 0:
		// Lines from a previous process belong to Replay until committed.
		j.foreign = true
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		j.mu.Unlock()
		return fmt.Errorf("usage journal: inspect path: %w", err)
	}
	j.asyncOn = true
	if opts.DrainWaitLimit > 0 {
		j.drainWait = opts.DrainWaitLimit
	}
	j.mu.Unlock()
	a := &usageJournalAsync{
		appendCh:   make(chan *journalAppend, opts.QueueSize),
		queue:      make(chan usage.Event, opts.QueueSize),
		maxBatch:   opts.MaxBatch,
		syncerDone: make(chan struct{}),
		writerDone: make(chan struct{}),
	}
	a.pendingCond = sync.NewCond(&a.pendingMu)
	a.mirror = &j.asyncPending
	j.async = a
	go j.runSyncer(a)
	go j.runWriter(a)
	return nil
}

// recordAsync is called with asyncMu held shared.
func (j *UsageJournal) recordAsync(a *usageJournalAsync, event usage.Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("usage journal: encode usage event: %w", err)
	}
	// The writer commits exactly what a crash replay would decode, so both
	// paths produce byte-identical primary records and stay idempotent.
	var replayable usage.Event
	if err := json.Unmarshal(data, &replayable); err != nil {
		return fmt.Errorf("usage journal: decode usage event: %w", err)
	}
	req := &journalAppend{line: append(data, '\n'), id: event.RecordID, client: strings.TrimSpace(event.ClientName), done: make(chan error, 1)}
	a.appendCh <- req
	if appendErr := <-req.done; appendErr != nil {
		// The journal is unavailable (for example a full disk): keep the old
		// guarantee by committing synchronously before acknowledging. Commit
		// the decoded form, byte-identical to what the writer or a Replay of
		// any partially appended line would store.
		if err := j.primary.Record(replayable); err != nil {
			return errors.Join(fmt.Errorf("journal write-ahead append: %w", appendErr), fmt.Errorf("primary usage write: %w", err))
		}
		if j.logger != nil {
			j.logger.Warn("usage journal append failed; usage committed synchronously", "error", appendErr)
		}
		return nil
	}
	a.addPending(1)
	select {
	case a.queue <- replayable:
	default:
		// The event is durable; the periodic Replay commits it.
		a.addPending(-1)
		j.mu.Lock()
		j.disownLocked(event.RecordID)
		j.foreign = true
		j.ownedCond.Broadcast()
		j.mu.Unlock()
		if n := j.deferred.Add(1); (n == 1 || n%1000 == 0) && j.logger != nil {
			j.logger.Warn("usage commit queue full; durable events deferred to journal replay", "deferred_total", n)
		}
	}
	return nil
}

// runSyncer group-commits journal appends: one write and one fsync cover
// every append that arrived while the previous fsync was in progress, with no
// added delay for a lone request.
func (j *UsageJournal) runSyncer(a *usageJournalAsync) {
	defer close(a.syncerDone)
	var lines []byte
	for first := range a.appendCh {
		batch := []*journalAppend{first}
	collect:
		for len(batch) < usageJournalMaxAppendBatch {
			select {
			case next, ok := <-a.appendCh:
				if !ok {
					break collect
				}
				batch = append(batch, next)
			default:
				break collect
			}
		}
		lines = lines[:0]
		for _, req := range batch {
			lines = append(lines, req.line...)
		}
		j.mu.Lock()
		err := j.appendLinesLocked(lines)
		if err == nil {
			for _, req := range batch {
				// A RecordID appended again while still owned is one owned
				// event: the writer's first disown releases it, and
				// disownLocked ignores the later copy, so count it once.
				if _, dup := j.owned[req.id]; dup {
					continue
				}
				j.owned[req.id] = req.client
				j.ownedByClient[req.client]++
			}
		} else {
			// A failed append may still have left bytes behind.
			j.foreign = true
		}
		j.mu.Unlock()
		for _, req := range batch {
			req.done <- err
		}
	}
}

func (j *UsageJournal) runWriter(a *usageJournalAsync) {
	defer close(a.writerDone)
	sinceCompact := 0
	for first := range a.queue {
		batch := []usage.Event{first}
	collect:
		for len(batch) < a.maxBatch {
			select {
			case next, ok := <-a.queue:
				if !ok {
					break collect
				}
				batch = append(batch, next)
			default:
				break collect
			}
		}
		failed := j.commitBatch(batch)
		j.mu.Lock()
		for _, event := range batch {
			j.disownLocked(event.RecordID)
			if _, bad := failed[event.RecordID]; bad {
				j.foreign = true
				continue
			}
			j.committed[event.RecordID] = struct{}{}
		}
		j.ownedCond.Broadcast()
		j.mu.Unlock()
		for _, event := range batch {
			if err, bad := failed[event.RecordID]; bad && j.logger != nil {
				j.logger.Warn("usage event durably queued for retry",
					"request_id", event.RequestID,
					"client", event.ClientName,
					"error", err,
				)
			}
		}
		sinceCompact++
		if len(a.queue) == 0 || sinceCompact >= usageJournalCompactEvery {
			sinceCompact = 0
			if err := j.compact(false); err != nil && j.logger != nil {
				j.logger.Warn("usage journal compaction failed", "error", err)
			}
		}
		a.addPending(-len(batch))
	}
}

// commitBatch returns the events that could not be committed.
func (j *UsageJournal) commitBatch(batch []usage.Event) map[string]error {
	if batcher, ok := j.primary.(usageBatchRecorder); ok {
		if err := batcher.RecordBatch(batch); err == nil {
			return nil
		}
		// All-or-nothing batch failed: isolate the rejected events.
	}
	var failed map[string]error
	for _, event := range batch {
		if err := j.primary.Record(event); err != nil {
			if failed == nil {
				failed = make(map[string]error)
			}
			failed[event.RecordID] = err
		}
	}
	return failed
}

// compact drops committed lines. When every line in the file is committed it
// truncates in place, which needs no rewrite and no directory sync; losing
// the truncation in a crash only replays committed events idempotently.
// Otherwise the file is rewritten once it grows large (or when forced).
func (j *UsageJournal) compact(force bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.committed) == 0 {
		return nil
	}
	if len(j.owned) == 0 && !j.foreign {
		file, err := os.OpenFile(j.path, os.O_RDWR, 0o600)
		if errors.Is(err, fs.ErrNotExist) {
			j.committed = make(map[string]struct{})
			return nil
		}
		if err != nil {
			return err
		}
		truncErr := file.Truncate(0)
		closeErr := file.Close()
		if truncErr != nil {
			return truncErr
		}
		if closeErr != nil {
			return closeErr
		}
		j.committed = make(map[string]struct{})
		return nil
	}
	if !force {
		info, err := os.Stat(j.path)
		if err != nil || info.Size() < usageJournalCompactBytes {
			return nil
		}
	}
	events, corrupt, err := j.readLocked()
	if err != nil {
		return err
	}
	if len(corrupt) > 0 {
		// Replay quarantines corrupt lines before any rewrite drops them.
		return nil
	}
	kept := make([]usage.Event, 0, len(events))
	for _, event := range events {
		if _, done := j.committed[event.RecordID]; !done {
			kept = append(kept, event)
		}
	}
	if len(kept) == len(events) {
		j.committed = make(map[string]struct{})
		return nil
	}
	if err := j.rewriteLocked(kept); err != nil {
		return err
	}
	j.resetCompactionStateLocked(kept)
	return nil
}

func (j *UsageJournal) disownLocked(id string) {
	client, ok := j.owned[id]
	if !ok {
		return
	}
	delete(j.owned, id)
	if j.ownedByClient[client]--; j.ownedByClient[client] <= 0 {
		delete(j.ownedByClient, client)
	}
}

// resetCompactionStateLocked records that the file now holds exactly kept.
func (j *UsageJournal) resetCompactionStateLocked(kept []usage.Event) {
	j.committed = make(map[string]struct{})
	j.foreign = false
	for _, event := range kept {
		if _, owned := j.owned[event.RecordID]; !owned {
			j.foreign = true
			return
		}
	}
}

// Flush waits until every event accepted by the asynchronous writer so far
// has been committed or handed back to Replay. Events deferred because the
// queue was full are not waited for. It is a no-op in synchronous mode.
func (j *UsageJournal) Flush() {
	if j == nil {
		return
	}
	j.asyncMu.RLock()
	a := j.async
	j.asyncMu.RUnlock()
	if a == nil {
		return
	}
	a.pendingMu.Lock()
	for a.pending > 0 {
		a.pendingCond.Wait()
	}
	a.pendingMu.Unlock()
}

// AsyncPending reports acknowledged events the writer has not processed yet.
func (j *UsageJournal) AsyncPending() int64 {
	if j == nil {
		return 0
	}
	return j.asyncPending.Load()
}

// AsyncDeferred reports how many acknowledged events skipped the full commit
// queue and wait for the periodic Replay instead.
func (j *UsageJournal) AsyncDeferred() int64 {
	if j == nil {
		return 0
	}
	return j.deferred.Load()
}

// Close drains the asynchronous writer: every acknowledged event is committed
// (or left in the journal for Replay) and committed lines are compacted away.
// Later Record calls use the synchronous path. Close is idempotent and a no-op
// in synchronous mode. Callers still run Replay afterwards for foreign events.
func (j *UsageJournal) Close() error {
	if j == nil {
		return nil
	}
	j.asyncMu.Lock()
	a := j.async
	j.async = nil
	j.asyncMu.Unlock()
	if a == nil {
		return nil
	}
	close(a.appendCh)
	<-a.syncerDone
	close(a.queue)
	<-a.writerDone
	j.mu.Lock()
	j.asyncOn = false
	j.mu.Unlock()
	return j.compact(true)
}

func (a *usageJournalAsync) addPending(delta int) {
	a.pendingMu.Lock()
	a.pending += delta
	if a.pending <= 0 {
		a.pending = 0
		a.pendingCond.Broadcast()
	}
	a.mirror.Store(int64(a.pending))
	a.pendingMu.Unlock()
}
