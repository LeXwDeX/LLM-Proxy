package admin

import (
	"bytes"
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

// 客户端写路径的 auth 契约（v1.11.2）：在 configMu 内、落库之前用
// auth.NewStoreFromConfig 构建下一份表，落库成功后再 Swap 发布。读取或构建
// 失败都在落库前中止（请求校验 4xx，内部故障 500），运行时 auth 与数据库同时保持旧状态；
// 因此不再有 auth_reload_pending 这一“已落库但未应用”的中间态。

func newClientAuthTestHandler(t *testing.T, withAuthStore bool) (*Handler, testStores) {
	t.Helper()
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := config.NewManager(configPath)
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}
	var authStore *auth.Store
	if withAuthStore {
		authStore, err = auth.NewStoreFromConfig(clients, nil)
		if err != nil {
			t.Fatalf("init auth store: %v", err)
		}
	}
	h := NewHandler(manager, authStore, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)
	return h, stores
}

func doClientWrite(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, "http://example.com/data/"+path, rdr)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 新建落库即发布：无需第二次重载，请求返回前新 key 就已可认证。
func TestCreateClientPublishesAuthOnCommit(t *testing.T) {
	h, stores := newClientAuthTestHandler(t, true)

	rec := doClientWrite(t, h, http.MethodPost, "clients", `{"name":"bob","access_key":"bob-key","allowed_targets":["alpha"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	principal, ok := h.authStore.Authenticate("bob-key")
	if !ok {
		t.Fatal("new client key must authenticate immediately after the write")
	}
	if !principal.CanAccess("alpha") || principal.CanAccess("beta") {
		t.Fatal("new principal must carry the submitted allowed_targets")
	}
	if stored, err := stores.clientStore.Get("bob"); err != nil || stored == nil {
		t.Fatalf("client must be persisted: %+v err=%v", stored, err)
	}
}

// 修改落库即发布：新密钥与 ACL 生效，旧密钥立刻失效。
func TestUpdateClientReplacesAuthOnCommit(t *testing.T) {
	h, stores := newClientAuthTestHandler(t, true)

	rec := doClientWrite(t, h, http.MethodPut, "clients/client-1", `{"name":"client-1","access_key":"k1-new","allowed_targets":["beta"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if _, ok := h.authStore.Authenticate("k1"); ok {
		t.Fatal("old key must stop authenticating immediately after update")
	}
	principal, ok := h.authStore.Authenticate("k1-new")
	if !ok {
		t.Fatal("new key must authenticate immediately after update")
	}
	if !principal.CanAccess("beta") || principal.CanAccess("alpha") {
		t.Fatal("updated principal must carry the new allowed_targets only")
	}
	if stored, err := stores.clientStore.Get("client-1"); err != nil || stored == nil || stored.AccessKey != "k1-new" {
		t.Fatalf("update must be persisted: %+v err=%v", stored, err)
	}
}

// 删除落库即撤权：key 立刻失效。
func TestDeleteClientRevokesAuthOnCommit(t *testing.T) {
	h, stores := newClientAuthTestHandler(t, true)

	rec := doClientWrite(t, h, http.MethodDelete, "clients/client-1", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	if _, ok := h.authStore.Authenticate("k1"); ok {
		t.Fatal("deleted client key must stop authenticating immediately")
	}
	if stored, err := stores.clientStore.Get("client-1"); err != nil || stored != nil {
		t.Fatalf("deletion must be persisted: %+v err=%v", stored, err)
	}
}

// auth store 不可用时在落库前拒绝：数据库与运行时都不变。
func TestClientWriteWithNilAuthStoreFailsBeforeCommit(t *testing.T) {
	h, stores := newClientAuthTestHandler(t, false)

	rec := doClientWrite(t, h, http.MethodPost, "clients", `{"name":"bob","access_key":"bob-key"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("create status = %d, want 500 (body=%s)", rec.Code, rec.Body.String())
	}
	if stored, err := stores.clientStore.Get("bob"); err != nil || stored != nil {
		t.Fatalf("rejected create must not persist: %+v err=%v", stored, err)
	}

	rec = doClientWrite(t, h, http.MethodPut, "clients/client-1", `{"name":"client-1","access_key":"k1-new"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("update status = %d, want 500 (body=%s)", rec.Code, rec.Body.String())
	}
	if stored, err := stores.clientStore.Get("client-1"); err != nil || stored == nil || stored.AccessKey != "k1" {
		t.Fatalf("rejected update must not persist: %+v err=%v", stored, err)
	}

	rec = doClientWrite(t, h, http.MethodDelete, "clients/client-1", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("delete status = %d, want 500 (body=%s)", rec.Code, rec.Body.String())
	}
	if stored, err := stores.clientStore.Get("client-1"); err != nil || stored == nil {
		t.Fatalf("rejected delete must not persist: %+v err=%v", stored, err)
	}
}

// 请求自身的校验失败（空 access_key、非法 IP、重复 access_key）在投影构建阶段
// 提前暴露：沿用 store 的 4xx 语义，不落库，运行时 auth 不变。
func TestClientWriteValidationFailureLeavesAuthUntouched(t *testing.T) {
	h, stores := newClientAuthTestHandler(t, true)

	rec := doClientWrite(t, h, http.MethodPost, "clients", `{"name":"bob","access_key":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if stored, err := stores.clientStore.Get("bob"); err != nil || stored != nil {
		t.Fatalf("rejected create must not persist: %+v err=%v", stored, err)
	}

	rec = doClientWrite(t, h, http.MethodPut, "clients/client-1", `{"name":"client-1","access_key":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if stored, err := stores.clientStore.Get("client-1"); err != nil || stored == nil || stored.AccessKey != "k1" {
		t.Fatalf("rejected update must not persist: %+v err=%v", stored, err)
	}

	// 非法 CIDR 与 store 校验一致：400，且不落库。
	rec = doClientWrite(t, h, http.MethodPost, "clients", `{"name":"bad-ip","access_key":"bad-ip-key","ip_address":"192.0.2.0/33"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid CIDR status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if stored, err := stores.clientStore.Get("bad-ip"); err != nil || stored != nil {
		t.Fatalf("rejected invalid-IP create must not persist: %+v err=%v", stored, err)
	}

	if _, ok := h.authStore.Authenticate("k1"); !ok {
		t.Fatal("auth must keep serving the committed state after a pre-commit validation failure")
	}
	if _, ok := h.authStore.Authenticate("bad-ip-key"); ok {
		t.Fatal("rejected create must not publish its key")
	}

	// 重复 access_key 在投影构建阶段被拒绝：409，不落库、不发布新表。
	rec = doClientWrite(t, h, http.MethodPost, "clients", `{"name":"bob","access_key":"k1"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate-key create status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if stored, err := stores.clientStore.Get("bob"); err != nil || stored != nil {
		t.Fatalf("rejected duplicate-key create must not persist: %+v err=%v", stored, err)
	}
	if _, ok := h.authStore.Authenticate("k1"); !ok {
		t.Fatal("existing key must keep authenticating after a rejected duplicate-key create")
	}
}

// 读取当前客户端失败时在落库前中止，运行时 auth 不变。
func TestClientWriteReadFailureLeavesAuthUntouched(t *testing.T) {
	h, stores := newClientAuthTestHandler(t, true)
	if err := stores.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	rec := doClientWrite(t, h, http.MethodPost, "clients", `{"name":"bob","access_key":"bob-key"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", rec.Code, rec.Body.String())
	}
	if _, ok := h.authStore.Authenticate("k1"); !ok {
		t.Fatal("auth must stay unchanged when the client list cannot be read")
	}
}

// 落库失败时丢弃已构建的表：运行时 auth 保持旧状态，数据库不变。
func TestClientWritePersistFailureLeavesAuthUntouched(t *testing.T) {
	h, stores := newClientAuthTestHandler(t, true)

	// 名字冲突（密钥不同，投影可构建）→ 由 store 拒绝（409），新密钥不得进入 auth。
	rec := doClientWrite(t, h, http.MethodPost, "clients", `{"name":"client-1","access_key":"other-key"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("create status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if _, ok := h.authStore.Authenticate("other-key"); ok {
		t.Fatal("rejected create must not publish its key")
	}
	if stored, err := stores.clientStore.Get("client-1"); err != nil || stored == nil || stored.AccessKey != "k1" {
		t.Fatalf("rejected create must not overwrite the existing client: %+v err=%v", stored, err)
	}

	// update 不存在的客户端 → 404，body 里的密钥不得进入 auth。
	rec = doClientWrite(t, h, http.MethodPut, "clients/ghost", `{"name":"ghost","access_key":"ghost-key"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if _, ok := h.authStore.Authenticate("ghost-key"); ok {
		t.Fatal("rejected update must not publish its key")
	}

	// delete 不存在的客户端 → 404，auth 不变。
	rec = doClientWrite(t, h, http.MethodDelete, "clients/ghost", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if _, ok := h.authStore.Authenticate("k1"); !ok {
		t.Fatal("existing key must keep authenticating after a rejected delete")
	}
}

// 回归（v1.11.2）：投影匹配必须与 store 的 bucket 键完全一致
// （strings.ToLower(strings.TrimSpace(name))）。strings.EqualFold 在 U+0130
// （"İ"）上与 ToLower 语义不同：ToLower("İ") == "i"，而 EqualFold("İ", "i") == false。
// 旧实现下，含 U+0130 的名字经小写路径更新/删除时投影命中不到条目，数据库已变更
// 但 Swap 仍发布旧表：旧 key 继续可认证、新 key 不生效。
func TestClientWriteLowercasePathMatchesStoreKey(t *testing.T) {
	h, stores := newClientAuthTestHandler(t, true)

	rec := doClientWrite(t, h, http.MethodPost, "clients", `{"name":"İbrahim","access_key":"ibrahim-old"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if _, ok := h.authStore.Authenticate("ibrahim-old"); !ok {
		t.Fatal("created key must authenticate")
	}

	// 小写路径换 key：落库成功，auth 投影必须命中同一条目并同步发布。
	rec = doClientWrite(t, h, http.MethodPut, "clients/ibrahim", `{"name":"İbrahim","access_key":"ibrahim-new"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if stored, err := stores.clientStore.Get("İbrahim"); err != nil || stored == nil || stored.AccessKey != "ibrahim-new" {
		t.Fatalf("update must be persisted: %+v err=%v", stored, err)
	}
	if _, ok := h.authStore.Authenticate("ibrahim-old"); ok {
		t.Fatal("old key must stop authenticating after a lowercase-path update")
	}
	if _, ok := h.authStore.Authenticate("ibrahim-new"); !ok {
		t.Fatal("new key must authenticate after a lowercase-path update")
	}

	// 小写路径删除：落库删除，auth 投影同样必须命中并撤权。
	rec = doClientWrite(t, h, http.MethodDelete, "clients/ibrahim", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	if stored, err := stores.clientStore.Get("İbrahim"); err != nil || stored != nil {
		t.Fatalf("delete must be persisted: %+v err=%v", stored, err)
	}
	if _, ok := h.authStore.Authenticate("ibrahim-new"); ok {
		t.Fatal("deleted key must stop authenticating after a lowercase-path delete")
	}
}

// 回归（v1.11.2）：4xx 判定必须基于哨兵错误，不能看错误文案。客户端名可以
// 合法包含 "duplicate access_key"，旧实现会把这类名字上的空 key / 非法 IP
// 校验失败误判成 409。
func TestClientWriteErrorTypingIgnoresNameLiterals(t *testing.T) {
	h, stores := newClientAuthTestHandler(t, true)

	// name 含 "duplicate access_key"：空 key 仍是 400，而不是 409。
	rec := doClientWrite(t, h, http.MethodPost, "clients", `{"name":"duplicate access_key","access_key":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blank-key status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if stored, err := stores.clientStore.Get("duplicate access_key"); err != nil || stored != nil {
		t.Fatalf("rejected blank-key create must not persist: %+v err=%v", stored, err)
	}

	// 同名 + 非法 IP → 400。
	rec = doClientWrite(t, h, http.MethodPost, "clients", `{"name":"duplicate access_key","access_key":"dup-key","ip_address":"192.0.2.0/33"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid-IP status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}

	// 真实重复 access_key：create 与 update 两个路径都必须 409。
	rec = doClientWrite(t, h, http.MethodPost, "clients", `{"name":"bob","access_key":"k1"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate-key create status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	rec = doClientWrite(t, h, http.MethodPost, "clients", `{"name":"bob","access_key":"bob-key"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup bob status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	rec = doClientWrite(t, h, http.MethodPut, "clients/bob", `{"name":"bob","access_key":"k1"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate-key update status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if _, ok := h.authStore.Authenticate("bob-key"); !ok {
		t.Fatal("rejected update must keep the committed key authenticating")
	}
}

// [""] 边缘：投影必须与落库后的视图一致。store 的 normalizeClient 丢弃空白
// allowed_targets 条目，落库列表为空 → allowAll=true；投影在构建下一份 auth 表
// 前做同样的丢弃，保证运行时 auth 与“从 DB 重建”的表语义一致。
func TestCreateClientBlankAllowedTargetsMatchesStore(t *testing.T) {
	h, stores := newClientAuthTestHandler(t, true)

	rec := doClientWrite(t, h, http.MethodPost, "clients", `{"name":"blank-target","access_key":"blank-key","allowed_targets":[""]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	stored, err := stores.clientStore.Get("blank-target")
	if err != nil || stored == nil {
		t.Fatalf("client must be persisted: %+v err=%v", stored, err)
	}
	if len(stored.AllowedTargets) != 0 {
		t.Fatalf("store must drop blank allowed_targets, got %#v", stored.AllowedTargets)
	}
	live, ok := h.authStore.Authenticate("blank-key")
	if !ok {
		t.Fatal("created key must authenticate")
	}
	rebuilt, err := auth.NewStoreFromConfig([]config.Client{*stored}, nil)
	if err != nil {
		t.Fatalf("rebuild auth from persisted client: %v", err)
	}
	persisted, ok := rebuilt.Authenticate("blank-key")
	if !ok {
		t.Fatal("rebuilt store must authenticate the key")
	}
	if live.AllowAll() != persisted.AllowAll() {
		t.Fatalf("allowAll drift: live=%v persisted=%v", live.AllowAll(), persisted.AllowAll())
	}
	if !live.AllowAll() {
		t.Fatal("blank allowed_targets stored as empty list must be allowAll, matching the store view")
	}
}

// Update 支持改名（store 在 newKey != oldKey 时迁移记录）：投影按路径名命中后
// 用请求体替换，改名后的 principal 与 key 必须一并发布。
func TestUpdateClientRenameReplacesPrincipal(t *testing.T) {
	h, stores := newClientAuthTestHandler(t, true)

	rec := doClientWrite(t, h, http.MethodPost, "clients", `{"name":"old-name","access_key":"old-key"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	rec = doClientWrite(t, h, http.MethodPut, "clients/old-name", `{"name":"new-name","access_key":"new-key"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if _, ok := h.authStore.Authenticate("old-key"); ok {
		t.Fatal("old key must stop authenticating after rename")
	}
	principal, ok := h.authStore.Authenticate("new-key")
	if !ok {
		t.Fatal("new key must authenticate after rename")
	}
	if principal.Name != "new-name" {
		t.Fatalf("principal name = %q, want new-name", principal.Name)
	}
	if stored, err := stores.clientStore.Get("new-name"); err != nil || stored == nil || stored.AccessKey != "new-key" {
		t.Fatalf("rename must be persisted under the new name: %+v err=%v", stored, err)
	}
	if stored, err := stores.clientStore.Get("old-name"); err != nil || stored != nil {
		t.Fatalf("old name must be gone after rename: %+v err=%v", stored, err)
	}
}
