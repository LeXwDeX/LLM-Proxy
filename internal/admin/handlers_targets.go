package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ===== Target CRUD =====

func targetNameParam(r *http.Request) (string, error) {
	name, err := url.PathUnescape(chi.URLParam(r, "name"))
	if err != nil {
		return "", errors.New("invalid target name")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name must not be empty")
	}
	return name, nil
}

func (h *Handler) handleListTargets(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}

	targets := make([]map[string]any, len(cfg.Targets))
	for i, t := range cfg.Targets {
		epType := config.NormalizeEndpointType(t.EndpointType)

		// Mask API keys for security (only return masked versions)
		maskedAPIKey := ""
		if t.APIKey != "" {
			maskedAPIKey = maskKey(t.APIKey)
		}
		maskedAPIKeys := make([]string, len(t.APIKeys))
		for j, k := range t.APIKeys {
			maskedAPIKeys[j] = maskKey(k)
		}

		m := map[string]any{
			"name":                            t.Name,
			"endpoint_type":                   epType,
			"endpoint":                        t.Endpoint,
			"has_api_key":                     t.APIKey != "",
			"api_key":                         maskedAPIKey,
			"api_keys":                        maskedAPIKeys,
			"api_key_count":                   len(t.APIKeys),
			"key_reset_time":                  t.KeyResetTime,
			"provider_class":                  t.ProviderClass,
			"usage_adapter":                   config.NormalizeUsageAdapter(t.UsageAdapter),
			"paused":                          t.Paused,
			"allow_bearer_passthrough":        t.AllowBearer,
			"auth_mode":                       t.AuthMode,
			"image_operation":                 t.ImageOperation,
			"allowed_models":                  nil, // kept for backward compat, always nil
			"model_mappings":                  t.ModelMappings,
			"model_prefix":                    t.ModelPrefix,
			"openai_prefix":                   t.OpenAIPrefix,
			"anthropic_prefix":                t.AnthropicPrefix,
			"supports_responses":              t.SupportsResponses,
			"request_stream_usage_if_missing": t.RequestStreamUsageIfMissing,
			"prefer_websocket":                t.PreferWebSocket,
			"custom_headers":                  t.CustomHeaders,
			"custom_body":                     t.CustomBody,
			"pool":                            t.Pool,
			"max_concurrency":                 t.MaxConcurrency,
		}
		// 附加 key 池运行时状态
		if statuses := h.proxyService.KeyPoolStatus(t.Name); statuses != nil {
			m["key_statuses"] = statuses
		}
		targets[i] = m
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"targets": targets,
		"count":   len(targets),
	})
}

