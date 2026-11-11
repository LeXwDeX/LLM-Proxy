// failure_cooldown.go — per-error-class circuit-breaker cooldowns.
//
// Every target failure picks a base cooldown from its class. Account-level
// rejections (401/402/403) use a fixed duration; every other class is the
// base of the existing breaker backoff (doubling per consecutive failure, up
// to breakerMaxBackoffFactor). 429/503/529 honour a bounded Retry-After.
package proxy

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

const (
	failureClassNetwork = "network"
	failureClassOther5x = "5xx"

	retryAfterMin = time.Second
	retryAfterMax = 10 * time.Minute
)

// Built-in defaults. Account rejections use a fixed hour. The other classes
// scale from the service quiet period (server.quiet_period_seconds, default
// 60s), so an existing quiet-period setting keeps governing them: 429, 500
// and 529 cool for half of it, every other class for the whole of it. With
// the default quiet period this is 30s / 60s. Config overrides by class.
var accountRejectionCooldowns = map[string]time.Duration{
	"401": upstreamForbiddenCooldown,
	"402": upstreamForbiddenCooldown,
	"403": upstreamForbiddenCooldown,
}

var halfQuietPeriodClasses = map[string]bool{"429": true, "500": true, "529": true}

// failureClass maps an upstream status (0 for a transport error) to its
// cooldown class.
func failureClass(status int) string {
	switch status {
	case 401, 402, 403, 408, 429, 500, 502, 503, 504, 529:
		return strconv.Itoa(status)
	}
	if status >= 500 {
		return failureClassOther5x
	}
	return failureClassNetwork
}

// failureMuteReason is the admin-facing cooldown source for a class.
func failureMuteReason(class string) string {
	if class == failureClassNetwork {
		return muteReasonNetwork
	}
	return "upstream_" + class
}

// failureCooldown returns the base cooldown for a failure. header may be nil.
func (s *Service) failureCooldown(status int, header http.Header) time.Duration {
	d, _ := s.failureCooldownExact(status, header, time.Now())
	return d
}

// failureCooldownExact also reports whether the duration came from an
// upstream Retry-After, which is honoured as-is and never doubled.
func (s *Service) failureCooldownExact(status int, header http.Header, now time.Time) (time.Duration, bool) {
	class := failureClass(status)
	switch class {
	case "429", "503", "529":
		if d, ok := parseRetryAfter(header, now); ok {
			return d, true
		}
	}
	if seconds, ok := s.failureCooldownOverrides[class]; ok && seconds > 0 {
		return time.Duration(seconds) * time.Second, false
	}
	if d, ok := accountRejectionCooldowns[class]; ok {
		return d, false
	}
	base := s.quietPeriod
	if base <= 0 {
		base = defaultQuietPeriod()
	}
	if halfQuietPeriodClasses[class] && base/2 > 0 {
		// Never round a tiny quiet period down to zero: the breaker treats a
		// non-positive base as its 60s legacy default.
		base /= 2
	}
	return base, false
}

// parseRetryAfter reads delta-seconds or an HTTP date, bounded to
// [retryAfterMin, retryAfterMax].
func parseRetryAfter(header http.Header, now time.Time) (time.Duration, bool) {
	if header == nil {
		return 0, false
	}
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}
	var d time.Duration
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs < 0 {
			return 0, false
		}
		d = time.Duration(secs) * time.Second
	} else if at, err := http.ParseTime(raw); err == nil {
		d = at.Sub(now)
	} else {
		return 0, false
	}
	if d < retryAfterMin {
		d = retryAfterMin
	}
	if d > retryAfterMax {
		d = retryAfterMax
	}
	return d, true
}

