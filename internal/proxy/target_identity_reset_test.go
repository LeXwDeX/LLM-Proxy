package proxy

import (
	"reflect"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// #184：目标熔断状态只在“上游连接身份”变化或 resume 时按新代际重置。
// 下面的变异表同时是字段分类的权威清单；TestHealthIdentityFieldClassificationExhaustive
// 要求 proxy.Target 的每个导出字段都在表中，防止新增字段漏分类。

type targetMutation struct {
	identity bool
	mutate   func(*config.Target)
}

var targetFieldMutations = map[string]targetMutation{
	// 身份字段：改变出站连接目标、凭据、认证或上游模型名。
	"EndpointType":    {true, func(t *config.Target) { t.EndpointType = config.EndpointTypeDualProtocol }},
	"Endpoint":        {true, func(t *config.Target) { t.Endpoint = "http://127.0.0.1:2" }},
	"APIKey":          {true, func(t *config.Target) { t.APIKey = "k-replaced" }},
	"APIKeys":         {true, func(t *config.Target) { t.APIKeys = append(t.APIKeys, "k-added") }},
	"AuthMode":        {true, func(t *config.Target) { t.AuthMode = "bearer" }},
	"AllowBearer":     {true, func(t *config.Target) { t.AllowBearer = true }},
	"ModelPrefix":     {true, func(t *config.Target) { t.ModelPrefix = "vendor/" }},
	"OpenAIPrefix":    {true, func(t *config.Target) { t.OpenAIPrefix = "/compat" }},
	"AnthropicPrefix": {true, func(t *config.Target) { t.AnthropicPrefix = "/anthropic" }},
	"PreferWebSocket": {true, func(t *config.Target) { t.PreferWebSocket = true }},
	"CustomHeaders":   {true, func(t *config.Target) { t.CustomHeaders = map[string]string{"X-A": "2"} }},
	"CustomBody":      {true, func(t *config.Target) { t.CustomBody = map[string]any{"a": float64(2)} }},

	// 非身份字段：调度、计量或展示配置，熔断保持。
	"Name":                        {false, func(t *config.Target) { t.Name = "A" }},
	"UsageAdapter":                {false, func(t *config.Target) { t.UsageAdapter = config.UsageAdapterQwen }},
	"KeyResetTime":                {false, func(t *config.Target) { t.KeyResetTime = "23" }},
	"Paused":                      {false, func(t *config.Target) { t.Paused = true }},
	"Pool":                        {false, func(t *config.Target) { t.Pool = "other-pool" }},
	"MaxConcurrency":              {false, func(t *config.Target) { t.MaxConcurrency = 9 }},
	"ImageOperation":              {false, func(t *config.Target) { t.ImageOperation = config.ImageOperationGenerations }},
	"ModelMappings":               {false, func(t *config.Target) { t.ModelMappings = append(t.ModelMappings, config.ModelMapping{Upstream: "m2"}) }},
	"SupportsResponses":           {false, func(t *config.Target) { t.SupportsResponses = true }},
	"RequestStreamUsageIfMissing": {false, func(t *config.Target) { t.RequestStreamUsageIfMissing = true }},
}

func identityBaseConfig() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{
			{
				Name: "a", EndpointType: config.EndpointTypeOpenAI, Endpoint: "http://127.0.0.1:1",
				UsageAdapter: config.UsageAdapterStandard,
				APIKey:       "k1", APIKeys: []string{"k2"},
				CustomHeaders: map[string]string{"X-A": "1"},
				CustomBody:    map[string]any{"a": float64(1)},
				ModelMappings: []config.ModelMapping{{Upstream: "m"}},
			},
			{
				Name: "other", EndpointType: config.EndpointTypeOpenAI, Endpoint: "http://127.0.0.1:3",
				UsageAdapter: config.UsageAdapterStandard, APIKey: "ko",
				ModelMappings: []config.ModelMapping{{Upstream: "m"}},
			},
		},
	}
}

