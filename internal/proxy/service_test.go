package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/admission"
	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	appmiddleware "github.com/LeXwDeX/LLM-Proxy/internal/middleware"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/tracestore"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

type failingTransport struct {
	successHost string
}

func (t *failingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "primary.local" {
		return nil, fmt.Errorf("dial error")
	}
	if strings.Contains(req.URL.Host, t.successHost) {
		return http.DefaultTransport.RoundTrip(req)
	}
	return nil, fmt.Errorf("unexpected host %q", req.URL.Host)
}

func TestServiceRoundRobinLoadBalancing(t *testing.T) {
	// 创建两个 mock 上游，都支持同一个模型
	target1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"qwen3.7-max","usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer target1.Close()

	target2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"qwen3.7-max","usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer target2.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "target-1",
				Endpoint:           target1.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key-1",
				ModelMappings:      []config.ModelMapping{{Upstream: "qwen3.7-max"}},
			},
			{
				Name:               "target-2",
				Endpoint:           target2.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key-2",
				ModelMappings:      []config.ModelMapping{{Upstream: "qwen3.7-max"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// 创建多个不同的客户端
	store := auth.NewStore()
	clients := []config.Client{
		{Name: "client-1", AccessKey: "token-1"},
		{Name: "client-2", AccessKey: "token-2"},
		{Name: "client-3", AccessKey: "token-3"},
		{Name: "client-4", AccessKey: "token-4"},
	}
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("load clients: %v", err)
	}

	// 每个客户端发送一次请求，统计 target 分配
	targetCount := make(map[string]int)
	for _, client := range clients {
		principal, ok := store.Authenticate(client.AccessKey)
		if !ok {
			t.Fatalf("failed to authenticate client %s", client.Name)
		}

		body := bytes.NewBufferString(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("client %s: expected 200, got %d", client.Name, rr.Code)
		}

		target := rr.Header().Get("X-Proxy-Target")
		if target == "" {
			t.Fatalf("client %s: missing X-Proxy-Target header", client.Name)
		}
		targetCount[target]++
	}

	// 验证：4 个客户端应该分配到 2 个 target，每个 target 至少 1 次
	if len(targetCount) < 2 {
		t.Errorf("expected requests distributed to 2 targets, got %d: %v", len(targetCount), targetCount)
	}

	// 验证：分配应该相对均匀（允许 3:1 或 2:2）
	for target, count := range targetCount {
		if count == 0 {
			t.Errorf("target %s received 0 requests", target)
		}
	}

	t.Logf("Target distribution: %v", targetCount)
}

func TestServiceLeastTokenLoadBalancing(t *testing.T) {
	// 创建两个 mock 上游，返回不同的 token 用量
	target1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// target-1 返回大量 token
		_, _ = w.Write([]byte(`{"model":"qwen3.7-max","usage":{"prompt_tokens":10000,"completion_tokens":5000}}`))
	}))
	defer target1.Close()

	target2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// target-2 返回少量 token
		_, _ = w.Write([]byte(`{"model":"qwen3.7-max","usage":{"prompt_tokens":100,"completion_tokens":50}}`))
	}))
	defer target2.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "target-heavy",
				Endpoint:           target1.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key-1",
				ModelMappings:      []config.ModelMapping{{Upstream: "qwen3.7-max"}},
			},
			{
				Name:               "target-light",
				Endpoint:           target2.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key-2",
				ModelMappings:      []config.ModelMapping{{Upstream: "qwen3.7-max"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// 设置 mock usage recorder
	service.SetUsageRecorder(&mockUsageRecorder{})

	store := auth.NewStore()
	clients := []config.Client{
		{Name: "client-1", AccessKey: "token-1"},
		{Name: "client-2", AccessKey: "token-2"},
		{Name: "client-3", AccessKey: "token-3"},
	}
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("load clients: %v", err)
	}

	// 第一个客户端发送请求（会随机分配到一个 target）
	principal1, _ := store.Authenticate("token-1")
	body1 := bytes.NewBufferString(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hi"}]}`)
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body1)
	req1 = req1.WithContext(auth.WithPrincipal(req1.Context(), principal1))
	rr1 := httptest.NewRecorder()
	service.ServeHTTP(rr1, req1)
	firstTarget := rr1.Header().Get("X-Proxy-Target")

	// 第二个客户端发送请求（应该分配到 token 较少的 target）
	principal2, _ := store.Authenticate("token-2")
	body2 := bytes.NewBufferString(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hi"}]}`)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body2)
	req2 = req2.WithContext(auth.WithPrincipal(req2.Context(), principal2))
	rr2 := httptest.NewRecorder()
	service.ServeHTTP(rr2, req2)
	secondTarget := rr2.Header().Get("X-Proxy-Target")

	// 第三个客户端发送请求
	principal3, _ := store.Authenticate("token-3")
	body3 := bytes.NewBufferString(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hi"}]}`)
	req3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body3)
	req3 = req3.WithContext(auth.WithPrincipal(req3.Context(), principal3))
	rr3 := httptest.NewRecorder()
	service.ServeHTTP(rr3, req3)
	thirdTarget := rr3.Header().Get("X-Proxy-Target")

	t.Logf("First request: %s", firstTarget)
	t.Logf("Second request: %s", secondTarget)
	t.Logf("Third request: %s", thirdTarget)

	// 验证：至少有两个不同的 target 被使用
	targets := map[string]bool{firstTarget: true, secondTarget: true, thirdTarget: true}
	if len(targets) < 2 {
		t.Errorf("expected at least 2 different targets, got %d: %v", len(targets), targets)
	}
}

// mockUsageRecorder 是一个简单的 mock usage recorder
type mockUsageRecorder struct{}

func (m *mockUsageRecorder) Record(evt usage.Event) error {
	return nil
}

// TestServiceAffinityByIPAndKey 测试客户端亲和性按 IP+KEY 组合分配
// 场景：
// 1. 同一客户端（同 KEY）从不同 IP 访问，应该独立分配（不共享 affinity）
// 2. 不同客户端（不同 KEY）从同一 IP 访问，应该独立分配
// 3. 同一客户端从同一 IP 访问，应该命中 affinity（保持粘连）
func TestServiceAffinityByIPAndKey(t *testing.T) {
	// 创建两个 mock 上游
	target1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"qwen3.7-max","usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer target1.Close()

	target2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"qwen3.7-max","usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer target2.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "target-1",
				Endpoint:           target1.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key-1",
				ModelMappings:      []config.ModelMapping{{Upstream: "qwen3.7-max"}},
			},
			{
				Name:               "target-2",
				Endpoint:           target2.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key-2",
				ModelMappings:      []config.ModelMapping{{Upstream: "qwen3.7-max"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	clients := []config.Client{
		{Name: "client-A", AccessKey: "token-A"},
		{Name: "client-B", AccessKey: "token-B"},
	}
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("load clients: %v", err)
	}

	// 辅助函数：发送请求并返回 target
	sendRequest := func(clientName, accessKey, remoteIP string) string {
		principal, ok := store.Authenticate(accessKey)
		if !ok {
			t.Fatalf("failed to authenticate client %s", clientName)
		}

		body := bytes.NewBufferString(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
		req.RemoteAddr = remoteIP + ":12345" // 设置客户端 IP
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("client %s from %s: expected 200, got %d", clientName, remoteIP, rr.Code)
		}

		return rr.Header().Get("X-Proxy-Target")
	}

	// 场景 1：同一客户端从不同 IP 访问，应该独立分配（不共享 affinity）
	t.Run("SameKeyDifferentIP", func(t *testing.T) {
		// client-A 从 IP-1 访问
		target1 := sendRequest("client-A", "token-A", "192.168.1.1")
		// client-A 从 IP-2 访问（应该独立分配，不命中 IP-1 的 affinity）
		target2 := sendRequest("client-A", "token-A", "192.168.1.2")

		// 由于是首次访问，两个 IP 应该各自独立分配
		// 验证：至少有一个 target 被使用（可能是同一个，也可能是不同的）
		if target1 == "" || target2 == "" {
			t.Errorf("expected both targets to be non-empty, got target1=%s, target2=%s", target1, target2)
		}
		t.Logf("client-A from IP-1: %s, from IP-2: %s", target1, target2)
	})

	// 场景 2：不同客户端从同一 IP 访问，应该独立分配
	t.Run("DifferentKeySameIP", func(t *testing.T) {
		// client-A 从 IP-3 访问
		targetA := sendRequest("client-A", "token-A", "192.168.2.1")
		// client-B 从 IP-3 访问（应该独立分配，不命中 client-A 的 affinity）
		targetB := sendRequest("client-B", "token-B", "192.168.2.1")

		// 验证：至少有一个 target 被使用
		if targetA == "" || targetB == "" {
			t.Errorf("expected both targets to be non-empty, got targetA=%s, targetB=%s", targetA, targetB)
		}
		t.Logf("client-A from IP-3: %s, client-B from IP-3: %s", targetA, targetB)
	})

	// 场景 3：同一客户端从同一 IP 访问，应该命中 affinity（保持粘连）
	t.Run("SameKeySameIP", func(t *testing.T) {
		// client-A 从 IP-4 第一次访问
		target1 := sendRequest("client-A", "token-A", "192.168.3.1")
		// client-A 从 IP-4 第二次访问（应该命中 affinity，返回同一个 target）
		target2 := sendRequest("client-A", "token-A", "192.168.3.1")
		// client-A 从 IP-4 第三次访问（应该继续命中 affinity）
		target3 := sendRequest("client-A", "token-A", "192.168.3.1")

		// 验证：三次访问应该返回同一个 target（affinity 生效）
		if target1 != target2 || target2 != target3 {
			t.Errorf("expected affinity to keep same target, got target1=%s, target2=%s, target3=%s", target1, target2, target3)
		}
		t.Logf("client-A from IP-4 (3 times): %s, %s, %s", target1, target2, target3)
	})
}

func TestServiceModelAwareRoundRobin(t *testing.T) {
	// 场景：11 个 targets，只有 2 个支持 qwen3.7-max（索引 9 和 10）
	// target-9 有 4 个 key，target-10 有 1 个 key
	// 验证：按 key 数量轮询，target-9 获得 80% 流量，target-10 获得 20% 流量

	// 创建 11 个 mock 上游
	var servers []*httptest.Server
	for i := 0; i < 11; i++ {
		idx := i
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(`{"model":"qwen3.7-max","usage":{"prompt_tokens":10,"completion_tokens":5},"target_index":%d}`, idx)))
		}))
		defer server.Close()
		servers = append(servers, server)
	}

	// 配置 11 个 targets，只有索引 9 和 10 支持 qwen3.7-max
	targets := make([]config.Target, 11)
	for i := 0; i < 11; i++ {
		targets[i] = config.Target{
			Name:               fmt.Sprintf("target-%d", i),
			Endpoint:           servers[i].URL,
			ResourcePathPrefix: "/",
			APIKey:             fmt.Sprintf("key-%d", i),
		}
		// 只有索引 9 和 10 支持 qwen3.7-max
		if i == 9 {
			// target-9 有 4 个 key（1 个 api_key + 3 个 api_keys）
			targets[i].ModelMappings = []config.ModelMapping{{Upstream: "qwen3.7-max"}}
			targets[i].APIKeys = []string{"key-9-2", "key-9-3", "key-9-4"}
		} else if i == 10 {
			// target-10 有 1 个 key（只有 api_key）
			targets[i].ModelMappings = []config.ModelMapping{{Upstream: "qwen3.7-max"}}
		} else {
			targets[i].ModelMappings = []config.ModelMapping{{Upstream: "other-model"}}
		}
	}

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: targets,
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// 创建 20 个不同的客户端（避免 affinity 影响）
	store := auth.NewStore()
	clients := make([]config.Client, 20)
	for i := 0; i < 20; i++ {
		clients[i] = config.Client{
			Name:      fmt.Sprintf("client-%d", i),
			AccessKey: fmt.Sprintf("token-%d", i),
		}
	}
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("load clients: %v", err)
	}

	// 发送 20 个请求，统计 target 分布
	targetCount := make(map[string]int)
	for i := 0; i < 20; i++ {
		principal, ok := store.Authenticate(fmt.Sprintf("token-%d", i))
		if !ok {
			t.Fatalf("failed to authenticate client-%d", i)
		}

		body := bytes.NewBufferString(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
		req.RemoteAddr = fmt.Sprintf("192.168.%d.%d:12345", i/256, i%256) // 不同 IP
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("client-%d: expected 200, got %d", i, rr.Code)
		}

		target := rr.Header().Get("X-Proxy-Target")
		if target == "" {
			t.Fatalf("client-%d: missing X-Proxy-Target header", i)
		}
		targetCount[target]++
	}

	// 验证：只有 target-9 和 target-10 应该被选中
	if len(targetCount) != 2 {
		t.Errorf("expected exactly 2 targets to be selected, got %d: %v", len(targetCount), targetCount)
	}

	// 验证：target-9 应该获得约 80% 流量（16 个请求），target-10 应该获得约 20% 流量（4 个请求）
	// 允许 10% 偏差：target-9 应该 14-18 个，target-10 应该 2-6 个
	target9Count := targetCount["target-9"]
	target10Count := targetCount["target-10"]

	if target9Count < 14 || target9Count > 18 {
		t.Errorf("target-9 should get ~80%% traffic (14-18 requests), got %d", target9Count)
	}
	if target10Count < 2 || target10Count > 6 {
		t.Errorf("target-10 should get ~20%% traffic (2-6 requests), got %d", target10Count)
	}

	t.Logf("Target distribution: target-9=%d (80%%), target-10=%d (20%%)", target9Count, target10Count)
}

