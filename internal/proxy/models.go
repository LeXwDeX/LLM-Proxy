// models.go — 本地模型列表（拦截 /v1/models 等端点）。
package proxy

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/catalog"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	appmiddleware "github.com/LeXwDeX/LLM-Proxy/internal/middleware"
)

// localCatalog 是包级别的模型目录单例，用于为模型补充属性。
var (
	localCatalogOnce sync.Once
	localCatalog     *catalog.Catalog
)

func getLocalCatalog() *catalog.Catalog {
	localCatalogOnce.Do(func() {
		c, err := catalog.New()
		if err != nil {
			slog.Warn("加载本地模型目录失败，模型属性将不可用", "error", err)
			return
		}
		localCatalog = c
	})
	return localCatalog
}

func isListEndpoint(path string) bool {
	path = strings.ToLower(path)
	return path == "/openai/deployments" ||
		path == "/openai/models" ||
		path == "/models" ||
		path == "/v1/models"
}

func (s *Service) maybeHandleLocalList(w http.ResponseWriter, r *http.Request) bool {
	if r == nil || r.Method != http.MethodGet {
		return false
	}
	path := strings.ToLower(r.URL.Path)
	if !isListEndpoint(path) {
		return false
	}

	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok || principal == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return true
	}

	var targetFilter map[string]struct{}
	if !principal.AllowAll() {
		targetFilter = principal.AllowedTargetsSet()
	}

	requested := strings.TrimSpace(r.Header.Get(headerProxyTarget))
	if requested == "" {
		requested = strings.TrimSpace(r.URL.Query().Get("target"))
	}
	if requested != "" {
		requestedLower := strings.ToLower(requested)
		if !principal.CanAccess(requestedLower) {
			http.Error(w, "target not allowed", http.StatusForbidden)
			return true
		}
		if _, exists := s.targetByName(requestedLower); !exists {
			http.Error(w, "unknown target", http.StatusBadRequest)
			return true
		}
		targetFilter = map[string]struct{}{requestedLower: {}}
	}

	items := s.buildLocalDeployments(targetFilter)
	resp := map[string]any{
		"object": "list",
		"data":   items,
		"first_id": func() string {
			if len(items) == 0 {
				return ""
			}
			if id, ok := items[0]["id"].(string); ok {
				return id
			}
			return ""
		}(),
		"last_id": func() string {
			if len(items) == 0 {
				return ""
			}
			if id, ok := items[len(items)-1]["id"].(string); ok {
				return id
			}
			return ""
		}(),
		"has_more": false,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.logger.Warn("failed to encode local list response",
			"request_id", appmiddleware.RequestIDFromContext(r.Context()),
			"error", err,
		)
		http.Error(w, "failed to build response", http.StatusInternalServerError)
		return true
	}
	return true
}

func (s *Service) buildLocalDeployments(targetFilter map[string]struct{}) []map[string]any {
	seen := make(map[string]struct{})
	result := make([]map[string]any, 0)
	snapshot := s.targetSnapshot()
	for _, state := range snapshot {
		if state == nil || state.Target() == nil {
			continue
		}
		target := state.Target()
		if target.Paused {
			continue
		}
		if targetFilter != nil {
			targetKey := strings.ToLower(strings.TrimSpace(target.Name))
			if _, allowed := targetFilter[targetKey]; !allowed {
				continue
			}
		}
		for _, m := range target.ModelMappings {
			upstream := strings.TrimSpace(m.Upstream)
			if upstream == "" {
				continue
			}
			if m.Paused {
				continue // 暂停的模型不对外暴露（含其 fallback 别名）
			}

			// Catalog attributes are resolved against the upstream model name
			// (aliases are not catalog entries). All surfaced names for this
			// mapping — upstream and aliases alike — inherit the same
			// attributes since they resolve to the same upstream model.
			var catEntry *catalog.ModelEntry
			if cat := getLocalCatalog(); cat != nil {
				catEntry = cat.Lookup(target.EndpointType, upstream)
			}

			// Surface the upstream name plus every comma-separated fallback
			// alias as independent, equally-ranked entries. No mapping
			// relation is exposed to the client.
			names := append([]string{upstream}, config.SplitFallbackAliases(m.Fallback)...)
			for _, name := range names {
				key := strings.ToLower(name)
				if _, exists := seen[key]; exists {
					continue
				}
				seen[key] = struct{}{}
				item := map[string]any{
					"object":   "model",
					"id":       name,
					"model":    name,
					"created":  time.Now().Unix(),
					"owned_by": "target:" + strings.ToLower(strings.TrimSpace(target.Name)),
				}

				if catEntry != nil {
					if catEntry.ContextWindow > 0 {
						item["context_length"] = catEntry.ContextWindow
					}
					if catEntry.MaxOutputTokens > 0 {
						item["max_output_tokens"] = catEntry.MaxOutputTokens
					}
					if len(catEntry.Capabilities) > 0 {
						item["capabilities"] = catEntry.Capabilities
					}
				}

				result = append(result, item)
			}
		}
	}

	// deterministic order
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i]["id"].(string)) < strings.ToLower(result[j]["id"].(string))
	})
	return result
}
