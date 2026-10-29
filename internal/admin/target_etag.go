package admin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/go-chi/chi/v5"
)

// 目标的内容 ETag 与稳定 key ID。
//
//   - ETag 是 targets bucket 中该目标存储字节的 SHA-256 前缀（nosql.TargetStore.ETag），
//     不依赖任何序列或写入点配合：存量目标、旧二进制改写、规范化回写都会使旧 ETag 失效。
//     PUT/DELETE/pause/resume 可带 If-Match 头，在写事务内重新计算并比较。
//   - key ID 由持久化密钥对"目标名 + key 值"做 HMAC 派生，与槽位下标、运行时代际无关；
//     `__existing_key_id__:<id>` 可在保存时引用已有 key，封禁路由也按它定位。
//     密钥不可用时 key_ids 为 JSON null，客户端不得提交 key 字段。
//
// 旧的 `__existing_key_index__` + X-Target-Key-Snapshot 路径保持不变。

const (
	existingTargetKeyIDRefPrefix = "__existing_key_id__:"
	targetRevisionConflictCode   = "target_revision_conflict" // 沿用旧 code，前端按它识别冲突
	keyIDDomain                  = "admin/target-key-id/v2\x00"
	keyIDBytes                   = 16
	authReloadPendingWarning     = "auth reload pending"
)

// writeTargetRevisionConflict 返回 409：目标已被改动、删除或不存在，调用方应刷新后重试。
func writeTargetRevisionConflict(w http.ResponseWriter) {
	writeJSON(w, http.StatusConflict, map[string]string{
		"error": "target changed; refresh targets",
		"code":  targetRevisionConflictCode,
	})
}

// ifMatch 是解析后的 If-Match 头。
type ifMatch struct {
	present  bool
	wildcard bool     // `*`：目标存在即匹配
	etags    []string // 非通配时可接受的 ETag 列表，任一项匹配即通过
}

func (im ifMatch) matches(current string) bool {
	if current == "" {
		return false
	}
	if im.wildcard {
		return true
	}
	for _, e := range im.etags {
		if e == current {
			return true
		}
	}
	return false
}

// parseIfMatch 解析可选的 If-Match 头：逗号分隔的列表，每项容忍引号与 W/ 前缀；
// `*` 表示目标存在即匹配。空项或畸形项（含非 t1- 形态）返回错误（400）。
func parseIfMatch(r *http.Request) (ifMatch, error) {
	values := r.Header.Values("If-Match")
	if len(values) == 0 {
		return ifMatch{}, nil
	}
	im := ifMatch{present: true}
	for _, raw := range strings.Split(strings.Join(values, ","), ",") {
		token := strings.TrimSpace(raw)
		if token == "*" {
			im.wildcard = true
			continue
		}
		token = strings.TrimPrefix(token, "W/")
		token = strings.Trim(token, `"`)
		if !strings.HasPrefix(token, "t1-") || len(token) <= len("t1-") {
			return ifMatch{present: true}, errors.New("invalid If-Match")
		}
		im.etags = append(im.etags, token)
	}
	return im, nil
}

// resolveIfMatch 在 configMu 内读取目标当前 ETag 并与 If-Match 比较；
// 返回传给 *IfMatch 存储调用的 ETag。不匹配（含目标不存在）时已写 409 并返回 false。
//
// 有意的纵深防御：此处的预检只用于快速失败，省去无谓的校验与落库准备；最终裁决由写事务内
// 对当前存储字节重新计算 ETag 完成（nosql.UpdateIfMatch/DeleteIfMatch），二者故意冗余。
// 前置条件失败沿用 409 + target_revision_conflict（未采用 412），前端按 code 识别。
func (h *Handler) resolveIfMatch(w http.ResponseWriter, name string, im ifMatch) (string, bool) {
	current, err := h.targetStore.ETag(name)
	if err != nil {
		h.writeInternalError(w, "failed to read target", err)
		return "", false
	}
	if !im.matches(current) {
		writeTargetRevisionConflict(w)
		return "", false
	}
	return current, true
}

