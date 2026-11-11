package admin

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type poolCapacityEntry struct {
	Name                     string   `json:"name"`
	RoutingStrategy          string   `json:"routing_strategy"`
	EffectiveRoutingStrategy string   `json:"effective_routing_strategy"`
	MemberCapacityAdvised    bool     `json:"member_capacity_advised"`
	MembersWithoutCapacity   []string `json:"members_without_capacity"`
}

func listPoolCapacity(t *testing.T, h http.Handler) map[string]poolCapacityEntry {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/pools", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list pools: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Pools []poolCapacityEntry `json:"pools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode pools: %v", err)
	}
	out := map[string]poolCapacityEntry{}
	for _, p := range resp.Pools {
		out[p.Name] = p
	}
	return out
}

func TestPoolListReportsMembersWithoutCapacity(t *testing.T) {
	h, _, _, _ := newPoolTestHandler(t, t.TempDir())
	for _, body := range []map[string]any{
		{"name": "legacy"},
		{"name": "perf", "routing_strategy": "load"},
		{"name": "empty"},
	} {
		if rec := postPool(t, h, body); rec.Code != http.StatusCreated {
			t.Fatalf("create pool %v: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if rec := putTarget(t, h, "target-1", poolTargetUpdateBody("legacy")); rec.Code != http.StatusOK {
		t.Fatalf("assign: %d %s", rec.Code, rec.Body.String())
	}

	pools := listPoolCapacity(t, h)
	legacy := pools["legacy"]
	if legacy.EffectiveRoutingStrategy != "cost" || !legacy.MemberCapacityAdvised ||
		len(legacy.MembersWithoutCapacity) != 1 || legacy.MembersWithoutCapacity[0] != "target-1" {
		t.Fatalf("legacy pool = %+v", legacy)
	}
	if legacy.RoutingStrategy != "" {
		t.Fatalf("stored routing_strategy must stay empty, got %q", legacy.RoutingStrategy)
	}
	for _, name := range []string{"perf", "empty"} {
		p := pools[name]
		if p.MembersWithoutCapacity == nil || len(p.MembersWithoutCapacity) != 0 {
			t.Fatalf("pool %s members_without_capacity must be an empty list, got %#v", name, p.MembersWithoutCapacity)
		}
	}
	if p := pools["perf"]; p.EffectiveRoutingStrategy != "load" || p.MemberCapacityAdvised {
		t.Fatalf("perf pool = %+v", p)
	}
	if p := pools["empty"]; p.EffectiveRoutingStrategy != "cost" || !p.MemberCapacityAdvised {
		t.Fatalf("empty cost pool = %+v", p)
	}

	// 配置了并发上限后不再计入。
	body := poolTargetUpdateBody("legacy")
	body["max_concurrency"] = 4
	if rec := putTarget(t, h, "target-1", body); rec.Code != http.StatusOK {
		t.Fatalf("set capacity: %d %s", rec.Code, rec.Body.String())
	}
	if got := listPoolCapacity(t, h)["legacy"].MembersWithoutCapacity; len(got) != 0 {
		t.Fatalf("configured member still listed: %v", got)
	}
}

func TestConfigCommitLogsCostPoolMembersWithoutCapacityOncePerChange(t *testing.T) {
	h, _, _, _ := newPoolTestHandler(t, t.TempDir())
	var buf bytes.Buffer
	h.(*Handler).logger = slog.New(slog.NewTextHandler(&buf, nil))
	warns := func() int { return strings.Count(buf.String(), "level=WARN msg=\"成本优先部署池") }

	if rec := postPool(t, h, map[string]any{"name": "legacy"}); rec.Code != http.StatusCreated {
		t.Fatalf("create pool: %d", rec.Code)
	}
	if warns() != 0 {
		t.Fatalf("empty pool must not warn:\n%s", buf.String())
	}
	if rec := putTarget(t, h, "target-1", poolTargetUpdateBody("legacy")); rec.Code != http.StatusOK {
		t.Fatalf("assign: %d %s", rec.Code, rec.Body.String())
	}
	if warns() != 1 {
		t.Fatalf("assigning an unlimited member must warn once:\n%s", buf.String())
	}
	for _, want := range []string{"pool=legacy", "members=[target-1]", "count=1"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("warning missing %q:\n%s", want, buf.String())
		}
	}
	if strings.Contains(buf.String(), "sk-") || strings.Contains(buf.String(), "example.com") {
		t.Fatalf("warning leaked credentials or addresses:\n%s", buf.String())
	}

	// 内容未变的后续提交不重复写。
	if rec := putPool(t, h, "legacy", map[string]any{"description": "x"}); rec.Code != http.StatusOK {
		t.Fatalf("update pool: %d", rec.Code)
	}
	if warns() != 1 {
		t.Fatalf("unchanged state must not warn again:\n%s", buf.String())
	}

	// 配置上限后不再提示，之后恢复为不限会再次提示。
	body := poolTargetUpdateBody("legacy")
	body["max_concurrency"] = 4
	if rec := putTarget(t, h, "target-1", body); rec.Code != http.StatusOK {
		t.Fatalf("set capacity: %d", rec.Code)
	}
	if warns() != 1 {
		t.Fatalf("configured member must not warn:\n%s", buf.String())
	}
	body["max_concurrency"] = 0 // 管理端留空保存即发送 0
	if rec := putTarget(t, h, "target-1", body); rec.Code != http.StatusOK {
		t.Fatalf("clear capacity: %d", rec.Code)
	}
	if warns() != 2 {
		t.Fatalf("regression to unlimited must warn again:\n%s", buf.String())
	}
}

func TestConfigReloadLogsCostPoolMembersWithoutCapacity(t *testing.T) {
	h, _, _, _ := newPoolTestHandler(t, t.TempDir())
	var buf bytes.Buffer
	h.(*Handler).logger = slog.New(slog.NewTextHandler(&buf, nil))
	if rec := postPool(t, h, map[string]any{"name": "legacy"}); rec.Code != http.StatusCreated {
		t.Fatalf("create pool: %d", rec.Code)
	}
	if rec := putTarget(t, h, "target-1", poolTargetUpdateBody("legacy")); rec.Code != http.StatusOK {
		t.Fatalf("assign: %d", rec.Code)
	}
	// 清掉去重记录，模拟新进程；重载后应重新提示。
	h.(*Handler).capacityWarnedMu.Lock()
	h.(*Handler).capacityWarned = nil
	h.(*Handler).capacityWarnedMu.Unlock()
	buf.Reset()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/config/reload", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("reload: %d %s", rec.Code, rec.Body.String())
	}
	if n := strings.Count(buf.String(), "level=WARN msg=\"成本优先部署池"); n != 1 || !strings.Contains(buf.String(), "pool=legacy") {
		t.Fatalf("reload must warn once for legacy pool:\n%s", buf.String())
	}
}
