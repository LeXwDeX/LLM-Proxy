package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
)

func (h *Handler) handleListClients(w http.ResponseWriter, r *http.Request) {
	clients, err := h.clientStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list clients", err)
		return
	}

	// Plaintext access keys are admin-only; other roles (and sessionless
	// requests) get the same suffix-only mask used by audit logs.
	if !isAdminSession(r) {
		for i := range clients {
			clients[i].AccessKey = auth.MaskClientKey(clients[i].AccessKey)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"clients": clients,
		"count":   len(clients),
	})
}

// 客户端写入与目标/部署池写入共用 configMu：commitConfigChange 在锁内读取 clients 并应用 auth，
// 客户端写入若不持锁，会被其落库后的 auth 应用以旧 clients 列表覆盖。锁内的客户端写路径为
// “读取 → 投影 → 在独立 Store 上构建 → 落库 → Swap”：构建失败不落库，落库成功后 Swap 不可失败，
// 因此不存在“已落库但 auth 未应用”的窗口。配额评估（可能较慢）在释放 configMu 之后执行，
// 不阻塞目标保存。

// evaluateClientQuota 触发该客户端的配额评估（配置可能已变化）。调用方不得持有 configMu。
func (h *Handler) evaluateClientQuota(name string) {
	if h.clientQuotaEvaluateHook != nil {
		h.clientQuotaEvaluateHook(name)
	}
	if h.quotaManager != nil {
		h.quotaManager.NoteConfigChange()
		h.quotaManager.Evaluate(name)
	}
}

// clientChangeError 标记“本次请求的投影无法构建 auth 表”的校验类失败（空 access_key、
// 非法 IP、重复 access_key 等）。它与读取失败、现有数据无法构建等内部故障区分：
// 后者是 500，前者沿用 store 校验错误的 4xx 语义。
type clientChangeError struct{ err error }

func (e *clientChangeError) Error() string { return e.err.Error() }
func (e *clientChangeError) Unwrap() error { return e.err }

// buildNextAuthStore 在落库前为客户端写路径构建下一份 auth 表（调用方持有 configMu）。
// 它只读当前 clients/targets 并构建独立 Store，不触碰运行时 auth。任何读取或构建失败
// 都返回错误：调用方必须中止本次写入，数据库与运行时 auth 均保持旧状态。
func (h *Handler) buildNextAuthStore(project func([]config.Client) []config.Client) (*auth.Store, error) {
	if h.authStore == nil {
		return nil, errors.New("auth store unavailable")
	}
	clients, err := h.clientStore.List()
	if err != nil {
		return nil, fmt.Errorf("load clients: %w", err)
	}
	var targets []config.Target
	if h.targetStore != nil {
		targets, err = h.targetStore.List()
		if err != nil {
			return nil, fmt.Errorf("load targets: %w", err)
		}
	}
	next, err := auth.NewStoreFromConfig(project(clients), targets)
	if err != nil {
		// 现有列表本身可构建 → 失败来自本次请求，按 store 校验错误返回 4xx；
		// 现有数据也无法构建才是内部故障。
		if _, baseErr := auth.NewStoreFromConfig(clients, targets); baseErr == nil {
			return nil, &clientChangeError{err: err}
		}
		return nil, err
	}
	return next, nil
}

// writeClientChangeError 响应落库前的读取/构建失败：请求校验失败沿用 store 的
// 4xx 语义（重复 access_key → 409，其余 → 400），内部故障 → 500。
func (h *Handler) writeClientChangeError(w http.ResponseWriter, err error) {
	var changeErr *clientChangeError
	if !errors.As(err, &changeErr) {
		h.writeInternalError(w, "failed to apply client change", err)
		return
	}
	status := http.StatusBadRequest
	if errors.Is(changeErr.err, auth.ErrDuplicateAccessKey) {
		status = http.StatusConflict
	}
	writeJSON(w, status, errorResponse(changeErr.Error()))
}

// authProjectionClient 返回与 clientStore 落库结果一致的 auth 相关视图：store 的
// normalizeClient 会丢弃空白 allowed_targets 条目，而 allowAll 按原始字段判空，
// 投影不能把这些条目算进去，否则新表与落库后的表在 [""] 边缘上不一致。
func authProjectionClient(client config.Client) config.Client {
	if len(client.AllowedTargets) == 0 {
		return client
	}
	targets := make([]string, 0, len(client.AllowedTargets))
	for _, target := range client.AllowedTargets {
		if strings.TrimSpace(target) != "" {
			targets = append(targets, target)
		}
	}
	client.AllowedTargets = targets
	return client
}

