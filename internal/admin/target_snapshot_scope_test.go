package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
)

func scopeHandler(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	cfg := testConfig(dir, 2, []string{"k1"})
	cfg.Targets[0].APIKey = "sk-a-0001"
	cfg.Targets[0].APIKeys = []string{"sk-a-0002"}
	clients := testClients([]string{"k1"})
	configPath := filepath.Join(dir, "config.json")
	writeConfigFile(t, configPath, cfg)
	stores := setupTestStores(t, dir)
	seedClients(t, stores.clientStore, clients)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatal(err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	h := NewHandler(config.NewManager(configPath), store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)
	hc, err := h.currentConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ApplyConfig(hc); err != nil {
		t.Fatal(err)
	}
	return h
}

func scopeSnapshot(t *testing.T, h http.Handler, name string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/targets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var body struct {
		Targets []struct {
			Name     string `json:"name"`
			Snapshot string `json:"key_snapshot"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, tg := range body.Targets {
		if tg.Name == name {
			return tg.Snapshot
		}
	}
	t.Fatalf("target %s not listed", name)
	return ""
}

func scopeDo(h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rdr = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, "http://example.com/data/targets/"+path, rdr)
	if token != "" {
		req.Header.Set(issue68Header, token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 旧 X-Target-Key-Snapshot 路径绑定全局运行时 revision：任何目标发布后旧快照都失效（409）。
// 这是预期行为，只影响升级期间残留的旧标签页；新前端走 ETag + key ID，不受此限。
func TestLegacySnapshotRejectedAfterOtherTargetChange(t *testing.T) {
	h := scopeHandler(t)
	token := scopeSnapshot(t, h, "target-1")
	if token == "" {
		t.Fatal("missing snapshot")
	}
	if rec := scopeDo(h, http.MethodPut, "target-2/pause", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("pause B: %d %s", rec.Code, rec.Body.String())
	}
	rec := scopeDo(h, http.MethodPut, "target-1", token, targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:1"}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT A with a snapshot predating B's change: %d %s, want 409", rec.Code, rec.Body.String())
	}
	fresh := scopeSnapshot(t, h, "target-1")
	if fresh == token {
		t.Fatal("snapshot did not change after another target's publish")
	}
	rec = scopeDo(h, http.MethodPut, "target-1", fresh, targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:1"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT A with a fresh snapshot: %d %s", rec.Code, rec.Body.String())
	}
}

// 按 key ID 封禁不推进全局运行时 revision：封禁后旧快照 token 仍可用。
func TestBlockKeyByIDDoesNotAdvanceRuntimeRevision(t *testing.T) {
	h := scopeHandler(t)
	token := scopeSnapshot(t, h, "target-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/targets", nil))
	var list struct {
		Targets []struct {
			Name   string    `json:"name"`
			KeyIDs []*string `json:"key_ids"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	var id string
	for _, tg := range list.Targets {
		if tg.Name == "target-1" && len(tg.KeyIDs) > 1 && tg.KeyIDs[1] != nil {
			id = *tg.KeyIDs[1]
		}
	}
	if id == "" {
		t.Fatal("target-1 has no key id for slot 1")
	}
	if rec := scopeDo(h, http.MethodPut, "target-1/keys/id/"+id+"/block", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("block by id: %d %s", rec.Code, rec.Body.String())
	}
	if got := scopeSnapshot(t, h, "target-1"); got != token {
		t.Fatal("block by id advanced the runtime revision (snapshot token changed)")
	}
	rec2 := scopeDo(h, http.MethodPut, "target-1", token, targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:1"}))
	if rec2.Code != http.StatusOK {
		t.Fatalf("old snapshot rejected after block by id: %d %s", rec2.Code, rec2.Body.String())
	}
}

func TestTargetSnapshotRejectedAfterOwnKeyChange(t *testing.T) {
	h := scopeHandler(t)
	token := scopeSnapshot(t, h, "target-1")
	if rec := scopeDo(h, http.MethodPut, "target-1", "", targetUpdateBody("sk-a-0001", []string{"sk-a-0009"})); rec.Code != http.StatusOK {
		t.Fatalf("setup: %d", rec.Code)
	}
	rec := scopeDo(h, http.MethodPut, "target-1", token, targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:1"}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}
