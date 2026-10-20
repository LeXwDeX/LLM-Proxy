package admin

import (
	"github.com/LeXwDeX/LLM-Proxy/internal/catalog"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/go-chi/chi/v5"
	"net/http"
)

// ===== Catalog API =====

func (h *Handler) handleListCatalog(w http.ResponseWriter, r *http.Request) {
	if h.modelCatalog == nil {
		writeJSON(w, http.StatusOK, map[string]any{"models": []any{}, "count": 0})
		return
	}

	epType := r.URL.Query().Get("endpoint_type")
	var models []catalog.ModelEntry
	if epType != "" {
		models = h.modelCatalog.ListByEndpointType(epType)
	} else {
		models = h.modelCatalog.ListAll()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"models": models,
		"count":  len(models),
	})
}

func (h *Handler) handleListCatalogByType(w http.ResponseWriter, r *http.Request) {
	if h.modelCatalog == nil {
		writeJSON(w, http.StatusOK, map[string]any{"models": []any{}, "count": 0})
		return
	}

	epType := chi.URLParam(r, "endpoint_type")
	models := h.modelCatalog.ListByEndpointType(epType)
	writeJSON(w, http.StatusOK, map[string]any{
		"models": models,
		"count":  len(models),
	})
}

// handleListEndpointTypes 暴露 endpoint_type 全集元数据，供 admin UI 渲染下拉、徽章。
//
// 单一信息源约束：UI 不得自己维护硬编码列表/标签/配色，必须从此 API 拉取。
// 数据源在 internal/config/endpoint_type.go 的 endpointTypes。
func (h *Handler) handleListEndpointTypes(w http.ResponseWriter, r *http.Request) {
	metas := config.AllEndpointTypeMetas()
	writeJSON(w, http.StatusOK, map[string]any{
		"endpoint_types": metas,
		"count":          len(metas),
	})
}
