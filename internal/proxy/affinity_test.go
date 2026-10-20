package proxy

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestAffinityMap(t *testing.T) {
	am := newAffinityMap()
	now := time.Now()

	// 空查询
	_, ok := am.Get("client1:gpt-4o", now)
	if ok {
		t.Fatal("expected miss on empty map")
	}

	// 写入+命中
	am.Set("client1:gpt-4o", "target-a", now)
	name, ok := am.Get("client1:gpt-4o", now)
	if !ok || name != "target-a" {
		t.Fatalf("expected hit target-a, got %q ok=%v", name, ok)
	}

	// 过期
	_, ok = am.Get("client1:gpt-4o", now.Add(affinityTTL+time.Second))
	if ok {
		t.Fatal("expected miss after TTL")
	}

	// 更新
	am.Set("client1:gpt-4o", "target-b", now.Add(time.Minute))
	name, ok = am.Get("client1:gpt-4o", now.Add(time.Minute))
	if !ok || name != "target-b" {
		t.Fatalf("expected hit target-b after update, got %q ok=%v", name, ok)
	}

	// 不同 key 隔离
	_, ok = am.Get("client2:gpt-4o", now)
	if ok {
		t.Fatal("expected miss for different client")
	}
}

// TestAffinityMapEvictsAtCapacity 验证达到容量上限时淘汰陈旧条目。
func TestAffinityMapEvictsAtCapacity(t *testing.T) {
	am := newAffinityMap()
	// 降低容量上限以便测试能真正触发 evictLocked（而非靠 Get 的惰性过期）
	const testCap = 5
	am.maxEntries = testCap
	now := time.Now()

	// 填入 testCap 条（到达上限）
	for i := 0; i < testCap; i++ {
		key := fmt.Sprintf("client%d:model", i)
		am.Set(key, "target-a", now)
	}
	if len(am.entries) != testCap {
		t.Fatalf("expected %d entries, got %d", testCap, len(am.entries))
	}

	// 把前 2 条标记为过期（时间早于 TTL），这样 evictLocked 第一轮会清理它们
	oldTime := now.Add(-(affinityTTL + time.Minute))
	am.mu.Lock()
	am.entries["client0:model"] = affinityEntry{targetName: "target-a", lastUsed: oldTime}
	am.entries["client1:model"] = affinityEntry{targetName: "target-a", lastUsed: oldTime}
	am.mu.Unlock()

	// 第 6 条新 key 应触发 evictLocked：先清理过期条目腾出空间，再插入
	am.Set("client5:model", "target-b", now)

	// 过期条目应被 evictLocked 淘汰（不是靠 Get 惰性删除）
	// 注意：这里用直接读 entries 而非 Get，以区分 evictLocked 和惰性删除
	am.mu.RLock()
	_, hasClient0 := am.entries["client0:model"]
	_, hasClient1 := am.entries["client1:model"]
	_, hasClient5 := am.entries["client5:model"]
	am.mu.RUnlock()

	if hasClient0 {
		t.Error("expected expired entry client0 to be evicted by evictLocked")
	}
	if hasClient1 {
		t.Error("expected expired entry client1 to be evicted by evictLocked")
	}
	if !hasClient5 {
		t.Error("expected new entry client5 to exist after eviction")
	}
}

// TestAffinityMapEvictsOldestWhenAllFresh 验证所有条目都未过期时，
// evictLocked 第二轮淘汰最旧的一条。
func TestAffinityMapEvictsOldestWhenAllFresh(t *testing.T) {
	am := newAffinityMap()
	am.maxEntries = 3
	base := time.Now()

	// 插入 3 条，时间递增（client0 最旧）
	am.Set("client0:model", "t0", base)
	am.Set("client1:model", "t1", base.Add(time.Second))
	am.Set("client2:model", "t2", base.Add(2*time.Second))

	// 第 4 条触发淘汰：全部未过期，应淘汰最旧的 client0
	am.Set("client3:model", "t3", base.Add(3*time.Second))

	am.mu.RLock()
	_, hasClient0 := am.entries["client0:model"]
	_, hasClient3 := am.entries["client3:model"]
	count := len(am.entries)
	am.mu.RUnlock()

	if hasClient0 {
		t.Error("expected oldest entry client0 to be evicted")
	}
	if !hasClient3 {
		t.Error("expected new entry client3 to exist")
	}
	if count != am.maxEntries {
		t.Errorf("expected %d entries after eviction, got %d", am.maxEntries, count)
	}
}

// TestAffinityMapEvictsToLowWatermark 验证所有条目都新鲜时，evictLocked 会
// 批量驱逐到 90% 低水位，而不是只驱逐一条。这是 O(n²) → O(n) 优化的行为契约：
// 用 maxEntries=20（低水位 18），一次驱逐应删 2 条，而非逐条删。
func TestAffinityMapEvictsToLowWatermark(t *testing.T) {
	am := newAffinityMap()
	const cap = 20
	am.maxEntries = cap
	base := time.Now()

	// 插入 cap 条，时间递增（client0 最旧）
	for i := 0; i < cap; i++ {
		am.Set(fmt.Sprintf("client%d:model", i), "t", base.Add(time.Duration(i)*time.Second))
	}

	// 第 21 条触发驱逐：全部新鲜，应批量降到低水位 cap*9/10 = 18，驱逐 2 条
	am.Set("client-new:model", "t", base.Add(time.Duration(cap)*time.Second))

	am.mu.RLock()
	count := len(am.entries)
	_, hasClient0 := am.entries["client0:model"]  // 最旧，应被驱逐
	_, hasClient1 := am.entries["client1:model"]  // 次旧，也应被驱逐
	_, hasClient2 := am.entries["client2:model"]  // 第三旧，应保留
	_, hasClientNew := am.entries["client-new:model"] // 新插入，应在
	am.mu.RUnlock()

	// 驱逐 2 条腾出空间，再插入 1 条：20 - 2 + 1 = 19
	if count != cap-1 {
		t.Errorf("expected count %d after batch-evict+insert, got %d", cap-1, count)
	}
	if hasClient0 {
		t.Error("expected oldest client0 to be evicted")
	}
	if hasClient1 {
		t.Error("expected second-oldest client1 to be evicted")
	}
	if !hasClient2 {
		t.Error("expected third-oldest client2 to be retained")
	}
	if !hasClientNew {
		t.Error("expected newly inserted client-new to exist")
	}
}