func (h *Handler) handleCreateTarget(w http.ResponseWriter, r *http.Request) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	var body struct {
		Name                        string                `json:"name"`
		EndpointType                string                `json:"endpoint_type"`
		UsageAdapter                string                `json:"usage_adapter"`
		Endpoint                    string                `json:"endpoint"`
		APIKey                      string                `json:"api_key"`
		APIKeys                     []string              `json:"api_keys"`
		KeyResetTime                string                `json:"key_reset_time"`
		Paused                      bool                  `json:"paused"`
		AllowBearer                 bool                  `json:"allow_bearer_passthrough"`
		AuthMode                    string                `json:"auth_mode"`
		ImageOperation              string                `json:"image_operation"`
		ModelMappings               []config.ModelMapping `json:"model_mappings"`
		ModelPrefix                 string                `json:"model_prefix"`
		OpenAIPrefix                string                `json:"openai_prefix"`
		AnthropicPrefix             string                `json:"anthropic_prefix"`
		SupportsResponses           bool                  `json:"supports_responses"`
		RequestStreamUsageIfMissing bool                  `json:"request_stream_usage_if_missing"`
		PreferWebSocket             bool                  `json:"prefer_websocket"`
		CustomHeaders               map[string]string     `json:"custom_headers"`
		CustomBody                  map[string]any        `json:"custom_body"`
		Pool                        string                `json:"pool"`
		Group                       string                `json:"group"`
		MaxConcurrency              int                   `json:"max_concurrency"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid request body"))
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("name must not be empty"))
		return
	}
	if !config.IsValidUsageAdapter(body.UsageAdapter) {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid usage_adapter"))
		return
	}
	body.AuthMode = strings.ToLower(strings.TrimSpace(body.AuthMode))
	if !config.IsValidAuthMode(body.AuthMode) {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid auth_mode"))
		return
	}
	if !config.IsValidImageOperation(config.NormalizeImageOperation(body.ImageOperation)) {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid image_operation"))
		return
	}
	epType := config.NormalizeEndpointType(body.EndpointType)
	if !config.IsValidEndpointType(epType) {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid endpoint_type"))
		return
	}
	endpoint := strings.TrimSpace(body.Endpoint)
	if endpoint == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("endpoint must not be empty"))
		return
	}
	apiKey := strings.TrimSpace(body.APIKey)
	if apiKey == "" && !body.AllowBearer {
		writeJSON(w, http.StatusBadRequest, errorResponse("api_key must not be empty when allow_bearer_passthrough is false"))
		return
	}
	pool := strings.ToLower(strings.TrimSpace(body.Pool))
	if pool != "" {
		if err := h.validatePoolExists(pool); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
			return
		}
	}
	maxConcurrency := body.MaxConcurrency
	if maxConcurrency < 0 {
		maxConcurrency = 0
	}

	if err := config.ValidateModelPrefix(body.ModelPrefix); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	if err := validateModelMappings(body.ModelMappings); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	if blocked := proxy.ValidateCustomHeaders(body.CustomHeaders); blocked != "" {
		writeJSON(w, http.StatusBadRequest, errorResponse(fmt.Sprintf("custom_headers contains blocked key %q", blocked)))
		return
	}
	if blocked := proxy.ValidateCustomBody(body.CustomBody); blocked != "" {
		writeJSON(w, http.StatusBadRequest, errorResponse(fmt.Sprintf("custom_body contains blocked field %q", blocked)))
		return
	}

	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}

	for _, t := range cfg.Targets {
		if strings.EqualFold(t.Name, name) {
			writeJSON(w, http.StatusConflict, errorResponse(fmt.Sprintf("target %q already exists", name)))
			return
		}
	}

	newTarget := config.Target{
		Name:                        name,
		EndpointType:                epType,
		UsageAdapter:                config.NormalizeUsageAdapter(body.UsageAdapter),
		Endpoint:                    endpoint,
		APIKey:                      apiKey,
		APIKeys:                     body.APIKeys,
		KeyResetTime:                body.KeyResetTime,
		Paused:                      body.Paused,
		AllowBearer:                 body.AllowBearer,
		AuthMode:                    body.AuthMode,
		ImageOperation:              body.ImageOperation,
		ModelMappings:               body.ModelMappings,
		ModelPrefix:                 body.ModelPrefix,
		OpenAIPrefix:                body.OpenAIPrefix,
		AnthropicPrefix:             body.AnthropicPrefix,
		SupportsResponses:           body.SupportsResponses,
		RequestStreamUsageIfMissing: body.RequestStreamUsageIfMissing,
		// prefer_websocket 仅对 canonical openai 有意义；其余类型一律归一化为 false。
		PreferWebSocket: body.PreferWebSocket && epType == config.EndpointTypeOpenAI,
		CustomHeaders:   body.CustomHeaders,
		CustomBody:      body.CustomBody,
		Pool:            pool,
		MaxConcurrency:  maxConcurrency,
	}
	// 与配置加载同一口径：以请求中的原始 endpoint_type 套用旧别名的厂商默认值
	// （如 deepseek/bailian 的前缀），显式填写的前缀不覆盖（#157 F1）。
	newTarget.EndpointType = body.EndpointType
	newTarget = config.NormalizeTargetForCompatibility(newTarget)
	cfg.Targets = append(cfg.Targets, newTarget)

	if err := h.applyConfigRuntime(cfg); err != nil {
		h.writeInternalError(w, "failed to apply config", err)
		return
	}

	if err := h.targetStore.Create(newTarget); err != nil {
		h.writeInternalError(w, "failed to persist target", err)
		return
	}
	h.configManager.Replace(cfg)
	if h.costCache != nil {
		h.costCache.Invalidate()
	}

	h.recordAudit(r, "create_target", name, "success", "")
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "name": name})
}

func (h *Handler) handleUpdateTarget(w http.ResponseWriter, r *http.Request) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	name, err := targetNameParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	var body struct {
		EndpointType                string                 `json:"endpoint_type"`
		UsageAdapter                json.RawMessage        `json:"usage_adapter"`
		Endpoint                    string                 `json:"endpoint"`
		APIKey                      *string                `json:"api_key"`
		APIKeys                     *[]string              `json:"api_keys"`
		KeyResetTime                *string                `json:"key_reset_time"`
		Paused                      *bool                  `json:"paused"`
		AllowBearer                 bool                   `json:"allow_bearer_passthrough"`
		AuthMode                    *string                `json:"auth_mode"`
		ImageOperation              *string                `json:"image_operation"`
		ModelMappings               *[]config.ModelMapping `json:"model_mappings"`
		ModelPrefix                 *string                `json:"model_prefix"`
		OpenAIPrefix                *string                `json:"openai_prefix"`
		AnthropicPrefix             *string                `json:"anthropic_prefix"`
		SupportsResponses           *bool                  `json:"supports_responses"`
		RequestStreamUsageIfMissing *bool                  `json:"request_stream_usage_if_missing"`
		PreferWebSocket             *bool                  `json:"prefer_websocket"`
		CustomHeaders               *map[string]string     `json:"custom_headers"`
		CustomBody                  *map[string]any        `json:"custom_body"`
		Pool                        *string                `json:"pool"`
		Group                       *string                `json:"group"`
		MaxConcurrency              *int                   `json:"max_concurrency"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid request body"))
		return
	}

	var usageAdapter string
	if body.UsageAdapter != nil {
		if string(body.UsageAdapter) == "null" || json.Unmarshal(body.UsageAdapter, &usageAdapter) != nil || !config.IsValidUsageAdapter(usageAdapter) {
			writeJSON(w, http.StatusBadRequest, errorResponse("invalid usage_adapter"))
			return
		}
	}
	if body.AuthMode != nil && !config.IsValidAuthMode(strings.ToLower(strings.TrimSpace(*body.AuthMode))) {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid auth_mode"))
		return
	}
	if body.ImageOperation != nil && !config.IsValidImageOperation(config.NormalizeImageOperation(*body.ImageOperation)) {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid image_operation"))
		return
	}
	if body.ModelPrefix != nil {
		if err := config.ValidateModelPrefix(*body.ModelPrefix); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
			return
		}
	}
	if body.ModelMappings != nil {
		if err := validateModelMappings(*body.ModelMappings); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
			return
		}
	}

	if body.CustomHeaders != nil {
		if blocked := proxy.ValidateCustomHeaders(*body.CustomHeaders); blocked != "" {
			writeJSON(w, http.StatusBadRequest, errorResponse(fmt.Sprintf("custom_headers contains blocked key %q", blocked)))
			return
		}
	}
	if body.CustomBody != nil {
		if blocked := proxy.ValidateCustomBody(*body.CustomBody); blocked != "" {
			writeJSON(w, http.StatusBadRequest, errorResponse(fmt.Sprintf("custom_body contains blocked field %q", blocked)))
			return
		}
	}

	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}

	found := false
	for i := range cfg.Targets {
		if strings.EqualFold(cfg.Targets[i].Name, name) {
			t := &cfg.Targets[i]
			existingKeys := existingTargetKeys(*t)
			if body.EndpointType != "" {
				epType := config.NormalizeEndpointType(body.EndpointType)
				if !config.IsValidEndpointType(epType) {
					writeJSON(w, http.StatusBadRequest, errorResponse("invalid endpoint_type"))
					return
				}
				t.EndpointType = epType
			}
			if body.Endpoint != "" {
				t.Endpoint = strings.TrimSpace(body.Endpoint)
			}
			if body.APIKey != nil {
				resolved, err := resolveSubmittedTargetKey(*body.APIKey, existingKeys)
				if err != nil {
					writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
					return
				}
				t.APIKey = resolved
			}
			if body.UsageAdapter != nil {
				t.UsageAdapter = config.NormalizeUsageAdapter(usageAdapter)
			}
			t.AllowBearer = body.AllowBearer
			if body.ModelPrefix != nil {
				t.ModelPrefix = *body.ModelPrefix
			}
			if body.ModelMappings != nil {
				t.ModelMappings = *body.ModelMappings
			}
			if body.AuthMode != nil {
				t.AuthMode = strings.ToLower(strings.TrimSpace(*body.AuthMode))
			}
			if body.ImageOperation != nil {
				t.ImageOperation = config.NormalizeImageOperation(*body.ImageOperation)
			}
			if body.APIKeys != nil {
				resolvedKeys := make([]string, 0, len(*body.APIKeys))
				for _, k := range *body.APIKeys {
					resolved, err := resolveSubmittedTargetKey(k, existingKeys)
					if err != nil {
						writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
						return
					}
					if resolved != "" {
						resolvedKeys = append(resolvedKeys, resolved)
					}
				}
				t.APIKeys = resolvedKeys
			}
			if body.KeyResetTime != nil {
				t.KeyResetTime = *body.KeyResetTime
			}
			if body.Paused != nil {
				t.Paused = *body.Paused
			}
			if body.OpenAIPrefix != nil {
				t.OpenAIPrefix = *body.OpenAIPrefix
			}
			if body.AnthropicPrefix != nil {
				t.AnthropicPrefix = *body.AnthropicPrefix
			}
			if body.SupportsResponses != nil {
				t.SupportsResponses = *body.SupportsResponses
			}
			if body.RequestStreamUsageIfMissing != nil {
				t.RequestStreamUsageIfMissing = *body.RequestStreamUsageIfMissing
			}
			if body.PreferWebSocket != nil {
				t.PreferWebSocket = *body.PreferWebSocket
			}
			if body.EndpointType != "" {
				// 与配置加载同一口径：按原始 endpoint_type 补齐旧别名的厂商默认值；
				// 已存储或本次显式填写的非空前缀不覆盖（#157 F1）。
				t.EndpointType = body.EndpointType
				*t = config.NormalizeTargetForCompatibility(*t)
			}
			// prefer_websocket 仅对 canonical openai 有效；其余类型（含本次切换目标类型后）
			// 一律清除，避免残留的 WS 优先标记落到不支持 WebSocket 的上游。
			if t.EndpointType != config.EndpointTypeOpenAI {
				t.PreferWebSocket = false
			}
			if body.CustomHeaders != nil {
				t.CustomHeaders = *body.CustomHeaders
			}
			if body.CustomBody != nil {
				t.CustomBody = *body.CustomBody
			}
			if body.Pool != nil {
				nextPool := strings.ToLower(strings.TrimSpace(*body.Pool))
				if nextPool != "" {
					if err := h.validatePoolExists(nextPool); err != nil {
						writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
						return
					}
				}
				t.Pool = nextPool
			}
			if body.MaxConcurrency != nil {
				mc := *body.MaxConcurrency
				if mc < 0 {
					mc = 0
				}
				t.MaxConcurrency = mc
			}

			// Validate: api_key must be non-empty when allow_bearer is false.
			if t.APIKey == "" && !t.AllowBearer {
				writeJSON(w, http.StatusBadRequest, errorResponse("api_key must not be empty when allow_bearer_passthrough is false"))
				return
			}
			found = true
			break
		}
	}

	if !found {
		writeJSON(w, http.StatusNotFound, errorResponse(fmt.Sprintf("target %q not found", name)))
		return
	}

	if err := h.applyConfigRuntime(cfg); err != nil {
		h.writeInternalError(w, "failed to apply config", err)
		return
	}

	var updatedTarget config.Target
	for _, target := range cfg.Targets {
		if strings.EqualFold(target.Name, name) {
			updatedTarget = target
			break
		}
	}
	if err := h.targetStore.Update(name, updatedTarget); err != nil {
		h.writeInternalError(w, "failed to persist target", err)
		return
	}
	h.configManager.Replace(cfg)
	if h.costCache != nil {
		h.costCache.Invalidate()
	}

	h.recordAudit(r, "update_target", name, "success", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

const existingTargetKeyRefPrefix = "__existing_key_index__:"

func existingTargetKeys(t config.Target) []string {
	keys := make([]string, 0, 1+len(t.APIKeys))
	keys = append(keys, t.APIKey)
	keys = append(keys, t.APIKeys...)
	return keys
}

func resolveSubmittedTargetKey(submitted string, existing []string) (string, error) {
	trimmed := strings.TrimSpace(submitted)
	if trimmed == "" {
		return "", nil
	}
	if strings.HasPrefix(trimmed, existingTargetKeyRefPrefix) {
		rawIndex := strings.TrimPrefix(trimmed, existingTargetKeyRefPrefix)
		idx, err := strconv.Atoi(rawIndex)
		if err != nil || idx < 0 || idx >= len(existing) || existing[idx] == "" {
			return "", fmt.Errorf("invalid existing key reference")
		}
		return existing[idx], nil
	}

	matched := ""
	for _, key := range existing {
		if key != "" && trimmed == maskKey(key) {
			if matched != "" {
				return "", fmt.Errorf("ambiguous masked api key")
			}
			matched = key
		}
	}
	if matched != "" {
		return matched, nil
	}
	if looksLikeMaskedTargetKey(trimmed) {
		return "", fmt.Errorf("unrecognized masked api key")
	}
	return trimmed, nil
}

func looksLikeMaskedTargetKey(value string) bool {
	return value == "****" || strings.Contains(value, "...")
}

func (h *Handler) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	name, err := targetNameParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}

	next := make([]config.Target, 0, len(cfg.Targets))
	found := false
	for _, t := range cfg.Targets {
		if strings.EqualFold(t.Name, name) {
			found = true
			continue
		}
		next = append(next, t)
	}

	if !found {
		writeJSON(w, http.StatusNotFound, errorResponse(fmt.Sprintf("target %q not found", name)))
		return
	}

	cfg.Targets = next

	if err := h.applyConfigRuntime(cfg); err != nil {
		h.writeInternalError(w, "failed to apply config", err)
		return
	}

	if err := h.targetStore.Delete(name); err != nil {
		h.writeInternalError(w, "failed to persist target deletion", err)
		return
	}
	h.configManager.Replace(cfg)
	if h.costCache != nil {
		h.costCache.Invalidate()
	}

	// Target deletion invalidates model-group bindings that reference it:
	// refresh in-memory quota counters so personnel quota and group states
	// reflect the current catalog (stale bindings are kept and flagged in UI).
	if h.quotaManager != nil {
		h.quotaManager.RefreshAll()
	}

	h.recordAudit(r, "delete_target", name, "success", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handlePauseTarget(w http.ResponseWriter, r *http.Request) {
	h.handleSetTargetPaused(w, r, true)
}

func (h *Handler) handleResumeTarget(w http.ResponseWriter, r *http.Request) {
	h.handleSetTargetPaused(w, r, false)
}

// handleTestModel 用指定 target 的上游凭证对给定 model 发起最小请求，
// 验证上游通路。body: {"model": "..."}.
//
// 错误码说明：TestModel 的 error 语义多样（target 不存在、model 为空、endpoint_type 不支持），
// 当前统一返回 400 Bad Request。上游真实错误（如 4xx/5xx）走 result.Error 字段在 200 响应里
// 返回，UI 可直接展示，不区分 status code。如需进一步细分可改为 errors.Is 判断。
func (h *Handler) handleTestModel(w http.ResponseWriter, r *http.Request) {
	name, err := targetNameParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid request body"))
		return
	}
	result, err := h.proxyService.TestModel(name, body.Model)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) handleSetTargetPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	name, err := targetNameParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}

	found := false
	var updatedTarget config.Target
	for i := range cfg.Targets {
		if strings.EqualFold(cfg.Targets[i].Name, name) {
			cfg.Targets[i].Paused = paused
			updatedTarget = cfg.Targets[i]
			found = true
			break
		}
	}
	if !found {
		writeJSON(w, http.StatusNotFound, errorResponse(fmt.Sprintf("target %q not found", name)))
		return
	}

	if err := h.applyConfigRuntime(cfg); err != nil {
		h.writeInternalError(w, "failed to apply config", err)
		return
	}
	if err := h.targetStore.Update(name, updatedTarget); err != nil {
		h.writeInternalError(w, "failed to persist target", err)
		return
	}
	h.configManager.Replace(cfg)

	action := "resume_target"
	if paused {
		action = "pause_target"
	}
	h.recordAudit(r, action, name, "success", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": updatedTarget.Name, "paused": paused})
}