// projectCreatedClient 把新建请求追加到当前列表。
func projectCreatedClient(clients []config.Client, req config.Client) []config.Client {
	projected := make([]config.Client, 0, len(clients)+1)
	projected = append(projected, clients...)
	return append(projected, authProjectionClient(req))
}

// projectUpdatedClient 按 name 替换，找不到时保留原列表，由 store 报告 not found。
// 匹配键必须与 store 的 bucket 键完全一致（strings.ToLower(strings.TrimSpace(name))）；
// strings.EqualFold 在 U+0130（"İ"）上与 ToLower 语义不同，会导致落库成功而投影未命中。
func projectUpdatedClient(clients []config.Client, name string, req config.Client) []config.Client {
	projected := make([]config.Client, len(clients))
	copy(projected, clients)
	name = strings.ToLower(strings.TrimSpace(name))
	for i := range projected {
		if strings.ToLower(strings.TrimSpace(projected[i].Name)) == name {
			projected[i] = authProjectionClient(req)
			return projected
		}
	}
	return projected
}

// projectDeletedClient 从当前列表移除 name（匹配键与 store 的 bucket 键一致）。
func projectDeletedClient(clients []config.Client, name string) []config.Client {
	name = strings.ToLower(strings.TrimSpace(name))
	projected := make([]config.Client, 0, len(clients))
	for _, client := range clients {
		if strings.ToLower(strings.TrimSpace(client.Name)) != name {
			projected = append(projected, client)
		}
	}
	return projected
}

func (h *Handler) handleCreateClient(w http.ResponseWriter, r *http.Request) {
	req, ok := h.createClientLocked(w, r)
	if !ok {
		return
	}
	h.evaluateClientQuota(req.Name)
	h.recordAudit(r, "client_create", req.Name, "success", auth.MaskClientKey(req.AccessKey))

	writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
}

// createClientLocked 在 configMu 内完成校验、落库与 auth 重载；失败时已写好响应并返回 false。
func (h *Handler) createClientLocked(w http.ResponseWriter, r *http.Request) (config.Client, bool) {
	limitConfigBody(w, r)
	var req config.Client
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return req, false
	}
	h.configMu.Lock()
	defer h.configMu.Unlock()

	if err := h.validateClientPoolQuotas(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return req, false
	}

	next, err := h.buildNextAuthStore(func(clients []config.Client) []config.Client {
		return projectCreatedClient(clients, req)
	})
	if err != nil {
		h.writeClientChangeError(w, err)
		return req, false
	}

	if err := h.clientStore.Create(req); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return req, false
	}

	// 落库成功：发布已构建的表（不可失败），新 key/ACL 在响应返回前生效。
	h.authStore.Swap(next)
	return req, true
}

func (h *Handler) handleUpdateClient(w http.ResponseWriter, r *http.Request) {
	name, req, quotaDirty, ok := h.updateClientLocked(w, r)
	if !ok {
		return
	}
	// 客户端更新不在响应路径上同步 Evaluate（Issue #240）：Evaluate 要扫描该客户端约 31 天的
	// 用量窗口，对有额度组的客户端是秒级，而 PUT 载荷里能改变额度判定的字段只有 job_level
	// 与改名。其余字段（IP、ACL、access_key、项目组等）不触发任何额度工作；job_level 变化
	// 与改名交给后台去抖刷新，窗口内准入沿用已校准的旧计数器（与职级、模型组写入同一口径）。
	body := map[string]any{"status": "ok"}
	if quotaDirty && h.quotaManager != nil {
		names := []string{name}
		if req.Name != name {
			names = append(names, req.Name) // 改名：旧名清理内存状态，新名重新校准
		}
		h.quotaManager.ScheduleRefresh(quota.RefreshScope{Clients: names})
		body = h.withQuotaRefresh(body)
	}
	h.recordAudit(r, "client_update", name, "success", auth.MaskClientKey(req.AccessKey))

	writeJSON(w, http.StatusOK, body)
}

