package proxy

import (
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// newAffinityCapService 构建两个 openai 节点的服务，供容量亲和测试使用。
func newAffinityCapService(t *testing.T, maxConcA, maxConcB int) *Service {
	t.Helper()
	targets := []config.Target{
		{Name: "cap-a", EndpointType: "openai", Endpoint: "http://127.0.0.1:1", APIKey: "k1"},
		{Name: "cap-b", EndpointType: "openai", Endpoint: "http://127.0.0.1:2", APIKey: "k2"},
	}
	if maxConcA > 0 {
		targets[0].MaxConcurrency = maxConcA
	}
	if maxConcB > 0 {
		targets[1].MaxConcurrency = maxConcB
	}
	cfg := &config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: targets,
		Logging: config.LoggingConfig{Level: "error", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// affRoute 以固定的 clientIP + principal 走一次自动选路。
func affRoute(t *testing.T, svc *Service, clientIP string) (*targetState, selectionKind) {
	t.Helper()
	p := &auth.Principal{Name: "tester"}
	st, _, kind, err := svc.selectTarget(p, clientIP, "", "", nil, nil, "", "/v1/chat/completions", time.Now())
	if err != nil {
		t.Fatalf("selectTarget: %v", err)
	}
	return st, kind
}

// affRecord 写入一条粘连记录（模拟 service 层转发成功后的 Set）。
func affRecord(svc *Service, clientIP, model, targetName string) {
	svc.affinity.Set(affinityKey(clientIP, "tester", model), targetName, time.Now())
}

// affLookup 直查粘连记录当前指向（校验"不被劫持"）。
func affLookup(svc *Service, clientIP, model string) (string, bool) {
	return svc.affinity.Get(affinityKey(clientIP, "tester", model), time.Now())
}

// TestAffinityStableRoutingSameTarget 验收①：亲和命中后同客户端+同模型稳定路由到同一节点，
// kind 为 selectionAffinityHit（TTL 内反复刷新而非重新轮询）。
func TestAffinityStableRoutingSameTarget(t *testing.T) {
	svc := newAffinityCapService(t, 10, 10)

	first, _ := affRoute(t, svc, "10.1.1.7")
	firstName := first.Target().Name
	affRecord(svc, "10.1.1.7", "", firstName) // 模拟转发成功后的粘连建立

	for i := 0; i < 5; i++ {
		st, kind := affRoute(t, svc, "10.1.1.7")
		if got := st.Target().Name; got != firstName {
			t.Fatalf("route #%d drifted from affinity target %q to %q", i, firstName, got)
		}
		if kind != selectionAffinityHit {
			t.Fatalf("route #%d expected selectionAffinityHit, got %d", i, kind)
		}
	}

	// 另一客户端独立建粘：不同 IP 不互相干扰。
	other, otherKind := affRoute(t, svc, "10.9.9.9")
	_ = otherKind
	affRecord(svc, "10.9.9.9", "", other.Target().Name)
	st, _ := affRoute(t, svc, "10.9.9.9")
	if got := st.Target().Name; got != other.Target().Name {
		t.Fatalf("second client affinity drifted: expected %q got %q", other.Target().Name, got)
	}
}

// TestAffinitySoftCapSpilloverAndReturn 验收②③：
// 亲和节点归一化负载达软上限(0.8)时分流到其他健康节点且不改写原粘连记录（不劫持）；
// 负载回落后流量自动回粘原节点。
func TestAffinitySoftCapSpilloverAndReturn(t *testing.T) {
	svc := newAffinityCapService(t, 10, 10)
	a := capState(t, svc, "cap-a")
	b := capState(t, svc, "cap-b")

	affRecord(svc, "10.2.2.3", "", "cap-a")

	// cap-a 填到 load=8/10=0.8：不低于软上限 ⇒ 不再回粘。
	fill(t, a, 8)
	st, kind := affRoute(t, svc, "10.2.2.3")
	if got := st.Target().Name; got != "cap-b" {
		t.Fatalf("expected spillover to cap-b at soft-cap saturation, got %q (load %.2f)", got, a.normalizedLoad())
	}
	if kind != selectionFailover {
		t.Fatalf("spillover must behave as failover (kind=%d) so the record is not hijacked", kind)
	}
	// 原记录保留：分流不能劫持粘连。
	if name, ok := affLookup(svc, "10.2.2.3", ""); !ok || name != "cap-a" {
		t.Fatalf("affinity record was hijacked during spillover: got %q ok=%v", name, ok)
	}

	// 负载回落到 6/10=0.6 < 0.8 ⇒ 流量自动回粘。
	a.releaseSlot()
	a.releaseSlot()
	st, kind = affRoute(t, svc, "10.2.2.3")
	if got := st.Target().Name; got != "cap-a" {
		t.Fatalf("expected sticky return to cap-a after load dropped, got %q (load %.2f)", got, a.normalizedLoad())
	}
	if kind != selectionAffinityHit {
		t.Fatalf("expected selectionAffinityHit on return, got %d", kind)
	}
	if b.inFlightCount() != 0 {
		t.Fatalf("cap-b must stay untouched in this scenario, inFlight=%d", b.inFlightCount())
	}
}

// TestAffinitySoftCapSkipsUncappedTarget 验收④防回归：未配置容量的节点仅计数不限流，
// 其亲和回粘不受软上限约束（保持 S3 之前的粘连行为不变）。
func TestAffinitySoftCapSkipsUncappedTarget(t *testing.T) {
	svc := newAffinityCapService(t, 0, 0) // 两节点都不设容量
	a := capState(t, svc, "cap-a")

	affRecord(svc, "10.3.3.4", "", "cap-a")
	fill(t, a, 5) // 归一化负载 5/1=5 > 0.8，但无容量节点不应被软上限拦截

	st, kind := affRoute(t, svc, "10.3.3.4")
	if got := st.Target().Name; got != "cap-a" {
		t.Fatalf("uncapped target must keep affinity despite high raw count, got %q", got)
	}
	if kind != selectionAffinityHit {
		t.Fatalf("expected selectionAffinityHit for uncapped target, got %d", kind)
	}
}

// TestAffinityFailoverOnMutedKeepsSticky 验收⑤：亲和节点故障（muted）时 failover 到他节点
// 且原记录不被劫持；恢复后流量回来 —— 与 #15 前语义一致（容量门不改变守护语义）。
func TestAffinityFailoverOnMutedKeepsSticky(t *testing.T) {
	svc := newAffinityCapService(t, 10, 10)
	a := capState(t, svc, "cap-a")

	affRecord(svc, "10.4.4.5", "", "cap-a")
	fill(t, a, 9)      // 同时压满负载
	now := time.Now() // 使 cap-a muted
	a.MarkFailure(now, 60*time.Second)

	st, _ := affRoute(t, svc, "10.4.4.5")
	if got := st.Target().Name; got != "cap-b" {
		t.Fatalf("expected failover to cap-b while cap-a muted+saturated, got %q", got)
	}
	if name, ok := affLookup(svc, "10.4.4.5", ""); !ok || name != "cap-a" {
		t.Fatalf("affinity record hijacked during muted failover: %q ok=%v", name, ok)
	}

	a.releaseSlot()
	a.releaseSlot()
	a.MarkSuccess(time.Now()) // 解除 muted 并清零失败计数
	st, kind := affRoute(t, svc, "10.4.4.5")
	if got := st.Target().Name; got != "cap-a" {
		t.Fatalf("expected return to cap-a after recovery, got %q", got)
	}
	if kind != selectionAffinityHit {
		t.Fatalf("expected selectionAffinityHit after recovery, got %d", kind)
	}
}
