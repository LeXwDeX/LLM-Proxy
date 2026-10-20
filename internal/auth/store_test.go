package auth

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestStoreLoadFromConfigAndAuthenticate(t *testing.T) {
	store := NewStore()
	clients := []config.Client{{
		Name:           "team-alpha",
		AccessKey:      "token123",
		AllowedTargets: []string{"primary"},
	}}

	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("expected load to succeed: %v", err)
	}

	principal, ok := store.Authenticate("token123")
	if !ok {
		t.Fatal("expected token to authenticate")
	}
	if principal.Name != "team-alpha" {
		t.Fatalf("unexpected principal name: %s", principal.Name)
	}
	if principal.AllowAll() {
		t.Fatal("expected principal to have restricted targets")
	}
	if !principal.CanAccess("primary") {
		t.Fatal("principal should access primary target")
	}
	if principal.CanAccess("secondary") {
		t.Fatal("principal should not access secondary target")
	}
}

func TestStoreLoadAllowAllClients(t *testing.T) {
	store := NewStore()
	clients := []config.Client{{
		Name:      "ops",
		AccessKey: "ops-token",
		// empty allowed_targets => allow all
	}}

	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("expected load to succeed: %v", err)
	}

	principal, ok := store.Authenticate("ops-token")
	if !ok {
		t.Fatal("expected ops token to authenticate")
	}
	if !principal.AllowAll() {
		t.Fatal("expected empty allowed targets to grant full access")
	}
}

func TestAuthenticateReturnsSharedImmutablePrincipal(t *testing.T) {
	store := NewStore()
	if err := store.LoadFromConfig([]config.Client{{
		Name:           "team",
		AccessKey:      "abc",
		AllowedTargets: []string{"primary"},
	}}); err != nil {
		t.Fatalf("load failed: %v", err)
	}

	principal, ok := store.Authenticate("abc")
	if !ok {
		t.Fatal("expected principal")
	}

	second, ok := store.Authenticate("abc")
	if !ok {
		t.Fatal("expected second principal")
	}

	// 新契约：Principal 构建后不可变，Authenticate 直接返回共享指针
	//（免除每请求深拷贝 allowedTargets map 的开销）。
	if principal != second {
		t.Fatal("expected authenticate to return the shared immutable principal")
	}

	// 热加载为整表替换：旧 principal 不受影响，新 Authenticate 返回新对象。
	if err := store.LoadFromConfig([]config.Client{{
		Name:           "team",
		AccessKey:      "abc",
		AllowedTargets: []string{"secondary"},
	}}); err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	reloaded, ok := store.Authenticate("abc")
	if !ok {
		t.Fatal("expected reloaded principal")
	}
	if reloaded == principal {
		t.Fatal("expected reload to publish a new principal instance")
	}
	if !principal.CanAccess("primary") || principal.CanAccess("secondary") {
		t.Fatal("old principal must stay unchanged after reload")
	}
}

func TestStoreLoadFromConfigRejectsDuplicateAccessKey(t *testing.T) {
	store := NewStore()
	err := store.LoadFromConfig([]config.Client{
		{Name: "team-a", AccessKey: "same-key"},
		{Name: "team-b", AccessKey: "same-key"},
	})
	if err == nil {
		t.Fatal("expected duplicate access_key validation error")
	}
}

func TestLoadFromConfigWithPoolsExpandsMembers(t *testing.T) {
	store := NewStore()
	targets := []config.Target{
		{Name: "member-a", Pool: "pool-1"},
		{Name: "member-b", Pool: "pool-1"},
		{Name: "outsider"},
	}
	clients := []config.Client{{
		Name:           "pooled",
		AccessKey:      "pool-token",
		AllowedTargets: []string{"outsider"},
		AllowedPools:   []string{"pool-1"},
	}}

	if err := store.LoadFromConfigWithPools(clients, targets); err != nil {
		t.Fatalf("load with pools: %v", err)
	}

	principal, ok := store.Authenticate("pool-token")
	if !ok {
		t.Fatal("expected pool-token to authenticate")
	}
	if principal.AllowAll() {
		t.Fatal("expected restricted principal")
	}
	for _, member := range []string{"member-a", "member-b", "outsider"} {
		if !principal.CanAccess(member) {
			t.Fatalf("principal should access %s (targets ∪ pool members)", member)
		}
	}
	if principal.CanAccess("member-c") {
		t.Fatal("principal must not access non-granted target")
	}
}