// clientQuotaInputsChanged 判断一次客户端更新是否改变了额度判定的输入：职级（额度上限的
// 系数来源与「未绑职级即拒绝」的客户端级门槛）或名称（计数器、额度计划按名称寻址）。
// 额度计划、模型组、职级系数都存放在各自的 store，不随客户端 PUT 变化。
func clientQuotaInputsChanged(before *config.Client, after config.Client) bool {
	if before == nil {
		return true // 读不到改前记录时无法证明没变化，按需要刷新处理
	}
	if strings.TrimSpace(before.JobLevel) != strings.TrimSpace(after.JobLevel) {
		return true
	}
	return strings.ToLower(strings.TrimSpace(before.Name)) != strings.ToLower(strings.TrimSpace(after.Name))
}

// updateClientLocked 返回的 quotaDirty 表示本次更新改变了额度判定输入，调用方须在释放
// configMu 之后登记后台额度刷新。
func (h *Handler) updateClientLocked(w http.ResponseWriter, r *http.Request) (name string, req config.Client, quotaDirty bool, ok bool) {
	limitConfigBody(w, r)
	name = pathParam(r, "name")
	if strings.TrimSpace(name) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing client name"))
		return name, req, false, false
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return name, req, false, false
	}
	h.configMu.Lock()
	defer h.configMu.Unlock()

	if err := h.validateClientPoolQuotas(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return name, req, false, false
	}

	next, err := h.buildNextAuthStore(func(clients []config.Client) []config.Client {
		return projectUpdatedClient(clients, name, req)
	})
	if err != nil {
		h.writeClientChangeError(w, err)
		return name, req, false, false
	}

	// 改前记录（单键读）：读取失败不阻止保存，只让本次保守地登记刷新。
	before, _ := h.clientStore.Get(name)

	if err := h.clientStore.Update(name, req); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return name, req, false, false
	}

	// 落库成功：发布已构建的表（不可失败），新 key/ACL 在响应返回前生效。
	h.authStore.Swap(next)
	return name, req, clientQuotaInputsChanged(before, req), true
}

func (h *Handler) handleDeleteClient(w http.ResponseWriter, r *http.Request) {
	name, ok := h.deleteClientLocked(w, r)
	if !ok {
		return
	}
	h.evaluateClientQuota(name)
	h.recordAudit(r, "client_delete", name, "success", "")

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteClientLocked(w http.ResponseWriter, r *http.Request) (string, bool) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	name := pathParam(r, "name")
	if strings.TrimSpace(name) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing client name"))
		return name, false
	}

	existing, err := h.clientStore.Get(name)
	if err != nil {
		h.writeInternalError(w, "failed to load client", err)
		return name, false
	}
	if existing == nil {
		writeJSON(w, http.StatusNotFound, errorResponse("client not found"))
		return name, false
	}

	next, err := h.buildNextAuthStore(func(clients []config.Client) []config.Client {
		return projectDeletedClient(clients, name)
	})
	if err != nil {
		h.writeClientChangeError(w, err)
		return name, false
	}

	if h.clientQuotaStore != nil {
		if err := h.clientQuotaStore.DeleteClientAndArchive(*existing); err != nil {
			h.writeInternalError(w, "failed to delete client", err)
			return name, false
		}
	} else if err := h.clientStore.Delete(name); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return name, false
	}

	// 落库成功：发布已构建的表（不可失败），被删 key 在响应返回前失效。
	h.authStore.Swap(next)
	return name, true
}

// validateClientPoolQuotas 校验 client.PoolQuotas：池引用必须存在（同
// allowed_pools 语义）、归一化池名；重复池名视为无效。
func (h *Handler) validateClientPoolQuotas(client *config.Client) error {
	if len(client.PoolQuotas) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(client.PoolQuotas))
	for i := range client.PoolQuotas {
		pq := &client.PoolQuotas[i]
		pq.Pool = strings.ToLower(strings.TrimSpace(pq.Pool))
		if pq.Pool == "" {
			return fmt.Errorf("pool_quotas[%d]: pool must not be empty", i)
		}
		if _, dup := seen[pq.Pool]; dup {
			return fmt.Errorf("pool_quotas: duplicate pool %q", pq.Pool)
		}
		seen[pq.Pool] = struct{}{}
		if err := h.validatePoolExists(pq.Pool); err != nil {
			return fmt.Errorf("pool_quotas[%d]: %w", i, err)
		}
	}
	return nil
}