// targetETag 读取目标当前 ETag（响应体回传用）；读取失败返回空串。
func (h *Handler) targetETag(name string) string {
	if h.targetStore == nil {
		return ""
	}
	etag, err := h.targetStore.ETag(name)
	if err != nil {
		h.logger.Error("read target etag", "target", name, "error", err)
		return ""
	}
	return etag
}

// keyIDSecret 返回 key ID 派生密钥（首次读取后缓存）；不可用时返回 nil。
func (h *Handler) keyIDSecret() []byte {
	h.keyIDMu.Lock()
	defer h.keyIDMu.Unlock()
	if h.keyIDSecretCache != nil {
		return h.keyIDSecretCache
	}
	load := h.keyIDSecretLoader
	if load == nil && h.targetStore != nil {
		load = h.targetStore.KeyIDSecret
	}
	if load == nil {
		return nil
	}
	secret, err := load()
	if err != nil || len(secret) == 0 {
		h.logger.Error("unable to load target key id secret", "error", err)
		return nil
	}
	h.keyIDSecretCache = secret
	return secret
}

// targetKeyIDs 与 key_runtime_indices 同形：长度 1+len(api_keys)，下标 0 是主 key，空槽为 nil。
// 同一目标内重复的 key 值按出现顺序追加 ".1"、".2"，首个无后缀。
// 密钥不可用时返回 nil（JSON null）：调用方据此判定 key 列表不可编辑。
func targetKeyIDs(secret []byte, t config.Target) []*string {
	if len(secret) == 0 {
		return nil
	}
	keys := existingTargetKeys(t)
	ids := make([]*string, len(keys))
	scope := strings.ToLower(strings.TrimSpace(t.Name))
	seen := make(map[string]int, len(keys))
	for i, key := range keys {
		if key == "" {
			continue
		}
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(keyIDDomain))
		mac.Write([]byte(scope))
		mac.Write([]byte{0})
		mac.Write([]byte(key))
		id := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:keyIDBytes])
		if n := seen[key]; n > 0 {
			id += "." + strconv.Itoa(n)
		}
		seen[key]++
		ids[i] = &id
	}
	return ids
}

// resolveKeyID 把 key ID 解析为 key 值及其在存储中的出现序号。
func resolveKeyID(secret []byte, t config.Target, id string) (key string, occurrence int, ok bool) {
	if id == "" {
		return "", 0, false
	}
	keys := existingTargetKeys(t)
	seen := make(map[string]int, len(keys))
	for i, candidate := range targetKeyIDs(secret, t) {
		if candidate == nil {
			continue
		}
		k := keys[i]
		if *candidate == id {
			return k, seen[k], true
		}
		seen[k]++
	}
	return "", 0, false
}

// resolveSubmittedTargetKeyRef 在旧的下标/掩码解析之上增加 `__existing_key_id__:` 引用。
func resolveSubmittedTargetKeyRef(submitted string, existing []string, allowEmptyRef bool, secret []byte, t config.Target) (string, error) {
	trimmed := strings.TrimSpace(submitted)
	if strings.HasPrefix(trimmed, existingTargetKeyIDRefPrefix) {
		key, _, ok := resolveKeyID(secret, t, strings.TrimPrefix(trimmed, existingTargetKeyIDRefPrefix))
		if !ok {
			return "", errors.New("invalid existing key reference")
		}
		return key, nil
	}
	return resolveSubmittedTargetKey(submitted, existing, allowEmptyRef)
}