func newIdentityService(t *testing.T, cfg *config.Config) *Service {
	t.Helper()
	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func publishConfig(t *testing.T, svc *Service, cfg *config.Config, via string) {
	t.Helper()
	var err error
	if via == "ApplyConfig" {
		err = svc.ApplyConfig(cfg)
	} else {
		err = svc.UpdateTargets(cfg.Targets)
	}
	if err != nil {
		t.Fatalf("%s: %v", via, err)
	}
}

type armedBreaker struct {
	lease *probeLease
	stats TargetStats
	phase string
}

// armBreaker puts the target into a failed state. "muted" has an escalated
// cooldown; "half-open-lease" holds the exclusive probe lease.
func armBreaker(t *testing.T, st *targetState, scenario string) armedBreaker {
	t.Helper()
	now := time.Now()
	var lease *probeLease
	switch scenario {
	case "muted":
		for i := 0; i < 3; i++ {
			st.MarkFailure(now, time.Minute)
		}
	case "half-open-lease":
		for i := 0; i < 2; i++ {
			st.MarkFailure(now.Add(-time.Hour), time.Second)
		}
		decision, l := st.admitWithProbeState(now)
		if decision != admitOpen || l == nil {
			t.Fatal("probe lease not acquired")
		}
		lease = l
	default:
		t.Fatalf("unknown scenario %q", scenario)
	}
	st.affinityHits.Add(3)
	armed := armedBreaker{lease: lease, stats: st.Stats(), phase: st.Phase(now)}
	if armed.phase == phaseReady || armed.stats.TotalFailure == 0 {
		t.Fatalf("fixture not failed: %+v", armed)
	}
	return armed
}

var breakerScenarios = []string{"muted", "half-open-lease"}
var publishPaths = []string{"ApplyConfig", "UpdateTargets"}

func assertBreakerFresh(t *testing.T, svc *Service, old, current *targetState, armed armedBreaker) {
	t.Helper()
	if current == old {
		t.Fatal("reload did not publish a new target generation")
	}
	if current.targetHealthState == old.targetHealthState {
		t.Fatal("identity change must not share the previous breaker state")
	}
	now := time.Now()
	stats := current.Stats()
	if got := current.Phase(now); got != phaseReady {
		t.Fatalf("phase = %s, want ready", got)
	}
	if stats.ConsecutiveFailure != 0 || !stats.MutedUntil.IsZero() {
		t.Fatalf("streak/mute not cleared: %+v", stats)
	}
	if current.probeLease.Load() != nil || current.probeSlotBusy(now) {
		t.Fatal("probe lease carried into the new identity")
	}
	if stats.TotalFailure != armed.stats.TotalFailure || stats.TotalSuccess != armed.stats.TotalSuccess ||
		!stats.LastFailure.Equal(armed.stats.LastFailure) || !stats.LastSuccess.Equal(armed.stats.LastSuccess) {
		t.Fatalf("cumulative counters not copied: old=%+v new=%+v", armed.stats, stats)
	}
	if current.breakerTripCount() != old.breakerTripCount() || current.affinityHitCount() != 3 {
		t.Fatalf("trip/affinity counters not copied: trips %d/%d hits %d", current.breakerTripCount(), old.breakerTripCount(), current.affinityHitCount())
	}
	if current.breakerThreshold != old.breakerThreshold {
		t.Fatal("breaker threshold not copied")
	}
	if current.inFlight != old.inFlight || current.runtimeWindow != old.runtimeWindow {
		t.Fatal("in-flight counter and runtime window must stay shared")
	}
	// The previous generation keeps its own failed state for late results.
	if old.Phase(now) == phaseReady {
		t.Fatal("old generation breaker was cleared")
	}
}

func assertSelectable(t *testing.T, svc *Service, want *targetState) {
	t.Helper()
	principal := newAllowAllPrincipal(t, "identity-client")
	selected, lease, _, err := svc.selectTarget(principal, "127.0.0.1", "", "", nil, nil, "m", "/v1/chat/completions", time.Now())
	if err != nil {
		t.Fatalf("selectTarget after reset: %v", err)
	}
	if selected != want {
		t.Fatalf("selected %p, want %p", selected, want)
	}
	selected.releaseProbe(lease)
}

func TestTargetIdentityChangeResetsBreaker(t *testing.T) {
	for field, m := range targetFieldMutations {
		if !m.identity {
			continue
		}
		for _, via := range publishPaths {
			for _, scenario := range breakerScenarios {
				t.Run(field+"/"+via+"/"+scenario, func(t *testing.T) {
					cfg := identityBaseConfig()
					svc := newIdentityService(t, cfg)
					old := svc.targetsByName["a"]
					armed := armBreaker(t, old, scenario)
					m.mutate(&cfg.Targets[0])
					publishConfig(t, svc, cfg, via)
					current := svc.targetsByName["a"]
					assertBreakerFresh(t, svc, old, current, armed)
					// Other target stays untouched and shared with itself.
					if svc.targetsByName["other"].Phase(time.Now()) != phaseReady {
						t.Fatal("unrelated target changed")
					}
					assertSelectable(t, svc, current)
				})
			}
		}
	}
}

func TestTargetIdentityChangeVariants(t *testing.T) {
	cases := map[string]func(*config.Target){
		"replace-all-credentials": func(t *config.Target) { t.APIKey = "n1"; t.APIKeys = []string{"n2"} },
		"remove-key":              func(t *config.Target) { t.APIKeys = nil },
		"clear-custom-headers":    func(t *config.Target) { t.CustomHeaders = nil },
		"remove-custom-body-key":  func(t *config.Target) { t.CustomBody = map[string]any{} },
		"nested-custom-body":      func(t *config.Target) { t.CustomBody = map[string]any{"a": map[string]any{"x": "y"}} },
		"auth-mode-valid-change":  func(t *config.Target) { t.AuthMode = "bearer" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := identityBaseConfig()
			svc := newIdentityService(t, cfg)
			old := svc.targetsByName["a"]
			armed := armBreaker(t, old, "muted")
			mutate(&cfg.Targets[0])
			publishConfig(t, svc, cfg, "ApplyConfig")
			assertBreakerFresh(t, svc, old, svc.targetsByName["a"], armed)
		})
	}
}