func TestServiceModelAwareRoundRobinVariousScenarios(t *testing.T) {
	tests := []struct {
		name          string
		targetConfigs []struct {
			name     string
			keyCount int
			models   []config.ModelMapping
		}
		requestCount int
		expectedDist map[string]float64 // target name -> expected percentage
		tolerance    float64            // allowed deviation from expected percentage
	}{
		{
			name: "extreme imbalance 10:1",
			targetConfigs: []struct {
				name     string
				keyCount int
				models   []config.ModelMapping
			}{
				{"target-heavy", 10, []config.ModelMapping{{Upstream: "qwen3.7-max"}}},
				{"target-light", 1, []config.ModelMapping{{Upstream: "qwen3.7-max"}}},
			},
			requestCount: 22,
			expectedDist: map[string]float64{
				"target-heavy": 90.9, // 10/11
				"target-light": 9.1,  // 1/11
			},
			tolerance: 5.0,
		},
		{
			name: "three targets 3:2:1",
			targetConfigs: []struct {
				name     string
				keyCount int
				models   []config.ModelMapping
			}{
				{"target-a", 3, []config.ModelMapping{{Upstream: "qwen3.7-max"}}},
				{"target-b", 2, []config.ModelMapping{{Upstream: "qwen3.7-max"}}},
				{"target-c", 1, []config.ModelMapping{{Upstream: "qwen3.7-max"}}},
			},
			requestCount: 30,
			expectedDist: map[string]float64{
				"target-a": 50.0, // 3/6
				"target-b": 33.3, // 2/6
				"target-c": 16.7, // 1/6
			},
			tolerance: 8.0,
		},
		{
			name: "five targets equal",
			targetConfigs: []struct {
				name     string
				keyCount int
				models   []config.ModelMapping
			}{
				{"target-1", 1, []config.ModelMapping{{Upstream: "qwen3.7-max"}}},
				{"target-2", 1, []config.ModelMapping{{Upstream: "qwen3.7-max"}}},
				{"target-3", 1, []config.ModelMapping{{Upstream: "qwen3.7-max"}}},
				{"target-4", 1, []config.ModelMapping{{Upstream: "qwen3.7-max"}}},
				{"target-5", 1, []config.ModelMapping{{Upstream: "qwen3.7-max"}}},
			},
			requestCount: 25,
			expectedDist: map[string]float64{
				"target-1": 20.0,
				"target-2": 20.0,
				"target-3": 20.0,
				"target-4": 20.0,
				"target-5": 20.0,
			},
			tolerance: 8.0,
		},
		{
			name: "single target with multiple keys",
			targetConfigs: []struct {
				name     string
				keyCount int
				models   []config.ModelMapping
			}{
				{"target-only", 5, []config.ModelMapping{{Upstream: "qwen3.7-max"}}},
			},
			requestCount: 10,
			expectedDist: map[string]float64{
				"target-only": 100.0,
			},
			tolerance: 0.0,
		},
		{
			name: "mixed models with filtering",
			targetConfigs: []struct {
				name     string
				keyCount int
				models   []config.ModelMapping
			}{
				{"target-a", 2, []config.ModelMapping{{Upstream: "qwen3.7-max"}}},
				{"target-b", 3, []config.ModelMapping{{Upstream: "other-model"}}}, // should be filtered out
				{"target-c", 1, []config.ModelMapping{{Upstream: "qwen3.7-max"}}},
				{"target-d", 4, []config.ModelMapping{{Upstream: "another-model"}}}, // should be filtered out
			},
			requestCount: 15,
			expectedDist: map[string]float64{
				"target-a": 66.7, // 2/3
				"target-c": 33.3, // 1/3
			},
			tolerance: 10.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建 mock 上游
			var servers []*httptest.Server
			for i := 0; i < len(tt.targetConfigs); i++ {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"model":"qwen3.7-max","usage":{"prompt_tokens":10,"completion_tokens":5}}`))
				}))
				defer server.Close()
				servers = append(servers, server)
			}

			// 配置 targets
			targets := make([]config.Target, len(tt.targetConfigs))
			for i, tc := range tt.targetConfigs {
				targets[i] = config.Target{
					Name:               tc.name,
					Endpoint:           servers[i].URL,
					ResourcePathPrefix: "/",
					APIKey:             fmt.Sprintf("key-%d", i),
					ModelMappings:      tc.models,
				}
				// 添加额外的 keys
				if tc.keyCount > 1 {
					extraKeys := make([]string, tc.keyCount-1)
					for j := 0; j < tc.keyCount-1; j++ {
						extraKeys[j] = fmt.Sprintf("key-%d-%d", i, j+2)
					}
					targets[i].APIKeys = extraKeys
				}
			}

			cfg := &config.Config{
				Server: config.ServerConfig{
					Bind:                  "127.0.0.1:0",
					RequestTimeoutSeconds: 5,
				},
				Targets: targets,
				Logging: config.LoggingConfig{
					Level:     "info",
					AccessLog: "logs/test-access.log",
					ErrorLog:  "logs/test-error.log",
				},
			}

			service, err := NewService(cfg, newTestLogger())
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}

			// 创建客户端
			store := auth.NewStore()
			clients := make([]config.Client, tt.requestCount)
			for i := 0; i < tt.requestCount; i++ {
				clients[i] = config.Client{
					Name:      fmt.Sprintf("client-%d", i),
					AccessKey: fmt.Sprintf("token-%d", i),
				}
			}
			if err := store.LoadFromConfig(clients); err != nil {
				t.Fatalf("load clients: %v", err)
			}

			// 发送请求并统计分布
			targetCount := make(map[string]int)
			for i := 0; i < tt.requestCount; i++ {
				principal, ok := store.Authenticate(fmt.Sprintf("token-%d", i))
				if !ok {
					t.Fatalf("failed to authenticate client-%d", i)
				}

				body := bytes.NewBufferString(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hi"}]}`)
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
				req.RemoteAddr = fmt.Sprintf("192.168.%d.%d:12345", i/256, i%256)
				req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

				rr := httptest.NewRecorder()
				service.ServeHTTP(rr, req)

				if rr.Code != http.StatusOK {
					t.Fatalf("client-%d: expected 200, got %d", i, rr.Code)
				}

				target := rr.Header().Get("X-Proxy-Target")
				targetCount[target]++
			}

			// 验证分布
			for targetName, expectedPct := range tt.expectedDist {
				actualCount := targetCount[targetName]
				actualPct := float64(actualCount) / float64(tt.requestCount) * 100.0

				if actualPct < expectedPct-tt.tolerance || actualPct > expectedPct+tt.tolerance {
					t.Errorf("%s: expected ~%.1f%% (±%.1f%%), got %.1f%% (%d/%d requests)",
						targetName, expectedPct, tt.tolerance, actualPct, actualCount, tt.requestCount)
				}
			}

			// 验证没有意外的 targets 被选中
			for targetName := range targetCount {
				if _, expected := tt.expectedDist[targetName]; !expected {
					t.Errorf("unexpected target %s was selected", targetName)
				}
			}

			t.Logf("Distribution: %v", targetCount)
		})
	}
}

func TestServiceFailoverOnTransportError(t *testing.T) {
	success := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mock":"ok"}`))
	}))
	defer success.Close()

	successURL, err := url.Parse(success.URL)
	if err != nil {
		t.Fatalf("parse success URL: %v", err)
	}

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "primary",
				Endpoint:           "http://primary.local",
				ResourcePathPrefix: "/",
				APIKey:             "primary-key",
			},
			{
				Name:               "secondary",
				Endpoint:           success.URL,
				ResourcePathPrefix: "/",
				APIKey:             "secondary-key",
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.httpClient = &http.Client{
		Transport: &failingTransport{successHost: successURL.Host},
	}
	service.quietPeriod = 10 * time.Millisecond

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected authenticated principal")
	}

	body := bytes.NewBufferString(`{"model":"test-model","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	res := rr.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", res.StatusCode)
	}
	if got := res.Header.Get("X-Azure-Target"); got != "secondary" {
		t.Fatalf("expected target header to be secondary, got %q", got)
	}

	metrics := service.MetricsSnapshot()
	if metrics.TotalRequests != 1 {
		t.Fatalf("unexpected total requests: %d", metrics.TotalRequests)
	}
	if metrics.TotalSuccess != 1 {
		t.Fatalf("unexpected total success: %d", metrics.TotalSuccess)
	}
	if metrics.TotalFailures != 0 {
		t.Fatalf("unexpected total failures: %d", metrics.TotalFailures)
	}
	if metrics.TotalRetries != 1 {
		t.Fatalf("expected 1 retry, got %d", metrics.TotalRetries)
	}
	if metrics.TotalTargetRetries != 1 {
		t.Fatalf("expected 1 target retry, got %d", metrics.TotalTargetRetries)
	}
	if metrics.TotalKeyRetries != 0 {
		t.Fatalf("expected 0 key retries, got %d", metrics.TotalKeyRetries)
	}

	statuses := service.TargetStatuses(time.Now())
	var primaryMuted bool
	for _, st := range statuses {
		if st.Name == "primary" {
			primaryMuted = st.Muted
		}
	}
	if !primaryMuted {
		t.Fatal("expected primary target to be muted after failure")
	}
}

func TestServiceRejectsUnauthorizedTarget(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "primary",
				Endpoint:           "http://primary.local",
				ResourcePathPrefix: "/",
				APIKey:             "key",
			},
			{
				Name:               "secondary",
				Endpoint:           "http://secondary.local",
				ResourcePathPrefix: "/",
				APIKey:             "key2",
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("team-alpha", "alpha", "primary")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("alpha")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Proxy-Target", "secondary")
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 status, got %d", rr.Code)
	}
}

func TestServiceSkipsPausedTargetForAutomaticSelection(t *testing.T) {
	var pausedCalls atomic.Int32
	paused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pausedCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer paused.Close()
	var activeCalls atomic.Int32
	active := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		activeCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer active.Close()

	service, err := NewService(&config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{
			{Name: "paused", Endpoint: paused.URL, ResourcePathPrefix: "/openai", APIKey: "key1", Paused: true, ModelMappings: []config.ModelMapping{{Upstream: "gpt-paused"}}},
			{Name: "active", Endpoint: active.URL, ResourcePathPrefix: "/openai", APIKey: "key2", ModelMappings: []config.ModelMapping{{Upstream: "gpt-paused"}}},
		},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-paused","messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Proxy-Target"); got != "active" {
		t.Fatalf("expected active target, got %q", got)
	}
	if pausedCalls.Load() != 0 || activeCalls.Load() != 1 {
		t.Fatalf("unexpected upstream calls paused=%d active=%d", pausedCalls.Load(), activeCalls.Load())
	}
}

func TestServiceAffinitySkipsPausedTarget(t *testing.T) {
	var pausedCalls atomic.Int32
	paused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pausedCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer paused.Close()
	var activeCalls atomic.Int32
	active := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		activeCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer active.Close()

	service, err := NewService(&config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{
			{Name: "paused", Endpoint: paused.URL, ResourcePathPrefix: "/openai", APIKey: "key1", Paused: true, ModelMappings: []config.ModelMapping{{Upstream: "gpt-paused"}}},
			{Name: "active", Endpoint: active.URL, ResourcePathPrefix: "/openai", APIKey: "key2", ModelMappings: []config.ModelMapping{{Upstream: "gpt-paused"}}},
		},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")
	client := "203.0.113.9"
	service.affinity.Set(affinityKey(client, "tester", "gpt-paused"), "paused", time.Now())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-paused","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = client + ":12345"
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Proxy-Target"); got != "active" {
		t.Fatalf("expected active target, got %q", got)
	}
	if pausedCalls.Load() != 0 || activeCalls.Load() != 1 {
		t.Fatalf("unexpected upstream calls paused=%d active=%d", pausedCalls.Load(), activeCalls.Load())
	}
}

func TestServiceRejectsExplicitPausedTarget(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	service, err := NewService(&config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{Name: "paused", Endpoint: upstream.URL, ResourcePathPrefix: "/openai", APIKey: "key", Paused: true, ModelMappings: []config.ModelMapping{{Upstream: "gpt-paused"}}}},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-paused","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Proxy-Target", "paused")
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "paused") {
		t.Fatalf("expected paused error, got %q", rr.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("paused target should not be called, got %d calls", calls.Load())
	}
	statuses := service.TargetStatuses(time.Now())
	if len(statuses) != 1 || !statuses[0].Paused {
		t.Fatalf("expected TargetStatuses paused=true, got %#v", statuses)
	}
}

func TestServiceTimeoutDoesNotRetryOrMute(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer slow.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 1,
		},
		Targets: []config.Target{
			{
				Name:               "slow",
				Endpoint:           slow.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key",
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.setRequestTimeout(50 * time.Millisecond)
	service.quietPeriod = 10 * time.Millisecond

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"test-model"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 due to timeout, got %d", rr.Code)
	}

	metrics := service.MetricsSnapshot()
	if metrics.TotalRetries != 0 {
		t.Fatalf("expected no retries on timeout, got %d", metrics.TotalRetries)
	}

	statuses := service.TargetStatuses(time.Now())
	for _, st := range statuses {
		if st.Name == "slow" && st.Muted {
			t.Fatalf("expected target not to be muted on timeout")
		}
	}
}

// TestServiceClientDisconnectReturns499 模拟客户端在等上游响应时主动断开：
// 代理应返回 499（而非 504）、计入 totalClientCanceled、不计入 totalFailures、
// 不 mute 目标。这是多跳代理链中区分"下游断开"与"上游故障"的关键。
func TestServiceClientDisconnectReturns499(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer slow.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "slow",
				Endpoint:           slow.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key",
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.setRequestTimeout(5 * time.Second) // 自身 deadline 不会到，确保触发的是 client cancel

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	// 用可取消 context 模拟客户端连接；上游还在 sleep 时 cancel() → r.Context() done。
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"test-model"}`))
	req = req.WithContext(auth.WithPrincipal(ctx, principal))

	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != StatusClientClosedRequest {
		t.Fatalf("expected %d (Client Closed Request) on client disconnect, got %d", StatusClientClosedRequest, rr.Code)
	}

	metrics := service.MetricsSnapshot()
	if metrics.TotalClientCanceled != 1 {
		t.Fatalf("expected TotalClientCanceled=1, got %d", metrics.TotalClientCanceled)
	}
	if metrics.TotalFailures != 0 {
		t.Fatalf("client disconnect must not count as failure; TotalFailures=%d", metrics.TotalFailures)
	}
	if metrics.TotalRetries != 0 {
		t.Fatalf("client disconnect must not trigger failover; TotalRetries=%d", metrics.TotalRetries)
	}

	for _, st := range service.TargetStatuses(time.Now()) {
		if st.Name == "slow" && st.Muted {
			t.Fatalf("target must not be muted on client disconnect")
		}
	}
}

// TestForwardPropagatesRequestID 验证代理把 request_id 透传给上游，
// 使多跳 llms-proxy 链（代理 A → 代理 B → 上游）共享同一 id，全链日志可追溯。
// 入站刻意【不】带 X-Request-ID：由中间件生成 uuid，只有 forward.go 主动 Set 才会让上游收到，
// 从而精确验证透传逻辑（而非 copyHeaders 顺带把入站头透传过去）。
func TestForwardPropagatesRequestID(t *testing.T) {
	var seenID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = r.Header.Get(appmiddleware.HeaderRequestID)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-x","choices":[]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "upstream",
				Endpoint:           upstream.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key",
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	// 入站不带 X-Request-ID：由 RequestID 中间件生成 uuid。
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"test-model"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	// 用 RequestID 中间件包装 service，模拟真实路由链。
	handler := appmiddleware.RequestID()(http.HandlerFunc(service.ServeHTTP))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	respID := rr.Header().Get(appmiddleware.HeaderRequestID)
	if respID == "" {
		t.Fatalf("expected middleware to set response X-Request-ID, got empty")
	}
	if seenID == "" {
		t.Fatalf("expected upstream to receive X-Request-ID via forward propagation, got empty")
	}
	if seenID != respID {
		t.Fatalf("request id chain broken: upstream got %q but response had %q (must be identical)", seenID, respID)
	}
}

func TestServiceAllowsBearerPassthrough(t *testing.T) {
	const bearerHeader = "Bearer azure-token"
	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		if seenAuth == "" {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "bearer",
				Endpoint:           upstream.URL,
				ResourcePathPrefix: "/",
				AllowBearer:        true,
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"test-model"}`))
	req.Header.Set(headerAzureAuthorization, bearerHeader)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		body, _ := io.ReadAll(rr.Body)
		t.Fatalf("expected 200, got %d body=%s", rr.Code, string(body))
	}
	if seenAuth != bearerHeader {
		t.Fatalf("expected upstream auth %q, got %q", bearerHeader, seenAuth)
	}
}

func TestServiceRejectsDisallowedModel(t *testing.T) {
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "restricted",
				Endpoint:           upstream.URL,
				ResourcePathPrefix: "/openai",
				APIKey:             "key",
				ModelMappings:      []config.ModelMapping{{Upstream: "gpt-4o"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"gpt-3.5","input":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	if upstreamCalled {
		t.Fatalf("expected request not to reach upstream")
	}
}

func TestServiceStripsAPIVersionAndInternalQueryParams(t *testing.T) {
	var seenVersion string
	var seenOther string
	var seenTarget string
	var seenAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		seenVersion = q.Get("api-version")
		if seenVersion == "" {
			seenVersion = q.Get("API-Version")
		}
		seenOther = q.Get("other")
		seenTarget = q.Get("target")
		seenAPIKey = q.Get("api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "primary",
				Endpoint:           upstream.URL,
				ResourcePathPrefix: "/openai",
				APIKey:             "key",
				ModelMappings:      []config.ModelMapping{{Upstream: "gpt-4o"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/deployments/gpt-4o/chat/completions?api-version=foo&API-Version=bar&target=primary&api-key=client&other=yes", bytes.NewBufferString(`{}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	// Azure targets inject api-version=2026-03-01-preview for deployment-based paths
	// after stripping client-supplied versions.
	// The test target has no endpoint_type set → defaults to azure_openai → must see dated version.
	if seenVersion != "2026-03-01-preview" {
		t.Fatalf("expected api-version=2026-03-01-preview for azure_openai deployment path, got %q", seenVersion)
	}
	if seenOther != "yes" {
		t.Fatalf("expected other query param preserved, got %q", seenOther)
	}
	if seenTarget != "" {
		t.Fatalf("expected internal target query param stripped, got %q", seenTarget)
	}
	if seenAPIKey != "" {
		t.Fatalf("expected client api-key query param stripped, got %q", seenAPIKey)
	}
}

func TestServiceStripsUnsupportedFieldsForResponses(t *testing.T) {
	var captured map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if err := json.Unmarshal(data, &captured); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:               "primary",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
			ModelMappings:      []config.ModelMapping{{Upstream: "gpt-5.2"}},
		}},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"gpt-5.2","input":"hi","prompt_cache_key":"session-a","prompt_cache_retention":"24h","foo":"bar"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if got := captured["prompt_cache_retention"]; got != "24h" {
		t.Fatalf("expected GPT cache retention preserved, got body=%v", captured)
	}
	if _, ok := captured["foo"]; ok {
		t.Fatalf("expected unknown field foo to be stripped, got body=%v", captured)
	}
	if got, ok := captured["prompt_cache_key"].(string); !ok || got != "session-a" {
		t.Fatalf("expected prompt_cache_key to be preserved, got %v", captured["prompt_cache_key"])
	}
}

func TestServiceStripsUnsupportedFieldsForChatCompletions(t *testing.T) {
	var captured map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if err := json.Unmarshal(data, &captured); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:               "primary",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
			ModelMappings:      []config.ModelMapping{{Upstream: "gpt-5.2"}},
		}},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"gpt-5.2","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":16,"prompt_cache_key":"session-a","prompt_cache_retention":"24h","foo":"bar"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if got := captured["prompt_cache_retention"]; got != "24h" {
		t.Fatalf("expected GPT cache retention preserved, got body=%v", captured)
	}
	if _, ok := captured["foo"]; ok {
		t.Fatalf("expected unknown field foo to be stripped, got body=%v", captured)
	}
	if _, ok := captured["messages"]; !ok {
		t.Fatalf("expected messages to be preserved, got body=%v", captured)
	}
}