func TestPoolOnlyClientIsNotAllowAll(t *testing.T) {
	// 提权守护：仅配置 allowed_pools 的客户端绝不能落入 allowAll 全放行分支。
	store := NewStore()
	targets := []config.Target{
		{Name: "member-a", Pool: "pool-1"},
		{Name: "outsider"},
	}
	clients := []config.Client{{
		Name:         "pool-only",
		AccessKey:    "pool-only-token",
		AllowedPools: []string{"pool-1"},
	}}

	if err := store.LoadFromConfigWithPools(clients, targets); err != nil {
		t.Fatalf("load with pools: %v", err)
	}

	principal, ok := store.Authenticate("pool-only-token")
	if !ok {
		t.Fatal("expected principal")
	}
	if principal.AllowAll() {
		t.Fatal("pool-only client must NOT be allowAll")
	}
	if !principal.CanAccess("member-a") {
		t.Fatal("pool-only client should access pool member")
	}
	if principal.CanAccess("outsider") {
		t.Fatal("pool-only client must not access non-member target")
	}
}

func TestUnknownPoolGrantsNothing(t *testing.T) {
	store := NewStore()
	targets := []config.Target{{Name: "member-a", Pool: "pool-1"}}
	clients := []config.Client{{
		Name:         "ghost",
		AccessKey:    "ghost-token",
		AllowedPools: []string{"no-such-pool"},
	}}

	if err := store.LoadFromConfigWithPools(clients, targets); err != nil {
		t.Fatalf("unknown pool must not error: %v", err)
	}

	principal, ok := store.Authenticate("ghost-token")
	if !ok {
		t.Fatal("expected principal")
	}
	if principal.AllowAll() {
		t.Fatal("client referencing unknown pool must not become allowAll")
	}
	if principal.CanAccess("member-a") {
		t.Fatal("unknown pool must grant nothing")
	}
}

func TestBlankAllowedTargetsEdgeUnchanged(t *testing.T) {
	// [""] 边缘语义：原始字段非空 → allowAll=false；空串成员不授予任何目标。
	store := NewStore()
	targets := []config.Target{{Name: "member-a", Pool: "pool-1"}}
	clients := []config.Client{
		{Name: "blank-target", AccessKey: "bt-token", AllowedTargets: []string{""}},
		{Name: "blank-pool", AccessKey: "bp-token", AllowedPools: []string{""}},
	}

	if err := store.LoadFromConfigWithPools(clients, targets); err != nil {
		t.Fatalf("load: %v", err)
	}

	for _, token := range []string{"bt-token", "bp-token"} {
		principal, ok := store.Authenticate(token)
		if !ok {
			t.Fatalf("expected principal for %s", token)
		}
		if principal.AllowAll() {
			t.Fatalf("%s: [\"\"] must not trigger allowAll", token)
		}
		if principal.CanAccess("member-a") {
			t.Fatalf("%s: [\"\"] must not grant access to any target", token)
		}
	}
}

func TestLoadFromConfigLegacySignatureIgnoresPools(t *testing.T) {
	// 旧签名（无 targets 视图）等价于 WithPools(clients, nil)：池展开为空集。
	store := NewStore()
	clients := []config.Client{{
		Name:         "legacy",
		AccessKey:    "legacy-token",
		AllowedPools: []string{"pool-1"},
	}}
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("legacy load: %v", err)
	}
	principal, ok := store.Authenticate("legacy-token")
	if !ok {
		t.Fatal("expected principal")
	}
	if principal.AllowAll() {
		t.Fatal("legacy load must still honour raw-field allowAll semantics")
	}
	if principal.CanAccess("member-a") {
		t.Fatal("legacy load has no pool member index; pools grant nothing")
	}
}