// commitConfigChange 是目标与部署池全部写 handler 共用的提交管道：先落库后应用。
//
//  1. 校验：proxy.PrepareConfig（纯计算）+ 临时 auth store 加载，沿用 reload 的做法；
//  2. 落库：persist（带 CAS 时由调用方使用 *IfMatch）；失败则运行时完全不变；
//  3. 应用：proxy.CommitPrepared（不返回错误）→ authStore → configManager → costCache。
//
// 落库成功之后不再有能改变响应的失败：auth 应用失败只记 Error 日志并返回 warning
// （数据已保存，报 500 会让用户误以为没存上）。调用方必须持有 h.configMu；所有运行时变更
// （admin 写入、reload、客户端写入）都在该锁下，因此校验到应用之间配置与 clients 都不会被改动。
// 成功返回 (warning, true)；失败时已写好响应。
func (h *Handler) commitConfigChange(w http.ResponseWriter, cfg *config.Config, persistMsg string, persist func() error) (string, bool) {
	clients, err := h.clientStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to apply config", fmt.Errorf("load clients: %w", err))
		return "", false
	}
	prepared, err := h.proxyService.PrepareConfig(cfg)
	if err != nil {
		h.writeInternalError(w, "failed to apply config", fmt.Errorf("prepare proxy config: %w", err))
		return "", false
	}
	// allowed_pools 按当前 targets 的池归属展开成成员目标集，先在临时 store 上验证。
	if err := auth.NewStore().LoadFromConfigWithPools(clients, cfg.Targets); err != nil {
		h.writeInternalError(w, "failed to apply config", fmt.Errorf("validate auth config: %w", err))
		return "", false
	}

	if err := persist(); err != nil {
		if errors.Is(err, nosql.ErrTargetETagConflict) {
			writeTargetRevisionConflict(w)
			return "", false
		}
		h.writeInternalError(w, persistMsg, err)
		return "", false
	}

	warning := ""
	h.proxyService.CommitPrepared(prepared)
	applyAuth := h.authApply
	if applyAuth == nil {
		applyAuth = h.authStore.LoadFromConfigWithPools
	}
	if err := applyAuth(clients, cfg.Targets); err != nil {
		// 校验已在临时 store 上通过，这里理论上不会失败。库与运行时配置已更新，
		// 仅 auth 展开未刷新；下一次 reload 会收敛。响应保持成功，附 warning。
		h.logger.Error("config committed but auth apply failed; reload to converge", "error", err)
		warning = authReloadPendingWarning
	}
	h.configManager.Replace(cfg)
	if h.costCache != nil {
		h.costCache.Invalidate()
	}
	return warning, true
}

// withWarning 在成功响应体上附加 commitConfigChange 返回的 warning。
func withWarning(body map[string]any, warning string) map[string]any {
	if warning != "" {
		body["warning"] = warning
	}
	return body
}

func (h *Handler) handleBlockTargetKeyByID(w http.ResponseWriter, r *http.Request) {
	h.handleSetTargetKeyBlockedByID(w, r, true)
}

func (h *Handler) handleUnblockTargetKeyByID(w http.ResponseWriter, r *http.Request) {
	h.handleSetTargetKeyBlockedByID(w, r, false)
}

// handleSetTargetKeyBlockedByID 按稳定 key ID 封禁/解封。它不需要也不检查任何 ETag，
// 封禁只改运行时密钥状态，不改变目标配置，因此目标 ETag 不变。
func (h *Handler) handleSetTargetKeyBlockedByID(w http.ResponseWriter, r *http.Request, blocked bool) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	name, err := targetNameParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	keyID := pathParam(r, "key_id")
	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}
	var target *config.Target
	for i := range cfg.Targets {
		if strings.EqualFold(cfg.Targets[i].Name, name) {
			target = &cfg.Targets[i]
			break
		}
	}
	if target == nil {
		writeJSON(w, http.StatusNotFound, errorResponse(fmt.Sprintf("target %q not found", name)))
		return
	}
	key, _, ok := resolveKeyID(h.keyIDSecret(), *target, keyID)
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse("unknown key id"))
		return
	}
	// 运行时密钥池按值去重（同值的存储重复项共用一个条目），所以无论 ID 的出现序号
	// 是几，都落在该值的第 0 个运行时条目上。
	if err := h.proxyService.SetKeyBlockedByValue(target.Name, key, 0, blocked); err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse(err.Error()))
		return
	}

	action := "unblock_target_key"
	if blocked {
		action = "block_target_key"
	}
	h.recordAudit(r, action, name, "success", "key_id="+chi.URLParam(r, "key_id"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