// TestAffinityMapUpdateDoesNotTriggerEviction 验证更新已有条目不触发淘汰。
func TestAffinityMapUpdateDoesNotTriggerEviction(t *testing.T) {
	am := newAffinityMap()
	now := time.Now()

	// 填入 3 条
	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("client%d:model", i)
		am.Set(key, "target-a", now)
	}

	// 更新已有条目不应丢失数据
	am.Set("client0:model", "target-b", now)
	name, ok := am.Get("client0:model", now)
	if !ok || name != "target-b" {
		t.Fatalf("expected updated entry target-b, got %q ok=%v", name, ok)
	}
}

// TestAffinityFailoverDoesNotHijack 验证 failover 场景不劫持粘连：
// 当粘连目标暂时不可用（muted），请求 failover 到其他目标后，
// 粘连记录仍指向原目标；原目标恢复后流量自动回来。
func TestAffinityFailoverDoesNotHijack(t *testing.T) {
	// 两个 mock 上游，都返回 200
	targetA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mock":"a"}`))
	}))
	defer targetA.Close()

	targetB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mock":"b"}`))
	}))
	defer targetB.Close()

	aURL, _ := url.Parse(targetA.URL)
	bURL, _ := url.Parse(targetB.URL)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "target-a",
				Endpoint:           targetA.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key-a",
				ModelMappings: []config.ModelMapping{{Upstream: "qwen3.7-max"}},
			},
			{
				Name:               "target-b",
				Endpoint:           targetB.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key-b",
				ModelMappings: []config.ModelMapping{{Upstream: "qwen3.7-max"}},
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
	service.quietPeriod = 50 * time.Millisecond

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected authenticated principal")
	}

	sendRequest := func() string {
		body := bytes.NewBufferString(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)
		res := rr.Result()
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", res.StatusCode)
		}
		return res.Header.Get("X-Proxy-Target")
	}

	// 步骤 1：手动建立粘连到 target-a
	service.affinity.Set(affinityKey("192.0.2.1", "tester", "qwen3.7-max"), "target-a", time.Now())

	// 步骤 2：验证粘连命中 target-a
	got := sendRequest()
	if got != "target-a" {
		t.Fatalf("step 2: expected affinity hit target-a, got %q", got)
	}

	// 步骤 3：mute target-a，模拟一次失败
	now := time.Now()
	stateA, exists := service.targetByName("target-a")
	if !exists {
		t.Fatal("target-a not found")
	}
	stateA.MarkFailure(now, 200*time.Millisecond)

	// 步骤 4：发送请求 → failover 到 target-b
	got = sendRequest()
	if got != "target-b" {
		t.Fatalf("step 4: expected failover to target-b, got %q", got)
	}

	// 步骤 5：验证粘连仍然指向 target-a（未被劫持到 target-b）
	aKey := affinityKey("192.0.2.1", "tester", "qwen3.7-max")
	affinityTarget, affOK := service.affinity.Get(aKey, time.Now())
	if !affOK {
		t.Fatal("step 5: expected affinity record to still exist")
	}
	if affinityTarget != "target-a" {
		t.Fatalf("step 5: expected affinity still pointing to target-a, got %q", affinityTarget)
	}

	// 步骤 6：等待 mute 过期，target-a 恢复
	time.Sleep(250 * time.Millisecond)

	// 步骤 7：发送请求 → 粘连命中 target-a，流量回来
	got = sendRequest()
	if got != "target-a" {
		t.Fatalf("step 7: expected affinity hit target-a after recovery, got %q", got)
	}

	_ = aURL
	_ = bURL
}

// TestAffinityKeyUsesUpstreamModelWhenAliasRequested 验证粘连 key 使用上游模型名
// 而非客户端 alias：当目标配置了 ModelMappings fallback（alias→upstream），
// 客户端用 alias 请求时，set/get 都应使用上游模型名作为 key，让不同 alias 但指向
// 同一上游的请求命中同一条粘连记录。
func TestAffinityKeyUsesUpstreamModelWhenAliasRequested(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mock":"ok"}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{
			Name:          "t1",
			Endpoint:      srv.URL,
			APIKey:        "key",
			ModelMappings: []config.ModelMapping{{Upstream: "Real-Model-Name", Fallback: "shortcut"}},
		}},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
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

	// 客户端用 alias "shortcut" 请求
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"shortcut","messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	now := time.Now()
	// 断言 1：粘连 key 应该使用上游名（保留原始大小写）。
	upKey := affinityKey("192.0.2.1", "tester", "Real-Model-Name")
	_, upOK := service.affinity.Get(upKey, now)
	if !upOK {
		t.Error("expected affinity entry under upstream model name key, but not found")
	}

	// 断言 2：粘连 key 不应该在 alias 名下（避免 Set 用 upstream、Get 用 alias 导致的 key 不匹配）。
	aliasKey := affinityKey("192.0.2.1", "tester", "shortcut")
	_, aliasOK := service.affinity.Get(aliasKey, now)
	if aliasOK {
		t.Error("affinity entry unexpectedly stored under alias key — Set should use upstream model name")
	}
}
