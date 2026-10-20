package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// ---------- buildModelIndex：暂停映射不进入模型索引 ----------

func TestBuildModelIndexSkipsPausedMappings(t *testing.T) {
	mappings := []config.ModelMapping{
		{Upstream: "gpt-4o", Fallback: "gpt4o-alias"},
		{Upstream: "gpt-3.5-turbo", Fallback: "gpt35-alias", Paused: true},
	}
	idx := buildModelIndex(mappings)
	if idx == nil {
		t.Fatal("index must not be nil when mappings are configured")
	}
	if _, ok := idx["gpt-4o"]; !ok {
		t.Errorf("active upstream missing from index: %v", idx)
	}
	if _, ok := idx["gpt4o-alias"]; !ok {
		t.Errorf("active alias missing from index: %v", idx)
	}
	if _, ok := idx["gpt-3.5-turbo"]; ok {
		t.Errorf("paused upstream must be excluded: %v", idx)
	}
	if _, ok := idx["gpt35-alias"]; ok {
		t.Errorf("paused alias must be excluded: %v", idx)
	}
}

// TestBuildModelIndexAllPausedIsEmptyNotNil guards the critical edge: a target
// whose mappings are ALL paused must keep model restrictions (empty non-nil
// index rejecting everything) and must never degrade to "unrestricted" (nil).
func TestBuildModelIndexAllPausedIsEmptyNotNil(t *testing.T) {
	idx := buildModelIndex([]config.ModelMapping{{Upstream: "gpt-4o", Paused: true}})
	if idx == nil {
		t.Fatal("all-paused target must produce an empty NON-NIL index")
	}
	if len(idx) != 0 {
		t.Fatalf("all-paused index must be empty, got %v", idx)
	}
	if none := buildModelIndex(nil); none != nil {
		t.Fatalf("no mappings must stay nil (unrestricted), got %v", none)
	}
}

func TestEnsureModelAllowedAllPausedRejectsEverything(t *testing.T) {
	s := &Service{}
	allPaused := &Target{
		Name:            "t-paused",
		ModelMappings:   []config.ModelMapping{{Upstream: "gpt-4o", Paused: true}},
		allowedModelIdx: buildModelIndex([]config.ModelMapping{{Upstream: "gpt-4o", Paused: true}}),
	}
	if _, err := s.ensureModelAllowed(allPaused, "gpt-4o"); err == nil {
		t.Fatal("all-paused target must reject its own paused model")
	}
	if _, err := s.ensureModelAllowed(allPaused, "anything-else"); err == nil {
		t.Fatal("all-paused target must reject every model")
	}
	if modelAllowed(allPaused, "gpt-4o") {
		t.Fatal("modelAllowed must be false on all-paused target")
	}
	unrestricted := &Target{Name: "t-open", allowedModelIdx: buildModelIndex(nil)}
	if _, err := s.ensureModelAllowed(unrestricted, "whatever"); err != nil {
		t.Fatalf("unrestricted target must allow any model, got %v", err)
	}
}

// ---------- /v1/models：暂停模型（含别名）不对外暴露 ----------

func TestBuildLocalDeploymentsHidesPausedModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{
			{
				Name: "t1", Endpoint: upstream.URL, ResourcePathPrefix: "/", APIKey: "k",
				ModelMappings: []config.ModelMapping{
					{Upstream: "active-model", Fallback: "active-alias"},
					{Upstream: "paused-model", Fallback: "paused-alias", Paused: true},
				},
			},
		},
		Logging: config.LoggingConfig{Level: "error"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	entries := service.buildLocalDeployments(nil)
	ids := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if id, ok := entry["id"].(string); ok {
			ids[id] = true
		}
	}
	if !ids["active-model"] || !ids["active-alias"] {
		t.Errorf("active model and alias must be listed, got %v", ids)
	}
	if ids["paused-model"] || ids["paused-alias"] {
		t.Errorf("paused model and alias must be hidden, got %v", ids)
	}
}

// ---------- 服务级：暂停只影响可用性，不影响负载均衡语义 ----------