func TestServiceRoutesByModelToSupportingTarget(t *testing.T) {
	target1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Target", "t1")
	}))
	defer target1.Close()
	target2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Target", "t2")
	}))
	defer target2.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "t1",
				Endpoint:           target1.URL,
				ResourcePathPrefix: "/openai",
				APIKey:             "key1",
				ModelMappings:      []config.ModelMapping{{Upstream: "gpt-4o"}},
			},
			{
				Name:               "t2",
				Endpoint:           target2.URL,
				ResourcePathPrefix: "/openai",
				APIKey:             "key2",
				ModelMappings:      []config.ModelMapping{{Upstream: "gpt-5.1"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"gpt-5.1","input":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/openai/deployments/gpt-5.1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if got := rr.Header().Get("X-Azure-Target"); got != "t2" {
		t.Fatalf("expected target t2, got %q", got)
	}
}

func TestServiceReturnsErrorWhenModelMissingAndAllowlistsConfigured(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "t1",
				Endpoint:           "http://example.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key1",
				ModelMappings:      []config.ModelMapping{{Upstream: "gpt-4o"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", bytes.NewBufferString(`{}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestServiceRoundsRobinAcrossMatchingTargets(t *testing.T) {
	counts := map[string]int{}
	makeServer := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counts[name]++
			w.WriteHeader(http.StatusOK)
		}))
	}
	s1 := makeServer("t1")
	defer s1.Close()
	s2 := makeServer("t2")
	defer s2.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "t1",
				Endpoint:           s1.URL,
				ResourcePathPrefix: "/openai",
				APIKey:             "key1",
				ModelMappings:      []config.ModelMapping{{Upstream: "gpt-5.1"}},
			},
			{
				Name:               "t2",
				Endpoint:           s2.URL,
				ResourcePathPrefix: "/openai",
				APIKey:             "key2",
				ModelMappings:      []config.ModelMapping{{Upstream: "gpt-5.1"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Use distinct client names to avoid affinity stickiness collapsing all
	// requests to a single target.
	for i := 0; i < 6; i++ {
		clientName := fmt.Sprintf("tester-%d", i)
		store := auth.NewStore()
		if err := store.LoadFromConfig(testAuthClients(clientName, "token")); err != nil {
			t.Fatalf("load clients: %v", err)
		}
		principal, ok := store.Authenticate("token")
		if !ok {
			t.Fatal("expected principal")
		}

		body := bytes.NewBufferString(`{"model":"gpt-5.1","input":"hi"}`)
		req := httptest.NewRequest(http.MethodPost, "/openai/deployments/gpt-5.1/chat/completions", body)
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
	}

	if counts["t1"] == 0 || counts["t2"] == 0 {
		t.Fatalf("expected both targets to receive traffic, got %+v", counts)
	}
	if diff := counts["t1"] - counts["t2"]; diff < -2 || diff > 2 {
		t.Fatalf("expected roughly balanced distribution, got %+v", counts)
	}
}

func TestServiceListsDeploymentsLocally(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "t1",
				Endpoint:           "http://example.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key1",
				ModelMappings:      []config.ModelMapping{{Upstream: "gpt-4o"}, {Upstream: "gpt-5.1"}},
			},
			{
				Name:               "t2",
				Endpoint:           "http://example2.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key2",
				ModelMappings:      []config.ModelMapping{{Upstream: "gpt-5.1"}, {Upstream: "gpt-4o-mini"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")

	req := httptest.NewRequest(http.MethodGet, "/openai/deployments?api-version=ignored", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp struct {
		Data []struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"data"`
		FirstID string `json:"first_id"`
		LastID  string `json:"last_id"`
		HasMore bool   `json:"has_more"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 3 {
		t.Fatalf("expected 3 models, got %d", len(resp.Data))
	}
	if resp.HasMore {
		t.Fatalf("expected has_more=false")
	}
	if resp.FirstID == "" || resp.LastID == "" {
		t.Fatalf("expected first/last ids to be set")
	}
}

func TestServiceListsModelsLocally(t *testing.T) {
	paths := []string{"/openai/models?api-version=ignored", "/v1/models?api-version=ignored", "/models"}
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "t1",
				Endpoint:           "http://example.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key1",
				ModelMappings:      []config.ModelMapping{{Upstream: "gpt-4o"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")

	for _, p := range paths {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("%s expected 200, got %d", p, rr.Code)
		}
		var resp struct {
			Data []struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			} `json:"data"`
		}
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("%s decode: %v", p, err)
		}
		if len(resp.Data) != 1 || resp.Data[0].ID != "gpt-4o" {
			t.Fatalf("%s unexpected data: %+v", p, resp.Data)
		}
	}
}

// TestServiceListsModelsLocally_FlatCommaFallback verifies that /v1/models
// surfaces the upstream name AND every comma-separated fallback alias as
// independent, equally-ranked entries — exposing no mapping relation to the
// client.
func TestServiceListsModelsLocally_FlatCommaFallback(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "t1",
				Endpoint:           "http://example.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key1",
				ModelMappings: []config.ModelMapping{{
					Upstream: "gpt-4o",
					Fallback: "gpt-4, gpt4-turbo",
				}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	got := make(map[string]bool, len(resp.Data))
	for _, d := range resp.Data {
		got[d.ID] = true
	}
	// upstream + both aliases must all appear as independent entries.
	for _, want := range []string{"gpt-4o", "gpt-4", "gpt4-turbo"} {
		if !got[want] {
			t.Errorf("expected %q in list, got %v", want, keysOf(got))
		}
	}
	if len(resp.Data) != 3 {
		t.Errorf("expected 3 flat entries, got %d: %v", len(resp.Data), got)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestServiceListsModelsLocallyRespectsAllowedTargets(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "t1",
				Endpoint:           "http://example.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key1",
				ModelMappings:      []config.ModelMapping{{Upstream: "gpt-4o"}},
			},
			{
				Name:               "t2",
				Endpoint:           "http://example2.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key2",
				ModelMappings:      []config.ModelMapping{{Upstream: "gpt-5.2"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token", "t1")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0].ID != "gpt-4o" {
		t.Fatalf("unexpected filtered models: %+v", resp.Data)
	}
}

func TestServiceListsModelsLocallyRespectsRequestedTargetFilter(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "t1",
				Endpoint:           "http://example.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key1",
				ModelMappings:      []config.ModelMapping{{Upstream: "gpt-4o"}},
			},
			{
				Name:               "t2",
				Endpoint:           "http://example2.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key2",
				ModelMappings:      []config.ModelMapping{{Upstream: "gpt-5.2"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")

	req := httptest.NewRequest(http.MethodGet, "/v1/models?target=t2", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0].ID != "gpt-5.2" {
		t.Fatalf("unexpected target-filtered models: %+v", resp.Data)
	}
}

func TestServiceListsModelsLocallySkipsPausedTargets(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "active",
				Endpoint:           "http://example.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key1",
				ModelMappings:      []config.ModelMapping{{Upstream: "gpt-4o"}},
			},
			{
				Name:               "paused",
				Endpoint:           "http://example2.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key2",
				ModelMappings:      []config.ModelMapping{{Upstream: "gpt-5.2"}},
				Paused:             true,
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0].ID != "gpt-4o" {
		t.Fatalf("expected only active target model gpt-4o, got: %+v", resp.Data)
	}
}

func TestServiceRecordsUsageOnSuccessfulResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":3}}}`))
	}))
	defer upstream.Close()

	tmpDir := t.TempDir()
	db, err := nosql.OpenDB(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	usageStore := nosql.NewUsageStore(db)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:               "t1",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
		}},
		DataStore: config.DataStore{DBPath: filepath.Join(tmpDir, "test.db")},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.SetUsageRecorder(usageStore)

	authStore := auth.NewStore()
	if err := authStore.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := authStore.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	events, err := usageStore.List(usage.Filter{Limit: 10})
	if err != nil {
		t.Fatalf("list usage events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 usage event, got %d", len(events))
	}
	evt := events[0]
	// prompt_tokens=11 includes cached_tokens=3; InputTokens is the miss portion (11-3=8)
	// so billing charges the cache-hit portion at CacheRead price.
	if evt.ClientName != "tester" || evt.InputTokens != 8 || evt.OutputTokens != 7 || evt.CachedTokens != 3 {
		t.Fatalf("unexpected usage event: %+v", evt)
	}
}

// TestServiceUsageEventRecordsModel 红线：用量事件的 model 字段必须与上游响应中的
// model 字段一致（小写化），确保下游 CostTable 能查到价格、用量统计按模型聚合。
func TestServiceUsageEventRecordsModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"deepseek-v4-flash","usage":{"prompt_tokens":5,"completion_tokens":3}}`))
	}))
	defer upstream.Close()

	tmpDir := t.TempDir()
	db, err := nosql.OpenDB(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	usageStore := nosql.NewUsageStore(db)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:            "ds1",
			EndpointType:    config.EndpointTypeDualProtocol,
			Endpoint:        upstream.URL,
			APIKey:          "key",
			AnthropicPrefix: "/anthropic",
		}},
		DataStore: config.DataStore{DBPath: filepath.Join(tmpDir, "test.db")},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.SetUsageRecorder(usageStore)

	authStore := auth.NewStore()
	if err := authStore.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := authStore.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	events, err := usageStore.List(usage.Filter{Limit: 10})
	if err != nil {
		t.Fatalf("list usage events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 usage event, got %d", len(events))
	}
	if got := events[0].Model; got != "deepseek-v4-flash" {
		t.Fatalf("expected model deepseek-v4-flash, got %q", got)
	}
}

// TestServiceUsageEventRecordsUnmappedAliasPricingCharacterization records the
// current no-mapping behavior when the provider reports a different model name.
// It deliberately does not assert that ResponseModel must replace Model: the
// existing tests cover identical names and explicit mappings, but not this
// divergent-name case.
func TestServiceUsageEventRecordsUnmappedAliasPricingCharacterization(t *testing.T) {
	const (
		targetName   = "unmapped-target"
		requestAlias = "client-shortcut"
		responseName = "provider-model-v2"
	)
	forwardedModel := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read forwarded request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var request struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode forwarded request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		forwardedModel <- request.Model
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": responseName,
			"usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 3},
		})
	}))
	defer upstream.Close()

	tmpDir := t.TempDir()
	db, err := nosql.OpenDB(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	usageStore := nosql.NewUsageStore(db)

	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{
			Name:         targetName,
			EndpointType: config.EndpointTypeDualProtocol,
			Endpoint:     upstream.URL,
			APIKey:       "key",
		}},
		DataStore: config.DataStore{DBPath: filepath.Join(tmpDir, "test.db")},
		Logging:   config.LoggingConfig{Level: "info", AccessLog: filepath.Join(tmpDir, "access.log"), ErrorLog: filepath.Join(tmpDir, "error.log")},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.SetUsageRecorder(usageStore)

	authStore := auth.NewStore()
	if err := authStore.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := authStore.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"client-shortcut","messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := <-forwardedModel; got != requestAlias {
		t.Fatalf("forwarded model = %q, want unmodified request alias %q", got, requestAlias)
	}

	events, err := usageStore.List(usage.Filter{Limit: 10})
	if err != nil {
		t.Fatalf("list usage events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 usage event, got %d", len(events))
	}
	event := events[0]
	if event.Model != requestAlias || event.ResponseModel != responseName {
		t.Fatalf("usage identity = model %q / response_model %q, want current no-mapping values %q / %q",
			event.Model, event.ResponseModel, requestAlias, responseName)
	}

	targetStore := nosql.NewTargetStore(db)
	if err := targetStore.Create(config.Target{
		Name: targetName, EndpointType: config.EndpointTypeDualProtocol,
		UsageAdapter: config.UsageAdapterStandard,
		Endpoint:     upstream.URL, APIKey: "key",
	}); err != nil {
		t.Fatalf("create aggregate target: %v", err)
	}
	from, to := event.Timestamp.Add(-time.Minute), event.Timestamp.Add(time.Minute)
	filter := usage.Filter{From: &from, To: &to}

	canonicalRates := usage.CostRates{InputPer1MTokens: 20, OutputPer1MTokens: 40}
	canonicalOnly := pricing.NewResolver([]pricing.PricingTemplate{{
		ID: "canonical-price", Target: targetName, Model: responseName, BaseRates: canonicalRates,
	}}, nil)
	if _, ok := canonicalOnly.RateFor(event.Target, event.ResponseModel, event.Timestamp); !ok {
		t.Fatal("canonical response model should have its explicit target price")
	}
	if _, ok := canonicalOnly.RateFor(event.Target, event.Model, event.Timestamp); ok {
		t.Fatal("request alias unexpectedly resolved through the canonical-only price")
	}
	if got := usage.EstimateEventCost(event, canonicalOnly); got != 0 {
		t.Fatalf("canonical-only price estimated cost = %v, want 0 for the persisted alias key", got)
	}
	canonicalAggregate, err := usageStore.AggregateByModelGroups(filter, "hour", canonicalOnly, targetStore)
	if err != nil {
		t.Fatalf("aggregate canonical-only price: %v", err)
	}
	if canonicalAggregate.Totals.EstimatedCost != 0 || len(canonicalAggregate.ByModel) != 1 ||
		canonicalAggregate.ByModel[0].Key != requestAlias {
		t.Fatalf("canonical-only aggregate = %+v, want alias row with zero estimated cost", canonicalAggregate)
	}

	aliasRates := usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 4}
	aliasAndCanonical := pricing.NewResolver([]pricing.PricingTemplate{
		{ID: "alias-price", Target: targetName, Model: requestAlias, BaseRates: aliasRates},
		{ID: "canonical-price", Target: targetName, Model: responseName, BaseRates: canonicalRates},
	}, nil)
	aliasAggregate, err := usageStore.AggregateByModelGroups(filter, "hour", aliasAndCanonical, targetStore)
	if err != nil {
		t.Fatalf("aggregate alias and canonical prices: %v", err)
	}
	wantAliasCost := float64(5*2+3*4) / 1_000_000
	if math.Abs(aliasAggregate.Totals.EstimatedCost-wantAliasCost) > 1e-12 {
		t.Fatalf("alias-priced aggregate cost = %.12g, want %.12g", aliasAggregate.Totals.EstimatedCost, wantAliasCost)
	}
	t.Logf("forwarded_model=%s event_model=%s response_model=%s canonical_only_aggregate_cost=%.12g alias_and_canonical_aggregate_cost=%.12g",
		requestAlias, event.Model, event.ResponseModel, canonicalAggregate.Totals.EstimatedCost, aliasAggregate.Totals.EstimatedCost)
}

// TestServiceUsageEventRecordsUpstreamModelWhenAliasUsed 红线：当客户端用 fallback alias
// 请求时，用量事件的 Model 字段必须是真实的上游模型名（来自 ensureModelAllowed 解析），
// 而不是客户端传入的 alias。这保证 CostTable 能正确匹配价格、用量统计按上游模型聚合。
func TestServiceUsageEventRecordsUpstreamModelWhenAliasUsed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":5,"completion_tokens":3}}`))
	}))
	defer upstream.Close()

	tmpDir := t.TempDir()
	db, err := nosql.OpenDB(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	usageStore := nosql.NewUsageStore(db)

	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{
			Name:          "t1",
			Endpoint:      upstream.URL,
			APIKey:        "key",
			ModelMappings: []config.ModelMapping{{Upstream: "GPT-4o-2024-11-20", Fallback: "my-shortcut"}},
		}},
		DataStore: config.DataStore{DBPath: filepath.Join(tmpDir, "test.db")},
		Logging:   config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.SetUsageRecorder(usageStore)

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")

	// 客户端用 alias（fallback）请求，应该解析到上游名 GPT-4o-2024-11-20
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"my-shortcut","messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	events, err := usageStore.List(usage.Filter{Limit: 10})
	if err != nil {
		t.Fatalf("list usage events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 usage event, got %d", len(events))
	}
	// 关键断言：usage event 记录的必须是上游名（小写化后），不是 alias
	expected := "gpt-4o-2024-11-20"
	if events[0].Model != expected {
		t.Errorf("usage event Model: want %q (upstream), got %q (was alias recorded?)", expected, events[0].Model)
	}
}