func TestTargetNonIdentityChangeKeepsBreaker(t *testing.T) {
	for field, m := range targetFieldMutations {
		if m.identity {
			continue
		}
		for _, via := range publishPaths {
			for _, scenario := range breakerScenarios {
				t.Run(field+"/"+via+"/"+scenario, func(t *testing.T) {
					cfg := identityBaseConfig()
					svc := newIdentityService(t, cfg)
					old := svc.targetsByName["a"]
					armed := armBreaker(t, old, scenario)
					m.mutate(&cfg.Targets[0])
					publishConfig(t, svc, cfg, via)
					assertBreakerKept(t, old, svc.targetsByName["a"], armed)
				})
			}
		}
	}
}

func TestTargetNoopChangesKeepBreaker(t *testing.T) {
	cases := map[string]func(*config.Config){
		"same-config":                func(*config.Config) {},
		"reorder-keys":               func(c *config.Config) { c.Targets[0].APIKey = "k2"; c.Targets[0].APIKeys = []string{"k1"} },
		"duplicate-and-blank-keys":   func(c *config.Config) { c.Targets[0].APIKeys = []string{" k2 ", "", "k1", "k2"} },
		"nil-vs-empty-headers":       func(c *config.Config) { c.Targets[0].CustomHeaders = nil; c.Targets[0].CustomBody = nil },
		"prefix-normalization":       func(c *config.Config) { c.Targets[0].OpenAIPrefix = " /"; c.Targets[0].AnthropicPrefix = "" },
		"endpoint-whitespace":        func(c *config.Config) { c.Targets[0].Endpoint = " http://127.0.0.1:1 " },
		"edit-other-target-identity": func(c *config.Config) { c.Targets[1].APIKey = "ko2" },
		"edit-other-target-endpoint": func(c *config.Config) { c.Targets[1].Endpoint = "http://127.0.0.1:4" },
	}
	for name, mutate := range cases {
		for _, via := range publishPaths {
			t.Run(name+"/"+via, func(t *testing.T) {
				cfg := identityBaseConfig()
				cfg.Targets[0].CustomHeaders = map[string]string{}
				cfg.Targets[0].CustomBody = map[string]any{}
				svc := newIdentityService(t, cfg)
				old := svc.targetsByName["a"]
				armed := armBreaker(t, old, "muted")
				mutate(cfg)
				publishConfig(t, svc, cfg, via)
				assertBreakerKept(t, old, svc.targetsByName["a"], armed)
			})
		}
	}
}

