package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/go-chi/chi/v5"
	bolt "go.etcd.io/bbolt"
)

// revEnv is a handler over a persistent bbolt file so tests can restart it.
type revEnv struct {
	t       *testing.T
	h       *Handler
	db      *bolt.DB
	stores  testStores
	service *proxy.Service
	dir     string
	sm      *SessionManager
}

func revConfig(dir string) *config.Config {
	cfg := testConfig(dir, 2, nil)
	cfg.Targets[0].Name = "target-a"
	cfg.Targets[0].APIKey = "sk-a-0001"
	cfg.Targets[0].APIKeys = []string{"sk-a-0002", "sk-a-0003"}
	cfg.Targets[1].Name = "target-b"
	cfg.Targets[1].APIKey = "sk-b-0001"
	return cfg
}

func newRevEnv(t *testing.T, dir string, withSessions bool) *revEnv {
	t.Helper()
	cfg := revConfig(dir)
	configPath := filepath.Join(dir, "config.json")
	writeConfigFile(t, configPath, cfg)
	db, err := nosql.OpenDB(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	stores := testStores{
		db: db, clientStore: nosql.NewClientStore(db), modelCostStore: nosql.NewModelCostStore(db),
		usageStore: nosql.NewUsageStore(db), userStore: nosql.NewUserStore(db), auditStore: nosql.NewAuditStore(db),
		targetStore: nosql.NewTargetStore(db),
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	var sm *SessionManager
	if withSessions {
		sm = NewSessionManager(config.AdminSessionConfig{Secret: "test-session-secret", CookieName: "session", TTLSeconds: 3600}, logger)
	}
	h := NewHandler(config.NewManager(configPath), auth.NewStore(), service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, sm, logger)
	hc, err := h.currentConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ApplyConfig(hc); err != nil {
		t.Fatal(err)
	}
	env := &revEnv{t: t, h: h, db: db, stores: stores, service: service, dir: dir, sm: sm}
	t.Cleanup(env.close)
	return env
}

func (e *revEnv) close() {
	_ = e.service.Close()
	_ = e.db.Close() // closing twice is harmless
}

type revView struct {
	Name      string    `json:"name"`
	ETag      *string   `json:"etag"`
	KeyIDs    []*string `json:"key_ids"`
	Snapshot  string    `json:"key_snapshot"`
	KeyStatus []struct {
		Blocked bool `json:"Blocked"`
	} `json:"key_statuses"`
	Indices []*int `json:"key_runtime_indices"`
}

func (e *revEnv) do(method, path, ifMatch string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		rdr = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, "http://example.com/data/"+path, rdr)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *revEnv) view(name string) revView {
	e.t.Helper()
	rec := e.do(http.MethodGet, "targets", "", nil)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Targets []revView `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		e.t.Fatal(err)
	}
	for _, tg := range body.Targets {
		if tg.Name == name {
			return tg
		}
	}
	e.t.Fatalf("target %s not listed", name)
	return revView{}
}

func quoted(etag string) string { return `"` + etag + `"` }

func (v revView) etag(t *testing.T) string {
	t.Helper()
	if v.ETag == nil || !strings.HasPrefix(*v.ETag, "t1-") {
		t.Fatalf("target %s has no etag in list response", v.Name)
	}
	return *v.ETag
}

func (v revView) id(t *testing.T, slot int) string {
	t.Helper()
	if slot >= len(v.KeyIDs) || v.KeyIDs[slot] == nil {
		t.Fatalf("target %s has no key id for slot %d: %v", v.Name, slot, v.KeyIDs)
	}
	return *v.KeyIDs[slot]
}

func idRef(id string) string { return "__existing_key_id__:" + id }

func (e *revEnv) runtimeEndpoint(name string) string {
	for _, st := range e.service.TargetStatuses(time.Now()) {
		if st.Name == name {
			return st.Endpoint
		}
	}
	return ""
}

func (e *revEnv) updateA(ifMatch string, mutate func(map[string]any)) *httptest.ResponseRecorder {
	v := e.view("target-a")
	body := targetUpdateBody(idRef(v.id(e.t, 0)), []string{idRef(v.id(e.t, 1)), idRef(v.id(e.t, 2))})
	body["endpoint"] = "https://a.example.com"
	if mutate != nil {
		mutate(body)
	}
	return e.do(http.MethodPut, "targets/target-a", ifMatch, body)
}

// (a) 编辑 A 时对 B 的任何写入都不改变 A 的 etag / key_ids。
func TestTargetETagStableAcrossOtherTargetWrites(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	before := e.view("target-a")
	etag := before.etag(t)
	if len(before.KeyIDs) != 3 {
		t.Fatalf("key_ids length = %d, want 3", len(before.KeyIDs))
	}

	if rec := e.do(http.MethodPut, "targets/target-b/pause", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("pause B: %d %s", rec.Code, rec.Body.String())
	}
	upd := targetUpdateBody("key", nil)
	upd["endpoint"] = "https://other.example.com"
	if rec := e.do(http.MethodPut, "targets/target-b", "", upd); rec.Code != http.StatusOK {
		t.Fatalf("update B: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(http.MethodPut, "targets/target-b/resume", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("resume B: %d", rec.Code)
	}
	if rec := e.do(http.MethodDelete, "targets/target-b", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete B: %d %s", rec.Code, rec.Body.String())
	}

	after := e.view("target-a")
	if after.etag(t) != etag {
		t.Fatalf("A etag changed: %s -> %s", etag, after.etag(t))
	}
	if strings.Join(derefAll(before.KeyIDs), ",") != strings.Join(derefAll(after.KeyIDs), ",") {
		t.Fatalf("A key_ids changed: %v -> %v", before.KeyIDs, after.KeyIDs)
	}
	rec := e.updateA(quoted(etag), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT A with If-Match: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ETag string `json:"etag"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.ETag == etag || resp.ETag == "" {
		t.Fatalf("response etag = %q (old %q), err=%v", resp.ETag, etag, err)
	}
	if got := e.view("target-a").etag(t); got != resp.ETag {
		t.Fatalf("listed etag %s != response etag %s", got, resp.ETag)
	}
}

func derefAll(in []*string) []string {
	out := make([]string, len(in))
	for i, p := range in {
		if p != nil {
			out[i] = *p
		}
	}
	return out
}

// (b) A 被他人保存后，旧 If-Match 一律 409，库与运行时都不变。
func TestTargetETagConflictLeavesStoreAndRuntimeUnchanged(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	stale := e.view("target-a").etag(t)
	if rec := e.updateA("", func(b map[string]any) { b["endpoint"] = "https://winner.example.com" }); rec.Code != http.StatusOK {
		t.Fatalf("winner save: %d %s", rec.Code, rec.Body.String())
	}
	stored := mustGetTarget(t, e.stores.targetStore, "target-a")
	storedRev := e.view("target-a").etag(t)
	if storedRev == stale {
		t.Fatal("winner save did not change etag")
	}

	check := func(label string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s: status %d body %s", label, rec.Code, rec.Body.String())
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["code"] != "target_revision_conflict" || body["error"] != "target changed; refresh targets" {
			t.Fatalf("%s: body %s", label, rec.Body.String())
		}
		if after := mustGetTarget(t, e.stores.targetStore, "target-a"); after.Endpoint != stored.Endpoint || len(after.APIKeys) != len(stored.APIKeys) || after.Paused != stored.Paused {
			t.Fatalf("%s changed the store: %+v", label, after)
		}
		if got := e.runtimeEndpoint("target-a"); got != "https://winner.example.com" {
			t.Fatalf("%s changed the runtime endpoint: %q", label, got)
		}
		if _, _, _, ok := e.service.TargetKeySnapshot(stored); !ok {
			t.Fatalf("%s: runtime diverged from the store", label)
		}
		if e.view("target-a").etag(t) != storedRev {
			t.Fatalf("%s changed the etag", label)
		}
	}
	check("PUT", e.updateA(quoted(stale), func(b map[string]any) { b["endpoint"] = "https://loser.example.com" }))
	check("pause", e.do(http.MethodPut, "targets/target-a/pause", quoted(stale), nil))
	check("resume", e.do(http.MethodPut, "targets/target-a/resume", quoted(stale), nil))
	check("delete", e.do(http.MethodDelete, "targets/target-a", quoted(stale), nil))

	// Unquoted tokens are tolerated; garbage is a client error.
	if rec := e.do(http.MethodPut, "targets/target-a/pause", storedRev, nil); rec.Code != http.StatusOK {
		t.Fatalf("unquoted If-Match: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(http.MethodPut, "targets/target-a/resume", `"abc"`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage If-Match: %d", rec.Code)
	}
	etag := e.view("target-a").etag(t)
	if rec := e.do(http.MethodDelete, "targets/target-a", quoted(etag), nil); rec.Code != http.StatusOK {
		t.Fatalf("matching delete: %d %s", rec.Code, rec.Body.String())
	}
}

// (c) 按稳定 key ID 引用已有 key，重排后结果正确。
func TestTargetUpdateWithExistingKeyIDs(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	v := e.view("target-a")
	etag := v.etag(t)
	body := targetUpdateBody(idRef(v.id(t, 2)), []string{idRef(v.id(t, 1)), "sk-a-0099", idRef(v.id(t, 0))})
	rec := e.do(http.MethodPut, "targets/target-a", quoted(etag), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder: %d %s", rec.Code, rec.Body.String())
	}
	got := mustGetTarget(t, e.stores.targetStore, "target-a")
	if got.APIKey != "sk-a-0003" || strings.Join(got.APIKeys, ",") != "sk-a-0002,sk-a-0099,sk-a-0001" {
		t.Fatalf("keys after reorder: %q %v", got.APIKey, got.APIKeys)
	}
	// 引用不依赖旧快照头，且在无 If-Match 时同样可用。
	v = e.view("target-a")
	rec = e.do(http.MethodPut, "targets/target-a", "", targetUpdateBody(idRef(v.id(t, 1)), []string{idRef(v.id(t, 3))}))
	if rec.Code != http.StatusOK {
		t.Fatalf("no If-Match: %d %s", rec.Code, rec.Body.String())
	}
	if got := mustGetTarget(t, e.stores.targetStore, "target-a"); got.APIKey != "sk-a-0002" || strings.Join(got.APIKeys, ",") != "sk-a-0001" {
		t.Fatalf("keys after second save: %q %v", got.APIKey, got.APIKeys)
	}
	before := mustGetTarget(t, e.stores.targetStore, "target-a")
	rec = e.do(http.MethodPut, "targets/target-a", "", targetUpdateBody(idRef("AAAAAAAAAAAAAAAAAAAAAA"), nil))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid existing key reference") {
		t.Fatalf("unknown key id: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(http.MethodPut, "targets/target-a", "", targetUpdateBody("sk-a-0001", []string{idRef("AAAAAAAAAAAAAAAAAAAAAA")}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown key id in api_keys: %d", rec.Code)
	}
	if after := mustGetTarget(t, e.stores.targetStore, "target-a"); after.APIKey != before.APIKey || len(after.APIKeys) != len(before.APIKeys) {
		t.Fatal("rejected save changed the store")
	}
}