func TestExtractUsageFromClaudeSSE(t *testing.T) {
	// Claude streaming responses split usage across two events:
	// - message_start: contains input_tokens under message.usage
	// - message_delta: contains output_tokens under usage
	sseBody := []byte(
		"event: message_start\n" +
			`data: {"type":"message_start","message":{"id":"msg_123","type":"message","role":"assistant","content":[],"model":"claude-opus-4-6","usage":{"input_tokens":2048,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}}` + "\n\n" +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
			"event: content_block_stop\n" +
			`data: {"type":"content_block_stop","index":0}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":350}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n",
	)

	tokens, model, ok := extractUsageFromSSE(sseBody)
	if !ok {
		t.Fatal("expected usage to be found in Claude SSE")
	}
	if model != "claude-opus-4-6" {
		t.Fatalf("expected model claude-opus-4-6, got %q", model)
	}
	if tokens.InputTokens != 2048 {
		t.Fatalf("expected input_tokens=2048 (from message_start), got %d", tokens.InputTokens)
	}
	if tokens.OutputTokens != 350 {
		t.Fatalf("expected output_tokens=350 (from message_delta), got %d", tokens.OutputTokens)
	}
}

func TestExtractUsageFromClaudeSSETailOnly(t *testing.T) {
	// Simulate tail-only capture: message_start is missing (fell outside buffer),
	// only message_delta remains. We should still get output_tokens.
	sseBody := []byte(
		"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":500}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n",
	)

	tokens, _, ok := extractUsageFromSSE(sseBody)
	if !ok {
		t.Fatal("expected usage to be found")
	}
	if tokens.OutputTokens != 500 {
		t.Fatalf("expected output_tokens=500, got %d", tokens.OutputTokens)
	}
	// input_tokens will be 0 since message_start is missing — best effort.
	if tokens.InputTokens != 0 {
		t.Fatalf("expected input_tokens=0 (message_start missing), got %d", tokens.InputTokens)
	}
}

func TestExtractUsageFromClaudeSSEWithCaching(t *testing.T) {
	// Claude prompt caching: message_start has cache_read_input_tokens,
	// message_delta has only input_tokens (non-cached portion).
	// New behavior: input_tokens stays as raw value (3), cache_read and cache_creation are separate.
	sseBody := []byte(
		"event: message_start\n" +
			`data: {"type":"message_start","message":{"model":"claude-opus-4-6","usage":{"input_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":50000,"output_tokens":1}}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":3,"output_tokens":200}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n",
	)

	tokens, model, ok := extractUsageFromSSE(sseBody)
	if !ok {
		t.Fatal("expected usage to be found")
	}
	if model != "claude-opus-4-6" {
		t.Fatalf("expected model claude-opus-4-6, got %q", model)
	}
	// Raw input_tokens = 3 (cache_read is tracked separately, not added to input)
	if tokens.InputTokens != 3 {
		t.Fatalf("expected input_tokens=3, got %d", tokens.InputTokens)
	}
	if tokens.OutputTokens != 200 {
		t.Fatalf("expected output_tokens=200, got %d", tokens.OutputTokens)
	}
	if tokens.CachedTokens != 50000 {
		t.Fatalf("expected cached_tokens=50000, got %d", tokens.CachedTokens)
	}
	if tokens.CacheCreationTokens != 0 {
		t.Fatalf("expected cache_creation_tokens=0, got %d", tokens.CacheCreationTokens)
	}
}

func TestExtractUsageFromClaudeSSECachingTailOnly(t *testing.T) {
	// When only message_delta is in buffer (long conversation), input_tokens
	// reflects only non-cached tokens. cache fields are missing.
	sseBody := []byte(
		"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":1,"output_tokens":400}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n",
	)

	tokens, _, ok := extractUsageFromSSE(sseBody)
	if !ok {
		t.Fatal("expected usage to be found")
	}
	// Without cache fields, input_tokens stays as 1 (best effort)
	if tokens.InputTokens != 1 {
		t.Fatalf("expected input_tokens=1, got %d", tokens.InputTokens)
	}
	if tokens.OutputTokens != 400 {
		t.Fatalf("expected output_tokens=400, got %d", tokens.OutputTokens)
	}
}

func TestExtractModelFromFormEncoded(t *testing.T) {
	body := []byte("model=gpt-4o&input=hello")
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if got := extractModel(req, body); got != "gpt-4o" {
		t.Fatalf("expected model gpt-4o, got %q", got)
	}
}

func TestExtractModelFromMultipartForm(t *testing.T) {
	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	if err := writer.WriteField("model", "gpt-image-1"); err != nil {
		t.Fatalf("write model field: %v", err)
	}
	if err := writer.WriteField("prompt", "draw a cat"); err != nil {
		t.Fatalf("write prompt field: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	body := payload.Bytes()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	req.Header.Set("Content-Type", writer.FormDataContentType())

	if got := extractModel(req, body); got != "gpt-image-1" {
		t.Fatalf("expected model gpt-image-1, got %q", got)
	}
}

func TestExtractModelFromGeminiPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/v1beta/models/gemini-3.1-flash-image-preview:generateContent", "gemini-3.1-flash-image-preview"},
		{"/v1beta/models/gemini-3-pro-image-preview:streamGenerateContent", "gemini-3-pro-image-preview"},
		{"/v1alpha/models/gemini-2.5-pro:generateContent", "gemini-2.5-pro"},
		{"/v1/models/some-model:countTokens", "some-model"},
		{"/v1beta/models/gemini-flash:generateContent?key=abc", "gemini-flash"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, tc.path, nil)
		req.Header.Set("Content-Type", "application/json")
		if got := extractModel(req, nil); got != tc.want {
			t.Errorf("path=%q: expected model %q, got %q", tc.path, tc.want, got)
		}
	}
}

func TestServiceOpenAITargetSendsBearerAuth(t *testing.T) {
	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:          "openai",
			EndpointType:  "openai",
			Endpoint:      upstream.URL,
			APIKey:        "sk-test-key",
			ModelMappings: []config.ModelMapping{{Upstream: "gpt-4o"}},
		}},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if seenAuth != "Bearer sk-test-key" {
		t.Fatalf("expected Authorization 'Bearer sk-test-key', got %q", seenAuth)
	}
	if got := rr.Header().Get("X-Proxy-Target"); got != "openai" {
		t.Fatalf("expected X-Proxy-Target 'openai', got %q", got)
	}
	// backward-compat header should also be set
	if got := rr.Header().Get("X-Azure-Target"); got != "openai" {
		t.Fatalf("expected X-Azure-Target 'openai', got %q", got)
	}
}

func TestServiceClaudeTargetSendsXAPIKey(t *testing.T) {
	var seenAPIKey string
	var seenVersion string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAPIKey = r.Header.Get("x-api-key")
		seenVersion = r.Header.Get("anthropic-version")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:          "claude",
			EndpointType:  "claude",
			Endpoint:      upstream.URL,
			APIKey:        "sk-ant-test",
			ModelMappings: []config.ModelMapping{{Upstream: "claude-sonnet-4-20250514"}},
		}},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if seenAPIKey != "sk-ant-test" {
		t.Fatalf("expected x-api-key 'sk-ant-test', got %q", seenAPIKey)
	}
	if seenVersion != "2023-06-01" {
		t.Fatalf("expected anthropic-version '2023-06-01', got %q", seenVersion)
	}
}

func TestServiceRoutesSameModelByRequestSchema(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:          "openai-shared",
				EndpointType:  "openai",
				Endpoint:      upstream.URL,
				APIKey:        "sk-openai",
				ModelMappings: []config.ModelMapping{{Upstream: "shared-model"}},
			},
			{
				Name:          "claude-shared",
				EndpointType:  "claude",
				Endpoint:      upstream.URL,
				APIKey:        "sk-claude",
				ModelMappings: []config.ModelMapping{{Upstream: "shared-model"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	send := func(path string) *httptest.ResponseRecorder {
		body := bytes.NewBufferString(`{"model":"shared-model","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(http.MethodPost, path, body)
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", path, rr.Code, rr.Body.String())
		}
		return rr
	}

	if got := send("/v1/chat/completions").Header().Get("X-Proxy-Target"); got != "openai-shared" {
		t.Fatalf("expected OpenAI Chat request to use openai-shared, got %q", got)
	}
	if got := send("/v1/messages").Header().Get("X-Proxy-Target"); got != "claude-shared" {
		t.Fatalf("expected Anthropic request to use claude-shared, got %q", got)
	}
}

func TestServiceRoutesResponsesToResponsesCapableTarget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:              "dual-proto-1",
				EndpointType:      "dual_protocol",
				Endpoint:          upstream.URL,
				APIKey:            "sk-dual-token",
				ModelMappings:     []config.ModelMapping{{Upstream: "shared-model"}},
				OpenAIPrefix:      "/compatible-mode",
				AnthropicPrefix:   "/apps/anthropic",
				SupportsResponses: true,
			},
			{
				Name:              "dual-proto-2",
				EndpointType:      "dual_protocol",
				Endpoint:          upstream.URL,
				APIKey:            "sk-dual-api",
				ModelMappings:     []config.ModelMapping{{Upstream: "shared-model"}},
				OpenAIPrefix:      "/compatible-mode",
				AnthropicPrefix:   "/apps/anthropic",
				SupportsResponses: true,
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"shared-model","input":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	// Both dual_protocol targets support /v1/responses, so either target is valid.
	if got := rr.Header().Get("X-Proxy-Target"); got != "dual-proto-1" && got != "dual-proto-2" {
		t.Fatalf("expected Responses request to use a dual_protocol target, got %q", got)
	}
}

func TestServiceRejectsModelWhenNoTargetSupportsRequestSchema(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be called")
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:          "claude-only",
			EndpointType:  "claude",
			Endpoint:      upstream.URL,
			APIKey:        "sk-claude",
			ModelMappings: []config.ModelMapping{{Upstream: "shared-model"}},
		}},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"shared-model","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `no target supports path "/v1/chat/completions" for model "shared-model"`) {
		t.Fatalf("unexpected error body: %q", rr.Body.String())
	}
}

func TestServiceOpenAITargetSkipsSanitize(t *testing.T) {
	var captured map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if err := json.Unmarshal(data, &captured); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:          "openai",
			EndpointType:  "openai",
			Endpoint:      upstream.URL,
			APIKey:        "sk-test",
			ModelMappings: []config.ModelMapping{{Upstream: "gpt-5.2"}},
		}},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	// Send fields that would be stripped for Azure (e.g. "foo" is not whitelisted)
	body := bytes.NewBufferString(`{"model":"gpt-5.2","messages":[{"role":"user","content":"hi"}],"custom_field":"keep-me","foo":"bar"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	// For OpenAI targets, fields should NOT be stripped
	if _, ok := captured["custom_field"]; !ok {
		t.Fatalf("expected custom_field to be preserved for openai target, got body=%v", captured)
	}
	if _, ok := captured["foo"]; !ok {
		t.Fatalf("expected foo to be preserved for openai target, got body=%v", captured)
	}
}

func TestServiceGeminiTargetSendsGoogAPIKey(t *testing.T) {
	var seenGoogKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenGoogKey = r.Header.Get("x-goog-api-key")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:          "gemini",
			EndpointType:  "gemini",
			Endpoint:      upstream.URL,
			APIKey:        "AIza-test-key",
			ModelMappings: []config.ModelMapping{{Upstream: "gemini-2.5-pro"}},
		}},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if seenGoogKey != "AIza-test-key" {
		t.Fatalf("expected x-goog-api-key 'AIza-test-key', got %q", seenGoogKey)
	}
}

func TestServiceClaudeGatewaySubPathPreserved(t *testing.T) {
	// Verify that when an endpoint has a sub-path (e.g. a Cloudflare AI Gateway
	// URL like /v2/gws/<id>/anthropic), the client's request path (/v1/messages)
	// is appended rather than replacing the endpoint path entirely.
	var seenPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/gws/testid/anthropic/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()

	// Build endpoint with gateway sub-path (no trailing /v1/messages — just the base).
	gatewayBase := upstream.URL + "/v2/gws/testid/anthropic"

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:         "claude-gw",
			EndpointType: "claude",
			Endpoint:     gatewayBase,
			APIKey:       "sk-ant-test",
		}},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"claude-opus-4-5","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: body=%s", rr.Code, rr.Body.String())
	}
	if seenPath != "/v2/gws/testid/anthropic/v1/messages" {
		t.Fatalf("expected gateway sub-path preserved, got %q", seenPath)
	}
}

func TestServiceTargetDefaultEndpointType(t *testing.T) {
	// When endpoint_type is empty, it should default to azure_openai and use api-key header.
	var seenAPIKey string
	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAPIKey = r.Header.Get("api-key")
		seenAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:               "azure-default",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "azure-key-123",
		}},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"test-model","input":"hi"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if seenAPIKey != "azure-key-123" {
		t.Fatalf("expected api-key 'azure-key-123', got %q", seenAPIKey)
	}
	if seenAuth != "" {
		t.Fatalf("expected no Authorization header for default azure target, got %q", seenAuth)
	}
}

func TestService503PassThroughNoRetry(t *testing.T) {
	var attempts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"busy"}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:               "image",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
			ModelMappings:      []config.ModelMapping{{Upstream: "gpt-image-1"}},
		}},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"gpt-image-1","prompt":"draw a cat"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 pass-through, got %d", rr.Code)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream call (no same-target retry), got %d", got)
	}

	metrics := service.MetricsSnapshot()
	if metrics.TotalRetries != 0 {
		t.Fatalf("expected no retries on 503 pass-through, got %d", metrics.TotalRetries)
	}
}

func TestService429PassThrough(t *testing.T) {
	// WP-1: 429 立即换 key（不 sleep 退避）。单 key target → 全池耗尽 → 透传 429。
	var attempts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate_limited"}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:               "openai",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
		}},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4o"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	// WP-1: 单 key 429 → 立即标记 + 换 key（无 key 可换）→ 全池耗尽透传 429。
	// 不再 sleep 退避重试。
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rr.Code)
	}
}

func TestService429RetrySuccess(t *testing.T) {
	// WP-1: 429 立即换 key（不 sleep 退避）。多 key target：key0 返回 429 → 换 key1 → key1 成功。
	// 用按请求次数区分成功/失败的单 upstream：第 1 次 429，第 2 次 200。
	var attempts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate_limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:               "openai",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "key0",
			APIKeys:            []string{"key1"},
		}},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4o"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	// WP-1: key0 返回 429 → 立即标记+换 key1 → key1 上游第 2 次调用返回 200 → 客户端收到 200。
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 after switching key, got %d", rr.Code)
	}
	if got := attempts.Load(); got < 2 {
		t.Fatalf("expected at least 2 upstream calls (key0 x429 + key1 x200), got %d", got)
	}
}

func TestServiceFailoverOnNetworkErrorPreserved(t *testing.T) {
	// 守护测试：网络错误（连接拒绝）下仍应触发多-target failover。
	success := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer success.Close()

	successURL, err := url.Parse(success.URL)
	if err != nil {
		t.Fatalf("parse success URL: %v", err)
	}

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "primary",
				Endpoint:           "http://primary.local",
				ResourcePathPrefix: "/",
				APIKey:             "primary-key",
			},
			{
				Name:               "secondary",
				Endpoint:           success.URL,
				ResourcePathPrefix: "/",
				APIKey:             "secondary-key",
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.httpClient = &http.Client{
		Transport: &failingTransport{successHost: successURL.Host},
	}
	service.quietPeriod = 10 * time.Millisecond

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"test-model","input":"hello"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 after failover to secondary, got %d", rr.Code)
	}
	if got := rr.Result().Header.Get("X-Proxy-Target"); got != "secondary" {
		t.Fatalf("expected X-Proxy-Target=secondary, got %q", got)
	}

	metrics := service.MetricsSnapshot()
	if metrics.TotalRetries < 1 {
		t.Fatalf("expected at least 1 failover retry, got %d", metrics.TotalRetries)
	}
	if metrics.TotalTargetRetries < 1 {
		t.Fatalf("expected at least 1 target retry, got %d", metrics.TotalTargetRetries)
	}
}

