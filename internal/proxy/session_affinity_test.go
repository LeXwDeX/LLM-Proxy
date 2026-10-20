package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// The upstreams are local fakes; only Service.ServeHTTP handles these requests.
func newSessionHTTPService(t *testing.T, mutators ...func(*config.Target)) *Service {
	t.Helper()
	var targets []config.Target
	for _, name := range []string{"t1", "t2"} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`))
		}))
		t.Cleanup(upstream.Close)
		target := config.Target{Name: name, EndpointType: "openai", Endpoint: upstream.URL, APIKey: "fake-key"}
		for _, mutate := range mutators {
			mutate(&target)
		}
		targets = append(targets, target)
	}
	svc, err := NewService(&config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5}, Targets: targets,
	}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func sendSessionRequest(t *testing.T, svc *Service, principal, ip, session string) string {
	t.Helper()
	return sendSessionModelRequest(t, svc, &auth.Principal{Name: principal}, ip, session, "test-model")
}

func sendSessionModelRequest(t *testing.T, svc *Service, principal *auth.Principal, ip, session, model string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)))
	r.RemoteAddr = ip + ":1234"
	r.Header.Set("X-Session-Id", session)
	r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("request failed: status=%d body=%s", w.Code, w.Body.String())
	}
	return w.Header().Get("X-Proxy-Target")
}

func ageSessionEntries(svc *Service, age time.Duration) {
	svc.affinity.mu.Lock()
	defer svc.affinity.mu.Unlock()
	for key, entry := range svc.affinity.entries {
		entry.lastUsed = entry.lastUsed.Add(-age)
		svc.affinity.entries[key] = entry
	}
}

func TestSessionAffinityServiceSurvivesIdleBeyondIPTTL(t *testing.T) {
	for _, session := range []string{"ses_task", ""} {
		t.Run("session="+session, func(t *testing.T) {
			svc := newSessionHTTPService(t)
			if got := sendSessionRequest(t, svc, "tester", "192.0.2.1", session); got != "t1" {
				t.Fatalf("first request should round robin to t1, got %q", got)
			}
			ageSessionEntries(svc, 10*time.Minute)
			want := "t1"
			if session == "" {
				want = "t2"
			}
			if got := sendSessionRequest(t, svc, "tester", "192.0.2.1", session); got != want {
				t.Fatalf("after 10 minutes target=%q, want %q", got, want)
			}
			if session != "" {
				state := capState(t, svc, "t1")
				if state.affinityHitCount() != 1 || state.runtimeWindow.snapshot(time.Now()).AffinityHits != 1 {
					t.Fatal("session hit missing from scheduling metrics")
				}
				svc.affinity.mu.RLock()
				entry := svc.affinity.entries[sessionAffinityKey("tester", session, "test-model")]
				svc.affinity.mu.RUnlock()
				if entry.ttl != sessionAffinityTTL || time.Since(entry.lastUsed) > time.Minute {
					t.Fatalf("session TTL was not refreshed: %+v", entry)
				}
			}
		})
	}
}

func TestSessionAffinityHeaders(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"priority", map[string]string{"X-Session-Affinity": "  first  ", "X-Session-Id": "second", "session_id": "third", "X-Claude-Code-Session-Id": "fourth"}, "first"},
		{"session-id", map[string]string{"X-Session-Id": " ses_task "}, "ses_task"},
		{"codex", map[string]string{"session_id": "codex", "X-Claude-Code-Session-Id": "claude"}, "codex"},
		{"claude-code", map[string]string{"X-Claude-Code-Session-Id": "claude"}, "claude"},
		{"blank", map[string]string{"X-Session-Affinity": "   ", "X-Session-Id": "second"}, "second"},
		{"max-length", map[string]string{"X-Session-Id": strings.Repeat("s", 256)}, strings.Repeat("s", 256)},
		{"too-long", map[string]string{"X-Session-Id": strings.Repeat("s", 257)}, ""},
		{"null", map[string]string{"X-Session-Id": "ses\x00task"}, ""},
		{"tab", map[string]string{"X-Session-Id": "ses\ttask"}, ""},
		{"delete", map[string]string{"X-Session-Id": "ses\x7ftask"}, ""},
		{"unicode-control", map[string]string{"X-Session-Id": "ses\u0085task"}, ""},
		{"invalid-first-no-fallback", map[string]string{"X-Session-Affinity": strings.Repeat("s", 257), "X-Session-Id": "second"}, ""},
		{"missing", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			for name, value := range tc.headers {
				r.Header.Set(name, value)
			}
			if got := sessionAffinityID(r); got != tc.want {
				t.Fatalf("session ID=%q, want %q", got, tc.want)
			}
		})
	}
}

func sessionRoute(t *testing.T, svc *Service, principal, ip, session string, now time.Time, wantTarget string, wantKind selectionKind) {
	t.Helper()
	var sessionBoundTarget string
	if session != "" {
		sessionBoundTarget, _ = svc.affinity.Get(sessionAffinityKey(principal, session, "test-model"), now)
	}
	state, _, kind, err := svc.selectTarget(&auth.Principal{Name: principal}, ip, sessionBoundTarget, "", nil, nil, "test-model", "/v1/chat/completions", now)
	if err != nil || state == nil {
		t.Fatalf("selectTarget: state=%v err=%v", state, err)
	}
	if state.Target().Name != wantTarget || kind != wantKind {
		t.Fatalf("target=%q kind=%v, want target=%q kind=%v", state.Target().Name, kind, wantTarget, wantKind)
	}
}

func TestSessionAffinitySelectionTTLAndIdentity(t *testing.T) {
	svc := newAffinityCapService(t, 10, 10)
	t.Cleanup(func() { _ = svc.Close() })
	now := time.Now()
	key := sessionAffinityKey("tester", "ses_task", "test-model")
	if key != "sess|6:tester|8:ses_task|test-model" {
		t.Fatalf("unexpected session key: %q", key)
	}
	sessionRoute(t, svc, "tester", "192.0.2.1", "ses_task", now, "cap-a", selectionRoundRobin)
	svc.affinity.SetWithTTL(key, "cap-a", now, sessionAffinityTTL)
	svc.affinity.Set(affinityKey("192.0.2.1", "tester", "test-model"), "cap-a", now)
	sessionRoute(t, svc, "tester", "192.0.2.1", "ses_task", now.Add(10*time.Minute), "cap-a", selectionAffinityHit)
	sessionRoute(t, svc, "tester", "192.0.2.2", "ses_task", now.Add(10*time.Minute), "cap-a", selectionAffinityHit)
	sessionRoute(t, svc, "other", "192.0.2.1", "ses_task", now.Add(10*time.Minute), "cap-b", selectionRoundRobin)
	// Without a session, the IP entry expires after five minutes.
	svc.rrCounter.Store(1)
	sessionRoute(t, svc, "tester", "192.0.2.1", "", now.Add(10*time.Minute), "cap-b", selectionRoundRobin)
	svc.rrCounter.Store(1)
	sessionRoute(t, svc, "tester", "192.0.2.2", "ses_task", now.Add(24*time.Hour+time.Second), "cap-b", selectionRoundRobin)
}

func TestSessionAffinityTwoIndependentSessions(t *testing.T) {
	svc := newSessionHTTPService(t)
	if got := sendSessionRequest(t, svc, "tester", "192.0.2.1", "ses_one"); got != "t1" {
		t.Fatalf("first session: %q", got)
	}
	// Expire only the short IP affinity so the second session gets its own turn.
	ageSessionEntries(svc, 10*time.Minute)
	if got := sendSessionRequest(t, svc, "tester", "192.0.2.1", "ses_two"); got != "t2" {
		t.Fatalf("second session: %q", got)
	}
	for _, tc := range []struct{ session, target string }{{"ses_one", "t1"}, {"ses_two", "t2"}} {
		if got := sendSessionRequest(t, svc, "tester", "192.0.2.99", tc.session); got != tc.target {
			t.Fatalf("%s drifted: got %q want %q", tc.session, got, tc.target)
		}
	}
}

func TestSessionAffinityServicePromotesIPHit(t *testing.T) {
	svc := newSessionHTTPService(t)
	sendSessionRequest(t, svc, "tester", "192.0.2.1", "")
	if got := sendSessionRequest(t, svc, "tester", "192.0.2.1", "ses_task"); got != "t1" {
		t.Fatalf("IP hit: %q", got)
	}
	ageSessionEntries(svc, 10*time.Minute)
	if got := sendSessionRequest(t, svc, "tester", "192.0.2.99", "ses_task"); got != "t1" {
		t.Fatalf("IP hit did not establish session affinity: %q", got)
	}
}

func TestSessionAffinityServicePrincipalIsolation(t *testing.T) {
	svc := newSessionHTTPService(t)
	if got := sendSessionRequest(t, svc, "first-client", "192.0.2.1", "ses_shared"); got != "t1" {
		t.Fatalf("first principal: %q", got)
	}
	if got := sendSessionRequest(t, svc, "second-client", "192.0.2.1", "ses_shared"); got != "t2" {
		t.Fatalf("second principal inherited another client's session: %q", got)
	}
	if got := sendSessionRequest(t, svc, "first-client", "192.0.2.99", "ses_shared"); got != "t1" {
		t.Fatalf("first principal's session was overwritten: %q", got)
	}
}

func TestSessionAffinityServiceWritesClientModel(t *testing.T) {
	svc := newSessionHTTPService(t)
	for _, state := range svc.targetSnapshot() {
		state.Target().allowedModelIdx = buildModelIndex([]config.ModelMapping{{Upstream: "Real-Model", Fallback: "test-model"}})
	}
	sendSessionModelRequest(t, svc, &auth.Principal{Name: "tester"}, "192.0.2.1", "ses_task", "TEST-Model")
	if got, ok := svc.affinity.Get(sessionAffinityKey("tester", "ses_task", "test-model"), time.Now()); !ok || got != "t1" {
		t.Fatalf("missing normalized client-model session entry: %q %v", got, ok)
	}
	if _, ok := svc.affinity.Get(sessionAffinityKey("tester", "ses_task", "Real-Model"), time.Now()); ok {
		t.Fatal("session entry incorrectly stored under upstream model")
	}
	if got, ok := svc.affinity.Get(affinityKey("192.0.2.1", "tester", "Real-Model"), time.Now()); !ok || got != "t1" {
		t.Fatalf("IP affinity must still use upstream model: %q %v", got, ok)
	}
	ageSessionEntries(svc, 10*time.Minute)
	if got := sendSessionRequest(t, svc, "tester", "192.0.2.99", "ses_task"); got != "t1" {
		t.Fatalf("lowercase alias did not find client-model session entry: %q", got)
	}
}

func TestSessionAffinityServiceFailoverKeepsOriginal(t *testing.T) {
	for _, unavailable := range []string{"paused", "cooling", "probe-busy"} {
		t.Run(unavailable, func(t *testing.T) {
			svc := newSessionHTTPService(t)
			sendSessionRequest(t, svc, "tester", "192.0.2.1", "ses_task")
			a := capState(t, svc, "t1")
			switch unavailable {
			case "paused":
				a.Target().Paused = true
			case "cooling":
				a.MarkFailure(time.Now(), time.Hour)
			case "probe-busy":
				a.MarkFailure(time.Now().Add(-time.Minute), time.Second)
				a.probeLease.Store(&probeLease{until: time.Now().Add(time.Hour).UnixNano()})
			}
			sessionRoute(t, svc, "tester", "192.0.2.1", "ses_task", time.Now(), "t2", selectionFailover)
			svc.affinity.mu.RLock()
			before := make(map[string]affinityEntry)
			for key, entry := range svc.affinity.entries {
				before[key] = entry
			}
			svc.affinity.mu.RUnlock()
			if got := sendSessionRequest(t, svc, "tester", "192.0.2.1", "ses_task"); got != "t2" {
				t.Fatalf("failover request: %q", got)
			}
			svc.affinity.mu.RLock()
			for key, entry := range before {
				if got := svc.affinity.entries[key]; got != entry {
					t.Errorf("failover rewrote %q: %+v -> %+v", key, entry, got)
				}
			}
			svc.affinity.mu.RUnlock()
			a.Target().Paused = false
			a.MarkSuccess(time.Now())
			sessionRoute(t, svc, "tester", "192.0.2.1", "ses_task", time.Now(), "t1", selectionAffinityHit)
			if got := sendSessionRequest(t, svc, "tester", "192.0.2.1", "ses_task"); got != "t1" {
				t.Fatalf("did not return to recovered target: %q", got)
			}
		})
	}
}

func TestSessionAffinityIgnoresSoftCap(t *testing.T) {
	svc := newAffinityCapService(t, 10, 10)
	t.Cleanup(func() { _ = svc.Close() })
	now := time.Now()
	svc.affinity.SetWithTTL(sessionAffinityKey("tester", "ses_task", "test-model"), "cap-a", now, sessionAffinityTTL)
	svc.affinity.Set(affinityKey("192.0.2.1", "tester", "test-model"), "cap-a", now)
	a := capState(t, svc, "cap-a")
	fill(t, a, 8)
	defer func() {
		for i := 0; i < 8; i++ {
			a.releaseSlot()
		}
	}()
	sessionRoute(t, svc, "tester", "192.0.2.1", "ses_task", now, "cap-a", selectionAffinityHit)
	sessionRoute(t, svc, "tester", "192.0.2.1", "", now, "cap-b", selectionFailover)
}

func TestSessionAffinityDoesNotBypassForwardHardCap(t *testing.T) {
	svc := newAffinityCapService(t, 1, 1)
	t.Cleanup(func() { _ = svc.Close() })
	now := time.Now()
	svc.affinity.SetWithTTL(sessionAffinityKey("tester", "ses_task", "test-model"), "cap-a", now, sessionAffinityTTL)
	a := capState(t, svc, "cap-a")
	fill(t, a, 1)
	defer a.releaseSlot()
	sessionRoute(t, svc, "tester", "192.0.2.1", "ses_task", now, "cap-a", selectionAffinityHit)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp, cancel, _, err := svc.forwardRequest(r, a, nil, "", nil)
	if cancel != nil {
		cancel()
	}
	if resp != nil {
		resp.Body.Close()
		t.Fatal("full target accepted a session request")
	}
	fe, ok := err.(*forwardAttemptError)
	if !ok || fe.status != http.StatusServiceUnavailable || !fe.retryable || !fe.skipMute || a.inFlightCount() != 1 {
		t.Fatalf("hard-cap enforcement changed: err=%v inFlight=%d", err, a.inFlightCount())
	}
}

func TestSessionAffinityUnavailableFilters(t *testing.T) {
	for _, filter := range []string{"allowed", "model", "path", "removed"} {
		t.Run(filter, func(t *testing.T) {
			svc := newAffinityCapService(t, 10, 10)
			t.Cleanup(func() { _ = svc.Close() })
			now := time.Now()
			key := sessionAffinityKey("tester", "ses_task", "test-model")
			svc.affinity.SetWithTTL(key, "cap-a", now, sessionAffinityTTL)
			// An IP affinity hit must not override an unavailable session record.
			svc.affinity.Set(affinityKey("192.0.2.1", "tester", "test-model"), "cap-b", now)
			a := capState(t, svc, "cap-a")
			var allowed map[string]struct{}
			switch filter {
			case "allowed":
				allowed = map[string]struct{}{"cap-b": {}}
			case "model":
				a.Target().allowedModelIdx = map[string]string{"other-model": "other-model"}
			case "path":
				a.Target().EndpointType = "claude"
			case "removed":
				svc.mu.Lock()
				delete(svc.targetsByName, "cap-a")
				svc.targetOrder = svc.targetOrder[1:]
				svc.mu.Unlock()
			}
			sessionBoundTarget, _ := svc.affinity.Get(key, now)
			state, _, kind, err := svc.selectTarget(&auth.Principal{Name: "tester"}, "192.0.2.1", sessionBoundTarget, "", allowed, nil, "test-model", "/v1/chat/completions", now)
			if err != nil || state == nil || state.Target().Name != "cap-b" || kind != selectionFailover {
				t.Fatalf("filter=%s: state=%v kind=%v err=%v", filter, state, kind, err)
			}
		})
	}
}

func TestSessionAffinityDoesNotUseUpstreamLookup(t *testing.T) {
	svc := newAffinityCapService(t, 10, 10)
	t.Cleanup(func() { _ = svc.Close() })
	for _, state := range svc.targetSnapshot() {
		state.Target().allowedModelIdx = buildModelIndex([]config.ModelMapping{{Upstream: "Real-Model", Fallback: "test-model"}})
	}
	now := time.Now()
	svc.affinity.SetWithTTL(sessionAffinityKey("tester", "ses_task", "Real-Model"), "cap-a", now, sessionAffinityTTL)
	svc.rrCounter.Store(1)
	sessionRoute(t, svc, "tester", "192.0.2.1", "ses_task", now, "cap-b", selectionRoundRobin)
	svc.affinity.SetWithTTL(sessionAffinityKey("tester", "ses_task", "test-model"), "cap-b", now, sessionAffinityTTL)
	sessionRoute(t, svc, "tester", "192.0.2.1", "ses_task", now, "cap-b", selectionAffinityHit)
}

func TestSessionAffinityDifferentUpstreamsSameAlias(t *testing.T) {
	svc := newSessionHTTPService(t, func(target *config.Target) {
		upstream := "Up-A"
		if target.Name == "t2" {
			upstream = "Up-B"
		}
		target.ModelMappings = []config.ModelMapping{{Upstream: upstream, Fallback: "test-model"}}
	})
	svc.rrCounter.Store(1) // First session starts on the non-first target.
	if got := sendSessionModelRequest(t, svc, &auth.Principal{Name: "tester"}, "192.0.2.1", "ses_task", "Test-MODEL"); got != "t2" {
		t.Fatalf("first route=%q, want non-first target t2", got)
	}
	ageSessionEntries(svc, 10*time.Minute)
	if got := sendSessionRequest(t, svc, "tester", "192.0.2.99", "ses_task"); got != "t2" {
		t.Fatalf("different upstream mapping lost session: target=%q, want t2", got)
	}
	if hits := capState(t, svc, "t2").affinityHitCount(); hits != 1 {
		t.Fatalf("session did not use affinity selection: hits=%d", hits)
	}
}

type sessionAffinityTransport func(*http.Request) (*http.Response, error)

func (f sessionAffinityTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func sessionEntry(t *testing.T, svc *Service, key string) affinityEntry {
	t.Helper()
	svc.affinity.mu.RLock()
	defer svc.affinity.mu.RUnlock()
	entry, ok := svc.affinity.entries[key]
	if !ok {
		t.Fatalf("session entry %q missing", key)
	}
	return entry
}

func TestSessionAffinityForwardFailoverPreservesBinding(t *testing.T) {
	for _, reason := range []string{"hard-cap", "network"} {
		t.Run(reason, func(t *testing.T) {
			svc := newSessionHTTPService(t, func(target *config.Target) { target.MaxConcurrency = 1 })
			sendSessionRequest(t, svc, "tester", "192.0.2.1", "ses_task")
			key := sessionAffinityKey("tester", "ses_task", "test-model")
			before := sessionEntry(t, svc, key)
			a := capState(t, svc, "t1")
			var networkFailed atomic.Bool
			heldSlot := false
			if reason == "hard-cap" {
				fill(t, a, 1)
				heldSlot = true
				t.Cleanup(func() {
					if heldSlot {
						a.releaseSlot()
					}
				})
			} else {
				base := svc.httpClient.Transport
				host := a.Target().Endpoint.Host
				svc.httpClient.Transport = sessionAffinityTransport(func(r *http.Request) (*http.Response, error) {
					if r.URL.Host == host && networkFailed.Load() {
						return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
					}
					return base.RoundTrip(r)
				})
				networkFailed.Store(true)
			}
			if got := sendSessionRequest(t, svc, "tester", "192.0.2.1", "ses_task"); got != "t2" {
				t.Fatalf("temporary failure did not fail over successfully: %q", got)
			}
			if svc.MetricsSnapshot().TotalTargetRetries != 1 {
				t.Fatal("test did not exercise forwarding-stage failover")
			}
			if got := sessionEntry(t, svc, key); got != before {
				t.Errorf("temporary %s failure overwrote session binding: %q -> %q", reason, before.targetName, got.targetName)
			}
			// The existing IP write policy is intentionally unchanged.
			if got, _ := svc.affinity.Get(affinityKey("192.0.2.1", "tester", "test-model"), time.Now()); got != "t2" {
				t.Fatalf("IP affinity write behavior changed: %q", got)
			}
			if heldSlot {
				a.releaseSlot()
				heldSlot = false
			}
			networkFailed.Store(false)
			a.MarkSuccess(time.Now())
			if got := sendSessionRequest(t, svc, "tester", "192.0.2.99", "ses_task"); got != "t1" {
				t.Fatalf("session did not return to recovered original target: %q", got)
			}
		})
	}
}

func TestSessionAffinityPermanentInvalidationRebinds(t *testing.T) {
	for _, reason := range []string{"removed", "model", "client"} {
		t.Run(reason, func(t *testing.T) {
			svc := newSessionHTTPService(t)
			sendSessionRequest(t, svc, "tester", "192.0.2.1", "ses_task")
			principal := &auth.Principal{Name: "tester"}
			if reason == "client" {
				store := auth.NewStore()
				if err := store.LoadFromConfig(testAuthClients("tester", "fake-client-key", "t2")); err != nil {
					t.Fatal(err)
				}
				principal, _ = store.Authenticate("fake-client-key")
			} else {
				var targets []config.Target
				for _, state := range svc.targetSnapshot() {
					target := state.Target()
					if reason == "removed" && target.Name == "t1" {
						continue
					}
					updated := config.Target{Name: target.Name, EndpointType: target.EndpointType, Endpoint: target.Endpoint.String(), APIKey: target.APIKey}
					if reason == "model" && target.Name == "t1" {
						updated.ModelMappings = []config.ModelMapping{{Upstream: "other-model"}}
					}
					targets = append(targets, updated)
				}
				if err := svc.UpdateTargets(targets); err != nil {
					t.Fatal(err)
				}
			}
			if got := sendSessionModelRequest(t, svc, principal, "192.0.2.1", "ses_task", "test-model"); got != "t2" {
				t.Fatalf("permanently invalid target blocked request: %q", got)
			}
			key := sessionAffinityKey("tester", "ses_task", "test-model")
			if got := sessionEntry(t, svc, key); got.targetName != "t2" {
				t.Fatalf("permanent %s invalidation did not rebind: target=%q", reason, got.targetName)
			}
			if got := sendSessionModelRequest(t, svc, principal, "192.0.2.99", "ses_task", "test-model"); got != "t2" {
				t.Fatalf("new binding did not survive IP change: %q", got)
			}
		})
	}
}

func TestSessionAffinityKeyUnambiguous(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		principal1, session1, model1 string
		principal2, session2, model2 string
	}{
		{"principal-session-pipe", "a|b", "c", "model", "a", "b|c", "model"},
		{"session-model-colon", "client", "ses:model", "suffix", "client", "ses", "model:suffix"},
		{"session-model-pipe-colon", "client", "ses|model:part", "suffix|tail", "client", "ses|model", "part:suffix|tail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := sessionAffinityKey(tc.principal1, tc.session1, tc.model1)
			second := sessionAffinityKey(tc.principal2, tc.session2, tc.model2)
			if first == second {
				t.Fatalf("distinct identities collide: %q", first)
			}
			a := newAffinityMap()
			now := time.Now()
			a.SetWithTTL(first, "t1", now, sessionAffinityTTL)
			a.SetWithTTL(second, "t2", now, sessionAffinityTTL)
			if got, ok := a.Get(first, now); !ok || got != "t1" {
				t.Fatalf("second identity overwrote first binding: %q %v", got, ok)
			}
		})
	}
}

func TestSessionAffinityInvalidHeadersUseIPRouting(t *testing.T) {
	for _, session := range []string{strings.Repeat("s", 257), "ses\x00task"} {
		svc := newAffinityCapService(t, 10, 10)
		t.Cleanup(func() { _ = svc.Close() })
		now := time.Now()
		svc.affinity.Set(affinityKey("192.0.2.1", "tester", "test-model"), "cap-a", now)
		// Control characters cannot reach a real net/http handler. Exercise the
		// extraction and routing boundary without sending invalid wire headers.
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("X-Session-Id", session)
		svc.rrCounter.Store(1)
		sessionRoute(t, svc, "tester", "192.0.2.1", sessionAffinityID(r), now.Add(10*time.Minute), "cap-b", selectionRoundRobin)
	}
	// A long (but wire-valid) header must not establish a session entry either.
	svc := newSessionHTTPService(t)
	session := strings.Repeat("s", 257)
	sendSessionRequest(t, svc, "tester", "192.0.2.1", session)
	svc.affinity.mu.RLock()
	count := len(svc.affinity.entries)
	svc.affinity.mu.RUnlock()
	if count != 1 {
		t.Fatalf("invalid session created extra affinity: count=%d", count)
	}
	ageSessionEntries(svc, 10*time.Minute)
	if got := sendSessionRequest(t, svc, "tester", "192.0.2.1", session); got != "t2" {
		t.Fatalf("invalid session should use expired IP routing, got %q", got)
	}
}

func TestSessionAffinityEntryTTLAndEviction(t *testing.T) {
	now := time.Now()
	a := newAffinityMap()
	a.maxEntries = 3
	a.Set("ip", "t1", now)
	a.SetWithTTL("session", "t2", now, sessionAffinityTTL)
	a.SetWithTTL("expired-session", "t1", now.Add(-25*time.Hour), sessionAffinityTTL)
	a.Set("new-ip", "t1", now.Add(10*time.Minute)) // capacity scan must use each entry's TTL
	if got, ok := a.Get("session", now.Add(10*time.Minute)); !ok || got != "t2" {
		t.Fatalf("capacity eviction incorrectly expired session: %q %v", got, ok)
	}
	if _, ok := a.Get("ip", now.Add(10*time.Minute)); ok {
		t.Fatal("default IP TTL did not expire")
	}
	if _, ok := a.entries["expired-session"]; ok {
		t.Fatal("expired custom TTL not removed by capacity scan")
	}
	if _, ok := a.Get("session", now.Add(24*time.Hour+time.Second)); ok {
		t.Fatal("session survived 24-hour expiry")
	}
	// Zero-value TTL remains compatible with pre-existing entries.
	a.entries["legacy"] = affinityEntry{targetName: "t1", lastUsed: now}
	if _, ok := a.Get("legacy", now.Add(4*time.Minute)); !ok {
		t.Fatal("legacy entry expired too early")
	}
	if _, ok := a.Get("legacy", now.Add(6*time.Minute)); ok {
		t.Fatal("legacy entry did not use default TTL")
	}
	// Refresh on successful writes slides the 24-hour window.
	a.SetWithTTL("session", "t1", now, sessionAffinityTTL)
	a.SetWithTTL("session", "t1", now.Add(23*time.Hour), sessionAffinityTTL)
	if _, ok := a.Get("session", now.Add(25*time.Hour)); !ok {
		t.Fatal("session TTL did not slide on write")
	}
}