// (d) 按 ID 封禁不改变 etag，之后带旧 If-Match 保存仍成功。
func TestBlockKeyByIDKeepsETag(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	v := e.view("target-a")
	etag := v.etag(t)
	id := v.id(t, 1) // sk-a-0002
	rec := e.do(http.MethodPut, "targets/target-a/keys/id/"+id+"/block", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("block: %d %s", rec.Code, rec.Body.String())
	}
	after := e.view("target-a")
	if after.etag(t) != etag {
		t.Fatalf("block changed etag %s -> %s", etag, after.etag(t))
	}
	if len(after.KeyStatus) != 3 || !after.KeyStatus[1].Blocked || after.KeyStatus[0].Blocked {
		t.Fatalf("key statuses after block: %+v", after.KeyStatus)
	}
	if rec := e.updateA(quoted(etag), func(b map[string]any) { b["endpoint"] = "https://after-block.example.com" }); rec.Code != http.StatusOK {
		t.Fatalf("save with old If-Match after block: %d %s", rec.Code, rec.Body.String())
	}
	if st := e.view("target-a").KeyStatus; len(st) != 3 || !st[1].Blocked {
		t.Fatalf("block lost across save: %+v", st)
	}
	rec = e.do(http.MethodPut, "targets/target-a/keys/id/"+id+"/unblock", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unblock: %d %s", rec.Code, rec.Body.String())
	}
	if st := e.view("target-a").KeyStatus; st[1].Blocked {
		t.Fatal("unblock did not clear the block")
	}
	for _, path := range []string{"targets/target-a/keys/id/AAAAAAAAAAAAAAAAAAAAAA/block", "targets/nope/keys/id/" + id + "/block"} {
		if rec := e.do(http.MethodPut, path, "", nil); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: %d, want 404", path, rec.Code)
		}
	}
}