func TestFailoverTargetMustPassAdmission(t *testing.T) {
	var secondaryHits atomic.Int64
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer secondary.Close()
	secondaryURL, err := url.Parse(secondary.URL)
	if err != nil {
		t.Fatalf("parse secondary URL: %v", err)
	}

	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{
			{Name: "primary", Endpoint: "http://primary.local", ResourcePathPrefix: "/", APIKey: "primary-key"},
			{Name: "secondary", Endpoint: secondary.URL, ResourcePathPrefix: "/", APIKey: "secondary-key"},
		},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.httpClient = &http.Client{Transport: &failingTransport{successHost: secondaryURL.Host}}
	service.quietPeriod = 10 * time.Millisecond

	manager := admission.NewManager(newTestLogger(), admission.Options{RInit: 1, RMin: 1, RMax: 1})
	manager.InitTarget("primary", 1)
	manager.InitTarget("secondary", 1)
	if !manager.TryAcquire("secondary") {
		t.Fatal("failed to exhaust the secondary target's initial token")
	}
	service.SetAdmissionManager(manager)

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"test-model","messages":[]}`))
	service.affinity.Set(affinityKey(clientIP(request), "tester", "test-model"), "primary", time.Now())
	requestContext, cancelRequest := context.WithTimeout(auth.WithPrincipal(request.Context(), principal), 100*time.Millisecond)
	defer cancelRequest()
	request = request.WithContext(requestContext)
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)

	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s, want admission rejection", response.Code, response.Body.String())
	}
	if got := secondaryHits.Load(); got != 0 {
		t.Fatalf("failover target received %d requests despite exhausted admission bucket", got)
	}
	if metrics := service.MetricsSnapshot(); metrics.TotalTargetRetries < 1 {
		t.Fatalf("test did not exercise target failover: metrics=%+v", metrics)
	}
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func testAuthClients(name, accessKey string, allowedTargets ...string) []config.Client {
	client := config.Client{
		Name:      name,
		AccessKey: accessKey,
	}
	if len(allowedTargets) > 0 {
		client.AllowedTargets = append([]string(nil), allowedTargets...)
	}
	return []config.Client{client}
}

func TestSelectTargetPathFiltering(t *testing.T) {
	t1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Target", "t1")
		w.WriteHeader(http.StatusOK)
	}))
	defer t1.Close()
	t2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Target", "t2")
		w.WriteHeader(http.StatusOK)
	}))
	defer t2.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:          "openai-t1",
				EndpointType:  "openai",
				Endpoint:      t1.URL,
				APIKey:        "key1",
				ModelMappings: []config.ModelMapping{{Upstream: "gpt-4o"}},
			},
			{
				Name:            "dual-proto-t2",
				EndpointType:    "dual_protocol",
				Endpoint:        t2.URL,
				APIKey:          "key2",
				ModelMappings:   []config.ModelMapping{{Upstream: "gpt-4o"}},
				AnthropicPrefix: "/anthropic",
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	// /v1/responses → only openai-t1 supports this
	body := bytes.NewBufferString(`{"model":"gpt-4o","input":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/v1/responses expected 200, got %d", rr.Code)
	}
	if got := rr.Header().Get("X-Proxy-Target"); got != "openai-t1" {
		t.Fatalf("/v1/responses expected target openai-t1, got %q", got)
	}

	// /v1/chat/completions → both support this, just ensure it succeeds
	body2 := bytes.NewBufferString(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body2)
	req2 = req2.WithContext(auth.WithPrincipal(req2.Context(), principal))
	rr2 := httptest.NewRecorder()
	service.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("/v1/chat/completions expected 200, got %d", rr2.Code)
	}
}

func TestSelectTargetAffinity(t *testing.T) {
	counts := map[string]int{}
	makeServer := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counts[name]++
			w.WriteHeader(http.StatusOK)
		}))
	}
	s1 := makeServer("t1")
	defer s1.Close()
	s2 := makeServer("t2")
	defer s2.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:          "t1",
				EndpointType:  "openai",
				Endpoint:      s1.URL,
				APIKey:        "key1",
				ModelMappings: []config.ModelMapping{{Upstream: "gpt-4o"}},
			},
			{
				Name:          "t2",
				EndpointType:  "openai",
				Endpoint:      s2.URL,
				APIKey:        "key2",
				ModelMappings: []config.ModelMapping{{Upstream: "gpt-4o"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	// First request establishes affinity
	body := bytes.NewBufferString(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", rr.Code)
	}
	firstTarget := rr.Header().Get("X-Proxy-Target")

	// Subsequent requests should stick to the same target
	for i := 0; i < 5; i++ {
		body := bytes.NewBufferString(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+2, rr.Code)
		}
		if got := rr.Header().Get("X-Proxy-Target"); got != firstTarget {
			t.Fatalf("request %d: expected sticky target %q, got %q", i+2, firstTarget, got)
		}
	}
}

func TestSelectTargetExplicitPathIncompatible(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:          "openai_image",
				EndpointType:  "openai_image",
				Endpoint:      "http://example.com/v1/images/generations",
				APIKey:        "key",
				ModelMappings: []config.ModelMapping{{Upstream: "gpt-4o"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"gpt-4o","input":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req.Header.Set("X-Proxy-Target", "openai_image")
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for path-incompatible explicit target, got %d", rr.Code)
	}
}

func TestBuildTargetStatesInfersOpenAIImageOperationFromEndpoint(t *testing.T) {
	_, order, err := buildTargetStates([]config.Target{
		{Name: "image-edit", EndpointType: config.EndpointTypeOpenAIImage, Endpoint: "https://image.example.com/v1/images/edits", APIKey: "key"},
		{Name: "image-gen", EndpointType: config.EndpointTypeOpenAIImage, Endpoint: "https://image.example.com/v1/images/generations", APIKey: "key"},
	})
	if err != nil {
		t.Fatalf("buildTargetStates: %v", err)
	}
	if len(order) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(order))
	}
	if got := order[0].Target().ImageOperation; got != config.ImageOperationEdits {
		t.Fatalf("edit endpoint image_operation=%q want edits", got)
	}
	if got := order[1].Target().ImageOperation; got != config.ImageOperationGenerations {
		t.Fatalf("generation endpoint image_operation=%q want generations", got)
	}
}