func (h *Handler) handleBlockTargetKey(w http.ResponseWriter, r *http.Request) {
	name, nameErr := targetNameParam(r)
	indexStr := chi.URLParam(r, "index")
	if nameErr != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(nameErr.Error()))
		return
	}
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid key index"))
		return
	}

	if err := h.proxyService.BlockKey(name, index); err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse(err.Error()))
		return
	}

	h.recordAudit(r, "block_target_key", name, "success", fmt.Sprintf("key_index=%d", index))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleUnblockTargetKey(w http.ResponseWriter, r *http.Request) {
	name, nameErr := targetNameParam(r)
	indexStr := chi.URLParam(r, "index")
	if nameErr != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(nameErr.Error()))
		return
	}
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid key index"))
		return
	}

	if err := h.proxyService.UnblockKey(name, index); err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse(err.Error()))
		return
	}

	h.recordAudit(r, "unblock_target_key", name, "success", fmt.Sprintf("key_index=%d", index))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleWakeUpTargetKeys(w http.ResponseWriter, r *http.Request) {
	name, err := targetNameParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	recovered, err := h.proxyService.WakeUpKeys(name)
	if err != nil {
		// "not found" → 404；"already in progress / cooldown" → 409
		code := http.StatusConflict
		if strings.Contains(err.Error(), "not found") {
			code = http.StatusNotFound
		}
		writeJSON(w, code, errorResponse(err.Error()))
		return
	}

	h.recordAudit(r, "wakeup_target_keys", name, "success", fmt.Sprintf("recovered=%d", recovered))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recovered": recovered})
}

