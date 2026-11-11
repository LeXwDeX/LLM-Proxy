package config

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

func TestCostPoolMembersWithoutCapacity(t *testing.T) {
	cfg := &Config{
		Pools: []DeploymentPool{
			{Name: "legacy"}, // 空值：生效 cost
			{Name: "explicit", RoutingStrategy: PoolRoutingCost},
			{Name: "perf", RoutingStrategy: PoolRoutingLoad},
			{Name: "healthy"},
			{Name: "empty"},
		},
		Targets: []Target{
			{Name: "a", Pool: "legacy"},
			{Name: "b", Pool: " Legacy ", MaxConcurrency: 0}, // 池名大小写与空白不敏感
			{Name: "c", Pool: "legacy", MaxConcurrency: 4},   // 已配上限不计
			{Name: "d", Pool: "legacy", Paused: true},        // 暂停成员不计
			{Name: "e", Pool: "legacy", MaxConcurrency: -1},  // 非正数视为不限
			{Name: "f", Pool: "explicit"},
			{Name: "g", Pool: "perf"}, // load 池不计
			{Name: "h", Pool: "healthy", MaxConcurrency: 2},
			{Name: "i"},                  // 未入池
			{Name: "j", Pool: "missing"}, // 池不存在
		},
	}
	got := CostPoolMembersWithoutCapacity(cfg)
	want := map[string][]string{
		"legacy":   {"a", "b", "e"},
		"explicit": {"f"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CostPoolMembersWithoutCapacity = %v, want %v", got, want)
	}
}

func TestCostPoolMembersWithoutCapacityNilAndEmpty(t *testing.T) {
	if got := CostPoolMembersWithoutCapacity(nil); len(got) != 0 {
		t.Fatalf("nil config = %v", got)
	}
	if got := CostPoolMembersWithoutCapacity(&Config{}); len(got) != 0 {
		t.Fatalf("empty config = %v", got)
	}
}

func TestLogCostPoolCapacityWarnings(t *testing.T) {
	cfg := &Config{
		Pools: []DeploymentPool{{Name: "legacy"}, {Name: "perf", RoutingStrategy: PoolRoutingLoad}},
		Targets: []Target{
			{Name: "a", Pool: "legacy", Endpoint: "https://secret.example/v1", APIKey: "sk-secret"},
			{Name: "b", Pool: "legacy"},
			{Name: "c", Pool: "perf"},
		},
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	LogCostPoolCapacityWarnings(logger, cfg, nil)
	out := buf.String()
	for _, want := range []string{"level=WARN", "pool=legacy", "count=2", "max_concurrency", "[a b]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "perf") || strings.Contains(out, "secret") {
		t.Fatalf("log leaked a load pool or credentials:\n%s", out)
	}
	if n := strings.Count(out, "level=WARN"); n != 1 {
		t.Fatalf("warn lines = %d, want 1", n)
	}

	// 带 seen 去重：同一内容不重复写，成员变化后再写，恢复正常后清除记录。
	seen := map[string]string{}
	buf.Reset()
	LogCostPoolCapacityWarnings(logger, cfg, seen)
	LogCostPoolCapacityWarnings(logger, cfg, seen)
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Fatalf("deduped warn lines = %d, want 1", n)
	}
	cfg.Targets[1].MaxConcurrency = 3
	buf.Reset()
	LogCostPoolCapacityWarnings(logger, cfg, seen)
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 || !strings.Contains(buf.String(), "count=1") {
		t.Fatalf("changed members must warn again:\n%s", buf.String())
	}
	cfg.Targets[0].MaxConcurrency = 3
	buf.Reset()
	LogCostPoolCapacityWarnings(logger, cfg, seen)
	if buf.Len() != 0 || len(seen) != 0 {
		t.Fatalf("clean config must not warn and must clear seen: %q %v", buf.String(), seen)
	}
	LogCostPoolCapacityWarnings(nil, cfg, nil) // nil logger 不 panic
}