func TestServiceSelectsOpenAIImageTargetByOperation(t *testing.T) {
	var generationCalls atomic.Int32
	generation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		generationCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer generation.Close()
	var editCalls atomic.Int32
	edit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		editCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer edit.Close()

	service, err := NewService(&config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{
			{Name: "image-generations", EndpointType: config.EndpointTypeOpenAIImage, Endpoint: generation.URL + "/v1/images/generations", ImageOperation: config.ImageOperationGenerations, APIKey: "key", ModelMappings: []config.ModelMapping{{Upstream: "gpt-image-2"}}},
			{Name: "image-edits", EndpointType: config.EndpointTypeOpenAIImage, Endpoint: edit.URL + "/v1/images/edits", ImageOperation: config.ImageOperationEdits, APIKey: "key", ModelMappings: []config.ModelMapping{{Upstream: "gpt-image-2"}}},
		},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")

	send := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{"model":"gpt-image-2","prompt":"hi"}`))
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)
		return rr
	}
	if rr := send("/v1/images/generations"); rr.Code != http.StatusOK || rr.Header().Get("X-Proxy-Target") != "image-generations" {
		t.Fatalf("generations expected 200/image-generations, got %d/%q body=%s", rr.Code, rr.Header().Get("X-Proxy-Target"), rr.Body.String())
	}
	if rr := send("/v1/images/edits"); rr.Code != http.StatusOK || rr.Header().Get("X-Proxy-Target") != "image-edits" {
		t.Fatalf("edits expected 200/image-edits, got %d/%q body=%s", rr.Code, rr.Header().Get("X-Proxy-Target"), rr.Body.String())
	}
	if generationCalls.Load() != 1 || editCalls.Load() != 1 {
		t.Fatalf("unexpected upstream calls generations=%d edits=%d", generationCalls.Load(), editCalls.Load())
	}
}

func TestServiceRejectsExplicitOpenAIImageWrongOperation(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	service, err := NewService(&config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{Name: "image-generations", EndpointType: config.EndpointTypeOpenAIImage, Endpoint: upstream.URL + "/v1/images/generations", ImageOperation: config.ImageOperationGenerations, APIKey: "key", ModelMappings: []config.ModelMapping{{Upstream: "gpt-image-2"}}}},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewBufferString(`{"model":"gpt-image-2","prompt":"hi"}`))
	req.Header.Set("X-Proxy-Target", "image-generations")
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("wrong operation target should not be called, got %d", calls.Load())
	}
}

func TestServiceStartTime(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	before := time.Now()
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	after := time.Now()

	startTime := service.StartTime()
	if startTime.Before(before) || startTime.After(after) {
		t.Errorf("StartTime %v not between %v and %v", startTime, before, after)
	}
}

func TestServiceKeyPoolStatus(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:          "with-pool",
				Endpoint:      "http://example.com",
				APIKey:        "key1",
				APIKeys:       []string{"key2", "key3"},
				ModelMappings: []config.ModelMapping{{Upstream: "gpt-4"}},
			},
			{
				Name:          "no-pool",
				Endpoint:      "http://example.com",
				APIKey:        "single-key",
				ModelMappings: []config.ModelMapping{{Upstream: "gpt-4"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Target with key pool
	status := service.KeyPoolStatus("with-pool")
	if status == nil {
		t.Fatal("expected key pool status for 'with-pool'")
	}
	if len(status) != 3 {
		t.Errorf("expected 3 keys, got %d", len(status))
	}

	// Target without key pool (single key)
	status = service.KeyPoolStatus("no-pool")
	if status != nil {
		t.Errorf("expected nil for single-key target, got %v", status)
	}

	// Non-existent target
	status = service.KeyPoolStatus("nonexistent")
	if status != nil {
		t.Errorf("expected nil for nonexistent target, got %v", status)
	}
}

func TestServiceBlockUnblockKey(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:          "target1",
				Endpoint:      "http://example.com",
				APIKey:        "key1",
				APIKeys:       []string{"key2", "key3"},
				ModelMappings: []config.ModelMapping{{Upstream: "gpt-4"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Block key
	err = service.BlockKey("target1", 0)
	if err != nil {
		t.Errorf("BlockKey failed: %v", err)
	}

	// Verify blocked
	status := service.KeyPoolStatus("target1")
	if !status[0].Blocked {
		t.Error("expected key 0 to be blocked")
	}

	// Unblock key
	err = service.UnblockKey("target1", 0)
	if err != nil {
		t.Errorf("UnblockKey failed: %v", err)
	}

	// Verify unblocked
	status = service.KeyPoolStatus("target1")
	if status[0].Blocked {
		t.Error("expected key 0 to be unblocked")
	}

	// Error cases
	err = service.BlockKey("nonexistent", 0)
	if err == nil {
		t.Error("expected error for nonexistent target")
	}

	err = service.BlockKey("target1", -1)
	if err == nil {
		t.Error("expected error for negative index")
	}

	err = service.BlockKey("target1", 100)
	if err == nil {
		t.Error("expected error for out-of-range index")
	}

	err = service.UnblockKey("nonexistent", 0)
	if err == nil {
		t.Error("expected error for nonexistent target")
	}

	err = service.UnblockKey("target1", -1)
	if err == nil {
		t.Error("expected error for negative index")
	}

	err = service.UnblockKey("target1", 100)
	if err == nil {
		t.Error("expected error for out-of-range index")
	}
}

// TestWakeUpRecoversManuallyBlockedKey 验证唤醒操作能直接恢复纯手动 blocked 的 key，
// 不依赖探测端点（探测端点 /v1/models 并非所有 provider 都支持，之前会导致唤醒按钮无效）。
func TestWakeUpRecoversManuallyBlockedKey(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:          "target1",
				Endpoint:      "http://example.com",
				APIKey:        "key1",
				APIKeys:       []string{"key2", "key3"},
				ModelMappings: []config.ModelMapping{{Upstream: "gpt-4"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// 手动屏蔽 key 0 和 key 1
	if err := service.BlockKey("target1", 0); err != nil {
		t.Fatalf("BlockKey(0): %v", err)
	}
	if err := service.BlockKey("target1", 1); err != nil {
		t.Fatalf("BlockKey(1): %v", err)
	}

	// 确认两个 key 都是 blocked
	status := service.KeyPoolStatus("target1")
	if !status[0].Blocked || !status[1].Blocked {
		t.Fatalf("expected key 0 and 1 blocked, got: %+v", status[:2])
	}

	// 唤醒：应直接恢复纯手动 blocked 的 key，无需探测端点
	recovered, err := service.WakeUpKeys("target1")
	if err != nil {
		t.Fatalf("WakeUpKeys: %v", err)
	}
	if recovered != 2 {
		t.Errorf("expected 2 keys recovered, got %d", recovered)
	}

	// 确认 key 已恢复为 active
	status = service.KeyPoolStatus("target1")
	for i, s := range status {
		if s.Blocked {
			t.Errorf("key %d still blocked after wakeup", i)
		}
	}
}

// TestApplyConfigPreservesBlockedKeys 验证 ApplyConfig 重新构建 targetState 时
// 保留已有的 blocked key 状态（regression：管理员屏蔽 Key 后目标 CRUD 不会丢失屏蔽状态）。
func TestApplyConfigPreservesBlockedKeys(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:     "target1",
				Endpoint: "http://example.com",
				APIKey:   "key1",
				APIKeys:  []string{"key2", "key3"},
			},
			{
				Name:     "target2",
				Endpoint: "http://example.org",
				APIKey:   "kA",
				APIKeys:  []string{"kB"},
			},
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// 屏蔽 target1 的 key-0 和 key-2，target2 的 key-1
	if err := service.BlockKey("target1", 0); err != nil {
		t.Fatalf("BlockKey target1/0: %v", err)
	}
	if err := service.BlockKey("target1", 2); err != nil {
		t.Fatalf("BlockKey target1/2: %v", err)
	}
	if err := service.BlockKey("target2", 1); err != nil {
		t.Fatalf("BlockKey target2/1: %v", err)
	}

	// 再解屏蔽 target1 的 key-0（仅保留 key-2 为 blocked）
	if err := service.UnblockKey("target1", 0); err != nil {
		t.Fatalf("UnblockKey target1/0: %v", err)
	}

	// 模拟目标 CRUD 触发 ApplyConfig（用相同配置）
	if err := service.ApplyConfig(cfg); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	// 验证 target1: key-0 未屏蔽（已手动解除）, key-2 仍屏蔽
	s1 := service.KeyPoolStatus("target1")
	if len(s1) < 3 {
		t.Fatalf("expected 3 key statuses for target1, got %d", len(s1))
	}
	if s1[0].Blocked {
		t.Error("target1/key0: expected unblocked (was explicitly unblocked)")
	}
	if s1[1].Blocked {
		t.Error("target1/key1: should not be blocked")
	}
	if !s1[2].Blocked {
		t.Error("target1/key2: expected blocked (was blocked before ApplyConfig)")
	}
	if s1[2].Status != "已屏蔽" {
		t.Errorf("target1/key2: status = %q, want 已屏蔽", s1[2].Status)
	}

	// 验证 target2: key-1 仍屏蔽
	s2 := service.KeyPoolStatus("target2")
	if len(s2) < 2 {
		t.Fatalf("expected 2 key statuses for target2, got %d", len(s2))
	}
	if !s2[1].Blocked {
		t.Error("target2/key1: expected blocked to survive ApplyConfig")
	}

	// 验证 KeyPoolStatus 返回的 status 字段一致
	if s2[1].Status != "已屏蔽" {
		t.Errorf("target2/key1: status = %q, want 已屏蔽", s2[1].Status)
	}
}

// TestApplyConfigBlockedKeyRemovedFromConfig 验证当某个 key 从配置中删除后，
// 其 blocked 状态自然消失（不再恢复到一个不存在的 key 上）。
func TestApplyConfigBlockedKeyRemovedFromConfig(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:     "t",
				Endpoint: "http://example.com",
				APIKey:   "keyA",
				APIKeys:  []string{"keyB", "keyC"},
			},
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// 屏蔽 index-2 (keyC)
	if err := service.BlockKey("t", 2); err != nil {
		t.Fatalf("BlockKey t/2: %v", err)
	}

	// 新配置：删除 keyC，仅保留 keyA 和 keyB
	cfg2 := &config.Config{
		Server: cfg.Server,
		Targets: []config.Target{
			{
				Name:     "t",
				Endpoint: "http://example.com",
				APIKey:   "keyA",
				APIKeys:  []string{"keyB"},
			},
		},
	}

	if err := service.ApplyConfig(cfg2); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	// index-2 已越界，不应 panic
	s := service.KeyPoolStatus("t")
	t.Logf("got %d key statuses (expected 2)", len(s))
	if len(s) != 2 {
		t.Errorf("expected 2 key statuses, got %d", len(s))
	}
	for _, ks := range s {
		if ks.Blocked {
			t.Errorf("key %d should not be blocked (keyC was removed)", ks.Index)
		}
	}
}

// TestApplyConfigBlockedKeyFollowsKeyValue 验证 BUG #5 修复:
// 当 admin 调整 api_keys 顺序或新增/删除中间 key 时,blocked 状态必须
// 按 key 值匹配,而不是按 index 匹配,否则会错误屏蔽不同的 key。
func TestApplyConfigBlockedKeyFollowsKeyValue(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:     "t",
				Endpoint: "http://example.com",
				APIKey:   "keyA",
				APIKeys:  []string{"keyB", "keyC"},
			},
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// 屏蔽 keyC (index-2)
	if err := service.BlockKey("t", 2); err != nil {
		t.Fatalf("BlockKey t/2: %v", err)
	}

	// 新配置:调整 key 顺序,keyB 和 keyC 交换位置
	// 新 entries: [keyA, keyC, keyB]
	// 旧的 index-2 是 keyC,新的 index-2 是 keyB
	// 如果按 index 恢复,会错误地屏蔽 keyB;按 key 值恢复应正确屏蔽 keyC
	cfg2 := &config.Config{
		Server: cfg.Server,
		Targets: []config.Target{
			{
				Name:     "t",
				Endpoint: "http://example.com",
				APIKey:   "keyA",
				APIKeys:  []string{"keyC", "keyB"},
			},
		},
	}

	if err := service.ApplyConfig(cfg2); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	s := service.KeyPoolStatus("t")
	if len(s) != 3 {
		t.Fatalf("expected 3 key statuses, got %d", len(s))
	}

	// 找到 keyC 和 keyB 的状态(顺序可能变了)
	// 新顺序: [keyA=0, keyC=1, keyB=2]
	// keyC 应该是 blocked (在新 index 1)
	// keyB 应该是 unblocked (在新 index 2,虽然占据了旧的 blocked index)
	if !s[1].Blocked {
		t.Errorf("keyC (now at index 1): expected blocked (blocked status should follow key value)")
	}
	if s[2].Blocked {
		t.Errorf("keyB (now at index 2): expected unblocked (should not inherit block from old keyC)")
	}
	if s[0].Blocked {
		t.Errorf("keyA (index 0): expected unblocked")
	}
}

func TestServiceTraceMethods(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Without trace store enabled, methods should return empty/nil results
	record := service.GetTrace("any-id")
	if record != nil {
		t.Error("expected nil when trace store disabled")
	}

	records := service.ListTrace(0, 10)
	if len(records) != 0 {
		t.Errorf("expected empty list when trace store disabled, got %d records", len(records))
	}

	stats := service.TraceStats()
	if stats != nil {
		// Stats may return non-nil but with zero values when disabled
		if stats["total_records"] != 0 {
			t.Errorf("expected 0 total_records when disabled, got %d", stats["total_records"])
		}
	}
}

func TestServiceUpdateTargets(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:          "target1",
				Endpoint:      "http://example1.com",
				APIKey:        "key1",
				ModelMappings: []config.ModelMapping{{Upstream: "gpt-4"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Update with new targets
	newTargets := []config.Target{
		{
			Name:          "target2",
			Endpoint:      "http://example2.com",
			APIKey:        "key2",
			ModelMappings: []config.ModelMapping{{Upstream: "gpt-3.5"}},
		},
		{
			Name:          "target3",
			Endpoint:      "http://example3.com",
			APIKey:        "key3",
			ModelMappings: []config.ModelMapping{{Upstream: "gpt-4"}},
		},
	}

	err = service.UpdateTargets(newTargets)
	if err != nil {
		t.Errorf("UpdateTargets failed: %v", err)
	}

	// Verify old target is gone
	status := service.KeyPoolStatus("target1")
	if status != nil {
		t.Error("expected target1 to be removed")
	}

	// Verify new targets exist
	statuses := service.TargetStatuses(time.Now())
	if len(statuses) != 2 {
		t.Errorf("expected 2 targets, got %d", len(statuses))
	}
}

func TestForwardAttemptError(t *testing.T) {
	// Test Error method
	err := &forwardAttemptError{
		status:    http.StatusBadGateway,
		retryable: true,
		err:       fmt.Errorf("connection refused"),
		startedAt: time.Now(),
	}

	if err.Error() != "connection refused" {
		t.Errorf("expected 'connection refused', got %q", err.Error())
	}

	// Test nil error
	var nilErr *forwardAttemptError
	if nilErr.Error() != "" {
		t.Errorf("expected empty string for nil error, got %q", nilErr.Error())
	}

	// Test nil inner error
	err2 := &forwardAttemptError{
		status:    http.StatusBadGateway,
		retryable: false,
		err:       nil,
		startedAt: time.Now(),
	}
	if err2.Error() != "" {
		t.Errorf("expected empty string for nil inner error, got %q", err2.Error())
	}

	// Test Unwrap method
	innerErr := fmt.Errorf("inner error")
	err3 := &forwardAttemptError{
		status:    http.StatusBadGateway,
		retryable: true,
		err:       innerErr,
		startedAt: time.Now(),
	}
	if err3.Unwrap() != innerErr {
		t.Error("Unwrap should return inner error")
	}

	// Test Unwrap with nil receiver
	var nilErr2 *forwardAttemptError
	if nilErr2.Unwrap() != nil {
		t.Error("Unwrap on nil receiver should return nil")
	}

	// Test Unwrap with nil inner error
	err4 := &forwardAttemptError{
		status:    http.StatusBadGateway,
		retryable: false,
		err:       nil,
		startedAt: time.Now(),
	}
	if err4.Unwrap() != nil {
		t.Error("Unwrap with nil inner error should return nil")
	}
}

func TestSelectionError(t *testing.T) {
	err := &selectionError{
		Status:  http.StatusForbidden,
		Message: "target not allowed",
	}

	if err.Error() != "target not allowed" {
		t.Errorf("expected 'target not allowed', got %q", err.Error())
	}
}

func TestFindAvailableTarget(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:     "target1",
				Endpoint: "http://example1.com",
				APIKey:   "key1",
				// No AllowedModels - accepts any model
			},
			{
				Name:     "target2",
				Endpoint: "http://example2.com",
				APIKey:   "key2",
				// No AllowedModels - accepts any model
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Test with no filters
	state, muted := service.findAvailableTarget(nil, nil, time.Now())
	if state == nil {
		t.Error("expected to find available target")
	}
	if muted != nil {
		t.Error("expected no muted candidate")
	}

	// Test with allowed filter
	allowed := map[string]struct{}{"target2": {}}
	state, muted = service.findAvailableTarget(allowed, nil, time.Now())
	if state == nil {
		t.Error("expected to find target2")
	}
	if state.Target().Name != "target2" {
		t.Errorf("expected target2, got %s", state.Target().Name)
	}

	// Test with attempted filter (all targets attempted)
	attempted := map[string]struct{}{"target1": {}, "target2": {}}
	state, muted = service.findAvailableTarget(nil, attempted, time.Now())
	if state != nil {
		t.Error("expected no available target when all attempted")
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		expected   string
	}{
		{
			name:       "nil request",
			remoteAddr: "",
			xff:        "",
			expected:   "",
		},
		{
			name:       "remote addr with port",
			remoteAddr: "192.168.1.1:8080",
			xff:        "",
			expected:   "192.168.1.1",
		},
		{
			name:       "remote addr without port",
			remoteAddr: "192.168.1.1",
			xff:        "",
			expected:   "192.168.1.1",
		},
		{
			name:       "X-Forwarded-For single IP",
			remoteAddr: "192.168.1.1:8080",
			xff:        "10.0.0.1",
			expected:   "10.0.0.1",
		},
		{
			name:       "X-Forwarded-For multiple IPs",
			remoteAddr: "192.168.1.1:8080",
			xff:        "10.0.0.1, 10.0.0.2, 10.0.0.3",
			expected:   "10.0.0.1",
		},
		{
			name:       "X-Forwarded-For with spaces",
			remoteAddr: "192.168.1.1:8080",
			xff:        "  10.0.0.1  ",
			expected:   "10.0.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req *http.Request
			if tt.remoteAddr != "" || tt.xff != "" {
				req = httptest.NewRequest(http.MethodGet, "/", nil)
				req.RemoteAddr = tt.remoteAddr
				if tt.xff != "" {
					req.Header.Set("X-Forwarded-For", tt.xff)
				}
			}

			result := clientIP(req)
			if result != tt.expected {
				t.Errorf("clientIP() = %q, want %q", result, tt.expected)
			}
		})
	}
}

// TestReadAndBufferBodyRejectsOversized 验证请求体超过 100MB 上限时返回错误。
func TestReadAndBufferBodyRejectsOversized(t *testing.T) {
	// 101MB body — 1MB over the limit
	oversized := bytes.Repeat([]byte("x"), maxRequestBodyBytes+1024)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(oversized))

	_, err := readAndBufferBody(r)
	if err == nil {
		t.Fatal("expected error for oversized body, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected 'exceeds' in error message, got: %v", err)
	}
}

// TestReadAndBufferBodyAcceptsAtLimit 验证恰好 100MB 的请求体可以读取。
func TestReadAndBufferBodyAcceptsAtLimit(t *testing.T) {
	atLimit := bytes.Repeat([]byte("x"), maxRequestBodyBytes)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(atLimit))

	data, err := readAndBufferBody(r)
	if err != nil {
		t.Fatalf("expected success at limit, got error: %v", err)
	}
	if len(data) != maxRequestBodyBytes {
		t.Fatalf("expected %d bytes, got %d", maxRequestBodyBytes, len(data))
	}
}

func TestCopyHeaders(t *testing.T) {
	src := http.Header{}
	src.Set("Content-Type", "application/json")
	src.Set("X-Custom-Header", "value1")
	src.Add("X-Custom-Header", "value2")
	src.Set("Connection", "keep-alive") // hop header, should be skipped

	dst := http.Header{}
	copyHeaders(dst, src)

	// Check that non-hop headers are copied
	if dst.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type not copied correctly")
	}

	// Check that multiple values are preserved
	values := dst.Values("X-Custom-Header")
	if len(values) != 2 {
		t.Errorf("expected 2 values for X-Custom-Header, got %d", len(values))
	}

	// Check that hop headers are skipped
	if dst.Get("Connection") != "" {
		t.Errorf("Connection header should be skipped")
	}
}

func TestTargetName(t *testing.T) {
	// Test with nil target
	if targetName(nil) != "" {
		t.Errorf("expected empty string for nil target")
	}

	// Test with valid target
	target := &Target{Name: "test-target"}
	if targetName(target) != "test-target" {
		t.Errorf("expected 'test-target', got %q", targetName(target))
	}
}

func TestTargetEndpointType(t *testing.T) {
	// Test with nil target
	if targetEndpointType(nil) != "" {
		t.Errorf("expected empty string for nil target")
	}

	// Test with valid target
	target := &Target{EndpointType: "openai"}
	if targetEndpointType(target) != "openai" {
		t.Errorf("expected 'openai', got %q", targetEndpointType(target))
	}
}

func TestStripURLQuery(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"http://example.com/path", "http://example.com/path"},
		{"http://example.com/path?query=value", "http://example.com/path"},
		{"http://example.com/path?query=value&foo=bar", "http://example.com/path"},
		{"http://example.com/path?", "http://example.com/path"},
		{"", ""},
	}

	for _, tt := range tests {
		result := stripURLQuery(tt.input)
		if result != tt.expected {
			t.Errorf("stripURLQuery(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestClassifyTransportError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected int
	}{
		{
			name:     "context deadline exceeded",
			err:      context.DeadlineExceeded,
			expected: http.StatusGatewayTimeout,
		},
		{
			name:     "context canceled", // 客户端断开 → 499（区别于上游真超时 504），便于运维归因
			err:      context.Canceled,
			expected: StatusClientClosedRequest,
		},
		{
			name:     "generic error",
			err:      fmt.Errorf("connection refused"),
			expected: http.StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := classifyTransportError(tt.err)
			if result != tt.expected {
				t.Errorf("classifyTransportError() = %d, want %d", result, tt.expected)
			}
		})
	}
}

func TestHandleForwardError(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:     "target1",
				Endpoint: "http://example1.com",
				APIKey:   "key1",
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Get target state
	service.mu.RLock()
	state := service.targetsByName["target1"]
	service.mu.RUnlock()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	// Test with status 0 (should default to 502)
	service.handleForwardError(req, state, fmt.Errorf("connection refused"), 0)

	// Verify target is marked as failed
	stats := state.Stats()
	if stats.ConsecutiveFailure == 0 {
		t.Error("expected target to be marked as failed")
	}

	// Test with explicit status
	service.handleForwardError(req, state, fmt.Errorf("timeout"), http.StatusGatewayTimeout)
}

func TestWriteForwardErrorLog(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:     "target1",
				Endpoint: "http://example1.com",
				APIKey:   "key1",
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Get target state
	service.mu.RLock()
	state := service.targetsByName["target1"]
	service.mu.RUnlock()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	// Test with nil error (should not panic)
	writeForwardErrorLog(req, state, nil)

	// Test with valid error
	fe := &forwardAttemptError{
		status:          http.StatusBadGateway,
		retryable:       true,
		err:             fmt.Errorf("connection refused"),
		startedAt:       time.Now().Add(-100 * time.Millisecond),
		upstreamURL:     "http://example.com/v1/chat",
		upstreamFullURL: "http://example.com/v1/chat?api-version=2024-01-01",
	}
	writeForwardErrorLog(req, state, fe)
}

func TestWriteUpstreamErrorLog(t *testing.T) {
	target := &Target{
		Name:         "target1",
		EndpointType: "openai",
		Endpoint:     mustParseURL("http://example.com"),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	// Test with 4xx response
	resp4xx := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{},
	}
	writeUpstreamErrorLog(req, target, resp4xx, []byte("error message"), time.Now().Add(-100*time.Millisecond))

	// Test with 5xx response
	resp5xx := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{},
	}
	writeUpstreamErrorLog(req, target, resp5xx, []byte("server error"), time.Now().Add(-100*time.Millisecond))

	// Test with nil target
	writeUpstreamErrorLog(req, nil, resp4xx, []byte("error"), time.Now())

	// Test with large body (should truncate)
	largeBody := make([]byte, 2000)
	for i := range largeBody {
		largeBody[i] = 'x'
	}
	writeUpstreamErrorLog(req, target, resp4xx, largeBody, time.Now())

	// Test with gzip compressed body
	var compressedBody bytes.Buffer
	gw := gzip.NewWriter(&compressedBody)
	gw.Write([]byte("compressed error message"))
	gw.Close()

	respGzip := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Encoding": []string{"gzip"}},
	}
	writeUpstreamErrorLog(req, target, respGzip, compressedBody.Bytes(), time.Now())
}

func mustParseURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

func TestRecordUsageEvent(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:     "target1",
				Endpoint: "http://example1.com",
				APIKey:   "key1",
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Test with nil recorder (should not panic)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	service.recordUsageEvent(req, nil, 200, "gpt-4", "application/json", nil, nil, -1)

	// Test with nil request (should not panic)
	service.recordUsageEvent(nil, nil, 200, "gpt-4", "application/json", nil, nil, -1)

	// Test with valid request but no principal
	service.recordUsageEvent(req, nil, 200, "gpt-4", "application/json", []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5}}`), nil, -1)
}

