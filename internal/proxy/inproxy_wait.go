// inproxy_wait.go — bounded in-proxy wait and the sticky retry verdict.
//
// A client bound to a target (session or IP affinity) is best served by that
// target: its cache, session state and the model it was routed for live there.
// When the bound target is only unavailable for now (a model-level 429
// cooldown, a short breaker cooldown, a half-open probe in flight, full
// capacity) the platform does what the client cannot: it waits a bounded time
// inside the request and retries, so a short outage costs latency instead of
// an error. A target that was already sent this request is never waited for or
// sent it again (every target gets a request at most once). When waiting is not
// possible it answers a retryable status with Retry-After instead of replaying
// a substitute target's 4xx, which a client such as Claude Code treats as final.
// A sticky target's own 4xx (a 429 included) is its verdict and is replayed.
//
// Nothing is sent to an upstream or a client during the wait, so no request is
// replayed: a wait only happens while no byte has been written.
package proxy

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// inProxyWaitMax is the longest recovery time a request will wait for.
	inProxyWaitMax = 30 * time.Second
	// inProxyWaitJitterMax spreads wake-ups so waiters do not stampede the
	// recovering target in one instant.
	inProxyWaitJitterMax = 500 * time.Millisecond
	// inProxyWaitSlots caps concurrent waiters process-wide; beyond it requests
	// fail fast exactly as they would without a wait.
	inProxyWaitSlots = 64
	// stickyHoldProbeRecovery is the Retry-After hint while a half-open probe
	// of the sticky target is in flight or the target is full (no recovery
	// time is known).
	stickyHoldProbeRecovery = time.Second
	// ipStickyHoldMax bounds how long an IP-affinity binding holds a request
	// for its target: a longer outage falls back to the substitute's answer.
	// A session binding holds for up to retryAfterMax.
	ipStickyHoldMax = 120 * time.Second
	// inProxyProbeWaitMax / inProxyProbePollStep bound the short polling that
	// follows a wait when the recovered target is half-open and another
	// request is its probe.
	inProxyProbeWaitMax  = 3 * time.Second
	inProxyProbePollStep = 200 * time.Millisecond
)

// SetInProxyWaitLimits tunes the bounded in-proxy wait: max is the longest
// recovery time a request waits for and jitter the wake-up spread. Zero keeps
// the built-in value (inProxyWaitMax, inProxyWaitJitterMax); a negative max
// disables the wait, a negative jitter removes the spread. It exists for
// deterministic acceptance tests of cooldown behaviour and is not read from
// configuration.
func (s *Service) SetInProxyWaitLimits(max, jitter time.Duration) {
	s.inProxyWaitMaxOverride, s.inProxyWaitJitterOverride = max, jitter
}

// inProxyWaitLimit returns the longest recovery time worth waiting for;
// ok is false when the wait is disabled.
func (s *Service) inProxyWaitLimit() (limit time.Duration, ok bool) {
	switch {
	case s.inProxyWaitMaxOverride < 0:
		return 0, false
	case s.inProxyWaitMaxOverride > 0:
		return s.inProxyWaitMaxOverride, true
	}
	return inProxyWaitMax, true
}

func (s *Service) inProxyWaitJitter() time.Duration {
	switch {
	case s.inProxyWaitJitterOverride < 0:
		return 0
	case s.inProxyWaitJitterOverride > 0:
		return time.Duration(rand.Int64N(int64(s.inProxyWaitJitterOverride) + 1))
	}
	return time.Duration(rand.Int64N(int64(inProxyWaitJitterMax) + 1))
}

// stickyVerdict describes a request that is owed its sticky target's verdict.
type stickyVerdict struct {
	state *targetState
	// retryAt is when the client should retry (always set, at least a second
	// ahead). waitUntil is set when waiting inside the proxy makes sense: the
	// target is cooling or probing and has not been sent this request yet.
	retryAt   time.Time
	waitUntil time.Time
}

// stickyVerdictPending reports whether the request is held by its sticky
// target: bound on the first selection round (bound, from selectionTrace) to a
// target that is still a legal candidate (exists, not paused, ACL, model,
// path), no bound target has answered this request with a 4xx (answered; a 429
// counts), and the target is not cooling after an account rejection. The target is then
// unavailable for now (cooling, a model-level 429 cooldown, a half-open probe,
// at capacity) or has already failed this request before sending (attempted),
// and a substitute target's 4xx is not its verdict. A known recovery further
// away than the binding allows (retryAfterMax for a session, ipStickyHoldMax
// for an IP binding) releases the request to the substitute's answer.
//
// It is read-only: it takes no probe lease and changes no state. Full targets
// and probes in flight get a one second Retry-After and are never waited for
// in the proxy beyond the probe polling; an attempted target is never waited
// for (a request is sent to a target at most once).
func (s *Service) stickyVerdictPending(bound []string, sessionBinding bool, allowed, attempted, answered map[string]struct{}, model, path string, now time.Time) (stickyVerdict, bool) {
	limit := ipStickyHoldMax
	if sessionBinding {
		limit = retryAfterMax
	}
	// A bound target that answered this request has given the verdict, whichever
	// spelling of the binding named it first.
	for _, name := range bound {
		if _, done := answered[strings.ToLower(name)]; done {
			return stickyVerdict{}, false
		}
	}
	for _, name := range bound {
		state, ok := s.stickyCandidate(name, allowed, model, path)
		if !ok {
			continue
		}
		key := strings.ToLower(state.Target().Name)
		stats := state.Stats()
		if stats.MutedUntil.After(now) && upstreamRejectionMutesTarget(stats.MuteStatus) {
			continue // account rejection: long-lived, surfaced as before
		}
		var until time.Time
		if stats.MutedUntil.After(now) {
			until = stats.MutedUntil
		}
		probing := state.probeSlotBusy(now)
		if adm, modelUntil := state.modelView(model, now); adm == modelCooling && modelUntil.After(until) {
			until = modelUntil
		} else if adm == modelProbeBusy {
			probing = true
		}
		if !until.IsZero() && until.Sub(now) > limit {
			continue
		}
		v := stickyVerdict{state: state, retryAt: until}
		if until.IsZero() {
			v.retryAt = now.Add(stickyHoldProbeRecovery)
			if probing {
				v.waitUntil = v.retryAt
			}
		} else {
			v.waitUntil = until
		}
		if _, tried := attempted[key]; tried {
			v.waitUntil = time.Time{}
		}
		return v, true
	}
	return stickyVerdict{}, false
}