// markTargetFailure records a classified failure. Only a proven account
// rejection (accountProven) takes the fixed account cooldown; a 401/402/403
// seen on a generic failure path without that proof (for example a pooled WS
// terminal error) gets the ordinary breaker cooldown, never the fixed hour.
// A Retry-After is used as-is and is not doubled by the breaker backoff.
//
// A 429 with a known upstream model cools only that (target, model): it is a
// model or subscription limit, so sibling models of the target keep serving.
// Every other class stays target-level, as does a 429 once the target's model
// map is full (maxModelLimits) or when the request had no upstream model.
func (s *Service) markTargetFailure(state *targetState, now time.Time, status int, header http.Header, accountProven bool, upstreamModel string) {
	if state == nil {
		return
	}
	if upstreamRejectionMutesTarget(status) {
		if accountProven {
			// The key-loop convergence already settled this attempt (cooled
			// the target with its confirmed/unconfirmed duration, or left it
			// in rotation for a serving key); settling again would count the
			// same failure twice.
			return
		}
		state.markFailureClassified(now, s.quietPeriodOrDefault(), false, 0, "")
		return
	}
	cooldown, exact := s.failureCooldownExact(status, header, now)
	class := failureClass(status)
	if class == "429" && upstreamModel != "" && state.markModelLimited(now, upstreamModel, cooldown, exact, status, failureMuteReason(class)) {
		return
	}
	state.markFailureClassified(now, cooldown, exact, status, failureMuteReason(class))
}

func (s *Service) quietPeriodOrDefault() time.Duration {
	if s.quietPeriod > 0 {
		return s.quietPeriod
	}
	return defaultQuietPeriod()
}

// unverifiedRejectionCooldown is the short first cooldown of an unconfirmed
// 401/403 (class 401_unverified / 403_unverified, default 5 minutes).
const defaultUnverifiedRejectionCooldown = 5 * time.Minute

// unverifiedEscalationWindow: a second unconfirmed rejection of the same
// target within this window, with no success in between, takes the fixed
// account cooldown.
const unverifiedEscalationWindow = time.Hour

func (s *Service) unverifiedRejectionCooldown(status int) time.Duration {
	class := strconv.Itoa(status) + "_unverified"
	if seconds, ok := s.failureCooldownOverrides[class]; ok && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return defaultUnverifiedRejectionCooldown
}

// applyAccountRejection cools a target after a proven account rejection
// (see targetState.markAccountRejection) and counts it once. It reports the
// cooldown in force and whether this call applied it.
func (s *Service) applyAccountRejection(state *targetState, now time.Time, status int, code string, confirmed bool) (time.Duration, bool) {
	if state == nil {
		return 0, false
	}
	cooldown, applied := state.markAccountRejection(now, status, code, confirmed,
		s.unverifiedRejectionCooldown(status), s.failureCooldown(status, nil), unverifiedEscalationWindow)
	if applied {
		s.countRejectionMute(status)
	}
	return cooldown, applied
}

// markKeyAccountRejection cools one key of a multi-key target after a
// 401/403 the key classification did not already handle: confirmed 403s
// cool for the fixed hour, unconfirmed 401/403 for the short cooldown. It
// reports whether the key was marked. Moderation 403s and the client's own
// credential never cool a proxy key.
func (s *Service) markKeyAccountRejection(state *targetState, target *Target, protocol config.ProtocolType, keyIndex, status int, body []byte, clientCredential bool) bool {
	if state == nil || state.keyPool == nil || keyIndex < 0 || clientCredential {
		return false
	}
	if status != http.StatusForbidden && status != http.StatusUnauthorized {
		return false
	}
	if status == http.StatusForbidden && body == nil {
		// The 4 KiB pre-read could not decode the body, so moderation cannot
		// be ruled out here. Leave the key alone; the cross-target dequeue
		// classifies the complete (64 KiB) body.
		return false
	}
	rejected, confirmed := accountRejectionDetail(target, protocol, status, body, body != nil)
	if !rejected {
		return false
	}
	reason, cooldown := keyReasonForbidden, s.failureCooldown(status, nil)
	switch {
	case status == http.StatusUnauthorized:
		reason, cooldown = keyReasonUnauthorized, s.unverifiedRejectionCooldown(status)
	case !confirmed:
		reason, cooldown = keyReasonForbiddenUnverified, s.unverifiedRejectionCooldown(status)
	}
	state.keyPool.markExhaustedWithCooldown(keyIndex, reason, time.Now().Add(cooldown), string(body))
	return true
}