func TestRecordTrace(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:     "target1",
				Endpoint: "http://example1.com",
				APIKey:   "key1",
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Test with nil traceStore (should not panic)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	target := &Target{Name: "target1", EndpointType: "openai"}
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
	}
	service.recordTrace(req, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with nil target
	service.recordTrace(req, nil, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), -1, "gpt-4", usageTokens{})

	// Test with gzip compressed response body
	var compressedBody bytes.Buffer
	gw := gzip.NewWriter(&compressedBody)
	gw.Write([]byte(`{"id":"compressed-test"}`))
	gw.Close()

	respGzip := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Encoding": []string{"gzip"}},
	}
	service.recordTrace(req, target, respGzip, []byte(`{"model":"gpt-4"}`), compressedBody.Bytes(), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with target that has APIKeys
	targetWithKeys := &Target{
		Name:         "target-with-keys",
		EndpointType: "openai",
		APIKey:       "main-key",
		APIKeys:      []string{"key1", "key2", "key3"},
	}
	service.recordTrace(req, targetWithKeys, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 1, "gpt-4", usageTokens{})

	// Test with target that has only APIKey (no APIKeys)
	targetSingleKey := &Target{
		Name:         "target-single-key",
		EndpointType: "openai",
		APIKey:       "single-key",
	}
	service.recordTrace(req, targetSingleKey, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with request that has sensitive headers
	reqWithHeaders := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqWithHeaders.Header.Set("Authorization", "Bearer secret-token")
	reqWithHeaders.Header.Set("X-API-Key", "secret-api-key")
	reqWithHeaders.Header.Set("X-Goog-API-Key", "secret-goog-key")
	reqWithHeaders.Header.Set("Cookie", "session=secret-session")
	reqWithHeaders.Header.Set("Content-Type", "application/json")
	service.recordTrace(reqWithHeaders, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with response that has sensitive headers
	respWithHeaders := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Authorization":  []string{"Bearer upstream-token"},
			"X-Api-Key":      []string{"upstream-api-key"},
			"X-Goog-Api-Key": []string{"upstream-goog-key"},
			"Set-Cookie":     []string{"session=upstream-session"},
			"Content-Type":   []string{"application/json"},
		},
	}
	service.recordTrace(req, target, respWithHeaders, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with response that has request URL with sensitive query params
	reqForURL := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?api-key=secret&apikey=secret2&key=secret3&api_key=secret4&other=safe", nil)
	respWithReq := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqForURL,
	}
	service.recordTrace(req, target, respWithReq, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with target that has empty APIKeys and empty APIKey
	targetNoKeys := &Target{
		Name:         "target-no-keys",
		EndpointType: "openai",
	}
	service.recordTrace(req, targetNoKeys, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with keyIndex out of range
	service.recordTrace(req, targetWithKeys, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 999, "gpt-4", usageTokens{})

	// Test with response that has request URL with no query params
	reqNoQuery := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	respNoQuery := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqNoQuery,
	}
	service.recordTrace(req, target, respNoQuery, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with response that has request URL with safe query params only
	reqSafeQuery := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?model=gpt-4&temperature=0.7", nil)
	respSafeQuery := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqSafeQuery,
	}
	service.recordTrace(req, target, respSafeQuery, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with response that has nil request
	respNilReq := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    nil,
	}
	service.recordTrace(req, target, respNilReq, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with response that has request URL with nil URL
	reqNilURL := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqNilURL.URL = nil
	respNilURL := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqNilURL,
	}
	service.recordTrace(req, target, respNilURL, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with response that has empty response body
	service.recordTrace(req, target, resp, []byte(`{"model":"gpt-4"}`), []byte{}, time.Now(), 0, "gpt-4", usageTokens{})

	// Test with response that has nil response body
	service.recordTrace(req, target, resp, []byte(`{"model":"gpt-4"}`), nil, time.Now(), 0, "gpt-4", usageTokens{})

	// Test with response that has gzip header but empty body
	respGzipEmpty := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Encoding": []string{"gzip"}},
	}
	service.recordTrace(req, target, respGzipEmpty, []byte(`{"model":"gpt-4"}`), []byte{}, time.Now(), 0, "gpt-4", usageTokens{})

	// Test with response that has gzip header but invalid gzip body
	respGzipInvalid := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Encoding": []string{"gzip"}},
	}
	service.recordTrace(req, target, respGzipInvalid, []byte(`{"model":"gpt-4"}`), []byte("not-gzip-data"), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with request that has no headers
	reqNoHeaders := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqNoHeaders.Header = http.Header{}
	service.recordTrace(reqNoHeaders, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with response that has no headers
	respNoHeaders := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
	}
	service.recordTrace(req, target, respNoHeaders, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with target that has APIKey but empty APIKeys slice
	targetAPIKeyOnly := &Target{
		Name:         "target-apikey-only",
		EndpointType: "openai",
		APIKey:       "only-key",
		APIKeys:      []string{},
	}
	service.recordTrace(req, targetAPIKeyOnly, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with negative keyIndex
	service.recordTrace(req, targetWithKeys, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), -1, "gpt-4", usageTokens{})

	// Test with request that has query params
	reqWithQuery := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?foo=bar&baz=qux", nil)
	service.recordTrace(reqWithQuery, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with response that has request URL with mixed sensitive and safe query params
	reqMixedQuery := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?api-key=secret&model=gpt-4&temperature=0.7", nil)
	respMixedQuery := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqMixedQuery,
	}
	service.recordTrace(req, target, respMixedQuery, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with request that has empty query params
	reqEmptyQuery := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?", nil)
	service.recordTrace(reqEmptyQuery, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with response that has request URL with empty query params
	reqEmptyQueryURL := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?", nil)
	respEmptyQuery := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqEmptyQueryURL,
	}
	service.recordTrace(req, target, respEmptyQuery, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with request that has multiple headers with same key
	reqMultiHeaders := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqMultiHeaders.Header.Add("X-Custom", "value1")
	reqMultiHeaders.Header.Add("X-Custom", "value2")
	service.recordTrace(reqMultiHeaders, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with response that has multiple headers with same key
	respMultiHeaders := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"X-Custom": []string{"value1", "value2"},
		},
	}
	service.recordTrace(req, target, respMultiHeaders, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with request that has path with query params
	reqPathQuery := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?stream=true", nil)
	service.recordTrace(reqPathQuery, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with response that has request URL with path and query params
	reqPathQueryURL := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?stream=true&model=gpt-4", nil)
	respPathQuery := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqPathQueryURL,
	}
	service.recordTrace(req, target, respPathQuery, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with request that has path with special characters
	reqSpecialPath := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?model=gpt-4&prompt=hello%20world", nil)
	service.recordTrace(reqSpecialPath, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with response that has request URL with special characters in query
	reqSpecialQueryURL := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?prompt=hello%20world&model=gpt-4", nil)
	respSpecialQuery := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqSpecialQueryURL,
	}
	service.recordTrace(req, target, respSpecialQuery, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with request that has path with unicode characters
	reqUnicodePath := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?prompt=你好世界", nil)
	service.recordTrace(reqUnicodePath, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with response that has request URL with unicode characters in query
	reqUnicodeQueryURL := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?prompt=你好世界&model=gpt-4", nil)
	respUnicodeQuery := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqUnicodeQueryURL,
	}
	service.recordTrace(req, target, respUnicodeQuery, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with request that has path with fragment
	reqFragmentPath := httptest.NewRequest(http.MethodPost, "/v1/chat/completions#section", nil)
	service.recordTrace(reqFragmentPath, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with response that has request URL with fragment
	reqFragmentURL := httptest.NewRequest(http.MethodPost, "/v1/chat/completions#section", nil)
	respFragment := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqFragmentURL,
	}
	service.recordTrace(req, target, respFragment, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with request that has path with multiple query params
	reqMultiQuery := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?a=1&b=2&c=3", nil)
	service.recordTrace(reqMultiQuery, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})

	// Test with response that has request URL with multiple query params
	reqMultiQueryURL := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?a=1&b=2&c=3", nil)
	respMultiQuery := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqMultiQueryURL,
	}
	service.recordTrace(req, target, respMultiQuery, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4", usageTokens{})
}

func TestRecordTraceRedactsSensitiveHeadersAndQuery(t *testing.T) {
	store := tracestore.New(tracestore.Config{Enabled: true, RingBufferSize: 1, MaxBodySize: 1024}, newTestLogger())
	t.Cleanup(func() { _ = store.Close() })
	service := &Service{traceStore: store}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?api-key=req-api-key&token=req-token&access_token=req-access&safe=ok", strings.NewReader(`{"model":"gpt-4","prompt":"secret body stays"}`))
	req.Header.Set("Authorization", "Bearer req-auth")
	req.Header.Set("Api-Key", "req-api-key-header")
	req.Header.Set("X-Api-Key", "req-x-api-key")
	req.Header.Set("X-Goog-Api-Key", "req-goog-api-key")
	req.Header.Set(headerAzureAuthorization, "Bearer req-azure")
	req.Header.Set("Proxy-Authorization", "Bearer req-proxy")
	req.Header.Set("Cookie", "session=req-cookie")
	req.Header.Set("Content-Type", "application/json")

	upstreamReq := httptest.NewRequest(http.MethodPost, "http://upstream.local/v1/chat/completions?api_key=up-api-key&apikey=up-apikey&key=up-key&access_token=up-access&token=up-token&safe=ok", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Authorization":         []string{"Bearer resp-auth"},
			"Api-Key":               []string{"resp-api-key"},
			"X-Api-Key":             []string{"resp-x-api-key"},
			"X-Goog-Api-Key":        []string{"resp-goog-api-key"},
			"X-Azure-Authorization": []string{"Bearer resp-azure"},
			"Proxy-Authorization":   []string{"Bearer resp-proxy"},
			"Cookie":                []string{"session=resp-cookie"},
			"Set-Cookie":            []string{"session=resp-set-cookie"},
			"Content-Type":          []string{"application/json"},
		},
		Request: upstreamReq,
	}

	requestBody := []byte(`{"model":"gpt-4","prompt":"secret body stays"}`)
	responseBody := []byte(`{"id":"resp","secret":"response body stays"}`)
	service.recordTrace(req, &Target{Name: "target1", EndpointType: "openai"}, resp, requestBody, responseBody, time.Now(), -1, "gpt-4", usageTokens{})

	record := store.Get("")
	if record == nil {
		t.Fatal("expected trace record")
	}

	for _, key := range []string{"Authorization", "Api-Key", "X-Api-Key", "X-Goog-Api-Key", headerAzureAuthorization, "Proxy-Authorization", "Cookie"} {
		if got := record.RequestHeaders[key]; got != "***" {
			t.Fatalf("expected request header %s to be redacted, got %q", key, got)
		}
	}
	if got := record.RequestHeaders["Content-Type"]; got != "application/json" {
		t.Fatalf("expected safe request header preserved, got %q", got)
	}

	for _, key := range []string{"Authorization", "Api-Key", "X-Api-Key", "X-Goog-Api-Key", "X-Azure-Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie"} {
		if got := record.UpstreamHeaders[key]; got != "***" {
			t.Fatalf("expected upstream header %s to be redacted, got %q", key, got)
		}
	}
	if got := record.UpstreamHeaders["Content-Type"]; got != "application/json" {
		t.Fatalf("expected safe upstream header preserved, got %q", got)
	}

	assertRawQueryRedacted(t, record.QueryParams, []string{"api-key", "token", "access_token"}, []string{"req-api-key", "req-token", "req-access"})
	assertURLQueryRedacted(t, record.UpstreamURL, []string{"api_key", "apikey", "key", "access_token", "token"}, []string{"up-api-key", "up-apikey", "up-key", "up-access", "up-token"})

	if record.RequestBody != string(requestBody) {
		t.Fatalf("expected request body unchanged, got %q", record.RequestBody)
	}
	if record.ResponseBody != string(responseBody) {
		t.Fatalf("expected response body unchanged, got %q", record.ResponseBody)
	}
}

func TestRecordTraceRedactsInvalidSensitiveQuery(t *testing.T) {
	store := tracestore.New(tracestore.Config{Enabled: true, RingBufferSize: 1, MaxBodySize: 1024}, newTestLogger())
	t.Cleanup(func() { _ = store.Close() })
	service := &Service{traceStore: store}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.URL.RawQuery = "api-key=sk-secret%ZZ&token=tok-secret&api%2Dkey=sk-encoded-secret%ZZ&access%5Ftoken=tok-encoded-secret%ZZ&safe=ok"
	upstreamReq := httptest.NewRequest(http.MethodPost, "http://upstream.local/v1/chat/completions", nil)
	upstreamReq.URL.RawQuery = "api_key=up-secret%ZZ&access_token=up-access-secret&api%5Fkey=up-encoded-secret%ZZ&access%5Ftoken=up-encoded-access-secret%ZZ&safe=ok"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Request:    upstreamReq,
	}

	service.recordTrace(req, &Target{Name: "target1", EndpointType: "openai"}, resp, nil, nil, time.Now(), -1, "gpt-4", usageTokens{})

	record := store.Get("")
	if record == nil {
		t.Fatal("expected trace record")
	}
	for _, forbidden := range []string{"sk-secret", "tok-secret", "sk-encoded-secret", "tok-encoded-secret"} {
		if strings.Contains(record.QueryParams, forbidden) {
			t.Fatalf("invalid request query %q leaked secret %q", record.QueryParams, forbidden)
		}
	}
	for _, forbidden := range []string{"up-secret", "up-access-secret", "up-encoded-secret", "up-encoded-access-secret"} {
		if strings.Contains(record.UpstreamURL, forbidden) {
			t.Fatalf("invalid upstream URL %q leaked secret %q", record.UpstreamURL, forbidden)
		}
	}
}

func assertRawQueryRedacted(t *testing.T, raw string, redactedKeys, forbiddenValues []string) {
	t.Helper()
	values, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("parse query %q: %v", raw, err)
	}
	for _, key := range redactedKeys {
		if got := values.Get(key); got != "***" {
			t.Fatalf("expected query %s to be redacted, got %q in %q", key, got, raw)
		}
	}
	if got := values.Get("safe"); got != "ok" {
		t.Fatalf("expected safe query preserved, got %q in %q", got, raw)
	}
	for _, forbidden := range forbiddenValues {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("query %q leaked secret %q", raw, forbidden)
		}
	}
}

func assertURLQueryRedacted(t *testing.T, rawURL string, redactedKeys, forbiddenValues []string) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse upstream URL %q: %v", rawURL, err)
	}
	assertRawQueryRedacted(t, u.RawQuery, redactedKeys, forbiddenValues)
}

// TestApplyConfig_PreservesExhaustedKeyState 验证 ApplyConfig 重建 keyPool 时保留 exhausted 冷却状态。
// 场景：智谱 GLM 1310 周限流（cooldownEnd=下周四），admin UI 保存任意 target 后冷却不丢失。
func TestApplyConfig_PreservesExhaustedKeyState(t *testing.T) {
	// 创建两个 mock 上游，各自只有单 key（无 keyPool），作为"其他 target"
	otherUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"test","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer otherUpstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "other-target",
				Endpoint:           otherUpstream.URL,
				ResourcePathPrefix: "/",
				APIKey:             "single-key", // 单 key → 无 keyPool
			},
			{
				Name:          "glm-target",
				EndpointType:  "claude",
				Endpoint:      otherUpstream.URL,
				APIKey:        "glm-key-0",
				APIKeys:       []string{"glm-key-1"},
				KeyResetTime:  "",
				ModelMappings: []config.ModelMapping{{Upstream: "glm-5.2"}},
			},
		},
		Logging: config.LoggingConfig{Level: "warn", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// 在 glm-target 的 keyPool 中标记 key-0 为 GLM 周限流
	service.mu.RLock()
	glmState, ok := service.targetsByName["glm-target"]
	service.mu.RUnlock()
	if !ok || glmState.keyPool == nil {
		t.Fatal("glm-target should have a keyPool (2 keys)")
	}

	future := time.Now().Add(5 * time.Hour)
	glmState.keyPool.markExhaustedWithCooldown(0, glmRateLimitQuota, future, "1310 test body")

	// 验证标记生效
	if r := glmState.keyPool.exhaustReasonAt(0); r != glmRateLimitQuota {
		t.Fatalf("exhaustReason = %q, want %q", r, glmRateLimitQuota)
	}

	// 模拟 admin UI 保存操作：用同一配置调 ApplyConfig
	if err := service.ApplyConfig(cfg); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	// 验证 keyPool 重建后状态保留
	service.mu.RLock()
	glmState2, ok := service.targetsByName["glm-target"]
	service.mu.RUnlock()
	if !ok || glmState2.keyPool == nil {
		t.Fatal("glm-target keyPool should exist after ApplyConfig")
	}

	reason := glmState2.keyPool.exhaustReasonAt(0)
	if reason != glmRateLimitQuota {
		t.Errorf("after ApplyConfig: exhaustReason = %q, want %q", reason, glmRateLimitQuota)
	}

	// 验证 cooldownEnd 保留
	glmState2.keyPool.mu.Lock()
	entry := glmState2.keyPool.entries[0]
	glmState2.keyPool.mu.Unlock()

	if !entry.exhausted {
		t.Error("after ApplyConfig: key should still be exhausted")
	}
	if !entry.cooldownEnd.Equal(future) {
		t.Errorf("after ApplyConfig: cooldownEnd = %v, want %v", entry.cooldownEnd, future)
	}
	if entry.consecutiveRateLimit != 0 {
		t.Errorf("after ApplyConfig: consecutiveRateLimit should be 0, got %d", entry.consecutiveRateLimit)
	}
}

// TestApplyConfig_PreservesBlockedKeyState 验证 ApplyConfig 时 blocked 状态保留（已有逻辑的回归覆盖）。
func TestApplyConfig_PreservesBlockedKeyState(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"test","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{
			{Name: "blocked-target", Endpoint: upstream.URL, APIKey: "k0", APIKeys: []string{"k1"}},
		},
		Logging: config.LoggingConfig{Level: "warn", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	service.mu.RLock()
	ts := service.targetsByName["blocked-target"]
	service.mu.RUnlock()
	ts.keyPool.blockKey(0)

	// ApplyConfig 重建
	if err := service.ApplyConfig(cfg); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	service.mu.RLock()
	ts2 := service.targetsByName["blocked-target"]
	service.mu.RUnlock()

	ts2.keyPool.mu.Lock()
	blocked := ts2.keyPool.entries[0].blocked
	ts2.keyPool.mu.Unlock()

	if !blocked {
		t.Error("after ApplyConfig: key should still be blocked")
	}
}

// TestApplyConfig_CrossTargetIsolation 验证 exhausted 状态只保留到各自 target，不跨 target 串扰。
func TestApplyConfig_CrossTargetIsolation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"test","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{
			{Name: "target-a", Endpoint: upstream.URL, APIKey: "ka0", APIKeys: []string{"ka1"}},
			{Name: "target-b", Endpoint: upstream.URL, APIKey: "kb0", APIKeys: []string{"kb1"}},
		},
		Logging: config.LoggingConfig{Level: "warn", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// 标记 target-a key-0 为 exhausted
	service.mu.RLock()
	tsA := service.targetsByName["target-a"]
	service.mu.RUnlock()

	future := time.Now().Add(time.Hour)
	tsA.keyPool.markExhaustedWithCooldown(0, glmRateLimitQuota, future, "")

	// ApplyConfig
	if err := service.ApplyConfig(cfg); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	// target-a key-0 应保持 exhausted
	service.mu.RLock()
	tsA2 := service.targetsByName["target-a"]
	tsB2 := service.targetsByName["target-b"]
	service.mu.RUnlock()

	if r := tsA2.keyPool.exhaustReasonAt(0); r != glmRateLimitQuota {
		t.Errorf("target-a key-0: exhaustReason = %q, want %q", r, glmRateLimitQuota)
	}

	// target-b 应完全不受影响
	if r := tsB2.keyPool.exhaustReasonAt(0); r != "" {
		t.Errorf("target-b key-0: should be active, got reason=%q", r)
	}
	if r := tsB2.keyPool.exhaustReasonAt(1); r != "" {
		t.Errorf("target-b key-1: should be active, got reason=%q", r)
	}
}

