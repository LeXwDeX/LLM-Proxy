package proxy

import (
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
)

// bindingInputs is what the IP and session binding write of one forwarding
// attempt needs; everything in it is known before the response is written.
type bindingInputs struct {
	principal *auth.Principal
	target    *Target
	clientIP  string
	// selKind and affinityBound describe how the request was routed;
	// probe is true for a request holding a half-open probe lease.
	selKind       selectionKind
	affinityBound bool
	probe         bool
	// sessionKey is empty without a session; sessionBoundTarget is the session
	// binding read when the request arrived.
	sessionKey         string
	sessionBoundTarget string
	allowed            map[string]struct{}
	model              string // normalized client request model
	upstreamModel      string // model of the IP binding key
	path               string // original URL path
}

// writeBindings applies the binding rules for a response that is (or is about
// to be) delivered, and returns one undo token per write attempt.
//
//   - a hit refreshes only a live sliding binding (a temporary one keeps its
//     absolute expiry); if it expired meanwhile it is recreated only on a
//     preferred cost tier, so in-flight traffic never renews a temporary one;
//   - a new binding is written for a fresh pick, a new session or a permanently
//     invalid session target; one on a non-preferred tier is temporary;
//   - a half-open probe request creates none (it may refresh its own).
//
// Callers have excluded account-level failures. Every write goes through
// affinityMap.Prewrite so it can be taken back.
func (s *Service) writeBindings(in bindingInputs, now time.Time) []affinityUndo {
	targetName := strings.ToLower(in.target.Name)
	// IP affinity: Set side keys on the real upstream model (original spelling),
	// matching selectTarget's Get side.
	ipHit := in.selKind == selectionAffinityHit
	ipNew := in.selKind == selectionRoundRobin && !in.affinityBound && !in.probe
	// Session writes are independent of the final selection kind: refresh the
	// original target, or rebind only if it is permanently invalid. A temporary
	// forwarding failover must not even refresh its TTL.
	sessHit := in.sessionKey != "" && strings.EqualFold(in.sessionBoundTarget, targetName)
	sessNew := in.sessionKey != "" && !sessHit && ((in.sessionBoundTarget == "" && !in.probe) || s.sessionTargetInvalid(in.sessionBoundTarget, in.allowed, in.model))
	if !ipHit && !ipNew && !sessHit && !sessNew {
		return nil
	}
	ipKey := affinityKey(in.clientIP, in.principal.Name, in.upstreamModel)
	undos := make([]affinityUndo, 0, 3)
	needIP, needSess := ipNew, sessNew
	if ipHit {
		refreshed := false
		undos = append(undos, s.affinity.Prewrite(ipKey, now, func() {
			refreshed = s.affinity.touchLocked(ipKey, targetName, now, 0)
		}))
		needIP = !refreshed
	}
	if sessHit {
		refreshed := false
		undos = append(undos, s.affinity.Prewrite(in.sessionKey, now, func() {
			refreshed = s.affinity.touchLocked(in.sessionKey, targetName, now, sessionAffinityTTL)
		}))
		needSess = !refreshed
	}
	if !needIP && !needSess {
		return undos
	}
	// A binding to a more expensive tier than this request could use (reached
	// because the cheap tier was down or saturated) is temporary: it never
	// slides, so the client returns to the cheap tier after it recovers.
	// Established sliding bindings are refreshed, never demoted.
	preferred := s.inPreferredCostTier(in.target, in.allowed, in.model, in.path, now)
	if needIP {
		switch {
		case preferred:
			undos = append(undos, s.affinity.Prewrite(ipKey, now, func() {
				s.affinity.setWithTTLLocked(ipKey, targetName, now, 0)
			}))
		case ipNew:
			undos = append(undos, s.affinity.Prewrite(ipKey, now, func() {
				s.affinity.setTemporaryLocked(ipKey, targetName, now, 0, affinityTTL)
			}))
		}
	}
	if needSess {
		switch {
		case preferred:
			undos = append(undos, s.affinity.Prewrite(in.sessionKey, now, func() {
				s.affinity.setWithTTLLocked(in.sessionKey, targetName, now, sessionAffinityTTL)
			}))
		case sessNew:
			undos = append(undos, s.affinity.Prewrite(in.sessionKey, now, func() {
				s.affinity.setTemporaryLocked(in.sessionKey, targetName, now, sessionAffinityTTL, temporarySessionAffinityTTL)
			}))
		}
	}
	return undos
}

// settlePrewrittenBindings finishes bindings written before the headers were
// committed. A response delivered in full keeps them, and a live sliding one
// is refreshed to the end of the request (the old write-after-delivery
// timestamp; Touch never renews a temporary binding nor recreates one). Any
// other outcome takes them back, newest first, restoring only what the request
// itself still owns.
func (s *Service) settlePrewrittenBindings(undos []affinityUndo, keep bool, now time.Time) {
	if keep {
		for _, u := range undos {
			if u.wrote {
				s.affinity.Touch(u.key, u.after.targetName, now, u.after.ttl)
			}
		}
		return
	}
	for i := len(undos) - 1; i >= 0; i-- {
		s.affinity.Rollback(undos[i])
	}
}
