package proxy

import (
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func preparedTestTarget(name string, keys ...string) config.Target {
	return config.Target{Name: name, EndpointType: "openai", Endpoint: "https://" + name + ".example.com", APIKey: keys[0], APIKeys: keys[1:], UsageAdapter: config.UsageAdapterStandard}
}

func newPreparedTestService(t *testing.T, cfg *config.Config) *Service {
	t.Helper()
	s, err := NewService(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func runtimeTargetNames(s *Service) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.targetOrder))
	for _, st := range s.targetOrder {
		names = append(names, st.target.Name+"|"+st.target.Endpoint.String()+"|"+boolString(st.target.Paused))
	}
	return names
}

func boolString(b bool) string {
	if b {
		return "paused"
	}
	return "live"
}

func TestPrepareConfigDoesNotTouchRuntime(t *testing.T) {
	s := newPreparedTestService(t, &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{preparedTestTarget("a", "ka1", "ka2")}})
	before := runtimeTargetNames(s)
	beforeTimeout := s.requestTimeout.Load()
	beforeRev := func() uint64 { s.mu.RLock(); defer s.mu.RUnlock(); return s.targetRevision }()

	next := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 9}, Targets: []config.Target{preparedTestTarget("a", "ka1", "ka2"), preparedTestTarget("b", "kb")}}
	prepared, err := s.PrepareConfig(next)
	if err != nil || prepared == nil {
		t.Fatalf("PrepareConfig: %v", err)
	}
	if got := runtimeTargetNames(s); !reflect.DeepEqual(got, before) {
		t.Fatalf("PrepareConfig changed runtime targets: %v -> %v", before, got)
	}
	if s.requestTimeout.Load() != beforeTimeout {
		t.Fatal("PrepareConfig changed the request timeout")
	}
	s.mu.RLock()
	rev := s.targetRevision
	s.mu.RUnlock()
	if rev != beforeRev {
		t.Fatal("PrepareConfig advanced the target revision")
	}
	if _, err := s.PrepareConfig(&config.Config{Targets: []config.Target{{Name: "bad", Endpoint: "not a url", APIKey: "k"}}}); err == nil {
		t.Fatal("PrepareConfig accepted an invalid endpoint")
	}
	if _, err := s.PrepareConfig(nil); err == nil {
		t.Fatal("PrepareConfig accepted nil")
	}
}

func TestCommitPreparedMatchesApplyConfig(t *testing.T) {
	base := func() *config.Config {
		return &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{preparedTestTarget("a", "ka1", "ka2")}}
	}
	next := func() *config.Config {
		return &config.Config{
			Server:  config.ServerConfig{RequestTimeoutSeconds: 9},
			Pools:   []config.DeploymentPool{{Name: "p", Paused: true}},
			Targets: []config.Target{func() config.Target { t := preparedTestTarget("a", "ka1", "ka2"); t.Pool = "p"; return t }(), preparedTestTarget("b", "kb")},
		}
	}
	viaApply := newPreparedTestService(t, base())
	if err := viaApply.ApplyConfig(next()); err != nil {
		t.Fatal(err)
	}
	viaPrepared := newPreparedTestService(t, base())
	prepared, err := viaPrepared.PrepareConfig(next())
	if err != nil {
		t.Fatal(err)
	}
	viaPrepared.CommitPrepared(prepared)

	if a, b := runtimeTargetNames(viaApply), runtimeTargetNames(viaPrepared); !reflect.DeepEqual(a, b) || len(a) != 2 {
		t.Fatalf("runtime differs: apply=%v prepared=%v", a, b)
	}
	if want := (9 * time.Second).Nanoseconds(); viaPrepared.requestTimeout.Load() != want || viaApply.requestTimeout.Load() != want {
		t.Fatalf("timeout apply=%d prepared=%d want=%d", viaApply.requestTimeout.Load(), viaPrepared.requestTimeout.Load(), want)
	}
	if !runtimeIsPaused(viaPrepared, "a") {
		t.Fatal("pool pause was not materialized by PrepareConfig")
	}
}