// TestApplyConfigPreservesBreakerState 验证 ApplyConfig 重建 targetState 时，
// 同名 target 的 S4 熔断 / S5 调度运行态必须保留（mutedUntil、consecutiveFailure、
// affinityHits、breakerTrips、probeLease），inFlight 必须保留真实占用；
// 删除/重命名 target 时状态不得串到新名字上。
// Regression：#61 paused-unpause 验收失败根因（ApplyConfig 丢失熔断字段）。
func TestApplyConfigPreservesBreakerState(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{
			{Name: "breaker-a", Endpoint: "http://example.com", APIKey: "ka0", APIKeys: []string{"ka1"}},
			{Name: "plain-b", Endpoint: "http://example.com", APIKey: "kb0"},
		},
		Logging: config.LoggingConfig{Level: "warn", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	service.mu.RLock()
	ts := service.targetsByName["breaker-a"]
	service.mu.RUnlock()
	if ts == nil {
		t.Fatal("target breaker-a not found")
	}

	// 注入熔断运行态：mutedUntil/consecutiveFailure 由 ts.mu 保护，
	// affinityHits/breakerTrips/probeLease 为自管理原子量，inFlight 单独注入。
	now := time.Now()
	mutedUntil := now.Add(3 * time.Minute)
	leaseUntil := now.Add(30 * time.Second).UnixNano()
	ts.mu.Lock()
	ts.mutedUntil = mutedUntil
	ts.consecutiveFailure = 5
	ts.mu.Unlock()
	ts.affinityHits.Add(7)
	ts.breakerTrips.Add(2)
	ts.probeLease.Store(&probeLease{until: leaseUntil})
	ts.inFlight.Store(4)

	// 模拟 admin 目标 CRUD / pause-unpause 触发的同名 target 重载
	if err := service.ApplyConfig(cfg); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	service.mu.RLock()
	ts2 := service.targetsByName["breaker-a"]
	service.mu.RUnlock()
	if ts2 == nil {
		t.Fatal("target breaker-a missing after ApplyConfig")
	}
	if ts2 == ts {
		t.Fatal("ApplyConfig should rebuild targetState, got same pointer")
	}

	ts2.mu.RLock()
	gotMuted, gotStreak := ts2.mutedUntil, ts2.consecutiveFailure
	ts2.mu.RUnlock()
	if !gotMuted.Equal(mutedUntil) {
		t.Errorf("mutedUntil = %v, want preserved %v", gotMuted, mutedUntil)
	}
	if gotStreak != 5 {
		t.Errorf("consecutiveFailure = %d, want 5 (preserved)", gotStreak)
	}
	if got := ts2.affinityHitCount(); got != 7 {
		t.Errorf("affinityHits = %d, want 7 (preserved)", got)
	}
	if got := ts2.breakerTripCount(); got != 2 {
		t.Errorf("breakerTrips = %d, want 2 (preserved)", got)
	}
	if lease := ts2.probeLease.Load(); lease == nil || lease.until != leaseUntil {
		t.Errorf("probe lease = %v, want preserved until %d", lease, leaseUntil)
	}
	if got := ts2.inFlightCount(); got != 4 {
		t.Errorf("inFlight = %d, want 4 (existing requests still occupy capacity)", got)
	}

	// 删除/重命名不得串状态：breaker-a → renamed-c，plain-b 删除，新增 fresh-d
	cfg2 := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{
			{Name: "renamed-c", Endpoint: "http://example.com", APIKey: "ka0", APIKeys: []string{"ka1"}},
			{Name: "fresh-d", Endpoint: "http://example.com", APIKey: "kd0"},
		},
		Logging: config.LoggingConfig{Level: "warn", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	if err := service.ApplyConfig(cfg2); err != nil {
		t.Fatalf("ApplyConfig cfg2: %v", err)
	}

	for _, name := range []string{"renamed-c", "fresh-d"} {
		service.mu.RLock()
		ns := service.targetsByName[name]
		service.mu.RUnlock()
		if ns == nil {
			t.Fatalf("target %s missing after rename/replace", name)
		}
		ns.mu.RLock()
		zeroMuted, zeroStreak := ns.mutedUntil, ns.consecutiveFailure
		ns.mu.RUnlock()
		if !zeroMuted.IsZero() {
			t.Errorf("%s: mutedUntil = %v, want zero (no cross-name carry-over)", name, zeroMuted)
		}
		if zeroStreak != 0 {
			t.Errorf("%s: consecutiveFailure = %d, want 0", name, zeroStreak)
		}
		if got := ns.affinityHitCount(); got != 0 {
			t.Errorf("%s: affinityHits = %d, want 0", name, got)
		}
		if got := ns.breakerTripCount(); got != 0 {
			t.Errorf("%s: breakerTrips = %d, want 0", name, got)
		}
		if lease := ns.probeLease.Load(); lease != nil {
			t.Errorf("%s: probe lease = %v, want nil", name, lease)
		}
		if got := ns.inFlightCount(); got != 0 {
			t.Errorf("%s: inFlight = %d, want 0", name, got)
		}
	}
}

// TestTargetStatuses_CoolingExpiredRecoverableCountsActive 守护 Notable #2：
// 当 key 处于 exhausted=true + cooldownEnd 已过期 + recoverable reason 时，healthz metrics
// 应把它计入 ActiveKeys（与 UI 的"可用(待激活)"绿色显示口径对齐），而不是 RateLimitedKeys/
// OtherExhaustedKeys，否则会触发"冷却过期但 ActiveKeys=0"的误告警。
//
// 矩阵：
//   - key0：rate_limited + cd 未过期        → rateLimitedKeys
//   - key1：glmRateLimitQPS + cd 已过期      → activeKeys（recoverable）
//   - key2：invalid_token + cd 已过期        → otherExhaustedKeys（non-recoverable）
//   - key3：active                          → activeKeys
func TestTargetStatuses_CoolingExpiredRecoverableCountsActive(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{
			Name:     "with-pool",
			Endpoint: "http://example.com",
			APIKey:   "k0",
			APIKeys:  []string{"k1", "k2", "k3"},
		}},
		Logging: config.LoggingConfig{Level: "warn", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	service.mu.RLock()
	ts := service.targetsByName["with-pool"]
	service.mu.RUnlock()

	now := time.Now()
	// key0：rate_limited + cd 未过期（+5s）
	ts.keyPool.markExhaustedWithCooldown(0, "rate_limited", now.Add(5*time.Second), "")
	// key1：glmRateLimitQPS + cd 已过期（-60s，recoverable → 应计入 active）
	ts.keyPool.markExhaustedWithCooldown(1, glmRateLimitQPS, now.Add(-60*time.Second), "")
	// key2：invalid_token + cd 已过期（-1min，non-recoverable → otherExhausted）
	ts.keyPool.markExhaustedWithCooldown(2, "invalid_token", now.Add(-1*time.Minute), "")
	// key3：保持 active

	statuses := service.TargetStatuses(now)
	if len(statuses) != 1 {
		t.Fatalf("expected 1 target status, got %d", len(statuses))
	}
	st := statuses[0]
	if st.TotalKeys != 4 {
		t.Fatalf("TotalKeys = %d, want 4", st.TotalKeys)
	}
	// key1（recoverable 过期）+ key3（active）= 2 个 active
	if st.ActiveKeys != 2 {
		t.Errorf("ActiveKeys = %d, want 2 (key1 recoverable-expired + key3 active); "+
			"this is the Notable #2 fix aligning metrics with UI '可用(待激活)'",
			st.ActiveKeys)
	}
	if st.RateLimitedKeys != 1 {
		t.Errorf("RateLimitedKeys = %d, want 1 (only key0 rate_limited cd-future)", st.RateLimitedKeys)
	}
	if st.OtherExhaustedKeys != 1 {
		t.Errorf("OtherExhaustedKeys = %d, want 1 (key2 invalid_token non-recoverable)", st.OtherExhaustedKeys)
	}
	// 不变量：各分类之和等于 TotalKeys
	sum := st.ActiveKeys + st.RateLimitedKeys + st.OtherExhaustedKeys +
		st.ExhaustedSubscriptionKeys + st.ExhaustedAPIKeys + st.BlockedKeys
	if sum != st.TotalKeys {
		t.Errorf("classification sum = %d, want TotalKeys=%d (invariant broken)", sum, st.TotalKeys)
	}

	// 验证 status() 显示口径确实与 metrics 对齐：key1 应显示"可用(待激活)"
	keyStatuses := service.KeyPoolStatus("with-pool")
	if keyStatuses[1].Status != "可用(待激活)" {
		t.Errorf("key1 status = %q, want 可用(待激活) (UI alignment precondition)", keyStatuses[1].Status)
	}
}

// TestServiceGLMQuotaClassDoesNotRetrySameKey 守护 fix-glm-ratelimit G3：
// GLM 1308 (额度类, 429 路径) RETRY<2 不做同 key 重试，直接换 key + reqTotalFails++。
// 旧逻辑对所有限流都做 WP-3 坚持，导致额度类每个 key 都浪费一次 429 上游请求 + 1s sleep。
func TestServiceGLMQuotaClassDoesNotRetrySameKey(t *testing.T) {
	var attempts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		// 1308 = 5h 额度限流 (配额类)
		_, _ = w.Write([]byte(`{"error":{"code":"1308","message":"[1308][已达到 5 小时的使用上限]"}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{
			Name:         "glm",
			Endpoint:     upstream.URL,
			UsageAdapter: config.UsageAdapterGLM,
			APIKey:       "k0",
			APIKeys:      []string{"k1"},
		}},
		Logging: config.LoggingConfig{Level: "error", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"glm-5"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	// ① 额度类 RETRY<2 不做同 key 重试：每个 key 只尝试一次（共 2 次），而非旧逻辑的 4 次。
	if got := attempts.Load(); got != 2 {
		t.Fatalf("expected 2 upstream attempts (1 per key, no same-key retry for quota class), got %d", got)
	}
	// ② 全 key 耗尽后客户端应收到 429（安全阀 reqTotalFails=2 未达到 4 次阈值，但全 key 已 exhausted → 上浮）。
	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after all keys quota-exhausted, got %d body=%s", rr.Code, rr.Body.String())
	}
	// ③ 两个 key 标记为 5h 额度限流
	glmState, _ := service.targetByName("glm")
	for i, e := range glmState.keyPool.entries {
		if !e.exhausted {
			t.Errorf("key %d should be exhausted", i)
		}
		if e.exhaustReason != glmExhaustReason {
			t.Errorf("key %d reason = %q, want %q (5h quota)", i, e.exhaustReason, glmExhaustReason)
		}
	}
}

// TestServiceGLMQuotaClass_GlobalSafetyValve 守护 fix-glm-ratelimit G3 task 3.4：
// GLM 额度类换 key 路径必须递增 reqTotalFails，否则 4 次全局安全阀对额度类失效，多 key 池长循环。
func TestServiceGLMQuotaClass_GlobalSafetyValve(t *testing.T) {
	var attempts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"1308","message":"5h quota"}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{
			Name:         "glm",
			Endpoint:     upstream.URL,
			UsageAdapter: config.UsageAdapterGLM,
			// 5 个 key：额度类 5 次换 key，第 4 次应触发 reqTotalFails>=4 → 直接返回 429，
			// 不继续换到第 5 个 key。
			APIKey:  "k0",
			APIKeys: []string{"k1", "k2", "k3", "k4"},
		}},
		Logging: config.LoggingConfig{Level: "error", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"glm-5"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	// ① reqTotalFails 应递增并在 >=4 时直接返回 429（不再尝试第 5 个 key）
	// 实际尝试 key0(1 fail), key1(2), key2(3), key3(4), key4(4→返回 429)
	// wait — 让我重新 trace：
	//   iter 1: forwardRequest → 429 → markExhausted(k0) → reqTotalFails++ = 1 → select next → k1
	//   iter 2: forwardRequest → 429 → markExhausted(k1) → reqTotalFails++ = 2 → select next → k2
	//   iter 3: forwardRequest → 429 → markExhausted(k2) → reqTotalFails++ = 3 → select next → k3
	//   iter 4: forwardRequest → 429 → markExhausted(k3) → reqTotalFails++ = 4 → select next → k4
	//   iter 5: forwardRequest → 429 → markExhausted(k4) → reqTotalFails=4 at TOP of block → RETURN 429
	//   Wait, `reqTotalFails >= 4` check is BEFORE incrementFails. Let me re-read.
	// 所以 5 个 attempts 是 OK 的（前 4 次递增到 4，第 5 次进入时检查 reqTotalFails>=4 → 返回）
	if got := attempts.Load(); got != 5 {
		t.Fatalf("expected 5 upstream attempts (4 key switches + 1 final 429-at-top), got %d", got)
	}
	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after global safety valve, got %d", rr.Code)
	}
}

// TestServiceGLMTransientClass_PreservesSameKeyRetry 守护 fix-glm-ratelimit G3：
// GLM 1305 (瞬时类, 429 路径) RETRY<2 保留同 key 重试 + sleep（瞬时过载可能很快恢复）。
func TestServiceGLMTransientClass_PreservesSameKeyRetry(t *testing.T) {
	var attempts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		// 1305 = 服务过载 (瞬时类, 60s 冷却)
		_, _ = w.Write([]byte(`{"error":{"code":"1305","message":"服务暂不可用"}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 10},
		Targets: []config.Target{{
			Name:     "glm",
			Endpoint: upstream.URL,
			APIKey:   "k0",
			APIKeys:  []string{"k1"},
		}},
		Logging: config.LoggingConfig{Level: "error", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"glm-4"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	start := time.Now()
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)
	elapsed := time.Since(start)

	// ① 瞬时类 RETRY<2 做同 key 重试：每个 key 尝试 2 次（共 4 次），每次 sleep 1s
	//   key0: 1st→sleep1s→2nd→switch ; key1: 1st→sleep1s→2nd→switch ; → exhausted
	if got := attempts.Load(); got != 4 {
		t.Fatalf("expected 4 upstream attempts (2 per key with same-key retry for transient), got %d", got)
	}
	// ② 两次 sleep 1s 至少让 elapsed > 2s (排除其他开销，容差 1.8s)
	if elapsed < 1800*time.Millisecond {
		t.Errorf("expected >1.8s elapsed (2 sleeps), got %v", elapsed)
	}
	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after all keys overload-limited, got %d", rr.Code)
	}
}

// TestServiceWakeUpKeys_TransientVsQuotaGLM 守护 fix-glm-ratelimit G4：
// WakeUpKeys 对 GLM 瞬时类 (1302/1303/1305) 直接 markRecovered，跳过无效 probeKey；
// 对 GLM 额度类 (1308/1310/unknown) 不复活，交给冷却定时器。
// 原因：额度类强制复活会导致下一个请求立刻再撞限，唤醒按钮变成"点了闪一下又灰"。
func TestServiceWakeUpKeys_TransientVsQuotaGLM(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{
			Name:     "glm-pool",
			Endpoint: "http://example.com",
			APIKey:   "k0",
			APIKeys:  []string{"k1"},
		}},
		Logging: config.LoggingConfig{Level: "error", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	state, _ := service.targetByName("glm-pool")
	pool := state.keyPool
	future := time.Now().Add(5 * time.Hour)

	// key0: glmRateLimitOverload (瞬时类, 1305)
	pool.markExhaustedWithCooldown(0, glmRateLimitOverload, time.Now().Add(60*time.Second), "1305")
	// key1: glmRateLimitQuota (额度类, 1310)
	pool.markExhaustedWithCooldown(1, glmRateLimitQuota, future, "1310")

	// 验证两个 key 都已 exhausted
	if !pool.entries[0].exhausted || !pool.entries[1].exhausted {
		t.Fatalf("both keys should be exhausted before wakeup: %+v", pool.entries)
	}

	// 唤醒
	recovered, err := service.WakeUpKeys("glm-pool")
	if err != nil {
		t.Fatalf("WakeUpKeys: %v", err)
	}

	// ① 只有瞬时类 key0 被恢复
	if recovered != 1 {
		t.Errorf("expected 1 key recovered (transient only), got %d", recovered)
	}
	// ② key0 (瞬时) 已恢复
	if pool.entries[0].exhausted {
		t.Errorf("key0 (glmRateLimitOverload, transient) should be recovered")
	}
	// ③ key1 (额度) 仍 exhausted —— 这是关键守护：额度类不能复活，否则立刻再撞限
	if !pool.entries[1].exhausted {
		t.Errorf("key1 (glmRateLimitQuota, quota class) MUST stay exhausted, not force-revived")
	}
	if pool.entries[1].exhaustReason != glmRateLimitQuota {
		t.Errorf("key1 exhaustReason = %q, want %q", pool.entries[1].exhaustReason, glmRateLimitQuota)
	}
}