func assertBreakerKept(t *testing.T, old, current *targetState, armed armedBreaker) {
	t.Helper()
	if current.targetHealthState != old.targetHealthState {
		t.Fatal("non-identity change must keep the shared breaker state")
	}
	if current.probeLease.Load() != armed.lease {
		t.Fatal("probe lease pointer changed")
	}
	if got := current.Stats(); got != armed.stats {
		t.Fatalf("breaker stats changed: before=%+v after=%+v", armed.stats, got)
	}
}

func TestTargetResumeResetsBreaker(t *testing.T) {
	t.Run("target resume", func(t *testing.T) {
		for _, via := range publishPaths {
			cfg := identityBaseConfig()
			cfg.Targets[0].Paused = true
			svc := newIdentityService(t, cfg)
			old := svc.targetsByName["a"]
			armed := armBreaker(t, old, "muted")
			cfg.Targets[0].Paused = false
			publishConfig(t, svc, cfg, via)
			assertBreakerFresh(t, svc, old, svc.targetsByName["a"], armed)
			assertSelectable(t, svc, svc.targetsByName["a"])
		}
	})
	poolCfg := func(poolPaused, targetPaused bool) *config.Config {
		cfg := identityBaseConfig()
		cfg.Targets[0].Pool = "p"
		cfg.Targets[0].Paused = targetPaused
		cfg.Pools = []config.DeploymentPool{{Name: "p", Paused: poolPaused}}
		return cfg
	}
	t.Run("pool resume", func(t *testing.T) {
		cfg := poolCfg(true, false)
		svc := newIdentityService(t, cfg)
		old := svc.targetsByName["a"]
		if !old.Target().Paused {
			t.Fatal("pool pause not materialized")
		}
		armed := armBreaker(t, old, "muted")
		cfg.Pools[0].Paused = false
		publishConfig(t, svc, cfg, "ApplyConfig")
		assertBreakerFresh(t, svc, old, svc.targetsByName["a"], armed)
		assertSelectable(t, svc, svc.targetsByName["a"])
	})
	t.Run("pool resume while target still paused keeps breaker", func(t *testing.T) {
		cfg := poolCfg(true, true)
		svc := newIdentityService(t, cfg)
		old := svc.targetsByName["a"]
		armed := armBreaker(t, old, "muted")
		cfg.Pools[0].Paused = false
		publishConfig(t, svc, cfg, "ApplyConfig")
		assertBreakerKept(t, old, svc.targetsByName["a"], armed)
		if !svc.targetsByName["a"].Target().Paused {
			t.Fatal("target must remain paused")
		}
	})
	t.Run("target resume while pool still paused keeps breaker", func(t *testing.T) {
		cfg := poolCfg(true, true)
		svc := newIdentityService(t, cfg)
		old := svc.targetsByName["a"]
		armed := armBreaker(t, old, "muted")
		cfg.Targets[0].Paused = false
		publishConfig(t, svc, cfg, "ApplyConfig")
		assertBreakerKept(t, old, svc.targetsByName["a"], armed)
	})
	t.Run("pause keeps breaker", func(t *testing.T) {
		for _, which := range []string{"target", "pool"} {
			cfg := poolCfg(false, false)
			svc := newIdentityService(t, cfg)
			old := svc.targetsByName["a"]
			armed := armBreaker(t, old, "muted")
			if which == "target" {
				cfg.Targets[0].Paused = true
			} else {
				cfg.Pools[0].Paused = true
			}
			publishConfig(t, svc, cfg, "ApplyConfig")
			if !svc.targetsByName["a"].Target().Paused {
				t.Fatalf("%s pause not applied", which)
			}
			assertBreakerKept(t, old, svc.targetsByName["a"], armed)
		}
	})
	t.Run("pause then resume resets", func(t *testing.T) {
		cfg := poolCfg(false, false)
		svc := newIdentityService(t, cfg)
		old := svc.targetsByName["a"]
		armed := armBreaker(t, old, "muted")
		cfg.Pools[0].Paused = true
		publishConfig(t, svc, cfg, "ApplyConfig")
		cfg.Pools[0].Paused = false
		publishConfig(t, svc, cfg, "ApplyConfig")
		assertBreakerFresh(t, svc, old, svc.targetsByName["a"], armed)
	})
}