func newPauseTestService(t *testing.T, aPaused, bPaused bool) (*Service, *auth.Principal, *httptest.Server, *httptest.Server) {
	t.Helper()
	mk := func(tag string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"mock":"` + tag + `"}`))
		}))
	}
	serverA, serverB := mk("a"), mk("b")
	aURL, _ := url.Parse(serverA.URL)
	bURL, _ := url.Parse(serverB.URL)
	if aURL == nil || bURL == nil {
		t.Fatal("parse upstream urls")
	}
	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{
			{Name: "target-a", Endpoint: serverA.URL, ResourcePathPrefix: "/", APIKey: "key-a",
				ModelMappings: []config.ModelMapping{{Upstream: "qwen3.8-max", Fallback: "qwen-alias", Paused: aPaused}}},
			{Name: "target-b", Endpoint: serverB.URL, ResourcePathPrefix: "/", APIKey: "key-b",
				ModelMappings: []config.ModelMapping{{Upstream: "qwen3.8-max", Fallback: "qwen-alias", Paused: bPaused}}},
		},
		Logging: config.LoggingConfig{Level: "error", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.quietPeriod = 50 * time.Millisecond
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected authenticated principal")
	}
	return service, principal, serverA, serverB
}

func sendPauseTestRequest(t *testing.T, service *Service, principal *auth.Principal, model string) *httptest.ResponseRecorder {
	t.Helper()
	body := bytes.NewBufferString(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)
	return rr
}

// A 暂停、B 可用：请求必须由 B 服务（暂停 = 该目标上不可请求，其余目标照常）。
func TestPausedModelServedByOtherTarget(t *testing.T) {
	service, principal, serverA, serverB := newPauseTestService(t, true, false)
	defer serverA.Close()
	defer serverB.Close()
	rr := sendPauseTestRequest(t, service, principal, "qwen3.8-max")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"mock":"b"`) {
		t.Fatalf("paused target A must be skipped, want response from B, got %s", rr.Body.String())
	}
}

// 全部目标都暂停该模型：走现有「无可用目标」错误路径，绝不放行。
func TestModelPausedOnAllTargetsRejected(t *testing.T) {
	service, principal, serverA, serverB := newPauseTestService(t, true, true)
	defer serverA.Close()
	defer serverB.Close()
	rr := sendPauseTestRequest(t, service, principal, "qwen3.8-max")
	if rr.Code == http.StatusOK {
		t.Fatalf("all-paused model must not be served, got 200: %s", rr.Body.String())
	}
}

func TestPausedModelAliasRejectsExplicitTargetAndRoutesToActiveTarget(t *testing.T) {
	service, principal, serverA, serverB := newPauseTestService(t, true, false)
	defer serverA.Close()
	defer serverB.Close()

	request := func(target string) *httptest.ResponseRecorder {
		t.Helper()
		body := bytes.NewBufferString(`{"model":"qwen-alias","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
		req.Header.Set(headerProxyTarget, target)
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		recorder := httptest.NewRecorder()
		service.ServeHTTP(recorder, req)
		return recorder
	}

	if blocked := request("target-a"); blocked.Code != http.StatusBadRequest {
		t.Fatalf("paused alias on explicitly selected target: status = %d, want 400; body=%s", blocked.Code, blocked.Body.String())
	}
	if active := request("target-b"); active.Code != http.StatusOK || !strings.Contains(active.Body.String(), `"mock":"b"`) {
		t.Fatalf("active alias on explicitly selected target: status = %d, body=%s", active.Code, active.Body.String())
	}
	if automatic := request(""); automatic.Code != http.StatusOK || !strings.Contains(automatic.Body.String(), `"mock":"b"`) {
		t.Fatalf("automatic alias routing must skip paused target: status = %d, body=%s", automatic.Code, automatic.Body.String())
	}
}

func TestPausedModelConnectivityTestIsExempt(t *testing.T) {
	service, _, serverA, serverB := newPauseTestService(t, true, true)
	defer serverA.Close()
	defer serverB.Close()

	result, err := service.TestModel("target-a", "qwen3.8-max")
	if err != nil || result == nil || !result.Success {
		t.Fatalf("connectivity test must reach paused model: result=%+v, err=%v", result, err)
	}
}