// (e) 同一目标内重复的 key：首个无后缀，其后依次 .1、.2；封禁命中对应条目。
func TestDuplicateKeyIDsGetOccurrenceSuffix(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	rec := e.do(http.MethodPut, "targets/target-a", "", targetUpdateBody("sk-dup", []string{"sk-dup", "sk-other", "sk-dup"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body.String())
	}
	v := e.view("target-a")
	if len(v.KeyIDs) != 4 {
		t.Fatalf("key_ids = %v", v.KeyIDs)
	}
	base, second, other, third := v.id(t, 0), v.id(t, 1), v.id(t, 2), v.id(t, 3)
	if strings.Contains(base, ".") || second != base+".1" || third != base+".2" || strings.Contains(other, ".") || other == base {
		t.Fatalf("duplicate ids: %q %q %q %q", base, second, other, third)
	}
	// 运行时池按值去重：[sk-dup, sk-other]。封禁重复项的第二次出现命中 sk-dup 条目。
	if rec := e.do(http.MethodPut, "targets/target-a/keys/id/"+second+"/block", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("block dup.1: %d %s", rec.Code, rec.Body.String())
	}
	st := e.view("target-a").KeyStatus
	if len(st) != 2 || !st[0].Blocked || st[1].Blocked {
		t.Fatalf("statuses after blocking dup.1: %+v", st)
	}
	if rec := e.do(http.MethodPut, "targets/target-a/keys/id/"+second+"/unblock", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("unblock dup.1: %d", rec.Code)
	}
	if rec := e.do(http.MethodPut, "targets/target-a/keys/id/"+other+"/block", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("block other: %d", rec.Code)
	}
	if st := e.view("target-a").KeyStatus; st[0].Blocked || !st[1].Blocked {
		t.Fatalf("statuses after blocking other: %+v", st)
	}
	if rec := e.do(http.MethodPut, "targets/target-a/keys/id/"+base+".3/block", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("nonexistent occurrence: %d", rec.Code)
	}
	// 重复项的引用解析为同一个 key 值。
	if rec := e.do(http.MethodPut, "targets/target-a", "", targetUpdateBody(idRef(third), []string{idRef(other)})); rec.Code != http.StatusOK {
		t.Fatalf("reference dup.2: %d %s", rec.Code, rec.Body.String())
	}
	if got := mustGetTarget(t, e.stores.targetStore, "target-a"); got.APIKey != "sk-dup" || strings.Join(got.APIKeys, ",") != "sk-other" {
		t.Fatalf("keys: %q %v", got.APIKey, got.APIKeys)
	}
}

// (f) 落库失败时运行时完全不变（先落库后应用）。
func TestPersistFailureLeavesRuntimeUntouched(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	stored := mustGetTarget(t, e.stores.targetStore, "target-a")
	etag := e.view("target-a").etag(t)
	statusesBefore := e.service.TargetStatuses(time.Now())

	cfg, err := e.h.currentConfig()
	if err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Targets {
		if cfg.Targets[i].Name == "target-a" {
			cfg.Targets[i].Endpoint = "https://never.example.com"
			cfg.Targets[i].Paused = true
		}
	}
	rec := httptest.NewRecorder()
	e.h.configMu.Lock()
	_, ok := e.h.commitConfigChange(rec, cfg, "failed to persist target", func() error { return errors.New("disk full") })
	e.h.configMu.Unlock()
	if ok || rec.Code != http.StatusInternalServerError {
		t.Fatalf("persist failure: ok=%v status=%d body=%s", ok, rec.Code, rec.Body.String())
	}
	if got := e.runtimeEndpoint("target-a"); got != stored.Endpoint {
		t.Fatalf("runtime endpoint changed: %q", got)
	}
	statusesAfter := e.service.TargetStatuses(time.Now())
	if len(statusesAfter) != len(statusesBefore) {
		t.Fatalf("runtime target count changed: %d -> %d", len(statusesBefore), len(statusesAfter))
	}
	for i := range statusesAfter {
		if statusesAfter[i].Paused != statusesBefore[i].Paused {
			t.Fatalf("runtime pause state changed for %s", statusesAfter[i].Name)
		}
	}
	if got := e.view("target-a").etag(t); got != etag {
		t.Fatalf("etag changed %s -> %s", etag, got)
	}
}

// (g) 服务重启（重开 DB、新 Handler）后，旧 etag 与 key ID 仍可用。
func TestETagAndKeyIDsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	e1 := newRevEnv(t, dir, false)
	before := e1.view("target-a")
	etag := before.etag(t)
	e1.close()

	e2 := newRevEnv(t, dir, false)
	after := e2.view("target-a")
	if after.etag(t) != etag {
		t.Fatalf("etag across restart: %s -> %s", etag, after.etag(t))
	}
	if strings.Join(derefAll(before.KeyIDs), ",") != strings.Join(derefAll(after.KeyIDs), ",") {
		t.Fatalf("key ids across restart: %v -> %v", before.KeyIDs, after.KeyIDs)
	}
	body := targetUpdateBody(idRef(before.id(t, 2)), []string{idRef(before.id(t, 1))})
	rec := e2.do(http.MethodPut, "targets/target-a", quoted(etag), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("save with pre-restart etag and ids: %d %s", rec.Code, rec.Body.String())
	}
	if got := mustGetTarget(t, e2.stores.targetStore, "target-a"); got.APIKey != "sk-a-0003" || strings.Join(got.APIKeys, ",") != "sk-a-0002" {
		t.Fatalf("keys: %q %v", got.APIKey, got.APIKeys)
	}
	if rec := e2.do(http.MethodPut, "targets/target-a/keys/id/"+before.id(t, 1)+"/block", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("block by pre-restart id: %d %s", rec.Code, rec.Body.String())
	}
}

