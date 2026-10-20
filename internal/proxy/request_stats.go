package proxy

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
)

// SetRequestStatsRecorder installs a non-billing completion recorder.
func (s *Service) SetRequestStatsRecorder(recorder requeststats.Recorder) {
	s.usageMu.Lock()
	s.requestStatsRecorder = recorder
	s.usageMu.Unlock()
}

func (s *Service) currentRequestStatsRecorder() requeststats.Recorder {
	s.usageMu.RLock()
	defer s.usageMu.RUnlock()
	return s.requestStatsRecorder
}

// HasRequestStatsRecorder reports whether live completion collection is configured.
func (s *Service) HasRequestStatsRecorder() bool {
	return s.currentRequestStatsRecorder() != nil
}

func (s *Service) recordRequestCompletion(recorder requeststats.Recorder, outcome requeststats.Outcome) {
	if recorder == nil {
		return
	}
	if err := recorder.Record(requeststats.Event{Timestamp: time.Now().UTC(), Outcome: outcome}); err != nil {
		s.requestStatsRecordFailures.Add(1)
		// A full queue under overload fails every request; log at most once
		// per window with the number of completions dropped since the last line.
		if dropped, ok := s.requestStatsLog.note(); ok && s.logger != nil {
			s.logger.Error("request stats record failed", "dropped", dropped, "error", err)
		}
	}
}

const requestStatsLogInterval = 10 * time.Second

// failureLogLimiter rate-limits a repeated failure log line. The zero value
// is ready to use.
type failureLogLimiter struct {
	mu      sync.Mutex
	now     func() time.Time // test hook; nil means time.Now
	last    time.Time
	pending int64
}

// note counts one failure and reports whether to log now, with the number
// of failures since the previous log line (including this one).
func (l *failureLogLimiter) note() (int64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	l.pending++
	if !l.last.IsZero() && now.Sub(l.last) < requestStatsLogInterval {
		return 0, false
	}
	dropped := l.pending
	l.pending = 0
	l.last = now
	return dropped, true
}

// RequestStatsRecordFailures counts completions that were not stored: Record
// errors (including a full asynchronous queue) plus writes the recorder's
// background writer reported as failed after accepting them.
func (s *Service) RequestStatsRecordFailures() int64 {
	failures := s.requestStatsRecordFailures.Load()
	if async, ok := s.currentRequestStatsRecorder().(interface{ Failures() int64 }); ok {
		failures += async.Failures()
	}
	return failures
}

// Only the local JSON model-list path uses this writer. Forwarded responses
// return their outcome directly; there is no outer HTTP or authentication wrapper.
type localStatsWriter struct {
	http.ResponseWriter
	status int
	err    error
}

func (w *localStatsWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *localStatsWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	if err != nil {
		w.err = err
	}
	return n, err
}

// This outcome is request-local and does not alter health, usage or retry logic.
func responseCompletion(r *http.Request, status int, copyErr error, downstreamFailed, lifecycleFailed bool) requeststats.Outcome {
	if downstreamFailed || errors.Is(r.Context().Err(), context.Canceled) || errors.Is(copyErr, context.Canceled) {
		return requeststats.Canceled
	}
	if copyErr != nil || r.Context().Err() != nil || status < 200 || status >= 300 || lifecycleFailed {
		return requeststats.Failure
	}
	return requeststats.Success
}