func TestHealthIdentityFieldClassificationExhaustive(t *testing.T) {
	typ := reflect.TypeOf(Target{})
	seen := map[string]bool{}
	var missing []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		seen[f.Name] = true
		if _, ok := targetFieldMutations[f.Name]; !ok {
			missing = append(missing, f.Name)
		}
	}
	for name := range targetFieldMutations {
		if !seen[name] {
			t.Errorf("classification lists %q which is not a proxy.Target field", name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("proxy.Target fields without breaker identity classification: %v — add each to targetFieldMutations (identity or not) and to targetIdentityChanges when it is identity", missing)
	}
}

// Late outcomes of the previous generation must not write into the new one,
// while identity flips race with admission and settlement.
func TestIdentityFlipRacesWithSettlementAndAdmission(t *testing.T) {
	cfg := identityBaseConfig()
	svc := newIdentityService(t, cfg)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < 200; i++ {
			c := identityBaseConfig()
			if i%2 == 0 {
				c.Targets[0].APIKey = "flip"
			}
			if err := svc.ApplyConfig(c); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				st, ok := svc.targetByName("a")
				if !ok {
					t.Error("target vanished")
					return
				}
				now := time.Now()
				if _, lease := st.admitWithProbeState(now); lease != nil {
					st.releaseProbe(lease)
				}
				if w%2 == 0 {
					st.MarkFailure(now, time.Second)
				} else {
					st.MarkSuccess(now)
				}
				_ = st.Phase(now)
				_ = st.Stats()
			}
		}(w)
	}
	wg.Wait()
	final := svc.targetsByName["a"]
	if final.targetHealthState == nil || final.inFlight == nil {
		t.Fatal("final state incomplete")
	}
}

// With a threshold of one, every MarkFailure increments both counters under
// health.mu. A fork must observe a single consistent point in that sequence.
// Several readers contend with the writer to exercise the RUnlock handoff;
// repeated snapshots detect mixed generations without production test hooks.
func TestIdentityForkCopiesConsistentFailureCounters(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(0)
	if previousProcs < 2 {
		runtime.GOMAXPROCS(2)
		defer runtime.GOMAXPROCS(previousProcs)
	}
	old := newBreakerState(t, 1)
	started, stop := make(chan struct{}), make(chan struct{})
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		old.MarkFailure(time.Now(), time.Second)
		close(started)
		for {
			select {
			case <-stop:
				return
			default:
				old.MarkFailure(time.Now(), time.Second)
			}
		}
	}()
	defer func() { close(stop); writer.Wait() }()
	<-started
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for sample := 0; sample < 20000; sample++ {
				fresh := old.targetHealthState.forkForNewIdentity()
				trips := fresh.breakerTrips.Load()
				if fresh.totalFailure != trips {
					t.Errorf("fork mixed failure snapshots: totalFailure=%d breakerTrips=%d", fresh.totalFailure, trips)
					return
				}
			}
		}()
	}
	readers.Wait()
}