// (h) 只读角色不能使用新写路由，但能看到 etag 与 key_ids（不泄露 key 值）。
func TestKeyIDBlockRouteIsGuardedAndReadonlyGetsIDs(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), true)
	secured := chi.NewRouter()
	secured.Route("/admin", func(r chi.Router) {
		r.Use(e.sm.Middleware)
		r.Mount("/", e.h)
	})
	_, adminCookie, err := e.sm.CreateSession("admin", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	_, readonlyCookie, err := e.sm.CreateSession("viewer", RoleReadonly)
	if err != nil {
		t.Fatal(err)
	}
	send := func(cookie *http.Cookie, method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/admin/data/"+path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		secured.ServeHTTP(rec, req)
		return rec
	}
	list := send(adminCookie, http.MethodGet, "targets")
	var body struct {
		Targets []revView `json:"targets"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &body); err != nil || len(body.Targets) != 2 {
		t.Fatalf("admin list: %v %s", err, list.Body.String())
	}
	var a revView
	for _, tg := range body.Targets {
		if tg.Name == "target-a" {
			a = tg
		}
	}
	id := a.id(t, 1)
	for _, action := range []string{"block", "unblock"} {
		rec := send(readonlyCookie, http.MethodPut, "targets/target-a/keys/id/"+id+"/"+action)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("readonly %s: %d, want 403", action, rec.Code)
		}
	}
	if st := e.service.KeyPoolStatus("target-a"); st[1].Blocked {
		t.Fatal("readonly request changed key state")
	}
	ro := send(readonlyCookie, http.MethodGet, "targets")
	if ro.Code != http.StatusOK {
		t.Fatalf("readonly list: %d", ro.Code)
	}
	if strings.Contains(ro.Body.String(), "sk-a-0002") {
		t.Fatal("readonly response contains a key value")
	}
	var roBody struct {
		Targets []revView `json:"targets"`
	}
	if err := json.Unmarshal(ro.Body.Bytes(), &roBody); err != nil {
		t.Fatal(err)
	}
	for _, tg := range roBody.Targets {
		if tg.ETag == nil || len(tg.KeyIDs) == 0 {
			t.Fatalf("readonly view lacks etag/key_ids: %+v", tg)
		}
	}
	if rec := send(adminCookie, http.MethodPut, "targets/target-a/keys/id/"+id+"/block"); rec.Code != http.StatusOK {
		t.Fatalf("admin block: %d %s", rec.Code, rec.Body.String())
	}
}

// 创建响应与列表都携带 etag；导入的存量目标已有 etag。
func TestCreateTargetReturnsETag(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	rec := e.do(http.MethodPost, "targets", "", map[string]any{"name": "target-c", "endpoint_type": "azure_openai", "usage_adapter": "standard", "endpoint": "https://c.example.com", "api_key": "kc"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ETag string `json:"etag"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.ETag == "" {
		t.Fatalf("create response: %s", rec.Body.String())
	}
	if got := e.view("target-c").etag(t); got != resp.ETag {
		t.Fatalf("listed %s != created %s", got, resp.ETag)
	}
}

// 存量库两个编辑者拿到同一个 ETag 并发保存：一个 200、一个 409（任何目标都有 ETag，
// 没有"未版本化"的无保护状态）。
func TestTwoEditorsSameETagOneWins(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	etag := e.view("target-a").etag(t)
	first := e.updateA(quoted(etag), func(b map[string]any) { b["endpoint"] = "https://editor-1.example.com" })
	second := e.updateA(quoted(etag), func(b map[string]any) { b["endpoint"] = "https://editor-2.example.com" })
	if first.Code != http.StatusOK || second.Code != http.StatusConflict {
		t.Fatalf("editors: %d / %d (%s)", first.Code, second.Code, second.Body.String())
	}
	if got := mustGetTarget(t, e.stores.targetStore, "target-a"); got.Endpoint != "https://editor-1.example.com" {
		t.Fatalf("loser overwrote winner: %s", got.Endpoint)
	}
}

// 旧二进制回滚后直接改写 targets 字节：旧 ETag 被拒；列表给出的新 ETag 可用。
func TestOutOfBandRewriteInvalidatesETag(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	stale := e.view("target-a").etag(t)
	stored := mustGetTarget(t, e.stores.targetStore, "target-a")
	stored.Endpoint = "https://rewritten-by-old-binary.example.com"
	data, _ := json.Marshal(stored)
	if err := e.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(nosql.BucketTargets)).Put([]byte("target-a"), data)
	}); err != nil {
		t.Fatal(err)
	}
	if rec := e.updateA(quoted(stale), nil); rec.Code != http.StatusConflict {
		t.Fatalf("stale etag after out-of-band rewrite: %d %s", rec.Code, rec.Body.String())
	}
	if e.view("target-a").etag(t) == stale {
		t.Fatal("list still reports the pre-rewrite etag")
	}
}

