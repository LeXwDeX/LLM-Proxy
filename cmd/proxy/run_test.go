package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/logging"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
)

// A serve failure (the listen address is busy) exits with status 1 through
// run's return, so every deferred cleanup still runs in the existing order:
// the quota manager stops, the database closes after the persistence drain,
// and the log manager flushes and closes last.
func TestRunServeErrorRunsDeferredCleanup(t *testing.T) {
	dir := t.TempDir()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cfg := config.DefaultConfig()
	cfg.DataStore.DBPath = filepath.Join(dir, "proxy.db")
	cfg.Logging.AccessLog = filepath.Join(dir, "access.log")
	cfg.Logging.ErrorLog = filepath.Join(dir, "error.log")
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SERVER_BIND", busy.Addr().String())
	t.Setenv("UPSTREAM_ERROR_LOG_PATH", filepath.Join(dir, "upstream-error.log"))

	var order []string
	prevQuota, prevLogs := stopQuotaManager, closeLogManager
	stopQuotaManager = func(m *quota.Manager) { order = append(order, "quota"); prevQuota(m) }
	closeLogManager = func(m *logging.Manager) error { order = append(order, "logs"); return prevLogs(m) }
	t.Cleanup(func() { stopQuotaManager, closeLogManager = prevQuota, prevLogs })

	if code := run([]string{"-config", configPath}); code != 1 {
		t.Fatalf("exit code=%d want 1", code)
	}
	if len(order) != 2 || order[0] != "quota" || order[1] != "logs" {
		t.Fatalf("deferred cleanup order=%v want [quota logs]", order)
	}
	logged, err := os.ReadFile(cfg.Logging.ErrorLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "http server stopped unexpectedly") {
		t.Fatalf("serve error not flushed to the log: %s", logged)
	}
	db, err := nosql.OpenDB(cfg.DataStore.DBPath)
	if err != nil {
		t.Fatalf("database still open after exit path: %v", err)
	}
	db.Close()
}

// -h and -help print usage and exit 0; other flag errors exit 2. Both return
// before any config is loaded or created.
func TestRunFlagExitCodes(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"-h"}, 0},
		{[]string{"-help"}, 0},
		{[]string{"--help"}, 0},
		{[]string{"-unknown"}, 2},
		{[]string{"-config"}, 2},
	} {
		if code := run(tc.args); code != tc.want {
			t.Fatalf("run(%q) = %d, want %d", tc.args, code, tc.want)
		}
	}
}

// 升级后空值策略池按成本优先调度：启动日志对启用中且未设 max_concurrency 的成员写一条 Warn，
// 已配上限、暂停与 load 池的成员不在其内，日志不含端点与密钥。
func TestRunLogsCostPoolMembersWithoutCapacity(t *testing.T) {
	dir := t.TempDir()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cfg := config.DefaultConfig()
	cfg.DataStore.DBPath = filepath.Join(dir, "proxy.db")
	cfg.Logging.AccessLog = filepath.Join(dir, "access.log")
	cfg.Logging.ErrorLog = filepath.Join(dir, "error.log")
	cfg.Pools = []config.DeploymentPool{
		{Name: "legacy"},
		{Name: "perf", RoutingStrategy: config.PoolRoutingLoad},
	}
	mk := func(name, pool string, max int, paused bool) config.Target {
		return config.Target{
			Name: name, EndpointType: "openai", Endpoint: "https://upstream.invalid/v1",
			APIKey: "sk-test-secret", UsageAdapter: "standard", Pool: pool, MaxConcurrency: max, Paused: paused,
		}
	}
	cfg.Targets = []config.Target{
		mk("open-a", "legacy", 0, false),
		mk("limited", "legacy", 5, false),
		mk("paused", "legacy", 0, true),
		mk("perf-a", "perf", 0, false),
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SERVER_BIND", busy.Addr().String())
	t.Setenv("UPSTREAM_ERROR_LOG_PATH", filepath.Join(dir, "upstream-error.log"))

	if code := run([]string{"-config", configPath}); code != 1 {
		t.Fatalf("exit code=%d want 1", code)
	}
	logged, err := os.ReadFile(cfg.Logging.ErrorLog)
	if err != nil {
		t.Fatal(err)
	}
	text := string(logged)
	if n := strings.Count(text, "level=WARN msg=\"成本优先部署池"); n != 1 {
		t.Fatalf("capacity warnings = %d, want 1:\n%s", n, text)
	}
	for _, want := range []string{"pool=legacy", "open-a", "count=1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("warning missing %q:\n%s", want, text)
		}
	}
	for _, bad := range []string{"limited", "paused", "perf-a", "sk-test-secret", "upstream.invalid"} {
		if strings.Contains(text, bad) {
			t.Fatalf("warning must not mention %q:\n%s", bad, text)
		}
	}
}