// validateModelMappings enforces that each mapping has a non-empty upstream,
// upstream names are unique (case-insensitively), and every comma-separated
// fallback alias does not collide with an upstream name or another alias
// within the same list.
func validateModelMappings(mappings []config.ModelMapping) error {
	upstreamSet := make(map[string]bool)
	fallbackSet := make(map[string]bool)
	for i, mm := range mappings {
		up := strings.TrimSpace(mm.Upstream)
		if up == "" {
			return fmt.Errorf("model_mappings[%d].upstream must not be empty", i)
		}
		upLow := strings.ToLower(up)
		if upstreamSet[upLow] {
			return fmt.Errorf("model_mappings[%d].upstream %q is duplicate (case-insensitive)", i, up)
		}
		upstreamSet[upLow] = true
		for _, alias := range config.SplitFallbackAliases(mm.Fallback) {
			aliasLow := strings.ToLower(alias)
			if upstreamSet[aliasLow] {
				return fmt.Errorf("model_mappings[%d].fallback %q conflicts with an upstream name", i, alias)
			}
			if fallbackSet[aliasLow] {
				return fmt.Errorf("model_mappings[%d].fallback %q is duplicate (case-insensitive)", i, alias)
			}
			fallbackSet[aliasLow] = true
		}
	}
	return nil
}

// maskKey returns a masked version of a key for safe logging.
func maskKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "..." + key[len(key)-4:]
}