// inProxyWaitPlan decides whether a failed selection may wait in the proxy and
// until when. Either the request is held by its sticky target and that target
// is worth waiting for (verdict.waitUntil), or nothing is available, no 4xx
// has been cached (a cached 4xx means a substitute already answered, which only
// a sticky verdict may override) and every blocker is transient (plainOK is the
// caller's conjunction of "not held" and "no 429 answer yet"). The recovery
// must be within the wait bound.
func (s *Service) inProxyWaitPlan(selErr *selectionError, verdict stickyVerdict, pending, haveCachedError, plainOK bool, now time.Time) (time.Time, bool) {
	var until time.Time
	switch {
	case pending:
		until = verdict.waitUntil
	case plainOK && !haveCachedError && selErr != nil && selErr.Transient && !selErr.EarliestRecovery.IsZero():
		until = selErr.EarliestRecovery
	}
	if until.IsZero() {
		return time.Time{}, false
	}
	if limit, enabled := s.inProxyWaitLimit(); !enabled || until.Sub(now) > limit {
		return time.Time{}, false
	}
	return until, true
}

// inProxyWait blocks until the recovery time (plus jitter) or ctx ends. It
// reports admitted=false, without waiting, when the global slots are taken.
// While waiting the request holds no target slot, key, probe lease or
// admission token: selection already released them.
//
// counted marks the request's one wait (metrics); jitter spreads the wake-up
// of a primary wait. The short probe polling uses the same slots; when it is
// the request's first and only wait it is counted too.
func (s *Service) inProxyWait(ctx context.Context, until time.Time, counted, jitter bool) (admitted bool, err error) {
	if s.inProxyWaiting.Add(1) > inProxyWaitSlots {
		s.inProxyWaiting.Add(-1)
		if counted {
			s.metrics.totalInProxyWaitRejected.Add(1)
		}
		return false, nil
	}
	defer s.inProxyWaiting.Add(-1)
	d := time.Until(until)
	if d < 0 {
		d = 0
	}
	if counted {
		s.metrics.totalInProxyWait.Add(1)
	}
	if jitter {
		d += s.inProxyWaitJitter()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return true, ctx.Err()
	case <-timer.C:
		return true, nil
	}
}

// retryAfterSeconds renders a remaining duration as Retry-After seconds,
// rounded up and at least one.
func retryAfterSeconds(until, now time.Time) int {
	d := until.Sub(now)
	secs := int(d / time.Second)
	if d%time.Second > 0 {
		secs++
	}
	if secs < 1 {
		secs = 1
	}
	return secs
}

// writeServiceUnavailable answers a failed selection. A 503 caused by a
// cooldown, full capacity or a recovering target carries Retry-After.
func writeServiceUnavailable(w http.ResponseWriter, selErr *selectionError, now time.Time) {
	if selErr.Status == http.StatusServiceUnavailable && !selErr.RetryAt.IsZero() {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(selErr.RetryAt, now)))
	}
	http.Error(w, selErr.Message, selErr.Status)
}

// writeStickyRetry answers a request held by its sticky target: 503 with
// Retry-After, a proxy-generated error body and, when a substitute target
// answered with a 4xx, its status in X-Upstream-Status and its error code (or
// type) as error.substitute_error_code for diagnosis. The substitute's body is
// not replayed and no target is named: the client should retry, and will
// return to the sticky target when it recovers.
func writeStickyRetry(w http.ResponseWriter, substituteStatus int, substituteCode string, until, now time.Time) {
	retryAfter := retryAfterSeconds(until, now)
	errBody := map[string]any{
		"type":        "upstream_temporarily_unavailable",
		"code":        "upstream_temporarily_unavailable",
		"retry_after": retryAfter,
		"message":     "The upstream this session is bound to is temporarily unavailable; retry after " + strconv.Itoa(retryAfter) + " seconds.",
	}
	if substituteCode != "" {
		errBody["substitute_error_code"] = substituteCode
	}
	body, _ := json.Marshal(map[string]any{"error": errBody})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	if substituteStatus != 0 {
		w.Header().Set(headerUpstreamStatus, strconv.Itoa(substituteStatus))
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(body)
}