func runtimeIsPaused(s *Service, name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.targetsByName[name].target.Paused
}

func TestSetKeyBlockedByValue(t *testing.T) {
	s := newPreparedTestService(t, &config.Config{Targets: []config.Target{preparedTestTarget("a", "k1", "k2", "k3")}})
	s.mu.RLock()
	revBefore := s.targetRevision
	s.mu.RUnlock()

	if err := s.SetKeyBlockedByValue("A", "k2", 0, true); err != nil {
		t.Fatal(err)
	}
	blocked := func() []bool {
		var out []bool
		for _, st := range s.KeyPoolStatus("a") {
			out = append(out, st.Blocked)
		}
		return out
	}
	if got := blocked(); !reflect.DeepEqual(got, []bool{false, true, false}) {
		t.Fatalf("blocked = %v", got)
	}
	if err := s.SetKeyBlockedByValue("a", "k2", 0, false); err != nil {
		t.Fatal(err)
	}
	if got := blocked(); !reflect.DeepEqual(got, []bool{false, false, false}) {
		t.Fatalf("blocked after unblock = %v", got)
	}
	if err := s.SetKeyBlockedByValue("a", "k3", 0, true); err != nil {
		t.Fatal(err)
	}
	if got := blocked(); !reflect.DeepEqual(got, []bool{false, false, true}) {
		t.Fatalf("blocked k3 = %v", got)
	}
	s.mu.RLock()
	revAfter := s.targetRevision
	s.mu.RUnlock()
	if revAfter != revBefore {
		t.Fatalf("by-value block advanced the global runtime revision: %d -> %d", revBefore, revAfter)
	}

	for name, call := range map[string]func() error{
		"occurrence beyond pool": func() error { return s.SetKeyBlockedByValue("a", "k1", 1, true) },
		"negative occurrence":    func() error { return s.SetKeyBlockedByValue("a", "k1", -1, true) },
		"unknown key":            func() error { return s.SetKeyBlockedByValue("a", "nope", 0, true) },
		"unknown target":         func() error { return s.SetKeyBlockedByValue("ghost", "k1", 0, true) },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if got := blocked(); !reflect.DeepEqual(got, []bool{false, false, true}) {
		t.Fatalf("failed calls changed blocking: %v", got)
	}
}

// 旧 X-Target-Key-Snapshot 路径沿用全局运行时 revision：任何目标的发布都会使旧快照失效，
// 这是预期行为（只影响升级期间残留的旧标签页）；按值封禁不推进 revision，旧快照仍可用。
func TestLegacySnapshotUsesGlobalRevision(t *testing.T) {
	cfg := &config.Config{Targets: []config.Target{preparedTestTarget("a", "k1", "k2"), preparedTestTarget("b", "kb")}}
	s := newPreparedTestService(t, cfg)
	a := cfg.Targets[0]
	revision, _, _, ok := s.TargetKeySnapshot(a)
	if !ok {
		t.Fatal("snapshot unavailable")
	}

	if err := s.SetKeyBlockedByValue("a", "k2", 0, true); err != nil {
		t.Fatal(err)
	}
	if rev, _, _, ok := s.TargetKeySnapshot(a); !ok || rev != revision {
		t.Fatalf("by-value block invalidated the snapshot: %d -> %d ok=%v", revision, rev, ok)
	}

	next := &config.Config{Targets: []config.Target{preparedTestTarget("a", "k1", "k2"), preparedTestTarget("b", "kb2")}}
	prepared, err := s.PrepareConfig(next)
	if err != nil {
		t.Fatal(err)
	}
	s.CommitPrepared(prepared)
	if err := s.SetKeyBlockedIfTargetRevision("a", 1, false, revision); !errors.Is(err, ErrTargetSnapshotChanged) {
		t.Fatalf("snapshot survived another target's publish: %v", err)
	}
}