func TestIfMatchForms(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	etag := e.view("target-a").etag(t)
	if rec := e.do(http.MethodPut, "targets/target-a/pause", `W/"`+etag+`"`, nil); rec.Code != http.StatusOK {
		t.Fatalf("weak etag: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(http.MethodPut, "targets/target-a/resume", "*", nil); rec.Code != http.StatusOK {
		t.Fatalf("wildcard on existing target: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(http.MethodPut, "targets/nope/pause", "*", nil); rec.Code != http.StatusConflict {
		t.Fatalf("wildcard on missing target: %d", rec.Code)
	}
	if rec := e.do(http.MethodPut, "targets/target-a/pause", `"7"`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("numeric legacy token: %d", rec.Code)
	}
}

// 不同目标上相同的 key 值，其 key ID 不同（HMAC 带目标名域分隔）。
func TestSameKeyOnTwoTargetsHasDifferentIDs(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	if rec := e.do(http.MethodPut, "targets/target-b", "", targetUpdateBody("sk-a-0001", nil)); rec.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body.String())
	}
	a, b := e.view("target-a").id(t, 0), e.view("target-b").id(t, 0)
	if a == b {
		t.Fatalf("same key on two targets shares an id: %s", a)
	}
}

// key ID 密钥不可用时，key_ids 是 JSON null（不是数组），其余字段照常返回。
func TestKeyIDsAreNullWhenSecretUnavailable(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	e.h.keyIDSecretLoader = func() ([]byte, error) { return nil, errors.New("meta unreadable") }
	e.h.keyIDMu.Lock()
	e.h.keyIDSecretCache = nil
	e.h.keyIDMu.Unlock()
	rec := e.do(http.MethodGet, "targets", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var body struct {
		Targets []map[string]json.RawMessage `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Targets) == 0 {
		t.Fatalf("decode: %v", err)
	}
	for _, tg := range body.Targets {
		if string(tg["key_ids"]) != "null" {
			t.Fatalf("key_ids = %s, want null", tg["key_ids"])
		}
		if len(tg["etag"]) < 5 {
			t.Fatalf("etag missing: %s", tg["etag"])
		}
	}
	// 不带 key 字段保存：key 保持不变，其余字段生效。
	etag := e.view("target-a").etag(t)
	before := mustGetTarget(t, e.stores.targetStore, "target-a")
	rec = e.do(http.MethodPut, "targets/target-a", quoted(etag), map[string]any{"endpoint": "https://no-keys.example.com"})
	if rec.Code != http.StatusOK {
		t.Fatalf("save without key fields: %d %s", rec.Code, rec.Body.String())
	}
	after := mustGetTarget(t, e.stores.targetStore, "target-a")
	if after.Endpoint != "https://no-keys.example.com" || after.APIKey != before.APIKey || strings.Join(after.APIKeys, ",") != strings.Join(before.APIKeys, ",") {
		t.Fatalf("keys not preserved: %+v", after)
	}
}

// 落库成功后 auth 应用失败：记录日志，响应仍为成功并带 warning（数据已保存，不能报 500）。
func TestAuthApplyFailureAfterCommitIsWarning(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	e.h.authApply = func([]config.Client, []config.Target) error { return errors.New("auth boom") }
	etag := e.view("target-a").etag(t)
	rec := e.updateA(quoted(etag), func(b map[string]any) { b["endpoint"] = "https://saved.example.com" })
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp["warning"] != "auth reload pending" || resp["ok"] != true {
		t.Fatalf("body %s", rec.Body.String())
	}
	if resp["etag"] == "" || resp["etag"] == nil {
		t.Fatalf("missing etag: %s", rec.Body.String())
	}
	if got := mustGetTarget(t, e.stores.targetStore, "target-a"); got.Endpoint != "https://saved.example.com" {
		t.Fatalf("not persisted: %s", got.Endpoint)
	}
	if got := e.runtimeEndpoint("target-a"); got != "https://saved.example.com" {
		t.Fatalf("runtime not applied: %s", got)
	}
}

// 保存目标与新建客户端交错：目标保存已读取 clients 但尚未应用 auth 时新建的客户端，
// 不能被随后的 auth 应用以旧 clients 列表覆盖。
func TestTargetSaveAndClientCreateInterleave(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	e.h.authApply = func(clients []config.Client, targets []config.Target) error {
		once.Do(func() { close(entered) })
		<-release
		return e.h.authStore.LoadFromConfigWithPools(clients, targets)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		b := targetUpdateBody("key", nil)
		b["endpoint"] = "https://b-interleave.example.com"
		if rec := e.do(http.MethodPut, "targets/target-b", "", b); rec.Code != http.StatusOK {
			t.Errorf("target save: %d %s", rec.Code, rec.Body.String())
		}
	}()
	<-entered // 目标保存已落库、停在 auth 应用前
	created := make(chan struct{})
	go func() {
		defer close(created)
		rec := e.do(http.MethodPost, "clients", "", config.Client{Name: "late", AccessKey: "ck-late", AllowedTargets: []string{"target-a"}})
		if rec.Code != http.StatusCreated {
			t.Errorf("client create: %d %s", rec.Code, rec.Body.String())
		}
	}()
	time.Sleep(50 * time.Millisecond) // 让客户端写入有机会（在无锁实现下）抢先应用
	close(release)
	wg.Wait()
	<-created
	if _, ok := e.h.authStore.Authenticate("ck-late"); !ok {
		t.Fatal("client created during a target save is missing from authStore")
	}
}

// If-Match 支持逗号分隔列表：任一项匹配即通过；空项或畸形项为 400。
func TestIfMatchList(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	etag := e.view("target-a").etag(t)
	stale := "t1-AAAAAAAAAAAAAAAAAAAAAA"
	if rec := e.do(http.MethodPut, "targets/target-a/pause", quoted(stale)+", "+quoted(etag), nil); rec.Code != http.StatusOK {
		t.Fatalf("list containing the current etag: %d %s", rec.Code, rec.Body.String())
	}
	etag = e.view("target-a").etag(t)
	if rec := e.do(http.MethodPut, "targets/target-a/resume", quoted(stale)+`,W/"t1-BBBBBBBBBBBBBBBBBBBBBB"`, nil); rec.Code != http.StatusConflict {
		t.Fatalf("list of stale etags: %d, want 409", rec.Code)
	}
	for _, bad := range []string{quoted(etag) + ",", "," + quoted(etag), quoted(etag) + ",,", quoted(etag) + `, "abc"`} {
		if rec := e.do(http.MethodPut, "targets/target-a/resume", bad, nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("If-Match %q: %d, want 400", bad, rec.Code)
		}
	}
	if rec := e.do(http.MethodPut, "targets/target-a/resume", quoted(stale)+", *", nil); rec.Code != http.StatusOK {
		t.Fatalf("list with wildcard: %d %s", rec.Code, rec.Body.String())
	}
}

// 按 ID 封禁/解封写审计：动作、目标名、key_id；不含 key 值。
func TestBlockKeyByIDWritesAudit(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	id := e.view("target-a").id(t, 1) // sk-a-0002
	if rec := e.do(http.MethodPut, "targets/target-a/keys/id/"+id+"/block", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("block: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(http.MethodPut, "targets/target-a/keys/id/"+id+"/unblock", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("unblock: %d %s", rec.Code, rec.Body.String())
	}
	events, err := e.stores.auditStore.List(20)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, ev := range events {
		if strings.Contains(ev.Detail, "sk-a-") || strings.Contains(ev.Object, "sk-a-") {
			t.Fatalf("audit leaks a key value: %+v", ev)
		}
		if ev.Object == "target-a" && ev.Detail == "key_id="+id && ev.Result == "success" {
			seen[ev.Action] = true
		}
	}
	if !seen["block_target_key"] || !seen["unblock_target_key"] {
		t.Fatalf("missing audit records, got %+v", events)
	}
}

// 同上，覆盖客户端 update 与 delete：目标保存卡在 auth 应用前时发起的客户端改动，
// 不能被随后以旧 clients 列表做的 auth 应用覆盖。
func TestTargetSaveAndClientUpdateDeleteInterleave(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	for _, c := range []config.Client{
		{Name: "keep", AccessKey: "ck-keep-old", AllowedTargets: []string{"target-a"}},
		{Name: "gone", AccessKey: "ck-gone", AllowedTargets: []string{"target-a"}},
	} {
		if rec := e.do(http.MethodPost, "clients", "", c); rec.Code != http.StatusCreated {
			t.Fatalf("seed client %s: %d %s", c.Name, rec.Code, rec.Body.String())
		}
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	e.h.authApply = func(clients []config.Client, targets []config.Target) error {
		once.Do(func() { close(entered) })
		<-release
		return e.h.authStore.LoadFromConfigWithPools(clients, targets)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		b := targetUpdateBody("key", nil)
		b["endpoint"] = "https://b-interleave2.example.com"
		if rec := e.do(http.MethodPut, "targets/target-b", "", b); rec.Code != http.StatusOK {
			t.Errorf("target save: %d %s", rec.Code, rec.Body.String())
		}
	}()
	<-entered
	done := make(chan struct{})
	go func() {
		defer close(done)
		if rec := e.do(http.MethodPut, "clients/keep", "", config.Client{Name: "keep", AccessKey: "ck-keep-new", AllowedTargets: []string{"target-a"}}); rec.Code != http.StatusOK {
			t.Errorf("client update: %d %s", rec.Code, rec.Body.String())
		}
		if rec := e.do(http.MethodDelete, "clients/gone", "", nil); rec.Code != http.StatusNoContent {
			t.Errorf("client delete: %d %s", rec.Code, rec.Body.String())
		}
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	<-done
	if _, ok := e.h.authStore.Authenticate("ck-keep-new"); !ok {
		t.Fatal("updated client key missing from authStore after interleave")
	}
	if _, ok := e.h.authStore.Authenticate("ck-keep-old"); ok {
		t.Fatal("stale client key still authenticates after interleave")
	}
	if _, ok := e.h.authStore.Authenticate("ck-gone"); ok {
		t.Fatal("deleted client still authenticates after interleave")
	}
}

// 配额评估在释放 configMu 之后执行：评估被阻塞期间，目标保存不受影响。
func TestClientQuotaEvaluationDoesNotHoldConfigLock(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	e.h.clientQuotaEvaluateHook = func(string) {
		once.Do(func() { close(entered) })
		<-release
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if rec := e.do(http.MethodPost, "clients", "", config.Client{Name: "slow", AccessKey: "ck-slow", AllowedTargets: []string{"target-a"}}); rec.Code != http.StatusCreated {
			t.Errorf("client create: %d %s", rec.Code, rec.Body.String())
		}
	}()
	<-entered
	saved := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		b := targetUpdateBody("key", nil)
		b["endpoint"] = "https://b-not-blocked.example.com"
		saved <- e.do(http.MethodPut, "targets/target-b", "", b)
	}()
	select {
	case rec := <-saved:
		if rec.Code != http.StatusOK {
			t.Errorf("target save: %d %s", rec.Code, rec.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Error("target save blocked while client quota evaluation was running")
	}
	close(release)
	<-done
}
